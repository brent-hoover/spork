package api_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"sutra/internal/api"
	_ "sutra/internal/faultsql"
)

// internal/api holds more uncovered arms than the rest of the module put
// together, and they are almost all the same shape: `err != nil` after a
// store call. Writing them out one at a time would be thousands of lines of
// near-identical test, so this sweeps them instead — and sweeps on a
// property strong enough to be worth asserting.
//
// THE PROPERTY. A store failure must reach the client as an UNSETTLED 5xx:
//
//   - 5xx, not 4xx. A 4xx would mean a broken database was mistaken for a
//     domain answer — the same absence-versus-failure confusion the store
//     packages were full of, and the way a write guard fails open.
//   - unsettled, so the idempotency key stays fresh. Replaying that key
//     after recovery must SUCCEED, which is what makes an agent's retry
//     safe. A settled 5xx would poison the key forever.
//
// So each operation is driven once per fault ordinal, and each failure is
// followed by a clean replay under the SAME key that has to succeed. That
// second half is what makes this more than a smoke test: it proves the
// transaction rolled back and nothing was recorded.

var sweepDBs atomic.Int64

// faultPair opens two servers over one database: one armed at the given
// ordinal, one clean. The armed handle is its own connector, so seeding
// through the clean one never consumes the fault.
func faultPair(t *testing.T, ordinal int) (armed, clean *httptest.Server, seed *sql.DB) {
	return faultPairMode(t, "any", ordinal)
}

// faultPairMode is faultPair with the fault kind chosen explicitly, for the
// synthetic modes that "any" excludes.
func faultPairMode(t *testing.T, mode string, ordinal int) (armed, clean *httptest.Server, seed *sql.DB) {
	t.Helper()
	name := fmt.Sprintf("sweep-%d", sweepDBs.Add(1))
	base := "file:" + name + "?mode=memory&cache=shared&_pragma=busy_timeout(5000)"

	open := func(dsn string) *sql.DB {
		db, err := sql.Open("sqlite-fault", dsn)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	// The clean handle is opened FIRST and kept for the test's lifetime:
	// a shared-cache database lives only as long as a handle on it.
	seed = open(base)
	cleanHandler, err := api.New(seed)
	if err != nil {
		t.Fatalf("wire clean api: %v", err)
	}
	clean = httptest.NewServer(cleanHandler)
	t.Cleanup(clean.Close)

	// Armed lazily: api.New runs the migrations, and counting those would
	// spend the ordinals before the operation under test even starts.
	armedDB := open(base + "&fault_op=" + mode + "&fault_after=" + strconv.Itoa(ordinal) +
		"&fault_arm_on=" + armMarker)
	armedHandler, err := api.New(armedDB)
	if err != nil {
		t.Fatalf("wire armed api: %v", err)
	}
	armed = httptest.NewServer(armedHandler)
	t.Cleanup(armed.Close)
	return armed, clean, armedDB
}

// armMarker appears in the statement that starts the fault counter. The
// sweep issues it once the world is prepared, so ordinal one is the first
// operation the request under test performs.
const armMarker = "faultsql_arm"

// faultsFired asks the driver how many faults have actually fired. The sweep
// needs it to tell "the ordinal was past the end of the operation" from "the
// fault fired and the handler SWALLOWED it" — and the second is the exact
// regression this sweep exists to catch, so a walk that cannot tell them
// apart stops early and passes (review 2108). That is not hypothetical: it
// is how the swallowed idempotency lookup hid through three iterations of
// this harness.
func faultsFired(t *testing.T, armedDB *sql.DB) int64 {
	t.Helper()
	var n int64
	if err := armedDB.QueryRow(`SELECT faultsql_fired()`).Scan(&n); err != nil {
		t.Fatalf("read fault count: %v", err)
	}
	return n
}

func armFaults(t *testing.T, armedDB *sql.DB) {
	t.Helper()
	if _, err := armedDB.Exec(`SELECT 1 /* ` + armMarker + ` */`); err != nil {
		t.Fatalf("arm the fault counter: %v", err)
	}
}

// do issues one request and returns its status and body.
func do(t *testing.T, srv *httptest.Server, method, path, key, body string) (int, string) {
	t.Helper()
	var payload io.Reader
	if body != "" {
		payload = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequest(method, srv.URL+path, payload)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp.StatusCode, string(raw)
}

// sweepWorld holds the ids a prepared database offers to the operation under
// test. Every operation is given a fully populated project, so no operation
// fails for want of something to act on — a 404 would prove nothing.
type sweepWorld struct {
	actor, project, issue, otherIssue, doc, version, label, thread, review string
	revision                                                               int64
	commit                                                                 string
}

// prepare fills a database through the CLEAN server and returns the ids.
func prepare(t *testing.T, clean *httptest.Server) sweepWorld {
	t.Helper()
	var w sweepWorld
	keys := 0
	next := func() string { keys++; return fmt.Sprintf("prep-%d", keys) }
	create := func(path, body string) string {
		t.Helper()
		status, out := do(t, clean, http.MethodPost, path, next(), body)
		if status != http.StatusCreated && status != http.StatusOK {
			t.Fatalf("prepare %s: %d %s", path, status, out)
		}
		var created struct {
			ID       string `json:"id"`
			Revision int64  `json:"revision"`
			Version  struct {
				ID string `json:"id"`
			} `json:"version"`
		}
		if err := json.Unmarshal([]byte(out), &created); err != nil {
			t.Fatalf("prepare %s: decode %v — %s", path, err, out)
		}
		if created.Version.ID != "" {
			w.version = created.Version.ID
		}
		if created.Revision != 0 {
			w.revision = created.Revision
		}
		return created.ID
	}

	w.actor = create("/identities", `{"handle":"operator","kind":"human"}`)
	repo, head := sweepRepo(t)
	w.commit = head
	w.project = create("/projects",
		`{"key":"SWP","name":"Sweep","actor":"`+w.actor+`","repo_path":"`+repo+`"}`)
	w.issue = create("/projects/"+w.project+"/issues",
		`{"title":"first","actor":"`+w.actor+`"}`)
	w.otherIssue = create("/projects/"+w.project+"/issues",
		`{"title":"second","actor":"`+w.actor+`"}`)
	w.doc = create("/projects/"+w.project+"/documents",
		`{"title":"doc","content":"body","author":"`+w.actor+`"}`)
	w.label = create("/labels", `{"name":"sweep"}`)
	w.thread = create("/threads",
		`{"title":"t","project":"`+w.project+`","actor":"`+w.actor+`","transcript":[{"speaker":"claude","text":"x"}]}`)
	w.review = create("/reviews",
		`{"issue":"`+w.issue+`","author":"`+w.actor+`","branch":"work","commit":"`+head+`"}`)
	return w
}

// sweepOp is one contract mutation, described so the sweep can replay it.
type sweepOp struct {
	name   string
	method string
	path   func(sweepWorld) string
	body   func(sweepWorld) string
}

func sweepOperations() []sweepOp {
	return []sweepOp{
		{"create identity", http.MethodPost,
			func(sweepWorld) string { return "/identities" },
			func(sweepWorld) string { return `{"handle":"another","kind":"agent"}` }},
		{"create project", http.MethodPost,
			func(sweepWorld) string { return "/projects" },
			func(w sweepWorld) string { return `{"key":"NEW","name":"New","actor":"` + w.actor + `"}` }},
		{"archive project", http.MethodPost,
			func(w sweepWorld) string { return "/projects/" + w.project + "/archive" },
			func(w sweepWorld) string { return `{"actor":"` + w.actor + `"}` }},
		{"create issue", http.MethodPost,
			func(w sweepWorld) string { return "/projects/" + w.project + "/issues" },
			func(w sweepWorld) string { return `{"title":"swept","actor":"` + w.actor + `"}` }},
		{"update issue", http.MethodPatch,
			func(w sweepWorld) string { return "/issues/" + w.issue },
			func(w sweepWorld) string { return `{"title":"renamed","actor":"` + w.actor + `"}` }},
		{"set issue status", http.MethodPost,
			func(w sweepWorld) string { return "/issues/" + w.issue + "/status" },
			func(w sweepWorld) string { return `{"status":"in-progress","actor":"` + w.actor + `"}` }},
		{"assign issue", http.MethodPost,
			func(w sweepWorld) string { return "/issues/" + w.issue + "/assign" },
			func(w sweepWorld) string { return `{"assignee":"` + w.actor + `","actor":"` + w.actor + `"}` }},
		{"add relation", http.MethodPost,
			func(w sweepWorld) string { return "/issues/" + w.issue + "/relations" },
			func(w sweepWorld) string {
				return `{"kind":"blocks","to":"` + w.otherIssue + `","actor":"` + w.actor + `"}`
			}},
		{"attach label", http.MethodPost,
			func(w sweepWorld) string { return "/issues/" + w.issue + "/labels" },
			func(w sweepWorld) string { return `{"label":"` + w.label + `","actor":"` + w.actor + `"}` }},
		{"create label", http.MethodPost,
			func(sweepWorld) string { return "/labels" },
			func(sweepWorld) string { return `{"name":"fresh"}` }},
		{"create document", http.MethodPost,
			func(w sweepWorld) string { return "/projects/" + w.project + "/documents" },
			func(w sweepWorld) string {
				return `{"title":"swept","content":"c","author":"` + w.actor + `"}`
			}},
		{"save doc version", http.MethodPost,
			func(w sweepWorld) string { return "/documents/" + w.doc + "/versions" },
			func(w sweepWorld) string { return `{"content":"revised","author":"` + w.actor + `"}` }},
		{"link doc to issue", http.MethodPost,
			func(w sweepWorld) string { return "/documents/" + w.doc + "/issue" },
			func(w sweepWorld) string { return `{"issue":"` + w.issue + `","actor":"` + w.actor + `"}` }},
		{"create comment", http.MethodPost,
			func(sweepWorld) string { return "/comments" },
			func(w sweepWorld) string {
				return `{"issue":"` + w.issue + `","author":"` + w.actor + `","body":"a comment"}`
			}},
		{"import thread", http.MethodPost,
			func(sweepWorld) string { return "/threads" },
			func(w sweepWorld) string {
				return `{"title":"swept","project":"` + w.project + `","actor":"` + w.actor +
					`","transcript":[{"speaker":"claude","text":"x"}]}`
			}},
		{"set thread anchor", http.MethodPost,
			func(w sweepWorld) string { return "/threads/" + w.thread + "/anchor" },
			func(w sweepWorld) string { return `{"issue":"` + w.issue + `","actor":"` + w.actor + `"}` }},
		{"create review", http.MethodPost,
			func(sweepWorld) string { return "/reviews" },
			func(w sweepWorld) string {
				return `{"issue":"` + w.otherIssue + `","author":"` + w.actor +
					`","branch":"work","commit":"` + w.commit + `"}`
			}},
		{"set review verdict", http.MethodPost,
			func(w sweepWorld) string { return "/reviews/" + w.review + "/verdict" },
			func(w sweepWorld) string {
				return `{"verdict":"approved","actor":"` + w.actor + `","revision":` +
					strconv.FormatInt(w.revision, 10) + `}`
			}},
		{"create template", http.MethodPost,
			func(sweepWorld) string { return "/templates" },
			func(sweepWorld) string { return `{"name":"swept","content":"c"}` }},
	}
}

// TestStoreFailuresAreUnsettled5xx is the sweep. For each operation it walks
// fault ordinals until the fault stops firing, and at every ordinal requires
// the two halves of the property.
func TestStoreFailuresAreUnsettled5xx(t *testing.T) {
	const maxOrdinals = 60
	for _, op := range sweepOperations() {
		t.Run(op.name, func(t *testing.T) {
			faults := 0
			for ordinal := 1; ordinal <= maxOrdinals; ordinal++ {
				armed, clean, armedDB := faultPair(t, ordinal)
				w := prepare(t, clean)
				armFaults(t, armedDB)
				key := fmt.Sprintf("%s-%d", op.name, ordinal)

				status, body := do(t, armed, op.method, op.path(w), key, op.body(w))
				fired := faultsFired(t, armedDB)

				if status < 400 {
					if fired > 0 {
						t.Fatalf("ordinal %d: the fault FIRED and the handler answered %d anyway — "+
							"a store failure was swallowed.\nbody: %s", ordinal, status, body)
					}
					// The fault never fired: this operation performs fewer
					// than `ordinal` database operations, so the walk is
					// done. Only the driver can say that.
					if ordinal == 1 {
						t.Fatal("the first fault did not fire; the sweep measured nothing")
					}
					return
				}
				if fired == 0 {
					t.Fatalf("ordinal %d answered %d without any fault firing — the failure has "+
						"another cause and this sweep is measuring the wrong thing.\nbody: %s",
						ordinal, status, body)
				}
				faults++

				if status < 500 {
					t.Fatalf("ordinal %d answered %d — a store failure reported as a domain answer.\n"+
						"That is a broken database mistaken for a real refusal, which is how a guard fails open.\nbody: %s",
						ordinal, status, body)
				}
				var envelope struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				}
				if err := json.Unmarshal([]byte(body), &envelope); err != nil || envelope.Code == "" {
					t.Fatalf("ordinal %d: a 5xx must still carry the contract's Error envelope, got: %s", ordinal, body)
				}

				// The other half: the key must be fresh, so the same request
				// through a clean server succeeds. A settled 5xx would
				// replay the failure forever.
				retryStatus, retryBody := do(t, clean, op.method, op.path(w), key, op.body(w))
				if retryStatus >= 400 {
					t.Fatalf("ordinal %d: the retry under the same key answered %d, so the failure was SETTLED — "+
						"the key is poisoned and no recovery can complete it.\nbody: %s",
						ordinal, retryStatus, retryBody)
				}
			}
			t.Fatalf("still failing at ordinal %d; raise maxOrdinals or the operation is looping", maxOrdinals)
		})
	}
}

func sweepRepo(t *testing.T) (path, head string) {
	t.Helper()
	repo := newGitRepo(t)
	return repo.path, repo.featureSHA
}

// TestUnreadableRowCountsAreUnsettled5xx sweeps the synthetic mode that
// fault_op=any deliberately excludes. SQLite never fails RowsAffected, so
// nothing else reaches these branches — and four store methods used to fold
// the failure into a domain answer, settling a 404 or a 409 for an outcome
// that was merely unknown (review 2108 named the issue-status path
// specifically). The property is the same as the main sweep's.
func TestUnreadableRowCountsAreUnsettled5xx(t *testing.T) {
	const maxOrdinals = 12
	ops := sweepOperations()
	var inert []string
	exercised := 0
	for _, op := range ops {
		hit := false
		t.Run(op.name, func(t *testing.T) {
			for ordinal := 1; ordinal <= maxOrdinals; ordinal++ {
				armed, clean, armedDB := faultPairMode(t, "rowsaffected", ordinal)
				w := prepare(t, clean)
				armFaults(t, armedDB)
				key := fmt.Sprintf("rows-%s-%d", op.name, ordinal)

				status, body := do(t, armed, op.method, op.path(w), key, op.body(w))
				fired := faultsFired(t, armedDB)

				if status < 400 {
					if fired > 0 {
						t.Fatalf("ordinal %d: an unreadable row count was swallowed and the handler answered %d.\nbody: %s",
							ordinal, status, body)
					}
					// Nothing fired AND the request succeeded: this
					// operation reads fewer than `ordinal` row counts, so
					// the walk is done.
					return
				}
				// A failure with nothing fired is not row-count coverage.
				// Returning here (as this sweep first did) let a broken
				// fixture or an unrelated handler regression end the walk
				// at ordinal 1, reporting a pass for an operation that had
				// never reached RowsAffected at all (review 2109).
				if fired == 0 {
					t.Fatalf("ordinal %d answered %d without any fault firing — the failure has another "+
						"cause and this sweep is measuring the wrong thing.\nbody: %s", ordinal, status, body)
				}
				hit = true

				if status < 500 {
					t.Fatalf("ordinal %d answered %d — an UNKNOWN row count reported as a definite domain answer. "+
						"The statement ran; only its count is unavailable, so a 404 or 409 here settles a lie.\nbody: %s",
						ordinal, status, body)
				}
				retryStatus, retryBody := do(t, clean, op.method, op.path(w), key, op.body(w))
				if retryStatus >= 400 {
					t.Fatalf("ordinal %d: the retry under the same key answered %d — the failure was settled.\nbody: %s",
						ordinal, retryStatus, retryBody)
				}
			}
			t.Fatalf("still failing at ordinal %d; raise maxOrdinals or the operation is looping", maxOrdinals)
		})
		if hit {
			exercised++
		} else {
			inert = append(inert, op.name)
		}
	}
	// Named rather than left silent: an operation that reads no row count is
	// a legitimate outcome here, but "the sweep covered everything" and "the
	// sweep found nothing to cover" must not look alike.
	t.Logf("row-count faults reached %d of %d operations; %d read no row count: %v",
		exercised, len(ops), len(inert), inert)
	if exercised == 0 {
		t.Fatal("no operation read a row count; this sweep measured nothing")
	}
}
