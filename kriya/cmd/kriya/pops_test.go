package main

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"kriya/internal/orchestrator"
	"kriya/internal/planner"
	"kriya/internal/recovery"
)

// planFence is the planner-owned admission guard for a database.
func planFence(db *sql.DB) planner.SQLFence { return planner.SQLFence{DB: db} }

// reserved writes a claimed-but-unbound run, as a crash between the claim and
// the binding leaves behind.
func reserved(t *testing.T, id, popKey string) *orchestrator.SQLStore {
	t.Helper()
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := orchestrator.SQLStore{DB: db}
	if err := store.Reserve(t.Context(), orchestrator.BuildRun{
		ID: id, Plan: "/spec", PopKey: popKey,
	}, planFence(db), 0); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	return &store
}

func TestUnboundPopsAreFoundForReconciliation(t *testing.T) {
	// A queued run with a pop key and no issue is a claim whose outcome is
	// unknown. Retirement reads it as pending work and would defer the ticket
	// a live build is about to pick up, so reconciliation must find it.
	store := reserved(t, "run-1", "pop-key-1")
	unbound, err := store.Unbound(t.Context())
	if err != nil {
		t.Fatalf("unbound: %v", err)
	}
	if len(unbound) != 1 {
		t.Fatalf("found %d unbound pops, want 1", len(unbound))
	}
	if unbound[0].PopKey != "pop-key-1" {
		t.Errorf("the unbound pop carries key %q", unbound[0].PopKey)
	}
}

func TestABoundRunIsNotUnbound(t *testing.T) {
	// The other half: reconciliation must not walk runs that already own
	// their claim, or it would replay a pop for work already in progress.
	store := reserved(t, "run-1", "pop-key-1")
	if err := store.Bind(t.Context(), "run-1", "issue-1", "T-1"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	unbound, err := store.Unbound(t.Context())
	if err != nil {
		t.Fatalf("unbound: %v", err)
	}
	if len(unbound) != 0 {
		t.Errorf("a bound run was offered for reconciliation: %+v", unbound)
	}
}

func TestReconciliationIsWiredIntoRecovery(t *testing.T) {
	// The step exists and runs in the TARGETS stage, before anything
	// classifies ownership. Wired later, retirement would read an unbound
	// claim as pending work.
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if reconcilePops(db, "actor-1") == nil {
		t.Fatal("no pop reconciliation step exists")
	}
	// With nothing unbound it must be a no-op rather than an error: the
	// ordinary case is a clean database.
	if err := reconcilePops(db, "actor-1")(t.Context()); err != nil {
		t.Errorf("reconciling a clean database failed: %v", err)
	}
}

func TestTheRecoveryOrderPutsPopsBeforeClassification(t *testing.T) {
	// Pop reconciliation must run before anything classifies ownership.
	// Wired later, retirement reads an unbound claim as pending work and
	// defers the ticket a live build is about to pick up.
	//
	// The order is observed by making the pops step FAIL: the stage returns
	// there, so anything that would have run after it demonstrably did not.
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var order []string
	steps := recoverySteps(intakerForTest(db), workspaceManagerForTest(t), bridgeForTest(t),
		loopForTest(db), submitterForTest(db), queueForTest(t, db), completerForTest(t, db),
		"actor-1", planner.Claimer{}, noTargets,
		func(context.Context) error {
			order = append(order, "work")
			return nil
		},
		func(context.Context) error {
			order = append(order, "pops")
			return errors.New("stop here")
		},
		"/target", orchestrator.Researcher{}, noFindingProject)

	for _, step := range steps {
		if step.Stage != recovery.StageTargets {
			continue
		}
		if err := step.Run(t.Context()); err == nil {
			t.Fatal("the stage ran past the failing pops step")
		}
	}
	if strings.Join(order, ",") != "pops" {
		t.Errorf("the targets stage ran %v; pops must come first", order)
	}
}

// replayPops answers a replayed key with what the original claim took.
type replayPops struct {
	byKey map[string]string
	err   error
	keys  []string
}

func (p *replayPops) Pop(_ context.Context, key string) (string, string, error) {
	p.keys = append(p.keys, key)
	if p.err != nil {
		return "", "", p.err
	}
	claimed, ok := p.byKey[key]
	if !ok {
		return "", "", nil
	}
	return claimed, "T-" + claimed, nil
}

func TestReconciliationBindsWhatTheOriginalClaimTook(t *testing.T) {
	// Replayed under the PERSISTED key, so sutra returns the same ticket
	// rather than taking a second.
	store := reserved(t, "run-1", "pop-key-1")
	pops := &replayPops{byKey: map[string]string{"pop-key-1": "issue-7"}}

	if err := reconcilePopsWith(store.DB, pops)(t.Context()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(pops.keys) != 1 || pops.keys[0] != "pop-key-1" {
		t.Errorf("the replay used keys %v, not the persisted one", pops.keys)
	}
	got, found, err := store.Find(t.Context(), "run-1")
	if err != nil || !found {
		t.Fatalf("find: %v found=%v", err, found)
	}
	if got.Issue != "issue-7" {
		t.Errorf("the reconciled run bound %q", got.Issue)
	}
}

func TestReconciliationSettlesAnEmptyReplay(t *testing.T) {
	// The claim never took anything. No work is invented for it, and nothing
	// about it is classified as issued.
	store := reserved(t, "run-1", "pop-key-1")
	pops := &replayPops{byKey: map[string]string{}}

	if err := reconcilePopsWith(store.DB, pops)(t.Context()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got, found, err := store.Find(t.Context(), "run-1")
	if err != nil || !found {
		t.Fatalf("find: %v found=%v", err, found)
	}
	if got.State != orchestrator.StateNoWork {
		t.Errorf("an empty replay left the run %q", got.State)
	}
	if got.Issue != "" {
		t.Errorf("an empty replay invented issue %q", got.Issue)
	}
}

func TestAFailedReplayStopsReconciliation(t *testing.T) {
	// "I could not replay the claim" is not "the claim took nothing".
	// Settling on that guess would abandon a real, claimed ticket.
	store := reserved(t, "run-1", "pop-key-1")
	pops := &replayPops{err: errors.New("sutra unreachable")}

	if err := reconcilePopsWith(store.DB, pops)(t.Context()); err == nil {
		t.Fatal("reconciliation continued past a failed replay")
	}
	got, _, err := store.Find(t.Context(), "run-1")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got.State == orchestrator.StateNoWork {
		t.Error("a run whose replay failed was settled as no-work")
	}
}

func TestReconciliationOnAnUnreachableStoreFails(t *testing.T) {
	store := reserved(t, "run-1", "pop-key-1")
	if err := store.DB.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := reconcilePopsWith(store.DB, &replayPops{})(t.Context()); err == nil {
		t.Error("reconciliation against a closed database succeeded")
	}
}
