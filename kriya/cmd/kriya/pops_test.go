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

func TestAPoppedTicketBuildsAgainstItsOwnPlansSnapshot(t *testing.T) {
	// The binding exists to say which decomposition produced a ticket's
	// acceptance criteria. Building it against the target's LATEST snapshot
	// would validate it against criteria that never described it — which is
	// exactly what a superseded plan's carried work would hit.
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	plans, tickets := planner.SQLPlans{DB: db}, planner.SQLTickets{DB: db}
	for _, p := range []planner.Plan{
		{Key: "plan-old", TargetKey: "/spec", SpecHash: "hash-old",
			Generation: 1, State: planner.PlanSuperseded},
		{Key: "plan-head", TargetKey: "/spec", SpecHash: "hash-new",
			Generation: 2, State: planner.PlanActive, Predecessor: "plan-old"},
	} {
		if err := plans.Upsert(t.Context(), p); err != nil {
			t.Fatalf("seed %s: %v", p.Key, err)
		}
	}
	// The ticket lives ONLY in the superseded plan: the amended spec dropped
	// it, but a live build still holds it.
	if err := tickets.Put(t.Context(), "/spec", planner.Ticket{
		Title: "dropped work", IssueID: "issue-1", Plan: "plan-old", Ordinal: 0,
	}); err != nil {
		t.Fatalf("seed ticket: %v", err)
	}
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO plan_head (target_key, current, generation, fence) VALUES (?, ?, 2, 0)`,
		"/spec", "plan-head"); err != nil {
		t.Fatalf("install head: %v", err)
	}

	bound, err := binderFor(db).Bind(t.Context(), "/spec", "issue-1")
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if bound.Plan.SpecHash != "hash-old" {
		t.Errorf("the pop bound snapshot %q, not the one that described the ticket",
			bound.Plan.SpecHash)
	}
}

func TestATicketFromAnotherTargetIsRefusedAtBinding(t *testing.T) {
	// A pop is identity-wide, so another repository's ticket can arrive.
	// Building it here would run this target's gate commands over the wrong
	// codebase.
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := (planner.SQLPlans{DB: db}).Upsert(t.Context(), planner.Plan{
		Key: "plan-head", TargetKey: "/spec", SpecHash: "h",
		Generation: 1, State: planner.PlanActive,
	}); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO plan_head (target_key, current, generation, fence) VALUES (?, ?, 1, 0)`,
		"/spec", "plan-head"); err != nil {
		t.Fatalf("install head: %v", err)
	}

	if _, err := binderFor(db).Bind(t.Context(), "/spec", "issue-from-elsewhere"); err == nil {
		t.Error("another target's ticket bound to this one")
	}
}

func TestAReservedSpikeIsFullyInitialisedOnAdoption(t *testing.T) {
	// A reservation knows its claim, not what kind of work the claim got.
	// Testing "is this initialised" on a FIELD can be answered wrongly by a
	// column default — kind defaults to implementation — so a reservation
	// looked fully built, resumeOrStart returned it untouched, and spikes went
	// down the implementation pipeline with no research path and no findings.
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := orchestrator.SQLStore{DB: db}
	if err := store.Reserve(t.Context(), orchestrator.BuildRun{
		ID: "pop-key-1", Plan: "/spec", PopKey: "pop-key-1",
	}, planFence(db), 0); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := store.Bind(t.Context(), "pop-key-1", "issue-1", "spike: is it headless"); err != nil {
		t.Fatalf("bind: %v", err)
	}

	got, err := resumeOrStart(t.Context(), store, planner.Ticket{
		Title: "spike: is it headless", IssueID: "issue-1", Kind: planner.KindSpike,
	}, "/spec")
	if err != nil {
		t.Fatalf("resume or start: %v", err)
	}
	if got.Kind != planner.KindSpike {
		t.Errorf("the adopted reservation is kind %q", got.Kind)
	}
	if got.State != orchestrator.StateResearchLoop {
		t.Errorf("the adopted spike is %q, not on the research path", got.State)
	}
	// And it keeps the reservation's identity, so the claim it owns is the
	// one this run finishes.
	if got.ID != "pop-key-1" {
		t.Errorf("adoption created a new run %q", got.ID)
	}
	if got.PopKey != "pop-key-1" {
		t.Error("the adopted run lost its pop key and could not replay its claim")
	}
}

func TestARunAlreadyUnderWayIsNotReinitialised(t *testing.T) {
	// The other half: a rework or a recovered build must be resumed, not
	// restarted from scratch.
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := orchestrator.SQLStore{DB: db}
	if err := store.Upsert(t.Context(), orchestrator.BuildRun{
		ID: "run-1", Ticket: "T", Issue: "issue-1", Plan: "/spec",
		State: orchestrator.StateDevLoop, Head: "abc123", Attempt: 3,
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	got, err := resumeOrStart(t.Context(), store, planner.Ticket{
		Title: "T", IssueID: "issue-1", Kind: planner.KindImplementation,
	}, "/spec")
	if err != nil {
		t.Fatalf("resume or start: %v", err)
	}
	if got.State != orchestrator.StateDevLoop || got.Head != "abc123" || got.Attempt != 3 {
		t.Errorf("a run under way was reinitialised: %+v", got)
	}
}

func TestARetriedPopReservesTheSameRun(t *testing.T) {
	// The pop key is stable across attempts — same target, same ordinal — so
	// a retried reservation must land on the same row. A fresh identifier per
	// attempt inserted a SECOND run for one idempotent pop, and ForTicket
	// then picked the newest bare reservation, abandoning the original run
	// and everything it had done.
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := orchestrator.SQLStore{DB: db}
	for attempt := range 2 {
		// The fence version is read afresh each time, as the loop does: the
		// first admission bumped it, so a stale version would lose the CAS
		// rather than showing what a retried reservation does.
		fence, err := planFence(db).Read(t.Context())
		if err != nil {
			t.Fatalf("read fence: %v", err)
		}
		if err := store.Reserve(t.Context(), orchestrator.BuildRun{
			ID: "pop-key-1", Plan: "/spec", PopKey: "pop-key-1",
		}, planFence(db), fence.Version); err != nil {
			t.Fatalf("reserve %d: %v", attempt, err)
		}
	}
	runs, err := store.ForPlan(t.Context(), "/spec")
	if err != nil {
		t.Fatalf("for plan: %v", err)
	}
	if len(runs) != 1 {
		t.Errorf("a retried pop produced %d runs", len(runs))
	}
}
