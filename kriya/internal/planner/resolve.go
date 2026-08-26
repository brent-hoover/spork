package planner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// DecompositionKey derives a plan's identity.
//
// From the project, the normalized target spec, the pinned snapshot AND the
// intake generation it was pinned under. The generation is what makes
// re-intaking a previously seen spec a FRESH plan rather than a replay: an
// operator reverting from H2 back to H1 gets a new key, so the request
// supersedes the current head through the ordinary path instead of resolving
// to the old H1 plan — which is superseded, and whose retry can mutate
// nothing.
//
// Length-prefixed, so a project key ending where a target begins cannot
// collide with a different split of the same characters.
func DecompositionKey(projectKey, targetSpec, specHash string, generation int) string {
	h := sha256.New()
	for _, part := range []string{projectKey, targetSpec, specHash} {
		_, _ = fmt.Fprintf(h, "%d:%s", len(part), part)
	}
	_, _ = fmt.Fprintf(h, "g:%d", generation)
	return hex.EncodeToString(h.Sum(nil))
}

// Resolution is what a same-key retry gets back.
type Resolution struct {
	Plan Plan
	// Resume is true when the caller must carry the plan's phases forward.
	// Every other outcome is a REPORT: the plan is already whole, already
	// history, or waiting on a human, and the retry touches nothing.
	Resume bool
	// Fresh is true when the key resolved to no row at all, which is the
	// only case that proceeds to insert-and-CAS.
	Fresh bool
}

// Resolve answers a request against the plans that already exist.
//
// This runs BEFORE any PlanHead logic, and that order is the point. A retry
// that reached the head first could compete with the plan it is retrying —
// two requests for one decomposition, one of them superseding the other. The
// key is unique, so resolving it first makes a retry find its own plan and
// makes competition possible only between DIFFERENT keys.
//
// Every outcome but Fresh and Resume mutates nothing at all.
func Resolve(ctx context.Context, plans PlanStore, key string) (Resolution, error) {
	plan, found, err := plans.ByKey(ctx, key)
	if err != nil {
		return Resolution{}, fmt.Errorf("resolve decomposition key %s: %w", Short(key), err)
	}
	if !found {
		return Resolution{Fresh: true}, nil
	}

	switch plan.State {
	case PlanPending:
		// Nothing has been issued yet, or a crash left it mid-issue. Either
		// way the durable per-step progress says where to pick up.
		return Resolution{Plan: plan, Resume: true}, nil
	case PlanActive:
		// Activation precedes the phases, so an active plan without the
		// stamp is one still creating, wiring or assigning. WITH the stamp
		// it is the current, already-complete result.
		return Resolution{Plan: plan, Resume: !plan.Completed}, nil
	case PlanSuperseded, PlanHistorical, PlanAwaitingOperator:
		// Three different reasons, one behaviour: report and touch nothing.
		// A superseded plan tells the caller where the head went; a
		// historical one is terminal and is never revived or re-parked; a
		// parked one belongs to the operator and is never silently resumed
		// past whatever stopped it.
		return Resolution{Plan: plan}, nil
	}
	// A state kriya does not know is a store it cannot reason about. Resuming
	// would issue tracker mutations against a plan of unknown shape.
	return Resolution{}, fmt.Errorf("plan %s is in unknown state %q", Short(key), plan.State)
}

// short truncates an identifier for an error message, safely.
//
// A bare key[:12] panics on anything shorter, which turns a diagnostic into a
// crash — and it fires exactly where things are already going wrong, so the
// message that would have explained the failure is lost with it.
func Short(id string) string {
	const width = 12
	if len(id) <= width {
		return id
	}
	return id[:width]
}
