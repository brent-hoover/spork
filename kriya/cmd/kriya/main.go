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
	db, err := sql.Open("sqlite", path)
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
