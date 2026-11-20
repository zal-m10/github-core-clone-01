// REST v3: repositories, branches, commits, contents, issues,
// labels, pull requests and merges.
//
// Every reply is JSON with a string message on errors, a
// non-empty X-GitHub-Request-Id (set by the router), and 404
// {"message": "Not Found"} for unknown routes and foreign owners.
// A repository belongs to the token owner only: the same name
// under another owner is 404, never 403.
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------- repos

func createRepoHandler(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		badJSON(w)
		return
	}
	var in struct {
		Name        string `json:"name"`
		Private     *bool  `json:"private"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		badJSON(w)
		return
	}
	if in.Name == "" {
		errJSON(w, http.StatusUnprocessableEntity, "name is required")
		return
	}
	private := true
	if in.Private != nil {
		private = *in.Private
	}
	repo, err := store.createRepo(viewerLogin, in.Name, private, in.Description)
	if err != nil {
		errJSON(w, http.StatusUnprocessableEntity, "name already exists on this account")
		return
	}
	writeJSON(w, http.StatusCreated, repoJSON(repo))
}

func listUserReposHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	perPage := 30
	if v := q.Get("per_page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			perPage = n
		}
	}
	page := 1
	if v := q.Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}
	store.mu.RLock()
	var all []*Repo
	for _, repo := range store.repos {
		if repo.Owner == viewerLogin {
			all = append(all, repo)
		}
	}
	store.mu.RUnlock()
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	total := len(all)
	start := (page - 1) * perPage
	if start > total {
		start = total
	}
	end := start + perPage
	if end > total {
		end = total
	}
	if end < total {
		next := fmt.Sprintf("http://%s/api/v3/user/repos?per_page=%d&page=%d", r.Host, perPage, page+1)
		lastPage := (total + perPage - 1) / perPage
		last := fmt.Sprintf("http://%s/api/v3/user/repos?per_page=%d&page=%d", r.Host, perPage, lastPage)
		w.Header().Set("Link", `<`+next+`>; rel="next", <`+last+`>; rel="last"`)
	}
	out := make([]map[string]any, 0, end-start)
	for _, repo := range all[start:end] {
		out = append(out, repoJSON(repo))
	}
	writeJSON(w, http.StatusOK, out)
}

func repoJSON(repo *Repo) map[string]any {
	return map[string]any{
		"id":             repo.ID,
		"node_id":        repoGlobalID(repo.ID),
		"name":           repo.Name,
		"full_name":      repo.Owner + "/" + repo.Name,
		"owner":          userJSON(repo.Owner, viewerID, viewerName, viewerEmail),
		"private":        repo.Private,
		"description":    repo.Description,
		"default_branch": repo.DefaultBranch,
		"created_at":     isoTime(repo.CreatedAt),
		"updated_at":     isoTime(repo.CreatedAt),
		"html_url":       "https://example.invalid/" + repo.Owner + "/" + repo.Name,
		"clone_url":      "https://example.invalid/" + repo.Owner + "/" + repo.Name + ".git",
		"has_issues":     true,
	}
}

// ---------------------------------------------------------------- repo routing

// routeRepo handles /api/v3/repos/{owner}/{name}/...
func routeRepo(w http.ResponseWriter, r *http.Request, parts []string) {
	owner, name := parts[0], parts[1]
	rest := parts[2:]
	if owner != viewerLogin {
		notFound(w)
		return
	}
	store.mu.RLock()
	repo := store.getRepo(owner, name)
	store.mu.RUnlock()
	if repo == nil {
		notFound(w)
		return
	}
	if len(rest) == 0 {
		switch r.Method {
		case "GET":
			writeJSON(w, http.StatusOK, repoJSON(repo))
		case "PATCH":
			patchRepoHandler(w, r, repo)
		case "DELETE":
			if store.deleteRepo(owner, name) {
				w.WriteHeader(http.StatusNoContent)
			} else {
				notFound(w)
			}
		default:
			notFound(w)
		}
		return
	}
	switch rest[0] {
	case "git":
		if len(rest) == 2 && rest[1] == "refs" && r.Method == "POST" {
			createRefHandler(w, r, repo)
			return
		}
	case "branches":
		if len(rest) == 1 && r.Method == "GET" {
			listBranchesHandler(w, repo)
			return
		}
	case "commits":
		if len(rest) == 1 && r.Method == "GET" {
			listCommitsHandler(w, r, repo)
			return
		}
	case "contents":
		routeContents(w, r, repo, rest[1:])
		return
	case "labels":
		if len(rest) == 1 && r.Method == "POST" {
			createLabelHandler(w, r, repo)
			return
		}
	case "issues":
		routeIssues(w, r, repo, rest[1:])
		return
	case "pulls":
		routePulls(w, r, repo, rest[1:])
		return
	}
	notFound(w)
}

func patchRepoHandler(w http.ResponseWriter, r *http.Request, repo *Repo) {
	body, err := readBody(r)
	if err != nil {
		badJSON(w)
		return
	}
	var in struct {
		Description *string `json:"description"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		badJSON(w)
		return
	}
	store.mu.Lock()
	if in.Description != nil {
		repo.Description = *in.Description
	}
	store.mu.Unlock()
	writeJSON(w, http.StatusOK, repoJSON(repo))
}

func createRefHandler(w http.ResponseWriter, r *http.Request, repo *Repo) {
	body, err := readBody(r)
	if err != nil {
		badJSON(w)
		return
	}
	var in struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		badJSON(w)
		return
	}
	if !strings.HasPrefix(in.Ref, "refs/") || len(in.SHA) != 40 {
		errJSON(w, http.StatusUnprocessableEntity, "invalid ref or sha")
		return
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if repo.object(in.SHA) == nil {
		errJSON(w, http.StatusUnprocessableEntity, "object does not exist")
		return
	}
	if _, ok := repo.Refs[in.Ref]; ok {
		errJSON(w, http.StatusUnprocessableEntity, "ref already exists")
		return
	}
	repo.Refs[in.Ref] = in.SHA
	writeJSON(w, http.StatusCreated, map[string]any{
		"ref":    in.Ref,
		"object": map[string]string{"sha": in.SHA, "type": "commit"},
	})
}

func listBranchesHandler(w http.ResponseWriter, repo *Repo) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	names := make([]string, 0)
	for name := range repo.Refs {
		if strings.HasPrefix(name, "refs/heads/") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	out := make([]map[string]any, 0, len(names))
	for _, name := range names {
		out = append(out, map[string]any{
			"name":   strings.TrimPrefix(name, "refs/heads/"),
			"commit": map[string]string{"sha": repo.Refs[name]},
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func listCommitsHandler(w http.ResponseWriter, r *http.Request, repo *Repo) {
	sha := r.URL.Query().Get("sha")
	store.mu.RLock()
	defer store.mu.RUnlock()
	var tip string
	if sha == "" {
		tip = repo.Refs["refs/heads/"+repo.DefaultBranch]
	} else if strings.HasPrefix(sha, "refs/") {
		tip = repo.Refs[sha]
	} else if strings.HasPrefix(sha, "refs/heads/") {
		tip = repo.Refs[sha]
	} else {
		// A branch name or an oid.
		if v, ok := repo.Refs["refs/heads/"+sha]; ok {
			tip = v
		} else {
			tip = sha
		}
	}
	var out []map[string]any
	seen := map[string]bool{}
	queue := []string{tip}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if oid == "" || oid == zeroOID || seen[oid] {
			continue
		}
		seen[oid] = true
		obj := repo.object(oid)
		if obj == nil || obj.Type != "commit" {
			continue
		}
		c := parseCommit(obj.Data)
		parents := make([]map[string]string, 0, len(c.Parents))
		for _, p := range c.Parents {
			parents = append(parents, map[string]string{"sha": p})
		}
		out = append(out, map[string]any{
			"sha": oid,
			"commit": map[string]any{
				"message": c.Message,
				"author":  map[string]string{"name": viewerName, "email": viewerEmail},
				"committer": map[string]string{"name": viewerName, "email": viewerEmail},
			},
			"parents": parents,
		})
		queue = append(queue, c.Parents...)
	}
	if out == nil {
		out = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------- contents

func routeContents(w http.ResponseWriter, r *http.Request, repo *Repo, parts []string) {
	path := strings.Join(parts, "/")
	switch r.Method {
	case "GET":
		getContentHandler(w, r, repo, path)
	case "PUT":
		putContentHandler(w, r, repo, path)
	case "DELETE":
		deleteContentHandler(w, r, repo, path)
	default:
		notFound(w)
	}
}

// resolveRef turns a ref query (branch, oid, or empty) into a
// commit oid. Empty means the default branch tip.
func (r *Repo) resolveRef(ref string) string {
	if ref == "" {
		return r.Refs["refs/heads/"+r.DefaultBranch]
	}
	if v, ok := r.Refs["refs/heads/"+ref]; ok {
		return v
	}
	if v, ok := r.Refs[ref]; ok {
		return v
	}
	return ref
}

func getContentHandler(w http.ResponseWriter, r *http.Request, repo *Repo, path string) {
	ref := r.URL.Query().Get("ref")
	store.mu.RLock()
	defer store.mu.RUnlock()
	tip := repo.resolveRef(ref)
	if tip == "" || tip == zeroOID {
		notFound(w)
		return
	}
	obj := repo.object(tip)
	if obj == nil || obj.Type != "commit" {
		notFound(w)
		return
	}
	files := repo.flattenTree(parseCommit(obj.Data).Tree)
	rootTree := parseCommit(obj.Data).Tree
	if path == "" {
		writeJSON(w, http.StatusOK, repo.listDir(rootTree, ""))
		return
	}
	if e, ok := files[path]; ok {
		blob := repo.object(e.OID)
		writeJSON(w, http.StatusOK, fileJSON(path, e.OID, blob))
		return
	}
	// A directory: list the tree object at that path.
	treeOID, ok := repo.treeAt(rootTree, path)
	if !ok {
		notFound(w)
		return
	}
	entries := repo.listDir(treeOID, path)
	if entries == nil {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

// listDir lists the direct entries of a tree object. prefix is
// the tree's path from the repo root ("" for the root tree); every
// entry carries name, full path, type, sha and size, as the
// contents API shape requires.
func (r *Repo) listDir(treeOID, prefix string) []map[string]any {
	obj := r.Objects[treeOID]
	if obj == nil || obj.Type != "tree" {
		return nil
	}
	var out []map[string]any
	for _, e := range parseTree(obj.Data) {
		path := e.Name
		if prefix != "" {
			path = prefix + "/" + e.Name
		}
		if e.Mode == "40000" {
			out = append(out, map[string]any{"name": e.Name, "path": path, "type": "dir", "sha": e.OID, "size": 0})
		} else {
			blob := r.Objects[e.OID]
			out = append(out, fileJSON(path, e.OID, blob))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["name"].(string) < out[j]["name"].(string) })
	if out == nil {
		out = []map[string]any{}
	}
	return out
}

func fileJSON(path, oid string, blob *Object) map[string]any {
	var data []byte
	if blob != nil {
		data = blob.Data
	}
	name := path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		name = path[i+1:]
	}
	return map[string]any{
		"name":     name,
		"path":     path,
		"type":     "file",
		"sha":      oid,
		"size":     len(data),
		"encoding": "base64",
		"content":  base64.StdEncoding.EncodeToString(data),
	}
}

func putContentHandler(w http.ResponseWriter, r *http.Request, repo *Repo, path string) {
	body, err := readBody(r)
	if err != nil {
		badJSON(w)
		return
	}
	var in struct {
		Message string `json:"message"`
		Content string `json:"content"`
		SHA     string `json:"sha"`
		Branch  string `json:"branch"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		badJSON(w)
		return
	}
	data, err := base64.StdEncoding.DecodeString(in.Content)
	if err != nil {
		errJSON(w, http.StatusUnprocessableEntity, "content is not valid base64")
		return
	}
	branch := in.Branch
	if branch == "" {
		branch = repo.DefaultBranch
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	refName := "refs/heads/" + branch
	tip := repo.Refs[refName]
	var files map[string]treeEntry
	if tip == "" || tip == zeroOID {
		// A new branch starts from the default branch's tip,
		// like GitHub: its files and its parent come from there.
		tip = repo.Refs["refs/heads/"+repo.DefaultBranch]
	}
	if tip == "" || tip == zeroOID {
		files = make(map[string]treeEntry)
	} else {
		obj := repo.object(tip)
		if obj == nil || obj.Type != "commit" {
			errJSON(w, http.StatusConflict, "branch tip is not a commit")
			return
		}
		files = repo.flattenTree(parseCommit(obj.Data).Tree)
	}
	blobOID := oidOf("blob", data)
	if cur, ok := files[path]; ok {
		// Update: a sha is required and must match.
		if in.SHA == "" || in.SHA != cur.OID {
			errJSON(w, http.StatusConflict, "sha does not match the current file")
			return
		}
	} else if in.SHA != "" {
		errJSON(w, http.StatusUnprocessableEntity, "file does not exist")
		return
	}
	blobOID = repo.addObject("blob", data)
	files[path] = treeEntry{Mode: "100644", Name: path, OID: blobOID}
	treeOID := repo.buildTree(files)
	var parents []string
	if tip != "" && tip != zeroOID {
		parents = []string{tip}
	}
	msg := in.Message
	if msg == "" {
		msg = "Update " + path + "\n"
	}
	commitBytes := buildCommit(treeOID, parents, msg, tickUnix())
	commitOID := repo.addObject("commit", commitBytes)
	repo.Refs[refName] = commitOID
	// An update carries a sha; a create does not.
	status := http.StatusCreated
	if in.SHA != "" {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{
		"content": map[string]any{"sha": blobOID, "encoding": "base64", "size": len(data)},
		"commit":  map[string]string{"sha": commitOID},
	})
}

func deleteContentHandler(w http.ResponseWriter, r *http.Request, repo *Repo, path string) {
	body, err := readBody(r)
	if err != nil {
		badJSON(w)
		return
	}
	var in struct {
		Message string `json:"message"`
		SHA     string `json:"sha"`
		Branch  string `json:"branch"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		badJSON(w)
		return
	}
	branch := in.Branch
	if branch == "" {
		branch = repo.DefaultBranch
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	refName := "refs/heads/" + branch
	tip := repo.Refs[refName]
	if tip == "" || tip == zeroOID {
		notFound(w)
		return
	}
	obj := repo.object(tip)
	if obj == nil || obj.Type != "commit" {
		notFound(w)
		return
	}
	files := repo.flattenTree(parseCommit(obj.Data).Tree)
	cur, ok := files[path]
	if !ok {
		notFound(w)
		return
	}
	if in.SHA != "" && in.SHA != cur.OID {
		errJSON(w, http.StatusConflict, "sha does not match the current file")
		return
	}
	delete(files, path)
	treeOID := repo.buildTree(files)
	msg := in.Message
	if msg == "" {
		msg = "Delete " + path + "\n"
	}
	commitBytes := buildCommit(treeOID, []string{tip}, msg, tickUnix())
	commitOID := repo.addObject("commit", commitBytes)
	repo.Refs[refName] = commitOID
	writeJSON(w, http.StatusOK, map[string]any{
		"commit": map[string]string{"sha": commitOID},
	})
}

// ---------------------------------------------------------------- labels

func createLabelHandler(w http.ResponseWriter, r *http.Request, repo *Repo) {
	body, err := readBody(r)
	if err != nil {
		badJSON(w)
		return
	}
	var in struct {
		Name  string `json:"name"`
		Color string `json:"color"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		badJSON(w)
		return
	}
	if in.Name == "" {
		errJSON(w, http.StatusUnprocessableEntity, "name is required")
		return
	}
	color := in.Color
	if color == "" {
		color = "ededed"
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, ok := repo.Labels[in.Name]; ok {
		errJSON(w, http.StatusUnprocessableEntity, "label already exists")
		return
	}
	repo.Labels[in.Name] = &Label{Name: in.Name, Color: color}
	writeJSON(w, http.StatusCreated, labelJSON(repo.Labels[in.Name]))
}

func labelJSON(l *Label) map[string]any {
	return map[string]any{"name": l.Name, "color": l.Color, "id": 0}
}

// ---------------------------------------------------------------- issues

func routeIssues(w http.ResponseWriter, r *http.Request, repo *Repo, parts []string) {
	if len(parts) == 0 {
		switch r.Method {
		case "POST":
			createIssueHandler(w, r, repo)
		case "GET":
			listIssuesHandler(w, r, repo)
		default:
			notFound(w)
		}
		return
	}
	n, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		notFound(w)
		return
	}
	if len(parts) == 1 {
		switch r.Method {
		case "GET":
			getIssueHandler(w, repo, n)
		case "PATCH":
			patchIssueHandler(w, r, repo, n)
		default:
			notFound(w)
		}
		return
	}
	if len(parts) == 2 && parts[1] == "comments" {
		switch r.Method {
		case "POST":
			createCommentHandler(w, r, repo, n)
		case "GET":
			listCommentsHandler(w, repo, n)
		default:
			notFound(w)
		}
		return
	}
	notFound(w)
}

func (s *Store) findIssue(repo *Repo, number int64) *Issue {
	for _, iss := range repo.Issues {
		if iss.Number == number {
			return iss
		}
	}
	return nil
}

func createIssueHandler(w http.ResponseWriter, r *http.Request, repo *Repo) {
	body, err := readBody(r)
	if err != nil {
		badJSON(w)
		return
	}
	var in struct {
		Title  string   `json:"title"`
		Body   string   `json:"body"`
		Labels []string `json:"labels"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		badJSON(w)
		return
	}
	if in.Title == "" {
		errJSON(w, http.StatusUnprocessableEntity, "title is required")
		return
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, name := range in.Labels {
		if _, ok := repo.Labels[name]; !ok {
			errJSON(w, http.StatusUnprocessableEntity, "label does not exist: "+name)
			return
		}
	}
	now := tickTime()
	iss := &Issue{
		ID:        store.nextID,
		Number:    repo.nextNumber,
		Title:     in.Title,
		Body:      in.Body,
		State:     "open",
		Labels:    append([]string{}, in.Labels...),
		CreatedAt: now,
		UpdatedAt: now,
	}
	store.nextID++
	repo.nextNumber++
	repo.Issues = append(repo.Issues, iss)
	iss.rebuildCache(repo)
	writeJSON(w, http.StatusCreated, issueJSON(repo, iss))
}

// rebuildCache re-encodes the issue's GET response.
func (iss *Issue) rebuildCache(repo *Repo) {
	iss.cachedJSON, _ = json.Marshal(issueJSON(repo, iss))
}

func issueJSON(repo *Repo, iss *Issue) map[string]any {
	labels := make([]map[string]any, 0, len(iss.Labels))
	for _, name := range iss.Labels {
		if l, ok := repo.Labels[name]; ok {
			labels = append(labels, labelJSON(l))
		}
	}
	return map[string]any{
		"id":         iss.ID,
		"node_id":    issueGlobalID(repo.ID, iss.Number),
		"number":     iss.Number,
		"title":      iss.Title,
		"body":       iss.Body,
		"state":      iss.State,
		"user":       userJSON(viewerLogin, viewerID, viewerName, viewerEmail),
		"labels":     labels,
		"created_at": isoTime(iss.CreatedAt),
		"updated_at": isoTime(iss.UpdatedAt),
		"comments":   len(iss.Comments),
		"html_url":   "https://example.invalid/" + repo.Owner + "/" + repo.Name + "/issues/" + strconv.FormatInt(iss.Number, 10),
	}
}

func listIssuesHandler(w http.ResponseWriter, r *http.Request, repo *Repo) {
	q := r.URL.Query()
	state := q.Get("state")
	if state == "" {
		state = "open"
	}
	sortDir := q.Get("direction")
	if sortDir == "" {
		sortDir = "desc"
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	var out []map[string]any
	for _, iss := range repo.Issues {
		if state != "all" && iss.State != state {
			continue
		}
		out = append(out, issueJSON(repo, iss))
	}
	if sortDir == "asc" {
		sort.Slice(out, func(i, j int) bool { return out[i]["number"].(int64) < out[j]["number"].(int64) })
	} else {
		sort.Slice(out, func(i, j int) bool { return out[i]["number"].(int64) > out[j]["number"].(int64) })
	}
	if out == nil {
		out = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, out)
}

func getIssueHandler(w http.ResponseWriter, repo *Repo, number int64) {
	store.mu.RLock()
	iss := store.findIssue(repo, number)
	if iss == nil {
		store.mu.RUnlock()
		notFound(w)
		return
	}
	cached := iss.cachedJSON
	store.mu.RUnlock()
	if cached == nil {
		store.mu.RLock()
		iss.rebuildCache(repo)
		cached = iss.cachedJSON
		store.mu.RUnlock()
	}
	writeRawJSON(w, http.StatusOK, cached)
}

func patchIssueHandler(w http.ResponseWriter, r *http.Request, repo *Repo, number int64) {
	body, err := readBody(r)
	if err != nil {
		badJSON(w)
		return
	}
	var in struct {
		State string `json:"state"`
		Title *string `json:"title"`
		Body  *string `json:"body"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		badJSON(w)
		return
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	iss := store.findIssue(repo, number)
	if iss == nil {
		notFound(w)
		return
	}
	if in.State == "closed" || in.State == "open" {
		iss.State = in.State
	}
	if in.Title != nil {
		iss.Title = *in.Title
	}
	if in.Body != nil {
		iss.Body = *in.Body
	}
	iss.UpdatedAt = tickTime()
	iss.rebuildCache(repo)
	writeJSON(w, http.StatusOK, issueJSON(repo, iss))
}

func createCommentHandler(w http.ResponseWriter, r *http.Request, repo *Repo, number int64) {
	body, err := readBody(r)
	if err != nil {
		badJSON(w)
		return
	}
	var in struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		badJSON(w)
		return
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	iss := store.findIssue(repo, number)
	if iss == nil {
		notFound(w)
		return
	}
	c := &Comment{ID: store.nextID, Body: in.Body, CreatedAt: tickTime()}
	store.nextID++
	iss.Comments = append(iss.Comments, c)
	iss.UpdatedAt = tickTime()
	iss.rebuildCache(repo)
	writeJSON(w, http.StatusCreated, commentJSON(repo, iss, c))
}

func listCommentsHandler(w http.ResponseWriter, repo *Repo, number int64) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	iss := store.findIssue(repo, number)
	if iss == nil {
		notFound(w)
		return
	}
	out := make([]map[string]any, 0, len(iss.Comments))
	for _, c := range iss.Comments {
		out = append(out, commentJSON(repo, iss, c))
	}
	writeJSON(w, http.StatusOK, out)
}

func commentJSON(repo *Repo, iss *Issue, c *Comment) map[string]any {
	return map[string]any{
		"id":         c.ID,
		"node_id":    commentGlobalID(c.ID),
		"body":       c.Body,
		"user":       userJSON(viewerLogin, viewerID, viewerName, viewerEmail),
		"created_at": isoTime(c.CreatedAt),
		"updated_at": isoTime(c.CreatedAt),
		"html_url":   "https://example.invalid/" + repo.Owner + "/" + repo.Name + "/issues/" + strconv.FormatInt(iss.Number, 10) + "#issuecomment-" + strconv.FormatInt(c.ID, 10),
	}
}

// ---------------------------------------------------------------- pull requests

func routePulls(w http.ResponseWriter, r *http.Request, repo *Repo, parts []string) {
	if len(parts) == 0 {
		switch r.Method {
		case "POST":
			createPullHandler(w, r, repo)
		case "GET":
			listPullsHandler(w, r, repo)
		default:
			notFound(w)
		}
		return
	}
	n, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		notFound(w)
		return
	}
	if len(parts) == 1 {
		if r.Method == "GET" {
			getPullHandler(w, repo, n)
			return
		}
		notFound(w)
		return
	}
	if len(parts) == 2 && parts[1] == "files" && r.Method == "GET" {
		listPullFilesHandler(w, repo, n)
		return
	}
	if len(parts) == 2 && parts[1] == "merge" && r.Method == "PUT" {
		mergePullHandler(w, r, repo, n)
		return
	}
	notFound(w)
}

func (s *Store) findPull(repo *Repo, number int64) *Pull {
	for _, pr := range repo.Pulls {
		if pr.Number == number {
			return pr
		}
	}
	return nil
}

func createPullHandler(w http.ResponseWriter, r *http.Request, repo *Repo) {
	body, err := readBody(r)
	if err != nil {
		badJSON(w)
		return
	}
	var in struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Head  string `json:"head"`
		Base  string `json:"base"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		badJSON(w)
		return
	}
	if in.Title == "" || in.Head == "" || in.Base == "" {
		errJSON(w, http.StatusUnprocessableEntity, "title, head and base are required")
		return
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	headSHA, ok := repo.Refs["refs/heads/"+in.Head]
	if !ok {
		errJSON(w, http.StatusUnprocessableEntity, "head branch does not exist")
		return
	}
	baseSHA, ok := repo.Refs["refs/heads/"+in.Base]
	if !ok {
		errJSON(w, http.StatusUnprocessableEntity, "base branch does not exist")
		return
	}
	now := tickTime()
	pr := &Pull{
		ID:        store.nextID,
		Number:    repo.nextNumber,
		Title:     in.Title,
		Body:      in.Body,
		HeadRef:   in.Head,
		BaseRef:   in.Base,
		HeadSHA:   headSHA,
		BaseSHA:   baseSHA,
		State:     "open",
		CreatedAt: now,
		UpdatedAt: now,
	}
	store.nextID++
	repo.nextNumber++
	repo.Pulls = append(repo.Pulls, pr)
	writeJSON(w, http.StatusCreated, pullJSON(repo, pr))
}

func pullJSON(repo *Repo, pr *Pull) map[string]any {
	mergedAt := any(nil)
	if pr.Merged {
		mergedAt = isoTime(pr.MergedAt)
	}
	mergeSHA := any(nil)
	if pr.MergeCommitSHA != "" {
		mergeSHA = pr.MergeCommitSHA
	}
	return map[string]any{
		"id":               pr.ID,
		"node_id":          pullGlobalID(repo.ID, pr.Number),
		"number":           pr.Number,
		"title":            pr.Title,
		"body":             pr.Body,
		"state":            pr.State,
		"merged":           pr.Merged,
		"mergeable":        repo.prMergeable(pr),
		"head":             map[string]string{"ref": pr.HeadRef, "sha": pr.HeadSHA},
		"base":             map[string]string{"ref": pr.BaseRef, "sha": pr.BaseSHA},
		"merged_at":        mergedAt,
		"merge_commit_sha": mergeSHA,
		"created_at":       isoTime(pr.CreatedAt),
		"updated_at":       isoTime(pr.UpdatedAt),
		"html_url":         "https://example.invalid/" + repo.Owner + "/" + repo.Name + "/pull/" + strconv.FormatInt(pr.Number, 10),
	}
}

// prMergeable reports whether the pull can merge cleanly: the head
// and the base must not have changed the same file differently
// since their merge-base.
func (r *Repo) prMergeable(pr *Pull) bool {
	if pr.Merged || pr.State != "open" {
		return false
	}
	// The base side is the CURRENT base branch tip, not the tip
	// recorded when the pull request was created: another pull
	// request may have merged since, and its changes are what
	// the head must not conflict with.
	baseTip := r.Refs["refs/heads/"+pr.BaseRef]
	if baseTip == "" {
		baseTip = pr.BaseSHA
	}
	base := r.mergeBase(pr.HeadSHA, baseTip)
	if base == "" {
		return true
	}
	headFiles := r.filesAt(pr.HeadSHA)
	baseFiles := r.filesAt(baseTip)
	mergeFiles := r.filesAt(base)
	for path, headEntry := range headFiles {
		mergeEntry, inMerge := mergeFiles[path]
		if !inMerge {
			continue // added on head only
		}
		if headEntry.OID == mergeEntry.OID {
			continue // unchanged on head
		}
		baseEntry, inBase := baseFiles[path]
		if !inBase || baseEntry.OID != mergeEntry.OID {
			return false // changed on both sides
		}
	}
	for path, baseEntry := range baseFiles {
		mergeEntry, inMerge := mergeFiles[path]
		if !inMerge {
			continue // added on base only
		}
		if baseEntry.OID == mergeEntry.OID {
			continue // unchanged on base
		}
		if _, inHead := headFiles[path]; !inHead {
			return false // deleted on head, changed on base
		}
	}
	return true
}

// filesAt returns the flat file map at a commit.
func (r *Repo) filesAt(commitOID string) map[string]treeEntry {
	obj := r.object(commitOID)
	if obj == nil || obj.Type != "commit" {
		return map[string]treeEntry{}
	}
	return r.flattenTree(parseCommit(obj.Data).Tree)
}

func listPullsHandler(w http.ResponseWriter, r *http.Request, repo *Repo) {
	state := r.URL.Query().Get("state")
	store.mu.RLock()
	defer store.mu.RUnlock()
	var out []map[string]any
	for _, pr := range repo.Pulls {
		if state == "all" || (state == "" && pr.State == "open") || pr.State == state {
			out = append(out, pullJSON(repo, pr))
		}
	}
	if out == nil {
		out = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, out)
}

func getPullHandler(w http.ResponseWriter, repo *Repo, number int64) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	pr := store.findPull(repo, number)
	if pr == nil {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, pullJSON(repo, pr))
}

// fileChange is one changed path between two trees.
type fileChange struct {
	Filename  string
	Status    string // added | modified | removed
	Additions int
	Deletions int
}

func (r *Repo) diffFiles(baseCommit, headCommit string) []fileChange {
	base := r.filesAt(baseCommit)
	head := r.filesAt(headCommit)
	var out []fileChange
	for path, h := range head {
		b, ok := base[path]
		if !ok {
			out = append(out, fileChange{Filename: path, Status: "added", Additions: countLines(r, h.OID)})
			continue
		}
		if b.OID == h.OID {
			continue
		}
		out = append(out, fileChange{Filename: path, Status: "modified", Additions: countLines(r, h.OID), Deletions: countLines(r, b.OID)})
	}
	for path, b := range base {
		if _, ok := head[path]; !ok {
			out = append(out, fileChange{Filename: path, Status: "removed", Deletions: countLines(r, b.OID)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Filename < out[j].Filename })
	return out
}

func countLines(r *Repo, oid string) int {
	obj := r.object(oid)
	if obj == nil {
		return 0
	}
	n := 0
	for _, b := range obj.Data {
		if b == '\n' {
			n++
		}
	}
	if len(obj.Data) > 0 && obj.Data[len(obj.Data)-1] != '\n' {
		n++
	}
	return n
}

func listPullFilesHandler(w http.ResponseWriter, repo *Repo, number int64) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	pr := store.findPull(repo, number)
	if pr == nil {
		notFound(w)
		return
	}
	changes := repo.diffFiles(pr.BaseSHA, pr.HeadSHA)
	out := make([]map[string]any, 0, len(changes))
	for _, c := range changes {
		out = append(out, map[string]any{
			"filename":  c.Filename,
			"status":    c.Status,
			"additions": c.Additions,
			"deletions": c.Deletions,
			"changes":   c.Additions + c.Deletions,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func mergePullHandler(w http.ResponseWriter, r *http.Request, repo *Repo, number int64) {
	body, err := readBody(r)
	if err != nil {
		badJSON(w)
		return
	}
	var in struct {
		MergeMethod string `json:"merge_method"`
	}
	_ = json.Unmarshal(body, &in)
	store.mu.Lock()
	defer store.mu.Unlock()
	pr := store.findPull(repo, number)
	if pr == nil {
		notFound(w)
		return
	}
	if pr.Merged || pr.State != "open" {
		errJSON(w, http.StatusMethodNotAllowed, "pull request is not open")
		return
	}
	if !repo.prMergeable(pr) {
		errJSON(w, http.StatusMethodNotAllowed, "Merge conflict")
		return
	}
	merged, msg := repo.doMerge(pr)
	if !merged {
		errJSON(w, http.StatusMethodNotAllowed, msg)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sha":     pr.MergeCommitSHA,
		"merged":  true,
		"message": "Pull Request successfully merged",
	})
}

// doMerge performs a squash merge onto the default branch. The
// squash commit carries the head tip's tree, so the head's content
// lands on the default branch. The caller holds the write lock.
func (r *Repo) doMerge(pr *Pull) (bool, string) {
	baseRef := "refs/heads/" + pr.BaseRef
	baseTip := r.Refs[baseRef]
	if baseTip == "" {
		return false, "base branch has no tip"
	}
	headObj := r.object(pr.HeadSHA)
	if headObj == nil || headObj.Type != "commit" {
		return false, "head is not a commit"
	}
	// A squash merge is a three-way merge of the head's changes
	// onto the current base tip: start from the base tip's files
	// and apply what the head changed since the merge base, so
	// files the base gained meanwhile are kept.
	mergeBase := r.mergeBase(pr.HeadSHA, baseTip)
	merged := r.filesAt(baseTip)
	headFiles := r.filesAt(pr.HeadSHA)
	mergeFiles := r.filesAt(mergeBase)
	for p, e := range headFiles {
		if me, ok := mergeFiles[p]; !ok || me.OID != e.OID {
			merged[p] = e
		}
	}
	for p := range mergeFiles {
		if _, inHead := headFiles[p]; !inHead {
			delete(merged, p)
		}
	}
	headTree := r.buildTree(merged)
	msg := pr.Title + "\n"
	commitBytes := buildCommit(headTree, []string{baseTip}, msg, tickUnix())
	commitOID := r.addObject("commit", commitBytes)
	r.Refs[baseRef] = commitOID
	now := tickTime()
	pr.Merged = true
	pr.State = "closed"
	pr.MergedAt = now
	pr.MergeCommitSHA = commitOID
	pr.UpdatedAt = now
	return true, ""
}
