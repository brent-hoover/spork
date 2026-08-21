package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	"kriya/internal/cli"
	"kriya/internal/planner"
	"kriya/internal/specverify"

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

// verifierArgv is how avspec is invoked. It is configurable in principle —
// this repo runs it under uv — and will move into kriya.toml with the rest of
// the configuration in a later step.
func verifierArgv() []string { return []string{"uv", "run", "avspec"} }

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
		in := planner.Intaker{Verify: specverify.CLI{
			Argv:    verifierArgv(),
			WorkDir: os.Getenv("KRIYA_AVSPEC_DIR"),
		}}
		return cli.Build(ctx, os.Stdout, in, target)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}
