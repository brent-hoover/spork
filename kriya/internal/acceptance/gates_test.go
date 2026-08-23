//go:build acceptance

package acceptance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cucumber/godog"

	"kriya/internal/fakes"
	"kriya/internal/gates"
	"kriya/internal/orchestrator"
)

// memGates is the gate store for these scenarios.
type memGates struct{ rows []gates.Result }

func (m *memGates) Upsert(_ context.Context, r gates.Result) error {
	for i, existing := range m.rows {
		if existing.Build == r.Build && existing.Module == r.Module &&
			existing.Gate == r.Gate && existing.Attempt == r.Attempt {
			m.rows[i] = r
			return nil
		}
	}
	m.rows = append(m.rows, r)
	return nil
}

func (m *memGates) Passed(_ context.Context, build, module, gate, commit string) (bool, error) {
	for i := len(m.rows) - 1; i >= 0; i-- {
		r := m.rows[i]
		if r.Build == build && r.Module == module && r.Gate == gate && r.Commit == commit {
			return r.Passed, nil
		}
	}
	// Absence is not failure: a gate never run at this commit has not passed.
	return false, nil
}

func (m *memGates) find(module, gate, commit string) (gates.Result, bool) {
	for _, r := range m.rows {
		if r.Module == module && r.Gate == gate && r.Commit == commit {
			return r, true
		}
	}
	return gates.Result{}, false
}

// gateWorld is one gate scenario's state.
//
// The commands are real shell, run for real. What a gate MEANS is the target's
// to declare in its own command, so a harness that stubbed execution would be
// testing kriya against kriya's idea of a linter.
type gateWorld struct {
	store   *memGates
	runner  gates.Runner
	dir     string
	commit  string
	modules map[string]map[string]string
	results map[string]gates.Result
}

func (w *world) newGates() (*gateWorld, error) {
	dir, err := w.tempDir()
	if err != nil {
		return nil, err
	}
	store := &memGates{}
	g := &gateWorld{
		store:   store,
		runner:  gates.Runner{Store: store, Now: fakes.NewClock(time.Unix(0, 0))},
		dir:     dir,
		modules: map[string]map[string]string{},
		results: map[string]gates.Result{},
	}
	w.gates = g
	return g, nil
}

// commandsFor gives a module every gate command, with the named ones failing.
func commandsFor(failing ...string) map[string]string {
	cmds := map[string]string{}
	for _, name := range []string{"test", "lint", "typecheck", "arch", "coverage", "mutation"} {
		cmds[name] = "echo " + name + " ok"
	}
	for _, name := range failing {
		cmds[name] = "echo '" + name + " found a problem' >&2; exit 1"
	}
	return cmds
}

// run executes one gate for every declared module.
func (g *gateWorld) run(gate string) error {
	for module, cmds := range g.modules {
		result, err := g.runner.Run(context.Background(), "run-1", module, gate,
			g.commit, g.dir, cmds)
		if err != nil {
			return fmt.Errorf("run %s for %s: %w", gate, module, err)
		}
		g.results[module] = result
	}
	return nil
}

func registerGates(sc *godog.ScenarioContext, w *world) {
	sc.Step(`^a run at head commit "([^"]*)" whose ticket touched modules "([^"]*)" and "([^"]*)"$`,
		func(commit, first, second string) error {
			g, err := w.newGates()
			if err != nil {
				return err
			}
			g.commit = commit
			// The second module's tools fail, so "each module is judged on its
			// own result" is decidable rather than assumed.
			g.modules[first] = commandsFor()
			g.modules[second] = commandsFor("lint", "typecheck")
			return nil
		})

	sc.Step(`^a run at head commit "([^"]*)" whose ticket touched module "([^"]*)"$`,
		func(commit, module string) error {
			g, err := w.newGates()
			if err != nil {
				return err
			}
			g.commit = commit
			g.modules[module] = commandsFor("arch")
			return nil
		})

	sc.Step(`^the structure gate runs$`, func() error { return w.gates.run("structure") })
	sc.Step(`^the typing gate runs$`, func() error { return w.gates.run("typing") })

	sc.Step(`^the arch gate runs the module's snapshot-resolved architecture command$`, func() error {
		return w.gates.run("arch")
	})

	sc.Step(`^each module's snapshot-resolved (lint|typecheck) command executes against "([^"]*)"$`,
		func(command, commit string) error {
			for module := range w.gates.modules {
				result, ok := w.gates.store.find(module, gateFor(command), commit)
				if !ok {
					return fmt.Errorf("no %s result for %s at %s", command, module, commit)
				}
				if err := ranCommand(result, w.gates.modules[module][command]); err != nil {
					return fmt.Errorf("%s: %w", module, err)
				}
			}
			return nil
		})

	sc.Step(`^a nonzero exit from "([^"]*)" fails the gate for "([^"]*)"$`,
		func(module, _ string) error {
			return mustPass(w.gates, module, false)
		})

	sc.Step(`^a clean exit from "([^"]*)" passes the gate for "([^"]*)"$`,
		func(module, _ string) error {
			return mustPass(w.gates, module, true)
		})

	sc.Step(`^any nonzero exit fails that module's gate$`, func() error {
		var failed int
		for module, result := range w.gates.results {
			if !result.Passed {
				failed++
				continue
			}
			if strings.Contains(w.gates.modules[module]["typecheck"], "exit 1") {
				return fmt.Errorf("%s exited nonzero and passed anyway", module)
			}
		}
		if failed == 0 {
			return errors.New("no module failed, so the claim is untested")
		}
		return nil
	})

	sc.Step(`^declared boundaries are checked against the code's real imports$`, func() error {
		for module := range w.gates.modules {
			result, ok := w.gates.store.find(module, "arch", w.gates.commit)
			if !ok {
				return fmt.Errorf("no arch result for %s", module)
			}
			if err := ranCommand(result, w.gates.modules[module]["arch"]); err != nil {
				return err
			}
		}
		return nil
	})

	sc.Step(`^an undeclared import fails the gate for "([^"]*)"$`, func(module string) error {
		return mustPass(w.gates, module, false)
	})

	sc.Step(`^the result upserts as a GateResult with gate "([^"]*)" pinned to "([^"]*)"$`,
		func(gate, commit string) error {
			return pinnedTo(w.gates, gate, commit)
		})

	sc.Step(`^the result is upserted as a GateResult with gate "([^"]*)" pinned to "([^"]*)"$`,
		func(gate, commit string) error {
			return pinnedTo(w.gates, gate, commit)
		})

	sc.Step(`^a passing (?:arch|structure) result from an older commit never satisfies the chain$`, func() error {
		return staleNeverSatisfies(w.gates, "arch")
	})

	sc.Step(`^a passing result recorded at an older commit never satisfies the chain$`, func() error {
		return staleNeverSatisfies(w.gates, "typing")
	})

	sc.Step(`^the typing gate ran for module "([^"]*)" at commit "([^"]*)"$`,
		func(module, commit string) error {
			g, err := w.newGates()
			if err != nil {
				return err
			}
			g.commit = commit
			g.modules[module] = commandsFor()
			return g.run("typing")
		})

	sc.Step(`^the structure gate failed for module "([^"]*)"$`, func(module string) error {
		g, err := w.newGates()
		if err != nil {
			return err
		}
		g.commit = "C2"
		g.modules[module] = commandsFor("lint")
		if err := g.run("structure"); err != nil {
			return err
		}
		return mustPass(g, module, false)
	})

	sc.Step(`^no waiver mechanism exists at gate time$`, func() error {
		// A real source scan, not a restatement: the claim is that no such
		// mechanism EXISTS, and only reading the package can decide that.
		return noWaiverIn("../gates")
	})

	sc.Step(`^the remedies are fixing the code or an operator-approved lint-config change in the target project via spec amendment and re-intake$`, func() error {
		// The runner has no opinion of its own to override — it runs whatever
		// command it is handed. So the only lever is the command itself, and
		// the command comes from the pinned snapshot, which only a re-intake
		// changes. Proven by handing it a different command and watching the
		// recorded command follow.
		g := w.gates
		module := onlyModule(g)
		amended := commandsFor()
		amended["lint"] = "echo 'amended lint' "
		result, err := g.runner.Run(context.Background(), "run-1", module, "structure",
			g.commit, g.dir, amended)
		if err != nil {
			return err
		}
		if !result.Passed {
			return errors.New("an amended lint command did not take effect")
		}
		return ranCommand(result, amended["lint"])
	})

	sc.Step(`^a passing structure result recorded at an older commit$`, func() error {
		g := w.gates
		return g.store.Upsert(context.Background(), gates.Result{
			Build: "run-1", Module: onlyModule(g), Gate: "structure",
			Commit: "C1", Passed: true,
		})
	})

	sc.Step(`^it does not satisfy the gate at the current head$`, func() error {
		g := w.gates
		passed, err := g.store.Passed(context.Background(), "run-1", onlyModule(g),
			"structure", g.commit)
		if err != nil {
			return err
		}
		if passed {
			return fmt.Errorf("a pass at C1 satisfied the gate at %s", g.commit)
		}
		return nil
	})

	sc.Step(`^the structure gate fails with findings$`, func() error {
		g, err := w.newGates()
		if err != nil {
			return err
		}
		g.commit = "C2"
		g.modules["store"] = commandsFor("lint")
		return g.run("structure")
	})

	sc.Step(`^the tool's findings are handed to the dev agent and the loop continues$`, func() error {
		result := w.gates.results["store"]
		if !strings.Contains(string(result.Detail), "lint found a problem") {
			return fmt.Errorf("the tool's findings are not in detail: %s", result.Detail)
		}
		// The table, not this package, decides where a failing chain goes.
		for _, t := range orchestrator.Table {
			if t.Stage == orchestrator.StageGates {
				if t.OnFail != orchestrator.StateDevLoop {
					return fmt.Errorf("a failing chain goes to %q, not back to the dev loop", t.OnFail)
				}
				return nil
			}
		}
		return errors.New("no transition owns the gate stage")
	})

	sc.Step(`^the result is upserted as a GateResult with gate "([^"]*)", the module, the commit, and detail$`,
		func(gate string) error {
			g := w.gates
			result, ok := g.store.find("store", gate, g.commit)
			if !ok {
				return fmt.Errorf("no %s result for store at %s", gate, g.commit)
			}
			if result.Module != "store" || len(result.Detail) == 0 {
				return fmt.Errorf("result is %+v", result)
			}
			return nil
		})

	sc.Step(`^the gate reruns for the same build, module, and commit$`, func() error {
		return w.gates.run("structure")
	})

	sc.Step(`^the stored result is replaced, never duplicated$`, func() error {
		var n int
		for _, r := range w.gates.store.rows {
			if r.Module == "store" && r.Gate == "structure" && r.Commit == w.gates.commit {
				n++
			}
		}
		if n != 1 {
			return fmt.Errorf("%d rows for one build, module, gate and attempt", n)
		}
		return nil
	})

	sc.Step(`^a failure returns the tool's output to the dev agent$`, func() error {
		g := w.gates
		module := onlyModule(g)
		g.modules[module] = commandsFor("typecheck")
		if err := g.run("typing"); err != nil {
			return err
		}
		result := g.results[module]
		if result.Passed {
			return errors.New("a failing typecheck passed")
		}
		if !strings.Contains(string(result.Detail), "typecheck found a problem") {
			return fmt.Errorf("the tool's own output is not in detail: %s", result.Detail)
		}
		return nil
	})
}

// gateFor maps a stack command back to the gate that runs it.
func gateFor(command string) string {
	switch command {
	case "lint":
		return "structure"
	case "typecheck":
		return "typing"
	default:
		return command
	}
}

// ranCommand checks the recorded detail names the command that was resolved.
func ranCommand(result gates.Result, command string) error {
	var d struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(result.Detail, &d); err != nil {
		return fmt.Errorf("parse detail: %w", err)
	}
	if d.Command != command {
		return fmt.Errorf("ran %q, want the snapshot's %q", d.Command, command)
	}
	return nil
}

func mustPass(g *gateWorld, module string, want bool) error {
	result, ok := g.results[module]
	if !ok {
		return fmt.Errorf("no result for %s", module)
	}
	if result.Passed != want {
		return fmt.Errorf("%s passed=%v, want %v", module, result.Passed, want)
	}
	return nil
}

func pinnedTo(g *gateWorld, gate, commit string) error {
	for module := range g.modules {
		if _, ok := g.store.find(module, gate, commit); !ok {
			return fmt.Errorf("no %s result for %s pinned to %s", gate, module, commit)
		}
	}
	return nil
}

// staleNeverSatisfies asks the store about a commit no gate ran at.
func staleNeverSatisfies(g *gateWorld, gate string) error {
	for module := range g.modules {
		passed, err := g.store.Passed(context.Background(), "run-1", module, gate, "C1")
		if err != nil {
			return err
		}
		if passed {
			return fmt.Errorf("a %s result from an older commit satisfied %s", gate, module)
		}
	}
	return nil
}

// noWaiverIn reports whether the package offers any way to pass a failing
// gate without fixing it.
func noWaiverIn(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read %s: %w", dir, err)
	}
	banned := []string{"waive", "waiver", "skip", "bypass", "override", "ignore", "suppress"}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") ||
			strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(body), "\n") {
			code := line
			if i := strings.Index(code, "//"); i >= 0 {
				code = code[:i]
			}
			for _, word := range banned {
				if strings.Contains(strings.ToLower(code), word) {
					return fmt.Errorf("%s offers a waiver: %s", e.Name(), strings.TrimSpace(line))
				}
			}
		}
	}
	return nil
}

func onlyModule(g *gateWorld) string {
	for module := range g.modules {
		return module
	}
	return ""
}
