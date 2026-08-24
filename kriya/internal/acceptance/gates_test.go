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
	"kriya/internal/owner"
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

func (m *memGates) Passed(
	_ context.Context, build, module, gate, commit string, attempt int,
) (bool, error) {
	for i := len(m.rows) - 1; i >= 0; i-- {
		r := m.rows[i]
		if r.Build == build && r.Module == module && r.Gate == gate &&
			r.Commit == commit && r.Attempt == attempt {
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
	// chainResults is what RunChain reported, for the scenarios about which
	// gates ran at all.
	chainResults []gates.Result
	review       *reviewStub
	// attempt, frozen and base carry the attempt-scoping scenarios' state.
	attempt int
	frozen  string
	base    *movingHead
	err     error
}

// chain runs the whole chain for every declared module.
func (g *gateWorld) chain() ([]gates.Result, error) {
	var out []gates.Result
	for module, cmds := range g.modules {
		results, err := g.runner.RunChain(context.Background(), "run-1", module,
			g.commit, g.frozen, g.dir, cmds, 0)
		if err != nil {
			return nil, fmt.Errorf("chain for %s: %w", module, err)
		}
		out = append(out, results...)
	}
	return out, nil
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
			g.commit, g.dir, cmds, 0)
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
			g.commit, g.dir, amended, 0)
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
			"structure", g.commit, 0)
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
		passed, err := g.store.Passed(context.Background(), "run-1", module, gate, "C1", 0)
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

// reviewStub answers the gate runner's review question.
type reviewStub struct {
	passedAt map[string]bool
	asked    []string
}

func (r *reviewStub) PassedAt(_ context.Context, _, commit string) (bool, error) {
	r.asked = append(r.asked, commit)
	return r.passedAt[commit], nil
}

func registerCoverageAndMutationGates(sc *godog.ScenarioContext, w *world) {
	sc.Step(`^the branch-coverage gate runs the module's snapshot-resolved coverage command$`, func() error {
		return w.gates.run("branch-coverage")
	})

	sc.Step(`^the gate fails for "([^"]*)" when that command fails$`, func(module string) error {
		g := w.gates
		g.modules[module] = commandsFor("coverage")
		if err := g.run("branch-coverage"); err != nil {
			return err
		}
		return mustPass(g, module, false)
	})

	sc.Step(`^the gate passes for "([^"]*)" when that command succeeds$`, func(module string) error {
		g := w.gates
		g.modules[module] = commandsFor()
		if err := g.run("branch-coverage"); err != nil {
			return err
		}
		return mustPass(g, module, true)
	})

	sc.Step(`^kriya counts no arms and applies no threshold of its own$`, func() error {
		// A source scan, because the claim is about what kriya does NOT do.
		// Parsing a coverage number anywhere in the gate package would mean
		// kriya deciding what a coverage tool means.
		return noThresholdIn("../gates")
	})

	sc.Step(`^a target declaring a floor is judged against that floor$`, func() error {
		return judgedByItsOwnCommand(w.gates,
			`test "$FLOOR" -le 75 && echo 'at or above the floor'`, true)
	})

	sc.Step(`^a target declaring no floor is judged against every arm$`, func() error {
		return judgedByItsOwnCommand(w.gates,
			`echo 'one arm uncovered' >&2; exit 1`, false)
	})

	sc.Step(`^the module's test command fails at "([^"]*)"$`, func(commit string) error {
		g := w.gates
		g.commit = commit
		for module := range g.modules {
			g.modules[module] = commandsFor("test")
		}
		results, err := g.chain()
		if err != nil {
			return err
		}
		g.chainResults = results
		return nil
	})

	sc.Step(`^the test gate fails as its own commit-pinned GateResult with gate "([^"]*)"$`,
		func(gate string) error {
			g := w.gates
			result, ok := g.store.find(onlyModule(g), gate, g.commit)
			if !ok {
				return fmt.Errorf("no %s result at %s", gate, g.commit)
			}
			if result.Passed {
				return errors.New("a failing test command passed the test gate")
			}
			return nil
		})

	sc.Step(`^the chain fails regardless of what coverage would report$`, func() error {
		g := w.gates
		for _, r := range g.chainResults {
			if r.Gate == "branch-coverage" {
				return errors.New("coverage was judged over a failing suite")
			}
		}
		if _, ok := g.store.find(onlyModule(g), "branch-coverage", g.commit); ok {
			return errors.New("a coverage result was recorded for a failing suite")
		}
		return nil
	})

	sc.Step(`^the test command passes at "([^"]*)"$`, func(commit string) error {
		g := w.gates
		g.commit = commit
		for module := range g.modules {
			g.modules[module] = commandsFor()
		}
		results, err := g.chain()
		if err != nil {
			return err
		}
		g.chainResults = results
		return nil
	})

	sc.Step(`^coverage is judged over the passing suite$`, func() error {
		g := w.gates
		var sawTest bool
		for _, r := range g.chainResults {
			if r.Gate == "test" {
				sawTest = r.Passed
			}
			if r.Gate == "branch-coverage" {
				if !sawTest {
					return errors.New("coverage ran before the test gate passed")
				}
				return nil
			}
		}
		return errors.New("coverage never ran")
	})

	sc.Step(`^the coverage gate ran for module "([^"]*)" at commit "([^"]*)"$`,
		func(module, commit string) error {
			g, err := w.newGates()
			if err != nil {
				return err
			}
			g.commit = commit
			cmds := commandsFor()
			cmds["coverage"] = "echo 'uncovered: api.go:31 arm false'"
			g.modules[module] = cmds
			return g.run("branch-coverage")
		})

	sc.Step(`^the uncovered arms are named in the result's detail for the dev agent$`, func() error {
		result := w.gates.results[onlyModule(w.gates)]
		if !strings.Contains(string(result.Detail), "api.go:31") {
			return fmt.Errorf("the tool's own report is not in detail: %s", result.Detail)
		}
		return nil
	})

	sc.Step(`^a run whose head commit "([^"]*)" has not yet passed the review gate$`,
		func(commit string) error {
			g, err := w.newGates()
			if err != nil {
				return err
			}
			g.commit = commit
			g.modules["store"] = commandsFor()
			g.review = &reviewStub{passedAt: map[string]bool{}}
			g.runner.Review = g.review
			results, err := g.chain()
			if err != nil {
				return err
			}
			g.chainResults = results
			return nil
		})

	sc.Step(`^the mutation gate does not run$`, func() error {
		return mutationDidNotRun(w.gates)
	})

	sc.Step(`^the mutation gate still does not run$`, func() error {
		return mutationDidNotRun(w.gates)
	})

	sc.Step(`^the review gate passes at "([^"]*)" but the branch-coverage gate has not$`,
		func(commit string) error {
			g := w.gates
			g.review.passedAt[commit] = true
			g.modules["store"] = commandsFor("coverage")
			results, err := g.chain()
			if err != nil {
				return err
			}
			g.chainResults = results
			return nil
		})

	sc.Step(`^the test, structure, typing, arch, and branch-coverage gates all pass at "([^"]*)"$`,
		func(commit string) error {
			g := w.gates
			g.review.passedAt[commit] = true
			g.modules["store"] = commandsFor()
			results, err := g.chain()
			if err != nil {
				return err
			}
			g.chainResults = results
			return nil
		})

	sc.Step(`^each touched module's snapshot-resolved mutation command runs against "([^"]*)"$`,
		func(commit string) error {
			g := w.gates
			for module := range g.modules {
				result, ok := g.store.find(module, "mutation", commit)
				if !ok {
					return fmt.Errorf("mutation never ran for %s at %s", module, commit)
				}
				if err := ranCommand(result, g.modules[module]["mutation"]); err != nil {
					return err
				}
			}
			return nil
		})

	sc.Step(`^the mutation command reports surviving mutants for module "([^"]*)"$`,
		func(module string) error {
			g, err := w.newGates()
			if err != nil {
				return err
			}
			g.commit = "C2"
			cmds := commandsFor()
			cmds["mutation"] = "echo 'LIVED CONDITIONALS_NEGATION at store.go:44'; exit 1"
			g.modules[module] = cmds
			return g.run("mutation")
		})

	sc.Step(`^the gate fails for "([^"]*)"$`, func(module string) error {
		return mustPass(w.gates, module, false)
	})

	sc.Step(`^the survivors are returned to the dev agent as named test gaps to kill$`, func() error {
		result := w.gates.results[onlyModule(w.gates)]
		if !strings.Contains(string(result.Detail), "store.go:44") {
			return fmt.Errorf("the survivors are not named in detail: %s", result.Detail)
		}
		return nil
	})

	sc.Step(`^the result upserts as a GateResult with gate "([^"]*)" pinned to the commit$`,
		func(gate string) error {
			return pinnedTo(w.gates, gate, w.gates.commit)
		})

	sc.Step(`^a run with zero survivors passes the gate$`, func() error {
		g := w.gates
		module := onlyModule(g)
		g.modules[module] = commandsFor()
		if err := g.run("mutation"); err != nil {
			return err
		}
		return mustPass(g, module, true)
	})
}

// mutationDidNotRun requires no mutation result at the current commit.
func mutationDidNotRun(g *gateWorld) error {
	for module := range g.modules {
		if _, ok := g.store.find(module, "mutation", g.commit); ok {
			return fmt.Errorf("mutation ran for %s before its preconditions held", module)
		}
	}
	for _, r := range g.chainResults {
		if r.Gate == "mutation" {
			return errors.New("the chain reported a mutation result")
		}
	}
	return nil
}

// judgedByItsOwnCommand replaces the coverage command and checks the verdict
// follows it rather than any opinion of kriya's.
func judgedByItsOwnCommand(g *gateWorld, command string, wantPass bool) error {
	module := onlyModule(g)
	cmds := commandsFor()
	cmds["coverage"] = "FLOOR=75; " + command
	g.modules[module] = cmds
	if err := g.run("branch-coverage"); err != nil {
		return err
	}
	return mustPass(g, module, wantPass)
}

// noThresholdIn reports whether the package decides a coverage verdict itself.
func noThresholdIn(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read %s: %w", dir, err)
	}
	banned := []string{"threshold", "percent", "ParseFloat", "coverageOf", "arms"}
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
				if strings.Contains(strings.ToLower(code), strings.ToLower(word)) {
					return fmt.Errorf("%s decides a coverage verdict: %s",
						e.Name(), strings.TrimSpace(line))
				}
			}
		}
	}
	return nil
}

func registerAttemptScoping(sc *godog.ScenarioContext, w *world) {
	sc.Step(`^integration of a new base left the branch head unchanged$`, func() error {
		g, err := w.newGates()
		if err != nil {
			return err
		}
		g.commit = "C2"
		g.modules["api"] = commandsFor()
		// Attempt 1's full chain, at this commit.
		for _, gate := range gates.Chain {
			if _, err := g.runner.Run(context.Background(), "run-1", "api", gate,
				g.commit, g.dir, g.modules["api"], 1); err != nil {
				return err
			}
		}
		return nil
	})

	sc.Step(`^the new gate-chain attempt starts and increments the run's attempt counter$`,
		func() error {
			// Driven through the orchestrator rather than assigned. The
			// counter lives on the run, and a stage that increments it is
			// worth nothing if Advance writes back the copy it loaded before
			// the stage ran — every later round would reuse attempt 1 and be
			// satisfied by attempt 1's results.
			store := &runStore{rows: map[string]orchestrator.BuildRun{}}
			run := orchestrator.BuildRun{
				ID: "run-1", Ticket: "api", State: orchestrator.StateGates, Attempt: 1,
			}
			if err := store.Upsert(context.Background(), run); err != nil {
				return err
			}
			o := orchestrator.Orchestrator{
				Store: store, Now: fakes.NewClock(time.Unix(0, 0)),
				Stages: orchestrator.Stages{
					"gates": func(
						_ context.Context, r orchestrator.BuildRun,
					) (orchestrator.BuildRun, error) {
						r.Attempt++
						// A FAILING chain, which is the case the increment has
						// to survive: a passing one would advance the run and
						// carry the field along regardless.
						return r, errors.New("gate test failed for api")
					},
				},
			}
			if _, err := o.Advance(context.Background(), "run-1"); err != nil {
				return err
			}
			got, found, err := store.Find(context.Background(), "run-1")
			if err != nil {
				return err
			}
			if !found {
				return errors.New("the run vanished")
			}
			if got.Attempt != 2 {
				return fmt.Errorf("the durable attempt is %d, want 2", got.Attempt)
			}
			w.gates.attempt = got.Attempt
			return nil
		})

	sc.Step(`^every prior round, gate result, and PO verdict carries the old attempt and satisfies nothing$`,
		func() error {
			g := w.gates
			passed, _, err := g.runner.AllPassed(context.Background(), "run-1", "api", g.commit, 1)
			if err != nil {
				return err
			}
			if !passed {
				return errors.New("attempt 1's own results did not satisfy it")
			}
			passed, missing, err := g.runner.AllPassed(context.Background(), "run-1", "api",
				g.commit, g.attempt)
			if err != nil {
				return err
			}
			if passed {
				return errors.New("attempt 1's results satisfied attempt 2 at the same commit")
			}
			if missing != "test" {
				return fmt.Errorf("named %q as the gap", missing)
			}
			// The PO verdict is keyed the same way: a verdict from the
			// previous attempt is not found under the new one.
			validations := &validations{rows: map[int]owner.Validation{}}
			if err := validations.Upsert(context.Background(), owner.Validation{
				Build: "run-1", Attempt: 1, Verdict: owner.VerdictSatisfied, Commit: g.commit,
			}); err != nil {
				return err
			}
			if _, found, err := validations.Find(context.Background(), "run-1", g.attempt); err != nil {
				return err
			} else if found {
				return errors.New("attempt 1's PO verdict satisfied attempt 2")
			}
			return nil
		})

	sc.Step(`^the full chain reruns and records results under the new attempt$`, func() error {
		g := w.gates
		results, err := g.runner.RunChain(context.Background(), "run-1", "api", g.commit,
			g.frozen, g.dir, g.modules["api"], g.attempt)
		if err != nil {
			return err
		}
		if len(results) != len(gates.Chain) {
			return fmt.Errorf("ran %d gates, want the full chain", len(results))
		}
		for _, r := range results {
			if r.Attempt != g.attempt {
				return fmt.Errorf("%s recorded under attempt %d", r.Gate, r.Attempt)
			}
		}
		passed, _, err := g.runner.AllPassed(context.Background(), "run-1", "api",
			g.commit, g.attempt)
		if err != nil {
			return err
		}
		if !passed {
			return errors.New("the rerun's own results did not satisfy it")
		}
		return nil
	})

	sc.Step(`^a gate-chain attempt froze its gated base at "([^"]*)"$`, func(base string) error {
		g, err := w.newGates()
		if err != nil {
			return err
		}
		g.commit = "C2"
		g.frozen = base
		g.modules["api"] = commandsFor()
		g.base = &movingHead{head: base}
		g.runner.Base = g.base
		return nil
	})

	sc.Step(`^the default branch moves to "([^"]*)" while later gates are still running$`,
		func(moved string) error {
			g := w.gates
			// After three gates, which is what makes this a MID-chain move
			// rather than a precondition failure.
			g.base.moved, g.base.after = moved, 3
			_, g.err = g.runner.RunChain(context.Background(), "run-1", "api", g.commit,
				g.frozen, g.dir, g.modules["api"], 1)
			return nil
		})

	sc.Step(`^the attempt aborts rather than recording a mixed-base pass$`, func() error {
		g := w.gates
		if !errors.Is(g.err, gates.ErrBaseMoved) {
			return fmt.Errorf("got %v, want ErrBaseMoved", g.err)
		}
		// Some gates ran, none of the later ones did, and nothing claims the
		// chain passed.
		passed, _, err := g.runner.AllPassed(context.Background(), "run-1", "api", g.commit, 1)
		if err != nil {
			return err
		}
		if passed {
			return errors.New("an aborted chain reported as complete")
		}
		return nil
	})

	sc.Step(`^the run integrates "([^"]*)" and the complete chain reruns against the new frozen base$`,
		func(moved string) error {
			g := w.gates
			// A new frozen base, a new attempt: the whole chain runs again and
			// every result carries the new attempt.
			g.base.head, g.base.after, g.base.reads = moved, 0, 0
			g.frozen = moved
			// The WORK commit does not change: integrating a new base can
			// leave the branch head exactly where it was.
			results, err := g.runner.RunChain(context.Background(), "run-1", "api", g.commit,
				g.frozen, g.dir, g.modules["api"], 2)
			if err != nil {
				return err
			}
			if len(results) != len(gates.Chain) {
				return fmt.Errorf("reran %d gates, want the full chain", len(results))
			}
			passed, _, err := g.runner.AllPassed(context.Background(), "run-1", "api", g.commit, 2)
			if err != nil {
				return err
			}
			if !passed {
				return errors.New("the rerun against the new base did not satisfy it")
			}
			return nil
		})
}

// movingHead reports a different head after n reads.
type movingHead struct {
	head  string
	moved string
	after int
	reads int
}

func (m *movingHead) Head(context.Context) (string, error) {
	m.reads++
	if m.after > 0 && m.reads > m.after {
		return m.moved, nil
	}
	return m.head, nil
}
