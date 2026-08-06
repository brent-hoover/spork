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
	"../verification/REQ-identities.feature:4", // identity is just a named kind
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
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal body: %w", err)
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
