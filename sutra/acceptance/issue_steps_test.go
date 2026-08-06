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
	subject := iw.issues[issueName]
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
	return fmt.Errorf("no %s event for %s by %s in %s", kind, issueName, byHandle, iw.s.lastBody)
}

func registerIssueSteps(sc *godog.ScenarioContext, s *testState) {
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
		return iw.expectEvent("issue.status-changed", "SUT-1", "operator")
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
	sc.Step(`^"claude" changes SUT-1 status to "in-progress"$`, func() error {
		actor, err := iw.identity("claude")
		if err != nil {
			return err
		}
		if err := iw.s.call(http.MethodPost, "/issues/"+iw.issues["SUT-1"]+"/status",
			map[string]string{"status": "in-progress", "actor": actor}); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	})
	sc.Step(`^an event exists for SUT-1 with actor "claude", kind "issue.status-changed", and a timestamp$`, func() error {
		return iw.expectEvent("issue.status-changed", "SUT-1", "claude")
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
}
