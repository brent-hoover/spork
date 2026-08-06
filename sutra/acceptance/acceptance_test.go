package acceptance_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/cucumber/godog"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	_ "modernc.org/sqlite"

	"sutra/internal/api"
)

// implementedFeatures lists the verification files whose steps are fully
// implemented. It grows as modules land; the suite runs in strict mode,
// so a listed feature with pending or undefined steps fails the build.
var implementedFeatures = []string{
	"../verification/REQ-identities.feature:4",             // identity is just a named kind
	"../verification/REQ-notifications.feature:5",          // cursor polling resumes losslessly
	"../verification/REQ-notifications.feature:12",         // filters narrow the feed
	"../verification/REQ-notifications.feature:27",         // draining to a watermark is bounded and provable
	"../verification/REQ-core-issues.feature:3",            // create mints uuid and display number
	"../verification/REQ-core-issues.feature:11",           // updates persist
	"../verification/REQ-status-workflow.feature:3",        // free transitions are recorded
	"../verification/REQ-status-workflow.feature:45",       // unknown statuses are rejected
	"../verification/REQ-issue-hierarchy.feature:3",        // parent and child see each other
	"../verification/REQ-issue-blocking.feature:3",         // both sides see the block
	"../verification/REQ-issue-blocking.feature:19",        // cycles are rejected
	"../verification/REQ-audit.feature:5",                  // every mutation is recorded
	"../verification/REQ-audit.feature:16",                 // events are append-only
	"../verification/REQ-close-requires-review.feature:5",  // approved review allows close
	"../verification/REQ-close-requires-review.feature:13", // a stale revision cannot authorize a close
	"../verification/REQ-close-requires-review.feature:19", // a reversed approval cannot authorize a close
	"../verification/REQ-close-requires-review.feature:24", // a merge-consumed approval still closes its issue
	"../verification/REQ-close-requires-review.feature:29", // a spent review cannot close a reopened issue
	"../verification/REQ-close-requires-review.feature:37", // naming the wrong review cannot close
	"../verification/REQ-close-requires-review.feature:44", // no approval no close
	"../verification/REQ-status-workflow.feature:9",        // complete can reopen
	"../verification/REQ-status-workflow.feature:16",       // conditional transitions guard racing writers
	"../verification/REQ-issue-hierarchy.feature:16",       // open children hold the parent open
	"../verification/REQ-issue-hierarchy.feature:27",       // subtree revision fences history, not just state
	"../verification/REQ-issue-hierarchy.feature:39",       // detaching a child cannot leave a stale close fence
	"../verification/REQ-issue-hierarchy.feature:46",       // reopening a child reopens a complete parent
	"../verification/REQ-issue-hierarchy.feature:52",       // a nested reopen cascades to every complete ancestor
	"../verification/REQ-issue-hierarchy.feature:58",       // a deferred child activating reopens its complete parent
	"../verification/REQ-issue-hierarchy.feature:63",       // a child becoming blocked reopens its complete parent
	"../verification/REQ-issue-hierarchy.feature:68",       // a blocked descendant holds the parent open
	"../verification/REQ-issue-hierarchy.feature:73",       // attaching an open child reopens a complete parent
	"../verification/REQ-issue-hierarchy.feature:78",       // an attached deferred subtree carrying active work reopens the parent
	"../verification/REQ-issue-hierarchy.feature:84",       // relation conflicts carry their distinct codes
	"../verification/REQ-agent-queue.feature:5",            // pop claims the issue
	"../verification/REQ-agent-queue.feature:12",           // concurrent pops never collide
	"../verification/REQ-agent-queue.feature:17",           // oldest issue comes first
	"../verification/REQ-agent-queue.feature:22",           // blocker is worked first
	"../verification/REQ-agent-queue.feature:28",           // externally blocked issues are skipped
	"../verification/REQ-agent-queue.feature:35",           // non-open statuses are never handed out
	"../verification/REQ-agent-queue.feature:45",           // empty stack is not an error
	"../verification/REQ-agent-queue.feature:50",           // archived project issues are never handed out
	"../verification/REQ-agent-queue.feature:57",           // same-key replay claims nothing new
	"../verification/REQ-status-workflow.feature:22",       // pop wins the race over a conditional defer
	"../verification/REQ-status-workflow.feature:29",       // conditional defer wins the race over a pop
	"../verification/REQ-doc-database.feature:4",           // doc files under its project
	"../verification/REQ-doc-database.feature:12",          // saves append immutable versions
	"../verification/REQ-doc-database.feature:18",          // latest by default
	"../verification/REQ-doc-database.feature:23",          // history lists and diffs versions
	"../verification/REQ-doc-templates.feature:3",          // templates are managed by name
	"../verification/REQ-doc-templates.feature:10",         // template seeds the first version
	"../verification/REQ-doc-issue-links.feature:3",        // link and unlink after creation
	"../verification/REQ-doc-issue-links.feature:12",       // both sides see the link
	"../verification/REQ-close-requires-review.feature:50", // doc deliverables gate like code
	"../verification/REQ-thread-catalog.feature:4",         // import preserves the transcript
	"../verification/REQ-thread-catalog.feature:15",        // thread content is searchable
	"../verification/REQ-thread-links.feature:3",           // threads anchor to their work
	"../verification/REQ-comments.feature:3",               // comment lands on the issue
	"../verification/REQ-comments.feature:8",               // replies nest without depth limit
	"../verification/REQ-comments.feature:13",              // no artificial caps
	"../verification/REQ-core-issues.feature:17",           // labels attach and detach
	"../verification/REQ-search.feature:3",                 // text search over issues
	"../verification/REQ-search.feature:8",                 // filters compose
	"../verification/REQ-search.feature:14",                // one surface over all content
	"../verification/REQ-search.feature:19",                // session id joins an instance's work
	"../verification/REQ-import-export.feature:3",          // export captures the whole project
	"../verification/REQ-import-export.feature:8",          // import round-trips losslessly
	"../verification/REQ-import-export.feature:15",         // a half-consumed review is rejected at import
	"../verification/REQ-import-export.feature:20",         // a malformed consumed review is rejected at import
	"../verification/REQ-import-export.feature:28",         // a verdict-bearing review missing its latest verdict event
	"../verification/REQ-import-export.feature:36",         // a malformed close-used review is rejected at import
	"../verification/REQ-import-export.feature:44",         // an invariant-violating hierarchy is rejected at import
	"../verification/REQ-import-export.feature:50",         // colliding import is rejected whole
	"../verification/REQ-import-export.feature:56",         // unknown import actor is rejected
	"../verification/REQ-projects.feature:16",              // content scopes to its project
	"../verification/REQ-projects.feature:21",              // archive hides without deleting
	"../verification/REQ-identities.feature:9",             // unknown identity ids are rejected
	"../verification/REQ-identities.feature:15",            // uniqueness collisions name their colliding resource
	"../verification/REQ-identities.feature:27",            // renaming a template into an existing name collides
	"../verification/REQ-audit.feature:10",                 // history reads back in order
	"../verification/REQ-core-issues.feature:23",           // assignment to any identity
	"../verification/REQ-issue-blocking.feature:13",        // blocker completion unblocks
	"../verification/REQ-notifications.feature:17",         // watermarks anchor reads to the feed
	"../verification/REQ-status-workflow.feature:36",       // simultaneous writers mutate exactly once
	"../verification/REQ-code-review.feature:6",            // agent submits a review
	"../verification/REQ-code-review.feature:12",           // reviewer sees the deliverable
	"../verification/REQ-code-review.feature:18",           // pinned base survives default branch movement
	"../verification/REQ-code-review.feature:24",           // feedback threads on the review
	"../verification/REQ-code-review.feature:30",           // verdict changes state and is recorded
	"../verification/REQ-code-review.feature:36",           // verdicts can be revised for the current revision
	"../verification/REQ-code-review.feature:45",           // approval publishes an event
	"../verification/REQ-code-review.feature:50",           // rework routes back with session context
	"../verification/REQ-code-review.feature:72",           // stale feedback is rejected
	"../verification/REQ-code-review.feature:79",           // stale comments are rejected
	"../verification/REQ-code-review.feature:86",           // approval consumption fences reversal
	"../verification/REQ-code-review.feature:100",          // a verdict ABA fences delayed operations
	"../verification/REQ-code-review.feature:105",          // delayed consumption naming a superseded approval
	"../verification/REQ-code-review.feature:110",          // delayed close naming a superseded approval
	"../verification/REQ-code-review.feature:115",          // a stale resubmission cannot land on a newer revision
	"../verification/REQ-code-review.feature:120",          // resubmission requires changes-requested
	"../verification/REQ-code-review.feature:130",          // both fences guard creation and resubmission
	"../verification/REQ-code-review.feature:143",          // stale expected base rejects before the review exists
	"../verification/REQ-code-review.feature:149",          // moved default head rejects despite unchanged merge base
	"../verification/REQ-code-review.feature:155",          // resubmission enforces the same base and head fences
	"../verification/REQ-code-review.feature:165",          // unresolvable repository rejects submission
}

var contractRouter routers.Router

func TestMain(m *testing.M) {
	if len(implementedFeatures) > 0 {
		loader := openapi3.NewLoader()
		doc, err := loader.LoadFromFile("../contracts/sutra.openapi.yaml")
		if err != nil {
			fmt.Fprintf(os.Stderr, "load contract: %v\n", err)
			os.Exit(1)
		}
		if err := doc.Validate(loader.Context); err != nil {
			fmt.Fprintf(os.Stderr, "validate contract: %v\n", err)
			os.Exit(1)
		}
		contractRouter, err = gorillamux.NewRouter(doc)
		if err != nil {
			fmt.Fprintf(os.Stderr, "contract router: %v\n", err)
			os.Exit(1)
		}

		status := godog.TestSuite{
			Name:                "sutra acceptance",
			ScenarioInitializer: InitializeScenario,
			Options: &godog.Options{
				Format:   "progress",
				Paths:    implementedFeatures,
				Strict:   true,
				TestingT: nil,
			},
		}.Run()
		if status != 0 {
			os.Exit(status)
		}
	}
	os.Exit(m.Run())
}

// testState is the per-scenario world: a fresh in-memory store, the API
// served over httptest, and the last exchange for assertions.
type testState struct {
	db       *sql.DB
	server   *httptest.Server
	lastReq  *http.Request
	lastResp *http.Response
	lastBody []byte
}

func (s *testState) reset() error {
	s.close()
	db, err := sql.Open("sqlite", "file::memory:?cache=shared&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	handler, err := api.New(db)
	if err != nil {
		return fmt.Errorf("wire api: %w", err)
	}
	s.db = db
	s.server = httptest.NewServer(handler)
	return nil
}

func (s *testState) close() {
	if s.server != nil {
		s.server.Close()
		s.server = nil
	}
	if s.db != nil {
		_ = s.db.Close()
		s.db = nil
	}
}

// call performs an API request and validates the response against the
// pinned OpenAPI contract before returning it. Every step that touches
// the API goes through here, so contract conformance is asserted on the
// entire acceptance surface, not in a separate suite.
func (s *testState) call(method, path string, body any) error {
	var payload io.Reader
	var raw []byte
	if body != nil {
		if rm, ok := body.(json.RawMessage); ok {
			// Pre-built bodies carry deliberate formatting (verbatim
			// transcript fixtures) that json.Marshal would compact.
			raw = rm
		} else {
			var err error
			raw, err = json.Marshal(body)
			if err != nil {
				return fmt.Errorf("marshal body: %w", err)
			}
		}
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, s.server.URL+path, payload)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet {
		req.Header.Set("Idempotency-Key", newIdempotencyKey())
	}
	resp, err := s.server.Client().Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	s.lastReq = req
	s.lastResp = resp
	s.lastBody = respBody
	return s.validateAgainstContract(req, raw, resp, respBody)
}

func (s *testState) validateAgainstContract(req *http.Request, reqBody []byte, resp *http.Response, respBody []byte) error {
	route, pathParams, err := contractRouter.FindRoute(req)
	if err != nil {
		return fmt.Errorf("no contract route for %s %s: %w", req.Method, req.URL.Path, err)
	}
	reqInput := &openapi3filter.RequestValidationInput{
		Request:    req,
		PathParams: pathParams,
		Route:      route,
	}
	if reqBody != nil {
		reqInput.Request.Body = io.NopCloser(bytes.NewReader(reqBody))
	}
	respInput := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: reqInput,
		Status:                 resp.StatusCode,
		Header:                 resp.Header,
	}
	respInput.SetBodyBytes(respBody)
	if err := openapi3filter.ValidateResponse(context.Background(), respInput); err != nil {
		return fmt.Errorf("response for %s %s violates contract: %w", req.Method, req.URL.Path, err)
	}
	return nil
}

// callKeyed performs a mutation under an EXPLICIT idempotency key and
// returns the raw exchange — for replay scenarios.
func (s *testState) callKeyed(method, path string, body any, key string) (int, string, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, "", err
	}
	req, err := http.NewRequest(method, s.server.URL+path, bytes.NewReader(raw))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	resp, err := s.server.Client().Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, "", err
	}
	return resp.StatusCode, string(out), nil
}

// expectStatus asserts the last response's status code.
func (s *testState) expectStatus(want int) error {
	if s.lastResp.StatusCode != want {
		return fmt.Errorf("%s %s: expected status %d, got %d — body %s",
			s.lastReq.Method, s.lastReq.URL.Path, want, s.lastResp.StatusCode, s.lastBody)
	}
	return nil
}

// expectErrorCode asserts the last response is an Error envelope with
// the given code, and that a unique-violation names its collisions.
func (s *testState) expectErrorCode(code string) error {
	var envelope struct {
		Code      string `json:"code"`
		Conflicts []any  `json:"conflicts"`
	}
	if err := json.Unmarshal(s.lastBody, &envelope); err != nil {
		return fmt.Errorf("decode error envelope: %w — body %s", err, s.lastBody)
	}
	if envelope.Code != code {
		return fmt.Errorf("expected error code %q, got %q — body %s", code, envelope.Code, s.lastBody)
	}
	if code == "unique-violation" && len(envelope.Conflicts) == 0 {
		return fmt.Errorf("unique-violation must name conflicts — body %s", s.lastBody)
	}
	return nil
}

func InitializeScenario(sc *godog.ScenarioContext) {
	s := &testState{}
	sc.Before(func(ctx context.Context, _ *godog.Scenario) (context.Context, error) {
		return ctx, s.reset()
	})
	sc.After(func(ctx context.Context, _ *godog.Scenario, _ error) (context.Context, error) {
		s.close()
		return ctx, nil
	})
	registerIdentitySteps(sc, s)
	registerFeedSteps(sc, s)
	iw := registerIssueSteps(sc, s)
	cw := registerCloseSteps(sc, iw)
	registerHierarchySteps(sc, cw)
	registerQueueSteps(sc, cw)
	registerDocsSteps(sc, cw)
	registerThreadsSteps(sc, cw)
	registerCommentsSteps(sc, iw)
	registerSearchSteps(sc, cw)
	registerImportExportSteps(sc, cw)
	registerMiscSteps(sc, cw)
	rw := registerReviewSteps(sc, cw)
	registerReviewFenceSteps(sc, cw, rw)
}

// newIdempotencyKey returns a fresh random key for a mutating call.
func newIdempotencyKey() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return fmt.Sprintf("test-%x", b)
}

// registerIdentitySteps covers REQ-identities scenarios.
func registerIdentitySteps(sc *godog.ScenarioContext, s *testState) {
	sc.Step(`^identity "([^"]*)" is created with kind "([^"]*)"$`, func(handle, kind string) error {
		if err := s.call(http.MethodPost, "/identities", map[string]string{"handle": handle, "kind": kind}); err != nil {
			return err
		}
		return s.expectStatus(http.StatusCreated)
	})
	sc.Step(`^it exists with handle "([^"]*)" and kind "([^"]*)"$`, func(handle, kind string) error {
		if err := s.call(http.MethodGet, "/identities", nil); err != nil {
			return err
		}
		var list []map[string]any
		if err := json.Unmarshal(s.lastBody, &list); err != nil {
			return fmt.Errorf("decode identities: %w", err)
		}
		for _, i := range list {
			if i["handle"] == handle && i["kind"] == kind {
				return nil
			}
		}
		return fmt.Errorf("no identity with handle %q and kind %q in %s", handle, kind, s.lastBody)
	})
	sc.Step(`^creating another "([^"]*)" is rejected as a duplicate$`, func(handle string) error {
		if err := s.call(http.MethodPost, "/identities", map[string]string{"handle": handle, "kind": "agent"}); err != nil {
			return err
		}
		if err := s.expectStatus(http.StatusConflict); err != nil {
			return err
		}
		return s.expectErrorCode("unique-violation")
	})
}
