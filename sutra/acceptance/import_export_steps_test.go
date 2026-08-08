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

	recordIDs map[string]string // kind -> id minted while seeding
	assigned  []string          // issue ids, in the order they were assigned
	exported  []byte
	// createdOnTarget is the issue minted on the import target after
	// the import — its number is what the sequence check reads.
	createdOnTarget struct {
		Number int64 `json:"number"`
	}
	// projectsBefore is how many projects the source held before a
	// rejected import, so the check after it compares against reality.
	projectsBefore int
	tampered       []byte
	target         *importTarget
	lastStatus     int
	lastBody       string
	actorForImp    string
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

	// A label, attached to an issue. The completeness check looks for
	// the "labels" key, which an EMPTY array satisfies — so an export
	// with no label passes it while carrying none, and import's label
	// validation never sees one.
	if err := iw.s.call(http.MethodPost, "/labels",
		map[string]any{"name": "exported-label", "color": "#abcdef"}); err != nil {
		return err
	}
	if err := iw.s.expectStatus(http.StatusCreated); err != nil {
		return err
	}
	var label struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(iw.s.lastBody, &label); err != nil {
		return err
	}
	ie.recordIDs["label"] = label.ID
	if err := iw.s.call(http.MethodPost, "/issues/"+issueID+"/labels",
		map[string]any{"label": label.ID, "actor": iw.identities["operator"]}); err != nil {
		return err
	}
	if err := iw.s.expectStatus(http.StatusOK); err != nil {
		return err
	}

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

	// An ISSUE-anchored thread too. A thread carries exactly one of the
	// two anchors, so an export holding only the project-anchored kind
	// round-trips without the issue anchor ever reaching import's
	// validation — and import must accept it, not read it as anchorless.
	issueTranscript, err := json.Marshal([]map[string]string{{"speaker": "claude", "text": "anchored to an issue"}})
	if err != nil {
		return err
	}
	if err := iw.s.call(http.MethodPost, "/threads", map[string]any{
		"title": "exported issue thread", "transcript": json.RawMessage(issueTranscript),
		"issue": issueID, "actor": iw.identities["operator"]}); err != nil {
		return err
	}
	if err := iw.s.expectStatus(http.StatusCreated); err != nil {
		return err
	}
	var issueThread struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(iw.s.lastBody, &issueThread); err != nil {
		return err
	}
	ie.recordIDs["issue-thread"] = issueThread.ID

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

	// And a removed relation of the OTHER kind. The snapshot inside a
	// relation-removed event carries the kind it removed, and import
	// validates that field too — so an export whose only removal is a
	// "blocks" one leaves the "parent_of" half of that check unexercised
	// and every parent_of removal ever imported would be called
	// malformed.
	if err := iw.s.call(http.MethodPost, "/issues/"+issueID+"/relations",
		map[string]string{"to": iw.issues["SUT-2"], "kind": "parent_of", "actor": actor}); err != nil {
		return err
	}
	if err := iw.s.expectStatus(http.StatusCreated); err != nil {
		return err
	}
	var removedParent struct {
		Relation struct {
			ID string `json:"id"`
		} `json:"relation"`
	}
	if err := json.Unmarshal(iw.s.lastBody, &removedParent); err != nil {
		return err
	}
	if err := iw.s.call(http.MethodDelete, "/issues/"+issueID+"/relations/"+removedParent.Relation.ID+"?actor="+actor, nil); err != nil {
		return err
	}
	if err := iw.s.expectStatus(http.StatusNoContent); err != nil {
		return err
	}

	// And a removed blocks relation pointing OUT of the project. Only
	// blocks may cross a project boundary, and the live edge cannot
	// round-trip (the cross-project gap in spec-gaps.md) — but its
	// removal event does, because export scopes events by subject and
	// the subject is the in-project source. So import must accept a
	// snapshot whose `to` names an issue the payload does not carry,
	// for a blocks removal and only for one.
	if err := iw.s.call(http.MethodPost, "/projects",
		map[string]string{"key": "OUT", "name": "Outside", "actor": actor}); err != nil {
		return err
	}
	if err := iw.s.expectStatus(http.StatusCreated); err != nil {
		return err
	}
	var outside struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(iw.s.lastBody, &outside); err != nil {
		return err
	}
	if err := iw.s.call(http.MethodPost, "/projects/"+outside.ID+"/issues",
		map[string]string{"title": "an issue in another project", "actor": actor}); err != nil {
		return err
	}
	if err := iw.s.expectStatus(http.StatusCreated); err != nil {
		return err
	}
	var foreign struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(iw.s.lastBody, &foreign); err != nil {
		return err
	}
	if err := iw.s.call(http.MethodPost, "/issues/"+issueID+"/relations",
		map[string]string{"to": foreign.ID, "kind": "blocks", "actor": actor}); err != nil {
		return err
	}
	if err := iw.s.expectStatus(http.StatusCreated); err != nil {
		return err
	}
	var crossing struct {
		Relation struct {
			ID string `json:"id"`
		} `json:"relation"`
	}
	if err := json.Unmarshal(iw.s.lastBody, &crossing); err != nil {
		return err
	}
	if err := iw.s.call(http.MethodDelete, "/issues/"+issueID+"/relations/"+crossing.Relation.ID+"?actor="+actor, nil); err != nil {
		return err
	}
	if err := iw.s.expectStatus(http.StatusNoContent); err != nil {
		return err
	}

	// A LIVE relation as well — an export whose only relation is a
	// deleted one round-trips without ever exercising the edge itself,
	// and a parent still holding an open child is the ordinary
	// arrangement import must accept while it rejects the invariant
	// violation.
	if err := iw.s.call(http.MethodPost, "/issues/"+issueID+"/relations",
		map[string]string{"to": iw.issues["SUT-2"], "kind": "parent_of", "actor": actor}); err != nil {
		return err
	}
	if err := iw.s.expectStatus(http.StatusCreated); err != nil {
		return err
	}
	var live struct {
		Relation struct {
			ID string `json:"id"`
		} `json:"relation"`
	}
	if err := json.Unmarshal(iw.s.lastBody, &live); err != nil {
		return err
	}
	ie.recordIDs["relation"] = live.Relation.ID

	// The OTHER relation kind, live as well. Only "blocks" and
	// "parent_of" exist, and an export carrying one of them leaves
	// import's kind check half-exercised — it would reject every
	// "blocks" edge ever imported and the round trip would not notice.
	// A fresh pair, both open: blocking is what the edge means, and
	// hanging it off the closed issue would make the seed a
	// completion-invariant test instead of a round-trip one.
	blockedID, err := iw.ensureIssue("SUT-4")
	if err != nil {
		return err
	}
	ie.recordIDs["blocked-issue"] = blockedID
	if err := iw.s.call(http.MethodPost, "/issues/"+iw.issues["SUT-2"]+"/relations",
		map[string]string{"to": blockedID, "kind": "blocks", "actor": actor}); err != nil {
		return err
	}
	if err := iw.s.expectStatus(http.StatusCreated); err != nil {
		return err
	}
	var blocking struct {
		Relation struct {
			ID string `json:"id"`
		} `json:"relation"`
	}
	if err := json.Unmarshal(iw.s.lastBody, &blocking); err != nil {
		return err
	}
	ie.recordIDs["blocks-relation"] = blocking.Relation.ID

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

	// A REVIEW-anchored comment, pinned to the revision it was written
	// against. The export's other comment anchors to an issue, so
	// without this one the review anchor and its revision never
	// round-trip and import's revision check never runs on real input.
	if err := iw.s.call(http.MethodPost, "/comments", map[string]any{
		"review": docReview.ID, "review_revision": 1, "author": author, "body": "exported review comment"}); err != nil {
		return err
	}
	if err := iw.s.expectStatus(http.StatusCreated); err != nil {
		return err
	}
	var reviewComment struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(iw.s.lastBody, &reviewComment); err != nil {
		return err
	}
	ie.recordIDs["review-comment"] = reviewComment.ID

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
	// A CLOSED issue and the review spent to close it. Completeness is
	// reachable only through a review-gated close, so an export with no
	// complete issue never puts that invariant to the import: the only
	// complete issues the suite imports are the malformed ones, and a
	// rejection proves nothing about the legitimate case.
	closedID, err := iw.ensureIssue("SUT-3")
	if err != nil {
		return err
	}
	ie.recordIDs["closed-issue"] = closedID
	if err := cw.approvedReview("SUT-3", "closed-work"); err != nil {
		return err
	}
	ie.recordIDs["close-used-review"] = cw.reviews["SUT-3"].id
	if err := cw.close("SUT-3", 0, ""); err != nil {
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

// assignTo assigns a source-server issue to a handle, recording the
// order — the work stack is FIFO by assignment, so the order IS the
// thing under test.
func (ie *ieWorld) assignTo(issueID, handle string) error {
	iw := ie.iw
	assignee, err := iw.identity(handle)
	if err != nil {
		return err
	}
	if err := iw.s.call(http.MethodPost, "/issues/"+issueID+"/assign",
		map[string]any{"assignee": assignee, "actor": iw.identities["operator"]}); err != nil {
		return err
	}
	if err := iw.s.expectStatus(http.StatusOK); err != nil {
		return err
	}
	ie.assigned = append(ie.assigned, issueID)
	return nil
}

// postToTarget posts a mutating request to the import target under a
// fresh key, returning its status and body. Scenarios that read the
// imported state back need the TARGET's own API, not the source's.
func (ie *ieWorld) postToTarget(path string, body any) (int, []byte, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequest(http.MethodPost, ie.target.server.URL+path, bytes.NewReader(encoded))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", newIdempotencyKey())
	resp, err := ie.target.server.Client().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, respBody, nil
}

// sourceProjectCount reports how many projects the SOURCE server lists.
func (ie *ieWorld) sourceProjectCount() (int, error) {
	if err := ie.iw.s.call(http.MethodGet, "/projects", nil); err != nil {
		return 0, err
	}
	var projects []any
	if err := json.Unmarshal(ie.iw.s.lastBody, &projects); err != nil {
		return 0, err
	}
	return len(projects), nil
}

// nextUUID returns a different but still canonical uuid, advancing the
// last hex digit — a position that carries neither the version nor the
// variant nibble, so the result still passes import's shape check.
func nextUUID(id string) string {
	const hex = "0123456789abcdef"
	return id[:len(id)-1] + string(hex[(strings.IndexByte(hex, id[len(id)-1])+1)%len(hex)])
}

// getFromTarget reads a resource back from the import target, returning
// its status and body.
func (ie *ieWorld) getFromTarget(path string) (int, []byte, error) {
	resp, err := ie.target.server.Client().Get(ie.target.server.URL + path)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, respBody, nil
}

// popOnTarget claims the next issue off an identity's work stack on the
// IMPORT TARGET, returning the claimed issue's id ("" for an empty
// stack).
func (ie *ieWorld) popOnTarget(identityID string) (string, error) {
	status, body, err := ie.postToTarget("/identities/"+identityID+"/work-stack/pop", map[string]any{})
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("pop on target: %d %s", status, body)
	}
	var popped struct {
		Issue *struct {
			ID string `json:"id"`
		} `json:"issue"`
	}
	if err := json.Unmarshal(body, &popped); err != nil {
		return "", fmt.Errorf("decode pop: %w (%s)", err, body)
	}
	if popped.Issue == nil {
		return "", nil
	}
	return popped.Issue.ID, nil
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
	sc.Step(`^an export of project "SUT", archived before it was exported$`, func() error {
		if err := ie.seedRichProject(); err != nil {
			return err
		}
		// Archiving LAST: it makes the project read-only, so nothing
		// else can be seeded afterwards.
		if err := iw.s.call(http.MethodPost, "/projects/"+iw.project+"/archive",
			map[string]any{"actor": iw.identities["operator"]}); err != nil {
			return err
		}
		if err := iw.s.expectStatus(http.StatusOK); err != nil {
			return err
		}
		return ie.export()
	})
	sc.Step(`^the imported project is still archived$`, func() error {
		status, body, err := ie.getFromTarget("/projects/" + iw.project)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("read imported project: %d %s", status, body)
		}
		var project struct {
			ArchivedAt *string `json:"archived_at"`
		}
		if err := json.Unmarshal(body, &project); err != nil {
			return fmt.Errorf("decode project: %w (%s)", err, body)
		}
		if project.ArchivedAt == nil {
			return fmt.Errorf("the imported project came back writable — the archive stamp did not survive the round trip")
		}
		return nil
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
	// --- an imported work stack pops in its original order
	sc.Step(`^an export of project "SUT" whose work stack was filled against issue-number order$`, func() error {
		// Numbers are minted in creation order and the queue breaks
		// assigned_at ties by number, so assigning the SECOND-created
		// issue FIRST is the one arrangement where a preserved queue
		// position and the number fallback disagree.
		first, err := iw.ensureIssue("SUT-1")
		if err != nil {
			return err
		}
		second, err := iw.ensureIssue("SUT-2")
		if err != nil {
			return err
		}
		if err := ie.assignTo(second, "claude"); err != nil {
			return err
		}
		if err := ie.assignTo(first, "claude"); err != nil {
			return err
		}
		ie.actorForImp = iw.identities["operator"]
		return ie.export()
	})
	sc.Step(`^the imported work stack pops in the order the issues were assigned$`, func() error {
		assignee, err := iw.identity("claude")
		if err != nil {
			return err
		}
		for i, want := range ie.assigned {
			got, err := ie.popOnTarget(assignee)
			if err != nil {
				return err
			}
			if got != want {
				return fmt.Errorf("pop %d claimed %q, wanted %q — the imported stack lost its assignment order",
					i+1, got, want)
			}
		}
		return nil
	})

	// --- issue numbering continues past an import
	sc.Step(`^an issue is created on the imported project$`, func() error {
		status, body, err := ie.postToTarget("/projects/"+iw.project+"/issues",
			map[string]string{"title": "created after the import", "actor": ie.actorForImp})
		if err != nil {
			return err
		}
		if status != http.StatusCreated {
			return fmt.Errorf("create on imported project: %d %s", status, body)
		}
		return json.Unmarshal(body, &ie.createdOnTarget)
	})
	sc.Step(`^it gets a number no imported issue already holds$`, func() error {
		var exported struct {
			Issues []struct {
				Number int64 `json:"number"`
			} `json:"issues"`
		}
		if err := json.Unmarshal(ie.exported, &exported); err != nil {
			return err
		}
		if len(exported.Issues) == 0 {
			return fmt.Errorf("the export carries no issues, so no number could collide")
		}
		for _, i := range exported.Issues {
			if i.Number == ie.createdOnTarget.Number {
				return fmt.Errorf("the new issue reused number %d — the display sequence restarted at the import",
					i.Number)
			}
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

	sc.Step(`^an export payload whose thread carries no transcript$`, func() error {
		if err := ie.ensureExport(); err != nil {
			return err
		}
		return ie.tamper(func(p map[string]any) error {
			exported, ok := p["threads"].([]any)
			if !ok || len(exported) == 0 {
				return fmt.Errorf("export carries no threads")
			}
			t, ok := exported[0].(map[string]any)
			if !ok {
				return fmt.Errorf("thread is not an object")
			}
			delete(t, "transcript")
			return nil
		})
	})

	// malformedAt corrupts one well-formedness rule at a named place in
	// the payload. The places are the payload's extremes on purpose: the
	// shape pass walks record types in a fixed order, so a fault in the
	// FIRST one proves nothing about the last, and one at the top level
	// proves nothing about the identifiers buried in an event payload.
	malformedAt := map[string]func(map[string]any) error{
		"project id, the first record in the payload": func(p map[string]any) error {
			project, ok := p["project"].(map[string]any)
			if !ok {
				return fmt.Errorf("export carries no project")
			}
			project["id"] = "not-a-uuid"
			return nil
		},
		"archive stamp on a project the export left unarchived": func(p map[string]any) error {
			project, ok := p["project"].(map[string]any)
			if !ok {
				return fmt.Errorf("export carries no project")
			}
			project["archived_at"] = "the day before yesterday"
			return nil
		},
		"event timestamp, the last record in the payload": func(p map[string]any) error {
			events, ok := p["events"].([]any)
			if !ok || len(events) == 0 {
				return fmt.Errorf("export carries no events")
			}
			last, ok := events[len(events)-1].(map[string]any)
			if !ok {
				return fmt.Errorf("event is not an object")
			}
			last["created"] = "the day before yesterday"
			return nil
		},
		"identifier inside a removed relation's snapshot": func(p map[string]any) error {
			events, ok := p["events"].([]any)
			if !ok {
				return fmt.Errorf("export carries no events")
			}
			for _, entry := range events {
				e, ok := entry.(map[string]any)
				if !ok || e["kind"] != "issue.relation-removed" {
					continue
				}
				snapshot, ok := e["payload"].(map[string]any)
				if !ok {
					return fmt.Errorf("relation-removed event carries no payload")
				}
				snapshot["relation"] = "not-a-uuid"
				return nil
			}
			return fmt.Errorf("export carries no relation-removed event")
		},
	}
	sc.Step(`^an export payload whose parent_of relations form a cycle$`, func() error {
		if err := ie.ensureExport(); err != nil {
			return err
		}
		return ie.tamper(func(p map[string]any) error {
			relations, ok := p["issue_relations"].([]any)
			if !ok {
				return fmt.Errorf("export carries no relations")
			}
			for _, entry := range relations {
				rel, ok := entry.(map[string]any)
				if !ok || rel["kind"] != "parent_of" {
					continue
				}
				id, ok := rel["id"].(string)
				if !ok {
					return fmt.Errorf("relation carries no id")
				}
				reversed := map[string]any{}
				for key, value := range rel {
					reversed[key] = value
				}
				reversed["from"], reversed["to"] = rel["to"], rel["from"]
				reversed["id"] = nextUUID(id)
				p["issue_relations"] = append(relations, reversed)
				return nil
			}
			return fmt.Errorf("export carries no parent_of relation to reverse")
		})
	})
	sc.Step(`^an export payload whose (.+) is malformed$`, func(where string) error {
		mutate, ok := malformedAt[where]
		if !ok {
			return fmt.Errorf("no tampering defined for %q", where)
		}
		if err := ie.ensureExport(); err != nil {
			return err
		}
		return ie.tamper(mutate)
	})

	sc.Step(`^an export payload whose comment names a review revision the review never had$`, func() error {
		if err := ie.ensureExport(); err != nil {
			return err
		}
		return ie.tamper(func(p map[string]any) error {
			exported, ok := p["comments"].([]any)
			if !ok {
				return fmt.Errorf("export carries no comments")
			}
			for _, entry := range exported {
				c, ok := entry.(map[string]any)
				if !ok || c["review"] == nil {
					continue
				}
				c["review_revision"] = 99
				return nil
			}
			return fmt.Errorf("export carries no review-anchored comment to misnumber")
		})
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
	sc.Step(`^an export payload whose review carries a verdict superseded by a later resubmission$`, func() error {
		if err := ie.ensureExport(); err != nil {
			return err
		}
		return ie.tamper(func(p map[string]any) error {
			r, err := firstReview(p)
			if err != nil {
				return err
			}
			// Append a resubmission event AFTER the review's verdict:
			// live, that would return the review to open.
			evs, ok := p["events"].([]any)
			if !ok {
				return fmt.Errorf("export carries no events")
			}
			p["events"] = append(evs, map[string]any{
				"id":        "01900000-0000-7000-8000-0000000000ff",
				"kind":      "review.resubmitted",
				"subject":   r["issue"],
				"operation": "01900000-0000-7000-8000-0000000000fe",
				"actor":     ie.actorForImp,
				"payload":   map[string]any{"review": r["id"]},
				"created":   "2026-08-06T23:59:59Z",
			})
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
		if err := ie.export(); err != nil {
			return err
		}
		// The seed leaves more than one project behind — a rejected
		// import must add none, so the count to compare against is the
		// one taken here, not a literal.
		count, err := ie.sourceProjectCount()
		if err != nil {
			return err
		}
		ie.projectsBefore = count
		return nil
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
		// The source server lists no more projects than it did before.
		count, err := ie.sourceProjectCount()
		if err != nil {
			return err
		}
		if count != ie.projectsBefore {
			return fmt.Errorf("expected %d projects after rejected re-import, got %d", ie.projectsBefore, count)
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
