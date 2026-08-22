// Package recovery sequences each owning module's Recover across the nine
// reconciliation stages, at startup, before any pop.
//
// NOT a declared avspec module. A legal route exists through devloop, which
// may import reviewbridge — but it would put cross-module startup sequencing
// inside a domain module, and inside the one module
// CON-deterministic-orchestrator most constrains. Sequencing is a
// composition-root concern. See feature-work/kriya-build/spec-gaps.md.
//
// Imported only by cmd/kriya. No module imports it.
package recovery

import (
	"context"
	"fmt"
	"sort"
)

// Stage is one reconciliation step. The order is declared, not discovered.
type Stage int

// The nine stages, in the order they must run.
//
// Pop binding is FIRST, before plan retirement. The spec is explicit — "the
// reconciliation pass below runs first so no claimed-but-unbound pop exists
// when those stamps land" — and REQ-parallel-build asserts the BuildRun is
// bound by pop replay BEFORE any ownership classification. An earlier draft
// had retirement first and claimed no stage depends on a later one; both were
// wrong in exactly the direction that scenario names.
const (
	StagePopBinding Stage = iota + 1
	StagePlanLifecycle
	StageTargets
	StageAttribution
	StageWorkspaces
	StageDevSessions
	StageBuildRuns
	StageMerges
	StageReviewRounds
)

// String names a stage for logs and errors.
func (s Stage) String() string {
	names := map[Stage]string{
		StagePopBinding: "pop-binding", StagePlanLifecycle: "plan-lifecycle",
		StageTargets: "targets", StageAttribution: "attribution",
		StageWorkspaces: "workspaces", StageDevSessions: "dev-sessions",
		StageBuildRuns: "build-runs", StageMerges: "merges",
		StageReviewRounds: "review-rounds",
	}
	if n, ok := names[s]; ok {
		return n
	}
	return fmt.Sprintf("stage-%d", int(s))
}

// Step is one module's reconciliation for one stage.
//
// A named function rather than an interface method taking a Stage: an
// interface would force every module to import this package for the Stage
// type, while this package must reach the modules — a two-way edge for no
// gain. Modules expose ordinary methods; the composition root maps them to
// stages, which also puts the whole ordering in one readable place.
type Step struct {
	Stage Stage
	// Owner names the module, for errors.
	Owner string
	Run   func(context.Context) error
}

// Run reconciles every step in stage order, before any pop.
//
// A stage may depend on an earlier stage's reconciliation; none depends on a
// later one, with one documented exception — plan retirement re-invokes pop
// binding when it discovers a claimed-but-unbound pop mid-pass, which is safe
// because binding is idempotent.
//
// Steps are sorted here rather than trusted to arrive in order: a caller that
// listed them out of sequence would otherwise reconcile later machines first,
// silently, and the scenarios that pin the ordering would still pass.
func Run(ctx context.Context, steps []Step) error {
	ordered := append([]Step{}, steps...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Stage < ordered[j].Stage })
	for _, step := range ordered {
		if err := step.Run(ctx); err != nil {
			return fmt.Errorf("recovery stage %s (%s): %w", step.Stage, step.Owner, err)
		}
	}
	return nil
}
