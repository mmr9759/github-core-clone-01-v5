// MCP endpoint (POST /mcp): Streamable HTTP JSON-RPC 2.0, modern era
// 2026-07-28. tools/list returns one prebuilt constant so two calls are
// byte-identical; tools/call maps the six canonical tools onto the same
// store the REST and GraphQL layers use.

package main

import (
	"encoding/json"
	"strconv"
)

const mcpToolsListJSON = `{"tools":[` +
	`{"name":"create_repo","description":"Create a new repository","inputSchema":{"type":"object","properties":{"name":{"type":"string"},"description":{"type":"string"},"private":{"type":"boolean"}},"required":["name"]}},` +
	`{"name":"get_repo","description":"Get a repository by owner and name","inputSchema":{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"}},"required":["owner","repo"]}},` +
	`{"name":"create_issue","description":"Create an issue in a repository","inputSchema":{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"},"title":{"type":"string"},"body":{"type":"string"}},"required":["owner","repo","title"]}},` +
	`{"name":"list_issues","description":"List issues in a repository","inputSchema":{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"},"state":{"type":"string"}},"required":["owner","repo"]}},` +
	`{"name":"create_pull_request","description":"Create a pull request","inputSchema":{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"},"title":{"type":"string"},"head":{"type":"string"},"base":{"type":"string"},"body":{"type":"string"}},"required":["owner","repo","title","head","base"]}},` +
	`{"name":"merge_pull_request","description":"Merge a pull request","inputSchema":{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"},"pull_number":{"type":"integer"}},"required":["owner","repo","pull_number"]}}` +
	`]}`

func handleMCP(st *Store, req *httpRequest) *httpResponse {
	var rpc map[string]any
	if err := json.Unmarshal(req.body, &rpc); err != nil {
		return jsonResp(400, []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error"}}`))
	}
	method, _ := rpc["method"].(string)
	id := rpc["id"]
	if method == "" {
		return jsonErr(400, "Bad Request")
	}
	// Notifications: no reply body, 202.
	if len(method) >= 14 && method[:14] == "notifications/" {
		return &httpResponse{status: 202, contentType: "application/json", body: []byte{}}
	}
	switch method {
	case "server/discover":
		result := map[string]any{
			"supportedVersions": []string{"2026-07-28", "2025-06-18", "2024-11-05"},
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"serverInfo":        map[string]any{"name": "forge", "version": "1.0.0"},
			"_meta": map[string]any{
				"io.modelcontextprotocol/serverInfo": map[string]any{"name": "forge", "version": "1.0.0"},
			},
		}
		return mcpResult(id, result)
	case "initialize":
		result := map[string]any{
			"protocolVersion": "2026-07-28",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "forge", "version": "1.0.0"},
		}
		return mcpResult(id, result)
	case "ping":
		return mcpResult(id, map[string]any{})
	case "tools/list":
		// Prebuilt constant: two calls are byte-identical.
		body := append([]byte(`{"jsonrpc":"2.0","id":`), mcpIDJSON(id)...)
		body = append(body, []byte(`,"result":`)...)
		body = append(body, mcpToolsListJSON...)
		body = append(body, '}')
		return jsonResp(200, body)
	case "tools/call":
		return mcpCall(st, id, rpc)
	}
	return mcpError(id, -32601, "Method not found: "+method)
}

func mcpIDJSON(id any) []byte {
	if id == nil {
		return []byte("null")
	}
	b, _ := json.Marshal(id)
	return b
}

func mcpResult(id any, result map[string]any) *httpResponse {
	envelope := map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
	return jsonResp(200, marshalJSON(envelope))
}

func mcpError(id any, code int, message string) *httpResponse {
	envelope := map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}}
	return jsonResp(200, marshalJSON(envelope))
}

// mcpCall executes one tools/call request.
func mcpCall(st *Store, id any, rpc map[string]any) *httpResponse {
	params, _ := rpc["params"].(map[string]any)
	name, _ := params["name"].(string)
	args, _ := params["arguments"].(map[string]any)
	if args == nil {
		args = map[string]any{}
	}
	// Required-argument validation per tool.
	required := map[string][]string{
		"create_repo":         {"name"},
		"get_repo":            {"owner", "repo"},
		"create_issue":        {"owner", "repo", "title"},
		"list_issues":         {"owner", "repo"},
		"create_pull_request": {"owner", "repo", "title", "head", "base"},
		"merge_pull_request":  {"owner", "repo", "pull_number"},
	}
	reqs, known := required[name]
	if !known {
		return mcpError(id, -32602, "Unknown tool: "+name)
	}
	for _, r := range reqs {
		if _, ok := args[r]; !ok {
			return mcpError(id, -32602, "Missing required argument: "+r)
		}
	}
	owner := strVal(args, "owner")
	repoName := strVal(args, "repo")
	switch name {
	case "create_repo":
		repoName := strVal(args, "name")
		if _, exists := st.repos[repoKey(st.viewer.login, repoName)]; exists {
			return mcpToolError(id, "Repository already exists: "+repoName)
		}
		r := st.createRepo(repoName, boolVal(args, "private"), strVal(args, "description"))
		return mcpToolOK(id, repoJSON(st, r))
	case "get_repo":
		r := st.getRepo(owner, repoName)
		if r == nil {
			return mcpToolError(id, "Repository not found: "+owner+"/"+repoName)
		}
		return mcpToolOK(id, repoJSON(st, r))
	case "create_issue":
		r := st.getRepo(owner, repoName)
		if r == nil {
			return mcpToolError(id, "Repository not found: "+owner+"/"+repoName)
		}
		is := st.createIssue(r, strVal(args, "title"), strVal(args, "body"), nil)
		r.rendered = nil
		return mcpToolOK(id, is.rendered)
	case "list_issues":
		r := st.getRepo(owner, repoName)
		if r == nil {
			return mcpToolError(id, "Repository not found: "+owner+"/"+repoName)
		}
		state := strVal(args, "state")
		if state == "" {
			state = "open"
		}
		var b []byte
		b = append(b, '[')
		first := true
		for _, is := range r.issues {
			if state != "all" && is.state != state {
				continue
			}
			if !first {
				b = append(b, ',')
			}
			first = false
			b = append(b, is.rendered...)
		}
		b = append(b, ']')
		return mcpToolOK(id, b)
	case "create_pull_request":
		r := st.getRepo(owner, repoName)
		if r == nil {
			return mcpToolError(id, "Repository not found: "+owner+"/"+repoName)
		}
		head := stripOwnerPrefix(strVal(args, "head"))
		base := stripOwnerPrefix(strVal(args, "base"))
		headSha, okH := r.refs["refs/heads/"+head]
		baseSha, okB := r.refs["refs/heads/"+base]
		if !okH || !okB {
			return mcpToolError(id, "Head or base branch does not exist")
		}
		for _, p := range r.prs {
			if p.state == "open" && p.headRef == head && p.baseRef == base {
				return mcpToolError(id, "A pull request already exists for this head and base")
			}
		}
		if headSha == baseSha {
			return mcpToolError(id, "No commits between head and base")
		}
		p := st.createPR(r, strVal(args, "title"), strVal(args, "body"), head, base)
		return mcpToolOK(id, renderPRJSON(st, r, p))
	case "merge_pull_request":
		r := st.getRepo(owner, repoName)
		if r == nil {
			return mcpToolError(id, "Repository not found: "+owner+"/"+repoName)
		}
		num := intArg(args, "pull_number")
		p, ok := r.prByNum[num]
		if !ok {
			return mcpToolError(id, "Pull request not found: "+strconv.Itoa(num))
		}
		if p.state != "open" {
			return mcpToolError(id, "Pull request is already closed")
		}
		mergeSha, ok := doMergePR(st, r, p)
		if !ok {
			return mcpToolError(id, "Merge conflict: this branch has conflicts that must be resolved")
		}
		body := []byte(`{"sha":` + strconv.Quote(mergeSha) + `,"merged":true,"number":` + strconv.Itoa(p.num) + `}`)
		return mcpToolOK(id, body)
	}
	return mcpError(id, -32602, "Unknown tool: "+name)
}

func intArg(m map[string]any, key string) int {
	if v, ok := m[key]; ok {
		switch n := v.(type) {
		case float64:
			return int(n)
		case int:
			return n
		case string:
			if i, err := strconv.Atoi(n); err == nil {
				return i
			}
		}
	}
	return 0
}

func boolVal(m map[string]any, key string) bool {
	if v, ok := m[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
}

// mcpToolOK renders a successful tool result: text JSON + structuredContent.
func mcpToolOK(id any, bodyJSON []byte) *httpResponse {
	var structured any
	_ = json.Unmarshal(bodyJSON, &structured)
	result := map[string]any{
		"content": []any{
			map[string]any{"type": "text", "text": string(bodyJSON)},
		},
		"structuredContent": structured,
	}
	return mcpResult(id, result)
}

// mcpToolError renders a domain failure as an isError tool result.
func mcpToolError(id any, message string) *httpResponse {
	result := map[string]any{
		"content": []any{
			map[string]any{"type": "text", "text": message},
		},
		"structuredContent": map[string]any{"error": message},
		"isError":           true,
	}
	return mcpResult(id, result)
}
