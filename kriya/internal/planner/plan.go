package planner

import "context"

// Plan lifecycle states.
//
// The spec's lifecycle is pending -> active -> superseded, with two terminal
// sidings: awaiting-operator, which the operator can restore, and historical,
// which nothing revives. COMPLETED is deliberately not among them — it is a
// stamp on an ACTIVE plan, not a state of its own, because activation precedes
// the creation, wiring and assignment phases and a crash can therefore leave a
// plan active with its ticket set still growing. A model that made completed a
// state would have no way to say "active, mid-decomposition".
const (
	// PlanPending is written before any tracker call. A crash here leaves a
	// row saying a plan was being built, which is what makes recovery
	// possible at all: no row is indistinguishable from a target nobody
	// has planned.
	PlanPending = "pending"
	// PlanActive is the head's working plan. Bearing the completed stamp it
	// is whole; without it, decomposition is still running its phases.
	PlanActive = "active"
	// PlanSuperseded is a plan a newer decomposition replaced. A same-key
	// retry returns its historical result and can never revive it.
	PlanSuperseded = "superseded"
	// PlanAwaitingOperator is a plan parked on an unrecoverable failure or a
	// lost replacement CAS. It is retryable BY THE OPERATOR and never
	// silently resumed.
	PlanAwaitingOperator = "awaiting-operator"
	// PlanHistorical is terminal. A candidate whose intake generation the
	// head has outrun lands here directly rather than parking: parking it
	// would offer the operator a retry that can never win.
	PlanHistorical = "historical"
)

// Plan is one decomposition's ticket set for a target.
//
// Written WRITE-AHEAD, before the first ticket exists, so a crash
// mid-decomposition leaves a row saying a plan was being built rather than
// nothing at all.
type Plan struct {
	// Key is the decomposition key, derived in DecompositionKey. It is the
	// plan's identity: every request resolves against it BEFORE any PlanHead
	// logic runs, so a retry finds its own plan rather than competing with it.
	Key       string
	TargetKey string
	// SpecHash is the snapshot this plan decomposed. A later snapshot is a
	// different plan, and completion is judged against the current one.
	SpecHash string
	// Generation is the intake generation the snapshot was pinned under. It
	// decides eligibility: a candidate must be STRICTLY NEWER than the head.
	Generation int
	State      string
	// Completed marks decomposition's final assignment barrier as passed.
	// Only an ACTIVE plan bearing it has a whole ticket set, and only then
	// may completion detection arm — a mid-phase plan would otherwise present
	// a partial set as done.
	Completed bool
	// Tickets is how many the decomposition produced. Recorded with the stamp
	// so a detector can tell a whole plan from one that lost rows.
	Tickets int
	// Predecessor is the plan this one superseded. Pointed at the head it
	// beat in the same transaction that moved the head, so retirement can
	// find its work after a crash — a successor that had to search for its
	// predecessor could not tell one it beat from one it never raced.
	Predecessor string
	// SupersededBy names the plan that replaced this one. It is how a
	// same-key retry on a superseded plan learns where the head went.
	SupersededBy string
	// Error is why a plan parked. The operator's inbox shows it, and a park
	// with no cause is one nobody can act on.
	Error string
}

// PlanStore persists plans.
type PlanStore interface {
	Upsert(ctx context.Context, p Plan) error
	Find(ctx context.Context, targetKey string) (Plan, bool, error)
	// ByKey resolves a decomposition key. Every request does this FIRST,
	// before any PlanHead logic: a row already present makes the request a
	// same-key retry, whose result is state-aware and mutates nothing.
	ByKey(ctx context.Context, key string) (Plan, bool, error)
	// Claim inserts a plan only if its key is unclaimed, reporting whether
	// THIS call created it. Resolve is a read, so two concurrent first
	// requests can both see no row; the claim is what makes exactly one of
	// them the decomposition and the other a retry of it.
	Claim(ctx context.Context, p Plan) (created bool, err error)
}
