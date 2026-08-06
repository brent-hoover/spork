package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"sutra/internal/events"
	"sutra/internal/issues"
	"sutra/internal/projects"
	"sutra/internal/review"
)

// issueRead is the contract's IssueRead: Issue plus the REQUIRED
// feed_watermark captured atomically with the read.
type issueRead struct {
	issues.Issue
	FeedWatermark string `json:"feed_watermark"`
}

// readIssue loads an issue and captures its watermark in one
// transaction (AC-feed-watermark).
func (s *server) readIssue(id string) (issueRead, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return issueRead{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	issue, err := issues.Get(tx, id)
	if err != nil {
		return issueRead{}, err
	}
	watermark, err := events.Watermark(tx)
	if err != nil {
		return issueRead{}, err
	}
	return issueRead{Issue: issue, FeedWatermark: watermark}, nil
}

func issueErrorFrom(err error) *apiError {
	switch e := err.(type) {
	case *issues.NotFoundError, *issues.RelationNotFoundError:
		return &apiError{status: http.StatusNotFound, code: "not-found", message: err.Error()}
	case *issues.RelationExistsError:
		return &apiError{status: http.StatusConflict, code: "relation-exists", message: err.Error(), conflicts: []string{e.Existing}}
	case *issues.CycleError:
		code := "ancestry-cycle"
		if e.Kind == "blocks" {
			code = "blocking-cycle"
		}
		return &apiError{status: http.StatusConflict, code: code, message: err.Error()}
	case *issues.HasParentError:
		return &apiError{status: http.StatusConflict, code: "relation-exists", message: err.Error(), conflicts: []string{e.Parent}}
	}
	return errorFrom(err)
}

// guardWritable rejects mutations into archived projects
// (AC-project-archive: read-only).
func guardWritable(tx *sql.Tx, project string) *apiError {
	archived, err := projects.IsArchived(tx, project)
	if err != nil {
		return errorFrom(err)
	}
	if archived {
		return &apiError{status: http.StatusConflict, code: "project-archived", message: fmt.Sprintf("project %s is archived and read-only", project)}
	}
	return nil
}

func (s *server) createIssue(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("projectId")
	s.idempotent(w, r, func(tx *sql.Tx) (int, any, *apiError) {
		var req struct {
			Title    string  `json:"title"`
			Body     *string `json:"body"`
			Assignee *string `json:"assignee"`
			Actor    string  `json:"actor"`
		}
		if apiErr := decodeBody(r, &req); apiErr != nil {
			return 0, nil, apiErr
		}
		if req.Title == "" {
			return 0, nil, &apiError{status: http.StatusBadRequest, code: "bad-request", message: "title is required"}
		}
		if apiErr := requireActor(tx, req.Actor); apiErr != nil {
			return 0, nil, apiErr
		}
		if apiErr := guardWritable(tx, project); apiErr != nil {
			return 0, nil, apiErr
		}
		if req.Assignee != nil {
			if apiErr := requireActor(tx, *req.Assignee); apiErr != nil {
				return 0, nil, apiErr
			}
		}
		issue, err := issues.Create(tx, project, req.Title, req.Body, req.Assignee)
		if err != nil {
			return 0, nil, issueErrorFrom(err)
		}
		if _, err := events.Emit(tx, "issue.created", issue.ID, events.NewOperation(), req.Actor, nil); err != nil {
			return 0, nil, errorFrom(err)
		}
		watermark, err := events.Watermark(tx)
		if err != nil {
			return 0, nil, errorFrom(err)
		}
		return http.StatusCreated, issueRead{Issue: issue, FeedWatermark: watermark}, nil
	})
}

func (s *server) getIssue(w http.ResponseWriter, r *http.Request) {
	read, err := s.readIssue(r.PathValue("issueId"))
	if err != nil {
		writeError(w, issueErrorFrom(err))
		return
	}
	writeJSON(w, http.StatusOK, read)
}

func (s *server) listIssues(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	// label filtering and ranked text search over comments belong to
	// the labels and search modules; until they exist these parameters
	// reject explicitly rather than return silently wrong results.
	if q.Get("q") != "" || len(q["label"]) > 0 {
		writeError(w, &apiError{status: http.StatusBadRequest, code: "bad-request", message: "q and label filters are not implemented yet"})
		return
	}
	f := issues.Filters{
		Statuses: q["status"],
		Assignee: q.Get("assignee"),
	}
	if raw := q.Get("number"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			writeError(w, &apiError{status: http.StatusBadRequest, code: "bad-request", message: fmt.Sprintf("number %q is not an integer", raw)})
			return
		}
		f.Number = &n
	}
	tx, err := s.db.Begin()
	if err != nil {
		writeError(w, errorFrom(err))
		return
	}
	defer func() { _ = tx.Rollback() }()
	list, err := issues.List(tx, r.PathValue("projectId"), f)
	if err != nil {
		writeError(w, issueErrorFrom(err))
		return
	}
	watermark, err := events.Watermark(tx)
	if err != nil {
		writeError(w, errorFrom(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"issues": list, "feed_watermark": watermark})
}

func (s *server) updateIssue(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("issueId")
	s.idempotent(w, r, func(tx *sql.Tx) (int, any, *apiError) {
		var req struct {
			Title *string `json:"title"`
			Body  *string `json:"body"`
			Actor string  `json:"actor"`
		}
		if apiErr := decodeBody(r, &req); apiErr != nil {
			return 0, nil, apiErr
		}
		if apiErr := requireActor(tx, req.Actor); apiErr != nil {
			return 0, nil, apiErr
		}
		current, err := issues.Get(tx, id)
		if err != nil {
			return 0, nil, issueErrorFrom(err)
		}
		if apiErr := guardWritable(tx, current.Project); apiErr != nil {
			return 0, nil, apiErr
		}
		updated, err := issues.Update(tx, id, req.Title, req.Body)
		if err != nil {
			return 0, nil, issueErrorFrom(err)
		}
		if _, err := events.Emit(tx, "issue.updated", id, events.NewOperation(), req.Actor, nil); err != nil {
			return 0, nil, errorFrom(err)
		}
		watermark, err := events.Watermark(tx)
		if err != nil {
			return 0, nil, errorFrom(err)
		}
		return http.StatusOK, issueRead{Issue: updated, FeedWatermark: watermark}, nil
	})
}

// transitionRequest is the strictly-decoded status-transition body.
type transitionRequest struct {
	Status                  string
	ExpectedStatus          *string
	ExpectedSubtreeRevision *int64
	Review                  string
	ReviewRevision          *int64
	ReviewVerdictEvent      string
	Actor                   string
}

// decodeTransition enforces the contract's CLOSED oneOf schemas
// (additionalProperties: false on both variants): unknown fields
// reject, and each variant's required set is validated before any state
// is touched — an incomplete complete-transition is schema-invalid 400,
// never 409.
func decodeTransition(r *http.Request) (transitionRequest, *apiError) {
	var raw map[string]json.RawMessage
	if apiErr := decodeBody(r, &raw); apiErr != nil {
		return transitionRequest{}, apiErr
	}
	var req transitionRequest
	get := func(field string, into any) *apiError {
		v, ok := raw[field]
		if !ok {
			return nil
		}
		// No transition field is nullable in the contract: an explicit
		// null is not an absent field — it would silently disarm the
		// optional fences (expected_status, expected_subtree_revision).
		if string(v) == "null" {
			return &apiError{status: http.StatusBadRequest, code: "bad-request", message: fmt.Sprintf("%s must not be null", field)}
		}
		if err := json.Unmarshal(v, into); err != nil {
			return &apiError{status: http.StatusBadRequest, code: "bad-request", message: fmt.Sprintf("malformed %s: %v", field, err)}
		}
		return nil
	}
	if apiErr := get("status", &req.Status); apiErr != nil {
		return transitionRequest{}, apiErr
	}

	var allowed map[string]bool
	var required []string
	switch req.Status {
	case issues.StatusComplete:
		allowed = map[string]bool{"status": true, "expected_status": true, "expected_subtree_revision": true,
			"review": true, "review_revision": true, "review_verdict_event": true, "actor": true}
		required = []string{"status", "review", "review_revision", "review_verdict_event", "actor"}
	case issues.StatusOpen, issues.StatusInProgress, issues.StatusBlocked, issues.StatusDeferred:
		allowed = map[string]bool{"status": true, "expected_status": true, "actor": true}
		required = []string{"status", "actor"}
	default:
		return transitionRequest{}, &apiError{status: http.StatusBadRequest, code: "bad-request", message: fmt.Sprintf("unknown status %q", req.Status)}
	}
	for field := range raw {
		if !allowed[field] {
			return transitionRequest{}, &apiError{status: http.StatusBadRequest, code: "bad-request", message: fmt.Sprintf("unknown field %q for %s transition", field, req.Status)}
		}
	}
	for _, field := range required {
		if _, ok := raw[field]; !ok {
			return transitionRequest{}, &apiError{status: http.StatusBadRequest, code: "bad-request", message: fmt.Sprintf("%s is required for %s transitions", field, req.Status)}
		}
	}
	for field, into := range map[string]any{
		"expected_status":           &req.ExpectedStatus,
		"expected_subtree_revision": &req.ExpectedSubtreeRevision,
		"review":                    &req.Review,
		"review_revision":           &req.ReviewRevision,
		"review_verdict_event":      &req.ReviewVerdictEvent,
		"actor":                     &req.Actor,
	} {
		if apiErr := get(field, into); apiErr != nil {
			return transitionRequest{}, apiErr
		}
	}
	// Value constraints are schema too: enum membership, non-null
	// required scalars, and uuid formats reject as 400 before any gate.
	if req.ExpectedStatus != nil {
		switch *req.ExpectedStatus {
		case issues.StatusOpen, issues.StatusInProgress, issues.StatusBlocked, issues.StatusDeferred, issues.StatusComplete:
		default:
			return transitionRequest{}, &apiError{status: http.StatusBadRequest, code: "bad-request", message: fmt.Sprintf("expected_status %q is not a valid status", *req.ExpectedStatus)}
		}
	}
	if !isUUID(req.Actor) {
		return transitionRequest{}, &apiError{status: http.StatusBadRequest, code: "bad-request", message: "actor must be an identity uuid"}
	}
	if req.Status == issues.StatusComplete {
		if !isUUID(req.Review) {
			return transitionRequest{}, &apiError{status: http.StatusBadRequest, code: "bad-request", message: "review must be a uuid"}
		}
		if !isUUID(req.ReviewVerdictEvent) {
			return transitionRequest{}, &apiError{status: http.StatusBadRequest, code: "bad-request", message: "review_verdict_event must be a uuid"}
		}
		if req.ReviewRevision == nil {
			return transitionRequest{}, &apiError{status: http.StatusBadRequest, code: "bad-request", message: "review_revision must be an integer"}
		}
	}
	return req, nil
}

// isUUID accepts the canonical 8-4-4-4-12 hex form.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}

// updateIssueStatus is the review-gated, cascade-carrying transition
// (AC-status-set, AC-parent-close-gate, AC-parent-reopen-cascade,
// AC-subtree-revision, AC-status-conditional). Complete transitions
// name a review; until the review module lands no review id can belong
// to any issue, so every complete attempt is truthfully the contract's
// 409.
func (s *server) updateIssueStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("issueId")
	s.idempotent(w, r, func(tx *sql.Tx) (int, any, *apiError) {
		req, apiErr := decodeTransition(r)
		if apiErr != nil {
			return 0, nil, apiErr
		}
		if apiErr := requireActor(tx, req.Actor); apiErr != nil {
			return 0, nil, apiErr
		}
		current, err := issues.Get(tx, id)
		if err != nil {
			return 0, nil, issueErrorFrom(err)
		}
		if apiErr := guardWritable(tx, current.Project); apiErr != nil {
			return 0, nil, apiErr
		}
		if req.ExpectedStatus != nil && *req.ExpectedStatus != current.Status {
			return 0, nil, &apiError{status: http.StatusConflict, code: "expected-status-mismatch",
				message: fmt.Sprintf("expected status %q, current is %q", *req.ExpectedStatus, current.Status)}
		}
		// The history-aware fence: a mismatch conflicts even when
		// current state matches, so a descendant that reopened and
		// recompleted since the caller's observation still rejects
		// (AC-subtree-revision).
		if req.ExpectedSubtreeRevision != nil && *req.ExpectedSubtreeRevision != current.SubtreeRevision {
			return 0, nil, &apiError{status: http.StatusConflict, code: "expected-subtree-revision-mismatch",
				message: fmt.Sprintf("expected subtree_revision %d, current is %d", *req.ExpectedSubtreeRevision, current.SubtreeRevision)}
		}
		if req.Status == issues.StatusComplete {
			return s.closeIssue(tx, id, current, req)
		}

		final, apiErr := applyTransition(tx, id, req.Status, req.Actor)
		if apiErr != nil {
			return 0, nil, apiErr
		}
		watermark, err := events.Watermark(tx)
		if err != nil {
			return 0, nil, errorFrom(err)
		}
		return http.StatusOK, issueRead{Issue: final, FeedWatermark: watermark}, nil
	})
}

// applyTransition performs a non-complete status change with its full
// consequence set — status event, complete-ancestor reopen cascade
// under one operation id, and the once-per-transaction subtree bump —
// shared by the transition endpoint and the work-stack pop.
func applyTransition(tx *sql.Tx, id, status, actor string) (issues.Issue, *apiError) {
	operation := events.NewOperation()
	if err := issues.SetStatus(tx, id, status); err != nil {
		return issues.Issue{}, issueErrorFrom(err)
	}
	if _, err := events.Emit(tx, "issue.status-changed", id, operation, actor, nil); err != nil {
		return issues.Issue{}, errorFrom(err)
	}
	// An issue entering an active status reopens every complete
	// ancestor atomically, one status event each, same operation.
	if issues.Active(status) {
		reopened, err := issues.CompleteAncestors(tx, id)
		if err != nil {
			return issues.Issue{}, errorFrom(err)
		}
		for _, ancestor := range reopened {
			if err := issues.SetStatus(tx, ancestor, issues.StatusOpen); err != nil {
				return issues.Issue{}, issueErrorFrom(err)
			}
			if _, err := events.Emit(tx, "issue.status-changed", ancestor, operation, actor, nil); err != nil {
				return issues.Issue{}, errorFrom(err)
			}
		}
	}
	ancestors, err := issues.Ancestors(tx, id)
	if err != nil {
		return issues.Issue{}, errorFrom(err)
	}
	if err := issues.BumpSubtree(tx, append([]string{id}, ancestors...)); err != nil {
		return issues.Issue{}, errorFrom(err)
	}
	final, err := issues.Get(tx, id)
	if err != nil {
		return issues.Issue{}, issueErrorFrom(err)
	}
	return final, nil
}

// assignIssue sets or clears the assignee. assignee is REQUIRED but
// nullable: absent is invalid, explicit null clears (AC-issue-assign).
func (s *server) assignIssue(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("issueId")
	s.idempotent(w, r, func(tx *sql.Tx) (int, any, *apiError) {
		var raw map[string]json.RawMessage
		if apiErr := decodeBody(r, &raw); apiErr != nil {
			return 0, nil, apiErr
		}
		assigneeRaw, present := raw["assignee"]
		if !present {
			return 0, nil, &apiError{status: http.StatusBadRequest, code: "bad-request", message: "assignee is required; send null to clear"}
		}
		var req struct {
			Assignee *string `json:"assignee"`
			Actor    string  `json:"actor"`
		}
		reencoded, err := json.Marshal(raw)
		if err == nil {
			err = json.Unmarshal(reencoded, &req)
		}
		if err != nil {
			return 0, nil, &apiError{status: http.StatusBadRequest, code: "bad-request", message: fmt.Sprintf("malformed request body: %v", err)}
		}
		if string(assigneeRaw) != "null" && (req.Assignee == nil || !isUUID(*req.Assignee)) {
			return 0, nil, &apiError{status: http.StatusBadRequest, code: "bad-request", message: "assignee must be an identity uuid or null"}
		}
		if apiErr := requireActor(tx, req.Actor); apiErr != nil {
			return 0, nil, apiErr
		}
		current, err := issues.Get(tx, id)
		if err != nil {
			return 0, nil, issueErrorFrom(err)
		}
		if apiErr := guardWritable(tx, current.Project); apiErr != nil {
			return 0, nil, apiErr
		}
		if req.Assignee != nil {
			if apiErr := requireActor(tx, *req.Assignee); apiErr != nil {
				return 0, nil, apiErr
			}
		}
		updated, err := issues.Assign(tx, id, req.Assignee)
		if err != nil {
			return 0, nil, issueErrorFrom(err)
		}
		kind := "issue.assigned"
		if req.Assignee == nil {
			kind = "issue.unassigned"
		}
		if _, err := events.Emit(tx, kind, id, events.NewOperation(), req.Actor, nil); err != nil {
			return 0, nil, errorFrom(err)
		}
		watermark, err := events.Watermark(tx)
		if err != nil {
			return 0, nil, errorFrom(err)
		}
		return http.StatusOK, issueRead{Issue: updated, FeedWatermark: watermark}, nil
	})
}

// popWorkStack atomically claims the next workable issue for an
// identity: oldest-assigned, walked to the deepest open self-assigned
// blocker, skipping external blocks and archived projects; the claim
// is a full in-progress transition with the popping identity as actor.
// An empty stack is an explicit result, never an error (AC-pop-empty).
func (s *server) popWorkStack(w http.ResponseWriter, r *http.Request) {
	identityID := r.PathValue("identityId")
	s.idempotent(w, r, func(tx *sql.Tx) (int, any, *apiError) {
		if apiErr := requireActor(tx, identityID); apiErr != nil {
			if apiErr.status == http.StatusBadRequest {
				return 0, nil, &apiError{status: http.StatusNotFound, code: "not-found", message: fmt.Sprintf("identity %s not found", identityID)}
			}
			return 0, nil, apiErr
		}
		candidate, err := issues.PopCandidate(tx, identityID)
		if err != nil {
			return 0, nil, errorFrom(err)
		}
		if candidate == nil {
			watermark, err := events.Watermark(tx)
			if err != nil {
				return 0, nil, errorFrom(err)
			}
			return http.StatusOK, map[string]any{"feed_watermark": watermark}, nil
		}
		claimed, apiErr := applyTransition(tx, candidate.ID, issues.StatusInProgress, identityID)
		if apiErr != nil {
			return 0, nil, apiErr
		}
		watermark, err := events.Watermark(tx)
		if err != nil {
			return 0, nil, errorFrom(err)
		}
		return http.StatusOK, map[string]any{"feed_watermark": watermark, "issue": claimed}, nil
	})
}

// closeIssue is the review-gated complete transition
// (AC-close-approved, AC-parent-close-gate, AC-close-blocked-without):
// the DESCENDANT-AWARE gate rejects while any descendant at any depth —
// including beneath deferred children — is open, in-progress, or
// blocked; then the named review is spent atomically (ownership,
// approved at exactly the named revision, latest verdict event
// matching, never close-used), stamping close_used and freezing the
// verdict in the same transaction as the status change.
func (s *server) closeIssue(tx *sql.Tx, id string, current issues.Issue, req transitionRequest) (int, any, *apiError) {
	activeBelow, err := issues.ActiveDescendants(tx, id)
	if err != nil {
		return 0, nil, errorFrom(err)
	}
	if len(activeBelow) > 0 {
		return 0, nil, &apiError{status: http.StatusConflict, code: "open-children",
			message:   fmt.Sprintf("issue %s has active descendants %v; close them first", id, activeBelow),
			conflicts: activeBelow}
	}
	if _, err := review.SpendForClose(tx, req.Review, id, *req.ReviewRevision, req.ReviewVerdictEvent); err != nil {
		return 0, nil, reviewErrorFrom(err)
	}
	operation := events.NewOperation()
	if err := issues.SetStatus(tx, id, issues.StatusComplete); err != nil {
		return 0, nil, issueErrorFrom(err)
	}
	if _, err := events.Emit(tx, "issue.status-changed", id, operation, req.Actor, nil); err != nil {
		return 0, nil, errorFrom(err)
	}
	ancestors, err := issues.Ancestors(tx, id)
	if err != nil {
		return 0, nil, errorFrom(err)
	}
	if err := issues.BumpSubtree(tx, append([]string{id}, ancestors...)); err != nil {
		return 0, nil, errorFrom(err)
	}
	final, err := issues.Get(tx, id)
	if err != nil {
		return 0, nil, issueErrorFrom(err)
	}
	watermark, err := events.Watermark(tx)
	if err != nil {
		return 0, nil, errorFrom(err)
	}
	return http.StatusOK, issueRead{Issue: final, FeedWatermark: watermark}, nil
}

func (s *server) addIssueRelation(w http.ResponseWriter, r *http.Request) {
	from := r.PathValue("issueId")
	s.idempotent(w, r, func(tx *sql.Tx) (int, any, *apiError) {
		var req struct {
			Kind  string `json:"kind"`
			To    string `json:"to"`
			Actor string `json:"actor"`
		}
		if apiErr := decodeBody(r, &req); apiErr != nil {
			return 0, nil, apiErr
		}
		if req.Kind != "parent_of" && req.Kind != "blocks" {
			return 0, nil, &apiError{status: http.StatusBadRequest, code: "bad-request", message: fmt.Sprintf("kind %q must be parent_of or blocks", req.Kind)}
		}
		if apiErr := requireActor(tx, req.Actor); apiErr != nil {
			return 0, nil, apiErr
		}
		fromIssue, err := issues.Get(tx, from)
		if err != nil {
			return 0, nil, issueErrorFrom(err)
		}
		if apiErr := guardWritable(tx, fromIssue.Project); apiErr != nil {
			return 0, nil, apiErr
		}
		toIssue, err := issues.Get(tx, req.To)
		if err != nil {
			return 0, nil, issueErrorFrom(err)
		}
		if toIssue.Project != fromIssue.Project {
			// parent_of stays within ONE project: the reopen cascade
			// and subtree_revision walk the ancestor chain, and a
			// cross-project chain would let a live descendant mutate an
			// archived ancestor project past its write guard
			// (AC-project-scoping: content belongs to exactly one
			// project). blocks relations may cross, with both sides
			// guarded — they never cascade.
			if req.Kind == "parent_of" {
				return 0, nil, &apiError{status: http.StatusBadRequest, code: "bad-request",
					message: "parent_of relations must stay within one project"}
			}
			if apiErr := guardWritable(tx, toIssue.Project); apiErr != nil {
				return 0, nil, apiErr
			}
		}
		rel, err := issues.AddRelation(tx, req.Kind, from, req.To)
		if err != nil {
			return 0, nil, issueErrorFrom(err)
		}
		operation := events.NewOperation()
		if _, err := events.Emit(tx, "issue.relation-added", from, operation, req.Actor, nil); err != nil {
			return 0, nil, errorFrom(err)
		}
		if rel.Kind == "parent_of" {
			// Attachment bumps the new parent and its ancestors; active
			// work beneath a complete parent reopens the chain.
			affected := []string{from}
			ancestors, err := issues.Ancestors(tx, from)
			if err != nil {
				return 0, nil, errorFrom(err)
			}
			affected = append(affected, ancestors...)
			child, err := issues.Get(tx, req.To)
			if err != nil {
				return 0, nil, issueErrorFrom(err)
			}
			activeBelow := issues.Active(child.Status)
			if !activeBelow {
				if activeBelow, err = issues.ActiveInSubtree(tx, req.To); err != nil {
					return 0, nil, errorFrom(err)
				}
			}
			if activeBelow {
				for _, candidate := range append([]string{from}, ancestors...) {
					c, err := issues.Get(tx, candidate)
					if err != nil {
						return 0, nil, issueErrorFrom(err)
					}
					if c.Status == issues.StatusComplete {
						if err := issues.SetStatus(tx, candidate, issues.StatusOpen); err != nil {
							return 0, nil, issueErrorFrom(err)
						}
						if _, err := events.Emit(tx, "issue.status-changed", candidate, operation, req.Actor, nil); err != nil {
							return 0, nil, errorFrom(err)
						}
					}
				}
			}
			if err := issues.BumpSubtree(tx, affected); err != nil {
				return 0, nil, errorFrom(err)
			}
		}
		return http.StatusCreated, map[string]any{"relation": rel, "operation": operation}, nil
	})
}

func (s *server) listIssueRelations(w http.ResponseWriter, r *http.Request) {
	tx, err := s.db.Begin()
	if err != nil {
		writeError(w, errorFrom(err))
		return
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := issues.Get(tx, r.PathValue("issueId")); err != nil {
		writeError(w, issueErrorFrom(err))
		return
	}
	rels, err := issues.Relations(tx, r.PathValue("issueId"))
	if err != nil {
		writeError(w, issueErrorFrom(err))
		return
	}
	writeJSON(w, http.StatusOK, rels)
}

func (s *server) removeIssueRelation(w http.ResponseWriter, r *http.Request) {
	s.idempotent(w, r, func(tx *sql.Tx) (int, any, *apiError) {
		actor := r.URL.Query().Get("actor")
		if apiErr := requireActor(tx, actor); apiErr != nil {
			return 0, nil, apiErr
		}
		rel, err := issues.RemoveRelation(tx, r.PathValue("relationId"))
		if err != nil {
			return 0, nil, issueErrorFrom(err)
		}
		if pathIssue := r.PathValue("issueId"); rel.From != pathIssue && rel.To != pathIssue {
			return 0, nil, &apiError{status: http.StatusNotFound, code: "not-found",
				message: fmt.Sprintf("relation %s does not involve issue %s", rel.ID, pathIssue)}
		}
		fromIssue, err := issues.Get(tx, rel.From)
		if err != nil {
			return 0, nil, issueErrorFrom(err)
		}
		if apiErr := guardWritable(tx, fromIssue.Project); apiErr != nil {
			return 0, nil, apiErr
		}
		toIssue, err := issues.Get(tx, rel.To)
		if err != nil {
			return 0, nil, issueErrorFrom(err)
		}
		if toIssue.Project != fromIssue.Project {
			if apiErr := guardWritable(tx, toIssue.Project); apiErr != nil {
				return 0, nil, apiErr
			}
		}
		payload, err := json.Marshal(map[string]string{
			"relation": rel.ID, "kind": rel.Kind, "from": rel.From, "to": rel.To,
		})
		if err != nil {
			return 0, nil, errorFrom(err)
		}
		payloadStr := string(payload)
		if _, err := events.Emit(tx, "issue.relation-removed", rel.From, events.NewOperation(), actor, &payloadStr); err != nil {
			return 0, nil, errorFrom(err)
		}
		if rel.Kind == "parent_of" {
			affected := []string{rel.From}
			ancestors, err := issues.Ancestors(tx, rel.From)
			if err != nil {
				return 0, nil, errorFrom(err)
			}
			if err := issues.BumpSubtree(tx, append(affected, ancestors...)); err != nil {
				return 0, nil, errorFrom(err)
			}
		}
		return http.StatusNoContent, nil, nil
	})
}
