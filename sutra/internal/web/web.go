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
	"net/http"
	"net/url"
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
	mux.HandleFunc("GET /d/{documentId}", s.document)
	mux.HandleFunc("POST /d/{documentId}/comment", s.sameOrigin(s.commentDoc))
	mux.HandleFunc("GET /d/{documentId}/poll", s.pollDocument)
	mux.HandleFunc("GET /t/{threadId}", s.thread)
	return mux
}

// sameOrigin rejects cross-origin mutations. Browsers send Origin (or
// at least Sec-Fetch-Site) on form posts; a value naming another
// origin is refused outright, and non-browser callers without either
// header pass — they hold no ambient browser credentials to launder.
func (s *Server) sameOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && origin != "null" {
			if u, err := url.Parse(origin); err != nil || u.Host != r.Host {
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

var projectsTmpl = template.Must(template.New("projects").Parse(`<!doctype html>
<title>sutra</title><h1>Projects</h1><ul>
{{range .}}<li><a href="/p/{{.Key}}">{{.Key}} — {{.Name}}</a></li>{{end}}
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
	Key      string
	Columns  []boardColumn
	Statuses []string
	Error    string
}

type boardColumn struct {
	Status string
	Cards  []issueView
}

var boardTmpl = template.Must(template.New("board").Parse(`<!doctype html>
<title>{{.Key}} board</title><h1>{{.Key}}</h1>
{{if .Error}}<p class="error" role="alert">{{.Error}}</p>{{end}}
<div class="board">
{{range $col := .Columns}}<section class="column" data-status="{{$col.Status}}"><h2>{{$col.Status}}</h2>
{{range $col.Cards}}<article class="card" draggable="true" data-issue="{{.ID}}">
<a href="/p/{{$.Key}}/i/{{.Number}}">{{$.Key}}-{{.Number}} {{.Title}}</a>
{{if .Assignee}}<span class="assignee">{{.Assignee}}</span>{{end}}
{{range .Labels}}<span class="label">{{.Name}}</span>{{end}}
<form class="move" method="post" action="/p/{{$.Key}}/i/{{.Number}}/move">
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
	data := boardData{Key: key, Statuses: statusColumns, Error: errMsg}
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

var issueTmpl = template.Must(template.New("issue").Parse(`<!doctype html>
<title>{{.Key}}-{{.Issue.Number}}</title>
<h1>{{.Key}}-{{.Issue.Number}} {{.Issue.Title}}</h1>
<p class="status">{{.Issue.Status}}</p>
{{if .Issue.Body}}<div class="body">{{.Issue.Body}}</div>{{end}}
{{if .Progress}}<p class="progress">{{.Progress}}</p>{{end}}`))

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
	_ = issueTmpl.Execute(w, map[string]any{"Key": key, "Issue": issue, "Progress": progress})
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

type commentView struct {
	ID         string  `json:"id"`
	DocVersion *string `json:"doc_version"`
	Parent     *string `json:"parent"`
	Anchor     *string `json:"anchor"`
	Author     string  `json:"author"`
	Body       string  `json:"body"`
}

var docTmpl = template.Must(template.New("doc").Parse(`<!doctype html>
<title>{{.Doc.Title}}</title>
<h1>{{.Doc.Title}}</h1>
<p class="version" data-version="{{.Doc.Version.Number}}">version {{.Doc.Version.Number}}</p>
<nav class="versions">
{{range .Versions}}<a href="/d/{{$.Doc.ID}}?version={{.Number}}">v{{.Number}}</a> {{end}}
</nav>
<main class="doc">{{.Rendered}}</main>
<aside class="discussion">
{{range .Comments}}<div class="comment{{if .Parent}} reply{{end}}" data-comment="{{.ID}}"{{if .Anchor}} data-anchor="{{.Anchor}}"{{end}} data-version="{{.VersionNumber}}">
<span class="author">{{.Author}}</span> <span class="on-version">on v{{.VersionNumber}}</span>
<p>{{.Body}}</p>
<form class="reply-form" method="post" action="/d/{{$.Doc.ID}}/comment">
<input type="hidden" name="doc_version" value="{{.DocVersionID}}">
<input type="hidden" name="parent" value="{{.ID}}">
<input name="body" placeholder="reply"><button>Reply</button>
</form>
</div>{{end}}
<form class="comment-form" method="post" action="/d/{{$.Doc.ID}}/comment">
<input type="hidden" name="doc_version" value="{{.Doc.Version.ID}}">
<input name="anchor" placeholder="block-1">
<input name="body" placeholder="comment on this version"><button>Comment</button>
</form>
</aside>
<script>
// Poll for newer versions; refresh to the latest when one lands.
(function () {
  var shown = {{.Doc.Version.Number}};
  setInterval(function () {
    fetch('/d/{{.Doc.ID}}/poll?since=' + shown)
      .then(function (r) { return r.json(); })
      .then(function (p) { if (p.refresh) { window.location = '/d/{{.Doc.ID}}'; } })
      .catch(function () {});
  }, 5000);
})();
</script>`))

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
	all := []docComment{}
	for _, v := range versions {
		var list []commentView
		if err := s.get("/comments?doc_version="+url.QueryEscape(v.ID), &list); err != nil {
			htmlError(w, err)
			return
		}
		for _, c := range list {
			all = append(all, docComment{commentView: c, VersionNumber: v.Number, DocVersionID: v.ID})
		}
	}
	_ = docTmpl.Execute(w, map[string]any{
		"Doc": doc, "Rendered": renderMarkdown(doc.Version.Content),
		"Comments": all, "Versions": versions,
	})
}

// commentDoc posts a block-anchored comment on the SHOWN version.
func (s *Server) commentDoc(w http.ResponseWriter, r *http.Request) {
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
	http.Redirect(w, r, "/d/"+id, http.StatusSeeOther)
}

// pollDocument reports whether the document has advanced past the
// viewer's version — the page's refresh signal (AC-docweb-live).
func (s *Server) pollDocument(w http.ResponseWriter, r *http.Request) {
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
	var t threadView
	if err := s.get("/threads/"+r.PathValue("threadId"), &t); err != nil {
		htmlError(w, err)
		return
	}
	type turn struct {
		Speaker string `json:"speaker"`
		Text    string `json:"text"`
	}
	var turns []turn
	if err := json.Unmarshal(t.Transcript, &turns); err != nil {
		turns = []turn{{Speaker: "transcript", Text: string(t.Transcript)}}
	}
	_ = threadTmpl.Execute(w, map[string]any{"Title": t.Title, "Turns": turns})
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
