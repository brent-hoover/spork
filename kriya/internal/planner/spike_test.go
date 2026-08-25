package planner_test

import (
	"context"
	"strings"
	"testing"

	"kriya/internal/planner"
)

const riskCriteria = `
requirements:
  - id: REQ-review
    acceptance:
      - id: AC-headless
      - id: AC-verdict
  - id: REQ-create
    acceptance:
      - id: AC-valid-url
`

// spikePlan is a decomposition carrying one spike and two dependents.
const spikePlan = `{"tickets":[
  {"title":"spike: can the review tool run headless","body":"",
   "kind":"spike","criteria":["AC-headless"],"blocks":["AC-verdict","AC-headless"]},
  {"title":"drive the review tool","body":"","skeleton":true,
   "kind":"implementation","criteria":["AC-verdict"],"layers":["http","store"]},
  {"title":"create a short link","body":"",
   "kind":"implementation","criteria":["AC-valid-url"],"layers":["http","store"]}]}`

func TestARiskBecomesASpikeThatBlocksItsDependents(t *testing.T) {
	// "a spike ticket exists for the risk ... and the spike blocks every
	// ticket depending on the risk's answer."
	in, tr, _ := decomposer(t, spikePlan)
	tickets, err := in.Decompose(context.Background(), target(), snapshotWith(riskCriteria), "actor")
	if err != nil {
		t.Fatalf("decompose: %v", err)
	}
	if len(tickets) != 3 {
		t.Fatalf("produced %d tickets", len(tickets))
	}
	var spike planner.Ticket
	for _, tk := range tickets {
		if tk.Kind == planner.KindSpike {
			spike = tk
		}
	}
	if spike.IssueID == "" {
		t.Fatal("no spike ticket was created")
	}
	// The blocking relation, from the spike to the dependent.
	var dependent string
	for _, tk := range tickets {
		if tk.Kind == planner.KindImplementation && tk.Criteria[0] == "AC-verdict" {
			dependent = tk.IssueID
		}
	}
	if !tr.blocks(spike.IssueID, dependent) {
		t.Errorf("the spike does not block its dependent: %v", tr.wired)
	}
}

func TestATicketNotDependingOnTheRiskIsNotBlocked(t *testing.T) {
	// "tickets not depending on the risk are not blocked by it." Blocking
	// everything would serialize a plan that has parallel work in it.
	in, tr, _ := decomposer(t, spikePlan)
	tickets, err := in.Decompose(context.Background(), target(), snapshotWith(riskCriteria), "actor")
	if err != nil {
		t.Fatalf("decompose: %v", err)
	}
	var spike, independent string
	for _, tk := range tickets {
		switch {
		case tk.Kind == planner.KindSpike:
			spike = tk.IssueID
		case tk.Criteria[0] == "AC-valid-url":
			independent = tk.IssueID
		}
	}
	if tr.blocks(spike, independent) {
		t.Error("a ticket depending on no risk was blocked anyway")
	}
}

func TestASpikeDoesNotBlockItself(t *testing.T) {
	// Its own criterion appears in its blocks list — the risk's answer is
	// what it produces — and a self-block is a ticket nothing can ever pop.
	in, tr, _ := decomposer(t, spikePlan)
	tickets, err := in.Decompose(context.Background(), target(), snapshotWith(riskCriteria), "actor")
	if err != nil {
		t.Fatalf("decompose: %v", err)
	}
	for _, tk := range tickets {
		if tk.Kind == planner.KindSpike && tr.blocks(tk.IssueID, tk.IssueID) {
			t.Error("the spike blocks itself")
		}
	}
}

func TestEverySpikeIsAssignedBeforeAnyImplementation(t *testing.T) {
	// "every spike assignment completes before any implementation assignment
	// begins." sutra's work stack is FIFO and no tracker-side priority is
	// assumed, so assignment ORDER is the whole mechanism.
	in, tr, _ := decomposer(t, spikePlan)
	tickets, err := in.Decompose(context.Background(), target(), snapshotWith(riskCriteria), "actor")
	if err != nil {
		t.Fatalf("decompose: %v", err)
	}
	spikes := map[string]bool{}
	for _, tk := range tickets {
		if tk.Kind == planner.KindSpike {
			spikes[tk.IssueID] = true
		}
	}
	seenImplementation := false
	for _, issue := range tr.assignOrder {
		if spikes[issue] {
			if seenImplementation {
				t.Errorf("a spike was assigned after implementation work: %v", tr.assignOrder)
			}
			continue
		}
		seenImplementation = true
	}
}

func TestATicketWithNoKindIsRefused(t *testing.T) {
	// The kind decides which path a ticket takes: a spike carries a document
	// deliverable and skips the gate chain entirely. Guessing would send
	// research work through the gates and fail it for having no code.
	in, _, _ := decomposer(t, `{"tickets":[
		{"title":"whatever","body":"","criteria":["AC-valid-url"]}]}`)
	_, err := in.Decompose(context.Background(), target(), snapshotWith(riskCriteria), "actor")
	if err == nil {
		t.Fatal("a ticket with no kind was accepted")
	}
	if !strings.Contains(err.Error(), "kind") {
		t.Errorf("the error does not name the missing kind: %v", err)
	}
}

func TestASpikeBlockingAnUnknownCriterionIsRefused(t *testing.T) {
	// The same rule as citations: an invented id produces a blocking relation
	// against nothing, and the dependents it was meant to protect run anyway.
	in, tr, _ := decomposer(t, `{"tickets":[
		{"title":"spike","body":"","kind":"spike","criteria":["AC-headless"],
		 "blocks":["AC-invented"]}]}`)
	_, err := in.Decompose(context.Background(), target(), snapshotWith(riskCriteria), "actor")
	if err == nil {
		t.Fatal("a spike blocking an id the snapshot does not carry was accepted")
	}
	if tr.issues != 0 {
		t.Errorf("%d tickets were created before the refusal", tr.issues)
	}
}

func TestAnImplementationTicketCannotBlock(t *testing.T) {
	// Blocking is what a RISK does. An implementation ticket declaring
	// blocks would serialize the plan on work that answers no question.
	in, _, _ := decomposer(t, `{"tickets":[
		{"title":"impl","body":"","kind":"implementation","criteria":["AC-valid-url"],"layers":["http","store"],
		 "blocks":["AC-headless"]}]}`)
	if _, err := in.Decompose(context.Background(), target(),
		snapshotWith(riskCriteria), "actor"); err == nil {
		t.Fatal("an implementation ticket declared a blocking relation")
	}
}
