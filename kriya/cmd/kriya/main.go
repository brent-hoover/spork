package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"kriya/internal/agent"
	"kriya/internal/cli"
	"kriya/internal/clock"
	"kriya/internal/planner"
	"kriya/internal/specverify"
	"kriya/internal/trackerclient"

	_ "modernc.org/sqlite"
)

// defaultDBPath is used when KRIYA_DB is unset. The command surface that
// will make this configurable arrives with internal/cli in M2.
const defaultDBPath = "kriya.db"

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

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "kriya:", err)
		os.Exit(1)
	}
}

// verifier decides how to invoke avspec.
//
// The default is plain `avspec` on PATH, which fails with a clear "executable
// not found" if it is not installed. An earlier version always used
// `uv run avspec`, which resolves its environment from the WORKING DIRECTORY
// — so the advertised command worked only when run from a directory where uv
// could find the avspec project, and failed obscurely everywhere else.
//
// KRIYA_AVSPEC_DIR names a uv project holding avspec, for a checkout that has
// not installed it: kriya then runs `uv run avspec` there. Both move into
// kriya.toml with the rest of the configuration.
// sutraURL is where the tracker lives. It moves into kriya.toml with the rest
// of the configuration.
func sutraURL() string {
	if u := os.Getenv("KRIYA_SUTRA_URL"); u != "" {
		return u
	}
	return "http://127.0.0.1:7357"
}

// loadTiers reads the role-to-model configuration.
//
// No model identifier appears in kriya's source (AC-tier-config). The file
// moves to kriya.toml with the rest of the configuration; KRIYA_TIERS names it
// meanwhile.
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

func verifier() specverify.CLI {
	if dir := os.Getenv("KRIYA_AVSPEC_DIR"); dir != "" {
		return specverify.CLI{Argv: []string{"uv", "run", "avspec"}, WorkDir: dir}
	}
	return specverify.CLI{Argv: []string{"avspec"}}
}

// run is the composition root: it opens the one store, applies each module's
// schema in the declared order, and will wire the modules and run recovery
// before any pop once those exist.
func run(ctx context.Context, args []string) error {
	path := os.Getenv("KRIYA_DB")
	if path == "" {
		path = defaultDBPath
	}
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return fmt.Errorf("open store %s: %w", path, err)
	}
	defer func() { _ = db.Close() }()

	if err := applyMigrations(ctx, db, migrations()); err != nil {
		return err
	}

	if len(args) == 0 {
		return fmt.Errorf("usage: kriya build <project>")
	}
	switch args[0] {
	case "build":
		if len(args) != 2 {
			return fmt.Errorf("usage: kriya build <project>")
		}
		target, err := filepath.Abs(args[1])
		if err != nil {
			return fmt.Errorf("resolve %s: %w", args[1], err)
		}
		tiers, err := loadTiers()
		if err != nil {
			return err
		}
		// Validated at startup, before any build begins: AC-tier-explicit
		// wants a missing tier to fail loudly, not on the first invocation
		// halfway through a decomposition.
		if err := tiers.Validate(agent.RolePM); err != nil {
			return err
		}
		pmAgent := agent.Recording{
			Inner:  agent.Claude{Tiers: tiers},
			Ledger: agent.Ledger{DB: db, Now: clock.System{}},
			Tiers:  tiers,
			Now:    clock.System{},
			Scope:  agent.Scope{Plan: target},
		}
		in := planner.Intaker{
			Verify:    verifier(),
			Snapshots: planner.SQLSnapshots{DB: db},
			Targets:   planner.SQLTargets{DB: db},
			Tracker:   sutraTracker{c: trackerclient.New(sutraURL())},
			Agent:     pmAgent,
			Now:       clock.System{},
		}
		return cli.Build(ctx, os.Stdout, in, target, os.Getenv("KRIYA_ACTOR"))
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}
