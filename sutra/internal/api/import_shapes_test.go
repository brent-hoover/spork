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
