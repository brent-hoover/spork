package planner_test

import (
	"context"
	"errors"
	"testing"

	"kriya/internal/planner"
)

func TestALocalMutationCommitsWithItsAdvance(t *testing.T) {
	// They are ONE fact. A supersession's replacement and its epoch advance
	// committed separately leave either an unadvanced epoch over replaced
	// work, or an advance for a replacement that never happened.
	s := sqlAdvances(t)
	if _, err := s.DB.Exec(`CREATE TABLE plan_head (target_key TEXT PRIMARY KEY, generation INTEGER)`); err != nil {
		t.Fatalf("seed table: %v", err)
	}
	e := planner.Epochs{Store: s}
	fresh, err := e.OnLocalWith(context.Background(), "/spec",
		planner.CauseSupersession, "generation-7", func(tx planner.Tx) error {
			_, err := tx.ExecContext(context.Background(),
				`INSERT INTO plan_head (target_key, generation) VALUES (?, ?)`, "/spec", 7)
			return err
		})
	if err != nil || !fresh {
		t.Fatalf("advance: %v fresh=%v", err, fresh)
	}
	var generation int
	if err := s.DB.QueryRow(`SELECT generation FROM plan_head WHERE target_key = ?`, "/spec").
		Scan(&generation); err != nil {
		t.Fatalf("the mutation did not commit: %v", err)
	}
	if generation != 7 {
		t.Errorf("the mutation wrote generation %d", generation)
	}
	if epoch, _ := e.Current(context.Background(), "/spec"); epoch != 1 {
		t.Errorf("the epoch is %d", epoch)
	}
}

func TestAFailedLocalMutationRollsBackItsAdvance(t *testing.T) {
	// The advance must not stand for a replacement that did not happen: the
	// epoch cannot be walked back, and a spent claim would be invalidated
	// over nothing.
	s := sqlAdvances(t)
	e := planner.Epochs{Store: s}
	_, err := e.OnLocalWith(context.Background(), "/spec",
		planner.CauseSupersession, "generation-7", func(planner.Tx) error {
			return errors.New("the replacement CAS lost")
		})
	if err == nil {
		t.Fatal("a failed mutation reported success")
	}
	if epoch, _ := e.Current(context.Background(), "/spec"); epoch != 0 {
		t.Errorf("the epoch moved to %d for a replacement that never happened", epoch)
	}
	// And the advance row is gone, so a genuine retry can still insert.
	fresh, err := e.OnLocalWith(context.Background(), "/spec",
		planner.CauseSupersession, "generation-7", func(planner.Tx) error { return nil })
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if !fresh {
		t.Error("the rolled-back advance blocked its own retry")
	}
}

func TestAReplayedLocalOperationAppliesItsMutationOnce(t *testing.T) {
	// The mutation runs only when the advance is FRESH. A replay has already
	// applied it, and applying it twice is the double-application the key
	// exists to prevent.
	s := sqlAdvances(t)
	e := planner.Epochs{Store: s}
	applied := 0
	for range 4 {
		if _, err := e.OnLocalWith(context.Background(), "/spec",
			planner.CauseSupersession, "generation-7", func(planner.Tx) error {
				applied++
				return nil
			}); err != nil {
			t.Fatalf("advance: %v", err)
		}
	}
	if applied != 1 {
		t.Errorf("the mutation was applied %d times", applied)
	}
	if epoch, _ := e.Current(context.Background(), "/spec"); epoch != 1 {
		t.Errorf("the epoch is %d", epoch)
	}
}
