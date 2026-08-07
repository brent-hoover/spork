// Package web — see MOD-web in avspec.yaml. The web UI is a pure
// HTTP client of the API (may_import is empty): every page renders
// from API responses fetched over the wire, so the browser surface
// can never bypass the API's guards. Handler-level HTML per the
// project decision — no browser driver; scenarios assert on markup.
package web

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"iter"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Server renders the UI against an API base URL.
type Server struct {
	api    string
	client *http.Client
	actor  string // identity id stamped on UI-driven mutations
	// canonicalHosts is the independently configured host allowlist
	// the mutation guard trusts — ALWAYS non-empty (New derives it
	// from the UI's own bind address when none is given). Trusting
	// r.Host would let a DNS-rebound attacker origin satisfy a
	// same-origin comparison against itself (reviews 1875, 1877).
	canonicalHosts map[string]bool
}

// New builds the web handler. actor is the identity UI mutations act
// as — the single-operator system has exactly one human at the
// keyboard.
// New builds the web handler. canonicalHosts is the Host allowlist
// mutations require; callers pass the UI's own listen address (and any
// alias it is reached through). It is NEVER empty in practice — the
// caller's bind address is the minimum — because an empty list would
// reopen the DNS-rebinding path the guard exists to close.
func New(apiBase, actor string, canonicalHosts ...string) *Server {
	hosts := map[string]bool{}
	for _, h := range canonicalHosts {
		if h != "" {
			hosts[h] = true
		}
	}
	return &Server{api: strings.TrimRight(apiBase, "/"), client: &http.Client{}, actor: actor, canonicalHosts: hosts}
}

// Handler routes the UI. Mutating routes pass the same-origin guard:
// a cross-origin form post must never spend the server-configured
// actor's authority (review 1817).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.projects)
	mux.HandleFunc("GET /p/{key}", s.board)
	mux.HandleFunc("POST /p/{key}/i/{num}/move", s.sameOrigin(s.moveCard))
	mux.HandleFunc("GET /p/{key}/i/{num}", s.issue)
	mux.HandleFunc("POST /p/{key}/i/{num}/comment", s.sameOrigin(s.commentIssue))
	mux.HandleFunc("GET /p/{key}/d/{documentId}", s.document)
	mux.HandleFunc("POST /p/{key}/d/{documentId}/comment", s.sameOrigin(s.commentDoc))
	mux.HandleFunc("POST /p/{key}/d/{documentId}/save", s.sameOrigin(s.saveDocVersion))
	mux.HandleFunc("GET /p/{key}/d/{documentId}/poll", s.pollDocument)
	mux.HandleFunc("GET /p/{key}/t/{threadId}", s.thread)
	mux.HandleFunc("GET /p/{key}/r/{reviewId}", s.review)
	mux.HandleFunc("POST /p/{key}/r/{reviewId}/verdict", s.sameOrigin(s.reviewVerdict))
	mux.HandleFunc("POST /p/{key}/r/{reviewId}/comment", s.sameOrigin(s.reviewComment))
	return mux
}

// sameOrigin rejects cross-origin mutations. Browsers send Origin (or
// at least Sec-Fetch-Site) on form posts: an opaque "null" origin, a
// cross-site fetch marker, or an origin whose SCHEME AND HOST differ
// from this server's all refuse. Callers sending neither header are
// non-browser processes — they hold no ambient browser authority to
// launder, and the API itself is equally reachable to them directly.
func (s *Server) sameOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// The Host header is attacker-influenced under DNS rebinding,
		// so it must name a CONFIGURED canonical host before any
		// origin comparison means anything. No allowlist means no
		// mutations — never an implicit trust of r.Host.
		if !s.canonicalHosts[r.Host] {
			http.Error(w, "unrecognized host", http.StatusForbidden)
			return
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			scheme := "http"
			if r.TLS != nil {
				scheme = "https"
			}
			expected := scheme + "://" + r.Host
			if origin == "null" || origin != expected {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
		}
		next(w, r)
	}
}

// get fetches an API path into out.
func (s *Server) get(path string, out any) error {
	resp, err := s.client.Get(s.api + path)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GET %s: %d %s", path, resp.StatusCode, body)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// post sends a mutation with a fresh idempotency key, returning the
// response and its status.
func (s *Server) post(path string, body any) (int, []byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequest(http.MethodPost, s.api+path, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", newKey())
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	return resp.StatusCode, respBody, err
}

// inProject verifies a resource's governing project matches the
// route's :key — a project-A URL must never render or mutate a
// project-B resource. Returns the project id, or writes 404.
func (s *Server) inProject(w http.ResponseWriter, key, resourceProject string) bool {
	p, err := s.projectByKey(key)
	if err != nil {
		http.NotFound(w, nil)
		return false
	}
	if p.ID != resourceProject {
		http.NotFound(w, nil)
		return false
	}
	return true
}

// scanFields decodes the named top-level fields from a response and
// STOPS at the first content-bearing key — never decoding a skipped
// value that could be unbounded. Every record serializes its content
// last (Issue.Body, DocVersion.Content, Thread.transcript), so the
// wanted ids always precede the stop key and the body closes with the
// content unread (review 1895).
func (s *Server) scanFields(path string, want map[string]any, stopKeys ...string) error {
	resp, err := s.client.Get(s.api + path)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %d", path, resp.StatusCode)
	}
	dec := json.NewDecoder(resp.Body)
	if err := expectObject(dec, path); err != nil {
		return err
	}
	return scanObject(dec, want, stopSet(stopKeys))
}

func stopSet(keys []string) map[string]bool {
	stop := map[string]bool{}
	for _, k := range keys {
		stop[k] = true
	}
	return stop
}

func expectObject(dec *json.Decoder, what string) error {
	open, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := open.(json.Delim); !ok || d != '{' {
		return fmt.Errorf("GET %s: not an object", what)
	}
	return nil
}

// scanObject collects the wanted fields of the object the decoder has
// just opened and returns at the first stop key or once everything
// wanted has been found — whichever comes first.
func scanObject(dec *json.Decoder, want map[string]any, stop map[string]bool) error {
	remaining := len(want)
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := keyTok.(string)
		if stop[key] {
			// Content begins here: everything wanted has passed.
			return nil
		}
		dst, wanted := want[key]
		if !wanted {
			// Skipping DOES materialize the value, so the invariant is
			// positional, not incidental: every record puts its
			// ownership ids ahead of any unbounded field (titles,
			// labels, sessions, content), and callers pass a stop key
			// for the first unbounded one. Nothing here may be reached
			// by a scan that has not already found what it wants.
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return err
			}
			continue
		}
		if err := dec.Decode(dst); err != nil {
			return err
		}
		if remaining--; remaining == 0 {
			return nil
		}
	}
	return nil
}

// scanField decodes ONE named field, requiring it to be present.
func (s *Server) scanField(path, field string, dst any, stopKeys ...string) error {
	found := false
	probe := map[string]any{field: dst}
	if err := s.scanFields(path, probe, stopKeys...); err != nil {
		return err
	}
	switch v := dst.(type) {
	case *string:
		found = *v != ""
	default:
		found = true
	}
	if !found {
		return fmt.Errorf("GET %s: no %s field", path, field)
	}
	return nil
}

// docMeta is the bounded document projection: everything the UI needs
// about a document that is not its content.
type docMeta struct {
	Document struct {
		Project string `json:"project"`
	} `json:"document"`
	Version struct {
		Number int64 `json:"number"`
	} `json:"version"`
}

// documentMeta fetches that projection. Scope guards and the live poll
// both go through it, so neither makes the API read a version's
// unbounded content — which stopping the client-side decode could not
// prevent, since the cost was upstream (review 1914).
func (s *Server) documentMeta(documentID string) (docMeta, error) {
	var meta docMeta
	err := s.get("/documents/"+url.PathEscape(documentID)+"/meta", &meta)
	return meta, err
}

// docProject resolves a document's governing project id.
func (s *Server) docProject(documentID string) (string, error) {
	meta, err := s.documentMeta(documentID)
	if err != nil {
		return "", err
	}
	return meta.Document.Project, nil
}

// reviewProject resolves a review's governing project via its issue.
func (s *Server) reviewProject(reviewID string) (string, error) {
	var issueID string
	if err := s.scanField("/reviews/"+url.PathEscape(reviewID), "issue", &issueID); err != nil {
		return "", err
	}
	var project string
	if err := s.scanField("/issues/"+url.PathEscape(issueID), "project", &project, "body"); err != nil {
		return "", err
	}
	return project, nil
}

// threadProject resolves a thread's governing project — its project
// anchor, or its anchored issue's project.
func (s *Server) threadProject(threadID string) (string, error) {
	// BOTH anchors resolve in ONE pass that stops at the transcript —
	// an issue-only thread must not be scanned past its anchors.
	path := "/threads/" + url.PathEscape(threadID)
	var project, issueID string
	// An issue-only thread omits "project" entirely, so the wanted-count
	// never completes and only a stop key ends the scan: session and
	// title both follow the anchors and both precede the transcript,
	// so stopping at the first of them keeps the unbounded title
	// unread as well (review 1900).
	if err := s.scanFields(path, map[string]any{"project": &project, "issue": &issueID}, "session", "title", "transcript"); err != nil {
		return "", err
	}
	if project != "" {
		return project, nil
	}
	if issueID == "" {
		return "", fmt.Errorf("thread %s carries no anchor", threadID)
	}
	var issueProject string
	if err := s.scanField("/issues/"+url.PathEscape(issueID), "project", &issueProject, "body"); err != nil {
		return "", err
	}
	return issueProject, nil
}

// guardDoc/guardReview/guardThread run the scope check for a route.
func (s *Server) guardDoc(w http.ResponseWriter, r *http.Request) bool {
	project, err := s.docProject(r.PathValue("documentId"))
	if err != nil {
		http.NotFound(w, r)
		return false
	}
	return s.inProject(w, r.PathValue("key"), project)
}

func (s *Server) guardReview(w http.ResponseWriter, r *http.Request) bool {
	project, err := s.reviewProject(r.PathValue("reviewId"))
	if err != nil {
		http.NotFound(w, r)
		return false
	}
	return s.inProject(w, r.PathValue("key"), project)
}

func (s *Server) guardThread(w http.ResponseWriter, r *http.Request) bool {
	project, err := s.threadProject(r.PathValue("threadId"))
	if err != nil {
		http.NotFound(w, r)
		return false
	}
	return s.inProject(w, r.PathValue("key"), project)
}

func htmlError(w http.ResponseWriter, err error) {
	http.Error(w, template.HTMLEscapeString(err.Error()), http.StatusBadGateway)
}

type project struct {
	ID   string `json:"id"`
	Key  string `json:"key"`
	Name string `json:"name"`
}

func (s *Server) projectByKey(key string) (project, error) {
	// Archived projects stay READABLE (hidden and frozen, not
	// deleted): route resolution must see them, while the projects
	// nav keeps the default filtered listing. Mutations still reject
	// server-side via the project-archived guard.
	var list []project
	if err := s.get("/projects?includeArchived=true", &list); err != nil {
		return project{}, err
	}
	for _, p := range list {
		if p.Key == key {
			return p, nil
		}
	}
	return project{}, fmt.Errorf("no project with key %q", key)
}

// uiFuncs: pesc path-escapes values embedded in URL path segments —
// the API permits arbitrary project keys, and a / ? or # in one must
// not restructure the route (review 1853). Go's mux decodes segments,
// so escaped keys round-trip.
var uiFuncs = template.FuncMap{"pesc": url.PathEscape}

var projectsTmpl = template.Must(template.New("projects").Funcs(uiFuncs).Parse(`<!doctype html>
<title>sutra</title><h1>Projects</h1><ul>
{{range .}}<li><a href="/p/{{pesc .Key}}">{{.Key}} — {{.Name}}</a></li>{{end}}
</ul>`))

func (s *Server) projects(w http.ResponseWriter, r *http.Request) {
	var list []project
	if err := s.get("/projects", &list); err != nil {
		htmlError(w, err)
		return
	}
	_ = projectsTmpl.Execute(w, list)
}

type issueView struct {
	ID       string  `json:"id"`
	Number   int64   `json:"number"`
	Title    string  `json:"title"`
	Body     *string `json:"body"`
	Status   string  `json:"status"`
	Assignee *string `json:"assignee"`
	Labels   []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"labels"`
}

// statusColumns is the board's fixed column order (AC-kanban-columns).
var statusColumns = []string{"open", "in-progress", "blocked", "deferred", "complete"}

type boardData struct {
	Key       string
	Columns   []boardColumn
	Statuses  []string
	Error     string
	Documents []struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	Threads []threadRef
}

type boardColumn struct {
	Status string
	Cards  []issueView
}

var boardTmpl = template.Must(template.New("board").Funcs(uiFuncs).Parse(`<!doctype html>
<title>{{.Key}} board</title><h1>{{.Key}}</h1>
{{if .Error}}<p class="error" role="alert">{{.Error}}</p>{{end}}
<nav class="project-nav">
<span>Documents:</span> {{range .Documents}}<a href="/p/{{pesc $.Key}}/d/{{.ID}}">{{.Title}}</a> {{end}}
<span>Threads:</span> {{range .Threads}}<a href="/p/{{pesc $.Key}}/t/{{.ID}}">{{.Title}}</a> {{end}}
</nav>
<div class="board">
{{range $col := .Columns}}<section class="column" data-status="{{$col.Status}}"><h2>{{$col.Status}}</h2>
{{range $col.Cards}}<article class="card" draggable="true" data-issue="{{.ID}}">
<a href="/p/{{pesc $.Key}}/i/{{.Number}}">{{$.Key}}-{{.Number}} {{.Title}}</a>
{{if .Assignee}}<span class="assignee">{{.Assignee}}</span>{{end}}
{{range .Labels}}<span class="label">{{.Name}}</span>{{end}}
<form class="move" method="post" action="/p/{{pesc $.Key}}/i/{{.Number}}/move">
{{range $.Statuses}}{{if ne . $col.Status}}<button name="status" value="{{.}}">→ {{.}}</button>{{end}}{{end}}
</form>
</article>{{end}}
</section>{{end}}
</div>
<script>
// Dragging a card to a column posts the SAME move form target — the
// drop is a real transition, never a client-side illusion.
document.querySelectorAll('.card').forEach(function (card) {
  card.addEventListener('dragstart', function (e) {
    e.dataTransfer.setData('text/plain', card.querySelector('form.move').action);
  });
});
document.querySelectorAll('.column').forEach(function (col) {
  col.addEventListener('dragover', function (e) { e.preventDefault(); });
  col.addEventListener('drop', function (e) {
    e.preventDefault();
    var action = e.dataTransfer.getData('text/plain');
    var form = document.createElement('form');
    form.method = 'post';
    form.action = action;
    var input = document.createElement('input');
    input.name = 'status';
    input.value = col.dataset.status;
    form.appendChild(input);
    document.body.appendChild(form);
    form.submit();
  });
});
</script>`))

func (s *Server) renderBoard(w http.ResponseWriter, key, errMsg string) {
	p, err := s.projectByKey(key)
	if err != nil {
		htmlError(w, err)
		return
	}
	// Cards never show bodies, and bodies are unbounded: the listing
	// streams element by element so only card metadata survives —
	// decoding the whole envelope would buffer every body (1883).
	cards := []issueView{}
	listFailed := false
	s.eachEnvelopeArray("/projects/"+p.ID+"/issues", "issues", func(dec *json.Decoder) bool {
		var card issueView
		if err := dec.Decode(&card); err != nil {
			listFailed = true
			return false
		}
		card.Body = nil // never rendered on a card; released immediately
		cards = append(cards, card)
		return true
	}, func() bool { listFailed = true; return false })
	if listFailed {
		htmlError(w, fmt.Errorf("issue listing unavailable"))
		return
	}
	var docsList []struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	if err := s.get("/projects/"+p.ID+"/documents", &docsList); err != nil {
		htmlError(w, err)
		return
	}
	threadRefs := []threadRef{}
	s.eachThreadRef("/threads/search?project="+url.QueryEscape(p.ID), func(t threadRef) bool {
		threadRefs = append(threadRefs, t)
		return true
	})
	data := boardData{Key: key, Statuses: statusColumns, Error: errMsg, Documents: docsList, Threads: threadRefs}
	for _, status := range statusColumns {
		col := boardColumn{Status: status}
		for _, i := range cards {
			if i.Status == status {
				col.Cards = append(col.Cards, i)
			}
		}
		data.Columns = append(data.Columns, col)
	}
	_ = boardTmpl.Execute(w, data)
}

func (s *Server) board(w http.ResponseWriter, r *http.Request) {
	s.renderBoard(w, r.PathValue("key"), "")
}

// moveCard performs the drag as a REAL status transition through the
// API (AC-kanban-drag); a rejection re-renders the board unchanged —
// the card "snaps back" — with the API's error shown.
func (s *Server) moveCard(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	p, err := s.projectByKey(key)
	if err != nil {
		htmlError(w, err)
		return
	}
	if err := r.ParseForm(); err != nil {
		htmlError(w, err)
		return
	}
	target := r.Form.Get("status")
	var page struct {
		Issues []issueView `json:"issues"`
	}
	if err := s.get("/projects/"+p.ID+"/issues?number="+url.QueryEscape(r.PathValue("num")), &page); err != nil {
		htmlError(w, err)
		return
	}
	if len(page.Issues) == 0 {
		htmlError(w, fmt.Errorf("no such issue"))
		return
	}
	transition := map[string]any{"status": target, "actor": s.actor}
	if target == "complete" {
		// A drag to complete is a REAL close: the UI resolves the
		// issue's approved review and names it. With none, the
		// close-requires-review error shows and the card snaps back —
		// the same missing-approval the API's gate reports.
		var approved []struct {
			ID                 string  `json:"id"`
			Revision           int64   `json:"revision"`
			LatestVerdictEvent *string `json:"latest_verdict_event"`
			CloseUsed          *string `json:"close_used"`
			Created            string  `json:"created"`
		}
		if err := s.get("/reviews?issue="+url.QueryEscape(page.Issues[0].ID)+"&state=approved", &approved); err != nil {
			htmlError(w, err)
			return
		}
		// A spent review stays approved; only an UNSPENT approval
		// authorizes a close — pick the newest usable one.
		chosen := -1
		for i, r := range approved {
			if r.CloseUsed != nil || r.LatestVerdictEvent == nil {
				continue
			}
			if chosen < 0 || r.Created > approved[chosen].Created {
				chosen = i
			}
		}
		if chosen < 0 {
			s.renderBoard(w, key, "missing-approval: no unspent approved review authorizes closing this issue")
			return
		}
		transition["review"] = approved[chosen].ID
		transition["review_revision"] = approved[chosen].Revision
		transition["review_verdict_event"] = *approved[chosen].LatestVerdictEvent
	}
	status, body, err := s.post("/issues/"+page.Issues[0].ID+"/status", transition)
	if err != nil {
		htmlError(w, err)
		return
	}
	if status != http.StatusOK {
		var apiErr struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		}
		_ = json.Unmarshal(body, &apiErr)
		s.renderBoard(w, key, apiErr.Code+": "+apiErr.Message)
		return
	}
	s.renderBoard(w, key, "")
}

var issueTmpl = template.Must(template.New("issue").Funcs(uiFuncs).Parse(`<!doctype html>
<title>{{.Key}}-{{.Issue.Number}}</title>
<h1>{{.Key}}-{{.Issue.Number}} {{.Issue.Title}}</h1>
<p class="status">{{.Issue.Status}}</p>
{{if .Issue.Assignee}}<p class="assignee">{{.Issue.Assignee}}</p>{{end}}
{{if .Issue.Body}}<div class="body">{{.Issue.Body}}</div>{{end}}
{{if .Progress}}<p class="progress">{{.Progress}}</p>{{end}}
<section class="documents"><h2>Documents</h2>
{{range .Documents}}<a href="/p/{{pesc $.Key}}/d/{{.ID}}">{{.Title}}</a> {{end}}
</section>
<section class="threads"><h2>Threads</h2>
{{range .Threads}}<a href="/p/{{pesc $.Key}}/t/{{.ID}}">{{.Title}}</a> {{end}}
</section>
<section class="reviews"><h2>Reviews</h2>
{{range .Reviews}}<a href="/p/{{pesc $.Key}}/r/{{.ID}}">review {{.ID}} ({{.State}})</a> {{end}}
</section>
<aside class="discussion"><h2>Comments</h2>
{{range .Comments}}<div class="comment{{if .Parent}} reply{{end}}" data-comment="{{.ID}}">
<span class="author">{{.Author}}</span><p>{{.Body}}</p>
</div>{{end}}
<form class="comment-form" method="post" action="/p/{{pesc .Key}}/i/{{.Issue.Number}}/comment">
<input name="body" placeholder="comment on this issue"><button>Comment</button>
</form>
</aside>
<section class="audit"><h2>History</h2>
{{range .Events}}<div class="event" data-kind="{{.Kind}}"><span class="kind">{{.Kind}}</span> <span class="actor">{{.Actor}}</span> <span class="at">{{.Created}}</span></div>{{end}}
</section>`))

// issue renders one issue with child progress (AC-parent-rollup).
func (s *Server) issue(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	p, err := s.projectByKey(key)
	if err != nil {
		htmlError(w, err)
		return
	}
	var page struct {
		Issues []issueView `json:"issues"`
	}
	if err := s.get("/projects/"+p.ID+"/issues?number="+url.QueryEscape(r.PathValue("num")), &page); err != nil {
		htmlError(w, err)
		return
	}
	if len(page.Issues) == 0 {
		http.NotFound(w, r)
		return
	}
	issue := page.Issues[0]
	var relations []struct {
		Kind string `json:"kind"`
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := s.get("/issues/"+issue.ID+"/relations", &relations); err != nil {
		htmlError(w, err)
		return
	}
	total, complete := 0, 0
	for _, rel := range relations {
		if rel.Kind != "parent_of" || rel.From != issue.ID {
			continue
		}
		total++
		var child struct {
			Status string `json:"status"`
		}
		if err := s.get("/issues/"+rel.To, &child); err != nil {
			htmlError(w, err)
			return
		}
		if child.Status == "complete" {
			complete++
		}
	}
	progress := ""
	if total > 0 {
		progress = fmt.Sprintf("%d of %d complete", complete, total)
	}
	// The declared issue view shows everything about the issue —
	// documents, threads, reviews, discussion, audit trail — each
	// streamed or metadata-light.
	var docsList []struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	if err := s.get("/issues/"+issue.ID+"/documents", &docsList); err != nil {
		htmlError(w, err)
		return
	}
	threadsSeq := func(yield func(threadRef) bool) { s.eachThreadRef("/issues/"+issue.ID+"/threads", yield) }
	var reviews []struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if err := s.get("/reviews?issue="+url.QueryEscape(issue.ID), &reviews); err != nil {
		htmlError(w, err)
		return
	}
	commentsSeq := func(yield func(commentView) bool) { s.eachComment("/comments?issue="+url.QueryEscape(issue.ID), yield) }
	eventsSeq := func(yield func(eventRef) bool) { s.eachEventRef("/issues/"+issue.ID+"/events", yield) }
	_ = issueTmpl.Execute(w, map[string]any{
		"Key": key, "Issue": issue, "Progress": progress,
		"Documents": docsList, "Threads": iter.Seq[threadRef](threadsSeq),
		"Reviews": reviews, "Comments": iter.Seq[commentView](commentsSeq),
		"Events": iter.Seq[eventRef](eventsSeq),
	})
}

// threadRef and eventRef are light listing views for the issue page.
type threadRef struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type eventRef struct {
	Kind    string `json:"kind"`
	Actor   string `json:"actor"`
	Created string `json:"created"`
}

// Failure markers: a broken listing must be VISIBLE on the page,
// never a complete-looking section silently missing entries — the
// same treatment the comment iterator applies.
var (
	threadFailure = threadRef{Title: "⚠ threads unavailable: failed to load"}
	eventFailure  = eventRef{Kind: "⚠ history unavailable: failed to load", Actor: "system"}
)

// eachArray walks one JSON array response with strict delimiters and
// EOF; onFailure fires for connection errors, non-200s, wrong shapes,
// and truncation. decodeOne decodes and yields one element, returning
// false when the consumer stopped.
func (s *Server) eachArray(path string, decodeOne func(*json.Decoder) bool, onFailure func() bool) {
	resp, err := s.client.Get(s.api + path)
	if err != nil {
		onFailure()
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		onFailure()
		return
	}
	dec := json.NewDecoder(resp.Body)
	open, err := dec.Token()
	if err != nil {
		onFailure()
		return
	}
	if d, ok := open.(json.Delim); !ok || d != '[' {
		onFailure()
		return
	}
	for dec.More() {
		if !decodeOne(dec) {
			return
		}
	}
	closing, err := dec.Token()
	if err != nil {
		onFailure()
		return
	}
	if d, ok := closing.(json.Delim); !ok || d != ']' {
		onFailure()
		return
	}
	if _, err := dec.Token(); err != io.EOF {
		onFailure()
	}
}

// eachEnvelopeArray streams the named array member of an object
// response — the listing envelopes ({feed_watermark, issues}) — with
// the same strict delimiters and failure semantics as eachArray.
func (s *Server) eachEnvelopeArray(path, member string, decodeOne func(*json.Decoder) bool, onFailure func() bool) {
	resp, err := s.client.Get(s.api + path)
	if err != nil {
		onFailure()
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		onFailure()
		return
	}
	dec := json.NewDecoder(resp.Body)
	envelope, err := dec.Token()
	if err != nil {
		onFailure()
		return
	}
	if d, ok := envelope.(json.Delim); !ok || d != '{' {
		onFailure()
		return
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			onFailure()
			return
		}
		if keyTok != member {
			// Sibling members are bounded scalars (watermarks,
			// cursors); skipping them costs nothing.
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				onFailure()
				return
			}
			continue
		}
		arrayOpen, err := dec.Token()
		if err != nil {
			onFailure()
			return
		}
		if d, ok := arrayOpen.(json.Delim); !ok || d != '[' {
			onFailure()
			return
		}
		for dec.More() {
			if !decodeOne(dec) {
				return
			}
		}
		if _, err := dec.Token(); err != nil { // ']'
			onFailure()
			return
		}
		// The envelope must CLOSE: a response truncated right after
		// the member would otherwise pass for a complete listing.
		for dec.More() {
			if _, err := dec.Token(); err != nil {
				onFailure()
				return
			}
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				onFailure()
				return
			}
		}
		closing, err := dec.Token()
		if err != nil {
			onFailure()
			return
		}
		if d, ok := closing.(json.Delim); !ok || d != '}' {
			onFailure()
			return
		}
		if _, err := dec.Token(); err != io.EOF {
			onFailure()
		}
		return
	}
	onFailure() // the member never appeared
}

// eachThreadRef streams a thread listing's id/title pairs.
func (s *Server) eachThreadRef(path string, yield func(threadRef) bool) {
	s.eachArray(path, func(dec *json.Decoder) bool {
		var t threadRef
		if err := dec.Decode(&t); err != nil || t.ID == "" {
			// A null or field-less element would render blank; the
			// failure marker keeps the breakage visible.
			return yield(threadFailure) && false
		}
		return yield(t)
	}, func() bool { return yield(threadFailure) })
}

// eachEventRef streams the audit listing's display fields.
func (s *Server) eachEventRef(path string, yield func(eventRef) bool) {
	s.eachArray(path, func(dec *json.Decoder) bool {
		var e eventRef
		if err := dec.Decode(&e); err != nil || e.Kind == "" {
			return yield(eventFailure) && false
		}
		return yield(e)
	}, func() bool { return yield(eventFailure) })
}

// commentIssue anchors a comment to the issue (ACT-comment on the
// issue view).
func (s *Server) commentIssue(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	p, err := s.projectByKey(key)
	if err != nil {
		htmlError(w, err)
		return
	}
	if err := r.ParseForm(); err != nil {
		htmlError(w, err)
		return
	}
	var page struct {
		Issues []issueView `json:"issues"`
	}
	if err := s.get("/projects/"+p.ID+"/issues?number="+url.QueryEscape(r.PathValue("num")), &page); err != nil {
		htmlError(w, err)
		return
	}
	if len(page.Issues) == 0 {
		http.NotFound(w, r)
		return
	}
	status, body, err := s.post("/comments", map[string]any{
		"issue": page.Issues[0].ID, "body": r.Form.Get("body"), "author": s.actor})
	if err != nil {
		htmlError(w, err)
		return
	}
	if status != http.StatusCreated {
		htmlError(w, fmt.Errorf("comment rejected: %s", body))
		return
	}
	http.Redirect(w, r, "/p/"+url.PathEscape(key)+"/i/"+r.PathValue("num"), http.StatusSeeOther)
}

type docView struct {
	ID             string  `json:"id"`
	Title          string  `json:"title"`
	CurrentVersion *string `json:"current_version"`
	Version        struct {
		ID      string `json:"id"`
		Number  int64  `json:"number"`
		Content string `json:"content"`
	} `json:"version"`
}

// discussionFailure is the marker rendered when a comment stream
// breaks mid-page — a failure must be VISIBLE, never a complete-
// looking discussion silently missing feedback.
var discussionFailure = commentView{Author: "system", Body: "⚠ discussion unavailable: failed to load comments"}

// eachComment walks one comments listing element by element, yielding
// each as decoded. Failures — connection, non-200, malformed JSON —
// yield the visible failure marker; returns false if the consumer
// stopped early.
func (s *Server) eachComment(path string, yield func(commentView) bool) bool {
	resp, err := s.client.Get(s.api + path)
	if err != nil {
		return yield(discussionFailure)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return yield(discussionFailure)
	}
	dec := json.NewDecoder(resp.Body)
	open, err := dec.Token()
	if err != nil {
		return yield(discussionFailure)
	}
	if d, ok := open.(json.Delim); !ok || d != '[' {
		return yield(discussionFailure)
	}
	for dec.More() {
		var c commentView
		if err := dec.Decode(&c); err != nil || c.ID == "" {
			// Null elements and field-less objects render as blank
			// rows otherwise — surface them as failures.
			return yield(discussionFailure)
		}
		if !yield(c) {
			return false
		}
	}
	// The listing must CLOSE — a response truncated after a complete
	// element would otherwise pass for a complete discussion.
	closing, err := dec.Token()
	if err != nil {
		return yield(discussionFailure)
	}
	if d, ok := closing.(json.Delim); !ok || d != ']' {
		return yield(discussionFailure)
	}
	if _, err := dec.Token(); err != io.EOF {
		return yield(discussionFailure)
	}
	return true
}

type commentView struct {
	ID             string  `json:"id"`
	DocVersion     *string `json:"doc_version"`
	Parent         *string `json:"parent"`
	Anchor         *string `json:"anchor"`
	Author         string  `json:"author"`
	Body           string  `json:"body"`
	ReviewRevision int64   `json:"review_revision"`
}

var docTmpl = template.Must(template.New("doc").Funcs(uiFuncs).Parse(`<!doctype html>
<title>{{.Doc.Title}}</title>
<h1>{{.Doc.Title}}</h1>
<p class="version" data-version="{{.Doc.Version.Number}}">version {{.Doc.Version.Number}}</p>
<nav class="versions">
{{range .Versions}}<a href="/p/{{pesc $.Key}}/d/{{$.Doc.ID}}?version={{.Number}}">v{{.Number}}</a> {{end}}
</nav>
<main class="doc">{{.Rendered}}</main>
<aside class="discussion">
{{range .Comments}}<div class="comment{{if .Parent}} reply{{end}}" data-comment="{{.ID}}"{{if .Anchor}} data-anchor="{{.Anchor}}"{{end}} data-version="{{.VersionNumber}}">
<span class="author">{{.Author}}</span> <span class="on-version">on v{{.VersionNumber}}</span>
<p>{{.Body}}</p>
<form class="reply-form" method="post" action="/p/{{pesc $.Key}}/d/{{$.Doc.ID}}/comment">
<input type="hidden" name="doc_version" value="{{.DocVersionID}}">
<input type="hidden" name="parent" value="{{.ID}}">
<input name="body" placeholder="reply"><button>Reply</button>
</form>
</div>{{end}}
<form class="comment-form" method="post" action="/p/{{pesc $.Key}}/d/{{$.Doc.ID}}/comment">
<input type="hidden" name="doc_version" value="{{.Doc.Version.ID}}">
<input name="anchor" placeholder="block-1">
<input name="body" placeholder="comment on this version"><button>Comment</button>
</form>
</aside>
<form class="save-version" method="post" action="/p/{{pesc .Key}}/d/{{.Doc.ID}}/save">
<textarea name="content" placeholder="new version content"></textarea>
<button>Save version</button>
</form>
{{if .Live}}<script>
// Poll for newer versions; refresh to the latest when one lands. Each
// poll is scheduled only after the previous one settles, so a slow
// response cannot pile requests up every five seconds (review 1910).
(function () {
  var shown = {{.Doc.Version.Number}};
  var url = '/p/{{pesc .Key}}/d/{{.Doc.ID}}';
  function poll() {
    fetch(url + '/poll?since=' + shown)
      .then(function (r) { return r.json(); })
      .then(function (p) { if (p.refresh) { window.location = url; return; } schedule(); })
      .catch(function () { schedule(); });
  }
  function schedule() { setTimeout(poll, 5000); }
  schedule();
})();
</script>{{end}}`))

type docComment struct {
	commentView
	VersionNumber int64
	DocVersionID  string
}

// document renders a doc version (latest unless ?version= chosen) with
// a minimal markdown rendering and the threaded discussion beside it.
// Comments from OTHER versions stay visible, labeled with their
// version — never silently orphaned (AC-docweb-comments).
func (s *Server) document(w http.ResponseWriter, r *http.Request) {
	if !s.guardDoc(w, r) {
		return
	}
	id := r.PathValue("documentId")
	path := "/documents/" + id
	if v := r.URL.Query().Get("version"); v != "" {
		path += "?version=" + url.QueryEscape(v)
	}
	var doc docView
	if err := s.get(path, &doc); err != nil {
		htmlError(w, err)
		return
	}
	// Discussion spans every version: fetch the version list, then
	// comments per version id.
	// The version listing carries every version's content; decoding
	// the whole array would buffer it, so elements stream one at a
	// time and only the selector's id/number survive.
	type versionRef struct {
		ID     string `json:"id"`
		Number int64  `json:"number"`
	}
	versions := []versionRef{}
	versionsFailed := false
	s.eachArray("/documents/"+id+"/versions", func(dec *json.Decoder) bool {
		var v versionRef
		if err := dec.Decode(&v); err != nil || v.ID == "" || v.Number < 1 {
			// null, {}, or a malformed entry would render a bogus v0
			// link and query comments with an empty version id.
			versionsFailed = true
			return false
		}
		versions = append(versions, v)
		return true
	}, func() bool { versionsFailed = true; return false })
	if versionsFailed {
		// A partial selector hides versions AND their discussion; the
		// failure must be visible, never a seemingly whole page.
		htmlError(w, fmt.Errorf("version history unavailable"))
		return
	}
	// The discussion STREAMS into the template — the sequence decodes
	// each comment lazily during execution, so an unbounded discussion
	// never accumulates before rendering.
	comments := func(yield func(docComment) bool) {
		for _, v := range versions {
			if !s.eachComment("/comments?doc_version="+url.QueryEscape(v.ID), func(c commentView) bool {
				return yield(docComment{commentView: c, VersionNumber: v.Number, DocVersionID: v.ID})
			}) {
				return
			}
		}
	}
	_ = docTmpl.Execute(w, map[string]any{
		"Doc": doc, "Rendered": renderMarkdown(doc.Version.Content),
		"Comments": iter.Seq[docComment](comments), "Versions": versions, "Key": r.PathValue("key"),
		// A reader who CHOSE a historical version stays on it; only
		// the latest view auto-refreshes to newer versions.
		"Live": r.URL.Query().Get("version") == "",
	})
}

// commentDoc posts a block-anchored comment on the SHOWN version.
func (s *Server) commentDoc(w http.ResponseWriter, r *http.Request) {
	if !s.guardDoc(w, r) {
		return
	}
	id := r.PathValue("documentId")
	if err := r.ParseForm(); err != nil {
		htmlError(w, err)
		return
	}
	versionID := r.Form.Get("doc_version")
	// The form value is independently controlled; the version must
	// belong to the ROUTE's document, or a forged form could comment
	// on another document under a misleading scoped URL.
	var versionDocument string
	if err := s.scanField("/doc-versions/"+url.PathEscape(versionID), "document", &versionDocument, "content"); err != nil {
		htmlError(w, err)
		return
	}
	if versionDocument != id {
		http.NotFound(w, r)
		return
	}
	payload := map[string]any{
		"doc_version": versionID,
		"body":        r.Form.Get("body"),
		"author":      s.actor,
	}
	if anchor := r.Form.Get("anchor"); anchor != "" {
		payload["anchor"] = anchor
	}
	if parent := r.Form.Get("parent"); parent != "" {
		payload["parent"] = parent
	}
	status, body, err := s.post("/comments", payload)
	if err != nil {
		htmlError(w, err)
		return
	}
	if status != http.StatusCreated {
		htmlError(w, fmt.Errorf("comment rejected: %s", body))
		return
	}
	http.Redirect(w, r, "/p/"+url.PathEscape(r.PathValue("key"))+"/d/"+id, http.StatusSeeOther)
}

// saveDocVersion appends a new version through the API (ACT-save-doc)
// and returns the reader to the latest view.
func (s *Server) saveDocVersion(w http.ResponseWriter, r *http.Request) {
	if !s.guardDoc(w, r) {
		return
	}
	id := r.PathValue("documentId")
	if err := r.ParseForm(); err != nil {
		htmlError(w, err)
		return
	}
	status, body, err := s.post("/documents/"+id+"/versions",
		map[string]any{"content": r.Form.Get("content"), "author": s.actor})
	if err != nil {
		htmlError(w, err)
		return
	}
	if status != http.StatusCreated {
		htmlError(w, fmt.Errorf("save version rejected: %s", body))
		return
	}
	http.Redirect(w, r, "/p/"+url.PathEscape(r.PathValue("key"))+"/d/"+id, http.StatusSeeOther)
}

// pollDocument reports whether the document has advanced past the
// viewer's version — the page's refresh signal (AC-docweb-live).
func (s *Server) pollDocument(w http.ResponseWriter, r *http.Request) {
	// ONE bounded request serves both the scope guard and the version
	// number, so the five-second loop costs a single metadata read
	// (review 1914). Guarding through guardDoc would repeat the fetch.
	meta, err := s.documentMeta(r.PathValue("documentId"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !s.inProject(w, r.PathValue("key"), meta.Document.Project) {
		return
	}
	latest := meta.Version.Number
	since := r.URL.Query().Get("since")
	w.Header().Set("Content-Type", "application/json")
	fresh := fmt.Sprintf("%d", latest) != since
	_ = json.NewEncoder(w).Encode(map[string]any{
		"latest": latest, "refresh": fresh,
	})
}

// The thread page renders in three pieces so turns stream.
var (
	threadHeadTmpl = template.Must(template.New("thread-head").Parse(`<!doctype html>
<title>{{.}}</title><h1>{{.}}</h1>
<ol class="conversation">
`))
	threadTurnTmpl = template.Must(template.New("thread-turn").Parse(
		`<li class="turn"><span class="speaker">{{.Speaker}}</span><p>{{.Text}}</p></li>` + "\n"))
	threadTailTmpl = template.Must(template.New("thread-tail").Parse(`</ol>`))
)

// threadFailureTurn marks a transcript that stopped mid-stream.
var threadFailureTurn = threadTurn{Speaker: "system", Text: "⚠ transcript unavailable: response incomplete"}

// threadEnvelopeComplete drains the thread envelope's remaining
// members and reports whether it closed with a well-formed end.
func threadEnvelopeComplete(dec *json.Decoder) bool {
	for dec.More() {
		if _, err := dec.Token(); err != nil {
			return false
		}
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return false
		}
	}
	closing, err := dec.Token()
	if err != nil {
		return false
	}
	if d, ok := closing.(json.Delim); !ok || d != '}' {
		return false
	}
	_, err = dec.Token()
	return err == io.EOF
}

// threadTurn is one rendered conversation row.
type threadTurn struct {
	Speaker string
	Text    string
}

// tokenText re-serializes the value whose first token was already
// consumed, emitting the structural separators the token stream drops
// and preserving numeric tokens verbatim (the decoder runs with
// UseNumber). Without this a non-array transcript rendered as
// malformed pseudo-JSON and integers past 2^53 shifted (review 1889).
func tokenText(first json.Token, dec *json.Decoder) string {
	var out strings.Builder
	type frame struct {
		object bool
		n      int // tokens emitted in this frame (object keys count)
	}
	var stack []frame

	separate := func() {
		if len(stack) == 0 {
			return
		}
		f := &stack[len(stack)-1]
		switch {
		case f.object && f.n > 0 && f.n%2 == 0:
			out.WriteString(",")
		case f.object && f.n%2 == 1:
			out.WriteString(":")
		case !f.object && f.n > 0:
			out.WriteString(",")
		}
		f.n++
	}

	write := func(tok json.Token) {
		switch v := tok.(type) {
		case json.Delim:
			if v == '{' || v == '[' {
				separate()
				out.WriteString(string(v))
				stack = append(stack, frame{object: v == '{'})
				return
			}
			out.WriteString(string(v))
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		case json.Number:
			separate()
			out.WriteString(v.String())
		default:
			separate()
			raw, err := json.Marshal(v)
			if err != nil {
				return
			}
			out.Write(raw)
		}
	}

	write(first)
	for len(stack) > 0 {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		write(tok)
	}
	return out.String()
}

// thread renders a transcript turn by turn with speakers
// distinguished (AC-thread-view). Transcripts are arbitrary JSON; the
// conventional [{speaker, text}] shape renders as a conversation and
// anything else falls back to per-entry rendering.
func (s *Server) thread(w http.ResponseWriter, r *http.Request) {
	if !s.guardThread(w, r) {
		return
	}
	// The transcript is unbounded: the page renders in pieces so each
	// turn decodes, renders, and is released — never the whole
	// transcript plus its entry slice plus rendered turns (1887).
	resp, err := s.client.Get(s.api + "/threads/" + url.PathEscape(r.PathValue("threadId")))
	if err != nil {
		htmlError(w, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		htmlError(w, fmt.Errorf("thread unavailable"))
		return
	}
	dec := json.NewDecoder(resp.Body)
	dec.UseNumber() // transcript numbers keep their original tokens
	envelope, err := dec.Token()
	if err != nil {
		htmlError(w, err)
		return
	}
	if d, ok := envelope.(json.Delim); !ok || d != '{' {
		htmlError(w, fmt.Errorf("malformed thread response"))
		return
	}
	title := ""
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			htmlError(w, fmt.Errorf("malformed thread response"))
			return
		}
		if keyTok == "title" {
			if err := dec.Decode(&title); err != nil {
				htmlError(w, fmt.Errorf("malformed thread response"))
				return
			}
			continue
		}
		if keyTok != "transcript" {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				htmlError(w, fmt.Errorf("malformed thread response"))
				return
			}
			continue
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = threadHeadTmpl.Execute(w, title)
		open, err := dec.Token()
		if err != nil {
			// Rendering has begun: the failure shows inside the page.
			_ = threadTurnTmpl.Execute(w, threadFailureTurn)
			_ = threadTailTmpl.Execute(w, nil)
			return
		}
		if d, ok := open.(json.Delim); ok && d == '[' {
			for dec.More() {
				var entry json.RawMessage
				if err := dec.Decode(&entry); err != nil {
					// Output has begun; the failure must be VISIBLE.
					_ = threadTurnTmpl.Execute(w, threadFailureTurn)
					_ = threadTailTmpl.Execute(w, nil)
					return
				}
				var decoded threadTurn
				// A structured turn needs BOTH conventional fields; a
				// half-shaped entry renders its raw JSON instead of a
				// blank-sided row.
				if err := json.Unmarshal(entry, &decoded); err == nil && decoded.Speaker != "" && decoded.Text != "" {
					_ = threadTurnTmpl.Execute(w, decoded)
					continue
				}
				_ = threadTurnTmpl.Execute(w, threadTurn{Speaker: "entry", Text: string(entry)})
			}
			if _, err := dec.Token(); err != nil { // ']'
				_ = threadTurnTmpl.Execute(w, threadFailureTurn)
				_ = threadTailTmpl.Execute(w, nil)
				return
			}
		} else {
			// A non-array transcript is ONE value — the
			// record-granularity floor — rendered raw.
			_ = threadTurnTmpl.Execute(w, threadTurn{Speaker: "transcript", Text: tokenText(open, dec)})
		}
		// The envelope must CLOSE and the body must END: a truncated
		// response would otherwise render as a complete conversation
		// under a 200 (review 1891).
		if !threadEnvelopeComplete(dec) {
			_ = threadTurnTmpl.Execute(w, threadFailureTurn)
		}
		_ = threadTailTmpl.Execute(w, nil)
		return
	}
	// Reaching here means the envelope closed without a transcript.
	htmlError(w, fmt.Errorf("thread carries no transcript"))
}

type reviewView struct {
	ID       string  `json:"id"`
	Issue    string  `json:"issue"`
	State    string  `json:"state"`
	Revision int64   `json:"revision"`
	Summary  *string `json:"summary"`
}

var reviewTmpl = template.Must(template.New("review").Funcs(uiFuncs).Parse(`<!doctype html>
<title>review {{.Review.ID}}</title>
<h1>Review</h1>
<p class="state" data-state="{{.Review.State}}">{{.Review.State}} · revision {{.Review.Revision}}</p>
{{if .Error}}<p class="error" role="alert">{{.Error}}</p>{{end}}
<main class="deliverable"><pre>{{.Deliverable}}</pre></main>
<aside class="discussion">
{{range .Comments}}<div class="comment{{if .Parent}} reply{{end}}" data-comment="{{.ID}}">
<span class="author">{{.Author}}</span> <span class="on-revision">on r{{.ReviewRevision}}</span>
<p>{{.Body}}</p>
{{if eq .ReviewRevision $.Review.Revision}}<form class="reply-form" method="post" action="/p/{{pesc $.Key}}/r/{{$.Review.ID}}/comment">
<input type="hidden" name="parent" value="{{.ID}}">
<input type="hidden" name="review_revision" value="{{$.Review.Revision}}">
<input name="body" placeholder="reply"><button>Reply</button>
</form>{{end}}
</div>{{end}}
<form class="comment-form" method="post" action="/p/{{pesc $.Key}}/r/{{$.Review.ID}}/comment">
<input type="hidden" name="review_revision" value="{{.Review.Revision}}">
<input name="body" placeholder="comment on this revision"><button>Comment</button>
</form>
</aside>
<form class="verdict" method="post" action="/p/{{pesc .Key}}/r/{{.Review.ID}}/verdict">
<input type="hidden" name="revision" value="{{.Review.Revision}}">
<button name="verdict" value="approved">Approve</button>
<button name="verdict" value="changes-requested">Request changes</button>
</form>`))

// review renders the review page: the pinned deliverable for reading
// (AC-review-web), the threaded discussion (AC-review-threads), and
// verdict controls that post the revision the reviewer SAW
// (AC-review-verdict, AC-review-stale-guard).
func (s *Server) review(w http.ResponseWriter, r *http.Request) {
	if !s.guardReview(w, r) {
		return
	}
	s.renderReview(w, r.PathValue("key"), r.PathValue("reviewId"), "")
}

func (s *Server) renderReview(w http.ResponseWriter, key, id, errMsg string) {
	var rev reviewView
	if err := s.get("/reviews/"+id, &rev); err != nil {
		htmlError(w, err)
		return
	}
	var deliverable struct {
		Content string `json:"content"`
	}
	if err := s.get("/reviews/"+id+"/deliverable", &deliverable); err != nil {
		htmlError(w, err)
		return
	}
	// The discussion streams into the template — unbounded bodies
	// never accumulate before rendering.
	comments := func(yield func(commentView) bool) {
		s.eachComment("/comments?review="+url.QueryEscape(id), yield)
	}
	_ = reviewTmpl.Execute(w, map[string]any{
		"Review": rev, "Deliverable": deliverable.Content, "Key": key,
		"Comments": iter.Seq[commentView](comments), "Error": errMsg,
	})
}

// reviewVerdict posts the human's verdict at the revision the page
// showed; a stale-revision rejection re-renders with the API's error.
func (s *Server) reviewVerdict(w http.ResponseWriter, r *http.Request) {
	if !s.guardReview(w, r) {
		return
	}
	id := r.PathValue("reviewId")
	if err := r.ParseForm(); err != nil {
		htmlError(w, err)
		return
	}
	revision, err := strconv.ParseInt(r.Form.Get("revision"), 10, 64)
	if err != nil {
		htmlError(w, fmt.Errorf("bad revision"))
		return
	}
	status, body, err := s.post("/reviews/"+id+"/verdict", map[string]any{
		"verdict": r.Form.Get("verdict"), "revision": revision, "actor": s.actor})
	if err != nil {
		htmlError(w, err)
		return
	}
	if status != http.StatusOK {
		var apiErr struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &apiErr)
		s.renderReview(w, r.PathValue("key"), id, apiErr.Code+": "+apiErr.Message)
		return
	}
	http.Redirect(w, r, "/p/"+url.PathEscape(r.PathValue("key"))+"/r/"+id, http.StatusSeeOther)
}

// reviewComment anchors a comment (or reply) to the review at its
// current revision.
func (s *Server) reviewComment(w http.ResponseWriter, r *http.Request) {
	if !s.guardReview(w, r) {
		return
	}
	id := r.PathValue("reviewId")
	if err := r.ParseForm(); err != nil {
		htmlError(w, err)
		return
	}
	// The form carries the revision the page SHOWED — a comment from a
	// stale page must hit the API's stale-guard, never silently attach
	// to unseen content.
	revision, err := strconv.ParseInt(r.Form.Get("review_revision"), 10, 64)
	if err != nil {
		htmlError(w, fmt.Errorf("bad review_revision"))
		return
	}
	payload := map[string]any{
		"review": id, "review_revision": revision,
		"body": r.Form.Get("body"), "author": s.actor,
	}
	if parent := r.Form.Get("parent"); parent != "" {
		payload["parent"] = parent
	}
	status, body, err := s.post("/comments", payload)
	if err != nil {
		htmlError(w, err)
		return
	}
	if status != http.StatusCreated {
		var apiErr struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &apiErr)
		s.renderReview(w, r.PathValue("key"), id, apiErr.Code+": "+apiErr.Message)
		return
	}
	http.Redirect(w, r, "/p/"+url.PathEscape(r.PathValue("key"))+"/r/"+id, http.StatusSeeOther)
}

// renderMarkdown is a deliberately small formatter: headings,
// paragraphs, and list items — enough for docs to read formatted
// without a rendering dependency (new dependencies need approval).
func renderMarkdown(src string) template.HTML {
	var out strings.Builder
	inList := false
	closeList := func() {
		if inList {
			out.WriteString("</ul>\n")
			inList = false
		}
	}
	for _, block := range strings.Split(src, "\n") {
		line := strings.TrimRight(block, "\r")
		esc := template.HTMLEscapeString(strings.TrimSpace(strings.TrimLeft(line, "#- ")))
		switch {
		case strings.HasPrefix(line, "### "):
			closeList()
			out.WriteString("<h3>" + esc + "</h3>\n")
		case strings.HasPrefix(line, "## "):
			closeList()
			out.WriteString("<h2>" + esc + "</h2>\n")
		case strings.HasPrefix(line, "# "):
			closeList()
			out.WriteString("<h1>" + esc + "</h1>\n")
		case strings.HasPrefix(line, "- "):
			if !inList {
				out.WriteString("<ul>\n")
				inList = true
			}
			out.WriteString("<li>" + esc + "</li>\n")
		case strings.TrimSpace(line) == "":
			closeList()
		default:
			closeList()
			out.WriteString("<p>" + template.HTMLEscapeString(line) + "</p>\n")
		}
	}
	closeList()
	return template.HTML(out.String()) //nolint:gosec // every fragment above is escaped
}

// newKey mints a random idempotency key for UI-driven mutations.
func newKey() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("crypto/rand unavailable: %v", err))
	}
	return fmt.Sprintf("web-%x", b)
}
