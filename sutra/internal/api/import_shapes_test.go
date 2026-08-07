package api

import (
	"strings"
	"testing"

	"sutra/internal/projects"
)

// TestImportShapeShortID pins that malformed ids — short, empty, or
// wrong-version — reject as malformed instead of panicking on fixed
// offsets (review 1847).
func TestImportShapeShortID(t *testing.T) {
	for _, id := range []string{"", "abc", "0190", strings.Repeat("0", 36),
		"01900000-0000-4000-8000-000000000000", // v4, not v7
		"01900000-0000-7000-7000-000000000000", // bad variant
	} {
		p := &importPayload{Project: projects.Project{ID: id, Key: "K", Name: "N"}}
		apiErr := validateImportShapes(p)
		if apiErr == nil || apiErr.code != "malformed-import" {
			t.Fatalf("id %q must reject as malformed-import, got %+v", id, apiErr)
		}
	}
	good := &importPayload{Project: projects.Project{ID: "01900000-0000-7000-8000-000000000000", Key: "K", Name: "N"}}
	if apiErr := validateImportShapes(good); apiErr != nil {
		t.Fatalf("valid v7 project rejected: %s", apiErr.message)
	}
}

// TestImportRejectsUnknownLabelFields pins that the embedded labels
// array decodes as strictly as every other record in the walker: the
// Label schema is closed, so an extra property must reject rather than
// be silently dropped and vanish on re-export (review 1908).
func TestImportRejectsUnknownLabelFields(t *testing.T) {
	payload := func(labels string) string {
		return `{"project":{"id":"01900000-0000-7000-8000-000000000000","key":"K","name":"N"},
			"identities":[],"issues":[{"id":"01900000-0000-7000-8000-000000000001",
			"project":"01900000-0000-7000-8000-000000000000","number":1,"status":"open",
			"created":"2026-08-07T00:00:00.000000000Z","updated":"2026-08-07T00:00:00.000000000Z",
			"subtree_revision":0,"title":"t","labels":` + labels + `}],
			"comments":[],"labels":[],"issue_relations":[],"documents":[],"threads":[],
			"reviews":[],"events":[]}`
	}
	known := `[{"id":"01900000-0000-7000-8000-00000000000a","name":"bug","color":"#fff"}]`
	if _, apiErr := collectImportMeta(strings.NewReader(payload(known))); apiErr != nil {
		t.Fatalf("valid label rejected: %s", apiErr.message)
	}
	unknown := `[{"id":"01900000-0000-7000-8000-00000000000a","name":"bug","color":"#fff","extra":1}]`
	_, apiErr := collectImportMeta(strings.NewReader(payload(unknown)))
	if apiErr == nil {
		t.Fatal("unknown label property accepted")
	}
	if !strings.Contains(apiErr.message, "extra") {
		t.Fatalf("rejection does not name the unknown property: %s", apiErr.message)
	}
}
