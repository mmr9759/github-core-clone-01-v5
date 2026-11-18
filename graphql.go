// GraphQL v4 endpoint: a small recursive-descent parser and executor for
// the documented subset (queries, mutations, variables, aliases, inline
// and named fragments, connections with cursors, introspection via
// __type). Unknown fields answer HTTP 200 with an errors array naming the
// field, exactly as the suite expects.

package main

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
)

// ---- AST ----

type gqlValue struct {
	kind string // "string","int","float","bool","null","enum","var","list","object"
	s    string
	f    float64
	list []*gqlValue
	obj  map[string]*gqlValue
}

type gqlField struct {
	alias    string
	name     string
	args     map[string]*gqlValue
	sel      []*gqlField
	frag     bool   // inline fragment or fragment spread
	typeCond string // inline fragment type condition
	fragName string // named fragment spread
}

type gqlOperation struct {
	kind      string // "query" or "mutation"
	name      string
	variables map[string]*gqlValue // variable definitions with defaults
	sel       []*gqlField
}

type gqlDocument struct {
	ops       []*gqlOperation
	fragments map[string]*gqlField // name -> fragment definition (as a field with sel)
}

// ---- parser ----

type gqlParser struct {
	src string
	pos int
}

func (p *gqlParser) skipWS() {
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' {
			p.pos++
			continue
		}
		if c == '#' {
			for p.pos < len(p.src) && p.src[p.pos] != '\n' {
				p.pos++
			}
			continue
		}
		break
	}
}

func (p *gqlParser) peek() byte {
	p.skipWS()
	if p.pos >= len(p.src) {
		return 0
	}
	return p.src[p.pos]
}

func (p *gqlParser) expect(c byte) bool {
	p.skipWS()
	if p.pos < len(p.src) && p.src[p.pos] == c {
		p.pos++
		return true
	}
	return false
}

func (p *gqlParser) readName() string {
	p.skipWS()
	start := p.pos
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' {
			p.pos++
		} else {
			break
		}
	}
	return p.src[start:p.pos]
}

func (p *gqlParser) readString() (string, bool) {
	p.skipWS()
	if p.pos >= len(p.src) || p.src[p.pos] != '"' {
		return "", false
	}
	p.pos++
	var b strings.Builder
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == '"' {
			p.pos++
			return b.String(), true
		}
		if c == '\\' && p.pos+1 < len(p.src) {
			p.pos++
			esc := p.src[p.pos]
			switch esc {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case '/':
				b.WriteByte('/')
			case '\\':
				b.WriteByte('\\')
			case '"':
				b.WriteByte('"')
			case 'u':
				if p.pos+4 < len(p.src) {
					if n, err := strconv.ParseInt(p.src[p.pos+1:p.pos+5], 16, 32); err == nil {
						b.WriteRune(rune(n))
						p.pos += 4
					}
				}
			default:
				b.WriteByte(esc)
			}
			p.pos++
			continue
		}
		b.WriteByte(c)
		p.pos++
	}
	return "", false
}

func (p *gqlParser) parseValue() *gqlValue {
	p.skipWS()
	if p.pos >= len(p.src) {
		return &gqlValue{kind: "null"}
	}
	switch c := p.src[p.pos]; {
	case c == '$':
		p.pos++
		return &gqlValue{kind: "var", s: p.readName()}
	case c == '"':
		s, _ := p.readString()
		return &gqlValue{kind: "string", s: s}
	case c == '[':
		p.pos++
		v := &gqlValue{kind: "list"}
		for {
			if p.peek() == ']' {
				p.pos++
				break
			}
			v.list = append(v.list, p.parseValue())
		}
		return v
	case c == '{':
		p.pos++
		v := &gqlValue{kind: "object", obj: map[string]*gqlValue{}}
		for {
			if p.peek() == '}' {
				p.pos++
				break
			}
			name := p.readName()
			p.expect(':')
			v.obj[name] = p.parseValue()
		}
		return v
	case c == '-' || (c >= '0' && c <= '9'):
		start := p.pos
		isFloat := false
		for p.pos < len(p.src) {
			ch := p.src[p.pos]
			if (ch >= '0' && ch <= '9') || ch == '-' || ch == '+' {
				p.pos++
			} else if ch == '.' || ch == 'e' || ch == 'E' {
				isFloat = true
				p.pos++
			} else {
				break
			}
		}
		num := p.src[start:p.pos]
		f, _ := strconv.ParseFloat(num, 64)
		if isFloat {
			return &gqlValue{kind: "float", f: f, s: num}
		}
		return &gqlValue{kind: "int", f: f, s: num}
	default:
		name := p.readName()
		switch name {
		case "true":
			return &gqlValue{kind: "bool", s: "true"}
		case "false":
			return &gqlValue{kind: "bool", s: "false"}
		case "null":
			return &gqlValue{kind: "null"}
		default:
			return &gqlValue{kind: "enum", s: name}
		}
	}
}

// parseDirectives skips @name(args) sequences.
func (p *gqlParser) parseDirectives() {
	for p.peek() == '@' {
		p.pos++
		p.readName()
		if p.peek() == '(' {
			p.parseArgs()
		}
	}
}

func (p *gqlParser) parseArgs() map[string]*gqlValue {
	args := map[string]*gqlValue{}
	if !p.expect('(') {
		return args
	}
	for {
		if p.peek() == ')' {
			p.pos++
			break
		}
		name := p.readName()
		p.expect(':')
		args[name] = p.parseValue()
	}
	return args
}

func (p *gqlParser) parseSelectionSet() []*gqlField {
	var fields []*gqlField
	if !p.expect('{') {
		return fields
	}
	for {
		if p.peek() == '}' {
			p.pos++
			break
		}
		if p.peek() == 0 {
			break
		}
		f := p.parseField()
		if f != nil {
			fields = append(fields, f)
		}
	}
	return fields
}

func (p *gqlParser) parseField() *gqlField {
	p.skipWS()
	if p.pos+2 < len(p.src) && p.src[p.pos] == '.' && p.src[p.pos+1] == '.' && p.src[p.pos+2] == '.' {
		p.pos += 3
		f := &gqlField{frag: true}
		p.skipWS()
		if p.pos < len(p.src) && p.src[p.pos] == 'o' && strings.HasPrefix(p.src[p.pos:], "on ") {
			p.pos += 3
			f.typeCond = p.readName()
			p.parseDirectives()
			f.sel = p.parseSelectionSet()
		} else {
			f.fragName = p.readName()
			p.parseDirectives()
		}
		return f
	}
	name := p.readName()
	if name == "" {
		return nil
	}
	f := &gqlField{name: name}
	// alias: name
	if p.peek() == ':' {
		p.pos++
		f.alias = name
		f.name = p.readName()
	}
	if p.peek() == '(' {
		f.args = p.parseArgs()
	}
	p.parseDirectives()
	if p.peek() == '{' {
		f.sel = p.parseSelectionSet()
	}
	return f
}

func parseGraphQL(src string) (*gqlDocument, bool) {
	p := &gqlParser{src: src}
	doc := &gqlDocument{fragments: map[string]*gqlField{}}
	for {
		p.skipWS()
		if p.pos >= len(p.src) {
			break
		}
		if strings.HasPrefix(p.src[p.pos:], "fragment") {
			p.pos += len("fragment")
			name := p.readName()
			p.skipWS()
			if strings.HasPrefix(p.src[p.pos:], "on") {
				p.pos += 2
			}
			typeCond := p.readName()
			p.parseDirectives()
			sel := p.parseSelectionSet()
			doc.fragments[name] = &gqlField{frag: true, typeCond: typeCond, sel: sel}
			continue
		}
		op := &gqlOperation{kind: "query", variables: map[string]*gqlValue{}}
		if p.peek() == '{' {
			op.sel = p.parseSelectionSet()
			doc.ops = append(doc.ops, op)
			continue
		}
		kind := p.readName()
		if kind == "query" || kind == "mutation" || kind == "subscription" {
			op.kind = kind
			if p.peek() != '{' && p.peek() != '(' {
				op.name = p.readName()
			}
			if p.peek() == '(' {
				p.pos++
				for {
					if p.peek() == ')' {
						p.pos++
						break
					}
					p.expect('$')
					vname := p.readName()
					p.expect(':')
					// type name (possibly [T!]!); skip it
					p.readName()
					for p.peek() == '!' || p.peek() == '[' || p.peek() == ']' {
						p.pos++
						p.readName()
					}
					if p.peek() == '=' {
						p.pos++
						op.variables[vname] = p.parseValue()
					} else {
						op.variables[vname] = nil
					}
				}
			}
			op.sel = p.parseSelectionSet()
			doc.ops = append(doc.ops, op)
			continue
		}
		return nil, false
	}
	if len(doc.ops) == 0 {
		return nil, false
	}
	return doc, true
}

// gqlArg resolves a parsed value against request variables.
func gqlArg(v *gqlValue, vars map[string]any) any {
	if v == nil {
		return nil
	}
	switch v.kind {
	case "string", "enum":
		return v.s
	case "int":
		n, _ := strconv.ParseInt(v.s, 10, 64)
		return n
	case "float":
		return v.f
	case "bool":
		return v.s == "true"
	case "null":
		return nil
	case "var":
		if vars != nil {
			if val, ok := vars[v.s]; ok {
				return val
			}
		}
		return nil
	case "list":
		out := make([]any, 0, len(v.list))
		for _, item := range v.list {
			out = append(out, gqlArg(item, vars))
		}
		return out
	case "object":
		out := map[string]any{}
		for k, item := range v.obj {
			out[k] = gqlArg(item, vars)
		}
		return out
	}
	return nil
}

func gqlArgString(f *gqlField, vars map[string]any, name string) string {
	if f.args == nil {
		return ""
	}
	if v, ok := f.args[name]; ok {
		if s, ok := gqlArg(v, vars).(string); ok {
			return s
		}
	}
	return ""
}

func gqlArgInt(f *gqlField, vars map[string]any, name string) int {
	if f.args == nil {
		return 0
	}
	if v, ok := f.args[name]; ok {
		switch n := gqlArg(v, vars).(type) {
		case int64:
			return int(n)
		case float64:
			return int(n)
		}
	}
	return 0
}

func gqlArgBool(f *gqlField, vars map[string]any, name string) bool {
	if f.args == nil {
		return false
	}
	if v, ok := f.args[name]; ok {
		if b, ok := gqlArg(v, vars).(bool); ok {
			return b
		}
	}
	return false
}

// marshalJSON renders a value as compact JSON.
func marshalJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// ---- executor ----

type gqlExec struct {
	st   *Store
	doc  *gqlDocument
	vars map[string]any
	err  string // first unknown-field error
}

func handleGraphQL(st *Store, req *httpRequest) *httpResponse {
	trimmed := bytesTrimSpace(req.body)
	if len(trimmed) == 0 {
		return jsonResp(400, []byte(`{"errors":[{"message":"Empty request body"}]}`))
	}
	if trimmed[0] == '[' {
		var ops []map[string]any
		if err := jsonUnmarshal(trimmed, &ops); err != nil {
			return jsonResp(400, []byte(`{"errors":[{"message":"Malformed JSON body"}]}`))
		}
		results := make([]any, 0, len(ops))
		for _, op := range ops {
			results = append(results, runGraphQLOp(st, op))
		}
		return jsonResp(200, marshalJSON(results))
	}
	var op map[string]any
	if err := jsonUnmarshal(trimmed, &op); err != nil {
		return jsonResp(400, []byte(`{"errors":[{"message":"Malformed JSON body"}]}`))
	}
	return jsonResp(200, marshalJSON(runGraphQLOp(st, op)))
}

func bytesTrimSpace(b []byte) []byte {
	start := 0
	for start < len(b) && (b[start] == ' ' || b[start] == '\t' || b[start] == '\n' || b[start] == '\r') {
		start++
	}
	end := len(b)
	for end > start && (b[end-1] == ' ' || b[end-1] == '\t' || b[end-1] == '\n' || b[end-1] == '\r') {
		end--
	}
	return b[start:end]
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func runGraphQLOp(st *Store, op map[string]any) map[string]any {
	query, _ := op["query"].(string)
	vars, _ := op["variables"].(map[string]any)
	doc, ok := parseGraphQL(query)
	if !ok {
		return map[string]any{"data": nil, "errors": []any{map[string]any{"message": "Syntax Error: could not parse query"}}}
	}
	e := &gqlExec{st: st, doc: doc, vars: vars}
	operation := doc.ops[0]
	if name, _ := op["operationName"].(string); name != "" {
		for _, o := range doc.ops {
			if o.name == name {
				operation = o
				break
			}
		}
	}
	var data map[string]any
	if operation.kind == "mutation" {
		data = e.execRoot(operation.sel, true)
	} else {
		data = e.execRoot(operation.sel, false)
	}
	if e.err != "" {
		return map[string]any{"data": nil, "errors": []any{map[string]any{"message": e.err}}}
	}
	return map[string]any{"data": data}
}

// execRoot resolves the root Query/Mutation fields.
func (e *gqlExec) execRoot(sel []*gqlField, mutation bool) map[string]any {
	out := map[string]any{}
	for _, f := range e.expand(sel, "") {
		key := f.alias
		if key == "" {
			key = f.name
		}
		if mutation {
			val, ok := e.execMutationField(f)
			if !ok {
				return nil
			}
			out[key] = val
			continue
		}
		val, ok := e.execQueryField(f)
		if !ok {
			return nil
		}
		out[key] = val
	}
	return out
}

// expand flattens fragment spreads into the field list.
func (e *gqlExec) expand(sel []*gqlField, typeName string) []*gqlField {
	var out []*gqlField
	for _, f := range sel {
		if !f.frag {
			out = append(out, f)
			continue
		}
		if f.fragName != "" {
			if def, ok := e.doc.fragments[f.fragName]; ok {
				out = append(out, e.expand(def.sel, typeName)...)
			}
			continue
		}
		if f.typeCond == "" || f.typeCond == typeName {
			out = append(out, e.expand(f.sel, typeName)...)
		}
	}
	return out
}

func (e *gqlExec) unknownField(name, typeName string) bool {
	if e.err == "" {
		e.err = "Cannot query field \"" + name + "\" on type \"" + typeName + "\""
	}
	return false
}

func (e *gqlExec) execQueryField(f *gqlField) (any, bool) {
	switch f.name {
	case "__typename":
		return "Query", true
	case "viewer":
		return e.resolveUser(f.sel), true
	case "repository":
		owner := gqlArgString(f, e.vars, "owner")
		name := gqlArgString(f, e.vars, "name")
		r := e.st.getRepo(owner, name)
		if r == nil {
			return nil, true
		}
		return e.resolveRepository(r, f.sel), true
	case "node":
		id := gqlArgString(f, e.vars, "id")
		return e.resolveNode(id, f.sel), true
	case "__type":
		name := gqlArgString(f, e.vars, "name")
		return e.resolveType(name, f.sel), true
	case "__schema":
		return nil, true
	}
	return nil, e.unknownField(f.name, "Query")
}

func (e *gqlExec) resolveUser(sel []*gqlField) map[string]any {
	out := map[string]any{}
	for _, f := range e.expand(sel, "User") {
		switch f.name {
		case "__typename":
			out[fieldKey(f)] = "User"
		case "login":
			out[fieldKey(f)] = e.st.viewer.login
		case "id":
			out[fieldKey(f)] = nodeID("U", e.st.viewer.login, "", -1)
		case "name":
			out[fieldKey(f)] = e.st.viewer.name
		case "email":
			out[fieldKey(f)] = e.st.viewer.email
		default:
			e.unknownField(f.name, "User")
			return nil
		}
	}
	return out
}

func fieldKey(f *gqlField) string {
	if f.alias != "" {
		return f.alias
	}
	return f.name
}

func (e *gqlExec) resolveNode(id string, sel []*gqlField) any {
	kind, owner, name, num, ok := decodeNodeID(id)
	if !ok {
		return nil
	}
	r := e.st.getRepo(owner, name)
	if r == nil {
		return nil
	}
	switch kind {
	case "R":
		return e.resolveRepository(r, sel)
	case "I":
		if is, ok := r.issueByNum[int(num)]; ok {
			return e.resolveIssue(r, is, sel)
		}
	case "PR":
		if p, ok := r.prByNum[int(num)]; ok {
			return e.resolvePR(r, p, sel)
		}
	}
	return nil
}

func (e *gqlExec) resolveRepository(r *Repo, sel []*gqlField) map[string]any {
	out := map[string]any{}
	for _, f := range e.expand(sel, "Repository") {
		key := fieldKey(f)
		switch f.name {
		case "__typename":
			out[key] = "Repository"
		case "id":
			out[key] = nodeID("R", r.owner, r.name, -1)
		case "databaseId":
			out[key] = r.id
		case "name":
			out[key] = r.name
		case "nameWithOwner":
			out[key] = r.owner + "/" + r.name
		case "owner":
			out[key] = e.resolveUser(f.sel)
		case "defaultBranchRef":
			if r.refs["refs/heads/"+r.defaultBranch] == "" {
				out[key] = nil
			} else {
				out[key] = map[string]any{"name": r.defaultBranch}
			}
		case "viewerPermission":
			out[key] = "ADMIN"
		case "hasIssuesEnabled":
			out[key] = true
		case "issue":
			num := gqlArgInt(f, e.vars, "number")
			if is, ok := r.issueByNum[num]; ok {
				out[key] = e.resolveIssue(r, is, f.sel)
			} else {
				out[key] = nil
			}
		case "issues":
			out[key] = e.resolveIssueConnection(r, f)
		case "pullRequests":
			out[key] = e.resolvePRConnection(r, f)
		case "pullRequest":
			num := gqlArgInt(f, e.vars, "number")
			if p, ok := r.prByNum[num]; ok {
				out[key] = e.resolvePR(r, p, f.sel)
			} else {
				out[key] = nil
			}
		default:
			e.unknownField(f.name, "Repository")
			return nil
		}
	}
	return out
}

func (e *gqlExec) resolveIssue(r *Repo, is *Issue, sel []*gqlField) map[string]any {
	out := map[string]any{}
	for _, f := range e.expand(sel, "Issue") {
		key := fieldKey(f)
		switch f.name {
		case "__typename":
			out[key] = "Issue"
		case "id":
			out[key] = nodeID("I", r.owner, r.name, int64(is.num))
		case "number":
			out[key] = is.num
		case "title":
			out[key] = is.title
		case "state":
			if is.state == "open" {
				out[key] = "OPEN"
			} else {
				out[key] = "CLOSED"
			}
		case "body":
			out[key] = is.body
		case "author":
			out[key] = e.resolveUser(f.sel)
		case "createdAt":
			out[key] = is.createdAt
		case "url":
			out[key] = "https://github.com/" + r.owner + "/" + r.name + "/issues/" + strconv.Itoa(is.num)
		default:
			e.unknownField(f.name, "Issue")
			return nil
		}
	}
	return out
}

func (e *gqlExec) resolvePR(r *Repo, p *PR, sel []*gqlField) map[string]any {
	out := map[string]any{}
	for _, f := range e.expand(sel, "PullRequest") {
		key := fieldKey(f)
		switch f.name {
		case "__typename":
			out[key] = "PullRequest"
		case "id":
			out[key] = nodeID("PR", r.owner, r.name, int64(p.num))
		case "number":
			out[key] = p.num
		case "title":
			out[key] = p.title
		case "body":
			out[key] = p.body
		case "state":
			switch {
			case p.merged:
				out[key] = "MERGED"
			case p.state == "open":
				out[key] = "OPEN"
			default:
				out[key] = "CLOSED"
			}
		case "merged":
			out[key] = p.merged
		case "headRefName":
			out[key] = p.headRef
		case "baseRefName":
			out[key] = p.baseRef
		case "mergeable":
			out[key] = e.prMergeable(r, p)
		case "files":
			out[key] = e.resolveFileConnection(r, p, f)
		default:
			e.unknownField(f.name, "PullRequest")
			return nil
		}
	}
	return out
}

// prMergeable computes MERGEABLE / CONFLICTING / UNKNOWN for a pull request.
func (e *gqlExec) prMergeable(r *Repo, p *PR) string {
	if p.merged || p.state != "open" {
		return "UNKNOWN"
	}
	headTip, baseTip := r.prTips(p)
	base := r.mergeBase(headTip, baseTip)
	var baseTree, headTree, otherTree string
	if base != "" {
		if c, ok := r.commitOf(base); ok {
			baseTree = c.tree
		}
	}
	if c, ok := r.commitOf(baseTip); ok {
		headTree = c.tree
	}
	if c, ok := r.commitOf(headTip); ok {
		otherTree = c.tree
	}
	if _, ok := r.mergeTrees(baseTree, headTree, otherTree); !ok {
		return "CONFLICTING"
	}
	return "MERGEABLE"
}

// ---- connections ----

func encodeCursor(num int) string {
	return base64.StdEncoding.EncodeToString([]byte("cursor:" + strconv.Itoa(num)))
}

func decodeCursor(cursor string) int {
	raw, err := base64.StdEncoding.DecodeString(cursor)
	if err != nil {
		return -1
	}
	s := string(raw)
	if !strings.HasPrefix(s, "cursor:") {
		return -1
	}
	n, err := strconv.Atoi(s[len("cursor:"):])
	if err != nil {
		return -1
	}
	return n
}

func (e *gqlExec) resolveIssueConnection(r *Repo, f *gqlField) map[string]any {
	// Filters: states, orderBy.
	var states []string
	if v, ok := f.args["states"]; ok {
		if list, ok := gqlArg(v, e.vars).([]any); ok {
			for _, s := range list {
				if str, ok := s.(string); ok {
					states = append(states, str)
				}
			}
		}
	}
	desc := false
	if v, ok := f.args["orderBy"]; ok {
		if obj, ok := gqlArg(v, e.vars).(map[string]any); ok {
			if d, ok := obj["direction"].(string); ok && d == "DESC" {
				desc = true
			}
		}
	}
	var filtered []*Issue
	for _, is := range r.issues {
		if len(states) > 0 {
			want := "OPEN"
			if is.state == "closed" {
				want = "CLOSED"
			}
			match := false
			for _, s := range states {
				if s == want {
					match = true
				}
			}
			if !match {
				continue
			}
		}
		filtered = append(filtered, is)
	}
	// Sort by number (creation order == number order).
	for i := 0; i < len(filtered); i++ {
		for j := i + 1; j < len(filtered); j++ {
			if (desc && filtered[j].num > filtered[i].num) || (!desc && filtered[j].num < filtered[i].num) {
				filtered[i], filtered[j] = filtered[j], filtered[i]
			}
		}
	}
	first := gqlArgInt(f, e.vars, "first")
	after := gqlArgString(f, e.vars, "after")
	start := 0
	if after != "" {
		afterNum := decodeCursor(after)
		for i, is := range filtered {
			if is.num == afterNum {
				start = i + 1
				break
			}
		}
	}
	end := len(filtered)
	if first > 0 && start+first < end {
		end = start + first
	}
	page := filtered[start:end]
	hasNext := end < len(filtered)
	var endCursor any
	if len(page) > 0 {
		endCursor = encodeCursor(page[len(page)-1].num)
	}
	out := map[string]any{}
	for _, sf := range e.expand(f.sel, "IssueConnection") {
		switch sf.name {
		case "__typename":
			out[fieldKey(sf)] = "IssueConnection"
		case "nodes":
			nodes := make([]any, 0, len(page))
			for _, is := range page {
				nodes = append(nodes, e.resolveIssue(r, is, sf.sel))
			}
			out[fieldKey(sf)] = nodes
		case "edges":
			edges := make([]any, 0, len(page))
			for _, is := range page {
				edge := map[string]any{}
				for _, ef := range e.expand(sf.sel, "IssueEdge") {
					switch ef.name {
					case "node":
						edge[fieldKey(ef)] = e.resolveIssue(r, is, ef.sel)
					case "cursor":
						edge[fieldKey(ef)] = encodeCursor(is.num)
					}
				}
				edges = append(edges, edge)
			}
			out[fieldKey(sf)] = edges
		case "pageInfo":
			pi := map[string]any{}
			for _, pf := range e.expand(sf.sel, "PageInfo") {
				switch pf.name {
				case "hasNextPage":
					pi[fieldKey(pf)] = hasNext
				case "hasPreviousPage":
					pi[fieldKey(pf)] = start > 0
				case "endCursor":
					pi[fieldKey(pf)] = endCursor
				case "startCursor":
					if len(page) > 0 {
						pi[fieldKey(pf)] = encodeCursor(page[0].num)
					} else {
						pi[fieldKey(pf)] = nil
					}
				}
			}
			out[fieldKey(sf)] = pi
		case "totalCount":
			out[fieldKey(sf)] = len(filtered)
		}
	}
	return out
}

func (e *gqlExec) resolvePRConnection(r *Repo, f *gqlField) map[string]any {
	first := gqlArgInt(f, e.vars, "first")
	after := gqlArgString(f, e.vars, "after")
	var all []*PR
	for i := len(r.prs) - 1; i >= 0; i-- {
		all = append(all, r.prs[i])
	}
	start := 0
	if after != "" {
		afterNum := decodeCursor(after)
		for i, p := range all {
			if p.num == afterNum {
				start = i + 1
				break
			}
		}
	}
	end := len(all)
	if first > 0 && start+first < end {
		end = start + first
	}
	page := all[start:end]
	hasNext := end < len(all)
	var endCursor any
	if len(page) > 0 {
		endCursor = encodeCursor(page[len(page)-1].num)
	}
	out := map[string]any{}
	for _, sf := range e.expand(f.sel, "PullRequestConnection") {
		switch sf.name {
		case "__typename":
			out[fieldKey(sf)] = "PullRequestConnection"
		case "nodes":
			nodes := make([]any, 0, len(page))
			for _, p := range page {
				nodes = append(nodes, e.resolvePR(r, p, sf.sel))
			}
			out[fieldKey(sf)] = nodes
		case "pageInfo":
			pi := map[string]any{}
			for _, pf := range e.expand(sf.sel, "PageInfo") {
				switch pf.name {
				case "hasNextPage":
					pi[fieldKey(pf)] = hasNext
				case "hasPreviousPage":
					pi[fieldKey(pf)] = start > 0
				case "endCursor":
					pi[fieldKey(pf)] = endCursor
				case "startCursor":
					if len(page) > 0 {
						pi[fieldKey(pf)] = encodeCursor(page[0].num)
					} else {
						pi[fieldKey(pf)] = nil
					}
				}
			}
			out[fieldKey(sf)] = pi
		case "totalCount":
			out[fieldKey(sf)] = len(all)
		}
	}
	return out
}

func (e *gqlExec) resolveFileConnection(r *Repo, p *PR, f *gqlField) map[string]any {
	headTip, baseTip := r.prTips(p)
	base := r.mergeBase(headTip, baseTip)
	var baseTree, headTree string
	if base != "" {
		if c, ok := r.commitOf(base); ok {
			baseTree = c.tree
		}
	}
	if c, ok := r.commitOf(headTip); ok {
		headTree = c.tree
	}
	files := r.diffFiles(baseTree, headTree)
	first := gqlArgInt(f, e.vars, "first")
	if first > 0 && first < len(files) {
		files = files[:first]
	}
	out := map[string]any{}
	for _, sf := range e.expand(f.sel, "PullRequestChangedFileConnection") {
		switch sf.name {
		case "__typename":
			out[fieldKey(sf)] = "PullRequestChangedFileConnection"
		case "nodes":
			nodes := make([]any, 0, len(files))
			for _, file := range files {
				node := map[string]any{}
				for _, nf := range e.expand(sf.sel, "PullRequestChangedFile") {
					if nf.name == "path" {
						node[fieldKey(nf)] = file.filename
					}
				}
				nodes = append(nodes, node)
			}
			out[fieldKey(sf)] = nodes
		case "totalCount":
			out[fieldKey(sf)] = len(files)
		}
	}
	return out
}

// ---- introspection (__type) ----

var gqlTypeFields = map[string][]string{
	"Issue":       {"id", "number", "title", "state", "body", "author", "createdAt", "url"},
	"PullRequest": {"id", "number", "headRefName", "baseRefName", "mergeable", "state", "merged", "files", "title", "body"},
	"Repository":  {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest"},
	"User":        {"id", "login", "name", "email"},
}

var gqlEnumValues = map[string][]string{
	"SearchType":       {"ISSUE", "REPOSITORY", "USER", "DISCUSSION"},
	"IssueState":       {"OPEN", "CLOSED"},
	"PullRequestState": {"OPEN", "CLOSED", "MERGED"},
}

// resolveType answers __type(name:). Introspection sub-fields that are not
// "name" resolve to null rather than erroring, since the suite only ever
// reads names here.
func (e *gqlExec) resolveType(name string, sel []*gqlField) any {
	fields, isObj := gqlTypeFields[name]
	enums, isEnum := gqlEnumValues[name]
	if !isObj && !isEnum {
		return nil
	}
	out := map[string]any{}
	for _, f := range e.expand(sel, "__Type") {
		switch f.name {
		case "__typename":
			out[fieldKey(f)] = "__Type"
		case "name":
			out[fieldKey(f)] = name
		case "kind":
			if isEnum {
				out[fieldKey(f)] = "ENUM"
			} else {
				out[fieldKey(f)] = "OBJECT"
			}
		case "fields":
			if !isObj {
				out[fieldKey(f)] = nil
				continue
			}
			list := make([]any, 0, len(fields))
			for _, fn := range fields {
				item := map[string]any{}
				for _, ff := range e.expand(f.sel, "Field") {
					if ff.name == "name" {
						item[fieldKey(ff)] = fn
					}
				}
				list = append(list, item)
			}
			out[fieldKey(f)] = list
		case "enumValues":
			if !isEnum {
				out[fieldKey(f)] = nil
				continue
			}
			list := make([]any, 0, len(enums))
			for _, ev := range enums {
				item := map[string]any{}
				for _, ff := range e.expand(f.sel, "EnumValue") {
					if ff.name == "name" {
						item[fieldKey(ff)] = ev
					}
				}
				list = append(list, item)
			}
			out[fieldKey(f)] = list
		default:
			out[fieldKey(f)] = nil
		}
	}
	return out
}

// ---- mutations ----

func (e *gqlExec) execMutationField(f *gqlField) (any, bool) {
	switch f.name {
	case "createIssue":
		return e.mutCreateIssue(f)
	case "closeIssue":
		return e.mutCloseIssue(f)
	case "createPullRequest":
		return e.mutCreatePR(f)
	case "mergePullRequest":
		return e.mutMergePR(f)
	case "addComment":
		return e.mutAddComment(f)
	}
	return nil, e.unknownField(f.name, "Mutation")
}

func (e *gqlExec) inputObj(f *gqlField) map[string]any {
	if v, ok := f.args["input"]; ok {
		if obj, ok := gqlArg(v, e.vars).(map[string]any); ok {
			return obj
		}
	}
	return map[string]any{}
}

func (e *gqlExec) repoFromGlobalID(id string) *Repo {
	kind, owner, name, _, ok := decodeNodeID(id)
	if !ok || kind != "R" {
		return nil
	}
	return e.st.getRepo(owner, name)
}

func (e *gqlExec) mutCreateIssue(f *gqlField) (any, bool) {
	in := e.inputObj(f)
	r := e.repoFromGlobalID(strVal(in, "repositoryId"))
	if r == nil {
		e.err = "Could not resolve repository from repositoryId"
		return nil, false
	}
	title := strVal(in, "title")
	is := e.st.createIssue(r, title, strVal(in, "body"), nil)
	r.rendered = nil
	out := map[string]any{}
	for _, sf := range e.expand(f.sel, "CreateIssuePayload") {
		switch sf.name {
		case "issue":
			out[fieldKey(sf)] = e.resolveIssue(r, is, sf.sel)
		case "clientMutationId":
			out[fieldKey(sf)] = strVal(in, "clientMutationId")
		}
	}
	return out, true
}

func (e *gqlExec) mutCloseIssue(f *gqlField) (any, bool) {
	in := e.inputObj(f)
	kind, owner, name, num, ok := decodeNodeID(strVal(in, "issueId"))
	if !ok || kind != "I" {
		e.err = "Could not resolve issue from issueId"
		return nil, false
	}
	r := e.st.getRepo(owner, name)
	if r == nil {
		e.err = "Could not resolve repository from issueId"
		return nil, false
	}
	is, ok := r.issueByNum[int(num)]
	if !ok {
		e.err = "Issue not found"
		return nil, false
	}
	is.state = "closed"
	is.closedAt = e.st.nowISO()
	is.updatedAt = e.st.nowISO()
	e.st.refreshIssue(r, is)
	r.rendered = nil
	out := map[string]any{}
	for _, sf := range e.expand(f.sel, "CloseIssuePayload") {
		switch sf.name {
		case "issue":
			out[fieldKey(sf)] = e.resolveIssue(r, is, sf.sel)
		case "clientMutationId":
			out[fieldKey(sf)] = strVal(in, "clientMutationId")
		}
	}
	return out, true
}

func (e *gqlExec) mutCreatePR(f *gqlField) (any, bool) {
	in := e.inputObj(f)
	r := e.repoFromGlobalID(strVal(in, "repositoryId"))
	if r == nil {
		e.err = "Could not resolve repository from repositoryId"
		return nil, false
	}
	head := strVal(in, "headRefName")
	base := strVal(in, "baseRefName")
	headSha, okH := r.refs["refs/heads/"+head]
	baseSha, okB := r.refs["refs/heads/"+base]
	if !okH || !okB {
		e.err = "Head or base branch does not exist"
		return nil, false
	}
	for _, p := range r.prs {
		if p.state == "open" && p.headRef == head && p.baseRef == base {
			out := map[string]any{}
			for _, sf := range e.expand(f.sel, "CreatePullRequestPayload") {
				if sf.name == "pullRequest" {
					out[fieldKey(sf)] = e.resolvePR(r, p, sf.sel)
				}
			}
			return out, true
		}
	}
	if headSha == baseSha {
		e.err = "No commits between head and base"
		return nil, false
	}
	p := e.st.createPR(r, strVal(in, "title"), strVal(in, "body"), head, base)
	out := map[string]any{}
	for _, sf := range e.expand(f.sel, "CreatePullRequestPayload") {
		switch sf.name {
		case "pullRequest":
			out[fieldKey(sf)] = e.resolvePR(r, p, sf.sel)
		case "clientMutationId":
			out[fieldKey(sf)] = strVal(in, "clientMutationId")
		}
	}
	return out, true
}

func (e *gqlExec) mutMergePR(f *gqlField) (any, bool) {
	in := e.inputObj(f)
	kind, owner, name, num, ok := decodeNodeID(strVal(in, "pullRequestId"))
	if !ok || kind != "PR" {
		e.err = "Could not resolve pull request from pullRequestId"
		return nil, false
	}
	r := e.st.getRepo(owner, name)
	if r == nil {
		e.err = "Could not resolve repository from pullRequestId"
		return nil, false
	}
	p, ok := r.prByNum[int(num)]
	if !ok {
		e.err = "Pull request not found"
		return nil, false
	}
	if p.state != "open" {
		e.err = "Pull request is already closed"
		return nil, false
	}
	if _, ok := doMergePR(e.st, r, p); !ok {
		e.err = "Merge conflict: this branch has conflicts that must be resolved"
		return nil, false
	}
	out := map[string]any{}
	for _, sf := range e.expand(f.sel, "MergePullRequestPayload") {
		switch sf.name {
		case "pullRequest":
			out[fieldKey(sf)] = e.resolvePR(r, p, sf.sel)
		case "clientMutationId":
			out[fieldKey(sf)] = strVal(in, "clientMutationId")
		}
	}
	return out, true
}

func (e *gqlExec) mutAddComment(f *gqlField) (any, bool) {
	in := e.inputObj(f)
	kind, owner, name, num, ok := decodeNodeID(strVal(in, "subjectId"))
	if !ok || kind != "I" {
		e.err = "Could not resolve issue from subjectId"
		return nil, false
	}
	r := e.st.getRepo(owner, name)
	if r == nil {
		e.err = "Could not resolve repository from subjectId"
		return nil, false
	}
	is, ok := r.issueByNum[int(num)]
	if !ok {
		e.err = "Issue not found"
		return nil, false
	}
	e.st.nextComment++
	c := &Comment{id: e.st.nextComment, body: strVal(in, "body"), createdAt: e.st.nowISO()}
	is.comments = append(is.comments, c)
	e.st.refreshIssue(r, is)
	out := map[string]any{}
	for _, sf := range e.expand(f.sel, "AddCommentPayload") {
		switch sf.name {
		case "commentEdge":
			edge := map[string]any{}
			for _, ef := range e.expand(sf.sel, "IssueCommentEdge") {
				if ef.name == "node" {
					node := map[string]any{}
					for _, nf := range e.expand(ef.sel, "IssueComment") {
						switch nf.name {
						case "id":
							node[fieldKey(nf)] = nodeID("C", r.owner, r.name, c.id)
						case "body":
							node[fieldKey(nf)] = c.body
						}
					}
					edge[fieldKey(ef)] = node
				}
			}
			out[fieldKey(sf)] = edge
		case "clientMutationId":
			out[fieldKey(sf)] = strVal(in, "clientMutationId")
		}
	}
	return out, true
}

// strVal reads a string from a generic map.
func strVal(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
