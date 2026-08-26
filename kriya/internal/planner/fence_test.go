package planner_test

import (
	"context"
	"errors"
	"testing"

	"kriya/internal/planner"
)

func fenceStore(t *testing.T) planner.SQLFence {
	t.Helper()
	return planner.SQLFence{DB: sqlDB(t, planner.FenceMigration)}
}

// bootstrap writes the singleton so a test can move it.
func bootstrap(t *testing.T, f planner.SQLFence, unactivated, version int) {
	t.Helper()
	if _, err := f.DB.ExecContext(context.Background(),
		`INSERT INTO pop_fence (singleton_key, unactivated_heads, version) VALUES (?, ?, ?)`,
		"pop-fence", unactivated, version); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
}

func TestAnUnwrittenFenceReadsAsClear(t *testing.T) {
	// Bootstrap state: no plan exists, so nothing is unactivated and
	// admissions flow immediately. Refusing here would deadlock the very
	// first build of a target.
	got, err := fenceStore(t).Read(context.Background())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.UnactivatedHeads != 0 || got.Version != 0 {
		t.Errorf("an unwritten fence reads as %+v", got)
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
