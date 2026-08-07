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

// TestImportRejectsNonCanonicalPayloadIDs pins that identifiers INSIDE
// an event payload carry the same rule as record ids. Payload bytes are
// stored verbatim, so an uppercase or non-v7 identifier there would
// outlive the import and name an entity no lookup can find (1924).
func TestImportRejectsNonCanonicalPayloadIDs(t *testing.T) {
	const (
		project  = "01900000-0000-7000-8000-000000000000"
		actor    = "01900000-0000-7000-8000-00000000000f"
		issueA   = "01900000-0000-7000-8000-000000000001"
		issueB   = "01900000-0000-7000-8000-000000000002"
		relation = "01900000-0000-7000-8000-000000000003"
		eventID  = "01900000-0000-7000-8000-000000000004"
	)
	payload := func(rel string) string {
		return `{"project":{"id":"` + project + `","key":"K","name":"N"},
			"identities":[{"id":"` + actor + `","handle":"a","kind":"agent"}],
			"issues":[],"comments":[],"labels":[],"issue_relations":[],
			"documents":[],"threads":[],"reviews":[],
			"events":[{"id":"` + eventID + `","kind":"issue.relation-removed",
				"subject":"` + issueA + `","operation":"` + relation + `","actor":"` + actor + `",
				"created":"2026-08-07T00:00:00.000000000Z",
				"payload":{"relation":"` + rel + `","kind":"blocks","from":"` + issueA + `","to":"` + issueB + `"}}]}`
	}
	for _, tc := range []struct {
		name string
		rel  string
	}{
		{name: "uppercase", rel: "01900000-0000-7000-8000-00000000000A"},
		{name: "not v7", rel: "01900000-0000-4000-8000-000000000003"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meta, apiErr := collectImportMeta(strings.NewReader(payload(tc.rel)))
			if apiErr != nil {
				t.Fatalf("payload did not even parse: %s", apiErr.message)
			}
			if apiErr := validateImportShapes(meta); apiErr == nil {
				t.Fatalf("non-canonical payload identifier %q accepted", tc.rel)
			}
		})
	}
	// The canonical form still validates, so the guard rejects the
	// casing rather than the shape.
	meta, apiErr := collectImportMeta(strings.NewReader(payload(relation)))
	if apiErr != nil {
		t.Fatalf("canonical payload did not parse: %s", apiErr.message)
	}
	if apiErr := validateImportShapes(meta); apiErr != nil {
		t.Fatalf("canonical payload identifier rejected: %s", apiErr.message)
	}
}

// TestImportRejectsMisfiledRelationRemoved pins the contract's routing
// rule for this event: EventBase.subject IS the relation's former `from`
// issue, so a payload disagreeing with the subject would file the audit
// entry under an issue the payload says was not involved — and the
// per-issue audit is built on subject (review 1928).
func TestImportRejectsMisfiledRelationRemoved(t *testing.T) {
	const (
		project  = "01900000-0000-7000-8000-000000000000"
		actor    = "01900000-0000-7000-8000-00000000000f"
		issueA   = "01900000-0000-7000-8000-000000000001"
		issueB   = "01900000-0000-7000-8000-000000000002"
		relation = "01900000-0000-7000-8000-000000000003"
		eventID  = "01900000-0000-7000-8000-000000000004"
	)
	payload := func(subject string) string {
		return `{"project":{"id":"` + project + `","key":"K","name":"N"},
			"identities":[{"id":"` + actor + `","handle":"a","kind":"agent"}],
			"issues":[],"comments":[],"labels":[],"issue_relations":[],
			"documents":[],"threads":[],"reviews":[],
			"events":[{"id":"` + eventID + `","kind":"issue.relation-removed",
				"subject":"` + subject + `","operation":"` + relation + `","actor":"` + actor + `",
				"created":"2026-08-07T00:00:00.000000000Z",
				"payload":{"relation":"` + relation + `","kind":"blocks","from":"` + issueA + `","to":"` + issueB + `"}}]}`
	}
	// Subject is the payload's `to` — the detached destination, not the
	// source the contract names.
	meta, apiErr := collectImportMeta(strings.NewReader(payload(issueB)))
	if apiErr != nil {
		t.Fatalf("payload did not parse: %s", apiErr.message)
	}
	apiErr = validateImportShapes(meta)
	if apiErr == nil {
		t.Fatal("relation-removed event filed under the wrong issue was accepted")
	}
	if !strings.Contains(apiErr.message, "relation-removed") {
		t.Fatalf("rejection does not name the cause: %s", apiErr.message)
	}
	// Filed under the payload's `from`, it validates.
	meta, apiErr = collectImportMeta(strings.NewReader(payload(issueA)))
	if apiErr != nil {
		t.Fatalf("payload did not parse: %s", apiErr.message)
	}
	if apiErr := validateImportShapes(meta); apiErr != nil {
		t.Fatalf("correctly filed relation-removed rejected: %s", apiErr.message)
	}
}
