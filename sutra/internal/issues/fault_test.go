package issues_test

import (
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	_ "sutra/internal/faultsql"
	"sutra/internal/issues"
)

// issues has seventy-eight uncovered arms, more than any other store
// package, and Get is the reason: it reads the issue row AND then its
// labels, so every function that calls Get spends two queries before its
// own work begins. Update, Assign and AddRelation each call it too, and
// AddRelation calls it twice. So the ordinals here are arithmetic on that
// structure, and every case asserts the error's context string — the only
// way to tell which of several identical-looking reads answered.

func handle(t *testing.T, suffix, fault string) *sql.DB {
	t.Helper()
	dsn := "file:" + t.Name() + suffix + "?mode=memory&cache=shared&_pragma=busy_timeout(5000)"
	if fault != "" {
		dsn += "&" + fault
	}
	h, err := sql.Open("sqlite-fault", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

const (
	projectID = "11111111-1111-7111-8111-111111111111"
	absentID  = "99999999-9999-7999-8999-999999999999"
	whoever   = "22222222-2222-7222-8222-222222222222"
)

type fixture struct{ a, b, c, label, relation string }

// seeds numbers each database so two seed calls in one subtest do not share
// a shared-cache handle — the second would re-run the fixtures and trip
// their own unique constraints.
var seeds atomic.Int64

// seed migrates and builds two open issues plus one label. The projects
// table is created with raw SQL rather than by importing the projects
// package: PopCandidate joins it, and MOD-issues imports no siblings.
func seed(t *testing.T, fault string) (*sql.DB, fixture) {
	return seedWith(t, fault, nil)
}

// seedWith is seed plus a hook that runs against the unarmed handle, for
// state the fixtures cannot express through the package's own API.
func seedWith(t *testing.T, fault string, prepare func(*sql.DB, fixture)) (*sql.DB, fixture) {
	t.Helper()
	suffix := "-" + strconv.FormatInt(seeds.Add(1), 10)
	setup := handle(t, suffix, "")
	if err := issues.Migrate(setup); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := setup.Exec(`
		CREATE TABLE IF NOT EXISTS projects (id TEXT PRIMARY KEY, archived_at TEXT);
		INSERT OR IGNORE INTO projects (id, archived_at) VALUES (?, NULL)`, projectID); err != nil {
		t.Fatalf("projects table: %v", err)
	}
	tx, err := setup.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	a, err := issues.Create(tx, projectID, "first", nil, nil)
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	b, err := issues.Create(tx, projectID, "second", nil, nil)
	if err != nil {
		t.Fatalf("create b: %v", err)
	}
	// A third issue with no relations of its own, so a parent_of can be
	// attempted without the duplicate check answering first.
	c, err := issues.Create(tx, projectID, "third", nil, nil)
	if err != nil {
		t.Fatalf("create c: %v", err)
	}
	label, err := issues.CreateLabel(tx, "seeded", nil)
	if err != nil {
		t.Fatalf("create label: %v", err)
	}
	if err := issues.AttachLabel(tx, a.ID, label.ID); err != nil {
		t.Fatalf("attach: %v", err)
	}
	// a parents b. Without a relation the traversal queries return no rows,
	// and a fault that corrupts a row has nothing to corrupt — the test
	// would pass for want of data rather than for the reason it claims.
	relation, err := issues.AddRelation(tx, "parent_of", a.ID, b.ID)
	if err != nil {
		t.Fatalf("relate: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	fx := fixture{a: a.ID, b: b.ID, c: c.ID, label: label.ID, relation: relation.ID}
	if prepare != nil {
		prepare(setup, fx)
	}
	return handle(t, suffix, fault), fx
}

func faultTx(t *testing.T, h *sql.DB, fn func(*sql.Tx) error) error {
	t.Helper()
	tx, err := h.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	return fn(tx)
}

// call runs one operation against an armed handle and requires the error to
// carry the given context.
func call(t *testing.T, fault, want string, fn func(*sql.Tx, fixture) error) {
	t.Helper()
	h, fx := seed(t, fault)
	err := faultTx(t, h, func(tx *sql.Tx) error { return fn(tx, fx) })
	if err == nil {
		t.Fatalf("expected a failure carrying %q, got success", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("expected an error containing %q, got: %v", want, err)
	}
}

func TestMigrationsReportFailedExecs(t *testing.T) {
	// Migrate runs the issue tables and then the label tables, so the
	// second exec is the labels migration — a separate function with its
	// own error context.
	t.Run("issues", func(t *testing.T) {
		h := handle(t, "", "fault_op=exec&fault_after=1")
		err := issues.Migrate(h)
		if err == nil || !strings.Contains(err.Error(), "migrate issues") {
			t.Fatalf("expected the issues migration failure, got: %v", err)
		}
	})
	t.Run("labels", func(t *testing.T) {
		h := handle(t, "", "fault_op=exec&fault_after=2")
		err := issues.Migrate(h)
		if err == nil || !strings.Contains(err.Error(), "migrate labels") {
			t.Fatalf("expected the labels migration failure, got: %v", err)
		}
	})
}

// Create mints its number with a RETURNING query and then inserts, so its
// two arms need different fault kinds.
func TestCreateReportsBothSteps(t *testing.T) {
	t.Run("number minting", func(t *testing.T) {
		call(t, "fault_op=query&fault_after=1", "mint number for",
			func(tx *sql.Tx, _ fixture) error { _, err := issues.Create(tx, projectID, "t", nil, nil); return err })
	})
	t.Run("the insert", func(t *testing.T) {
		call(t, "fault_op=exec&fault_after=1", "insert issue",
			func(tx *sql.Tx, _ fixture) error { _, err := issues.Create(tx, projectID, "t", nil, nil); return err })
	})
}

// Get reads the issue and then its labels; a failure in either must not
// read as a missing issue.
func TestGetHandlesEveryFailure(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		h, _ := seed(t, "")
		err := faultTx(t, h, func(tx *sql.Tx) error { _, err := issues.Get(tx, absentID); return err })
		var nf *issues.NotFoundError
		if !errors.As(err, &nf) {
			t.Fatalf("expected NotFoundError, got: %v", err)
		}
	})
	t.Run("issue row unreadable", func(t *testing.T) {
		h, fx := seed(t, "fault_op=query&fault_after=1")
		err := faultTx(t, h, func(tx *sql.Tx) error { _, err := issues.Get(tx, fx.a); return err })
		var nf *issues.NotFoundError
		if errors.As(err, &nf) {
			t.Fatal("a query failure must not be reported as not-found")
		}
		if err == nil || !strings.Contains(err.Error(), "get issue") {
			t.Fatalf("expected the read failure, got: %v", err)
		}
	})
	t.Run("labels unreadable", func(t *testing.T) {
		call(t, "fault_op=query&fault_after=2", "labels of",
			func(tx *sql.Tx, fx fixture) error { _, err := issues.Get(tx, fx.a); return err })
	})
}

func TestRefByIDSeparatesAbsenceFromFailure(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		h, _ := seed(t, "")
		err := faultTx(t, h, func(tx *sql.Tx) error { _, err := issues.RefByID(tx, absentID); return err })
		var nf *issues.NotFoundError
		if !errors.As(err, &nf) {
			t.Fatalf("expected NotFoundError, got: %v", err)
		}
	})
	t.Run("failed query", func(t *testing.T) {
		call(t, "fault_op=query&fault_after=1", "issue ref",
			func(tx *sql.Tx, fx fixture) error { _, err := issues.RefByID(tx, fx.a); return err })
	})
}

// The listing and traversal helpers, each with its query and scan arms.
func TestListingsAndTraversalsReportTheirFailures(t *testing.T) {
	cases := []struct {
		name, queryCtx, scanCtx string
		run                     func(*sql.Tx, fixture) error
	}{
		{"SearchIDs", "search issue ids", "scan issue ref",
			func(tx *sql.Tx, _ fixture) error {
				_, err := issues.SearchIDs(tx, projectID, issues.Filters{})
				return err
			}},
		{"Ancestors", "ancestors of", "scan ancestor",
			func(tx *sql.Tx, fx fixture) error { _, err := issues.Ancestors(tx, fx.b); return err }},
		{"ActiveDescendants", "active-descendant check for", "scan active descendant",
			func(tx *sql.Tx, fx fixture) error { _, err := issues.ActiveDescendants(tx, fx.a); return err }},
		{"Relations", "relations of", "scan relation",
			func(tx *sql.Tx, fx fixture) error { _, err := issues.Relations(tx, fx.a); return err }},
		{"LabelIDsForProject", "label ids for", "scan label id",
			func(tx *sql.Tx, _ fixture) error { _, err := issues.LabelIDsForProject(tx, projectID); return err }},
		{"LabelsOf", "labels of", "scan label",
			func(tx *sql.Tx, fx fixture) error { _, err := issues.LabelsOf(tx, fx.a); return err }},
	}
	for _, tc := range cases {
		t.Run(tc.name+"/failed query", func(t *testing.T) {
			call(t, "fault_op=query&fault_after=1", tc.queryCtx, tc.run)
		})
		t.Run(tc.name+"/unscannable row", func(t *testing.T) {
			call(t, "fault_op=badrow&fault_after=1", tc.scanCtx, tc.run)
		})
	}
}

// ActiveInSubtree answers a boolean, so its absent case is `false, nil` and
// only a real failure is an error — conflating them would let the close gate
// pass on a broken database.
func TestActiveInSubtreeSeparatesEmptinessFromFailure(t *testing.T) {
	t.Run("no active descendants", func(t *testing.T) {
		h, fx := seed(t, "")
		err := faultTx(t, h, func(tx *sql.Tx) error {
			active, err := issues.ActiveInSubtree(tx, fx.b)
			if err != nil {
				return err
			}
			if active {
				t.Fatal("a childless issue has no active descendants")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("failed query", func(t *testing.T) {
		call(t, "fault_op=query&fault_after=1", "active-descendant check for",
			func(tx *sql.Tx, fx fixture) error { _, err := issues.ActiveInSubtree(tx, fx.a); return err })
	})
}

// ListEach collects ids and then loads each issue, so its failures split
// across the id pass and the per-id Get.
func TestListEachHandlesEveryFailure(t *testing.T) {
	t.Run("failed id query", func(t *testing.T) {
		call(t, "fault_op=query&fault_after=1", "list issues",
			func(tx *sql.Tx, _ fixture) error {
				return issues.ListEach(tx, projectID, issues.Filters{}, func(issues.Issue) error { return nil })
			})
	})
	t.Run("unscannable id", func(t *testing.T) {
		call(t, "fault_op=badrow&fault_after=1", "scan issue id",
			func(tx *sql.Tx, _ fixture) error {
				return issues.ListEach(tx, projectID, issues.Filters{}, func(issues.Issue) error { return nil })
			})
	})
	t.Run("id cursor breaks", func(t *testing.T) {
		call(t, "fault_op=next&fault_after=1", "iterate issues",
			func(tx *sql.Tx, _ fixture) error {
				return issues.ListEach(tx, projectID, issues.Filters{}, func(issues.Issue) error { return nil })
			})
	})
	t.Run("the per-id Get fails", func(t *testing.T) {
		call(t, "fault_op=query&fault_after=2", "get issue",
			func(tx *sql.Tx, _ fixture) error {
				return issues.ListEach(tx, projectID, issues.Filters{}, func(issues.Issue) error { return nil })
			})
	})
	t.Run("callback stops the stream", func(t *testing.T) {
		h, _ := seed(t, "")
		sentinel := errors.New("caller stopped")
		err := faultTx(t, h, func(tx *sql.Tx) error {
			return issues.ListEach(tx, projectID, issues.Filters{}, func(issues.Issue) error { return sentinel })
		})
		if !errors.Is(err, sentinel) {
			t.Fatalf("the callback's error must reach the caller unwrapped, got: %v", err)
		}
	})
}

// Update and Assign both open with Get and then write, so each has an
// opening-read arm and a write arm.
func TestUpdateAndAssignReportTheirFailures(t *testing.T) {
	title := "renamed"
	assignee := whoever
	for _, tc := range []struct {
		name, fault, want string
		run               func(*sql.Tx, fixture) error
	}{
		{"Update's opening read", "fault_op=query&fault_after=1", "get issue",
			func(tx *sql.Tx, fx fixture) error { _, err := issues.Update(tx, fx.a, &title, nil); return err }},
		{"Update's write", "fault_op=exec&fault_after=1", "update issue",
			func(tx *sql.Tx, fx fixture) error { _, err := issues.Update(tx, fx.a, &title, nil); return err }},
		{"Assign's opening read", "fault_op=query&fault_after=1", "get issue",
			func(tx *sql.Tx, fx fixture) error { _, err := issues.Assign(tx, fx.a, &assignee); return err }},
		{"Assign's write", "fault_op=exec&fault_after=1", "assign issue",
			func(tx *sql.Tx, fx fixture) error { _, err := issues.Assign(tx, fx.a, &assignee); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) { call(t, tc.fault, tc.want, tc.run) })
	}
}

// SetStatus answers not-found from a zero row count, and that is the whole
// of its existence check — so both halves of the condition matter.
func TestSetStatusReportsNotFoundForBothHalves(t *testing.T) {
	t.Run("failed write", func(t *testing.T) {
		call(t, "fault_op=exec&fault_after=1", "set status of",
			func(tx *sql.Tx, fx fixture) error { return issues.SetStatus(tx, fx.a, "complete") })
	})
	t.Run("no such issue", func(t *testing.T) {
		h, _ := seed(t, "")
		err := faultTx(t, h, func(tx *sql.Tx) error { return issues.SetStatus(tx, absentID, "complete") })
		var nf *issues.NotFoundError
		if !errors.As(err, &nf) {
			t.Fatalf("expected NotFoundError, got: %v", err)
		}
	})
	t.Run("an unreadable row count is not an absent issue", func(t *testing.T) {
		// Corrected per review 2104. DetachLabel and projects.Archive
		// already drew this line; these sites did not.
		h, fx := seed(t, "fault_op=rowsaffected&fault_after=1")
		err := faultTx(t, h, func(tx *sql.Tx) error { return issues.SetStatus(tx, fx.a, "complete") })
		var nf *issues.NotFoundError
		if errors.As(err, &nf) {
			t.Fatalf("an unknown row count must not settle as not-found: %v", err)
		}
		if err == nil || !strings.Contains(err.Error(), "row count unavailable") {
			t.Fatalf("expected the row-count failure, got: %v", err)
		}
	})
}

// BumpSubtree's write, which carries the subtree revision every in-flight
// claim is fenced against.
func TestBumpSubtreeReportsAFailedWrite(t *testing.T) {
	call(t, "fault_op=exec&fault_after=1", "bump subtree_revision on",
		func(tx *sql.Tx, fx fixture) error { return issues.BumpSubtree(tx, []string{fx.a}) })
}

// AddRelation performs, in order: Get(from) — two queries — Get(to) — two
// more — a duplicate check, a cycle check, a parent check for parent_of,
// then the insert. Each arm below names its ordinal off that sequence.
func TestAddRelationReportsEveryFailure(t *testing.T) {
	for _, tc := range []struct {
		name, fault, want string
		kind              string
	}{
		{"the from-side read", "fault_op=query&fault_after=1", "get issue", "blocks"},
		{"the to-side read", "fault_op=query&fault_after=3", "get issue", "blocks"},
		{"the duplicate check", "fault_op=query&fault_after=5", "check relation", "blocks"},
		{"the cycle check", "fault_op=query&fault_after=6", "cycle check", "blocks"},
		{"the parent check", "fault_op=query&fault_after=7", "parent check", "parent_of"},
		{"the insert", "fault_op=exec&fault_after=1", "insert relation", "blocks"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			call(t, tc.fault, tc.want, func(tx *sql.Tx, fx fixture) error {
				// b -> c: neither an existing relation nor a cycle, so
				// every check runs and the ordinal selects which fails.
				_, err := issues.AddRelation(tx, tc.kind, fx.b, fx.c)
				return err
			})
		})
	}

	// The insert's UNIQUE-violation branch answers "someone created this
	// relation concurrently" rather than reporting a failure. SQLite would
	// produce it only in a real race, so the message is injected.
	t.Run("a concurrent duplicate", func(t *testing.T) {
		h, fx := seed(t, "fault_op=exec&fault_after=1&fault_msg=UNIQUE+constraint+failed:+issue_relations.kind")
		err := faultTx(t, h, func(tx *sql.Tx) error {
			_, err := issues.AddRelation(tx, "blocks", fx.b, fx.c)
			return err
		})
		var exists *issues.RelationExistsError
		if !errors.As(err, &exists) {
			t.Fatalf("expected RelationExistsError, got: %v", err)
		}
		if exists.Existing != "concurrent" {
			t.Fatalf("a raced insert names no existing id, got %q", exists.Existing)
		}
	})
}

// RemoveRelation reads the relation for its event payload and then deletes.
func TestRemoveRelationReportsItsFailures(t *testing.T) {
	t.Run("absent relation", func(t *testing.T) {
		h, _ := seed(t, "")
		err := faultTx(t, h, func(tx *sql.Tx) error { _, err := issues.RemoveRelation(tx, absentID); return err })
		var nf *issues.RelationNotFoundError
		if !errors.As(err, &nf) {
			t.Fatalf("expected RelationNotFoundError, got: %v", err)
		}
	})
	t.Run("failed read", func(t *testing.T) {
		call(t, "fault_op=query&fault_after=1", "get relation",
			func(tx *sql.Tx, _ fixture) error { _, err := issues.RemoveRelation(tx, absentID); return err })
	})
	t.Run("failed delete", func(t *testing.T) {
		// The read must land and only the DELETE fail, so the fault is an
		// exec — and the relation is the one the fixtures committed.
		h, fx := seed(t, "fault_op=exec&fault_after=1")
		err := faultTx(t, h, func(tx *sql.Tx) error { _, err := issues.RemoveRelation(tx, fx.relation); return err })
		if err == nil || !strings.Contains(err.Error(), "delete relation") {
			t.Fatalf("expected the delete failure, got: %v", err)
		}
	})
}

// The label catalog: create, its duplicate resolution, the cursor, and the
// attach/detach pair.
func TestLabelOperationsReportTheirFailures(t *testing.T) {
	t.Run("CreateLabel's non-duplicate failure", func(t *testing.T) {
		h, _ := seed(t, "fault_op=exec&fault_after=1")
		err := faultTx(t, h, func(tx *sql.Tx) error { _, err := issues.CreateLabel(tx, "fresh", nil); return err })
		var dup *issues.DuplicateLabelError
		if errors.As(err, &dup) {
			t.Fatal("a non-duplicate failure took the duplicate path")
		}
		if err == nil || !strings.Contains(err.Error(), "insert label") {
			t.Fatalf("expected the insert failure, got: %v", err)
		}
	})
	t.Run("CreateLabel's duplicate resolution fails", func(t *testing.T) {
		h, _ := seed(t, "fault_op=query&fault_after=1")
		err := faultTx(t, h, func(tx *sql.Tx) error { _, err := issues.CreateLabel(tx, "seeded", nil); return err })
		var dup *issues.DuplicateLabelError
		if errors.As(err, &dup) {
			t.Fatal("an unresolved duplicate must not be reported as a resolved one")
		}
		if err == nil || !strings.Contains(err.Error(), "resolve duplicate label") {
			t.Fatalf("expected the resolution failure, got: %v", err)
		}
	})
	t.Run("OpenLabels fails", func(t *testing.T) {
		call(t, "fault_op=query&fault_after=1", "list labels",
			func(tx *sql.Tx, _ fixture) error { _, err := issues.OpenLabels(tx); return err })
	})
	t.Run("the label cursor's row will not scan", func(t *testing.T) {
		h, _ := seed(t, "fault_op=badrow&fault_after=1")
		err := faultTx(t, h, func(tx *sql.Tx) error {
			cursor, err := issues.OpenLabels(tx)
			if err != nil {
				return err
			}
			defer func() { _ = cursor.Close() }()
			return cursor.Each(func(issues.Label) error { return nil })
		})
		if err == nil || !strings.Contains(err.Error(), "scan label") {
			t.Fatalf("expected the scan failure, got: %v", err)
		}
	})
	t.Run("the label cursor's caller stops", func(t *testing.T) {
		h, _ := seed(t, "")
		sentinel := errors.New("caller stopped")
		err := faultTx(t, h, func(tx *sql.Tx) error {
			cursor, err := issues.OpenLabels(tx)
			if err != nil {
				return err
			}
			defer func() { _ = cursor.Close() }()
			return cursor.Each(func(issues.Label) error { return sentinel })
		})
		if !errors.Is(err, sentinel) {
			t.Fatalf("the callback's error must reach the caller unwrapped, got: %v", err)
		}
	})
	t.Run("GetLabel's pair", func(t *testing.T) {
		h, _ := seed(t, "")
		err := faultTx(t, h, func(tx *sql.Tx) error { _, err := issues.GetLabel(tx, absentID); return err })
		var nf *issues.LabelNotFoundError
		if !errors.As(err, &nf) {
			t.Fatalf("expected LabelNotFoundError, got: %v", err)
		}
		call(t, "fault_op=query&fault_after=1", "get label",
			func(tx *sql.Tx, fx fixture) error { _, err := issues.GetLabel(tx, fx.label); return err })
	})
	t.Run("AttachLabel's read and write", func(t *testing.T) {
		call(t, "fault_op=query&fault_after=1", "get label",
			func(tx *sql.Tx, fx fixture) error { return issues.AttachLabel(tx, fx.b, fx.label) })
		call(t, "fault_op=exec&fault_after=1", "attach label",
			func(tx *sql.Tx, fx fixture) error { return issues.AttachLabel(tx, fx.b, fx.label) })
	})
	t.Run("AttachLabel refuses a label already carried", func(t *testing.T) {
		h, fx := seed(t, "")
		err := faultTx(t, h, func(tx *sql.Tx) error { return issues.AttachLabel(tx, fx.a, fx.label) })
		var attached *issues.LabelAttachedError
		if !errors.As(err, &attached) {
			t.Fatalf("expected LabelAttachedError, got: %v", err)
		}
	})
	t.Run("DetachLabel's three", func(t *testing.T) {
		call(t, "fault_op=exec&fault_after=1", "detach label",
			func(tx *sql.Tx, fx fixture) error { return issues.DetachLabel(tx, fx.a, fx.label) })

		h, fx := seed(t, "")
		err := faultTx(t, h, func(tx *sql.Tx) error { return issues.DetachLabel(tx, fx.b, fx.label) })
		var notAttached *issues.LabelNotAttachedError
		if !errors.As(err, &notAttached) {
			t.Fatalf("expected LabelNotAttachedError, got: %v", err)
		}

		armed, fx2 := seed(t, "fault_op=rowsaffected&fault_after=1")
		err = faultTx(t, armed, func(tx *sql.Tx) error { return issues.DetachLabel(tx, fx2.a, fx2.label) })
		if err == nil || !strings.Contains(err.Error(), "detach label") {
			t.Fatalf("expected the row-count failure, got: %v", err)
		}
	})
}

// CompleteAncestors walks the chain and then reads each ancestor's status.
func TestCompleteAncestorsReportsItsFailures(t *testing.T) {
	call(t, "fault_op=query&fault_after=1", "ancestors of",
		func(tx *sql.Tx, fx fixture) error { _, err := issues.CompleteAncestors(tx, fx.b); return err })
}

// PopCandidate and the blocker walk beneath it. The walk is where the pop
// protocol's correctness lives, so its failure arms matter as much as its
// happy path: a broken read must never be mistaken for "nothing workable",
// which would silently idle an agent.
func TestPopCandidateReportsEveryFailure(t *testing.T) {
	// assigned puts one issue on the identity's work stack, then arms the
	// fault. Without an assignment the candidate query returns NO rows, so
	// the scan and iteration arms have nothing to fail on — they would pass
	// for want of data rather than for the reason they claim.
	assigned := func(t *testing.T, fault string) *sql.DB {
		t.Helper()
		h, _ := seedWith(t, fault, func(setup *sql.DB, fx fixture) {
			if _, err := setup.Exec(`UPDATE issues SET assignee = ?, assigned_at = '2026-01-01T00:00:00.000000000Z' WHERE id = ?`,
				whoever, fx.a); err != nil {
				t.Fatalf("assign: %v", err)
			}
		})
		return h
	}
	pop := func(t *testing.T, h *sql.DB, want string) {
		t.Helper()
		err := faultTx(t, h, func(tx *sql.Tx) error { _, err := issues.PopCandidate(tx, whoever); return err })
		if err == nil {
			t.Fatalf("expected a failure carrying %q, got success", want)
		}
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected an error containing %q, got: %v", want, err)
		}
	}

	t.Run("the candidate query", func(t *testing.T) {
		pop(t, assigned(t, "fault_op=query&fault_after=1"), "pop candidates for")
	})
	t.Run("an unscannable candidate", func(t *testing.T) {
		pop(t, assigned(t, "fault_op=badrow&fault_after=1"), "scan candidate")
	})
	t.Run("the candidate cursor breaks", func(t *testing.T) {
		pop(t, assigned(t, "fault_op=next&fault_after=1"), "iterate candidates")
	})
	t.Run("the blocker query", func(t *testing.T) {
		pop(t, assigned(t, "fault_op=query&fault_after=2"), "blockers of")
	})
	t.Run("the claimed issue cannot be loaded", func(t *testing.T) {
		// candidates, blockers, then Get(claim) — the third query.
		pop(t, assigned(t, "fault_op=query&fault_after=3"), "get issue")
	})
}

// --- guard conditions, which need shaped data rather than faults ----------

// issues.go:354, :358 — Update's two optional fields. The suite had only
// ever sent a title, so the body clause and the title-absent arm were both
// unbuilt.
func TestUpdateAcceptsEitherFieldAlone(t *testing.T) {
	h, fx := seed(t, "")
	body := "a body and no title"
	err := faultTx(t, h, func(tx *sql.Tx) error {
		updated, err := issues.Update(tx, fx.a, nil, &body)
		if err != nil {
			return err
		}
		if updated.Body == nil || *updated.Body != body {
			t.Fatalf("the body was not written: %+v", updated.Body)
		}
		if updated.Title != "first" {
			t.Fatalf("the title should not have moved, got %q", updated.Title)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// issues.go:449 — AddRelation's self-relation check, which is a cycle of
// length one and the cheapest one to get wrong.
func TestRelationToItselfIsACycle(t *testing.T) {
	h, fx := seed(t, "")
	err := faultTx(t, h, func(tx *sql.Tx) error {
		_, err := issues.AddRelation(tx, "blocks", fx.a, fx.a)
		return err
	})
	var cycle *issues.CycleError
	if !errors.As(err, &cycle) {
		t.Fatalf("expected CycleError, got: %v", err)
	}
}

// issues.go:541 — BumpSubtree's dedupe. The once-per-transaction guarantee
// of AC-subtree-revision depends on it: a repeated id must bump ONCE, or an
// in-flight claim gets fenced by a revision that moved twice for one change.
func TestBumpSubtreeCountsEachIDOnce(t *testing.T) {
	h, fx := seed(t, "")
	before := subtreeRevision(t, h, fx.a)
	err := faultTx(t, h, func(tx *sql.Tx) error {
		if err := issues.BumpSubtree(tx, []string{fx.a, fx.a, fx.a}); err != nil {
			return err
		}
		return tx.Commit()
	})
	if err != nil {
		t.Fatalf("bump: %v", err)
	}
	if after := subtreeRevision(t, h, fx.a); after != before+1 {
		t.Fatalf("three copies of one id bumped %d times, expected once", after-before)
	}
}

func subtreeRevision(t *testing.T, h *sql.DB, id string) int64 {
	t.Helper()
	var rev int64
	if err := h.QueryRow(`SELECT subtree_revision FROM issues WHERE id = ?`, id).Scan(&rev); err != nil {
		t.Fatalf("read subtree_revision: %v", err)
	}
	return rev
}

// issues.go:757 — CompleteAncestors' per-ancestor status read. The chain
// query lands and the status read fails, so the second query is the target.
func TestCompleteAncestorsReportsAFailedStatusRead(t *testing.T) {
	h, fx := seed(t, "fault_op=query&fault_after=2")
	err := faultTx(t, h, func(tx *sql.Tx) error { _, err := issues.CompleteAncestors(tx, fx.b); return err })
	if err == nil || !strings.Contains(err.Error(), "status of ancestor") {
		t.Fatalf("expected the ancestor status failure, got: %v", err)
	}
}

// blocked shapes a work stack where `a` is assigned and BLOCKED by `c`, so
// the walk actually descends. shape runs last, for the per-test variations.
func blocked(t *testing.T, fault string, shape func(*sql.DB, fixture)) (*sql.DB, fixture) {
	t.Helper()
	return seedWith(t, fault, func(setup *sql.DB, fx fixture) {
		if _, err := setup.Exec(`
			UPDATE issues SET assignee = ?, assigned_at = '2026-01-01T00:00:00.000000000Z'
			WHERE id IN (?, ?)`, whoever, fx.a, fx.c); err != nil {
			t.Fatalf("assign: %v", err)
		}
		// c blocks a. Raw SQL because AddRelation's cycle and parent checks
		// are not what this is testing.
		if _, err := setup.Exec(`
			INSERT INTO issue_relations (id, kind, from_issue, to_issue)
			VALUES ('44444444-4444-7444-8444-444444444444', 'blocks', ?, ?)`, fx.c, fx.a); err != nil {
			t.Fatalf("block: %v", err)
		}
		if shape != nil {
			shape(setup, fx)
		}
	})
}

// issues.go:691, :696, :727 — the blocker query's scan and iteration arms,
// and the recursion's error propagation. A broken read here must never be
// mistaken for "nothing workable", which would silently idle an agent.
func TestBlockerWalkReportsItsFailures(t *testing.T) {
	pop := func(t *testing.T, h *sql.DB, want string) {
		t.Helper()
		err := faultTx(t, h, func(tx *sql.Tx) error { _, err := issues.PopCandidate(tx, whoever); return err })
		if err == nil {
			t.Fatalf("expected a failure carrying %q, got success", want)
		}
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected an error containing %q, got: %v", want, err)
		}
	}
	t.Run("an unscannable blocker", func(t *testing.T) {
		// candidates is query 1; the blocker read is query 2.
		h, _ := blocked(t, "fault_op=badrow&fault_after=2", nil)
		pop(t, h, "scan blocker")
	})
	t.Run("the blocker cursor breaks", func(t *testing.T) {
		// The candidate cursor is drained FIRST — two assigned rows plus
		// the EOF Next make three — so the blocker cursor's first Next is
		// the fourth. Ordinals here are arithmetic on the traversal, not
		// guesses; getting it wrong lands on "iterate candidates" and the
		// context assertion is what catches that.
		h, _ := blocked(t, "fault_op=next&fault_after=4", nil)
		pop(t, h, "iterate blockers")
	})
	t.Run("a deeper blocker's read fails", func(t *testing.T) {
		// The walk recurses into c, whose own blocker query is the third.
		h, _ := blocked(t, "fault_op=query&fault_after=3", nil)
		pop(t, h, "blockers of")
	})
}

// issues.go:706 — the eligibility test's two remaining halves: a blocker
// nobody owns, and one owned but not open. Either makes the whole chain
// unworkable, because its prerequisites cannot all be worked by this
// identity — so an older workable fallback must win instead.
func TestBlockersNotWorkableByThisIdentityPoisonTheChain(t *testing.T) {
	cases := []struct {
		name  string
		shape func(*sql.DB, fixture)
	}{
		{"a blocker nobody owns", func(setup *sql.DB, fx fixture) {
			if _, err := setup.Exec(`UPDATE issues SET assignee = NULL, assigned_at = NULL WHERE id = ?`, fx.c); err != nil {
				panic(err)
			}
		}},
		{"a blocker owned but not open", func(setup *sql.DB, fx fixture) {
			if _, err := setup.Exec(`UPDATE issues SET status = 'in-progress' WHERE id = ?`, fx.c); err != nil {
				panic(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := blocked(t, "", tc.shape)
			err := faultTx(t, h, func(tx *sql.Tx) error {
				claimed, err := issues.PopCandidate(tx, whoever)
				if err != nil {
					return err
				}
				// c is blocking a and is not workable here, so a is not
				// claimable either. Nothing else is assigned, so the pop
				// finds nothing rather than handing out blocked work.
				if claimed != nil {
					t.Fatalf("pop handed out %s despite an unworkable blocker", claimed.ID)
				}
				return nil
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// issues.go:666 — the onPath guard, which the code says is reachable only
// through corrupted data: blocking cycles are refused at creation. So the
// cycle is written with raw SQL, which is precisely the corruption the guard
// exists for. Without it the walk recurses until the stack gives out.
func TestBlockingCycleInCorruptDataTerminates(t *testing.T) {
	h, _ := blocked(t, "", func(setup *sql.DB, fx fixture) {
		// a blocks c, closing the loop against the c-blocks-a edge.
		if _, err := setup.Exec(`
			INSERT INTO issue_relations (id, kind, from_issue, to_issue)
			VALUES ('55555555-5555-7555-8555-555555555555', 'blocks', ?, ?)`, fx.a, fx.c); err != nil {
			panic(err)
		}
	})
	err := faultTx(t, h, func(tx *sql.Tx) error {
		claimed, err := issues.PopCandidate(tx, whoever)
		if err != nil {
			return err
		}
		// The walk must terminate. What it returns matters less than that
		// it returns at all — a cycle it did not notice would not come
		// back to be asserted about.
		_ = claimed
		return nil
	})
	if err != nil {
		t.Fatalf("a corrupted blocking cycle must terminate, not error: %v", err)
	}
}

// issues.go:738 — the poison guard, and two gaps in the pop tests that
// finding it exposed.
//
// First gap: `!poisoned` and `bestID != ""` coincide whenever the poison
// happens on the FIRST branch, because bestID is still empty and either check
// suppresses the claim. They diverge only when an EARLIER branch already
// produced a claim and a LATER one turns out unworkable.
//
// Second gap, and the harder one: even in that shape the two behaviours can
// return the SAME issue. A poisoned candidate's bestID is one of its own
// blockers, and an eligible blocker is by definition open and self-assigned —
// so it is a candidate in its own right and would be claimed next anyway.
// Identical output from different reasoning, which is defect species 3.
//
// What separates them is the property the code comment actually claims: an
// older workable fallback must win instead. So the shape below puts a
// workable issue BETWEEN the poisoned candidate and its blockers in FIFO
// order. Honest walk: skip the poisoned candidate, claim the fallback.
// Poison ignored: claim the blocker instead — a different issue, and the
// wrong one.
func TestAPoisonedCandidateYieldsToTheOlderFallback(t *testing.T) {
	const (
		fallback = "88888888-8888-7888-8888-888888888888"
		unowned  = "66666666-6666-7666-8666-666666666666"
	)
	h, seeded := seedWith(t, "", func(setup *sql.DB, fx fixture) {
		insert := func(id string, number int64, title string) {
			if _, err := setup.Exec(`
				INSERT INTO issues (id, project, number, title, status, subtree_revision, created, updated)
				VALUES (?, ?, ?, ?, 'open', 0,
					'2026-01-01T00:00:00.000000000Z', '2026-01-01T00:00:00.000000000Z')`,
				id, projectID, number, title); err != nil {
				t.Fatalf("insert %s: %v", title, err)
			}
		}
		insert(fallback, 4, "fallback")
		insert(unowned, 5, "unowned")

		// FIFO order is assigned_at, then number. a is oldest, then the
		// fallback, then a's two blockers. unowned belongs to nobody.
		for _, row := range []struct {
			id, at string
		}{
			{fx.a, "2026-01-01T00:00:01.000000000Z"},
			{fallback, "2026-01-01T00:00:02.000000000Z"},
			{fx.b, "2026-01-01T00:00:03.000000000Z"},
			{fx.c, "2026-01-01T00:00:04.000000000Z"},
		} {
			if _, err := setup.Exec(`UPDATE issues SET assignee = ?, assigned_at = ? WHERE id = ?`,
				whoever, row.at, row.id); err != nil {
				t.Fatalf("assign %s: %v", row.id, err)
			}
		}

		// b blocks a and resolves cleanly, so bestID is set first. c blocks
		// a too but is itself blocked by unowned, so it poisons a AFTER
		// bestID already holds b.
		for i, pair := range [][2]string{{fx.b, fx.a}, {fx.c, fx.a}, {unowned, fx.c}} {
			if _, err := setup.Exec(`
				INSERT INTO issue_relations (id, kind, from_issue, to_issue)
				VALUES (?, 'blocks', ?, ?)`,
				"7777777"+strconv.Itoa(i)+"-7777-7777-8777-777777777777", pair[0], pair[1]); err != nil {
				t.Fatalf("block %d: %v", i, err)
			}
		}
	})

	err := faultTx(t, h, func(tx *sql.Tx) error {
		claimed, err := issues.PopCandidate(tx, whoever)
		if err != nil {
			return err
		}
		if claimed == nil {
			t.Fatal("the fallback is workable and should have been claimed")
		}
		switch claimed.ID {
		case fallback:
			return nil // the poisoned candidate yielded, as it must
		case seeded.a:
			t.Fatal("the poisoned candidate itself was claimed")
		case seeded.b:
			t.Fatal("a blocker of the poisoned candidate was claimed ahead of an older workable issue")
		default:
			t.Fatalf("unexpected claim %s", claimed.ID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
