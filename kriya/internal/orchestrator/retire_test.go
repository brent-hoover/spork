package orchestrator_test

import (
	"context"
	"errors"
	"testing"

	"kriya/internal/orchestrator"
)

// spikeGate stands in for sutra's approved-review close gate.
type spikeGate struct {
	closedByKey map[string]bool
	calls       []string
	err         error
}

func newSpikeGate() *spikeGate { return &spikeGate{closedByKey: map[string]bool{}} }

func (g *spikeGate) Complete(
	_ context.Context, _, _ string, _ int, _, key string,
) error {
	g.calls = append(g.calls, key)
	if g.err != nil {
		return g.err
	}
	if g.closedByKey[key] {
		// Replayed under the key that closed it: the original success, never
		// a close-used conflict.
		return nil
	}
	g.closedByKey[key] = true
	return nil
}

func retiring(store *memStore, gate *spikeGate) orchestrator.Researcher {
	return orchestrator.Researcher{Store: store, Tickets: gate}
}

func approvedSpike() orchestrator.BuildRun {
	return orchestrator.BuildRun{
		ID: "run-spike", Ticket: "spike: headless?", Issue: "issue-7",
		Kind: orchestrator.KindSpike, State: orchestrator.StateFindingSubmitted,
		ReviewID: "review-1", ReviewRevision: 1, FindingVersion: "ver-1",
		ReviewState: orchestrator.SubmitSubmitted,
	}
}

func TestAnApprovedFindingClosesItsSpike(t *testing.T) {
	// The close is what retires the risk: sutra releases the blocking
	// relations when the blocker closes, so kriya asks for no unblocking of
	// its own.
	store, gate := newMemStore(), newSpikeGate()
	got, err := retiring(store, gate).
		Retire(context.Background(), approvedSpike(), "event-9")
	if err != nil {
		t.Fatalf("retire: %v", err)
	}
	if got.State != orchestrator.StateClosed {
		t.Errorf("the run settled as %q", got.State)
	}
	if got.CompletionState != orchestrator.CompleteClosed {
		t.Errorf("the completion state is %q", got.CompletionState)
	}
	if len(gate.closedByKey) != 1 {
		t.Errorf("the spike closed %d times", len(gate.closedByKey))
	}
}

func TestTheCloseIsWrittenAheadWithItsKey(t *testing.T) {
	// A crash after the close landed replays under the SAME key — the
	// original success, never a close-used conflict.
	store, gate := newMemStore(), newSpikeGate()
	gate.err = errors.New("crash mid-close")
	if _, err := retiring(store, gate).
		Retire(context.Background(), approvedSpike(), "event-9"); err == nil {
		t.Fatal("expected the crash")
	}
	got := store.rows["run-spike"]
	if got.CloseKey == "" || got.ReviewVerdictEvent != "event-9" {
		t.Errorf("the close was not written ahead: %+v", got)
	}
	if got.CompletionState != orchestrator.CompleteCompleting {
		t.Errorf("the run is in completion state %q", got.CompletionState)
	}
}

func TestARetirementReplaysToItsOriginalSuccess(t *testing.T) {
	store, gate := newMemStore(), newSpikeGate()
	r := retiring(store, gate)
	if _, err := r.Retire(context.Background(), approvedSpike(), "event-9"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	before := len(gate.closedByKey)
	if _, err := r.Retire(context.Background(), store.rows["run-spike"], "event-9"); err != nil {
		t.Fatalf("replayed retire: %v", err)
	}
	if len(gate.closedByKey) != before {
		t.Error("the replay closed a second time")
	}
	if gate.calls[0] != gate.calls[1] {
		t.Errorf("the replay presented a different key: %v", gate.calls)
	}
}

func TestAReapprovalAfterAConflictGetsAFreshCloseKey(t *testing.T) {
	// A reversal-induced conflict is cached under the key that hit it, so a
	// reapproval must issue under a fresh one — reusing it would return that
	// conflict forever and the risk could never retire.
	store, gate := newMemStore(), newSpikeGate()
	r := retiring(store, gate)
	gate.err = errors.New("conflict: the approval was reversed")
	if _, err := r.Retire(context.Background(), approvedSpike(), "event-9"); err == nil {
		t.Fatal("expected the conflict")
	}
	first := store.rows["run-spike"].CloseKey

	gate.err = nil
	got, err := r.Retire(context.Background(), store.rows["run-spike"], "event-10")
	if err != nil {
		t.Fatalf("retried retire: %v", err)
	}
	if got.CloseKey == first {
		t.Error("the reapproval reused the conflicted key")
	}
	if gate.calls[len(gate.calls)-1] == first {
		t.Error("the retry went out under the conflicted key")
	}
}

func TestASpikeWithNoFindingCannotRetire(t *testing.T) {
	// A risk retired without documented evidence is the one thing risk-first
	// exists to prevent.
	store, gate := newMemStore(), newSpikeGate()
	run := approvedSpike()
	run.FindingVersion = ""
	if _, err := retiring(store, gate).
		Retire(context.Background(), run, "event-9"); err == nil {
		t.Fatal("a spike with no finding closed")
	}
	run = approvedSpike()
	run.ReviewID = ""
	if _, err := retiring(store, gate).
		Retire(context.Background(), run, "event-9"); err == nil {
		t.Fatal("a spike with no review closed")
	}
}

func TestARetirementNeedsItsApprovalEvent(t *testing.T) {
	store, gate := newMemStore(), newSpikeGate()
	if _, err := retiring(store, gate).
		Retire(context.Background(), approvedSpike(), ""); err == nil {
		t.Fatal("a retirement with no verdict event was accepted")
	}
}

func TestRecoveryReplaysASpikeCloseAndLeavesTicketClosesAlone(t *testing.T) {
	store, gate := newMemStore(), newSpikeGate()
	spike := approvedSpike()
	spike.CompletionState = orchestrator.CompleteCompleting
	spike.ReviewVerdictEvent = "event-9"
	spike.CloseKey = orchestrator.SpikeCloseKey("run-spike", "event-9")
	if err := store.Upsert(context.Background(), spike); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// A merged ticket's close. Its own recovery replays it; doing it here
	// would close a code ticket on a finding review.
	if err := store.Upsert(context.Background(), orchestrator.BuildRun{
		ID: "run-code", Issue: "issue-8", CompletionState: orchestrator.CompleteCompleting,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	n, err := retiring(store, gate).RecoverRetirements(context.Background())
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if n != 1 {
		t.Errorf("replayed %d retirements", n)
	}
	if store.rows["run-code"].State == orchestrator.StateClosed {
		t.Error("a code ticket's close was replayed as a spike retirement")
	}
}

func TestTheSpikeCloseKeyIsScopedToItsApproval(t *testing.T) {
	if orchestrator.SpikeCloseKey("run-1", "event-9") ==
		orchestrator.SpikeCloseKey("run-1", "event-10") {
		t.Error("two approvals share a close key")
	}
	if orchestrator.SpikeCloseKey("run-1", "event-9") ==
		orchestrator.SpikeCloseKey("run-2", "event-9") {
		t.Error("two runs share a close key")
	}
}
