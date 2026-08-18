package events_test

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"sutra/internal/events"
	_ "sutra/internal/faultsql"
)

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

// seeded migrates and emits a few events of two kinds on two subjects, then
// returns a second handle with the fault armed. The variety matters: the
// kind and subject filters are arms of their own.
func seeded(t *testing.T, fault string) *sql.DB {
	t.Helper()
	setup := handle(t, "", "")
	if err := events.Migrate(setup); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	tx, err := setup.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	op := events.NewOperation()
	for _, e := range []struct{ kind, subject string }{
		{"issue.created", "issue-1"},
		{"issue.closed", "issue-1"},
		{"doc.created", "doc-1"},
	} {
		if _, err := events.Emit(tx, e.kind, e.subject, op, "actor-1", nil); err != nil {
			t.Fatalf("emit: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return handle(t, "", fault)
}

// events.go:68 — Migrate's `err != nil`.
func TestMigrateReportsAFailedExec(t *testing.T) {
	h := handle(t, "", "fault_op=exec&fault_after=1")
	if err := events.Migrate(h); err == nil {
		t.Fatal("Migrate reported success on a failing exec")
	} else if !strings.Contains(err.Error(), "migrate events") {
		t.Fatalf("error lost its context: %v", err)
	}
}

// events.go:89 — Emit's `err != nil`. An event that was not recorded must
// not come back with an id, because every write-ahead protocol here treats
// a returned id as proof the row exists.
func TestEmitReportsAFailedInsert(t *testing.T) {
	setup := handle(t, "", "")
	if err := events.Migrate(setup); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	h := handle(t, "", "fault_op=exec&fault_after=1")
	tx, err := h.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	id, err := events.Emit(tx, "issue.created", "issue-9", events.NewOperation(), "actor-1", nil)
	if err == nil {
		t.Fatal("Emit reported success on a failing insert")
	}
	if id != "" {
		t.Fatalf("Emit returned an id alongside an error: %q", id)
	}
	if !strings.Contains(err.Error(), "emit issue.created for issue-9") {
		t.Fatalf("error lost its context: %v", err)
	}
}

// events.go:101 — Watermark's `err != nil`. A watermark is a promise about
// what the caller has seen; a failed capture must not read as position zero.
func TestWatermarkReportsAFailedRead(t *testing.T) {
	h := seeded(t, "fault_op=query&fault_after=1")
	tx, err := h.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	mark, err := events.Watermark(tx)
	if err == nil {
		t.Fatal("Watermark reported success on a failing read")
	}
	if mark != "" {
		t.Fatalf("Watermark returned a position alongside an error: %q", mark)
	}
	if !strings.Contains(err.Error(), "capture watermark") {
		t.Fatalf("error lost its context: %v", err)
	}
}

// events.go:139 — list's `err != nil` reading the feed head, which happens
// before any page is built. A head it could not read must not become a
// silent zero, because a cursor is validated against it.
func TestListReportsAFailedHeadRead(t *testing.T) {
	h := seeded(t, "fault_op=query&fault_after=1")
	_, err := events.List(h, "", "", "", 10, "")
	if err == nil {
		t.Fatal("List reported success when the feed head could not be read")
	}
	if !strings.Contains(err.Error(), "read feed head") {
		t.Fatalf("error lost its context: %v", err)
	}
}

// events.go:142 — `after > head.Int64`: a cursor past the head. The feed
// never issued it, and honoring it would skip every later event forever.
func TestCursorBeyondTheHeadIsRefused(t *testing.T) {
	h := seeded(t, "")
	_, err := events.List(h, "999999", "", "", 10, "")
	var bad *events.BadCursorError
	if !errors.As(err, &bad) {
		t.Fatalf("expected BadCursorError for a cursor past the head, got: %v", err)
	}
}

// events.go:169 — list's `err != nil` on the page query itself, which is
// the SECOND query: the head read has to land first.
func TestListReportsAFailedPageQuery(t *testing.T) {
	h := seeded(t, "fault_op=query&fault_after=2")
	_, err := events.List(h, "", "", "", 10, "")
	if err == nil {
		t.Fatal("List reported success on a failing page query")
	}
	if !strings.Contains(err.Error(), "list events") {
		t.Fatalf("error lost its context: %v", err)
	}
}

// events.go:179 — the scan inside the page loop.
func TestListReportsAnUnscannableRow(t *testing.T) {
	h := seeded(t, "fault_op=badrow&fault_after=2")
	_, err := events.List(h, "", "", "", 10, "")
	if err == nil {
		t.Fatal("List reported success on an unscannable row")
	}
	if !strings.Contains(err.Error(), "scan event") {
		t.Fatalf("error lost its context: %v", err)
	}
}

// events.go:190 — the streaming callback's error, which is a consumer
// abandoning the page rather than a database failure.
func TestListStreamStopsWhenTheConsumerFails(t *testing.T) {
	h := seeded(t, "")
	sentinel := errors.New("consumer stopped")
	_, err := events.ListStream(h, "", "", "", 10, "", func(events.Event) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("the consumer's error must reach the caller unwrapped, got: %v", err)
	}
}

// events.go:197 — `rows.Err() != nil`: the page cursor breaks mid-stream.
// The head read is a QueryRow, so it consumes a Next of its own — the
// page's is the second.
func TestListReportsACursorThatBreaksMidPage(t *testing.T) {
	h := seeded(t, "fault_op=next&fault_after=2")
	_, err := events.List(h, "", "", "", 10, "")
	if err == nil {
		t.Fatal("List reported success on a broken cursor")
	}
	if !strings.Contains(err.Error(), "iterate events") {
		t.Fatalf("error lost its context: %v", err)
	}
}

// events.go:205 and :209 — the kind and subject filters on the DRAIN
// remainder count. Both had only ever been unset there, so the two clauses
// that scope a drain to a filtered feed were never built.
func TestDrainCountRespectsTheFilters(t *testing.T) {
	h := seeded(t, "")
	// A bound at the head, so the drain question is answerable.
	page, err := events.List(h, "", "", "", 10, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	head := page.NextCursor

	for _, tc := range []struct {
		name, kind, subject string
	}{
		{"kind only", "issue.created", ""},
		{"subject only", "", "issue-1"},
		{"kind and subject", "issue.closed", "issue-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := events.List(h, "", tc.kind, tc.subject, 10, head)
			if err != nil {
				t.Fatalf("filtered drain: %v", err)
			}
			if !p.Drained {
				t.Fatal("a page that returned everything through the bound must report drained")
			}
		})
	}
}

// events.go:214 — the remainder count's own `err != nil`. It is the third
// query of a bounded page: head, page, remainder.
func TestDrainCountReportsAFailedQuery(t *testing.T) {
	h := seeded(t, "")
	page, err := events.List(h, "", "", "", 10, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	faulty := handle(t, "", "fault_op=query&fault_after=3")
	_, err = events.List(faulty, "", "", "", 10, page.NextCursor)
	if err == nil {
		t.Fatal("List reported success when the drain count failed")
	}
	if !strings.Contains(err.Error(), "check drain bound") {
		t.Fatalf("error lost its context: %v", err)
	}
}

// events.go:228, :235, :241 — BySubjectEach's three arms.
func TestBySubjectEachHandlesEveryFailure(t *testing.T) {
	t.Run("failed query", func(t *testing.T) {
		h := seeded(t, "fault_op=query&fault_after=1")
		tx, err := h.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		err = events.BySubjectEach(tx, "issue-1", func(events.Event) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "events of issue-1") {
			t.Fatalf("expected the query failure, got: %v", err)
		}
	})
	t.Run("unscannable row", func(t *testing.T) {
		h := seeded(t, "fault_op=badrow&fault_after=1")
		tx, err := h.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		err = events.BySubjectEach(tx, "issue-1", func(events.Event) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "scan event") {
			t.Fatalf("expected the scan failure, got: %v", err)
		}
	})
	t.Run("callback stops the stream", func(t *testing.T) {
		h := seeded(t, "")
		tx, err := h.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		sentinel := errors.New("caller stopped")
		err = events.BySubjectEach(tx, "issue-1", func(events.Event) error { return sentinel })
		if !errors.Is(err, sentinel) {
			t.Fatalf("the callback's error must reach the caller unwrapped, got: %v", err)
		}
	})
}

// events.go:129 — the `until` bound's own parseCursor failure. The cursor
// argument's malformed case was covered; its twin was not, and a fabricated
// until would otherwise report a feed drained before the events inside its
// bound exist.
func TestMalformedUntilIsRefused(t *testing.T) {
	h := seeded(t, "")
	_, err := events.List(h, "", "", "", 10, "not-a-position")
	var bad *events.BadCursorError
	if !errors.As(err, &bad) {
		t.Fatalf("expected BadCursorError for a malformed until, got: %v", err)
	}
}
