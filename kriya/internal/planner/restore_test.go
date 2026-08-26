package planner_test

import (
	"context"
	"strings"
	"testing"

	"kriya/internal/planner"
)

func restorer(t *testing.T) (planner.Restorer, planner.SQLPlans, planner.SQLHeads, planner.SQLSteps) {
	t.Helper()
	db := sqlDB(t, planner.PlanMigration, planner.PlanKeyMigration,
		planner.PlanPredecessorMigration, planner.HeadMigration, planner.StepMigration)
	plans, heads, steps := planner.SQLPlans{DB: db}, planner.SQLHeads{DB: db}, planner.SQLSteps{DB: db}
	return planner.Restorer{Plans: plans, Heads: heads, Steps: steps}, plans, heads, steps
}

// parked writes a plan awaiting the operator with the given step progress.
func parked(t *testing.T, plans planner.SQLPlans, steps planner.SQLSteps,
	key string, generation int, states ...string,
) planner.Plan {
	t.Helper()
	ctx := context.Background()
	p := planner.Plan{
		Key: key, TargetKey: "/spec", SpecHash: "h", Generation: generation,
		State: planner.PlanAwaitingOperator, Error: "the tracker refused the epic",
	}
	if err := plans.Upsert(ctx, p); err != nil {
		t.Fatalf("park plan: %v", err)
	}
	rows := make([]planner.Step, len(states))
	for n, state := range states {
		rows[n] = planner.Step{
			Plan: key, Seq: n, Ordinal: n, Other: -1,
			Kind: planner.StepCreate, Key: "k", State: state,
		}
	}
	if err := steps.Write(ctx, rows); err != nil {
		t.Fatalf("write sequence: %v", err)
	}
	return p
}

func TestOnlyAParkedPlanIsRestored(t *testing.T) {
	// Every other state is either running or terminal. Restoring an active
	// plan would reset progress under a decomposition still making it, and
	// restoring a historical one would revive what the CAS buried.
	r, plans, _, _ := restorer(t)
	ctx := context.Background()
	for _, state := range []string{
		planner.PlanActive, planner.PlanPending,
		planner.PlanSuperseded, planner.PlanHistorical,
	} {
		key := "plan-" + state
		if err := plans.Upsert(ctx, planner.Plan{
			Key: key, TargetKey: "/spec", SpecHash: "h", State: state,
		}); err != nil {
			t.Fatalf("seed %s: %v", state, err)
		}
		if _, err := r.Restore(ctx, key); err == nil {
			t.Errorf("a %s plan was restored", state)
		}
	}
}

func TestRestoringAPlanThatDoesNotExistIsRefused(t *testing.T) {
	r, _, _, _ := restorer(t)
	_, err := r.Restore(context.Background(), "plan-missing")
	if err == nil {
		t.Fatal("a plan with no row was restored")
	}
	if !strings.Contains(err.Error(), "no plan") {
		t.Errorf("the error %q does not say the plan is missing", err)
	}
}

func TestAHeadParkedWithNothingIssuedReturnsToPending(t *testing.T) {
	r, plans, heads, steps := restorer(t)
	ctx := context.Background()
	p := parked(t, plans, steps, "plan-1", 1, planner.StepPending, planner.StepPending)
	if _, err := heads.DB.ExecContext(ctx,
		`INSERT INTO plan_head (target_key, current, generation, fence) VALUES (?, ?, 1, 1)`,
		"/spec", p.Key); err != nil {
		t.Fatalf("seed head: %v", err)
	}

	got, err := r.Restore(ctx, p.Key)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got.State != planner.PlanPending {
		t.Errorf("the plan returned to %q, want pending", got.State)
	}
	if got.Error != "" {
		t.Error("the restored plan still carries its park cause")
	}
}

func TestAHeadParkedMidPhaseReturnsToActiveNotWhole(t *testing.T) {
	// Recomputed from per-step progress, never assumed. A plan restored as
	// whole because it once looked whole would arm completion detection over
	// a ticket set still missing its last mutations.
	r, plans, heads, steps := restorer(t)
	ctx := context.Background()
	p := parked(t, plans, steps, "plan-1", 1, planner.StepComplete, planner.StepIssued)
	if _, err := heads.DB.ExecContext(ctx,
		`INSERT INTO plan_head (target_key, current, generation, fence) VALUES (?, ?, 1, 1)`,
		"/spec", p.Key); err != nil {
		t.Fatalf("seed head: %v", err)
	}

	got, err := r.Restore(ctx, p.Key)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got.State != planner.PlanActive {
		t.Errorf("the plan returned to %q, want active", got.State)
	}
	if got.Completed {
		t.Error("a plan with an unfinished step came back stamped whole")
	}
}

func TestAHeadWhoseEveryStepLandedComesBackWhole(t *testing.T) {
	// The other half: recomputation must be able to say yes, or a plan that
	// genuinely finished could never complete.
	r, plans, heads, steps := restorer(t)
	ctx := context.Background()
	p := parked(t, plans, steps, "plan-1", 1, planner.StepComplete, planner.StepComplete)
	if _, err := heads.DB.ExecContext(ctx,
		`INSERT INTO plan_head (target_key, current, generation, fence) VALUES (?, ?, 1, 1)`,
		"/spec", p.Key); err != nil {
		t.Fatalf("seed head: %v", err)
	}

	got, err := r.Restore(ctx, p.Key)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got.State != planner.PlanActive || !got.Completed {
		t.Errorf("the plan returned %q completed=%v", got.State, got.Completed)
	}
}

// installedHead makes a plan the activated head at a generation.
func installedHead(t *testing.T, plans planner.SQLPlans, heads planner.SQLHeads,
	key string, generation int,
) {
	t.Helper()
	ctx := context.Background()
	p := planner.Plan{
		Key: key, TargetKey: "/spec", SpecHash: "h", Generation: generation,
		State: planner.PlanActive, Completed: true,
	}
	if err := plans.Upsert(ctx, p); err != nil {
		t.Fatalf("seed head plan: %v", err)
	}
	if verdict, err := heads.Replace(ctx, p); err != nil || !verdict.Won {
		t.Fatalf("install head: %v won=%v", err, verdict.Won)
	}
	if err := heads.Activate(ctx, key); err != nil {
		t.Fatalf("activate: %v", err)
	}
}

func TestAnEligibleParkedCandidateReentersTheCASAndRepoints(t *testing.T) {
	r, plans, heads, steps := restorer(t)
	ctx := context.Background()
	installedHead(t, plans, heads, "plan-head", 2)
	p := parked(t, plans, steps, "plan-candidate", 4, planner.StepPending)

	got, err := r.Restore(ctx, p.Key)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got.State != planner.PlanPending {
		t.Errorf("the winning retry is %q, want pending", got.State)
	}
	// The predecessor pointer is what makes the retry a genuine replacement
	// rather than a second head with no history behind it.
	if got.Predecessor != "plan-head" {
		t.Errorf("the retried candidate points at %q", got.Predecessor)
	}
	head, _, err := heads.Head(ctx, "/spec")
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if head.Current != p.Key {
		t.Errorf("the head is %q after a winning retry", head.Current)
	}
	beaten, _, err := plans.ByKey(ctx, "plan-head")
	if err != nil {
		t.Fatalf("read the beaten head: %v", err)
	}
	if beaten.State != planner.PlanSuperseded {
		t.Errorf("the beaten head is %q", beaten.State)
	}
}

func TestAnOutrunParkedCandidateGoesTerminalWithoutTheCAS(t *testing.T) {
	// Observing is ENOUGH. A candidate the head has outrun is dead the moment
	// anyone looks, and the operator's inbox should not have to enter the CAS
	// to find out — nor be offered a retry that can only be rejected.
	r, plans, heads, steps := restorer(t)
	ctx := context.Background()
	installedHead(t, plans, heads, "plan-head", 9)
	p := parked(t, plans, steps, "plan-outrun", 3, planner.StepPending)

	got, err := r.Restore(ctx, p.Key)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got.State != planner.PlanHistorical {
		t.Errorf("the outrun candidate is %q, want historical", got.State)
	}
	if got.Error == "" {
		t.Error("the terminal candidate records no cause")
	}
	// The head is untouched.
	head, _, err := heads.Head(ctx, "/spec")
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if head.Current != "plan-head" {
		t.Errorf("observing an outrun candidate moved the head to %q", head.Current)
	}
	// And it is out of the actionable set: restoring it again is refused.
	if _, err := r.Restore(ctx, p.Key); err == nil {
		t.Error("a terminal historical plan was restored again")
	}
}

func TestACandidateThatLosesItsRetryIsReportedAsLanded(t *testing.T) {
	// Eligible on paper, but the head's plan is mid-replacement so the CAS
	// refuses it. It must come back with the state Replace landed it in,
	// not with an optimistic pending.
	r, plans, heads, steps := restorer(t)
	ctx := context.Background()
	installedHead(t, plans, heads, "plan-head", 2)
	if err := plans.Upsert(ctx, planner.Plan{
		Key: "plan-head", TargetKey: "/spec", SpecHash: "h", Generation: 2,
		State: planner.PlanPending,
	}); err != nil {
		t.Fatalf("unsettle the head plan: %v", err)
	}
	p := parked(t, plans, steps, "plan-candidate", 4, planner.StepPending)

	got, err := r.Restore(ctx, p.Key)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got.State == planner.PlanPending {
		t.Error("a candidate that lost its retry came back pending")
	}
	if got.State != planner.PlanAwaitingOperator {
		t.Errorf("the losing retry is %q, want %q", got.State, planner.PlanAwaitingOperator)
	}
}

func TestAParkedCandidateWithNoHeadAtAllBootstraps(t *testing.T) {
	// The target's head row was never written — a crash in the very first
	// decomposition. The retry takes the ordinary bootstrap path.
	r, plans, _, steps := restorer(t)
	ctx := context.Background()
	p := parked(t, plans, steps, "plan-first", 1, planner.StepPending)

	got, err := r.Restore(ctx, p.Key)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got.State != planner.PlanPending {
		t.Errorf("the bootstrapped retry is %q, want pending", got.State)
	}
}
