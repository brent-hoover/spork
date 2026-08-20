package api_test

import (
	"encoding/json"
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
		{"list relations", func(w sweepWorld) string { return "/issues/" + w.issue + "/relations" }},
		{"list reviews", func(w sweepWorld) string { return "/reviews?issue=" + w.issue }},
		{"get review", func(w sweepWorld) string { return "/reviews/" + w.review }},
		{"get deliverable", func(w sweepWorld) string { return "/reviews/" + w.review + "/deliverable" }},
		{"list project documents", func(w sweepWorld) string { return "/projects/" + w.project + "/documents" }},
		{"list issue documents", func(w sweepWorld) string { return "/issues/" + w.issue + "/documents" }},
		{"list issue events", func(w sweepWorld) string { return "/issues/" + w.issue + "/events" }},
		{"list comments", func(w sweepWorld) string { return "/comments?issue=" + w.issue }},
		{"list labels", func(sweepWorld) string { return "/labels" }},
		{"search threads", func(sweepWorld) string { return "/threads/search?q=x" }},
		{"search", func(sweepWorld) string { return "/search?q=x" }},
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
				armed, clean, armedDB := faultPair(t, ordinal)
				w := prepare(t, clean)
				armFaults(t, armedDB)

				status, body := do(t, armed, http.MethodGet, op.path(w), "", "")
				fired := faultsFired(t, armedDB)

				if fired == 0 {
					if status >= 400 {
						t.Fatalf("ordinal %d answered %d with no fault firing — the failure has another "+
							"cause and this sweep is measuring the wrong thing.\nbody: %s", ordinal, status, body)
					}
					if ordinal == 1 {
						t.Fatal("the first fault did not fire; this read was never measured")
					}
					depth[op.name] = faults
					return
				}
				faults++

				if status >= 500 {
					continue // failed before committing to a status
				}
				if status >= 400 {
					t.Fatalf("ordinal %d answered %d — a store failure reported as a domain answer. "+
						"A broken database is not a 404.\nbody: %s", ordinal, status, body)
				}
				// A 2xx while a fault fired: the only acceptable shape is a
				// body the caller cannot mistake for a complete answer.
				var parsed any
				if err := json.Unmarshal([]byte(body), &parsed); err == nil {
					t.Fatalf("ordinal %d answered %d with WELL-FORMED json while a store failure fired.\n"+
						"A truncated list that still parses is read as a short one, so the caller learns "+
						"nothing broke.\nbody: %s", ordinal, status, body)
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
