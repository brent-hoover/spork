package api

import (
	"strings"
	"testing"
)

// skeletonExport wraps a reviews array in the nine other top-level
// fields walkImport requires. Only the reviews matter here; the rest are
// present so the stream reaches them.
func skeletonExport(reviews string) string {
	return `{"project":{},"identities":[],"issues":[],"comments":[],"labels":[],` +
		`"issue_relations":[],"documents":[],"threads":[],"events":[],"reviews":` + reviews + `}`
}

// TestCollectImportMetaBoundsSubmissionContent pins the metadata pass's
// bargain: it must hold the payload's SHAPE without holding its bulk.
// Validation reads only whether a submission's content is present —
// absent means the deliverable is a doc version, present-but-empty is a
// legitimate empty diff — so the value itself is replaced by a short
// sentinel and re-read from the spool in pass B. Keep the value and a
// multi-gigabyte export sits in memory twice while it validates; strip
// it too eagerly and an empty diff becomes indistinguishable from an
// absent one. Nothing in the response distinguishes the three, which is
// why no scenario can reach this.
func TestCollectImportMetaBoundsSubmissionContent(t *testing.T) {
	const stamp = `"2026-08-08T00:00:00Z"`
	huge := strings.Repeat("d", 1<<16)
	reviews := `[{"id":"rv-1","submissions":[` +
		`{"id":"s-1","review":"rv-1","revision":1,"content":"` + huge + `","created":` + stamp + `},` +
		`{"id":"s-2","review":"rv-1","revision":2,"content":"","created":` + stamp + `},` +
		`{"id":"s-3","review":"rv-1","revision":3,"created":` + stamp + `}` +
		`]}]`

	meta, apiErr := collectImportMeta(strings.NewReader(skeletonExport(reviews)))
	if apiErr != nil {
		t.Fatalf("collect: %s", apiErr.message)
	}
	if len(meta.Reviews) != 1 || len(meta.Reviews[0].Submissions) != 3 {
		t.Fatalf("expected one review carrying three submissions, got %+v", meta.Reviews)
	}
	subs := meta.Reviews[0].Submissions

	switch {
	case subs[0].Content == nil:
		t.Fatal("a submission's content went absent; validation would call it a doc deliverable")
	case len(*subs[0].Content) >= len(huge):
		t.Fatalf("unbounded content survived the metadata pass: %d bytes held", len(*subs[0].Content))
	}
	if subs[1].Content == nil || *subs[1].Content != "" {
		t.Fatalf("an empty diff did not stay empty: %v", subs[1].Content)
	}
	if subs[2].Content != nil {
		t.Fatalf("an absent content materialized as %q; the submission would read as a code deliverable", *subs[2].Content)
	}
}
