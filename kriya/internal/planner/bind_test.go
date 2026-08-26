package planner_test

import (
	"context"
	"strings"
	"testing"

	"kriya/internal/planner"
)

// binding builds a Binder over a real chain.
func binding(t *testing.T) (planner.Binder, planner.SQLPlans, planner.SQLHeads, *planningTickets) {
	t.Helper()
	db := sqlDB(t, planner.PlanMigration, planner.PlanKeyMigration,
		planner.PlanPredecessorMigration, planner.HeadMigration, planner.FenceMigration)
	plans, heads := planner.SQLPlans{DB: db}, planner.SQLHeads{DB: db}
	tickets := &planningTickets{}
	return planner.Binder{Plans: plans, Heads: heads, Tickets: tickets}, plans, heads, tickets
}

// chainOf writes a plan and, optionally, its row for issue-1.
func chainOf(
	t *testing.T, plans planner.SQLPlans, tickets *planningTickets,
	key, predecessor, state string, holds bool,
) {
	t.Helper()
	ctx := context.Background()
	if err := plans.Upsert(ctx, planner.Plan{
		Key: key, TargetKey: "/spec", SpecHash: "hash-" + key,
		Generation: 1, State: state, Predecessor: predecessor,
	}); err != nil {
		t.Fatalf("write %s: %v", key, err)
	}
	if !holds {
		return
	}
	if err := tickets.Put(ctx, "/spec", planner.Ticket{
		Title: "work", IssueID: "issue-1", Plan: key, Ordinal: 0,
	}); err != nil {
		t.Fatalf("write %s row: %v", key, err)
	}
}

// pointHead installs the head row.
func pointHead(t *testing.T, heads planner.SQLHeads, key string, fence int) {
	t.Helper()
	if _, err := heads.DB.ExecContext(context.Background(),
		`INSERT INTO plan_head (target_key, current, generation, fence) VALUES (?, ?, 1, ?)
		 ON CONFLICT(target_key) DO UPDATE SET current = excluded.current, fence = excluded.fence`,
		"/spec", key, fence); err != nil {
		t.Fatalf("install head: %v", err)
	}
}

func TestBindingRefusesATargetWithNoHead(t *testing.T) {
	// Without a head there is no chain to walk, and guessing a plan would
	// build the ticket against whatever snapshot happened to be nearest.
	b, _, _, _ := binding(t)
	_, err := b.Bind(context.Background(), "/spec", "issue-1")
	if err == nil {
		t.Fatal("a target with no head bound anyway")
	}
	if !strings.Contains(err.Error(), "no plan head") {
		t.Errorf("the error %q does not say the head is missing", err)
	}
}

func TestBindingRefusesATicketNoPlanHolds(t *testing.T) {
	// A pop is identity-wide, so a ticket from another target can arrive.
	// Binding it to this target's head would build the wrong repository's
	// work against this target's snapshot.
	b, plans, heads, tickets := binding(t)
	chainOf(t, plans, tickets, "plan-head", "", planner.PlanActive, false)
	pointHead(t, heads, "plan-head", 0)

	_, err := b.Bind(context.Background(), "/spec", "issue-1")
	if err == nil {
		t.Fatal("a ticket no plan holds was bound")
	}
	if !strings.Contains(err.Error(), "belongs to no plan") {
		t.Errorf("the error %q does not say the ticket is unplanned", err)
	}
}

func TestBindingRefusesAChainNamingAMissingPlan(t *testing.T) {
	// The head row and the plan rows can only disagree if something wrote one
	// without the other. Walking past it would silently shorten the chain and
	// bind the wrong plan.
	b, plans, heads, tickets := binding(t)
	chainOf(t, plans, tickets, "plan-head", "plan-gone", planner.PlanActive, true)
	pointHead(t, heads, "plan-head", 0)

	_, err := b.Bind(context.Background(), "/spec", "issue-1")
	if err == nil {
		t.Fatal("a chain naming a missing plan bound anyway")
	}
	if !strings.Contains(err.Error(), "has no row") {
		t.Errorf("the error %q does not name the missing plan", err)
	}
}

func TestBindingStopsOnACycle(t *testing.T) {
	// A predecessor pointer that cycled would walk forever. A corrupt chain
	// must fail — or here, terminate — rather than hang.
	b, plans, heads, tickets := binding(t)
	chainOf(t, plans, tickets, "plan-a", "plan-b", planner.PlanSuperseded, true)
	chainOf(t, plans, tickets, "plan-b", "plan-a", planner.PlanSuperseded, true)
	pointHead(t, heads, "plan-a", 0)

	got, err := b.Bind(context.Background(), "/spec", "issue-1")
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if got.Plan.Key == "" {
		t.Error("a cyclic chain bound nothing")
	}
}

func TestAnUnactivatedHeadAloneBindsNothing(t *testing.T) {
	// An unactivated head never binds, and if it is the only plan holding the
	// ticket there is nothing else to bind to. Refusing is the honest answer:
	// the work is not ready to start.
	b, plans, heads, tickets := binding(t)
	chainOf(t, plans, tickets, "plan-head", "", planner.PlanActive, true)
	pointHead(t, heads, "plan-head", 1)

	_, err := b.Bind(context.Background(), "/spec", "issue-1")
	if err == nil {
		t.Fatal("an unactivated head bound the ticket")
	}
	if !strings.Contains(err.Error(), "never binds") {
		t.Errorf("the error %q does not say why", err)
	}
}

func TestAWalkedPastRowIsStampedNeutrally(t *testing.T) {
	// The stamp says "a binding walked past this", not what became of the
	// work. Retirement is what decides whether a row was retired, carried
	// forward, bound or completed — a binding makes no such judgement.
	b, plans, heads, tickets := binding(t)
	chainOf(t, plans, tickets, "plan-old", "", planner.PlanSuperseded, true)
	chainOf(t, plans, tickets, "plan-head", "plan-old", planner.PlanActive, true)
	pointHead(t, heads, "plan-head", 0)

	got, err := b.Bind(context.Background(), "/spec", "issue-1")
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if got.Plan.Key != "plan-old" {
		t.Fatalf("the pop bound %q", got.Plan.Key)
	}
	if len(got.WalkedPast) != 1 {
		t.Fatalf("the binding walked past %d rows, want 1", len(got.WalkedPast))
	}
	for _, row := range tickets.rows {
		if row.Plan != "plan-head" {
			continue
		}
		if !row.Consumed {
			t.Error("the walked-past row was not stamped")
		}
		if row.Disposition != "" {
			t.Errorf("the binding stamped a disposition %q, which is retirement's to decide",
				row.Disposition)
		}
	}
}

func TestAPlanThatOmitsTheTicketDoesNotShortenTheChain(t *testing.T) {
	// Depth counts hops along the REAL chain. A plan that dropped the ticket
	// must not make its predecessor look nearer the head than it is, or
	// "nearest" and "deepest" both answer wrongly.
	b, plans, heads, tickets := binding(t)
	chainOf(t, plans, tickets, "plan-oldest", "", planner.PlanSuperseded, true)
	chainOf(t, plans, tickets, "plan-middle", "plan-oldest", planner.PlanSuperseded, false)
	chainOf(t, plans, tickets, "plan-head", "plan-middle", planner.PlanActive, true)
	pointHead(t, heads, "plan-head", 0)

	got, err := b.Bind(context.Background(), "/spec", "issue-1")
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	// The deepest unstamped superseded row is the oldest, two hops down —
	// not the head, and not the middle plan which holds nothing.
	if got.Plan.Key != "plan-oldest" {
		t.Errorf("the pop bound %q", got.Plan.Key)
	}
}
