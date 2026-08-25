package planner

import "context"

// PlanHead is the one durable head row per build target.
//
// Exactly one, and replacing it is a SINGLE store transaction — one durable
// store makes that free, and it is what closes the window in which the head
// could point at a plan that lost. There is no moment where a head-move has
// happened and the supersession has not.
type PlanHead struct {
	TargetKey string
	// Current is the decomposition key of the head plan.
	Current string
	// Generation is the HEAD's generation, bumped on every replacement. It is
	// not the plan's intake generation: two different counters, and confusing
	// them would compare a head's age against a snapshot's.
	Generation int
	// Fence refuses pops while a head plan is unactivated. A COUNT rather
	// than a flag, because it is raised by the head-moving transaction and
	// lowered by a separate activation, and a flag could not survive two
	// supersessions overlapping.
	Fence int
}

// Replacement is the verdict of one candidate entering the CAS.
//
// The landing state of a LOSER is decided in the same atomic verdict as the
// loss itself. Deciding it afterwards would mean reading the head again, and
// a head that moved between the two reads would park a candidate that is by
// then ineligible — offering the operator a retry that can never win.
type Replacement struct {
	Won  bool
	Head PlanHead
	// Landing is the state a losing candidate durably moves to:
	// PlanAwaitingOperator while it is still generation-eligible, and
	// PlanHistorical once the head has outrun it.
	Landing string
}

// HeadStore holds the head row and performs the replacement CAS.
type HeadStore interface {
	// Replace runs the whole atomic verdict: it requires the current head's
	// plan to be ACTIVE and the candidate's intake generation to be STRICTLY
	// NEWER than the head's plan's, then marks the predecessor superseded,
	// installs the candidate as head, bumps the head generation and raises
	// the pop fence. A target with no head yet takes the same path, where
	// the row's insert IS the CAS.
	Replace(ctx context.Context, candidate Plan) (Replacement, error)
	// Head reads the current head row.
	Head(ctx context.Context, targetKey string) (PlanHead, bool, error)
	// Activate lowers the pop fence once retirement has finished. A SEPARATE
	// transaction from the head move, deliberately: lowering it in the
	// head-moving transaction would admit pops before retirement ran.
	Activate(ctx context.Context, key string) error
}

// Eligible reports whether a candidate may still replace a head plan.
//
// STRICTLY newer, so a generation-1 intake recovering after generation 2
// installed its plan loses — and equal generations lose too, because a
// candidate that is merely as new as the head is a twin of it, and a twin
// resolves through its own decomposition key rather than competing.
func Eligible(candidate, head int) bool { return candidate > head }

// LandingFor decides where a losing candidate durably goes.
//
// A loser that is still generation-eligible parks awaiting-operator: it could
// win a retry, so an operator has something to act on. One the head has
// already outrun goes DIRECTLY to terminal historical — parking it would put
// a retry in the operator's inbox that the CAS can only ever reject again.
func LandingFor(candidate, head int) string {
	if Eligible(candidate, head) {
		return PlanAwaitingOperator
	}
	return PlanHistorical
}
