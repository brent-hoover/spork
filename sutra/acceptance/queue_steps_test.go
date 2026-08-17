package acceptance_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/cucumber/godog"
)

// queueWorld drives the work-stack scenarios.
type queueWorld struct {
	iw       *issueWorld
	lastPop  popResult
	replayed struct {
		key    string
		status int
		body   string
	}
}

type popResult struct {
	status int
	Issue  *struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"issue"`
	FeedWatermark string `json:"feed_watermark"`
}

func (qw *queueWorld) reset() {
	qw.lastPop = popResult{}
	qw.replayed.key = ""
}

// assign assigns an issue to a handle through the API.
func (qw *queueWorld) assign(issueName, handle string) error {
	iw := qw.iw
	issueID, err := iw.ensureIssue(issueName)
	if err != nil {
		return err
	}
	assignee, err := iw.identity(handle)
	if err != nil {
		return err
	}
	actor := iw.identities["operator"]
	if err := iw.s.call(http.MethodPost, "/issues/"+issueID+"/assign",
		map[string]any{"assignee": assignee, "actor": actor}); err != nil {
		return err
	}
	return iw.s.expectStatus(http.StatusOK)
}

// pop pops the handle's stack with a fresh key and decodes the result.
func (qw *queueWorld) pop(handle string) error {
	iw := qw.iw
	id, err := iw.identity(handle)
	if err != nil {
		return err
	}
	if err := iw.s.call(http.MethodPost, "/identities/"+id+"/work-stack/pop", map[string]string{}); err != nil {
		return err
	}
	qw.lastPop = popResult{status: iw.s.lastResp.StatusCode}
	return json.Unmarshal(iw.s.lastBody, &qw.lastPop)
}

func (qw *queueWorld) expectClaimed(issueName string) error {
	if qw.lastPop.status != http.StatusOK {
		return fmt.Errorf("pop status %d", qw.lastPop.status)
	}
	if qw.lastPop.Issue == nil {
		return fmt.Errorf("pop returned empty, want %s", issueName)
	}
	if qw.lastPop.Issue.ID != qw.iw.issues[issueName] {
		return fmt.Errorf("pop claimed %s, want %s (%s)", qw.lastPop.Issue.ID, issueName, qw.iw.issues[issueName])
	}
	return nil
}

func registerQueueSteps(sc *godog.ScenarioContext, cw *closeWorld) {
	iw := cw.iw
	qw := &queueWorld{iw: iw}
	sc.Before(func(ctx context.Context, _ *godog.Scenario) (context.Context, error) {
		qw.reset()
		return ctx, nil
	})

	// --- pop claims the issue
	sc.Step(`^issue (SUT-\d+) is assigned to agent "([^"]*)" with status "open"$`, func(name, handle string) error {
		return qw.assign(name, handle)
	})
	sc.Step(`^issue (SUT-\d+) is assigned to "([^"]*)" with status "open"$`, func(name, handle string) error {
		return qw.assign(name, handle)
	})
	sc.Step(`^"([^"]*)" pops its work stack$`, func(handle string) error {
		return qw.pop(handle)
	})
	sc.Step(`^it receives (SUT-\d+)$`, func(name string) error {
		return qw.expectClaimed(name)
	})
	sc.Step(`^(SUT-\d+) has status "in-progress"$`, func(name string) error {
		if err := iw.readIssue(name); err != nil {
			return err
		}
		if iw.lastIssue.Status != "in-progress" {
			return fmt.Errorf("%s is %q", name, iw.lastIssue.Status)
		}
		return nil
	})
	sc.Step(`^an "issue.status-changed" event with subject (SUT-\d+), actor "([^"]*)", and a timestamp is recorded$`, func(name, handle string) error {
		return iw.expectEvent("issue.status-changed", name, handle)
	})

	// --- concurrent pops never collide
	sc.Step(`^issues (SUT-\d+) and (SUT-\d+) are assigned to agent "([^"]*)" with status "open"$`, func(a, b, handle string) error {
		if err := qw.assign(a, handle); err != nil {
			return err
		}
		return qw.assign(b, handle)
	})
	sc.Step(`^two instances of "([^"]*)" pop concurrently$`, func(handle string) error {
		id, err := iw.identity(handle)
		if err != nil {
			return err
		}
		results := make(chan string, 2)
		errs := make(chan error, 2)
		var wg sync.WaitGroup
		for range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				// Each instance uses its own client call to avoid shared
				// state races in the test world.
				req, err := http.NewRequest(http.MethodPost, iw.s.server.URL+"/identities/"+id+"/work-stack/pop", nil)
				if err != nil {
					errs <- err
					return
				}
				req.Header.Set("Idempotency-Key", newIdempotencyKey())
				resp, err := iw.s.server.Client().Do(req)
				if err != nil {
					errs <- err
					return
				}
				defer func() { _ = resp.Body.Close() }()
				var out struct {
					Issue *struct {
						ID string `json:"id"`
					} `json:"issue"`
				}
				if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
					errs <- err
					return
				}
				if out.Issue == nil {
					results <- ""
				} else {
					results <- out.Issue.ID
				}
			}()
		}
		wg.Wait()
		close(results)
		close(errs)
		for err := range errs {
			return err
		}
		var claimed []string
		for r := range results {
			claimed = append(claimed, r)
		}
		want := map[string]bool{iw.issues["SUT-1"]: true, iw.issues["SUT-2"]: true}
		if len(claimed) != 2 || claimed[0] == claimed[1] || !want[claimed[0]] || !want[claimed[1]] {
			return fmt.Errorf("concurrent pops collided or missed: %v", claimed)
		}
		return nil
	})
	sc.Step(`^one instance receives (SUT-\d+) and the other receives (SUT-\d+)$`, func(_, _ string) error {
		return nil // asserted in the concurrent step
	})

	// --- oldest issue comes first
	sc.Step(`^issue (SUT-\d+) was assigned to "([^"]*)" before issue (SUT-\d+)$`, func(a, handle, b string) error {
		if err := qw.assign(a, handle); err != nil {
			return err
		}
		return qw.assign(b, handle)
	})

	// --- blocker is worked first
	sc.Step(`^issue (SUT-\d+) is assigned to "([^"]*)"$`, func(name, handle string) error {
		return qw.assign(name, handle)
	})
	sc.Step(`^(SUT-\d+) is blocked by issue (SUT-\d+), also assigned to "([^"]*)" and open$`, func(blocked, blocker, handle string) error {
		if err := qw.assign(blocker, handle); err != nil {
			return err
		}
		if err := iw.addRelation("blocks", blocker, blocked, "operator"); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusCreated)
	})

	// --- externally blocked issues are skipped
	sc.Step(`^(SUT-\d+) is blocked by an open issue assigned to "([^"]*)"$`, func(blocked, handle string) error {
		if err := qw.assign("SUT-2", handle); err != nil {
			return err
		}
		if err := iw.addRelation("blocks", "SUT-2", blocked, "operator"); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusCreated)
	})
	sc.Step(`^issue (SUT-\d+) is assigned to "([^"]*)" and unblocked$`, func(name, handle string) error {
		return qw.assign(name, handle)
	})

	// --- non-open statuses are never handed out
	sc.Step(`^issues assigned to "([^"]*)" with statuses "blocked", "deferred", "in-progress", and "complete"$`, func(handle string) error {
		if err := cw.ensureGitProject(); err != nil {
			return err
		}
		for i, status := range []string{"blocked", "deferred", "in-progress"} {
			name := fmt.Sprintf("SUT-%d", i+1)
			if err := qw.assign(name, handle); err != nil {
				return err
			}
			if err := iw.transition(name, status); err != nil {
				return err
			}
			if err := iw.s.expectStatus(http.StatusOK); err != nil {
				return err
			}
		}
		// The complete one goes through the real review-gated close.
		if err := qw.assign("SUT-4", handle); err != nil {
			return err
		}
		if err := cw.approvedReview("SUT-4", "queue-done"); err != nil {
			return err
		}
		if err := cw.close("SUT-4", 0, ""); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	})
	sc.Step(`^issue (SUT-\d+) assigned to "([^"]*)" with status "open"$`, func(name, handle string) error {
		return qw.assign(name, handle)
	})
	sc.Step(`^every non-open issue retains its original status and stays assigned to "([^"]*)"$`, func(handle string) error {
		for i, status := range []string{"blocked", "deferred", "in-progress", "complete"} {
			name := fmt.Sprintf("SUT-%d", i+1)
			if err := iw.readIssue(name); err != nil {
				return err
			}
			if iw.lastIssue.Status != status {
				return fmt.Errorf("%s moved to %q", name, iw.lastIssue.Status)
			}
			if iw.lastIssue.Assignee == nil || *iw.lastIssue.Assignee != iw.identities[handle] {
				return fmt.Errorf("%s lost its assignment", name)
			}
		}
		return nil
	})
	sc.Step(`^(SUT-\d+) is no longer assigned to "([^"]*)"$`, func(name, _ string) error {
		issueID := iw.issues[name]
		actor := iw.identities["operator"]
		if err := iw.s.call(http.MethodPost, "/issues/"+issueID+"/assign",
			map[string]any{"assignee": nil, "actor": actor}); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	})
	sc.Step(`^it receives an explicit empty result$`, func() error {
		if qw.lastPop.status != http.StatusOK {
			return fmt.Errorf("empty pop must be 200, got %d", qw.lastPop.status)
		}
		if qw.lastPop.Issue != nil {
			return fmt.Errorf("expected empty, got %s", qw.lastPop.Issue.ID)
		}
		if qw.lastPop.FeedWatermark == "" {
			return fmt.Errorf("empty pop must carry its watermark")
		}
		return nil
	})

	// --- empty stack is not an error
	sc.Step(`^agent "([^"]*)" has no open assigned issues$`, func(handle string) error {
		_, err := iw.identity(handle)
		return err
	})

	// --- archived project issues are never handed out
	sc.Step(`^issue (SUT-\d+) is assigned to "([^"]*)" in an archived project$`, func(name, handle string) error {
		if err := qw.assign(name, handle); err != nil {
			return err
		}
		actor := iw.identities["operator"]
		if err := iw.s.call(http.MethodPost, "/projects/"+iw.project+"/archive", map[string]string{"actor": actor}); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	})
	sc.Step(`^issue (SUT-\d+) is assigned to "([^"]*)" in an active project$`, func(name, handle string) error {
		// A second, active project holds the workable issue.
		actor := iw.identities["operator"]
		if err := iw.s.call(http.MethodPost, "/projects", map[string]string{"key": "ACT", "name": "Active", "actor": actor}); err != nil {
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
		if err := iw.s.call(http.MethodPost, "/projects/"+created.ID+"/issues",
			map[string]string{"title": "active work", "actor": actor}); err != nil {
			return err
		}
		if err := iw.s.expectStatus(http.StatusCreated); err != nil {
			return err
		}
		var issue struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(iw.s.lastBody, &issue); err != nil {
			return err
		}
		iw.issues[name] = issue.ID
		assignee, err := iw.identity(handle)
		if err != nil {
			return err
		}
		if err := iw.s.call(http.MethodPost, "/issues/"+issue.ID+"/assign",
			map[string]any{"assignee": assignee, "actor": actor}); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	})
	sc.Step(`^(SUT-\d+) is unmutated$`, func(name string) error {
		if err := iw.readIssue(name); err != nil {
			return err
		}
		if iw.lastIssue.Status != "open" {
			return fmt.Errorf("archived-project issue mutated to %q", iw.lastIssue.Status)
		}
		return nil
	})

	// --- same-key replay claims nothing new
	sc.Step(`^"([^"]*)" popped its work stack with idempotency key "K" and received (SUT-\d+)$`, func(handle, name string) error {
		if err := qw.assign(name, handle); err != nil {
			return err
		}
		if err := qw.assign("SUT-2", handle); err != nil {
			return err
		}
		id := iw.identities[handle]
		status, body, err := iw.s.callKeyed(http.MethodPost, "/identities/"+id+"/work-stack/pop", map[string]string{}, "K")
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("pop: %d %s", status, body)
		}
		qw.replayed.key = "K"
		qw.replayed.status = status
		qw.replayed.body = body
		var out popResult
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			return err
		}
		if out.Issue == nil || out.Issue.ID != iw.issues[name] {
			return fmt.Errorf("expected %s claimed", name)
		}
		return nil
	})
	sc.Step(`^the pop is replayed with idempotency key "K"$`, func() error {
		id := iw.identities["claude"]
		status, body, err := iw.s.callKeyed(http.MethodPost, "/identities/"+id+"/work-stack/pop", map[string]string{}, "K")
		if err != nil {
			return err
		}
		if status != qw.replayed.status || body != qw.replayed.body {
			return fmt.Errorf("replay differs: %d %s", status, body)
		}
		return nil
	})
	sc.Step(`^the response is identical to the original$`, func() error {
		return nil // asserted in the replay step
	})
	sc.Step(`^no additional issue is claimed$`, func() error {
		if err := iw.readIssue("SUT-2"); err != nil {
			return err
		}
		if iw.lastIssue.Status != "open" {
			return fmt.Errorf("replay claimed a second issue: SUT-2 is %q", iw.lastIssue.Status)
		}
		return nil
	})

	// --- pop/defer races (REQ-status-workflow)
	sc.Step(`^a pop by "([^"]*)" claims (SUT-\d+) first$`, func(handle, name string) error {
		if err := qw.pop(handle); err != nil {
			return err
		}
		return qw.expectClaimed(name)
	})
	sc.Step(`^a transition to "deferred" with expected status "open" lands first$`, func() error {
		issueID := iw.issues["SUT-1"]
		actor := iw.identities["operator"]
		if err := iw.s.call(http.MethodPost, "/issues/"+issueID+"/status", map[string]any{
			"status": "deferred", "expected_status": "open", "actor": actor,
		}); err != nil {
			return err
		}
		return iw.s.expectStatus(http.StatusOK)
	})
	sc.Step(`^(SUT-\d+) still has status "deferred"$`, func(name string) error {
		if err := iw.readIssue(name); err != nil {
			return err
		}
		if iw.lastIssue.Status != "deferred" {
			return fmt.Errorf("%s is %q", name, iw.lastIssue.Status)
		}
		return nil
	})
}
