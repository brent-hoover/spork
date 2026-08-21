package planner

import (
	"context"
	"fmt"
	"strings"

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

// Intaker verifies a target spec and decides whether it may be built.
type Intaker struct {
	Verify specverify.Verifier
}

// Admit reports nil when dir holds a spec kriya may build, or a *Refusal.
//
// "Ready means buildable without further prompting" (REQ-spec-intake), and
// kriya holds the spec to its own claim. Three separate conditions, because
// no single signal covers them:
//
//   - avspec exits 0 for a DRAFT spec and reports OK=true, since todos do not
//     block at draft. Trusting the exit code or OK admits drafts, which
//     AC-intake-refuse forbids.
//   - status must be exactly ready; any other status is refused by name.
//   - error AND todo counts must both be zero. Todos block at ready, and a
//     spec can claim ready while carrying them.
func (i Intaker) Admit(ctx context.Context, dir string) error {
	report, err := i.Verify.Verify(ctx, dir)
	if err != nil {
		return fmt.Errorf("verify %s: %w", dir, err)
	}
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
	return i.admitCommands(ctx, dir, report.Status)
}

// admitCommands enforces AC-intake-commands.
//
// A module whose effective stack is missing any of test, lint, typecheck,
// arch, coverage, or mutation is refused, naming the module and the missing
// command — "even when avspec verify alone reports ready". avspec does not
// check this: a spec with no commands at all verifies ready, because the
// format does not require them. The gate chain runs per module with these
// resolved commands, so a missing one is a gate that would silently not run.
func (i Intaker) admitCommands(ctx context.Context, dir, status string) error {
	model, err := i.Verify.Resolve(ctx, dir)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", dir, err)
	}
	if len(model.Modules) == 0 {
		return &Refusal{
			Reason: fmt.Sprintf("spec at %s declares no modules, so no gate chain can run", dir),
			Status: status,
		}
	}
	for _, m := range model.Modules {
		if missing := m.Missing(); len(missing) > 0 {
			return &Refusal{
				Reason: fmt.Sprintf("module %s (%s) is missing %s",
					m.ID, m.Name, strings.Join(missing, ", ")),
				Status: status,
			}
		}
	}
	return nil
}
