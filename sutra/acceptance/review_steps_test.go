package acceptance_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/cucumber/godog"
)

// crWorld drives REQ-code-review. The feature names 40-char shas
// symbolically ("a1b2c3d4…"); real commits are minted per scenario and
// aliased, mirroring the symbolic-pin amendment logged in
// spec-gaps.md for REQ-close-requires-review.
type crWorld struct {
	iw *issueWorld
	cw *closeWorld

	sha           map[string]string // symbolic -> minted
	deliverable   string            // last-read deliverable content
	verdictEvents map[string]string // label ("A", "first-cr") -> event id
	comments      []string          // comment ids on the review
	lastStatus    int
	lastBody      string
	preState      map[string]any // review snapshot before a rejected mutation
}

func (rw *crWorld) reset() {
	*rw = crWorld{iw: rw.iw, cw: rw.cw, sha: map[string]string{}, verdictEvents: map[string]string{}}
}

// real resolves a symbolic sha, minting a fresh commit on first use.
func (rw *crWorld) real(symbolic string) (string, error) {
	if sha, ok := rw.sha[symbolic]; ok {
		return sha, nil
	}
	sha, err := rw.cw.mintCommit()
	if err != nil {
		return "", err
	}
	rw.sha[symbolic] = sha
	return sha, nil
}

func (rw *crWorld) git(args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", rw.cw.repoPath}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %v: %v — %s", args, err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// submit posts a review; body extras merge over the defaults.
func (rw *crWorld) submit(issueName string, extras map[string]any) error {
	iw := rw.iw
	if err := rw.cw.ensureGitProject(); err != nil {
		return err
	}
	issueID, err := iw.ensureIssue(issueName)
	if err != nil {
		return err
	}
	author, err := iw.identity("claude")
	if err != nil {
		return err
	}
	body := map[string]any{"issue": issueID, "author": author}
	for k, v := range extras {
		body[k] = v
	}
	if err := iw.s.call(http.MethodPost, "/reviews", body); err != nil {
		return err
	}
	rw.lastStatus = iw.s.lastResp.StatusCode
	rw.lastBody = string(iw.s.lastBody)
	if rw.lastStatus != http.StatusCreated {
		return nil // rejection scenarios assert on lastStatus
	}
	var created struct {
		ID       string `json:"id"`
		Revision int64  `json:"revision"`
	}
	if err := json.Unmarshal(iw.s.lastBody, &created); err != nil {
		return err
	}
	rw.cw.reviews[issueName] = reviewRef{id: created.ID, revision: created.Revision}
	return nil
}

// verdictLabeled applies a verdict at the current revision and stores
// the resulting event id under label.
func (rw *crWorld) verdictLabeled(verdict, label string) error {
	if err := rw.cw.verdict("SUT-1", verdict); err != nil {
		return err
	}
	ref := rw.cw.reviews["SUT-1"]
	rw.verdictEvents[label] = ref.verdictEvent
	return nil
}

// resubmit posts a resubmission with a fresh commit unless extras pin
// one, recording the raw outcome.
func (rw *crWorld) resubmit(extras map[string]any) error {
	iw := rw.iw
	ref := rw.cw.reviews["SUT-1"]
	author := iw.identities["claude"]
	if author == "" {
		author = iw.identities["operator"]
	}
	verdictEvent := ref.verdictEvent
	if verdictEvent == "" {
		// An open review has no verdict event to answer; a placeholder
		// uuid lets the request reach the state guard, which is the
		// rejection under test.
		verdictEvent = "01900000-0000-7000-8000-00000000dead"
	}
	body := map[string]any{
		"author": author, "expected_revision": ref.revision, "expected_verdict_event": verdictEvent,
	}
	if _, ok := extras["commit"]; !ok {
		sha, err := rw.cw.mintCommit()
		if err != nil {
			return err
		}
		body["branch"] = "work"
		body["commit"] = sha
	}
	for k, v := range extras {
		body[k] = v
	}
	if err := iw.s.call(http.MethodPost, "/reviews/"+ref.id+"/resubmit", body); err != nil {
		return err
	}
	rw.lastStatus = iw.s.lastResp.StatusCode
	rw.lastBody = string(iw.s.lastBody)
	if rw.lastStatus == http.StatusOK {
		var updated struct {
			Revision           int64   `json:"revision"`
			LatestVerdictEvent *string `json:"latest_verdict_event"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &updated); err != nil {
			return err
		}
		ref.revision = updated.Revision
		if updated.LatestVerdictEvent != nil {
			ref.verdictEvent = *updated.LatestVerdictEvent
		}
		rw.cw.reviews["SUT-1"] = ref
	}
	return nil
}

func (rw *crWorld) reviewState() (map[string]any, error) {
	ref := rw.cw.reviews["SUT-1"]
	if err := rw.iw.s.call(http.MethodGet, "/reviews/"+ref.id, nil); err != nil {
		return nil, err
	}
	var got map[string]any
	if err := json.Unmarshal(rw.iw.s.lastBody, &got); err != nil {
		return nil, err
	}
	return got, nil
}

func (rw *crWorld) readDeliverable(query string) (string, error) {
	ref := rw.cw.reviews["SUT-1"]
	if err := rw.iw.s.call(http.MethodGet, "/reviews/"+ref.id+"/deliverable"+query, nil); err != nil {
		return "", err
	}
	if err := rw.iw.s.expectStatus(http.StatusOK); err != nil {
		return "", err
	}
	return string(rw.iw.s.lastBody), nil
}

func (rw *crWorld) expectConflict() error {
	if rw.lastStatus != http.StatusConflict {
		return fmt.Errorf("expected 409, got %d: %s", rw.lastStatus, rw.lastBody)
	}
	return nil
}

// snapshot captures the review's externally visible state for
// no-mutation assertions.
func (rw *crWorld) snapshot() error {
	got, err := rw.reviewState()
	if err != nil {
		return err
	}
	rw.preState = got
	return nil
}

func (rw *crWorld) unchanged() error {
	got, err := rw.reviewState()
	if err != nil {
		return err
	}
	for _, field := range []string{"state", "revision", "commit", "branch", "latest_verdict_event"} {
		if fmt.Sprint(got[field]) != fmt.Sprint(rw.preState[field]) {
			return fmt.Errorf("%s changed: %v -> %v", field, rw.preState[field], got[field])
		}
	}
	preSubs, _ := rw.preState["submissions"].([]any)
	gotSubs, _ := got["submissions"].([]any)
	if len(preSubs) != len(gotSubs) {
		return fmt.Errorf("submission count changed: %d -> %d", len(preSubs), len(gotSubs))
	}
	return nil
}

func registerReviewSteps(sc *godog.ScenarioContext, cw *closeWorld) {
	iw := cw.iw
	rw := &crWorld{iw: iw, cw: cw, sha: map[string]string{}, verdictEvents: map[string]string{}}
	sc.Before(func(ctx context.Context, _ *godog.Scenario) (context.Context, error) {
		rw.reset()
		return ctx, nil
	})

	// --- agent submits a review
	sc.Step(`^agent "([^"]*)" finished work on issue (SUT-\d+) on branch "([^"]*)" at commit "([0-9a-f]{40})"$`, func(handle, issueName, branch, symbolic string) error {
		if err := cw.ensureGitProject(); err != nil {
			return err
		}
		if _, err := iw.ensureIssue(issueName); err != nil {
			return err
		}
		if _, err := iw.identity(handle); err != nil {
			return err
		}
		_, err := rw.real(symbolic)
		return err
	})
	sc.Step(`^it creates a review for (SUT-\d+) with deliverable branch "([^"]*)" pinned at commit "([0-9a-f]{40})" and session "([^"]*)"$`, func(issueName, branch, symbolic, session string) error {
		sha, err := rw.real(symbolic)
		if err != nil {
			return err
		}
		return rw.submit(issueName, map[string]any{"branch": branch, "commit": sha, "session": session})
	})
	sc.Step(`^the review exists in state "([^"]*)"$`, func(state string) error {
		got, err := rw.reviewState()
		if err != nil {
			return err
		}
		if got["state"] != state {
			return fmt.Errorf("state %v, want %s", got["state"], state)
		}
		return nil
	})
	sc.Step(`^it is listed for reviewers$`, func() error {
		if err := iw.s.call(http.MethodGet, "/reviews?state=open", nil); err != nil {
			return err
		}
		if !strings.Contains(string(iw.s.lastBody), cw.reviews["SUT-1"].id) {
			return fmt.Errorf("review missing from open listing")
		}
		return nil
	})

	// --- reviewer sees the deliverable
	sc.Step(`^an open review with deliverable branch "([^"]*)" pinned at commit "([0-9a-f]{40})"$`, func(branch, symbolic string) error {
		sha, err := rw.real(symbolic)
		if err != nil {
			return err
		}
		return rw.submit("SUT-1", map[string]any{"branch": branch, "commit": sha})
	})
	sc.Step(`^a human opens it in the web UI$`, func() error {
		content, err := rw.readDeliverable("")
		if err != nil {
			return err
		}
		rw.deliverable = content
		return nil
	})
	sc.Step(`^the deliverable content is shown for reading$`, func() error {
		if rw.deliverable == "" {
			return fmt.Errorf("deliverable is empty")
		}
		return nil
	})
	sc.Step(`^later commits pushed to "([^"]*)" do not change the reviewed content$`, func(string) error {
		if _, err := cw.mintCommit(); err != nil {
			return err
		}
		content, err := rw.readDeliverable("")
		if err != nil {
			return err
		}
		if content != rw.deliverable {
			return fmt.Errorf("reviewed content moved with the branch")
		}
		return nil
	})

	// --- pinned base survives default branch movement
	sc.Step(`^a code submission for (SUT-\d+) with base_commit "([0-9a-f]{40})" and commit "([0-9a-f]{40})"$`, func(issueName, baseSym, commitSym string) error {
		sha, err := rw.real(commitSym)
		if err != nil {
			return err
		}
		if err := rw.submit(issueName, map[string]any{"branch": "work", "commit": sha}); err != nil {
			return err
		}
		got, err := rw.reviewState()
		if err != nil {
			return err
		}
		subs, _ := got["submissions"].([]any)
		if len(subs) == 0 {
			return fmt.Errorf("no submissions on the review")
		}
		base, _ := subs[0].(map[string]any)["base_commit"].(string)
		if base == "" {
			return fmt.Errorf("submission stored no base_commit")
		}
		rw.sha[baseSym] = base
		content, err := rw.readDeliverable("")
		if err != nil {
			return err
		}
		rw.deliverable = content
		return nil
	})
	sc.Step(`^new commits are merged onto the project's default branch$`, func() error {
		if _, err := rw.git("checkout", "main"); err != nil {
			return err
		}
		if err := os.WriteFile(rw.cw.repoPath+"/main-move.txt", []byte("moved"), 0o644); err != nil {
			return err
		}
		if _, err := rw.git("add", "."); err != nil {
			return err
		}
		if _, err := rw.git("commit", "-m", "advance main"); err != nil {
			return err
		}
		_, err := rw.git("checkout", "work")
		return err
	})
	sc.Step(`^the submission's stored base_commit is still "([0-9a-f]{40})"$`, func(baseSym string) error {
		got, err := rw.reviewState()
		if err != nil {
			return err
		}
		subs, _ := got["submissions"].([]any)
		base, _ := subs[0].(map[string]any)["base_commit"].(string)
		if base != rw.sha[baseSym] {
			return fmt.Errorf("base_commit moved: %s != %s", base, rw.sha[baseSym])
		}
		return nil
	})
	sc.Step(`^the rendered diff is identical to before the default branch moved$`, func() error {
		content, err := rw.readDeliverable("")
		if err != nil {
			return err
		}
		if content != rw.deliverable {
			return fmt.Errorf("diff changed after default branch movement")
		}
		return nil
	})

	// --- feedback threads on the review
	sc.Step(`^an open review$`, func() error {
		return cw.createReview("SUT-1", "work")
	})
	sc.Step(`^a human comments and another replies$`, func() error {
		ref := cw.reviews["SUT-1"]
		first, err := iw.identity("human-brent")
		if err != nil {
			return err
		}
		second, err := iw.identity("human-alex")
		if err != nil {
			return err
		}
		if err := iw.s.call(http.MethodPost, "/comments", map[string]any{
			"review": ref.id, "review_revision": ref.revision, "author": first, "body": "looks odd here"}); err != nil {
			return err
		}
		if err := iw.s.expectStatus(http.StatusCreated); err != nil {
			return err
		}
		var c1 struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &c1); err != nil {
			return err
		}
		rw.comments = append(rw.comments, c1.ID)
		if err := iw.s.call(http.MethodPost, "/comments", map[string]any{
			"review": ref.id, "review_revision": ref.revision, "author": second, "body": "agreed", "parent": c1.ID}); err != nil {
			return err
		}
		if err := iw.s.expectStatus(http.StatusCreated); err != nil {
			return err
		}
		var c2 struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &c2); err != nil {
			return err
		}
		rw.comments = append(rw.comments, c2.ID)
		return nil
	})
	sc.Step(`^both comments anchor to the review$`, func() error {
		ref := cw.reviews["SUT-1"]
		if err := iw.s.call(http.MethodGet, "/comments?review="+ref.id, nil); err != nil {
			return err
		}
		for _, id := range rw.comments {
			if !strings.Contains(string(iw.s.lastBody), id) {
				return fmt.Errorf("comment %s not anchored to the review", id)
			}
		}
		return nil
	})
	sc.Step(`^the reply is threaded under the first comment$`, func() error {
		var list []struct {
			ID     string  `json:"id"`
			Parent *string `json:"parent"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &list); err != nil {
			return err
		}
		for _, c := range list {
			if c.ID == rw.comments[1] {
				if c.Parent == nil || *c.Parent != rw.comments[0] {
					return fmt.Errorf("reply not threaded under the first comment")
				}
				return nil
			}
		}
		return fmt.Errorf("reply missing")
	})

	// --- verdict changes state and is recorded
	sc.Step(`^a human sets it to "(approved|changes-requested)"$`, func(verdict string) error {
		return rw.verdictLabeled(verdict, "latest")
	})
	sc.Step(`^the review state is "([^"]*)"$`, func(state string) error {
		got, err := rw.reviewState()
		if err != nil {
			return err
		}
		if got["state"] != state {
			return fmt.Errorf("state %v, want %s", got["state"], state)
		}
		return nil
	})
	sc.Step(`^an event records the actor and time$`, func() error {
		return iw.expectEvent("review.changes-requested", "SUT-1", "human-brent")
	})

	// --- verdicts can be revised for the current revision
	sc.Step(`^a review approved at its current revision$`, func() error {
		return cw.approvedReview("SUT-1", "work")
	})
	sc.Step(`^a human sets it to "(approved|changes-requested)" carrying the same revision$`, func(verdict string) error {
		return rw.verdictLabeled(verdict, "latest")
	})
	sc.Step(`^a verdict event is recorded$`, func() error {
		return iw.expectEvent("review.changes-requested", "SUT-1", "human-brent")
	})
	sc.Step(`^another verdict event is recorded$`, func() error {
		return iw.expectEvent("review.approved", "SUT-1", "human-brent")
	})

	// --- approval publishes an event
	sc.Step(`^an open review for issue (SUT-\d+)$`, func(issueName string) error {
		return cw.createReview(issueName, "work")
	})
	sc.Step(`^an event of kind "review\.approved" referencing the review and (SUT-\d+) is published$`, func(issueName string) error {
		if err := iw.s.call(http.MethodGet, "/events?kind=review.approved&subject="+iw.issues[issueName], nil); err != nil {
			return err
		}
		if !strings.Contains(string(iw.s.lastBody), cw.reviews[issueName].id) {
			return fmt.Errorf("approval event does not reference the review: %s", iw.s.lastBody)
		}
		return nil
	})

	// --- rework routes back with session context
	sc.Step(`^a review created with session "([^"]*)" is set to "changes-requested"$`, func(session string) error {
		sha, err := cw.mintCommit()
		if err != nil {
			return err
		}
		if err := rw.submit("SUT-1", map[string]any{"branch": "work", "commit": sha, "session": session}); err != nil {
			return err
		}
		return rw.verdictLabeled("changes-requested", "cr1")
	})
	sc.Step(`^the emitted event carries session "([^"]*)" and the issue ref$`, func(session string) error {
		if err := iw.s.call(http.MethodGet, "/events?kind=review.changes-requested&subject="+iw.issues["SUT-1"], nil); err != nil {
			return err
		}
		var page struct {
			Events []struct {
				Payload map[string]any `json:"payload"`
				Subject string         `json:"subject"`
			} `json:"events"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &page); err != nil {
			return err
		}
		if len(page.Events) == 0 {
			return fmt.Errorf("no changes-requested event")
		}
		last := page.Events[len(page.Events)-1]
		if last.Payload["session"] != session {
			return fmt.Errorf("event session %v, want %s", last.Payload["session"], session)
		}
		if last.Subject != iw.issues["SUT-1"] {
			return fmt.Errorf("event subject is not the issue")
		}
		return nil
	})
	sc.Step(`^a reviewer commented on the review before resubmission$`, func() error {
		ref := cw.reviews["SUT-1"]
		author, err := iw.identity("human-brent")
		if err != nil {
			return err
		}
		if err := iw.s.call(http.MethodPost, "/comments", map[string]any{
			"review": ref.id, "review_revision": ref.revision, "author": author, "body": "pre-rework note"}); err != nil {
			return err
		}
		if err := iw.s.expectStatus(http.StatusCreated); err != nil {
			return err
		}
		var c struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &c); err != nil {
			return err
		}
		rw.comments = append(rw.comments, c.ID)
		return nil
	})
	sc.Step(`^the agent resubmits the deliverable pinned at commit "([0-9a-f]{40})" under session "([^"]*)"$`, func(symbolic, session string) error {
		sha, err := rw.real(symbolic)
		if err != nil {
			return err
		}
		if err := rw.resubmit(map[string]any{"branch": "work", "commit": sha, "session": session}); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	})
	sc.Step(`^the review returns to state "open"$`, func() error {
		got, err := rw.reviewState()
		if err != nil {
			return err
		}
		if got["state"] != "open" {
			return fmt.Errorf("state %v after resubmission", got["state"])
		}
		return nil
	})
	sc.Step(`^the review's pinned commit is "([0-9a-f]{40})"$`, func(symbolic string) error {
		got, err := rw.reviewState()
		if err != nil {
			return err
		}
		if got["commit"] != rw.sha[symbolic] {
			return fmt.Errorf("pinned commit %v, want %s", got["commit"], rw.sha[symbolic])
		}
		return nil
	})
	sc.Step(`^the review's revision is (\d+)$`, func(revision int) error {
		got, err := rw.reviewState()
		if err != nil {
			return err
		}
		if int(got["revision"].(float64)) != revision {
			return fmt.Errorf("revision %v, want %d", got["revision"], revision)
		}
		return nil
	})
	sc.Step(`^submission 1 still carries session "([^"]*)" while submission 2 carries "([^"]*)"$`, func(s1, s2 string) error {
		return rw.expectSubmissionSessions(map[int]*string{1: &s1, 2: &s2})
	})
	sc.Step(`^the review's session mirrors the latest submission, "([^"]*)"$`, func(session string) error {
		got, err := rw.reviewState()
		if err != nil {
			return err
		}
		if got["session"] != session {
			return fmt.Errorf("review session %v, want %s", got["session"], session)
		}
		return nil
	})
	sc.Step(`^a human sets the review to "changes-requested" carrying revision (\d+)$`, func(int) error {
		return rw.verdictLabeled("changes-requested", "latest")
	})
	sc.Step(`^that event carries session "([^"]*)", the session of revision (\d+)$`, func(session string, _ int) error {
		if err := iw.s.call(http.MethodGet, "/events?kind=review.changes-requested&subject="+iw.issues["SUT-1"], nil); err != nil {
			return err
		}
		var page struct {
			Events []struct {
				Payload map[string]any `json:"payload"`
			} `json:"events"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &page); err != nil {
			return err
		}
		last := page.Events[len(page.Events)-1]
		if last.Payload["session"] != session {
			return fmt.Errorf("event session %v, want %s", last.Payload["session"], session)
		}
		return nil
	})
	sc.Step(`^the agent resubmits the deliverable pinned at commit "([0-9a-f]{40})" with no session$`, func(symbolic string) error {
		sha, err := rw.real(symbolic)
		if err != nil {
			return err
		}
		if err := rw.resubmit(map[string]any{"branch": "work", "commit": sha}); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	})
	sc.Step(`^submission (\d+) carries no session$`, func(n int) error {
		return rw.expectSubmissionSessions(map[int]*string{n: nil})
	})
	sc.Step(`^the review returns to state "open" at revision (\d+)$`, func(revision int) error {
		got, err := rw.reviewState()
		if err != nil {
			return err
		}
		if got["state"] != "open" || int(got["revision"].(float64)) != revision {
			return fmt.Errorf("state %v revision %v, want open %d", got["state"], got["revision"], revision)
		}
		return nil
	})
	sc.Step(`^the review's session is absent, mirroring the latest submission$`, func() error {
		got, err := rw.reviewState()
		if err != nil {
			return err
		}
		if got["session"] != nil {
			return fmt.Errorf("review session %v, want absent", got["session"])
		}
		return nil
	})
	sc.Step(`^submissions 1 and 2 retain "([^"]*)" and "([^"]*)"$`, func(s1, s2 string) error {
		return rw.expectSubmissionSessions(map[int]*string{1: &s1, 2: &s2})
	})
	sc.Step(`^the earlier comment remains associated with revision 1$`, func() error {
		ref := cw.reviews["SUT-1"]
		if err := iw.s.call(http.MethodGet, "/comments?review="+ref.id, nil); err != nil {
			return err
		}
		var list []struct {
			ID             string `json:"id"`
			ReviewRevision int64  `json:"review_revision"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &list); err != nil {
			return err
		}
		for _, c := range list {
			if c.ID == rw.comments[len(rw.comments)-1] {
				if c.ReviewRevision != 1 {
					return fmt.Errorf("comment revision %d, want 1", c.ReviewRevision)
				}
				return nil
			}
		}
		return fmt.Errorf("earlier comment vanished")
	})
	sc.Step(`^revision 1's submission still exists and resolves to its original deliverable$`, func() error {
		content, err := rw.readDeliverable("?revision=1")
		if err != nil {
			return err
		}
		if content == "" {
			return fmt.Errorf("revision 1 deliverable is empty")
		}
		return nil
	})
	sc.Step(`^revision 2's submission exists and carries the deliverable pinned at commit "([0-9a-f]{40})"$`, func(symbolic string) error {
		got, err := rw.reviewState()
		if err != nil {
			return err
		}
		subs, _ := got["submissions"].([]any)
		for _, raw := range subs {
			sub, _ := raw.(map[string]any)
			if int(sub["revision"].(float64)) == 2 {
				if sub["commit"] != rw.sha[symbolic] {
					return fmt.Errorf("revision 2 commit %v, want %s", sub["commit"], rw.sha[symbolic])
				}
				return nil
			}
		}
		return fmt.Errorf("revision 2 submission missing")
	})

	// --- stale feedback / stale comments
	sc.Step(`^a review at revision 1 open in a reviewer's browser$`, func() error {
		if err := cw.createReview("SUT-1", "work"); err != nil {
			return err
		}
		return rw.verdictLabeled("changes-requested", "cr1")
	})
	sc.Step(`^the agent resubmits the deliverable, advancing the review to revision 2$`, func() error {
		if err := rw.resubmit(nil); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	})
	sc.Step(`^the reviewer submits an approval carrying revision 1$`, func() error {
		ref := cw.reviews["SUT-1"]
		human := iw.identities["human-brent"]
		if err := iw.s.call(http.MethodPost, "/reviews/"+ref.id+"/verdict", map[string]any{
			"verdict": "approved", "revision": 1, "actor": human}); err != nil {
			return err
		}
		rw.lastStatus = iw.s.lastResp.StatusCode
		rw.lastBody = string(iw.s.lastBody)
		return nil
	})
	sc.Step(`^the verdict is rejected$`, func() error {
		return rw.expectConflict()
	})
	sc.Step(`^the review remains unapproved$`, func() error {
		got, err := rw.reviewState()
		if err != nil {
			return err
		}
		if got["state"] == "approved" {
			return fmt.Errorf("stale approval landed")
		}
		return nil
	})
	sc.Step(`^a review at revision 1 rendered for a reviewer$`, func() error {
		if err := cw.createReview("SUT-1", "work"); err != nil {
			return err
		}
		return rw.verdictLabeled("changes-requested", "cr1")
	})
	sc.Step(`^the reviewer submits a comment carrying revision 1$`, func() error {
		ref := cw.reviews["SUT-1"]
		author := iw.identities["human-brent"]
		if err := iw.s.call(http.MethodPost, "/comments", map[string]any{
			"review": ref.id, "review_revision": 1, "author": author, "body": "stale note"}); err != nil {
			return err
		}
		rw.lastStatus = iw.s.lastResp.StatusCode
		rw.lastBody = string(iw.s.lastBody)
		return nil
	})
	sc.Step(`^the comment is rejected$`, func() error {
		return rw.expectConflict()
	})
	sc.Step(`^no comment is created$`, func() error {
		ref := cw.reviews["SUT-1"]
		if err := iw.s.call(http.MethodGet, "/comments?review="+ref.id, nil); err != nil {
			return err
		}
		if string(iw.s.lastBody) != "[]" {
			return fmt.Errorf("a comment persisted: %s", iw.s.lastBody)
		}
		return nil
	})

	// --- approval consumption fences reversal
	sc.Step(`^an approved review at revision 2$`, func() error {
		if err := cw.createReview("SUT-1", "work"); err != nil {
			return err
		}
		if err := rw.verdictLabeled("changes-requested", "cr1"); err != nil {
			return err
		}
		if err := rw.resubmit(nil); err != nil {
			return err
		}
		if err := iw.s.expectStatus(http.StatusOK); err != nil {
			return err
		}
		return rw.verdictLabeled("approved", "approval")
	})
	sc.Step(`^a subscriber consumes the approval expecting revision 2 and naming its approval's verdict event$`, func() error {
		ref := cw.reviews["SUT-1"]
		actor := iw.identities["operator"]
		key := newIdempotencyKey()
		body := map[string]any{"expected_revision": 2, "expected_verdict_event": ref.verdictEvent, "actor": actor}
		status1, body1, err := iw.s.callKeyed(http.MethodPost, "/reviews/"+ref.id+"/consume", body, key)
		if err != nil {
			return err
		}
		if status1 != http.StatusOK {
			return fmt.Errorf("consume failed: %d %s", status1, body1)
		}
		status2, body2, err := iw.s.callKeyed(http.MethodPost, "/reviews/"+ref.id+"/consume", body, key)
		if err != nil {
			return err
		}
		if status2 != http.StatusOK || body2 != body1 {
			return fmt.Errorf("same-key replay diverged: %d", status2)
		}
		return nil
	})
	sc.Step(`^the consumption is durably recorded and replaying it under the same idempotency key returns the original success$`, func() error {
		got, err := rw.reviewState()
		if err != nil {
			return err
		}
		if got["consumed"] == nil || int(got["consumed_revision"].(float64)) != 2 {
			return fmt.Errorf("consumption not recorded: %v", got)
		}
		return nil
	})
	sc.Step(`^a "review\.consumed" event records the actor, the review, and revision 2$`, func() error {
		if err := iw.s.call(http.MethodGet, "/events?kind=review.consumed&subject="+iw.issues["SUT-1"], nil); err != nil {
			return err
		}
		body := string(iw.s.lastBody)
		if !strings.Contains(body, cw.reviews["SUT-1"].id) || !strings.Contains(body, `"consumed_revision":2`) {
			return fmt.Errorf("consumed event incomplete: %s", body)
		}
		return nil
	})
	sc.Step(`^a second subscriber's distinct consumption attempt against the consumed approval is rejected with a conflict$`, func() error {
		ref := cw.reviews["SUT-1"]
		actor := iw.identities["operator"]
		status, _, err := iw.s.callKeyed(http.MethodPost, "/reviews/"+ref.id+"/consume",
			map[string]any{"expected_revision": 2, "expected_verdict_event": ref.verdictEvent, "actor": actor}, newIdempotencyKey())
		if err != nil {
			return err
		}
		if status != http.StatusConflict {
			return fmt.Errorf("second consumption got %d", status)
		}
		return nil
	})
	sc.Step(`^a later verdict on the review is rejected with no mutation$`, func() error {
		ref := cw.reviews["SUT-1"]
		human := iw.identities["human-brent"]
		if err := iw.s.call(http.MethodPost, "/reviews/"+ref.id+"/verdict", map[string]any{
			"verdict": "changes-requested", "revision": 2, "actor": human}); err != nil {
			return err
		}
		if iw.s.lastResp.StatusCode != http.StatusConflict {
			return fmt.Errorf("post-consumption verdict got %d", iw.s.lastResp.StatusCode)
		}
		got, err := rw.reviewState()
		if err != nil {
			return err
		}
		if got["state"] != "approved" {
			return fmt.Errorf("verdict mutated a consumed review")
		}
		return nil
	})
	sc.Step(`^an approved review whose verdict was reversed before consumption$`, func() error {
		if err := cw.createReview("SUT-1", "work"); err != nil {
			return err
		}
		if err := rw.verdictLabeled("approved", "approval"); err != nil {
			return err
		}
		rw.verdictEvents["pre-reversal"] = cw.reviews["SUT-1"].verdictEvent
		return rw.verdictLabeled("changes-requested", "reversal")
	})
	sc.Step(`^the subscriber attempts consumption$`, func() error {
		ref := cw.reviews["SUT-1"]
		actor := iw.identities["operator"]
		status, body, err := iw.s.callKeyed(http.MethodPost, "/reviews/"+ref.id+"/consume",
			map[string]any{"expected_revision": ref.revision, "expected_verdict_event": rw.verdictEvents["pre-reversal"], "actor": actor}, newIdempotencyKey())
		if err != nil {
			return err
		}
		rw.lastStatus = status
		rw.lastBody = body
		return nil
	})
	sc.Step(`^it fails with a conflict and consumes nothing$`, func() error {
		if err := rw.expectConflict(); err != nil {
			return err
		}
		got, err := rw.reviewState()
		if err != nil {
			return err
		}
		if got["consumed"] != nil {
			return fmt.Errorf("consumption stamped despite conflict")
		}
		return nil
	})
	sc.Step(`^a review that advanced to a newer approved revision since the subscriber's read$`, func() error {
		if err := cw.createReview("SUT-1", "work"); err != nil {
			return err
		}
		if err := rw.verdictLabeled("approved", "old-approval"); err != nil {
			return err
		}
		rw.verdictEvents["read"] = cw.reviews["SUT-1"].verdictEvent
		if err := rw.verdictLabeled("changes-requested", "cr"); err != nil {
			return err
		}
		if err := rw.resubmit(nil); err != nil {
			return err
		}
		if err := iw.s.expectStatus(http.StatusOK); err != nil {
			return err
		}
		return rw.verdictLabeled("approved", "new-approval")
	})
	sc.Step(`^the subscriber attempts consumption expecting the previous revision$`, func() error {
		ref := cw.reviews["SUT-1"]
		actor := iw.identities["operator"]
		status, body, err := iw.s.callKeyed(http.MethodPost, "/reviews/"+ref.id+"/consume",
			map[string]any{"expected_revision": 1, "expected_verdict_event": rw.verdictEvents["read"], "actor": actor}, newIdempotencyKey())
		if err != nil {
			return err
		}
		rw.lastStatus = status
		rw.lastBody = body
		return nil
	})
	sc.Step(`^it fails with a conflict and no consumption is stamped$`, func() error {
		if err := rw.expectConflict(); err != nil {
			return err
		}
		got, err := rw.reviewState()
		if err != nil {
			return err
		}
		if got["consumed"] != nil {
			return fmt.Errorf("consumption stamped despite conflict")
		}
		return nil
	})

	// --- verdict ABA fences
	sc.Step(`^a review whose verdict went changes-requested, then approved, then changes-requested again at the same revision$`, func() error {
		if err := cw.createReview("SUT-1", "work"); err != nil {
			return err
		}
		if err := rw.verdictLabeled("changes-requested", "first-cr"); err != nil {
			return err
		}
		if err := rw.verdictLabeled("approved", "mid"); err != nil {
			return err
		}
		return rw.verdictLabeled("changes-requested", "second-cr")
	})
	sc.Step(`^a delayed resubmission arrives naming the FIRST changes-requested event$`, func() error {
		return rw.resubmit(map[string]any{"expected_verdict_event": rw.verdictEvents["first-cr"], "expected_revision": 1,
			"branch": "work", "commit": mustMint(cw)})
	})
	sc.Step(`^it is rejected with a conflict — that event is no longer the review's latest verdict$`, func() error {
		return rw.expectConflict()
	})
	sc.Step(`^a review approved by event "A", reversed, then approved again by event "B" at the same revision$`, func() error {
		if err := cw.createReview("SUT-1", "work"); err != nil {
			return err
		}
		if err := rw.verdictLabeled("approved", "A"); err != nil {
			return err
		}
		rw.verdictEvents["A"] = cw.reviews["SUT-1"].verdictEvent
		if err := rw.verdictLabeled("changes-requested", "reversal"); err != nil {
			return err
		}
		if err := rw.verdictLabeled("approved", "B"); err != nil {
			return err
		}
		rw.verdictEvents["B"] = cw.reviews["SUT-1"].verdictEvent
		return nil
	})
	sc.Step(`^a delayed consumption arrives expecting verdict event "A"$`, func() error {
		ref := cw.reviews["SUT-1"]
		actor := iw.identities["operator"]
		status, body, err := iw.s.callKeyed(http.MethodPost, "/reviews/"+ref.id+"/consume",
			map[string]any{"expected_revision": ref.revision, "expected_verdict_event": rw.verdictEvents["A"], "actor": actor}, newIdempotencyKey())
		if err != nil {
			return err
		}
		rw.lastStatus = status
		rw.lastBody = body
		return nil
	})
	sc.Step(`^it is rejected with a conflict and consumes nothing — the current approved state does not excuse the superseded event$`, func() error {
		if err := rw.expectConflict(); err != nil {
			return err
		}
		got, err := rw.reviewState()
		if err != nil {
			return err
		}
		if got["consumed"] != nil {
			return fmt.Errorf("superseded event consumed the approval")
		}
		return nil
	})
	sc.Step(`^an issue whose review was approved by event "A", reversed, then approved again by event "B" at the same revision$`, func() error {
		if err := cw.createReview("SUT-1", "work"); err != nil {
			return err
		}
		if err := rw.verdictLabeled("approved", "A"); err != nil {
			return err
		}
		rw.verdictEvents["A"] = cw.reviews["SUT-1"].verdictEvent
		if err := rw.verdictLabeled("changes-requested", "reversal"); err != nil {
			return err
		}
		if err := rw.verdictLabeled("approved", "B"); err != nil {
			return err
		}
		rw.verdictEvents["B"] = cw.reviews["SUT-1"].verdictEvent
		return nil
	})
	sc.Step(`^a delayed complete transition arrives naming the review, its revision, and verdict event "A"$`, func() error {
		ref := cw.reviews["SUT-1"]
		actor := iw.identities["operator"]
		if err := iw.s.call(http.MethodPost, "/issues/"+iw.issues["SUT-1"]+"/status", map[string]any{
			"status": "complete", "review": ref.id, "review_revision": ref.revision,
			"review_verdict_event": rw.verdictEvents["A"], "actor": actor}); err != nil {
			return err
		}
		rw.lastStatus = iw.s.lastResp.StatusCode
		rw.lastBody = string(iw.s.lastBody)
		return nil
	})
	sc.Step(`^it is rejected with a conflict — the review is not close-used and the issue is not complete$`, func() error {
		if err := rw.expectConflict(); err != nil {
			return err
		}
		got, err := rw.reviewState()
		if err != nil {
			return err
		}
		if got["close_used"] != nil {
			return fmt.Errorf("review close-used despite conflict")
		}
		if err := iw.s.call(http.MethodGet, "/issues/"+iw.issues["SUT-1"], nil); err != nil {
			return err
		}
		if strings.Contains(string(iw.s.lastBody), `"complete"`) {
			return fmt.Errorf("issue completed despite conflict")
		}
		return nil
	})

	// --- stale resubmission / resubmission requires changes-requested
	sc.Step(`^a review at revision 3 in state "changes-requested"$`, func() error {
		if err := cw.createReview("SUT-1", "work"); err != nil {
			return err
		}
		if err := rw.verdictLabeled("changes-requested", "cr1"); err != nil {
			return err
		}
		rw.verdictEvents["cr1"] = cw.reviews["SUT-1"].verdictEvent
		if err := rw.resubmit(nil); err != nil {
			return err
		}
		if err := rw.verdictLabeled("changes-requested", "cr2"); err != nil {
			return err
		}
		rw.verdictEvents["cr2"] = cw.reviews["SUT-1"].verdictEvent
		if err := rw.resubmit(nil); err != nil {
			return err
		}
		return rw.verdictLabeled("changes-requested", "cr3")
	})
	sc.Step(`^a delayed resubmission arrives expecting revision 2, naming the changes-requested event it answers$`, func() error {
		if err := rw.snapshot(); err != nil {
			return err
		}
		return rw.resubmit(map[string]any{"expected_revision": 2, "expected_verdict_event": rw.verdictEvents["cr2"],
			"branch": "work", "commit": mustMint(cw)})
	})
	sc.Step(`^it is rejected with a conflict and no mutation$`, func() error {
		if err := rw.expectConflict(); err != nil {
			return err
		}
		return rw.unchanged()
	})
	sc.Step(`^the agent resubmits the deliverable$`, func() error {
		if err := rw.snapshot(); err != nil {
			return err
		}
		return rw.resubmit(nil)
	})
	sc.Step(`^the resubmission is rejected with a conflict$`, func() error {
		return rw.expectConflict()
	})
	sc.Step(`^no submission, state change, or event results$`, func() error {
		return rw.unchanged()
	})
}

// expectSubmissionSessions asserts the session of numbered submissions.
func (rw *crWorld) expectSubmissionSessions(want map[int]*string) error {
	got, err := rw.reviewState()
	if err != nil {
		return err
	}
	subs, _ := got["submissions"].([]any)
	for n, expected := range want {
		found := false
		for _, raw := range subs {
			sub, _ := raw.(map[string]any)
			if int(sub["revision"].(float64)) != n {
				continue
			}
			found = true
			if expected == nil {
				if sub["session"] != nil {
					return fmt.Errorf("submission %d carries session %v, want none", n, sub["session"])
				}
			} else if sub["session"] != *expected {
				return fmt.Errorf("submission %d session %v, want %s", n, sub["session"], *expected)
			}
		}
		if !found {
			return fmt.Errorf("submission %d missing", n)
		}
	}
	return nil
}

func mustMint(cw *closeWorld) string {
	sha, err := cw.mintCommit()
	if err != nil {
		panic(err)
	}
	return sha
}
