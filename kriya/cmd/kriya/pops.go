package main

import (
	"context"
	"database/sql"
	"fmt"

	"kriya/internal/orchestrator"
	"kriya/internal/trackerclient"
)

// reconcilePops binds every pop a crash left claimed-but-unbound.
//
// It runs BEFORE anything classifies ownership. A queued run with a pop key
// and no issue is a claim whose outcome is unknown, and retirement would read
// it as pending work — deferring a ticket that a live build is about to pick
// up. Replaying the pop under the PERSISTED key is what settles the question:
// sutra returns the same ticket if one was claimed, and an explicitly empty
// result if none was.
func reconcilePops(db *sql.DB, identity string) func(context.Context) error {
	return reconcilePopsWith(db, sutraPops{c: trackerclient.New(sutraURL()), identity: identity})
}

// reconcilePopsWith is the body, with the claim source supplied.
//
// A seam rather than a hidden client: the replay is the whole behaviour here,
// and a version that could only be exercised against a live sutra would be a
// recovery path nothing tested.
func reconcilePopsWith(db *sql.DB, pops orchestrator.Popper) func(context.Context) error {
	return func(ctx context.Context) error {
		store := orchestrator.SQLStore{DB: db}
		unbound, err := store.Unbound(ctx)
		if err != nil {
			return err
		}
		for _, run := range unbound {
			issue, title, err := pops.Pop(ctx, run.PopKey)
			if err != nil {
				return fmt.Errorf("replay the pop of run %s: %w", run.ID, err)
			}
			if issue == "" {
				// An explicitly empty replay: the claim never took anything.
				// No work is invented for it and nothing is classified as
				// issued.
				if err := store.SettleEmpty(ctx, run.ID); err != nil {
					return err
				}
				continue
			}
			if err := store.Bind(ctx, run.ID, issue, title); err != nil {
				return err
			}
		}
		return nil
	}
}
