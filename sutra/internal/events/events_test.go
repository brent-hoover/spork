package events_test

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"testing"

	_ "modernc.org/sqlite"

	"sutra/internal/events"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=memory&cache=shared&_pragma=busy_timeout(5000)", t.Name()))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := events.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func emit(t *testing.T, db *sql.DB, kind, subject string) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := events.Emit(tx, kind, subject, events.NewOperation(), "actor-id", nil); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func TestAppendOnlyEnforcedByStore(t *testing.T) {
	db := openDB(t)
	emit(t, db, "project.created", "p1")
	if _, err := db.Exec(`UPDATE events SET kind = 'tampered'`); err == nil {
		t.Fatal("UPDATE on events succeeded; append-only is not enforced")
	}
	if _, err := db.Exec(`DELETE FROM events`); err == nil {
		t.Fatal("DELETE on events succeeded; append-only is not enforced")
	}
}

func TestCursorPollingIsLossless(t *testing.T) {
	db := openDB(t)
	emit(t, db, "a", "s1")
	emit(t, db, "b", "s2")
	emit(t, db, "c", "s3")

	first, err := events.List(db, "", "", "", 1, "")
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	rest, err := events.List(db, first.NextCursor, "", "", 100, "")
	if err != nil {
		t.Fatalf("rest: %v", err)
	}
	if len(rest.Events) != 2 || rest.Events[0].Kind != "b" || rest.Events[1].Kind != "c" {
		t.Fatalf("expected b,c in order, got %+v", rest.Events)
	}
	again, err := events.List(db, rest.NextCursor, "", "", 100, "")
	if err != nil {
		t.Fatalf("again: %v", err)
	}
	if len(again.Events) != 0 {
		t.Fatalf("expected empty page at head, got %+v", again.Events)
	}
	if again.NextCursor != rest.NextCursor {
		t.Fatalf("empty page moved the cursor: %s -> %s", rest.NextCursor, again.NextCursor)
	}
}

func TestFiltersNarrowTheFeed(t *testing.T) {
	db := openDB(t)
	emit(t, db, "review.approved", "sut1")
	emit(t, db, "issue.updated", "sut1")
	emit(t, db, "review.approved", "sut2")

	page, err := events.List(db, "", "review.approved", "sut1", 100, "")
	if err != nil {
		t.Fatalf("filtered list: %v", err)
	}
	if len(page.Events) != 1 || page.Events[0].Kind != "review.approved" || page.Events[0].Subject != "sut1" {
		t.Fatalf("filter leaked: %+v", page.Events)
	}
}

// TestDrainBoundIsFixed pins AC-feed-drain: the until watermark is a
// fixed position — events appended after it never delay drained=true,
// and drained stays false until everything through the bound returned.
func TestDrainBoundIsFixed(t *testing.T) {
	db := openDB(t)
	emit(t, db, "a", "s")
	emit(t, db, "b", "s")
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	watermark, err := events.Watermark(tx)
	if err != nil {
		t.Fatalf("watermark: %v", err)
	}
	_ = tx.Rollback()

	page1, err := events.List(db, "", "", "", 1, watermark)
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if page1.Drained {
		t.Fatal("drained true with events remaining through the bound")
	}

	emit(t, db, "concurrent", "s") // arrives after the bound was fixed

	page2, err := events.List(db, page1.NextCursor, "", "", 1, watermark)
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if !page2.Drained {
		t.Fatal("drained false after everything through the fixed bound was returned; the concurrent append moved the bound")
	}
}

// TestUntilBoundsThePageItself pins that a post-watermark event never
// enters a drain's pages even with spare page capacity, and the cursor
// never advances past the bound.
func TestUntilBoundsThePageItself(t *testing.T) {
	db := openDB(t)
	emit(t, db, "a", "s")
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	watermark, err := events.Watermark(tx)
	if err != nil {
		t.Fatalf("watermark: %v", err)
	}
	_ = tx.Rollback()

	emit(t, db, "past-bound", "s")

	page, err := events.List(db, "", "", "", 100, watermark)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Events) != 1 || page.Events[0].Kind != "a" {
		t.Fatalf("post-watermark event leaked into the drain: %+v", page.Events)
	}
	if page.NextCursor != watermark {
		t.Fatalf("cursor advanced past the bound: %s > %s", page.NextCursor, watermark)
	}
	if !page.Drained {
		t.Fatal("drain through the bound must report drained true")
	}
}

func TestNoUntilMeansDrainedFalse(t *testing.T) {
	db := openDB(t)
	emit(t, db, "a", "s")
	page, err := events.List(db, "", "", "", 100, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if page.Drained {
		t.Fatal("drained must be false without until")
	}
}

func TestBadCursorRejected(t *testing.T) {
	db := openDB(t)
	_, err := events.List(db, "not-a-cursor", "", "", 100, "")
	var bad *events.BadCursorError
	if !errors.As(err, &bad) {
		t.Fatalf("expected BadCursorError, got %v", err)
	}
}

// TestCursorZeroIsTheStart pins the boundary between the positions the
// feed can issue and the ones it cannot. Zero is the cursor a client
// holds before it has seen anything — the same position the empty
// string names — so it is accepted; one below it names nothing the feed
// could ever have handed out. Only these two legs, one apart, tell a
// rejection of negatives from a rejection of non-positives.
func TestCursorZeroIsTheStart(t *testing.T) {
	db := openDB(t)
	emit(t, db, "a", "s1")

	page, err := events.List(db, "0", "", "", 100, "")
	if err != nil {
		t.Fatalf("cursor 0 is the start of the feed, not a bad cursor: %v", err)
	}
	if len(page.Events) != 1 {
		t.Fatalf("cursor 0 must return the whole feed, got %d events", len(page.Events))
	}

	var bad *events.BadCursorError
	if _, err := events.List(db, "-1", "", "", 100, ""); !errors.As(err, &bad) {
		t.Fatalf("expected BadCursorError for -1, got %v", err)
	}
}

// TestFabricatedUntilRejected pins the OTHER half of the head check.
// A cursor past the head is rejected because it would skip events
// forever; an until past the head is rejected because a drain would
// report itself complete over a range whose events do not exist yet.
// The two are separate guards, and every other bound test here passes
// an until the feed actually issued.
func TestFabricatedUntilRejected(t *testing.T) {
	db := openDB(t)
	emit(t, db, "a", "s1")
	emit(t, db, "b", "s2")

	head, err := events.List(db, "", "", "", 100, "")
	if err != nil {
		t.Fatalf("read head: %v", err)
	}
	future := fmt.Sprintf("%d", mustAtoi(t, head.NextCursor)+1)

	var bad *events.BadCursorError
	if _, err := events.List(db, "", "", "", 100, future); !errors.As(err, &bad) {
		t.Fatalf("expected BadCursorError for until past the head, got %v", err)
	}
	// The head itself is still a legal bound: the rejection is of
	// positions the feed never issued, not of the newest one it did.
	if _, err := events.List(db, "", "", "", 100, head.NextCursor); err != nil {
		t.Fatalf("until at the head must be accepted: %v", err)
	}
}

func mustAtoi(t *testing.T, s string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatalf("cursor %q is not a number: %v", s, err)
	}
	return n
}
