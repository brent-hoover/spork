package api_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"sutra/internal/api"
)

// TestArchiveInsideThePrepareWindow drives the guard that the
// archived-project scenario cannot reach.
//
// Two stages guard a review: one in prepare, which reads OUTSIDE the
// write transaction so git and oversized bodies never hold a database
// connection, and one inside the transaction. The acceptance scenario
// only ever meets the first — it archives the project up front, so
// prepare refuses and the transactional check is never consulted.
// Reviews 2017/2018 pointed out what that hides: delete the
// transactional guard and all 195 scenarios still pass, because the
// prepare-stage guard answers first.
//
// The two are not redundant. Prepare's verdict is stale by the time the
// transaction opens, so a project archived in that window would be
// admitted on prepare's say-so. Here the archive happens exactly there,
// deterministically, and the review must still be refused — which can
// only be the transactional check doing it.
func TestArchiveInsideThePrepareWindow(t *testing.T) {
	srv, _ := startAPI(t)

	status, body := post(t, srv, "/identities", "who", `{"handle":"operator","kind":"human"}`)
	if status != http.StatusCreated {
		t.Fatalf("identity: %d %s", status, body)
	}
	actor := idFrom(t, body)

	repo := newGitRepo(t)
	status, body = post(t, srv, "/projects", "proj",
		`{"key":"WIN","name":"Window","actor":"`+actor+`","repo_path":"`+repo.path+`"}`)
	if status != http.StatusCreated {
		t.Fatalf("project: %d %s", status, body)
	}
	project := idFrom(t, body)

	status, body = post(t, srv, "/projects/"+project+"/issues", "iss",
		`{"title":"in the window","actor":"`+actor+`"}`)
	if status != http.StatusCreated {
		t.Fatalf("issue: %d %s", status, body)
	}
	issue := idFrom(t, body)

	// The project is still live, so prepare will pass. It is archived
	// after prepare and before the transaction — the window itself.
	//
	// The one-shot is a compare-and-swap, not a sync.Once, and the
	// difference is a deadlock. The hook's own archive request goes
	// through the same handler path and re-enters the hook, so a
	// primitive that BLOCKS the second caller blocks it behind the first
	// — which is waiting on that very request. CAS lets the re-entrant
	// call fall straight through. Atomic rather than a plain bool
	// because the hook, its nested request, and this test are three
	// different goroutines (review 2024).
	var archived atomic.Bool
	api.SetBetweenPrepareAndCommitForTest(srv.Config.Handler, func() {
		if !archived.CompareAndSwap(false, true) {
			return
		}
		if st, b := post(t, srv, "/projects/"+project+"/archive", "arch",
			`{"actor":"`+actor+`"}`); st != http.StatusOK {
			t.Errorf("archive inside the window: %d %s", st, b)
		}
	})
	t.Cleanup(func() { api.SetBetweenPrepareAndCommitForTest(srv.Config.Handler, nil) })

	status, body = post(t, srv, "/reviews", "rev",
		`{"issue":"`+issue+`","author":"`+actor+`","branch":"feature","commit":"`+repo.featureSHA+`"}`)
	if !archived.Load() {
		t.Fatal("the hook never ran; the window this test aims at was not opened")
	}
	if status == http.StatusCreated {
		t.Fatalf("a review was opened into a project archived inside the prepare window: %s", body)
	}
	if status != http.StatusConflict {
		t.Fatalf("expected 409 from the transactional guard, got %d — %s", status, body)
	}
}

func idFrom(t *testing.T, body string) string {
	t.Helper()
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("decode id: %v — %s", err, body)
	}
	if created.ID == "" {
		t.Fatalf("no id in %s", body)
	}
	return created.ID
}

// TestPrepareWindowHookIsPerServer pins the property that made the hook a
// field rather than a package variable: two servers must not see each
// other's. A global one lets an unrelated request consume this test's
// one-shot and archive the project early, after which
// TestArchiveInsideThePrepareWindow passes on the prepare-stage 409
// without ever reaching the transactional guard it exists to prove
// (review 2036).
func TestPrepareWindowHookIsPerServer(t *testing.T) {
	first, _ := startAPI(t)
	second := startSecondAPI(t)

	var firstFired, secondFired atomic.Int64
	api.SetBetweenPrepareAndCommitForTest(first.Config.Handler, func() { firstFired.Add(1) })
	api.SetBetweenPrepareAndCommitForTest(second.Config.Handler, func() { secondFired.Add(1) })
	t.Cleanup(func() {
		api.SetBetweenPrepareAndCommitForTest(first.Config.Handler, nil)
		api.SetBetweenPrepareAndCommitForTest(second.Config.Handler, nil)
	})

	// Only the FIRST server is driven, and only through an endpoint that
	// has a prepare stage at all — createReview is the one that does.
	// Driving the first is what makes this discriminating: under a single
	// global hook the second installation would have REPLACED the first,
	// so this request would fire the second server's closure and neither
	// assertion below would hold.
	if !reviewAttempt(t, first) {
		t.Fatal("the first server's prepare stage never ran")
	}
	if got := firstFired.Load(); got == 0 {
		t.Fatal("the first server's own hook never fired")
	}
	if got := secondFired.Load(); got != 0 {
		t.Fatalf("a request to the first server fired the second server's hook %d times", got)
	}

	// And symmetrically, so that an implementation which simply IGNORED
	// the second installation cannot pass by never being asked about it
	// (review 2041).
	firstBefore := firstFired.Load()
	if !reviewAttempt(t, second) {
		t.Fatal("the second server's prepare stage never ran")
	}
	if got := secondFired.Load(); got == 0 {
		t.Fatal("the second server's own hook never fired")
	}
	if got := firstFired.Load(); got != firstBefore {
		t.Fatalf("a request to the second server fired the first server's hook (%d -> %d)", firstBefore, got)
	}
}

// startSecondAPI is startAPI with a distinct in-memory database, so the two
// servers share nothing but the process.
func startSecondAPI(t *testing.T) *httptest.Server {
	t.Helper()
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s-second?mode=memory&cache=shared&_pragma=busy_timeout(5000)", t.Name()))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	handler, err := api.New(db)
	if err != nil {
		t.Fatalf("wire api: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(func() { srv.Close(); _ = db.Close() })
	return srv
}

// reviewAttempt drives one createReview far enough to reach the
// prepare-to-commit window, and reports whether it got there.
func reviewAttempt(t *testing.T, srv *httptest.Server) bool {
	t.Helper()
	status, body := post(t, srv, "/identities", "who2", `{"handle":"operator","kind":"human"}`)
	if status != http.StatusCreated {
		t.Fatalf("identity: %d %s", status, body)
	}
	actor := idFrom(t, body)
	repo := newGitRepo(t)
	status, body = post(t, srv, "/projects", "proj2",
		`{"key":"TWO","name":"Two","actor":"`+actor+`","repo_path":"`+repo.path+`"}`)
	if status != http.StatusCreated {
		t.Fatalf("project: %d %s", status, body)
	}
	project := idFrom(t, body)
	status, body = post(t, srv, "/projects/"+project+"/issues", "iss2",
		`{"title":"second","actor":"`+actor+`"}`)
	if status != http.StatusCreated {
		t.Fatalf("issue: %d %s", status, body)
	}
	issue := idFrom(t, body)
	status, body = post(t, srv, "/reviews", "rev2",
		`{"issue":"`+issue+`","author":"`+actor+`","branch":"feature","commit":"`+repo.featureSHA+`"}`)
	if status != http.StatusCreated {
		t.Fatalf("review: %d %s", status, body)
	}
	return true
}
