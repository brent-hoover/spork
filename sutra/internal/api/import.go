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
	"sutra/internal/docs"
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
// (AC-import-round-trip) in two STREAMING passes over the spooled
// body, so memory holds record metadata plus one content field at a
// time — never the whole payload. Pass A (the prepare stage, before
// the idempotency reservation takes the write lock) walks the payload
// stripping unbounded content to presence-preserving sentinels and
// validates the resulting metadata. Pass B rewinds the body and walks
// it again inside the transaction, inserting each record verbatim under
// deferred foreign keys, so caller-chosen field order cannot break
// referential inserts. Rejected whole on any collision
// (AC-import-collision) or invariant violation — no partial imports.
func (s *server) importProject(w http.ResponseWriter, r *http.Request) {
	actor := r.URL.Query().Get("actor")
	s.idempotentPrepared(w, r, func(r *http.Request) (any, *apiError) {
		meta, apiErr := collectImportMeta(r.Body)
		if apiErr != nil {
			return nil, apiErr
		}
		if apiErr := validateImport(meta, actor); apiErr != nil {
			return nil, apiErr
		}
		return meta, nil
	}, func(tx *sql.Tx, prepped any) (int, any, *apiError) {
		meta := prepped.(*importPayload)
		if apiErr := s.checkImportCollisions(tx, meta); apiErr != nil {
			return 0, nil, apiErr
		}
		// Deferred FKs let pass B insert in payload order regardless
		// of reference direction; enforcement lands at commit.
		if _, err := tx.Exec(`PRAGMA defer_foreign_keys = ON`); err != nil {
			return 0, nil, errorFrom(err)
		}
		if apiErr := insertImportStream(tx, r.Body, meta); apiErr != nil {
			return 0, nil, apiErr
		}
		// Exactly one appended audit event; per-record events arrived
		// verbatim above and are never re-stamped.
		payload := fmt.Sprintf(`{"project":%q}`, meta.Project.ID)
		if _, err := events.Emit(tx, "project.imported", meta.Project.ID, events.NewOperation(), actor, &payload); err != nil {
			return 0, nil, errorFrom(err)
		}
		return http.StatusCreated, meta.Project, nil
	})
}

// collectImportMeta is pass A: stream the payload, strip unbounded
// content to short presence-preserving sentinels, and return the
// bounded metadata for validation.
func collectImportMeta(body io.Reader) (*importPayload, *apiError) {
	meta := &importPayload{}
	const sentinel = "x"
	versionsByDoc := map[string][]docs.Version{}
	subsByReview := map[string][]review.Submission{}
	apiErr := walkImport(body, importCallbacks{
		project:  func(p projects.Project) error { meta.Project = p; return nil },
		identity: func(v identity.Identity) error { meta.Identities = append(meta.Identities, v); return nil },
		issue: func(v issues.Issue) error {
			// Bodies are unbounded and irrelevant to validation; the
			// insert pass re-reads them from the spool.
			if v.Body != nil && *v.Body != "" {
				s := sentinel
				v.Body = &s
			}
			meta.Issues = append(meta.Issues, v)
			return nil
		},
		comment: func(c comments.Comment) error {
			if c.Body != "" {
				c.Body = sentinel
			}
			meta.Comments = append(meta.Comments, c)
			return nil
		},
		label:    func(v issues.Label) error { meta.Labels = append(meta.Labels, v); return nil },
		relation: func(v issues.Relation) error { meta.IssueRelations = append(meta.IssueRelations, v); return nil },
		document: func(d docs.Document) error {
			meta.Documents = append(meta.Documents, documentExport{Document: d})
			return nil
		},
		docVersion: func(v docs.Version) error {
			v.Content = ""
			versionsByDoc[v.Document] = append(versionsByDoc[v.Document], v)
			return nil
		},
		thread: func(t threads.Thread) error {
			if len(t.Transcript) > 0 {
				t.Transcript = json.RawMessage(`0`)
			}
			meta.Threads = append(meta.Threads, t)
			return nil
		},
		review: func(rv review.Review) error { meta.Reviews = append(meta.Reviews, rv); return nil },
		submission: func(sub review.Submission) error {
			if sub.Content != nil && *sub.Content != "" {
				s := sentinel
				sub.Content = &s
			} // present-but-empty stays "", distinct from absent
			subsByReview[sub.Review] = append(subsByReview[sub.Review], sub)
			return nil
		},
		event: func(e events.Event) error {
			// Payloads are needed only where validation reads them:
			// the verdict-event agreement checks. Everything else
			// strips — the insert pass re-reads the original bytes.
			switch e.Kind {
			case "review.approved", "review.changes-requested", "issue.relation-removed":
				// Verdict agreement and the RelationRemovedPayload
				// requirement read these payloads; both are small.
			default:
				e.Payload = nil
			}
			meta.Events = append(meta.Events, e)
			return nil
		},
	})
	if apiErr != nil {
		return nil, apiErr
	}
	// Attach streamed children by their own foreign keys — property
	// order in the payload never matters. A child naming an unknown
	// parent must reject, not vanish.
	for i := range meta.Documents {
		id := meta.Documents[i].Document.ID
		meta.Documents[i].Versions = versionsByDoc[id]
		delete(versionsByDoc, id)
	}
	for id := range versionsByDoc {
		return nil, malformedImport("doc versions name unknown document %s", id)
	}
	for i := range meta.Reviews {
		id := meta.Reviews[i].ID
		meta.Reviews[i].Submissions = subsByReview[id]
		delete(subsByReview, id)
	}
	for id := range subsByReview {
		return nil, malformedImport("submissions name unknown review %s", id)
	}
	return meta, nil
}

// insertImportStream is pass B: re-walk the payload and insert every
// record verbatim — UUIDs, timestamps, and server-stamped fields
// preserved, never recomputed. Validation already passed on the
// metadata, so failures here are internal.
func insertImportStream(tx *sql.Tx, body io.Reader, meta *importPayload) *apiError {
	fail := func(what string, err error) error {
		return fmt.Errorf("import %s: %w", what, err)
	}
	apiErr := walkImport(body, importCallbacks{
		project: func(p projects.Project) error {
			defaultBranch := "main"
			if p.DefaultBranch != nil {
				defaultBranch = *p.DefaultBranch
			}
			if _, err := tx.Exec(`INSERT INTO projects (id, key, name, description, repo_path, default_branch, archived_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
				p.ID, p.Key, p.Name, p.Description, p.RepoPath, defaultBranch, p.ArchivedAt); err != nil {
				return fail("project", err)
			}
			return nil
		},
		identity: func(v identity.Identity) error {
			if _, err := tx.Exec(`INSERT INTO identities (id, handle, kind, display_name) VALUES (?, ?, ?, ?)`,
				v.ID, v.Handle, v.Kind, v.DisplayName); err != nil {
				return fail("identity", err)
			}
			return nil
		},
		issue: func(i issues.Issue) error {
			// assigned_at is internal FIFO state absent from the
			// contract (spec gap); imported assignees inherit the
			// issue's updated stamp as their queue position.
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
			for _, l := range i.Labels {
				if _, err := tx.Exec(`INSERT INTO issue_labels (issue, label) VALUES (?, ?)`, i.ID, l.ID); err != nil {
					return fail("issue label", err)
				}
			}
			return nil
		},
		comment: func(c comments.Comment) error {
			if _, err := tx.Exec(`INSERT INTO comments (id, issue, doc_version, review, review_revision, parent, anchor, author, body, created)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				c.ID, c.Issue, c.DocVersion, c.Review, c.ReviewRevision, c.Parent, c.Anchor, c.Author, c.Body, c.Created); err != nil {
				return fail("comment", err)
			}
			return nil
		},
		label: func(l issues.Label) error {
			if _, err := tx.Exec(`INSERT INTO labels (id, name, color) VALUES (?, ?, ?)`, l.ID, l.Name, l.Color); err != nil {
				return fail("label", err)
			}
			return nil
		},
		relation: func(rel issues.Relation) error {
			if _, err := tx.Exec(`INSERT INTO issue_relations (id, kind, from_issue, to_issue) VALUES (?, ?, ?, ?)`,
				rel.ID, rel.Kind, rel.From, rel.To); err != nil {
				return fail("relation", err)
			}
			return nil
		},
		document: func(d docs.Document) error {
			if _, err := tx.Exec(`INSERT INTO documents (id, project, issue, title, current_version) VALUES (?, ?, ?, ?, ?)`,
				d.ID, d.Project, d.Issue, d.Title, d.CurrentVersion); err != nil {
				return fail("document", err)
			}
			return nil
		},
		docVersion: func(v docs.Version) error {
			if _, err := tx.Exec(`INSERT INTO doc_versions (id, document, number, content, author, created) VALUES (?, ?, ?, ?, ?, ?)`,
				v.ID, v.Document, v.Number, v.Content, v.Author, v.Created); err != nil {
				return fail("doc version", err)
			}
			return nil
		},
		thread: func(th threads.Thread) error {
			if _, err := tx.Exec(`INSERT INTO threads (id, title, transcript, session, project, issue, imported_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
				th.ID, th.Title, string(th.Transcript), th.Session, th.Project, th.Issue, th.ImportedAt); err != nil {
				return fail("thread", err)
			}
			return nil
		},
		review: func(rv review.Review) error {
			if _, err := tx.Exec(`INSERT INTO reviews (id, issue, author, state, revision, session, summary, branch, commit_sha, doc_version, latest_verdict_event, consumed, consumed_revision, close_used, created)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				rv.ID, rv.Issue, rv.Author, rv.State, rv.Revision, rv.Session, rv.Summary, rv.Branch, rv.Commit, rv.DocVersion,
				rv.LatestVerdictEvent, rv.Consumed, rv.ConsumedRevision, rv.CloseUsed, rv.Created); err != nil {
				return fail("review", err)
			}
			return nil
		},
		submission: func(sub review.Submission) error {
			if _, err := tx.Exec(`INSERT INTO review_submissions (id, review, revision, branch, commit_sha, base_commit, doc_version, session, content, created)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				sub.ID, sub.Review, sub.Revision, sub.Branch, sub.Commit, sub.BaseCommit, sub.DocVersion, sub.Session, sub.Content, sub.Created); err != nil {
				return fail("submission", err)
			}
			return nil
		},
		event: func(e events.Event) error {
			// Payload bytes store VERBATIM — a decode/re-marshal would
			// round integers past 2^53 and break the round trip.
			var payload *string
			if len(e.Payload) > 0 {
				str := string(e.Payload)
				payload = &str
			}
			if _, err := tx.Exec(`INSERT INTO events (id, kind, subject, operation, actor, payload, created) VALUES (?, ?, ?, ?, ?, ?, ?)`,
				e.ID, e.Kind, e.Subject, e.Operation, e.Actor, payload, e.Created); err != nil {
				return fail("event", err)
			}
			return nil
		},
	})
	if apiErr != nil {
		return &apiError{status: http.StatusInternalServerError, code: "bad-request", message: apiErr.message}
	}
	maxNumber := int64(0)
	for _, i := range meta.Issues {
		if i.Number > maxNumber {
			maxNumber = i.Number
		}
	}
	if maxNumber > 0 {
		// Future creates continue the display sequence past the import.
		if _, err := tx.Exec(`INSERT INTO issue_numbers (project, next) VALUES (?, ?)`, meta.Project.ID, maxNumber); err != nil {
			return errorFrom(fmt.Errorf("import issue numbers: %w", err))
		}
	}
	return nil
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
	humanSet := map[string]bool{}
	for _, i := range p.Identities {
		identitySet[i.ID] = true
		if i.Kind == "human" {
			humanSet[i.ID] = true
		}
	}
	if actor == "" || !identitySet[actor] {
		// AC-import-actor: an actor outside the export's identities
		// would break the round-trip guarantee.
		return &apiError{status: http.StatusBadRequest, code: "bad-request",
			message: "actor must reference an identity contained in the export's identities"}
	}
	// Storage uniqueness invariants reject as malformed BEFORE any
	// insert — a constraint violation mid-write would surface as a 500.
	handles := map[string]bool{}
	for _, i := range p.Identities {
		if handles[i.Handle] {
			return malformedImport("identities repeat handle %q", i.Handle)
		}
		handles[i.Handle] = true
	}
	labelNames := map[string]bool{}
	for _, l := range p.Labels {
		if labelNames[l.Name] {
			return malformedImport("labels repeat name %q", l.Name)
		}
		labelNames[l.Name] = true
	}
	issueSet := map[string]bool{}
	numbers := map[int64]bool{}
	for _, i := range p.Issues {
		if i.Project != p.Project.ID {
			return malformedImport("issue %s belongs to another project", i.ID)
		}
		if i.Assignee != nil && !identitySet[*i.Assignee] {
			return malformedImport("issue %s assignee is not in identities", i.ID)
		}
		if numbers[i.Number] {
			return malformedImport("issues repeat display number %d", i.Number)
		}
		numbers[i.Number] = true
		issueSet[i.ID] = true
	}
	labelSet := map[string]bool{}
	for _, l := range p.Labels {
		labelSet[l.ID] = true
	}
	for _, i := range p.Issues {
		attached := map[string]bool{}
		for _, l := range i.Labels {
			if !labelSet[l.ID] {
				return malformedImport("issue %s carries label %s absent from labels", i.ID, l.ID)
			}
			if attached[l.ID] {
				return malformedImport("issue %s repeats label %s", i.ID, l.ID)
			}
			attached[l.ID] = true
		}
	}
	children := map[string][]string{}
	blocks := map[string][]string{}
	parentOf := map[string]bool{} // child -> has a parent (single-parent cardinality)
	seenEdges := map[string]bool{}
	for _, rel := range p.IssueRelations {
		if !issueSet[rel.From] || !issueSet[rel.To] {
			return malformedImport("relation %s references an issue outside the export", rel.ID)
		}
		if rel.From == rel.To {
			return malformedImport("relation %s relates an issue to itself", rel.ID)
		}
		edge := rel.Kind + ":" + rel.From + ">" + rel.To
		if seenEdges[edge] {
			return malformedImport("relation %s duplicates an existing edge", rel.ID)
		}
		seenEdges[edge] = true
		switch rel.Kind {
		case "parent_of":
			if parentOf[rel.To] {
				return malformedImport("issue %s has more than one parent", rel.To)
			}
			parentOf[rel.To] = true
			children[rel.From] = append(children[rel.From], rel.To)
		case "blocks":
			blocks[rel.From] = append(blocks[rel.From], rel.To)
		}
	}
	// Cycles in either graph are unreachable through the API and
	// reject the payload whole.
	for _, graph := range []map[string][]string{children, blocks} {
		if node, found := findCycle(graph); found {
			return malformedImport("relations form a cycle through issue %s", node)
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
		if len(d.Versions) == 0 {
			// A document is unreachable without its seeding version.
			return malformedImport("document %s carries no versions", d.Document.ID)
		}
		for i, v := range d.Versions {
			if v.Number != int64(i+1) {
				return malformedImport("document %s version numbers are not contiguous from 1", d.Document.ID)
			}
		}
		latest := d.Versions[len(d.Versions)-1]
		if d.Document.CurrentVersion == nil || *d.Document.CurrentVersion != latest.ID {
			// The default read serves current_version; anything but the
			// highest-numbered version makes metadata disagree with it.
			return malformedImport("document %s current_version must name its latest version", d.Document.ID)
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
		if !identitySet[p.Reviews[i].Author] {
			return malformedImport("review %s author is not in identities", p.Reviews[i].ID)
		}
		if apiErr := validateImportedReview(&p.Reviews[i], p, issueSet, humanSet, docVersionSet); apiErr != nil {
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
	// Reply threads never span anchors or review revisions.
	commentByID := map[string]*comments.Comment{}
	for i := range p.Comments {
		commentByID[p.Comments[i].ID] = &p.Comments[i]
	}
	reviewRevisions := map[string]int64{}
	for _, r := range p.Reviews {
		reviewRevisions[r.ID] = r.Revision
	}
	parentGraph := map[string][]string{}
	for _, c := range p.Comments {
		if c.Review != nil && c.ReviewRevision != nil {
			// The named revision must exist: submissions are validated
			// contiguous 1..revision, so the range check is existence.
			if *c.ReviewRevision < 1 || *c.ReviewRevision > reviewRevisions[*c.Review] {
				return malformedImport("comment %s names review revision %d, which does not exist", c.ID, *c.ReviewRevision)
			}
		}
		if c.Parent == nil {
			continue
		}
		if *c.Parent == c.ID {
			return malformedImport("comment %s replies to itself", c.ID)
		}
		parent, ok := commentByID[*c.Parent]
		if !ok {
			return malformedImport("comment %s replies to a parent outside the export", c.ID)
		}
		if !ptrEq(parent.Issue, c.Issue) || !ptrEq(parent.DocVersion, c.DocVersion) || !ptrEq(parent.Review, c.Review) || !ptrEqInt(parent.ReviewRevision, c.ReviewRevision) {
			return malformedImport("comment %s replies across anchors or review revisions", c.ID)
		}
		parentGraph[*c.Parent] = append(parentGraph[*c.Parent], c.ID)
	}
	if node, found := findCycle(parentGraph); found {
		return malformedImport("comment reply threads form a cycle through %s", node)
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
	reviewIssueByID := map[string]string{}
	for _, r := range p.Reviews {
		reviewIssueByID[r.ID] = r.Issue
	}
	for _, e := range p.Events {
		if !identitySet[e.Actor] {
			return malformedImport("event %s actor is not in identities", e.ID)
		}
		if e.Subject != p.Project.ID && !issueSet[e.Subject] {
			return malformedImport("event %s subject is outside the export", e.ID)
		}
		if e.Kind == "review.approved" || e.Kind == "review.changes-requested" {
			// EVERY verdict event is human-authored against a carried
			// review's issue — not only the latest one; imported audit
			// history must be reachable through the verdict API.
			if !humanSet[e.Actor] {
				return malformedImport("verdict event %s actor is not a human identity", e.ID)
			}
			var payload struct {
				Review string `json:"review"`
			}
			if json.Unmarshal(e.Payload, &payload) != nil || payload.Review == "" {
				return malformedImport("verdict event %s names no review", e.ID)
			}
			issue, ok := reviewIssueByID[payload.Review]
			if !ok {
				return malformedImport("verdict event %s names a review outside the export", e.ID)
			}
			if e.Subject != issue {
				return malformedImport("verdict event %s subject is not its review's issue", e.ID)
			}
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
	if p.Project.ArchivedAt != nil {
		if apiErr := timeOf("project archived_at", *p.Project.ArchivedAt); apiErr != nil {
			return apiErr
		}
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
		if r.Consumed != nil {
			if apiErr := timeOf("review consumed", *r.Consumed); apiErr != nil {
				return apiErr
			}
		}
		if r.CloseUsed != nil {
			if apiErr := timeOf("review close_used", *r.CloseUsed); apiErr != nil {
				return apiErr
			}
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
		if apiErr := uuidOf("event operation", e.Operation); apiErr != nil {
			return apiErr
		}
		if e.Kind == "" {
			return malformedImport("event %s has no kind", e.ID)
		}
		if apiErr := timeOf("event created", e.Created); apiErr != nil {
			return apiErr
		}
		if e.Kind == "issue.relation-removed" {
			// The contract's discriminated variant REQUIRES a relation
			// snapshot payload on every relation-removed event.
			if len(e.Payload) == 0 {
				return malformedImport("event %s must carry a RelationRemovedPayload", e.ID)
			}
			var payload struct {
				Relation string `json:"relation"`
				Kind     string `json:"kind"`
				From     string `json:"from"`
				To       string `json:"to"`
			}
			if json.Unmarshal(e.Payload, &payload) != nil || !isUUID(payload.Relation) ||
				!isUUID(payload.From) || !isUUID(payload.To) ||
				(payload.Kind != "parent_of" && payload.Kind != "blocks") {
				return malformedImport("event %s carries a malformed RelationRemovedPayload", e.ID)
			}
		}
	}
	return nil
}

func ptrEq(a, b *string) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	return a == nil || *a == *b
}

func ptrEqInt(a, b *int64) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	return a == nil || *a == *b
}

// findCycle detects a cycle in a relation graph via colored DFS,
// returning a node on the cycle.
func findCycle(graph map[string][]string) (string, bool) {
	const (
		visiting = 1
		done     = 2
	)
	state := map[string]int{}
	var walk func(string) (string, bool)
	walk = func(node string) (string, bool) {
		state[node] = visiting
		for _, next := range graph[node] {
			switch state[next] {
			case visiting:
				return next, true
			case done:
				continue
			default:
				if hit, found := walk(next); found {
					return hit, true
				}
			}
		}
		state[node] = done
		return "", false
	}
	for node := range graph {
		if state[node] == 0 {
			if hit, found := walk(node); found {
				return hit, true
			}
		}
	}
	return "", false
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
func validateImportedReview(r *review.Review, p *importPayload, issueSet, humanSet, docVersionSet map[string]bool) *apiError {
	if !issueSet[r.Issue] {
		return malformedImport("review %s names an issue outside the export", r.ID)
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
		if code {
			if sub.Branch == nil || sub.Commit == nil || sub.BaseCommit == nil {
				return malformedImport("review %s code submission %d must carry branch, commit, and base_commit together", r.ID, sub.Revision)
			}
			if !isCommitSHA(*sub.Commit) || !isCommitSHA(*sub.BaseCommit) {
				return malformedImport("review %s code submission %d carries a malformed object id", r.ID, sub.Revision)
			}
			if sub.Content == nil {
				// ReviewSubmissionExport: the durable copy is the only
				// one. Present-but-empty is valid — a commit identical
				// to its base renders an empty diff.
				return malformedImport("review %s code submission %d omits its content", r.ID, sub.Revision)
			}
			if sub.DocVersion != nil {
				return malformedImport("review %s submission %d mixes deliverable shapes", r.ID, sub.Revision)
			}
		} else {
			if sub.DocVersion == nil || !docVersionSet[*sub.DocVersion] {
				return malformedImport("review %s submission %d names no resolvable deliverable", r.ID, sub.Revision)
			}
			if sub.Content != nil {
				// The immutable referenced version IS the deliverable;
				// stored content could silently diverge from what the
				// verdict covered. The live system never stores it.
				return malformedImport("review %s doc submission %d must not carry stored content", r.ID, sub.Revision)
			}
		}
	}
	latest := r.Submissions[len(r.Submissions)-1]
	if !deliverableMatches(r, latest) {
		return malformedImport("review %s top-level deliverable disagrees with its latest submission", r.ID)
	}
	if !ptrEq(r.Session, latest.Session) {
		// The live system mirrors the latest submission's session on
		// the review; a divergent import would misroute rework.
		return malformedImport("review %s session disagrees with its latest submission", r.ID)
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
		actual, found := latestVerdictEventFor(r.ID, p.Events)
		if !found || actual.ID != *r.LatestVerdictEvent {
			return malformedImport("review %s latest_verdict_event is not its actual latest verdict event", r.ID)
		}
		if actual.Kind != "review."+r.State {
			return malformedImport("review %s state %s disagrees with its latest verdict event's kind %s", r.ID, r.State, actual.Kind)
		}
		if actual.Subject != r.Issue {
			return malformedImport("review %s verdict event's subject is not its issue", r.ID)
		}
		// Verdicts are human-only (AC-review-verdict); a fabricated
		// agent-authored approval cannot arrive through import either.
		if humanSet != nil && !humanSet[actual.Actor] {
			return malformedImport("review %s verdict event's actor is not a human identity", r.ID)
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
func latestVerdictEventFor(reviewID string, list []events.Event) (events.Event, bool) {
	var last events.Event
	found := false
	for _, e := range list {
		if e.Kind != "review.approved" && e.Kind != "review.changes-requested" {
			continue
		}
		var payload struct {
			Review string `json:"review"`
		}
		if json.Unmarshal(e.Payload, &payload) == nil && payload.Review == reviewID {
			last, found = e, true
		}
	}
	return last, found
}

// checkImportCollisions rejects the payload whole if ANY of its UUIDs
// already exists ANYWHERE on this server (AC-import-collision) —
// "any pre-existing UUID is a collision", regardless of record type —
// naming every conflict. Payload-internal duplicates are equally
// malformed: no two records may share an id.
func (s *server) checkImportCollisions(tx *sql.Tx, p *importPayload) *apiError {
	all := []string{p.Project.ID}
	for _, i := range p.Identities {
		all = append(all, i.ID)
	}
	for _, i := range p.Issues {
		all = append(all, i.ID)
	}
	for _, l := range p.Labels {
		all = append(all, l.ID)
	}
	for _, r := range p.IssueRelations {
		all = append(all, r.ID)
	}
	for _, d := range p.Documents {
		all = append(all, d.Document.ID)
		for _, v := range d.Versions {
			all = append(all, v.ID)
		}
	}
	for _, t := range p.Threads {
		all = append(all, t.ID)
	}
	for _, r := range p.Reviews {
		all = append(all, r.ID)
		for _, sub := range r.Submissions {
			all = append(all, sub.ID)
		}
	}
	for _, c := range p.Comments {
		all = append(all, c.ID)
	}
	for _, e := range p.Events {
		all = append(all, e.ID)
	}
	seen := map[string]bool{}
	for _, id := range all {
		if seen[id] {
			return malformedImport("payload reuses id %s across records", id)
		}
		seen[id] = true
	}
	conflictSet := map[string]bool{}
	tables := []string{"projects", "identities", "issues", "labels", "issue_relations",
		"documents", "doc_versions", "doc_templates", "threads", "reviews", "review_submissions", "comments", "events"}
	for _, table := range tables {
		for _, chunk := range chunkIDs(all, 500) {
			query := `SELECT id FROM ` + table + ` WHERE id IN (?` + strings.Repeat(",?", len(chunk)-1) + `)`
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
				conflictSet[id] = true
			}
			if err := rows.Err(); err != nil {
				_ = rows.Close()
				return errorFrom(err)
			}
			_ = rows.Close()
		}
	}
	if len(conflictSet) > 0 {
		return &apiError{status: http.StatusConflict, code: "uuid-collision",
			message: fmt.Sprintf("%d record ids already exist on this server", len(conflictSet)), conflicts: sortedKeys(conflictSet)}
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
