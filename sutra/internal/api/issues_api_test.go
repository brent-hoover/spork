package api_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func req(t *testing.T, srv *httptest.Server, method, path, key, body string) (int, string) {
	t.Helper()
	var payload io.Reader
	if body != "" {
		payload = readerOf(body)
	}
	r, err := http.NewRequest(method, srv.URL+path, payload)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	resp, err := srv.Client().Do(r)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return resp.StatusCode, string(raw)
}

func readerOf(s string) io.Reader { return &stringReader{s: s} }

type stringReader struct{ s string }

func (r *stringReader) Read(p []byte) (int, error) {
	if len(r.s) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.s)
	r.s = r.s[n:]
	return n, nil
}

// world builds identity -> project -> issues -> relation through the
// API and returns the ids.
type world struct {
	actor, project string
	issues         []string
	relation       string
}

func buildWorld(t *testing.T, srv *httptest.Server, issueCount int, relate bool) world {
	t.Helper()
	var w world
	var out struct {
		ID       string `json:"id"`
		Relation struct {
			ID string `json:"id"`
		} `json:"relation"`
	}
	mustJSON := func(status int, body string, want int) {
		t.Helper()
		if status != want {
			t.Fatalf("expected %d, got %d: %s", want, status, body)
		}
		out.Relation.ID = ""
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode: %v — %s", err, body)
		}
	}
	status, body := req(t, srv, http.MethodPost, "/identities", "w-id", `{"handle":"op","kind":"human"}`)
	mustJSON(status, body, http.StatusCreated)
	w.actor = out.ID
	status, body = req(t, srv, http.MethodPost, "/projects", "w-proj", fmt.Sprintf(`{"key":"SUT","name":"Sutra","actor":%q}`, w.actor))
	mustJSON(status, body, http.StatusCreated)
	w.project = out.ID
	for i := range issueCount {
		status, body = req(t, srv, http.MethodPost, "/projects/"+w.project+"/issues",
			fmt.Sprintf("w-issue-%d", i), fmt.Sprintf(`{"title":"issue %d","actor":%q}`, i, w.actor))
		mustJSON(status, body, http.StatusCreated)
		w.issues = append(w.issues, out.ID)
	}
	if relate {
		status, body = req(t, srv, http.MethodPost, "/issues/"+w.issues[0]+"/relations", "w-rel",
			fmt.Sprintf(`{"kind":"blocks","to":%q,"actor":%q}`, w.issues[1], w.actor))
		mustJSON(status, body, http.StatusCreated)
		w.relation = out.Relation.ID
	}
	return w
}

// TestArchivedProjectRelationRemovalRejected pins that the archive
// write-guard covers relation deletion: 409, relation intact, no event.
func TestArchivedProjectRelationRemovalRejected(t *testing.T) {
	srv, _ := startAPI(t)
	w := buildWorld(t, srv, 2, true)
	if status, body := req(t, srv, http.MethodPost, "/projects/"+w.project+"/archive", "w-arch",
		fmt.Sprintf(`{"actor":%q}`, w.actor)); status != http.StatusOK {
		t.Fatalf("archive: %d %s", status, body)
	}
	status, body := req(t, srv, http.MethodDelete,
		"/issues/"+w.issues[0]+"/relations/"+w.relation+"?actor="+w.actor, "w-del", "")
	if status != http.StatusConflict {
		t.Fatalf("expected 409 for archived-project removal, got %d %s", status, body)
	}
	status, body = req(t, srv, http.MethodGet, "/issues/"+w.issues[0]+"/relations", "", "")
	if status != http.StatusOK || body == "[]" {
		t.Fatalf("relation gone despite rejection: %d %s", status, body)
	}
	status, body = req(t, srv, http.MethodGet, "/events?kind=issue.relation-removed", "", "")
	var page struct {
		Events []any `json:"events"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil || status != http.StatusOK {
		t.Fatalf("feed: %d %s (%v)", status, body, err)
	}
	if len(page.Events) != 0 {
		t.Fatalf("rejected removal emitted an event: %s", body)
	}
}

// TestTransitionClosedSchemas pins the contract's additionalProperties
// false on both transition variants: unknown fields and missing
// required fields are schema-invalid 400s, with no status change and
// no event.
func TestTransitionClosedSchemas(t *testing.T) {
	srv, _ := startAPI(t)
	w := buildWorld(t, srv, 1, false)
	issue := w.issues[0]

	cases := []struct {
		name string
		body string
	}{
		{"unknown field on non-complete", fmt.Sprintf(`{"status":"in-progress","actor":%q,"extra":1}`, w.actor)},
		{"expected_subtree_revision on non-complete", fmt.Sprintf(`{"status":"in-progress","actor":%q,"expected_subtree_revision":0}`, w.actor)},
		{"complete missing review_revision", fmt.Sprintf(`{"status":"complete","review":"00000000-0000-7000-8000-000000000001","review_verdict_event":"00000000-0000-7000-8000-000000000002","actor":%q}`, w.actor)},
		{"complete missing review entirely", fmt.Sprintf(`{"status":"complete","actor":%q}`, w.actor)},
		{"unknown field on complete", fmt.Sprintf(`{"status":"complete","review":"00000000-0000-7000-8000-000000000001","review_revision":1,"review_verdict_event":"00000000-0000-7000-8000-000000000002","actor":%q,"extra":true}`, w.actor)},
		{"invalid expected_status enum", fmt.Sprintf(`{"status":"in-progress","actor":%q,"expected_status":"invalid"}`, w.actor)},
		{"null review on complete", fmt.Sprintf(`{"status":"complete","review":null,"review_revision":1,"review_verdict_event":"00000000-0000-7000-8000-000000000002","actor":%q}`, w.actor)},
		{"malformed review uuid", fmt.Sprintf(`{"status":"complete","review":"not-a-uuid","review_revision":1,"review_verdict_event":"00000000-0000-7000-8000-000000000002","actor":%q}`, w.actor)},
	}
	for i, tc := range cases {
		status, body := req(t, srv, http.MethodPost, "/issues/"+issue+"/status", fmt.Sprintf("tc-%d", i), tc.body)
		if status != http.StatusBadRequest {
			t.Fatalf("%s: expected 400, got %d %s", tc.name, status, body)
		}
	}

	status, body := req(t, srv, http.MethodGet, "/issues/"+issue, "", "")
	var read struct {
		Status          string `json:"status"`
		SubtreeRevision int64  `json:"subtree_revision"`
	}
	if err := json.Unmarshal([]byte(body), &read); err != nil || status != http.StatusOK {
		t.Fatalf("read: %d %s (%v)", status, body, err)
	}
	if read.Status != "open" || read.SubtreeRevision != 0 {
		t.Fatalf("schema-invalid requests changed state: %+v", read)
	}
	status, body = req(t, srv, http.MethodGet, "/events?kind=issue.status-changed", "", "")
	var page struct {
		Events []any `json:"events"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil || status != http.StatusOK {
		t.Fatalf("feed: %d %s (%v)", status, body, err)
	}
	if len(page.Events) != 0 {
		t.Fatalf("schema-invalid requests emitted events: %s", body)
	}
}

// TestValidCompleteTransitionIs409UntilReviews pins that a
// schema-VALID complete transition fails the ownership gate (no review
// module yet) with a conflict, never a schema 400.
func TestValidCompleteTransitionIs409UntilReviews(t *testing.T) {
	srv, _ := startAPI(t)
	w := buildWorld(t, srv, 1, false)
	body := fmt.Sprintf(`{"status":"complete","review":"00000000-0000-7000-8000-000000000001","review_revision":1,"review_verdict_event":"00000000-0000-7000-8000-000000000002","actor":%q}`, w.actor)
	status, resp := req(t, srv, http.MethodPost, "/issues/"+w.issues[0]+"/status", "close-1", body)
	if status != http.StatusConflict {
		t.Fatalf("expected 409 ownership conflict, got %d %s", status, resp)
	}
}

// TestSubtreeRevisionFence pins the history-aware fence: a schema-valid
// complete with a mismatched expected_subtree_revision conflicts with
// the revision code before any review gate runs; a matching revision
// proceeds to the ownership conflict.
func TestSubtreeRevisionFence(t *testing.T) {
	srv, _ := startAPI(t)
	w := buildWorld(t, srv, 1, false)
	base := `{"status":"complete","review":"00000000-0000-7000-8000-000000000001","review_revision":1,"review_verdict_event":"00000000-0000-7000-8000-000000000002","actor":%q,"expected_subtree_revision":%d}`

	status, body := req(t, srv, http.MethodPost, "/issues/"+w.issues[0]+"/status", "fence-miss",
		fmt.Sprintf(base, w.actor, 99))
	if status != http.StatusConflict {
		t.Fatalf("expected 409, got %d %s", status, body)
	}
	var envelope struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil || envelope.Code != "expected-subtree-revision-mismatch" {
		t.Fatalf("expected expected-subtree-revision-mismatch, got %s", body)
	}

	status, body = req(t, srv, http.MethodPost, "/issues/"+w.issues[0]+"/status", "fence-match",
		fmt.Sprintf(base, w.actor, 0))
	if err := json.Unmarshal([]byte(body), &envelope); err != nil || status != http.StatusConflict || envelope.Code != "missing-approval" {
		t.Fatalf("matching fence must reach the ownership gate: %d %s", status, body)
	}
}

// TestCrossProjectArchiveGuard pins that an archived TARGET project
// rejects relation mutations from a live source project.
func TestCrossProjectArchiveGuard(t *testing.T) {
	srv, _ := startAPI(t)
	w := buildWorld(t, srv, 1, false)
	var out struct {
		ID string `json:"id"`
	}
	status, body := req(t, srv, http.MethodPost, "/projects", "p2",
		fmt.Sprintf(`{"key":"OTH","name":"Other","actor":%q}`, w.actor))
	if err := json.Unmarshal([]byte(body), &out); err != nil || status != http.StatusCreated {
		t.Fatalf("second project: %d %s", status, body)
	}
	otherProject := out.ID
	status, body = req(t, srv, http.MethodPost, "/projects/"+otherProject+"/issues", "p2-issue",
		fmt.Sprintf(`{"title":"other","actor":%q}`, w.actor))
	if err := json.Unmarshal([]byte(body), &out); err != nil || status != http.StatusCreated {
		t.Fatalf("other issue: %d %s", status, body)
	}
	otherIssue := out.ID
	if status, body := req(t, srv, http.MethodPost, "/projects/"+otherProject+"/archive", "p2-arch",
		fmt.Sprintf(`{"actor":%q}`, w.actor)); status != http.StatusOK {
		t.Fatalf("archive: %d %s", status, body)
	}
	status, body = req(t, srv, http.MethodPost, "/issues/"+w.issues[0]+"/relations", "p2-rel",
		fmt.Sprintf(`{"kind":"blocks","to":%q,"actor":%q}`, otherIssue, w.actor))
	if status != http.StatusConflict {
		t.Fatalf("cross-project relation into archived project must 409, got %d %s", status, body)
	}
}
