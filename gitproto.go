// Git smart-HTTP protocol: pkt-line codec, advertisements, upload-pack
// (v2 and v0) and receive-pack (v0).
//
// The clone workload is fifty identical concurrent fetches, so a fetch
// response is cached per (repo generation, wants, haves, shallows, depth):
// the first clone builds the pack, the other forty-nine copy bytes.

package main

import (
	"bytes"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// ---- pkt-line codec ----

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

func pktLine(s string) []byte {
	return pktLineBytes([]byte(s))
}

func pktLineBytes(payload []byte) []byte {
	n := len(payload) + 4
	out := make([]byte, 0, n)
	out = append(out, hexDigit(n>>12), hexDigit(n>>8), hexDigit(n>>4), hexDigit(n))
	out = append(out, payload...)
	return out
}

func hexDigit(v int) byte {
	v &= 0xf
	if v < 10 {
		return byte('0' + v)
	}
	return byte('a' + v - 10)
}

type pkt struct {
	kind    string // "data", "flush", "delim", "response-end"
	payload []byte
}

// parsePktLines splits a pkt-line stream into packets.
func parsePktLines(body []byte) []pkt {
	var out []pkt
	for len(body) >= 4 {
		if string(body[:4]) == "0000" {
			out = append(out, pkt{kind: "flush"})
			body = body[4:]
			continue
		}
		if string(body[:4]) == "0001" {
			out = append(out, pkt{kind: "delim"})
			body = body[4:]
			continue
		}
		if string(body[:4]) == "0002" {
			out = append(out, pkt{kind: "response-end"})
			body = body[4:]
			continue
		}
		n, err := strconv.ParseInt(string(body[:4]), 16, 32)
		if err != nil || n < 4 || int(n) > len(body) {
			return out
		}
		out = append(out, pkt{kind: "data", payload: body[4:n]})
		body = body[n:]
	}
	return out
}

// sideband wraps raw bytes as band-1 sideband pkt-lines (max 65520 total).
func sideband(data []byte) []byte {
	var out []byte
	const maxPayload = 65520 - 4 - 1
	for len(data) > 0 {
		chunk := maxPayload
		if len(data) < chunk {
			chunk = len(data)
		}
		payload := make([]byte, 0, chunk+1)
		payload = append(payload, 1)
		payload = append(payload, data[:chunk]...)
		out = append(out, pktLineBytes(payload)...)
		data = data[chunk:]
	}
	return out
}

// ---- advertisements ----

const uploadV0Caps = "multi_ack thin-pack side-band side-band-64k ofs-delta shallow deepen-since deepen-not deepen-relative no-progress include-tag multi_ack_detailed symref=HEAD:refs/heads/main object-format=sha1 agent=forge/1.0"
const receiveV0Caps = "report-status side-band-64k delete-refs ofs-delta object-format=sha1 agent=forge/1.0"

// refLine renders one v0 advertisement ref line.
func refLine(oid, name string, first bool, caps string) []byte {
	var b bytes.Buffer
	b.WriteString(oid)
	b.WriteByte(' ')
	b.WriteString(name)
	if first {
		b.WriteByte(0)
		b.WriteString(caps)
	}
	b.WriteByte('\n')
	return pktLineBytes(b.Bytes())
}

// advertiseUploadV0 builds the v0 upload-pack advertisement.
func advertiseUploadV0(r *Repo) []byte {
	var out []byte
	out = append(out, pktLine("# service=git-upload-pack\n")...)
	out = append(out, pktFlush...)
	names := r.sortedRefNames()
	if len(names) == 0 {
		out = append(out, refLine(zeroOID, "capabilities^{}", true, uploadV0Caps)...)
	} else {
		for i, name := range names {
			out = append(out, refLine(r.refs[name], name, i == 0, uploadV0Caps)...)
		}
		// peeled lines for annotated tags
		for _, name := range names {
			if !strings.HasPrefix(name, "refs/tags/") {
				continue
			}
			if o, ok := r.objects[r.refs[name]]; ok && o.typ == objTag {
				if c, ok := r.commitOf(tagTarget(o.data)); ok {
					_ = c
					out = append(out, refLine(tagTarget(o.data), name+"^{}", false, "")...)
				}
			}
		}
	}
	out = append(out, pktFlush...)
	return out
}

// tagTarget returns the object a tag points at.
func tagTarget(data []byte) string {
	for _, line := range bytes.Split(data, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("object ")) {
			return string(line[7:])
		}
	}
	return ""
}

// advertiseUploadV2 builds the v2 capability advertisement.
func advertiseUploadV2(r *Repo) []byte {
	var out []byte
	out = append(out, pktLine("version 2\n")...)
	out = append(out, pktLine("agent=forge/1.0\n")...)
	out = append(out, pktLine("ls-refs\n")...)
	out = append(out, pktLine("fetch=shallow\n")...)
	out = append(out, pktLine("object-format=sha1\n")...)
	out = append(out, pktLine("object-info\n")...)
	out = append(out, pktFlush...)
	return out
}

// advertiseReceiveV0 builds the v0 receive-pack advertisement.
func advertiseReceiveV0(r *Repo) []byte {
	var out []byte
	out = append(out, pktLine("# service=git-receive-pack\n")...)
	out = append(out, pktFlush...)
	names := r.sortedRefNames()
	if len(names) == 0 {
		out = append(out, refLine(zeroOID, "capabilities^{}", true, receiveV0Caps)...)
	} else {
		for i, name := range names {
			out = append(out, refLine(r.refs[name], name, i == 0, receiveV0Caps)...)
		}
	}
	out = append(out, pktFlush...)
	return out
}

// ---- fetch selection ----

// fetchRequest is a parsed upload-pack fetch.
type fetchRequest struct {
	wants    []string
	haves    []string
	shallows []string // client-side shallow boundary commits
	deepen   int      // -1 when absent
}

// selectFetch computes the objects a fetch must deliver and the shallow-info
// lines it must report.
func selectFetch(r *Repo, req fetchRequest) (oids []string, shallowLines, unshallowLines []string) {
	// Objects the client already holds: everything reachable from its haves
	// and shallow boundaries, with the parent links of shallow boundaries
	// cut — a shallow client holds the boundary commit itself, never its
	// ancestors, even when it names the boundary as a have.
	held := map[string]bool{}
	if len(req.haves) > 0 || len(req.shallows) > 0 {
		cutSet := map[string]bool{}
		for _, s := range req.shallows {
			cutSet[s] = true
		}
		tips := append(append([]string{}, req.haves...), req.shallows...)
		for oid := range r.reachableCut(tips, cutSet) {
			held[oid] = true
		}
	}
	// Candidate objects: reachable from wants, cut at the deepen boundary.
	var candidate map[string]bool
	var boundary map[string]bool
	if req.deepen >= 0 {
		candidate, boundary = r.reachableDepth(req.wants, req.deepen)
	} else {
		candidate = r.reachable(req.wants)
	}
	for oid := range candidate {
		if !held[oid] {
			oids = append(oids, oid)
		}
	}
	sort.Strings(oids)
	if req.deepen >= 0 {
		for oid := range boundary {
			if !held[oid] {
				shallowLines = append(shallowLines, oid)
			}
		}
		sort.Strings(shallowLines)
		// A formerly-shallow commit is now unshallow when it lies inside
		// the new depth (closer to a want than the boundary).
		inside := map[string]bool{}
		for oid := range candidate {
			inside[oid] = true
		}
		for _, s := range req.shallows {
			if inside[s] && !boundary[s] {
				unshallowLines = append(unshallowLines, s)
			}
		}
		sort.Strings(unshallowLines)
	}
	return oids, shallowLines, unshallowLines
}

// reachableDepth walks from tips following parents, limited to depth: commits
// at distance < depth are included with their trees and blobs; commits at
// exactly depth are the new shallow boundary.
func (r *Repo) reachableDepth(tips []string, depth int) (included map[string]bool, boundary map[string]bool) {
	included = map[string]bool{}
	boundary = map[string]bool{}
	if depth <= 0 {
		return included, boundary
	}
	seen := map[string]int{} // commit -> shortest distance
	queue := []string{}
	for _, t := range tips {
		if t == "" || t == zeroOID {
			continue
		}
		if _, ok := r.objects[t]; !ok {
			continue
		}
		if _, ok := seen[t]; !ok {
			seen[t] = 0
			queue = append(queue, t)
		}
	}
	var walkTree func(oid string)
	walkTree = func(oid string) {
		if included[oid] {
			return
		}
		included[oid] = true
		for _, e := range r.treeOf(oid) {
			if e.isDir {
				walkTree(e.oid)
			} else {
				included[e.oid] = true
			}
		}
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		d := seen[cur]
		c, ok := r.commitOf(cur)
		if !ok {
			// non-commit tip (tag): include it and its target
			included[cur] = true
			if o, ok2 := r.objects[cur]; ok2 && o.typ == objTag {
				target := tagTarget(o.data)
				if t, ok3 := r.objects[target]; ok3 {
					if t.typ == objCommit {
						if _, seen2 := seen[target]; !seen2 {
							seen[target] = d
							queue = append(queue, target)
						}
					} else {
						included[target] = true
					}
				}
			}
			continue
		}
		included[cur] = true
		walkTree(c.tree)
		if d >= depth-1 {
			// Deepest included commit: the new shallow boundary.
			boundary[cur] = true
			continue
		}
		for _, p := range c.parents {
			if _, ok := seen[p]; !ok {
				seen[p] = d + 1
				queue = append(queue, p)
			}
		}
	}
	return included, boundary
}

// ackFor computes the acknowledgment lines for a fetch.
func ackFor(r *Repo, req fetchRequest) []string {
	if len(req.haves) == 0 {
		return []string{"NAK"}
	}
	// A have is common when it is an ancestor of (or equal to) a want.
	for _, h := range req.haves {
		for _, w := range req.wants {
			if h == w || r.isAncestor(h, w) {
				return []string{"ACK " + h, "ready"}
			}
		}
	}
	return []string{"NAK"}
}

var packCacheMu sync.Mutex

// cachedPack returns the cached fetch response for a key, building it once.
// The build holds the cache mutex, so fifty concurrent identical clones
// build the pack once instead of fifty times (singleflight).
func cachedPack(r *Repo, key string, build func() []byte) []byte {
	packCacheMu.Lock()
	defer packCacheMu.Unlock()
	if b, ok := r.packCache[key]; ok {
		return b
	}
	b := build()
	r.packCache[key] = b
	return b
}

// ---- upload-pack ----

// handleUploadPack serves POST /{owner}/{repo}/git-upload-pack.
// protoV2 is true when the Git-Protocol: version=2 header was sent.
func handleUploadPack(r *Repo, body []byte, protoV2 bool) (resp []byte, status int) {
	packets := parsePktLines(body)
	if len(packets) == 0 || packets[0].kind != "data" {
		return nil, 400
	}
	first := string(packets[0].payload)
	if strings.HasPrefix(first, "command=") {
		cmd := strings.TrimSuffix(strings.TrimPrefix(first, "command="), "\n")
		switch cmd {
		case "ls-refs":
			return handleLsRefs(r, packets), 200
		case "fetch":
			return handleFetchV2(r, packets), 200
		default:
			return nil, 400
		}
	}
	// v0 fetch: want/have/shallow/deepen lines, two flush-separated sections.
	return handleFetchV0(r, packets), 200
}

// handleLsRefs serves the v2 ls-refs command.
func handleLsRefs(r *Repo, packets []pkt) []byte {
	var peel, symrefs bool
	var prefixes []string
	inArgs := false
	for _, p := range packets[1:] {
		if p.kind == "delim" {
			inArgs = true
			continue
		}
		if p.kind != "data" || !inArgs {
			continue
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		switch {
		case line == "peel":
			peel = true
		case line == "symrefs":
			symrefs = true
		case strings.HasPrefix(line, "ref-prefix "):
			prefixes = append(prefixes, strings.TrimPrefix(line, "ref-prefix "))
		}
	}
	var out []byte
	mainOID := r.refs["refs/heads/main"]
	if len(prefixes) == 0 && mainOID != "" {
		line := mainOID + " HEAD"
		if symrefs {
			line += " symref-target:refs/heads/main"
		}
		out = append(out, pktLine(line+"\n")...)
	}
	for _, name := range r.sortedRefNames() {
		if len(prefixes) > 0 {
			match := false
			for _, p := range prefixes {
				if strings.HasPrefix(name, p) {
					match = true
					break
				}
			}
			if !match {
				continue
			}
		}
		oid := r.refs[name]
		line := oid + " " + name
		if peel {
			if o, ok := r.objects[oid]; ok && o.typ == objTag {
				line += " peeled:" + tagTarget(o.data)
			}
		}
		out = append(out, pktLine(line+"\n")...)
	}
	out = append(out, pktFlush...)
	return out
}

// parseFetchArgs extracts want/have/shallow/deepen from fetch pkt-lines.
func parseFetchArgs(packets []pkt) fetchRequest {
	req := fetchRequest{deepen: -1}
	for _, p := range packets {
		if p.kind != "data" {
			continue
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		switch {
		case strings.HasPrefix(line, "want "):
			req.wants = append(req.wants, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "have "):
			req.haves = append(req.haves, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "shallow "):
			req.shallows = append(req.shallows, strings.TrimSpace(line[8:]))
		case strings.HasPrefix(line, "deepen "):
			if n, err := strconv.Atoi(strings.TrimSpace(line[7:])); err == nil {
				req.deepen = n
			}
		}
	}
	return req
}

// fetchCacheKey builds the cache key for a fetch response.
func fetchCacheKey(r *Repo, req fetchRequest, v2 bool) string {
	var b strings.Builder
	b.WriteString(strconv.FormatUint(r.gen, 10))
	b.WriteByte('|')
	if v2 {
		b.WriteByte('2')
	} else {
		b.WriteByte('0')
	}
	for _, w := range req.wants {
		b.WriteByte('w')
		b.WriteString(w)
	}
	for _, h := range req.haves {
		b.WriteByte('h')
		b.WriteString(h)
	}
	for _, s := range req.shallows {
		b.WriteByte('s')
		b.WriteString(s)
	}
	b.WriteString("d")
	b.WriteString(strconv.Itoa(req.deepen))
	return b.String()
}

// buildFetchResponse renders a fetch response (v2 sections or v0 lines).
func buildFetchResponse(r *Repo, req fetchRequest, v2 bool) []byte {
	oids, shallowLines, unshallowLines := selectFetch(r, req)
	pack := r.packObjects(oids)
	if v2 {
		var out []byte
		out = append(out, pktLine("acknowledgments\n")...)
		for _, a := range ackFor(r, req) {
			out = append(out, pktLine(a+"\n")...)
		}
		if len(shallowLines) > 0 || len(unshallowLines) > 0 {
			out = append(out, pktDelim...)
			out = append(out, pktLine("shallow-info\n")...)
			for _, s := range shallowLines {
				out = append(out, pktLine("shallow "+s+"\n")...)
			}
			for _, s := range unshallowLines {
				out = append(out, pktLine("unshallow "+s+"\n")...)
			}
		}
		out = append(out, pktDelim...)
		out = append(out, pktLine("packfile\n")...)
		out = append(out, sideband(pack)...)
		out = append(out, pktFlush...)
		return out
	}
	// v0: ack lines, shallow lines, then the sideband pack.
	var out []byte
	for _, a := range ackFor(r, req) {
		out = append(out, pktLine(a+"\n")...)
	}
	for _, s := range shallowLines {
		out = append(out, pktLine("shallow "+s+"\n")...)
	}
	for _, s := range unshallowLines {
		out = append(out, pktLine("unshallow "+s+"\n")...)
	}
	out = append(out, sideband(pack)...)
	out = append(out, pktFlush...)
	return out
}

// handleFetchV2 serves the v2 fetch command (cached per request shape).
func handleFetchV2(r *Repo, packets []pkt) []byte {
	req := parseFetchArgs(packets)
	key := fetchCacheKey(r, req, true)
	return cachedPack(r, key, func() []byte {
		return buildFetchResponse(r, req, true)
	})
}

// handleFetchV0 serves a v0 fetch (cached per request shape).
func handleFetchV0(r *Repo, packets []pkt) []byte {
	req := parseFetchArgs(packets)
	key := fetchCacheKey(r, req, false)
	return cachedPack(r, key, func() []byte {
		return buildFetchResponse(r, req, false)
	})
}

// ---- receive-pack ----

type pushCmd struct {
	old string
	new string
	ref string
}

// handleReceivePack serves POST /{owner}/{repo}/git-receive-pack.
// Returns the response body and HTTP status.
func handleReceivePack(r *Repo, body []byte) ([]byte, int) {
	packets := parsePktLines(body)
	var cmds []pushCmd
	packStart := -1
	for i, p := range packets {
		if p.kind == "flush" {
			// The pack follows the flush packet in the raw body; find the
			// byte offset by re-walking the raw stream.
			packStart = pktOffsetAfter(body, i)
			break
		}
		if p.kind != "data" {
			continue
		}
		line := string(p.payload)
		if nul := strings.IndexByte(line, 0); nul >= 0 {
			line = line[:nul] // capabilities follow the first command
		}
		line = strings.TrimSuffix(line, "\n")
		parts := strings.SplitN(line, " ", 3)
		if len(parts) != 3 {
			return nil, 400
		}
		cmds = append(cmds, pushCmd{old: parts[0], new: parts[1], ref: parts[2]})
	}
	if len(cmds) == 0 {
		return nil, 400
	}
	// Ingest the pack (present whenever any update exists).
	if packStart >= 0 && packStart < len(body) {
		pack := body[packStart:]
		if len(pack) > 0 {
			objs, err := readPack(pack)
			if err != nil {
				return nil, 400
			}
			for oid, o := range objs {
				if _, ok := r.objects[oid]; !ok {
					r.objects[oid] = o
				}
			}
		}
	}
	// Apply ref updates in order; refused refs stay unmoved.
	var report []string
	report = append(report, "unpack ok")
	for _, c := range cmds {
		cur, exists := r.refs[c.ref]
		switch {
		case c.new == zeroOID:
			// delete
			if !exists || cur != c.old {
				report = append(report, "ng "+c.ref+" old value does not match")
			} else {
				delete(r.refs, c.ref)
				report = append(report, "ok "+c.ref)
			}
		case c.old == zeroOID:
			// create
			if exists {
				report = append(report, "ng "+c.ref+" already exists")
			} else if _, ok := r.objects[c.new]; !ok {
				report = append(report, "ng "+c.ref+" new object does not exist")
			} else {
				r.refs[c.ref] = c.new
				report = append(report, "ok "+c.ref)
			}
		default:
			// update: old must match, new must be a descendant of old
			if !exists || cur != c.old {
				report = append(report, "ng "+c.ref+" old value does not match")
			} else if _, ok := r.objects[c.new]; !ok {
				report = append(report, "ng "+c.ref+" new object does not exist")
			} else if !r.isAncestor(c.old, c.new) {
				report = append(report, "ng "+c.ref+" non-fast-forward")
			} else {
				r.refs[c.ref] = c.new
				report = append(report, "ok "+c.ref)
			}
		}
	}
	r.bump()
	// Sideband band-1 carrying the pkt-lined report.
	var inner []byte
	for _, line := range report {
		inner = append(inner, pktLine(line+"\n")...)
	}
	resp := pktLineBytes(append([]byte{1}, inner...))
	resp = append(resp, pktFlush...)
	return resp, 200
}

// pktOffsetAfter returns the byte offset in body just after the i-th packet
// of the parsed stream.
func pktOffsetAfter(body []byte, index int) int {
	pos := 0
	for i := 0; i <= index; i++ {
		if pos+4 > len(body) {
			return len(body)
		}
		if string(body[pos:pos+4]) == "0000" || string(body[pos:pos+4]) == "0001" || string(body[pos:pos+4]) == "0002" {
			pos += 4
			continue
		}
		n, err := strconv.ParseInt(string(body[pos:pos+4]), 16, 32)
		if err != nil || n < 4 {
			return len(body)
		}
		pos += int(n)
	}
	return pos
}
