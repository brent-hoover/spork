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
}

// New builds the web handler. actor is the identity UI mutations act
// as — the single-operator system has exactly one human at the
// keyboard.
func New(apiBase, actor string) *Server {
	return &Server{api: strings.TrimRight(apiBase, "/"), client: &http.Client{}, actor: actor}
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
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			scheme := "http"
			if r.TLS != nil {
				scheme = "https"
			}
			if origin == "null" || origin != scheme+"://"+r.Host {
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

// docProject resolves a document's governing project id.
func (s *Server) docProject(documentID string) (string, error) {
	var doc struct {
		Project string `json:"project"`
	}
	if err := s.get("/documents/"+documentID, &doc); err != nil {
		return "", err
	}
	return doc.Project, nil
}

// reviewProject resolves a review's governing project via its issue.
func (s *Server) reviewProject(reviewID string) (string, error) {
	var rev struct {
		Issue string `json:"issue"`
	}
	if err := s.get("/reviews/"+reviewID, &rev); err != nil {
		return "", err
	}
	var issue struct {
		Project string `json:"project"`
	}
	if err := s.get("/issues/"+rev.Issue, &issue); err != nil {
		return "", err
	}
	return issue.Project, nil
}

// threadProject resolves a thread's governing project — its project
// anchor, or its anchored issue's project.
func (s *Server) threadProject(threadID string) (string, error) {
	var t struct {
		Project *string `json:"project"`
		Issue   *string `json:"issue"`
	}
	if err := s.get("/threads/"+threadID, &t); err != nil {
		return "", err
	}
	if t.Project != nil {
		return *t.Project, nil
	}
	if t.Issue != nil {
		var issue struct {
			Project string `json:"project"`
		}
		if err := s.get("/issues/"+*t.Issue, &issue); err != nil {
			return "", err
		}
		return issue.Project, nil
	}
	return "", fmt.Errorf("thread %s carries no anchor", threadID)
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
	var list []project
	if err := s.get("/projects", &list); err != nil {
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
	var page struct {
		Issues []issueView `json:"issues"`
	}
	if err := s.get("/projects/"+p.ID+"/issues", &page); err != nil {
		htmlError(w, err)
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
		for _, i := range page.Issues {
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

// eachThreadRef streams a thread listing's id/title pairs.
func (s *Server) eachThreadRef(path string, yield func(threadRef) bool) {
	resp, err := s.client.Get(s.api + path)
	if err != nil {
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return
	}
	dec := json.NewDecoder(resp.Body)
	if _, err := dec.Token(); err != nil {
		return
	}
	for dec.More() {
		var t threadRef
		if err := dec.Decode(&t); err != nil {
			return
		}
		if !yield(t) {
			return
		}
	}
}

// eachEventRef streams the audit listing's display fields.
func (s *Server) eachEventRef(path string, yield func(eventRef) bool) {
	resp, err := s.client.Get(s.api + path)
	if err != nil {
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return
	}
	dec := json.NewDecoder(resp.Body)
	if _, err := dec.Token(); err != nil {
		return
	}
	for dec.More() {
		var e eventRef
		if err := dec.Decode(&e); err != nil {
			return
		}
		if !yield(e) {
			return
		}
	}
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
		if err := dec.Decode(&c); err != nil {
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
// Poll for newer versions; refresh to the latest when one lands.
(function () {
  var shown = {{.Doc.Version.Number}};
  setInterval(function () {
    fetch('/p/{{pesc .Key}}/d/{{.Doc.ID}}/poll?since=' + shown)
      .then(function (r) { return r.json(); })
      .then(function (p) { if (p.refresh) { window.location = '/p/{{pesc .Key}}/d/{{.Doc.ID}}'; } })
      .catch(function () {});
  }, 5000);
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
	var versions []struct {
		ID     string `json:"id"`
		Number int64  `json:"number"`
	}
	if err := s.get("/documents/"+id+"/versions", &versions); err != nil {
		htmlError(w, err)
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
	if !s.guardDoc(w, r) {
		return
	}
	id := r.PathValue("documentId")
	var doc docView
	if err := s.get("/documents/"+id, &doc); err != nil {
		htmlError(w, err)
		return
	}
	since := r.URL.Query().Get("since")
	w.Header().Set("Content-Type", "application/json")
	fresh := fmt.Sprintf("%d", doc.Version.Number) != since
	_ = json.NewEncoder(w).Encode(map[string]any{
		"latest": doc.Version.Number, "refresh": fresh,
	})
}

type threadView struct {
	ID         string          `json:"id"`
	Title      string          `json:"title"`
	Transcript json.RawMessage `json:"transcript"`
}

var threadTmpl = template.Must(template.New("thread").Parse(`<!doctype html>
<title>{{.Title}}</title><h1>{{.Title}}</h1>
<ol class="conversation">
{{range .Turns}}<li class="turn"><span class="speaker">{{.Speaker}}</span><p>{{.Text}}</p></li>{{end}}
</ol>`))

// thread renders a transcript turn by turn with speakers
// distinguished (AC-thread-view). Transcripts are arbitrary JSON; the
// conventional [{speaker, text}] shape renders as a conversation and
// anything else falls back to per-entry rendering.
func (s *Server) thread(w http.ResponseWriter, r *http.Request) {
	if !s.guardThread(w, r) {
		return
	}
	var t threadView
	if err := s.get("/threads/"+r.PathValue("threadId"), &t); err != nil {
		htmlError(w, err)
		return
	}
	type turn struct {
		Speaker string `json:"speaker"`
		Text    string `json:"text"`
	}
	// The conventional [{speaker, text}] shape renders as a
	// conversation; entries in any OTHER shape fall back to their raw
	// JSON per entry — never a blank row.
	turns := []turn{}
	var entries []json.RawMessage
	if err := json.Unmarshal(t.Transcript, &entries); err != nil || entries == nil {
		// Non-array transcripts — JSON null included — render raw.
		turns = []turn{{Speaker: "transcript", Text: string(t.Transcript)}}
	} else {
		for _, entry := range entries {
			var decoded turn
			// A structured turn needs BOTH conventional fields; a
			// half-shaped entry renders its raw JSON instead of a
			// blank-sided row.
			if err := json.Unmarshal(entry, &decoded); err == nil && decoded.Speaker != "" && decoded.Text != "" {
				turns = append(turns, decoded)
				continue
			}
			turns = append(turns, turn{Speaker: "entry", Text: string(entry)})
		}
	}
	_ = threadTmpl.Execute(w, map[string]any{"Title": t.Title, "Turns": turns})
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
