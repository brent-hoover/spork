package planner

import (
	"context"
	"fmt"
	"strings"

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
	Now       clock.Clock
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
func (i Intaker) AdmitAndPin(ctx context.Context, dir string) (Snapshot, error) {
	report, err := i.Verify.Verify(ctx, dir)
	if err != nil {
		return Snapshot{}, fmt.Errorf("verify %s: %w", dir, err)
	}
	if err := admitReport(dir, report); err != nil {
		return Snapshot{}, err
	}
	// ONE resolve, and the model that is validated is the model that is
	// pinned. An earlier version called Admit and then resolved again, so a
	// working-tree edit between the two calls meant intake validated one model
	// and pinned another — including, in principle, an ok:false empty one.
	model, err := i.admitCommands(ctx, dir, report.Status)
	if err != nil {
		return Snapshot{}, err
	}
	return i.pin(ctx, dir, model)
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
	model, err := i.Verify.Resolve(ctx, dir)
	if err != nil {
		return specverify.Model{}, fmt.Errorf("resolve %s: %w", dir, err)
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
