// Git object layer: sha1 ids, object encoding, pack read/write, trees,
// commits, and the three-way merge the pull-request flow needs.
//
// Packs are plain (no deltas): the suite's client resolves thin packs and
// verifies byte-exact objects, and plain objects are the fastest thing the
// forge can both ingest and emit. Pack ingestion keeps a pool of zlib
// readers so repeated pushes never re-allocate decompressors.

package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"sort"
	"sync"
)

const zeroOID = "0000000000000000000000000000000000000000"

// oidOf returns the sha1 hex id of one object of the given type and payload.
func oidOf(typ byte, data []byte) string {
	var typeName string
	switch typ {
	case objCommit:
		typeName = "commit"
	case objTree:
		typeName = "tree"
	case objBlob:
		typeName = "blob"
	case objTag:
		typeName = "tag"
	default:
		typeName = "blob"
	}
	h := sha1.New()
	h.Write([]byte(typeName))
	h.Write([]byte{' '})
	h.Write([]byte(itoa(len(data))))
	h.Write([]byte{0})
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// oidBytes decodes a 40-hex oid into 20 raw bytes.
func oidBytes(oid string) []byte {
	b, _ := hex.DecodeString(oid)
	return b
}

// oidHex encodes 20 raw bytes into a 40-hex oid.
func oidHex(b []byte) string { return hex.EncodeToString(b) }

// storeObject inserts an object; returns its oid.
func (r *Repo) storeObject(typ byte, data []byte) string {
	oid := oidOf(typ, data)
	if _, ok := r.objects[oid]; !ok {
		r.objects[oid] = &Obj{typ: typ, data: append([]byte(nil), data...)}
	}
	return oid
}

// blobOID stores raw content as a blob.
func (r *Repo) blobOID(content []byte) string { return r.storeObject(objBlob, content) }

// TreeEntry is one entry of a git tree.
type TreeEntry struct {
	mode  string
	name  string
	oid   string
	isDir bool
}

// buildTree serializes sorted tree entries into a tree object and stores it.
func (r *Repo) buildTree(entries []TreeEntry) string {
	sort.Slice(entries, func(i, j int) bool {
		ni, nj := entries[i].name, entries[j].name
		if entries[i].isDir {
			ni += "/"
		}
		if entries[j].isDir {
			nj += "/"
		}
		return ni < nj
	})
	var buf bytes.Buffer
	for _, e := range entries {
		mode := e.mode
		if e.isDir {
			mode = "40000"
		}
		buf.WriteString(mode)
		buf.WriteByte(' ')
		buf.WriteString(e.name)
		buf.WriteByte(0)
		buf.Write(oidBytes(e.oid))
	}
	return r.storeObject(objTree, buf.Bytes())
}

// parseTree decodes a tree object's entries.
func parseTree(data []byte) []TreeEntry {
	var out []TreeEntry
	for len(data) > 0 {
		sp := bytes.IndexByte(data, ' ')
		if sp < 0 {
			return out
		}
		mode := string(data[:sp])
		data = data[sp+1:]
		nul := bytes.IndexByte(data, 0)
		if nul < 0 || len(data) < nul+21 {
			return out
		}
		name := string(data[:nul])
		oid := oidHex(data[nul+1 : nul+21])
		data = data[nul+21:]
		out = append(out, TreeEntry{mode: mode, name: name, oid: oid, isDir: mode == "40000"})
	}
	return out
}

// treeOf returns the tree entries of a tree oid, or nil.
func (r *Repo) treeOf(oid string) []TreeEntry {
	o, ok := r.objects[oid]
	if !ok || o.typ != objTree {
		return nil
	}
	return parseTree(o.data)
}

// commitInfo holds the parsed fields of a commit object.
type commitInfo struct {
	tree      string
	parents   []string
	author    string
	committer string
	message   string
}

// parseCommit decodes a commit object.
func parseCommit(data []byte) commitInfo {
	var c commitInfo
	head := data
	if i := bytes.Index(data, []byte("\n\n")); i >= 0 {
		head = data[:i]
		c.message = string(data[i+2:])
	}
	for _, line := range bytes.Split(head, []byte("\n")) {
		switch {
		case bytes.HasPrefix(line, []byte("tree ")):
			c.tree = string(line[5:])
		case bytes.HasPrefix(line, []byte("parent ")):
			c.parents = append(c.parents, string(line[7:]))
		case bytes.HasPrefix(line, []byte("author ")):
			c.author = string(line[7:])
		case bytes.HasPrefix(line, []byte("committer ")):
			c.committer = string(line[10:])
		}
	}
	return c
}

// commitOf returns the parsed commit for a commit oid, or false.
func (r *Repo) commitOf(oid string) (commitInfo, bool) {
	o, ok := r.objects[oid]
	if !ok || o.typ != objCommit {
		return commitInfo{}, false
	}
	return parseCommit(o.data), true
}

// makeCommit builds and stores a commit object.
func (r *Repo) makeCommit(tree string, parents []string, author, committer, message string) string {
	var buf bytes.Buffer
	buf.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		buf.WriteString("parent " + p + "\n")
	}
	buf.WriteString("author " + author + "\n")
	buf.WriteString("committer " + committer + "\n")
	buf.WriteString("\n")
	buf.WriteString(message)
	return r.storeObject(objCommit, buf.Bytes())
}

// flattenTree returns path -> (mode, oid, isDir) for every entry of a tree,
// recursively.
func (r *Repo) flattenTree(treeOID string, prefix string, out map[string]TreeEntry) {
	for _, e := range r.treeOf(treeOID) {
		path := prefix + e.name
		if e.isDir {
			r.flattenTree(e.oid, path+"/", out)
		} else {
			out[path] = e
		}
	}
}

// reachable returns the set of object oids reachable from the given tips
// (commits, trees, blobs, tags), walking parents and trees.
func (r *Repo) reachable(tips []string) map[string]bool {
	seen := map[string]bool{}
	var walkCommit func(oid string)
	var walkTree func(oid string)
	walkTree = func(oid string) {
		if seen[oid] {
			return
		}
		seen[oid] = true
		for _, e := range r.treeOf(oid) {
			if e.isDir {
				walkTree(e.oid)
			} else {
				seen[e.oid] = true
			}
		}
	}
	walkCommit = func(oid string) {
		if seen[oid] {
			return
		}
		seen[oid] = true
		c, ok := r.commitOf(oid)
		if !ok {
			return
		}
		walkTree(c.tree)
		for _, p := range c.parents {
			walkCommit(p)
		}
	}
	for _, tip := range tips {
		if tip == "" || tip == zeroOID {
			continue
		}
		o, ok := r.objects[tip]
		if !ok {
			continue
		}
		switch o.typ {
		case objCommit:
			walkCommit(tip)
		case objTree:
			walkTree(tip)
		case objTag:
			// annotated tag: follow its target line
			for _, line := range bytes.Split(o.data, []byte("\n")) {
				if bytes.HasPrefix(line, []byte("object ")) {
					target := string(line[7:])
					if t, ok := r.objects[target]; ok {
						if t.typ == objCommit {
							walkCommit(target)
						} else {
							seen[target] = true
						}
					}
				}
			}
			seen[tip] = true
		default:
			seen[tip] = true
		}
	}
	return seen
}

// reachableShallow returns the objects a shallow client holds for the
// given boundary commits: the commits themselves plus their trees and
// blobs — never their ancestors, which a shallow clone does not have.
func (r *Repo) reachableShallow(tips []string) map[string]bool {
	seen := map[string]bool{}
	var walkTree func(oid string)
	walkTree = func(oid string) {
		if seen[oid] {
			return
		}
		seen[oid] = true
		for _, e := range r.treeOf(oid) {
			if e.isDir {
				walkTree(e.oid)
			} else {
				seen[e.oid] = true
			}
		}
	}
	for _, tip := range tips {
		if tip == "" || tip == zeroOID {
			continue
		}
		o, ok := r.objects[tip]
		if !ok {
			continue
		}
		seen[tip] = true
		switch o.typ {
		case objCommit:
			if c, ok := r.commitOf(tip); ok {
				walkTree(c.tree)
			}
		case objTree:
			walkTree(tip)
		case objTag:
			for _, line := range bytes.Split(o.data, []byte("\n")) {
				if bytes.HasPrefix(line, []byte("object ")) {
					target := string(line[7:])
					if t, ok := r.objects[target]; ok && t.typ == objCommit {
						if c, ok := r.commitOf(target); ok {
							walkTree(c.tree)
						}
					} else {
						seen[target] = true
					}
				}
			}
		}
	}
	return seen
}

// reachableCut is reachable() with the parent links of cutSet commits cut:
// a shallow boundary commit contributes itself plus its tree and blobs, but
// never its ancestors — a shallow client does not hold those, even when it
// names the boundary as a have.
func (r *Repo) reachableCut(tips []string, cutSet map[string]bool) map[string]bool {
	seen := map[string]bool{}
	var walkCommit func(oid string)
	var walkTree func(oid string)
	walkTree = func(oid string) {
		if seen[oid] {
			return
		}
		seen[oid] = true
		for _, e := range r.treeOf(oid) {
			if e.isDir {
				walkTree(e.oid)
			} else {
				seen[e.oid] = true
			}
		}
	}
	walkCommit = func(oid string) {
		if seen[oid] {
			return
		}
		seen[oid] = true
		c, ok := r.commitOf(oid)
		if !ok {
			return
		}
		walkTree(c.tree)
		if cutSet[oid] {
			return
		}
		for _, p := range c.parents {
			walkCommit(p)
		}
	}
	for _, tip := range tips {
		if tip == "" || tip == zeroOID {
			continue
		}
		o, ok := r.objects[tip]
		if !ok {
			continue
		}
		switch o.typ {
		case objCommit:
			walkCommit(tip)
		case objTree:
			walkTree(tip)
		case objTag:
			for _, line := range bytes.Split(o.data, []byte("\n")) {
				if bytes.HasPrefix(line, []byte("object ")) {
					target := string(line[7:])
					if t, ok := r.objects[target]; ok {
						if t.typ == objCommit {
							walkCommit(target)
						} else {
							seen[target] = true
						}
					}
				}
			}
			seen[tip] = true
		default:
			seen[tip] = true
		}
	}
	return seen
}

// isAncestor reports whether anc is an ancestor of (or equal to) desc.
func (r *Repo) isAncestor(anc, desc string) bool {
	if anc == desc {
		return true
	}
	seen := map[string]bool{}
	stack := []string{desc}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur == anc {
			return true
		}
		if seen[cur] {
			continue
		}
		seen[cur] = true
		if c, ok := r.commitOf(cur); ok {
			stack = append(stack, c.parents...)
		}
	}
	return false
}

// mergeBase finds the first common ancestor of a and b (BFS from both sides).
func (r *Repo) mergeBase(a, b string) string {
	if a == "" || b == "" {
		return ""
	}
	ancA := map[string]bool{}
	stack := []string{a}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if ancA[cur] {
			continue
		}
		ancA[cur] = true
		if c, ok := r.commitOf(cur); ok {
			stack = append(stack, c.parents...)
		}
	}
	// BFS from b; first hit that is in ancA, preferring nearest to b.
	queue := []string{b}
	seen := map[string]bool{}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		if ancA[cur] {
			return cur
		}
		if c, ok := r.commitOf(cur); ok {
			queue = append(queue, c.parents...)
		}
	}
	return ""
}

// diffFiles computes the changed-file list between two trees (base -> tip),
// with line counts for blobs.
func (r *Repo) diffFiles(baseTree, tipTree string) []PRFile {
	base := map[string]TreeEntry{}
	tip := map[string]TreeEntry{}
	if baseTree != "" {
		r.flattenTree(baseTree, "", base)
	}
	if tipTree != "" {
		r.flattenTree(tipTree, "", tip)
	}
	var files []PRFile
	for path, te := range tip {
		be, ok := base[path]
		if !ok {
			files = append(files, PRFile{filename: path, status: "added", additions: countLines(r.blobBytes(te.oid)), deletions: 0, changes: countLines(r.blobBytes(te.oid))})
		} else if be.oid != te.oid {
			add, del := diffLineCounts(r.blobBytes(be.oid), r.blobBytes(te.oid))
			files = append(files, PRFile{filename: path, status: "modified", additions: add, deletions: del, changes: add + del})
		}
	}
	for path := range base {
		if _, ok := tip[path]; !ok {
			files = append(files, PRFile{filename: path, status: "removed", additions: 0, deletions: countLines(r.blobBytes(base[path].oid)), changes: countLines(r.blobBytes(base[path].oid))})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].filename < files[j].filename })
	return files
}

func (r *Repo) blobBytes(oid string) []byte {
	if o, ok := r.objects[oid]; ok && o.typ == objBlob {
		return o.data
	}
	return nil
}

func countLines(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	n := bytes.Count(b, []byte("\n"))
	if b[len(b)-1] != '\n' {
		n++
	}
	return n
}

// diffLineCounts is a cheap LCS-free approximation: lines added = lines in
// new not present in old (by exact line), lines removed likewise.
func diffLineCounts(oldB, newB []byte) (add, del int) {
	oldLines := map[string]int{}
	for _, l := range bytes.Split(oldB, []byte("\n")) {
		oldLines[string(l)]++
	}
	newLines := map[string]int{}
	for _, l := range bytes.Split(newB, []byte("\n")) {
		newLines[string(l)]++
	}
	for l, c := range newLines {
		if oldLines[l] < c {
			add += c - oldLines[l]
		}
	}
	for l, c := range oldLines {
		if newLines[l] < c {
			del += c - newLines[l]
		}
	}
	if add == 0 && del == 0 && !bytes.Equal(oldB, newB) {
		add = 1
	}
	return add, del
}

// prTips returns a pull request's current head and base tips from the live
// refs (a PR's stored shas go stale when another PR merges into its base).
func (r *Repo) prTips(p *PR) (headTip, baseTip string) {
	headTip = r.refs["refs/heads/"+p.headRef]
	if headTip == "" {
		headTip = p.headSha
	}
	baseTip = r.refs["refs/heads/"+p.baseRef]
	if baseTip == "" {
		baseTip = p.baseSha
	}
	return headTip, baseTip
}

// mergeTrees performs a three-way merge of base/head/other trees.
// Returns the merged tree oid, and false when a file conflicts.
func (r *Repo) mergeTrees(baseTree, headTree, otherTree string) (string, bool) {
	base := map[string]TreeEntry{}
	head := map[string]TreeEntry{}
	other := map[string]TreeEntry{}
	if baseTree != "" {
		r.flattenTree(baseTree, "", base)
	}
	if headTree != "" {
		r.flattenTree(headTree, "", head)
	}
	if otherTree != "" {
		r.flattenTree(otherTree, "", other)
	}
	merged := map[string]TreeEntry{}
	paths := map[string]bool{}
	for p := range base {
		paths[p] = true
	}
	for p := range head {
		paths[p] = true
	}
	for p := range other {
		paths[p] = true
	}
	for path := range paths {
		b, inB := base[path]
		h, inH := head[path]
		o, inO := other[path]
		switch {
		case inH && inO && h.oid == o.oid:
			merged[path] = h
		case inB && inH && inO && b.oid == h.oid:
			merged[path] = o
		case inB && inH && inO && b.oid == o.oid:
			merged[path] = h
		case !inB && inH && !inO:
			merged[path] = h
		case !inB && !inH && inO:
			merged[path] = o
		case inB && !inH && !inO:
			// deleted on both sides
		case inB && inH && !inO && b.oid == h.oid:
			// deleted on other, unchanged on head
		case inB && !inH && inO && b.oid == o.oid:
			// deleted on head, unchanged on other
		case !inB && inH && inO && h.oid != o.oid:
			return "", false
		case inB && inH && inO && b.oid != h.oid && b.oid != o.oid && h.oid != o.oid:
			return "", false
		case inB && !inH && inO && b.oid != o.oid:
			return "", false
		case inB && inH && !inO && b.oid != h.oid:
			return "", false
		default:
			// both deleted or one side deleted while other changed
			if inH && inO {
				if h.oid == o.oid {
					merged[path] = h
				} else {
					return "", false
				}
			}
		}
	}
	return r.nestTree(merged), true
}

// nestTree rebuilds a nested tree object from a flat path map.
func (r *Repo) nestTree(flat map[string]TreeEntry) string {
	return r.nestTreePrefix(flat, "")
}

func (r *Repo) nestTreePrefix(flat map[string]TreeEntry, prefix string) string {
	dirs := map[string]map[string]TreeEntry{}
	var entries []TreeEntry
	for path, e := range flat {
		rel := path[len(prefix):]
		if i := indexOf(rel, "/"); i >= 0 {
			dir := rel[:i]
			sub, ok := dirs[dir]
			if !ok {
				sub = map[string]TreeEntry{}
				dirs[dir] = sub
			}
			sub[path] = e
		} else {
			entries = append(entries, e)
		}
	}
	for dir, sub := range dirs {
		subOID := r.nestTreePrefix(sub, prefix+dir+"/")
		entries = append(entries, TreeEntry{mode: "40000", name: dir, oid: subOID, isDir: true})
	}
	return r.buildTree(entries)
}

// ---- pack read / write ----

// readPack parses a version-2 pack into objects (plain + delta-resolved).
// Returns oid -> Obj for every entry.
func readPack(pack []byte) (map[string]*Obj, error) {
	if len(pack) < 12 || string(pack[:4]) != "PACK" {
		return nil, errors.New("bad pack header")
	}
	if binary.BigEndian.Uint32(pack[4:8]) != 2 {
		return nil, errors.New("bad pack version")
	}
	count := int(binary.BigEndian.Uint32(pack[8:12]))
	pos := 12
	out := make(map[string]*Obj, count)
	hasDelta := false
	for i := 0; i < count; i++ {
		typ, size, n, err := readPackHeader(pack, pos)
		if err != nil {
			return nil, err
		}
		pos += n
		switch typ {
		case objCommit, objTree, objBlob, objTag:
			data, used, err := inflateAt(pack, pos, size)
			if err != nil {
				return nil, err
			}
			pos += used
			oid := oidOf(byte(typ), data)
			out[oid] = &Obj{typ: byte(typ), data: data}
		case 6: // OFS_DELTA
			hasDelta = true
			_, m, err := readVarint(pack, pos)
			if err != nil {
				return nil, err
			}
			pos += m
			_, used, err := inflateAt(pack, pos, size)
			if err != nil {
				return nil, err
			}
			pos += used
		case 7: // REF_DELTA
			hasDelta = true
			if len(pack) < pos+20 {
				return nil, errors.New("truncated ref delta")
			}
			pos += 20
			_, used, err := inflateAt(pack, pos, size)
			if err != nil {
				return nil, err
			}
			pos += used
		default:
			return nil, errors.New("unknown pack entry type")
		}
	}
	if hasDelta {
		if err := resolveDeltas(pack, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// resolveDeltas resolves OFS and REF deltas in a pack using the plain
// objects already parsed plus previously resolved deltas.
func resolveDeltas(pack []byte, out map[string]*Obj) error {
	count := int(binary.BigEndian.Uint32(pack[8:12]))
	type entry struct {
		start int
		typ   int
		size  int64
		base  string
		data  []byte
	}
	entries := make([]entry, 0, count)
	pos := 12
	for i := 0; i < count; i++ {
		start := pos
		typ, size, n, err := readPackHeader(pack, pos)
		if err != nil {
			return err
		}
		pos += n
		e := entry{start: start, typ: typ, size: size}
		if typ == 6 {
			off, m, err := readVarint(pack, pos)
			if err != nil {
				return err
			}
			pos += m
			e.base = "ofs:" + itoa(start-int(off))
		} else if typ == 7 {
			e.base = oidHex(pack[pos : pos+20])
			pos += 20
		}
		data, used, err := inflateAt(pack, pos, size)
		if err != nil {
			return err
		}
		pos += used
		e.data = data
		entries = append(entries, e)
	}
	byOffset := map[int]string{}
	// First pass: plain objects are already in out; map their offsets.
	for _, e := range entries {
		if e.typ <= 4 {
			byOffset[e.start] = oidOf(byte(e.typ), e.data)
		}
	}
	// Iteratively resolve deltas (a delta's base may itself be a delta).
	pending := []entry{}
	for _, e := range entries {
		if e.typ == 6 || e.typ == 7 {
			pending = append(pending, e)
		}
	}
	for len(pending) > 0 {
		progress := false
		rest := pending[:0]
		for _, e := range pending {
			baseOID := e.base
			if len(baseOID) > 4 && baseOID[:4] == "ofs:" {
				off, _ := strconvAtoi(baseOID[4:])
				baseOID = byOffset[off]
			}
			base, ok := out[baseOID]
			if !ok {
				rest = append(rest, e)
				continue
			}
			resolved, err := applyDelta(base.data, e.data)
			if err != nil {
				return err
			}
			oid := oidOf(base.typ, resolved)
			out[oid] = &Obj{typ: base.typ, data: resolved}
			byOffset[e.start] = oid
			progress = true
		}
		pending = rest
		if !progress {
			return errors.New("unresolvable pack delta")
		}
	}
	return nil
}

func strconvAtoi(s string) (int, error) {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, errors.New("bad number")
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, nil
}

// readPackHeader decodes a pack object header: type, size, bytes consumed.
func readPackHeader(pack []byte, pos int) (typ int, size int64, n int, err error) {
	if pos >= len(pack) {
		return 0, 0, 0, errors.New("truncated pack header")
	}
	b := pack[pos]
	typ = int((b >> 4) & 7)
	size = int64(b & 0x0f)
	shift := uint(4)
	n = 1
	for b&0x80 != 0 {
		if pos+n >= len(pack) {
			return 0, 0, 0, errors.New("truncated pack header")
		}
		b = pack[pos+n]
		size |= int64(b&0x7f) << shift
		shift += 7
		n++
	}
	return typ, size, n, nil
}

// readVarint decodes the OFS_DELTA offset varint.
func readVarint(pack []byte, pos int) (uint64, int, error) {
	if pos >= len(pack) {
		return 0, 0, errors.New("truncated varint")
	}
	b := pack[pos]
	v := uint64(b & 0x7f)
	n := 1
	for b&0x80 != 0 {
		if pos+n >= len(pack) {
			return 0, 0, errors.New("truncated varint")
		}
		b = pack[pos+n]
		v = ((v + 1) << 7) | uint64(b&0x7f)
		n++
	}
	return v, n, nil
}

// inflateAt decompresses one zlib stream starting at pos; returns the
// decompressed bytes and the exact number of input bytes consumed.
//
// The stream length is measured with a counting reader that also implements
// io.ByteReader, so the flate decoder reads byte-by-byte through it and
// never reads past the end of the stream. Decompressors are pooled and
// reset between uses.
func inflateAt(pack []byte, pos int, sizeHint int64) ([]byte, int, error) {
	cr := &countReader{r: bytes.NewReader(pack[pos:])}
	var zr io.ReadCloser
	if v := zlibReaderPool.Get(); v != nil {
		zr = v.(io.ReadCloser)
		if err := zr.(zlib.Resetter).Reset(cr, nil); err != nil {
			zlibReaderPool.Put(zr)
			var err2 error
			zr, err2 = zlib.NewReader(cr)
			if err2 != nil {
				return nil, 0, err2
			}
		}
	} else {
		var err error
		zr, err = zlib.NewReader(cr)
		if err != nil {
			return nil, 0, err
		}
	}
	capacity := int64(0)
	if sizeHint > 0 && sizeHint < 1<<26 {
		capacity = sizeHint
	}
	buf := bytes.NewBuffer(make([]byte, 0, capacity))
	_, err := io.Copy(buf, zr)
	zr.Close()
	zlibReaderPool.Put(zr)
	if err != nil {
		return nil, 0, err
	}
	return buf.Bytes(), cr.n, nil
}

var zlibReaderPool = sync.Pool{
	New: func() any { return nil },
}

type countReader struct {
	r *bytes.Reader
	n int
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func (c *countReader) ReadByte() (byte, error) {
	b, err := c.r.ReadByte()
	if err == nil {
		c.n++
	}
	return b, err
}

// applyDelta applies a git delta stream to base bytes.
func applyDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	// Git delta varint decode (LSB-first, MSB continuation).
	readVar := func() (uint64, error) {
		var v uint64
		shift := uint(0)
		for {
			if pos >= len(delta) {
				return 0, errors.New("truncated delta")
			}
			b := delta[pos]
			pos++
			v |= uint64(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				return v, nil
			}
		}
	}
	srcSize, err := readVar()
	if err != nil {
		return nil, err
	}
	if int(srcSize) != len(base) {
		return nil, errors.New("delta base size mismatch")
	}
	dstSize, err := readVar()
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.Grow(int(dstSize))
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if op&0x80 != 0 {
			// copy from base
			var off, size uint64
			for i := 0; i < 4; i++ {
				if op&(1<<uint(i)) != 0 {
					if pos >= len(delta) {
						return nil, errors.New("truncated delta copy")
					}
					off |= uint64(delta[pos]) << (8 * uint(i))
					pos++
				}
			}
			for i := 0; i < 3; i++ {
				if op&(0x10<<uint(i)) != 0 {
					if pos >= len(delta) {
						return nil, errors.New("truncated delta copy")
					}
					size |= uint64(delta[pos]) << (8 * uint(i))
					pos++
				}
			}
			if size == 0 {
				size = 0x10000
			}
			if int(off+size) > len(base) {
				return nil, errors.New("delta copy out of range")
			}
			out.Write(base[off : off+size])
		} else if op != 0 {
			n := int(op)
			if pos+n > len(delta) {
				return nil, errors.New("truncated delta insert")
			}
			out.Write(delta[pos : pos+n])
			pos += n
		} else {
			return nil, errors.New("delta op 0")
		}
	}
	if uint64(out.Len()) != dstSize {
		return nil, errors.New("delta size mismatch")
	}
	return out.Bytes(), nil
}

// writePack serializes objects as a plain (delta-free) version-2 pack.
func writePack(objs []*Obj) []byte {
	var buf bytes.Buffer
	buf.WriteString("PACK")
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], 2)
	binary.BigEndian.PutUint32(hdr[4:8], uint32(len(objs)))
	buf.Write(hdr[:])
	for _, o := range objs {
		writePackEntry(&buf, o.typ, o.data)
	}
	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes()
}

// writePackEntry writes one plain object entry (type/size header + zlib).
// The zlib stream uses level 0 (stored, no compression): a clone pack is
// built once and inflated by the client once, and for the small objects
// here the deflate search costs ~20x the memcpy it would save. The pack
// stays a valid version-2 pack either way.
func writePackEntry(buf *bytes.Buffer, typ byte, data []byte) {
	size := len(data)
	// first byte: (size & 0x0f) | (typ << 4), continuation if size >= 16
	first := byte((size & 0x0f) | (int(typ) << 4))
	size >>= 4
	if size > 0 {
		first |= 0x80
	}
	buf.WriteByte(first)
	for size > 0 {
		b := byte(size & 0x7f)
		size >>= 7
		if size > 0 {
			b |= 0x80
		}
		buf.WriteByte(b)
	}
	zw := zlibWriterPool.Get()
	if zw == nil {
		zw2, _ := zlib.NewWriterLevel(buf, 0)
		zw = zw2
	} else {
		zw.(*zlib.Writer).Reset(buf)
	}
	w := zw.(*zlib.Writer)
	w.Write(data)
	w.Close()
	zlibWriterPool.Put(w)
}

var zlibWriterPool = sync.Pool{
	New: func() any {
		w, _ := zlib.NewWriterLevel(nil, 0)
		return w
	},
}

// packObjects returns the pack bytes for the given oids in deterministic order.
func (r *Repo) packObjects(oids []string) []byte {
	sort.Strings(oids)
	objs := make([]*Obj, 0, len(oids))
	for _, oid := range oids {
		if o, ok := r.objects[oid]; ok {
			objs = append(objs, o)
		}
	}
	return writePack(objs)
}
