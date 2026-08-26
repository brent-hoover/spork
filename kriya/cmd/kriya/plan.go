package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"

	"kriya/internal/planner"
)

// planCommand runs the operator's plan actions.
//
// ACT-plan-restore and ACT-plan-retry. They are one code path because they
// are one operation: the operator says "unpark this", and which way it goes —
// back to pending, back to active, into the CAS again, or straight to
// terminal historical — is decided by the plan's own durable progress, not by
// which word the operator typed.
func planCommand(ctx context.Context, out io.Writer, db *sql.DB, args []string) error {
	if len(args) != 2 || (args[0] != "restore" && args[0] != "retry") {
		return errors.New("usage: kriya plan restore <decomposition-key> | kriya plan retry <decomposition-key>")
	}
	restorer := planner.Restorer{
		Plans: planner.SQLPlans{DB: db},
		Heads: planner.SQLHeads{DB: db, Advances: planner.SQLAdvances{DB: db}},
		Steps: planner.SQLSteps{DB: db},
	}
	got, err := restorer.Restore(ctx, args[1])
	if err != nil {
		return err
	}

	// What it BECAME, not what was asked for. A retry that found the head had
	// outrun the candidate reports terminal historical, and the operator needs
	// to see that rather than a cheerful "retried".
	if _, err := fmt.Fprintf(out, "plan %s is now %s\n", planner.Short(got.Key), got.State); err != nil {
		return err
	}
	if got.State == planner.PlanHistorical {
		_, err := fmt.Fprintf(out,
			"  %s\n  adopting this decomposition again requires a fresh intake\n", got.Error)
		return err
	}
	return nil
}
