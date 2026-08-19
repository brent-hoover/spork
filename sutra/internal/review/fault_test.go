package review_test

import (
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	_ "sutra/internal/faultsql"
	"sutra/internal/review"
)

// review's forty-nine uncovered arms cluster around one fact: Get is called
// at the top of SetVerdict, Consume, Resubmit and SpendForClose, and again
// at the bottom of each. So the ORDINAL is doing most of the work here —
// Get makes two queries (the review row, then its submissions), which means
// the operation counts have to be read off that structure rather than
// guessed. Every case asserts the error's context string, because the
// difference between standing on the opening Get and the closing one is
// invisible otherwise.

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
	issueID  = "11111111-1111-7111-8111-111111111111"
	authorID = "22222222-2222-7222-8222-222222222222"
	absentID = "99999999-9999-7999-8999-999999999999"
	eventID  = "33333333-3333-7333-8333-333333333333"
)

func str(s string) *string { return &s }

// seed migrates and creates one open review with one submission, returning a
// second handle with the fault armed.
func seed(t *testing.T, fault string) (*sql.DB, string) {
	t.Helper()
	setup := handle(t, "", "")
	if err := review.Migrate(setup); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	tx, err := setup.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	r, err := review.Create(tx, issueID, authorID,
		review.Deliverable{Branch: str("work"), Commit: str(strings.Repeat("a", 40))},
		str("summary"), strings.Repeat("b", 40), str("diff content"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return handle(t, "", fault), r.ID
}

// approved drives the seeded review to approved at revision 1, then arms the
// fault — so Consume and SpendForClose can reach their own write arms rather
// than being turned away by a state check.
func approved(t *testing.T, fault string) (*sql.DB, string) {
	t.Helper()
	h, id := seed(t, "")
	tx, err := h.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := review.SetVerdict(tx, id, "approved", 1, eventID); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return handle(t, "", fault), id
}

// changesRequested is the same for Resubmit's precondition.
func changesRequested(t *testing.T, fault string) (*sql.DB, string) {
	t.Helper()
	h, id := seed(t, "")
	tx, err := h.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := review.SetVerdict(tx, id, "changes-requested", 1, eventID); err != nil {
		t.Fatalf("request changes: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return handle(t, "", fault), id
}

func inTx(t *testing.T, h *sql.DB, fn func(*sql.Tx) error) error {
	t.Helper()
	tx, err := h.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	return fn(tx)
}

func wantErr(t *testing.T, err error, context string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a failure carrying %q, got success", context)
	}
	if !strings.Contains(err.Error(), context) {
		t.Fatalf("expected an error containing %q, got: %v", context, err)
	}
}

// review.go:99 — Migrate.
func TestMigrateReportsAFailedExec(t *testing.T) {
	h := handle(t, "", "fault_op=exec&fault_after=1")
	wantErr(t, review.Migrate(h), "migrate reviews")
}

// review.go:219, :241 — Create's insert and the submission insert it calls.
// A review row without its first submission would be a review nobody can
// open, so the second failure has to surface too.
func TestCreateReportsBothInserts(t *testing.T) {
	d := review.Deliverable{Branch: str("work"), Commit: str(strings.Repeat("c", 40))}
	t.Run("review row", func(t *testing.T) {
		setup := handle(t, "", "")
		if err := review.Migrate(setup); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		h := handle(t, "", "fault_op=exec&fault_after=1")
		wantErr(t, inTx(t, h, func(tx *sql.Tx) error {
			_, err := review.Create(tx, issueID, authorID, d, nil, "", nil)
			return err
		}), "insert review")
	})
	t.Run("submission row", func(t *testing.T) {
		setup := handle(t, "", "")
		if err := review.Migrate(setup); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		h := handle(t, "", "fault_op=exec&fault_after=2")
		wantErr(t, inTx(t, h, func(tx *sql.Tx) error {
			_, err := review.Create(tx, issueID, authorID, d, nil, "", nil)
			return err
		}), "insert submission")
	})
}

// review.go:265 — Get's four arms. Its second query streams the submission
// history, so the scan and iteration failures live there rather than on the
// review row.
func TestGetHandlesEveryFailure(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		h, _ := seed(t, "")
		err := inTx(t, h, func(tx *sql.Tx) error { _, err := review.Get(tx, absentID); return err })
		var nf *review.NotFoundError
		if !errors.As(err, &nf) {
			t.Fatalf("expected NotFoundError, got: %v", err)
		}
	})
	t.Run("review row unreadable", func(t *testing.T) {
		h, id := seed(t, "fault_op=query&fault_after=1")
		err := inTx(t, h, func(tx *sql.Tx) error { _, err := review.Get(tx, id); return err })
		var nf *review.NotFoundError
		if errors.As(err, &nf) {
			t.Fatal("a query failure must not be reported as not-found")
		}
		wantErr(t, err, "get review")
	})
	t.Run("submission history unreadable", func(t *testing.T) {
		h, id := seed(t, "fault_op=query&fault_after=2")
		wantErr(t, inTx(t, h, func(tx *sql.Tx) error { _, err := review.Get(tx, id); return err }),
			"submissions of")
	})
	t.Run("submission row will not scan", func(t *testing.T) {
		h, id := seed(t, "fault_op=badrow&fault_after=2")
		wantErr(t, inTx(t, h, func(tx *sql.Tx) error { _, err := review.Get(tx, id); return err }),
			"scan submission")
	})
	t.Run("submission cursor breaks", func(t *testing.T) {
		// The review row's QueryRow consumes a Next of its own, so the
		// history cursor's first Next is the second.
		h, id := seed(t, "fault_op=next&fault_after=2")
		wantErr(t, inTx(t, h, func(tx *sql.Tx) error { _, err := review.Get(tx, id); return err }),
			"iterate submissions")
	})
}

// review.go:302 — GetMeta's pair, separate code from Get's.
func TestGetMetaSeparatesAbsenceFromFailure(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		h, _ := seed(t, "")
		err := inTx(t, h, func(tx *sql.Tx) error { _, err := review.GetMeta(tx, absentID); return err })
		var nf *review.NotFoundError
		if !errors.As(err, &nf) {
			t.Fatalf("expected NotFoundError, got: %v", err)
		}
	})
	t.Run("failed query", func(t *testing.T) {
		h, id := seed(t, "fault_op=query&fault_after=1")
		wantErr(t, inTx(t, h, func(tx *sql.Tx) error { _, err := review.GetMeta(tx, id); return err }),
			"get review meta")
	})
}

// review.go:370 — RefByID's pair.
func TestRefByIDSeparatesAbsenceFromFailure(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		h, _ := seed(t, "")
		err := inTx(t, h, func(tx *sql.Tx) error { _, err := review.RefByID(tx, absentID); return err })
		var nf *review.NotFoundError
		if !errors.As(err, &nf) {
			t.Fatalf("expected NotFoundError, got: %v", err)
		}
	})
	t.Run("failed query", func(t *testing.T) {
		h, id := seed(t, "fault_op=query&fault_after=1")
		wantErr(t, inTx(t, h, func(tx *sql.Tx) error { _, err := review.RefByID(tx, id); return err }),
			"review ref")
	})
}

// review.go:385 — EachRef's three.
func TestEachRefHandlesEveryFailure(t *testing.T) {
	t.Run("failed query", func(t *testing.T) {
		h, _ := seed(t, "fault_op=query&fault_after=1")
		wantErr(t, inTx(t, h, func(tx *sql.Tx) error {
			return review.EachRef(tx, issueID, "", "", func(review.Ref) error { return nil })
		}), "list review refs")
	})
	t.Run("unscannable row", func(t *testing.T) {
		h, _ := seed(t, "fault_op=badrow&fault_after=1")
		wantErr(t, inTx(t, h, func(tx *sql.Tx) error {
			return review.EachRef(tx, issueID, "", "", func(review.Ref) error { return nil })
		}), "scan review ref")
	})
	t.Run("callback stops the stream", func(t *testing.T) {
		h, _ := seed(t, "")
		sentinel := errors.New("caller stopped")
		err := inTx(t, h, func(tx *sql.Tx) error {
			return review.EachRef(tx, issueID, "", "", func(review.Ref) error { return sentinel })
		})
		if !errors.Is(err, sentinel) {
			t.Fatalf("the callback's error must reach the caller unwrapped, got: %v", err)
		}
	})
}

// review.go:406 — ListEach's five. It collects ids first and then calls Get
// per id, which is why its failures split across two shapes.
func TestListEachHandlesEveryFailure(t *testing.T) {
	t.Run("failed id query", func(t *testing.T) {
		h, _ := seed(t, "fault_op=query&fault_after=1")
		wantErr(t, inTx(t, h, func(tx *sql.Tx) error {
			return review.ListEach(tx, issueID, "", "", func(review.Review) error { return nil })
		}), "list reviews")
	})
	t.Run("unscannable id", func(t *testing.T) {
		h, _ := seed(t, "fault_op=badrow&fault_after=1")
		wantErr(t, inTx(t, h, func(tx *sql.Tx) error {
			return review.ListEach(tx, issueID, "", "", func(review.Review) error { return nil })
		}), "scan review id")
	})
	t.Run("id cursor breaks", func(t *testing.T) {
		h, _ := seed(t, "fault_op=next&fault_after=1")
		wantErr(t, inTx(t, h, func(tx *sql.Tx) error {
			return review.ListEach(tx, issueID, "", "", func(review.Review) error { return nil })
		}), "iterate reviews")
	})
	t.Run("the per-id Get fails", func(t *testing.T) {
		// The id listing is query 1; the Get that follows is query 2.
		h, _ := seed(t, "fault_op=query&fault_after=2")
		wantErr(t, inTx(t, h, func(tx *sql.Tx) error {
			return review.ListEach(tx, issueID, "", "", func(review.Review) error { return nil })
		}), "get review")
	})
	t.Run("callback stops the stream", func(t *testing.T) {
		h, _ := seed(t, "")
		sentinel := errors.New("caller stopped")
		err := inTx(t, h, func(tx *sql.Tx) error {
			return review.ListEach(tx, issueID, "", "", func(review.Review) error { return sentinel })
		})
		if !errors.Is(err, sentinel) {
			t.Fatalf("the callback's error must reach the caller unwrapped, got: %v", err)
		}
	})
}

// review.go:444 — SetVerdict's opening Get and its UPDATE.
func TestSetVerdictReportsItsFailures(t *testing.T) {
	t.Run("opening read fails", func(t *testing.T) {
		h, id := seed(t, "fault_op=query&fault_after=1")
		wantErr(t, inTx(t, h, func(tx *sql.Tx) error {
			_, err := review.SetVerdict(tx, id, "approved", 1, eventID)
			return err
		}), "get review")
	})
	t.Run("update fails", func(t *testing.T) {
		h, id := seed(t, "fault_op=exec&fault_after=1")
		wantErr(t, inTx(t, h, func(tx *sql.Tx) error {
			_, err := review.SetVerdict(tx, id, "approved", 1, eventID)
			return err
		}), "set verdict on")
	})
}

// review.go:468 — Consume's opening Get, its UPDATE, and the raced
// zero-row case that its RowsAffected check answers.
func TestConsumeReportsItsFailures(t *testing.T) {
	t.Run("opening read fails", func(t *testing.T) {
		h, id := approved(t, "fault_op=query&fault_after=1")
		wantErr(t, inTx(t, h, func(tx *sql.Tx) error {
			_, err := review.Consume(tx, id, 1, eventID)
			return err
		}), "get review")
	})
	t.Run("update fails", func(t *testing.T) {
		h, id := approved(t, "fault_op=exec&fault_after=1")
		wantErr(t, inTx(t, h, func(tx *sql.Tx) error {
			_, err := review.Consume(tx, id, 1, eventID)
			return err
		}), "consume")
	})
	t.Run("row count unavailable reads as a race", func(t *testing.T) {
		// SQLite never fails RowsAffected, so this arm is reachable only
		// through the injector — and it must NOT be deleted instead: the
		// zero-row branch is what stops two subscribers both consuming
		// one approval, so an unreadable count has to be treated as a
		// loss, never as a win.
		h, id := approved(t, "fault_op=rowsaffected&fault_after=1")
		err := inTx(t, h, func(tx *sql.Tx) error {
			_, err := review.Consume(tx, id, 1, eventID)
			return err
		})
		var conflict *review.ConflictError
		if !errors.As(err, &conflict) || conflict.Code != "review-consumed" {
			t.Fatalf("expected a review-consumed conflict, got: %v", err)
		}
	})
}

// review.go:507 — Resubmit's opening Get, its UPDATE, and the submission
// append it delegates to.
func TestResubmitReportsItsFailures(t *testing.T) {
	d := review.Deliverable{Branch: str("rework"), Commit: str(strings.Repeat("d", 40))}
	t.Run("opening read fails", func(t *testing.T) {
		h, id := changesRequested(t, "fault_op=query&fault_after=1")
		wantErr(t, inTx(t, h, func(tx *sql.Tx) error {
			_, err := review.Resubmit(tx, id, 1, eventID, d, nil, "", nil)
			return err
		}), "get review")
	})
	t.Run("update fails", func(t *testing.T) {
		h, id := changesRequested(t, "fault_op=exec&fault_after=1")
		wantErr(t, inTx(t, h, func(tx *sql.Tx) error {
			_, err := review.Resubmit(tx, id, 1, eventID, d, nil, "", nil)
			return err
		}), "resubmit")
	})
	t.Run("the new submission fails", func(t *testing.T) {
		h, id := changesRequested(t, "fault_op=exec&fault_after=2")
		wantErr(t, inTx(t, h, func(tx *sql.Tx) error {
			_, err := review.Resubmit(tx, id, 1, eventID, d, nil, "", nil)
			return err
		}), "insert submission")
	})
}

// review.go:543 — SpendForClose. Its opening Get is special: an unknown id
// is the ownership failure, not a 404, so the not-found case converts to a
// conflict — while a genuine query failure must still propagate as itself.
func TestSpendForCloseReportsItsFailures(t *testing.T) {
	t.Run("unknown id becomes a conflict, not a 404", func(t *testing.T) {
		h, _ := approved(t, "")
		err := inTx(t, h, func(tx *sql.Tx) error {
			_, err := review.SpendForClose(tx, absentID, issueID, 1, eventID)
			return err
		})
		var conflict *review.ConflictError
		if !errors.As(err, &conflict) || conflict.Code != "missing-approval" {
			t.Fatalf("expected a missing-approval conflict, got: %v", err)
		}
		var nf *review.NotFoundError
		if errors.As(err, &nf) {
			t.Fatal("the close path must not surface a 404")
		}
	})
	t.Run("opening read fails and stays a failure", func(t *testing.T) {
		h, id := approved(t, "fault_op=query&fault_after=1")
		err := inTx(t, h, func(tx *sql.Tx) error {
			_, err := review.SpendForClose(tx, id, issueID, 1, eventID)
			return err
		})
		var conflict *review.ConflictError
		if errors.As(err, &conflict) {
			t.Fatalf("a broken database must not be reported as an ownership conflict: %v", err)
		}
		wantErr(t, err, "get review")
	})
	t.Run("update fails", func(t *testing.T) {
		h, id := approved(t, "fault_op=exec&fault_after=1")
		wantErr(t, inTx(t, h, func(tx *sql.Tx) error {
			_, err := review.SpendForClose(tx, id, issueID, 1, eventID)
			return err
		}), "spend")
	})
	t.Run("row count unavailable reads as a race", func(t *testing.T) {
		h, id := approved(t, "fault_op=rowsaffected&fault_after=1")
		err := inTx(t, h, func(tx *sql.Tx) error {
			_, err := review.SpendForClose(tx, id, issueID, 1, eventID)
			return err
		})
		var conflict *review.ConflictError
		if !errors.As(err, &conflict) || conflict.Code != "review-close-used" {
			t.Fatalf("expected a review-close-used conflict, got: %v", err)
		}
	})
}

// review.go:713 — ContentAt's arms. Its nil-for-absent case is deliberate
// and documented, so the test asserts the SILENCE as much as the error.
func TestContentAtHandlesEveryOutcome(t *testing.T) {
	t.Run("absent submission is nil, not an error", func(t *testing.T) {
		h, id := seed(t, "")
		err := inTx(t, h, func(tx *sql.Tx) error {
			content, err := review.ContentAt(tx, id, 99)
			if err != nil {
				return err
			}
			if content != nil {
				t.Fatal("an absent submission must read as nil content")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("failed query", func(t *testing.T) {
		h, id := seed(t, "fault_op=query&fault_after=1")
		wantErr(t, inTx(t, h, func(tx *sql.Tx) error { _, err := review.ContentAt(tx, id, 1); return err }),
			"content of")
	})
}

// review.go:733 — SubmissionAt's miss, which is a pure lookup over data
// already in hand.
func TestSubmissionAtReportsAMiss(t *testing.T) {
	if _, ok := review.SubmissionAt(review.Review{}, 0); ok {
		t.Fatal("a review with no submissions has none to return")
	}
	if _, ok := review.SubmissionAt(review.Review{
		Submissions: []review.Submission{{Revision: 1}},
	}, 7); ok {
		t.Fatal("revision 7 does not exist")
	}
}

// review.go:607 — Diff's failures, which are git's rather than the
// database's: no repo configured, and a commit pair git cannot resolve.
func TestDiffReportsGitFailures(t *testing.T) {
	t.Run("no repo path", func(t *testing.T) {
		_, err := review.Diff("", "a", "b")
		var gitErr *review.GitError
		if !errors.As(err, &gitErr) {
			t.Fatalf("expected GitError, got: %v", err)
		}
	})
	t.Run("unresolvable commits", func(t *testing.T) {
		repo := gitRepo(t)
		_, err := review.Diff(repo, strings.Repeat("0", 40), strings.Repeat("1", 40))
		var gitErr *review.GitError
		if !errors.As(err, &gitErr) {
			t.Fatalf("expected GitError, got: %v", err)
		}
		if !strings.Contains(err.Error(), "unresolvable") {
			t.Fatalf("expected the unresolvable message, got: %v", err)
		}
	})
}

// review.go:680 — RevalidateFences' early return when no fence was supplied.
// Fences that were never named are not the caller's business, and the arm
// that says so had never been taken.
func TestRevalidateFencesWithoutFencesIsANoOp(t *testing.T) {
	if err := review.RevalidateFences(review.Repo{}, "abc", review.Fences{}); err != nil {
		t.Fatalf("no fences means nothing to revalidate: %v", err)
	}
}

func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v — %s", args, err, out)
		}
	}
	run("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	run("add", ".")
	run("commit", "-m", "base")
	return dir
}

// --- the guard conditions, which need shaped state rather than faults -----

// review.go:512, :516, :520 — Resubmit's three preconditions. Rework must
// answer the LATEST feedback at the revision it was given, so each of these
// is a different way stale rework gets refused.
func TestResubmitRefusesStaleRework(t *testing.T) {
	d := review.Deliverable{Branch: str("rework"), Commit: str(strings.Repeat("e", 40))}

	t.Run("wrong state", func(t *testing.T) {
		h, id := seed(t, "") // still open, never reviewed
		err := inTx(t, h, func(tx *sql.Tx) error {
			_, err := review.Resubmit(tx, id, 1, eventID, d, nil, "", nil)
			return err
		})
		var conflict *review.ConflictError
		if !errors.As(err, &conflict) || conflict.Code != "expected-status-mismatch" {
			t.Fatalf("expected expected-status-mismatch, got: %v", err)
		}
	})
	t.Run("wrong revision", func(t *testing.T) {
		h, id := changesRequested(t, "")
		err := inTx(t, h, func(tx *sql.Tx) error {
			_, err := review.Resubmit(tx, id, 7, eventID, d, nil, "", nil)
			return err
		})
		var conflict *review.ConflictError
		if !errors.As(err, &conflict) || conflict.Code != "expected-revision-mismatch" {
			t.Fatalf("expected expected-revision-mismatch, got: %v", err)
		}
	})
	t.Run("verdict event moved", func(t *testing.T) {
		h, id := changesRequested(t, "")
		err := inTx(t, h, func(tx *sql.Tx) error {
			_, err := review.Resubmit(tx, id, 1, "44444444-4444-7444-8444-444444444444", d, nil, "", nil)
			return err
		})
		var conflict *review.ConflictError
		if !errors.As(err, &conflict) || conflict.Code != "expected-verdict-event-mismatch" {
			t.Fatalf("expected expected-verdict-event-mismatch, got: %v", err)
		}
	})
}

// review.go:485, :520, :571 — the `LatestVerdictEvent == nil` halves of the
// verdict-event fences in Consume, Resubmit and SpendForClose.
//
// No API path produces this state: SetVerdict always writes an event, so a
// review is never approved or changes-requested with a null one. These rows
// are therefore shaped with raw SQL, and that is the point rather than a
// shortcut — the nil half of each fence exists precisely for a row the API
// did not write, and leaving it untested would mean nobody had checked that
// a missing event is refused rather than dereferenced.
func TestVerdictEventFencesRefuseANullEvent(t *testing.T) {
	shape := func(t *testing.T, state string) (*sql.DB, string) {
		t.Helper()
		h, id := seed(t, "")
		if _, err := h.Exec(`UPDATE reviews SET state = ?, latest_verdict_event = NULL WHERE id = ?`,
			state, id); err != nil {
			t.Fatalf("shape the row: %v", err)
		}
		return h, id
	}

	t.Run("Consume", func(t *testing.T) {
		h, id := shape(t, "approved")
		err := inTx(t, h, func(tx *sql.Tx) error {
			_, err := review.Consume(tx, id, 1, eventID)
			return err
		})
		var conflict *review.ConflictError
		if !errors.As(err, &conflict) || conflict.Code != "expected-verdict-event-mismatch" {
			t.Fatalf("expected expected-verdict-event-mismatch, got: %v", err)
		}
	})
	t.Run("Resubmit", func(t *testing.T) {
		h, id := shape(t, "changes-requested")
		err := inTx(t, h, func(tx *sql.Tx) error {
			_, err := review.Resubmit(tx, id, 1, eventID,
				review.Deliverable{Branch: str("r"), Commit: str(strings.Repeat("f", 40))}, nil, "", nil)
			return err
		})
		var conflict *review.ConflictError
		if !errors.As(err, &conflict) || conflict.Code != "expected-verdict-event-mismatch" {
			t.Fatalf("expected expected-verdict-event-mismatch, got: %v", err)
		}
	})
	t.Run("SpendForClose", func(t *testing.T) {
		h, id := shape(t, "approved")
		err := inTx(t, h, func(tx *sql.Tx) error {
			_, err := review.SpendForClose(tx, id, issueID, 1, eventID)
			return err
		})
		var conflict *review.ConflictError
		if !errors.As(err, &conflict) || conflict.Code != "expected-verdict-event-mismatch" {
			t.Fatalf("expected expected-verdict-event-mismatch, got: %v", err)
		}
	})
}

// review.go:575 — SpendForClose's consumed-at-a-different-revision fence.
// An approval consumed at revision 1 does not authorize closing revision 2,
// and the only way to hold that state is to shape it: Resubmit would clear
// the approval on its way past.
func TestSpendForCloseRefusesAStaleConsumption(t *testing.T) {
	h, id := seed(t, "")
	if _, err := h.Exec(`
		UPDATE reviews SET state = 'approved', latest_verdict_event = ?, revision = 2,
			consumed = '2026-01-01T00:00:00.000000000Z', consumed_revision = 1
		WHERE id = ?`, eventID, id); err != nil {
		t.Fatalf("shape the row: %v", err)
	}
	err := inTx(t, h, func(tx *sql.Tx) error {
		_, err := review.SpendForClose(tx, id, issueID, 2, eventID)
		return err
	})
	var conflict *review.ConflictError
	if !errors.As(err, &conflict) || conflict.Code != "review-consumed" {
		t.Fatalf("expected a review-consumed conflict, got: %v", err)
	}
}

// review.go:497, :589 — the `n == 0` halves of the two compare-and-sets.
// Both stop a second actor from spending something already spent, and both
// are otherwise reachable only by winning a real race against yourself. The
// injector's zerorows mode reports a statement that succeeded and changed
// nothing, which is exactly what the loser of that race sees.
func TestCompareAndSetLosersAreRefused(t *testing.T) {
	t.Run("Consume", func(t *testing.T) {
		h, id := approved(t, "fault_op=zerorows&fault_after=1")
		err := inTx(t, h, func(tx *sql.Tx) error {
			_, err := review.Consume(tx, id, 1, eventID)
			return err
		})
		var conflict *review.ConflictError
		if !errors.As(err, &conflict) || conflict.Code != "review-consumed" {
			t.Fatalf("expected a review-consumed conflict, got: %v", err)
		}
	})
	t.Run("SpendForClose", func(t *testing.T) {
		h, id := approved(t, "fault_op=zerorows&fault_after=1")
		err := inTx(t, h, func(tx *sql.Tx) error {
			_, err := review.SpendForClose(tx, id, issueID, 1, eventID)
			return err
		})
		var conflict *review.ConflictError
		if !errors.As(err, &conflict) || conflict.Code != "review-close-used" {
			t.Fatalf("expected a review-close-used conflict, got: %v", err)
		}
	})
}

// review.go:694 — RevalidateFences' resolveHead failure: a fence was named,
// and the repository cannot say what its head is. The fence must fail closed
// rather than pass for want of an answer.
func TestRevalidateFencesFailsWhenTheHeadIsUnresolvable(t *testing.T) {
	head := strings.Repeat("0", 40)
	err := review.RevalidateFences(
		review.Repo{Path: filepath.Join(t.TempDir(), "not-a-repo")},
		strings.Repeat("1", 40),
		review.Fences{ExpectedDefaultHead: &head},
	)
	if err == nil {
		t.Fatal("a fence must not pass when the head cannot be resolved")
	}
}

// review.go:694 — RevalidateFences' merge-base failure. Reaching it needs
// resolveHead to SUCCEED first, so this wants a real repository with a
// matching head expectation and a commit git cannot place.
func TestRevalidateFencesFailsWhenTheMergeBaseIsUnresolvable(t *testing.T) {
	repo := gitRepo(t)
	head, err := exec.Command("git", "-C", repo, "show-ref", "--verify", "--hash", "refs/heads/main").Output()
	if err != nil {
		t.Fatalf("read head: %v", err)
	}
	actual := strings.TrimSpace(string(head))
	base := strings.Repeat("0", 40)
	err = review.RevalidateFences(
		review.Repo{Path: repo, DefaultBranch: "main"},
		strings.Repeat("1", 40), // a commit this repository has never seen
		review.Fences{ExpectedDefaultHead: &actual, ExpectedBaseCommit: &base},
	)
	var gitErr *review.GitError
	if !errors.As(err, &gitErr) {
		t.Fatalf("expected GitError, got: %v", err)
	}
	if !strings.Contains(err.Error(), "merge base") {
		t.Fatalf("expected the merge-base failure, got: %v", err)
	}
}

// review.go:622 — Diff's `cmd.Start()` failure, which is distinct from git
// running and exiting nonzero (that is the Wait path, covered above). Start
// fails when the binary cannot be found at all, so this removes git from
// PATH for the duration.
func TestDiffReportsAFailedStart(t *testing.T) {
	t.Setenv("PATH", "")
	_, err := review.Diff(t.TempDir(), strings.Repeat("0", 40), strings.Repeat("1", 40))
	var gitErr *review.GitError
	if !errors.As(err, &gitErr) {
		t.Fatalf("expected GitError, got: %v", err)
	}
	if strings.Contains(err.Error(), "unresolvable") {
		t.Fatal("this must be the start failure, not git exiting nonzero")
	}
}

// --- what is left, and why ------------------------------------------------
//
// Two arms in Diff stay uncovered, and both are legitimate guards rather
// than dead code — so neither is deleted:
//
//   review.go:619  cmd.StdoutPipe() failing. Of its three causes, two are
//                  impossible here by construction (Diff never sets Stdout,
//                  and calls StdoutPipe before Start), but the third is
//                  os.Pipe() failing — file-descriptor exhaustion. Real,
//                  and not triggerable on demand without a seam inside
//                  os/exec.
//   review.go:636  io.ReadAll on that pipe returning a non-EOF error. A
//                  broken pipe rather than a closed one; same problem.
//
// This is the residue class: checks whose failure the platform permits and
// no test can schedule. The alternative — deleting them — would ignore a
// real error to satisfy a number, which is the opposite of what the gate is
// for. Compare the rand.Read guard, which WAS deleted: there the standard
// library proved the error cannot be returned at all.
