package acceptance_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"

	"github.com/cucumber/godog"

	"sutra/internal/api"
)

// importTarget is a second, empty server — the destination of import
// scenarios.
type importTarget struct {
	db     *sql.DB
	server *httptest.Server
}

var importTargetSeq int

func newImportTarget() (*importTarget, error) {
	importTargetSeq++
	db, err := sql.Open("sqlite", fmt.Sprintf("file:importtarget%d?mode=memory&cache=shared&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)", importTargetSeq))
	if err != nil {
		return nil, err
	}
	handler, err := api.New(db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &importTarget{db: db, server: httptest.NewServer(handler)}, nil
}

func (t *importTarget) close() {
	if t.server != nil {
		t.server.Close()
	}
	if t.db != nil {
		_ = t.db.Close()
	}
}

// ieWorld drives REQ-import-export.
type ieWorld struct {
	iw *issueWorld
	cw *closeWorld

	recordIDs   map[string]string // kind -> id minted while seeding
	exported    []byte
	tampered    []byte
	target      *importTarget
	lastStatus  int
	lastBody    string
	actorForImp string
}

func (ie *ieWorld) reset() {
	if ie.target != nil {
		ie.target.close()
	}
	*ie = ieWorld{iw: ie.iw, cw: ie.cw, recordIDs: map[string]string{}}
}

// seedRichProject builds identities, issues, a comment, a doc with two
// versions, a thread, a consumed-approval review plus a plain one, and
// their events.
func (ie *ieWorld) seedRichProject() error {
	iw, cw := ie.iw, ie.cw
	if err := cw.ensureGitProject(); err != nil {
		return err
	}
	issueID, err := iw.ensureIssue("SUT-1")
	if err != nil {
		return err
	}
	ie.recordIDs["issue"] = issueID
	author, err := iw.identity("human-brent")
	if err != nil {
		return err
	}
	if err := iw.s.call(http.MethodPost, "/comments",
		map[string]any{"issue": issueID, "author": author, "body": "exported comment"}); err != nil {
		return err
	}
	if err := iw.s.expectStatus(http.StatusCreated); err != nil {
		return err
	}
	var comment struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(iw.s.lastBody, &comment); err != nil {
		return err
	}
	ie.recordIDs["comment"] = comment.ID

	if err := iw.s.call(http.MethodPost, "/projects/"+iw.project+"/documents",
		map[string]string{"title": "exported doc", "content": "v1 body", "author": author}); err != nil {
		return err
	}
	if err := iw.s.expectStatus(http.StatusCreated); err != nil {
		return err
	}
	var doc struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(iw.s.lastBody, &doc); err != nil {
		return err
	}
	ie.recordIDs["document"] = doc.ID
	if err := iw.s.call(http.MethodPost, "/documents/"+doc.ID+"/versions",
		map[string]string{"content": "v2 body", "author": author}); err != nil {
		return err
	}
	if err := iw.s.expectStatus(http.StatusCreated); err != nil {
		return err
	}

	transcript, err := json.Marshal([]map[string]string{{"speaker": "claude", "text": "export me"}})
	if err != nil {
		return err
	}
	if err := iw.s.call(http.MethodPost, "/threads", map[string]any{
		"title": "exported thread", "transcript": json.RawMessage(transcript),
		"project": iw.project, "actor": iw.identities["operator"]}); err != nil {
		return err
	}
	if err := iw.s.expectStatus(http.StatusCreated); err != nil {
		return err
	}
	var thread struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(iw.s.lastBody, &thread); err != nil {
		return err
	}
	ie.recordIDs["thread"] = thread.ID

	// A removed relation: its relation-removed event (flat
	// RelationRemovedPayload) must survive the round trip.
	if _, err := iw.ensureIssue("SUT-2"); err != nil {
		return err
	}
	actor := iw.identities["operator"]
	if err := iw.s.call(http.MethodPost, "/issues/"+issueID+"/relations",
		map[string]string{"to": iw.issues["SUT-2"], "kind": "blocks", "actor": actor}); err != nil {
		return err
	}
	if err := iw.s.expectStatus(http.StatusCreated); err != nil {
		return err
	}
	var created struct {
		Relation struct {
			ID string `json:"id"`
		} `json:"relation"`
	}
	if err := json.Unmarshal(iw.s.lastBody, &created); err != nil {
		return err
	}
	if err := iw.s.call(http.MethodDelete, "/issues/"+issueID+"/relations/"+created.Relation.ID+"?actor="+actor, nil); err != nil {
		return err
	}
	if err := iw.s.expectStatus(http.StatusNoContent); err != nil {
		return err
	}

	// A doc-deliverable review: its submission carries NO stored
	// content (the immutable version resolves), and the round trip
	// must carry it (review 1841 regression).
	if err := iw.s.call(http.MethodGet, "/documents/"+doc.ID, nil); err != nil {
		return err
	}
	var docView struct {
		Version struct {
			ID string `json:"id"`
		} `json:"version"`
	}
	if err := json.Unmarshal(iw.s.lastBody, &docView); err != nil {
		return err
	}
	if err := iw.s.call(http.MethodPost, "/reviews", map[string]string{
		"issue": iw.issues["SUT-2"], "author": iw.identities["operator"], "doc_version": docView.Version.ID}); err != nil {
		return err
	}
	if err := iw.s.expectStatus(http.StatusCreated); err != nil {
		return err
	}
	var docReview struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(iw.s.lastBody, &docReview); err != nil {
		return err
	}
	ie.recordIDs["doc-review"] = docReview.ID

	// A consumed approval: approve, then consume at that revision.
	if err := cw.approvedReview("SUT-1", "exported"); err != nil {
		return err
	}
	ref := cw.reviews["SUT-1"]
	ie.recordIDs["review"] = ref.id
	if err := iw.s.call(http.MethodPost, "/reviews/"+ref.id+"/consume", map[string]any{
		"expected_revision": ref.revision, "expected_verdict_event": ref.verdictEvent,
		"actor": iw.identities["operator"]}); err != nil {
		return err
	}
	if err := iw.s.expectStatus(http.StatusOK); err != nil {
		return err
	}
	ie.actorForImp = iw.identities["operator"]
	return nil
}

func (ie *ieWorld) export() error {
	iw := ie.iw
	if err := iw.s.call(http.MethodGet, "/projects/"+iw.project+"/export", nil); err != nil {
		return err
	}
	if err := iw.s.expectStatus(http.StatusOK); err != nil {
		return err
	}
	ie.exported = append([]byte{}, iw.s.lastBody...)
	return nil
}

// importInto posts payload to the target server under a fresh key.
func (ie *ieWorld) importInto(target *importTarget, payload []byte, actor string) error {
	req, err := http.NewRequest(http.MethodPost, target.server.URL+"/projects/import?actor="+actor, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", newIdempotencyKey())
	resp, err := target.server.Client().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	ie.lastStatus = resp.StatusCode
	ie.lastBody = string(body)
	return nil
}

// tamper decodes the export, applies mutate to the first review (or
// the whole payload), and re-encodes.
func (ie *ieWorld) tamper(mutate func(payload map[string]any) error) error {
	var payload map[string]any
	if err := json.Unmarshal(ie.exported, &payload); err != nil {
		return err
	}
	if err := mutate(payload); err != nil {
		return err
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	ie.tampered = out
	return nil
}

func firstReview(payload map[string]any) (map[string]any, error) {
	reviews, ok := payload["reviews"].([]any)
	if !ok || len(reviews) == 0 {
		return nil, fmt.Errorf("export carries no reviews")
	}
	r, ok := reviews[0].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("review is not an object")
	}
	return r, nil
}

// expectRejectedImport asserts a fresh empty target rejects the
// tampered payload as malformed and writes nothing.
func (ie *ieWorld) expectRejectedImport() error {
	target, err := newImportTarget()
	if err != nil {
		return err
	}
	defer target.close()
	if err := ie.importInto(target, ie.tampered, ie.actorForImp); err != nil {
		return err
	}
	if ie.lastStatus != http.StatusBadRequest {
		return fmt.Errorf("expected 400, got %d: %s", ie.lastStatus, ie.lastBody)
	}
	if !strings.Contains(ie.lastBody, "malformed-import") {
		return fmt.Errorf("expected malformed-import, got %s", ie.lastBody)
	}
	return ie.expectNothingWritten(target)
}

func (ie *ieWorld) expectNothingWritten(target *importTarget) error {
	resp, err := target.server.Client().Get(target.server.URL + "/projects")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var projects []any
	if err := json.Unmarshal(body, &projects); err != nil {
		return fmt.Errorf("decode projects: %w (%s)", err, body)
	}
	if len(projects) != 0 {
		return fmt.Errorf("records written despite rejection: %s", body)
	}
	return nil
}

func registerImportExportSteps(sc *godog.ScenarioContext, cw *closeWorld) {
	iw := cw.iw
	ie := &ieWorld{iw: iw, cw: cw, recordIDs: map[string]string{}}
	sc.Before(func(ctx context.Context, _ *godog.Scenario) (context.Context, error) {
		ie.reset()
		return ctx, nil
	})

	// --- export captures the whole project
	sc.Step(`^project "SUT" has identities, issues, comments, docs with versions, threads, reviews — including one with a consumed approval — and events$`, func() error {
		return ie.seedRichProject()
	})
	sc.Step(`^"SUT" is exported$`, func() error {
		return ie.export()
	})
	sc.Step(`^the export contains every one of those records$`, func() error {
		body := string(ie.exported)
		for kind, id := range ie.recordIDs {
			if !strings.Contains(body, id) {
				return fmt.Errorf("export missing %s %s", kind, id)
			}
		}
		for _, marker := range []string{`"identities"`, `"events"`, `"labels"`, `"issue_relations"`, `"consumed"`, `"v2 body"`} {
			if !strings.Contains(body, marker) {
				return fmt.Errorf("export missing %s", marker)
			}
		}
		return nil
	})

	// --- import round-trips losslessly
	sc.Step(`^an export of project "SUT"$`, func() error {
		if err := ie.seedRichProject(); err != nil {
			return err
		}
		return ie.export()
	})
	sc.Step(`^it is imported into an empty server$`, func() error {
		target, err := newImportTarget()
		if err != nil {
			return err
		}
		ie.target = target
		if err := ie.importInto(target, ie.exported, ie.actorForImp); err != nil {
			return err
		}
		if ie.lastStatus != http.StatusCreated {
			return fmt.Errorf("import failed: %d %s", ie.lastStatus, ie.lastBody)
		}
		return nil
	})
	sc.Step(`^the project's content matches the original, excluding the import audit event$`, func() error {
		resp, err := ie.target.server.Client().Get(ie.target.server.URL + "/projects/" + iw.project + "/export")
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		reExported, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		var original, roundTripped map[string]any
		if err := json.Unmarshal(ie.exported, &original); err != nil {
			return err
		}
		if err := json.Unmarshal(reExported, &roundTripped); err != nil {
			return err
		}
		// Discount exactly the appended project.imported event.
		events, ok := roundTripped["events"].([]any)
		if !ok {
			return fmt.Errorf("re-export carries no events")
		}
		trimmed := make([]any, 0, len(events))
		removed := 0
		for _, e := range events {
			if m, ok := e.(map[string]any); ok && m["kind"] == "project.imported" {
				removed++
				continue
			}
			trimmed = append(trimmed, e)
		}
		if removed != 1 {
			return fmt.Errorf("expected exactly one import audit event, found %d", removed)
		}
		roundTripped["events"] = trimmed
		if !reflect.DeepEqual(original, roundTripped) {
			return fmt.Errorf("round-trip diverged:\noriginal:  %.2000s\nreexport:  %.2000s", ie.exported, reExported)
		}
		return nil
	})
	sc.Step(`^every record keeps its original UUID$`, func() error {
		return nil // implied by the DeepEqual above; ids are part of every record
	})
	sc.Step(`^the consumed review's consumption stamp and revision survive the round-trip$`, func() error {
		resp, err := ie.target.server.Client().Get(ie.target.server.URL + "/reviews/" + ie.recordIDs["review"])
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		var got struct {
			Consumed         *string `json:"consumed"`
			ConsumedRevision *int64  `json:"consumed_revision"`
			Revision         int64   `json:"revision"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			return err
		}
		if got.Consumed == nil || got.ConsumedRevision == nil || *got.ConsumedRevision != got.Revision {
			return fmt.Errorf("consumption stamps lost: %+v", got)
		}
		return nil
	})

	// --- malformed review payloads
	sc.Step(`^an export payload whose review carries a consumption stamp without its consumed revision$`, func() error {
		if err := ie.ensureExport(); err != nil {
			return err
		}
		return ie.tamper(func(p map[string]any) error {
			r, err := firstReview(p)
			if err != nil {
				return err
			}
			r["consumed"] = "2026-01-01T00:00:00Z"
			delete(r, "consumed_revision")
			return nil
		})
	})
	sc.Step(`^it is imported$`, func() error {
		return nil // the rejection step performs the import against a fresh target
	})
	sc.Step(`^the import is rejected as malformed and nothing is created$`, func() error {
		return ie.expectRejectedImport()
	})
	sc.Step(`^an export payload whose consumed review — not close-used — is in state "changes-requested"$`, func() error {
		if err := ie.ensureExport(); err != nil {
			return err
		}
		return ie.tamper(func(p map[string]any) error {
			r, err := firstReview(p)
			if err != nil {
				return err
			}
			delete(r, "close_used")
			r["state"] = "changes-requested"
			return nil
		})
	})
	sc.Step(`^the import is rejected as malformed — consumption fences the verdict, in imports as in the API$`, func() error {
		return ie.expectRejectedImport()
	})
	sc.Step(`^an export payload whose consumed review's consumed revision lags its current revision$`, func() error {
		return ie.tamper(func(p map[string]any) error {
			r, err := firstReview(p)
			if err != nil {
				return err
			}
			rev, ok := r["revision"].(float64)
			if !ok {
				return fmt.Errorf("review revision missing")
			}
			r["consumed_revision"] = rev - 1
			return nil
		})
	})
	sc.Step(`^an export payload whose approved review carries no latest verdict event$`, func() error {
		if err := ie.ensureExport(); err != nil {
			return err
		}
		return ie.tamper(func(p map[string]any) error {
			r, err := firstReview(p)
			if err != nil {
				return err
			}
			delete(r, "latest_verdict_event")
			return nil
		})
	})
	sc.Step(`^the import is rejected as malformed — such a review could never be closed, consumed, or resubmitted$`, func() error {
		return ie.expectRejectedImport()
	})
	sc.Step(`^an export payload whose review names a latest verdict event that is not its actual latest$`, func() error {
		return ie.tamper(func(p map[string]any) error {
			r, err := firstReview(p)
			if err != nil {
				return err
			}
			r["latest_verdict_event"] = r["id"] // a uuid that is not the verdict event
			return nil
		})
	})
	sc.Step(`^an export payload whose review is close-used but carries no consumption fields$`, func() error {
		if err := ie.ensureExport(); err != nil {
			return err
		}
		return ie.tamper(func(p map[string]any) error {
			r, err := firstReview(p)
			if err != nil {
				return err
			}
			r["close_used"] = "2026-01-01T00:00:00Z"
			delete(r, "consumed")
			delete(r, "consumed_revision")
			return nil
		})
	})
	sc.Step(`^an export payload whose close-used review is in state "changes-requested" or whose consumed revision lags its current revision$`, func() error {
		return ie.tamper(func(p map[string]any) error {
			r, err := firstReview(p)
			if err != nil {
				return err
			}
			r["close_used"] = "2026-01-01T00:00:00Z"
			r["state"] = "changes-requested"
			return nil
		})
	})
	sc.Step(`^the import is rejected as malformed — a spent review's verdict cannot be rewritten through import$`, func() error {
		return ie.expectRejectedImport()
	})

	// --- invariant-violating hierarchy
	sc.Step(`^an export payload containing a complete parent whose deferred child hides a descendant in status "open"$`, func() error {
		if err := cw.ensureGitProject(); err != nil {
			return err
		}
		for _, name := range []string{"SUT-1", "SUT-2", "SUT-3"} {
			if _, err := iw.ensureIssue(name); err != nil {
				return err
			}
		}
		actor := iw.identities["operator"]
		for _, pair := range [][2]string{{"SUT-1", "SUT-2"}, {"SUT-2", "SUT-3"}} {
			if err := iw.s.call(http.MethodPost, "/issues/"+iw.issues[pair[0]]+"/relations",
				map[string]string{"to": iw.issues[pair[1]], "kind": "parent_of", "actor": actor}); err != nil {
				return err
			}
			if err := iw.s.expectStatus(http.StatusCreated); err != nil {
				return err
			}
		}
		ie.actorForImp = actor
		if err := ie.export(); err != nil {
			return err
		}
		// The API cannot reach this state (the cascade repairs it);
		// only a tampered payload can carry it.
		return ie.tamper(func(p map[string]any) error {
			issues, ok := p["issues"].([]any)
			if !ok {
				return fmt.Errorf("export carries no issues")
			}
			byID := map[string]map[string]any{}
			for _, raw := range issues {
				if m, ok := raw.(map[string]any); ok {
					byID[m["id"].(string)] = m
				}
			}
			byID[iw.issues["SUT-1"]]["status"] = "complete"
			byID[iw.issues["SUT-2"]]["status"] = "deferred"
			byID[iw.issues["SUT-3"]]["status"] = "open"
			return nil
		})
	})
	sc.Step(`^the import is rejected whole as malformed — imported state gets no cascade to repair it$`, func() error {
		return ie.expectRejectedImport()
	})
	sc.Step(`^nothing is written$`, func() error {
		return nil // asserted inside expectRejectedImport against the fresh target
	})

	// --- colliding import
	sc.Step(`^project "SUT" already exists on the server$`, func() error {
		if err := ie.seedRichProject(); err != nil {
			return err
		}
		return ie.export()
	})
	sc.Step(`^the same export is imported again$`, func() error {
		// Import into the SOURCE server: every UUID already exists.
		source := &importTarget{server: iw.s.server}
		return ie.importInto(source, ie.exported, ie.actorForImp)
	})
	sc.Step(`^the import is rejected naming the conflicting records$`, func() error {
		if ie.lastStatus != http.StatusConflict {
			return fmt.Errorf("expected 409, got %d: %s", ie.lastStatus, ie.lastBody)
		}
		if !strings.Contains(ie.lastBody, "uuid-collision") || !strings.Contains(ie.lastBody, iw.project) {
			return fmt.Errorf("conflicts do not name colliding records: %s", ie.lastBody)
		}
		return nil
	})
	sc.Step(`^no records were partially written$`, func() error {
		// The source server still lists exactly one project.
		if err := iw.s.call(http.MethodGet, "/projects", nil); err != nil {
			return err
		}
		var projects []any
		if err := json.Unmarshal(iw.s.lastBody, &projects); err != nil {
			return err
		}
		if len(projects) != 1 {
			return fmt.Errorf("expected 1 project after rejected re-import, got %d", len(projects))
		}
		return nil
	})

	// --- unknown import actor
	sc.Step(`^identity "outsider" exists on the server but not in the export's identities$`, func() error {
		target, err := newImportTarget()
		if err != nil {
			return err
		}
		ie.target = target
		req, err := http.NewRequest(http.MethodPost, target.server.URL+"/identities",
			strings.NewReader(`{"handle":"outsider","kind":"human"}`))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", newIdempotencyKey())
		resp, err := target.server.Client().Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		var created struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
			return err
		}
		ie.recordIDs["outsider"] = created.ID
		return nil
	})
	sc.Step(`^the export is imported with "outsider" as the actor$`, func() error {
		return ie.importInto(ie.target, ie.exported, ie.recordIDs["outsider"])
	})
	sc.Step(`^the import is rejected as a bad request$`, func() error {
		if ie.lastStatus != http.StatusBadRequest {
			return fmt.Errorf("expected 400, got %d: %s", ie.lastStatus, ie.lastBody)
		}
		// The target still holds no projects: nothing was written.
		return ie.expectNothingWritten(ie.target)
	})
}

// ensureExport seeds and exports once per scenario; repeated Givens in
// the outline-style scenarios reuse it.
func (ie *ieWorld) ensureExport() error {
	if ie.exported != nil {
		return nil
	}
	if err := ie.seedRichProject(); err != nil {
		return err
	}
	return ie.export()
}
