//go:build acceptance

package acceptance

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cucumber/godog"

	kctx "kriya/internal/context"
	"kriya/internal/fakes"
)

// memBundles records what an agent was given.
type memBundles struct{ rows map[string]kctx.Bundle }

func (m *memBundles) Put(_ stdctx.Context, b kctx.Bundle) error {
	m.rows[b.Build] = b
	return nil
}

func (m *memBundles) Get(_ stdctx.Context, build string) (kctx.Bundle, bool, error) {
	b, ok := m.rows[build]
	return b, ok, nil
}

// memLearnings matches on either tag, as the real store does.
type memLearnings struct{ rows []kctx.Learning }

func (m *memLearnings) Matching(_ stdctx.Context, modules, patterns []string) ([]kctx.Learning, error) {
	var out []kctx.Learning
	for _, l := range m.rows {
		if inList(modules, l.Module) || inList(patterns, l.Pattern) {
			out = append(out, l)
		}
	}
	return out, nil
}

func inList(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// contextWorld is one context-assembly scenario's state.
type contextWorld struct {
	spec         kctx.Spec
	ticket       kctx.Ticket
	instructions string
	learnings    *memLearnings
	bundles      *memBundles
	content      map[string]any
	raw          string
}

func (w *world) newContext() *contextWorld {
	c := &contextWorld{
		bundles:   &memBundles{rows: map[string]kctx.Bundle{}},
		learnings: &memLearnings{},
		spec: kctx.Spec{
			Constitution: []kctx.Principle{
				{ID: "CON-test-first", Statement: "No behaviour lands before its failing test."},
			},
			Modules: []kctx.Module{
				{
					ID: "MOD-api", Name: "api", MayImport: []string{"MOD-store"},
					Commands: sixCommands("api"),
					Contracts: []kctx.Contract{
						{ID: "CTR-api", Type: "openapi", Path: "contracts/api.yaml"},
					},
				},
				{ID: "MOD-store", Name: "store", Commands: sixCommands("store")},
				{
					ID: "MOD-billing", Name: "billing", MayImport: []string{"MOD-ledger"},
					Commands: sixCommands("billing"),
					Contracts: []kctx.Contract{
						{ID: "CTR-billing", Type: "openapi", Path: "contracts/billing.yaml"},
					},
				},
			},
			Artifacts: map[string]string{
				"avspec.yaml":            "REQ-everything: the entire manifest, verbatim",
				"contracts/api.yaml":     "openapi: 3.1.0  # the api contract",
				"contracts/billing.yaml": "openapi: 3.1.0  # the billing contract",
			},
		},
		ticket: kctx.Ticket{
			Title:    "Create a short link",
			Criteria: []string{"AC-valid-url"},
			Modules:  []string{"MOD-api", "MOD-store"},
			Patterns: []string{"off-by-one"},
		},
	}
	w.ctxw = c
	return c
}

// sixCommands gives a module the full gate chain, named after it so a leak is
// traceable to its source.
func sixCommands(module string) map[string]string {
	cmds := map[string]string{}
	for _, name := range []string{"test", "lint", "typecheck", "arch", "coverage", "mutation"} {
		cmds[name] = name + "-" + module
	}
	return cmds
}

func (c *contextWorld) assemble() error {
	a := kctx.Assembler{
		Store: c.bundles, Learnings: c.learnings, Now: fakes.NewClock(time.Unix(0, 0)),
	}
	bundle, err := a.Assemble(stdctx.Background(), "run-1", c.spec, c.ticket, c.instructions)
	if err != nil {
		return err
	}
	c.raw = string(bundle.Content)
	return json.Unmarshal(bundle.Content, &c.content)
}

// section renders one part of the bundle back to JSON for substring checks.
func (c *contextWorld) section(name string) string {
	b, err := json.Marshal(c.content[name])
	if err != nil {
		return ""
	}
	return string(b)
}

func registerContextAssembly(sc *godog.ScenarioContext, w *world) {
	sc.Step(`^a BuildRun bound to a snapshot and a ticket touching modules "([^"]*)" and "([^"]*)"$`,
		func(first, second string) error {
			c := w.newContext()
			c.ticket.Modules = []string{moduleID(first), moduleID(second)}
			return nil
		})

	sc.Step(`^the context manager assembles the dev agent's context$`, func() error {
		return w.ctxw.assemble()
	})

	sc.Step(`^the context is assembled$`, func() error { return w.ctxw.assemble() })
	sc.Step(`^the toolset is assembled$`, func() error { return w.ctxw.assemble() })

	sc.Step(`^it contains the ticket's acceptance criteria$`, func() error {
		if !strings.Contains(w.ctxw.section("criteria"), "AC-valid-url") {
			return fmt.Errorf("criteria are %s", w.ctxw.section("criteria"))
		}
		return nil
	})

	sc.Step(`^the boundaries, contracts, and constitution of "([^"]*)" and "([^"]*)"$`,
		func(first, second string) error {
			touched := w.ctxw.section("touched_modules")
			for _, want := range []string{
				moduleID(first), moduleID(second), "MOD-store", "CTR-api", "the api contract",
			} {
				if !strings.Contains(touched, want) {
					return fmt.Errorf("%q is missing from the touched modules: %s", want, touched)
				}
			}
			if !strings.Contains(w.ctxw.section("constitution"), "CON-test-first") {
				return errors.New("the constitution is missing")
			}
			return nil
		})

	sc.Step(`^it is not a dump of the whole spec$`, func() error {
		// A manifest pasted whole is not context; it is the thing context
		// exists to replace.
		if strings.Contains(w.ctxw.raw, "the entire manifest") {
			return errors.New("the manifest was dumped into the context")
		}
		return nil
	})

	sc.Step(`^the project carries an operator-authored instruction file$`, func() error {
		c := w.newContext()
		c.instructions = "# House rules\n\nNever swallow an exception.\n"
		return nil
	})

	sc.Step(`^the operator's instructions are included verbatim$`, func() error {
		got, _ := w.ctxw.content["instructions"].(string)
		if got != w.ctxw.instructions {
			return fmt.Errorf("instructions came through as %q", got)
		}
		return nil
	})

	sc.Step(`^kriya's process playbook is included — driving roborev, the pair-loop protocol, and commit conventions$`,
		func() error {
			playbook, _ := w.ctxw.content["playbook"].(string)
			for _, want := range []string{"roborev", "pair-programming loop", "Conventional commit"} {
				if !strings.Contains(playbook, want) {
					return fmt.Errorf("the playbook does not cover %q", want)
				}
			}
			return nil
		})

	sc.Step(`^the ticket's modules may call module "([^"]*)" but not import its internals$`,
		func(module string) error {
			c := w.newContext()
			if !hasModule(c.spec, moduleID(module)) {
				return fmt.Errorf("the fixture has no module %s", moduleID(module))
			}
			return nil
		})

	sc.Step(`^"([^"]*)" appears only as its contract$`, func(module string) error {
		reachable := w.ctxw.section("reachable_modules")
		if !strings.Contains(reachable, "the "+moduleName(module)+" contract") {
			return fmt.Errorf("%s's contract is missing: %s", module, reachable)
		}
		return nil
	})

	sc.Step(`^no internal source of "([^"]*)" is included$`, func(module string) error {
		name := moduleName(module)
		// Its boundary, its commands: everything the agent would need only if
		// it were importing the module rather than calling it.
		for _, internal := range []string{"MOD-ledger", "test-" + name, "mutation-" + name} {
			if strings.Contains(w.ctxw.raw, internal) {
				return fmt.Errorf("%s leaked into the context", internal)
			}
		}
		return nil
	})

	sc.Step(`^the snapshot resolves commands for the ticket's modules$`, func() error {
		w.newContext()
		return nil
	})

	sc.Step(`^the touched modules' resolved test, lint, typecheck, arch, coverage, and mutation commands are wired in$`,
		func() error {
			tools := w.ctxw.section("tools")
			for _, module := range w.ctxw.ticket.Modules {
				name := strings.TrimPrefix(module, "MOD-")
				for _, gate := range []string{"test", "lint", "typecheck", "arch", "coverage", "mutation"} {
					if !strings.Contains(tools, gate+"-"+name) {
						return fmt.Errorf("%s's %s command is not wired in: %s", module, gate, tools)
					}
				}
			}
			return nil
		})

	sc.Step(`^tools irrelevant to the ticket are absent$`, func() error {
		if strings.Contains(w.ctxw.section("tools"), "billing") {
			return fmt.Errorf("a tool for an untouched module is wired in: %s",
				w.ctxw.section("tools"))
		}
		return nil
	})

	sc.Step(`^persistent learnings exist for module "([^"]*)" and for a failure pattern the ticket matches$`,
		func(module string) error {
			c := w.newContext()
			c.learnings.rows = []kctx.Learning{
				{Lesson: "pagination here is one-based", Module: moduleID(module), Pattern: "pagination"},
				{Lesson: "loops run to len, not len-1", Module: "MOD-store", Pattern: "off-by-one"},
				{Lesson: "billing rounds half-even", Module: "MOD-billing", Pattern: "rounding"},
			}
			return nil
		})

	sc.Step(`^those learnings are present in the context$`, func() error {
		got := w.ctxw.section("learnings")
		for _, want := range []string{"pagination here is one-based", "loops run to len"} {
			if !strings.Contains(got, want) {
				return fmt.Errorf("a matching learning is missing: %q in %s", want, got)
			}
		}
		return nil
	})

	sc.Step(`^learnings for unrelated modules are not$`, func() error {
		if strings.Contains(w.ctxw.section("learnings"), "billing rounds half-even") {
			return errors.New("an unrelated module's learning was injected")
		}
		return nil
	})

	sc.Step(`^a run's context was assembled$`, func() error {
		w.newContext()
		return w.ctxw.assemble()
	})

	sc.Step(`^the run durably records what the agent was given$`, func() error {
		if _, found, err := w.ctxw.bundles.Get(stdctx.Background(), "run-1"); err != nil || !found {
			return fmt.Errorf("no bundle recorded for the run: %v found=%v", err, found)
		}
		return nil
	})

	sc.Step(`^a reviewer can reconstruct exactly what the agent knew when it acted$`, func() error {
		saved, _, err := w.ctxw.bundles.Get(stdctx.Background(), "run-1")
		if err != nil {
			return err
		}
		if string(saved.Content) != w.ctxw.raw {
			return errors.New("the recorded bundle differs from what the agent was given")
		}
		return nil
	})
}

func moduleName(id string) string {
	return strings.ToLower(strings.TrimPrefix(id, "MOD-"))
}

func hasModule(spec kctx.Spec, id string) bool {
	for _, m := range spec.Modules {
		if m.ID == id {
			return true
		}
	}
	return false
}
