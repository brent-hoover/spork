package planner_test

import (
	"context"
	"strings"
	"testing"

	"kriya/internal/planner"
)

// staleTarget is a target row whose recorded hash is the ORIGINAL one.
//
// This is what EnsureEpic returns after a re-intake, deliberately: the epic's
// idempotency keys were derived from that hash and one epic umbrellas a
// target forever.
func staleTarget() planner.BuildTarget {
	t := target()
	t.SpecHash = "hash-h1-original"
	t.ProjectKey = "SHORT"
	return t
}

func TestAPlanIdentifiesTheSnapshotItDecomposed(t *testing.T) {
	// The plan must be H2's, not the target row's H1. Taking the target's
	// hash made the key, the row and every report claim H1 while the tickets
	// came from H2 — a build reporting the wrong spec version as its own.
	plans := newMemPlans()
	in, _, _ := decomposer(t, `{"tickets":[
		{"title":"walking skeleton","body":"","kind":"implementation","skeleton":true,
		 "criteria":["AC-valid-url"],"layers":["http","store"]}]}`)
	in.Plans, in.Tickets = plans, &planningTickets{}

	snap := snapshotWith(twoCriteria)
	snap.Hash = "hash-h2-current"
	if _, err := in.Decompose(context.Background(), staleTarget(), snap, 2, "actor"); err != nil {
		t.Fatalf("decompose: %v", err)
	}

	got, found, err := plans.Find(context.Background(), "/spec")
	if err != nil || !found {
		t.Fatalf("no plan: %v found=%v", err, found)
	}
	if got.SpecHash != "hash-h2-current" {
		t.Errorf("the plan records spec %q; it decomposed hash-h2-current", got.SpecHash)
	}
	want := planner.DecompositionKey("SHORT", "/spec", "hash-h2-current", 2)
	if got.Key != want {
		t.Error("the plan's key was derived from the target's hash, not the snapshot's")
	}
}

// keyRecordingTracker records the idempotency key of every mutation.
type keyRecordingTracker struct {
	countingTracker
	mutationKeys []string
}

func (k *keyRecordingTracker) CreateIssue(
	ctx context.Context, project, title, body, actor, idem string,
) (string, error) {
	k.mutationKeys = append(k.mutationKeys, idem)
	return k.countingTracker.CreateIssue(ctx, project, title, body, actor, idem)
}

func (k *keyRecordingTracker) AddRelation(
	ctx context.Context, issue, kind, to, actor, idem string,
) error {
	k.mutationKeys = append(k.mutationKeys, idem)
	return k.countingTracker.AddRelation(ctx, issue, kind, to, actor, idem)
}

func (k *keyRecordingTracker) AssignIssue(
	ctx context.Context, issue, assignee, actor, idem string,
) error {
	k.mutationKeys = append(k.mutationKeys, idem)
	return k.countingTracker.AssignIssue(ctx, issue, assignee, actor, idem)
}

// decomposeUnder runs one decomposition and returns the keys it presented.
func decomposeUnder(t *testing.T, specHash string, generation int) []string {
	t.Helper()
	tr := &keyRecordingTracker{}
	in, _, _ := decomposer(t, `{"tickets":[
		{"title":"walking skeleton","body":"","kind":"implementation","skeleton":true,
		 "criteria":["AC-valid-url"],"layers":["http","store"]}]}`)
	in.Tracker = tr
	snap := snapshotWith(twoCriteria)
	snap.Hash = specHash
	if _, err := in.Decompose(context.Background(), staleTarget(), snap, generation, "actor"); err != nil {
		t.Fatalf("decompose: %v", err)
	}
	return tr.mutationKeys
}

func TestARevertPresentsFreshTrackerKeys(t *testing.T) {
	// H1 -> H2 -> H1. The third decomposition pins the SAME snapshot as the
	// first, so keys derived from (target, spec hash) would be identical:
	// sutra settles each call under its key and replays the ORIGINAL tickets,
	// and the revert produces no new work at all. The decomposition key
	// carries the generation, which is what keeps them apart.
	first := decomposeUnder(t, "hash-h1", 1)
	revert := decomposeUnder(t, "hash-h1", 3)

	if len(first) == 0 || len(first) != len(revert) {
		t.Fatalf("the two decompositions issued %d and %d mutations", len(first), len(revert))
	}
	for n := range first {
		if first[n] == revert[n] {
			t.Fatalf("mutation %d presented the same key twice (%s): the revert replays the old plan",
				n, first[n][:12])
		}
	}
}

func TestTheSamePlanPresentsStableTrackerKeys(t *testing.T) {
	// The other half. Keys that varied per attempt would make every recovery
	// create duplicate tickets instead of replaying into the same ones.
	first := decomposeUnder(t, "hash-h1", 1)
	again := decomposeUnder(t, "hash-h1", 1)
	for n := range first {
		if first[n] != again[n] {
			t.Fatalf("mutation %d presented a different key on a retry of the same plan", n)
		}
	}
}

func TestARetryAnswersWithItsOwnPlansTickets(t *testing.T) {
	// A target accumulates plans. Answering a retry from the TARGET's tickets
	// hands back every generation's work as though one decomposition produced
	// it — so a caller sees a plan twice the size of the one it asked about.
	plans, tickets := newMemPlans(), &planningTickets{}
	in, _, ag := decomposer(t, `{"tickets":[
		{"title":"walking skeleton","body":"","kind":"implementation","skeleton":true,
		 "criteria":["AC-valid-url"],"layers":["http","store"]}]}`)
	in.Plans, in.Tickets = plans, tickets
	ag.Repeat = true

	snap := snapshotWith(twoCriteria)
	snap.Hash = "hash-h1"
	ctx := context.Background()
	for _, generation := range []int{1, 2} {
		if _, err := in.Decompose(ctx, staleTarget(), snap, generation, "actor"); err != nil {
			t.Fatalf("generation %d: %v", generation, err)
		}
	}
	if len(tickets.rows) != 2 {
		t.Fatalf("two decompositions recorded %d tickets", len(tickets.rows))
	}

	// A retry of generation 1 must answer with generation 1's single ticket.
	got, err := in.Decompose(ctx, staleTarget(), snap, 1, "actor")
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("the retry answered with %d tickets; its plan has 1", len(got))
	}
	for _, ticket := range got {
		if ticket.Plan != planner.DecompositionKey("SHORT", "/spec", "hash-h1", 1) {
			t.Errorf("the retry returned a ticket from plan %s", ticket.Plan)
		}
	}
}

func TestEveryPlannedTicketNamesItsPlan(t *testing.T) {
	tickets := &planningTickets{}
	in, _, _ := decomposer(t, `{"tickets":[
		{"title":"walking skeleton","body":"","kind":"implementation","skeleton":true,
		 "criteria":["AC-valid-url"],"layers":["http","store"]}]}`)
	in.Plans, in.Tickets = newMemPlans(), tickets

	snap := snapshotWith(twoCriteria)
	snap.Hash = "hash-h1"
	if _, err := in.Decompose(context.Background(), staleTarget(), snap, 1, "actor"); err != nil {
		t.Fatalf("decompose: %v", err)
	}
	for _, ticket := range tickets.rows {
		if ticket.Plan == "" {
			t.Errorf("ticket %q names no plan, so nothing can tell which decomposition made it",
				ticket.Title)
		}
		if !strings.HasPrefix(ticket.Plan,
			planner.DecompositionKey("SHORT", "/spec", "hash-h1", 1)[:8]) {
			t.Errorf("ticket %q names plan %s", ticket.Title, ticket.Plan)
		}
	}
}
