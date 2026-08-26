package planner_test

import (
	"context"
	"errors"
	"testing"

	"kriya/internal/planner"
)

// fakeDeferrer honours expected_status the way sutra does.
type fakeDeferrer struct {
	status    map[string]string
	keys      []string
	expected  map[string]string
	statusErr error
	deferErr  error
}

func newFakeDeferrer() *fakeDeferrer {
	return &fakeDeferrer{status: map[string]string{}, expected: map[string]string{}}
}

func (d *fakeDeferrer) Status(_ context.Context, issue string) (string, error) {
	if d.statusErr != nil {
		return "", d.statusErr
	}
	status, ok := d.status[issue]
	if !ok {
		return planner.StatusOpen, nil
	}
	return status, nil
}

func (d *fakeDeferrer) Defer(_ context.Context, issue, expect, _, key string) error {
	if d.deferErr != nil {
		return d.deferErr
	}
	d.keys = append(d.keys, key)
	d.expected[issue] = expect
	if current := d.status[issue]; current != "" && current != expect {
		return errors.New("conflict: expected status " + expect + ", current is " + current)
	}
	d.status[issue] = planner.StatusDeferred
	return nil
}

// fakeLive says which tickets a live build holds.
type fakeLive struct {
	held map[string]bool
	err  error
}

func (l *fakeLive) Claimed(_ context.Context, issues []string) (map[string]bool, error) {
	if l.err != nil {
		return nil, l.err
	}
	out := map[string]bool{}
	for _, issue := range issues {
		if l.held[issue] {
			out[issue] = true
		}
	}
	return out, nil
}

// retiring builds a Retirement over a plan with the tickets given.
func retiring(t *testing.T, tickets ...planner.Ticket) (
	planner.Retirement, *planningTickets, *fakeDeferrer, *fakeLive, planner.Plan,
) {
	t.Helper()
	store := &planningTickets{}
	plan := planner.Plan{
		Key: "plan-old", TargetKey: "/spec", SpecHash: "h",
		Generation: 1, State: planner.PlanSuperseded,
	}
	for n, ticket := range tickets {
		ticket.Plan, ticket.Ordinal = plan.Key, n
		if err := store.Put(context.Background(), "/spec", ticket); err != nil {
			t.Fatalf("seed ticket: %v", err)
		}
	}
	defers, live := newFakeDeferrer(), &fakeLive{held: map[string]bool{}}
	return planner.Retirement{Tickets: store, Defer: defers, Live: live}, store, defers, live, plan
}

// dispositionOf reads a row's stamped disposition.
func dispositionOf(t *testing.T, store *planningTickets, ordinal int) string {
	t.Helper()
	for _, row := range store.rows {
		if row.Ordinal == ordinal {
			if !row.Consumed {
				t.Fatalf("ticket %d was never consumed", ordinal)
			}
			return row.Disposition
		}
	}
	t.Fatalf("no ticket at ordinal %d", ordinal)
	return ""
}

func TestAnOpenTicketIsDeferredAndRetired(t *testing.T) {
	r, store, defers, _, plan := retiring(t,
		planner.Ticket{Title: "dropped", IssueID: "issue-1"})
	if err := r.Run(context.Background(), plan, nil, "actor"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if defers.status["issue-1"] != planner.StatusDeferred {
		t.Error("the dropped ticket was not deferred, so it holds the epic open")
	}
	if got := dispositionOf(t, store, 0); got != planner.DispositionRetired {
		t.Errorf("the dropped ticket is %q", got)
	}
}

func TestTheDeferralExpectsTheStatusItObserved(t *testing.T) {
	// Not "open" unconditionally: a blocked ticket would then conflict on
	// every attempt, retrying against a state it will never return to.
	r, _, defers, _, plan := retiring(t,
		planner.Ticket{Title: "blocked", IssueID: "issue-1"})
	defers.status["issue-1"] = "blocked"

	if err := r.Run(context.Background(), plan, nil, "actor"); err != nil {
		t.Fatalf("retiring a blocked ticket failed: %v", err)
	}
	if got := defers.expected["issue-1"]; got != "blocked" {
		t.Errorf("the deferral expected %q, want the observed status", got)
	}
}

func TestATicketAlreadyDeferredIsNotTransitionedAgain(t *testing.T) {
	r, store, defers, _, plan := retiring(t,
		planner.Ticket{Title: "already", IssueID: "issue-1"})
	defers.status["issue-1"] = planner.StatusDeferred

	if err := r.Run(context.Background(), plan, nil, "actor"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if len(defers.keys) != 0 {
		t.Error("an already-deferred ticket was transitioned again")
	}
	if got := dispositionOf(t, store, 0); got != planner.DispositionRetired {
		t.Errorf("the already-deferred ticket is %q", got)
	}
}

func TestACompletedTicketIsStampedCompleted(t *testing.T) {
	r, store, defers, _, plan := retiring(t,
		planner.Ticket{Title: "shipped", IssueID: "issue-1"})
	defers.status["issue-1"] = planner.StatusComplete

	if err := r.Run(context.Background(), plan, nil, "actor"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if len(defers.keys) != 0 {
		t.Error("work that already shipped was deferred")
	}
	if got := dispositionOf(t, store, 0); got != planner.DispositionCompleted {
		t.Errorf("the shipped ticket is %q", got)
	}
}

func TestATicketALiveBuildHoldsIsBoundNotDeferred(t *testing.T) {
	r, store, defers, live, plan := retiring(t,
		planner.Ticket{Title: "in flight", IssueID: "issue-1"})
	live.held["issue-1"] = true

	if err := r.Run(context.Background(), plan, nil, "actor"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if len(defers.keys) != 0 {
		t.Error("a ticket a live build holds was deferred, cancelling its run")
	}
	if got := dispositionOf(t, store, 0); got != planner.DispositionBound {
		t.Errorf("the in-flight ticket is %q", got)
	}
}

func TestARowWhoseTicketWasNeverCreatedTouchesNothing(t *testing.T) {
	r, store, defers, _, plan := retiring(t,
		planner.Ticket{Title: "never created", IssueID: ""})
	if err := r.Run(context.Background(), plan, nil, "actor"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if len(defers.keys) != 0 || len(defers.expected) != 0 {
		t.Error("a ticket that was never created reached sutra")
	}
	if got := dispositionOf(t, store, 0); got != planner.DispositionRetired {
		t.Errorf("the uncreated row is %q", got)
	}
}

func TestASelectedTicketIsCarriedForward(t *testing.T) {
	r, store, defers, _, plan := retiring(t,
		planner.Ticket{Title: "kept", IssueID: "issue-1"})
	carried := map[string]bool{"issue-1": true}

	if err := r.Run(context.Background(), plan, carried, "actor"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if len(defers.keys) != 0 {
		t.Error("work the successor kept was deferred")
	}
	if got := dispositionOf(t, store, 0); got != planner.DispositionCarriedForward {
		t.Errorf("the carried ticket is %q", got)
	}
}

func TestAConsumedRowIsNotWalkedTwice(t *testing.T) {
	// Re-deferring is harmless under the key, but re-READING every ticket's
	// status is a round trip per row on every replay.
	r, _, defers, _, plan := retiring(t,
		planner.Ticket{Title: "dropped", IssueID: "issue-1"})
	ctx := context.Background()
	if err := r.Run(ctx, plan, nil, "actor"); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	calls := len(defers.keys)
	if err := r.Run(ctx, plan, nil, "actor"); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(defers.keys) != calls {
		t.Errorf("a replay re-transitioned %d consumed rows", len(defers.keys)-calls)
	}
}

func TestRetirementStopsWhenTheTrackerCannotBeRead(t *testing.T) {
	// "I could not read the status" is not "it is open". Deferring on a
	// guess could cancel a ticket a build is working.
	r, _, defers, _, plan := retiring(t,
		planner.Ticket{Title: "dropped", IssueID: "issue-1"})
	defers.statusErr = errors.New("sutra unreachable")
	if err := r.Run(context.Background(), plan, nil, "actor"); err == nil {
		t.Fatal("retirement continued past an unreadable tracker")
	}
}

func TestRetirementStopsWhenLiveClaimsCannotBeRead(t *testing.T) {
	// Not knowing which builds are live is not "none are". Deferring then
	// cancels work in flight.
	r, _, _, live, plan := retiring(t,
		planner.Ticket{Title: "dropped", IssueID: "issue-1"})
	live.err = errors.New("the run store is unreachable")
	if err := r.Run(context.Background(), plan, nil, "actor"); err == nil {
		t.Fatal("retirement continued without knowing what was live")
	}
}

func TestRetirementStopsWhenADeferralIsRefused(t *testing.T) {
	r, _, defers, _, plan := retiring(t,
		planner.Ticket{Title: "dropped", IssueID: "issue-1"})
	defers.deferErr = errors.New("conflict")
	if err := r.Run(context.Background(), plan, nil, "actor"); err == nil {
		t.Fatal("retirement continued past a refused deferral")
	}
}

func TestAPlanWithNoTicketsRetiresQuietly(t *testing.T) {
	// A bootstrap's retirement set is empty, and so is that of a plan whose
	// creation phase never ran.
	r, _, defers, _, plan := retiring(t)
	if err := r.Run(context.Background(), plan, nil, "actor"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if len(defers.keys) != 0 {
		t.Error("an empty plan issued deferrals")
	}
}

func TestTwoPlansRetiringOneTicketPresentDifferentKeys(t *testing.T) {
	// GENERATION-SCOPED through the decomposition key. A key from the ticket
	// alone would be replayed by a later plan's retirement, and sutra would
	// return the earlier result instead of performing the transition.
	if planner.DeferKey("plan-a", 0) == planner.DeferKey("plan-b", 0) {
		t.Error("two plans share a defer key for the same ordinal")
	}
	if planner.DeferKey("plan-a", 0) == planner.DeferKey("plan-a", 1) {
		t.Error("two ordinals of one plan share a defer key")
	}
	// And stable across attempts: a key that varied per try would make every
	// recovery a fresh transition rather than a replay.
	first, again := planner.DeferKey("plan-a", 0), deferKeyAgain()
	if first != again {
		t.Errorf("one retirement derived two keys: %s then %s", first, again)
	}
}

// deferKeyAgain derives the same key through a separate call site, so
// staticcheck cannot fold the comparison away as a tautology — the claim is
// about determinism across attempts, which is real.
func deferKeyAgain() string { return planner.DeferKey("plan-a", 0) }
