package acceptance_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/cucumber/godog"
)

// miscWorld drives the remaining API-level scenarios: project scoping
// and archiving, identity uniqueness, audit ordering, assignment and
// work-stack membership, blocker completion, watermark anchoring, and
// the pop-vs-defer barrier race.
type miscWorld struct {
	iw *issueWorld
	cw *closeWorld

	otherProject string // "OTH"
	// Every unarchived project the scenario created, keyed by project
	// key. Keyed, not counted: an id looked up under the wrong name —
	// or an id that is the empty string, which every body contains —
	// is an assertion that cannot fail.
	liveProjects map[string]string
	otherIssue   string
	otherDoc     string
	crossBlock   string // a blocks relation spanning SUT and OTH
	ghostID      string // an identity id naming nothing
	collisionErr struct {
		status int
		body   string
	}
	templates map[string]string

	watermark  string
	raceStatus [2]int
	raceBodies [2]string
}

func (mw *miscWorld) reset() {
	*mw = miscWorld{iw: mw.iw, cw: mw.cw, templates: map[string]string{}, liveProjects: map[string]string{}}
}

func registerMiscSteps(sc *godog.ScenarioContext, cw *closeWorld) {
	iw := cw.iw
	mw := &miscWorld{iw: iw, cw: cw, templates: map[string]string{}, liveProjects: map[string]string{}}
	sc.Before(func(ctx context.Context, _ *godog.Scenario) (context.Context, error) {
		mw.reset()
		return ctx, nil
	})

	// --- content scopes to its project
	sc.Step(`^projects "SUT" and "OTH" both have issues and docs$`, func() error {
		if err := cw.ensureGitProject(); err != nil {
			return err
		}
		if _, err := iw.ensureIssue("SUT-1"); err != nil {
			return err
		}
		actor := iw.identities["operator"]
		if err := iw.s.call(http.MethodPost, "/projects/"+iw.project+"/documents",
			map[string]string{"title": "sut doc", "content": "sut content", "author": actor}); err != nil {
			return err
		}
		other, err := iw.createProjectKeyed("OTH")
		if err != nil {
			return err
		}
		mw.otherProject = other
		if err := iw.s.call(http.MethodPost, "/projects/"+other+"/issues",
			map[string]string{"title": "other issue", "actor": actor}); err != nil {
			return err
		}
		if err := iw.s.expectStatus(http.StatusCreated); err != nil {
			return err
		}
		var oi struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &oi); err != nil {
			return err
		}
		mw.otherIssue = oi.ID
		if err := iw.s.call(http.MethodPost, "/projects/"+other+"/documents",
			map[string]string{"title": "other doc", "content": "other content", "author": actor}); err != nil {
			return err
		}
		if err := iw.s.expectStatus(http.StatusCreated); err != nil {
			return err
		}
		var od struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &od); err != nil {
			return err
		}
		mw.otherDoc = od.ID
		return nil
	})
	sc.Step(`^SUT's issues and docs are listed$`, func() error {
		return nil // listings requested in the assertion step
	})
	sc.Step(`^nothing from "OTH" appears$`, func() error {
		if err := iw.s.call(http.MethodGet, "/projects/"+iw.project+"/issues", nil); err != nil {
			return err
		}
		if strings.Contains(string(iw.s.lastBody), mw.otherIssue) {
			return fmt.Errorf("OTH issue leaked into SUT listing")
		}
		if err := iw.s.call(http.MethodGet, "/projects/"+iw.project+"/documents", nil); err != nil {
			return err
		}
		if strings.Contains(string(iw.s.lastBody), mw.otherDoc) {
			return fmt.Errorf("OTH doc leaked into SUT listing")
		}
		return nil
	})

	// --- archive hides without deleting
	sc.Step(`^project "OTH" is archived$`, func() error {
		other, err := iw.createProjectKeyed("OTH")
		if err != nil {
			return err
		}
		mw.otherProject = other
		actor := iw.identities["operator"]
		if err := iw.s.call(http.MethodPost, "/projects/"+other+"/issues",
			map[string]string{"title": "archived content", "actor": actor}); err != nil {
			return err
		}
		var oi struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &oi); err != nil {
			return err
		}
		mw.otherIssue = oi.ID
		if err := iw.s.call(http.MethodPost, "/projects/"+other+"/archive",
			map[string]string{"actor": actor}); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	})
	sc.Step(`^live project "([^"]*)" exists$`, func(key string) error {
		live, err := iw.createProjectKeyed(key)
		if err != nil {
			return err
		}
		mw.liveProjects[key] = live
		return nil
	})
	sc.Step(`^default project listings hold "([^"]*)" and "([^"]*)", and not "OTH"$`, func(first, second string) error {
		if err := iw.s.call(http.MethodGet, "/projects", nil); err != nil {
			return err
		}
		body := string(iw.s.lastBody)
		if strings.Contains(body, mw.otherProject) {
			return fmt.Errorf("archived project in default listing: %s", body)
		}
		// Both live projects, named individually: "the archived one is
		// gone" is equally true of a listing that returned nothing, or
		// one that stopped after its first row.
		for _, key := range []string{first, second} {
			id, ok := mw.liveProjects[key]
			if !ok {
				return fmt.Errorf("scenario never created live project %q", key)
			}
			if !strings.Contains(body, id) {
				return fmt.Errorf("live project %q (%s) missing from default listing: %s", key, id, body)
			}
		}
		return nil
	})
	sc.Step(`^writing to "OTH" is rejected$`, func() error {
		actor := iw.identities["operator"]
		if err := iw.s.call(http.MethodPost, "/projects/"+mw.otherProject+"/issues",
			map[string]string{"title": "post-archive", "actor": actor}); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusConflict)
	})
	sc.Step(`^reading "OTH"'s content still works$`, func() error {
		if err := iw.s.call(http.MethodGet, "/issues/"+mw.otherIssue, nil); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	})

	// --- a cross-project block is guarded at both ends
	sc.Step(`^issue SUT-1 blocks issue OTH-1 in another project$`, func() error {
		from, err := iw.ensureIssue("SUT-1")
		if err != nil {
			return err
		}
		other, err := iw.createProjectKeyed("OTH")
		if err != nil {
			return err
		}
		mw.otherProject = other
		actor, err := iw.identity("human-brent")
		if err != nil {
			return err
		}
		// Archiving comes later, so OTH-1 is minted while OTH is still
		// writable — the block has to predate the freeze for removal to
		// be the thing under test.
		if err := iw.s.call(http.MethodPost, "/projects/"+other+"/issues",
			map[string]string{"title": "blocked elsewhere", "actor": actor}); err != nil {
			return err
		}
		if err := iw.s.expectStatus(http.StatusCreated); err != nil {
			return err
		}
		var oi struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &oi); err != nil {
			return err
		}
		mw.otherIssue = oi.ID
		if err := iw.s.call(http.MethodPost, "/issues/"+from+"/relations",
			map[string]string{"kind": "blocks", "to": mw.otherIssue, "actor": actor}); err != nil {
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
		mw.crossBlock = created.Relation.ID
		return nil
	})
	sc.Step(`^"OTH" is archived after the block exists$`, func() error {
		actor := iw.identities["operator"]
		if err := iw.s.call(http.MethodPost, "/projects/"+mw.otherProject+"/archive",
			map[string]string{"actor": actor}); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	})
	sc.Step(`^adding another block into "OTH" is rejected as read-only$`, func() error {
		from, err := iw.ensureIssue("SUT-2")
		if err != nil {
			return err
		}
		actor, err := iw.identity("human-brent")
		if err != nil {
			return err
		}
		if err := iw.s.call(http.MethodPost, "/issues/"+from+"/relations",
			map[string]string{"kind": "blocks", "to": mw.otherIssue, "actor": actor}); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusConflict)
	})
	sc.Step(`^removing the existing block from SUT-1's side is rejected as read-only$`, func() error {
		actor, err := iw.identity("human-brent")
		if err != nil {
			return err
		}
		// SUT is live, so the path project passes its own guard: only a
		// second look at the far end can refuse this.
		if err := iw.s.call(http.MethodDelete,
			"/issues/"+iw.issues["SUT-1"]+"/relations/"+mw.crossBlock+"?actor="+actor, nil); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusConflict)
	})
	sc.Step(`^both sides still show the block$`, func() error {
		for _, issue := range []string{iw.issues["SUT-1"], mw.otherIssue} {
			if err := iw.s.call(http.MethodGet, "/issues/"+issue+"/relations", nil); err != nil {
				return err
			}
			if err := iw.s.expectStatus(http.StatusOK); err != nil {
				return err
			}
			var rels []map[string]string
			if err := json.Unmarshal(iw.s.lastBody, &rels); err != nil {
				return err
			}
			found := false
			for _, rel := range rels {
				if rel["id"] == mw.crossBlock {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("block vanished from issue %s: %v", issue, rels)
			}
		}
		return nil
	})

	// --- unknown identity ids are rejected
	sc.Step(`^an identity id that names no identity$`, func() error {
		mw.ghostID = "01900000-0000-7000-8000-000000000000"
		return nil
	})
	sc.Step(`^issue (SUT-\d+) is assigned to that identity id$`, func(issueName string) error {
		if _, err := iw.ensureIssue(issueName); err != nil {
			return err
		}
		actor := iw.identities["operator"]
		return iw.s.call(http.MethodPost, "/issues/"+iw.issues[issueName]+"/assign",
			map[string]any{"assignee": mw.ghostID, "actor": actor})
	})
	sc.Step(`^nothing is created$`, func() error {
		if err := iw.s.call(http.MethodGet, "/issues/"+iw.issues["SUT-1"], nil); err != nil {
			return err
		}
		var got struct {
			Assignee *string `json:"assignee"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &got); err != nil {
			return err
		}
		if got.Assignee != nil {
			return fmt.Errorf("assignment persisted to ghost identity")
		}
		return nil
	})

	// --- an identifier in non-canonical casing
	sc.Step(`^an existing identity id rendered in uppercase$`, func() error {
		id, err := iw.identity("claude")
		if err != nil {
			return err
		}
		mw.ghostID = strings.ToUpper(id)
		return nil
	})
	sc.Step(`^the operation is rejected for the identifier's form, not as an unknown identity$`, func() error {
		if err := iw.s.expectStatus(http.StatusBadRequest); err != nil {
			return err
		}
		if err := iw.s.expectErrorCode("bad-request"); err != nil {
			return err
		}
		// Both refusals are bad requests, so the message is what tells
		// them apart — and the wrong one of the two is a lie: the id
		// names an identity, in the only casing the system stores.
		var envelope struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &envelope); err != nil {
			return err
		}
		if strings.Contains(envelope.Message, "names no identity") {
			return fmt.Errorf("uppercase id reached the store and came back unknown: %s", envelope.Message)
		}
		if !strings.Contains(envelope.Message, "must be an identity uuid") {
			return fmt.Errorf("expected the refusal to name the identifier's form, got %q", envelope.Message)
		}
		return nil
	})

	// --- every road into the server refuses a non-canonical identifier
	sc.Step(`^that id is supplied (.+)$`, func(road string) error {
		issueID, err := iw.ensureIssue("SUT-1")
		if err != nil {
			return err
		}
		switch strings.TrimSpace(road) {
		case "as the author of a new comment":
			return iw.s.call(http.MethodPost, "/comments",
				map[string]any{"issue": issueID, "author": mw.ghostID, "body": "from nobody"})
		case "as the identity in a work-stack pop path":
			return iw.s.call(http.MethodPost, "/identities/"+mw.ghostID+"/work-stack/pop", map[string]any{})
		case "as the assignee filter on a listing":
			return iw.s.call(http.MethodGet, "/projects/"+iw.project+"/issues?assignee="+url.QueryEscape(mw.ghostID), nil)
		}
		return fmt.Errorf("unknown road %q", road)
	})
	sc.Step(`^the request is refused for the identifier's form$`, func() error {
		if err := iw.s.expectStatus(http.StatusBadRequest); err != nil {
			return err
		}
		if err := iw.s.expectErrorCode("bad-request"); err != nil {
			return err
		}
		var envelope struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &envelope); err != nil {
			return err
		}
		// The message is what separates "this is not an identifier" from
		// "this identifier names nothing" — and the second is false here.
		if !strings.Contains(envelope.Message, "uuid") || strings.Contains(envelope.Message, "names no") {
			return fmt.Errorf("expected a refusal naming the identifier's form, got %q", envelope.Message)
		}
		return nil
	})

	// --- uniqueness collisions name their colliding resource
	sc.Step(`^a request that would create (.+)$`, func(kind string) error {
		actor, err := iw.identity("operator")
		if err != nil {
			return err
		}
		attempt := func(method, path string, body any) error {
			if err := iw.s.call(method, path, body); err != nil {
				return err
			}
			if err := iw.s.expectStatus(http.StatusCreated); err != nil {
				return err
			}
			// The duplicate attempt.
			if err := iw.s.call(method, path, body); err != nil {
				return err
			}
			mw.collisionErr.status = iw.s.lastResp.StatusCode
			mw.collisionErr.body = string(iw.s.lastBody)
			return nil
		}
		switch strings.TrimSpace(kind) {
		case "a duplicate project key":
			return attempt(http.MethodPost, "/projects", map[string]string{"key": "DUP", "name": "Dup", "actor": actor})
		case "a duplicate handle":
			return attempt(http.MethodPost, "/identities", map[string]string{"handle": "dup-handle", "kind": "agent"})
		case "a duplicate label name":
			return attempt(http.MethodPost, "/labels", map[string]string{"name": "dup-label"})
		case "a duplicate template name":
			return attempt(http.MethodPost, "/templates", map[string]string{"name": "dup-template", "content": "body"})
		default:
			return fmt.Errorf("unknown duplicate kind %q", kind)
		}
	})
	sc.Step(`^the error's code is "unique-violation" and conflicts names the colliding resource$`, func() error {
		var e struct {
			Code      string   `json:"code"`
			Conflicts []string `json:"conflicts"`
		}
		if err := json.Unmarshal([]byte(mw.collisionErr.body), &e); err != nil {
			return err
		}
		if e.Code != "unique-violation" || len(e.Conflicts) == 0 {
			return fmt.Errorf("collision error malformed: %s", mw.collisionErr.body)
		}
		return nil
	})

	// --- renaming a template into an existing name collides
	sc.Step(`^templates "([^"]*)" and "([^"]*)" exist$`, func(a, b string) error {
		for _, name := range []string{a, b} {
			if err := iw.s.call(http.MethodPost, "/templates", map[string]string{"name": name, "content": "body of " + name}); err != nil {
				return err
			}
			if err := iw.s.expectStatus(http.StatusCreated); err != nil {
				return err
			}
			var created struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(iw.s.lastBody, &created); err != nil {
				return err
			}
			mw.templates[name] = created.ID
		}
		return nil
	})
	sc.Step(`^"([^"]*)" is renamed to "([^"]*)"$`, func(from, to string) error {
		return iw.s.call(http.MethodPut, "/templates/"+mw.templates[from], map[string]string{"name": to})
	})
	sc.Step(`^the update is rejected with code "unique-violation"$`, func() error {
		if err := iw.s.expectStatus(http.StatusConflict); err != nil {
			return err
		}
		if !strings.Contains(string(iw.s.lastBody), "unique-violation") {
			return fmt.Errorf("wrong code: %s", iw.s.lastBody)
		}
		return nil
	})
	sc.Step(`^conflicts names the existing "([^"]*)" template's id$`, func(name string) error {
		if !strings.Contains(string(iw.s.lastBody), mw.templates[name]) {
			return fmt.Errorf("conflicts does not name %s's id: %s", name, iw.s.lastBody)
		}
		return nil
	})

	// --- history reads back in order
	sc.Step(`^issue (SUT-\d+) was created, assigned, and closed in that order$`, func(issueName string) error {
		if err := cw.ensureGitProject(); err != nil {
			return err
		}
		if _, err := iw.ensureIssue(issueName); err != nil {
			return err
		}
		assignee, err := iw.identity("claude")
		if err != nil {
			return err
		}
		actor := iw.identities["operator"]
		if err := iw.s.call(http.MethodPost, "/issues/"+iw.issues[issueName]+"/assign",
			map[string]any{"assignee": assignee, "actor": actor}); err != nil {
			return err
		}
		if err := iw.s.expectStatus(http.StatusOK); err != nil {
			return err
		}
		if err := cw.approvedReview(issueName, "audit-order"); err != nil {
			return err
		}
		if err := cw.close(issueName, 0, ""); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	})
	sc.Step(`^the audit history of (SUT-\d+) is requested$`, func(issueName string) error {
		if err := iw.s.call(http.MethodGet, "/issues/"+iw.issues[issueName]+"/events", nil); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	})
	sc.Step(`^each event shows actor, change kind, and time$`, func() error {
		var history []struct {
			Kind    string `json:"kind"`
			Actor   string `json:"actor"`
			Created string `json:"created"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &history); err != nil {
			return err
		}
		for i, e := range history {
			if e.Kind == "" || e.Actor == "" || e.Created == "" {
				return fmt.Errorf("event %d lacks actor/kind/time: %+v", i, e)
			}
		}
		return nil
	})
	sc.Step(`^the events are returned in chronological order$`, func() error {
		var history []struct {
			Kind    string `json:"kind"`
			Created string `json:"created"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &history); err != nil {
			return err
		}
		wantOrder := []string{"issue.created", "issue.assigned", "issue.status-changed"}
		positions := make([]int, 0, len(wantOrder))
		for _, want := range wantOrder {
			found := -1
			for i, e := range history {
				if e.Kind == want {
					found = i
					break
				}
			}
			if found < 0 {
				return fmt.Errorf("missing %s in history", want)
			}
			positions = append(positions, found)
		}
		for i := 1; i < len(positions); i++ {
			if positions[i] < positions[i-1] {
				return fmt.Errorf("history out of order: %v (%s before %s)", positions, wantOrder[i], wantOrder[i-1])
			}
		}
		for i := 1; i < len(history); i++ {
			if history[i].Created < history[i-1].Created {
				return fmt.Errorf("created timestamps regress at %d", i)
			}
		}
		return nil
	})

	// --- assignment to any identity
	sc.Step(`^identities "([^"]*)" and "([^"]*)" exist$`, func(a, b string) error {
		if _, err := iw.ensureIssue("SUT-1"); err != nil {
			return err
		}
		for _, handle := range []string{a, b} {
			if _, err := iw.identity(handle); err != nil {
				return err
			}
		}
		return nil
	})
	sc.Step(`^(SUT-\d+) is assigned to "([^"]*)"$`, func(issueName, handle string) error {
		actor := iw.identities["operator"]
		if err := iw.s.call(http.MethodPost, "/issues/"+iw.issues[issueName]+"/assign",
			map[string]any{"assignee": iw.identities[handle], "actor": actor}); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	})
	sc.Step(`^(SUT-\d+) shows assignee "([^"]*)"$`, func(issueName, handle string) error {
		if err := iw.s.call(http.MethodGet, "/issues/"+iw.issues[issueName], nil); err != nil {
			return err
		}
		var got struct {
			Assignee *string `json:"assignee"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &got); err != nil {
			return err
		}
		if got.Assignee == nil || *got.Assignee != iw.identities[handle] {
			return fmt.Errorf("assignee %v, want %s", got.Assignee, handle)
		}
		return nil
	})
	// The work stack IS the set of open issues assigned to the agent
	// (AC-pop-claims); membership asserts through the assignee listing.
	sc.Step(`^(SUT-\d+) is on "([^"]*)"'s work stack$`, func(issueName, handle string) error {
		if err := iw.s.call(http.MethodGet, "/projects/"+iw.project+"/issues?assignee="+iw.identities[handle]+"&status=open", nil); err != nil {
			return err
		}
		if !strings.Contains(string(iw.s.lastBody), iw.issues[issueName]) {
			return fmt.Errorf("%s missing from %s's stack: %s", issueName, handle, iw.s.lastBody)
		}
		return nil
	})
	sc.Step(`^(SUT-\d+) is unassigned by "([^"]*)"$`, func(issueName, handle string) error {
		actor := iw.identities[handle]
		if err := iw.s.call(http.MethodPost, "/issues/"+iw.issues[issueName]+"/assign",
			map[string]any{"assignee": nil, "actor": actor}); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	})
	sc.Step(`^(SUT-\d+) shows no assignee$`, func(issueName string) error {
		if err := iw.s.call(http.MethodGet, "/issues/"+iw.issues[issueName], nil); err != nil {
			return err
		}
		var got struct {
			Assignee *string `json:"assignee"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &got); err != nil {
			return err
		}
		if got.Assignee != nil {
			return fmt.Errorf("assignee still %v", *got.Assignee)
		}
		return nil
	})
	sc.Step(`^(SUT-\d+) is no longer on "([^"]*)"'s work stack$`, func(issueName, handle string) error {
		if err := iw.s.call(http.MethodGet, "/projects/"+iw.project+"/issues?assignee="+iw.identities[handle]+"&status=open", nil); err != nil {
			return err
		}
		if strings.Contains(string(iw.s.lastBody), iw.issues[issueName]) {
			return fmt.Errorf("%s still on %s's stack", issueName, handle)
		}
		return nil
	})

	// --- blocker completion unblocks
	sc.Step(`^(SUT-\d+) is blocked by open issue (SUT-\d+), both assigned to "([^"]*)"$`, func(blocked, blocker, handle string) error {
		if err := cw.ensureGitProject(); err != nil {
			return err
		}
		assignee, err := iw.identity(handle)
		if err != nil {
			return err
		}
		actor := iw.identities["operator"]
		for _, name := range []string{blocker, blocked} {
			if _, err := iw.ensureIssue(name); err != nil {
				return err
			}
			if err := iw.s.call(http.MethodPost, "/issues/"+iw.issues[name]+"/assign",
				map[string]any{"assignee": assignee, "actor": actor}); err != nil {
				return err
			}
			if err := iw.s.expectStatus(http.StatusOK); err != nil {
				return err
			}
		}
		if err := iw.s.call(http.MethodPost, "/issues/"+iw.issues[blocker]+"/relations",
			map[string]string{"to": iw.issues[blocked], "kind": "blocks", "actor": actor}); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusCreated)
	})
	sc.Step(`^(SUT-\d+) has an approved review$`, func(issueName string) error {
		return cw.approvedReview(issueName, "unblock")
	})
	sc.Step(`^popping "([^"]*)"'s stack can return (SUT-\d+)$`, func(handle string, expect string) error {
		if err := iw.s.call(http.MethodPost, "/identities/"+iw.identities[handle]+"/work-stack/pop",
			map[string]string{"actor": iw.identities[handle]}); err != nil {
			return err
		}
		if err := iw.s.expectStatus(http.StatusOK); err != nil {
			return err
		}
		if !strings.Contains(string(iw.s.lastBody), iw.issues[expect]) {
			return fmt.Errorf("pop did not return %s: %s", expect, iw.s.lastBody)
		}
		return nil
	})

	// --- watermarks anchor reads to the feed
	sc.Step(`^an issue read returns state with its feed watermark captured atomically$`, func() error {
		if _, err := iw.ensureIssue("SUT-1"); err != nil {
			return err
		}
		if err := iw.s.call(http.MethodGet, "/issues/"+iw.issues["SUT-1"], nil); err != nil {
			return err
		}
		var got struct {
			FeedWatermark string `json:"feed_watermark"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &got); err != nil {
			return err
		}
		if got.FeedWatermark == "" {
			return fmt.Errorf("issue read carries no watermark")
		}
		mw.watermark = got.FeedWatermark
		return nil
	})
	sc.Step(`^processing the feed through that watermark covers every event that could have affected the returned state$`, func() error {
		// Drain with until=watermark; the drain must complete (drained
		// true) and include the issue's creation event.
		if err := iw.s.call(http.MethodGet, "/events?until="+mw.watermark+"&limit=1000", nil); err != nil {
			return err
		}
		var page struct {
			Events []struct {
				Kind    string `json:"kind"`
				Subject string `json:"subject"`
			} `json:"events"`
			Drained bool `json:"drained"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &page); err != nil {
			return err
		}
		if !page.Drained {
			return fmt.Errorf("drain through the watermark did not complete")
		}
		for _, e := range page.Events {
			if e.Kind == "issue.created" && e.Subject == iw.issues["SUT-1"] {
				return nil
			}
		}
		return fmt.Errorf("creation event missing inside the watermark bound")
	})
	sc.Step(`^an issue list matches nothing$`, func() error {
		if err := iw.s.call(http.MethodGet, "/projects/"+iw.project+"/issues?status=blocked", nil); err != nil {
			return err
		}
		var page struct {
			Issues        []any  `json:"issues"`
			FeedWatermark string `json:"feed_watermark"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &page); err != nil {
			return err
		}
		if len(page.Issues) != 0 {
			return fmt.Errorf("expected an empty match")
		}
		mw.watermark = page.FeedWatermark
		return nil
	})
	sc.Step(`^the empty response still carries its watermark$`, func() error {
		if mw.watermark == "" {
			return fmt.Errorf("empty listing carries no watermark")
		}
		return nil
	})
	sc.Step(`^a work-stack pop claims an issue$`, func() error {
		assignee, err := iw.identity("claude")
		if err != nil {
			return err
		}
		actor := iw.identities["operator"]
		if err := iw.s.call(http.MethodPost, "/issues/"+iw.issues["SUT-1"]+"/assign",
			map[string]any{"assignee": assignee, "actor": actor}); err != nil {
			return err
		}
		if err := iw.s.call(http.MethodPost, "/identities/"+assignee+"/work-stack/pop",
			map[string]string{"actor": assignee}); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	})
	sc.Step(`^the result carries ONE authoritative top-level watermark — the nested issue carries none, so no two drain positions can disagree$`, func() error {
		var got map[string]json.RawMessage
		if err := json.Unmarshal(iw.s.lastBody, &got); err != nil {
			return err
		}
		if _, ok := got["feed_watermark"]; !ok {
			return fmt.Errorf("pop result carries no top-level watermark")
		}
		var nested struct {
			FeedWatermark *string `json:"feed_watermark"`
		}
		if raw, ok := got["issue"]; ok {
			if err := json.Unmarshal(raw, &nested); err != nil {
				return err
			}
			if nested.FeedWatermark != nil {
				return fmt.Errorf("nested issue carries its own watermark")
			}
		}
		return nil
	})
	sc.Step(`^a work-stack pop finds the stack empty$`, func() error {
		assignee := iw.identities["claude"]
		if err := iw.s.call(http.MethodPost, "/identities/"+assignee+"/work-stack/pop",
			map[string]string{"actor": assignee}); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	})
	sc.Step(`^the empty result still carries a top-level watermark anchoring the negative answer to the feed$`, func() error {
		var got struct {
			FeedWatermark string          `json:"feed_watermark"`
			Issue         json.RawMessage `json:"issue"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &got); err != nil {
			return err
		}
		if got.FeedWatermark == "" {
			return fmt.Errorf("empty pop carries no watermark")
		}
		if len(got.Issue) != 0 && string(got.Issue) != "null" {
			return fmt.Errorf("empty pop returned an issue: %s", got.Issue)
		}
		return nil
	})

	// --- simultaneous writers mutate exactly once
	sc.Step(`^two clients have each read status "open" client-side, before either server operation starts$`, func() error {
		return nil // both requests below carry expectations formed from that read
	})
	sc.Step(`^a pop by "([^"]*)" and a conditional transition to "deferred" expecting "open" are released together at a synchronization barrier so the server executes them overlapping$`, func(handle string) error {
		assignee := iw.identities[handle]
		actor := iw.identities["operator"]
		issueID := iw.issues["SUT-1"]
		var start, done sync.WaitGroup
		start.Add(1)
		done.Add(2)
		run := func(slot int, method, path string, body any) {
			defer done.Done()
			start.Wait()
			status, respBody, err := iw.s.callKeyed(method, path, body, newIdempotencyKey())
			if err != nil {
				mw.raceStatus[slot] = -1
				mw.raceBodies[slot] = err.Error()
				return
			}
			mw.raceStatus[slot] = status
			mw.raceBodies[slot] = respBody
		}
		go run(0, http.MethodPost, "/identities/"+assignee+"/work-stack/pop", map[string]string{"actor": assignee})
		go run(1, http.MethodPost, "/issues/"+issueID+"/status", map[string]any{
			"status": "deferred", "expected_status": "open", "actor": actor})
		start.Done()
		done.Wait()
		return nil
	})
	sc.Step(`^exactly one mutation is applied to (SUT-\d+)$`, func(issueName string) error {
		if err := iw.s.call(http.MethodGet, "/issues/"+iw.issues[issueName], nil); err != nil {
			return err
		}
		var got struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &got); err != nil {
			return err
		}
		if got.Status != "in-progress" && got.Status != "deferred" {
			return fmt.Errorf("status %q is neither winner's outcome", got.Status)
		}
		return nil
	})
	sc.Step(`^exactly one status event is recorded for (SUT-\d+)$`, func(issueName string) error {
		if err := iw.s.call(http.MethodGet, "/events?kind=issue.status-changed&subject="+iw.issues[issueName], nil); err != nil {
			return err
		}
		var page struct {
			Events []any `json:"events"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &page); err != nil {
			return err
		}
		if len(page.Events) != 1 {
			return fmt.Errorf("expected exactly 1 status event, got %d", len(page.Events))
		}
		return nil
	})
	sc.Step(`^if the pop won, the defer receives a conflict response reflecting the winner's committed status, never its stale client-side read$`, func() error {
		popWon := mw.raceStatus[0] == http.StatusOK && strings.Contains(mw.raceBodies[0], iw.issues["SUT-1"])
		if !popWon {
			return nil
		}
		if mw.raceStatus[1] != http.StatusConflict {
			return fmt.Errorf("defer against a won pop must 409, got %d: %s", mw.raceStatus[1], mw.raceBodies[1])
		}
		if !strings.Contains(mw.raceBodies[1], "in-progress") {
			return fmt.Errorf("conflict does not reflect the committed status: %s", mw.raceBodies[1])
		}
		return nil
	})
	sc.Step(`^if the defer won, the pop receives an explicit empty result and (SUT-\d+) remains "deferred"$`, func(issueName string) error {
		deferWon := mw.raceStatus[1] == http.StatusOK
		if !deferWon {
			return nil
		}
		if mw.raceStatus[0] != http.StatusOK || strings.Contains(mw.raceBodies[0], iw.issues[issueName]) {
			return fmt.Errorf("pop against a won defer must be empty: %d %s", mw.raceStatus[0], mw.raceBodies[0])
		}
		if err := iw.s.call(http.MethodGet, "/issues/"+iw.issues[issueName], nil); err != nil {
			return err
		}
		if !strings.Contains(string(iw.s.lastBody), `"deferred"`) {
			return fmt.Errorf("issue left %s", iw.s.lastBody)
		}
		return nil
	})
}
