package api

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"testing"

	_ "modernc.org/sqlite"

	"sutra/internal/review"
)

// hangUpWriter is a ResponseWriter whose client stops reading after
// failAfter successful writes — the disconnect an export must notice.
type hangUpWriter struct {
	header    http.Header
	writes    int
	failAfter int
}

func (h *hangUpWriter) Header() http.Header {
	if h.header == nil {
		h.header = http.Header{}
	}
	return h.header
}

func (h *hangUpWriter) WriteHeader(int) {}

func (h *hangUpWriter) Write(b []byte) (int, error) {
	h.writes++
	if h.writes > h.failAfter {
		return 0, errors.New("connection reset by peer")
	}
	return len(b), nil
}

// TestArrayWriterStopsAfterTheClientHangsUp pins the failure flag that
// every one of the export's ten abort checkpoints reads. An export runs
// inside a read transaction; finishing a scan for a client that has
// gone pins the WAL for nothing. The flag has to be SET by the failing
// write and has to suppress the writes that follow — a streamed
// response has no other way to learn its reader left.
func TestArrayWriterStopsAfterTheClientHangsUp(t *testing.T) {
	w := &hangUpWriter{failAfter: 1}
	a := newArrayWriter(w)

	a.write([]byte("a"))
	if a.failed() {
		t.Fatal("a successful write reported failure")
	}

	a.write([]byte("b")) // the client is gone
	if !a.failed() {
		t.Fatal("a failed write went unrecorded; the export would run the scan to completion")
	}

	before := w.writes
	a.write([]byte("c"))
	if w.writes != before {
		t.Fatalf("writing continued after the client left: %d further attempts", w.writes-before)
	}
}

// TestNestedArrayWriterSharesTheFailure pins that an inner array — the
// submissions spliced inside a review — reports the SAME failure. A
// nested writer with its own flag would keep streaming into a dead
// connection while the outer one had already given up.
func TestNestedArrayWriterSharesTheFailure(t *testing.T) {
	w := &hangUpWriter{failAfter: 0}
	a := newArrayWriter(w)
	inner := a.nested()

	a.write([]byte("x"))
	if !inner.failed() {
		t.Fatal("the inner array did not see the outer array's failure")
	}
}

func migratedDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite",
		fmt.Sprintf("file:%s?mode=memory&cache=shared&_pragma=busy_timeout(5000)", t.Name()))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := New(db); err != nil {
		t.Fatalf("wire api: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestEachSubmissionStopsAtTheFirstCallbackError pins that the walk
// abandons its rows the moment the caller reports trouble. The caller
// here is the export writer, and the trouble it reports is a client
// that hung up: a walk that ignored it would scan every remaining
// submission — the largest rows in the export — to write them nowhere.
func TestEachSubmissionStopsAtTheFirstCallbackError(t *testing.T) {
	db := migratedDB(t)
	for revision := 1; revision <= 3; revision++ {
		if _, err := db.Exec(
			`INSERT INTO review_submissions (id, review, revision, created) VALUES (?, ?, ?, ?)`,
			fmt.Sprintf("sub-%d", revision), "rv-1", revision, "2026-08-08T00:00:00Z"); err != nil {
			t.Fatalf("seed submission %d: %v", revision, err)
		}
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	hungUp := errors.New("connection reset by peer")
	calls := 0
	err = eachSubmission(tx, "rv-1", func(review.Submission) error {
		calls++
		return hungUp
	})
	if !errors.Is(err, hungUp) {
		t.Fatalf("callback failure not returned to the caller: %v", err)
	}
	if calls != 1 {
		t.Fatalf("walk continued past the failure: %d callbacks over 3 submissions", calls)
	}
}
