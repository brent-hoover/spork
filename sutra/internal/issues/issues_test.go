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

// TestPopEqualDepthBlockersBreakTowardLowerNumber pins the tie-break the
// deepest-branch rule leaves open. Every other branch test here gives the
// branches DIFFERENT depths, where "keep the deepest" and "keep the last
// one that ties the deepest" pick the same issue; two branches of equal
// depth are the only shape that separates them. The blocker query is
// ordered by display number, so keeping the first of an equal-depth pair
// is what makes the claim deterministic — and, since numbers follow
// creation, hands out the older prerequisite.
func TestPopEqualDepthBlockersBreakTowardLowerNumber(t *testing.T) {
	db := openDB(t)
	agent := "00000000-0000-7000-8000-00000000000a"
	var lower issues.Issue
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
		lower = mk("first blocker")
		higher := mk("second blocker")
		for _, blocker := range []issues.Issue{lower, higher} {
			if _, err := issues.AddRelation(tx, "blocks", blocker.ID, candidate.ID); err != nil {
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
		if claim == nil || claim.ID != lower.ID {
			return fmt.Errorf("equal-depth blockers must break toward %s, got %+v", lower.ID, claim)
		}
		return nil
	})
}

// TestPopDiamondBlockerGraph pins converging DAGs with the shared node
// reached through the SHORT branch first (blocker order is by number,
// so "a shared" sorts before "m mid"). The fallback sits second in
// FIFO: shared-visited traversal that suppresses the long branch turns
// it into a poisoned nil and falls through to the fallback, while
// memoized traversal claims the deepest blocker through the candidate.
func TestPopDiamondBlockerGraph(t *testing.T) {
	db := openDB(t)
	agent := "00000000-0000-7000-8000-00000000000a"
	var fallback, deepest issues.Issue
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
		fallback = mk("fallback assigned second")
		shared := mk("a shared")  // direct blocker — SHORT path, visited first
		mid := mk("m mid")        // long path: candidate <- mid <- shared
		deepest = mk("z deepest") // beneath shared
		for _, rel := range [][2]string{
			{shared.ID, candidate.ID},
			{mid.ID, candidate.ID},
			{shared.ID, mid.ID},
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
		if claim == nil || claim.ID == fallback.ID {
			return fmt.Errorf("shared-visited suppression poisoned the diamond: got %+v, want deepest %s", claim, deepest.ID)
		}
		if claim.ID != deepest.ID {
			return fmt.Errorf("expected deepest %s through the long path, got %s", deepest.ID, claim.ID)
		}
		return nil
	})
}

// TestPopMixedBranchesPoisonCandidate pins that ONE unworkable blocker
// branch poisons the candidate even when ANOTHER branch is fully
// workable. The fallback sits between the candidate and the branch
// nodes in FIFO order: traversal that ignores the poisoned branch
// claims deep branch work through the candidate; correct traversal
// falls through to the fallback first.
func TestPopMixedBranchesPoisonCandidate(t *testing.T) {
	db := openDB(t)
	agent := "00000000-0000-7000-8000-00000000000a"
	other := "00000000-0000-7000-8000-00000000000b"
	var fallback, deepMine issues.Issue
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
		candidate := mk("candidate", agent)
		fallback = mk("fallback assigned second", agent)
		workable := mk("workable branch root", agent)
		deepMine = mk("deep workable leaf", agent)
		poisonRoot := mk("poisoned branch root", agent)
		external := mk("external deep blocker", other)
		for _, rel := range [][2]string{
			{workable.ID, candidate.ID},
			{deepMine.ID, workable.ID},
			{poisonRoot.ID, candidate.ID},
			{external.ID, poisonRoot.ID},
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
		if claim == nil {
			return fmt.Errorf("expected the fallback claimable")
		}
		if claim.ID == deepMine.ID {
			return fmt.Errorf("poisoned candidate leaked its workable branch (claimed %s)", claim.ID)
		}
		if claim.ID != fallback.ID {
			return fmt.Errorf("expected fall-through to %s, got %s", fallback.ID, claim.ID)
		}
		return nil
	})
}
