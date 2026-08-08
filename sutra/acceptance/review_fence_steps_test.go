package acceptance_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/cucumber/godog"
)

// fenceWorld drives the expected-base / expected-default-head fences
// and the unresolvable-repository outline of REQ-code-review.
type fenceWorld struct {
	iw *issueWorld
	cw *closeWorld
	rw *crWorld

	operation   string // "submission" | "resubmission" | "create" | "resubmit"
	fenceField  string // "expected base" | "expected default head"
	fenceValue  string // the mismatching sha the caller will send
	failure     string
	badCommit   string
	preReviews  int
	preEvents   int
	hadReview   bool
	lastStatus  int
	lastBody    string
	issueForOp  string
	oldMainHead string
}

func (fw *fenceWorld) reset() {
	*fw = fenceWorld{iw: fw.iw, cw: fw.cw, rw: fw.rw}
}

// countState snapshots review and event counts for no-mutation checks.
func (fw *fenceWorld) countState() error {
	iw := fw.iw
	if err := iw.s.call(http.MethodGet, "/reviews", nil); err != nil {
		return err
	}
	var reviews []any
	if err := json.Unmarshal(iw.s.lastBody, &reviews); err != nil {
		return err
	}
	fw.preReviews = len(reviews)
	if err := iw.s.call(http.MethodGet, "/events?limit=1000", nil); err != nil {
		return err
	}
	var page struct {
		Events []any `json:"events"`
	}
	if err := json.Unmarshal(iw.s.lastBody, &page); err != nil {
		return err
	}
	fw.preEvents = len(page.Events)
	return nil
}

func (fw *fenceWorld) unchangedCounts() error {
	pre, preEv := fw.preReviews, fw.preEvents
	if err := fw.countState(); err != nil {
		return err
	}
	if fw.preReviews != pre {
		return fmt.Errorf("review count changed: %d -> %d", pre, fw.preReviews)
	}
	if fw.preEvents != preEv {
		return fmt.Errorf("event count changed: %d -> %d", preEv, fw.preEvents)
	}
	fw.preReviews, fw.preEvents = pre, preEv
	return nil
}

// wrongShaFor returns a real sha guaranteed to mismatch the named
// fence: the work-branch head mismatches both the merge base (main's
// commit) and the default head.
func (fw *fenceWorld) wrongShaFor() (string, error) {
	return fw.cw.mintCommit()
}

// submitCode posts a review creation with the recorded fence.
func (fw *fenceWorld) submitCode(extras map[string]any) error {
	iw := fw.iw
	issueID, err := iw.ensureIssue("SUT-1")
	if err != nil {
		return err
	}
	author, err := iw.identity("claude")
	if err != nil {
		return err
	}
	sha, err := fw.cw.mintCommit()
	if err != nil {
		return err
	}
	body := map[string]any{"issue": issueID, "author": author, "branch": "work", "commit": sha}
	for k, v := range extras {
		body[k] = v
	}
	if err := iw.s.call(http.MethodPost, "/reviews", body); err != nil {
		return err
	}
	fw.lastStatus = iw.s.lastResp.StatusCode
	fw.lastBody = string(iw.s.lastBody)
	return nil
}

func registerReviewFenceSteps(sc *godog.ScenarioContext, cw *closeWorld, rw *crWorld) {
	iw := cw.iw
	fw := &fenceWorld{iw: iw, cw: cw, rw: rw}
	sc.Before(func(ctx context.Context, _ *godog.Scenario) (context.Context, error) {
		fw.reset()
		return ctx, nil
	})

	// --- both fences guard creation and resubmission (outline)
	sc.Step(`^a caller performs a (submission|resubmission) expecting (expected base|expected default head) "D1"$`, func(operation, field string) error {
		fw.operation = operation
		fw.fenceField = field
		if err := cw.ensureGitProject(); err != nil {
			return err
		}
		if operation == "resubmission" {
			if err := cw.createReview("SUT-1", "work"); err != nil {
				return err
			}
			if err := cw.verdict("SUT-1", "changes-requested"); err != nil {
				return err
			}
			fw.hadReview = true
			if err := rw.snapshot(); err != nil {
				return err
			}
		}
		if _, err := iw.ensureIssue("SUT-1"); err != nil {
			return err
		}
		if _, err := iw.identity("claude"); err != nil {
			return err
		}
		wrong, err := fw.wrongShaFor()
		if err != nil {
			return err
		}
		fw.fenceValue = wrong
		return fw.countState()
	})
	sc.Step(`^sutra resolves the actual (expected base|expected default head) as "D2"$`, func(string) error {
		return nil // the repo's real state IS D2; the fence value above mismatches it
	})
	sc.Step(`^the request is processed$`, func() error {
		fenceKey := "expected_base_commit"
		if fw.fenceField == "expected default head" {
			fenceKey = "expected_default_head"
		}
		if fw.operation == "submission" {
			return fw.submitCode(map[string]any{fenceKey: fw.fenceValue})
		}
		if err := rw.resubmit(map[string]any{fenceKey: fw.fenceValue}); err != nil {
			return err
		}
		fw.lastStatus = rw.lastStatus
		fw.lastBody = rw.lastBody
		return nil
	})
	sc.Step(`^it is rejected with a conflict — nothing is created or mutated, no new submission or event exists, and for a resubmission the existing review, revision, and verdict remain unchanged$`, func() error {
		if fw.lastStatus != http.StatusConflict {
			return fmt.Errorf("expected 409, got %d: %s", fw.lastStatus, fw.lastBody)
		}
		if err := fw.unchangedCounts(); err != nil {
			return err
		}
		if fw.hadReview {
			return rw.unchanged()
		}
		return nil
	})

	// --- stale expected base rejects submission before the review exists
	sc.Step(`^a caller submits a code deliverable expecting base "D1"$`, func() error {
		if err := cw.ensureGitProject(); err != nil {
			return err
		}
		if _, err := iw.ensureIssue("SUT-1"); err != nil {
			return err
		}
		if _, err := iw.identity("claude"); err != nil {
			return err
		}
		wrong, err := fw.wrongShaFor()
		if err != nil {
			return err
		}
		fw.fenceValue = wrong
		return fw.countState()
	})
	sc.Step(`^sutra resolves the merge base as "D2"$`, func() error {
		return nil
	})
	sc.Step(`^the submission is processed$`, func() error {
		fenceKey := "expected_base_commit"
		if fw.fenceField == "expected default head" {
			fenceKey = "expected_default_head"
		}
		return fw.submitCode(map[string]any{fenceKey: fw.fenceValue})
	})
	sc.Step(`^it is rejected with a conflict and no review, submission, or event exists$`, func() error {
		if fw.lastStatus != http.StatusConflict {
			return fmt.Errorf("expected 409, got %d: %s", fw.lastStatus, fw.lastBody)
		}
		return fw.unchangedCounts()
	})

	// --- a moved default head rejects submission even when the merge base is unchanged
	sc.Step(`^a caller submits a code deliverable expecting default head "D1"$`, func() error {
		fw.fenceField = "expected default head"
		if err := cw.ensureGitProject(); err != nil {
			return err
		}
		if _, err := iw.ensureIssue("SUT-1"); err != nil {
			return err
		}
		if _, err := iw.identity("claude"); err != nil {
			return err
		}
		head, err := fw.rw.git("rev-parse", "main")
		if err != nil {
			return err
		}
		fw.oldMainHead = head
		fw.fenceValue = head // D1: the head as the caller observed it
		return fw.countState()
	})
	sc.Step(`^the default branch fast-forwarded to "D2" while the merge base remains "D1"$`, func() error {
		// Advance main; the work branch still forked from the old
		// head, so the merge base stays D1 while the head moves.
		if _, err := fw.rw.git("checkout", "main"); err != nil {
			return err
		}
		if _, err := fw.rw.git("commit", "--allow-empty", "-m", "fast-forward"); err != nil {
			return err
		}
		_, err := fw.rw.git("checkout", "work")
		return err
	})
	sc.Step(`^it is rejected with a conflict and nothing is created — the fence is the head, not the merge base$`, func() error {
		if fw.lastStatus != http.StatusConflict {
			return fmt.Errorf("expected 409, got %d: %s", fw.lastStatus, fw.lastBody)
		}
		if !strings.Contains(fw.lastBody, "expected-default-head-mismatch") {
			return fmt.Errorf("wrong fence tripped: %s", fw.lastBody)
		}
		return fw.unchangedCounts()
	})

	// --- resubmission enforces the same base and head fences
	sc.Step(`^a changes-requested review whose caller resubmits a code deliverable expecting base "D1"$`, func() error {
		if err := cw.createReview("SUT-1", "work"); err != nil {
			return err
		}
		if err := cw.verdict("SUT-1", "changes-requested"); err != nil {
			return err
		}
		if err := rw.snapshot(); err != nil {
			return err
		}
		wrong, err := fw.wrongShaFor()
		if err != nil {
			return err
		}
		fw.fenceValue = wrong
		return fw.countState()
	})
	sc.Step(`^sutra resolves the resubmission's merge base as "D0"$`, func() error {
		return nil
	})
	sc.Step(`^the resubmission is processed$`, func() error {
		fenceKey := "expected_base_commit"
		if fw.fenceField == "expected default head" {
			fenceKey = "expected_default_head"
		}
		if err := rw.resubmit(map[string]any{fenceKey: fw.fenceValue}); err != nil {
			return err
		}
		fw.lastStatus = rw.lastStatus
		fw.lastBody = rw.lastBody
		return nil
	})
	sc.Step(`^the caller resubmits expecting default head "D1"$`, func() error {
		fw.fenceField = "expected default head"
		head, err := fw.rw.git("rev-parse", "main")
		if err != nil {
			return err
		}
		fw.fenceValue = head
		return nil
	})
	sc.Step(`^the default branch now stands at "D2"$`, func() error {
		if _, err := fw.rw.git("checkout", "main"); err != nil {
			return err
		}
		if _, err := fw.rw.git("commit", "--allow-empty", "-m", "advance"); err != nil {
			return err
		}
		_, err := fw.rw.git("checkout", "work")
		return err
	})

	// --- a deliverable is pinned only by a full object id (outline)
	sc.Step(`^a git-backed project and issue (SUT-\d+)$`, func(issueName string) error {
		if err := cw.ensureGitProject(); err != nil {
			return err
		}
		_, err := iw.ensureIssue(issueName)
		return err
	})
	sc.Step(`^an agent creates a review pinned at (.+)$`, func(form string) error {
		var commit string
		switch strings.TrimSpace(form) {
		case "an unknown object id of the other width":
			// 64 hex digits — git's sha-256 object format, well formed
			// and held by no repository here.
			commit = strings.Repeat("deadbeef", 8)
		case "a real object id cut to twelve digits":
			sha, err := fw.rw.real("pin")
			if err != nil {
				return err
			}
			commit = sha[:12]
		case "a full-width string that is not hex":
			commit = strings.Repeat("z", 40)
		default:
			return fmt.Errorf("unknown commit form %q", form)
		}
		issueID, err := iw.ensureIssue("SUT-1")
		if err != nil {
			return err
		}
		author, err := iw.identity("claude")
		if err != nil {
			return err
		}
		return iw.s.call(http.MethodPost, "/reviews", map[string]any{
			"issue": issueID, "author": author, "branch": "work", "commit": commit})
	})
	sc.Step(`^the submission is refused as a conflict$`, func() error {
		return iw.s.expectStatus(http.StatusConflict)
	})
	sc.Step(`^the submission is refused as a bad request, naming the form$`, func() error {
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
		if !strings.Contains(envelope.Message, "canonical full object id") {
			return fmt.Errorf("expected the refusal to name the id's form, got %q", envelope.Message)
		}
		return nil
	})

	// --- unresolvable repository rejects submission (outline)
	sc.Step(`^a project with (.+)$`, func(failure string) error {
		fw.failure = strings.TrimSpace(failure)
		actor, err := iw.identity("operator")
		if err != nil {
			return err
		}
		switch fw.failure {
		case "an unset repo_path":
			if err := iw.s.call(http.MethodPost, "/projects",
				map[string]string{"key": "SUT", "name": "Sutra", "actor": actor}); err != nil {
				return err
			}
		case "inaccessible repository path":
			if err := iw.s.call(http.MethodPost, "/projects",
				map[string]string{"key": "SUT", "name": "Sutra", "actor": actor, "repo_path": "/nonexistent/sutra-fence-repo"}); err != nil {
				return err
			}
		case "unknown commit":
			if err := cw.ensureGitProject(); err != nil {
				return err
			}
			fw.badCommit = strings.Repeat("deadbeef", 5)
			return nil
		case "unavailable merge base":
			if err := cw.ensureGitProject(); err != nil {
				return err
			}
			// An orphan-branch commit shares no ancestor with main.
			if _, err := fw.rw.git("checkout", "--orphan", "orphaned"); err != nil {
				return err
			}
			if _, err := fw.rw.git("commit", "--allow-empty", "-m", "orphan"); err != nil {
				return err
			}
			sha, err := fw.rw.git("rev-parse", "HEAD")
			if err != nil {
				return err
			}
			fw.badCommit = sha
			_, err = fw.rw.git("checkout", "work")
			return err
		default:
			return fmt.Errorf("unknown failure %q", fw.failure)
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
		iw.project = created.ID
		fw.badCommit = strings.Repeat("deadbeef", 5)
		return nil
	})
	sc.Step(`^an agent (create|resubmit)s a code deliverable for (SUT-\d+)$`, func(operation, issueName string) error {
		fw.operation = operation
		fw.issueForOp = issueName
		if operation == "resubmit" {
			if err := fw.prepareResubmittableReview(issueName); err != nil {
				return err
			}
			if err := rw.snapshot(); err != nil {
				return err
			}
			fw.hadReview = true
		}
		issueID, err := iw.ensureIssue(issueName)
		if err != nil {
			return err
		}
		author, err := iw.identity("claude")
		if err != nil {
			return err
		}
		if err := fw.countState(); err != nil {
			return err
		}
		if operation == "create" {
			if err := iw.s.call(http.MethodPost, "/reviews", map[string]any{
				"issue": issueID, "author": author, "branch": "work", "commit": fw.badCommit}); err != nil {
				return err
			}
			fw.lastStatus = iw.s.lastResp.StatusCode
			fw.lastBody = string(iw.s.lastBody)
			return nil
		}
		if err := rw.resubmit(map[string]any{"branch": "work", "commit": fw.badCommit}); err != nil {
			return err
		}
		fw.lastStatus = rw.lastStatus
		fw.lastBody = rw.lastBody
		return nil
	})
	sc.Step(`^the (create|resubmit) is rejected with a conflict$`, func(string) error {
		if fw.lastStatus != http.StatusConflict {
			return fmt.Errorf("expected 409, got %d: %s", fw.lastStatus, fw.lastBody)
		}
		return nil
	})
	sc.Step(`^no new submission, review, or event results$`, func() error {
		return fw.unchangedCounts()
	})
	sc.Step(`^any pre-existing review's state, revision, deliverable, submissions, and events are unchanged$`, func() error {
		if !fw.hadReview {
			return nil
		}
		return rw.unchanged()
	})
}

// prepareResubmittableReview builds a changes-requested review whose
// project may lack a usable repository: a DOC deliverable review works
// on any project, and its resubmission with a code deliverable then
// exercises the repository resolution under test.
func (fw *fenceWorld) prepareResubmittableReview(issueName string) error {
	iw := fw.iw
	cw := fw.cw
	if fw.failure == "unknown commit" || fw.failure == "unavailable merge base" {
		// The repository works; a normal code review sets the stage.
		if err := cw.createReview(issueName, "work"); err != nil {
			return err
		}
		return cw.verdict(issueName, "changes-requested")
	}
	issueID, err := iw.ensureIssue(issueName)
	if err != nil {
		return err
	}
	author, err := iw.identity("human-brent")
	if err != nil {
		return err
	}
	if err := iw.s.call(http.MethodPost, "/projects/"+iw.project+"/documents",
		map[string]string{"title": "doc deliverable", "content": "reviewable text", "author": author}); err != nil {
		return err
	}
	if err := iw.s.expectStatus(http.StatusCreated); err != nil {
		return err
	}
	var doc struct {
		Version struct {
			ID string `json:"id"`
		} `json:"version"`
	}
	if err := json.Unmarshal(iw.s.lastBody, &doc); err != nil {
		return err
	}
	if err := iw.s.call(http.MethodPost, "/reviews", map[string]string{
		"issue": issueID, "author": iw.identities["operator"], "doc_version": doc.Version.ID}); err != nil {
		return err
	}
	if err := iw.s.expectStatus(http.StatusCreated); err != nil {
		return err
	}
	var created struct {
		ID       string `json:"id"`
		Revision int64  `json:"revision"`
	}
	if err := json.Unmarshal(iw.s.lastBody, &created); err != nil {
		return err
	}
	cw.reviews[issueName] = reviewRef{id: created.ID, revision: created.Revision}
	return cw.verdict(issueName, "changes-requested")
}
