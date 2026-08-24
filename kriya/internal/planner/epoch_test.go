package planner_test

import (
	"context"
	"errors"
	"testing"

	"kriya/internal/planner"
)

// memAdvances is an insert-once CompletionAdvance store.
type memAdvances struct {
	rows map[string]planner.CompletionAdvance
	// epoch is the target's counter, moved only by a fresh insert.
	epoch    map[string]int
	stamped  map[string]int
	err      error
	inserted int
	// mutated records the local operations whose primary mutation committed
	// with their advance.
	mutated []string
}

func newMemAdvances() *memAdvances {
	return &memAdvances{
		rows:  map[string]planner.CompletionAdvance{},
		epoch: map[string]int{}, stamped: map[string]int{},
	}
}

func (m *memAdvances) Insert(_ context.Context, a planner.CompletionAdvance) (bool, error) {
	if m.err != nil {
		return false, m.err
	}
	if _, taken := m.rows[a.Key]; taken {
		return false, nil
	}
	m.inserted++
	m.rows[a.Key] = a
	m.epoch[a.TargetKey]++
	delete(m.stamped, a.TargetKey)
	return true, nil
}

// InsertWith runs the bound mutation inside the "transaction", which for an
// in-memory double means: only on a fresh insert, and its failure undoes the
// advance.
func (m *memAdvances) InsertWith(
	ctx context.Context, a planner.CompletionAdvance, mutate func(planner.Tx) error,
) (bool, error) {
	fresh, err := m.Insert(ctx, a)
	if err != nil || !fresh {
		return fresh, err
	}
	if mutate != nil {
		if err := mutate(nil); err != nil {
			// Rolled back: the advance never happened either.
			delete(m.rows, a.Key)
			m.epoch[a.TargetKey]--
			m.inserted--
			return false, err
		}
	}
	m.mutated = append(m.mutated, a.LocalSource)
	return true, nil
}

func (m *memAdvances) Epoch(_ context.Context, targetKey string) (int, error) {
	return m.epoch[targetKey], m.err
}

func (m *memAdvances) Stamp(_ context.Context, targetKey string, claimEpoch int) (bool, error) {
	if m.err != nil {
		return false, m.err
	}
	if m.epoch[targetKey] != claimEpoch {
		return false, nil
	}
	m.stamped[targetKey] = claimEpoch
	return true, nil
}

func epochs(store *memAdvances) planner.Epochs { return planner.Epochs{Store: store} }

func TestAnEventConsumptionAdvanceMovesTheEpochOnce(t *testing.T) {
	store := newMemAdvances()
	e := epochs(store)
	moved, err := e.OnEvent(context.Background(), "/spec", planner.CauseTicketReopen, "event-9")
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if !moved {
		t.Fatal("a fresh event did not advance the epoch")
	}
	if got, _ := store.Epoch(context.Background(), "/spec"); got != 1 {
		t.Errorf("the epoch is %d", got)
	}
}

func TestAReplayedEventAdvancesNothingInAnyOrder(t *testing.T) {
	// "every event is consumed at most once, a valid completion is never
	// cleared twice, and the cause needed to recover an explicit reopen is
	// never overwritten."
	store := newMemAdvances()
	e := epochs(store)
	for _, id := range []string{"E1", "E2"} {
		if _, err := e.OnEvent(context.Background(), "/spec", planner.CauseTicketReopen, id); err != nil {
			t.Fatalf("advance %s: %v", id, err)
		}
	}
	before, _ := store.Epoch(context.Background(), "/spec")

	// E1 replayed AFTER E2 — the out-of-order case the key exists for.
	moved, err := e.OnEvent(context.Background(), "/spec", planner.CauseSupersession, "E1")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if moved {
		t.Error("a replayed event advanced the epoch")
	}
	if got, _ := store.Epoch(context.Background(), "/spec"); got != before {
		t.Errorf("the epoch moved from %d to %d on a replay", before, got)
	}
	// And the recorded cause is the ORIGINAL one: the replay named a
	// different cause, and overwriting it would lose what an explicit reopen
	// needs to recover.
	if got := store.rows[planner.EventAdvanceKey("/spec", "E1")]; got.Cause != planner.CauseTicketReopen {
		t.Errorf("the replay rewrote the cause to %q", got.Cause)
	}
}

func TestAnEventConsumptionAdvanceNeedsItsEvent(t *testing.T) {
	// The key IS the event. Without one there is nothing to collide on, and
	// every replay would advance again.
	if _, err := epochs(newMemAdvances()).
		OnEvent(context.Background(), "/spec", planner.CauseTicketReopen, ""); err == nil {
		t.Fatal("an event-consumption advance with no event was accepted")
	}
}

func TestALocalOperationAdvancesExactlyOncePerSource(t *testing.T) {
	// A supersession consumes no event. Its identity is the new PlanHead
	// generation — immutable and unique to the one operation that advanced
	// the epoch — so a crash-replay of the replacement collides.
	store := newMemAdvances()
	e := epochs(store)
	for range 4 {
		if _, err := e.OnLocal(context.Background(), "/spec",
			planner.CauseSupersession, "generation-7"); err != nil {
			t.Fatalf("advance: %v", err)
		}
	}
	if store.inserted != 1 {
		t.Errorf("one supersession inserted %d advances", store.inserted)
	}
	if got, _ := store.Epoch(context.Background(), "/spec"); got != 1 {
		t.Errorf("the epoch is %d after one supersession replayed four times", got)
	}
}

func TestTwoLocalOperationsOfOneCauseAdvanceSeparately(t *testing.T) {
	// Two supersessions are two operations. Sharing a key would leave the
	// second one's work behind a stamp the first one's epoch validated.
	store := newMemAdvances()
	e := epochs(store)
	for _, src := range []string{"generation-7", "generation-8"} {
		if _, err := e.OnLocal(context.Background(), "/spec",
			planner.CauseSupersession, src); err != nil {
			t.Fatalf("advance: %v", err)
		}
	}
	if got, _ := store.Epoch(context.Background(), "/spec"); got != 2 {
		t.Errorf("two supersessions moved the epoch to %d", got)
	}
}

func TestALocalOperationNeedsItsSource(t *testing.T) {
	if _, err := epochs(newMemAdvances()).
		OnLocal(context.Background(), "/spec", planner.CauseSupersession, ""); err == nil {
		t.Fatal("a local-operation advance with no source was accepted")
	}
}

func TestAnEventAdvanceAndALocalAdvanceNeverShareAKey(t *testing.T) {
	// They are different variants with different identity sources. A
	// collision between them would silently drop one.
	if planner.EventAdvanceKey("/spec", "E1") ==
		planner.LocalAdvanceKey("/spec", planner.CauseSupersession, "E1") {
		t.Error("an event advance and a local advance share a key")
	}
}

func TestAStampNeedsTheEpochItClaimed(t *testing.T) {
	// "a stale approval never stamps an unfinished target". The claim was
	// made against epoch 3; a reopen or supersession has moved it to 4, and
	// the approval the human gave was for work that is no longer all of it.
	store := newMemAdvances()
	e := epochs(store)
	if _, err := e.OnEvent(context.Background(), "/spec", planner.CauseTicketReopen, "E1"); err != nil {
		t.Fatalf("advance: %v", err)
	}
	claim, _ := store.Epoch(context.Background(), "/spec")

	// The world moves under the in-flight claim.
	if _, err := e.OnEvent(context.Background(), "/spec", planner.CauseNewWork, "E2"); err != nil {
		t.Fatalf("advance: %v", err)
	}
	stamped, err := e.Stamp(context.Background(), "/spec", claim)
	if err != nil {
		t.Fatalf("stamp: %v", err)
	}
	if stamped {
		t.Error("a stale claim stamped the target complete")
	}
	if _, ok := store.stamped["/spec"]; ok {
		t.Error("something was stamped anyway")
	}
}

func TestAStampAtItsOwnEpochLands(t *testing.T) {
	store := newMemAdvances()
	e := epochs(store)
	if _, err := e.OnEvent(context.Background(), "/spec", planner.CauseTicketReopen, "E1"); err != nil {
		t.Fatalf("advance: %v", err)
	}
	claim, _ := store.Epoch(context.Background(), "/spec")
	stamped, err := e.Stamp(context.Background(), "/spec", claim)
	if err != nil {
		t.Fatalf("stamp: %v", err)
	}
	if !stamped {
		t.Error("a claim at the current epoch did not stamp")
	}
}

func TestAnAdvanceClearsAStampThatAlreadyLanded(t *testing.T) {
	// "the completion epoch advances and the completion stamp is atomically
	// cleared". A target whose work came back is not complete, whatever it
	// said a moment ago.
	store := newMemAdvances()
	e := epochs(store)
	if stamped, err := e.Stamp(context.Background(), "/spec", 0); err != nil || !stamped {
		t.Fatalf("stamp: %v stamped=%v", err, stamped)
	}
	if _, err := e.OnEvent(context.Background(), "/spec", planner.CauseTicketReopen, "E1"); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if _, ok := store.stamped["/spec"]; ok {
		t.Error("the stamp survived the work coming back")
	}
}

func TestAnUnwritableAdvanceIsAFailure(t *testing.T) {
	// An advance nobody recorded is a stamp that stays valid over work that
	// returned.
	store := newMemAdvances()
	store.err = errors.New("disk full")
	if _, err := epochs(store).
		OnEvent(context.Background(), "/spec", planner.CauseTicketReopen, "E1"); err == nil {
		t.Fatal("an advance that was never written read as recorded")
	}
}

func TestAnUnknownCauseIsRefused(t *testing.T) {
	// The cause selects how recovery reconciles the advance. One outside the
	// declared set is a defect, and storing it would strand the row.
	if _, err := epochs(newMemAdvances()).
		OnEvent(context.Background(), "/spec", "whatever", "E1"); err == nil {
		t.Fatal("an undeclared cause was accepted")
	}
}
