package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The import path validates a whole exported project, and its review rules
// are the densest part of it: contiguous submissions, agreement between a
// review and its latest submission, deliverable shapes that never mix.
//
// Every one of those rules was unreachable, for the reason species 9 names:
// the FIXTURES cannot violate them. The suite's only import documents were
// hand-written minimal ones and round-tripped exports, and a valid export
// satisfies every rule by construction. So each rule here starts from a
// valid document and breaks exactly one thing.
//
// A rule that no input violates is a rule no one has tested.

// importDocument returns a valid, importable export document and the actor
// that document contains.
func importDocument(t *testing.T) (*httptest.Server, string, string) {
	t.Helper()
	srv, _ := startAPI(t)
	w := prepare(t, srv)
	return srv, w.importDoc, w.importActor
}

// mutateImport decodes the document, hands it to fn, and re-encodes.
func mutateImport(t *testing.T, doc string, fn func(map[string]any)) string {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal([]byte(doc), &decoded); err != nil {
		t.Fatalf("decode the import document: %v", err)
	}
	fn(decoded)
	out, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("re-encode the import document: %v", err)
	}
	return string(out)
}

// reviewOf returns the first review and its first submission, so a case can
// break one field without restating the navigation.
func reviewOf(t *testing.T, doc map[string]any) (map[string]any, map[string]any) {
	t.Helper()
	reviews, ok := doc["reviews"].([]any)
	if !ok || len(reviews) == 0 {
		t.Fatal("the export carried no reviews; these rules cannot be reached")
	}
	r, ok := reviews[0].(map[string]any)
	if !ok {
		t.Fatalf("review 0 is not an object: %T", reviews[0])
	}
	subs, ok := r["submissions"].([]any)
	if !ok || len(subs) == 0 {
		t.Fatalf("review %v carried no submissions", r["id"])
	}
	sub, ok := subs[0].(map[string]any)
	if !ok {
		t.Fatalf("submission 0 is not an object: %T", subs[0])
	}
	return r, sub
}

func TestImportRejectsIncoherentReviews(t *testing.T) {
	srv, doc, actor := importDocument(t)

	// Each case breaks ONE rule and names the message that rule emits, so a
	// case that starts passing for a different reason still fails.
	cases := []struct {
		name   string
		broken func(*testing.T, map[string]any)
		want   string
	}{
		{"issue outside the export", func(t *testing.T, d map[string]any) {
			r, _ := reviewOf(t, d)
			r["issue"] = "0190aaaa-0001-7000-8000-000000000001"
		}, "names an issue outside the export"},

		{"no submissions", func(t *testing.T, d map[string]any) {
			r, _ := reviewOf(t, d)
			r["submissions"] = []any{}
		}, "carries no submissions"},

		{"revision span disagrees", func(t *testing.T, d map[string]any) {
			r, _ := reviewOf(t, d)
			r["revision"] = float64(7)
		}, "do not span revisions"},

		{"revisions not contiguous", func(t *testing.T, d map[string]any) {
			_, sub := reviewOf(t, d)
			sub["revision"] = float64(2)
		}, "not contiguous"},

		// SHADOWED, and the shadowing is the finding. import.go:1077
		// ("submission %s belongs to another review") cannot be reached
		// from the wire: submissions are grouped BY their review field
		// before validation, so re-pointing one removes it from this
		// review rather than leaving it mismatched, and the empty-set rule
		// answers first. Giving the review a second submission does not
		// help either — the revision-span rule then answers first. Pinned
		// here as what actually happens; the arm itself belongs to the
		// unreachable residue.
		{"submission belongs elsewhere", func(t *testing.T, d map[string]any) {
			r, sub := reviewOf(t, d)
			reviews := d["reviews"].([]any)
			for _, other := range reviews {
				o := other.(map[string]any)
				if o["id"] != r["id"] {
					sub["review"] = o["id"]
					return
				}
			}
			t.Fatal("the export carried only one review; this rule needs two")
		}, "carries no submissions"},

		{"code submission without base_commit", func(t *testing.T, d map[string]any) {
			_, sub := reviewOf(t, d)
			delete(sub, "base_commit")
		}, "must carry branch, commit, and base_commit together"},

		{"malformed object id", func(t *testing.T, d map[string]any) {
			_, sub := reviewOf(t, d)
			sub["commit"] = "not-a-sha"
		}, "malformed object id"},

		{"code submission without content", func(t *testing.T, d map[string]any) {
			_, sub := reviewOf(t, d)
			delete(sub, "content")
		}, "omits its content"},

		{"mixed deliverable shapes", func(t *testing.T, d map[string]any) {
			_, sub := reviewOf(t, d)
			sub["doc_version"] = "0190aaaa-0003-7000-8000-000000000003"
		}, "deliverable shape is invalid"},

		// Also shadowed: the decode-time shape check at import.go:912
		// refuses a submission carrying neither shape, so the
		// "names no resolvable deliverable" arm below it sees only inputs
		// that already passed that gate.
		{"doc submission with no deliverable", func(t *testing.T, d map[string]any) {
			_, sub := reviewOf(t, d)
			delete(sub, "branch")
			delete(sub, "commit")
			delete(sub, "doc_version")
			delete(sub, "base_commit")
			delete(sub, "content")
		}, "deliverable shape is invalid"},
		// THREE rules are shadowed, not two: this one reaches
		// validateImportShapes at import.go:912 rather than
		// validateImportedReview's "mixes deliverable shapes" at
		// import.go:1094, which is therefore unreachable too (review 2122).

		{"doc submission naming an absent version", func(t *testing.T, d map[string]any) {
			_, sub := reviewOf(t, d)
			delete(sub, "branch")
			delete(sub, "commit")
			delete(sub, "base_commit")
			delete(sub, "content")
			sub["doc_version"] = "0190aaaa-0004-7000-8000-000000000004"
		}, "names no resolvable deliverable"},

		{"top-level deliverable disagrees", func(t *testing.T, d map[string]any) {
			r, _ := reviewOf(t, d)
			r["branch"] = "a-different-branch"
		}, "disagrees with its latest submission"},

		{"session disagrees", func(t *testing.T, d map[string]any) {
			r, _ := reviewOf(t, d)
			r["session"] = "0190aaaa-0005-7000-8000-000000000005"
		}, "session disagrees with its latest submission"},
	}

	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mutated := mutateImport(t, doc, func(d map[string]any) { c.broken(t, d) })
			key := fmt.Sprintf("malformed-%d", i)
			status, body := do(t, srv, http.MethodPost, "/projects/import?actor="+actor, key, mutated)
			if status != http.StatusBadRequest {
				t.Fatalf("expected 400 for %s, got %d\nbody: %s", c.name, status, body)
			}
			var envelope struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal([]byte(body), &envelope); err != nil {
				t.Fatalf("the rejection must carry the contract's Error envelope: %s", body)
			}
			if envelope.Code != "malformed-import" {
				t.Fatalf("expected code malformed-import, got %q: %s", envelope.Code, body)
			}
			if !strings.Contains(envelope.Message, c.want) {
				t.Fatalf("expected a message containing %q, got: %s", c.want, envelope.Message)
			}
		})
	}
}
