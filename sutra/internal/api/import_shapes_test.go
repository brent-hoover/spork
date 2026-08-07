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
			"issues":[
				{"id":"` + issueA + `","project":"` + project + `","number":1,"status":"open","title":"a",
				 "created":"2026-08-07T00:00:00.000000000Z","updated":"2026-08-07T00:00:00.000000000Z","subtree_revision":0},
				{"id":"` + issueB + `","project":"` + project + `","number":2,"status":"open","title":"b",
				 "created":"2026-08-07T00:00:00.000000000Z","updated":"2026-08-07T00:00:00.000000000Z","subtree_revision":0}],"comments":[],"labels":[],"issue_relations":[],
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
			"issues":[
				{"id":"` + issueA + `","project":"` + project + `","number":1,"status":"open","title":"a",
				 "created":"2026-08-07T00:00:00.000000000Z","updated":"2026-08-07T00:00:00.000000000Z","subtree_revision":0},
				{"id":"` + issueB + `","project":"` + project + `","number":2,"status":"open","title":"b",
				 "created":"2026-08-07T00:00:00.000000000Z","updated":"2026-08-07T00:00:00.000000000Z","subtree_revision":0}],"comments":[],"labels":[],"issue_relations":[],
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

// TestImportRejectsDanglingRelationSnapshot pins that a relation-removed
// snapshot names issues this export actually carries — except a removed
// cross-project BLOCKS relation, whose destination is legitimately
// foreign and whose rejection would make a valid export unimportable
// (review 1932).
func TestImportRejectsDanglingRelationSnapshot(t *testing.T) {
	const (
		project = "01900000-0000-7000-8000-000000000000"
		actor   = "01900000-0000-7000-8000-00000000000f"
		issueA  = "01900000-0000-7000-8000-000000000001"
		foreign = "01900000-0000-7000-8000-0000000000ff"
		relID   = "01900000-0000-7000-8000-000000000003"
		eventID = "01900000-0000-7000-8000-000000000004"
	)
	payload := func(kind, subject, from, to string) string {
		return `{"project":{"id":"` + project + `","key":"K","name":"N"},
			"identities":[{"id":"` + actor + `","handle":"a","kind":"agent"}],
			"issues":[{"id":"` + issueA + `","project":"` + project + `","number":1,"status":"open","title":"a",
				"created":"2026-08-07T00:00:00.000000000Z","updated":"2026-08-07T00:00:00.000000000Z","subtree_revision":0}],
			"comments":[],"labels":[],"issue_relations":[],
			"documents":[],"threads":[],"reviews":[],
			"events":[{"id":"` + eventID + `","kind":"issue.relation-removed",
				"subject":"` + subject + `","operation":"` + relID + `","actor":"` + actor + `",
				"created":"2026-08-07T00:00:00.000000000Z",
				"payload":{"relation":"` + relID + `","kind":"` + kind + `","from":"` + from + `","to":"` + to + `"}}]}`
	}
	check := func(t *testing.T, body string, wantReject bool) {
		t.Helper()
		meta, apiErr := collectImportMeta(strings.NewReader(body))
		if apiErr != nil {
			t.Fatalf("payload did not parse: %s", apiErr.message)
		}
		apiErr = validateImportShapes(meta)
		if wantReject && apiErr == nil {
			t.Fatal("dangling relation snapshot accepted")
		}
		if !wantReject && apiErr != nil {
			t.Fatalf("valid snapshot rejected: %s", apiErr.message)
		}
	}
	// A source this export does not carry is dangling — and since the
	// source is also the subject, the event is filed under nothing.
	t.Run("foreign source", func(t *testing.T) {
		check(t, payload("blocks", foreign, foreign, issueA), true)
	})
	// parent_of cannot cross projects, so a foreign child is dangling.
	t.Run("foreign parent_of child", func(t *testing.T) {
		check(t, payload("parent_of", issueA, issueA, foreign), true)
	})
	// A removed CROSS-PROJECT block legitimately names a foreign
	// destination; the export carries the event because its subject is
	// in scope.
	t.Run("foreign blocks destination", func(t *testing.T) {
		check(t, payload("blocks", issueA, issueA, foreign), false)
	})
}

// TestImportMetaStripsUnreadStrings pins WHICH strings the validation
// pass keeps. Anything it does not read collapses to a presence
// sentinel, so pass A's memory is bounded by the fields validation
// actually compares — pass B re-reads the originals from the spool, so
// nothing is lost (review 1934).
func TestImportMetaStripsUnreadStrings(t *testing.T) {
	const (
		project = "01900000-0000-7000-8000-000000000000"
		actor   = "01900000-0000-7000-8000-00000000000f"
		issueA  = "01900000-0000-7000-8000-000000000001"
	)
	big := strings.Repeat("t", 4096)
	body := `{"project":{"id":"` + project + `","key":"K","name":"N","description":"` + big + `","repo_path":"` + big + `"},
		"identities":[{"id":"` + actor + `","handle":"a","kind":"agent","display_name":"` + big + `"}],
		"issues":[{"id":"` + issueA + `","project":"` + project + `","number":1,"status":"open","title":"` + big + `",
			"created":"2026-08-07T00:00:00.000000000Z","updated":"2026-08-07T00:00:00.000000000Z","subtree_revision":0}],
		"comments":[],"labels":[],"issue_relations":[],"documents":[],"threads":[],"reviews":[],"events":[]}`

	meta, apiErr := collectImportMeta(strings.NewReader(body))
	if apiErr != nil {
		t.Fatalf("payload did not parse: %s", apiErr.message)
	}
	retained := map[string]string{
		"issue title":         meta.Issues[0].Title,
		"identity display":    derefOr(meta.Identities[0].DisplayName),
		"project description": derefOr(meta.Project.Description),
		"project repo_path":   derefOr(meta.Project.RepoPath),
	}
	for what, got := range retained {
		if len(got) > 1 {
			t.Errorf("%s retained %d bytes; validation never reads it", what, len(got))
		}
		if got == "" {
			t.Errorf("%s lost its presence; validation checks non-emptiness", what)
		}
	}
	// The handle IS compared for uniqueness, so it must survive intact.
	if meta.Identities[0].Handle != "a" {
		t.Errorf("handle was stripped, but uniqueness checks compare it: %q", meta.Identities[0].Handle)
	}
	// And the payload still validates — stripping preserves presence.
	if apiErr := validateImportShapes(meta); apiErr != nil {
		t.Fatalf("stripped metadata no longer validates: %s", apiErr.message)
	}
}

func derefOr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
