package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"sutra/internal/comments"
	"sutra/internal/docs"
	"sutra/internal/events"
	"sutra/internal/identity"
	"sutra/internal/issues"
	"sutra/internal/projects"
	"sutra/internal/review"
	"sutra/internal/threads"
)

// exportMeta is the bounded portion of ProjectExport — record
// metadata, never unbounded content. The content-bearing groups
// (comments, doc versions, review submissions, thread transcripts) are
// streamed around it row by row at serving time, so export memory
// stays at one record regardless of project size.
type exportMeta struct {
	Project        projects.Project    `json:"project"`
	Identities     []identity.Identity `json:"identities"`
	Labels         []issues.Label      `json:"labels"`
	IssueRelations []issues.Relation   `json:"issue_relations"`
}

// documentExport mirrors the contract's DocumentExport; import decodes
// into it.
type documentExport struct {
	Document docs.Document  `json:"document"`
	Versions []docs.Version `json:"versions"`
}

// exportPlan carries the id lists the streaming writer walks.
type exportPlan struct {
	meta       exportMeta
	issueIDs   []string
	docs       []docs.Document
	versionIDs []string
	reviews    []review.Review // metadata only; submissions stream
	threadIDs  []string
}

// assembleExportPlan collects id lists and identity references in one
// read transaction (AC-export-full) WITHOUT loading unbounded content —
// issue bodies, event payload sets, and every content group stream at
// write time.
// Cross-project blocks relations cannot round-trip through a
// single-project export and are excluded — normative on the
// exportProject contract description and logged in spec-gaps.md.
func assembleExportPlan(tx *sql.Tx, projectID string) (exportPlan, error) {
	var plan exportPlan
	var err error
	if plan.meta.Project, err = projects.GetTx(tx, projectID); err != nil {
		return plan, err
	}
	issueIDSet := map[string]bool{}
	labelSet := map[string]issues.Label{}
	identityIDs := map[string]bool{}
	irows, err := tx.Query(`SELECT id, assignee FROM issues WHERE project = ? ORDER BY number`, projectID)
	if err != nil {
		return plan, err
	}
	for irows.Next() {
		var id string
		var assignee *string
		if err := irows.Scan(&id, &assignee); err != nil {
			_ = irows.Close()
			return plan, err
		}
		issueIDSet[id] = true
		plan.issueIDs = append(plan.issueIDs, id) // number order for streaming
		if assignee != nil {
			identityIDs[*assignee] = true
		}
	}
	if err := irows.Err(); err != nil {
		_ = irows.Close()
		return plan, err
	}
	_ = irows.Close()
	for _, id := range plan.issueIDs {
		labels, err := issues.LabelsOf(tx, id)
		if err != nil {
			return plan, err
		}
		for _, l := range labels {
			labelSet[l.ID] = l
		}
	}
	plan.meta.Labels = make([]issues.Label, 0, len(labelSet))
	for _, l := range labelSet {
		plan.meta.Labels = append(plan.meta.Labels, l)
	}
	slices.SortFunc(plan.meta.Labels, func(a, b issues.Label) int { return strings.Compare(a.Name, b.Name) })

	plan.meta.IssueRelations = []issues.Relation{}
	rows, err := tx.Query(`SELECT id, kind, from_issue, to_issue FROM issue_relations`)
	if err != nil {
		return plan, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var rel issues.Relation
		if err := rows.Scan(&rel.ID, &rel.Kind, &rel.From, &rel.To); err != nil {
			return plan, err
		}
		if issueIDSet[rel.From] && issueIDSet[rel.To] {
			plan.meta.IssueRelations = append(plan.meta.IssueRelations, rel)
		}
	}
	if err := rows.Err(); err != nil {
		return plan, err
	}

	// Documents: metadata plus per-version (id, author) — content
	// stays in the store until the writer streams it.
	if plan.docs, err = docs.ListByProject(tx, projectID); err != nil {
		return plan, err
	}
	for _, d := range plan.docs {
		vrows, err := tx.Query(`SELECT id, author FROM doc_versions WHERE document = ? ORDER BY number`, d.ID)
		if err != nil {
			return plan, err
		}
		for vrows.Next() {
			var id, author string
			if err := vrows.Scan(&id, &author); err != nil {
				_ = vrows.Close()
				return plan, err
			}
			plan.versionIDs = append(plan.versionIDs, id)
			identityIDs[author] = true
		}
		if err := vrows.Err(); err != nil {
			_ = vrows.Close()
			return plan, err
		}
		_ = vrows.Close()
	}

	// Reviews: metadata only; submissions stream with content later.
	for _, id := range plan.issueIDs {
		list, err := review.List(tx, id, "", "")
		if err != nil {
			return plan, err
		}
		for _, r := range list {
			identityIDs[r.Author] = true
			plan.reviews = append(plan.reviews, r)
		}
	}

	// Comment authors, via metadata-only scans per anchor.
	reviewIDs := make([]string, 0, len(plan.reviews))
	for _, r := range plan.reviews {
		reviewIDs = append(reviewIDs, r.ID)
	}
	for _, group := range []struct {
		column string
		ids    []string
	}{{"issue", plan.issueIDs}, {"doc_version", plan.versionIDs}, {"review", reviewIDs}} {
		for _, id := range group.ids {
			arows, err := tx.Query(`SELECT DISTINCT author FROM comments WHERE `+group.column+` = ?`, id)
			if err != nil {
				return plan, err
			}
			for arows.Next() {
				var author string
				if err := arows.Scan(&author); err != nil {
					_ = arows.Close()
					return plan, err
				}
				identityIDs[author] = true
			}
			if err := arows.Err(); err != nil {
				_ = arows.Close()
				return plan, err
			}
			_ = arows.Close()
		}
	}

	// Threads by id, deduplicated: an anchor may name BOTH the project
	// and one of its issues, and a twice-exported id would collide
	// with itself at import.
	seenThreads := map[string]bool{}
	collect := func(t threads.Thread) error {
		if !seenThreads[t.ID] {
			seenThreads[t.ID] = true
			plan.threadIDs = append(plan.threadIDs, t.ID)
		}
		return nil
	}
	if err := threads.SearchEach(tx, nil, nil, &projectID, collect); err != nil {
		return plan, err
	}
	for _, id := range plan.issueIDs {
		if err := threads.ListByIssueEach(tx, id, collect); err != nil {
			return plan, err
		}
	}

	// Event actors, collected without materializing event rows; the
	// rows themselves stream in feed order at write time.
	arows, err := tx.Query(`SELECT DISTINCT actor FROM events`)
	if err != nil {
		return plan, err
	}
	for arows.Next() {
		var actor, probe string
		if err := arows.Scan(&actor); err != nil {
			_ = arows.Close()
			return plan, err
		}
		// Only actors of in-scope events join the export's identities.
		err := tx.QueryRow(`SELECT id FROM events WHERE actor = ? AND (subject = ? OR subject IN (SELECT id FROM issues WHERE project = ?)) LIMIT 1`,
			actor, projectID, projectID).Scan(&probe)
		if err == nil {
			identityIDs[actor] = true
		} else if err != sql.ErrNoRows {
			_ = arows.Close()
			return plan, err
		}
	}
	if err := arows.Err(); err != nil {
		_ = arows.Close()
		return plan, err
	}
	_ = arows.Close()

	plan.meta.Identities = []identity.Identity{}
	for _, id := range sortedKeys(identityIDs) {
		ident, err := identity.Get(tx, id)
		if err != nil {
			return plan, err
		}
		plan.meta.Identities = append(plan.meta.Identities, ident)
	}
	return plan, nil
}

// sortedKeys makes export assembly deterministic — map iteration order
// must never shape the payload.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// jsonArrayWriter streams a JSON array element by element.
type jsonArrayWriter struct {
	w     http.ResponseWriter
	first bool
}

func (a *jsonArrayWriter) open(name string) {
	_, _ = a.w.Write([]byte(`,"` + name + `":[`))
	a.first = true
}

func (a *jsonArrayWriter) elem(raw []byte) {
	if !a.first {
		_, _ = a.w.Write([]byte{','})
	}
	a.first = false
	_, _ = a.w.Write(raw)
}

func (a *jsonArrayWriter) marshalElem(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	a.elem(raw)
	return nil
}

func (a *jsonArrayWriter) close() { _, _ = a.w.Write([]byte{']'}) }

// reviewMetaShadow is review.Review minus submissions, so the writer
// can splice streamed full-content submissions into its place.
type reviewMetaShadow struct {
	ID                 string  `json:"id"`
	Issue              string  `json:"issue"`
	Branch             *string `json:"branch,omitempty"`
	Commit             *string `json:"commit,omitempty"`
	DocVersion         *string `json:"doc_version,omitempty"`
	Session            *string `json:"session,omitempty"`
	Summary            *string `json:"summary,omitempty"`
	Author             string  `json:"author"`
	State              string  `json:"state"`
	Revision           int64   `json:"revision"`
	LatestVerdictEvent *string `json:"latest_verdict_event,omitempty"`
	Consumed           *string `json:"consumed,omitempty"`
	ConsumedRevision   *int64  `json:"consumed_revision,omitempty"`
	CloseUsed          *string `json:"close_used,omitempty"`
	Created            string  `json:"created"`
}

// exportProject streams the full export (AC-export-full): the bounded
// metadata envelope marshals once; comments, doc versions, review
// submissions, and transcripts — all unbounded per AC-comment-no-cap
// and the store's physical limits — stream one row at a time. After
// the first byte the status is committed; a mid-stream failure can
// only truncate.
func (s *server) exportProject(w http.ResponseWriter, r *http.Request) {
	tx, err := s.db.Begin()
	if err != nil {
		writeError(w, errorFrom(err))
		return
	}
	defer func() { _ = tx.Rollback() }()
	plan, err := assembleExportPlan(tx, r.PathValue("projectId"))
	if err != nil {
		writeError(w, errorFrom(err))
		return
	}
	raw, err := json.Marshal(plan.meta)
	if err != nil {
		writeError(w, errorFrom(err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw[:len(raw)-1])
	arr := &jsonArrayWriter{w: w}

	// Issues stream one row at a time — bodies are unbounded strings.
	arr.open("issues")
	for _, id := range plan.issueIDs {
		issue, err := issues.Get(tx, id)
		if err != nil {
			return // status committed; truncation is the only signal
		}
		if err := arr.marshalElem(issue); err != nil {
			return
		}
	}
	arr.close()

	// Events stream in feed order; out-of-scope subjects skip in the
	// scan, so one ordered pass serves any number of subjects.
	arr.open("events")
	if err := streamProjectEvents(tx, plan.meta.Project.ID, plan.issueIDs, arr); err != nil {
		return
	}
	arr.close()

	arr.open("comments")
	reviewIDs := make([]string, 0, len(plan.reviews))
	for _, rv := range plan.reviews {
		reviewIDs = append(reviewIDs, rv.ID)
	}
	for _, group := range []struct {
		column string
		ids    []string
	}{{"issue", plan.issueIDs}, {"doc_version", plan.versionIDs}, {"review", reviewIDs}} {
		for _, id := range group.ids {
			if err := comments.EachByAnchor(tx, group.column, id, func(c comments.Comment) error {
				return arr.marshalElem(c)
			}); err != nil {
				return // status committed; truncation is the only signal
			}
		}
	}
	arr.close()

	arr.open("documents")
	for _, d := range plan.docs {
		docRaw, err := json.Marshal(d)
		if err != nil {
			return
		}
		if !arr.first {
			_, _ = w.Write([]byte{','})
		}
		arr.first = false
		_, _ = w.Write([]byte(`{"document":`))
		_, _ = w.Write(docRaw)
		_, _ = w.Write([]byte(`,"versions":[`))
		inner := &jsonArrayWriter{w: w, first: true}
		if err := docs.VersionsEach(tx, d.ID, func(v docs.Version) error {
			return inner.marshalElem(v)
		}); err != nil {
			return
		}
		_, _ = w.Write([]byte(`]}`))
	}
	arr.close()

	arr.open("reviews")
	for _, rv := range plan.reviews {
		shadow := reviewMetaShadow{
			ID: rv.ID, Issue: rv.Issue, Branch: rv.Branch, Commit: rv.Commit,
			DocVersion: rv.DocVersion, Session: rv.Session, Summary: rv.Summary,
			Author: rv.Author, State: rv.State, Revision: rv.Revision,
			LatestVerdictEvent: rv.LatestVerdictEvent, Consumed: rv.Consumed,
			ConsumedRevision: rv.ConsumedRevision, CloseUsed: rv.CloseUsed, Created: rv.Created,
		}
		metaRaw, err := json.Marshal(shadow)
		if err != nil {
			return
		}
		if !arr.first {
			_, _ = w.Write([]byte{','})
		}
		arr.first = false
		_, _ = w.Write(metaRaw[:len(metaRaw)-1])
		_, _ = w.Write([]byte(`,"submissions":[`))
		inner := &jsonArrayWriter{w: w, first: true}
		if err := eachSubmission(tx, rv.ID, func(sub review.Submission) error {
			return inner.marshalElem(sub)
		}); err != nil {
			return
		}
		_, _ = w.Write([]byte(`]}`))
	}
	arr.close()

	arr.open("threads")
	for _, id := range plan.threadIDs {
		t, err := threads.Get(tx, id)
		if err != nil {
			return
		}
		one, apiErr := threadJSON(t)
		if apiErr != nil {
			return
		}
		arr.elem(one)
	}
	arr.close()
	_, _ = w.Write([]byte{'}'})
}

// streamProjectEvents walks the whole feed in order, emitting events
// whose subject is the project or one of its issues.
func streamProjectEvents(tx *sql.Tx, projectID string, issueIDs []string, arr *jsonArrayWriter) error {
	scope := map[string]bool{projectID: true}
	for _, id := range issueIDs {
		scope[id] = true
	}
	rows, err := tx.Query(`SELECT id, kind, subject, operation, actor, payload, created FROM events ORDER BY seq`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var e events.Event
		var payload sql.NullString
		if err := rows.Scan(&e.ID, &e.Kind, &e.Subject, &e.Operation, &e.Actor, &payload, &e.Created); err != nil {
			return err
		}
		if !scope[e.Subject] {
			continue
		}
		// Payload bytes splice VERBATIM around the marshaled metadata —
		// json.Marshal would compact them, and imports promise the
		// original bytes back.
		shadow := e
		shadow.Payload = nil
		raw, err := json.Marshal(shadow)
		if err != nil {
			return err
		}
		if payload.Valid {
			raw = append(raw[:len(raw)-1], []byte(`,"payload":`)...)
			raw = append(raw, payload.String...)
			raw = append(raw, '}')
		}
		arr.elem(raw)
	}
	return rows.Err()
}

// eachSubmission streams a review's full submission history including
// stored content, ordered by revision.
func eachSubmission(tx *sql.Tx, reviewID string, fn func(review.Submission) error) error {
	rows, err := tx.Query(`
		SELECT id, review, revision, branch, commit_sha, base_commit, doc_version, session, content, created
		FROM review_submissions WHERE review = ? ORDER BY revision`, reviewID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var sub review.Submission
		if err := rows.Scan(&sub.ID, &sub.Review, &sub.Revision, &sub.Branch, &sub.Commit,
			&sub.BaseCommit, &sub.DocVersion, &sub.Session, &sub.Content, &sub.Created); err != nil {
			return err
		}
		if err := fn(sub); err != nil {
			return err
		}
	}
	return rows.Err()
}
