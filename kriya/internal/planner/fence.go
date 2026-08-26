package planner

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// FenceMigration is the singleton row that serializes admission with head
// transitions.
//
// ONE row, and singleton_key's uniqueness is what makes the bootstrap insert
// its own CAS: a concurrent first writer either inserts it or reads the row
// the winner inserted. It starts at zero on both counts — no plan exists yet,
// so nothing is unactivated and admissions flow immediately.
const FenceMigration = `
CREATE TABLE pop_fence (
    singleton_key     TEXT PRIMARY KEY,
    unactivated_heads INTEGER NOT NULL DEFAULT 0,
    version           INTEGER NOT NULL DEFAULT 0
)`

// fenceKey is the constant that makes the row a singleton.
const fenceKey = "pop-fence"

// Fence is the durable admission guard.
//
// GLOBAL, not per-target: it counts unactivated heads across every target, so
// one target mid-supersession refuses admission everywhere. That is the point
// — a pop admitted against any unactivated head is work started before its
// predecessor retired.
type Fence struct {
	UnactivatedHeads int
	// Version strictly increases on every write. An admission CAS is
	// conditional on it, which is what makes admission and a concurrent head
	// transition genuine write-write conflicts on one row.
	Version int
}

// ErrFenced says admission was refused.
//
// Its own type because the caller's response is specific: a fenced pop is not
// a failure to retry immediately but a wait for a head to activate.
type ErrFenced struct {
	UnactivatedHeads int
	StaleVersion     bool
}

func (e *ErrFenced) Error() string {
	if e.StaleVersion {
		return "admission lost the fence CAS: the fence moved under the read"
	}
	return fmt.Sprintf("admission refused: %d head(s) are unactivated", e.UnactivatedHeads)
}

// SQLFence is the planner-owned pop fence.
//
// Owned by the planner and written ONLY here, including by the admission
// guard. The orchestrator invokes Admit inside its own BuildRun-creating
// transaction but never writes the row itself.
type SQLFence struct{ DB *sql.DB }

// Read returns the fence as it stands.
//
// The version it returns is what a later Admit must present. A caller that
// read and then admitted without carrying the version could commit beside a
// concurrent replacement — which is precisely what a read-assert cannot stop.
func (f SQLFence) Read(ctx context.Context) (Fence, error) {
	var got Fence
	err := f.DB.QueryRowContext(ctx,
		`SELECT unactivated_heads, version FROM pop_fence WHERE singleton_key = ?`,
		fenceKey).Scan(&got.UnactivatedHeads, &got.Version)
	if errors.Is(err, sql.ErrNoRows) {
		// Nothing has ever written it. Zero on both counts is the honest
		// answer and it is also the bootstrap state.
		return Fence{}, nil
	}
	if err != nil {
		return Fence{}, fmt.Errorf("read the pop fence: %w", err)
	}
	return got, nil
}

// Admit takes one admission slot, or refuses.
//
// A CONDITIONAL WRITE, never a read-assert. The guard CAS-increments version
// conditional on unactivated_heads == 0 AND version being the one the caller
// read. A read-only assertion could still commit beside a concurrent
// replacement — both would observe a clear fence and both would proceed. The
// conditional write makes them write-write conflicts on the same row, of
// which exactly one commits.
func (f SQLFence) Admit(ctx context.Context, readVersion int) error {
	res, err := f.DB.ExecContext(ctx,
		`UPDATE pop_fence SET version = version + 1
		   WHERE singleton_key = ? AND unactivated_heads = 0 AND version = ?`,
		fenceKey, readVersion)
	if err != nil {
		return fmt.Errorf("admission CAS: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("admission CAS: %w", err)
	}
	if rows == 1 {
		return nil
	}

	// Refused. WHICH refusal matters to the caller: a stale version is a lost
	// race worth retrying at once, while a nonzero counter is a wait for a
	// head to activate.
	current, err := f.Read(ctx)
	if err != nil {
		return err
	}
	if current.UnactivatedHeads > 0 {
		return &ErrFenced{UnactivatedHeads: current.UnactivatedHeads}
	}
	return &ErrFenced{StaleVersion: true}
}

// raiseWithin adds an unactivated head, inside the caller's transaction.
func raiseWithin(ctx context.Context, tx Tx, by int) error {
	if by == 0 {
		return nil
	}
	if err := ensureFenceWithin(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE pop_fence SET unactivated_heads = unactivated_heads + ?, version = version + 1
		   WHERE singleton_key = ?`, by, fenceKey); err != nil {
		return fmt.Errorf("raise the pop fence: %w", err)
	}
	return nil
}

// lowerWithin removes an unactivated head, inside the caller's transaction.
//
// Floored at zero: a replayed activation must not take the counter negative,
// where a later replacement's raise would bring it back to zero and admit
// pops against a head that never activated.
func lowerWithin(ctx context.Context, tx Tx) error {
	if err := ensureFenceWithin(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE pop_fence SET unactivated_heads = unactivated_heads - 1, version = version + 1
		   WHERE singleton_key = ? AND unactivated_heads > 0`, fenceKey); err != nil {
		return fmt.Errorf("lower the pop fence: %w", err)
	}
	return nil
}

// ensureFenceWithin inserts the singleton if it is not there yet.
func ensureFenceWithin(ctx context.Context, tx Tx) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO pop_fence (singleton_key, unactivated_heads, version)
		 VALUES (?, 0, 0) ON CONFLICT(singleton_key) DO NOTHING`, fenceKey); err != nil {
		return fmt.Errorf("bootstrap the pop fence: %w", err)
	}
	return nil
}
