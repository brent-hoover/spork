package acceptance_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/cucumber/godog"
)

// closeWorld drives the review-gated close lifecycle: a git-backed
// project, reviews created and approved through the API, and closes
// naming review, revision, and verdict event.
type closeWorld struct {
	iw         *issueWorld
	repoPath   string
	commits    []string // feature commits, in order minted
	reviews    map[string]reviewRef
	tempDirs   []string
	lastClosed string // issue name of the most recent close attempt
}

type reviewRef struct {
	id           string
	revision     int64
	verdictEvent string
	commit       string
}

func (cw *closeWorld) reset() {
	cw.cleanup()
	cw.repoPath = ""
	cw.commits = nil
	cw.reviews = map[string]reviewRef{}
}

// cleanup removes every repository this world minted, including
// partially initialized ones left by setup failures.
func (cw *closeWorld) cleanup() {
	for _, dir := range cw.tempDirs {
		_ = os.RemoveAll(dir)
	}
	cw.tempDirs = nil
}

// ensureRepo builds a throwaway git repository and stamps it onto the
// scenario's project as repo_path.
func (cw *closeWorld) ensureRepo() error {
	if cw.repoPath != "" {
		return nil
	}
	dir, err := os.MkdirTemp("", "sutra-close-*")
	if err != nil {
		return err
	}
	cw.tempDirs = append(cw.tempDirs, dir) // removed by cleanup even if setup fails below
	run := func(args ...string) error {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %v: %v — %s", args, err, out)
		}
		return nil
	}
	if err := run("init", "-b", "main"); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base"), 0o644); err != nil {
		return err
	}
	if err := run("add", "."); err != nil {
		return err
	}
	if err := run("commit", "-m", "base"); err != nil {
		return err
	}
	if err := run("checkout", "-b", "work"); err != nil {
		return err
	}
	cw.repoPath = dir
	return nil
}

// mintCommit adds one commit on the work branch and returns its sha.
func (cw *closeWorld) mintCommit() (string, error) {
	if err := cw.ensureRepo(); err != nil {
		return "", err
	}
	name := fmt.Sprintf("c%d.txt", len(cw.commits)+1)
	if err := os.WriteFile(filepath.Join(cw.repoPath, name), []byte(name), 0o644); err != nil {
		return "", err
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-m", name}} {
		cmd := exec.Command("git", append([]string{"-C", cw.repoPath}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("git %v: %v — %s", args, err, out)
		}
	}
	out, err := exec.Command("git", "-C", cw.repoPath, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(string(out))
	cw.commits = append(cw.commits, sha)
	return sha, nil
}

// ensureGitProject makes the scenario's project repo-anchored before
// any issue exists under it.
func (cw *closeWorld) ensureGitProject() error {
	if cw.iw.project != "" {
		return nil
	}
	if err := cw.ensureRepo(); err != nil {
		return err
	}
	actor, err := cw.iw.identity("operator")
	if err != nil {
		return err
	}
	if err := cw.iw.s.call(http.MethodPost, "/projects", map[string]string{
		"key": "SUT", "name": "Sutra", "actor": actor, "repo_path": cw.repoPath,
	}); err != nil {
		return err
	}
	if err := cw.iw.s.expectStatus(http.StatusCreated); err != nil {
		return err
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(cw.iw.s.lastBody, &created); err != nil {
		return err
	}
	cw.iw.project = created.ID
	return nil
}

// createReview submits a code review for the named issue on a fresh
// commit and records its ref under the issue name.
func (cw *closeWorld) createReview(issueName, branch string) error {
	if err := cw.ensureGitProject(); err != nil {
		return err
	}
	issueID, err := cw.iw.ensureIssue(issueName)
	if err != nil {
		return err
	}
	sha, err := cw.mintCommit()
	if err != nil {
		return err
	}
	author := cw.iw.identities["operator"]
	if err := cw.iw.s.call(http.MethodPost, "/reviews", map[string]string{
		"issue": issueID, "author": author, "branch": branch, "commit": sha,
	}); err != nil {
		return err
	}
	if err := cw.iw.s.expectStatus(http.StatusCreated); err != nil {
		return err
	}
	var created struct {
		ID       string `json:"id"`
		Revision int64  `json:"revision"`
	}
	if err := json.Unmarshal(cw.iw.s.lastBody, &created); err != nil {
		return err
	}
	cw.reviews[issueName] = reviewRef{id: created.ID, revision: created.Revision, commit: sha}
	return nil
}

// verdict applies a verdict at the review's current revision and
// refreshes the stored ref.
func (cw *closeWorld) verdict(issueName, verdict string) error {
	ref := cw.reviews[issueName]
	human, err := cw.iw.identity("human-brent")
	if err != nil {
		return err
	}
	if err := cw.iw.s.call(http.MethodPost, "/reviews/"+ref.id+"/verdict", map[string]any{
		"verdict": verdict, "revision": ref.revision, "actor": human,
	}); err != nil {
		return err
	}
	if err := cw.iw.s.expectStatus(http.StatusOK); err != nil {
		return err
	}
	var updated struct {
		Revision           int64  `json:"revision"`
		LatestVerdictEvent string `json:"latest_verdict_event"`
	}
	if err := json.Unmarshal(cw.iw.s.lastBody, &updated); err != nil {
		return err
	}
	ref.revision = updated.Revision
	ref.verdictEvent = updated.LatestVerdictEvent
	cw.reviews[issueName] = ref
	return nil
}

// approvedReview builds review→approved for an issue in one call.
func (cw *closeWorld) approvedReview(issueName, branch string) error {
	if err := cw.createReview(issueName, branch); err != nil {
		return err
	}
	return cw.verdict(issueName, "approved")
}

// close attempts the complete transition naming the stored ref, with
// overridable revision/event for the fence scenarios.
func (cw *closeWorld) close(issueName string, revision int64, verdictEvent string) error {
	cw.lastClosed = issueName
	ref := cw.reviews[issueName]
	if revision == 0 {
		revision = ref.revision
	}
	if verdictEvent == "" {
		verdictEvent = ref.verdictEvent
	}
	issueID := cw.iw.issues[issueName]
	actor := cw.iw.identities["operator"]
	return cw.iw.s.call(http.MethodPost, "/issues/"+issueID+"/status", map[string]any{
		"status": "complete", "review": ref.id, "review_revision": revision,
		"review_verdict_event": verdictEvent, "actor": actor,
	})
}

func (cw *closeWorld) readReview(issueName string) (map[string]any, error) {
	ref := cw.reviews[issueName]
	if err := cw.iw.s.call(http.MethodGet, "/reviews/"+ref.id, nil); err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(cw.iw.s.lastBody, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func registerCloseSteps(sc *godog.ScenarioContext, iw *issueWorld) *closeWorld {
	cw := &closeWorld{iw: iw}
	sc.Before(func(ctx context.Context, _ *godog.Scenario) (context.Context, error) {
		cw.reset()
		return ctx, nil
	})
	sc.After(func(ctx context.Context, _ *godog.Scenario, _ error) (context.Context, error) {
		cw.cleanup()
		return ctx, nil
	})

	// --- approved review allows close
	sc.Step(`^issue (SUT-\d+) has a review of branch "([^"]*)" pinned at an immutable commit in state "approved"$`, func(name, branch string) error {
		return cw.approvedReview(name, branch)
	})
	sc.Step(`^(SUT-\d+) is transitioned to "complete" naming that review at its approved revision with its approval's verdict event$`, func(name string) error {
		return cw.close(name, 0, "")
	})
	// closedAndPersisted asserts BOTH the response and the stored
	// state: a close that returns 200 but does not persist complete
	// must fail the scenario.
	closedAndPersisted := func() error {
		if err := iw.s.expectStatus(http.StatusOK); err != nil {
			return err
		}
		if err := iw.readIssue(cw.lastClosed); err != nil {
			return err
		}
		if iw.lastIssue.Status != "complete" {
			return fmt.Errorf("close returned 200 but %s persisted as %q", cw.lastClosed, iw.lastIssue.Status)
		}
		return nil
	}
	sc.Step(`^the transition succeeds$`, closedAndPersisted)
	sc.Step(`^the transition succeeds — verdict consumption is a fence, not a spend$`, closedAndPersisted)
	sc.Step(`^the recorded approval names the pinned commit$`, func() error {
		out, err := cw.readReview("SUT-1")
		if err != nil {
			return err
		}
		if out["commit"] != cw.reviews["SUT-1"].commit {
			return fmt.Errorf("approval covers %v, pinned %s", out["commit"], cw.reviews["SUT-1"].commit)
		}
		return nil
	})
	sc.Step(`^the review is stamped close-used and its verdict frozen in the same transaction$`, func() error {
		out, err := cw.readReview("SUT-1")
		if err != nil {
			return err
		}
		if out["close_used"] == nil || out["consumed"] == nil {
			return fmt.Errorf("close did not stamp close_used+consumed: %v", out)
		}
		return nil
	})
	sc.Step(`^a later verdict on that review is rejected with no mutation$`, func() error {
		before, err := cw.readReview("SUT-1")
		if err != nil {
			return err
		}
		ref := cw.reviews["SUT-1"]
		human := iw.identities["human-brent"]
		if err := iw.s.call(http.MethodPost, "/reviews/"+ref.id+"/verdict", map[string]any{
			"verdict": "changes-requested", "revision": ref.revision, "actor": human,
		}); err != nil {
			return err
		}
		if err := iw.s.expectStatus(http.StatusConflict); err != nil {
			return err
		}
		after, err := cw.readReview("SUT-1")
		if err != nil {
			return err
		}
		if after["state"] != before["state"] || after["latest_verdict_event"] != before["latest_verdict_event"] {
			return fmt.Errorf("frozen verdict mutated: %v -> %v", before, after)
		}
		return nil
	})

	// --- a stale revision cannot authorize a close
	sc.Step(`^issue (SUT-\d+) has a review approved at revision 2 after an earlier revision was reviewed$`, func(name string) error {
		if err := cw.createReview(name, "rework"); err != nil {
			return err
		}
		if err := cw.verdict(name, "changes-requested"); err != nil {
			return err
		}
		ref := cw.reviews[name]
		sha, err := cw.mintCommit()
		if err != nil {
			return err
		}
		author := iw.identities["operator"]
		if err := iw.s.call(http.MethodPost, "/reviews/"+ref.id+"/resubmit", map[string]any{
			"author": author, "expected_revision": ref.revision, "expected_verdict_event": ref.verdictEvent,
			"branch": "rework", "commit": sha,
		}); err != nil {
			return err
		}
		if err := iw.s.expectStatus(http.StatusOK); err != nil {
			return err
		}
		var updated struct {
			Revision int64 `json:"revision"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &updated); err != nil {
			return err
		}
		ref.revision = updated.Revision
		cw.reviews[name] = ref
		return cw.verdict(name, "approved")
	})
	sc.Step(`^(SUT-\d+) is transitioned to "complete" naming that review at revision 1 with its approval's verdict event$`, func(name string) error {
		return cw.close(name, 1, "")
	})
	sc.Step(`^the transition is rejected with a conflict$`, func() error {
		return iw.s.expectStatus(http.StatusConflict)
	})
	sc.Step(`^the approval is not consumed and (SUT-\d+) is not complete$`, func(name string) error {
		out, err := cw.readReview(name)
		if err != nil {
			return err
		}
		if out["consumed"] != nil {
			return fmt.Errorf("stale close consumed the approval: %v", out)
		}
		if err := iw.readIssue(name); err != nil {
			return err
		}
		if iw.lastIssue.Status == "complete" {
			return fmt.Errorf("stale close completed the issue")
		}
		return nil
	})

	// --- a reversed approval cannot authorize a close
	sc.Step(`^issue (SUT-\d+) has an approved review whose verdict is reversed to "changes-requested" before the close commits$`, func(name string) error {
		if err := cw.approvedReview(name, "reversed"); err != nil {
			return err
		}
		approvedRef := cw.reviews[name]
		if err := cw.verdict(name, "changes-requested"); err != nil {
			return err
		}
		// The close below names the APPROVED revision/event snapshot.
		reversed := cw.reviews[name]
		reversed.revision = approvedRef.revision
		reversed.verdictEvent = approvedRef.verdictEvent
		cw.reviews[name] = reversed
		return nil
	})
	sc.Step(`^(SUT-\d+) is transitioned to "complete" naming that review at its last approved revision with its latest verdict event$`, func(name string) error {
		return cw.close(name, 0, "")
	})
	sc.Step(`^the transition is rejected naming the missing approval$`, func() error {
		if err := iw.s.expectStatus(http.StatusConflict); err != nil {
			return err
		}
		var envelope struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &envelope); err != nil {
			return err
		}
		if envelope.Code != "review-not-approved" && envelope.Code != "expected-verdict-event-mismatch" && envelope.Code != "missing-approval" {
			return fmt.Errorf("expected an approval-shaped conflict, got %q", envelope.Code)
		}
		return nil
	})

	// --- merge-consumed approval still closes
	sc.Step(`^issue (SUT-\d+) has an approved review whose approval was consumed by a subscriber before merging$`, func(name string) error {
		if err := cw.approvedReview(name, "merge"); err != nil {
			return err
		}
		ref := cw.reviews[name]
		actor := iw.identities["operator"]
		if err := iw.s.call(http.MethodPost, "/reviews/"+ref.id+"/consume", map[string]any{
			"expected_revision": ref.revision, "expected_verdict_event": ref.verdictEvent, "actor": actor,
		}); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	})
	sc.Step(`^(SUT-\d+) is transitioned to "complete" naming that review at its consumed revision with its approval's verdict event$`, func(name string) error {
		return cw.close(name, 0, "")
	})

	// --- spent review cannot close a reopened issue
	sc.Step(`^issue (SUT-\d+) closed under its approved review and was later reopened$`, func(name string) error {
		if err := cw.approvedReview(name, "spent"); err != nil {
			return err
		}
		if err := cw.close(name, 0, ""); err != nil {
			return err
		}
		if err := iw.s.expectStatus(http.StatusOK); err != nil {
			return err
		}
		if err := iw.transition(name, "open"); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	})
	sc.Step(`^(SUT-\d+) is transitioned to "complete" naming the same review at its approved revision with its approval's verdict event$`, func(name string) error {
		return cw.close(name, 0, "")
	})
	sc.Step(`^the transition is rejected — the review is already close-used$`, func() error {
		if err := iw.s.expectStatus(http.StatusConflict); err != nil {
			return err
		}
		return iw.s.expectErrorCode("review-close-used")
	})
	sc.Step(`^a fresh review of (SUT-\d+) is approved$`, func(name string) error {
		return cw.approvedReview(name, "fresh")
	})
	sc.Step(`^(SUT-\d+) is transitioned to "complete" naming the fresh review at its approved revision with its approval's verdict event$`, func(name string) error {
		return cw.close(name, 0, "")
	})

	// --- naming the wrong review cannot close
	sc.Step(`^issue (SUT-\d+) has a changes-requested review and issue (SUT-\d+) has an approved one$`, func(a, b string) error {
		if err := cw.createReview(a, "cr"); err != nil {
			return err
		}
		if err := cw.verdict(a, "changes-requested"); err != nil {
			return err
		}
		return cw.approvedReview(b, "ok")
	})
	sc.Step(`^(SUT-\d+) is transitioned to "complete" naming its changes-requested review at its current revision with its latest verdict event$`, func(name string) error {
		return cw.close(name, 0, "")
	})
	sc.Step(`^the transition is rejected$`, func() error {
		if iw.s.lastResp.StatusCode < 400 {
			return fmt.Errorf("expected rejection, got %d", iw.s.lastResp.StatusCode)
		}
		return nil
	})
	sc.Step(`^(SUT-\d+) is transitioned to "complete" naming (SUT-\d+)'s review at its approved revision with its approval's verdict event$`, func(a, b string) error {
		cw.lastClosed = a
		other := cw.reviews[b]
		issueID := iw.issues[a]
		actor := iw.identities["operator"]
		return iw.s.call(http.MethodPost, "/issues/"+issueID+"/status", map[string]any{
			"status": "complete", "review": other.id, "review_revision": other.revision,
			"review_verdict_event": other.verdictEvent, "actor": actor,
		})
	})
	sc.Step(`^the transition is rejected — the review belongs to another issue$`, func() error {
		if err := iw.s.expectStatus(http.StatusConflict); err != nil {
			return err
		}
		return iw.s.expectErrorCode("missing-approval")
	})

	// --- no approval no close
	sc.Step(`^issue (SUT-\d+) has no review in state "approved"$`, func(name string) error {
		if err := cw.ensureGitProject(); err != nil {
			return err
		}
		_, err := iw.ensureIssue(name)
		return err
	})
	sc.Step(`^a transition of (SUT-\d+) to "complete" is attempted$`, func(name string) error {
		issueID := iw.issues[name]
		actor := iw.identities["operator"]
		return iw.s.call(http.MethodPost, "/issues/"+issueID+"/status", map[string]any{
			"status": "complete", "review": "00000000-0000-7000-8000-000000000001",
			"review_revision": 1, "review_verdict_event": "00000000-0000-7000-8000-000000000002", "actor": actor,
		})
	})
	sc.Step(`^it is rejected$`, func() error {
		return iw.s.expectStatus(http.StatusConflict)
	})
	sc.Step(`^the error names the missing approval$`, func() error {
		return iw.s.expectErrorCode("missing-approval")
	})

	// --- complete can reopen (REQ-status-workflow)
	sc.Step(`^issue (SUT-\d+) is "complete" with an approved review$`, func(name string) error {
		if err := cw.approvedReview(name, "done"); err != nil {
			return err
		}
		if err := cw.close(name, 0, ""); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	})
	sc.Step(`^it is reopened$`, func() error {
		if err := iw.transition("SUT-1", "open"); err != nil {
			return err
		}
		if err := iw.s.expectStatus(http.StatusOK); err != nil {
			return err
		}
		// Refresh stored state so the next assertion reads the
		// persisted status, not the creation-time snapshot.
		return iw.readIssue("SUT-1")
	})
	sc.Step(`^the reopening is recorded$`, func() error {
		return iw.expectEvent("issue.status-changed", "SUT-1", "operator")
	})
	sc.Step(`^the approved review remains in history$`, func() error {
		out, err := cw.readReview("SUT-1")
		if err != nil {
			return err
		}
		if out["state"] != "approved" || out["close_used"] == nil {
			return fmt.Errorf("history lost: %v", out)
		}
		return nil
	})

	// --- conditional transitions guard racing writers
	sc.Step(`^issue (SUT-\d+) has status "in-progress"$`, func(name string) error {
		if _, err := iw.ensureIssue(name); err != nil {
			return err
		}
		if err := iw.transition(name, "in-progress"); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	})
	sc.Step(`^a transition to "deferred" with expected status "open" is attempted$`, func() error {
		issueID := iw.issues["SUT-1"]
		actor := iw.identities["operator"]
		return iw.s.call(http.MethodPost, "/issues/"+issueID+"/status", map[string]any{
			"status": "deferred", "expected_status": "open", "actor": actor,
		})
	})
	// Shared by the conditional-transition scenario and the
	// relation-conflict outline: assert the conflict status here; each
	// scenario's next step pins its specific code.
	sc.Step(`^it is rejected with a conflict$`, func() error {
		return iw.s.expectStatus(http.StatusConflict)
	})
	sc.Step(`^(SUT-\d+) still has status "in-progress"$`, func(name string) error {
		if err := iw.s.expectErrorCode("expected-status-mismatch"); err != nil {
			return err
		}
		if err := iw.readIssue(name); err != nil {
			return err
		}
		if iw.lastIssue.Status != "in-progress" {
			return fmt.Errorf("status %q", iw.lastIssue.Status)
		}
		return nil
	})
	return cw
}
