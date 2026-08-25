package planner_test

import (
	"context"
	"errors"
	"testing"

	"kriya/internal/planner"
)

// workFeed hands out lifecycle events from a cursor.
type workFeed struct {
	pages map[string][]planner.WorkEvent
	next  map[string]string
	err   error
}

func (f *workFeed) Since(
	_ context.Context, cursor string,
) ([]planner.WorkEvent, string, error) {
	if f.err != nil {
		return nil, "", f.err
	}
	return f.pages[cursor], f.next[cursor], nil
}

// workCursors persists a consumer's feed position.
type workCursors struct{ at map[string]string }

func newWorkCursors() *workCursors { return &workCursors{at: map[string]string{}} }

func (c *workCursors) Current(_ context.Context, name string) (string, error) {
	return c.at[name], nil
}

func (c *workCursors) Advance(_ context.Context, name, cursor string) error {
	c.at[name] = cursor
	return nil
}

type watch struct {
	work     planner.Work
	advances *memAdvances
	claims   *memClaims
	tickets  *memTickets
	cursors  *workCursors
}

func watcher(t *testing.T, events []planner.WorkEvent, status map[string]string) *watch {
	t.Helper()
	_ = status
	w := &watch{
		advances: newMemAdvances(), claims: newMemClaims(), tickets: newMemTickets(),
		cursors: newWorkCursors(),
	}
	w.work = planner.Work{
		Feed: &workFeed{
			pages: map[string][]planner.WorkEvent{"": events},
			next:  map[string]string{"": "c-1"},
		},
		Epochs:  planner.Epochs{Store: w.advances},
		Tickets: w.tickets, Claims: w.claims, Cursors: w.cursors,
		TargetKey: "/spec", Epic: "epic-1", Actor: "actor-1",
	}
	return w
}

// stamped seeds a target that reads as complete.
func (w *watch) stamped(t *testing.T) *watch {
	t.Helper()
	if err := w.claims.Upsert(context.Background(), planner.CompletionClaim{
		TargetKey: "/spec", State: planner.CompletionComplete, Epoch: 0,
	}); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	if ok, err := (planner.Epochs{Store: w.advances}).
		Stamp(context.Background(), "/spec", 0); err != nil || !ok {
		t.Fatalf("seed stamp: %v ok=%v", err, ok)
	}
	return w
}

func (w *watch) plans(t *testing.T, id string) *watch {
	t.Helper()
	if err := w.tickets.Put(context.Background(), "/spec",
		planner.Ticket{Title: id, IssueID: id}); err != nil {
		t.Fatalf("seed ticket: %v", err)
	}
	return w
}

func (w *watch) epoch(t *testing.T) int {
	t.Helper()
	got, err := w.advances.Epoch(context.Background(), "/spec")
	if err != nil {
		t.Fatalf("epoch: %v", err)
	}
	return got
}

func reopenOf(issue string) []planner.WorkEvent {
	return []planner.WorkEvent{{ID: "E1", Kind: planner.KindStatusChanged, Subject: issue}}
}

func TestAReopenedPlanTicketAdvancesTheEpochAndClearsTheStamp(t *testing.T) {
	// "completion clears when work returns". A ticket that was complete and
	// is active again is work the human's approval never covered.
	w := watcher(t, reopenOf("issue-1"), map[string]string{"issue-1": "open"}).
		stamped(t).plans(t, "issue-1")

	n, _, err := w.work.Consume(context.Background())
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if n != 1 {
		t.Errorf("consumed %d advances", n)
	}
	if w.epoch(t) != 1 {
		t.Errorf("the epoch is %d", w.epoch(t))
	}
	if _, ok := w.advances.stamped["/spec"]; ok {
		t.Error("the stamp survived the work returning")
	}
	if got := w.advances.rows[planner.EventAdvanceKey("/spec", "E1")]; got.Cause != planner.CauseTicketReopen {
		t.Errorf("the advance records cause %q", got.Cause)
	}
}

func TestAStatusChangeUnderAStandingClaimIsAlwaysAReopen(t *testing.T) {
	// Every target-scoped ticket was COMPLETE when the claim was captured.
	// A status change on one since can only be work coming back, whatever
	// the status reads now — and reading it now is precisely how a
	// reopen-and-recomplete slips past.
	w := watcher(t, reopenOf("issue-1"), map[string]string{"issue-1": "complete"}).
		stamped(t).plans(t, "issue-1")
	if _, _, err := w.work.Consume(context.Background()); err != nil {
		t.Fatalf("consume: %v", err)
	}
	if w.epoch(t) != 1 {
		t.Error("a status change under a standing claim did not advance")
	}
	if _, ok := w.advances.stamped["/spec"]; ok {
		t.Error("the completion survived work returning")
	}
}

func TestAnotherTargetsIssueAdvancesNothing(t *testing.T) {
	// The feed is project-wide. An issue this target never planned, and that
	// is not its epic, belongs to somebody else's build.
	w := watcher(t, reopenOf("issue-99"), map[string]string{"issue-99": "open"}).
		stamped(t).plans(t, "issue-1")
	if _, _, err := w.work.Consume(context.Background()); err != nil {
		t.Fatalf("consume: %v", err)
	}
	if w.epoch(t) != 0 {
		t.Error("another target's issue advanced the epoch")
	}
	if len(w.advances.rows) != 0 {
		t.Errorf("a foreign issue recorded an advance: %v", w.advances.rows)
	}
}

func TestTheEpicReopeningAdvancesWithItsOwnCause(t *testing.T) {
	// "any epic reopen forces a fresh completion" — deferred work activating,
	// or an active subtree attached. The cause differs from a ticket reopen
	// because recovery reconciles them differently.
	w := watcher(t, reopenOf("epic-1"), map[string]string{"epic-1": "open"}).stamped(t)
	if _, _, err := w.work.Consume(context.Background()); err != nil {
		t.Fatalf("consume: %v", err)
	}
	got := w.advances.rows[planner.EventAdvanceKey("/spec", "E1")]
	if got.Cause != planner.CauseDeferredActivation {
		t.Errorf("the epic's reopen records cause %q", got.Cause)
	}
}

func TestATargetWithNothingToInvalidateAdvancesNothing(t *testing.T) {
	// Mid-build, tickets move between statuses constantly. Advancing on each
	// would rotate the epoch — and every key derived from it — for no reason,
	// and there is no completion to invalidate.
	w := watcher(t, reopenOf("issue-1"), map[string]string{"issue-1": "open"}).
		plans(t, "issue-1")
	if _, _, err := w.work.Consume(context.Background()); err != nil {
		t.Fatalf("consume: %v", err)
	}
	if w.epoch(t) != 0 {
		t.Errorf("an in-flight build advanced its epoch to %d", w.epoch(t))
	}
}

func TestAClaimAwaitingAHumanIsInvalidatedByReturningWork(t *testing.T) {
	// They are being asked about a build that has changed underneath them.
	w := watcher(t, reopenOf("issue-1"), map[string]string{"issue-1": "open"}).
		plans(t, "issue-1")
	if err := w.claims.Upsert(context.Background(), planner.CompletionClaim{
		TargetKey: "/spec", State: planner.CompletionSubmitted, Epoch: 0,
		ReviewID: "review-1",
	}); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	if _, _, err := w.work.Consume(context.Background()); err != nil {
		t.Fatalf("consume: %v", err)
	}
	if w.epoch(t) != 1 {
		t.Error("a claim awaiting a human was not invalidated")
	}
}

func TestAStaleClaimIsNotInvalidatedAgain(t *testing.T) {
	// It is already spent. Advancing for it would rotate the epoch a second
	// time for one piece of returning work.
	w := watcher(t, reopenOf("issue-1"), map[string]string{"issue-1": "open"}).
		plans(t, "issue-1")
	if err := w.claims.Upsert(context.Background(), planner.CompletionClaim{
		TargetKey: "/spec", State: planner.CompletionStale, Epoch: 0,
	}); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	if _, _, err := w.work.Consume(context.Background()); err != nil {
		t.Fatalf("consume: %v", err)
	}
	if w.epoch(t) != 0 {
		t.Error("a spent claim was invalidated again")
	}
}

func TestANonStatusEventAdvancesNothing(t *testing.T) {
	// Creations and relation changes are ATTRIBUTION's business, and
	// attribution is a lifecycle of its own. Advancing on them without it
	// would rotate the epoch for work that may belong to another target.
	w := watcher(t, []planner.WorkEvent{
		{ID: "E1", Kind: planner.KindCreated, Subject: "issue-77"},
	}, map[string]string{"issue-77": "open"}).stamped(t)
	if _, _, err := w.work.Consume(context.Background()); err != nil {
		t.Fatalf("consume: %v", err)
	}
	if w.epoch(t) != 0 {
		t.Error("a creation advanced the epoch without attribution")
	}
}

func TestAReplayedEventAdvancesNothingTwice(t *testing.T) {
	w := watcher(t, reopenOf("issue-1"), map[string]string{"issue-1": "open"}).
		stamped(t).plans(t, "issue-1")
	for range 3 {
		if _, _, err := w.work.Consume(context.Background()); err != nil {
			t.Fatalf("consume: %v", err)
		}
	}
	if w.epoch(t) != 1 {
		t.Errorf("a replayed event advanced the epoch to %d", w.epoch(t))
	}
}

func TestTheCursorDoesNotMoveUntilTheCallerHasActed(t *testing.T) {
	// The caller still has work to do with what Consume returned. A cursor
	// advanced here is advanced before any of it.
	w := watcher(t, nil, nil)
	_, next, err := w.work.Consume(context.Background())
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if at := w.cursors.at[w.work.Name()]; at != "" {
		t.Errorf("the cursor advanced to %q inside Consume", at)
	}
	if next != "c-1" {
		t.Fatalf("Consume reported next cursor %q", next)
	}
	if err := w.work.Advance(context.Background(), next); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if at := w.cursors.at[w.work.Name()]; at != "c-1" {
		t.Errorf("the cursor is at %q after the caller acted", at)
	}
}

func TestAnEmptyCursorAdvancesNothing(t *testing.T) {
	w := watcher(t, nil, nil)
	if err := w.work.Advance(context.Background(), ""); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if at := w.cursors.at[w.work.Name()]; at != "" {
		t.Errorf("an empty cursor wrote %q", at)
	}
}

func TestTheCursorIsScopedToTheActorAndTheTarget(t *testing.T) {
	// The feed is project-wide and each target skips what is not its own, so
	// one shared cursor would have the first consumer advance past every
	// other target's events — never offered again.
	a := planner.Work{TargetKey: "/a", Actor: "actor-1"}
	b := planner.Work{TargetKey: "/b", Actor: "actor-1"}
	c := planner.Work{TargetKey: "/a", Actor: "actor-2"}
	if a.Name() == b.Name() || a.Name() == c.Name() {
		t.Errorf("cursors collide: %q %q %q", a.Name(), b.Name(), c.Name())
	}
}

func TestAnUnreadableFeedStopsTheWatcher(t *testing.T) {
	w := watcher(t, nil, nil)
	w.work.Feed = &workFeed{err: errors.New("tracker unavailable")}
	if _, _, err := w.work.Consume(context.Background()); err == nil {
		t.Fatal("an unreachable tracker read as an empty feed")
	}
}

func TestAnUnreadableTicketSetStopsTheWatcher(t *testing.T) {
	// "I could not tell whether this issue is mine" is not "it is not". A
	// skipped event is a reopen the cursor then moves past forever.
	w := watcher(t, reopenOf("issue-1"), nil).stamped(t).plans(t, "issue-1")
	w.tickets.err = errors.New("disk full")
	if _, _, err := w.work.Consume(context.Background()); err == nil {
		t.Fatal("an unreadable ticket set read as a foreign issue")
	}
}

func TestAnUnreadableClaimStopsTheWatcher(t *testing.T) {
	w := watcher(t, reopenOf("issue-1"), map[string]string{"issue-1": "open"}).
		plans(t, "issue-1")
	w.claims.err = errors.New("disk full")
	if _, _, err := w.work.Consume(context.Background()); err == nil {
		t.Fatal("an unreadable claim read as nothing to invalidate")
	}
}
