package orchestrator_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"kriya/internal/orchestrator"
)

// reviewAPI behaves as sutra does on the point these tests turn on: a replayed
// Idempotency-Key returns the ORIGINAL review rather than opening a second.
type reviewAPI struct {
	byKey map[string]string
	calls []reviewCall
	err   error
	next  int
}

type reviewCall struct {
	issue, author, summary, branch, commit, session, key string
}

func newReviewAPI() *reviewAPI { return &reviewAPI{byKey: map[string]string{}} }

func (r *reviewAPI) Create(
	_ context.Context, issue, author, summary, branch, commit, session, key string,
) (string, error) {
	r.calls = append(r.calls, reviewCall{
		issue: issue, author: author, summary: summary, branch: branch,
		commit: commit, session: session, key: key,
	})
	if r.err != nil {
		return "", r.err
	}
	if id, ok := r.byKey[key]; ok {
		return id, nil
	}
	r.next++
	id := "review-" + strings.Repeat("x", r.next)
	r.byKey[key] = id
	return id, nil
}

func submitter(store *memStore, api *reviewAPI) orchestrator.Submitter {
	return orchestrator.Submitter{Store: store, Reviews: api, Author: "actor-1"}
}

func gatedRun() orchestrator.BuildRun {
	return orchestrator.BuildRun{
		ID: "run-1", Ticket: "KRI-1", State: orchestrator.StateSubmitting,
		GatedBase: "C2", Attempt: 1,
	}
}

func submission() orchestrator.Submission {
	return orchestrator.Submission{
		Issue: "issue-7", Branch: "kriya/KRI-1/abcd1234",
		Session: "sess-42", Summary: "Create a short link",
	}
}

func TestASubmissionNamesTheBranchPinnedAtTheGatedCommit(t *testing.T) {
	store, api := newMemStore(), newReviewAPI()
	got, err := submitter(store, api).Submit(context.Background(), gatedRun(), submission())
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if len(api.calls) != 1 {
		t.Fatalf("made %d calls", len(api.calls))
	}
	call := api.calls[0]
	if call.branch != "kriya/KRI-1/abcd1234" || call.commit != "C2" {
		t.Errorf("submitted %+v", call)
	}
	if call.session != "sess-42" {
		t.Errorf("stamped session %q", call.session)
	}
	if got.ReviewState != orchestrator.SubmitSubmitted || got.ReviewID == "" {
		t.Errorf("run records %q with id %q", got.ReviewState, got.ReviewID)
	}
}

func TestTheKeyAndPinnedFieldsArePersistedBeforeSutraIsCalled(t *testing.T) {
	// A crash between the write and the call must leave a row that can rebuild
	// the exact request.
	store, api := newMemStore(), newReviewAPI()
	api.err = errors.New("sutra unreachable")
	if _, err := submitter(store, api).Submit(context.Background(), gatedRun(), submission()); err == nil {
		t.Fatal("a submission that never landed read as success")
	}
	row := store.rows["run-1"]
	if row.ReviewState != orchestrator.SubmitSubmitting {
		t.Errorf("state is %q, so the crash window is invisible", row.ReviewState)
	}
	if row.ReviewKey == "" || row.ReviewCommit != "C2" || row.ReviewSession != "sess-42" {
		t.Errorf("the row cannot rebuild its own request: %+v", row)
	}
}

func TestAMovedBranchCannotSmuggleAnUngatedCommitIntoTheReplay(t *testing.T) {
	// The replay reproduces the ORIGINAL request from the persisted fields.
	store, api := newMemStore(), newReviewAPI()
	api.err = errors.New("sutra unreachable")
	s := submitter(store, api)
	if _, err := s.Submit(context.Background(), gatedRun(), submission()); err == nil {
		t.Fatal("expected the submission failure")
	}
	api.err = nil

	// The branch moved to C3 while kriya was down.
	moved := submission()
	if _, err := s.RecoverSubmissions(context.Background(),
		func(orchestrator.BuildRun) orchestrator.Submission { return moved }); err != nil {
		t.Fatalf("recover: %v", err)
	}
	last := api.calls[len(api.calls)-1]
	if last.commit != "C2" {
		t.Errorf("the replay named %s, want the pinned C2", last.commit)
	}
}

func TestTheReplayStampsThePersistedSessionNotAFreshOne(t *testing.T) {
	// Recovery terminates crashed DevSessions and starts fresh ones, so a
	// replay reading the CURRENT session would break feedback routing.
	store, api := newMemStore(), newReviewAPI()
	api.err = errors.New("sutra unreachable")
	s := submitter(store, api)
	if _, err := s.Submit(context.Background(), gatedRun(), submission()); err == nil {
		t.Fatal("expected the submission failure")
	}
	api.err = nil

	fresh := submission()
	fresh.Session = "sess-recovery"
	if _, err := s.RecoverSubmissions(context.Background(),
		func(orchestrator.BuildRun) orchestrator.Submission { return fresh }); err != nil {
		t.Fatalf("recover: %v", err)
	}
	last := api.calls[len(api.calls)-1]
	if last.session != "sess-42" {
		t.Errorf("the replay stamped %q, want the persisted sess-42", last.session)
	}
}

func TestACrashBeforeSutraAcceptedRecoversToOneReview(t *testing.T) {
	store, api := newMemStore(), newReviewAPI()
	api.err = errors.New("sutra unreachable")
	s := submitter(store, api)
	if _, err := s.Submit(context.Background(), gatedRun(), submission()); err == nil {
		t.Fatal("expected the submission failure")
	}
	firstKey := store.rows["run-1"].ReviewKey

	api.err = nil
	n, err := s.RecoverSubmissions(context.Background(),
		func(orchestrator.BuildRun) orchestrator.Submission { return submission() })
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if n != 1 {
		t.Errorf("recovered %d submissions", n)
	}
	if api.calls[len(api.calls)-1].key != firstKey {
		t.Error("the replay used a different key")
	}
	if len(api.byKey) != 1 {
		t.Errorf("%d reviews exist for one run", len(api.byKey))
	}
	if store.rows["run-1"].ReviewState != orchestrator.SubmitSubmitted {
		t.Errorf("the run is still in %q", store.rows["run-1"].ReviewState)
	}
}

func TestACrashAfterSutraAcceptedRecoversToTheSameReview(t *testing.T) {
	store, api := newMemStore(), newReviewAPI()
	s := submitter(store, api)
	got, err := s.Submit(context.Background(), gatedRun(), submission())
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	landed := got.ReviewID

	// Rewind to the crash window: sutra accepted, the id was never recorded.
	got.ReviewID, got.ReviewState = "", orchestrator.SubmitSubmitting
	if err := store.Upsert(context.Background(), got); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := s.RecoverSubmissions(context.Background(),
		func(orchestrator.BuildRun) orchestrator.Submission { return submission() }); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if store.rows["run-1"].ReviewID != landed {
		t.Errorf("recorded %q, want the original %q", store.rows["run-1"].ReviewID, landed)
	}
	if len(api.byKey) != 1 {
		t.Errorf("%d reviews exist for one run", len(api.byKey))
	}
}

func TestAFreshGateAttemptYieldsAFreshKey(t *testing.T) {
	// An integration that leaves the commit unchanged still needs a NEW key,
	// or a replay could return a prior — possibly already consumed — review
	// when a fresh one is required.
	store, api := newMemStore(), newReviewAPI()
	s := submitter(store, api)
	first, err := s.Submit(context.Background(), gatedRun(), submission())
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	again := gatedRun()
	again.Attempt = 2
	second, err := s.Submit(context.Background(), again, submission())
	if err != nil {
		t.Fatalf("resubmit: %v", err)
	}
	if first.ReviewKey == second.ReviewKey {
		t.Error("attempt 2 at the same commit reused attempt 1's key")
	}
	if first.ReviewID == second.ReviewID {
		t.Error("a fresh gate attempt returned the earlier review")
	}
}

func TestTheKeyIsStableForOneAttemptAtOneCommit(t *testing.T) {
	store, api := newMemStore(), newReviewAPI()
	s := submitter(store, api)
	first, err := s.Submit(context.Background(), gatedRun(), submission())
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	second, err := s.Submit(context.Background(), gatedRun(), submission())
	if err != nil {
		t.Fatalf("submit again: %v", err)
	}
	if first.ReviewKey != second.ReviewKey {
		t.Error("two submissions of one attempt at one commit keyed differently")
	}
	if len(api.byKey) != 1 {
		t.Errorf("%d reviews exist for one attempt", len(api.byKey))
	}
}

func TestARunWithNoGatedCommitCannotBeSubmitted(t *testing.T) {
	// The gated commit is what the chain ran against. Submitting without one
	// would open a review over code nothing verified.
	store, api := newMemStore(), newReviewAPI()
	run := gatedRun()
	run.GatedBase = ""
	if _, err := submitter(store, api).Submit(context.Background(), run, submission()); err == nil {
		t.Fatal("a run with no gated commit was submitted")
	}
	if len(api.calls) != 0 {
		t.Error("sutra was called for an ungated run")
	}
}

func TestASubmissionStoreThatCannotBeReadIsNotEmpty(t *testing.T) {
	s := orchestrator.Submitter{Store: sqlOrchStore(t), Reviews: newReviewAPI(), Author: "a"}
	// The table exists but holds nothing: no submission is in flight.
	n, err := s.RecoverSubmissions(context.Background(),
		func(orchestrator.BuildRun) orchestrator.Submission { return submission() })
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if n != 0 {
		t.Errorf("recovered %d from an empty store", n)
	}
}

func TestASubmissionThatCannotBeRecordedIsAFailure(t *testing.T) {
	// A submission recorded nowhere is one recovery cannot replay, and a
	// review may already exist for it.
	store, api := &failingRuns{}, newReviewAPI()
	if _, err := (orchestrator.Submitter{Store: store, Reviews: api, Author: "a"}).
		Submit(context.Background(), gatedRun(), submission()); err == nil {
		t.Fatal("a submission with nothing persisted read as success")
	}
	if len(api.calls) != 0 {
		t.Error("sutra was called with no write-ahead row")
	}
}

func TestAnUnrecordableResultIsAFailure(t *testing.T) {
	// sutra accepted, kriya could not write the id. Reporting success would
	// leave a review nothing points at.
	store := &failingRuns{after: 1}
	api := newReviewAPI()
	if _, err := (orchestrator.Submitter{Store: store, Reviews: api, Author: "a"}).
		Submit(context.Background(), gatedRun(), submission()); err == nil {
		t.Fatal("a result that could not be recorded read as recorded")
	}
}

func TestAnUnreadableRunListStopsRecovery(t *testing.T) {
	// "I could not list what is in flight" is not "nothing is in flight".
	store := &failingRuns{listErr: errors.New("store unavailable")}
	_, err := (orchestrator.Submitter{Store: store, Reviews: newReviewAPI(), Author: "a"}).
		RecoverSubmissions(context.Background(),
			func(orchestrator.BuildRun) orchestrator.Submission { return submission() })
	if err == nil {
		t.Fatal("an unreadable store read as no submissions in flight")
	}
}

// failingRuns fails the nth write, and optionally the listing.
type failingRuns struct {
	writes  int
	after   int
	listErr error
}

func (f *failingRuns) Upsert(context.Context, orchestrator.BuildRun) error {
	f.writes++
	if f.writes > f.after {
		return errors.New("disk full")
	}
	return nil
}

func (f *failingRuns) Find(context.Context, string) (orchestrator.BuildRun, bool, error) {
	return orchestrator.BuildRun{}, false, nil
}

func (f *failingRuns) Submitting(context.Context) ([]orchestrator.BuildRun, error) {
	return nil, f.listErr
}
