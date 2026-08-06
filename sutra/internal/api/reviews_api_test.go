package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"sutra/internal/review"
)

// gitRepo builds a throwaway repository: one commit on main (the merge
// base), then a feature branch with one commit ahead.
type gitRepo struct {
	path                 string
	baseSHA, featureSHA  string
	featureSHA2, headSHA string
}

func newGitRepo(t *testing.T) gitRepo {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v — %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	run("init", "-b", "main")
	write("a.txt", "base")
	run("add", ".")
	run("commit", "-m", "base")
	base := run("rev-parse", "HEAD")
	run("checkout", "-b", "feature")
	write("b.txt", "feature work")
	run("add", ".")
	run("commit", "-m", "feature")
	feature := run("rev-parse", "HEAD")
	write("c.txt", "rework")
	run("add", ".")
	run("commit", "-m", "rework")
	feature2 := run("rev-parse", "HEAD")
	run("checkout", "main")
	return gitRepo{path: dir, baseSHA: base, featureSHA: feature, featureSHA2: feature2, headSHA: base}
}

func TestReviewLifecycleEndToEnd(t *testing.T) {
	srv, _ := startAPI(t)
	repo := newGitRepo(t)

	var out map[string]any
	decode := func(status int, body string, want int, label string) {
		t.Helper()
		if status != want {
			t.Fatalf("%s: expected %d, got %d: %s", label, want, status, body)
		}
		out = nil
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("%s: decode: %v — %s", label, err, body)
		}
	}

	status, body := req(t, srv, http.MethodPost, "/identities", "id", `{"handle":"op","kind":"human"}`)
	decode(status, body, http.StatusCreated, "identity")
	actor := out["id"].(string)
	status, body = req(t, srv, http.MethodPost, "/projects", "proj",
		fmt.Sprintf(`{"key":"SUT","name":"Sutra","actor":%q,"repo_path":%q}`, actor, repo.path))
	decode(status, body, http.StatusCreated, "project")
	project := out["id"].(string)
	status, body = req(t, srv, http.MethodPost, "/projects/"+project+"/issues", "iss",
		fmt.Sprintf(`{"title":"work","actor":%q}`, actor))
	decode(status, body, http.StatusCreated, "issue")
	issue := out["id"].(string)

	// Create pins base_commit as the merge base, never client-supplied.
	status, body = req(t, srv, http.MethodPost, "/reviews", "rev",
		fmt.Sprintf(`{"issue":%q,"author":%q,"branch":"feature","commit":%q}`, issue, actor, repo.featureSHA))
	decode(status, body, http.StatusCreated, "review")
	reviewID := out["id"].(string)
	subs := out["submissions"].([]any)
	if len(subs) != 1 || subs[0].(map[string]any)["base_commit"] != repo.baseSHA {
		t.Fatalf("submission must pin the resolved merge base %s: %v", repo.baseSHA, subs)
	}

	// Changes requested at revision 1.
	status, body = req(t, srv, http.MethodPost, "/reviews/"+reviewID+"/verdict", "v1",
		fmt.Sprintf(`{"verdict":"changes-requested","revision":1,"actor":%q}`, actor))
	decode(status, body, http.StatusOK, "verdict-1")
	changesEvent := out["latest_verdict_event"].(string)

	// A stale verdict naming revision 1 content the reviewer saw is
	// fine now, but after resubmission it must reject (tested below).
	status, body = req(t, srv, http.MethodPost, "/reviews/"+reviewID+"/resubmit", "rs",
		fmt.Sprintf(`{"author":%q,"expected_revision":1,"expected_verdict_event":%q,"branch":"feature","commit":%q}`,
			actor, changesEvent, repo.featureSHA2))
	decode(status, body, http.StatusOK, "resubmit")
	if out["revision"].(float64) != 2 || out["state"].(string) != "open" {
		t.Fatalf("resubmit must reopen at revision 2: %s", body)
	}

	// Stale verdict for revision 1 rejects — the reviewer never saw
	// revision 2's content (AC-review-stale-guard).
	status, body = req(t, srv, http.MethodPost, "/reviews/"+reviewID+"/verdict", "v-stale",
		fmt.Sprintf(`{"verdict":"approved","revision":1,"actor":%q}`, actor))
	if status != http.StatusConflict {
		t.Fatalf("stale verdict must 409: %d %s", status, body)
	}

	// Approve revision 2.
	status, body = req(t, srv, http.MethodPost, "/reviews/"+reviewID+"/verdict", "v2",
		fmt.Sprintf(`{"verdict":"approved","revision":2,"actor":%q}`, actor))
	decode(status, body, http.StatusOK, "verdict-2")
	approvalEvent := out["latest_verdict_event"].(string)

	// Close the issue naming review, revision, and verdict event.
	closeBody := fmt.Sprintf(`{"status":"complete","review":%q,"review_revision":2,"review_verdict_event":%q,"actor":%q}`,
		reviewID, approvalEvent, actor)
	status, body = req(t, srv, http.MethodPost, "/issues/"+issue+"/status", "close", closeBody)
	decode(status, body, http.StatusOK, "close")
	if out["status"].(string) != "complete" {
		t.Fatalf("close did not complete: %s", body)
	}

	// The spend stamped close_used and the consumption fields.
	status, body = req(t, srv, http.MethodGet, "/reviews/"+reviewID, "", "")
	decode(status, body, http.StatusOK, "review-after-close")
	if out["close_used"] == nil || out["consumed"] == nil || out["consumed_revision"].(float64) != 2 {
		t.Fatalf("close must stamp close_used and consumption: %s", body)
	}

	// A verdict on the consumed review is frozen.
	status, body = req(t, srv, http.MethodPost, "/reviews/"+reviewID+"/verdict", "v3",
		fmt.Sprintf(`{"verdict":"changes-requested","revision":2,"actor":%q}`, actor))
	if status != http.StatusConflict {
		t.Fatalf("consumed review verdict must freeze: %d %s", status, body)
	}

	// Reopen the issue, then a second close with the SPENT review
	// rejects — a spent review never authorizes another close.
	status, body = req(t, srv, http.MethodPost, "/issues/"+issue+"/status", "reopen",
		fmt.Sprintf(`{"status":"open","actor":%q}`, actor))
	decode(status, body, http.StatusOK, "reopen")
	status, body = req(t, srv, http.MethodPost, "/issues/"+issue+"/status", "close-2", closeBody)
	if status != http.StatusConflict {
		t.Fatalf("spent review must not close again: %d %s", status, body)
	}
	var envelope struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil || envelope.Code != "review-close-used" {
		t.Fatalf("expected review-close-used, got %s", body)
	}
}

func TestConsumeFencesAndSingleWinner(t *testing.T) {
	srv, _ := startAPI(t)
	repo := newGitRepo(t)

	var out map[string]any
	decode := func(status int, body string, want int, label string) {
		t.Helper()
		if status != want {
			t.Fatalf("%s: expected %d, got %d: %s", label, want, status, body)
		}
		out = nil
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("%s: decode: %v", label, err)
		}
	}
	status, body := req(t, srv, http.MethodPost, "/identities", "id", `{"handle":"op","kind":"human"}`)
	decode(status, body, http.StatusCreated, "identity")
	actor := out["id"].(string)
	status, body = req(t, srv, http.MethodPost, "/projects", "proj",
		fmt.Sprintf(`{"key":"SUT","name":"S","actor":%q,"repo_path":%q}`, actor, repo.path))
	decode(status, body, http.StatusCreated, "project")
	project := out["id"].(string)
	status, body = req(t, srv, http.MethodPost, "/projects/"+project+"/issues", "iss",
		fmt.Sprintf(`{"title":"w","actor":%q}`, actor))
	decode(status, body, http.StatusCreated, "issue")
	issue := out["id"].(string)
	status, body = req(t, srv, http.MethodPost, "/reviews", "rev",
		fmt.Sprintf(`{"issue":%q,"author":%q,"branch":"feature","commit":%q}`, issue, actor, repo.featureSHA))
	decode(status, body, http.StatusCreated, "review")
	reviewID := out["id"].(string)
	status, body = req(t, srv, http.MethodPost, "/reviews/"+reviewID+"/verdict", "v",
		fmt.Sprintf(`{"verdict":"approved","revision":1,"actor":%q}`, actor))
	decode(status, body, http.StatusOK, "verdict")
	approvalEvent := out["latest_verdict_event"].(string)

	consumeBody := fmt.Sprintf(`{"expected_revision":1,"expected_verdict_event":%q,"actor":%q}`, approvalEvent, actor)
	status, body = req(t, srv, http.MethodPost, "/reviews/"+reviewID+"/consume", "c1", consumeBody)
	decode(status, body, http.StatusOK, "consume")

	// Replay under the ORIGINAL key returns the original success.
	status2, body2 := req(t, srv, http.MethodPost, "/reviews/"+reviewID+"/consume", "c1", consumeBody)
	if status2 != http.StatusOK || body2 != body {
		t.Fatalf("original-key replay must return the original success: %d", status2)
	}

	// A DISTINCT key against the consumed approval conflicts — two
	// subscribers can never both act.
	status, body = req(t, srv, http.MethodPost, "/reviews/"+reviewID+"/consume", "c2", consumeBody)
	if status != http.StatusConflict {
		t.Fatalf("distinct-key consume of a consumed approval must 409: %d %s", status, body)
	}
	var envelope struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil || envelope.Code != "review-consumed" {
		t.Fatalf("expected review-consumed, got %s", body)
	}
}

func TestExpectedBaseAndHeadFences(t *testing.T) {
	srv, _ := startAPI(t)
	repo := newGitRepo(t)

	var out map[string]any
	decode := func(status int, body string, want int, label string) {
		t.Helper()
		if status != want {
			t.Fatalf("%s: expected %d, got %d: %s", label, want, status, body)
		}
		out = nil
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("%s: decode: %v", label, err)
		}
	}
	status, body := req(t, srv, http.MethodPost, "/identities", "id", `{"handle":"op","kind":"human"}`)
	decode(status, body, http.StatusCreated, "identity")
	actor := out["id"].(string)
	status, body = req(t, srv, http.MethodPost, "/projects", "proj",
		fmt.Sprintf(`{"key":"SUT","name":"S","actor":%q,"repo_path":%q}`, actor, repo.path))
	decode(status, body, http.StatusCreated, "project")
	project := out["id"].(string)
	status, body = req(t, srv, http.MethodPost, "/projects/"+project+"/issues", "iss",
		fmt.Sprintf(`{"title":"w","actor":%q}`, actor))
	decode(status, body, http.StatusCreated, "issue")
	issue := out["id"].(string)

	// A wrong expected_base_commit rejects with nothing created.
	status, body = req(t, srv, http.MethodPost, "/reviews", "fence-1",
		fmt.Sprintf(`{"issue":%q,"author":%q,"branch":"feature","commit":%q,"expected_base_commit":%q}`,
			issue, actor, repo.featureSHA, repo.featureSHA2))
	if status != http.StatusConflict {
		t.Fatalf("base fence must 409: %d %s", status, body)
	}
	status, body = req(t, srv, http.MethodGet, "/reviews?issue="+issue, "", "")
	if status != http.StatusOK || body != "[]" {
		t.Fatalf("fence rejection must create nothing: %d %s", status, body)
	}

	// A wrong expected_default_head rejects with nothing created — the
	// head fence is exercised independently of the base fence.
	status, body = req(t, srv, http.MethodPost, "/reviews", "fence-head",
		fmt.Sprintf(`{"issue":%q,"author":%q,"branch":"feature","commit":%q,"expected_default_head":%q}`,
			issue, actor, repo.featureSHA, repo.featureSHA2))
	if status != http.StatusConflict {
		t.Fatalf("head fence must 409: %d %s", status, body)
	}
	status, body = req(t, srv, http.MethodGet, "/reviews?issue="+issue, "", "")
	if status != http.StatusOK || body != "[]" {
		t.Fatalf("head-fence rejection must create nothing: %d %s", status, body)
	}

	// Matching fences pass.
	status, body = req(t, srv, http.MethodPost, "/reviews", "fence-2",
		fmt.Sprintf(`{"issue":%q,"author":%q,"branch":"feature","commit":%q,"expected_base_commit":%q,"expected_default_head":%q}`,
			issue, actor, repo.featureSHA, repo.baseSHA, repo.headSHA))
	decode(status, body, http.StatusCreated, "fenced create")

	// Resubmission fences guard the same way: request changes, move the
	// default branch so BOTH the head and the recomputed merge base
	// differ from the caller's stale observation, and assert rejection
	// with no mutation. (Movement BETWEEN resolution and revalidation
	// inside one request is not injectable through the public API; both
	// checks run the same comparison, so stale-observation coverage
	// exercises the identical rejection paths.)
	var created map[string]any
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("decode fenced create: %v", err)
	}
	reviewID := created["id"].(string)
	status, body = req(t, srv, http.MethodPost, "/reviews/"+reviewID+"/verdict", "fence-cr",
		fmt.Sprintf(`{"verdict":"changes-requested","revision":1,"actor":%q}`, actor))
	if status != http.StatusOK {
		t.Fatalf("cr: %d %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatalf("decode cr: %v", err)
	}
	crEvent := created["latest_verdict_event"].(string)

	move := exec.Command("git", "-C", repo.path, "merge", "--ff-only", "feature")
	move.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := move.CombinedOutput(); err != nil {
		t.Fatalf("advance default branch: %v — %s", err, out)
	}

	status, body = req(t, srv, http.MethodPost, "/reviews/"+reviewID+"/resubmit", "fence-rs",
		fmt.Sprintf(`{"author":%q,"expected_revision":1,"expected_verdict_event":%q,"branch":"feature","commit":%q,"expected_default_head":%q}`,
			actor, crEvent, repo.featureSHA2, repo.headSHA))
	if status != http.StatusConflict {
		t.Fatalf("stale head on resubmit must 409 after branch movement: %d %s", status, body)
	}
	status, body = req(t, srv, http.MethodGet, "/reviews/"+reviewID, "", "")
	if err := json.Unmarshal([]byte(body), &created); err != nil || status != http.StatusOK {
		t.Fatalf("read: %d %s (%v)", status, body, err)
	}
	if created["revision"].(float64) != 1 || created["state"].(string) != "changes-requested" {
		t.Fatalf("rejected resubmission mutated the review: %s", body)
	}
}

// TestAgentVerdictsRejected pins the human-approval rule: an agent
// identity can never satisfy the close gate by approving a review.
func TestAgentVerdictsRejected(t *testing.T) {
	srv, _ := startAPI(t)
	repo := newGitRepo(t)
	var out map[string]any
	decode := func(status int, body string, want int, label string) {
		t.Helper()
		if status != want {
			t.Fatalf("%s: expected %d, got %d: %s", label, want, status, body)
		}
		out = nil
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("%s: decode: %v", label, err)
		}
	}
	status, body := req(t, srv, http.MethodPost, "/identities", "h", `{"handle":"op","kind":"human"}`)
	decode(status, body, http.StatusCreated, "human")
	human := out["id"].(string)
	status, body = req(t, srv, http.MethodPost, "/identities", "a", `{"handle":"bot","kind":"agent"}`)
	decode(status, body, http.StatusCreated, "agent")
	agent := out["id"].(string)
	status, body = req(t, srv, http.MethodPost, "/projects", "p",
		fmt.Sprintf(`{"key":"SUT","name":"S","actor":%q,"repo_path":%q}`, human, repo.path))
	decode(status, body, http.StatusCreated, "project")
	project := out["id"].(string)
	status, body = req(t, srv, http.MethodPost, "/projects/"+project+"/issues", "i",
		fmt.Sprintf(`{"title":"w","actor":%q}`, human))
	decode(status, body, http.StatusCreated, "issue")
	issue := out["id"].(string)
	status, body = req(t, srv, http.MethodPost, "/reviews", "r",
		fmt.Sprintf(`{"issue":%q,"author":%q,"branch":"feature","commit":%q,"session":"sess-1"}`, issue, agent, repo.featureSHA))
	decode(status, body, http.StatusCreated, "review")
	reviewID := out["id"].(string)

	status, body = req(t, srv, http.MethodPost, "/reviews/"+reviewID+"/verdict", "v-agent",
		fmt.Sprintf(`{"verdict":"approved","revision":1,"actor":%q}`, agent))
	if status != http.StatusBadRequest {
		t.Fatalf("agent verdict must reject: %d %s", status, body)
	}
	status, body = req(t, srv, http.MethodGet, "/reviews/"+reviewID, "", "")
	decode(status, body, http.StatusOK, "after")
	if out["state"].(string) != "open" {
		t.Fatalf("agent verdict mutated state: %s", body)
	}

	// The human's changes-requested payload routes rework: it carries
	// the review, its issue, and the submitting session.
	status, body = req(t, srv, http.MethodPost, "/reviews/"+reviewID+"/verdict", "v-human",
		fmt.Sprintf(`{"verdict":"changes-requested","revision":1,"actor":%q}`, human))
	decode(status, body, http.StatusOK, "human verdict")
	status, body = req(t, srv, http.MethodGet, "/events?kind=review.changes-requested", "", "")
	var page struct {
		Events []struct {
			Payload json.RawMessage `json:"payload"`
		} `json:"events"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil || status != http.StatusOK || len(page.Events) != 1 {
		t.Fatalf("feed: %d %s (%v)", status, body, err)
	}
	var payload struct {
		Review  string `json:"review"`
		Issue   string `json:"issue"`
		Session string `json:"session"`
	}
	if err := json.Unmarshal(page.Events[0].Payload, &payload); err != nil {
		t.Fatalf("payload: %v — %s", err, page.Events[0].Payload)
	}
	if payload.Review != reviewID || payload.Issue != issue || payload.Session != "sess-1" {
		t.Fatalf("rework payload must carry review, issue, and session: %+v", payload)
	}
}

// TestArchivedProjectFreezesReviewMutations pins the read-only rule for
// verdict and consumption through the review's issue's project.
func TestArchivedProjectFreezesReviewMutations(t *testing.T) {
	srv, _ := startAPI(t)
	repo := newGitRepo(t)
	var out map[string]any
	decode := func(status int, body string, want int, label string) {
		t.Helper()
		if status != want {
			t.Fatalf("%s: expected %d, got %d: %s", label, want, status, body)
		}
		out = nil
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("%s: decode: %v", label, err)
		}
	}
	status, body := req(t, srv, http.MethodPost, "/identities", "h", `{"handle":"op","kind":"human"}`)
	decode(status, body, http.StatusCreated, "human")
	actor := out["id"].(string)
	status, body = req(t, srv, http.MethodPost, "/projects", "p",
		fmt.Sprintf(`{"key":"SUT","name":"S","actor":%q,"repo_path":%q}`, actor, repo.path))
	decode(status, body, http.StatusCreated, "project")
	project := out["id"].(string)
	status, body = req(t, srv, http.MethodPost, "/projects/"+project+"/issues", "i",
		fmt.Sprintf(`{"title":"w","actor":%q}`, actor))
	decode(status, body, http.StatusCreated, "issue")
	issue := out["id"].(string)
	status, body = req(t, srv, http.MethodPost, "/reviews", "r",
		fmt.Sprintf(`{"issue":%q,"author":%q,"branch":"feature","commit":%q}`, issue, actor, repo.featureSHA))
	decode(status, body, http.StatusCreated, "review")
	reviewID := out["id"].(string)
	status, body = req(t, srv, http.MethodPost, "/reviews/"+reviewID+"/verdict", "v",
		fmt.Sprintf(`{"verdict":"approved","revision":1,"actor":%q}`, actor))
	decode(status, body, http.StatusOK, "approve")
	approvalEvent := out["latest_verdict_event"].(string)

	if status, body := req(t, srv, http.MethodPost, "/projects/"+project+"/archive", "arch",
		fmt.Sprintf(`{"actor":%q}`, actor)); status != http.StatusOK {
		t.Fatalf("archive: %d %s", status, body)
	}

	status, body = req(t, srv, http.MethodPost, "/reviews/"+reviewID+"/verdict", "v2",
		fmt.Sprintf(`{"verdict":"changes-requested","revision":1,"actor":%q}`, actor))
	if status != http.StatusConflict {
		t.Fatalf("verdict into archived project must 409: %d %s", status, body)
	}
	status, body = req(t, srv, http.MethodPost, "/reviews/"+reviewID+"/consume", "c",
		fmt.Sprintf(`{"expected_revision":1,"expected_verdict_event":%q,"actor":%q}`, approvalEvent, actor))
	if status != http.StatusConflict {
		t.Fatalf("consume into archived project must 409: %d %s", status, body)
	}
	status, body = req(t, srv, http.MethodGet, "/reviews/"+reviewID, "", "")
	decode(status, body, http.StatusOK, "after")
	if out["state"].(string) != "approved" || out["consumed"] != nil {
		t.Fatalf("archived-project review mutated: %s", body)
	}
}

// TestDeliverableResolvesPinnedDiff pins AC-review-web: content is the
// complete diff between the submission's pinned commits, per revision,
// with historical revisions still resolvable after resubmission.
func TestDeliverableResolvesPinnedDiff(t *testing.T) {
	srv, _ := startAPI(t)
	repo := newGitRepo(t)
	var out map[string]any
	decode := func(status int, body string, want int, label string) {
		t.Helper()
		if status != want {
			t.Fatalf("%s: expected %d, got %d: %s", label, want, status, body)
		}
		out = nil
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("%s: decode: %v", label, err)
		}
	}
	status, body := req(t, srv, http.MethodPost, "/identities", "h", `{"handle":"op","kind":"human"}`)
	decode(status, body, http.StatusCreated, "identity")
	actor := out["id"].(string)
	status, body = req(t, srv, http.MethodPost, "/projects", "p",
		fmt.Sprintf(`{"key":"SUT","name":"S","actor":%q,"repo_path":%q}`, actor, repo.path))
	decode(status, body, http.StatusCreated, "project")
	project := out["id"].(string)
	status, body = req(t, srv, http.MethodPost, "/projects/"+project+"/issues", "i",
		fmt.Sprintf(`{"title":"w","actor":%q}`, actor))
	decode(status, body, http.StatusCreated, "issue")
	issue := out["id"].(string)
	status, body = req(t, srv, http.MethodPost, "/reviews", "r",
		fmt.Sprintf(`{"issue":%q,"author":%q,"branch":"feature","commit":%q}`, issue, actor, repo.featureSHA))
	decode(status, body, http.StatusCreated, "review")
	reviewID := out["id"].(string)

	status, body = req(t, srv, http.MethodGet, "/reviews/"+reviewID+"/deliverable", "", "")
	decode(status, body, http.StatusOK, "deliverable")
	if out["kind"].(string) != "code" || !strings.Contains(out["content"].(string), "feature work") {
		t.Fatalf("deliverable must carry the pinned diff: %s", body)
	}

	// After a resubmission, revision 1's deliverable still resolves to
	// ITS pinned diff — history never shifts.
	status, body = req(t, srv, http.MethodPost, "/reviews/"+reviewID+"/verdict", "v",
		fmt.Sprintf(`{"verdict":"changes-requested","revision":1,"actor":%q}`, actor))
	decode(status, body, http.StatusOK, "cr")
	crEvent := out["latest_verdict_event"].(string)
	status, body = req(t, srv, http.MethodPost, "/reviews/"+reviewID+"/resubmit", "rs",
		fmt.Sprintf(`{"author":%q,"expected_revision":1,"expected_verdict_event":%q,"branch":"feature","commit":%q,"summary":"rework summary"}`,
			actor, crEvent, repo.featureSHA2))
	decode(status, body, http.StatusOK, "resubmit")
	if out["summary"].(string) != "rework summary" {
		t.Fatalf("resubmit summary discarded: %s", body)
	}

	status, body = req(t, srv, http.MethodGet, "/reviews/"+reviewID+"/deliverable?revision=1", "", "")
	decode(status, body, http.StatusOK, "historical deliverable")
	if strings.Contains(out["content"].(string), "rework") {
		t.Fatalf("revision 1 deliverable leaked revision 2 content: %s", body)
	}
	status, body = req(t, srv, http.MethodGet, "/reviews/"+reviewID+"/deliverable", "", "")
	decode(status, body, http.StatusOK, "latest deliverable")
	if !strings.Contains(out["content"].(string), "rework") {
		t.Fatalf("latest deliverable missing revision 2 content: %s", body)
	}
}

// TestNullSubmissionFencesRejected pins that explicit nulls never
// disarm the creation fences.
func TestNullSubmissionFencesRejected(t *testing.T) {
	srv, _ := startAPI(t)
	repo := newGitRepo(t)
	var out map[string]any
	decode := func(status int, body string, want int, label string) {
		t.Helper()
		if status != want {
			t.Fatalf("%s: expected %d, got %d: %s", label, want, status, body)
		}
		out = nil
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("%s: decode: %v", label, err)
		}
	}
	status, body := req(t, srv, http.MethodPost, "/identities", "h", `{"handle":"op","kind":"human"}`)
	decode(status, body, http.StatusCreated, "identity")
	actor := out["id"].(string)
	status, body = req(t, srv, http.MethodPost, "/projects", "p",
		fmt.Sprintf(`{"key":"SUT","name":"S","actor":%q,"repo_path":%q}`, actor, repo.path))
	decode(status, body, http.StatusCreated, "project")
	project := out["id"].(string)
	status, body = req(t, srv, http.MethodPost, "/projects/"+project+"/issues", "i",
		fmt.Sprintf(`{"title":"w","actor":%q}`, actor))
	decode(status, body, http.StatusCreated, "issue")
	issue := out["id"].(string)

	status, body = req(t, srv, http.MethodPost, "/reviews", "null-fence",
		fmt.Sprintf(`{"issue":%q,"author":%q,"branch":"feature","commit":%q,"expected_base_commit":null}`, issue, actor, repo.featureSHA))
	if status != http.StatusBadRequest {
		t.Fatalf("null fence must 400: %d %s", status, body)
	}
	status, body = req(t, srv, http.MethodGet, "/reviews?issue="+issue, "", "")
	if status != http.StatusOK || body != "[]" {
		t.Fatalf("null-fence rejection created a review: %d %s", status, body)
	}
}

// TestDiffIgnoresRepoDiffConfig pins that repository-configured diff
// helpers never execute or transform the pinned patch.
func TestDiffIgnoresRepoDiffConfig(t *testing.T) {
	srv, _ := startAPI(t)
	repo := newGitRepo(t)
	// A hostile or cosmetic external diff configured in the repo must
	// not run: the endpoint serves git's own patch bytes.
	cfg := exec.Command("git", "-C", repo.path, "config", "diff.external", "echo HIJACKED")
	if out, err := cfg.CombinedOutput(); err != nil {
		t.Fatalf("config: %v — %s", err, out)
	}
	var out map[string]any
	decode := func(status int, body string, want int, label string) {
		t.Helper()
		if status != want {
			t.Fatalf("%s: expected %d, got %d: %s", label, want, status, body)
		}
		out = nil
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("%s: decode: %v", label, err)
		}
	}
	status, body := req(t, srv, http.MethodPost, "/identities", "h", `{"handle":"op","kind":"human"}`)
	decode(status, body, http.StatusCreated, "identity")
	actor := out["id"].(string)
	status, body = req(t, srv, http.MethodPost, "/projects", "p",
		fmt.Sprintf(`{"key":"SUT","name":"S","actor":%q,"repo_path":%q}`, actor, repo.path))
	decode(status, body, http.StatusCreated, "project")
	project := out["id"].(string)
	status, body = req(t, srv, http.MethodPost, "/projects/"+project+"/issues", "i",
		fmt.Sprintf(`{"title":"w","actor":%q}`, actor))
	decode(status, body, http.StatusCreated, "issue")
	issue := out["id"].(string)
	status, body = req(t, srv, http.MethodPost, "/reviews", "r",
		fmt.Sprintf(`{"issue":%q,"author":%q,"branch":"feature","commit":%q}`, issue, actor, repo.featureSHA))
	decode(status, body, http.StatusCreated, "review")
	reviewID := out["id"].(string)

	status, body = req(t, srv, http.MethodGet, "/reviews/"+reviewID+"/deliverable", "", "")
	decode(status, body, http.StatusOK, "deliverable")
	content := out["content"].(string)
	if strings.Contains(content, "HIJACKED") {
		t.Fatalf("repo-configured external diff executed: %s", content)
	}
	if !strings.Contains(content, "feature work") {
		t.Fatalf("expected the real patch: %s", content)
	}
}

// TestDiffSizeCap pins the documented output bound.
func TestDiffSizeCap(t *testing.T) {
	old := review.MaxDiffBytes
	review.MaxDiffBytes = 16
	t.Cleanup(func() { review.MaxDiffBytes = old })

	srv, _ := startAPI(t)
	repo := newGitRepo(t)
	var out map[string]any
	decode := func(status int, body string, want int, label string) {
		t.Helper()
		if status != want {
			t.Fatalf("%s: expected %d, got %d: %s", label, want, status, body)
		}
		out = nil
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("%s: decode: %v", label, err)
		}
	}
	status, body := req(t, srv, http.MethodPost, "/identities", "h", `{"handle":"op","kind":"human"}`)
	decode(status, body, http.StatusCreated, "identity")
	actor := out["id"].(string)
	status, body = req(t, srv, http.MethodPost, "/projects", "p",
		fmt.Sprintf(`{"key":"SUT","name":"S","actor":%q,"repo_path":%q}`, actor, repo.path))
	decode(status, body, http.StatusCreated, "project")
	project := out["id"].(string)
	status, body = req(t, srv, http.MethodPost, "/projects/"+project+"/issues", "i",
		fmt.Sprintf(`{"title":"w","actor":%q}`, actor))
	decode(status, body, http.StatusCreated, "issue")
	issue := out["id"].(string)
	status, body = req(t, srv, http.MethodPost, "/reviews", "r",
		fmt.Sprintf(`{"issue":%q,"author":%q,"branch":"feature","commit":%q}`, issue, actor, repo.featureSHA))
	decode(status, body, http.StatusCreated, "review")
	reviewID := out["id"].(string)

	status, body = req(t, srv, http.MethodGet, "/reviews/"+reviewID+"/deliverable", "", "")
	if status != http.StatusConflict {
		t.Fatalf("oversized diff must 409, got %d %s", status, body)
	}
	if !strings.Contains(body, "limit") {
		t.Fatalf("expected the limit named: %s", body)
	}
}

// TestRevisionSyntaxBranchRejected pins exact-ref resolution: a
// default_branch like "main~1" must fail, never resolve to an ancestor.
func TestRevisionSyntaxBranchRejected(t *testing.T) {
	srv, _ := startAPI(t)
	repo := newGitRepo(t)
	var out map[string]any
	decode := func(status int, body string, want int, label string) {
		t.Helper()
		if status != want {
			t.Fatalf("%s: expected %d, got %d: %s", label, want, status, body)
		}
		out = nil
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("%s: decode: %v", label, err)
		}
	}
	status, body := req(t, srv, http.MethodPost, "/identities", "h", `{"handle":"op","kind":"human"}`)
	decode(status, body, http.StatusCreated, "identity")
	actor := out["id"].(string)
	status, body = req(t, srv, http.MethodPost, "/projects", "p",
		fmt.Sprintf(`{"key":"SUT","name":"S","actor":%q,"repo_path":%q,"default_branch":"main~1"}`, actor, repo.path))
	decode(status, body, http.StatusCreated, "project")
	project := out["id"].(string)
	status, body = req(t, srv, http.MethodPost, "/projects/"+project+"/issues", "i",
		fmt.Sprintf(`{"title":"w","actor":%q}`, actor))
	decode(status, body, http.StatusCreated, "issue")
	issue := out["id"].(string)
	status, body = req(t, srv, http.MethodPost, "/reviews", "r",
		fmt.Sprintf(`{"issue":%q,"author":%q,"branch":"feature","commit":%q}`, issue, actor, repo.featureSHA))
	if status != http.StatusConflict {
		t.Fatalf("revision-syntax branch must fail resolution: %d %s", status, body)
	}
}
