package planner

import (
	"context"
	"fmt"
	"strings"

	"kriya/internal/agent"
	"kriya/internal/clock"

	"kriya/internal/specverify"
)

// Refusal explains why a spec cannot be built. It is a value, not an error:
// refusing a spec is the ordinary outcome of pointing kriya at one that is
// not ready, and the operator needs the findings that justify it.
type Refusal struct {
	Reason   string
	Status   string
	Findings []specverify.Finding
}

// Error lets a Refusal travel as an error where that reads better.
func (r Refusal) Error() string { return r.Reason }

// Intaker verifies a target spec, decides whether it may be built, and pins
// what it validated.
type Intaker struct {
	Verify    specverify.Verifier
	Snapshots SnapshotStore
	Targets   TargetStore
	Attempts  AttemptStore
	Tracker   Tracker
	Agent     agent.Agent
	// Tickets records what a decomposition produced. Nil records nothing,
	// which is what a test of decomposition's own output wants.
	Tickets TicketStore
	// Plans records the decomposition's own lifecycle. Nil records nothing —
	// a module-level test of decomposition does not need one — but production
	// wires it, because the completed stamp is what arms build-completion
	// detection.
	Plans PlanStore
	// Heads is the durable head row and its replacement CAS. Nil skips the
	// CAS, which is what a module-level test of decomposition's own phases
	// wants; production wires it, because without it two decompositions can
	// both believe they are the head.
	Heads HeadStore
	// Steps is the plan's durable mutation sequence, persisted before any
	// tracker call. Nil skips it, which is what a module-level test of the
	// phases themselves wants; production wires it, because without it a
	// recovery has nothing to replay and must ask the PM again.
	Steps StepStore
	Now   clock.Clock
}

// TicketStore persists the tickets a decomposition produced.
type TicketStore interface {
	Put(ctx context.Context, targetKey string, t Ticket) error
	Find(ctx context.Context, targetKey, issue string) (Ticket, bool, error)
	// ForPlan lists ONE decomposition's tickets. A same-key retry that must
	// not re-decompose answers from here rather than by asking the agent
	// again — and by plan rather than by target, because a target
	// accumulates plans as decompositions supersede each other.
	ForPlan(ctx context.Context, decompositionKey string) ([]Ticket, error)
}

// admitReport applies the three report-level checks.
func admitReport(dir string, report specverify.Report) error {
	if !report.OK {
		return &Refusal{
			Reason:   fmt.Sprintf("spec at %s does not verify", dir),
			Status:   report.Status,
			Findings: report.Findings,
		}
	}
	if report.Status != "ready" {
		return &Refusal{
			Reason:   fmt.Sprintf("spec at %s is %s, not ready", dir, report.Status),
			Status:   report.Status,
			Findings: report.Findings,
		}
	}
	if report.Counts.Error > 0 || report.Counts.Todo > 0 {
		return &Refusal{
			Reason: fmt.Sprintf("spec at %s claims ready with %d error and %d todo findings",
				dir, report.Counts.Error, report.Counts.Todo),
			Status:   report.Status,
			Findings: report.Findings,
		}
	}
	return nil
}

// AdmitAndPin verifies a spec, decides whether kriya may build it, and pins
// what it validated.
//
// "Ready means buildable without further prompting" (REQ-spec-intake), and
// kriya holds the spec to its own claim. There is deliberately no
// admit-without-pinning entry point: it existed briefly, was reachable only
// from tests, and tempted exactly the bug this function now avoids — checking
// one read of the spec and pinning another.
//
// Nothing is pinned unless every check passed: AC-intake-refuse requires a
// refused intake to pin no snapshot and enqueue nothing.
func (i Intaker) AdmitAndPin(ctx context.Context, dir, token string) (Intake, error) {
	report, err := i.Verify.Verify(ctx, dir)
	if err != nil {
		return Intake{}, fmt.Errorf("verify %s: %w", dir, err)
	}
	if err := admitReport(dir, report); err != nil {
		return Intake{}, err
	}
	// ONE resolve, and the model that is validated is the model that is
	// pinned. An earlier version called Admit and then resolved again, so a
	// working-tree edit between the two calls meant intake validated one model
	// and pinned another — including, in principle, an ok:false empty one.
	model, err := i.admitCommands(ctx, dir, report.Status)
	if err != nil {
		return Intake{}, err
	}

	// The attempt is reserved BEFORE the snapshot is pinned, so a crash in
	// between is replayable: the retry under the same token finds the recorded
	// attempt and reuses its generation rather than allocating a second and
	// superseding itself.
	attempt, err := i.Attempts.Reserve(ctx, token, dir)
	if err != nil {
		return Intake{}, fmt.Errorf("reserve intake generation: %w", err)
	}

	snap, err := i.pin(ctx, dir, model)
	if err != nil {
		return Intake{}, err
	}
	if err := i.Attempts.Complete(ctx, token, snap.Hash); err != nil {
		return Intake{}, err
	}
	// Fenced on generation: a delayed seed carrying an older one loses.
	if err := i.Attempts.MapSpec(ctx, SpecMapping{
		TargetKey: dir, Generation: attempt.Generation, SnapshotHash: snap.Hash,
	}); err != nil {
		return Intake{}, err
	}
	return Intake{Snapshot: snap, Generation: attempt.Generation}, nil
}

// Intake is what one admitted request pinned.
//
// The generation travels WITH the snapshot rather than being looked up from
// the target's mapping later. The mapping keeps the HIGHER generation, so a
// stale intake recovering after a newer one would read the newer generation
// back and derive a decomposition key that is not its own — and a stale
// intake presenting itself as current is exactly what the replacement CAS
// exists to reject.
type Intake struct {
	Snapshot   Snapshot
	Generation int
}

// admitCommands enforces AC-intake-commands.
//
// A module whose effective stack is missing any of test, lint, typecheck,
// arch, coverage, or mutation is refused, naming the module and the missing
// command — "even when avspec verify alone reports ready". avspec does not
// check this: a spec with no commands at all verifies ready, because the
// format does not require them. The gate chain runs per module with these
// resolved commands, so a missing one is a gate that would silently not run.
func (i Intaker) admitCommands(ctx context.Context, dir, status string) (specverify.Model, error) {
	model, err := i.Verify.Inspect(ctx, dir)
	if err != nil {
		return specverify.Model{}, fmt.Errorf("inspect %s: %w", dir, err)
	}
	if len(model.Modules) == 0 {
		return model, &Refusal{
			Reason: fmt.Sprintf("spec at %s declares no modules, so no gate chain can run", dir),
			Status: status,
		}
	}
	for _, m := range model.Modules {
		if missing := m.Missing(); len(missing) > 0 {
			return model, &Refusal{
				Reason: fmt.Sprintf("module %s (%s) is missing %s",
					m.ID, m.Name, strings.Join(missing, ", ")),
				Status: status,
			}
		}
	}
	return model, nil
}
