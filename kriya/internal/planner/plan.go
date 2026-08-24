package planner

import "context"

// Plan lifecycle states.
//
// The stamp is what arms build-completion detection. A plan mid-decomposition
// has a ticket set that is still growing, so "every ticket created so far is
// complete" says nothing about whether the build is done.
const (
	PlanDecomposing = "decomposing"
	PlanCompleted   = "completed"
)

// Plan is one decomposition's ticket set for a target.
//
// Written WRITE-AHEAD, before the first ticket exists, so a crash
// mid-decomposition leaves a row saying a plan was being built rather than
// nothing at all. It reaches completed only when the last ticket has been
// created, recorded, parented and assigned — decomposition's final barrier.
type Plan struct {
	TargetKey string
	// SpecHash is the snapshot this plan decomposed. A later snapshot is a
	// different plan, and completion is judged against the current one.
	SpecHash string
	State    string
	// Tickets is how many the decomposition produced. Recorded with the stamp
	// so a detector can tell a whole plan from one that lost rows.
	Tickets int
}

// PlanStore persists plans.
type PlanStore interface {
	Upsert(ctx context.Context, p Plan) error
	Find(ctx context.Context, targetKey string) (Plan, bool, error)
}
