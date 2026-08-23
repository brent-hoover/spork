package trackerclient_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kriya/internal/trackerclient"
)

// capture records what reached the wire.
//
// The point of these tests is the REQUEST, not the reply: every mutation
// carries an Idempotency-Key the caller persisted before calling, and a
// mutation that quietly dropped it would replay as a second write after a
// crash. Only a real server can see the header.
type capture struct {
	method string
	path   string
	key    string
	body   string
	ctype  string
}

func serve(t *testing.T, status int, reply string) (*trackerclient.Client, *capture) {
	t.Helper()
	got := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path = r.Method, r.URL.Path
		got.key = r.Header.Get("Idempotency-Key")
		got.ctype = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		got.body = string(b)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return trackerclient.New(srv.URL), got
}

func TestCreateIdentitySendsTheKeyAndDecodesTheReply(t *testing.T) {
	c, got := serve(t, http.StatusCreated, `{"id":"id-1","handle":"kriya"}`)
	id, err := c.CreateIdentity(context.Background(), "kriya", "agent", "Kriya", "key-1")
	if err != nil {
		t.Fatalf("create identity: %v", err)
	}
	if id.ID != "id-1" || id.Handle != "kriya" {
		t.Errorf("decoded %+v", id)
	}
	if got.key != "key-1" {
		t.Errorf("Idempotency-Key was %q — a dropped key replays as a second write", got.key)
	}
	if got.ctype != "application/json" {
		t.Errorf("Content-Type was %q", got.ctype)
	}
	if got.path != "/identities" || got.method != http.MethodPost {
		t.Errorf("sent %s %s", got.method, got.path)
	}
}

func TestCreateProjectCarriesTheActor(t *testing.T) {
	c, got := serve(t, http.StatusCreated, `{"id":"p-1","key":"K","name":"n"}`)
	p, err := c.CreateProject(context.Background(), "K", "n", "actor-1", "key-2")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if p.ID != "p-1" {
		t.Errorf("decoded %+v", p)
	}
	if !strings.Contains(got.body, `"actor":"actor-1"`) {
		t.Errorf("body %s omitted the actor", got.body)
	}
}

func TestCreateIssueOmitsAnEmptyBody(t *testing.T) {
	// sutra distinguishes an absent body from an empty one; sending "" would
	// write an empty description over nothing.
	c, got := serve(t, http.StatusCreated, `{"id":"i-1","number":3,"title":"t","state":"open"}`)
	i, err := c.CreateIssue(context.Background(), "p-1", "t", "", "actor-1", "key-3")
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if i.Number != 3 {
		t.Errorf("decoded %+v", i)
	}
	if strings.Contains(got.body, `"body"`) {
		t.Errorf("body %s sent an empty description", got.body)
	}
	if got.path != "/projects/p-1/issues" {
		t.Errorf("posted to %s", got.path)
	}
}

func TestCreateIssueSendsABodyWhenThereIsOne(t *testing.T) {
	c, got := serve(t, http.StatusCreated, `{"id":"i-1"}`)
	if _, err := c.CreateIssue(context.Background(), "p-1", "t", "why", "actor-1", "key-4"); err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if !strings.Contains(got.body, `"body":"why"`) {
		t.Errorf("body %s dropped the description", got.body)
	}
}

func TestAddRelationExpectsNoReplyBody(t *testing.T) {
	c, got := serve(t, http.StatusNoContent, "")
	if err := c.AddRelation(context.Background(), "i-1", "parent_of", "i-2", "actor-1", "key-5"); err != nil {
		t.Fatalf("add relation: %v", err)
	}
	if got.path != "/issues/i-1/relations" {
		t.Errorf("posted to %s", got.path)
	}
}

func TestANonSuccessCarriesItsStatus(t *testing.T) {
	// The difference between 409 and 500 is the difference between "already
	// done" and "try again", so the status must survive the error.
	c, _ := serve(t, http.StatusConflict, `{"detail":"key already exists"}`)
	_, err := c.CreateProject(context.Background(), "K", "n", "actor-1", "key-6")
	var apiErr *trackerclient.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("got %v, want an APIError", err)
	}
	if apiErr.Status != http.StatusConflict {
		t.Errorf("status %d, want 409", apiErr.Status)
	}
	if !strings.Contains(apiErr.Error(), "key already exists") {
		t.Errorf("error %q dropped the tracker's explanation", apiErr.Error())
	}
}

func TestAnUndecodableReplyFails(t *testing.T) {
	c, _ := serve(t, http.StatusOK, "not json")
	if _, err := c.CreateIdentity(context.Background(), "h", "agent", "d", "key-7"); err == nil {
		t.Fatal("a 2xx with an unparseable body must not read as success")
	}
}

func TestAnUnreachableTrackerFails(t *testing.T) {
	c := trackerclient.New("http://127.0.0.1:1")
	if err := c.AddRelation(context.Background(), "i-1", "blocks", "i-2", "a", "key-8"); err == nil {
		t.Fatal("an unreachable tracker must fail loudly")
	}
}

func TestAnInvalidBaseURLFailsBeforeSending(t *testing.T) {
	c := trackerclient.New("://nonsense")
	if _, err := c.CreateIssue(context.Background(), "p", "t", "", "a", "key-9"); err == nil {
		t.Fatal("an unbuildable request must fail")
	}
}

func TestTheClientCannotHangForever(t *testing.T) {
	// An unbounded client hangs a build on a tracker that stops replying, and
	// nothing above it has a deadline of its own.
	if timeout := trackerclient.New("http://example.invalid").HTTP.Timeout; timeout <= 0 {
		t.Errorf("timeout is %s", timeout)
	}
}

func TestTheSuccessRangeIsExactlyTwoHundreds(t *testing.T) {
	// 200 must succeed and 300 must not: a redirect kriya cannot follow is not
	// a mutation that landed.
	c, _ := serve(t, http.StatusOK, `{"id":"id-1"}`)
	if _, err := c.CreateIdentity(context.Background(), "h", "agent", "d", "key-a"); err != nil {
		t.Errorf("200 was rejected: %v", err)
	}
	c, _ = serve(t, http.StatusMultipleChoices, `{"id":"id-1"}`)
	if _, err := c.CreateIdentity(context.Background(), "h", "agent", "d", "key-b"); err == nil {
		t.Error("300 read as success")
	}
}
