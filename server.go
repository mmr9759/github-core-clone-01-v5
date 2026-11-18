// Raw TCP HTTP/1.1 server: no net/http, no allocations on the hot path
// beyond a pooled buffered reader per connection and one response buffer
// per request. Keep-alive is honoured so the read workload's two thousand
// GETs ride a single connection.

package main

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// httpHeaders holds only the request headers the forge ever reads, as
// plain fields — no per-request map allocation on the hot path.
type httpHeaders struct {
	authorization    string
	contentLength    string
	contentType      string
	connection       string
	expect           string
	gitProtocol      string
	host             string
	transferEncoding string
}

func (h *httpHeaders) set(name, value string) {
	switch name {
	case "authorization":
		h.authorization = value
	case "content-length":
		h.contentLength = value
	case "content-type":
		h.contentType = value
	case "connection":
		h.connection = value
	case "expect":
		h.expect = value
	case "git-protocol":
		h.gitProtocol = value
	case "host":
		h.host = value
	case "transfer-encoding":
		h.transferEncoding = value
	}
}

type httpRequest struct {
	method      string
	path        string
	queryRaw    string
	queryParsed url.Values
	headers     httpHeaders
	body        []byte
}

// queryGet returns one query parameter, parsing the raw query string lazily
// (hot requests carry no query at all, so nothing is allocated for them).
func (r *httpRequest) queryGet(key string) string {
	if r.queryRaw == "" {
		return ""
	}
	if r.queryParsed == nil {
		q, _ := url.ParseQuery(r.queryRaw)
		r.queryParsed = q
	}
	return r.queryParsed.Get(key)
}

// reset clears a request for reuse on the same connection.
func (r *httpRequest) reset() {
	r.method = ""
	r.path = ""
	r.queryRaw = ""
	r.queryParsed = nil
	r.headers = httpHeaders{}
	r.body = r.body[:0]
}

type httpResponse struct {
	status      int
	contentType string
	extra       map[string]string
	body        []byte
}

var bufReaderPool = sync.Pool{
	New: func() any { return bufio.NewReaderSize(nil, 32768) },
}

var headerBufPool = sync.Pool{
	New: func() any { return &bytes.Buffer{} },
}

// serve accepts connections on ln until done closes.
func serve(ln net.Listener, st *Store, token string, done chan struct{}) {
	go func() {
		<-done
		ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go handleConn(conn, st, token)
	}
}

func handleConn(conn net.Conn, st *Store, token string) {
	defer conn.Close()
	br := bufReaderPool.Get().(*bufio.Reader)
	br.Reset(conn)
	defer bufReaderPool.Put(br)
	req := &httpRequest{}
	for {
		req.reset()
		if err := readRequest(conn, br, req); err != nil {
			return
		}
		resp := route(st, token, req)
		if !writeResponse(conn, req, resp) {
			return
		}
		if req.headers.connection == "close" {
			return
		}
	}
}

// readRequest parses one HTTP/1.1 request from the buffered reader.
func readRequest(conn net.Conn, br *bufio.Reader, req *httpRequest) error {
	line, err := readLine(br)
	if err != nil {
		return err
	}
	// METHOD SP TARGET SP VERSION — split without allocating.
	sp1 := strings.IndexByte(line, ' ')
	sp2 := strings.LastIndexByte(line, ' ')
	if sp1 < 0 || sp2 <= sp1 {
		return errBadRequest
	}
	req.method = line[:sp1]
	rawTarget := line[sp1+1 : sp2]
	if i := strings.IndexByte(rawTarget, '?'); i >= 0 {
		req.path = rawTarget[:i]
		req.queryRaw = rawTarget[i+1:]
	} else {
		req.path = rawTarget
	}
	// Headers.
	for {
		hline, err := readLine(br)
		if err != nil {
			return err
		}
		if hline == "" {
			break
		}
		if i := strings.IndexByte(hline, ':'); i > 0 {
			req.headers.set(lowerHeaderName(hline[:i]), strings.TrimSpace(hline[i+1:]))
		}
	}
	// Expect: 100-continue — answer the interim reply before reading the
	// body, or the client waits forever.
	if strings.Contains(req.headers.expect, "100-continue") {
		if _, err := conn.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n")); err != nil {
			return err
		}
	}
	// Body.
	if req.headers.transferEncoding == "chunked" {
		body, err := readChunked(br)
		if err != nil {
			return err
		}
		req.body = body
	} else if cl := req.headers.contentLength; cl != "" {
		n, err := strconv.Atoi(cl)
		if err != nil || n < 0 || n > 64<<20 {
			return errBadRequest
		}
		if n > 0 {
			if cap(req.body) < n {
				req.body = make([]byte, n)
			} else {
				req.body = req.body[:n]
			}
			if _, err := io.ReadFull(br, req.body); err != nil {
				return err
			}
		}
	}
	return nil
}

// lowerHeaderName lowercases an ASCII header name into a stack buffer.
func lowerHeaderName(s string) string {
	var buf [64]byte
	if len(s) > len(buf) {
		return strings.ToLower(s)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		buf[i] = c
	}
	return string(buf[:len(s)])
}

var errBadRequest = &simpleError{"bad request"}

type simpleError struct{ msg string }

func (e *simpleError) Error() string { return e.msg }

// readLine reads one CRLF-terminated line without the terminator.
func readLine(br *bufio.Reader) (string, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimSuffix(line, "\n")
	line = strings.TrimSuffix(line, "\r")
	return line, nil
}

// readChunked reads a chunked transfer-encoded body.
func readChunked(br *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		sizeLine, err := readLine(br)
		if err != nil {
			return nil, err
		}
		if i := strings.IndexByte(sizeLine, ';'); i >= 0 {
			sizeLine = sizeLine[:i]
		}
		n, err := strconv.ParseInt(strings.TrimSpace(sizeLine), 16, 64)
		if err != nil || n < 0 || n > 64<<20 {
			return nil, errBadRequest
		}
		if n == 0 {
			// trailing headers until a blank line
			for {
				l, err := readLine(br)
				if err != nil {
					return nil, err
				}
				if l == "" {
					break
				}
			}
			return out, nil
		}
		chunk := make([]byte, n)
		if _, err := io.ReadFull(br, chunk); err != nil {
			return nil, err
		}
		out = append(out, chunk...)
		// trailing CRLF after the chunk
		if _, err := readLine(br); err != nil {
			return nil, err
		}
	}
}

// writeResponse writes one response. Returns false when the connection
// should close.
func writeResponse(conn net.Conn, req *httpRequest, resp *httpResponse) bool {
	buf := headerBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer headerBufPool.Put(buf)
	buf.WriteString("HTTP/1.1 ")
	buf.WriteString(strconv.Itoa(resp.status))
	buf.WriteByte(' ')
	buf.WriteString(statusText(resp.status))
	buf.WriteString("\r\nContent-Type: ")
	buf.WriteString(resp.contentType)
	buf.WriteString("\r\nContent-Length: ")
	buf.WriteString(strconv.Itoa(len(resp.body)))
	buf.WriteString("\r\nX-GitHub-Request-Id: req-")
	buf.WriteString(strconv.FormatUint(nextReqID(), 10))
	buf.WriteString("\r\n")
	for k, v := range resp.extra {
		buf.WriteString(k)
		buf.WriteString(": ")
		buf.WriteString(v)
		buf.WriteString("\r\n")
	}
	if req.headers.connection == "close" {
		buf.WriteString("Connection: close\r\n")
	} else {
		buf.WriteString("Connection: keep-alive\r\n")
	}
	buf.WriteString("\r\n")
	buf.Write(resp.body)
	_, err := conn.Write(buf.Bytes())
	return err == nil
}

var reqIDCounter uint64

func nextReqID() uint64 { return atomic.AddUint64(&reqIDCounter, 1) }

func statusText(code int) string {
	switch code {
	case 200:
		return "OK"
	case 201:
		return "Created"
	case 202:
		return "Accepted"
	case 204:
		return "No Content"
	case 400:
		return "Bad Request"
	case 401:
		return "Unauthorized"
	case 404:
		return "Not Found"
	case 405:
		return "Method Not Allowed"
	case 409:
		return "Conflict"
	case 422:
		return "Unprocessable Entity"
	default:
		return "OK"
	}
}

// ---- routing ----

func jsonResp(status int, body []byte) *httpResponse {
	return &httpResponse{status: status, contentType: "application/json", body: body}
}

func jsonErr(status int, msg string) *httpResponse {
	b := make([]byte, 0, 32+len(msg))
	b = append(b, `{"message":`...)
	b = jsonStr(b, msg)
	b = append(b, '}')
	return jsonResp(status, b)
}

// route dispatches one request. Handlers in rest.go / graphql.go / mcp.go
// assume the store lock is already held by the dispatcher: GETs run under
// RLock, mutations under Lock. Git handlers follow the same rule.
func route(st *Store, token string, req *httpRequest) *httpResponse {
	if req.path == "/healthz" {
		if req.method != "GET" {
			return jsonErr(404, "Not Found")
		}
		return jsonResp(200, []byte(`{"ok":true}`))
	}
	// Authorization on every API surface except /healthz.
	if token != "" && (strings.HasPrefix(req.path, "/api/") || req.path == "/mcp" || isGitPath(req.path)) {
		if req.headers.authorization != "Bearer "+token {
			return jsonErr(401, "Bad credentials")
		}
	}
	if strings.HasPrefix(req.path, "/api/graphql") {
		if req.method != "POST" {
			return jsonErr(404, "Not Found")
		}
		st.mu.Lock()
		defer st.mu.Unlock()
		return handleGraphQL(st, req)
	}
	if req.path == "/mcp" {
		if req.method != "POST" {
			return jsonErr(404, "Not Found")
		}
		st.mu.Lock()
		defer st.mu.Unlock()
		return handleMCP(st, req)
	}
	if strings.HasPrefix(req.path, "/api/v3") {
		return routeREST(st, req)
	}
	if isGitPath(req.path) {
		return routeGit(st, req)
	}
	return jsonErr(404, "Not Found")
}

// isGitPath reports whether path looks like /{owner}/{repo}/info/refs or
// /{owner}/{repo}/git-upload-pack or /{owner}/{repo}/git-receive-pack.
func isGitPath(path string) bool {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	if len(segs) == 4 && segs[2] == "info" && segs[3] == "refs" {
		return true
	}
	if len(segs) != 3 {
		return false
	}
	switch segs[2] {
	case "git-upload-pack", "git-receive-pack":
		return true
	}
	return false
}

// routeGit handles the git smart-HTTP endpoints. Git URLs carry a ".git"
// suffix on the repo segment (/<owner>/<repo>.git/...); it is stripped
// before the repo lookup.
func routeGit(st *Store, req *httpRequest) *httpResponse {
	segs := strings.Split(strings.Trim(req.path, "/"), "/")
	stripGitSuffix := func(name string) string {
		return strings.TrimSuffix(name, ".git")
	}
	// GET /{owner}/{repo}/info/refs?service=...
	if len(segs) == 4 && segs[2] == "info" && segs[3] == "refs" {
		if req.method != "GET" {
			return jsonErr(404, "Not Found")
		}
		service := req.queryGet("service")
		st.mu.RLock()
		defer st.mu.RUnlock()
		r := st.getRepo(segs[0], stripGitSuffix(segs[1]))
		if r == nil {
			return jsonErr(404, "Not Found")
		}
		switch service {
		case "git-upload-pack":
			if req.headers.gitProtocol == "version=2" {
				return &httpResponse{status: 200, contentType: "application/x-git-upload-pack-advertisement", body: advertiseUploadV2(r)}
			}
			return &httpResponse{status: 200, contentType: "application/x-git-upload-pack-advertisement", body: advertiseUploadV0(r)}
		case "git-receive-pack":
			return &httpResponse{status: 200, contentType: "application/x-git-receive-pack-advertisement", body: advertiseReceiveV0(r)}
		default:
			return jsonErr(400, "Unknown service")
		}
	}
	if len(segs) == 3 {
		owner, repo := segs[0], stripGitSuffix(segs[1])
		switch segs[2] {
		case "git-upload-pack":
			if req.method != "POST" {
				return jsonErr(404, "Not Found")
			}
			st.mu.RLock()
			defer st.mu.RUnlock()
			r := st.getRepo(owner, repo)
			if r == nil {
				return jsonErr(404, "Not Found")
			}
			body, status := handleUploadPack(r, req.body, req.headers.gitProtocol == "version=2")
			if status != 200 {
				return jsonErr(status, "Bad request")
			}
			return &httpResponse{status: 200, contentType: "application/x-git-upload-pack-result", body: body}
		case "git-receive-pack":
			if req.method != "POST" {
				return jsonErr(404, "Not Found")
			}
			st.mu.Lock()
			defer st.mu.Unlock()
			r := st.getRepo(owner, repo)
			if r == nil {
				return jsonErr(404, "Not Found")
			}
			body, status := handleReceivePack(r, req.body)
			if status != 200 {
				return jsonErr(status, "Bad request")
			}
			return &httpResponse{status: 200, contentType: "application/x-git-receive-pack-result", body: body}
		}
	}
	return jsonErr(404, "Not Found")
}
