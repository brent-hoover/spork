// Package orchestrator is the outer loop as a deterministic state machine.
//
// It pops tickets, advances BuildRun states from an explicit table, and
// delegates every stage. It contains no `if` that decides WHAT should happen —
// only `if` that decides whether a write succeeded. If a change requires this
// package to decide anything, the change is wrong
// (CON-deterministic-orchestrator).
package orchestrator

import (
	"context"
	"fmt"

	"kriya/internal/clock"
)

// State is a BuildRun's position in the chain.
type State string

// The sixteen BuildRun states. M3 delivers the code traversal; the research
// path and the completion states land with M4.
const (
	StateQueued           State = "queued"
	StateNoWork           State = "no-work"
	StateDevLoop          State = "dev-loop"
	StateGates            State = "gates"
	StatePOValidation     State = "po-validation"
	StateReviewSubmitted  State = "review-submitted"
	StateAwaitingOperator State = "awaiting-operator"
	StateMerging          State = "merging"
	StateMerged           State = "merged"
	StateFailed           State = "failed"
	StateCancelling       State = "cancelling"
	StateCancelled        State = "cancelled"
	StateResearchLoop     State = "research-loop"
	StateFindingSubmitted State = "finding-submitted"
	StateCompleting       State = "completing"
	StateClosed           State = "closed"
)

// Stage names the work a transition delegates.
type Stage string

// The stages M3 delivers.
const (
	StageWorkspace Stage = "workspace"
	StageDevLoop   Stage = "dev-loop"
	StageGates     Stage = "gates"
	StageValidate  Stage = "po-validation"
)

// BuildRun is one ticket's journey through the chain.
type BuildRun struct {
	ID     string
	Ticket string
	Plan   string
	State  State
	// GatedBase is the commit the gate chain ran at. A result from any other
	// commit never satisfies the chain.
	GatedBase string
	// Error is the durable cause an awaiting-operator run parked with, so the
	// inbox can say why rather than only that.
	Error string
	// Attempt counts gate-chain rounds, so results pin to the round that
	// produced them.
	Attempt int
	// RoundLimit is snapshotted at the run's creation. A later change to the
	// configured limit affects only future runs; this one keeps the limit it
	// started under, and recovery reads it from here rather than from config.
	RoundLimit int
}

// Transition is one row of the table.
//
// Every branch a reader might expect — a gate failed, a review came back with
// changes, a merge conflicted — is a row here, not a code path. Adding a stage
// is adding a row.
type Transition struct {
	From   State
	Stage  Stage
	OnOK   State
	OnFail State
}

// Table is the machine.
//
// Read it top to bottom and you have the whole traversal; there is nowhere
// else a state change can come from.
var Table = []Transition{
	{From: StateQueued, Stage: StageWorkspace, OnOK: StateDevLoop, OnFail: StateAwaitingOperator},
	{From: StateDevLoop, Stage: StageDevLoop, OnOK: StateGates, OnFail: StateAwaitingOperator},
	// A failing gate chain returns to the dev loop rather than parking: the
	// findings ARE the next instruction, and the loop is how they get acted
	// on. Parking would need an operator to relay what the tools already said.
	{From: StateGates, Stage: StageGates, OnOK: StatePOValidation, OnFail: StateDevLoop},
	// A PO rejection returns to the dev loop for the same reason a failing
	// gate does: the findings are the next instruction. AC-po-verdict says so
	// explicitly — fail returns the findings to the dev agent and the pair
	// loop resumes.
	{From: StatePOValidation, Stage: StageValidate, OnOK: StateReviewSubmitted, OnFail: StateDevLoop},
}

// Lookup returns the transition for a state.
func Lookup(from State) (Transition, bool) {
	for _, t := range Table {
		if t.From == from {
			return t, true
		}
	}
	return Transition{}, false
}

// Terminal reports whether a state has no outgoing transition.
func Terminal(s State) bool {
	_, ok := Lookup(s)
	return !ok
}

// Store persists build runs.
type Store interface {
	Upsert(ctx context.Context, r BuildRun) error
	Find(ctx context.Context, id string) (BuildRun, bool, error)
}

// Stages maps a stage to the module that performs it.
//
// Functions supplied by the composition root, for the same reason recovery
// takes functions: an interface would force every module to import this
// package while this package must reach them.
type Stages map[Stage]func(ctx context.Context, run BuildRun) (BuildRun, error)

// Orchestrator advances runs.
type Orchestrator struct {
	Store  Store
	Stages Stages
	Now    clock.Clock
}

// Advance moves a run one step.
//
// Load, look up, delegate, write — in that order, and nothing else. The only
// judgement is the table's.
func (o Orchestrator) Advance(ctx context.Context, id string) (BuildRun, error) {
	run, found, err := o.Store.Find(ctx, id)
	if err != nil {
		return BuildRun{}, fmt.Errorf("find run: %w", err)
	}
	if !found {
		return BuildRun{}, fmt.Errorf("no build run %s", id)
	}

	transition, ok := Lookup(run.State)
	if !ok {
		// Terminal is not an error: a caller driving to completion needs to
		// see it stop rather than be told it broke.
		return run, nil
	}
	stage, ok := o.Stages[transition.Stage]
	if !ok {
		return BuildRun{}, fmt.Errorf("no implementation for stage %q", transition.Stage)
	}

	next, stageErr := stage(ctx, run)
	if stageErr != nil {
		// A stage ERROR is a malfunction, distinct from a stage reporting
		// failure: the run parks with the durable cause so the inbox can say
		// why, rather than looping on something no dev agent can fix.
		run.State = transition.OnFail
		run.Error = stageErr.Error()
		if err := o.Store.Upsert(ctx, run); err != nil {
			return BuildRun{}, err
		}
		return run, nil
	}
	next.State = transition.OnOK
	next.Error = ""
	if err := o.Store.Upsert(ctx, next); err != nil {
		return BuildRun{}, err
	}
	return next, nil
}

// Drive advances a run until it reaches a terminal state or stops making
// progress.
//
// The bound is not a timeout in disguise: a table that fails to advance is a
// defect, and looping forever would hide it behind a hang.
func (o Orchestrator) Drive(ctx context.Context, id string, maxSteps int) (BuildRun, error) {
	var run BuildRun
	for range maxSteps {
		// Zero on the first pass, which no real state equals, so the first
		// Advance is never mistaken for a stall.
		before := run.State
		var err error
		if run, err = o.Advance(ctx, id); err != nil {
			return run, err
		}
		if Terminal(run.State) {
			return run, nil
		}
		if run.State == before {
			return run, fmt.Errorf("run %s stopped advancing in state %q", id, run.State)
		}
	}
	return run, fmt.Errorf("run %s did not settle within %d steps", id, maxSteps)
}
