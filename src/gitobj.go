// Git object store: packfile read and write, delta application,
// and the object collection a fetch sends.
//
// Packs carry full objects only (no deltas are ever emitted); the
// reader resolves OFS_DELTA and REF_DELTA so a client that sends
// deltas still lands. zlib streams are read through a *bytes.Reader,
// which implements io.ByteReader, so the decompressor never reads
// past the end of a stream and the next object's offset is exact.
package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"io"
	"sort"
)

// packObject is one object to write into a packfile.
type packObject struct {
	Type string
	Data []byte
}

// writePack builds a packfile version 2 from plain objects (no
// deltas), in the order given, with a trailing SHA-1 checksum.
func writePack(objects []packObject) []byte {
	var buf bytes.Buffer
	buf.WriteString("PACK")
	_ = binary.Write(&buf, binary.BigEndian, uint32(2))
	_ = binary.Write(&buf, binary.BigEndian, uint32(len(objects)))
	for _, o := range objects {
		buf.Write(encodePackObjectHeader(objTypeCode(o.Type), len(o.Data)))
		zw, _ := zlib.NewWriterLevel(&buf, zlib.BestSpeed)
		_, _ = zw.Write(o.Data)
		_ = zw.Close()
	}
	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes()
}

func objTypeCode(typ string) int {
	switch typ {
	case "commit":
		return 1
	case "tree":
		return 2
	case "blob":
		return 3
	case "tag":
		return 4
	}
	return 0
}

func objTypeName(code int) string {
	switch code {
	case 1:
		return "commit"
	case 2:
		return "tree"
	case 3:
		return "blob"
	case 4:
		return "tag"
	}
	return ""
}

// encodePackObjectHeader writes the varint type+size header.
func encodePackObjectHeader(typ, size int) []byte {
	var out []byte
	first := byte((typ << 4) | (size & 0x0f))
	size >>= 4
	if size == 0 {
		return []byte{first}
	}
	out = append(out, first|0x80)
	for size > 0 {
		b := byte(size & 0x7f)
		size >>= 7
		if size > 0 {
			b |= 0x80
		}
		out = append(out, b)
	}
	return out
}

// verifyPackChecksum checks a packfile's trailing checksum.
func verifyPackChecksum(pack []byte) bool {
	if len(pack) < 20 {
		return false
	}
	sum := sha1.Sum(pack[:len(pack)-20])
	return bytes.Equal(sum[:], pack[len(pack)-20:])
}

// readPack parses a packfile, resolving deltas, and returns its
// objects by id. haveObjects supplies bases for thin packs.
func readPack(pack []byte, haveObjects map[string]*Object) (map[string]*Object, int, error) {
	out := make(map[string]*Object)
	if len(pack) < 12 || string(pack[:4]) != "PACK" {
		return nil, 0, fmt.Errorf("bad pack header")
	}
	version := binary.BigEndian.Uint32(pack[4:8])
	if version != 2 && version != 3 {
		return nil, 0, fmt.Errorf("unsupported pack version %d", version)
	}
	count := int(binary.BigEndian.Uint32(pack[8:12]))
	// First pass: read raw objects, remembering delta bases.
	type rawObj struct {
		typ    int
		data   []byte
		ofs    int64 // OFS_DELTA base offset, -1 when none
		refOID string
	}
	raws := make([]rawObj, 0, count)
	offsets := make([]int64, 0, count)
	pos := int64(12)
	deltaCount := 0
	for i := 0; i < count; i++ {
		start := pos
		// Varint type+size.
		b := pack[pos]
		pos++
		typ := int((b >> 4) & 0x7)
		size := int64(b & 0x0f)
		shift := uint(4)
		for b&0x80 != 0 {
			b = pack[pos]
			pos++
			size |= int64(b&0x7f) << shift
			shift += 7
		}
		var ro rawObj
		ro.typ = typ
		if typ == 6 { // OFS_DELTA
			b = pack[pos]
			pos++
			off := int64(b & 0x7f)
			for b&0x80 != 0 {
				b = pack[pos]
				pos++
				off = ((off + 1) << 7) | int64(b&0x7f)
			}
			ro.ofs = start - off
			deltaCount++
		} else if typ == 7 { // REF_DELTA
			ro.refOID = fmt.Sprintf("%x", pack[pos:pos+20])
			pos += 20
			deltaCount++
		}
		// Deflate the object body. The reader is a *bytes.Reader,
		// which implements io.ByteReader, so the zlib reader stops
		// exactly at the stream's end and the reader's remaining
		// length tells us how many compressed bytes were consumed.
		br := bytes.NewReader(pack[pos:])
		zr, err := zlib.NewReader(br)
		if err != nil {
			return nil, 0, fmt.Errorf("zlib open: %w", err)
		}
		data := make([]byte, size)
		if _, err := readFull(zr, data); err != nil {
			_ = zr.Close()
			return nil, 0, fmt.Errorf("zlib read: %w", err)
		}
		_ = zr.Close()
		pos += int64(len(pack[pos:]) - br.Len())
		ro.data = data
		raws = append(raws, ro)
		offsets = append(offsets, start)
	}
	// Resolve: map offset -> index for OFS_DELTA bases.
	byOffset := make(map[int64]int, len(offsets))
	for i, off := range offsets {
		byOffset[off] = i
	}
	// Recursive delta resolution with memoization.
	resolved := make([]*Object, len(raws))
	var resolve func(i int) (*Object, error)
	resolve = func(i int) (*Object, error) {
		if resolved[i] != nil {
			return resolved[i], nil
		}
		ro := raws[i]
		var obj *Object
		switch ro.typ {
		case 1, 2, 3, 4:
			obj = &Object{Type: objTypeName(ro.typ), Data: ro.data}
		case 6:
			baseIdx, ok := byOffset[ro.ofs]
			if !ok {
				return nil, fmt.Errorf("ofs-delta base offset %d not found", ro.ofs)
			}
			base, err := resolve(baseIdx)
			if err != nil {
				return nil, err
			}
			data, err := applyDelta(base.Data, ro.data)
			if err != nil {
				return nil, err
			}
			obj = &Object{Type: base.Type, Data: data}
		case 7:
			var base *Object
			if o, ok := out[ro.refOID]; ok {
				base = o
			} else if o, ok := haveObjects[ro.refOID]; ok {
				base = o
			} else {
				// The base may itself be in this pack; find it.
				for j, r2 := range raws {
					if r2.typ < 6 && oidOf(objTypeName(r2.typ), r2.data) == ro.refOID {
						b, err := resolve(j)
						if err != nil {
							return nil, err
						}
						base = b
						break
					}
				}
			}
			if base == nil {
				return nil, fmt.Errorf("ref-delta base %s not found", ro.refOID)
			}
			data, err := applyDelta(base.Data, ro.data)
			if err != nil {
				return nil, err
			}
			obj = &Object{Type: base.Type, Data: data}
		default:
			return nil, fmt.Errorf("unknown pack object type %d", ro.typ)
		}
		resolved[i] = obj
		out[oidOf(obj.Type, obj.Data)] = obj
		return obj, nil
	}
	for i := range raws {
		if _, err := resolve(i); err != nil {
			return nil, 0, err
		}
	}
	return out, deltaCount, nil
}

// readFull reads exactly len(b) bytes. (A reader may return its
// last bytes together with io.EOF; that is not an error here.)
func readFull(r io.Reader, b []byte) (int, error) {
	return io.ReadFull(r, b)
}

// applyDelta applies a git delta to a base, returning the result.
func applyDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	readVarint := func() (int64, error) {
		var v int64
		shift := uint(0)
		for {
			if pos >= len(delta) {
				return 0, fmt.Errorf("delta truncated")
			}
			b := delta[pos]
			pos++
			v |= int64(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				return v, nil
			}
		}
	}
	srcSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	if srcSize != int64(len(base)) {
		return nil, fmt.Errorf("delta base size %d, have %d", srcSize, len(base))
	}
	tgtSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	var out []byte
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if op&0x80 != 0 {
			// Copy from base: offset and size in following bytes.
			var off, size int64
			for i := 0; i < 4; i++ {
				if op&(1<<uint(i)) != 0 {
					if pos >= len(delta) {
						return nil, fmt.Errorf("delta copy truncated")
					}
					off |= int64(delta[pos]) << (8 * uint(i))
					pos++
				}
			}
			for i := 0; i < 3; i++ {
				if op&(0x10<<uint(i)) != 0 {
					if pos >= len(delta) {
						return nil, fmt.Errorf("delta copy truncated")
					}
					size |= int64(delta[pos]) << (8 * uint(i))
					pos++
				}
			}
			if size == 0 {
				size = 0x10000
			}
			if off+size > int64(len(base)) {
				return nil, fmt.Errorf("delta copy out of range")
			}
			out = append(out, base[off:off+size]...)
		} else if op != 0 {
			// Insert literal bytes.
			n := int(op)
			if pos+n > len(delta) {
				return nil, fmt.Errorf("delta insert truncated")
			}
			out = append(out, delta[pos:pos+n]...)
			pos += n
		} else {
			return nil, fmt.Errorf("delta opcode 0")
		}
	}
	if int64(len(out)) != tgtSize {
		return nil, fmt.Errorf("delta result size %d, expected %d", len(out), tgtSize)
	}
	return out, nil
}

// ---------------------------------------------------------------- fetch collection

// collectFetch computes the objects a fetch sends.
//
// wants are the wanted oids; haves are oids the client already
// holds; depth limits generations from each want (0 = unlimited);
// shallow are the client's current shallow boundaries (for deepen).
// It returns the objects to pack (sorted by oid), the new shallow
// boundaries, and the boundaries that stop being shallow.
// resolveDepth converts a fetch's depth to the absolute depth
// collectFetch expects (a want is generation 1). A plain
// "deepen N" is already absolute; with "deepen-relative" the N
// generations count from the client's current shallow boundary,
// so that boundary's generation from the wants is added.
func (r *Repo) resolveDepth(wants, shallow []string, depth int, relative bool) int {
	if depth <= 0 || !relative {
		return depth
	}
	gen := 0
	for _, s := range shallow {
		if g := r.generationOf(wants, s); g > 0 && (gen == 0 || g < gen) {
			gen = g
		}
	}
	if gen == 0 {
		return depth
	}
	return gen + depth
}

// generationOf returns the 1-based generation of target from the
// wants (a want is generation 1), or 0 if it is not reachable.
func (r *Repo) generationOf(wants []string, target string) int {
	type item struct {
		oid string
		gen int
	}
	seen := map[string]bool{}
	queue := []item{}
	for _, w := range wants {
		queue = append(queue, item{w, 1})
	}
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		if seen[it.oid] {
			continue
		}
		seen[it.oid] = true
		if it.oid == target {
			return it.gen
		}
		obj := r.Objects[it.oid]
		if obj == nil || obj.Type != "commit" {
			continue
		}
		for _, p := range parseCommit(obj.Data).Parents {
			if !seen[p] {
				queue = append(queue, item{p, it.gen + 1})
			}
		}
	}
	return 0
}

func (r *Repo) collectFetch(wants, haves []string, depth int, shallow []string) ([]packObject, []string, []string) {
	// held: objects the client already has, so the pack must not
	// re-send them.
	// - Every have commit: the commit itself and its whole tree
	//   closure (trees and blobs).
	// - With no depth limit, also every ancestor of a have and
	//   their tree closures: a full clone holds that history.
	// - The client's shallow boundaries: the commit and its tree
	//   closure, but NOT its ancestors: a deepen fetch asks for
	//   exactly those.
	held := map[string]bool{}
	addTreeClosure := func(root string) {
		var walk func(oid string)
		walk = func(oid string) {
			if oid == zeroOID || held[oid] {
				return
			}
			obj := r.Objects[oid]
			if obj == nil {
				return
			}
			held[oid] = true
			if obj.Type != "tree" {
				return
			}
			for _, e := range parseTree(obj.Data) {
				walk(e.OID)
			}
		}
		walk(root)
	}
	for _, h := range haves {
		obj := r.Objects[h]
		if obj == nil {
			continue
		}
		held[h] = true
		if obj.Type == "commit" {
			addTreeClosure(parseCommit(obj.Data).Tree)
			if depth == 0 {
				for c := range r.reachableCommits(h) {
					if c == h {
						continue
					}
					held[c] = true
					if co := r.Objects[c]; co != nil && co.Type == "commit" {
						addTreeClosure(parseCommit(co.Data).Tree)
					}
				}
			}
		}
	}
	for _, s := range shallow {
		obj := r.Objects[s]
		if obj == nil {
			continue
		}
		held[s] = true
		if obj.Type == "commit" {
			addTreeClosure(parseCommit(obj.Data).Tree)
		}
	}

	// Walk from the wants, generation by generation (want = 1).
	// A held commit is not sent, but its parents are still walked:
	// a deepen fetch's want is held and its parent is the prize.
	// A commit at the depth boundary is sent and marked shallow,
	// and its parents are not walked.
	type item struct {
		oid string
		gen int
	}
	var sendCommits []string
	seen := map[string]bool{}
	queue := []item{}
	for _, w := range wants {
		queue = append(queue, item{oid: w, gen: 1})
	}
	newShallowSet := map[string]bool{}
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		if seen[it.oid] {
			continue
		}
		seen[it.oid] = true
		obj := r.Objects[it.oid]
		if obj == nil {
			continue
		}
		if obj.Type == "tag" {
			// A tag want: send the tag and walk its target.
			if !held[it.oid] {
				sendCommits = append(sendCommits, it.oid)
			}
			target := parseTagTarget(obj.Data)
			if target != "" && !seen[target] {
				queue = append(queue, item{oid: target, gen: it.gen})
			}
			continue
		}
		if obj.Type != "commit" {
			continue
		}
		if depth > 0 && it.gen > depth {
			continue
		}
		if !held[it.oid] {
			sendCommits = append(sendCommits, it.oid)
			if depth > 0 && it.gen == depth {
				newShallowSet[it.oid] = true
				continue
			}
		}
		if depth > 0 && it.gen >= depth {
			continue
		}
		for _, p := range parseCommit(obj.Data).Parents {
			if !seen[p] {
				queue = append(queue, item{oid: p, gen: it.gen + 1})
			}
		}
	}

	// Objects to send: the commits, plus every tree and blob they
	// reach that the client does not already hold. A held subtree
	// is skipped whole: its contents are held too.
	send := map[string]packObject{}
	addObj := func(oid string) {
		if oid == zeroOID || held[oid] {
			return
		}
		o := r.Objects[oid]
		if o == nil {
			return
		}
		if _, ok := send[oid]; ok {
			return
		}
		send[oid] = packObject{Type: o.Type, Data: o.Data}
	}
	for _, c := range sendCommits {
		obj := r.Objects[c]
		if obj == nil {
			continue
		}
		addObj(c)
		if obj.Type == "tag" {
			addObj(parseTagTarget(obj.Data))
			continue
		}
		// Walk the commit's tree.
		var walkTree func(oid string)
		walkTree = func(oid string) {
			if held[oid] {
				return
			}
			addObj(oid)
			t := r.Objects[oid]
			if t == nil || t.Type != "tree" {
				return
			}
			for _, e := range parseTree(t.Data) {
				if e.Mode == "40000" {
					walkTree(e.OID)
				} else {
					addObj(e.OID)
				}
			}
		}
		walkTree(parseCommit(obj.Data).Tree)
	}

	// Sort objects by oid for a deterministic pack.
	oids := make([]string, 0, len(send))
	for oid := range send {
		oids = append(oids, oid)
	}
	sort.Strings(oids)
	objects := make([]packObject, 0, len(oids))
	for _, oid := range oids {
		objects = append(objects, send[oid])
	}

	var newShallow []string
	for oid := range newShallowSet {
		newShallow = append(newShallow, oid)
	}
	sort.Strings(newShallow)

	// Unshallow: old boundaries that are now interior (reachable
	// within the new depth, i.e. sent and not boundaries).
	var unshallow []string
	if depth > 0 {
		for _, s := range shallow {
			if seen[s] && !newShallowSet[s] {
				unshallow = append(unshallow, s)
			}
		}
		sort.Strings(unshallow)
	}
	return objects, newShallow, unshallow
}

// parseTagTarget returns the object a tag points at.
func parseTagTarget(data []byte) string {
	for _, line := range bytes.Split(data, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("object ")) {
			return string(line[7:])
		}
		if len(line) == 0 {
			break
		}
	}
	return ""
}

// parseTagPeel returns the commit a tag peels to, following tags.
func (r *Repo) parseTagPeel(oid string) string {
	seen := 0
	for seen < 8 {
		obj := r.Objects[oid]
		if obj == nil || obj.Type != "tag" {
			return oid
		}
		oid = parseTagTarget(obj.Data)
		seen++
	}
	return oid
}
