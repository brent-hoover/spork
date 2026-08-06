package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"sutra/internal/events"
	"sutra/internal/identity"
	"sutra/internal/issues"
	"sutra/internal/projects"
	"sutra/internal/review"
)

// requireHumanVerdictActor enforces the contract's human-approval rule:
// verdict actors are HUMAN identities — an agent can never satisfy the
// close gate by approving its own review.
func requireHumanVerdictActor(tx *sql.Tx, actor string) *apiError {
	if !isUUID(actor) {
		return &apiError{status: http.StatusBadRequest, code: "bad-request", message: "actor must be an identity uuid"}
	}
	who, err := identity.Lookup(tx, actor)
	if err != nil {
		var notFound *identity.NotFoundError
		if errors.As(err, &notFound) {
			return &apiError{status: http.StatusBadRequest, code: "bad-request", message: fmt.Sprintf("actor %q names no identity", actor)}
		}
		return &apiError{status: http.StatusInternalServerError, code: "bad-request", message: err.Error()}
	}
	if who.Kind != "human" {
		return &apiError{status: http.StatusBadRequest, code: "bad-request",
			message: fmt.Sprintf("verdicts require a human identity; %q is %s", who.Handle, who.Kind)}
	}
	return nil
}

// guardReviewProject applies the archived write-guard through the
// review's issue — archived projects are read-only for verdicts and
// consumption too.
func guardReviewProject(tx *sql.Tx, rev review.Review) *apiError {
	issue, err := issues.Get(tx, rev.Issue)
	if err != nil {
		return issueErrorFrom(err)
	}
	return guardWritable(tx, issue.Project)
}

func reviewErrorFrom(err error) *apiError {
	var notFound *review.NotFoundError
	if errors.As(err, &notFound) {
		return &apiError{status: http.StatusNotFound, code: "not-found", message: err.Error()}
	}
	var conflict *review.ConflictError
	if errors.As(err, &conflict) {
		return &apiError{status: http.StatusConflict, code: conflict.Code, message: conflict.Message}
	}
	var gitErr *review.GitError
	if errors.As(err, &gitErr) {
		return &apiError{status: http.StatusConflict, code: "bad-request", message: gitErr.Message}
	}
	return issueErrorFrom(err)
}

// deliverableFields is the exactly-one shape shared by create and
// resubmit bodies (contract oneOf: branch+commit XOR doc_version).
type deliverableFields struct {
	Branch     *string `json:"branch"`
	Commit     *string `json:"commit"`
	DocVersion *string `json:"doc_version"`
	Session    *string `json:"session"`
}

func (d *deliverableFields) validate() *apiError {
	code := d.Branch != nil || d.Commit != nil
	doc := d.DocVersion != nil
	switch {
	case code && doc:
		return &apiError{status: http.StatusBadRequest, code: "bad-request", message: "exactly one deliverable: branch+commit or doc_version, not both"}
	case code && (d.Branch == nil || d.Commit == nil):
		return &apiError{status: http.StatusBadRequest, code: "bad-request", message: "code deliverables require branch and commit together"}
	case !code && !doc:
		return &apiError{status: http.StatusBadRequest, code: "bad-request", message: "exactly one deliverable is required"}
	}
	if d.Commit != nil && !isCommitSHA(*d.Commit) {
		return &apiError{status: http.StatusBadRequest, code: "bad-request", message: "commit must be a canonical full object id"}
	}
	return nil
}

// isCommitSHA accepts full 40-hex (SHA-1) or 64-hex (SHA-256) ids only.
func isCommitSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// resolveDeliverable validates the shape and, for code deliverables,
// resolves and fences base_commit via the issue's project repo. Doc
// deliverables reject truthfully until the docs module exists — no
// doc_version can name a stored version yet.
func resolveDeliverable(tx *sql.Tx, issueProject string, d deliverableFields, expectedBase, expectedHead *string) (review.Deliverable, string, *apiError) {
	if apiErr := d.validate(); apiErr != nil {
		return review.Deliverable{}, "", apiErr
	}
	out := review.Deliverable{Branch: d.Branch, Commit: d.Commit, DocVersion: d.DocVersion, Session: d.Session}
	if d.DocVersion != nil {
		return review.Deliverable{}, "", &apiError{status: http.StatusNotFound, code: "not-found",
			message: fmt.Sprintf("doc version %s does not exist", *d.DocVersion)}
	}
	p, err := projects.GetTx(tx, issueProject)
	if err != nil {
		return review.Deliverable{}, "", errorFrom(err)
	}
	repo := review.Repo{DefaultBranch: "main"}
	if p.RepoPath != nil {
		repo.Path = *p.RepoPath
	}
	if p.DefaultBranch != nil {
		repo.DefaultBranch = *p.DefaultBranch
	}
	base, err := review.ResolveCode(repo, *d.Commit, review.Fences{
		ExpectedBaseCommit: expectedBase, ExpectedDefaultHead: expectedHead,
	})
	if err != nil {
		return review.Deliverable{}, "", reviewErrorFrom(err)
	}
	return out, base, nil
}

type newReviewRequest struct {
	deliverableFields
	Issue               string  `json:"issue"`
	Author              string  `json:"author"`
	Summary             *string `json:"summary"`
	ExpectedBaseCommit  *string `json:"expected_base_commit"`
	ExpectedDefaultHead *string `json:"expected_default_head"`
}

// preparedReview carries the decoded request and the git-resolved pin
// from the prepare stage into the transaction.
type preparedReview struct {
	req  newReviewRequest
	d    review.Deliverable
	base string
}

func (s *server) createReview(w http.ResponseWriter, r *http.Request) {
	// Git resolution runs in the prepare stage — bounded and OUTSIDE
	// SQLite's write lock, so a slow repository never blocks unrelated
	// mutations. The transaction revalidates the database-side facts.
	prepare := func(r *http.Request) (any, *apiError) {
		var req newReviewRequest
		if apiErr := decodeBody(r, &req); apiErr != nil {
			return nil, apiErr
		}
		read, err := s.db.Begin()
		if err != nil {
			return nil, errorFrom(err)
		}
		defer func() { _ = read.Rollback() }()
		issue, err := issues.Get(read, req.Issue)
		if err != nil {
			return nil, issueErrorFrom(err)
		}
		d, base, apiErr := resolveDeliverable(read, issue.Project, req.deliverableFields, req.ExpectedBaseCommit, req.ExpectedDefaultHead)
		if apiErr != nil {
			return nil, apiErr
		}
		return preparedReview{req: req, d: d, base: base}, nil
	}
	s.idempotentPrepared(w, r, prepare, func(tx *sql.Tx, prepped any) (int, any, *apiError) {
		p := prepped.(preparedReview)
		if apiErr := requireActor(tx, p.req.Author); apiErr != nil {
			return 0, nil, apiErr
		}
		issue, err := issues.Get(tx, p.req.Issue)
		if err != nil {
			return 0, nil, issueErrorFrom(err)
		}
		if apiErr := guardWritable(tx, issue.Project); apiErr != nil {
			return 0, nil, apiErr
		}
		created, err := review.Create(tx, p.req.Issue, p.req.Author, p.d, p.req.Summary, p.base)
		if err != nil {
			return 0, nil, reviewErrorFrom(err)
		}
		payload := reviewPayload(created.ID)
		if _, err := events.Emit(tx, "review.created", p.req.Issue, events.NewOperation(), p.req.Author, &payload); err != nil {
			return 0, nil, errorFrom(err)
		}
		return http.StatusCreated, created, nil
	})
}

func reviewPayload(reviewID string) string {
	raw, _ := json.Marshal(map[string]string{"review": reviewID})
	return string(raw)
}

func changesRequestedPayload(r review.Review) string {
	body := map[string]any{"review": r.ID, "issue": r.Issue}
	if r.Session != nil {
		body["session"] = *r.Session
	}
	raw, _ := json.Marshal(body)
	return string(raw)
}

func (s *server) getReview(w http.ResponseWriter, r *http.Request) {
	tx, err := s.db.Begin()
	if err != nil {
		writeError(w, errorFrom(err))
		return
	}
	defer func() { _ = tx.Rollback() }()
	rev, err := review.Get(tx, r.PathValue("reviewId"))
	if err != nil {
		writeError(w, reviewErrorFrom(err))
		return
	}
	writeJSON(w, http.StatusOK, rev)
}

func (s *server) listReviews(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tx, err := s.db.Begin()
	if err != nil {
		writeError(w, errorFrom(err))
		return
	}
	defer func() { _ = tx.Rollback() }()
	list, err := review.List(tx, q.Get("issue"), q.Get("state"), q.Get("session"))
	if err != nil {
		writeError(w, reviewErrorFrom(err))
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *server) setReviewVerdict(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("reviewId")
	s.idempotent(w, r, func(tx *sql.Tx) (int, any, *apiError) {
		var req struct {
			Verdict  string `json:"verdict"`
			Revision *int64 `json:"revision"`
			Actor    string `json:"actor"`
		}
		if apiErr := decodeBody(r, &req); apiErr != nil {
			return 0, nil, apiErr
		}
		if req.Verdict != review.StateApproved && req.Verdict != review.StateChangesRequested {
			return 0, nil, &apiError{status: http.StatusBadRequest, code: "bad-request", message: fmt.Sprintf("verdict %q must be approved or changes-requested", req.Verdict)}
		}
		if req.Revision == nil {
			return 0, nil, &apiError{status: http.StatusBadRequest, code: "bad-request", message: "revision is required"}
		}
		if apiErr := requireHumanVerdictActor(tx, req.Actor); apiErr != nil {
			return 0, nil, apiErr
		}
		current, err := review.Get(tx, id)
		if err != nil {
			return 0, nil, reviewErrorFrom(err)
		}
		if apiErr := guardReviewProject(tx, current); apiErr != nil {
			return 0, nil, apiErr
		}
		kind := "review.approved"
		payload := reviewPayload(id)
		if req.Verdict == review.StateChangesRequested {
			kind = "review.changes-requested"
			// The rework payload carries the review's session and issue
			// so a subscriber routes the resubmission back to the
			// originating agent instance (AC-review-rework).
			payload = changesRequestedPayload(current)
		}
		eventID, err := events.Emit(tx, kind, current.Issue, events.NewOperation(), req.Actor, &payload)
		if err != nil {
			return 0, nil, errorFrom(err)
		}
		updated, err := review.SetVerdict(tx, id, req.Verdict, *req.Revision, eventID)
		if err != nil {
			// The savepoint discards the event with the failed verdict.
			return 0, nil, reviewErrorFrom(err)
		}
		return http.StatusOK, updated, nil
	})
}

func (s *server) consumeReviewApproval(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("reviewId")
	s.idempotent(w, r, func(tx *sql.Tx) (int, any, *apiError) {
		var req struct {
			ExpectedRevision     *int64 `json:"expected_revision"`
			ExpectedVerdictEvent string `json:"expected_verdict_event"`
			Actor                string `json:"actor"`
		}
		if apiErr := decodeBody(r, &req); apiErr != nil {
			return 0, nil, apiErr
		}
		if req.ExpectedRevision == nil || !isUUID(req.ExpectedVerdictEvent) {
			return 0, nil, &apiError{status: http.StatusBadRequest, code: "bad-request", message: "expected_revision and expected_verdict_event are required"}
		}
		if apiErr := requireActor(tx, req.Actor); apiErr != nil {
			return 0, nil, apiErr
		}
		current, err := review.Get(tx, id)
		if err != nil {
			return 0, nil, reviewErrorFrom(err)
		}
		if apiErr := guardReviewProject(tx, current); apiErr != nil {
			return 0, nil, apiErr
		}
		consumed, err := review.Consume(tx, id, *req.ExpectedRevision, req.ExpectedVerdictEvent)
		if err != nil {
			return 0, nil, reviewErrorFrom(err)
		}
		payload, _ := json.Marshal(map[string]any{"review": id, "consumed_revision": *consumed.ConsumedRevision})
		payloadStr := string(payload)
		if _, err := events.Emit(tx, "review.consumed", consumed.Issue, events.NewOperation(), req.Actor, &payloadStr); err != nil {
			return 0, nil, errorFrom(err)
		}
		return http.StatusOK, consumed, nil
	})
}

type resubmitRequest struct {
	deliverableFields
	Author               string  `json:"author"`
	Summary              *string `json:"summary"`
	ExpectedRevision     *int64  `json:"expected_revision"`
	ExpectedVerdictEvent string  `json:"expected_verdict_event"`
	ExpectedBaseCommit   *string `json:"expected_base_commit"`
	ExpectedDefaultHead  *string `json:"expected_default_head"`
}

type preparedResubmit struct {
	req  resubmitRequest
	d    review.Deliverable
	base string
}

func (s *server) resubmitReview(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("reviewId")
	prepare := func(r *http.Request) (any, *apiError) {
		var req resubmitRequest
		if apiErr := decodeBody(r, &req); apiErr != nil {
			return nil, apiErr
		}
		if req.ExpectedRevision == nil || !isUUID(req.ExpectedVerdictEvent) {
			return nil, &apiError{status: http.StatusBadRequest, code: "bad-request", message: "expected_revision and expected_verdict_event are required"}
		}
		read, err := s.db.Begin()
		if err != nil {
			return nil, errorFrom(err)
		}
		defer func() { _ = read.Rollback() }()
		current, err := review.Get(read, id)
		if err != nil {
			return nil, reviewErrorFrom(err)
		}
		issue, err := issues.Get(read, current.Issue)
		if err != nil {
			return nil, issueErrorFrom(err)
		}
		d, base, apiErr := resolveDeliverable(read, issue.Project, req.deliverableFields, req.ExpectedBaseCommit, req.ExpectedDefaultHead)
		if apiErr != nil {
			return nil, apiErr
		}
		return preparedResubmit{req: req, d: d, base: base}, nil
	}
	s.idempotentPrepared(w, r, prepare, func(tx *sql.Tx, prepped any) (int, any, *apiError) {
		p := prepped.(preparedResubmit)
		if apiErr := requireActor(tx, p.req.Author); apiErr != nil {
			return 0, nil, apiErr
		}
		current, err := review.Get(tx, id)
		if err != nil {
			return 0, nil, reviewErrorFrom(err)
		}
		if apiErr := guardReviewProject(tx, current); apiErr != nil {
			return 0, nil, apiErr
		}
		updated, err := review.Resubmit(tx, id, *p.req.ExpectedRevision, p.req.ExpectedVerdictEvent, p.d, p.base)
		if err != nil {
			return 0, nil, reviewErrorFrom(err)
		}
		payload := reviewPayload(id)
		if _, err := events.Emit(tx, "review.resubmitted", current.Issue, events.NewOperation(), p.req.Author, &payload); err != nil {
			return 0, nil, errorFrom(err)
		}
		return http.StatusOK, updated, nil
	})
}
