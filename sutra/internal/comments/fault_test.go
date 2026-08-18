package comments_test

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"sutra/internal/comments"
	_ "sutra/internal/faultsql"
)

// internal/comments had no tests of its own. Two groups here: the error
// branches that need a failing database, and the anchor-matching arms that
// need only a reply whose parent sits somewhere else — the suite had never
// built one, so every comparison had gone one way.

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

func ptr(s string) *string { return &s }

// seeded migrates, inserts one comment on an issue, and returns a second
// armed handle on the same shared cache plus the comment's id.
func seeded(t *testing.T, fault string) (*sql.DB, string) {
	t.Helper()
	setup := handle(t, "", "")
	if err := comments.Migrate(setup); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	tx, err := setup.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	c, err := comments.Create(tx, comments.New{
		Issue:  ptr("11111111-1111-7111-8111-111111111111"),
		Author: "22222222-2222-7222-8222-222222222222", Body: "seeded",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return handle(t, "", fault), c.ID
}

// comments.go:78 — Migrate's `err != nil`.
func TestMigrateReportsAFailedExec(t *testing.T) {
	h := handle(t, "", "fault_op=exec&fault_after=1")
	err := comments.Migrate(h)
	if err == nil {
		t.Fatal("Migrate reported success on a failing exec")
	}
	if !strings.Contains(err.Error(), "migrate comments") {
		t.Fatalf("error lost its context: %v", err)
	}
}

// comments.go:110 and :113 — anchorOf's absent-versus-broken pair, reached
// through Create with a parent. A reply to a comment that cannot be READ
// must not be stored as if its parent matched.
func TestReplyToAnUnreadableParentIsRefused(t *testing.T) {
	t.Run("parent absent", func(t *testing.T) {
		armed, _ := seeded(t, "")
		tx, err := armed.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		_, err = comments.Create(tx, comments.New{
			Issue:  ptr("11111111-1111-7111-8111-111111111111"),
			Parent: ptr("99999999-9999-7999-8999-999999999999"),
			Author: "22222222-2222-7222-8222-222222222222", Body: "orphan",
		})
		var nf *comments.NotFoundError
		if !errors.As(err, &nf) {
			t.Fatalf("expected NotFoundError for an absent parent, got: %v", err)
		}
	})
	t.Run("parent unreadable", func(t *testing.T) {
		armed, parent := seeded(t, "fault_op=query&fault_after=1")
		tx, err := armed.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		_, err = comments.Create(tx, comments.New{
			Issue: ptr("11111111-1111-7111-8111-111111111111"), Parent: &parent,
			Author: "22222222-2222-7222-8222-222222222222", Body: "reply",
		})
		if err == nil {
			t.Fatal("Create reported success when the parent could not be read")
		}
		var nf *comments.NotFoundError
		if errors.As(err, &nf) {
			t.Fatal("a query failure must not be reported as an absent parent")
		}
		if !strings.Contains(err.Error(), "comment anchor") {
			t.Fatalf("error lost its context: %v", err)
		}
	})
}

// comments.go:146 — Create's `err != nil` on the insert.
func TestCreateReportsAFailedInsert(t *testing.T) {
	setup := handle(t, "", "")
	if err := comments.Migrate(setup); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	armed := handle(t, "", "fault_op=exec&fault_after=1")
	tx, err := armed.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	_, err = comments.Create(tx, comments.New{
		Issue:  ptr("11111111-1111-7111-8111-111111111111"),
		Author: "22222222-2222-7222-8222-222222222222", Body: "doomed",
	})
	if err == nil {
		t.Fatal("Create reported success on a failing insert")
	}
	if !strings.Contains(err.Error(), "insert comment") {
		t.Fatalf("error lost its context: %v", err)
	}
}

// comments.go:130, :131, :153, :156, :160, :163 — the anchor-matching arms.
// A reply must share its parent's anchor target, and the suite had only ever
// built replies that did, so every comparison had gone one way. Each case
// below differs from the parent in exactly one field, which is what makes it
// a test of THAT comparison rather than of the conjunction.
func TestReplyMustShareItsParentsAnchor(t *testing.T) {
	issue := "11111111-1111-7111-8111-111111111111"
	other := "33333333-3333-7333-8333-333333333333"
	author := "22222222-2222-7222-8222-222222222222"
	rev := int64(1)
	otherRev := int64(2)

	// A parent on each anchor kind, so a mismatching reply can be aimed at
	// the right comparison.
	setup := handle(t, "", "")
	if err := comments.Migrate(setup); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	tx, err := setup.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	onIssue, err := comments.Create(tx, comments.New{Issue: &issue, Author: author, Body: "p"})
	if err != nil {
		t.Fatalf("create issue parent: %v", err)
	}
	onDoc, err := comments.Create(tx, comments.New{DocVersion: &issue, Author: author, Body: "p"})
	if err != nil {
		t.Fatalf("create doc parent: %v", err)
	}
	onReview, err := comments.Create(tx, comments.New{
		Review: &issue, ReviewRevision: &rev, Author: author, Body: "p"})
	if err != nil {
		t.Fatalf("create review parent: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	cases := []struct {
		name  string
		reply comments.New
	}{
		{"a different issue", comments.New{Issue: &other, Parent: &onIssue.ID, Author: author, Body: "r"}},
		{"no issue at all", comments.New{DocVersion: &issue, Parent: &onIssue.ID, Author: author, Body: "r"}},
		{"a different doc version", comments.New{DocVersion: &other, Parent: &onDoc.ID, Author: author, Body: "r"}},
		{"a different review", comments.New{Review: &other, ReviewRevision: &rev, Parent: &onReview.ID, Author: author, Body: "r"}},
		{"a different review revision", comments.New{Review: &issue, ReviewRevision: &otherRev, Parent: &onReview.ID, Author: author, Body: "r"}},
		{"no revision where the parent has one", comments.New{Review: &issue, Parent: &onReview.ID, Author: author, Body: "r"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := setup.Begin()
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer func() { _ = tx.Rollback() }()
			_, err = comments.Create(tx, tc.reply)
			var anchorErr *comments.ParentAnchorError
			if !errors.As(err, &anchorErr) {
				t.Fatalf("expected ParentAnchorError, got: %v", err)
			}
		})
	}
}

// comments.go:171 — EachByAnchor's `column == "review"`, the last arm of the
// allowed-column switch, plus the refusal for anything outside it. The
// column is interpolated into SQL, so the switch is the injection guard and
// its default is not decoration.
func TestEachByAnchorAcceptsOnlyKnownColumns(t *testing.T) {
	armed, _ := seeded(t, "")
	tx, err := armed.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, column := range []string{"issue", "doc_version", "review"} {
		if err := comments.EachByAnchor(tx, column, "x", func(comments.Comment) error { return nil }); err != nil {
			t.Fatalf("column %q was refused: %v", column, err)
		}
	}
	err = comments.EachByAnchor(tx, "body; DROP TABLE comments", "x",
		func(comments.Comment) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "unknown anchor column") {
		t.Fatalf("an unknown column must be refused, got: %v", err)
	}
}

// comments.go:178, :184, :187 — EachByAnchor's three failure arms.
func TestEachByAnchorHandlesEveryFailure(t *testing.T) {
	issue := "11111111-1111-7111-8111-111111111111"

	t.Run("failed query", func(t *testing.T) {
		armed, _ := seeded(t, "fault_op=query&fault_after=1")
		tx, err := armed.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		err = comments.EachByAnchor(tx, "issue", issue, func(comments.Comment) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "stream comments by issue") {
			t.Fatalf("expected the query failure, got: %v", err)
		}
	})

	t.Run("unscannable row", func(t *testing.T) {
		armed, _ := seeded(t, "fault_op=badrow&fault_after=1")
		tx, err := armed.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		err = comments.EachByAnchor(tx, "issue", issue, func(comments.Comment) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "scan comment") {
			t.Fatalf("expected the scan failure, got: %v", err)
		}
	})

	t.Run("callback stops the stream", func(t *testing.T) {
		armed, _ := seeded(t, "")
		tx, err := armed.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		sentinel := errors.New("caller stopped")
		err = comments.EachByAnchor(tx, "issue", issue, func(comments.Comment) error { return sentinel })
		if !errors.Is(err, sentinel) {
			t.Fatalf("the callback's error must reach the caller unwrapped, got: %v", err)
		}
	})
}
