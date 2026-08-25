package planner_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"kriya/internal/planner"
)

func TestTheIntakeGenerationIsPartOfAPlansIdentity(t *testing.T) {
	// "reverting to a previously seen spec is a fresh plan, not a replay":
	// an operator moving H1 -> H2 -> H1 re-intakes the ORIGINAL hash. Without
	// the generation in the key, that request would resolve to the old H1
	// plan — which is superseded, and whose retry can mutate nothing — so the
	// revert would silently do nothing at all.
	first := planner.DecompositionKey("SHORT", "/spec", "hash-h1", 1)
	revert := planner.DecompositionKey("SHORT", "/spec", "hash-h1", 3)
	if first == revert {
		t.Error("re-intaking a seen spec under a new generation reused the old plan's key")
	}
}

func TestTheSameRequestAlwaysDerivesTheSameKey(t *testing.T) {
	// The key IS the idempotency of decomposition. A key that varied between
	// a crash and its recovery would make the retry a competitor rather than
	// a resumption of its own plan.
	a := planner.DecompositionKey("SHORT", "/spec", "hash-h1", 2)
	b := planner.DecompositionKey("SHORT", "/spec", "hash-h1", 2)
	if a != b {
		t.Errorf("one request derived two keys: %s then %s", a[:12], b[:12])
	}
}

func TestKeyPartsCannotRunTogether(t *testing.T) {
	// Length-prefixed, so a project key ending where a target begins cannot
	// collide with a different split of the same characters. Two different
	// plans sharing a key is two decompositions resolving to one row.
	run := planner.DecompositionKey("SHORTS", "pec", "h", 1)
	split := planner.DecompositionKey("SHORT", "Spec", "h", 1)
	if run == split {
		t.Error("two different requests collided on one decomposition key")
	}
}

// resolveStore is a PlanStore holding one plan, counting its writes.
type resolveStore struct {
	plan   planner.Plan
	found  bool
	writes int
	err    error
}

func (r *resolveStore) Upsert(_ context.Context, p planner.Plan) error {
	r.writes++
	r.plan, r.found = p, true
	return nil
}

func (r *resolveStore) Claim(_ context.Context, p planner.Plan) (bool, error) {
	if r.found {
		return false, nil
	}
	r.writes++
	r.plan, r.found = p, true
	return true, nil
}

func (r *resolveStore) Find(context.Context, string) (planner.Plan, bool, error) {
	return r.plan, r.found, nil
}

func (r *resolveStore) ByKey(_ context.Context, _ string) (planner.Plan, bool, error) {
	if r.err != nil {
		return planner.Plan{}, false, r.err
	}
	return r.plan, r.found, nil
}

const someKey = "0123456789abcdef0123456789abcdef"

func TestAKeyMatchingNoPlanIsFresh(t *testing.T) {
	// The ONLY case that proceeds to insert-and-CAS. Every other outcome
	// resolves to an existing plan and competes with nothing.
	res, err := planner.Resolve(context.Background(), &resolveStore{}, someKey)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !res.Fresh {
		t.Error("a key matching no plan did not resolve as fresh")
	}
	if res.Resume {
		t.Error("a key matching no plan asked to resume something")
	}
}

func TestAnUnknownPlanStateIsRefusedRatherThanResumed(t *testing.T) {
	// A state kriya does not understand is a store it cannot reason about.
	// Resuming would issue tracker mutations against a plan of unknown shape;
	// treating it as fresh would decompose a second time over the first.
	store := &resolveStore{plan: planner.Plan{State: "retiring"}, found: true}
	_, err := planner.Resolve(context.Background(), store, someKey)
	if err == nil {
		t.Fatal("an unknown plan state resolved without complaint")
	}
	if !strings.Contains(err.Error(), "retiring") {
		t.Errorf("the error %q does not name the state", err)
	}
}

func TestAnUnreadablePlanStoreIsNotAFreshKey(t *testing.T) {
	// Fresh means "no plan has this key". A store that could not answer has
	// said nothing of the sort, and treating the silence as fresh would run a
	// second decomposition over a plan that already exists.
	store := &resolveStore{err: errors.New("database is locked")}
	res, err := planner.Resolve(context.Background(), store, someKey)
	if err == nil {
		t.Fatal("an unreadable store resolved without an error")
	}
	if res.Fresh {
		t.Error("an unreadable store resolved as a fresh key")
	}
}

// planningTickets is a TicketStore that can list what it recorded.
type planningTickets struct{ rows []planner.Ticket }

// Put upserts on (plan, ordinal), like the real store. Appending instead
// made the write-ahead row and the post-create row two tickets, so a plan of
// one appeared to hold two.
func (p *planningTickets) Put(_ context.Context, _ string, t planner.Ticket) error {
	for n, existing := range p.rows {
		if existing.Plan == t.Plan && existing.Ordinal == t.Ordinal {
			p.rows[n] = t
			return nil
		}
	}
	p.rows = append(p.rows, t)
	return nil
}

func (p *planningTickets) Find(context.Context, string, string) (planner.Ticket, bool, error) {
	return planner.Ticket{}, false, nil
}

func (p *planningTickets) ForPlan(_ context.Context, key string) ([]planner.Ticket, error) {
	var out []planner.Ticket
	for _, t := range p.rows {
		if t.Plan == key {
			out = append(out, t)
		}
	}
	return out, nil
}

func TestASameKeyRetryNeverDecomposesTwice(t *testing.T) {
	// The wiring, not the rule: Resolve can be perfectly correct while
	// Decompose calls it too late to matter. A retry that reached the tracker
	// before resolving its own key would file a second set of tickets and
	// compete with the plan it was retrying.
	plans, tickets := newMemPlans(), &planningTickets{}
	in, tr, ag := decomposer(t, `{"tickets":[
		{"title":"walking skeleton","body":"","kind":"implementation","skeleton":true,
		 "criteria":["AC-valid-url"],"layers":["http","store"]}]}`)
	in.Plans, in.Tickets = plans, tickets
	ag.Repeat = true

	first, err := in.Decompose(context.Background(), target(), snapshotWith(twoCriteria), 1, "actor")
	if err != nil {
		t.Fatalf("first decomposition: %v", err)
	}
	issuesAfterFirst, callsAfterFirst := tr.issues, len(ag.Requests)

	// The SAME request again: same target, same snapshot, same generation.
	second, err := in.Decompose(context.Background(), target(), snapshotWith(twoCriteria), 1, "actor")
	if err != nil {
		t.Fatalf("same-key retry: %v", err)
	}

	if len(ag.Requests) != callsAfterFirst {
		t.Errorf("the retry invoked the PM agent again (%d calls, was %d)",
			len(ag.Requests), callsAfterFirst)
	}
	if tr.issues != issuesAfterFirst {
		t.Errorf("the retry created %d more issues", tr.issues-issuesAfterFirst)
	}
	// And it must answer with the plan, not with nothing: the caller asked
	// what the decomposition produced and a silent empty set reads as a plan
	// with no work in it.
	if len(second) != len(first) {
		t.Errorf("the retry returned %d tickets; the plan has %d", len(second), len(first))
	}
}

func TestADifferentGenerationDecomposesAgain(t *testing.T) {
	// The control's other half: a NEW intake generation is a new key, so it
	// must NOT resolve to the existing plan. Without this, a test that only
	// proved the retry was quiet would also pass against a Decompose that
	// never decomposed at all.
	plans, tickets := newMemPlans(), &planningTickets{}
	in, tr, ag := decomposer(t, `{"tickets":[
		{"title":"walking skeleton","body":"","kind":"implementation","skeleton":true,
		 "criteria":["AC-valid-url"],"layers":["http","store"]}]}`)
	in.Plans, in.Tickets = plans, tickets
	ag.Repeat = true

	if _, err := in.Decompose(context.Background(), target(), snapshotWith(twoCriteria), 1, "actor"); err != nil {
		t.Fatalf("first decomposition: %v", err)
	}
	issuesAfterFirst := tr.issues

	if _, err := in.Decompose(context.Background(), target(), snapshotWith(twoCriteria), 2, "actor"); err != nil {
		t.Fatalf("second generation: %v", err)
	}
	if tr.issues == issuesAfterFirst {
		t.Error("a new intake generation resolved to the old plan instead of decomposing")
	}
}
