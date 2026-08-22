// Package main is the composition root: it opens the store, applies each
// module's DDL, wires the modules together, runs recovery, and owns main().
//
// NOT a declared avspec module — avspec has no composition-root concept, yet
// arch-go requires 100% package coverage. Same gap sutra logged.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"kriya/internal/agent"
	"kriya/internal/cli"
	"kriya/internal/clock"
	"kriya/internal/planner"
	"kriya/internal/recovery"
	"kriya/internal/specverify"
	"kriya/internal/trackerclient"
)

// defaultDBPath is used when KRIYA_DB is unset. Configuration moves to
// kriya.toml with the rest of it.
const defaultDBPath = "kriya.db"

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "kriya:", err)
		os.Exit(1)
	}
}

// run opens the store and dispatches.
func run(ctx context.Context, args []string) error {
	db, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	if len(args) == 0 {
		return errors.New("usage: kriya build <project>")
	}
	switch args[0] {
	case "build":
		if len(args) != 2 {
			return errors.New("usage: kriya build <project>")
		}
		return build(ctx, db, args[1])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// dsn builds the connection string.
//
// foreign_keys is OFF by default in SQLite, so REFERENCES clauses in module
// schemas would parse and then enforce nothing. busy_timeout keeps parallel
// dev agents from failing outright on a momentarily locked database.
//
// Tests call this rather than repeating the pragmas: a test that rebuilt the
// same string would pass while production had none, proving only that the
// test agrees with itself.
func dsn(path string) string {
	return "file:" + path + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
}

// openStore opens the one database and applies each module's schema.
func openStore(ctx context.Context) (*sql.DB, error) {
	path := os.Getenv("KRIYA_DB")
	if path == "" {
		path = defaultDBPath
	}
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("open store %s: %w", path, err)
	}
	if err := applyMigrations(ctx, db, migrations()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// build wires the modules for one target, reconciles what a previous run left
// open, and runs the command.
func build(ctx context.Context, db *sql.DB, arg string) error {
	target, err := filepath.Abs(arg)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", arg, err)
	}
	actor := os.Getenv("KRIYA_ACTOR")
	// Validated BEFORE any mutation. sutra settles a rejected request under
	// its idempotency key — deliberately, so a different request cannot reuse
	// a key and mutate — so a request kriya knew was invalid would poison that
	// key permanently. The cure is not to send it.
	if _, err := uuid.Parse(actor); err != nil {
		return fmt.Errorf("KRIYA_ACTOR must be an identity uuid: %w", err)
	}
	tiers, err := loadTiers()
	if err != nil {
		return err
	}
	// Validated at startup: AC-tier-explicit wants a missing tier to fail
	// loudly, not halfway through a decomposition.
	if err := tiers.Validate(agent.RolePM); err != nil {
		return err
	}

	in := planner.Intaker{
		Verify:    verifier(),
		Snapshots: planner.SQLSnapshots{DB: db},
		Targets:   planner.SQLTargets{DB: db},
		Tracker:   sutraTracker{c: trackerclient.New(sutraURL())},
		Agent: agent.Recording{
			Inner:  agent.Claude{Tiers: tiers},
			Ledger: agent.Ledger{DB: db, Now: clock.System{}},
			Tiers:  tiers,
			Now:    clock.System{},
			Scope:  agent.Scope{Plan: target},
		},
		Now: clock.System{},
	}

	// Recovery runs BEFORE any new work, in declared stage order. Nothing pops
	// until every crash window a previous run left open is reconciled.
	if err := recovery.Run(ctx, recoverySteps(in)); err != nil {
		return err
	}
	return cli.Build(ctx, os.Stdout, in, target, actor)
}

// recoverySteps maps modules to the stages they reconcile.
//
// The whole ordering lives here, in the composition root, rather than being
// spread across the modules — which is the point of sequencing being a
// composition-root concern. Stages with no owner yet simply have no step; they
// gain one as their module lands.
func recoverySteps(in planner.Intaker) []recovery.Step {
	return []recovery.Step{
		{Stage: recovery.StageTargets, Owner: "planner", Run: func(ctx context.Context) error {
			_, err := in.RecoverTargets(ctx)
			return err
		}},
	}
}

// verifier decides how to invoke avspec.
//
// The default is plain `avspec` on PATH, which fails with a clear "executable
// not found" if it is not installed. An earlier version always used
// `uv run avspec`, which resolves its environment from the WORKING DIRECTORY —
// so the advertised command worked only when run from a directory where uv
// could find the avspec project, and failed obscurely everywhere else.
//
// KRIYA_AVSPEC_DIR names a uv project holding avspec, for a checkout that has
// not installed it.
func verifier() specverify.CLI {
	if dir := os.Getenv("KRIYA_AVSPEC_DIR"); dir != "" {
		return specverify.CLI{Argv: []string{"uv", "run", "avspec"}, WorkDir: dir}
	}
	return specverify.CLI{Argv: []string{"avspec"}}
}

// sutraURL is where the tracker lives.
func sutraURL() string {
	if u := os.Getenv("KRIYA_SUTRA_URL"); u != "" {
		return u
	}
	return "http://127.0.0.1:7357"
}

// loadTiers reads the role-to-model configuration.
//
// No model identifier appears in kriya's source (AC-tier-config).
func loadTiers() (agent.Tiers, error) {
	path := os.Getenv("KRIYA_TIERS")
	if path == "" {
		return agent.Tiers{}, errors.New("KRIYA_TIERS is unset: no role tiers configured")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return agent.Tiers{}, fmt.Errorf("read tiers %s: %w", path, err)
	}
	var t agent.Tiers
	if err := json.Unmarshal(raw, &t); err != nil {
		return agent.Tiers{}, fmt.Errorf("parse tiers %s: %w", path, err)
	}
	return t, nil
}
