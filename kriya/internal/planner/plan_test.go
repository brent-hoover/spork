package planner_test

import (
	"context"
	"errors"
	"testing"

	"kriya/internal/planner"
)

// memPlans is an in-memory PlanStore.
type memPlans struct {
	rows map[string]planner.Plan
	err  error
}

func newMemPlans() *memPlans { return &memPlans{rows: map[string]planner.Plan{}} }

func (m *memPlans) Upsert(_ context.Context, p planner.Plan) error {
	if m.err != nil {
		return m.err
	}
	m.rows[p.TargetKey] = p
	return nil
}

func (m *memPlans) Find(_ context.Context, targetKey string) (planner.Plan, bool, error) {
	if m.err != nil {
		return planner.Plan{}, false, m.err
	}
	p, ok := m.rows[targetKey]
	return p, ok, nil
}

// ByKey resolves a decomposition key, which is a plan's real identity.
func (m *memPlans) ByKey(_ context.Context, key string) (planner.Plan, bool, error) {
	if m.err != nil {
		return planner.Plan{}, false, m.err
	}
	for _, p := range m.rows {
		if p.Key == key {
			return p, true, nil
		}
	}
	return planner.Plan{}, false, nil
}

func TestAPlanIsWholeOnlyOnceDecompositionStampsIt(t *testing.T) {
	// Completion detection reads this stamp before anything else. A plan
	// mid-decomposition has a ticket set that is still growing, and "every
	// ticket so far is complete" says nothing about a build that is done.
	plans := newMemPlans()
	in, tr, _ := decomposer(t, `{"tickets":[
		{"title":"walking skeleton","body":"","kind":"implementation","skeleton":true,"criteria":["AC-valid-url"],"layers":["http","store"]},
		{"title":"reject bad urls","body":"","kind":"implementation","criteria":["AC-bad-url"],"layers":["http","store"]}]}`)
	in.Plans = plans

	if _, found, _ := plans.Find(context.Background(), "/spec"); found {
		t.Fatal("a plan row existed before decomposition ran")
	}
	if _, err := in.Decompose(context.Background(), target(), snapshotWith(twoCriteria), 1, "actor"); err != nil {
		t.Fatalf("decompose: %v", err)
	}
	got, found, err := plans.Find(context.Background(), "/spec")
	if err != nil || !found {
		t.Fatalf("no plan row after decomposition: %v found=%v", err, found)
	}
	if got.State != planner.PlanActive || !got.Completed {
		t.Errorf("the plan is %q completed=%v after decomposition finished", got.State, got.Completed)
	}
	if got.Tickets != tr.issues {
		t.Errorf("the plan claims %d tickets; decomposition created %d", got.Tickets, tr.issues-1)
	}
}

func TestAFailedDecompositionLeavesNoCompletedStamp(t *testing.T) {
	// The stamp is what arms completion detection. Stamping a decomposition
	// that did not finish would let a build report done against half a plan.
	plans := newMemPlans()
	in, tr, _ := decomposer(t, `{"tickets":[
		{"title":"walking skeleton","body":"","kind":"implementation","skeleton":true,"criteria":["AC-valid-url"],"layers":["http","store"]},
		{"title":"reject bad urls","body":"","kind":"implementation","criteria":["AC-bad-url"],"layers":["http","store"]}]}`)
	in.Plans = plans
	tr.assignErr = errors.New("tracker unavailable")

	if _, err := in.Decompose(context.Background(), target(), snapshotWith(twoCriteria), 1, "actor"); err == nil {
		t.Fatal("expected the decomposition to fail")
	}
	got, found, _ := plans.Find(context.Background(), "/spec")
	if found && got.Completed {
		t.Error("a decomposition that did not finish stamped its plan whole")
	}
}

func TestAPlanIsRecordedBeforeItsTicketsSoACrashIsVisible(t *testing.T) {
	// Write-ahead, like every other external step: a crash mid-decomposition
	// must leave a row saying a plan was being built, not nothing at all.
	plans := newMemPlans()
	in, tr, _ := decomposer(t, `{"tickets":[
		{"title":"walking skeleton","body":"","kind":"implementation","skeleton":true,"criteria":["AC-valid-url"],"layers":["http","store"]}]}`)
	in.Plans = plans
	tr.failIssue = errors.New("sutra unreachable")

	if _, err := in.Decompose(context.Background(), target(), snapshotWith(twoCriteria), 1, "actor"); err == nil {
		t.Fatal("expected the decomposition to fail")
	}
	got, found, _ := plans.Find(context.Background(), "/spec")
	if !found {
		t.Fatal("no plan row survived the failure; there is nothing to recover from")
	}
	// ACTIVE without the stamp: activation precedes the creation, wiring and
	// assignment phases, so this is exactly the state a crash mid-phase
	// leaves — and the state a resumed retry must recognise.
	if got.State != planner.PlanActive || got.Completed {
		t.Errorf("the plan is %q completed=%v, want active mid-decomposition", got.State, got.Completed)
	}
}

func TestADecompositionWithNoPlanStoreStillWorks(t *testing.T) {
	// The store is optional in the same way the ticket store is: a
	// module-level test of decomposition itself does not need one.
	in, _, _ := decomposer(t, `{"tickets":[
		{"title":"walking skeleton","body":"","kind":"implementation","skeleton":true,"criteria":["AC-valid-url"],"layers":["http","store"]}]}`)
	if _, err := in.Decompose(context.Background(), target(), snapshotWith(twoCriteria), 1, "actor"); err != nil {
		t.Fatalf("decompose: %v", err)
	}
}
