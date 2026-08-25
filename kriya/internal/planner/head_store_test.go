package planner_test

import (
	"context"
	"errors"
	"testing"

	"kriya/internal/planner"
)

func headStore(t *testing.T) (planner.SQLHeads, planner.SQLPlans) {
	t.Helper()
	db := sqlDB(t, planner.PlanMigration, planner.PlanKeyMigration, planner.HeadMigration)
	return planner.SQLHeads{DB: db}, planner.SQLPlans{DB: db}
}

// candidate builds a plan for one generation.
func candidate(generation int, state string) planner.Plan {
	return planner.Plan{
		Key:        planner.DecompositionKey("SHORT", "/spec", "hash", generation),
		TargetKey:  "/spec",
		SpecHash:   "hash",
		Generation: generation,
		State:      state,
	}
}

// install writes a plan and makes it the active head.
func install(t *testing.T, heads planner.SQLHeads, plans planner.SQLPlans, p planner.Plan) {
	t.Helper()
	ctx := context.Background()
	if err := plans.Upsert(ctx, p); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	verdict, err := heads.Replace(ctx, p)
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if !verdict.Won {
		t.Fatalf("generation %d did not take the head", p.Generation)
	}
	p.State = planner.PlanActive
	if err := plans.Upsert(ctx, p); err != nil {
		t.Fatalf("activate plan: %v", err)
	}
}

func TestTheFirstHeadIsInstalledFenced(t *testing.T) {
	// Bootstrap is the degenerate case of the same transaction, so it raises
	// the fence like any other head move. Starting unfenced would make the
	// first plan the one case where pops are admitted before activation.
	heads, plans := headStore(t)
	install(t, heads, plans, candidate(1, planner.PlanPending))

	head, found, err := heads.Head(context.Background(), "/spec")
	if err != nil || !found {
		t.Fatalf("head: %v found=%v", err, found)
	}
	if head.Fence != 1 {
		t.Errorf("the first head installed with fence %d, so pops are already admitted", head.Fence)
	}
	if head.Generation != 1 {
		t.Errorf("the first head generation is %d", head.Generation)
	}
}

func TestATargetWithNoHeadHasNoRow(t *testing.T) {
	heads, _ := headStore(t)
	if _, found, err := heads.Head(context.Background(), "/spec"); err != nil || found {
		t.Errorf("an unplanned target has a head row: found=%v err=%v", found, err)
	}
}

func TestReplacingAHeadSupersedesItAndBumps(t *testing.T) {
	heads, plans := headStore(t)
	ctx := context.Background()
	first := candidate(1, planner.PlanPending)
	install(t, heads, plans, first)
	if err := heads.Activate(ctx, first.Key); err != nil {
		t.Fatalf("activate: %v", err)
	}

	second := candidate(2, planner.PlanPending)
	if err := plans.Upsert(ctx, second); err != nil {
		t.Fatalf("write candidate: %v", err)
	}
	verdict, err := heads.Replace(ctx, second)
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if !verdict.Won {
		t.Fatal("a strictly newer candidate lost the CAS")
	}

	// Everything the one transaction promised, checked from the store.
	old, _, err := plans.ByKey(ctx, first.Key)
	if err != nil {
		t.Fatalf("read predecessor: %v", err)
	}
	if old.State != planner.PlanSuperseded {
		t.Errorf("the predecessor is %q, not superseded", old.State)
	}
	if old.SupersededBy != second.Key {
		t.Error("the predecessor does not name its replacement")
	}
	head, _, err := heads.Head(ctx, "/spec")
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if head.Current != second.Key {
		t.Error("the head did not move")
	}
	if head.Generation != 2 {
		t.Errorf("the head generation is %d after one replacement", head.Generation)
	}
	if head.Fence != 1 {
		t.Errorf("the fence is %d after a head move over an activated head", head.Fence)
	}
}

func TestAStaleCandidateLosesAndTheHeadIsUntouched(t *testing.T) {
	heads, plans := headStore(t)
	ctx := context.Background()
	winner := candidate(2, planner.PlanPending)
	install(t, heads, plans, winner)

	stale := candidate(1, planner.PlanPending)
	if err := plans.Upsert(ctx, stale); err != nil {
		t.Fatalf("write candidate: %v", err)
	}
	verdict, err := heads.Replace(ctx, stale)
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if verdict.Won {
		t.Fatal("a stale candidate took the head")
	}
	if verdict.Landing != planner.PlanHistorical {
		t.Errorf("the loser's landing is %q, want %q", verdict.Landing, planner.PlanHistorical)
	}
	// A rejected CAS must change NOTHING. One that bumped the generation
	// anyway would have mutated what it refused to replace.
	head, _, err := heads.Head(ctx, "/spec")
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if head.Current != winner.Key || head.Generation != 1 {
		t.Errorf("a rejected CAS moved the head to %s at generation %d",
			head.Current[:12], head.Generation)
	}
}

func TestAHeadWhosePlanIsNotActiveRefusesReplacement(t *testing.T) {
	// Replacing a head whose plan is itself mid-replacement would interleave
	// two supersessions over one predecessor.
	heads, plans := headStore(t)
	ctx := context.Background()
	first := candidate(1, planner.PlanPending)
	if err := plans.Upsert(ctx, first); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	if _, err := heads.Replace(ctx, first); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	// Left PENDING, never activated.

	second := candidate(2, planner.PlanPending)
	if err := plans.Upsert(ctx, second); err != nil {
		t.Fatalf("write candidate: %v", err)
	}
	verdict, err := heads.Replace(ctx, second)
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if verdict.Won {
		t.Error("a candidate replaced a head whose own plan had not activated")
	}
	// Still eligible — it is strictly newer — so it parks rather than dying.
	if verdict.Landing != planner.PlanAwaitingOperator {
		t.Errorf("the loser's landing is %q, want %q", verdict.Landing, planner.PlanAwaitingOperator)
	}
}

func TestActivationLowersTheFenceOnce(t *testing.T) {
	heads, plans := headStore(t)
	ctx := context.Background()
	first := candidate(1, planner.PlanPending)
	install(t, heads, plans, first)

	for range 2 {
		if err := heads.Activate(ctx, first.Key); err != nil {
			t.Fatalf("activate: %v", err)
		}
	}
	head, _, err := heads.Head(ctx, "/spec")
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	// Never below zero: a second activation is a replay, and a negative
	// fence would take a later supersession's raise back to zero and admit
	// pops against an unactivated head.
	if head.Fence != 0 {
		t.Errorf("the fence is %d after two activations", head.Fence)
	}
}

func TestAHeadNamingAPlanThatDoesNotExistIsRefused(t *testing.T) {
	// The head row and the plan rows can only disagree if something wrote one
	// without the other. Proceeding would compare the candidate's generation
	// against a zero and let anything win.
	heads, plans := headStore(t)
	ctx := context.Background()
	first := candidate(1, planner.PlanPending)
	install(t, heads, plans, first)
	if _, err := heads.DB.ExecContext(ctx, `DELETE FROM decomposition_plan`); err != nil {
		t.Fatalf("delete plans: %v", err)
	}

	if _, err := heads.Replace(ctx, candidate(2, planner.PlanPending)); err == nil {
		t.Fatal("a head naming no plan replaced without complaint")
	}
}

func TestAnUnreachableStoreFailsEveryHeadOperation(t *testing.T) {
	heads, _ := headStore(t)
	if err := heads.DB.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	ctx := context.Background()
	if _, _, err := heads.Head(ctx, "/spec"); err == nil {
		t.Error("reading a head from a closed database succeeded")
	}
	if _, err := heads.Replace(ctx, candidate(1, planner.PlanPending)); err == nil {
		t.Error("replacing a head in a closed database succeeded")
	}
	if err := heads.Activate(ctx, "key"); err == nil {
		t.Error("activating in a closed database succeeded")
	}
}

func TestAPlanThatAlreadyOwnsTheHeadWinsAgain(t *testing.T) {
	// A resumed plan re-enters the CAS: a crash can land between the head
	// move and activation, so resumption must be able to finish either.
	// Comparing it against ITSELF makes it an equal-generation loser — buried
	// historical, with the head left pointing at a terminal plan.
	heads, plans := headStore(t)
	ctx := context.Background()
	plan := candidate(1, planner.PlanPending)
	install(t, heads, plans, plan)

	verdict, err := heads.Replace(ctx, plan)
	if err != nil {
		t.Fatalf("re-entering the CAS: %v", err)
	}
	if !verdict.Won {
		t.Fatalf("a plan that already owns the head lost to itself, landing %q", verdict.Landing)
	}
	// And the head must be UNMOVED: an idempotent win that bumped the
	// generation would raise the fence again with nothing to activate it.
	head, _, err := heads.Head(ctx, "/spec")
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if head.Generation != 1 || head.Fence != 1 {
		t.Errorf("re-entering the CAS moved the head to generation %d fence %d",
			head.Generation, head.Fence)
	}
}

func TestASupersededPlanCannotWriteItselfBackToActive(t *testing.T) {
	// An active-but-incomplete head may be replaced while its own
	// decomposition is still running its phases. That decomposition then
	// reaches its next durable write — and unguarded, it wrote itself active
	// and completed, erasing the supersession. Its activation then silently
	// missed, because it was no longer current, leaking a fence increment
	// nothing would ever lower.
	heads, plans := headStore(t)
	ctx := context.Background()
	first := candidate(1, planner.PlanPending)
	install(t, heads, plans, first)
	if err := heads.Activate(ctx, first.Key); err != nil {
		t.Fatalf("activate: %v", err)
	}

	second := candidate(2, planner.PlanPending)
	if err := plans.Upsert(ctx, second); err != nil {
		t.Fatalf("write candidate: %v", err)
	}
	if verdict, err := heads.Replace(ctx, second); err != nil || !verdict.Won {
		t.Fatalf("replace: %v won=%v", err, verdict.Won)
	}

	// The superseded decomposition carries on and tries to stamp itself.
	stamping := first
	stamping.State, stamping.Completed = planner.PlanActive, true
	err := plans.Upsert(ctx, stamping)
	if err == nil {
		t.Fatal("a superseded plan wrote itself back to active")
	}
	var terminal *planner.TerminalPlanError
	if !errors.As(err, &terminal) {
		t.Errorf("the refusal is %v, want a TerminalPlanError the caller can act on", err)
	}

	got, _, err := plans.ByKey(ctx, first.Key)
	if err != nil {
		t.Fatalf("read plan: %v", err)
	}
	if got.State != planner.PlanSuperseded {
		t.Errorf("the predecessor is %q after the refused write", got.State)
	}
	if got.Completed {
		t.Error("the refused write still landed the completed stamp")
	}
}

func TestAHistoricalPlanIsNeverRevived(t *testing.T) {
	heads, plans := headStore(t)
	ctx := context.Background()
	install(t, heads, plans, candidate(2, planner.PlanPending))

	stale := candidate(1, planner.PlanPending)
	if err := plans.Upsert(ctx, stale); err != nil {
		t.Fatalf("write candidate: %v", err)
	}
	if _, err := heads.Replace(ctx, stale); err != nil {
		t.Fatalf("replace: %v", err)
	}

	revived := stale
	revived.State = planner.PlanActive
	if err := plans.Upsert(ctx, revived); err == nil {
		t.Error("a terminal historical plan was revived")
	}
}
