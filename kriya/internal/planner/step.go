package planner

import (
	"context"
	"fmt"
)

// Mutation kinds, in the order a plan's phases issue them.
const (
	StepCreate = "create"
	StepParent = "parent"
	StepBlock  = "block"
	StepAssign = "assign"
)

// Step durability states.
//
// ISSUED is the one that matters. A step marked issued is one whose call may
// or may not have landed — the crash window is exactly there — so recovery
// replays it under its persisted key rather than assuming either outcome.
const (
	StepPending  = "pending"
	StepIssued   = "issued"
	StepComplete = "complete"
)

// Step is one durable mutation in a plan's sequence.
//
// The whole sequence is persisted BEFORE the first call, which is what lets
// recovery replay the exact plan rather than asking the PM again. Re-asking is
// not a smaller version of the same thing: an agent is not a pure function, so
// a second answer can differ, and the differing request would then be sent
// under an idempotency key the first answer already settled — sutra returns
// the ORIGINAL, and the plan silently becomes a mixture of two decompositions.
type Step struct {
	Plan string
	// Seq orders the whole plan's sequence. Phases are contiguous ranges of
	// it, so replaying in Seq order reproduces the barriers for free.
	Seq int
	// Ordinal is the ticket this step acts on, indexing the plan's ticket
	// list. Relations carry a second one in Other.
	Ordinal int
	Other   int
	Kind    string
	// Key is the idempotency key, derived when the sequence was built and
	// persisted with it. Recovery presents THIS key, never a freshly derived
	// one — a derivation that drifted would create duplicates instead of
	// replaying.
	Key   string
	State string
	// Issue is the id a create returned, filled in when it does. Later steps
	// read it rather than a fresh lookup by title.
	Issue string
}

// StepStore persists a plan's mutation sequence.
type StepStore interface {
	// Write persists the whole sequence in one transaction, before any call.
	// A partially written sequence is one recovery would replay incompletely.
	Write(ctx context.Context, steps []Step) error
	// ForPlan reads a plan's sequence in Seq order.
	ForPlan(ctx context.Context, plan string) ([]Step, error)
	// Mark moves one step's state, recording the issue id a create returned.
	Mark(ctx context.Context, plan string, seq int, state, issue string) error
}

// buildSequence derives a plan's full ordered mutation sequence.
//
// Every step and every key, computed once and persisted before anything is
// called. The ORDER here is the phase barriers: creates and their parenting
// first, then every blocking relation, then assignments with the spikes ahead
// of implementation work and the walking skeleton leading it.
func buildSequence(plan Plan, tickets []Ticket) []Step {
	var steps []Step
	add := func(kind string, ordinal, other int, name string) {
		steps = append(steps, Step{
			Plan: plan.Key, Seq: len(steps), Ordinal: ordinal, Other: other,
			Kind: kind, Key: planStepKey(name, plan.Key), State: StepPending,
		})
	}

	for n := range tickets {
		add(StepCreate, n, -1, fmt.Sprintf("ticket-%d", n))
		add(StepParent, n, -1, fmt.Sprintf("parent-%d", n))
	}
	for n, spike := range tickets {
		if spike.Kind != KindSpike {
			continue
		}
		blocked := make(map[string]bool, len(spike.Blocks))
		for _, id := range spike.Blocks {
			blocked[id] = true
		}
		for m, other := range tickets {
			if n == m || !dependsOn(other, blocked) {
				continue
			}
			add(StepBlock, n, m, fmt.Sprintf("blocks-%d-%d", n, m))
		}
	}
	for _, kind := range []string{KindSpike, KindImplementation} {
		for _, n := range assignmentOrder(tickets, kind) {
			add(StepAssign, n, -1, fmt.Sprintf("assign-%d", n))
		}
	}
	return steps
}
