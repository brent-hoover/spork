package reviewbridge_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"kriya/internal/fakes"
	"kriya/internal/reviewbridge"
)

type memStore struct {
	attempts map[string]reviewbridge.EnqueueAttempt
	rounds   map[string]reviewbridge.Round
}

func newMemStore() *memStore {
	return &memStore{
		attempts: map[string]reviewbridge.EnqueueAttempt{},
		rounds:   map[string]reviewbridge.Round{},
	}
}

func (m *memStore) UpsertAttempt(_ context.Context, a reviewbridge.EnqueueAttempt) error {
	m.attempts[a.Round] = a
	return nil
}

func (m *memStore) UpsertRound(_ context.Context, r reviewbridge.Round) error {
	m.rounds[r.ID] = r
	return nil
}

func (m *memStore) Unresolved(context.Context) ([]reviewbridge.EnqueueAttempt, error) {
	var out []reviewbridge.EnqueueAttempt
	for _, a := range m.attempts {
		if a.State == reviewbridge.AttemptUnresolved || a.State == reviewbridge.AttemptPending {
			out = append(out, a)
		}
	}
	return out, nil
}

type fakeRoborev struct {
	jobID      int
	enqueueErr error
	status     string
	report     string
	statusErr  error
	closed     []int
	enqueued   int
}

func (f *fakeRoborev) Enqueue(context.Context, string, string) (int, error) {
	if f.enqueueErr != nil {
		return 0, f.enqueueErr
	}
	f.enqueued++
	return f.jobID, nil
}

func (f *fakeRoborev) Status(context.Context, string, int) (string, string, error) {
	if f.statusErr != nil {
		return "", "", f.statusErr
	}
	return f.status, f.report, nil
}

func (f *fakeRoborev) Close(_ context.Context, _ string, id int) error {
	f.closed = append(f.closed, id)
	return nil
}

func bridge(store *memStore, rev *fakeRoborev) reviewbridge.Bridge {
	return reviewbridge.Bridge{
		Repo: "/repo", Store: store, Rev: rev, Now: fakes.NewClock(time.Unix(0, 0)),
	}
}

func TestTheAttemptIsRecordedBeforeRoborevIsCalled(t *testing.T) {
	// The call has no correlation key. If kriya crashes between sending and
	// recording, nothing in roborev says which job was its own — the attempt
	// is the only evidence the call happened at all.
	store := newMemStore()
	rev := &fakeRoborev{enqueueErr: errors.New("daemon down")}
	if _, err := bridge(store, rev).Submit(context.Background(), "run-1", "round-1", "abc123"); err == nil {
		t.Fatal("expected the enqueue failure to surface")
	}
	a, ok := store.attempts["round-1"]
	if !ok {
		t.Fatal("no attempt survived; the call left no evidence")
	}
	if a.State != reviewbridge.AttemptUnresolved {
		t.Errorf("state is %q, want unresolved", a.State)
	}
	if a.Note == "" {
		t.Error("nothing explains the unresolved attempt to the operator")
	}
}

func TestAFailedEnqueueNeverAdoptsAJob(t *testing.T) {
	// R1: roborev does not dedupe by SHA, so a job for this commit may be
	// someone else's. Kriya acts only on ids from its own returns.
	store := newMemStore()
	rev := &fakeRoborev{enqueueErr: errors.New("timeout")}
	if _, err := bridge(store, rev).Submit(context.Background(), "run-1", "round-1", "abc123"); err == nil {
		t.Fatal("expected an error")
	}
	if a := store.attempts["round-1"]; a.JobID != 0 {
		t.Errorf("an unresolved attempt adopted job %d", a.JobID)
	}
	if len(store.rounds) != 0 {
		t.Error("a round was created for an unproven job")
	}
}

func TestASuccessfulSubmitRecordsTheJobItCreated(t *testing.T) {
	store := newMemStore()
	rev := &fakeRoborev{jobID: 4242}
	round, err := bridge(store, rev).Submit(context.Background(), "run-1", "round-1", "abc123")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if round.JobID != 4242 {
		t.Errorf("round holds job %d, want 4242", round.JobID)
	}
	if a := store.attempts["round-1"]; a.State != reviewbridge.AttemptResolved || a.JobID != 4242 {
		t.Errorf("attempt not resolved against its job: %+v", a)
	}
}

func TestAPendingJobIsNotAVerdict(t *testing.T) {
	// Waiting is the ordinary state; the caller decides how long.
	store := newMemStore()
	rev := &fakeRoborev{jobID: 1, status: "running"}
	b := bridge(store, rev)
	round, _ := b.Submit(context.Background(), "run-1", "round-1", "abc")
	got, err := b.Poll(context.Background(), round)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if got.Verdict != reviewbridge.VerdictPending {
		t.Errorf("verdict is %q, want pending", got.Verdict)
	}
}

func TestACleanReportIsAPass(t *testing.T) {
	store := newMemStore()
	rev := &fakeRoborev{jobID: 1, status: "done", report: "No issues found.\n\nSummary: fine."}
	b := bridge(store, rev)
	round, _ := b.Submit(context.Background(), "run-1", "round-1", "abc")
	got, err := b.Poll(context.Background(), round)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if got.Verdict != reviewbridge.VerdictClean {
		t.Errorf("verdict is %q, want clean", got.Verdict)
	}
}

func TestAReportWithFindingsRequestsChanges(t *testing.T) {
	store := newMemStore()
	report := "## Review Findings\n\n- **Severity**: Medium\n- **Problem**: it is wrong\n"
	rev := &fakeRoborev{jobID: 1, status: "done", report: report}
	b := bridge(store, rev)
	round, _ := b.Submit(context.Background(), "run-1", "round-1", "abc")
	got, err := b.Poll(context.Background(), round)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if got.Verdict != reviewbridge.VerdictFindings {
		t.Errorf("verdict is %q, want findings", got.Verdict)
	}
	// The findings are handed back to the dev agent verbatim: they ARE the
	// next instruction, and summarising them would drop the file and line.
	if got.Findings != report {
		t.Error("the report was not carried through intact")
	}
}

func TestPollingARoundWithNoJobIsAnError(t *testing.T) {
	// An unresolved attempt has no job to poll, and inventing one would be
	// the adoption R1 forbids.
	store := newMemStore()
	rev := &fakeRoborev{}
	_, err := bridge(store, rev).Poll(context.Background(), reviewbridge.Round{ID: "round-1"})
	if err == nil {
		t.Fatal("expected an error for a round with no job id")
	}
}

func TestUnresolvedAttemptsAreListableForTheOperator(t *testing.T) {
	// R1: rare crash-window orphans are surfaced, not resolved by guessing.
	store := newMemStore()
	rev := &fakeRoborev{enqueueErr: errors.New("boom")}
	_, _ = bridge(store, rev).Submit(context.Background(), "run-1", "round-1", "abc")
	got, err := store.Unresolved(context.Background())
	if err != nil {
		t.Fatalf("unresolved: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("found %d unresolved attempts, want 1", len(got))
	}
}

func TestSettleClosesOnlyKriyasOwnJob(t *testing.T) {
	store, rev := newMemStore(), &fakeRoborev{jobID: 91, status: "done", report: "No issues found"}
	b := bridge(store, rev)
	round, err := b.Submit(context.Background(), "run-1", "round-1", "abc123")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	round, err = b.Poll(context.Background(), round)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if err := b.Settle(context.Background(), round); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if len(rev.closed) != 1 || rev.closed[0] != 91 {
		t.Errorf("closed %v, want only job 91 — the one kriya's own enqueue returned", rev.closed)
	}
}

func TestSettleRefusesARoundWithNoProvenJob(t *testing.T) {
	// R1: a job kriya cannot prove is its own must never be closed.
	rev := &fakeRoborev{}
	err := bridge(newMemStore(), rev).Settle(context.Background(),
		reviewbridge.Round{ID: "round-1", Commit: "abc123", Verdict: reviewbridge.VerdictClean})
	if err == nil {
		t.Fatal("a round with no job id must not close anything")
	}
	if len(rev.closed) != 0 {
		t.Errorf("closed %v despite having no proven job", rev.closed)
	}
}

func TestSettleRefusesAJobStillRunning(t *testing.T) {
	rev := &fakeRoborev{}
	err := bridge(newMemStore(), rev).Settle(context.Background(),
		reviewbridge.Round{ID: "round-1", JobID: 7, Verdict: reviewbridge.VerdictPending})
	if err == nil {
		t.Fatal("closing a review that has not reported is a lost verdict")
	}
	if len(rev.closed) != 0 {
		t.Errorf("closed %v while still pending", rev.closed)
	}
}

func TestRecoverSurfacesAmbiguousAttemptsWithoutGuessing(t *testing.T) {
	store := newMemStore()
	rev := &fakeRoborev{enqueueErr: errors.New("connection reset")}
	b := bridge(store, rev)
	if _, err := b.Submit(context.Background(), "run-1", "round-1", "abc123"); err == nil {
		t.Fatal("a failed enqueue must be reported")
	}
	stuck, err := b.Recover(context.Background())
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(stuck) != 1 || stuck[0].Round != "round-1" {
		t.Fatalf("recovered %v, want the one ambiguous attempt", stuck)
	}
	if stuck[0].JobID != 0 {
		t.Error("recovery invented a job id for an attempt whose outcome is unknown")
	}
	if len(rev.closed) != 0 {
		t.Errorf("recovery closed %v — it must resolve nothing on its own", rev.closed)
	}
}
