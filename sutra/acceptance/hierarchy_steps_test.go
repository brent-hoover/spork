package acceptance_test

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/cucumber/godog"
)

// registerHierarchySteps drives the cascade/fence scenarios on top of
// the close world's review lifecycle.
func registerHierarchySteps(sc *godog.ScenarioContext, cw *closeWorld) {
	iw := cw.iw

	// makeChild attaches child under parent (parent_of). Every hierarchy
	// scenario's project must be git-backed BEFORE its first issue, so
	// later closes can submit code reviews.
	makeChild := func(parent, child string) error {
		if err := cw.ensureGitProject(); err != nil {
			return err
		}
		if err := iw.addRelation("parent_of", parent, child, "operator"); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusCreated)
	}
	// complete closes an issue through a fresh approved review and
	// asserts the status PERSISTED — a 200 with an unclosed issue must
	// fail the scenario that relied on it.
	complete := func(name string) error {
		if err := cw.ensureGitProject(); err != nil {
			return err
		}
		if err := cw.approvedReview(name, "close-"+name); err != nil {
			return err
		}
		if err := cw.close(name, 0, ""); err != nil {
			return err
		}
		if err := iw.s.expectStatus(http.StatusOK); err != nil {
			return err
		}
		if err := iw.readIssue(name); err != nil {
			return err
		}
		if iw.lastIssue.Status != "complete" {
			return fmt.Errorf("close of %s returned 200 but persisted %q", name, iw.lastIssue.Status)
		}
		return nil
	}
	transition := func(name, status string) error {
		if err := iw.transition(name, status); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	}
	expectStatusOf := func(name, want string) error {
		if err := iw.readIssue(name); err != nil {
			return err
		}
		if iw.lastIssue.Status != want {
			return fmt.Errorf("%s is %q, want %q", name, iw.lastIssue.Status, want)
		}
		return nil
	}

	// --- open children hold the parent open
	sc.Step(`^(SUT-\d+) has an approved review and a child in status "in-progress"$`, func(parent string) error {
		if err := cw.approvedReview(parent, "parent"); err != nil {
			return err
		}
		if err := makeChild(parent, "SUT-90"); err != nil {
			return err
		}
		return transition("SUT-90", "in-progress")
	})
	sc.Step(`^(SUT-\d+) is transitioned to "complete" naming its approved review at its current revision with its approval's verdict event$`, func(name string) error {
		return cw.close(name, 0, "")
	})
	expectOpenChildrenNaming := func(descendant string) error {
		if err := iw.s.expectStatus(http.StatusConflict); err != nil {
			return err
		}
		if err := iw.s.expectErrorCode("open-children"); err != nil {
			return err
		}
		var envelope struct {
			Conflicts []string `json:"conflicts"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &envelope); err != nil {
			return err
		}
		for _, id := range envelope.Conflicts {
			if id == iw.issues[descendant] {
				return nil
			}
		}
		return fmt.Errorf("conflicts %v does not name %s (%s)", envelope.Conflicts, descendant, iw.issues[descendant])
	}
	sc.Step(`^the transition is rejected naming the open child, not the review gate$`, func() error {
		return expectOpenChildrenNaming("SUT-90")
	})
	sc.Step(`^the child moves to status "deferred"$`, func() error {
		return transition("SUT-90", "deferred")
	})
	sc.Step(`^the transition succeeds — deferred children are parked, not open$`, func() error {
		if err := iw.s.expectStatus(http.StatusOK); err != nil {
			return err
		}
		return expectStatusOf(cw.lastClosed, "complete")
	})
	sc.Step(`^a deferred child of (SUT-\d+) itself has a descendant in status "open"$`, func(parent string) error {
		// SUT-90 is deferred beneath the (now complete) parent; give it
		// an open descendant, which reopens the parent via cascade.
		if err := makeChild("SUT-90", "SUT-91"); err != nil {
			return err
		}
		return expectStatusOf("SUT-91", "open")
	})
	sc.Step(`^(SUT-\d+) is transitioned to "complete" naming a fresh approved review at its current revision with its approval's verdict event$`, func(name string) error {
		if err := cw.approvedReview(name, "fresh-"+name); err != nil {
			return err
		}
		return cw.close(name, 0, "")
	})
	sc.Step(`^the transition is rejected naming the active descendant — deferred nodes cannot hide active work$`, func() error {
		return expectOpenChildrenNaming("SUT-91")
	})

	// --- subtree revision fences history, not just state
	revisionObserved := int64(-1)
	sc.Step(`^(SUT-\d+)'s subtree_revision is 7 as observed by a caller$`, func(name string) error {
		// Build history: a child attached and closed, transitions on the
		// child moving the parent's revision to exactly 7 by the end of
		// the NEXT step. Here: attach a child (rev 1) and mint activity.
		// attach (1), five transitions (2-6), close (7): the child ends
		// COMPLETE at the observed revision, so only history separates
		// the fence's view from the retry's.
		if err := makeChild(name, "SUT-95"); err != nil {
			return err
		}
		for _, s := range []string{"in-progress", "open", "in-progress", "open", "in-progress"} {
			if err := transition("SUT-95", s); err != nil {
				return err
			}
		}
		if err := complete("SUT-95"); err != nil {
			return err
		}
		if err := iw.readIssue(name); err != nil {
			return err
		}
		if iw.lastIssue.SubtreeRevision != 7 {
			return fmt.Errorf("setup expected revision 7, got %d", iw.lastIssue.SubtreeRevision)
		}
		revisionObserved = 7
		return nil
	})
	sc.Step(`^a child of (SUT-\d+) reopens and recompletes — two API transactions, each moving (SUT-\d+)'s revision exactly once, to 9$`, func(parent, _ string) error {
		// reopen (8) then recomplete (9): two transactions, each moving
		// the parent's revision exactly once, current state ending
		// exactly as observed — only history moved.
		if err := transition("SUT-95", "open"); err != nil {
			return err
		}
		if err := complete("SUT-95"); err != nil {
			return err
		}
		if err := iw.readIssue(parent); err != nil {
			return err
		}
		if iw.lastIssue.SubtreeRevision != 9 {
			return fmt.Errorf("expected revision 9, got %d", iw.lastIssue.SubtreeRevision)
		}
		return nil
	})
	sc.Step(`^(SUT-\d+) has an unspent approved review named, with its current revision and verdict event, on every attempt$`, func(name string) error {
		return cw.approvedReview(name, "fence-"+name)
	})
	sc.Step(`^(SUT-\d+) is transitioned to "complete" with expected_subtree_revision 7$`, func(name string) error {
		cw.lastClosed = name
		ref := cw.reviews[name]
		issueID := iw.issues[name]
		actor := iw.identities["operator"]
		return iw.s.call(http.MethodPost, "/issues/"+issueID+"/status", map[string]any{
			"status": "complete", "review": ref.id, "review_revision": ref.revision,
			"review_verdict_event": ref.verdictEvent, "actor": actor,
			"expected_subtree_revision": revisionObserved,
		})
	})
	sc.Step(`^the transition is rejected with a conflict — current state matches but history moved$`, func() error {
		if err := iw.s.expectStatus(http.StatusConflict); err != nil {
			return err
		}
		return iw.s.expectErrorCode("expected-subtree-revision-mismatch")
	})
	sc.Step(`^the caller re-reads and retries with expected_subtree_revision 9$`, func() error {
		cw.lastClosed = "SUT-1"
		if err := iw.readIssue("SUT-1"); err != nil {
			return err
		}
		ref := cw.reviews["SUT-1"]
		issueID := iw.issues["SUT-1"]
		actor := iw.identities["operator"]
		return iw.s.call(http.MethodPost, "/issues/"+issueID+"/status", map[string]any{
			"status": "complete", "review": ref.id, "review_revision": ref.revision,
			"review_verdict_event": ref.verdictEvent, "actor": actor,
			"expected_subtree_revision": iw.lastIssue.SubtreeRevision,
		})
	})
	sc.Step(`^the transition succeeds under its ordinary gates$`, func() error {
		if err := iw.s.expectStatus(http.StatusOK); err != nil {
			return err
		}
		return expectStatusOf(cw.lastClosed, "complete")
	})
	sc.Step(`^a complete-free hierarchy where nothing can reopen$`, func() error {
		for _, name := range []string{"SUT-96", "SUT-97"} {
			if _, err := iw.ensureIssue(name); err != nil {
				return err
			}
		}
		return iw.readIssue("SUT-96")
	})
	sc.Step(`^a child is attached beneath a parent$`, func() error {
		revisionObserved = iw.lastIssue.SubtreeRevision
		return makeChild("SUT-96", "SUT-97")
	})
	sc.Step(`^subtree_revision still increments on the parent and every ancestor$`, func() error {
		if err := iw.readIssue("SUT-96"); err != nil {
			return err
		}
		if iw.lastIssue.SubtreeRevision != revisionObserved+1 {
			return fmt.Errorf("attach moved revision %d -> %d, want +1", revisionObserved, iw.lastIssue.SubtreeRevision)
		}
		return nil
	})

	// --- detaching a child cannot leave a stale close fence
	sc.Step(`^parent (SUT-\d+) has an approved review and no other descendant, so it is otherwise closable$`, func(parent string) error {
		if err := cw.ensureGitProject(); err != nil {
			return err
		}
		if _, err := iw.ensureIssue(parent); err != nil {
			return err
		}
		return cw.approvedReview(parent, "detach-"+parent)
	})
	sc.Step(`^a caller observed (SUT-\d+)'s subtree_revision while its child (SUT-\d+) was complete$`, func(parent, child string) error {
		if err := makeChild(parent, child); err != nil {
			return err
		}
		if err := complete(child); err != nil {
			return err
		}
		if err := iw.readIssue(parent); err != nil {
			return err
		}
		revisionObserved = iw.lastIssue.SubtreeRevision
		return nil
	})
	sc.Step(`^(SUT-\d+) is detached from (SUT-\d+) and then reopened$`, func(child, parent string) error {
		rel := iw.relations[parent+">"+child]
		actor := iw.identities["operator"]
		if err := iw.s.call(http.MethodDelete, "/issues/"+iw.issues[parent]+"/relations/"+rel+"?actor="+actor, nil); err != nil {
			return err
		}
		if err := iw.s.expectStatus(http.StatusNoContent); err != nil {
			return err
		}
		return transition(child, "open")
	})
	sc.Step(`^the removal incremented (SUT-\d+)'s subtree_revision at detach time$`, func(parent string) error {
		if err := iw.readIssue(parent); err != nil {
			return err
		}
		if iw.lastIssue.SubtreeRevision <= revisionObserved {
			return fmt.Errorf("detach left revision at %d (observed %d)", iw.lastIssue.SubtreeRevision, revisionObserved)
		}
		return nil
	})
	sc.Step(`^completing (SUT-\d+) — naming its approved review, revision, and verdict event — with the previously observed expected_subtree_revision is rejected with a conflict naming the revision, not the review gate$`, func(parent string) error {
		ref := cw.reviews[parent]
		actor := iw.identities["operator"]
		if err := iw.s.call(http.MethodPost, "/issues/"+iw.issues[parent]+"/status", map[string]any{
			"status": "complete", "review": ref.id, "review_revision": ref.revision,
			"review_verdict_event": ref.verdictEvent, "actor": actor,
			"expected_subtree_revision": revisionObserved,
		}); err != nil {
			return err
		}
		if err := iw.s.expectStatus(http.StatusConflict); err != nil {
			return err
		}
		return iw.s.expectErrorCode("expected-subtree-revision-mismatch")
	})

	// --- reopen cascades
	sc.Step(`^(SUT-\d+) is "complete" and its child (SUT-\d+) is "complete"$`, func(parent, child string) error {
		if err := makeChild(parent, child); err != nil {
			return err
		}
		if err := complete(child); err != nil {
			return err
		}
		return complete(parent)
	})
	sc.Step(`^(SUT-\d+) is reopened to "open"$`, func(name string) error {
		return transition(name, "open")
	})
	sc.Step(`^(SUT-\d+) returns to "open" in the same transaction$`, func(name string) error {
		return expectStatusOf(name, "open")
	})
	sc.Step(`^a status event is recorded for both (SUT-\d+) and (SUT-\d+), sharing the reopen request's operation id$`, func(a, b string) error {
		ops := map[string]map[string]bool{}
		for _, name := range []string{a, b} {
			if err := iw.s.call(http.MethodGet, "/events?kind=issue.status-changed&subject="+iw.issues[name], nil); err != nil {
				return err
			}
			var page struct {
				Events []struct {
					Operation string `json:"operation"`
				} `json:"events"`
			}
			if err := json.Unmarshal(iw.s.lastBody, &page); err != nil {
				return err
			}
			ops[name] = map[string]bool{}
			for _, e := range page.Events {
				ops[name][e.Operation] = true
			}
		}
		for op := range ops[a] {
			if ops[b][op] {
				return nil // a shared operation id exists
			}
		}
		return fmt.Errorf("no shared operation id between %s and %s status events", a, b)
	})
	sc.Step(`^a fresh hierarchy where grandparent (SUT-\d+), parent (SUT-\d+), and leaf (SUT-\d+) are all "complete"$`, func(gp, p, leaf string) error {
		if err := makeChild(gp, p); err != nil {
			return err
		}
		if err := makeChild(p, leaf); err != nil {
			return err
		}
		for _, name := range []string{leaf, p, gp} {
			if err := complete(name); err != nil {
				return err
			}
		}
		return nil
	})
	sc.Step(`^(SUT-\d+) and (SUT-\d+) both return to "open" in the same transaction$`, func(a, b string) error {
		if err := expectStatusOf(a, "open"); err != nil {
			return err
		}
		return expectStatusOf(b, "open")
	})
	sc.Step(`^a status event is recorded for each reopened ancestor$`, func() error {
		// The reopen transaction's operation id is the newest status
		// event on the LEAF; every reopened ancestor must carry a
		// status event under that SAME operation — setup-time closes
		// cannot satisfy this.
		if err := iw.s.call(http.MethodGet, "/events?kind=issue.status-changed&subject="+iw.issues["SUT-12"], nil); err != nil {
			return err
		}
		var page struct {
			Events []struct {
				Operation string `json:"operation"`
			} `json:"events"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &page); err != nil {
			return err
		}
		if len(page.Events) == 0 {
			return fmt.Errorf("no status events on the leaf")
		}
		reopenOp := page.Events[len(page.Events)-1].Operation
		for _, name := range []string{"SUT-11", "SUT-10"} {
			if err := iw.s.call(http.MethodGet, "/events?kind=issue.status-changed&subject="+iw.issues[name], nil); err != nil {
				return err
			}
			if err := json.Unmarshal(iw.s.lastBody, &page); err != nil {
				return err
			}
			found := false
			for _, e := range page.Events {
				if e.Operation == reopenOp {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("%s has no status event under the reopen operation %s", name, reopenOp)
			}
		}
		return nil
	})
	sc.Step(`^a fresh hierarchy where (SUT-\d+) is "complete" with a child (SUT-\d+) in status "deferred"$`, func(parent, child string) error {
		if err := makeChild(parent, child); err != nil {
			return err
		}
		if err := transition(child, "deferred"); err != nil {
			return err
		}
		return complete(parent)
	})
	sc.Step(`^(SUT-\d+) transitions to "in-progress"$`, func(name string) error {
		return transition(name, "in-progress")
	})
	sc.Step(`^(SUT-\d+) transitions to "blocked"$`, func(name string) error {
		return transition(name, "blocked")
	})
	sc.Step(`^(SUT-\d+) reopens in the same transaction$`, func(name string) error {
		return expectStatusOf(name, "open")
	})
	sc.Step(`^(SUT-\d+) reopens in the same transaction — blocked is active work$`, func(name string) error {
		return expectStatusOf(name, "open")
	})

	// --- blocked descendant beneath a deferred child holds the parent
	sc.Step(`^a fresh hierarchy where (SUT-\d+) has an approved review and a descendant in status "blocked" beneath a deferred child$`, func(parent string) error {
		if err := makeChild(parent, "SUT-28"); err != nil {
			return err
		}
		if err := makeChild("SUT-28", "SUT-29"); err != nil {
			return err
		}
		if err := transition("SUT-29", "blocked"); err != nil {
			return err
		}
		if err := transition("SUT-28", "deferred"); err != nil {
			return err
		}
		return cw.approvedReview(parent, "blocked-desc")
	})
	sc.Step(`^the transition is rejected naming the blocked descendant$`, func() error {
		return expectOpenChildrenNaming("SUT-29")
	})

	// --- attachments reopening complete parents
	sc.Step(`^a fresh issue (SUT-\d+) in status "complete" and an unrelated issue (SUT-\d+) in status "open"$`, func(parent, child string) error {
		if err := complete(parent); err != nil {
			return err
		}
		_, err := iw.ensureIssue(child)
		return err
	})
	sc.Step(`^(SUT-\d+) is attached as a child of (SUT-\d+)$`, func(child, parent string) error {
		return makeChild(parent, child)
	})
	sc.Step(`^a fresh issue (SUT-\d+) in status "complete"$`, func(name string) error {
		return complete(name)
	})
	sc.Step(`^a deferred issue (SUT-\d+) whose own subtree contains an issue in status "open"$`, func(root string) error {
		if err := makeChild(root, "SUT-42"); err != nil {
			return err
		}
		if err := transition(root, "deferred"); err != nil {
			return err
		}
		return expectStatusOf("SUT-42", "open")
	})
	sc.Step(`^(SUT-\d+) reopens in the same transaction — a deferred root cannot hide active work it carries in$`, func(name string) error {
		return expectStatusOf(name, "open")
	})

	// --- relation conflicts carry their distinct codes
	lastCondition := ""
	sc.Step(`^a relation request that fails because of (.+)$`, func(condition string) error {
		lastCondition = condition
		for _, name := range []string{"SUT-60", "SUT-61"} {
			if _, err := iw.ensureIssue(name); err != nil {
				return err
			}
		}
		actor := iw.identities["operator"]
		mustCreate := func(kind, from, to string) error {
			if err := iw.addRelation(kind, from, to, "operator"); err != nil {
				return err
			}
			// The prerequisite must actually exist — an implementation
			// rejecting everything with the right code must not pass.
			return iw.s.expectStatus(http.StatusCreated)
		}
		switch condition {
		case "the relation already existing":
			if err := mustCreate("blocks", "SUT-60", "SUT-61"); err != nil {
				return err
			}
			return iw.addRelation("blocks", "SUT-60", "SUT-61", "operator")
		case "an ancestry cycle":
			if err := mustCreate("parent_of", "SUT-60", "SUT-61"); err != nil {
				return err
			}
			return iw.addRelation("parent_of", "SUT-61", "SUT-60", "operator")
		case "a blocking cycle":
			if err := mustCreate("blocks", "SUT-60", "SUT-61"); err != nil {
				return err
			}
			return iw.addRelation("blocks", "SUT-61", "SUT-60", "operator")
		case "the project being archived":
			if err := iw.s.call(http.MethodPost, "/projects/"+iw.project+"/archive", map[string]string{"actor": actor}); err != nil {
				return err
			}
			if err := iw.s.expectStatus(http.StatusOK); err != nil {
				return err
			}
			return iw.addRelation("blocks", "SUT-60", "SUT-61", "operator")
		}
		return fmt.Errorf("unknown condition %q", condition)
	})
	sc.Step(`^the error's code is exactly (.+), never inferred from message text$`, func(code string) error {
		code = trimQuotes(code)
		_ = lastCondition
		return iw.s.expectErrorCode(code)
	})
}

func trimQuotes(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}
