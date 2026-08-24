package trackerclient_test

import (
	"context"
	"encoding/json"
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
	// query is the raw query string. Recorded separately because a filter the
	// client fails to send is a request that returns MORE than it asked for,
	// and a path-only capture cannot see it.
	query string
}

func serve(t *testing.T, status int, reply string) (*trackerclient.Client, *capture) {
	t.Helper()
	got := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path, got.query = r.Method, r.URL.Path, r.URL.RawQuery
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

func TestImportThreadOmitsAnAbsentAnchor(t *testing.T) {
	// sutra distinguishes an absent anchor from a blank one, and a blank issue
	// id is not "a thread anchored nowhere".
	c, got := serve(t, http.StatusCreated, `{"id":"t-1"}`)
	transcript := json.RawMessage(`[{"role":"dev","prompt":"go"}]`)
	if _, err := c.ImportThread(context.Background(), "KRI-1", transcript, "", "", "actor-1", "k"); err != nil {
		t.Fatalf("import: %v", err)
	}
	for _, field := range []string{`"session"`, `"issue"`} {
		if strings.Contains(got.body, field) {
			t.Errorf("body %s sent an empty %s", got.body, field)
		}
	}
	if !strings.Contains(got.body, `"transcript"`) {
		t.Errorf("body %s dropped the transcript", got.body)
	}
}

func TestImportThreadSendsTheAnchorWhenThereIsOne(t *testing.T) {
	c, got := serve(t, http.StatusCreated, `{"id":"t-1","session":"sess-42"}`)
	transcript := json.RawMessage(`[]`)
	thread, err := c.ImportThread(context.Background(), "KRI-1", transcript,
		"sess-42", "issue-7", "actor-1", "key-1")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if thread.ID != "t-1" || thread.Session != "sess-42" {
		t.Errorf("decoded %+v", thread)
	}
	for _, want := range []string{`"session":"sess-42"`, `"issue":"issue-7"`} {
		if !strings.Contains(got.body, want) {
			t.Errorf("body %s is missing %s", got.body, want)
		}
	}
	if got.key != "key-1" {
		t.Errorf("Idempotency-Key was %q", got.key)
	}
}

func TestCreateReviewPinsTheBranchAtACommit(t *testing.T) {
	c, got := serve(t, http.StatusCreated, `{"id":"r-1","revision":1}`)
	rv, err := c.CreateReview(context.Background(), "issue-7", "actor-1",
		"Create a short link", "kriya/KRI-1/abcd", "C2", "sess-42", "D1", "D1", "key-1")
	if err != nil {
		t.Fatalf("create review: %v", err)
	}
	if rv.ID != "r-1" || rv.Revision != 1 {
		t.Errorf("decoded %+v", rv)
	}
	for _, want := range []string{`"branch":"kriya/KRI-1/abcd"`, `"commit":"C2"`,
		`"session":"sess-42"`, `"summary":"Create a short link"`,
		`"expected_base_commit":"D1"`, `"expected_default_head":"D1"`} {
		if !strings.Contains(got.body, want) {
			t.Errorf("body %s is missing %s", got.body, want)
		}
	}
	if got.key != "key-1" || got.path != "/reviews" {
		t.Errorf("sent to %s with key %q", got.path, got.key)
	}
}

func TestCreateReviewOmitsAnAbsentSummaryAndSession(t *testing.T) {
	// sutra rejects an explicit null for these and distinguishes absent from
	// blank.
	c, got := serve(t, http.StatusCreated, `{"id":"r-1"}`)
	if _, err := c.CreateReview(context.Background(), "issue-7", "actor-1",
		"", "kriya/KRI-1/abcd", "C2", "", "D1", "D1", "key-1"); err != nil {
		t.Fatalf("create review: %v", err)
	}
	for _, field := range []string{`"summary"`, `"session"`} {
		if strings.Contains(got.body, field) {
			t.Errorf("body %s sent an empty %s", got.body, field)
		}
	}
}

func TestGetReviewReadsTheCurrentState(t *testing.T) {
	c, got := serve(t, http.StatusOK,
		`{"id":"r-1","state":"changes-requested","revision":2,"latest_verdict_event":"event-9"}`)
	rv, err := c.GetReview(context.Background(), "r-1")
	if err != nil {
		t.Fatalf("get review: %v", err)
	}
	if rv.Revision != 2 || rv.LatestVerdictEvent != "event-9" {
		t.Errorf("decoded %+v", rv)
	}
	if got.method != http.MethodGet || got.path != "/reviews/r-1" {
		t.Errorf("sent %s %s", got.method, got.path)
	}
	// A read is not a mutation: claiming otherwise would make sutra settle it
	// under a key and replay a stale body.
	if got.key != "" || got.ctype != "" {
		t.Errorf("a read sent key %q and Content-Type %q", got.key, got.ctype)
	}
}

func TestGetReviewSurfacesANotFound(t *testing.T) {
	c, _ := serve(t, http.StatusNotFound, `{"detail":"no such review"}`)
	_, err := c.GetReview(context.Background(), "r-absent")
	var apiErr *trackerclient.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		t.Fatalf("got %v, want a 404 APIError", err)
	}
}

func TestGetReviewFailsOnAnUnparseableBody(t *testing.T) {
	c, _ := serve(t, http.StatusOK, "not json")
	if _, err := c.GetReview(context.Background(), "r-1"); err == nil {
		t.Fatal("a 2xx with an unparseable body read as a review")
	}
}

func TestAnUnreachableTrackerFailsAReview(t *testing.T) {
	c := trackerclient.New("http://127.0.0.1:1")
	if _, err := c.GetReview(context.Background(), "r-1"); err == nil {
		t.Fatal("an unreachable tracker must fail loudly")
	}
}

func TestAnInvalidBaseURLFailsAReviewRead(t *testing.T) {
	c := trackerclient.New("://nonsense")
	if _, err := c.GetReview(context.Background(), "r-1"); err == nil {
		t.Fatal("an unbuildable request must fail")
	}
}

func TestResubmitCarriesItsFences(t *testing.T) {
	// sutra refuses the call if the review has moved on, which is what stops a
	// replay advancing a revision twice.
	c, got := serve(t, http.StatusOK, `{"id":"r-1","revision":3}`)
	rv, err := c.ResubmitReview(context.Background(), "r-1", "actor-1", "summary",
		"kriya/KRI-1/abcd", "C3", "sess-42", 2, "event-9", "D1", "D1", "key-1")
	if err != nil {
		t.Fatalf("resubmit: %v", err)
	}
	if rv.Revision != 3 {
		t.Errorf("decoded revision %d", rv.Revision)
	}
	for _, want := range []string{`"expected_revision":2`, `"expected_verdict_event":"event-9"`,
		`"commit":"C3"`, `"branch":"kriya/KRI-1/abcd"`,
		`"expected_base_commit":"D1"`, `"expected_default_head":"D1"`} {
		if !strings.Contains(got.body, want) {
			t.Errorf("body %s is missing %s", got.body, want)
		}
	}
	if got.path != "/reviews/r-1/resubmit" || got.key != "key-1" {
		t.Errorf("sent to %s with key %q", got.path, got.key)
	}
}

func TestResubmitOmitsAnAbsentSummaryAndSession(t *testing.T) {
	c, got := serve(t, http.StatusOK, `{"id":"r-1","revision":3}`)
	if _, err := c.ResubmitReview(context.Background(), "r-1", "actor-1", "",
		"kriya/KRI-1/abcd", "C3", "", 2, "event-9", "D1", "D1", "key-1"); err != nil {
		t.Fatalf("resubmit: %v", err)
	}
	for _, field := range []string{`"summary"`, `"session"`} {
		if strings.Contains(got.body, field) {
			t.Errorf("body %s sent an empty %s", got.body, field)
		}
	}
}

func TestAReviewReadAcceptsExactlyTheTwoHundreds(t *testing.T) {
	// 200 must succeed and 300 must not: a redirect kriya cannot follow is not
	// a review it can read.
	c, _ := serve(t, http.StatusOK, `{"id":"r-1"}`)
	if _, err := c.GetReview(context.Background(), "r-1"); err != nil {
		t.Errorf("200 was rejected: %v", err)
	}
	c, _ = serve(t, http.StatusMultipleChoices, `{"id":"r-1"}`)
	if _, err := c.GetReview(context.Background(), "r-1"); err == nil {
		t.Error("300 read as a review")
	}
}

func TestTheEventFeedIsReadFromACursor(t *testing.T) {
	// The cursor is what makes the feed resumable: a restart picks up exactly
	// where it left off rather than re-reading or skipping.
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		if r.Header.Get("Idempotency-Key") != "" {
			t.Error("a read sent an Idempotency-Key")
		}
		_, _ = io.WriteString(w,
			`{"events":[{"id":"e-1","kind":"review.verdict","payload":{"verdict":"approved"}}],`+
				`"next_cursor":"c-2","drained":true}`)
	}))
	defer srv.Close()

	page, err := trackerclient.New(srv.URL).
		Events(context.Background(), "c-1", "review.verdict", 50)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(page.Events) != 1 || page.Events[0].ID != "e-1" {
		t.Errorf("decoded %+v", page.Events)
	}
	if page.NextCursor != "c-2" || !page.Drained {
		t.Errorf("decoded cursor %q drained=%v", page.NextCursor, page.Drained)
	}
	for _, want := range []string{"cursor=c-1", "kind=review.verdict", "limit=50"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query %q is missing %s", gotQuery, want)
		}
	}
}

func TestAFirstReadCarriesNoCursor(t *testing.T) {
	// A consumer that has read nothing is at the START of the feed, and
	// sending an empty cursor parameter would ask sutra to interpret one.
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = io.WriteString(w, `{"events":[],"next_cursor":"c-1","drained":true}`)
	}))
	defer srv.Close()

	if _, err := trackerclient.New(srv.URL).
		Events(context.Background(), "", "", 0); err != nil {
		t.Fatalf("events: %v", err)
	}
	for _, unwanted := range []string{"cursor=", "kind=", "limit="} {
		if strings.Contains(gotQuery, unwanted) {
			t.Errorf("query %q sent an empty %s", gotQuery, unwanted)
		}
	}
}

func TestAFeedFailureSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"detail":"try later"}`)
	}))
	defer srv.Close()

	_, err := trackerclient.New(srv.URL).Events(context.Background(), "", "", 0)
	var apiErr *trackerclient.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusServiceUnavailable {
		t.Fatalf("got %v, want a 503 APIError", err)
	}
}

func TestAnUnparseableFeedIsAnError(t *testing.T) {
	c, _ := serve(t, http.StatusOK, "not json")
	if _, err := c.Events(context.Background(), "", "", 0); err == nil {
		t.Fatal("an unparseable feed read as a page")
	}
}

func TestAnUnreachableFeedFails(t *testing.T) {
	if _, err := trackerclient.New("http://127.0.0.1:1").
		Events(context.Background(), "", "", 0); err == nil {
		t.Fatal("an unreachable tracker must fail loudly")
	}
	if _, err := trackerclient.New("://nonsense").
		Events(context.Background(), "", "", 0); err == nil {
		t.Fatal("an unbuildable request must fail")
	}
}

func TestPopClaimsTheNextTicket(t *testing.T) {
	// feed_watermark is a STRING on the wire. Decoding it as a number failed
	// every pop, which is to say every build.
	c, got := serve(t, http.StatusOK,
		`{"issue":{"id":"i-1","title":"Create a short link"},"feed_watermark":"7"}`)
	popped, err := c.Pop(context.Background(), "actor-1", "key-1")
	if err != nil {
		t.Fatalf("pop: %v", err)
	}
	if popped.Issue.ID != "i-1" || popped.FeedWatermark != "7" {
		t.Errorf("decoded %+v", popped)
	}
	if got.path != "/identities/actor-1/work-stack/pop" || got.key != "key-1" {
		t.Errorf("sent to %s with key %q", got.path, got.key)
	}
}

func TestAnEmptyPopIsNotAnError(t *testing.T) {
	// Idling is the ordinary state of a plan whose remaining tickets are
	// blocked or in flight.
	c, _ := serve(t, http.StatusOK, `{"feed_watermark":"7"}`)
	popped, err := c.Pop(context.Background(), "actor-1", "key-1")
	if err != nil {
		t.Fatalf("an empty work stack was treated as a failure: %v", err)
	}
	if popped.Issue.ID != "" {
		t.Errorf("decoded an issue %q from an empty pop", popped.Issue.ID)
	}
}

func TestCompleteIssueNamesTheApprovingReview(t *testing.T) {
	c, got := serve(t, http.StatusOK, `{}`)
	err := c.CompleteIssue(context.Background(), "i-1", "review-1", 2,
		"event-9", "actor-1", "key-1")
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	for _, want := range []string{`"status":"complete"`, `"review":"review-1"`,
		`"review_revision":2`, `"review_verdict_event":"event-9"`} {
		if !strings.Contains(got.body, want) {
			t.Errorf("body %s is missing %s", got.body, want)
		}
	}
	if got.path != "/issues/i-1/status" || got.key != "key-1" {
		t.Errorf("sent to %s with key %q", got.path, got.key)
	}
}

func TestConsumeApprovalCarriesItsFences(t *testing.T) {
	c, got := serve(t, http.StatusOK, `{"id":"r-1","revision":2}`)
	if _, err := c.ConsumeApproval(context.Background(), "r-1", "actor-1", 2,
		"event-9", "key-1"); err != nil {
		t.Fatalf("consume: %v", err)
	}
	for _, want := range []string{`"expected_revision":2`, `"expected_verdict_event":"event-9"`} {
		if !strings.Contains(got.body, want) {
			t.Errorf("body %s is missing %s", got.body, want)
		}
	}
	if got.path != "/reviews/r-1/consume" {
		t.Errorf("sent to %s", got.path)
	}
}

func TestTheFeedAcceptsExactlyTheTwoHundreds(t *testing.T) {
	// 200 must succeed and 300 must not: a redirect kriya cannot follow is not
	// a page of events.
	c, _ := serve(t, http.StatusOK, `{"events":[],"next_cursor":"c-1"}`)
	if _, err := c.Events(context.Background(), "", "", 0); err != nil {
		t.Errorf("200 was rejected: %v", err)
	}
	c, _ = serve(t, http.StatusMultipleChoices, `{"events":[]}`)
	if _, err := c.Events(context.Background(), "", "", 0); err == nil {
		t.Error("300 read as a page of events")
	}
}

func TestListIssuesDecodesStatusAndTheWatermark(t *testing.T) {
	// STATUS, which is sutra's own field name. It was decoded from "state",
	// which sutra does not emit, so every issue came back with an empty
	// status — invisible until completion detection asked.
	c, got := serve(t, http.StatusOK,
		`{"feed_watermark":"w-7","issues":[
			{"id":"i-1","number":1,"title":"Create a short link","status":"open","subtree_revision":3},
			{"id":"i-2","number":2,"title":"Redirect","status":"complete","subtree_revision":9}]}`)
	listing, err := c.ListIssues(context.Background(), "p-1", []string{"open", "blocked"})
	if err != nil {
		t.Fatalf("list issues: %v", err)
	}
	if listing.Watermark != "w-7" {
		t.Errorf("watermark decoded as %q", listing.Watermark)
	}
	if len(listing.Issues) != 2 {
		t.Fatalf("decoded %d issues", len(listing.Issues))
	}
	if listing.Issues[0].Status != "open" {
		t.Errorf("status decoded as %q", listing.Issues[0].Status)
	}
	if listing.Issues[0].SubtreeRevision != 3 {
		t.Errorf("subtree revision decoded as %d", listing.Issues[0].SubtreeRevision)
	}
	if !strings.HasPrefix(got.path, "/projects/p-1/issues") {
		t.Errorf("sent to %s", got.path)
	}
	// Both statuses travel, so sutra filters rather than kriya paging a
	// long-lived project's whole history to find the active few.
	for _, want := range []string{"status=open", "status=blocked"} {
		if !strings.Contains(got.query, want) {
			t.Errorf("the request query %q omits %s", got.query, want)
		}
	}
	// A READ: no idempotency key.
	if got.key != "" {
		t.Errorf("a read carried idempotency key %q", got.key)
	}
}

func TestAProjectWithNoActiveIssuesIsNotAnError(t *testing.T) {
	c, _ := serve(t, http.StatusOK, `{"feed_watermark":"w-1","issues":[]}`)
	listing, err := c.ListIssues(context.Background(), "p-1", nil)
	if err != nil {
		t.Fatalf("an empty project was treated as a failure: %v", err)
	}
	if len(listing.Issues) != 0 || listing.Watermark != "w-1" {
		t.Errorf("decoded %+v", listing)
	}
}
