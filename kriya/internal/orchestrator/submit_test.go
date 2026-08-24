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
	// revision is the review's current revision, advanced once per new key.
	revision  int
	revisions map[string]int
	// fenceBase and fenceHead are what sutra RESOLVES. When set and different
	// from what a call expects, the call is rejected and nothing is created.
	fenceBase string
	fenceHead string
}

type reviewCall struct {
	issue, author, summary, branch, commit, session, key string
	// The base fences every submission and resubmission carries.
	expectedBase, expectedDefaultHead string
	// Set on a resubmission.
	expectedRevision int
	verdictEvent     string
}

func newReviewAPI() *reviewAPI {
	return &reviewAPI{byKey: map[string]string{}, revisions: map[string]int{}}
}

func (r *reviewAPI) Create(
	_ context.Context, issue, author, summary, branch, commit, session string,
	expectedBase, expectedDefaultHead, key string,
) (string, error) {
	r.calls = append(r.calls, reviewCall{
		issue: issue, author: author, summary: summary, branch: branch,
		commit: commit, session: session, key: key,
		expectedBase: expectedBase, expectedDefaultHead: expectedDefaultHead,
	})
	if r.fenceBase != "" && r.fenceBase != expectedBase {
		// sutra resolving a different actual base: rejected atomically, and
		// nothing is created.
		return "", errors.New("base fence: the branch is not based on " + expectedBase)
	}
	if r.fenceHead != "" && r.fenceHead != expectedDefaultHead {
		return "", errors.New("head fence: the default branch is not at " + expectedDefaultHead)
	}
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

// Resubmit advances a revision once per key, as sutra does.
func (r *reviewAPI) Resubmit(
	_ context.Context, id, author, summary, branch, commit, session string,
	expectedRevision int, verdictEvent, expectedBase, expectedDefaultHead, key string,
) (int, error) {
	r.calls = append(r.calls, reviewCall{
		issue: id, author: author, summary: summary, branch: branch,
		commit: commit, session: session, key: key,
		expectedBase: expectedBase, expectedDefaultHead: expectedDefaultHead,
		expectedRevision: expectedRevision, verdictEvent: verdictEvent,
	})
	if r.fenceBase != "" && r.fenceBase != expectedBase {
		return 0, errors.New("base fence: the branch is not based on " + expectedBase)
	}
	if r.fenceHead != "" && r.fenceHead != expectedDefaultHead {
		return 0, errors.New("head fence: the default branch is not at " + expectedDefaultHead)
	}
	if r.err != nil {
		return 0, r.err
	}
	if landed, ok := r.revisions[key]; ok {
		// Replayed key: the original result, and no second advance.
		return landed, nil
	}
	if r.revision != expectedRevision {
		return 0, errors.New("revision fence: the review has moved on")
	}
	r.revision++
	r.revisions[key] = r.revision
	return r.revision, nil
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

func (f *failingRuns) Resubmitting(context.Context) ([]orchestrator.BuildRun, error) {
	return nil, f.listErr
}

func (f *failingRuns) Completing(context.Context) ([]orchestrator.BuildRun, error) {
	return nil, f.listErr
}

func submittedRun() orchestrator.BuildRun {
	return orchestrator.BuildRun{
		ID: "run-1", Ticket: "KRI-1", State: orchestrator.StateSubmitting,
		GatedBase: "C3", Attempt: 2, ReviewID: "review-x",
		ReviewState: orchestrator.SubmitSubmitted,
	}
}

func rework() orchestrator.Rework {
	return orchestrator.Rework{
		Branch: "kriya/KRI-1/abcd1234", Session: "sess-42",
		Summary: "Create a short link", Revision: 1, VerdictEvent: "event-9",
	}
}

func TestAResubmissionAdvancesTheRevisionOnce(t *testing.T) {
	store, api := newMemStore(), newReviewAPI()
	api.revision = 1
	got, err := submitter(store, api).Resubmit(context.Background(), submittedRun(), rework())
	if err != nil {
		t.Fatalf("resubmit: %v", err)
	}
	if got.ReviewRevision != 2 {
		t.Errorf("revision is %d, want 2", got.ReviewRevision)
	}
	if got.ReviewState != orchestrator.SubmitSubmitted {
		t.Errorf("state is %q", got.ReviewState)
	}
	last := api.calls[len(api.calls)-1]
	if last.expectedRevision != 1 || last.verdictEvent != "event-9" {
		t.Errorf("the fences were %+v", last)
	}
}

func TestTheResubmittingStateAndItsFencesArePersistedFirst(t *testing.T) {
	store, api := newMemStore(), newReviewAPI()
	api.revision = 1
	api.err = errors.New("sutra unreachable")
	if _, err := submitter(store, api).Resubmit(context.Background(), submittedRun(), rework()); err == nil {
		t.Fatal("a resubmission that never landed read as success")
	}
	row := store.rows["run-1"]
	if row.ReviewState != orchestrator.SubmitResubmitting {
		t.Errorf("state is %q, so the crash window is invisible", row.ReviewState)
	}
	if row.ReviewKey == "" || row.ReviewCommit != "C3" ||
		row.ReviewRevision != 1 || row.ReviewVerdictEvent != "event-9" {
		t.Errorf("the row cannot rebuild its own call: %+v", row)
	}
}

func TestAReplayedResubmissionAdvancesTheRevisionExactlyOnce(t *testing.T) {
	store, api := newMemStore(), newReviewAPI()
	api.revision = 1
	s := submitter(store, api)
	api.err = errors.New("sutra unreachable")
	if _, err := s.Resubmit(context.Background(), submittedRun(), rework()); err == nil {
		t.Fatal("expected the resubmission failure")
	}
	api.err = nil

	// Twice, because a replay may itself crash and run again.
	for i := range 2 {
		n, err := s.RecoverResubmissions(context.Background(),
			func(orchestrator.BuildRun) orchestrator.Rework { return rework() })
		if err != nil {
			t.Fatalf("recover: %v", err)
		}
		// The first pass finds the stuck one; the second finds none, because
		// the first settled it.
		want := 1 - i
		if n != want {
			t.Errorf("pass %d recovered %d, want %d", i, n, want)
		}
	}
	if api.revision != 2 {
		t.Errorf("the revision advanced to %d, want exactly 2", api.revision)
	}
	if store.rows["run-1"].ReviewRevision != 2 {
		t.Errorf("recorded revision %d", store.rows["run-1"].ReviewRevision)
	}
}

func TestTheReplayNamesThePersistedEventAndCommit(t *testing.T) {
	// Neither a moved branch nor a later verdict can change what the replay
	// requests.
	store, api := newMemStore(), newReviewAPI()
	api.revision = 1
	s := submitter(store, api)
	api.err = errors.New("sutra unreachable")
	if _, err := s.Resubmit(context.Background(), submittedRun(), rework()); err == nil {
		t.Fatal("expected the resubmission failure")
	}
	api.err = nil

	moved := rework()
	moved.VerdictEvent = "event-later"
	moved.Revision = 7
	if _, err := s.RecoverResubmissions(context.Background(),
		func(orchestrator.BuildRun) orchestrator.Rework { return moved }); err != nil {
		t.Fatalf("recover: %v", err)
	}
	last := api.calls[len(api.calls)-1]
	if last.verdictEvent != "event-9" {
		t.Errorf("the replay answered %q, want the persisted event-9", last.verdictEvent)
	}
	if last.expectedRevision != 1 {
		t.Errorf("the replay expected revision %d, want the persisted 1", last.expectedRevision)
	}
	if last.commit != "C3" {
		t.Errorf("the replay named %s, want the pinned C3", last.commit)
	}
}

func TestAReviewThatMovedUnderTheFenceIsReported(t *testing.T) {
	// The fence exists to catch exactly this, and a silent acceptance would
	// record a revision the review does not have.
	store, api := newMemStore(), newReviewAPI()
	api.revision = 5
	if _, err := submitter(store, api).Resubmit(context.Background(), submittedRun(), rework()); err == nil {
		t.Fatal("a resubmission past a moved fence read as success")
	}
}

func TestAResubmissionNeedsAReviewAndAVerdictEvent(t *testing.T) {
	store, api := newMemStore(), newReviewAPI()
	s := submitter(store, api)

	noReview := submittedRun()
	noReview.ReviewID = ""
	if _, err := s.Resubmit(context.Background(), noReview, rework()); err == nil {
		t.Error("a run with no review was resubmitted")
	}
	noCommit := submittedRun()
	noCommit.GatedBase = ""
	if _, err := s.Resubmit(context.Background(), noCommit, rework()); err == nil {
		t.Error("a run with no gated commit was resubmitted")
	}
	// Without an event sutra cannot fence the call, and the rework could
	// answer a verdict the human has since replaced.
	unfenced := rework()
	unfenced.VerdictEvent = ""
	if _, err := s.Resubmit(context.Background(), submittedRun(), unfenced); err == nil {
		t.Error("a resubmission with no verdict event was sent")
	}
	if len(api.calls) != 0 {
		t.Errorf("sutra was called %d times for refused resubmissions", len(api.calls))
	}
}

func TestEachReworkOfARevisionKeysDifferently(t *testing.T) {
	// A second changes-requested at the same revision is a different rework,
	// and reusing the key would replay the first instead of sending it.
	store, api := newMemStore(), newReviewAPI()
	api.revision = 1
	s := submitter(store, api)
	first, err := s.Resubmit(context.Background(), submittedRun(), rework())
	if err != nil {
		t.Fatalf("resubmit: %v", err)
	}
	again := rework()
	again.VerdictEvent = "event-10"
	again.Revision = 2
	second, err := s.Resubmit(context.Background(), first, again)
	if err != nil {
		t.Fatalf("resubmit again: %v", err)
	}
	if first.ReviewKey == second.ReviewKey {
		t.Error("two reworks shared one key")
	}
}

func TestAnUnreadableResubmitListStopsRecovery(t *testing.T) {
	store := &failingRuns{listErr: errors.New("store unavailable")}
	_, err := (orchestrator.Submitter{Store: store, Reviews: newReviewAPI(), Author: "a"}).
		RecoverResubmissions(context.Background(),
			func(orchestrator.BuildRun) orchestrator.Rework { return rework() })
	if err == nil {
		t.Fatal("an unreadable store read as no resubmissions in flight")
	}
}

func TestASubmissionCarriesBothBaseFences(t *testing.T) {
	// sutra resolves the branch's actual base and the default branch's actual
	// head, and rejects atomically when either differs — so a diff whose base
	// was never gated cannot reach a human at all.
	store, api := newMemStore(), newReviewAPI()
	if _, err := submitter(store, api).Submit(context.Background(), gatedRun(), submission()); err != nil {
		t.Fatalf("submit: %v", err)
	}
	call := api.calls[0]
	if call.expectedBase != "C2" || call.expectedDefaultHead != "C2" {
		t.Errorf("fenced on base %q and head %q, want the run's gated C2",
			call.expectedBase, call.expectedDefaultHead)
	}
}

func TestAResubmissionCarriesThemToo(t *testing.T) {
	// Rework is no more entitled to put an ungated diff in front of a human.
	store, api := newMemStore(), newReviewAPI()
	api.revision = 1
	if _, err := submitter(store, api).Resubmit(context.Background(), submittedRun(), rework()); err != nil {
		t.Fatalf("resubmit: %v", err)
	}
	call := api.calls[len(api.calls)-1]
	if call.expectedBase != "C3" || call.expectedDefaultHead != "C3" {
		t.Errorf("fenced on base %q and head %q", call.expectedBase, call.expectedDefaultHead)
	}
}

func TestAFencedSubmissionCreatesNothing(t *testing.T) {
	store, api := newMemStore(), newReviewAPI()
	api.fenceHead = "D2"
	if _, err := submitter(store, api).Submit(context.Background(), gatedRun(), submission()); err == nil {
		t.Fatal("an ungated submission was accepted")
	}
	if len(api.byKey) != 0 {
		t.Errorf("%d reviews were created", len(api.byKey))
	}
	// The write-ahead row survives, which is what lets the run be seen as
	// having tried.
	if store.rows["run-1"].ReviewState != orchestrator.SubmitSubmitting {
		t.Errorf("the run is in %q", store.rows["run-1"].ReviewState)
	}
}

func TestAFencedResubmissionLeavesTheRevisionAlone(t *testing.T) {
	store, api := newMemStore(), newReviewAPI()
	api.revision = 1
	api.fenceBase = "D0"
	if _, err := submitter(store, api).Resubmit(context.Background(), submittedRun(), rework()); err == nil {
		t.Fatal("an ungated resubmission was accepted")
	}
	if api.revision != 1 {
		t.Errorf("the revision moved to %d", api.revision)
	}
}
