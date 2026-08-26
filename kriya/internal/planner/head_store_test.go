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

// whole stamps a plan active and completed, as decomposition's final barrier
// does. Activation requires it, so a test that activates must reach it.
func whole(t *testing.T, plans planner.SQLPlans, p planner.Plan) {
	t.Helper()
	p.State, p.Completed = planner.PlanActive, true
	if err := plans.Upsert(context.Background(), p); err != nil {
		t.Fatalf("stamp plan whole: %v", err)
	}
}

func TestActivationLowersTheFenceOnce(t *testing.T) {
	heads, plans := headStore(t)
	ctx := context.Background()
	first := candidate(1, planner.PlanPending)
	install(t, heads, plans, first)
	whole(t, plans, first)

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

func TestAMidPhasePlanCannotLowerTheFence(t *testing.T) {
	// Every retry path reaches activation. One that replayed it for a parked,
	// superseded or still-decomposing plan would admit pops against a head
	// that is not ready — which is the whole thing the fence prevents.
	heads, plans := headStore(t)
	ctx := context.Background()
	first := candidate(1, planner.PlanPending)
	install(t, heads, plans, first)

	// ACTIVE but not yet stamped whole: decomposition is still running.
	first.State = planner.PlanActive
	if err := plans.Upsert(ctx, first); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	if err := heads.Activate(ctx, first.Key); err != nil {
		t.Fatalf("activate: %v", err)
	}
	head, _, err := heads.Head(ctx, "/spec")
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if head.Fence != 1 {
		t.Errorf("a plan still mid-decomposition lowered the fence to %d", head.Fence)
	}
}

func TestReplacingAnIncompleteHeadDoesNotStackTheFence(t *testing.T) {
	// An active-but-incomplete head still holds its own outstanding fence
	// contribution. A plain increment made the fence 2, and the predecessor
	// could never lower it again because Activate matches only the CURRENT
	// head — so the successor's activation left it at 1 and pops were refused
	// forever.
	heads, plans := headStore(t)
	ctx := context.Background()
	first := candidate(1, planner.PlanPending)
	install(t, heads, plans, first)
	// Active, mid-phase, never activated: its fence contribution stands.
	first.State = planner.PlanActive
	if err := plans.Upsert(ctx, first); err != nil {
		t.Fatalf("write plan: %v", err)
	}

	second := candidate(2, planner.PlanPending)
	if err := plans.Upsert(ctx, second); err != nil {
		t.Fatalf("write candidate: %v", err)
	}
	if verdict, err := heads.Replace(ctx, second); err != nil || !verdict.Won {
		t.Fatalf("replace: %v won=%v", err, verdict.Won)
	}
	head, _, err := heads.Head(ctx, "/spec")
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if head.Fence != 1 {
		t.Fatalf("the fence is %d after replacing an unactivated head", head.Fence)
	}

	// The successor finishes and activates: the fence must reach zero.
	whole(t, plans, second)
	if err := heads.Activate(ctx, second.Key); err != nil {
		t.Fatalf("activate: %v", err)
	}
	head, _, err = heads.Head(ctx, "/spec")
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if head.Fence != 0 {
		t.Errorf("the fence is %d after the successor activated: pops are refused forever", head.Fence)
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

// advanceCounting wraps a real AdvanceStore and counts fresh advances.
type advanceCounting struct {
	inner  planner.AdvanceStore
	causes []string
}

func (a *advanceCounting) Insert(
	ctx context.Context, adv planner.CompletionAdvance,
) (bool, error) {
	return a.inner.Insert(ctx, adv)
}

func (a *advanceCounting) InsertWith(
	ctx context.Context, adv planner.CompletionAdvance, mutate func(planner.Tx) error,
) (bool, error) {
	fresh, err := a.inner.InsertWith(ctx, adv, mutate)
	if fresh && err == nil {
		a.causes = append(a.causes, adv.Cause)
	}
	return fresh, err
}

func (a *advanceCounting) Epoch(ctx context.Context, key string) (int, error) {
	return a.inner.Epoch(ctx, key)
}

func (a *advanceCounting) Stamp(ctx context.Context, key string, epoch int) (bool, error) {
	return a.inner.Stamp(ctx, key, epoch)
}

// epochHeads builds a head store bound to a real advance store.
func epochHeads(t *testing.T) (planner.SQLHeads, planner.SQLPlans, *advanceCounting) {
	t.Helper()
	db := sqlDB(t, planner.PlanMigration, planner.PlanKeyMigration,
		planner.HeadMigration, planner.EpochMigration)
	advances := &advanceCounting{inner: planner.SQLAdvances{DB: db}}
	return planner.SQLHeads{DB: db, Advances: advances}, planner.SQLPlans{DB: db}, advances
}

func TestASupersessionAdvancesTheCompletionEpoch(t *testing.T) {
	// Without it the OLD plan's completion claim stays current across the
	// replacement — free to stamp the target done, or to suppress a fresh
	// review, over work the new head has not built.
	heads, plans, advances := epochHeads(t)
	ctx := context.Background()
	first := candidate(1, planner.PlanPending)
	install(t, heads, plans, first)
	if err := heads.Activate(ctx, first.Key); err != nil {
		t.Fatalf("activate: %v", err)
	}
	epochBefore, err := (planner.Epochs{Store: advances}).Current(ctx, "/spec")
	if err != nil {
		t.Fatalf("epoch: %v", err)
	}

	second := candidate(2, planner.PlanPending)
	if err := plans.Upsert(ctx, second); err != nil {
		t.Fatalf("write candidate: %v", err)
	}
	if verdict, err := heads.Replace(ctx, second); err != nil || !verdict.Won {
		t.Fatalf("replace: %v won=%v", err, verdict.Won)
	}

	epochAfter, err := (planner.Epochs{Store: advances}).Current(ctx, "/spec")
	if err != nil {
		t.Fatalf("epoch: %v", err)
	}
	if epochAfter <= epochBefore {
		t.Errorf("the epoch is %d after a supersession, was %d", epochAfter, epochBefore)
	}
	if len(advances.causes) == 0 || advances.causes[len(advances.causes)-1] != planner.CauseSupersession {
		t.Errorf("the advance causes are %v, want one for a supersession", advances.causes)
	}
}

func TestALostCASAdvancesNothing(t *testing.T) {
	// A candidate that moved no head must move no epoch: advancing would
	// kill the winning head's completion claim on behalf of a plan that
	// never became the head.
	heads, plans, advances := epochHeads(t)
	ctx := context.Background()
	install(t, heads, plans, candidate(2, planner.PlanPending))
	before, err := (planner.Epochs{Store: advances}).Current(ctx, "/spec")
	if err != nil {
		t.Fatalf("epoch: %v", err)
	}

	stale := candidate(1, planner.PlanPending)
	if err := plans.Upsert(ctx, stale); err != nil {
		t.Fatalf("write candidate: %v", err)
	}
	if verdict, err := heads.Replace(ctx, stale); err != nil || verdict.Won {
		t.Fatalf("the stale candidate won: %v won=%v", err, verdict.Won)
	}
	after, err := (planner.Epochs{Store: advances}).Current(ctx, "/spec")
	if err != nil {
		t.Fatalf("epoch: %v", err)
	}
	if after != before {
		t.Errorf("a losing candidate moved the epoch from %d to %d", before, after)
	}
	// And the loser still landed, in its own transaction.
	got, _, err := plans.ByKey(ctx, stale.Key)
	if err != nil {
		t.Fatalf("read loser: %v", err)
	}
	if got.State != planner.PlanHistorical {
		t.Errorf("the loser is %q", got.State)
	}
}

func TestAResumedPlanReenteringTheCASAdvancesNothing(t *testing.T) {
	// A resumed plan re-enters the CAS. Advancing there would clear a
	// completion claim for a head that did not move.
	heads, plans, advances := epochHeads(t)
	ctx := context.Background()
	plan := candidate(1, planner.PlanPending)
	install(t, heads, plans, plan)
	before, err := (planner.Epochs{Store: advances}).Current(ctx, "/spec")
	if err != nil {
		t.Fatalf("epoch: %v", err)
	}

	if verdict, err := heads.Replace(ctx, plan); err != nil || !verdict.Won {
		t.Fatalf("re-entering the CAS: %v won=%v", err, verdict.Won)
	}
	after, err := (planner.Epochs{Store: advances}).Current(ctx, "/spec")
	if err != nil {
		t.Fatalf("epoch: %v", err)
	}
	if after != before {
		t.Errorf("re-entering the CAS moved the epoch from %d to %d", before, after)
	}
}

func TestALandingIsRevalidatedAgainstTheCurrentHead(t *testing.T) {
	// The deciding transaction rolls back before the landing is written, so
	// the head can move in between. Landing the stale verdict would park a
	// candidate awaiting-operator that the head has since outrun — an
	// operator offered a retry the CAS can only ever reject again.
	heads, plans := headStore(t)
	ctx := context.Background()
	install(t, heads, plans, candidate(5, planner.PlanPending))
	whole(t, plans, candidate(5, planner.PlanPending))

	// A candidate that is still eligible against generation 5 — it would park.
	loser := candidate(7, planner.PlanPending)
	if err := plans.Upsert(ctx, loser); err != nil {
		t.Fatalf("write candidate: %v", err)
	}
	// But the head plan is mid-replacement, so the CAS refuses it.
	if _, err := heads.DB.ExecContext(ctx,
		`UPDATE decomposition_plan SET state = ? WHERE decomposition_key = ?`,
		planner.PlanPending, candidate(5, planner.PlanPending).Key); err != nil {
		t.Fatalf("unsettle the head plan: %v", err)
	}
	verdict, err := heads.Replace(ctx, loser)
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if verdict.Won {
		t.Fatal("a candidate replaced a head whose plan had not activated")
	}
	// Still eligible against generation 5, so it parks for the operator.
	if verdict.Landing != planner.PlanAwaitingOperator {
		t.Errorf("the landing is %q, want %q", verdict.Landing, planner.PlanAwaitingOperator)
	}
}

func TestACandidateThatBecameTheHeadIsNotLandedAsALoser(t *testing.T) {
	// The worst version of the same window: the candidate loses, and before
	// the landing is written it BECOMES the head. Landing the stale verdict
	// would bury the current head as a loser.
	heads, plans := headStore(t)
	ctx := context.Background()
	install(t, heads, plans, candidate(2, planner.PlanPending))
	whole(t, plans, candidate(2, planner.PlanPending))

	winner := candidate(3, planner.PlanPending)
	if err := plans.Upsert(ctx, winner); err != nil {
		t.Fatalf("write candidate: %v", err)
	}
	// It already holds the head when the stale landing arrives.
	if verdict, err := heads.Replace(ctx, winner); err != nil || !verdict.Won {
		t.Fatalf("replace: %v won=%v", err, verdict.Won)
	}

	// Replaying the CAS is the same shape as a stale landing arriving late:
	// the candidate is now the head, and must be reported as the winner.
	verdict, err := heads.Replace(ctx, winner)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !verdict.Won {
		t.Errorf("the current head was reported as a loser, landing %q", verdict.Landing)
	}
	got, _, err := plans.ByKey(ctx, winner.Key)
	if err != nil {
		t.Fatalf("read plan: %v", err)
	}
	if got.State == planner.PlanHistorical || got.State == planner.PlanAwaitingOperator {
		t.Errorf("the current head plan was landed as %q", got.State)
	}
}
