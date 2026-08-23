//go:build acceptance

package acceptance

import (
	"fmt"
	"strings"

	"github.com/cucumber/godog"
)

// projectCommands is what a project stack declares. The values are markers
// rather than real commands: these scenarios are about RESOLUTION, and a real
// command would make a failure ambiguous between "resolved wrong" and "the
// command itself broke".
const projectCommands = `      test: project-test
      lint: project-lint
      typecheck: project-typecheck
      arch: project-arch
      coverage: project-coverage
      mutation: project-mutation`

// readySpec builds a manifest complete enough to verify ready with no
// findings, with the given module stack block spliced in.
func readySpec(moduleStack string) string {
	return `avspec: "0.3"
project:
  name: overrides
  status: ready
  description: A fixture for command resolution.
  stack:
    languages: [{ name: go, version: "1.25" }]
    commands:
` + projectCommands + `
constitution:
  - id: CON-a
    statement: Dependencies flow one way.
requirements:
  - id: REQ-a
    title: Do the thing
    rationale: Because the fixture needs one.
    acceptance:
      - id: AC-a
        statement: The thing is done.
        test: verification/REQ-a.feature#the thing is done
data:
  entities:
    - id: ENT-a
      name: Thing
      fields:
        - name: id
          type: uuid
          required: true
modules:
  - id: MOD-web
    name: web
    responsibility: The surface.
    boundaries: { may_import: [] }
    owns: [ENT-a]
` + moduleStack + `
apps:
  - id: APP-a
    name: app
    modules: [MOD-web]
`
}

// registerModuleCommands wires the resolution scenarios.
func registerModuleCommands(sc *godog.ScenarioContext, w *world) {
	sc.Given(`^a project whose avspec verifies status "ready" with zero errors$`,
		func() error { return w.writeReadySpec("") })
	sc.Given(`^module "([^"]*)" resolves a blank effective "([^"]*)" command$`,
		func(module, command string) error { return w.dropCommand(module, command) })
	sc.Given(`^a ready spec whose module "([^"]*)" overrides every stack command$`,
		func(string) error {
			return w.writeReadySpec(`    stack:
      commands:
        test: web-test
        lint: web-lint
        typecheck: web-typecheck
        arch: web-arch
        coverage: web-coverage
        mutation: web-mutation`)
		})
	sc.Given(`^a ready spec whose module "([^"]*)" overrides only the "([^"]*)" command$`,
		func(_, command string) error {
			return w.writeReadySpec("    stack:\n      commands:\n        " + command + ": web-" + command)
		})

	sc.When(`^intake pins the SpecSnapshot$`, func() error {
		if err := w.run(); err != nil {
			return err
		}
		if w.err != nil {
			return fmt.Errorf("intake failed: %w", w.err)
		}
		return nil
	})

	sc.Then(`^intake is refused naming module "([^"]*)" and command "([^"]*)"$`,
		func(module, command string) error { return w.assertRefusalNames(module, command) })
	sc.Then(`^the snapshot records module "([^"]*)" with exactly its own commands$`,
		func(module string) error { return w.assertAllOwn(module) })
	sc.Then(`^no field of module "([^"]*)" falls back to the project stack$`,
		func(module string) error { return w.assertNoneInherited(module) })
	sc.Then(`^the snapshot records module "([^"]*)" with its own "([^"]*)" command$`,
		func(module, command string) error { return w.assertOwn(module, command) })
	sc.Then(`^every other command for module "([^"]*)" is the project stack's$`,
		func(module string) error { return w.assertOthersInherited(module) })
}

func (w *world) writeReadySpec(moduleStack string) error {
	if err := w.writeSpec(readySpec(moduleStack)); err != nil {
		return err
	}
	// The fake PM must cite THIS fixture's criterion. Citations are validated
	// against the snapshot, so a reply carrying another spec's ids fails —
	// correctly, which is why the reply has to track the fixture.
	w.citeCriteria("AC-a")
	return w.writeFeature()
}

// dropCommand removes one command from the project stack so the module's
// effective stack resolves it blank.
func (w *world) dropCommand(_, command string) error {
	spec := strings.Replace(readySpec(""), "      "+command+": project-"+command+"\n", "", 1)
	if err := w.writeSpec(spec); err != nil {
		return err
	}
	w.citeCriteria("AC-a")
	return w.writeFeature()
}

// moduleID accepts either the spec's id or the bare name the scenarios use.
//
// The Gherkin speaks of "api" and "billing" because that is how a person names
// a module; the spec addresses them as MOD-api. Normalising here keeps the
// scenarios readable without teaching the production code two spellings.
func moduleID(name string) string {
	if strings.HasPrefix(name, "MOD-") {
		return name
	}
	return "MOD-" + name
}

func (w *world) resolved(module string) (map[string]string, error) {
	snap, err := w.store.only()
	if err != nil {
		return nil, err
	}
	got, ok := snap.ResolvedCommands[moduleID(module)]
	if !ok {
		return nil, fmt.Errorf("snapshot carries no commands for %s", moduleID(module))
	}
	return got, nil
}

func (w *world) assertRefusalNames(module, command string) error {
	refusal, err := w.refusal()
	if err != nil {
		return err
	}
	for _, want := range []string{module, command} {
		if !strings.Contains(refusal.Reason, want) {
			return fmt.Errorf("refusal %q does not name %q", refusal.Reason, want)
		}
	}
	return nil
}

func (w *world) assertAllOwn(module string) error {
	got, err := w.resolved(module)
	if err != nil {
		return err
	}
	for _, name := range requiredCommands {
		if want := module + "-" + name; got[name] != want {
			return fmt.Errorf("%s resolved %q, want %q", name, got[name], want)
		}
	}
	return nil
}

func (w *world) assertNoneInherited(module string) error {
	got, err := w.resolved(module)
	if err != nil {
		return err
	}
	for name, value := range got {
		if strings.HasPrefix(value, "project-") {
			return fmt.Errorf("%s fell back to the project stack (%q)", name, value)
		}
	}
	return nil
}

func (w *world) assertOwn(module, command string) error {
	got, err := w.resolved(module)
	if err != nil {
		return err
	}
	if want := module + "-" + command; got[command] != want {
		return fmt.Errorf("%s resolved %q, want the module's own %q", command, got[command], want)
	}
	return nil
}

// assertOthersInherited is the half that makes override PER-FIELD rather than
// per-block: a block replacement would leave the others empty, which a
// consumer reads as "not declared".
func (w *world) assertOthersInherited(module string) error {
	got, err := w.resolved(module)
	if err != nil {
		return err
	}
	for _, name := range requiredCommands {
		if strings.HasPrefix(got[name], module+"-") {
			continue // the overridden one
		}
		if want := "project-" + name; got[name] != want {
			return fmt.Errorf("%s resolved %q, want the project stack's %q", name, got[name], want)
		}
	}
	return nil
}
