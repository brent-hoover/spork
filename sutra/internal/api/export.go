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

// projectExport mirrors the contract's ProjectExport. Threads carry
// verbatim transcripts, so the envelope is spliced by hand around
// their raw bytes at serving time.
type projectExport struct {
	Project        projects.Project    `json:"project"`
	Identities     []identity.Identity `json:"identities"`
	Issues         []issues.Issue      `json:"issues"`
	Comments       []comments.Comment  `json:"comments"`
	Labels         []issues.Label      `json:"labels"`
	IssueRelations []issues.Relation   `json:"issue_relations"`
	Documents      []documentExport    `json:"documents"`
	Reviews        []review.Review     `json:"reviews"`
	Events         []events.Event      `json:"events"`
}

type documentExport struct {
	Document docs.Document  `json:"document"`
	Versions []docs.Version `json:"versions"`
}

// assembleExport builds the full export in one read transaction
// (AC-export-full). Cross-project blocks relations cannot round-trip
// through a single-project export and are excluded — logged as a spec
// gap in feature-work/sutra-build/spec-gaps.md.
func assembleExport(tx *sql.Tx, projectID string) (projectExport, []threads.Thread, error) {
	var ex projectExport
	var err error
	if ex.Project, err = projects.GetTx(tx, projectID); err != nil {
		return ex, nil, err
	}
	if ex.Issues, err = issues.List(tx, projectID, issues.Filters{}); err != nil {
		return ex, nil, err
	}
	issueIDs := map[string]bool{}
	labelSet := map[string]issues.Label{}
	identityIDs := map[string]bool{}
	for i := range ex.Issues {
		issue := &ex.Issues[i]
		issueIDs[issue.ID] = true
		labels, err := issues.LabelsOf(tx, issue.ID)
		if err != nil {
			return ex, nil, err
		}
		if len(labels) > 0 {
			issue.Labels = labels
		}
		for _, l := range labels {
			labelSet[l.ID] = l
		}
		if issue.Assignee != nil {
			identityIDs[*issue.Assignee] = true
		}
	}
	ex.Labels = make([]issues.Label, 0, len(labelSet))
	for _, l := range labelSet {
		ex.Labels = append(ex.Labels, l)
	}
	slices.SortFunc(ex.Labels, func(a, b issues.Label) int { return strings.Compare(a.Name, b.Name) })

	// Relations among this project's issues; blocks with a foreign end
	// are excluded (see the spec-gap note above).
	ex.IssueRelations = []issues.Relation{}
	rows, err := tx.Query(`SELECT id, kind, from_issue, to_issue FROM issue_relations`)
	if err != nil {
		return ex, nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var r issues.Relation
		if err := rows.Scan(&r.ID, &r.Kind, &r.From, &r.To); err != nil {
			return ex, nil, err
		}
		if issueIDs[r.From] && issueIDs[r.To] {
			ex.IssueRelations = append(ex.IssueRelations, r)
		}
	}
	if err := rows.Err(); err != nil {
		return ex, nil, err
	}

	// Documents with all versions.
	docList, err := docs.ListByProject(tx, projectID)
	if err != nil {
		return ex, nil, err
	}
	ex.Documents = make([]documentExport, 0, len(docList))
	docVersionIDs := map[string]bool{}
	for _, d := range docList {
		versions, err := docs.ListVersions(tx, d.ID)
		if err != nil {
			return ex, nil, err
		}
		for _, v := range versions {
			docVersionIDs[v.ID] = true
			identityIDs[v.Author] = true
		}
		ex.Documents = append(ex.Documents, documentExport{Document: d, Versions: versions})
	}

	// Reviews of this project's issues, submissions complete WITH
	// content (ReviewSubmissionExport: the durable deliverable copy).
	ex.Reviews = []review.Review{}
	reviewIDs := map[string]bool{}
	for _, id := range sortedKeys(issueIDs) {
		list, err := review.List(tx, id, "", "")
		if err != nil {
			return ex, nil, err
		}
		for _, r := range list {
			full, err := exportSubmissions(tx, r)
			if err != nil {
				return ex, nil, err
			}
			reviewIDs[full.ID] = true
			identityIDs[full.Author] = true
			ex.Reviews = append(ex.Reviews, full)
		}
	}

	// Comments anchored to any exported record.
	ex.Comments = []comments.Comment{}
	anchors := []struct {
		column string
		ids    map[string]bool
	}{{"issue", issueIDs}, {"doc_version", docVersionIDs}, {"review", reviewIDs}}
	for _, a := range anchors {
		for _, id := range sortedKeys(a.ids) {
			list, err := comments.ListByAnchor(tx, a.column, id)
			if err != nil {
				return ex, nil, err
			}
			for _, c := range list {
				identityIDs[c.Author] = true
			}
			ex.Comments = append(ex.Comments, list...)
		}
	}

	// Threads anchored to the project or its issues.
	threadList := []threads.Thread{}
	err = threads.SearchEach(tx, nil, nil, &projectID, func(t threads.Thread) error {
		threadList = append(threadList, t)
		return nil
	})
	if err != nil {
		return ex, nil, err
	}
	for _, id := range sortedKeys(issueIDs) {
		if err := threads.ListByIssueEach(tx, id, func(t threads.Thread) error {
			threadList = append(threadList, t)
			return nil
		}); err != nil {
			return ex, nil, err
		}
	}

	// Events whose subject is the project or one of its issues.
	subjects := map[string]bool{projectID: true}
	for id := range issueIDs {
		subjects[id] = true
	}
	ex.Events = []events.Event{}
	for _, subject := range sortedKeys(subjects) {
		list, err := events.BySubject(tx, subject)
		if err != nil {
			return ex, nil, err
		}
		for _, e := range list {
			identityIDs[e.Actor] = true
		}
		ex.Events = append(ex.Events, list...)
	}
	events.SortByFeedOrder(ex.Events)

	// Every referenced identity travels with the export.
	ex.Identities = []identity.Identity{}
	for _, id := range sortedKeys(identityIDs) {
		ident, err := identity.Get(tx, id)
		if err != nil {
			return ex, nil, err
		}
		ex.Identities = append(ex.Identities, ident)
	}
	return ex, threadList, nil
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

// exportSubmissions loads a review's full submission history including
// stored content, ordered by revision.
func exportSubmissions(tx *sql.Tx, r review.Review) (review.Review, error) {
	rows, err := tx.Query(`
		SELECT id, review, revision, branch, commit_sha, base_commit, doc_version, session, content, created
		FROM review_submissions WHERE review = ? ORDER BY revision`, r.ID)
	if err != nil {
		return r, err
	}
	defer func() { _ = rows.Close() }()
	r.Submissions = []review.Submission{}
	for rows.Next() {
		var s review.Submission
		if err := rows.Scan(&s.ID, &s.Review, &s.Revision, &s.Branch, &s.Commit,
			&s.BaseCommit, &s.DocVersion, &s.Session, &s.Content, &s.Created); err != nil {
			return r, err
		}
		r.Submissions = append(r.Submissions, s)
	}
	return r, rows.Err()
}

func (s *server) exportProject(w http.ResponseWriter, r *http.Request) {
	tx, err := s.db.Begin()
	if err != nil {
		writeError(w, errorFrom(err))
		return
	}
	defer func() { _ = tx.Rollback() }()
	ex, threadList, err := assembleExport(tx, r.PathValue("projectId"))
	if err != nil {
		writeError(w, errorFrom(err))
		return
	}
	raw, err := json.Marshal(ex)
	if err != nil {
		writeError(w, errorFrom(err))
		return
	}
	// Threads splice in verbatim; the marshaled envelope closes after
	// them.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw[:len(raw)-1])
	_, _ = w.Write([]byte(`,"threads":[`))
	for i, t := range threadList {
		one, apiErr := threadJSON(t)
		if apiErr != nil {
			return // committed status; truncation is the only signal
		}
		if i > 0 {
			_, _ = w.Write([]byte{','})
		}
		_, _ = w.Write(one)
	}
	_, _ = w.Write([]byte(`]}`))
}
