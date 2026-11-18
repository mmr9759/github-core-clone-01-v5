// In-memory store: repositories, git objects, issues, pull requests.
//
// One process, one store, no disk: the trial's pod is fresh every run and the
// suite is the only client. A single RWMutex guards the whole store; hot
// reads (GET one issue, clone packs) run under a read lock so fifty
// concurrent clones proceed in parallel, while a push or an issue create
// takes the write lock only for the few microseconds its mutation needs.
// Every rendered JSON body a hot path serves is pre-rendered at mutation time
// and cached on the object, so a read is a memory copy, not a re-render.

package main

import (
	"encoding/base64"
	"encoding/json"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Object types, as git's pack type codes.
const (
	objCommit = 1
	objTree   = 2
	objBlob   = 3
	objTag    = 4
)

// Obj is one git object: its type and its exact bytes.
type Obj struct {
	typ  byte
	data []byte
}

// Label is an issue label.
type Label struct {
	id    int64
	name  string
	color string
	desc  string
}

// Comment is an issue comment.
type Comment struct {
	id        int64
	body      string
	createdAt string
}

// Issue is one repository issue.
type Issue struct {
	id         int64
	num        int
	title      string
	body       string
	state      string // "open" or "closed"
	labels     []*Label
	createdSeq int64
	createdAt  string
	updatedAt  string
	closedAt   string
	comments   []*Comment
	rendered   []byte // cached JSON rendering, rebuilt on change
}

// PRFile is one changed file of a pull request.
type PRFile struct {
	filename  string
	status    string // "added", "modified", "removed"
	additions int
	deletions int
	changes   int
}

// PR is one pull request.
type PR struct {
	id         int64
	num        int
	title      string
	body       string
	state      string // "open" or "closed"
	merged     bool
	mergedAt   string
	mergeSha   string
	headRef    string
	baseRef    string
	headSha    string
	baseSha    string
	createdSeq int64
	createdAt  string
}

// Repo is one repository: its git store plus its issues and pull requests.
type Repo struct {
	id            int
	owner         string
	name          string
	private       bool
	desc          string
	defaultBranch string
	createdSeq    int64
	createdAt     string
	updatedAt     string
	refs          map[string]string
	objects       map[string]*Obj
	issues        []*Issue
	issueByNum    map[int]*Issue
	prs           []*PR
	prByNum       map[int]*PR
	nextNum       int
	labels        map[string]*Label
	gen           uint64
	adCache       map[string][]byte
	packCache     map[string][]byte
	rendered      []byte // cached repo JSON
}

// Store is the whole forge state.
type Store struct {
	mu          sync.RWMutex
	repos       map[string]*Repo
	order       []*Repo // creation order
	nextRepoID  int
	nextIssueID int64
	nextComment int64
	nextLabelID int64
	nowBase     int64 // trial-now unix seconds
	nowCounter  int64 // milliseconds added per timestamp, strictly increasing
	seqCounter  int64
	viewer      scenarioUser
	reqCounter  uint64
}

func newStore(nowBase int64, viewer scenarioUser) *Store {
	return &Store{
		repos:      map[string]*Repo{},
		nextRepoID: 1,
		nowBase:    nowBase,
		viewer:     viewer,
	}
}

// nextSeq hands out a strictly increasing sequence number (creation order).
func (s *Store) nextSeq() int64 {
	s.seqCounter++
	return s.seqCounter
}

// nowISO returns a deterministic ISO-8601 timestamp: the trial's now plus a
// strictly increasing millisecond counter, so two runs that make the same
// requests in the same order mint the same timestamps.
func (s *Store) nowISO() string {
	s.nowCounter++
	sec := s.nowBase + s.nowCounter/1000
	ms := s.nowCounter % 1000
	return time.Unix(sec, ms*1e6).UTC().Format("2006-01-02T15:04:05.000Z")
}

// repoKey is the store key for owner/name.
func repoKey(owner, name string) string { return owner + "/" + name }

// getRepo returns the repo owned by the viewer, or nil.
func (s *Store) getRepo(owner, name string) *Repo {
	if owner != s.viewer.login {
		return nil
	}
	return s.repos[repoKey(owner, name)]
}

// createRepo makes a repository owned by the viewer; nil when the name exists.
func (s *Store) createRepo(name string, private bool, desc string) *Repo {
	if _, ok := s.repos[repoKey(s.viewer.login, name)]; ok {
		return nil
	}
	now := s.nowISO()
	r := &Repo{
		id:            s.nextRepoID,
		owner:         s.viewer.login,
		name:          name,
		private:       private,
		desc:          desc,
		defaultBranch: "main",
		createdSeq:    s.nextSeq(),
		createdAt:     now,
		updatedAt:     now,
		refs:          map[string]string{},
		objects:       map[string]*Obj{},
		issueByNum:    map[int]*Issue{},
		prByNum:       map[int]*PR{},
		nextNum:       1,
		labels:        map[string]*Label{},
		adCache:       map[string][]byte{},
		packCache:     map[string][]byte{},
	}
	s.nextRepoID++
	s.repos[repoKey(r.owner, name)] = r
	s.order = append(s.order, r)
	r.rendered = renderRepoJSON(s, r)
	return r
}

// deleteRepo removes a repository.
func (s *Store) deleteRepo(r *Repo) {
	delete(s.repos, repoKey(r.owner, r.name))
	for i, other := range s.order {
		if other == r {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}

// bump invalidates a repo's cached advertisements and packs.
func (r *Repo) bump() {
	r.gen++
	packCacheMu.Lock()
	r.packCache = map[string][]byte{}
	packCacheMu.Unlock()
	r.adCache = map[string][]byte{}
	r.rendered = nil
}

// nodeID builds an opaque Relay-style global id.
func nodeID(kind, owner, name string, num int64) string {
	raw := kind + ":" + owner + "/" + name
	if num >= 0 {
		raw += ":" + strconv.FormatInt(num, 10)
	}
	return base64.StdEncoding.EncodeToString([]byte(raw))
}

// decodeNodeID parses a global id back into its parts. Ids are
// "<kind>:<owner>/<name>" with an optional ":<num>" suffix.
func decodeNodeID(id string) (kind, owner, name string, num int64, ok bool) {
	raw, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return "", "", "", 0, false
	}
	s := string(raw)
	colon := indexOf(s, ":")
	if colon < 0 {
		return "", "", "", 0, false
	}
	kind = s[:colon]
	rest := s[colon+1:]
	// Optional ":<num>" suffix.
	if i := len(rest) - 1; i >= 0 {
		j := i
		for j >= 0 && rest[j] >= '0' && rest[j] <= '9' {
			j--
		}
		if j >= 0 && rest[j] == ':' && j < i {
			n, err := strconv.ParseInt(rest[j+1:], 10, 64)
			if err == nil {
				num = n
				rest = rest[:j]
			}
		}
	}
	slash := indexOf(rest, "/")
	if slash < 0 {
		return "", "", "", 0, false
	}
	owner, name = rest[:slash], rest[slash+1:]
	return kind, owner, name, num, true
}

func splitN(s, sep string, n int) []string {
	var out []string
	for len(out) < n-1 {
		i := indexOf(s, sep)
		if i < 0 {
			break
		}
		out = append(out, s[:i])
		s = s[i+len(sep):]
	}
	out = append(out, s)
	return out
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// sortedRefNames returns the repo's ref names in git order.
func (r *Repo) sortedRefNames() []string {
	names := make([]string, 0, len(r.refs))
	for name := range r.refs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// jsonStr appends a JSON-encoded string to dst.
func jsonStr(dst []byte, s string) []byte {
	b, _ := json.Marshal(s)
	return append(dst, b...)
}

// jsonBytes appends a base64 JSON string of raw bytes.
func jsonB64(dst []byte, b []byte) []byte {
	return jsonStr(dst, base64.StdEncoding.EncodeToString(b))
}

// ---- JSON rendering (hand-rolled, allocation-light, deterministic order) ----

func renderRepoJSON(s *Store, r *Repo) []byte {
	open := 0
	for _, is := range r.issues {
		if is.state == "open" {
			open++
		}
	}
	b := make([]byte, 0, 512)
	b = append(b, '{')
	b = append(b, `"id":`...)
	b = strconv.AppendInt(b, int64(r.id), 10)
	b = append(b, `,"node_id":`...)
	b = jsonStr(b, nodeID("R", r.owner, r.name, -1))
	b = append(b, `,"name":`...)
	b = jsonStr(b, r.name)
	b = append(b, `,"full_name":`...)
	b = jsonStr(b, r.owner+"/"+r.name)
	b = append(b, `,"owner":{"login":`...)
	b = jsonStr(b, r.owner)
	b = append(b, `,"id":`...)
	b = strconv.AppendInt(b, int64(s.viewer.id), 10)
	b = append(b, `,"type":"User"},"private":`...)
	b = strconv.AppendBool(b, r.private)
	b = append(b, `,"description":`...)
	b = jsonStr(b, r.desc)
	b = append(b, `,"default_branch":`...)
	b = jsonStr(b, r.defaultBranch)
	b = append(b, `,"html_url":`...)
	b = jsonStr(b, "https://github.com/"+r.owner+"/"+r.name)
	b = append(b, `,"url":`...)
	b = jsonStr(b, "https://api.github.com/repos/"+r.owner+"/"+r.name)
	b = append(b, `,"created_at":`...)
	b = jsonStr(b, r.createdAt)
	b = append(b, `,"updated_at":`...)
	b = jsonStr(b, r.updatedAt)
	b = append(b, `,"has_issues":true,"has_projects":false,"has_wiki":false,"forks":0,"watchers":0,"open_issues":`...)
	b = strconv.AppendInt(b, int64(open), 10)
	b = append(b, `,"permissions":{"admin":true,"maintain":true,"push":true,"triage":true,"pull":true}}`...)
	return b
}

func renderLabelJSON(l *Label) []byte {
	b := make([]byte, 0, 128)
	b = append(b, '{')
	b = append(b, `"id":`...)
	b = strconv.AppendInt(b, l.id, 10)
	b = append(b, `,"name":`...)
	b = jsonStr(b, l.name)
	b = append(b, `,"color":`...)
	b = jsonStr(b, l.color)
	b = append(b, `,"description":`...)
	b = jsonStr(b, l.desc)
	b = append(b, `,"default":false}`...)
	return b
}

func renderCommentJSON(s *Store, r *Repo, c *Comment) []byte {
	b := make([]byte, 0, 256)
	b = append(b, '{')
	b = append(b, `"id":`...)
	b = strconv.AppendInt(b, c.id, 10)
	b = append(b, `,"node_id":`...)
	b = jsonStr(b, nodeID("C", r.owner, r.name, c.id))
	b = append(b, `,"body":`...)
	b = jsonStr(b, c.body)
	b = append(b, `,"user":{"login":`...)
	b = jsonStr(b, s.viewer.login)
	b = append(b, `,"id":`...)
	b = strconv.AppendInt(b, int64(s.viewer.id), 10)
	b = append(b, `,"type":"User"},"created_at":`...)
	b = jsonStr(b, c.createdAt)
	b = append(b, `,"updated_at":`...)
	b = jsonStr(b, c.createdAt)
	b = append(b, `}`...)
	return b
}

func renderIssueJSON(s *Store, r *Repo, is *Issue) []byte {
	b := make([]byte, 0, 512)
	b = append(b, '{')
	b = append(b, `"id":`...)
	b = strconv.AppendInt(b, is.id, 10)
	b = append(b, `,"node_id":`...)
	b = jsonStr(b, nodeID("I", r.owner, r.name, int64(is.num)))
	b = append(b, `,"number":`...)
	b = strconv.AppendInt(b, int64(is.num), 10)
	b = append(b, `,"title":`...)
	b = jsonStr(b, is.title)
	b = append(b, `,"body":`...)
	b = jsonStr(b, is.body)
	b = append(b, `,"state":`...)
	b = jsonStr(b, is.state)
	b = append(b, `,"labels":[`...)
	for i, l := range is.labels {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, renderLabelJSON(l)...)
	}
	b = append(b, `],"user":{"login":`...)
	b = jsonStr(b, s.viewer.login)
	b = append(b, `,"id":`...)
	b = strconv.AppendInt(b, int64(s.viewer.id), 10)
	b = append(b, `,"type":"User"},"assignee":null,"created_at":`...)
	b = jsonStr(b, is.createdAt)
	b = append(b, `,"updated_at":`...)
	b = jsonStr(b, is.updatedAt)
	b = append(b, `,"closed_at":`...)
	if is.closedAt == "" {
		b = append(b, "null"...)
	} else {
		b = jsonStr(b, is.closedAt)
	}
	b = append(b, `,"comments":`...)
	b = strconv.AppendInt(b, int64(len(is.comments)), 10)
	b = append(b, `,"html_url":`...)
	b = jsonStr(b, "https://github.com/"+r.owner+"/"+r.name+"/issues/"+strconv.Itoa(is.num))
	b = append(b, `,"url":`...)
	b = jsonStr(b, "https://api.github.com/repos/"+r.owner+"/"+r.name+"/issues/"+strconv.Itoa(is.num))
	b = append(b, `}`...)
	return b
}

func renderPRJSON(s *Store, r *Repo, p *PR) []byte {
	b := make([]byte, 0, 640)
	b = append(b, '{')
	b = append(b, `"id":`...)
	b = strconv.AppendInt(b, p.id, 10)
	b = append(b, `,"node_id":`...)
	b = jsonStr(b, nodeID("PR", r.owner, r.name, int64(p.num)))
	b = append(b, `,"number":`...)
	b = strconv.AppendInt(b, int64(p.num), 10)
	b = append(b, `,"title":`...)
	b = jsonStr(b, p.title)
	b = append(b, `,"body":`...)
	b = jsonStr(b, p.body)
	b = append(b, `,"state":`...)
	b = jsonStr(b, p.state)
	b = append(b, `,"merged":`...)
	b = strconv.AppendBool(b, p.merged)
	b = append(b, `,"merged_at":`...)
	if p.mergedAt == "" {
		b = append(b, "null"...)
	} else {
		b = jsonStr(b, p.mergedAt)
	}
	b = append(b, `,"merge_commit_sha":`...)
	if p.mergeSha == "" {
		b = append(b, "null"...)
	} else {
		b = jsonStr(b, p.mergeSha)
	}
	b = append(b, `,"head":{"ref":`...)
	b = jsonStr(b, p.headRef)
	b = append(b, `,"sha":`...)
	b = jsonStr(b, p.headSha)
	b = append(b, `,"label":`...)
	b = jsonStr(b, r.owner+":"+p.headRef)
	b = append(b, `},"base":{"ref":`...)
	b = jsonStr(b, p.baseRef)
	b = append(b, `,"sha":`...)
	b = jsonStr(b, p.baseSha)
	b = append(b, `,"label":`...)
	b = jsonStr(b, r.owner+":"+p.baseRef)
	b = append(b, `},"user":{"login":`...)
	b = jsonStr(b, s.viewer.login)
	b = append(b, `,"id":`...)
	b = strconv.AppendInt(b, int64(s.viewer.id), 10)
	b = append(b, `,"type":"User"},"created_at":`...)
	b = jsonStr(b, p.createdAt)
	b = append(b, `,"updated_at":`...)
	b = jsonStr(b, p.createdAt)
	b = append(b, `,"closed_at":`...)
	if p.state == "closed" {
		b = jsonStr(b, p.createdAt)
	} else {
		b = append(b, "null"...)
	}
	b = append(b, `,"draft":false,"mergeable":true,"mergeable_state":"clean","html_url":`...)
	b = jsonStr(b, "https://github.com/"+r.owner+"/"+r.name+"/pull/"+strconv.Itoa(p.num))
	b = append(b, `}`...)
	return b
}

// ---- issue / PR creation ----

func (s *Store) createLabel(r *Repo, name, color string) *Label {
	if l, ok := r.labels[name]; ok {
		return l
	}
	s.nextLabelID++
	l := &Label{id: s.nextLabelID, name: name, color: color}
	r.labels[name] = l
	return l
}

func (s *Store) createIssue(r *Repo, title, body string, labels []*Label) *Issue {
	num := r.nextNum
	r.nextNum++
	s.nextIssueID++
	now := s.nowISO()
	is := &Issue{
		id:         s.nextIssueID,
		num:        num,
		title:      title,
		body:       body,
		state:      "open",
		labels:     labels,
		createdSeq: s.nextSeq(),
		createdAt:  now,
		updatedAt:  now,
	}
	is.rendered = renderIssueJSON(s, r, is)
	r.issues = append(r.issues, is)
	r.issueByNum[num] = is
	return is
}

func (s *Store) createPR(r *Repo, title, body, headRef, baseRef string) *PR {
	num := r.nextNum
	r.nextNum++
	s.nextIssueID++
	now := s.nowISO()
	p := &PR{
		id:         s.nextIssueID,
		num:        num,
		title:      title,
		body:       body,
		state:      "open",
		headRef:    headRef,
		baseRef:    baseRef,
		headSha:    r.refs["refs/heads/"+headRef],
		baseSha:    r.refs["refs/heads/"+baseRef],
		createdSeq: s.nextSeq(),
		createdAt:  now,
	}
	r.prs = append(r.prs, p)
	r.prByNum[num] = p
	return p
}

// refreshIssue re-renders an issue after a mutation.
func (s *Store) refreshIssue(r *Repo, is *Issue) {
	is.rendered = renderIssueJSON(s, r, is)
}
