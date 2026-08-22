//go:build acceptance

package acceptance

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/cucumber/godog"

	"kriya/internal/agent"
	"kriya/internal/fakes"
)

// modelIdentifier matches a real model name, the thing AC-tier-config forbids
// in source. Deliberately broad across vendors: the rule is that kriya's code
// names no model, not that it names no Claude model.
var modelIdentifier = regexp.MustCompile(`\b(claude-(opus|sonnet|haiku|fable|mythos)|gpt-[0-9]|gemini-|llama-)[A-Za-z0-9.-]*`)

// tierWorld is this feature's state. Routing is about configuration and the
// ledger, not about specs, so it does not use the intake world.
type tierWorld struct {
	tiers   agent.Tiers
	startup error
	db      *sql.DB
	cleanup []func()
}

func registerTierRouting(sc *godog.ScenarioContext) {
	var w *tierWorld
	sc.Before(func(ctx context.Context, _ *godog.Scenario) (context.Context, error) {
		w = &tierWorld{}
		return ctx, nil
	})
	sc.After(func(ctx context.Context, _ *godog.Scenario, _ error) (context.Context, error) {
		for _, fn := range w.cleanup {
			fn()
		}
		return ctx, nil
	})

	sc.Given(`^kriya's routing configuration$`, func() error {
		w.tiers = fullTiers()
		return nil
	})
	sc.Given(`^a routing configuration with no entry for the SA role$`, func() error {
		w.tiers = fullTiers()
		delete(w.tiers.Roles, agent.RoleSA)
		return nil
	})
	sc.Given(`^the dev role is routed to tier "([^"]*)"$`, func(tier string) error {
		w.tiers = fullTiers()
		w.tiers.Roles[agent.RoleDev] = tier
		w.tiers.Models[tier] = tier + "-model"
		return nil
	})

	sc.When(`^kriya starts$`, func() error {
		// Startup validates every role it will invoke.
		w.startup = w.tiers.Validate(agent.RolePM, agent.RoleSA, agent.RolePO, agent.RoleDev)
		return nil
	})
	sc.When(`^the operator changes the dev role to tier "([^"]*)" and a new run starts$`,
		func(tier string) error { return w.rerouteAndRun(tier) })

	sc.Then(`^each role — PM, SA, PO, dev — maps to a model tier there$`, func() error {
		for _, role := range []agent.Role{agent.RolePM, agent.RoleSA, agent.RolePO, agent.RoleDev} {
			if _, _, err := w.tiers.Resolve(role); err != nil {
				return fmt.Errorf("role %s: %w", role, err)
			}
		}
		return nil
	})
	sc.Then(`^no model identifier is hardcoded in kriya's source$`,
		func() error { return assertNoHardcodedModels() })
	sc.Then(`^startup fails naming the unconfigured role$`, func() error {
		if w.startup == nil {
			return fmt.Errorf("startup succeeded with the SA role unconfigured")
		}
		if !strings.Contains(w.startup.Error(), string(agent.RoleSA)) {
			return fmt.Errorf("the failure does not name the role: %v", w.startup)
		}
		return nil
	})
	sc.Then(`^no silent default is applied$`, func() error {
		// Resolve must refuse, not substitute. A default is exactly how a
		// build runs for a week on a model nobody chose.
		if _, model, err := w.tiers.Resolve(agent.RoleSA); err == nil {
			return fmt.Errorf("an unconfigured role resolved to %q", model)
		}
		return nil
	})
	sc.Then(`^the dev agent runs on tier "([^"]*)" with no code change$`,
		func(tier string) error { return w.assertLedgerRecorded(tier) })
	sc.Then(`^every agent invocation durably records its role, configured tier, and the resolved model that actually ran$`,
		func() error { return w.assertEveryInvocationIsComplete() })
	sc.Then(`^a plan-scoped PM invocation is recorded even though no BuildRun exists yet$`,
		func() error { return w.assertPlanScopedPMRecorded() })
}

func fullTiers() agent.Tiers {
	return agent.Tiers{
		Roles: map[agent.Role]string{
			agent.RolePM: "deep", agent.RoleSA: "deep",
			agent.RolePO: "deep", agent.RoleDev: "fast",
		},
		Models: map[string]string{"deep": "deep-model", "fast": "fast-model"},
	}
}

// rerouteAndRun changes the dev tier and runs an agent through the ledger.
func (w *tierWorld) rerouteAndRun(tier string) error {
	w.tiers.Roles[agent.RoleDev] = tier
	if _, ok := w.tiers.Models[tier]; !ok {
		w.tiers.Models[tier] = tier + "-model"
	}
	db, err := w.openLedger()
	if err != nil {
		return err
	}
	rec := agent.Recording{
		Inner:  &fakes.Agent{Replies: []agent.Result{{SessionID: "s1", Model: w.tiers.Models[tier]}}},
		Ledger: agent.Ledger{DB: db, Now: fakes.NewClock(time.Unix(0, 0))},
		Tiers:  w.tiers,
		Now:    fakes.NewClock(time.Unix(0, 0)),
		Scope:  agent.Scope{Build: "build-1"},
	}
	if _, err := rec.Run(context.Background(), agent.Request{Role: agent.RoleDev, Prompt: "x"}); err != nil {
		return fmt.Errorf("run dev agent: %w", err)
	}
	return nil
}

func (w *tierWorld) openLedger() (*sql.DB, error) {
	dir, err := os.MkdirTemp("", "kriya-tiers-")
	if err != nil {
		return nil, fmt.Errorf("temp dir: %w", err)
	}
	w.cleanup = append(w.cleanup, func() { _ = os.RemoveAll(dir) })
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "k.db"))
	if err != nil {
		return nil, fmt.Errorf("open ledger: %w", err)
	}
	w.cleanup = append(w.cleanup, func() { _ = db.Close() })
	if _, err := db.Exec(agent.Migration); err != nil {
		return nil, fmt.Errorf("create ledger: %w", err)
	}
	w.db = db
	return db, nil
}

// assertLedgerRecorded checks the run recorded the tier AND the model.
func (w *tierWorld) assertLedgerRecorded(tier string) error {
	var gotTier, gotModel string
	err := w.db.QueryRow(
		`SELECT tier, model FROM agent_invocation WHERE role = ?`, string(agent.RoleDev)).
		Scan(&gotTier, &gotModel)
	if err != nil {
		return fmt.Errorf("read ledger: %w", err)
	}
	if gotTier != tier {
		return fmt.Errorf("ledger recorded tier %q, want %q", gotTier, tier)
	}
	if gotModel == "" {
		return fmt.Errorf("ledger recorded no model")
	}
	return nil
}

// assertEveryInvocationIsComplete checks no recorded row is missing a field.
//
// "Durably" and "actually ran" are both load-bearing. The row survives the
// process, and the model is what the invocation reported rather than what was
// requested — the first real run recorded two models where one was
// configured, so those genuinely differ.
func (w *tierWorld) assertEveryInvocationIsComplete() error {
	if w.db == nil {
		return fmt.Errorf("no ledger: nothing ran")
	}
	rows, err := w.db.Query(`SELECT role, tier, model, session FROM agent_invocation`)
	if err != nil {
		return fmt.Errorf("read ledger: %w", err)
	}
	defer func() { _ = rows.Close() }()

	n := 0
	for rows.Next() {
		var role, tier, model, session string
		if err := rows.Scan(&role, &tier, &model, &session); err != nil {
			return fmt.Errorf("scan invocation: %w", err)
		}
		n++
		for name, value := range map[string]string{
			"role": role, "tier": tier, "model": model, "session": session,
		} {
			if value == "" {
				return fmt.Errorf("an invocation recorded no %s", name)
			}
		}
	}
	// Checked, because a cursor failing mid-iteration otherwise returns a
	// SHORT list that reads exactly like "every row was complete".
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate ledger: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("no invocations were recorded")
	}
	return nil
}

// assertPlanScopedPMRecorded runs a PM agent with no BuildRun in existence.
//
// This is the case that forced internal/agent to own the ledger at all:
// ENT-agent-invocation is declared under MOD-orchestrator, but planner must
// record a PM invocation before any BuildRun exists, and planner may not
// import orchestrator. Exactly one of plan or build is set, enforced by a
// CHECK constraint because the format cannot express conditional
// requiredness — so a row scoped to neither or both cannot be written.
func (w *tierWorld) assertPlanScopedPMRecorded() error {
	db := w.db
	if db == nil {
		var err error
		if db, err = w.openLedger(); err != nil {
			return err
		}
	}
	rec := agent.Recording{
		Inner:  &fakes.Agent{Replies: []agent.Result{{SessionID: "pm-session", Model: "deep-model"}}},
		Ledger: agent.Ledger{DB: db, Now: fakes.NewClock(time.Unix(0, 0))},
		Tiers:  w.tiers,
		Now:    fakes.NewClock(time.Unix(0, 0)),
		Scope:  agent.Scope{Plan: "plan-1"},
	}
	if _, err := rec.Run(context.Background(), agent.Request{Role: agent.RolePM, Prompt: "x"}); err != nil {
		return fmt.Errorf("run pm agent: %w", err)
	}
	var plan, build, model string
	err := db.QueryRow(
		`SELECT plan, build, model FROM agent_invocation WHERE role = ?`, string(agent.RolePM)).
		Scan(&plan, &build, &model)
	if err != nil {
		return fmt.Errorf("read pm invocation: %w", err)
	}
	if plan == "" {
		return fmt.Errorf("the PM invocation is not plan-scoped")
	}
	if build != "" {
		return fmt.Errorf("the PM invocation is also build-scoped (%q); exactly one applies", build)
	}
	if model == "" {
		return fmt.Errorf("the PM invocation recorded no resolved model")
	}
	return nil
}

// assertNoHardcodedModels scans kriya's production source.
//
// A real scan, not a promise. AC-tier-config says no model identifier appears
// in kriya's source, and the only way that stays true is a check that fails
// when someone types one.
func assertNoHardcodedModels() error {
	root, err := repoRoot()
	if err != nil {
		return err
	}
	kriya := filepath.Join(root, "kriya")
	var offenders []string
	walkErr := filepath.Walk(kriya, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// PRODUCTION source only. The rule is that kriya's shipped code names
		// no model; a test may legitimately pin one to assert what a fixture
		// resolved to, and this file names several in the pattern that finds
		// them. Excluding _test.go is the scope the AC means, not a loophole:
		// nothing in a _test.go file reaches a running build.
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, hit := range modelIdentifier.FindAllString(string(b), -1) {
			rel, _ := filepath.Rel(kriya, path)
			offenders = append(offenders, rel+": "+hit)
		}
		return nil
	})
	if walkErr != nil {
		return fmt.Errorf("scan source: %w", walkErr)
	}
	if len(offenders) > 0 {
		return fmt.Errorf("model identifiers in source: %s", strings.Join(offenders, ", "))
	}
	return nil
}
