package acceptance_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/cucumber/godog"
)

// issueWorld carries issue-scenario state: identities by handle, one
// project, and issues by display name (SUT-1).
type issueWorld struct {
	s          *testState
	identities map[string]string // handle -> id
	project    string            // project uuid
	issues     map[string]string // display name (SUT-1) -> uuid
	relations  map[string]string // "from>to" -> relation id
	lastIssue  struct {
		ID              string  `json:"id"`
		Number          int64   `json:"number"`
		Title           string  `json:"title"`
		Status          string  `json:"status"`
		Updated         string  `json:"updated"`
		Assignee        *string `json:"assignee"`
		SubtreeRevision int64   `json:"subtree_revision"`
		FeedWatermark   string  `json:"feed_watermark"`
	}
	updatedBefore string
}

func (iw *issueWorld) reset() {
	iw.identities = map[string]string{}
	iw.project = ""
	iw.issues = map[string]string{}
	iw.relations = map[string]string{}
	iw.updatedBefore = ""
}

func (iw *issueWorld) identity(handle string) (string, error) {
	if id, ok := iw.identities[handle]; ok {
		return id, nil
	}
	kind := "agent"
	if strings.HasPrefix(handle, "human-") || handle == "operator" {
		kind = "human"
	}
	if err := iw.s.call(http.MethodPost, "/identities", map[string]string{"handle": handle, "kind": kind}); err != nil {
		return "", err
	}
	if err := iw.s.expectStatus(http.StatusCreated); err != nil {
		return "", err
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(iw.s.lastBody, &created); err != nil {
		return "", fmt.Errorf("decode identity: %w", err)
	}
	iw.identities[handle] = created.ID
	return created.ID, nil
}

func (iw *issueWorld) ensureProject() error {
	if iw.project != "" {
		return nil
	}
	actor, err := iw.identity("operator")
	if err != nil {
		return err
	}
	if err := iw.s.call(http.MethodPost, "/projects", map[string]string{"key": "SUT", "name": "Sutra", "actor": actor}); err != nil {
		return err
	}
	if err := iw.s.expectStatus(http.StatusCreated); err != nil {
		return err
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(iw.s.lastBody, &created); err != nil {
		return fmt.Errorf("decode project: %w", err)
	}
	iw.project = created.ID
	return nil
}

// ensureIssue creates the named issue (SUT-n) if absent; creation order
// follows first mention, which matches the display-number sequence in
// every scenario that names more than one.
func (iw *issueWorld) ensureIssue(name string) (string, error) {
	if id, ok := iw.issues[name]; ok {
		return id, nil
	}
	if err := iw.ensureProject(); err != nil {
		return "", err
	}
	actor := iw.identities["operator"]
	if err := iw.s.call(http.MethodPost, "/projects/"+iw.project+"/issues",
		map[string]string{"title": "issue " + name, "actor": actor}); err != nil {
		return "", err
	}
	if err := iw.s.expectStatus(http.StatusCreated); err != nil {
		return "", err
	}
	if err := json.Unmarshal(iw.s.lastBody, &iw.lastIssue); err != nil {
		return "", fmt.Errorf("decode issue: %w", err)
	}
	iw.issues[name] = iw.lastIssue.ID
	return iw.lastIssue.ID, nil
}

func (iw *issueWorld) readIssue(name string) error {
	id, err := iw.ensureIssue(name)
	if err != nil {
		return err
	}
	if err := iw.s.call(http.MethodGet, "/issues/"+id, nil); err != nil {
		return err
	}
	if err := iw.s.expectStatus(http.StatusOK); err != nil {
		return err
	}
	return json.Unmarshal(iw.s.lastBody, &iw.lastIssue)
}

func (iw *issueWorld) transition(name, status string) error {
	id, err := iw.ensureIssue(name)
	if err != nil {
		return err
	}
	actor := iw.identities["operator"]
	return iw.s.call(http.MethodPost, "/issues/"+id+"/status", map[string]string{"status": status, "actor": actor})
}

func (iw *issueWorld) addRelation(kind, fromName, toName, byHandle string) error {
	from, err := iw.ensureIssue(fromName)
	if err != nil {
		return err
	}
	to, err := iw.ensureIssue(toName)
	if err != nil {
		return err
	}
	actor, err := iw.identity(byHandle)
	if err != nil {
		return err
	}
	if err := iw.s.call(http.MethodPost, "/issues/"+from+"/relations",
		map[string]string{"kind": kind, "to": to, "actor": actor}); err != nil {
		return err
	}
	if iw.s.lastResp.StatusCode == http.StatusCreated {
		var created struct {
			Relation struct {
				ID string `json:"id"`
			} `json:"relation"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &created); err != nil {
			return fmt.Errorf("decode relation: %w", err)
		}
		iw.relations[fromName+">"+toName] = created.Relation.ID
	}
	return nil
}

// relationsOf fetches an issue's relations.
func (iw *issueWorld) relationsOf(name string) ([]map[string]string, error) {
	id, err := iw.ensureIssue(name)
	if err != nil {
		return nil, err
	}
	if err := iw.s.call(http.MethodGet, "/issues/"+id+"/relations", nil); err != nil {
		return nil, err
	}
	if err := iw.s.expectStatus(http.StatusOK); err != nil {
		return nil, err
	}
	var rels []map[string]string
	if err := json.Unmarshal(iw.s.lastBody, &rels); err != nil {
		return nil, fmt.Errorf("decode relations: %w", err)
	}
	return rels, nil
}

// expectEvent asserts the feed holds an event of kind for the named
// issue with the given actor handle, carrying a timestamp.
func (iw *issueWorld) expectEvent(kind, issueName, byHandle string) error {
	return iw.expectEventBySubject(kind, iw.issues[issueName], byHandle)
}

// expectEventBySubject asserts an event exists for a raw subject id —
// project-subject events (thread anchors) use it directly.
func (iw *issueWorld) expectEventBySubject(kind, subject, byHandle string) error {
	actor := iw.identities[byHandle]
	if err := iw.s.call(http.MethodGet, "/events?kind="+url.QueryEscape(kind)+"&subject="+subject, nil); err != nil {
		return err
	}
	var page struct {
		Events []struct {
			Actor   string `json:"actor"`
			Created string `json:"created"`
		} `json:"events"`
	}
	if err := json.Unmarshal(iw.s.lastBody, &page); err != nil {
		return fmt.Errorf("decode events: %w", err)
	}
	for _, e := range page.Events {
		if e.Actor == actor && e.Created != "" {
			return nil
		}
	}
	return fmt.Errorf("no %s event for subject %s by %s in %s", kind, subject, byHandle, iw.s.lastBody)
}

// applyMutation performs one of the mutation kinds AC-audit-mutations
// enumerates against SUT-1, as the named actor. Each arm goes through the
// public API so the event it emits is the one a real client would produce.
func (iw *issueWorld) applyMutation(handle, mutation string) error {
	actor, err := iw.identity(handle)
	if err != nil {
		return err
	}
	issue, err := iw.ensureIssue("SUT-1")
	if err != nil {
		return err
	}
	body := "audit trail"
	switch mutation {
	case "its creation":
		// Already performed by the Given; the creation event is the assertion.
		return nil
	case "a status change":
		if err := iw.s.call(http.MethodPost, "/issues/"+issue+"/status",
			map[string]string{"status": "in-progress", "actor": actor}); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	case "an assignment":
		if err := iw.s.call(http.MethodPost, "/issues/"+issue+"/assign",
			map[string]string{"assignee": actor, "actor": actor}); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	case "a label":
		label, err := iw.createLabel("audited")
		if err != nil {
			return err
		}
		if err := iw.s.call(http.MethodPost, "/issues/"+issue+"/labels",
			map[string]string{"label": label, "actor": actor}); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	case "a comment":
		return iw.comment(map[string]any{"issue": issue, "author": actor, "body": body})
	case "a comment on one of its documents":
		_, version, err := iw.createDoc("Audited design", &issue)
		if err != nil {
			return err
		}
		return iw.comment(map[string]any{"doc_version": version, "author": actor, "body": body})
	case "a comment on one of its reviews":
		// Doc-anchored: a code deliverable would need the project's
		// repo_path, which this scenario has no reason to configure.
		_, version, err := iw.createDoc("Reviewed design", &issue)
		if err != nil {
			return err
		}
		review, revision, err := iw.createReview(issue, actor, version)
		if err != nil {
			return err
		}
		return iw.comment(map[string]any{
			"review": review, "review_revision": revision, "author": actor, "body": body})
	case "a document link":
		doc, _, err := iw.createDoc("Unfiled note", nil)
		if err != nil {
			return err
		}
		if err := iw.s.call(http.MethodPost, "/documents/"+doc+"/issue",
			map[string]string{"issue": issue, "actor": actor}); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	case "a relation to another issue":
		return iw.addRelation("blocks", "SUT-1", "SUT-2", handle)
	}
	return fmt.Errorf("unknown mutation %q", mutation)
}

func (iw *issueWorld) createLabel(name string) (string, error) {
	if err := iw.s.call(http.MethodPost, "/labels", map[string]string{"name": name}); err != nil {
		return "", err
	}
	if err := iw.s.expectStatus(http.StatusCreated); err != nil {
		return "", err
	}
	var created struct {
		ID string `json:"id"`
	}
	err := json.Unmarshal(iw.s.lastBody, &created)
	return created.ID, err
}

// createDoc creates a document with inline content, optionally anchored to
// an issue, and returns its document and first-version ids.
func (iw *issueWorld) createDoc(title string, issue *string) (string, string, error) {
	req := map[string]any{"title": title, "content": "# " + title, "author": iw.identities["operator"]}
	if issue != nil {
		req["issue"] = *issue
	}
	if err := iw.s.call(http.MethodPost, "/projects/"+iw.project+"/documents", req); err != nil {
		return "", "", err
	}
	if err := iw.s.expectStatus(http.StatusCreated); err != nil {
		return "", "", err
	}
	var created struct {
		ID      string `json:"id"`
		Version struct {
			ID string `json:"id"`
		} `json:"version"`
	}
	err := json.Unmarshal(iw.s.lastBody, &created)
	return created.ID, created.Version.ID, err
}

func (iw *issueWorld) createReview(issue, author, docVersion string) (string, int64, error) {
	if err := iw.s.call(http.MethodPost, "/reviews", map[string]string{
		"issue": issue, "author": author, "doc_version": docVersion,
	}); err != nil {
		return "", 0, err
	}
	if err := iw.s.expectStatus(http.StatusCreated); err != nil {
		return "", 0, err
	}
	var created struct {
		ID       string `json:"id"`
		Revision int64  `json:"revision"`
	}
	err := json.Unmarshal(iw.s.lastBody, &created)
	return created.ID, created.Revision, err
}

func (iw *issueWorld) comment(payload map[string]any) error {
	if err := iw.s.call(http.MethodPost, "/comments", payload); err != nil {
		return err
	}
	return iw.s.expectStatus(http.StatusCreated)
}

func registerIssueSteps(sc *godog.ScenarioContext, s *testState) *issueWorld {
	iw := &issueWorld{s: s}
	sc.Before(func(ctx context.Context, _ *godog.Scenario) (context.Context, error) {
		iw.reset()
		return ctx, nil
	})

	// --- create mints uuid and display number
	sc.Step(`^project "SUT" exists$`, func() error { return iw.ensureProject() })
	sc.Step(`^an issue "([^"]*)" is created in "SUT"$`, func(title string) error {
		if err := iw.ensureProject(); err != nil {
			return err
		}
		actor := iw.identities["operator"]
		if err := iw.s.call(http.MethodPost, "/projects/"+iw.project+"/issues",
			map[string]string{"title": title, "actor": actor}); err != nil {
			return err
		}
		if err := iw.s.expectStatus(http.StatusCreated); err != nil {
			return err
		}
		if err := json.Unmarshal(iw.s.lastBody, &iw.lastIssue); err != nil {
			return err
		}
		iw.issues[fmt.Sprintf("SUT-%d", iw.lastIssue.Number)] = iw.lastIssue.ID
		return nil
	})
	sc.Step(`^it has a UUIDv7 id$`, func() error {
		id := iw.lastIssue.ID
		if len(id) != 36 || id[14] != '7' {
			return fmt.Errorf("not a UUIDv7: %q", id)
		}
		return nil
	})
	sc.Step(`^its display number is the next sequential number in "SUT"$`, func() error {
		if iw.lastIssue.Number != int64(len(iw.issues)) {
			return fmt.Errorf("expected number %d, got %d", len(iw.issues), iw.lastIssue.Number)
		}
		return nil
	})
	sc.Step(`^its status is "open"$`, func() error {
		if iw.lastIssue.Status != "open" {
			return fmt.Errorf("status %q", iw.lastIssue.Status)
		}
		return nil
	})
	sc.Step(`^a creation event is recorded$`, func() error {
		return iw.expectEvent("issue.created", fmt.Sprintf("SUT-%d", iw.lastIssue.Number), "operator")
	})

	// --- updates persist
	sc.Step(`^issue SUT-1 exists$`, func() error {
		_, err := iw.ensureIssue("SUT-1")
		return err
	})
	sc.Step(`^its title is changed to "([^"]*)"$`, func(title string) error {
		if err := iw.readIssue("SUT-1"); err != nil {
			return err
		}
		iw.updatedBefore = iw.lastIssue.Updated
		actor := iw.identities["operator"]
		if err := iw.s.call(http.MethodPatch, "/issues/"+iw.issues["SUT-1"],
			map[string]string{"title": title, "actor": actor}); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	})
	sc.Step(`^reading SUT-1 shows the new title$`, func() error {
		if err := iw.readIssue("SUT-1"); err != nil {
			return err
		}
		if iw.lastIssue.Title != "fix the lexer" {
			return fmt.Errorf("title %q", iw.lastIssue.Title)
		}
		return nil
	})
	sc.Step(`^its updated timestamp changed$`, func() error {
		if iw.lastIssue.Updated == iw.updatedBefore {
			return fmt.Errorf("updated did not change from %s", iw.updatedBefore)
		}
		return nil
	})

	// --- free transitions are recorded / unknown statuses rejected
	sc.Step(`^issue SUT-1 is "open"$`, func() error {
		return iw.readIssue("SUT-1")
	})
	sc.Step(`^it moves to "in-progress", then "deferred", then "blocked"$`, func() error {
		for _, status := range []string{"in-progress", "deferred", "blocked"} {
			if err := iw.transition("SUT-1", status); err != nil {
				return err
			}
			if err := iw.s.expectStatus(http.StatusOK); err != nil {
				return err
			}
		}
		return nil
	})
	sc.Step(`^each transition succeeds$`, func() error { return nil }) // asserted per-transition above
	sc.Step(`^each is recorded as an event with actor and time$`, func() error {
		subject := iw.issues["SUT-1"]
		actor := iw.identities["operator"]
		if err := iw.s.call(http.MethodGet, "/events?kind=issue.status-changed&subject="+subject, nil); err != nil {
			return err
		}
		var page struct {
			Events []struct {
				ID        string `json:"id"`
				Operation string `json:"operation"`
				Actor     string `json:"actor"`
				Created   string `json:"created"`
			} `json:"events"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &page); err != nil {
			return fmt.Errorf("decode events: %w", err)
		}
		perOperation := map[string]int{}
		for _, e := range page.Events {
			if e.Actor != actor || e.Created == "" {
				return fmt.Errorf("event missing actor/timestamp: %+v", e)
			}
			perOperation[e.Operation]++
		}
		if len(perOperation) != 3 {
			return fmt.Errorf("expected 3 transitions' operations, got %d: %s", len(perOperation), iw.s.lastBody)
		}
		for op, n := range perOperation {
			if n != 1 {
				return fmt.Errorf("operation %s emitted %d events, want exactly 1", op, n)
			}
		}
		return nil
	})
	sc.Step(`^issue SUT-1 is set to status "someday"$`, func() error {
		return iw.transition("SUT-1", "someday")
	})
	// Shared by the unknown-status and append-only scenarios: an HTTP
	// attempt must have been rejected; a store-level attempt (which
	// clears lastResp) carried its own assertion.
	sc.Step(`^the operation is rejected$`, func() error {
		if iw.s.lastResp == nil {
			return nil
		}
		if iw.s.lastResp.StatusCode < 400 {
			return fmt.Errorf("expected rejection, got %d", iw.s.lastResp.StatusCode)
		}
		return nil
	})

	// --- parent and child see each other
	sc.Step(`^issues SUT-1 and SUT-2 exist$`, func() error {
		for _, name := range []string{"SUT-1", "SUT-2"} {
			if _, err := iw.ensureIssue(name); err != nil {
				return err
			}
		}
		return nil
	})
	sc.Step(`^SUT-2 becomes a child of SUT-1 by "([^"]*)"$`, func(handle string) error {
		if err := iw.addRelation("parent_of", "SUT-1", "SUT-2", handle); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusCreated)
	})
	sc.Step(`^SUT-1 lists SUT-2 among its children$`, func() error {
		rels, err := iw.relationsOf("SUT-1")
		if err != nil {
			return err
		}
		for _, rel := range rels {
			if rel["kind"] == "parent_of" && rel["from"] == iw.issues["SUT-1"] && rel["to"] == iw.issues["SUT-2"] {
				return nil
			}
		}
		return fmt.Errorf("SUT-2 not among SUT-1 children: %v", rels)
	})
	sc.Step(`^SUT-2 shows SUT-1 as its parent$`, func() error {
		rels, err := iw.relationsOf("SUT-2")
		if err != nil {
			return err
		}
		for _, rel := range rels {
			if rel["kind"] == "parent_of" && rel["from"] == iw.issues["SUT-1"] {
				return nil
			}
		}
		return fmt.Errorf("SUT-1 not shown as parent: %v", rels)
	})
	sc.Step(`^an "([^"]*)" event with actor "([^"]*)" and a timestamp is recorded for SUT-1$`, func(kind, handle string) error {
		return iw.expectEvent(kind, "SUT-1", handle)
	})
	sc.Step(`^making SUT-1 a child of SUT-2 is rejected as a cycle$`, func() error {
		if err := iw.addRelation("parent_of", "SUT-2", "SUT-1", "human-brent"); err != nil {
			return err
		}
		if err := iw.s.expectStatus(http.StatusConflict); err != nil {
			return err
		}
		return iw.s.expectErrorCode("ancestry-cycle")
	})

	// --- both sides see the block / cycles are rejected
	sc.Step(`^SUT-1 is marked as blocking SUT-2 by "([^"]*)"$`, func(handle string) error {
		if err := iw.addRelation("blocks", "SUT-1", "SUT-2", handle); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusCreated)
	})
	sc.Step(`^SUT-1 shows "blocks SUT-2"$`, func() error {
		rels, err := iw.relationsOf("SUT-1")
		if err != nil {
			return err
		}
		for _, rel := range rels {
			if rel["kind"] == "blocks" && rel["to"] == iw.issues["SUT-2"] {
				return nil
			}
		}
		return fmt.Errorf("block not visible from SUT-1: %v", rels)
	})
	sc.Step(`^SUT-2 shows "blocked by SUT-1"$`, func() error {
		rels, err := iw.relationsOf("SUT-2")
		if err != nil {
			return err
		}
		for _, rel := range rels {
			if rel["kind"] == "blocks" && rel["from"] == iw.issues["SUT-1"] {
				return nil
			}
		}
		return fmt.Errorf("block not visible from SUT-2: %v", rels)
	})
	sc.Step(`^the relationship is removed by "([^"]*)"$`, func(handle string) error {
		actor, err := iw.identity(handle)
		if err != nil {
			return err
		}
		rel := iw.relations["SUT-1>SUT-2"]
		if err := iw.s.call(http.MethodDelete, "/issues/"+iw.issues["SUT-1"]+"/relations/"+rel+"?actor="+actor, nil); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusNoContent)
	})
	// --- removed from either end, and from nowhere else
	sc.Step(`^issue (SUT-\d+) exists, involved in no relation$`, func(name string) error {
		_, err := iw.ensureIssue(name)
		return err
	})
	sc.Step(`^removing that relation is attempted with (SUT-\d+) as the path issue$`, func(name string) error {
		actor, err := iw.identity("human-brent")
		if err != nil {
			return err
		}
		rel := iw.relations["SUT-1>SUT-2"]
		return iw.s.call(http.MethodDelete,
			"/issues/"+iw.issues[name]+"/relations/"+rel+"?actor="+actor, nil)
	})
	sc.Step(`^it is rejected as not found, naming the relation and (SUT-\d+)$`, func(name string) error {
		if err := iw.s.expectStatus(http.StatusNotFound); err != nil {
			return err
		}
		if err := iw.s.expectErrorCode("not-found"); err != nil {
			return err
		}
		// The relation exists and so does the issue — only the pairing
		// is wrong, so the message has to say which two it refused.
		var envelope struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &envelope); err != nil {
			return err
		}
		for _, want := range []string{iw.relations["SUT-1>SUT-2"], iw.issues[name]} {
			if !strings.Contains(envelope.Message, want) {
				return fmt.Errorf("refusal does not name %s: %q", want, envelope.Message)
			}
		}
		return nil
	})
	sc.Step(`^the relationship is removed from the blocked end by "([^"]*)"$`, func(handle string) error {
		actor, err := iw.identity(handle)
		if err != nil {
			return err
		}
		rel := iw.relations["SUT-1>SUT-2"]
		if err := iw.s.call(http.MethodDelete,
			"/issues/"+iw.issues["SUT-2"]+"/relations/"+rel+"?actor="+actor, nil); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusNoContent)
	})
	sc.Step(`^neither side shows it$`, func() error {
		for _, name := range []string{"SUT-1", "SUT-2"} {
			rels, err := iw.relationsOf(name)
			if err != nil {
				return err
			}
			if len(rels) != 0 {
				return fmt.Errorf("%s still shows relations: %v", name, rels)
			}
		}
		return nil
	})
	sc.Step(`^SUT-1 blocks SUT-2 and SUT-2 blocks SUT-3$`, func() error {
		if err := iw.addRelation("blocks", "SUT-1", "SUT-2", "operator"); err != nil {
			return err
		}
		if err := iw.s.expectStatus(http.StatusCreated); err != nil {
			return err
		}
		if err := iw.addRelation("blocks", "SUT-2", "SUT-3", "operator"); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusCreated)
	})
	sc.Step(`^SUT-3 is marked as blocking SUT-1$`, func() error {
		return iw.addRelation("blocks", "SUT-3", "SUT-1", "operator")
	})
	sc.Step(`^the operation is rejected as a cycle$`, func() error {
		if err := iw.s.expectStatus(http.StatusConflict); err != nil {
			return err
		}
		return iw.s.expectErrorCode("blocking-cycle")
	})

	// --- audit: every mutation is recorded
	sc.Step(`^"([^"]*)" applies (.+) to SUT-1$`, func(handle, mutation string) error {
		return iw.applyMutation(handle, mutation)
	})
	sc.Step(`^an event exists for SUT-1 with actor "([^"]*)", kind "([^"]*)", and a timestamp$`,
		func(handle, kind string) error {
			return iw.expectEvent(kind, "SUT-1", handle)
		})

	// --- audit: events are append-only
	sc.Step(`^an event exists for issue SUT-1$`, func() error {
		if _, err := iw.ensureIssue("SUT-1"); err != nil {
			return err
		}
		return iw.expectEvent("issue.created", "SUT-1", "operator")
	})
	sc.Step(`^any API operation attempts to modify or delete it$`, func() error {
		// No API surface mutates events; the store's triggers are the
		// enforcement every path shares. Attempt both directly.
		if _, err := iw.s.db.Exec(`UPDATE events SET kind = 'tampered'`); err == nil {
			return fmt.Errorf("UPDATE on events succeeded")
		}
		if _, err := iw.s.db.Exec(`DELETE FROM events`); err == nil {
			return fmt.Errorf("DELETE on events succeeded")
		}
		iw.s.lastResp = nil // signal: rejection already asserted here
		return nil
	})
	return iw
}
