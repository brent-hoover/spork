package api_test

import (
	"encoding/json"
	"net/http"
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
	api.SetBetweenPrepareAndCommitForTest(func() {
		if !archived.CompareAndSwap(false, true) {
			return
		}
		if st, b := post(t, srv, "/projects/"+project+"/archive", "arch",
			`{"actor":"`+actor+`"}`); st != http.StatusOK {
			t.Errorf("archive inside the window: %d %s", st, b)
		}
	})
	t.Cleanup(func() { api.SetBetweenPrepareAndCommitForTest(nil) })

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
