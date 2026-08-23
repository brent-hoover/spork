package devloop_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"kriya/internal/devloop"
	"kriya/internal/fakes"
	"kriya/internal/reviewbridge"
)

type memStore struct{ rows []devloop.Session }

func (m *memStore) Upsert(_ context.Context, s devloop.Session) error {
	for i, existing := range m.rows {
		if existing.Run == s.Run {
			m.rows[i] = s
			return nil
		}
	}
	m.rows = append(m.rows, s)
	return nil
}

func (m *memStore) only(t *testing.T) devloop.Session {
	t.Helper()
	if len(m.rows) != 1 {
		t.Fatalf("expected one session, got %d", len(m.rows))
	}
	return m.rows[0]
}

func request() devloop.Request {
	return devloop.Request{
		Run: "run-1", Ticket: "KRI-1", Title: "Create a short link",
		Criteria:   []string{"AC-valid-url"},
		Workspace:  "/wt/kri-1",
		Commands:   map[string]string{"test": "go test ./...", "mutation": "gremlins"},
		AllowRules: []string{"Bash(/wt/kri-1/.kriya/gate-test *)"},
	}
}

func TestASessionIsRecordedBeforeTheAgentRuns(t *testing.T) {
	// AC-thread-no-loss: every outcome reaches the thread catalog, crashed
	// included. A crash mid-session must leave a row recovery can terminate
	// and import from, so the row exists before the agent is invoked.
	store := &memStore{}
	ag := &fakes.Agent{Err: errors.New("agent died")}
	loop := devloop.Loop{Agent: ag, Store: store, Now: fakes.NewClock(time.Unix(0, 0))}
	if _, err := loop.Work(context.Background(), request()); err == nil {
		t.Fatal("expected the agent failure to surface")
	}
	row := store.only(t)
	if row.Ticket != "KRI-1" {
		t.Errorf("no session row survived the failure: %+v", row)
	}
	if row.Ended {
		t.Error("a crashed session is not ended")
	}
}

func TestACompletedSessionRecordsItsIdentity(t *testing.T) {
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply()}
	loop := devloop.Loop{Agent: ag, Store: store, Now: fakes.NewClock(time.Unix(0, 0))}
	got, err := loop.Work(context.Background(), request())
	if err != nil {
		t.Fatalf("work: %v", err)
	}
	if got.SessionID == "" {
		t.Error("no session id, so the transcript cannot be found from the ticket")
	}
	if got.Model == "" {
		t.Error("no model recorded")
	}
	if !store.only(t).Ended {
		t.Error("the stored session is not stamped ended")
	}
}

func TestTheAgentIsToldWhatWillJudgeIt(t *testing.T) {
	// The gate commands travel as CONTENT so the agent knows the bar; the
	// permission to run them is granted separately, and the prompt must not
	// be the only place either appears.
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply()}
	loop := devloop.Loop{Agent: ag, Store: store, Now: fakes.NewClock(time.Unix(0, 0))}
	if _, err := loop.Work(context.Background(), request()); err != nil {
		t.Fatalf("work: %v", err)
	}
	req := ag.Requests[0]
	for _, want := range []string{"KRI-1", "AC-valid-url", "go test ./...", "test first"} {
		if !strings.Contains(req.Prompt, want) && !strings.Contains(req.Prompt, "Create a short link") {
			t.Errorf("the prompt does not carry %q", want)
		}
	}
	if req.Workspace != "/wt/kri-1" {
		t.Errorf("the agent was pointed at %q", req.Workspace)
	}
	if len(req.AllowRules) == 0 {
		t.Error("no permission rules were passed, so the agent gets defaults")
	}
	if req.Role != "dev" {
		t.Errorf("role is %q, want dev", req.Role)
	}
}

// fakeCommitter turns the workspace into a commit.
type fakeCommitter struct {
	shas     []string
	messages []string
	err      error
}

func (f *fakeCommitter) Commit(_ context.Context, _, message string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.messages = append(f.messages, message)
	sha := "sha-" + message
	f.shas = append(f.shas, sha)
	return sha, nil
}

// fakeReviewer replies with a scripted verdict per round.
type fakeReviewer struct {
	verdicts []string
	findings string
	round    int
	submits  int
	settled  []string
}

func (f *fakeReviewer) Submit(_ context.Context, run, roundID, commit string) (reviewbridge.Round, error) {
	f.submits++
	return reviewbridge.Round{ID: roundID, Run: run, Commit: commit, JobID: f.submits}, nil
}

func (f *fakeReviewer) Poll(_ context.Context, r reviewbridge.Round) (reviewbridge.Round, error) {
	verdict := reviewbridge.VerdictClean
	if f.round < len(f.verdicts) {
		verdict = f.verdicts[f.round]
	}
	f.round++
	r.Verdict = verdict
	if verdict == reviewbridge.VerdictFindings {
		r.Findings = f.findings
	}
	return r, nil
}

func (f *fakeReviewer) Settle(_ context.Context, r reviewbridge.Round) error {
	f.settled = append(f.settled, r.ID)
	return nil
}

func pairLoop(store *memStore, ag *fakes.Agent, c *fakeCommitter, rev *fakeReviewer, max int) devloop.Loop {
	return devloop.Loop{
		Agent: ag, Store: store, Commit: c, Review: rev,
		Now: fakes.NewClock(time.Unix(0, 0)), MaxRounds: max,
	}
}

func TestACleanFirstReviewEndsTheLoop(t *testing.T) {
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	c, rev := &fakeCommitter{}, &fakeReviewer{verdicts: []string{reviewbridge.VerdictClean}}
	got, err := pairLoop(store, ag, c, rev, 5).Work(context.Background(), request())
	if err != nil {
		t.Fatalf("work: %v", err)
	}
	if got.Rounds != 1 {
		t.Errorf("ran %d rounds, want 1", got.Rounds)
	}
	if len(c.shas) != 1 {
		t.Errorf("made %d commits, want 1", len(c.shas))
	}
	if len(rev.settled) != 1 {
		t.Errorf("closed %d jobs, want the 1 kriya opened", len(rev.settled))
	}
}

func TestFindingsGoBackToTheAgentVerbatim(t *testing.T) {
	// The findings ARE the next instruction. Summarising them drops the file
	// and line the fix needs.
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	findings := "- **Severity**: Medium\n- **Location**: `foo.go:42`\n- **Problem**: off by one"
	c := &fakeCommitter{}
	rev := &fakeReviewer{
		verdicts: []string{reviewbridge.VerdictFindings, reviewbridge.VerdictClean},
		findings: findings,
	}
	if _, err := pairLoop(store, ag, c, rev, 5).Work(context.Background(), request()); err != nil {
		t.Fatalf("work: %v", err)
	}
	// The first invocation is the implementation; the second carries findings.
	if len(ag.Requests) < 2 {
		t.Fatalf("the agent was invoked %d times; the fix round never ran", len(ag.Requests))
	}
	if !strings.Contains(ag.Requests[1].Prompt, "foo.go:42") {
		t.Error("the findings did not reach the agent intact")
	}
}

func TestTheLoopRecommitsAfterAFix(t *testing.T) {
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	c := &fakeCommitter{}
	rev := &fakeReviewer{
		verdicts: []string{reviewbridge.VerdictFindings, reviewbridge.VerdictClean},
		findings: "- **Severity**: Low",
	}
	got, err := pairLoop(store, ag, c, rev, 5).Work(context.Background(), request())
	if err != nil {
		t.Fatalf("work: %v", err)
	}
	if len(c.shas) != 2 {
		t.Errorf("made %d commits, want 2 — small commits, not one at the end", len(c.shas))
	}
	if got.Rounds != 2 {
		t.Errorf("recorded %d rounds, want 2", got.Rounds)
	}
}

func TestAReviewThatKeepsFindingThingsStalls(t *testing.T) {
	// An unbounded loop turns "review latency in minutes" into never. The
	// operator must see the stall.
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	c := &fakeCommitter{}
	rev := &fakeReviewer{
		verdicts: []string{
			reviewbridge.VerdictFindings, reviewbridge.VerdictFindings,
			reviewbridge.VerdictFindings, reviewbridge.VerdictFindings,
		},
		findings: "- **Severity**: High",
	}
	if _, err := pairLoop(store, ag, c, rev, 3).Work(context.Background(), request()); err == nil {
		t.Fatal("a review that never goes clean must surface as a stall")
	}
	if len(c.shas) != 3 {
		t.Errorf("made %d commits, want the 3 rounds allowed", len(c.shas))
	}
}

func TestAPendingReviewDoesNotBlockTheLoop(t *testing.T) {
	// A run waiting on a review is a state the TUI shows, not a goroutine
	// nobody can see.
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	c := &fakeCommitter{}
	rev := &fakeReviewer{verdicts: []string{reviewbridge.VerdictPending}}
	if _, err := pairLoop(store, ag, c, rev, 5).Work(context.Background(), request()); err != nil {
		t.Fatalf("work: %v", err)
	}
	if len(ag.Requests) != 1 {
		t.Errorf("the agent ran %d times while a review was still pending", len(ag.Requests))
	}
	if len(rev.settled) != 0 {
		t.Error("a job still running was closed")
	}
}
