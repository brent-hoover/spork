package planner_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"kriya/internal/fakes"
	"kriya/internal/planner"
)

// memSteps is an in-memory StepStore.
type memSteps struct {
	rows    map[string][]planner.Step
	writes  int
	readErr error
}

func newMemSteps() *memSteps { return &memSteps{rows: map[string][]planner.Step{}} }

func (m *memSteps) Write(_ context.Context, steps []planner.Step) error {
	m.writes++
	for _, step := range steps {
		set := m.rows[step.Plan]
		// DO NOTHING on conflict, like the real store: a re-derived sequence
		// is identical, and overwriting would reset durable step state.
		if step.Seq < len(set) {
			continue
		}
		m.rows[step.Plan] = append(set, step)
	}
	return nil
}

func (m *memSteps) ForPlan(_ context.Context, plan string) ([]planner.Step, error) {
	if m.readErr != nil {
		return nil, m.readErr
	}
	return m.rows[plan], nil
}

func (m *memSteps) Mark(_ context.Context, plan string, seq int, state, issue string) error {
	set := m.rows[plan]
	if seq >= len(set) {
		return errors.New("no such step")
	}
	set[seq].State = state
	if issue != "" {
		set[seq].Issue = issue
	}
	return nil
}

// resuming builds an Intaker with every durable store a resume needs.
func resuming(t *testing.T, reply string) (planner.Intaker, *countingTracker, *memSteps, *fakes.Agent) {
	t.Helper()
	in, tr, ag := decomposer(t, reply)
	steps := newMemSteps()
	// ONE database for plans and heads, because production has one: the
	// replacement CAS reads plan rows inside its own transaction, so an
	// in-memory plan store beside a SQLite head store leaves the CAS unable
	// to see any plan at all — and every verdict it reaches is meaningless.
	//
	// A REAL head store, not nil. Leaving it out let the resume tests pass
	// while resumption re-entered the CAS and buried the plan historical —
	// the whole path they exist to cover was skipped.
	db := sqlDB(t, planner.PlanMigration, planner.PlanKeyMigration, planner.HeadMigration)
	in.Plans, in.Steps, in.Tickets = planner.SQLPlans{DB: db}, steps, &planningTickets{}
	in.Heads = planner.SQLHeads{DB: db}
	ag.Repeat = true
	return in, tr, steps, ag
}

// planKeyFor derives the key production would derive for the test target.
//
// From target().ProjectKey and the snapshot's hash — the same inputs
// Decompose uses. Hand-writing the parts made two tests look for a plan that
// was never keyed that way, and both then measured nothing.
func planKeyFor(generation int) string {
	return planner.DecompositionKey(
		target().ProjectKey, target().TargetKey, snapshotWith(twoCriteria).Hash, generation)
}

const threeTicketPlan = `{"tickets":[
  {"title":"spike: which store","body":"","kind":"spike","criteria":["AC-bad-url"],"blocks":["AC-bad-url"]},
  {"title":"skeleton","body":"","kind":"implementation","skeleton":true,
   "criteria":["AC-valid-url"],"layers":["http","store"]},
  {"title":"later","body":"","kind":"implementation",
   "criteria":["AC-bad-url"],"layers":["http","store"]}]}`

func TestTheWholeSequenceIsPersistedBeforeAnyTrackerCall(t *testing.T) {
	// The payload is what makes a resume a replay. Written after the first
	// call, a crash in that window leaves mutations that landed with nothing
	// recording that they were even planned.
	in, tr, steps, _ := resuming(t, threeTicketPlan)
	tr.failIssue = errors.New("sutra unreachable")

	if _, err := in.Decompose(context.Background(), target(), snapshotWith(twoCriteria), 1, "actor"); err == nil {
		t.Fatal("expected the decomposition to fail at the first create")
	}
	recorded, err := steps.ForPlan(context.Background(), planKeyFor(1))
	if err != nil {
		t.Fatalf("read sequence: %v", err)
	}
	// Three creates, three parents, one block, three assigns.
	if len(recorded) != 10 {
		t.Fatalf("the sequence has %d steps, want the whole plan's 10", len(recorded))
	}
	// Every step carries its key, derived once and persisted. A recovery
	// deriving them afresh could drift and create duplicates.
	for _, step := range recorded {
		if step.Key == "" {
			t.Errorf("step %d (%s) carries no idempotency key", step.Seq, step.Kind)
		}
	}
}

func TestAResumeReplaysWithoutAskingThePMAgain(t *testing.T) {
	// An agent is not a pure function. A second answer can differ, and the
	// differing request would then be sent under a key the first answer
	// already settled — sutra returns the ORIGINAL, and the plan silently
	// becomes a mixture of two decompositions.
	in, tr, _, ag := resuming(t, threeTicketPlan)

	tr.assignErr = errors.New("tracker unavailable")
	if _, err := in.Decompose(context.Background(), target(), snapshotWith(twoCriteria), 1, "actor"); err == nil {
		t.Fatal("expected the decomposition to fail in the assignment phase")
	}
	callsAfterCrash := len(ag.Requests)

	// The crash left the plan ACTIVE without its completed stamp. The retry
	// resolves to it and must resume.
	tr.assignErr = nil
	if _, err := in.Decompose(context.Background(), target(), snapshotWith(twoCriteria), 1, "actor"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := len(ag.Requests); got != callsAfterCrash {
		t.Errorf("the resume asked the PM again: %d calls, was %d", got, callsAfterCrash)
	}
}

func TestAResumeDoesNotRepeatCompletedSteps(t *testing.T) {
	// Replaying a completed create is safe — sutra returns the original — but
	// only because the key is right. Skipping them is what keeps a resume
	// proportional to what is left rather than to the whole plan.
	in, tr, steps, _ := resuming(t, threeTicketPlan)
	tr.assignErr = errors.New("tracker unavailable")
	ctx := context.Background()
	if _, err := in.Decompose(ctx, target(), snapshotWith(twoCriteria), 1, "actor"); err == nil {
		t.Fatal("expected the assignment phase to fail")
	}
	issuesBefore := tr.issues

	tr.assignErr = nil
	if _, err := in.Decompose(ctx, target(), snapshotWith(twoCriteria), 1, "actor"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if tr.issues != issuesBefore {
		t.Errorf("the resume created %d more issues; every create had completed",
			tr.issues-issuesBefore)
	}

	recorded, _ := steps.ForPlan(ctx, planKeyFor(1))
	for _, step := range recorded {
		if step.State != planner.StepComplete {
			t.Errorf("step %d (%s) is %q after a successful resume", step.Seq, step.Kind, step.State)
		}
	}
}

func TestAPlanWithNoRecordedSequenceCannotResume(t *testing.T) {
	// Resuming from nothing would mean re-deriving the plan, which means
	// asking the PM again. Refusing is the honest answer: the durable state
	// this plan needs is not there.
	in, _, _, _ := resuming(t, threeTicketPlan)
	plans := in.Plans
	if err := plans.Upsert(context.Background(), planner.Plan{
		Key: planKeyFor(1), TargetKey: "/spec", SpecHash: "hash1234567890abcdef",
		Generation: 1, State: planner.PlanActive,
	}); err != nil {
		t.Fatalf("seed plan: %v", err)
	}

	_, err := in.Decompose(context.Background(), target(), snapshotWith(twoCriteria), 1, "actor")
	if err == nil {
		t.Fatal("a plan with no sequence resumed anyway")
	}
	if !strings.Contains(err.Error(), "no recorded sequence") {
		t.Errorf("the error %q does not say the sequence is missing", err)
	}
}

func TestATwinThatLosesTheClaimBecomesARetry(t *testing.T) {
	// Resolve is a READ, so two concurrent first requests for one key can
	// both find no row and both believe they are fresh. Only one insert can
	// win; the loser must become an ordinary same-key retry rather than a
	// second decomposition overwriting the first — or a build files two sets
	// of tickets for one plan.
	in, tr, _, ag := resuming(t, threeTicketPlan)
	ctx := context.Background()

	// The twin got there first and finished.
	if _, err := in.Decompose(ctx, target(), snapshotWith(twoCriteria), 1, "actor"); err != nil {
		t.Fatalf("the twin's decomposition: %v", err)
	}
	issues, calls := tr.issues, len(ag.Requests)

	// This request resolved fresh a moment earlier and only now claims. It
	// loses, re-resolves, and answers with the twin's plan.
	got, err := in.Decompose(ctx, target(), snapshotWith(twoCriteria), 1, "actor")
	if err != nil {
		t.Fatalf("the loser: %v", err)
	}
	if tr.issues != issues {
		t.Errorf("the loser created %d more issues", tr.issues-issues)
	}
	if len(ag.Requests) != calls {
		t.Errorf("the loser asked the PM again")
	}
	if len(got) != 3 {
		t.Errorf("the loser answered with %d tickets; the plan has 3", len(got))
	}
}

func TestAClaimedPlanThatVanishesIsRefused(t *testing.T) {
	// The row was there a moment ago. Gone now means something deleted a plan
	// mid-decomposition, and decomposing again would race the twin that is
	// already running.
	in, _, _, _ := resuming(t, threeTicketPlan)
	in.Plans = &vanishingPlans{}

	_, err := in.Decompose(context.Background(), target(), snapshotWith(twoCriteria), 1, "actor")
	if err == nil {
		t.Fatal("a plan that vanished after being claimed decomposed anyway")
	}
	if !strings.Contains(err.Error(), "vanished") {
		t.Errorf("the error %q does not say the plan vanished", err)
	}
}

// vanishingPlans always reports the key taken and then finds no row.
type vanishingPlans struct{}

func (vanishingPlans) Upsert(context.Context, planner.Plan) error { return nil }

func (vanishingPlans) Find(context.Context, string) (planner.Plan, bool, error) {
	return planner.Plan{}, false, nil
}

func (vanishingPlans) ByKey(context.Context, string) (planner.Plan, bool, error) {
	return planner.Plan{}, false, nil
}

func (vanishingPlans) Claim(context.Context, planner.Plan) (bool, error) { return false, nil }

func TestAPlanStillPendingResumesThroughTheCAS(t *testing.T) {
	// The head is moved BEFORE the plan is marked active, so a crash between
	// those two writes leaves a pending plan that already OWNS the head. Its
	// resume re-enters the CAS and must win — comparing it against itself
	// makes it an equal-generation loser, buried historical, with the head
	// left pointing at a terminal plan and the fence never lowered.
	//
	// A CAS that merely FAILED is not this case: no head was installed, so a
	// resume bootstraps and wins trivially. The state has to be seeded.
	in, tr, _, ag := resuming(t, threeTicketPlan)
	ctx := context.Background()
	// Reached through the production path: a decomposition that fails at its
	// first create has claimed its key, written its sequence and taken the
	// head. Rewinding the plan row to pending reproduces exactly the durable
	// state a crash between the head move and the activation write leaves.
	tr.failIssue = errors.New("sutra unreachable")
	if _, err := in.Decompose(ctx, target(), snapshotWith(twoCriteria), 1, "actor"); err == nil {
		t.Fatal("expected the first create to fail")
	}
	tr.failIssue = nil
	plan, found, err := in.Plans.ByKey(ctx, planKeyFor(1))
	if err != nil || !found {
		t.Fatalf("no plan row: %v found=%v", err, found)
	}
	plan.State = planner.PlanPending
	if err := in.Plans.Upsert(ctx, plan); err != nil {
		t.Fatalf("rewind the plan: %v", err)
	}
	if head, _, _ := in.Heads.Head(ctx, "/spec"); head.Current != plan.Key {
		t.Fatalf("the seeded plan does not own the head")
	}

	calls := len(ag.Requests)
	tickets, err := in.Decompose(ctx, target(), snapshotWith(twoCriteria), 1, "actor")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if len(ag.Requests) != calls {
		t.Error("the resume asked the PM again")
	}
	if len(tickets) != 3 {
		t.Errorf("the resume produced %d tickets", len(tickets))
	}
	final, _, err := in.Plans.ByKey(ctx, plan.Key)
	if err != nil {
		t.Fatalf("read plan: %v", err)
	}
	if final.State != planner.PlanActive || !final.Completed {
		t.Errorf("the resumed plan is %q completed=%v", final.State, final.Completed)
	}
	// And the fence it raised must have come down.
	head, _, err := in.Heads.Head(ctx, "/spec")
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if head.Fence != 0 {
		t.Errorf("the fence is %d after the resume completed the plan", head.Fence)
	}
}

func TestACompletedPlanBehindARaisedFenceStillActivates(t *testing.T) {
	// Activation is a SEPARATE write from the completed stamp, so a crash
	// between them leaves a whole plan behind a raised fence. Every later
	// retry then resolves "already complete" and returns without lowering it
	// — pops refused forever, for a plan that finished.
	in, _, _, _ := resuming(t, threeTicketPlan)
	ctx := context.Background()
	if _, err := in.Decompose(ctx, target(), snapshotWith(twoCriteria), 1, "actor"); err != nil {
		t.Fatalf("decompose: %v", err)
	}

	// Reproduce the crash window: the plan is stamped whole, the fence is up.
	heads := in.Heads.(planner.SQLHeads)
	if _, err := heads.DB.ExecContext(ctx,
		`UPDATE plan_head SET fence = 1 WHERE target_key = ?`, "/spec"); err != nil {
		t.Fatalf("raise the fence: %v", err)
	}

	if _, err := in.Decompose(ctx, target(), snapshotWith(twoCriteria), 1, "actor"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	head, _, err := heads.Head(ctx, "/spec")
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if head.Fence != 0 {
		t.Errorf("the fence is still %d after a retry of a completed plan", head.Fence)
	}
}
