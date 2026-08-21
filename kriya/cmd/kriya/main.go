package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"

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
	if err := run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "kriya:", err)
		os.Exit(1)
	}
}

// run is the composition root: it opens the one store, applies each module's
// schema in the declared order, and will wire the modules and run recovery
// before any pop once those exist.
func run(ctx context.Context) error {
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
	// Module wiring, recovery.Run, and the command surface land in M2.
	return nil
}
