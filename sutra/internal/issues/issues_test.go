package issues_test

import (
	"database/sql"
	"fmt"
	"testing"

	_ "modernc.org/sqlite"

	"sutra/internal/issues"
	"sutra/internal/projects"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=memory&cache=shared&_pragma=busy_timeout(5000)", t.Name()))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, migrate := range []func(*sql.DB) error{projects.Migrate, issues.Migrate} {
		if err := migrate(db); err != nil {
			t.Fatalf("migrate: %v", err)
		}
	}
	return db
}

func inTx(t *testing.T, db *sql.DB, fn func(tx *sql.Tx) error) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("tx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestPopFIFOAcrossCreationAndAssignment pins that a creation-time
// assignee stamps assigned_at: an issue assigned EARLIER through the
// endpoint pops before a later preassigned creation.
func TestPopFIFOAcrossCreationAndAssignment(t *testing.T) {
	db := openDB(t)
	agent := "00000000-0000-7000-8000-00000000000a"
	var project projects.Project
	var first, second issues.Issue
	inTx(t, db, func(tx *sql.Tx) error {
		var err error
		project, err = projects.Create(tx, projects.New{Key: "SUT", Name: "S"})
		if err != nil {
			return err
		}
		first, err = issues.Create(tx, project.ID, "assigned later", nil, nil)
		if err != nil {
			return err
		}
		if _, err := issues.Assign(tx, first.ID, &agent); err != nil {
			return err
		}
		second, err = issues.Create(tx, project.ID, "preassigned", nil, &agent)
		return err
	})
	inTx(t, db, func(tx *sql.Tx) error {
		claim, err := issues.PopCandidate(tx, agent)
		if err != nil {
			return err
		}
		if claim == nil || claim.ID != first.ID {
			return fmt.Errorf("expected the earlier assignment %s first, got %+v (second=%s)", first.ID, claim, second.ID)
		}
		return nil
	})
}

// TestPopSkipsArchivedBlockerChains pins that a blocker frozen in an
// archived project makes the chain unworkable — never claimable.
func TestPopSkipsArchivedBlockerChains(t *testing.T) {
	db := openDB(t)
	agent := "00000000-0000-7000-8000-00000000000a"
	var blocked, blocker, fallback issues.Issue
	inTx(t, db, func(tx *sql.Tx) error {
		active, err := projects.Create(tx, projects.New{Key: "SUT", Name: "S"})
		if err != nil {
			return err
		}
		archived, err := projects.Create(tx, projects.New{Key: "OLD", Name: "O"})
		if err != nil {
			return err
		}
		blocked, err = issues.Create(tx, active.ID, "blocked", nil, &agent)
		if err != nil {
			return err
		}
		blocker, err = issues.Create(tx, archived.ID, "frozen blocker", nil, &agent)
		if err != nil {
			return err
		}
		if _, err := issues.AddRelation(tx, "blocks", blocker.ID, blocked.ID); err != nil {
			return err
		}
		if _, err := projects.Archive(tx, archived.ID); err != nil {
			return err
		}
		fallback, err = issues.Create(tx, active.ID, "workable", nil, &agent)
		return err
	})
	inTx(t, db, func(tx *sql.Tx) error {
		claim, err := issues.PopCandidate(tx, agent)
		if err != nil {
			return err
		}
		if claim == nil {
			return fmt.Errorf("expected the fallback claimable")
		}
		if claim.ID == blocker.ID || claim.ID == blocked.ID {
			return fmt.Errorf("archived-blocker chain handed out %s", claim.ID)
		}
		if claim.ID != fallback.ID {
			return fmt.Errorf("expected %s, got %s", fallback.ID, claim.ID)
		}
		return nil
	})
}

// TestPopDeepestAcrossBranches pins branched blocker graphs: with two
// direct blockers of unequal chain depth, the DEEPEST open blocker is
// claimed, not the first shallow branch.
func TestPopDeepestAcrossBranches(t *testing.T) {
	db := openDB(t)
	agent := "00000000-0000-7000-8000-00000000000a"
	var deep issues.Issue
	inTx(t, db, func(tx *sql.Tx) error {
		project, err := projects.Create(tx, projects.New{Key: "SUT", Name: "S"})
		if err != nil {
			return err
		}
		mk := func(title string) issues.Issue {
			i, err := issues.Create(tx, project.ID, title, nil, &agent)
			if err != nil {
				t.Fatalf("create %s: %v", title, err)
			}
			return i
		}
		candidate := mk("candidate")
		shallow := mk("shallow blocker")
		mid := mk("mid blocker")
		deep = mk("deep blocker")
		for _, rel := range [][2]string{
			{shallow.ID, candidate.ID}, // branch 1: depth 1
			{mid.ID, candidate.ID},     // branch 2: depth 2 via deep
			{deep.ID, mid.ID},
		} {
			if _, err := issues.AddRelation(tx, "blocks", rel[0], rel[1]); err != nil {
				return err
			}
		}
		return nil
	})
	inTx(t, db, func(tx *sql.Tx) error {
		claim, err := issues.PopCandidate(tx, agent)
		if err != nil {
			return err
		}
		if claim == nil || claim.ID != deep.ID {
			return fmt.Errorf("expected the deepest blocker %s, got %+v", deep.ID, claim)
		}
		return nil
	})
}

// TestPopDiamondBlockerGraph pins converging DAGs: a shared descendant
// reached first through a short branch must still contribute its full
// depth to the longer branch — the deepest blocker wins regardless of
// traversal order.
func TestPopDiamondBlockerGraph(t *testing.T) {
	db := openDB(t)
	agent := "00000000-0000-7000-8000-00000000000a"
	var deepest issues.Issue
	inTx(t, db, func(tx *sql.Tx) error {
		project, err := projects.Create(tx, projects.New{Key: "SUT", Name: "S"})
		if err != nil {
			return err
		}
		mk := func(title string) issues.Issue {
			i, err := issues.Create(tx, project.ID, title, nil, &agent)
			if err != nil {
				t.Fatalf("create %s: %v", title, err)
			}
			return i
		}
		// candidate <- a <- shared ; candidate <- b <- shared <- deepest
		// (a sits before b by number so the SHORT path visits shared
		// first; the long path must still see shared's full subtree.)
		candidate := mk("candidate")
		a := mk("a short")
		shared := mk("shared")
		deepest = mk("deepest")
		for _, rel := range [][2]string{
			{a.ID, candidate.ID},
			{shared.ID, a.ID},
			{shared.ID, candidate.ID},
			{deepest.ID, shared.ID},
		} {
			if _, err := issues.AddRelation(tx, "blocks", rel[0], rel[1]); err != nil {
				return err
			}
		}
		return nil
	})
	inTx(t, db, func(tx *sql.Tx) error {
		claim, err := issues.PopCandidate(tx, agent)
		if err != nil {
			return err
		}
		if claim == nil || claim.ID != deepest.ID {
			return fmt.Errorf("diamond graph: expected deepest %s, got %+v", deepest.ID, claim)
		}
		return nil
	})
}

// TestPopMixedBranchesPoisonCandidate pins that ONE unworkable blocker
// branch (externally blocked deeper down) makes the whole candidate
// unworkable — the pop falls back to older workable work instead of
// redirecting into a partially-blocked chain.
func TestPopMixedBranchesPoisonCandidate(t *testing.T) {
	db := openDB(t)
	agent := "00000000-0000-7000-8000-00000000000a"
	other := "00000000-0000-7000-8000-00000000000b"
	var fallback issues.Issue
	inTx(t, db, func(tx *sql.Tx) error {
		project, err := projects.Create(tx, projects.New{Key: "SUT", Name: "S"})
		if err != nil {
			return err
		}
		mk := func(title string, assignee string) issues.Issue {
			a := assignee
			i, err := issues.Create(tx, project.ID, title, nil, &a)
			if err != nil {
				t.Fatalf("create %s: %v", title, err)
			}
			return i
		}
		// candidate <- blockedMine (ours, open) <- external (other's):
		// the walk into blockedMine hits the external block and must
		// poison the candidate rather than claim anything through it;
		// blockedMine as its own candidate is directly external-blocked
		// too, so the pop falls through to the later fallback.
		candidate := mk("candidate", agent)
		blockedMine := mk("mine but blocked deeper", agent)
		external := mk("external deep blocker", other)
		for _, rel := range [][2]string{
			{blockedMine.ID, candidate.ID},
			{external.ID, blockedMine.ID},
		} {
			if _, err := issues.AddRelation(tx, "blocks", rel[0], rel[1]); err != nil {
				return err
			}
		}
		fallback = mk("older workable fallback", agent)
		return nil
	})
	inTx(t, db, func(tx *sql.Tx) error {
		claim, err := issues.PopCandidate(tx, agent)
		if err != nil {
			return err
		}
		if claim == nil {
			return fmt.Errorf("expected the fallback claimable")
		}
		if claim.ID != fallback.ID {
			return fmt.Errorf("poisoned chain must fall through: got %s, want fallback %s", claim.ID, fallback.ID)
		}
		return nil
	})
}
