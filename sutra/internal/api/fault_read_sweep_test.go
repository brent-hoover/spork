package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// The mutation sweep asserts a store failure never SETTLES. Reads need a
// different property, because most of these handlers STREAM: once the first
// bytes are out the status line is already sent, and a store failure after
// that cannot become a 5xx however well the handler behaves.
//
// What must never happen is the quiet version. A truncated list that is
// still well-formed JSON is indistinguishable from a short one, so a caller
// reads "no more issues" from what was really "the database broke". That is
// the same absence-versus-failure confusion the store bugs were, moved into
// the wire format.
//
// So each read may answer three ways under an injected failure:
//   - a 5xx, if it failed before committing to a status;
//   - a 2xx whose body is NOT parseable, so the caller sees the break;
//   - nothing at all, if the ordinal ran past the operation's reads.
//
// A 2xx carrying well-formed JSON while a fault fired is the failure, and so
// is any 4xx: a broken database is not a domain answer.
// ok2xx is the success band, stated explicitly. "below 400" also admits a
// 3xx, which would let a redirect terminate the walk or pass as an
// acceptable answer under an injected failure (review 2121).
func ok2xx(status int) bool { return status >= 200 && status < 300 }

type readOp struct {
	name string
	path func(sweepWorld) string
}

func readOperations() []readOp {
	return []readOp{
		{"list identities", func(sweepWorld) string { return "/identities" }},
		{"list projects", func(sweepWorld) string { return "/projects" }},
		{"get project", func(w sweepWorld) string { return "/projects/" + w.project }},
		{"list events", func(sweepWorld) string { return "/events" }},
		{"list project issues", func(w sweepWorld) string { return "/projects/" + w.project + "/issues" }},
		{"get issue", func(w sweepWorld) string { return "/issues/" + w.issue }},
		// relIssue, not issue: the prepared relation hangs from relIssue, so
		// reading issue's relations swept an EMPTY collection (2120).
		{"list relations", func(w sweepWorld) string { return "/issues/" + w.relIssue + "/relations" }},
		{"list reviews", func(w sweepWorld) string { return "/reviews?issue=" + w.issue }},
		{"get review", func(w sweepWorld) string { return "/reviews/" + w.review }},
		{"get deliverable", func(w sweepWorld) string { return "/reviews/" + w.review + "/deliverable" }},
		{"list project documents", func(w sweepWorld) string { return "/projects/" + w.project + "/documents" }},
		{"list issue documents", func(w sweepWorld) string { return "/issues/" + w.issue + "/documents" }},
		{"list issue events", func(w sweepWorld) string { return "/issues/" + w.issue + "/events" }},
		{"list comments", func(w sweepWorld) string { return "/comments?issue=" + w.issue }},
		{"list labels", func(sweepWorld) string { return "/labels" }},
		{"search threads", func(sweepWorld) string { return "/threads/search?q=findable" }},
		{"search", func(sweepWorld) string { return "/search?q=findable" }},
		{"export project", func(w sweepWorld) string { return "/projects/" + w.project + "/export" }},
		{"get thread", func(w sweepWorld) string { return "/threads/" + w.thread }},
		{"list issue threads", func(w sweepWorld) string { return "/issues/" + w.issue + "/threads" }},
		{"get document", func(w sweepWorld) string { return "/documents/" + w.doc }},
		{"get document meta", func(w sweepWorld) string { return "/documents/" + w.doc + "/meta" }},
		{"list document versions", func(w sweepWorld) string { return "/documents/" + w.doc + "/versions" }},
		{"get doc version", func(w sweepWorld) string { return "/doc-versions/" + w.version }},
		{"diff document", func(w sweepWorld) string {
			return "/documents/" + w.doc + "/diff?from=1&to=2"
		}},
		{"list templates", func(sweepWorld) string { return "/templates" }},
		{"get template", func(w sweepWorld) string { return "/templates/" + w.template }},
	}
}

func TestReadFailuresAreNeverQuiet(t *testing.T) {
	// Exporting a project is the deepest read in the contract at 251
	// database operations — it walks every collection — so the bound is set
	// above that rather than at the mutation sweep's 150.
	const maxOrdinals = 400
	depth := map[string]int{}
	for _, op := range readOperations() {
		t.Run(op.name, func(t *testing.T) {
			faults := 0
			for ordinal := 1; ordinal <= maxOrdinals; ordinal++ {
				// Each ordinal gets its OWN scope. Registering the server,
				// the database handles and the git repository on the
				// operation's t held every fixture open until the whole
				// walk finished — around 260 pairs at once for the export
				// — which is a file-descriptor exhaustion waiting to
				// happen (review 2120).
				past := false
				t.Run(fmt.Sprintf("ordinal-%d", ordinal), func(t *testing.T) {
					armed, clean, armedDB := faultPair(t, ordinal)
					w := prepare(t, clean)
					armFaults(t, armedDB)

					status, body := do(t, armed, http.MethodGet, op.path(w), "", "")
					fired := faultsFired(t, armedDB)

					if fired == 0 {
						if !ok2xx(status) {
							t.Fatalf("ordinal %d answered %d with no fault firing — the failure has another "+
								"cause and this sweep is measuring the wrong thing.\nbody: %s", ordinal, status, body)
						}
						if ordinal == 1 {
							t.Fatal("the first fault did not fire; this read was never measured")
						}
						past = true
						return
					}
					faults++

					if status >= 500 {
						return // failed before committing to a status
					}
					if !ok2xx(status) {
						t.Fatalf("ordinal %d answered %d — a store failure reported as a domain answer. "+
							"A broken database is not a 404, and it is not a redirect.\nbody: %s", ordinal, status, body)
					}
					// A 2xx while a fault fired: the only acceptable shape
					// is a body the caller cannot mistake for a complete
					// answer.
					var parsed any
					if err := json.Unmarshal([]byte(body), &parsed); err == nil {
						t.Fatalf("ordinal %d answered %d with WELL-FORMED json while a store failure fired.\n"+
							"A truncated list that still parses is read as a short one, so the caller learns "+
							"nothing broke.\nbody: %s", ordinal, status, body)
					}
				})
				if t.Failed() {
					return
				}
				if past {
					depth[op.name] = faults
					return
				}
			}
			t.Fatalf("still failing at ordinal %d; raise maxOrdinals or the read is looping", maxOrdinals)
		})
	}
	total, deepest, deepestName := 0, 0, ""
	for name, n := range depth {
		total += n
		if n > deepest {
			deepest, deepestName = n, name
		}
	}
	t.Logf("swept %d read failures across %d operations; deepest is %q at %d (bound %d)",
		total, len(depth), deepestName, deepest, maxOrdinals)
}

// TestUnscannableRowsAreNeverQuiet is the companion the main read sweep
// cannot be: fault_op=any deliberately excludes the synthetic modes, so a
// handler could mishandle a Scan failure while every other read sweep
// passed. badrow makes a whole result set unscannable, which is the shape a
// column mismatch takes.
//
// The property is the main read sweep's, unchanged — a store failure must
// never arrive as a well-formed short answer.
func TestUnscannableRowsAreNeverQuiet(t *testing.T) {
	// Export opens a cursor per collection and is far the deepest read, so
	// this shares the read sweep's bound rather than a smaller one.
	const maxOrdinals = 400
	reached := 0
	var inert []string
	for _, op := range readOperations() {
		hit := false
		t.Run(op.name, func(t *testing.T) {
			for ordinal := 1; ordinal <= maxOrdinals; ordinal++ {
				past := false
				t.Run(fmt.Sprintf("ordinal-%d", ordinal), func(t *testing.T) {
					armed, clean, armedDB := faultPairMode(t, "badrow", ordinal)
					w := prepare(t, clean)
					armFaults(t, armedDB)

					status, body := do(t, armed, http.MethodGet, op.path(w), "", "")
					fired := faultsFired(t, armedDB)

					// Termination is by ordinals MATCHED, not by faults
					// manifested. badrow corrupts rows, so an ordinal
					// landing on an empty cursor — the events feed opens a
					// LIMIT 0 bounds probe — selects without injecting.
					// Stopping there would end the walk on the first such
					// cursor and skip everything past it.
					if faultsTripped(t, armedDB) == 0 {
						if !ok2xx(status) {
							t.Fatalf("ordinal %d answered %d with no ordinal matching — the failure has "+
								"another cause.\nbody: %s", ordinal, status, body)
						}
						past = true
						return
					}
					if fired == 0 {
						return // selected an empty cursor; nothing was injected
					}
					hit = true
					if status >= 500 {
						return
					}
					if !ok2xx(status) {
						t.Fatalf("ordinal %d answered %d — an unscannable row reported as a domain answer.\nbody: %s",
							ordinal, status, body)
					}
					var parsed any
					if err := json.Unmarshal([]byte(body), &parsed); err == nil {
						t.Fatalf("ordinal %d answered %d with WELL-FORMED json while a row was unscannable.\n"+
							"The caller cannot tell a truncated answer from a short one.\nbody: %s",
							ordinal, status, body)
					}
				})
				if t.Failed() {
					return
				}
				if past {
					return
				}
			}
			t.Fatalf("still failing at ordinal %d; raise maxOrdinals or the read is looping", maxOrdinals)
		})
		if hit {
			reached++
		} else {
			inert = append(inert, op.name)
		}
	}
	// Named rather than silent: a read that opens no cursor is a legitimate
	// outcome, but it must not look like coverage.
	t.Logf("badrow reached %d of %d reads; %d open no cursor: %v",
		reached, len(readOperations()), len(inert), inert)
	if reached == 0 {
		t.Fatal("no read opened a cursor; this sweep measured nothing")
	}
}
