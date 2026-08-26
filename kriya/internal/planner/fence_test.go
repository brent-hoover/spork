package planner_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"kriya/internal/planner"
)

func fenceStore(t *testing.T) planner.SQLFence {
	t.Helper()
	return planner.SQLFence{DB: sqlDB(t, planner.FenceMigration)}
}

// bootstrap moves the singleton the migration already inserted.
//
// An INSERT here would collide: the migration seeds the row, precisely so a
// fresh database's first admission matches something rather than reading as a
// lost race forever.
func bootstrap(t *testing.T, f planner.SQLFence, unactivated, version int) {
	t.Helper()
	if _, err := f.DB.ExecContext(context.Background(),
		`UPDATE pop_fence SET unactivated_heads = ?, version = ? WHERE singleton_key = ?`,
		unactivated, version, "pop-fence"); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
}

func TestAFreshDatabaseAdmitsImmediately(t *testing.T) {
	// Bootstrap state: no plan exists, so nothing is unactivated and
	// admissions flow. The migration INSERTS the singleton for exactly this
	// reason — created empty, Read answers zero correctly but every admission
	// UPDATE matches no row, which the guard can only read as a lost race. The
	// loop then retries, idles, and never pops again until some unrelated head
	// transition happens to create the row.
	f := fenceStore(t)
	ctx := context.Background()
	got, err := f.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.UnactivatedHeads != 0 {
		t.Errorf("a fresh fence reads %d unactivated heads", got.UnactivatedHeads)
	}
	if err := f.Admit(ctx, got.Version); err != nil {
		t.Fatalf("the first admission on a fresh database was refused: %v", err)
	}
}

func TestAdmissionIsAWriteNotAnAssertion(t *testing.T) {
	// A read-only assertion could commit beside a concurrent replacement:
	// both observe a clear fence, both proceed. The conditional WRITE makes
	// them write-write conflicts on one row, of which exactly one commits.
	f := fenceStore(t)
	bootstrap(t, f, 0, 7)
	ctx := context.Background()

	if err := f.Admit(ctx, 7); err != nil {
		t.Fatalf("admission at the current version: %v", err)
	}
	got, err := f.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Version != 8 {
		t.Errorf("the version is %d after admission; admission did not write", got.Version)
	}
}

func TestASecondAdmissionAtTheSameVersionLoses(t *testing.T) {
	// Two agents that read the same version race. Exactly one wins, and the
	// loser is told its read is stale rather than that the fence is closed.
	f := fenceStore(t)
	bootstrap(t, f, 0, 3)
	ctx := context.Background()

	if err := f.Admit(ctx, 3); err != nil {
		t.Fatalf("the first admission: %v", err)
	}
	err := f.Admit(ctx, 3)
	if err == nil {
		t.Fatal("two admissions committed at one version")
	}
	var fenced *planner.ErrFenced
	if !errors.As(err, &fenced) {
		t.Fatalf("the loser got %v, not a fence refusal", err)
	}
	if !fenced.StaleVersion {
		t.Error("the loser was told the fence is closed, not that its read is stale")
	}
}

func TestANonzeroCounterRefusesAdmissionAtAnyVersion(t *testing.T) {
	f := fenceStore(t)
	bootstrap(t, f, 2, 5)
	ctx := context.Background()

	err := f.Admit(ctx, 5)
	if err == nil {
		t.Fatal("admission was accepted with unactivated heads")
	}
	var fenced *planner.ErrFenced
	if !errors.As(err, &fenced) {
		t.Fatalf("got %v, not a fence refusal", err)
	}
	if fenced.StaleVersion {
		t.Error("a closed fence was reported as a stale read")
	}
	if fenced.UnactivatedHeads != 2 {
		t.Errorf("the refusal names %d unactivated heads", fenced.UnactivatedHeads)
	}
	// And it wrote NOTHING: a refused admission must not move the version,
	// or every refusal would invalidate every other agent's read.
	got, err := f.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Version != 5 {
		t.Errorf("a refused admission moved the version to %d", got.Version)
	}
}

func TestTheCounterIsGlobalAcrossTargets(t *testing.T) {
	// One target mid-supersession refuses admission everywhere. A pop
	// admitted against any unactivated head is work started before its
	// predecessor retired.
	db := sqlDB(t, planner.PlanMigration, planner.PlanKeyMigration,
		planner.PlanPredecessorMigration, planner.HeadMigration, planner.FenceMigration)
	heads, plans, f := planner.SQLHeads{DB: db}, planner.SQLPlans{DB: db}, planner.SQLFence{DB: db}
	ctx := context.Background()

	for _, targetKey := range []string{"/a", "/b"} {
		p := planner.Plan{
			Key: "plan" + targetKey, TargetKey: targetKey, SpecHash: "h",
			Generation: 1, State: planner.PlanPending,
		}
		if err := plans.Upsert(ctx, p); err != nil {
			t.Fatalf("seed %s: %v", targetKey, err)
		}
		if verdict, err := heads.Replace(ctx, p); err != nil || !verdict.Won {
			t.Fatalf("install %s: %v won=%v", targetKey, err, verdict.Won)
		}
	}

	got, err := f.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.UnactivatedHeads != 2 {
		t.Fatalf("two unactivated heads counted as %d", got.UnactivatedHeads)
	}
	// Activating one is not enough: the other still holds admission closed.
	whole(t, plans, planner.Plan{Key: "plan/a", TargetKey: "/a", SpecHash: "h", Generation: 1})
	if err := heads.Activate(ctx, "plan/a"); err != nil {
		t.Fatalf("activate: %v", err)
	}
	got, err = f.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.UnactivatedHeads != 1 {
		t.Errorf("the counter is %d after one of two activations", got.UnactivatedHeads)
	}
	if err := f.Admit(ctx, got.Version); err == nil {
		t.Error("admission was accepted while another target's head was unactivated")
	}
}

func TestAReplayedActivationDoesNotTakeTheCounterNegative(t *testing.T) {
	// A negative counter would be brought back to zero by a LATER
	// replacement's raise, admitting pops against a head that never
	// activated.
	db := sqlDB(t, planner.PlanMigration, planner.PlanKeyMigration,
		planner.PlanPredecessorMigration, planner.HeadMigration, planner.FenceMigration)
	heads, plans, f := planner.SQLHeads{DB: db}, planner.SQLPlans{DB: db}, planner.SQLFence{DB: db}
	ctx := context.Background()
	p := planner.Plan{
		Key: "plan-1", TargetKey: "/spec", SpecHash: "h", Generation: 1,
		State: planner.PlanPending,
	}
	if err := plans.Upsert(ctx, p); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if verdict, err := heads.Replace(ctx, p); err != nil || !verdict.Won {
		t.Fatalf("install: %v won=%v", err, verdict.Won)
	}
	whole(t, plans, p)

	for range 3 {
		if err := heads.Activate(ctx, p.Key); err != nil {
			t.Fatalf("activate: %v", err)
		}
	}
	got, err := f.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.UnactivatedHeads != 0 {
		t.Errorf("three activations left the counter at %d", got.UnactivatedHeads)
	}
}

func TestAnUnreachableFenceRefusesRatherThanAdmits(t *testing.T) {
	f := fenceStore(t)
	if err := f.DB.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	ctx := context.Background()
	if _, err := f.Read(ctx); err == nil {
		t.Error("reading a closed database succeeded")
	}
	if err := f.Admit(ctx, 0); err == nil {
		t.Error("admission against a closed database was accepted")
	}
}

func TestReplacingAnUnactivatedHeadDoesNotDoubleCount(t *testing.T) {
	// The GLOBAL counter, not the per-target marker. An active-but-incomplete
	// head still holds its contribution, so a second raise would leave the
	// counter at 1 after the successor activated — admissions refused
	// everywhere, forever, for a build that finished.
	db := sqlDB(t, planner.PlanMigration, planner.PlanKeyMigration,
		planner.PlanPredecessorMigration, planner.HeadMigration, planner.FenceMigration)
	heads, plans, f := planner.SQLHeads{DB: db}, planner.SQLPlans{DB: db}, planner.SQLFence{DB: db}
	ctx := context.Background()

	first := planner.Plan{
		Key: "plan-1", TargetKey: "/spec", SpecHash: "h", Generation: 1,
		State: planner.PlanPending,
	}
	if err := plans.Upsert(ctx, first); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if verdict, err := heads.Replace(ctx, first); err != nil || !verdict.Won {
		t.Fatalf("install: %v won=%v", err, verdict.Won)
	}
	// ACTIVE but never activated: its contribution stands.
	first.State = planner.PlanActive
	if err := plans.Upsert(ctx, first); err != nil {
		t.Fatalf("write plan: %v", err)
	}

	second := planner.Plan{
		Key: "plan-2", TargetKey: "/spec", SpecHash: "h", Generation: 2,
		State: planner.PlanPending,
	}
	if err := plans.Upsert(ctx, second); err != nil {
		t.Fatalf("seed candidate: %v", err)
	}
	if verdict, err := heads.Replace(ctx, second); err != nil || !verdict.Won {
		t.Fatalf("replace: %v won=%v", err, verdict.Won)
	}
	got, err := f.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.UnactivatedHeads != 1 {
		t.Fatalf("one unactivated head counted as %d", got.UnactivatedHeads)
	}

	// The successor finishes: admissions must flow again.
	whole(t, plans, second)
	if err := heads.Activate(ctx, second.Key); err != nil {
		t.Fatalf("activate: %v", err)
	}
	got, err = f.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.UnactivatedHeads != 0 {
		t.Errorf("the counter is %d after the successor activated", got.UnactivatedHeads)
	}
	if err := f.Admit(ctx, got.Version); err != nil {
		t.Errorf("admission is still refused after activation: %v", err)
	}
}

func TestAReplayedActivationLowersTheGlobalCounterOnce(t *testing.T) {
	// The per-target marker gates the global one: only an activation that
	// actually moved plan_head.fence may lower the counter. Without that
	// gate, every replayed activation would drive it negative — and a later
	// replacement's raise would bring it back to zero, admitting pops against
	// a head that never activated.
	db := sqlDB(t, planner.PlanMigration, planner.PlanKeyMigration,
		planner.PlanPredecessorMigration, planner.HeadMigration, planner.FenceMigration)
	heads, plans, f := planner.SQLHeads{DB: db}, planner.SQLPlans{DB: db}, planner.SQLFence{DB: db}
	ctx := context.Background()

	// TWO targets, so the counter starts at 2 and a stray decrement is
	// visible as 0 rather than being clamped at zero by the floor.
	for _, targetKey := range []string{"/a", "/b"} {
		p := planner.Plan{
			Key: "plan" + targetKey, TargetKey: targetKey, SpecHash: "h",
			Generation: 1, State: planner.PlanPending,
		}
		if err := plans.Upsert(ctx, p); err != nil {
			t.Fatalf("seed %s: %v", targetKey, err)
		}
		if verdict, err := heads.Replace(ctx, p); err != nil || !verdict.Won {
			t.Fatalf("install %s: %v won=%v", targetKey, err, verdict.Won)
		}
	}
	whole(t, plans, planner.Plan{Key: "plan/a", TargetKey: "/a", SpecHash: "h", Generation: 1})

	for range 3 {
		if err := heads.Activate(ctx, "plan/a"); err != nil {
			t.Fatalf("activate: %v", err)
		}
	}
	got, err := f.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.UnactivatedHeads != 1 {
		t.Errorf("three activations of one head left the counter at %d, want 1 for the other target",
			got.UnactivatedHeads)
	}
}

func TestAMarkerNeverSurvivesItsOwnActivation(t *testing.T) {
	// The local marker gates the global raise, so a marker that survives its
	// own activation makes the NEXT replacement skip the raise — and pops are
	// admitted during that supersession.
	//
	// It takes an UNACTIVATED head in the chain to expose: arithmetic on the
	// marker then reaches two, one activation leaves it at one, and the third
	// replacement sees a contribution that is not really there. A chain where
	// every head activates behaves identically either way, which is why the
	// straightforward version of this test proved nothing.
	db := sqlDB(t, planner.PlanMigration, planner.PlanKeyMigration,
		planner.PlanPredecessorMigration, planner.HeadMigration, planner.FenceMigration)
	heads, plans, f := planner.SQLHeads{DB: db}, planner.SQLPlans{DB: db}, planner.SQLFence{DB: db}
	ctx := context.Background()

	install := func(generation int) planner.Plan {
		p := planner.Plan{
			Key: fmt.Sprintf("plan-%d", generation), TargetKey: "/spec", SpecHash: "h",
			Generation: generation, State: planner.PlanPending,
		}
		if err := plans.Upsert(ctx, p); err != nil {
			t.Fatalf("seed %d: %v", generation, err)
		}
		if verdict, err := heads.Replace(ctx, p); err != nil || !verdict.Won {
			t.Fatalf("replace %d: %v won=%v", generation, err, verdict.Won)
		}
		return p
	}

	// Generation 1 takes the head and is left ACTIVE but never activated.
	first := install(1)
	first.State = planner.PlanActive
	if err := plans.Upsert(ctx, first); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	// Generation 2 replaces it and completes properly.
	second := install(2)
	whole(t, plans, second)
	if err := heads.Activate(ctx, second.Key); err != nil {
		t.Fatalf("activate: %v", err)
	}
	head, _, err := heads.Head(ctx, "/spec")
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if head.Fence != 0 {
		t.Fatalf("the activated head's marker is still %d", head.Fence)
	}

	// Generation 3 must raise the global counter. With a marker left behind
	// it would skip the raise and admit pops mid-supersession.
	install(3)
	got, err := f.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.UnactivatedHeads != 1 {
		t.Fatalf("the third replacement left the counter at %d, want 1", got.UnactivatedHeads)
	}
	if err := f.Admit(ctx, got.Version); err == nil {
		t.Error("a pop was admitted while the third head was unactivated")
	}
}

func TestAnActivatedHeadLeavesNoMarkerBehind(t *testing.T) {
	// The local marker gates the global raise, so a marker that survives its
	// own activation makes the NEXT replacement skip the raise — and pops are
	// admitted during that supersession. Arithmetic on the marker is what
	// allowed it: a value of two survived one activation as one.
	db := sqlDB(t, planner.PlanMigration, planner.PlanKeyMigration,
		planner.PlanPredecessorMigration, planner.HeadMigration, planner.FenceMigration)
	heads, plans, f := planner.SQLHeads{DB: db}, planner.SQLPlans{DB: db}, planner.SQLFence{DB: db}
	ctx := context.Background()

	// Three generations, each activated in turn. Every replacement must raise
	// the global counter and every activation must clear it.
	for generation := 1; generation <= 3; generation++ {
		p := planner.Plan{
			Key: fmt.Sprintf("plan-%d", generation), TargetKey: "/spec", SpecHash: "h",
			Generation: generation, State: planner.PlanPending,
		}
		if err := plans.Upsert(ctx, p); err != nil {
			t.Fatalf("seed %d: %v", generation, err)
		}
		if verdict, err := heads.Replace(ctx, p); err != nil || !verdict.Won {
			t.Fatalf("replace %d: %v won=%v", generation, err, verdict.Won)
		}
		got, err := f.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if got.UnactivatedHeads != 1 {
			t.Fatalf("generation %d raised the counter to %d, want 1 — "+
				"a marker left behind by the previous activation skipped the raise",
				generation, got.UnactivatedHeads)
		}
		if err := f.Admit(ctx, got.Version); err == nil {
			t.Fatalf("generation %d admitted a pop mid-supersession", generation)
		}

		whole(t, plans, p)
		if err := heads.Activate(ctx, p.Key); err != nil {
			t.Fatalf("activate %d: %v", generation, err)
		}
		head, _, err := heads.Head(ctx, "/spec")
		if err != nil {
			t.Fatalf("head: %v", err)
		}
		if head.Fence != 0 {
			t.Fatalf("generation %d activated with its marker still at %d",
				generation, head.Fence)
		}
	}
}
