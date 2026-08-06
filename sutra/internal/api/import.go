package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"sutra/internal/comments"
	"sutra/internal/events"
	"sutra/internal/identity"
	"sutra/internal/issues"
	"sutra/internal/projects"
	"sutra/internal/review"
	"sutra/internal/threads"
)

// importPayload is projectExport plus the threads group, decoded
// strictly: unknown fields anywhere — a leaked feed_watermark
// included — reject the payload as malformed.
type importPayload struct {
	Project        projects.Project    `json:"project"`
	Identities     []identity.Identity `json:"identities"`
	Issues         []issues.Issue      `json:"issues"`
	Comments       []comments.Comment  `json:"comments"`
	Labels         []issues.Label      `json:"labels"`
	IssueRelations []issues.Relation   `json:"issue_relations"`
	Documents      []documentExport    `json:"documents"`
	Threads        []threads.Thread    `json:"threads"`
	Reviews        []review.Review     `json:"reviews"`
	Events         []events.Event      `json:"events"`
}

func malformedImport(format string, args ...any) *apiError {
	return &apiError{status: http.StatusBadRequest, code: "malformed-import", message: fmt.Sprintf(format, args...)}
}

// importProject reconstructs a project from an export
// (AC-import-round-trip): UUIDs preserved, records inserted verbatim,
// rejected whole on any collision (AC-import-collision) or invariant
// violation — no partial imports.
func (s *server) importProject(w http.ResponseWriter, r *http.Request) {
	actor := r.URL.Query().Get("actor")
	// Decode and payload validation run in the PREPARE stage — before
	// the idempotency reservation takes SQLite's write lock — so a
	// multi-gigabyte decode never blocks other mutations. Only the
	// collision probe and the inserts hold the writer.
	s.idempotentPrepared(w, r, func(r *http.Request) (any, *apiError) {
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		var p importPayload
		if err := dec.Decode(&p); err != nil {
			return nil, malformedImport("decode export: %v", err)
		}
		if err := dec.Decode(new(any)); err != io.EOF {
			return nil, malformedImport("trailing data after the export payload")
		}
		if apiErr := validateImport(&p, actor); apiErr != nil {
			return nil, apiErr
		}
		return &p, nil
	}, func(tx *sql.Tx, prepped any) (int, any, *apiError) {
		p := prepped.(*importPayload)
		if apiErr := s.checkImportCollisions(tx, p); apiErr != nil {
			return 0, nil, apiErr
		}
		if apiErr := insertImport(tx, p); apiErr != nil {
			return 0, nil, apiErr
		}
		// Exactly one appended audit event; per-record events arrived
		// verbatim above and are never re-stamped.
		payload := fmt.Sprintf(`{"project":%q}`, p.Project.ID)
		if _, err := events.Emit(tx, "project.imported", p.Project.ID, events.NewOperation(), actor, &payload); err != nil {
			return 0, nil, errorFrom(err)
		}
		return http.StatusCreated, p.Project, nil
	})
}

// validateImport checks internal consistency: every reference resolves
// within the export, the completion invariant holds, and review
// consumption/verdict stamps are coherent (REQ-import-export's
// malformed-review scenarios).
func validateImport(p *importPayload, actor string) *apiError {
	if apiErr := validateImportShapes(p); apiErr != nil {
		return apiErr
	}
	identitySet := map[string]bool{}
	for _, i := range p.Identities {
		identitySet[i.ID] = true
	}
	if actor == "" || !identitySet[actor] {
		// AC-import-actor: an actor outside the export's identities
		// would break the round-trip guarantee.
		return &apiError{status: http.StatusBadRequest, code: "bad-request",
			message: "actor must reference an identity contained in the export's identities"}
	}
	issueSet := map[string]bool{}
	for _, i := range p.Issues {
		if i.Project != p.Project.ID {
			return malformedImport("issue %s belongs to another project", i.ID)
		}
		if i.Assignee != nil && !identitySet[*i.Assignee] {
			return malformedImport("issue %s assignee is not in identities", i.ID)
		}
		issueSet[i.ID] = true
	}
	labelSet := map[string]bool{}
	for _, l := range p.Labels {
		labelSet[l.ID] = true
	}
	for _, i := range p.Issues {
		for _, l := range i.Labels {
			if !labelSet[l.ID] {
				return malformedImport("issue %s carries label %s absent from labels", i.ID, l.ID)
			}
		}
	}
	children := map[string][]string{}
	for _, rel := range p.IssueRelations {
		if !issueSet[rel.From] || !issueSet[rel.To] {
			return malformedImport("relation %s references an issue outside the export", rel.ID)
		}
		if rel.Kind == "parent_of" {
			children[rel.From] = append(children[rel.From], rel.To)
		}
	}
	// Completion invariant: a complete issue with any active
	// descendant — at any depth, including beneath deferred children —
	// is unreachable state; imported state gets no cascade to repair
	// it (AC-parent-reopen-cascade).
	statuses := map[string]string{}
	for _, i := range p.Issues {
		statuses[i.ID] = i.Status
	}
	for _, i := range p.Issues {
		if i.Status != issues.StatusComplete {
			continue
		}
		if id, found := findActiveDescendant(i.ID, children, statuses); found {
			return malformedImport("complete issue %s has active descendant %s", i.ID, id)
		}
	}
	docVersionSet := map[string]bool{}
	for _, d := range p.Documents {
		if d.Document.Project != p.Project.ID {
			return malformedImport("document %s belongs to another project", d.Document.ID)
		}
		if d.Document.Issue != nil && !issueSet[*d.Document.Issue] {
			return malformedImport("document %s ties to an issue outside the export", d.Document.ID)
		}
		for _, v := range d.Versions {
			if v.Document != d.Document.ID {
				return malformedImport("version %s belongs to another document", v.ID)
			}
			if !identitySet[v.Author] {
				return malformedImport("version %s author is not in identities", v.ID)
			}
			docVersionSet[v.ID] = true
		}
	}
	reviewSet := map[string]bool{}
	for i := range p.Reviews {
		if apiErr := validateImportedReview(&p.Reviews[i], p, issueSet, identitySet, docVersionSet); apiErr != nil {
			return apiErr
		}
		reviewSet[p.Reviews[i].ID] = true
	}
	for _, c := range p.Comments {
		if !identitySet[c.Author] {
			return malformedImport("comment %s author is not in identities", c.ID)
		}
		set := 0
		if c.Issue != nil {
			if !issueSet[*c.Issue] {
				return malformedImport("comment %s anchors to an issue outside the export", c.ID)
			}
			set++
		}
		if c.DocVersion != nil {
			if !docVersionSet[*c.DocVersion] {
				return malformedImport("comment %s anchors to a doc version outside the export", c.ID)
			}
			set++
		}
		if c.Review != nil {
			if !reviewSet[*c.Review] {
				return malformedImport("comment %s anchors to a review outside the export", c.ID)
			}
			set++
		}
		if set != 1 {
			return malformedImport("comment %s must anchor to exactly one target", c.ID)
		}
		if (c.ReviewRevision != nil) != (c.Review != nil) {
			return malformedImport("comment %s review_revision must accompany review exactly", c.ID)
		}
	}
	for _, t := range p.Threads {
		if t.Project == nil && t.Issue == nil {
			return malformedImport("thread %s carries no anchor", t.ID)
		}
		if t.Project != nil && *t.Project != p.Project.ID {
			return malformedImport("thread %s anchors to another project", t.ID)
		}
		if t.Issue != nil && !issueSet[*t.Issue] {
			return malformedImport("thread %s anchors to an issue outside the export", t.ID)
		}
	}
	for _, e := range p.Events {
		if !identitySet[e.Actor] {
			return malformedImport("event %s actor is not in identities", e.ID)
		}
		if e.Subject != p.Project.ID && !issueSet[e.Subject] {
			return malformedImport("event %s subject is outside the export", e.ID)
		}
	}
	return nil
}

// validateImportShapes enforces what DisallowUnknownFields cannot:
// UUID formats on every record id, RFC 3339 timestamps, closed enums,
// required strings, and the exactly-one deliverable shape — a payload
// the OpenAPI schema would reject must never persist.
func validateImportShapes(p *importPayload) *apiError {
	uuidOf := func(what, id string) *apiError {
		if !isUUID(id) {
			return malformedImport("%s id %q is not a uuid", what, id)
		}
		return nil
	}
	timeOf := func(what, value string) *apiError {
		if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
			return malformedImport("%s carries a malformed timestamp %q", what, value)
		}
		return nil
	}
	if apiErr := uuidOf("project", p.Project.ID); apiErr != nil {
		return apiErr
	}
	if p.Project.Key == "" || p.Project.Name == "" {
		return malformedImport("project key and name are required")
	}
	for _, i := range p.Identities {
		if apiErr := uuidOf("identity", i.ID); apiErr != nil {
			return apiErr
		}
		if i.Handle == "" || (i.Kind != "human" && i.Kind != "agent") {
			return malformedImport("identity %s has a bad handle or kind", i.ID)
		}
	}
	validStatus := map[string]bool{"open": true, "in-progress": true, "blocked": true, "deferred": true, "complete": true}
	for _, i := range p.Issues {
		if apiErr := uuidOf("issue", i.ID); apiErr != nil {
			return apiErr
		}
		if i.Title == "" || !validStatus[i.Status] || i.Number < 1 {
			return malformedImport("issue %s has a bad title, status, or number", i.ID)
		}
		for _, field := range []struct{ what, value string }{{"issue created", i.Created}, {"issue updated", i.Updated}} {
			if apiErr := timeOf(field.what, field.value); apiErr != nil {
				return apiErr
			}
		}
	}
	for _, l := range p.Labels {
		if apiErr := uuidOf("label", l.ID); apiErr != nil {
			return apiErr
		}
		if l.Name == "" {
			return malformedImport("label %s has no name", l.ID)
		}
	}
	for _, rel := range p.IssueRelations {
		if apiErr := uuidOf("relation", rel.ID); apiErr != nil {
			return apiErr
		}
		if rel.Kind != "parent_of" && rel.Kind != "blocks" {
			return malformedImport("relation %s has kind %q", rel.ID, rel.Kind)
		}
	}
	for _, d := range p.Documents {
		if apiErr := uuidOf("document", d.Document.ID); apiErr != nil {
			return apiErr
		}
		if d.Document.Title == "" {
			return malformedImport("document %s has no title", d.Document.ID)
		}
		for _, v := range d.Versions {
			if apiErr := uuidOf("doc version", v.ID); apiErr != nil {
				return apiErr
			}
			if apiErr := timeOf("doc version created", v.Created); apiErr != nil {
				return apiErr
			}
		}
	}
	for _, t := range p.Threads {
		if apiErr := uuidOf("thread", t.ID); apiErr != nil {
			return apiErr
		}
		if t.Title == "" || len(t.Transcript) == 0 {
			return malformedImport("thread %s lacks a title or transcript", t.ID)
		}
		if apiErr := timeOf("thread imported_at", t.ImportedAt); apiErr != nil {
			return apiErr
		}
	}
	validState := map[string]bool{"open": true, "changes-requested": true, "approved": true}
	for _, r := range p.Reviews {
		if apiErr := uuidOf("review", r.ID); apiErr != nil {
			return apiErr
		}
		if !validState[r.State] {
			return malformedImport("review %s has state %q", r.ID, r.State)
		}
		if apiErr := timeOf("review created", r.Created); apiErr != nil {
			return apiErr
		}
		code := r.Branch != nil && r.Commit != nil
		doc := r.DocVersion != nil
		if code == doc || (r.Branch != nil) != (r.Commit != nil) {
			return malformedImport("review %s must carry exactly one deliverable: a branch pinned at a commit, or a doc version", r.ID)
		}
		for _, sub := range r.Submissions {
			if apiErr := uuidOf("submission", sub.ID); apiErr != nil {
				return apiErr
			}
			subCode := sub.Branch != nil && sub.Commit != nil
			subDoc := sub.DocVersion != nil
			if subCode == subDoc || (sub.Branch != nil) != (sub.Commit != nil) {
				return malformedImport("review %s submission %d deliverable shape is invalid", r.ID, sub.Revision)
			}
			if apiErr := timeOf("submission created", sub.Created); apiErr != nil {
				return apiErr
			}
		}
	}
	for _, c := range p.Comments {
		if apiErr := uuidOf("comment", c.ID); apiErr != nil {
			return apiErr
		}
		if c.Body == "" {
			return malformedImport("comment %s has no body", c.ID)
		}
		if apiErr := timeOf("comment created", c.Created); apiErr != nil {
			return apiErr
		}
	}
	for _, e := range p.Events {
		if apiErr := uuidOf("event", e.ID); apiErr != nil {
			return apiErr
		}
		if e.Kind == "" {
			return malformedImport("event %s has no kind", e.ID)
		}
		if apiErr := timeOf("event created", e.Created); apiErr != nil {
			return apiErr
		}
	}
	return nil
}

// findActiveDescendant walks the parent_of tree below root looking for
// an active (open, in-progress, blocked) issue at any depth.
func findActiveDescendant(root string, children map[string][]string, statuses map[string]string) (string, bool) {
	stack := append([]string{}, children[root]...)
	seen := map[string]bool{}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[id] {
			continue
		}
		seen[id] = true
		if issues.Active(statuses[id]) {
			return id, true
		}
		stack = append(stack, children[id]...)
	}
	return "", false
}

// validateImportedReview enforces the ReviewExport invariants the
// contract makes normative for import: contiguous submissions,
// deliverable agreement, coherent consumption and verdict stamps.
func validateImportedReview(r *review.Review, p *importPayload, issueSet, identitySet, docVersionSet map[string]bool) *apiError {
	if !issueSet[r.Issue] {
		return malformedImport("review %s names an issue outside the export", r.ID)
	}
	if !identitySet[r.Author] {
		return malformedImport("review %s author is not in identities", r.ID)
	}
	if len(r.Submissions) == 0 {
		return malformedImport("review %s carries no submissions", r.ID)
	}
	if int64(len(r.Submissions)) != r.Revision {
		return malformedImport("review %s submissions do not span revisions 1..%d", r.ID, r.Revision)
	}
	for i, sub := range r.Submissions {
		if sub.Revision != int64(i+1) {
			return malformedImport("review %s submission revisions are not contiguous", r.ID)
		}
		if sub.Review != r.ID {
			return malformedImport("submission %s belongs to another review", sub.ID)
		}
		code := sub.Branch != nil || sub.Commit != nil
		if code && (sub.Content == nil || *sub.Content == "") {
			// ReviewSubmissionExport: the durable copy is the only one.
			return malformedImport("review %s code submission %d omits its content", r.ID, sub.Revision)
		}
		if !code && (sub.DocVersion == nil || !docVersionSet[*sub.DocVersion]) {
			return malformedImport("review %s submission %d names no resolvable deliverable", r.ID, sub.Revision)
		}
	}
	latest := r.Submissions[len(r.Submissions)-1]
	if !deliverableMatches(r, latest) {
		return malformedImport("review %s top-level deliverable disagrees with its latest submission", r.ID)
	}
	// Consumption stamps travel together and freeze an approved
	// verdict at the current revision — half-consumed, verdict-torn,
	// or revision-lagging stamps are unreachable state.
	if (r.Consumed == nil) != (r.ConsumedRevision == nil) {
		return malformedImport("review %s consumption stamp is incomplete", r.ID)
	}
	if r.CloseUsed != nil && r.Consumed == nil {
		return malformedImport("review %s is close-used but carries no consumption fields", r.ID)
	}
	if r.Consumed != nil {
		if r.State != review.StateApproved {
			return malformedImport("review %s is consumed but not approved — consumption fences the verdict", r.ID)
		}
		if *r.ConsumedRevision != r.Revision {
			return malformedImport("review %s consumed revision lags its current revision", r.ID)
		}
	}
	// A verdict-bearing review must name its latest verdict event —
	// without it, the review could never be closed, consumed, or
	// resubmitted — and that event must be its ACTUAL latest in the
	// exported events.
	if r.State == review.StateApproved || r.State == review.StateChangesRequested {
		if r.LatestVerdictEvent == nil {
			return malformedImport("review %s carries a verdict but no latest verdict event", r.ID)
		}
		actualID, actualKind := latestVerdictEventFor(r.ID, p.Events)
		if actualID == "" || actualID != *r.LatestVerdictEvent {
			return malformedImport("review %s latest_verdict_event is not its actual latest verdict event", r.ID)
		}
		if actualKind != "review."+r.State {
			return malformedImport("review %s state %s disagrees with its latest verdict event's kind %s", r.ID, r.State, actualKind)
		}
	}
	return nil
}

func deliverableMatches(r *review.Review, latest review.Submission) bool {
	eq := func(a, b *string) bool {
		if (a == nil) != (b == nil) {
			return false
		}
		return a == nil || *a == *b
	}
	return eq(r.Branch, latest.Branch) && eq(r.Commit, latest.Commit) && eq(r.DocVersion, latest.DocVersion)
}

// latestVerdictEventFor scans the export's events — already in feed
// order — for the last verdict event naming the review, returning its
// id and kind so the caller can require kind/state agreement.
func latestVerdictEventFor(reviewID string, list []events.Event) (string, string) {
	id, kind := "", ""
	for _, e := range list {
		if e.Kind != "review.approved" && e.Kind != "review.changes-requested" {
			continue
		}
		raw, err := json.Marshal(e.Payload)
		if err != nil {
			continue
		}
		var payload struct {
			Review string `json:"review"`
		}
		if json.Unmarshal(raw, &payload) == nil && payload.Review == reviewID {
			id, kind = e.ID, e.Kind
		}
	}
	return id, kind
}

// checkImportCollisions rejects the payload whole if ANY of its UUIDs
// already exists on this server (AC-import-collision), naming every
// conflict.
func (s *server) checkImportCollisions(tx *sql.Tx, p *importPayload) *apiError {
	type probe struct {
		table string
		ids   []string
	}
	probes := []probe{
		{"projects", []string{p.Project.ID}},
		{"identities", nil},
		{"issues", nil},
		{"labels", nil},
		{"issue_relations", nil},
		{"documents", nil},
		{"doc_versions", nil},
		{"threads", nil},
		{"reviews", nil},
		{"review_submissions", nil},
		{"comments", nil},
		{"events", nil},
	}
	for _, i := range p.Identities {
		probes[1].ids = append(probes[1].ids, i.ID)
	}
	for _, i := range p.Issues {
		probes[2].ids = append(probes[2].ids, i.ID)
	}
	for _, l := range p.Labels {
		probes[3].ids = append(probes[3].ids, l.ID)
	}
	for _, r := range p.IssueRelations {
		probes[4].ids = append(probes[4].ids, r.ID)
	}
	for _, d := range p.Documents {
		probes[5].ids = append(probes[5].ids, d.Document.ID)
		for _, v := range d.Versions {
			probes[6].ids = append(probes[6].ids, v.ID)
		}
	}
	for _, t := range p.Threads {
		probes[7].ids = append(probes[7].ids, t.ID)
	}
	for _, r := range p.Reviews {
		probes[8].ids = append(probes[8].ids, r.ID)
		for _, sub := range r.Submissions {
			probes[9].ids = append(probes[9].ids, sub.ID)
		}
	}
	for _, c := range p.Comments {
		probes[10].ids = append(probes[10].ids, c.ID)
	}
	for _, e := range p.Events {
		probes[11].ids = append(probes[11].ids, e.ID)
	}
	conflicts := []string{}
	for _, pr := range probes {
		for _, chunk := range chunkIDs(pr.ids, 500) {
			query := `SELECT id FROM ` + pr.table + ` WHERE id IN (?` + strings.Repeat(",?", len(chunk)-1) + `)`
			args := make([]any, len(chunk))
			for i, id := range chunk {
				args[i] = id
			}
			rows, err := tx.Query(query, args...)
			if err != nil {
				return errorFrom(err)
			}
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					_ = rows.Close()
					return errorFrom(err)
				}
				conflicts = append(conflicts, id)
			}
			if err := rows.Err(); err != nil {
				_ = rows.Close()
				return errorFrom(err)
			}
			_ = rows.Close()
		}
	}
	if len(conflicts) > 0 {
		return &apiError{status: http.StatusConflict, code: "uuid-collision",
			message: fmt.Sprintf("%d record ids already exist on this server", len(conflicts)), conflicts: conflicts}
	}
	return nil
}

func chunkIDs(ids []string, size int) [][]string {
	var out [][]string
	for len(ids) > size {
		out = append(out, ids[:size])
		ids = ids[size:]
	}
	if len(ids) > 0 {
		out = append(out, ids)
	}
	return out
}

// insertImport writes every record verbatim — UUIDs, timestamps, and
// server-stamped fields preserved, never recomputed.
func insertImport(tx *sql.Tx, p *importPayload) *apiError {
	fail := func(what string, err error) *apiError {
		return &apiError{status: http.StatusInternalServerError, code: "bad-request", message: fmt.Sprintf("import %s: %v", what, err)}
	}
	proj := p.Project
	defaultBranch := "main"
	if proj.DefaultBranch != nil {
		defaultBranch = *proj.DefaultBranch
	}
	if _, err := tx.Exec(`INSERT INTO projects (id, key, name, description, repo_path, default_branch, archived_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		proj.ID, proj.Key, proj.Name, proj.Description, proj.RepoPath, defaultBranch, proj.ArchivedAt); err != nil {
		return fail("project", err)
	}
	for _, i := range p.Identities {
		if _, err := tx.Exec(`INSERT INTO identities (id, handle, kind, display_name) VALUES (?, ?, ?, ?)`,
			i.ID, i.Handle, i.Kind, i.DisplayName); err != nil {
			return fail("identity", err)
		}
	}
	maxNumber := int64(0)
	for _, i := range p.Issues {
		// assigned_at is internal FIFO state absent from the contract
		// (spec gap); imported assignees inherit the issue's updated
		// stamp as their queue position.
		var assignedAt *string
		if i.Assignee != nil {
			at := i.Updated
			assignedAt = &at
		}
		if _, err := tx.Exec(`INSERT INTO issues (id, project, number, title, body, status, assignee, assigned_at, subtree_revision, created, updated)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			i.ID, i.Project, i.Number, i.Title, i.Body, i.Status, i.Assignee, assignedAt, i.SubtreeRevision, i.Created, i.Updated); err != nil {
			return fail("issue", err)
		}
		if i.Number > maxNumber {
			maxNumber = i.Number
		}
	}
	if maxNumber > 0 {
		// Future creates continue the display sequence past the import.
		if _, err := tx.Exec(`INSERT INTO issue_numbers (project, next) VALUES (?, ?)`, proj.ID, maxNumber); err != nil {
			return fail("issue numbers", err)
		}
	}
	for _, l := range p.Labels {
		if _, err := tx.Exec(`INSERT INTO labels (id, name, color) VALUES (?, ?, ?)`, l.ID, l.Name, l.Color); err != nil {
			return fail("label", err)
		}
	}
	for _, i := range p.Issues {
		for _, l := range i.Labels {
			if _, err := tx.Exec(`INSERT INTO issue_labels (issue, label) VALUES (?, ?)`, i.ID, l.ID); err != nil {
				return fail("issue label", err)
			}
		}
	}
	for _, r := range p.IssueRelations {
		if _, err := tx.Exec(`INSERT INTO issue_relations (id, kind, from_issue, to_issue) VALUES (?, ?, ?, ?)`,
			r.ID, r.Kind, r.From, r.To); err != nil {
			return fail("relation", err)
		}
	}
	for _, d := range p.Documents {
		if _, err := tx.Exec(`INSERT INTO documents (id, project, issue, title, current_version) VALUES (?, ?, ?, ?, ?)`,
			d.Document.ID, d.Document.Project, d.Document.Issue, d.Document.Title, d.Document.CurrentVersion); err != nil {
			return fail("document", err)
		}
		for _, v := range d.Versions {
			if _, err := tx.Exec(`INSERT INTO doc_versions (id, document, number, content, author, created) VALUES (?, ?, ?, ?, ?, ?)`,
				v.ID, v.Document, v.Number, v.Content, v.Author, v.Created); err != nil {
				return fail("doc version", err)
			}
		}
	}
	for _, t := range p.Threads {
		if _, err := tx.Exec(`INSERT INTO threads (id, title, transcript, session, project, issue, imported_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			t.ID, t.Title, string(t.Transcript), t.Session, t.Project, t.Issue, t.ImportedAt); err != nil {
			return fail("thread", err)
		}
	}
	for _, r := range p.Reviews {
		if _, err := tx.Exec(`INSERT INTO reviews (id, issue, author, state, revision, session, summary, branch, commit_sha, doc_version, latest_verdict_event, consumed, consumed_revision, close_used, created)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.ID, r.Issue, r.Author, r.State, r.Revision, r.Session, r.Summary, r.Branch, r.Commit, r.DocVersion,
			r.LatestVerdictEvent, r.Consumed, r.ConsumedRevision, r.CloseUsed, r.Created); err != nil {
			return fail("review", err)
		}
		for _, sub := range r.Submissions {
			if _, err := tx.Exec(`INSERT INTO review_submissions (id, review, revision, branch, commit_sha, base_commit, doc_version, session, content, created)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				sub.ID, sub.Review, sub.Revision, sub.Branch, sub.Commit, sub.BaseCommit, sub.DocVersion, sub.Session, sub.Content, sub.Created); err != nil {
				return fail("submission", err)
			}
		}
	}
	for _, c := range p.Comments {
		if _, err := tx.Exec(`INSERT INTO comments (id, issue, doc_version, review, review_revision, parent, anchor, author, body, created)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			c.ID, c.Issue, c.DocVersion, c.Review, c.ReviewRevision, c.Parent, c.Anchor, c.Author, c.Body, c.Created); err != nil {
			return fail("comment", err)
		}
	}
	for _, e := range p.Events {
		var payload *string
		if e.Payload != nil {
			raw, err := json.Marshal(e.Payload)
			if err != nil {
				return fail("event payload", err)
			}
			str := string(raw)
			payload = &str
		}
		if _, err := tx.Exec(`INSERT INTO events (id, kind, subject, operation, actor, payload, created) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			e.ID, e.Kind, e.Subject, e.Operation, e.Actor, payload, e.Created); err != nil {
			return fail("event", err)
		}
	}
	return nil
}
