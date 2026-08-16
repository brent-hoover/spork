package api

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"sutra/internal/docs"
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
	// door: record a review verdict, consume a review approval, resubmit a review
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

// rejectExplicitNulls decodes the request body while rejecting an
// explicit null on any of the named fields — a null fence is not an
// absent fence and must never silently disarm one.
// rejectExplicitNulls decodes the request body into `into` after
// verifying none of the named top-level fields is an explicit JSON
// null (the omit-when-absent convention). The null check is a
// streaming token scan — no field value is retained, so a large
// content field is resident exactly once: in the decoded struct.
func rejectExplicitNulls(r *http.Request, into any, fields ...string) *apiError {
	nulls, apiErr := scanExplicitNulls(r.Body)
	if apiErr != nil {
		return apiErr
	}
	for _, field := range fields {
		if nulls[field] {
			return &apiError{status: http.StatusBadRequest, code: "bad-request", message: fmt.Sprintf("%s must not be null", field)}
		}
	}
	if apiErr := rewindBody(r); apiErr != nil {
		return apiErr
	}
	return decodeBody(r, into)
}

// rewindBody returns a captured body to its start so a second pass can
// read it. Every idempotent route captures its body up front, so a
// non-seekable body here is a wiring error, not client input.
func rewindBody(r *http.Request) *apiError {
	seeker, ok := r.Body.(io.Seeker)
	if !ok {
		return &apiError{status: http.StatusInternalServerError, code: "bad-request", message: "request body is not rewindable"}
	}
	if _, err := seeker.Seek(0, io.SeekStart); err != nil {
		return &apiError{status: http.StatusInternalServerError, code: "bad-request", message: fmt.Sprintf("rewind body: %v", err)}
	}
	return nil
}

// scanExplicitNulls lexes the body's top-level object byte by byte
// and reports which keys carry a literal null. Nothing materializes —
// not even string tokens, unlike a json.Decoder walk — so the scan
// runs in O(1) memory over gigabyte content fields. Only gross shape
// errors reject here; pass B's decode is the authority on malformed
// JSON, and it re-reads the same captured body.
func scanExplicitNulls(body io.Reader) (map[string]bool, *apiError) {
	malformed := func(detail string) *apiError {
		return &apiError{status: http.StatusBadRequest, code: "bad-request", message: "malformed request body: " + detail}
	}
	br := bufio.NewReaderSize(body, 64<<10)
	next := func() (byte, error) {
		for {
			c, err := br.ReadByte()
			if err != nil {
				return 0, err
			}
			switch c {
			case ' ', '\t', '\n', '\r':
				continue
			}
			return c, nil
		}
	}
	// skipString consumes bytes to the closing quote, honoring
	// backslash escapes, retaining nothing.
	skipString := func() error {
		escaped := false
		for {
			c, err := br.ReadByte()
			if err != nil {
				return err
			}
			if escaped {
				escaped = false
				continue
			}
			switch c {
			case '\\':
				escaped = true
			case '"':
				return nil
			}
		}
	}
	// maxKeyCapture bounds key retention; a longer key cannot name a
	// handler field, so its null-ness is irrelevant.
	const maxKeyCapture = 256

	c, err := next()
	if err != nil || c != '{' {
		return nil, malformed("expected a JSON object")
	}
	nulls := map[string]bool{}
	first := true
	for {
		c, err := next()
		if err != nil {
			return nil, malformed("truncated object")
		}
		if c == '}' {
			return nulls, nil
		}
		if !first {
			if c != ',' {
				return nil, malformed("expected a comma between members")
			}
			if c, err = next(); err != nil {
				return nil, malformed("truncated object")
			}
		}
		first = false
		if c != '"' {
			return nil, malformed("expected a string key")
		}
		// Raw key bytes are captured escapes-and-all, then decoded by
		// encoding/json below — so \u-escaped names resolve exactly as
		// the second-pass decoder will see them, and an escaped null
		// can never slip past the field check.
		key := make([]byte, 0, 32)
		overlong := false
		escaped := false
		// One cap, checked in one place: every byte of the key — plain,
		// backslash, or escaped — passes through here, so the retention
		// bound cannot drift between the three ways a byte arrives.
		capture := func(b byte) {
			if len(key) < maxKeyCapture {
				key = append(key, b)
				return
			}
			overlong = true
		}
		for {
			b, err := br.ReadByte()
			if err != nil {
				return nil, malformed("truncated key")
			}
			if escaped {
				escaped = false
				capture(b)
				continue
			}
			if b == '\\' {
				escaped = true
				capture(b)
				continue
			}
			if b == '"' {
				break
			}
			capture(b)
		}
		// An overlong key was cut off mid-stream, so what sits in the
		// buffer is a prefix, not a name — and a prefix can end inside
		// an escape sequence, which decodes as garbage or not at all.
		// Its null-ness is already discarded below, so decoding it buys
		// nothing and costs a refusal: without this guard a body whose
		// key merely happens to carry a backslash across the cap comes
		// back rejected as a bad escape it does not contain. This pass
		// only reports gross shape errors; the decode in pass B is the
		// authority on malformed JSON.
		decodedKey := string(key)
		if !overlong && bytes.ContainsRune(key, '\\') {
			var s string
			if err := json.Unmarshal(append(append([]byte{'"'}, key...), '"'), &s); err != nil {
				return nil, malformed("bad key escape")
			}
			decodedKey = s
		}
		if c, err = next(); err != nil || c != ':' {
			return nil, malformed("expected a colon")
		}
		c, err = next()
		if err != nil {
			return nil, malformed("truncated value")
		}
		switch c {
		case 'n':
			// null — verify the literal so "nonsense" doesn't pass
			rest := make([]byte, 3)
			if _, err := io.ReadFull(br, rest); err != nil || string(rest) != "ull" {
				return nil, malformed("bad literal")
			}
			if !overlong {
				nulls[decodedKey] = true
			}
		case '"':
			if err := skipString(); err != nil {
				return nil, malformed("truncated string")
			}
		case '{', '[':
			depth := 1
			for depth > 0 {
				b, err := br.ReadByte()
				if err != nil {
					return nil, malformed("truncated value")
				}
				switch b {
				case '"':
					if err := skipString(); err != nil {
						return nil, malformed("truncated string")
					}
				case '{', '[':
					depth++
				case '}', ']':
					depth--
				}
			}
		default:
			// number, true, false: consume to the next member/end
			for {
				b, err := br.ReadByte()
				if err != nil {
					return nil, malformed("truncated value")
				}
				if b == ',' || b == '}' {
					if err := br.UnreadByte(); err != nil {
						return nil, malformed("lexer rewind")
					}
					break
				}
			}
		}
	}
}

// repoFacts snapshots the project fields git resolution needs, so the
// read transaction closes BEFORE any git process runs.
func repoFacts(p projects.Project) review.Repo {
	repo := review.Repo{DefaultBranch: "main"}
	if p.RepoPath != nil {
		repo.Path = *p.RepoPath
	}
	if p.DefaultBranch != nil {
		repo.DefaultBranch = *p.DefaultBranch
	}
	return repo
}

// resolveDeliverable validates the shape and, for code deliverables,
// resolves and fences base_commit against the snapshotted repo facts.
// Runs git — the caller must have closed its read transaction. Doc
// deliverables are validated by the caller against the docs store (a
// database concern), so they never reach this function.
func resolveDeliverable(repo review.Repo, d deliverableFields, expectedBase, expectedHead *string) (review.Deliverable, string, string, *apiError) {
	if apiErr := d.validate(); apiErr != nil {
		return review.Deliverable{}, "", "", apiErr
	}
	out := review.Deliverable{Branch: d.Branch, Commit: d.Commit, DocVersion: d.DocVersion, Session: d.Session}
	base, err := review.ResolveCode(repo, *d.Commit, review.Fences{
		ExpectedBaseCommit: expectedBase, ExpectedDefaultHead: expectedHead,
	})
	if err != nil {
		return review.Deliverable{}, "", "", reviewErrorFrom(err)
	}
	// The deliverable renders ONCE, here at submission — validated
	// (resolvable, size-capped, UTF-8) and returned for storage with
	// the submission; it is served verbatim ever after, immutable by
	// construction.
	content, err := review.Render(repo.Path, base, *d.Commit)
	if err != nil {
		return review.Deliverable{}, "", "", reviewErrorFrom(err)
	}
	return out, base, content, nil
}

type newReviewRequest struct {
	deliverableFields
	Issue               string  `json:"issue"`
	Author              string  `json:"author"`
	Summary             *string `json:"summary"`
	ExpectedBaseCommit  *string `json:"expected_base_commit"`
	ExpectedDefaultHead *string `json:"expected_default_head"`
}

// preparedReview carries the decoded request, the git-resolved pin,
// and the submission-time render from the prepare stage into the
// transaction.
type preparedReview struct {
	req     newReviewRequest
	d       review.Deliverable
	base    string
	content string
	repo    review.Repo
}

func (s *server) createReview(w http.ResponseWriter, r *http.Request) {
	// Git resolution runs in the prepare stage — bounded and OUTSIDE
	// SQLite's write lock, so a slow repository never blocks unrelated
	// mutations. The transaction revalidates the database-side facts.
	prepare := func(r *http.Request) (any, *apiError) {
		var req newReviewRequest
		if apiErr := rejectExplicitNulls(r, &req, "issue", "author", "summary", "session", "branch", "commit", "doc_version", "expected_base_commit", "expected_default_head"); apiErr != nil {
			return nil, apiErr
		}
		// Snapshot database facts, CLOSE the read transaction, and only
		// then run git — no connection is held across a git process.
		// EVERY deterministic validation runs here, before any durable
		// pin lands: a validation-rejected request pins nothing; only
		// crash/lost-race windows can leave (benign, content-addressed)
		// surplus pins. The transaction revalidates authoritatively.
		read, err := s.db.Begin()
		if err != nil {
			return nil, errorFrom(err)
		}
		// The explicit rollbacks below are what release the connection
		// before git runs — that ordering is the point of this stage.
		// This defer covers only what they cannot: a panic between here
		// and one of them. Rolling back a finished transaction is a
		// no-op, so it holds nothing open.
		defer func() { _ = read.Rollback() }()
		if apiErr := requireActor(read, req.Author); apiErr != nil {
			_ = read.Rollback()
			return nil, apiErr
		}
		issue, err := issues.Get(read, req.Issue)
		if err != nil {
			_ = read.Rollback()
			return nil, issueErrorFrom(err)
		}
		// door: open a review
		if apiErr := guardWritable(read, issue.Project); apiErr != nil {
			_ = read.Rollback()
			return nil, apiErr
		}
		if apiErr := req.validate(); apiErr != nil {
			_ = read.Rollback()
			return nil, apiErr
		}
		if req.DocVersion != nil {
			// Doc deliverable: the version must exist AND belong to
			// the issue's project — a foreign project's doc must never
			// authorize this issue's completion, exactly as code
			// deliverables resolve through the issue's own project.
			// The immutable version IS the deliverable — served by
			// resolution, never copied (AC-review-deliverable-kinds).
			version, err := docs.VersionMetaByID(read, *req.DocVersion)
			if err != nil {
				_ = read.Rollback()
				return nil, docErrorFrom(err)
			}
			// The deliverable check compares projects, nothing more.
			doc, err := docs.MetaByID(read, version.Document)
			_ = read.Rollback()
			if err != nil {
				return nil, docErrorFrom(err)
			}
			if doc.Project != issue.Project {
				return nil, &apiError{status: http.StatusBadRequest, code: "bad-request", message: "doc deliverable must belong to the issue's project"}
			}
			d := review.Deliverable{DocVersion: req.DocVersion, Session: req.Session}
			// No content travels: the immutable version IS the
			// deliverable, resolved at serving time — loading its
			// bytes here would be a copy made only to be discarded.
			return preparedReview{req: req, d: d}, nil
		}
		project, err := projects.GetTx(read, issue.Project)
		_ = read.Rollback()
		if err != nil {
			return nil, errorFrom(err)
		}
		repo := repoFacts(project)
		d, base, content, apiErr := resolveDeliverable(repo, req.deliverableFields, req.ExpectedBaseCommit, req.ExpectedDefaultHead)
		if apiErr != nil {
			return nil, apiErr
		}
		// Fences revalidate as prepare's last step — bounded git with
		// no database lock held. The residual window from here to
		// COMMIT is inherent to fencing an external store: even an
		// in-transaction recheck leaves the same gap between its
		// rev-parse and the commit, so this placement trades nothing
		// while keeping git out of the SQLite writer entirely. Fences
		// that were never supplied are RevalidateFences' business, not
		// the caller's — it returns before touching git.
		if err := review.RevalidateFences(repo, *d.Commit, review.Fences{
			ExpectedBaseCommit: req.ExpectedBaseCommit, ExpectedDefaultHead: req.ExpectedDefaultHead,
		}); err != nil {
			return nil, reviewErrorFrom(err)
		}
		return preparedReview{req: req, d: d, base: base, content: content, repo: repo}, nil
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
		// door: open a review
		if apiErr := guardWritable(tx, issue.Project); apiErr != nil {
			return 0, nil, apiErr
		}
		// Doc deliverables store NO content copy: the referenced
		// version is immutable and the deliverable endpoint resolves
		// it — a stored copy could only ever diverge (review 1841).
		content := &p.content
		if p.d.DocVersion != nil {
			content = nil
		}
		created, err := review.Create(tx, p.req.Issue, p.req.Author, p.d, p.req.Summary, p.base, content)
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

// getReviewDeliverable resolves the pinned content approval covers —
// the complete diff between the selected submission's immutable
// base_commit and commit, never recomputed against the current default
// branch and never a summary in content's place (AC-review-web).
func (s *server) getReviewDeliverable(w http.ResponseWriter, r *http.Request) {
	revisionParam := r.URL.Query().Get("revision")
	var revision int64
	if revisionParam != "" {
		n, err := strconv.ParseInt(revisionParam, 10, 64)
		if err != nil || n < 1 {
			writeError(w, &apiError{status: http.StatusBadRequest, code: "bad-request", message: fmt.Sprintf("revision %q must be a positive integer", revisionParam)})
			return
		}
		revision = n
	}
	tx, err := s.db.Begin()
	if err != nil {
		writeError(w, errorFrom(err))
		return
	}
	// The explicit rollback before the response is what keeps a
	// connection off the wire write; this catches a panic before it.
	defer func() { _ = tx.Rollback() }()
	rev, err := review.Get(tx, r.PathValue("reviewId"))
	if err != nil {
		_ = tx.Rollback()
		writeError(w, reviewErrorFrom(err))
		return
	}
	sub, ok := review.SubmissionAt(rev, revision)
	if !ok {
		_ = tx.Rollback()
		writeError(w, &apiError{status: http.StatusNotFound, code: "not-found", message: fmt.Sprintf("no submission at revision %d", revision)})
		return
	}
	kind := "code"
	if sub.Commit == nil {
		kind = "doc"
	}
	content, err := review.ContentAt(tx, rev.ID, revision)
	if err != nil {
		_ = tx.Rollback()
		writeError(w, errorFrom(err))
		return
	}
	if content == nil && sub.DocVersion != nil {
		// A doc review stores no copy of what it reviews: the immutable
		// version IS the deliverable, resolved here at read time. Code
		// deliverables render at submission and store the render, so the
		// two kinds reach the reader by different routes.
		version, err := docs.VersionByID(tx, *sub.DocVersion)
		if err != nil {
			_ = tx.Rollback()
			writeError(w, docErrorFrom(err))
			return
		}
		content = &version.Content
	}
	_ = tx.Rollback()
	if content == nil {
		writeError(w, &apiError{status: http.StatusConflict, code: "bad-request", message: "submission predates stored content"})
		return
	}
	// Served verbatim from the submission-time render: immutable by
	// construction — no git, no repository configuration, no
	// reachability concerns can alter what approval covered.
	resolved := map[string]any{"kind": kind, "content": *content}
	if rev.Summary != nil {
		resolved["summary"] = *rev.Summary
	}
	writeJSON(w, http.StatusOK, resolved)
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
	// Review summaries are unbounded; rows stream to the wire one at
	// a time instead of accumulating an aggregate slice and buffer.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte{'['})
	first := true
	streamErr := review.ListEach(tx, q.Get("issue"), q.Get("state"), q.Get("session"), func(rv review.Review) error {
		raw, err := json.Marshal(rv)
		if err != nil {
			return err
		}
		if !first {
			_, _ = w.Write([]byte{','})
		}
		first = false
		_, writeErr := w.Write(raw)
		return writeErr
	})
	if streamErr != nil {
		return // status committed; truncation is the only signal
	}
	_, _ = w.Write([]byte{']'})
}

func (s *server) setReviewVerdict(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("reviewId")
	type verdictRequest struct {
		Verdict  string `json:"verdict"`
		Revision *int64 `json:"revision"`
		Actor    string `json:"actor"`
	}
	s.idempotentPrepared(w, r, func(r *http.Request) (any, *apiError) {
		var req verdictRequest
		if apiErr := decodeBody(r, &req); apiErr != nil {
			return nil, apiErr
		}
		if req.Verdict != review.StateApproved && req.Verdict != review.StateChangesRequested {
			return nil, &apiError{status: http.StatusBadRequest, code: "bad-request", message: fmt.Sprintf("verdict %q must be approved or changes-requested", req.Verdict)}
		}
		if req.Revision == nil {
			return nil, &apiError{status: http.StatusBadRequest, code: "bad-request", message: "revision is required"}
		}
		return req, nil
	}, func(tx *sql.Tx, prepped any) (int, any, *apiError) {
		req := prepped.(verdictRequest)
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
	type consumeRequest struct {
		ExpectedRevision     *int64 `json:"expected_revision"`
		ExpectedVerdictEvent string `json:"expected_verdict_event"`
		Actor                string `json:"actor"`
	}
	s.idempotentPrepared(w, r, func(r *http.Request) (any, *apiError) {
		var req consumeRequest
		if apiErr := decodeBody(r, &req); apiErr != nil {
			return nil, apiErr
		}
		if req.ExpectedRevision == nil || !isUUID(req.ExpectedVerdictEvent) {
			return nil, &apiError{status: http.StatusBadRequest, code: "bad-request", message: "expected_revision and expected_verdict_event are required"}
		}
		return req, nil
	}, func(tx *sql.Tx, prepped any) (int, any, *apiError) {
		req := prepped.(consumeRequest)
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
	req     resubmitRequest
	d       review.Deliverable
	base    string
	content string
	repo    review.Repo
}

func (s *server) resubmitReview(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("reviewId")
	prepare := func(r *http.Request) (any, *apiError) {
		var req resubmitRequest
		if apiErr := rejectExplicitNulls(r, &req, "author", "summary", "session", "branch", "commit", "doc_version", "expected_revision", "expected_verdict_event", "expected_base_commit", "expected_default_head"); apiErr != nil {
			return nil, apiErr
		}
		if req.ExpectedRevision == nil || !isUUID(req.ExpectedVerdictEvent) {
			return nil, &apiError{status: http.StatusBadRequest, code: "bad-request", message: "expected_revision and expected_verdict_event are required"}
		}
		read, err := s.db.Begin()
		if err != nil {
			return nil, errorFrom(err)
		}
		// The explicit rollbacks below are what release the connection
		// before git runs — that ordering is the point of this stage.
		// This defer covers only what they cannot: a panic between here
		// and one of them. Rolling back a finished transaction is a
		// no-op, so it holds nothing open.
		defer func() { _ = read.Rollback() }()
		if apiErr := requireActor(read, req.Author); apiErr != nil {
			_ = read.Rollback()
			return nil, apiErr
		}
		current, err := review.Get(read, id)
		if err != nil {
			_ = read.Rollback()
			return nil, reviewErrorFrom(err)
		}
		// Best-effort state/fence validation before any pin lands; the
		// transaction re-runs these authoritatively via Resubmit.
		if current.State != review.StateChangesRequested {
			_ = read.Rollback()
			return nil, &apiError{status: http.StatusConflict, code: "expected-status-mismatch",
				message: fmt.Sprintf("review %s is %s; only changes-requested resubmits", id, current.State)}
		}
		if current.Revision != *req.ExpectedRevision {
			_ = read.Rollback()
			return nil, &apiError{status: http.StatusConflict, code: "expected-revision-mismatch",
				message: fmt.Sprintf("review %s is at revision %d, expected %d", id, current.Revision, *req.ExpectedRevision)}
		}
		if current.LatestVerdictEvent == nil || *current.LatestVerdictEvent != req.ExpectedVerdictEvent {
			_ = read.Rollback()
			return nil, &apiError{status: http.StatusConflict, code: "expected-verdict-event-mismatch",
				message: fmt.Sprintf("review %s verdict event moved; rework must answer the latest feedback", id)}
		}
		issue, err := issues.Get(read, current.Issue)
		if err != nil {
			_ = read.Rollback()
			return nil, issueErrorFrom(err)
		}
		// door: resubmit a review
		if apiErr := guardWritable(read, issue.Project); apiErr != nil {
			_ = read.Rollback()
			return nil, apiErr
		}
		if apiErr := req.validate(); apiErr != nil {
			_ = read.Rollback()
			return nil, apiErr
		}
		if req.DocVersion != nil {
			version, err := docs.VersionMetaByID(read, *req.DocVersion)
			if err != nil {
				_ = read.Rollback()
				return nil, docErrorFrom(err)
			}
			// The deliverable check compares projects, nothing more.
			doc, err := docs.MetaByID(read, version.Document)
			_ = read.Rollback()
			if err != nil {
				return nil, docErrorFrom(err)
			}
			if doc.Project != issue.Project {
				return nil, &apiError{status: http.StatusBadRequest, code: "bad-request", message: "doc deliverable must belong to the issue's project"}
			}
			d := review.Deliverable{DocVersion: req.DocVersion, Session: req.Session}
			// No content travels — see the creation path.
			return preparedResubmit{req: req, d: d}, nil
		}
		project, err := projects.GetTx(read, issue.Project)
		_ = read.Rollback()
		if err != nil {
			return nil, errorFrom(err)
		}
		repo := repoFacts(project)
		d, base, content, apiErr := resolveDeliverable(repo, req.deliverableFields, req.ExpectedBaseCommit, req.ExpectedDefaultHead)
		if apiErr != nil {
			return nil, apiErr
		}
		if err := review.RevalidateFences(repo, *d.Commit, review.Fences{
			ExpectedBaseCommit: req.ExpectedBaseCommit, ExpectedDefaultHead: req.ExpectedDefaultHead,
		}); err != nil {
			return nil, reviewErrorFrom(err)
		}
		return preparedResubmit{req: req, d: d, base: base, content: content, repo: repo}, nil
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
		content := &p.content
		if p.d.DocVersion != nil {
			content = nil
		}
		updated, err := review.Resubmit(tx, id, *p.req.ExpectedRevision, p.req.ExpectedVerdictEvent, p.d, p.req.Summary, p.base, content)
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
