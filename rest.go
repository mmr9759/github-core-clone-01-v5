// REST v3 API (/api/v3/*).
//
// Dispatch runs under the store lock held by route(): GETs under RLock,
// mutations under Lock. Handlers never lock again.

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// routeREST dispatches /api/v3/* requests. The path is relative to /api/v3.
func routeREST(st *Store, req *httpRequest) *httpResponse {
	if req.method == "GET" {
		st.mu.RLock()
		defer st.mu.RUnlock()
	} else {
		st.mu.Lock()
		defer st.mu.Unlock()
	}
	path := strings.TrimPrefix(req.path, "/api/v3")
	segs := splitPath(path)
	// Root and meta.
	if len(segs) == 0 {
		if req.method != "GET" {
			return jsonErr(404, "Not Found")
		}
		return &httpResponse{status: 200, contentType: "application/json", extra: map[string]string{"X-OAuth-Scopes": "repo, read:org"}, body: []byte(`{"current_user_url":"https://api.github.com/user","repository_url":"https://api.github.com/repos/{owner}/{repo}","rate_limit_url":"https://api.github.com/rate_limit"}`)}
	}
	if len(segs) == 1 && segs[0] == "meta" {
		if req.method != "GET" {
			return jsonErr(404, "Not Found")
		}
		return jsonResp(200, []byte(`{"installed_version":"1.0.0"}`))
	}
	if len(segs) == 1 && segs[0] == "user" {
		if req.method != "GET" {
			return jsonErr(404, "Not Found")
		}
		return jsonResp(200, renderUserJSON(st))
	}
	if len(segs) == 2 && segs[0] == "user" && segs[1] == "repos" {
		if req.method == "GET" {
			return listUserRepos(st, req)
		}
		if req.method == "POST" {
			return createUserRepo(st, req)
		}
		return jsonErr(404, "Not Found")
	}
	if len(segs) >= 3 && segs[0] == "repos" {
		return routeRepo(st, req, segs[1], segs[2:])
	}
	return jsonErr(404, "Not Found")
}

// splitPath splits a path into non-empty, URL-decoded segments.
func splitPath(path string) []string {
	var out []string
	for _, s := range strings.Split(path, "/") {
		if s == "" {
			continue
		}
		if dec, err := urlUnescape(s); err == nil {
			s = dec
		}
		out = append(out, s)
	}
	return out
}

func urlUnescape(s string) (string, error) {
	// Fast path: no percent escapes, return the segment as-is.
	if !strings.Contains(s, "%") {
		return s, nil
	}
	// Minimal percent-decoding for path segments.
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			hi := hexVal(s[i+1])
			lo := hexVal(s[i+2])
			if hi >= 0 && lo >= 0 {
				b.WriteByte(byte(hi<<4 | lo))
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String(), nil
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

func renderUserJSON(st *Store) []byte {
	b := make([]byte, 0, 256)
	b = append(b, '{')
	b = append(b, `"login":`...)
	b = jsonStr(b, st.viewer.login)
	b = append(b, `,"id":`...)
	b = strconv.AppendInt(b, int64(st.viewer.id), 10)
	b = append(b, `,"node_id":`...)
	b = jsonStr(b, nodeID("U", st.viewer.login, "", -1))
	b = append(b, `,"type":"User","name":`...)
	b = jsonStr(b, st.viewer.name)
	b = append(b, `,"email":`...)
	b = jsonStr(b, st.viewer.email)
	b = append(b, `,"html_url":`...)
	b = jsonStr(b, "https://github.com/"+st.viewer.login)
	b = append(b, `}`...)
	return b
}

// bodyMap parses the request body as a JSON object.
func bodyMap(req *httpRequest) (map[string]any, bool) {
	if len(req.body) == 0 {
		return map[string]any{}, true
	}
	var m map[string]any
	if err := json.Unmarshal(req.body, &m); err != nil {
		return nil, false
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, true
}

func strField(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func boolField(m map[string]any, key string) bool {
	if v, ok := m[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
}

// repoJSON returns the repo's cached JSON, rendering it when invalidated.
func repoJSON(st *Store, r *Repo) []byte {
	if r.rendered == nil {
		r.rendered = renderRepoJSON(st, r)
	}
	return r.rendered
}

// listUserRepos serves GET /user/repos with Link-header pagination.
func listUserRepos(st *Store, req *httpRequest) *httpResponse {
	perPage := queryInt(req, "per_page", 30)
	if perPage <= 0 {
		perPage = 30
	}
	if perPage > 100 {
		perPage = 100
	}
	page := queryInt(req, "page", 1)
	if page <= 0 {
		page = 1
	}
	total := len(st.order)
	start := (page - 1) * perPage
	if start > total {
		start = total
	}
	end := start + perPage
	if end > total {
		end = total
	}
	var b []byte
	b = append(b, '[')
	for i := start; i < end; i++ {
		if i > start {
			b = append(b, ',')
		}
		b = append(b, repoJSON(st, st.order[i])...)
	}
	b = append(b, ']')
	resp := jsonResp(200, b)
	if end < total || page > 1 {
		host := req.headers.host
		if host == "" {
			host = "127.0.0.1"
		}
		base := "http://" + host + "/api/v3/user/repos?per_page=" + strconv.Itoa(perPage)
		var links []string
		if end < total {
			links = append(links, "<"+base+"&page="+strconv.Itoa(page+1)+">; rel=\"next\"")
			last := (total + perPage - 1) / perPage
			links = append(links, "<"+base+"&page="+strconv.Itoa(last)+">; rel=\"last\"")
		}
		if page > 1 {
			links = append(links, "<"+base+"&page="+strconv.Itoa(page-1)+">; rel=\"prev\"")
			links = append(links, "<"+base+"&page=1>; rel=\"first\"")
		}
		resp.extra = map[string]string{"Link": strings.Join(links, ", ")}
	}
	return resp
}

func queryInt(req *httpRequest, key string, def int) int {
	if v := req.queryGet(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// createUserRepo serves POST /user/repos.
func createUserRepo(st *Store, req *httpRequest) *httpResponse {
	m, ok := bodyMap(req)
	if !ok {
		return jsonErr(400, "Malformed JSON body")
	}
	name := strField(m, "name")
	if name == "" {
		return jsonErr(422, "Validation Failed: name is required")
	}
	if _, exists := st.repos[repoKey(st.viewer.login, name)]; exists {
		return jsonErr(422, "Validation Failed: name already exists on this account")
	}
	r := st.createRepo(name, boolField(m, "private"), strField(m, "description"))
	return jsonResp(201, repoJSON(st, r))
}

// routeRepo dispatches /repos/{owner}/{repo}/... (rest = segments after repo).
func routeRepo(st *Store, req *httpRequest, owner string, rest []string) *httpResponse {
	if len(rest) == 0 {
		return jsonErr(404, "Not Found")
	}
	repoName := rest[0]
	sub := rest[1:]
	repo := st.getRepo(owner, repoName)
	if repo == nil {
		return jsonErr(404, "Not Found")
	}
	if len(sub) == 0 {
		switch req.method {
		case "GET":
			return jsonResp(200, repoJSON(st, repo))
		case "PATCH":
			m, ok := bodyMap(req)
			if !ok {
				return jsonErr(400, "Malformed JSON body")
			}
			if d := strField(m, "description"); m["description"] != nil {
				repo.desc = d
			}
			if _, ok := m["private"]; ok {
				repo.private = boolField(m, "private")
			}
			repo.rendered = nil
			return jsonResp(200, repoJSON(st, repo))
		case "DELETE":
			st.deleteRepo(repo)
			return &httpResponse{status: 204, contentType: "application/json", body: nil}
		default:
			return jsonErr(404, "Not Found")
		}
	}
	switch sub[0] {
	case "git":
		return routeRepoGit(st, req, repo, sub[1:])
	case "branches":
		return routeBranches(st, req, repo, sub[1:])
	case "commits":
		if req.method == "GET" && len(sub) == 1 {
			return listCommits(st, repo, req)
		}
		return jsonErr(404, "Not Found")
	case "contents":
		return routeContents(st, req, repo, sub[1:])
	case "labels":
		if req.method == "POST" && len(sub) == 1 {
			return createLabel(st, req, repo)
		}
		return jsonErr(404, "Not Found")
	case "issues":
		return routeIssues(st, req, repo, sub[1:])
	case "pulls":
		return routePulls(st, req, repo, sub[1:])
	}
	return jsonErr(404, "Not Found")
}

// routeRepoGit handles /git/refs*.
func routeRepoGit(st *Store, req *httpRequest, r *Repo, sub []string) *httpResponse {
	if len(sub) >= 1 && sub[0] == "refs" {
		if req.method == "POST" && len(sub) == 1 {
			m, ok := bodyMap(req)
			if !ok {
				return jsonErr(400, "Malformed JSON body")
			}
			ref := strField(m, "ref")
			sha := strField(m, "sha")
			if ref == "" || sha == "" {
				return jsonErr(422, "Validation Failed: ref and sha are required")
			}
			if _, exists := r.refs[ref]; exists {
				return jsonErr(422, "Validation Failed: ref already exists")
			}
			if _, ok := r.objects[sha]; !ok {
				return jsonErr(422, "Validation Failed: sha does not exist")
			}
			r.refs[ref] = sha
			r.bump()
			return jsonResp(201, refJSON(ref, sha))
		}
		if req.method == "GET" && len(sub) >= 3 && sub[1] == "heads" {
			branch := strings.Join(sub[2:], "/")
			ref := "refs/heads/" + branch
			sha, ok := r.refs[ref]
			if !ok {
				return jsonErr(404, "Not Found")
			}
			return jsonResp(200, refJSON(ref, sha))
		}
	}
	return jsonErr(404, "Not Found")
}

func refJSON(ref, sha string) []byte {
	b := make([]byte, 0, 128)
	b = append(b, `{"ref":`...)
	b = jsonStr(b, ref)
	b = append(b, `,"object":{"sha":`...)
	b = jsonStr(b, sha)
	b = append(b, `,"type":"commit"},"node_id":`...)
	b = jsonStr(b, nodeID("REF", ref, "", -1))
	b = append(b, `}`...)
	return b
}

// routeBranches handles /branches and /branches/{branch}.
func routeBranches(st *Store, req *httpRequest, r *Repo, sub []string) *httpResponse {
	if req.method != "GET" {
		return jsonErr(404, "Not Found")
	}
	if len(sub) == 0 {
		var b []byte
		b = append(b, '[')
		first := true
		for _, name := range r.sortedRefNames() {
			if !strings.HasPrefix(name, "refs/heads/") {
				continue
			}
			if !first {
				b = append(b, ',')
			}
			first = false
			branch := strings.TrimPrefix(name, "refs/heads/")
			b = append(b, branchJSON(branch, r.refs[name])...)
		}
		b = append(b, ']')
		return jsonResp(200, b)
	}
	branch := strings.Join(sub, "/")
	sha, ok := r.refs["refs/heads/"+branch]
	if !ok {
		return jsonErr(404, "Not Found")
	}
	return jsonResp(200, branchJSON(branch, sha))
}

func branchJSON(name, sha string) []byte {
	b := make([]byte, 0, 160)
	b = append(b, `{"name":`...)
	b = jsonStr(b, name)
	b = append(b, `,"commit":{"sha":`...)
	b = jsonStr(b, sha)
	b = append(b, `},"protected":false}`...)
	return b
}

// listCommits serves GET /commits[?sha=<ref>].
func listCommits(st *Store, r *Repo, req *httpRequest) *httpResponse {
	ref := req.queryGet("sha")
	var tip string
	if ref == "" {
		tip = r.refs["refs/heads/"+r.defaultBranch]
	} else if strings.HasPrefix(ref, "refs/") {
		tip = r.refs[ref]
	} else if sha, ok := r.refs["refs/heads/"+ref]; ok {
		tip = sha
	} else if _, ok := r.objects[ref]; ok {
		tip = ref
	}
	if tip == "" {
		return jsonResp(200, []byte("[]"))
	}
	// First-parent walk, then any other reachable commits.
	var order []string
	seen := map[string]bool{}
	cur := tip
	for cur != "" && !seen[cur] {
		seen[cur] = true
		order = append(order, cur)
		c, ok := r.commitOf(cur)
		if !ok || len(c.parents) == 0 {
			break
		}
		cur = c.parents[0]
	}
	// Any other reachable commits (merge second parents), newest first by
	// walk order.
	all := r.reachable([]string{tip})
	for oid := range all {
		if o, ok := r.objects[oid]; ok && o.typ == objCommit && !seen[oid] {
			seen[oid] = true
			order = append(order, oid)
		}
	}
	var b []byte
	b = append(b, '[')
	for i, oid := range order {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, commitJSON(r, oid)...)
	}
	b = append(b, ']')
	return jsonResp(200, b)
}

// commitJSON renders one commit for the REST list.
func commitJSON(r *Repo, oid string) []byte {
	c, _ := r.commitOf(oid)
	b := make([]byte, 0, 320)
	b = append(b, `{"sha":`...)
	b = jsonStr(b, oid)
	b = append(b, `,"node_id":`...)
	b = jsonStr(b, nodeID("C", oid, "", -1))
	b = append(b, `,"commit":{`...)
	b = append(b, `"author":`...)
	b = append(b, personJSON(c.author)...)
	b = append(b, `,"committer":`...)
	b = append(b, personJSON(c.committer)...)
	b = append(b, `,"message":`...)
	b = jsonStr(b, c.message)
	b = append(b, `,"tree":{"sha":`...)
	b = jsonStr(b, c.tree)
	b = append(b, `}},"parents":[`...)
	for i, p := range c.parents {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, `{"sha":`...)
		b = jsonStr(b, p)
		b = append(b, `}`...)
	}
	b = append(b, `]}`...)
	return b
}

// personJSON parses "Name <email> ts +0000" into a GitHub person object.
func personJSON(ident string) []byte {
	name, email, ts := parseIdent(ident)
	b := make([]byte, 0, 160)
	b = append(b, `{"name":`...)
	b = jsonStr(b, name)
	b = append(b, `,"email":`...)
	b = jsonStr(b, email)
	b = append(b, `,"date":`...)
	b = jsonStr(b, ts)
	b = append(b, `}`...)
	return b
}

// parseIdent splits a git ident line into name, email, ISO date.
func parseIdent(ident string) (name, email, iso string) {
	lt := strings.IndexByte(ident, '<')
	gt := strings.IndexByte(ident, '>')
	if lt < 0 || gt < 0 || gt < lt {
		return ident, "", ""
	}
	name = strings.TrimSpace(ident[:lt])
	email = ident[lt+1 : gt]
	rest := strings.TrimSpace(ident[gt+1:])
	fields := strings.Fields(rest)
	if len(fields) >= 1 {
		if sec, err := strconv.ParseInt(fields[0], 10, 64); err == nil {
			iso = formatUnix(sec)
		}
	}
	return name, email, iso
}

func formatUnix(sec int64) string {
	return time.Unix(sec, 0).UTC().Format("2006-01-02T15:04:05Z")
}

// ---- tree helpers for the contents API ----

// fileEntry finds the entry for a path inside a tree.
func (r *Repo) fileEntry(treeOID string, parts []string) (TreeEntry, bool) {
	cur := treeOID
	for i, p := range parts {
		var next string
		found := false
		for _, e := range r.treeOf(cur) {
			if e.name == p {
				if i == len(parts)-1 {
					return e, true
				}
				if !e.isDir {
					return TreeEntry{}, false
				}
				next = e.oid
				found = true
				break
			}
		}
		if !found {
			return TreeEntry{}, false
		}
		cur = next
	}
	return TreeEntry{}, false
}

// dirEntries lists the entries of the directory at parts (empty = root).
func (r *Repo) dirEntries(treeOID string, parts []string) ([]TreeEntry, bool) {
	cur := treeOID
	for _, p := range parts {
		var next string
		found := false
		for _, e := range r.treeOf(cur) {
			if e.name == p && e.isDir {
				next = e.oid
				found = true
				break
			}
		}
		if !found {
			return nil, false
		}
		cur = next
	}
	return r.treeOf(cur), true
}

// setFileInTree returns a new tree with path set to blobOID (or removed when
// del is true), rebuilding parent trees as needed.
func (r *Repo) setFileInTree(treeOID string, parts []string, blobOID string, del bool) string {
	entries := r.treeOf(treeOID)
	if len(parts) == 1 {
		var out []TreeEntry
		replaced := false
		for _, e := range entries {
			if e.name == parts[0] {
				if del {
					replaced = true
					continue
				}
				out = append(out, TreeEntry{mode: "100644", name: parts[0], oid: blobOID})
				replaced = true
				continue
			}
			out = append(out, e)
		}
		if !replaced && !del {
			out = append(out, TreeEntry{mode: "100644", name: parts[0], oid: blobOID})
		}
		return r.buildTree(out)
	}
	var out []TreeEntry
	replaced := false
	for _, e := range entries {
		if e.name == parts[0] && e.isDir {
			out = append(out, TreeEntry{mode: "40000", name: parts[0], oid: r.setFileInTree(e.oid, parts[1:], blobOID, del), isDir: true})
			replaced = true
			continue
		}
		out = append(out, e)
	}
	if !replaced && !del {
		sub := r.setFileInTree("", parts[1:], blobOID, false)
		out = append(out, TreeEntry{mode: "40000", name: parts[0], oid: sub, isDir: true})
	}
	return r.buildTree(out)
}

// commitIdent builds a deterministic git ident line for the viewer.
func (s *Store) commitIdent() string {
	s.nowCounter += 1000
	sec := s.nowBase + s.nowCounter/1000
	return s.viewer.name + " <" + s.viewer.email + "> " + strconv.FormatInt(sec, 10) + " +0000"
}

// commitTreeChange commits treeOID on branch (creating the branch from the
// default tip when missing) and returns the new commit oid.
func (s *Store) commitTreeChange(r *Repo, branch, treeOID, message string) string {
	tip := r.refs["refs/heads/"+branch]
	parents := []string{}
	if tip != "" {
		parents = []string{tip}
	}
	ident := s.commitIdent()
	commitOID := r.makeCommit(treeOID, parents, ident, ident, message)
	r.refs["refs/heads/"+branch] = commitOID
	r.bump()
	return commitOID
}

// branchTip resolves a branch name to its tip oid ("" when unborn).
func (r *Repo) branchTip(branch string) string {
	return r.refs["refs/heads/"+branch]
}

// ---- contents ----

func routeContents(st *Store, req *httpRequest, r *Repo, sub []string) *httpResponse {
	pathParts := sub
	branch := req.queryGet("ref")
	if branch == "" {
		branch = r.defaultBranch
	}
	// PUT/DELETE read the branch from the body instead.
	if req.method == "PUT" || req.method == "DELETE" {
		m, ok := bodyMap(req)
		if !ok {
			return jsonErr(400, "Malformed JSON body")
		}
		if b := strField(m, "branch"); b != "" {
			branch = b
		}
		return writeContents(st, req, r, pathParts, branch, m, req.method == "DELETE")
	}
	if req.method != "GET" {
		return jsonErr(404, "Not Found")
	}
	// Resolve the ref: branch name, full ref, or raw commit oid.
	tip := ""
	if strings.HasPrefix(branch, "refs/") {
		tip = r.refs[branch]
	} else if sha, ok := r.refs["refs/heads/"+branch]; ok {
		tip = sha
	} else if _, ok := r.objects[branch]; ok {
		tip = branch
	}
	if tip == "" {
		return jsonErr(404, "Not Found")
	}
	c, ok := r.commitOf(tip)
	if !ok {
		return jsonErr(404, "Not Found")
	}
	if len(pathParts) == 0 {
		// Root directory listing.
		entries := r.treeOf(c.tree)
		return jsonResp(200, dirListingJSON(r, entries, ""))
	}
	entry, ok := r.fileEntry(c.tree, pathParts)
	if !ok {
		// Maybe a directory.
		if entries, ok := r.dirEntries(c.tree, pathParts); ok {
			return jsonResp(200, dirListingJSON(r, entries, strings.Join(pathParts, "/")))
		}
		return jsonErr(404, "Not Found")
	}
	if entry.isDir {
		entries, _ := r.dirEntries(c.tree, pathParts)
		return jsonResp(200, dirListingJSON(r, entries, strings.Join(pathParts, "/")))
	}
	content := r.blobBytes(entry.oid)
	return jsonResp(200, fileContentJSON(r, entry, strings.Join(pathParts, "/"), content))
}

func dirListingJSON(r *Repo, entries []TreeEntry, prefix string) []byte {
	var b []byte
	b = append(b, '[')
	for i, e := range entries {
		if i > 0 {
			b = append(b, ',')
		}
		path := e.name
		if prefix != "" {
			path = prefix + "/" + e.name
		}
		b = append(b, '{')
		typ := "file"
		if e.isDir {
			typ = "dir"
		}
		b = append(b, `"type":`...)
		b = jsonStr(b, typ)
		b = append(b, `,"name":`...)
		b = jsonStr(b, e.name)
		b = append(b, `,"path":`...)
		b = jsonStr(b, path)
		b = append(b, `,"sha":`...)
		b = jsonStr(b, e.oid)
		if !e.isDir {
			b = append(b, `,"size":`...)
			b = strconv.AppendInt(b, int64(len(r.blobBytes(e.oid))), 10)
		}
		b = append(b, `,"url":`...)
		b = jsonStr(b, "https://api.github.com/repos/"+r.owner+"/"+r.name+"/contents/"+path)
		b = append(b, `}`...)
	}
	b = append(b, ']')
	return b
}

func fileContentJSON(r *Repo, e TreeEntry, path string, content []byte) []byte {
	b := make([]byte, 0, 256+len(content))
	b = append(b, '{')
	b = append(b, `"type":"file","encoding":"base64","name":`...)
	b = jsonStr(b, e.name)
	b = append(b, `,"path":`...)
	b = jsonStr(b, path)
	b = append(b, `,"sha":`...)
	b = jsonStr(b, e.oid)
	b = append(b, `,"size":`...)
	b = strconv.AppendInt(b, int64(len(content)), 10)
	b = append(b, `,"content":`...)
	b = jsonB64(b, content)
	b = append(b, `,"url":`...)
	b = jsonStr(b, "https://api.github.com/repos/"+r.owner+"/"+r.name+"/contents/"+path)
	b = append(b, `}`...)
	return b
}

func writeContents(st *Store, req *httpRequest, r *Repo, pathParts []string, branch string, m map[string]any, isDelete bool) *httpResponse {
	if len(pathParts) == 0 {
		return jsonErr(404, "Not Found")
	}
	// Resolve the branch, creating it from the default tip when missing.
	tip := r.branchTip(branch)
	if tip == "" && branch != r.defaultBranch {
		if defTip := r.branchTip(r.defaultBranch); defTip != "" {
			r.refs["refs/heads/"+branch] = defTip
			tip = defTip
		}
	}
	var curTree string
	if tip != "" {
		c, ok := r.commitOf(tip)
		if !ok {
			return jsonErr(404, "Not Found")
		}
		curTree = c.tree
	}
	message := strField(m, "message")
	if message == "" {
		message = "Update " + strings.Join(pathParts, "/")
	}
	if isDelete {
		entry, ok := r.fileEntry(curTree, pathParts)
		if !ok || entry.isDir {
			return jsonErr(404, "Not Found")
		}
		if sha := strField(m, "sha"); sha != "" && sha != entry.oid {
			return jsonErr(409, "Conflict: sha does not match")
		}
		newTree := r.setFileInTree(curTree, pathParts, "", true)
		commitOID := st.commitTreeChange(r, branch, newTree, message)
		return jsonResp(200, []byte(`{"commit":`+string(commitRefJSON(commitOID, message))+`}`))
	}
	// PUT: create or update a file.
	raw := strField(m, "content")
	cleaned := strings.Map(func(ch rune) rune {
		if ch == '\n' || ch == '\r' || ch == ' ' || ch == '\t' {
			return -1
		}
		return ch
	}, raw)
	content, err := base64.StdEncoding.DecodeString(cleaned)
	if err != nil {
		return jsonErr(422, "Validation Failed: content is not valid base64")
	}
	existing, exists := r.fileEntry(curTree, pathParts)
	status := 201
	if exists {
		if sha := strField(m, "sha"); sha == "" {
			return jsonErr(409, "Conflict: sha is required to update a file")
		} else if sha != existing.oid {
			return jsonErr(409, "Conflict: sha does not match")
		}
		status = 200
	}
	blobOID := r.blobOID(content)
	newTree := r.setFileInTree(curTree, pathParts, blobOID, false)
	commitOID := st.commitTreeChange(r, branch, newTree, message)
	entry := TreeEntry{mode: "100644", name: pathParts[len(pathParts)-1], oid: blobOID}
	b := make([]byte, 0, 512)
	b = append(b, `{"content":`...)
	b = append(b, fileContentJSON(r, entry, strings.Join(pathParts, "/"), content)...)
	b = append(b, `,"commit":`...)
	b = append(b, commitRefJSON(commitOID, message)...)
	b = append(b, `}`...)
	return jsonResp(status, b)
}

func commitRefJSON(sha, message string) []byte {
	b := make([]byte, 0, 128)
	b = append(b, `{"sha":`...)
	b = jsonStr(b, sha)
	b = append(b, `,"message":`...)
	b = jsonStr(b, message)
	b = append(b, `}`...)
	return b
}

// ---- labels ----

func createLabel(st *Store, req *httpRequest, r *Repo) *httpResponse {
	m, ok := bodyMap(req)
	if !ok {
		return jsonErr(400, "Malformed JSON body")
	}
	name := strField(m, "name")
	if name == "" {
		return jsonErr(422, "Validation Failed: name is required")
	}
	if existing, ok := r.labels[name]; ok {
		return jsonResp(200, renderLabelJSON(existing))
	}
	color := strField(m, "color")
	if color == "" {
		color = "ededed"
	}
	l := st.createLabel(r, name, color)
	l.desc = strField(m, "description")
	return jsonResp(201, renderLabelJSON(l))
}

// ---- issues ----

func routeIssues(st *Store, req *httpRequest, r *Repo, sub []string) *httpResponse {
	if len(sub) == 0 {
		switch req.method {
		case "GET":
			return listIssues(st, r, req)
		case "POST":
			return createIssueREST(st, req, r)
		default:
			return jsonErr(404, "Not Found")
		}
	}
	num, err := strconv.Atoi(sub[0])
	if err != nil {
		return jsonErr(404, "Not Found")
	}
	is, ok := r.issueByNum[num]
	if !ok {
		return jsonErr(404, "Not Found")
	}
	if len(sub) == 1 {
		switch req.method {
		case "GET":
			return jsonResp(200, is.rendered)
		case "PATCH":
			return patchIssue(st, req, r, is)
		default:
			return jsonErr(404, "Not Found")
		}
	}
	if len(sub) == 2 && sub[1] == "comments" {
		switch req.method {
		case "GET":
			var b []byte
			b = append(b, '[')
			for i, c := range is.comments {
				if i > 0 {
					b = append(b, ',')
				}
				b = append(b, renderCommentJSON(st, r, c)...)
			}
			b = append(b, ']')
			return jsonResp(200, b)
		case "POST":
			m, ok := bodyMap(req)
			if !ok {
				return jsonErr(400, "Malformed JSON body")
			}
			body := strField(m, "body")
			st.nextComment++
			c := &Comment{id: st.nextComment, body: body, createdAt: st.nowISO()}
			is.comments = append(is.comments, c)
			st.refreshIssue(r, is)
			return jsonResp(201, renderCommentJSON(st, r, c))
		}
	}
	return jsonErr(404, "Not Found")
}

func listIssues(st *Store, r *Repo, req *httpRequest) *httpResponse {
	state := req.queryGet("state")
	if state == "" {
		state = "open"
	}
	direction := req.queryGet("direction")
	asc := direction == "asc"
	var list []*Issue
	for _, is := range r.issues {
		if state != "all" && is.state != state {
			continue
		}
		list = append(list, is)
	}
	// Sort by creation order.
	for i := 0; i < len(list); i++ {
		for j := i + 1; j < len(list); j++ {
			if (asc && list[j].createdSeq < list[i].createdSeq) || (!asc && list[j].createdSeq > list[i].createdSeq) {
				list[i], list[j] = list[j], list[i]
			}
		}
	}
	var b []byte
	b = append(b, '[')
	for i, is := range list {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, is.rendered...)
	}
	b = append(b, ']')
	return jsonResp(200, b)
}

func createIssueREST(st *Store, req *httpRequest, r *Repo) *httpResponse {
	// Fast path: scan title/body/labels straight out of the small JSON
	// body (the write workload posts 200 of these); fall back to a full
	// decode for anything unexpected.
	if title, body, labels, ok := parseIssueBody(req.body); ok {
		if title == "" {
			return jsonErr(422, "Validation Failed: title is required")
		}
		var ls []*Label
		for _, name := range labels {
			l, exists := r.labels[name]
			if !exists {
				return jsonErr(422, "Validation Failed: label '"+name+"' does not exist")
			}
			ls = append(ls, l)
		}
		is := st.createIssue(r, title, body, ls)
		r.rendered = nil
		return jsonResp(201, is.rendered)
	}
	m, ok := bodyMap(req)
	if !ok {
		return jsonErr(400, "Malformed JSON body")
	}
	title := strField(m, "title")
	if title == "" {
		return jsonErr(422, "Validation Failed: title is required")
	}
	var labels []*Label
	if raw, ok := m["labels"]; ok {
		if arr, ok := raw.([]any); ok {
			for _, item := range arr {
				var name string
				switch v := item.(type) {
				case string:
					name = v
				case map[string]any:
					name = strField(v, "name")
				}
				if name == "" {
					continue
				}
				l, exists := r.labels[name]
				if !exists {
					return jsonErr(422, "Validation Failed: label '"+name+"' does not exist")
				}
				labels = append(labels, l)
			}
		}
	}
	is := st.createIssue(r, title, strField(m, "body"), labels)
	r.rendered = nil
	return jsonResp(201, is.rendered)
}

// scanJSONString parses a JSON string starting at b[i] == '"', returning
// the decoded string and the index just past the closing quote.
func scanJSONString(b []byte, i int) (string, int, bool) {
	if i >= len(b) || b[i] != '"' {
		return "", 0, false
	}
	i++
	var sb strings.Builder
	for i < len(b) {
		c := b[i]
		if c == '"' {
			return sb.String(), i + 1, true
		}
		if c == '\\' && i+1 < len(b) {
			i++
			switch b[i] {
			case 'n':
				sb.WriteByte('\n')
			case 't':
				sb.WriteByte('\t')
			case 'r':
				sb.WriteByte('\r')
			case 'b':
				sb.WriteByte('\b')
			case 'f':
				sb.WriteByte('\f')
			case '/':
				sb.WriteByte('/')
			case '\\':
				sb.WriteByte('\\')
			case '"':
				sb.WriteByte('"')
			case 'u':
				if i+4 < len(b) {
					if n, err := strconv.ParseUint(string(b[i+1:i+5]), 16, 32); err == nil {
						sb.WriteRune(rune(n))
						i += 4
					}
				}
			default:
				sb.WriteByte(b[i])
			}
			i++
			continue
		}
		sb.WriteByte(c)
		i++
	}
	return "", 0, false
}

// findJSONField finds a top-level "key": and returns the index of its value.
func findJSONField(b []byte, key string) (int, bool) {
	needle := []byte(`"` + key + `"`)
	for i := 0; i+len(needle) <= len(b); i++ {
		if !bytes.Equal(b[i:i+len(needle)], needle) {
			continue
		}
		// The key must open an object member, not sit inside a string.
		j := i - 1
		for j >= 0 && (b[j] == ' ' || b[j] == '\t' || b[j] == '\n' || b[j] == '\r') {
			j--
		}
		if j < 0 || (b[j] != '{' && b[j] != ',') {
			continue
		}
		j = i + len(needle)
		for j < len(b) && (b[j] == ' ' || b[j] == '\t' || b[j] == '\n' || b[j] == '\r') {
			j++
		}
		if j >= len(b) || b[j] != ':' {
			continue
		}
		j++
		for j < len(b) && (b[j] == ' ' || b[j] == '\t' || b[j] == '\n' || b[j] == '\r') {
			j++
		}
		return j, true
	}
	return 0, false
}

// parseIssueBody extracts title, body and label names from an issue-create
// JSON body without a full decode.
func parseIssueBody(b []byte) (title, body string, labels []string, ok bool) {
	ti, found := findJSONField(b, "title")
	if !found {
		return "", "", nil, false
	}
	title, _, ok = scanJSONString(b, ti)
	if !ok {
		return "", "", nil, false
	}
	if bi, found := findJSONField(b, "body"); found {
		body, _, ok = scanJSONString(b, bi)
		if !ok {
			return "", "", nil, false
		}
	}
	if li, found := findJSONField(b, "labels"); found {
		if li >= len(b) || b[li] != '[' {
			return "", "", nil, false
		}
		i := li + 1
		for {
			for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r' || b[i] == ',') {
				i++
			}
			if i >= len(b) {
				return "", "", nil, false
			}
			if b[i] == ']' {
				break
			}
			var name string
			name, i, ok = scanJSONString(b, i)
			if !ok {
				return "", "", nil, false
			}
			labels = append(labels, name)
		}
	}
	return title, body, labels, true
}

func patchIssue(st *Store, req *httpRequest, r *Repo, is *Issue) *httpResponse {
	m, ok := bodyMap(req)
	if !ok {
		return jsonErr(400, "Malformed JSON body")
	}
	if v, ok := m["title"]; ok {
		if s, ok := v.(string); ok {
			is.title = s
		}
	}
	if v, ok := m["body"]; ok {
		if s, ok := v.(string); ok {
			is.body = s
		}
	}
	if v, ok := m["state"]; ok {
		if s, ok := v.(string); ok && (s == "open" || s == "closed") {
			is.state = s
			if s == "closed" {
				is.closedAt = st.nowISO()
			} else {
				is.closedAt = ""
			}
		}
	}
	is.updatedAt = st.nowISO()
	st.refreshIssue(r, is)
	r.rendered = nil
	return jsonResp(200, is.rendered)
}

// ---- pull requests ----

func routePulls(st *Store, req *httpRequest, r *Repo, sub []string) *httpResponse {
	if len(sub) == 0 {
		switch req.method {
		case "GET":
			return listPulls(st, r, req)
		case "POST":
			return createPullREST(st, req, r)
		default:
			return jsonErr(404, "Not Found")
		}
	}
	num, err := strconv.Atoi(sub[0])
	if err != nil {
		return jsonErr(404, "Not Found")
	}
	p, ok := r.prByNum[num]
	if !ok {
		return jsonErr(404, "Not Found")
	}
	if len(sub) == 1 {
		if req.method == "GET" {
			return jsonResp(200, renderPRJSON(st, r, p))
		}
		return jsonErr(404, "Not Found")
	}
	if len(sub) == 2 && sub[1] == "files" && req.method == "GET" {
		return listPRFiles(st, r, p)
	}
	if len(sub) == 2 && sub[1] == "merge" && req.method == "PUT" {
		return mergePullREST(st, req, r, p)
	}
	return jsonErr(404, "Not Found")
}

func listPulls(st *Store, r *Repo, req *httpRequest) *httpResponse {
	state := req.queryGet("state")
	if state == "" {
		state = "open"
	}
	var b []byte
	b = append(b, '[')
	first := true
	for i := len(r.prs) - 1; i >= 0; i-- {
		p := r.prs[i]
		if state != "all" && p.state != state {
			continue
		}
		if !first {
			b = append(b, ',')
		}
		first = false
		b = append(b, renderPRJSON(st, r, p)...)
	}
	b = append(b, ']')
	return jsonResp(200, b)
}

func createPullREST(st *Store, req *httpRequest, r *Repo) *httpResponse {
	m, ok := bodyMap(req)
	if !ok {
		return jsonErr(400, "Malformed JSON body")
	}
	title := strField(m, "title")
	head := stripOwnerPrefix(strField(m, "head"))
	base := stripOwnerPrefix(strField(m, "base"))
	if title == "" || head == "" || base == "" {
		return jsonErr(422, "Validation Failed: title, head and base are required")
	}
	headSha, okH := r.refs["refs/heads/"+head]
	baseSha, okB := r.refs["refs/heads/"+base]
	if !okH || !okB {
		return jsonErr(422, "Validation Failed: head or base branch does not exist")
	}
	for _, p := range r.prs {
		if p.state == "open" && p.headRef == head && p.baseRef == base {
			return jsonErr(422, "Validation Failed: a pull request already exists for this head and base")
		}
	}
	if headSha == baseSha {
		return jsonErr(422, "Validation Failed: no commits between head and base")
	}
	p := st.createPR(r, title, strField(m, "body"), head, base)
	return jsonResp(201, renderPRJSON(st, r, p))
}

func stripOwnerPrefix(ref string) string {
	if i := strings.IndexByte(ref, ':'); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

func listPRFiles(st *Store, r *Repo, p *PR) *httpResponse {
	headTip, baseTip := r.prTips(p)
	base := r.mergeBase(headTip, baseTip)
	var baseTree string
	if base != "" {
		if c, ok := r.commitOf(base); ok {
			baseTree = c.tree
		}
	}
	var headTree string
	if c, ok := r.commitOf(headTip); ok {
		headTree = c.tree
	}
	files := r.diffFiles(baseTree, headTree)
	var b []byte
	b = append(b, '[')
	for i, f := range files {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '{')
		b = append(b, `"filename":`...)
		b = jsonStr(b, f.filename)
		b = append(b, `,"status":`...)
		b = jsonStr(b, f.status)
		b = append(b, `,"additions":`...)
		b = strconv.AppendInt(b, int64(f.additions), 10)
		b = append(b, `,"deletions":`...)
		b = strconv.AppendInt(b, int64(f.deletions), 10)
		b = append(b, `,"changes":`...)
		b = strconv.AppendInt(b, int64(f.changes), 10)
		b = append(b, `}`...)
	}
	b = append(b, ']')
	return jsonResp(200, b)
}

func mergePullREST(st *Store, req *httpRequest, r *Repo, p *PR) *httpResponse {
	if p.state != "open" {
		return jsonErr(405, "Pull request is already closed")
	}
	mergeSha, ok := doMergePR(st, r, p)
	if !ok {
		return jsonErr(405, "Merge conflict: this branch has conflicts that must be resolved")
	}
	message := "Merge pull request #" + strconv.Itoa(p.num) + " from " + r.owner + "/" + p.headRef
	b := make([]byte, 0, 256)
	b = append(b, `{"sha":`...)
	b = jsonStr(b, mergeSha)
	b = append(b, `,"merged":true,"message":`...)
	b = jsonStr(b, message)
	b = append(b, `}`...)
	return jsonResp(200, b)
}

// doMergePR performs a squash merge of p into its base branch. Returns the
// merge commit sha, or "" when the merge conflicts.
func doMergePR(st *Store, r *Repo, p *PR) (string, bool) {
	headTip, baseTip := r.prTips(p)
	p.headSha, p.baseSha = headTip, baseTip
	base := r.mergeBase(headTip, baseTip)
	var baseTree, headTree, otherTree string
	if base != "" {
		if c, ok := r.commitOf(base); ok {
			baseTree = c.tree
		}
	}
	if c, ok := r.commitOf(p.baseSha); ok {
		headTree = c.tree
	}
	if c, ok := r.commitOf(p.headSha); ok {
		otherTree = c.tree
	}
	mergedTree, ok := r.mergeTrees(baseTree, headTree, otherTree)
	if !ok {
		return "", false
	}
	message := "Merge pull request #" + strconv.Itoa(p.num) + " from " + r.owner + "/" + p.headRef
	ident := st.commitIdent()
	mergeSha := r.makeCommit(mergedTree, []string{p.baseSha}, ident, ident, message)
	r.refs["refs/heads/"+p.baseRef] = mergeSha
	r.bump()
	p.merged = true
	p.state = "closed"
	p.mergedAt = st.nowISO()
	p.mergeSha = mergeSha
	return mergeSha, true
}
