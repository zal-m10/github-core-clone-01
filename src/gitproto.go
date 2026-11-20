// Git smart HTTP: the /{owner}/{repo}.git endpoints.
//
// Upload-pack speaks protocol v2 (ls-refs, fetch) and v0; receive-
// pack speaks v0 only, with report-status and side-band-64k. Packs
// are sideband-framed on every response that carries one, and the
// push report is a pkt-lined report-status stream inside band 1.
package main

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
)

const gitAgent = "forge/1.0"

// ---------------------------------------------------------------- pkt-line

func pktLine(s string) []byte {
	b := []byte(s)
	out := make([]byte, 4+len(b))
	copy(out[4:], b)
	hexLen := fmt.Sprintf("%04x", len(out))
	copy(out, hexLen)
	return out
}

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

// decodePktLines splits a buffer into packets. A packet is
// (kind, payload): kind is "data", "flush" or "delim".
type pkt struct {
	kind    string
	payload []byte
}

func decodePktLines(b []byte) ([]pkt, error) {
	var out []pkt
	for len(b) > 0 {
		if len(b) < 4 {
			return nil, fmt.Errorf("truncated pkt-line")
		}
		var n int
		if _, err := fmt.Sscanf(string(b[:4]), "%04x", &n); err != nil {
			return nil, fmt.Errorf("bad pkt-line length")
		}
		switch n {
		case 0:
			out = append(out, pkt{kind: "flush"})
			b = b[4:]
		case 1:
			out = append(out, pkt{kind: "delim"})
			b = b[4:]
		case 2:
			out = append(out, pkt{kind: "end"})
			b = b[4:]
		default:
			if n < 4 || len(b) < n {
				return nil, fmt.Errorf("truncated pkt-line body")
			}
			out = append(out, pkt{kind: "data", payload: b[4:n]})
			b = b[n:]
		}
	}
	return out, nil
}

// sideband wraps data into side-band-64k packets (band 1 = data),
// each at most 65520 bytes on the wire.
func sideband(data []byte) []byte {
	var out []byte
	for len(data) > 0 {
		n := len(data)
		if n > 65515 {
			n = 65515
		}
		payload := make([]byte, 1+n)
		payload[0] = 1
		copy(payload[1:], data[:n])
		out = append(out, pktLine(string(payload))...)
		data = data[n:]
	}
	return out
}

// ---------------------------------------------------------------- routing

func routeGit(w http.ResponseWriter, r *http.Request) {
	if !needAuth(w, r) {
		return
	}
	path := r.URL.Path
	// /{owner}/{repo}.git/<service>
	gitIdx := strings.Index(path, ".git/")
	if gitIdx < 0 {
		notFound(w)
		return
	}
	ownerRepo := path[1:gitIdx]
	service := path[gitIdx+5:]
	slash := strings.Index(ownerRepo, "/")
	if slash < 0 {
		notFound(w)
		return
	}
	owner, repoName := ownerRepo[:slash], ownerRepo[slash+1:]

	store.mu.RLock()
	repo := store.getRepo(owner, repoName)
	store.mu.RUnlock()
	if repo == nil {
		notFound(w)
		return
	}

	// One lock for the whole git exchange: a read lock for
	// advertisements and fetches, the write lock for pushes.
	switch service {
	case "info/refs":
		if r.Method != "GET" {
			notFound(w)
			return
		}
		store.mu.RLock()
		defer store.mu.RUnlock()
		handleInfoRefs(w, r, repo)
	case "git-upload-pack":
		if r.Method != "POST" {
			notFound(w)
			return
		}
		store.mu.RLock()
		defer store.mu.RUnlock()
		handleUploadPack(w, r, repo)
	case "git-receive-pack":
		if r.Method != "POST" {
			notFound(w)
			return
		}
		store.mu.Lock()
		defer store.mu.Unlock()
		handleReceivePack(w, r, repo)
	default:
		notFound(w)
	}
}

// sortedRefs returns the repo's refs sorted by name, HEAD first
// when it resolves to a branch.
func (r *Repo) sortedRefs() []struct {
	name, oid string
} {
	names := make([]string, 0, len(r.Refs))
	for name := range r.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]struct{ name, oid string }, 0, len(names)+1)
	if tip, ok := r.Refs["refs/heads/"+r.DefaultBranch]; ok {
		out = append(out, struct{ name, oid string }{"HEAD", tip})
	}
	for _, name := range names {
		out = append(out, struct{ name, oid string }{name, r.Refs[name]})
	}
	return out
}

// ---------------------------------------------------------------- info/refs

func handleInfoRefs(w http.ResponseWriter, r *http.Request, repo *Repo) {
	service := r.URL.Query().Get("service")
	if service != "git-upload-pack" && service != "git-receive-pack" {
		errJSON(w, http.StatusBadRequest, "unknown service")
		return
	}
	w.Header().Set("Content-Type", "application/x-"+service+"-advertisement")

	// Protocol v2 for upload-pack, when the client asks for it.
	if service == "git-upload-pack" && r.Header.Get("Git-Protocol") == "version=2" {
		var b bytes.Buffer
		b.Write(pktLine("version 2\n"))
		b.Write(pktLine("ls-refs=unborn\n"))
		b.Write(pktLine("fetch=shallow\n"))
		b.Write(pktLine("object-format=sha1\n"))
		b.Write(pktLine("agent=" + gitAgent + "\n"))
		b.Write(pktFlush)
		_, _ = w.Write(b.Bytes())
		return
	}

	// v0 advertisement.
	var caps string
	if service == "git-upload-pack" {
		caps = "side-band-64k thin-pack ofs-delta shallow multi_ack agent=" + gitAgent + " symref=HEAD:refs/heads/" + repo.DefaultBranch
	} else {
		caps = "report-status side-band-64k delete-refs ofs-delta agent=" + gitAgent
	}
	var b bytes.Buffer
	b.Write(pktLine("# service=" + service + "\n"))
	b.Write(pktFlush)
	refs := repo.sortedRefs()
	if len(refs) == 0 {
		b.Write(pktLine(zeroOID + " capabilities^{}\x00" + caps + "\n"))
	} else {
		for i, ref := range refs {
			line := ref.oid + " " + ref.name
			if i == 0 {
				line += "\x00" + caps
			}
			b.Write(pktLine(line + "\n"))
		}
		// Peeled annotated tags.
		for _, ref := range refs {
			if !strings.HasPrefix(ref.name, "refs/tags/") {
				continue
			}
			if obj := repo.object(ref.oid); obj != nil && obj.Type == "tag" {
				peeled := repo.parseTagPeel(ref.oid)
				b.Write(pktLine(peeled + " " + ref.name + "^{}\n"))
			}
		}
	}
	b.Write(pktFlush)
	_, _ = w.Write(b.Bytes())
}

// ---------------------------------------------------------------- upload-pack

func handleUploadPack(w http.ResponseWriter, r *http.Request, repo *Repo) {
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	body, err := readBody(r)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "cannot read request")
		return
	}
	if r.Header.Get("Git-Protocol") == "version=2" {
		handleUploadPackV2(w, repo, body)
		return
	}
	handleUploadPackV0(w, repo, body)
}

func handleUploadPackV2(w http.ResponseWriter, repo *Repo, body []byte) {
	packets, err := decodePktLines(body)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "bad pkt-line stream")
		return
	}
	var command string
	var args []string
	seenDelim := false
	for _, p := range packets {
		if p.kind == "delim" {
			seenDelim = true
			continue
		}
		if p.kind != "data" {
			continue
		}
		line := string(p.payload)
		if !seenDelim {
			if strings.HasPrefix(line, "command=") {
				command = line[8:]
			}
			continue
		}
		args = append(args, strings.TrimSuffix(line, "\n"))
	}
	command = strings.TrimSuffix(command, "\n")

	switch command {
	case "ls-refs":
		handleLsRefs(w, repo, args)
	case "fetch":
		handleFetchV2(w, repo, args)
	default:
		errJSON(w, http.StatusBadRequest, "unknown command")
	}
}

func handleLsRefs(w http.ResponseWriter, repo *Repo, args []string) {
	peel, symrefs, unborn := false, false, false
	var prefixes []string
	for _, a := range args {
		switch {
		case a == "peel":
			peel = true
		case a == "symrefs":
			symrefs = true
		case a == "unborn":
			unborn = true
		case strings.HasPrefix(a, "ref-prefix "):
			prefixes = append(prefixes, a[len("ref-prefix "):])
		}
	}
	keep := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	var b bytes.Buffer
	refs := repo.sortedRefs()
	if len(refs) == 0 {
		if unborn {
			line := "unborn HEAD"
			if symrefs {
				line += " symref-target:refs/heads/" + repo.DefaultBranch
			}
			b.Write(pktLine(line + "\n"))
		}
	} else {
		for _, ref := range refs {
			if !keep(ref.name) {
				continue
			}
			line := ref.oid + " " + ref.name
			if ref.name == "HEAD" && symrefs {
				line += " symref-target:refs/heads/" + repo.DefaultBranch
			}
			if peel && strings.HasPrefix(ref.name, "refs/tags/") {
				if obj := repo.object(ref.oid); obj != nil && obj.Type == "tag" {
					line += " peeled:" + repo.parseTagPeel(ref.oid)
				}
			}
			b.Write(pktLine(line + "\n"))
		}
	}
	b.Write(pktFlush)
	_, _ = w.Write(b.Bytes())
}

func handleFetchV2(w http.ResponseWriter, repo *Repo, args []string) {
	var wants, haves, shallow []string
	depth := 0
	relative := false
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "want "):
			wants = append(wants, a[5:])
		case strings.HasPrefix(a, "have "):
			haves = append(haves, a[5:])
		case strings.HasPrefix(a, "shallow "):
			shallow = append(shallow, a[8:])
		case strings.HasPrefix(a, "deepen "):
			fmt.Sscanf(a[7:], "%d", &depth)
		case a == "deepen-relative":
			relative = true
		}
	}
	if len(wants) == 0 {
		_, _ = w.Write(pktFlush)
		return
	}
	depth = repo.resolveDepth(wants, shallow, depth, relative)
	cp := repo.fetchPack(wants, haves, depth, shallow)
	newShallow, unshallow := cp.newShallow, cp.unshallow

	var b bytes.Buffer
	// acknowledgments: sent whenever the request carried a have
	// line. Each held have is ACKed, then "ready" closes the
	// negotiation; with no held have the server sends NAK. This
	// is what real git's server does, and the kit client parses
	// the section generically.
	if len(haves) > 0 {
		b.Write(pktLine("acknowledgments\n"))
		acked := false
		for _, h := range haves {
			if repo.object(h) != nil {
				b.Write(pktLine("ACK " + h + "\n"))
				acked = true
			}
		}
		if !acked {
			b.Write(pktLine("NAK\n"))
		} else {
			b.Write(pktLine("ready\n"))
		}
		b.Write(pktDelim)
	}
	// shallow-info: only when there are boundaries to report.
	// Real git sends no wanted-refs section, so neither do we.
	if len(newShallow) > 0 || len(unshallow) > 0 {
		b.Write(pktLine("shallow-info\n"))
		for _, s := range newShallow {
			b.Write(pktLine("shallow " + s + "\n"))
		}
		for _, s := range unshallow {
			b.Write(pktLine("unshallow " + s + "\n"))
		}
		b.Write(pktDelim)
	}
	// packfile, sideband-framed (an empty pack is still a pack).
	// The framing is cached with the pack, so a repeated fetch
	// writes it without rebuilding it.
	b.Write(pktLine("packfile\n"))
	b.Write(cp.framed)
	b.Write(pktFlush)
	_, _ = w.Write(b.Bytes())
}

func handleUploadPackV0(w http.ResponseWriter, repo *Repo, body []byte) {
	packets, err := decodePktLines(body)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "bad pkt-line stream")
		return
	}
	var wants, haves, shallow []string
	depth := 0
	relative := false
	for _, p := range packets {
		if p.kind != "data" {
			continue
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		// Capabilities ride after a NUL on the first want line.
		if i := strings.Index(line, "\x00"); i >= 0 {
			line = line[:i]
		}
		switch {
		case strings.HasPrefix(line, "want "):
			wants = append(wants, line[5:])
		case strings.HasPrefix(line, "have "):
			haves = append(haves, line[5:])
		case strings.HasPrefix(line, "shallow "):
			shallow = append(shallow, line[8:])
		case strings.HasPrefix(line, "deepen "):
			fmt.Sscanf(line[7:], "%d", &depth)
		case line == "deepen-relative":
			relative = true
		}
	}
	if len(wants) == 0 {
		_, _ = w.Write(pktFlush)
		return
	}
	depth = repo.resolveDepth(wants, shallow, depth, relative)
	cp := repo.fetchPack(wants, haves, depth, shallow)
	newShallow, unshallow := cp.newShallow, cp.unshallow

	var b bytes.Buffer
	acked := false
	for _, h := range haves {
		if repo.object(h) != nil {
			b.Write(pktLine("ACK " + h + "\n"))
			acked = true
		}
	}
	if !acked {
		b.Write(pktLine("NAK\n"))
	}
	for _, s := range newShallow {
		b.Write(pktLine("shallow " + s + "\n"))
	}
	for _, s := range unshallow {
		b.Write(pktLine("unshallow " + s + "\n"))
	}
	b.Write(cp.framed)
	b.Write(pktFlush)
	_, _ = w.Write(b.Bytes())
}

// ---------------------------------------------------------------- receive-pack

func handleReceivePack(w http.ResponseWriter, r *http.Request, repo *Repo) {
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	body, err := readBody(r)
	if err != nil {
		errJSON(w, http.StatusBadRequest, "cannot read request")
		return
	}
	// Split the command list (pkt-lines up to the first flush) from
	// the pack that follows it.
	packets, pack, err := splitReceivePack(body)
	if err != nil {
		// A malformed request must not kill the server; answer an
		// unpack error with no ref reports.
		writePushReport(w, "unpack error\n", nil)
		return
	}
	type cmd struct {
		old, new, ref string
	}
	var cmds []cmd
	for _, p := range packets {
		if p.kind != "data" {
			continue
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		if i := strings.Index(line, "\x00"); i >= 0 {
			line = line[:i]
		}
		parts := strings.SplitN(line, " ", 3)
		if len(parts) != 3 {
			continue
		}
		cmds = append(cmds, cmd{old: parts[0], new: parts[1], ref: parts[2]})
	}

	// Unpack first: every update needs its objects.
	var unpacked map[string]*Object
	unpackOK := true
	var unpackErr error
	if len(pack) > 0 {
		if !verifyPackChecksum(pack) {
			unpackOK = false
			unpackErr = fmt.Errorf("pack checksum mismatch")
		} else if objs, _, err := readPack(pack, nil); err != nil {
			unpackOK = false
			unpackErr = err
		} else {
			unpacked = objs
		}
	}
	if !unpackOK {
		fmt.Fprintf(os.Stderr, "receive-pack unpack failed: %v (pack %d bytes)\n", unpackErr, len(pack))
		var lines []string
		for _, c := range cmds {
			lines = append(lines, "ng "+c.ref+" unpacker error")
		}
		writePushReportLocked(w, "unpack error\n", lines)
		return
	}
	for oid, obj := range unpacked {
		repo.Objects[oid] = obj
	}

	var report strings.Builder
	report.WriteString("unpack ok\n")
	changed := false
	for _, c := range cmds {
		if c.new == zeroOID {
			// Delete.
			if _, ok := repo.Refs[c.ref]; ok {
				delete(repo.Refs, c.ref)
				changed = true
				report.WriteString("ok " + c.ref + "\n")
			} else {
				report.WriteString("ng " + c.ref + " no such ref\n")
			}
			continue
		}
		// Create or update: the new object must be present.
		if repo.object(c.new) == nil {
			report.WriteString("ng " + c.ref + " object not found\n")
			continue
		}
		current, exists := repo.Refs[c.ref]
		if !exists {
			if c.old != zeroOID {
				report.WriteString("ng " + c.ref + " non-fast-forward\n")
				continue
			}
			repo.Refs[c.ref] = c.new
			changed = true
			report.WriteString("ok " + c.ref + "\n")
			continue
		}
		if c.old != current {
			report.WriteString("ng " + c.ref + " non-fast-forward\n")
			continue
		}
		if !repo.isAncestor(current, c.new) {
			report.WriteString("ng " + c.ref + " non-fast-forward\n")
			continue
		}
		repo.Refs[c.ref] = c.new
		changed = true
		report.WriteString("ok " + c.ref + "\n")
	}
	if changed {
	}
	writePushReportLocked(w, report.String(), nil)
}

// splitReceivePack separates the command list from the pack bytes.
// The command list ends at the first flush packet.
func splitReceivePack(body []byte) ([]pkt, []byte, error) {
	pos := 0
	var packets []pkt
	for pos < len(body) {
		if len(body)-pos < 4 {
			return nil, nil, fmt.Errorf("truncated pkt-line")
		}
		var n int
		if _, err := fmt.Sscanf(string(body[pos:pos+4]), "%04x", &n); err != nil {
			return nil, nil, fmt.Errorf("bad pkt-line length")
		}
		if n == 0 {
			pos += 4
			return packets, body[pos:], nil
		}
		if n < 4 || len(body)-pos < n {
			return nil, nil, fmt.Errorf("truncated pkt-line body")
		}
		packets = append(packets, pkt{kind: "data", payload: body[pos+4 : pos+n]})
		pos += n
	}
	return nil, nil, fmt.Errorf("no flush packet")
}

// writePushReport writes a report-status stream inside sideband
// band 1. lines are extra report lines after the first line.
func writePushReport(w http.ResponseWriter, first string, lines []string) {
	var b strings.Builder
	b.WriteString(first)
	for _, l := range lines {
		b.WriteString(l)
		b.WriteString("\n")
	}
	// The report is itself pkt-lined inside band 1.
	var inner bytes.Buffer
	for _, line := range strings.SplitAfter(b.String(), "\n") {
		if line == "" {
			continue
		}
		inner.Write(pktLine(line))
	}
	inner.Write(pktFlush)
	_, _ = w.Write(append(sideband(inner.Bytes()), pktFlush...))
}

func writePushReportLocked(w http.ResponseWriter, report string, lines []string) {
	writePushReport(w, report, lines)
}
