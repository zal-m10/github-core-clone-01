// Package main's store: every object the forge serves, in memory.
//
// Repositories, refs, git objects, issues, labels, pull requests
// and comments live here under one RWMutex. Nothing in the store
// depends on the wall clock or on randomness: ids and numbers come
// from counters that start at fixed values, timestamps come from
// the logical clock, and every map is serialized in sorted order.
package main

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Object is one git object: its type and its exact bytes (no
// header; the id is SHA-1 over "<type> <size>\0" + bytes).
type Object struct {
	Type string // commit, tree, blob, tag
	Data []byte
}

// oidOf returns the object id of a type and its bytes.
func oidOf(typ string, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", typ, len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// Label is a repository label.
type Label struct {
	Name  string
	Color string
}

// Comment is an issue comment.
type Comment struct {
	ID        int64
	Body      string
	CreatedAt time.Time
}

// Issue is one issue. cachedJSON is the serialized GET response,
// rebuilt whenever the issue changes, so the read workload serves
// pre-encoded bytes.
type Issue struct {
	ID         int64
	Number     int64
	Title      string
	Body       string
	State      string // open | closed
	Labels     []string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	Comments   []*Comment
	cachedJSON []byte
}

// Pull is one pull request.
type Pull struct {
	ID             int64
	Number         int64
	Title          string
	Body           string
	HeadRef        string
	BaseRef        string
	HeadSHA        string
	BaseSHA        string
	State          string // open | closed
	Merged         bool
	MergedAt       time.Time
	MergeCommitSHA string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Repo is one repository: its git data and its issue tracker.
type Repo struct {
	ID            int64
	Owner         string
	Name          string
	Private       bool
	Description   string
	DefaultBranch string
	CreatedAt     time.Time

	Refs    map[string]string   // ref name -> oid
	Objects map[string]*Object  // oid -> object
	Labels  map[string]*Label   // label name -> label
	Issues  []*Issue
	Pulls   []*Pull

	nextNumber int64 // next shared issue/PR number, starts at 1

	packCacheMu sync.Mutex
	packCache map[string]*cachedPack // pack cache, keyed by request shape
}

// cachedPack is one fetch's result: the pack bytes plus the
// shallow-info lines, keyed by the fetch's shape.
type cachedPack struct {
	pack       []byte
	framed     []byte // pack sideband-framed once, reused by every fetch
	newShallow []string
	unshallow  []string
}

// Store is the whole forge state.
type Store struct {
	mu       sync.RWMutex
	repos    map[string]*Repo // owner + "/" + name -> repo
	byID     map[int64]*Repo
	nextRepo int64 // starts at 5000, the scenario's viewer block
	nextID   int64 // starts at 10000, for issue/comment ids
}

func NewStore() *Store {
	return &Store{
		repos:    make(map[string]*Repo),
		byID:     make(map[int64]*Repo),
		nextRepo: 5000,
		nextID:   10000,
	}
}

func repoKey(owner, name string) string { return owner + "/" + name }

// getRepo returns the repo or nil. The caller must hold at least a
// read lock.
func (s *Store) getRepo(owner, name string) *Repo {
	return s.repos[repoKey(owner, name)]
}

// createRepo creates a repository owned by owner. It returns nil and
// an error naming the conflict when it already exists.
func (s *Store) createRepo(owner, name string, private bool, description string) (*Repo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.repos[repoKey(owner, name)]; ok {
		return nil, fmt.Errorf("repository %s/%s already exists", owner, name)
	}
	r := &Repo{
		ID:            s.nextRepo,
		Owner:         owner,
		Name:          name,
		Private:       private,
		Description:   description,
		DefaultBranch: "main",
		CreatedAt:     tickTime(),
		Refs:          make(map[string]string),
		Objects:       make(map[string]*Object),
		Labels:        make(map[string]*Label),
		nextNumber:    1,
		packCache:     make(map[string]*cachedPack),
	}
	s.nextRepo++
	s.repos[repoKey(owner, name)] = r
	s.byID[r.ID] = r
	return r, nil
}

// deleteRepo removes a repository. It returns false when absent.
func (s *Store) deleteRepo(owner, name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.repos[repoKey(owner, name)]
	if !ok {
		return false
	}
	delete(s.repos, repoKey(owner, name))
	delete(s.byID, r.ID)
	return true
}

// addObject stores an object, returning its id. It is a no-op when
// the object is already present. The caller holds the write lock.
func (r *Repo) addObject(typ string, data []byte) string {
	oid := oidOf(typ, data)
	if _, ok := r.Objects[oid]; !ok {
		r.Objects[oid] = &Object{Type: typ, Data: data}
		// A new object can change any pack, so the fetch cache
		// is dropped. Every object creation goes through here,
		// so this is the single invalidation point.
		r.packCacheMu.Lock()
		r.packCache = make(map[string]*cachedPack)
		r.packCacheMu.Unlock()
	}
	return oid
}

// fetchPack returns the pack for a fetch, reusing a cached pack
// when the same repo has already answered the same fetch shape.
// The caller holds the store read lock, so the objects the pack
// is built from cannot change while it is built or stored; the
// cache mutex serialises concurrent identical fetches so the
// pack is built once.
func (r *Repo) fetchPack(wants, haves []string, depth int, shallow []string) *cachedPack {
	key := strings.Join(wants, "\x00") + "|" + strings.Join(haves, "\x00") + "|" + strconv.Itoa(depth) + "|" + strings.Join(shallow, "\x00")
	r.packCacheMu.Lock()
	defer r.packCacheMu.Unlock()
	if cp, ok := r.packCache[key]; ok {
		return cp
	}
	objects, newShallow, unshallow := r.collectFetch(wants, haves, depth, shallow)
	pack := writePack(objects)
	cp := &cachedPack{pack: pack, framed: sideband(pack), newShallow: newShallow, unshallow: unshallow}
	r.packCache[key] = cp
	return cp
}

// object returns the object with this id, or nil.
func (r *Repo) object(oid string) *Object { return r.Objects[oid] }

// ---------------------------------------------------------------- commits and trees

// commitInfo is the parsed head of a commit object.
type commitInfo struct {
	Tree    string
	Parents []string
	Message string
}

// parseCommit parses a commit's tree, parents and message.
func parseCommit(data []byte) commitInfo {
	var c commitInfo
	rest := data
	for {
		nl := indexByte(rest, '\n')
		if nl < 0 {
			break
		}
		line := string(rest[:nl])
		rest = rest[nl+1:]
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "tree ") {
			c.Tree = line[5:]
		} else if strings.HasPrefix(line, "parent ") {
			c.Parents = append(c.Parents, line[7:])
		}
	}
	c.Message = string(rest)
	return c
}

// treeEntry is one entry of a tree object.
type treeEntry struct {
	Mode string
	Name string
	OID  string
}

// parseTree parses a tree object's entries in stored order.
func parseTree(data []byte) []treeEntry {
	var out []treeEntry
	for len(data) > 0 {
		sp := indexByte(data, ' ')
		if sp < 0 || len(data) < sp+1+20 {
			break
		}
		mode := string(data[:sp])
		nul := indexByte(data[sp+1:], 0)
		if nul < 0 {
			break
		}
		name := string(data[sp+1 : sp+1+nul])
		oid := hex.EncodeToString(data[sp+1+nul+1 : sp+1+nul+21])
		out = append(out, treeEntry{Mode: mode, Name: name, OID: oid})
		data = data[sp+1+nul+21:]
	}
	return out
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

// buildCommit builds commit bytes for a tree, parents, message and
// timestamp, with the viewer as author and committer.
func buildCommit(tree string, parents []string, message string, ts int64) []byte {
	var b strings.Builder
	b.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		b.WriteString("parent " + p + "\n")
	}
	ident := fmt.Sprintf("%s <%s> %d +0000", viewerName, viewerEmail, ts)
	b.WriteString("author " + ident + "\n")
	b.WriteString("committer " + ident + "\n")
	b.WriteString("\n")
	b.WriteString(message)
	return []byte(b.String())
}

// flattenTree returns path -> entry for every file in a tree,
// walking subtrees. The result is a flat file map.
func (r *Repo) flattenTree(treeOID string) map[string]treeEntry {
	out := make(map[string]treeEntry)
	var walk func(oid, prefix string)
	walk = func(oid, prefix string) {
		obj := r.Objects[oid]
		if obj == nil || obj.Type != "tree" {
			return
		}
		for _, e := range parseTree(obj.Data) {
			if e.Mode == "40000" {
				walk(e.OID, prefix+e.Name+"/")
			} else {
				out[prefix+e.Name] = e
			}
		}
	}
	walk(treeOID, "")
	return out
}

// treeAt walks the tree objects from rootTree following path
// components, returning the tree oid at that path.
func (r *Repo) treeAt(rootTreeOID, path string) (string, bool) {
	if path == "" {
		return rootTreeOID, true
	}
	cur := rootTreeOID
	for _, comp := range strings.Split(path, "/") {
		obj := r.Objects[cur]
		if obj == nil || obj.Type != "tree" {
			return "", false
		}
		found := ""
		for _, e := range parseTree(obj.Data) {
			if e.Name == comp && e.Mode == "40000" {
				found = e.OID
				break
			}
		}
		if found == "" {
			return "", false
		}
		cur = found
	}
	return cur, true
}

// buildTree builds (and stores) the tree objects for a flat file
// map, returning the root tree id. Entries are sorted by name, as
// git stores them.
func (r *Repo) buildTree(files map[string]treeEntry) string {
	// Group paths by their first component.
	type node struct {
		files map[string]treeEntry // leaf name -> entry
		dirs  map[string]*node
	}
	root := &node{files: make(map[string]treeEntry), dirs: make(map[string]*node)}
	for path, e := range files {
		parts := strings.Split(path, "/")
		cur := root
		for _, p := range parts[:len(parts)-1] {
			next, ok := cur.dirs[p]
			if !ok {
				next = &node{files: make(map[string]treeEntry), dirs: make(map[string]*node)}
				cur.dirs[p] = next
			}
			cur = next
		}
		cur.files[parts[len(parts)-1]] = e
	}
	var build func(n *node) string
	build = func(n *node) string {
		var entries []treeEntry
		for name, e := range n.files {
			entries = append(entries, treeEntry{Mode: e.Mode, Name: name, OID: e.OID})
		}
		for name, d := range n.dirs {
			entries = append(entries, treeEntry{Mode: "40000", Name: name, OID: build(d)})
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
		var b []byte
		for _, e := range entries {
			raw, _ := hex.DecodeString(e.OID)
			b = append(b, []byte(e.Mode+" "+e.Name+"\x00")...)
			b = append(b, raw...)
		}
		return r.addObject("tree", b)
	}
	return build(root)
}

// isAncestor reports whether ancestor is reachable from descendant
// by following parent links (including equality).
func (r *Repo) isAncestor(ancestor, descendant string) bool {
	if ancestor == descendant {
		return true
	}
	if ancestor == zeroOID {
		return true
	}
	seen := map[string]bool{}
	queue := []string{descendant}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := r.Objects[oid]
		if obj == nil || obj.Type != "commit" {
			continue
		}
		for _, p := range parseCommit(obj.Data).Parents {
			if p == ancestor {
				return true
			}
			queue = append(queue, p)
		}
	}
	return false
}

// mergeBase returns the nearest common ancestor of a and b, or "".
func (r *Repo) mergeBase(a, b string) string {
	if a == "" || b == "" || a == zeroOID || b == zeroOID {
		return ""
	}
	fromA := map[string]bool{}
	queue := []string{a}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if fromA[oid] {
			continue
		}
		fromA[oid] = true
		obj := r.Objects[oid]
		if obj == nil || obj.Type != "commit" {
			continue
		}
		queue = append(queue, parseCommit(obj.Data).Parents...)
	}
	queue = []string{b}
	seen := map[string]bool{}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		if fromA[oid] {
			return oid
		}
		obj := r.Objects[oid]
		if obj == nil || obj.Type != "commit" {
			continue
		}
		queue = append(queue, parseCommit(obj.Data).Parents...)
	}
	return ""
}

// reachableCommits returns every commit reachable from tip,
// in no particular order.
func (r *Repo) reachableCommits(tip string) map[string]bool {
	out := map[string]bool{}
	queue := []string{tip}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if out[oid] || oid == zeroOID {
			continue
		}
		obj := r.Objects[oid]
		if obj == nil || obj.Type != "commit" {
			continue
		}
		out[oid] = true
		queue = append(queue, parseCommit(obj.Data).Parents...)
	}
	return out
}

// ---------------------------------------------------------------- global ids

// Global ids are opaque and stable across reads and surfaces.
// Repo: R_<id>; issue: I_<repoID>_<number>; pull: P_<repoID>_<number>;
// comment: C_<id>; user: U_<id>.
func repoGlobalID(id int64) string    { return fmt.Sprintf("R_%d", id) }
func issueGlobalID(repoID, n int64) string { return fmt.Sprintf("I_%d_%d", repoID, n) }
func pullGlobalID(repoID, n int64) string  { return fmt.Sprintf("P_%d_%d", repoID, n) }
func commentGlobalID(id int64) string { return fmt.Sprintf("C_%d", id) }

// parseGlobalID splits a global id into its kind and numbers.
func parseGlobalID(id string) (kind string, a, b int64, ok bool) {
	var n int
	if strings.HasPrefix(id, "R_") {
		kind = "R"
		n, _ = fmt.Sscanf(id, "R_%d", &a)
	} else if strings.HasPrefix(id, "I_") {
		kind = "I"
		n, _ = fmt.Sscanf(id, "I_%d_%d", &a, &b)
	} else if strings.HasPrefix(id, "P_") {
		kind = "P"
		n, _ = fmt.Sscanf(id, "P_%d_%d", &a, &b)
	} else if strings.HasPrefix(id, "C_") {
		kind = "C"
		n, _ = fmt.Sscanf(id, "C_%d", &a)
	} else if strings.HasPrefix(id, "U_") {
		kind = "U"
		n, _ = fmt.Sscanf(id, "U_%d", &a)
	}
	ok = n > 0
	return
}
