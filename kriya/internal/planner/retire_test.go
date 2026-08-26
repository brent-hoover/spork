package planner_test

import (
	"context"
	"errors"
	"strings"
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
	if err := r.Run(context.Background(), plan, "project-1", nil, "actor"); err != nil {
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

	if err := r.Run(context.Background(), plan, "project-1", nil, "actor"); err != nil {
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

	if err := r.Run(context.Background(), plan, "project-1", nil, "actor"); err != nil {
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

	if err := r.Run(context.Background(), plan, "project-1", nil, "actor"); err != nil {
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

	if err := r.Run(context.Background(), plan, "project-1", nil, "actor"); err != nil {
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
	if err := r.Run(context.Background(), plan, "project-1", nil, "actor"); err != nil {
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

	if err := r.Run(context.Background(), plan, "project-1", carried, "actor"); err != nil {
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
	if err := r.Run(ctx, plan, "project-1", nil, "actor"); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	calls := len(defers.keys)
	if err := r.Run(ctx, plan, "project-1", nil, "actor"); err != nil {
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
	if err := r.Run(context.Background(), plan, "project-1", nil, "actor"); err == nil {
		t.Fatal("retirement continued past an unreadable tracker")
	}
}

func TestRetirementStopsWhenLiveClaimsCannotBeRead(t *testing.T) {
	// Not knowing which builds are live is not "none are". Deferring then
	// cancels work in flight.
	r, _, _, live, plan := retiring(t,
		planner.Ticket{Title: "dropped", IssueID: "issue-1"})
	live.err = errors.New("the run store is unreachable")
	if err := r.Run(context.Background(), plan, "project-1", nil, "actor"); err == nil {
		t.Fatal("retirement continued without knowing what was live")
	}
}

func TestRetirementStopsWhenADeferralIsRefused(t *testing.T) {
	r, _, defers, _, plan := retiring(t,
		planner.Ticket{Title: "dropped", IssueID: "issue-1"})
	defers.deferErr = errors.New("conflict")
	if err := r.Run(context.Background(), plan, "project-1", nil, "actor"); err == nil {
		t.Fatal("retirement continued past a refused deferral")
	}
}

func TestAPlanWithNoTicketsRetiresQuietly(t *testing.T) {
	// A bootstrap's retirement set is empty, and so is that of a plan whose
	// creation phase never ran.
	r, _, defers, _, plan := retiring(t)
	if err := r.Run(context.Background(), plan, "project-1", nil, "actor"); err != nil {
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
	if planner.DeferKey("plan-a", 0, 0) == planner.DeferKey("plan-b", 0, 0) {
		t.Error("two plans share a defer key for the same ordinal")
	}
	if planner.DeferKey("plan-a", 0, 0) == planner.DeferKey("plan-a", 1, 0) {
		t.Error("two ordinals of one plan share a defer key")
	}
	// And stable across attempts: a key that varied per try would make every
	// recovery a fresh transition rather than a replay.
	first, again := planner.DeferKey("plan-a", 0, 0), deferKeyAgain()
	if first != again {
		t.Errorf("one retirement derived two keys: %s then %s", first, again)
	}
}

// deferKeyAgain derives the same key through a separate call site, so
// staticcheck cannot fold the comparison away as a tautology — the claim is
// about determinism across attempts, which is real.
func deferKeyAgain() string { return planner.DeferKey("plan-a", 0, 0) }

// creatingTracker records replayed creation calls.
type creatingTracker struct {
	countingTracker
	replayKeys []string
	replayed   string
}

func (c *creatingTracker) CreateIssue(
	ctx context.Context, project, title, body, actor, key string,
) (string, error) {
	c.replayKeys = append(c.replayKeys, key)
	if c.replayed != "" {
		return c.replayed, nil
	}
	return c.countingTracker.CreateIssue(ctx, project, title, body, actor, key)
}

// retiringWithSteps builds a Retirement that can replay creation steps.
func retiringWithSteps(t *testing.T, stepState string, ticket planner.Ticket) (
	planner.Retirement, *planningTickets, *fakeDeferrer, *creatingTracker, planner.Plan,
) {
	t.Helper()
	ctx := context.Background()
	store, steps := &planningTickets{}, newMemSteps()
	tracker := &creatingTracker{}
	plan := planner.Plan{
		Key: "plan-old", TargetKey: "/spec", SpecHash: "h",
		Generation: 1, State: planner.PlanSuperseded,
	}
	ticket.Plan, ticket.Ordinal = plan.Key, 0
	if err := store.Put(ctx, "/spec", ticket); err != nil {
		t.Fatalf("seed ticket: %v", err)
	}
	if err := steps.Write(ctx, []planner.Step{{
		Plan: plan.Key, Seq: 0, Ordinal: 0, Other: -1,
		Kind: planner.StepCreate, Key: "create-key-0", State: stepState,
	}}); err != nil {
		t.Fatalf("seed sequence: %v", err)
	}
	defers := newFakeDeferrer()
	r := planner.Retirement{
		Tickets: store, Steps: steps, Defer: defers,
		Live: &fakeLive{held: map[string]bool{}}, Tracker: tracker,
	}
	return r, store, defers, tracker, plan
}

func TestAnIssuedCreateIsReplayedBeforeItsRowRetires(t *testing.T) {
	// An empty recorded id is NOT proof the ticket does not exist. The create
	// may have landed and crashed before its id was recorded — the step is
	// marked issued precisely to say "this may have happened". Assuming it
	// never did leaves a real, open sutra issue holding the epic forever
	// while its row is stamped retired.
	r, store, defers, tracker, plan := retiringWithSteps(t,
		planner.StepIssued, planner.Ticket{Title: "orphaned", IssueID: ""})
	tracker.replayed = "issue-recovered"

	if err := r.Run(context.Background(), plan, "project-1", nil, "actor"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	// Replayed under the step's PERSISTED key, so sutra returns the original.
	if len(tracker.replayKeys) != 1 || tracker.replayKeys[0] != "create-key-0" {
		t.Errorf("the replay used keys %v, not the step's persisted one", tracker.replayKeys)
	}
	if defers.status["issue-recovered"] != planner.StatusDeferred {
		t.Error("the recovered issue was never deferred, so it still holds the epic open")
	}
	if got := dispositionOf(t, store, 0); got != planner.DispositionRetired {
		t.Errorf("the recovered row is %q", got)
	}
}

func TestAPendingCreateIsNotReplayed(t *testing.T) {
	// Its step was never sent, so there is genuinely nothing in sutra.
	// Replaying would CREATE the ticket a retirement is trying to be rid of.
	r, store, defers, tracker, plan := retiringWithSteps(t,
		planner.StepPending, planner.Ticket{Title: "never sent", IssueID: ""})

	if err := r.Run(context.Background(), plan, "project-1", nil, "actor"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if len(tracker.replayKeys) != 0 {
		t.Error("a creation step that was never sent was replayed, creating the ticket")
	}
	if len(defers.keys) != 0 {
		t.Error("a ticket that does not exist was deferred")
	}
	if got := dispositionOf(t, store, 0); got != planner.DispositionRetired {
		t.Errorf("the unsent row is %q", got)
	}
}

func TestATicketSutraReportsInProgressIsBoundNotDeferred(t *testing.T) {
	// The live-run read happens before the status read, so a build can start
	// in between. sutra's own status is the authority, and it is fresher.
	r, store, defers, _, plan := retiring(t,
		planner.Ticket{Title: "just claimed", IssueID: "issue-1"})
	defers.status["issue-1"] = planner.StatusInProgress

	if err := r.Run(context.Background(), plan, "project-1", nil, "actor"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if len(defers.keys) != 0 {
		t.Error("a ticket sutra reports in-progress was deferred, cancelling its build")
	}
	if got := dispositionOf(t, store, 0); got != planner.DispositionBound {
		t.Errorf("the in-progress ticket is %q", got)
	}
}

// conflictOnce refuses the first deferral, then behaves normally.
type conflictOnce struct {
	*fakeDeferrer
	refused bool
	// after is the status the fresh read reports once the conflict has
	// happened, standing in for whatever moved underneath.
	after string
}

func (c *conflictOnce) Status(ctx context.Context, issue string) (string, error) {
	if c.refused && c.after != "" {
		return c.after, nil
	}
	return c.fakeDeferrer.Status(ctx, issue)
}

func (c *conflictOnce) Defer(ctx context.Context, issue, expect, actor, key string) error {
	if !c.refused {
		c.refused = true
		// Recorded here, because the delegate never sees this attempt.
		c.keys = append(c.keys, key)
		return errors.New("conflict: expected status " + expect)
	}
	return c.fakeDeferrer.Defer(ctx, issue, expect, actor, key)
}

func TestAConflictedDeferralRetriesUnderAFreshKey(t *testing.T) {
	// sutra settles a REJECTED request under its idempotency key. Retrying
	// under the key that conflicted replays the cached 409 forever, whatever
	// the ticket's status by then.
	store := &planningTickets{}
	plan := planner.Plan{Key: "plan-old", TargetKey: "/spec", State: planner.PlanSuperseded}
	if err := store.Put(context.Background(), "/spec", planner.Ticket{
		Title: "moved", IssueID: "issue-1", Plan: plan.Key, Ordinal: 0,
	}); err != nil {
		t.Fatalf("seed ticket: %v", err)
	}
	defers := &conflictOnce{fakeDeferrer: newFakeDeferrer()}
	r := planner.Retirement{
		Tickets: store, Defer: defers, Live: &fakeLive{held: map[string]bool{}},
	}

	if err := r.Run(context.Background(), plan, "project-1", nil, "actor"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if len(defers.keys) != 2 {
		t.Fatalf("the conflicted deferral was attempted %d time(s)", len(defers.keys))
	}
	if defers.keys[0] == defers.keys[1] {
		t.Error("the retry reused the key that conflicted, replaying the cached rejection")
	}
}

func TestAConflictRevealingAFreshBuildLeavesItBound(t *testing.T) {
	// The conflict says the status moved. If it moved to in-progress, a build
	// started while the deferral was in flight — and forcing it through would
	// cancel that build.
	store := &planningTickets{}
	plan := planner.Plan{Key: "plan-old", TargetKey: "/spec", State: planner.PlanSuperseded}
	if err := store.Put(context.Background(), "/spec", planner.Ticket{
		Title: "raced", IssueID: "issue-1", Plan: plan.Key, Ordinal: 0,
	}); err != nil {
		t.Fatalf("seed ticket: %v", err)
	}
	defers := &conflictOnce{fakeDeferrer: newFakeDeferrer(), after: planner.StatusInProgress}
	r := planner.Retirement{
		Tickets: store, Defer: defers, Live: &fakeLive{held: map[string]bool{}},
	}

	if err := r.Run(context.Background(), plan, "project-1", nil, "actor"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if got := dispositionOf(t, store, 0); got != planner.DispositionBound {
		t.Errorf("a ticket claimed mid-deferral is %q, want %q", got, planner.DispositionBound)
	}
}

func TestTheDeferKeyVariesWithTheAttempt(t *testing.T) {
	if planner.DeferKey("plan-a", 0, 0) == planner.DeferKey("plan-a", 0, 1) {
		t.Error("two attempts share a defer key, so the second replays the first's rejection")
	}
}

func TestDeferAttemptsNeverRepeatAcrossPasses(t *testing.T) {
	// sutra settles a REJECTED request under its key, so a conflicted
	// deferral poisons that key permanently. An attempt counter living only
	// inside one pass restarted at zero on the next — re-presenting keys
	// sutra had already refused, forever, so the ticket could never be
	// deferred once its status stabilised and the successor stayed fenced.
	store := &planningTickets{}
	plan := planner.Plan{Key: "plan-old", TargetKey: "/spec", State: planner.PlanSuperseded}
	if err := store.Put(context.Background(), "/spec", planner.Ticket{
		Title: "moving", IssueID: "issue-1", Plan: plan.Key, Ordinal: 0,
	}); err != nil {
		t.Fatalf("seed ticket: %v", err)
	}
	defers := &alwaysConflicts{fakeDeferrer: newFakeDeferrer()}
	r := planner.Retirement{
		Tickets: store, Defer: defers, Live: &fakeLive{held: map[string]bool{}},
	}
	ctx := context.Background()

	// Two passes, each exhausting its attempts.
	for pass := range 2 {
		if err := r.Run(ctx, plan, "project-1", nil, "actor"); err == nil {
			t.Fatalf("pass %d: expected the deferral to fail", pass)
		}
	}
	if len(defers.keys) != 4 {
		t.Fatalf("two passes made %d attempts, want 4", len(defers.keys))
	}
	seen := map[string]bool{}
	for _, key := range defers.keys {
		if seen[key] {
			t.Errorf("key %s was presented twice, replaying a settled rejection", planner.Short(key))
		}
		seen[key] = true
	}
}

// alwaysConflicts refuses every deferral, as a ticket whose status keeps
// moving would.
type alwaysConflicts struct{ *fakeDeferrer }

func (a *alwaysConflicts) Defer(_ context.Context, _, expect, _, key string) error {
	a.keys = append(a.keys, key)
	return errors.New("conflict: expected status " + expect)
}

func TestAnIssuedCreateWithNoTrackerIsRefusedNotAssumedAbsent(t *testing.T) {
	// A missing tracker is NOT proof the issue does not exist. Treated as
	// one, the row is consumed and its real, open sutra issue holds the epic
	// forever — silently, which is the worst version.
	r, _, _, _, plan := retiringWithSteps(t,
		planner.StepIssued, planner.Ticket{Title: "orphaned", IssueID: ""})
	r.Tracker = nil

	err := r.Run(context.Background(), plan, "project-1", nil, "actor")
	if err == nil {
		t.Fatal("an issued create was consumed with no tracker to replay it")
	}
	if !strings.Contains(err.Error(), "no tracker") {
		t.Errorf("the error %q does not name the missing tracker", err)
	}
}
