//go:build acceptance

package acceptance

import (
	"context"
	"fmt"
	"strings"

	"github.com/cucumber/godog"
)

// draftManifest verifies status draft: complete enough to load, incomplete
// enough that todos remain — which is what "draft" means.
const draftManifest = `avspec: "0.3"
project:
  name: drafty
  status: draft
`

// erroringManifest claims ready and carries an ERROR finding: a requirement
// referencing an acceptance criterion id that is duplicated.
const erroringManifest = `avspec: "0.3"
project:
  name: erroring
  status: ready
requirements:
  - id: REQ-a
    title: a
    acceptance:
      - id: AC-dup
        statement: one
      - id: AC-dup
        statement: two
`

// todoManifest claims ready but is missing the sections that produce todos.
const todoManifest = `avspec: "0.3"
project:
  name: todoish
  status: ready
`

// registerSpecIntake wires REQ-spec-intake.
func registerSpecIntake(sc *godog.ScenarioContext, w *world) {
	sc.Given(`^a project "([^"]*)" whose avspec verifies status "ready" with zero errors and zero todo findings$`,
		func(string) error { return w.copyLinkshort() })
	sc.Given(`^a project whose avspec verifies status "draft"$`,
		func() error { return w.writeSpec(draftManifest) })
	sc.Given(`^a project whose avspec claims status "ready" but verify reports an error finding$`,
		func() error { return w.writeSpec(erroringManifest) })
	sc.Given(`^a project whose avspec claims status "ready" but verify reports a todo finding$`,
		func() error { return w.writeSpec(todoManifest) })
	sc.Given(`^every module's effective stack resolves non-empty test, lint, typecheck, arch, coverage, and mutation commands$`,
		func() error { return w.assertEveryModuleDeclaresEveryCommand() })

	sc.When(`^the operator points kriya at the project$`,
		func() error { return w.run() })

	sc.Then(`^intake succeeds and a SpecSnapshot is pinned$`, func() error {
		if w.err != nil {
			return fmt.Errorf("intake failed: %w", w.err)
		}
		if n := w.snapshotCount(); n != 1 {
			return fmt.Errorf("expected one pinned snapshot, got %d", n)
		}
		return nil
	})
	sc.Then(`^the snapshot holds the full artifact set and every module's resolved commands$`,
		func() error { return w.assertSnapshotIsComplete() })
	sc.Then(`^intake is refused and the response carries the verify findings$`,
		func() error { return w.assertRefusedWithFindings("") })
	sc.Then(`^intake is refused and the response carries the error finding$`,
		func() error { return w.assertRefusedWithFindings("error") })
	sc.Then(`^intake is refused and the response carries the todo finding$`,
		func() error { return w.assertRefusedWithFindings("todo") })
	sc.Then(`^no snapshot is pinned and nothing is enqueued$`, func() error {
		if n := w.snapshotCount(); n != 0 {
			return fmt.Errorf("a refused intake pinned %d snapshots", n)
		}
		if w.tracker.enqueued() {
			return fmt.Errorf("a refused intake reached the tracker: %d projects, %d issues",
				w.tracker.projects, w.tracker.issues)
		}
		return nil
	})
}

// assertEveryModuleDeclaresEveryCommand checks the premise rather than
// assuming it: a Given that asserts nothing lets the scenario pass on a
// fixture that stopped satisfying it.
func (w *world) assertEveryModuleDeclaresEveryCommand() error {
	v, err := realVerifier()
	if err != nil {
		return err
	}
	model, err := v.Inspect(context.Background(), w.dir)
	if err != nil {
		return fmt.Errorf("inspect: %w", err)
	}
	if len(model.Modules) == 0 {
		return fmt.Errorf("the fixture declares no modules")
	}
	for _, m := range model.Modules {
		if missing := m.Missing(); len(missing) > 0 {
			return fmt.Errorf("module %s is missing %v", m.ID, missing)
		}
	}
	return nil
}

func (w *world) assertSnapshotIsComplete() error {
	snap, err := w.store.only()
	if err != nil {
		return err
	}
	v, err := realVerifier()
	if err != nil {
		return err
	}
	model, err := v.Inspect(context.Background(), w.dir)
	if err != nil {
		return fmt.Errorf("inspect: %w", err)
	}
	// The FULL artifact set: every file the spec references, not just the
	// manifest. A snapshot missing one is silently incomplete, and a build
	// reading from it diverges from the spec that was validated.
	for _, want := range model.Artifacts {
		if _, ok := snap.Content[want]; !ok {
			return fmt.Errorf("snapshot is missing artifact %q", want)
		}
	}
	for _, m := range model.Modules {
		got, ok := snap.ResolvedCommands[m.ID]
		if !ok {
			return fmt.Errorf("snapshot carries no commands for %s", m.ID)
		}
		for _, name := range requiredCommands {
			if got[name] == "" {
				return fmt.Errorf("snapshot's %s is missing the %s command", m.ID, name)
			}
		}
	}
	return nil
}

// assertRefusedWithFindings checks the refusal carried its evidence.
//
// AC-intake-refuse says refused WITH the verify findings: a bare status line
// leaves the operator nothing to act on, so the findings must reach the
// output, not merely exist on the error.
func (w *world) assertRefusedWithFindings(severity string) error {
	refusal, err := w.refusal()
	if err != nil {
		return err
	}
	if len(refusal.Findings) == 0 {
		return fmt.Errorf("the refusal carries no findings: %s", refusal.Reason)
	}
	if severity != "" {
		found := false
		for _, f := range refusal.Findings {
			if f.Severity == severity {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("no %s finding among %d", severity, len(refusal.Findings))
		}
	}
	if !strings.Contains(w.output(), "refused") {
		return fmt.Errorf("the refusal did not reach the operator; output was:\n%s", w.output())
	}
	for _, f := range refusal.Findings {
		if !strings.Contains(w.output(), f.Code) {
			return fmt.Errorf("finding %s never reached the output", f.Code)
		}
	}
	return nil
}
