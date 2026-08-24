package devloop_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"kriya/internal/architect"
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

func (m *memStore) Find(_ context.Context, run string) (devloop.Session, bool, error) {
	for _, s := range m.rows {
		if s.Run == run {
			return s, true, nil
		}
	}
	return devloop.Session{}, false, nil
}

func (m *memStore) Importing(context.Context) ([]devloop.Session, error) {
	var out []devloop.Session
	for _, s := range m.rows {
		if s.ImportState == devloop.ImportImporting {
			out = append(out, s)
		}
	}
	return out, nil
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
	// submitted records every round id, so two sessions reusing one is
	// visible rather than silently overwriting a job.
	submitted []string
	settled   []string
	responses []string
}

func (f *fakeReviewer) Submit(_ context.Context, run, roundID, commit string) (reviewbridge.Round, error) {
	f.submits++
	f.submitted = append(f.submitted, roundID)
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

func (f *fakeReviewer) Settle(_ context.Context, r reviewbridge.Round, response string) error {
	f.settled = append(f.settled, r.ID)
	f.responses = append(f.responses, response)
	return nil
}

func pairLoop(store *memStore, ag *fakes.Agent, c devloop.Committer, rev *fakeReviewer, max int) devloop.Loop {
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
	// nobody can see. It returns rather than blocking — with ErrReviewPending,
	// which is neither advance nor failure.
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	c := &fakeCommitter{}
	rev := &fakeReviewer{verdicts: []string{reviewbridge.VerdictPending}}
	_, err := pairLoop(store, ag, c, rev, 5).Work(context.Background(), request())
	if !errors.Is(err, devloop.ErrReviewPending) {
		t.Fatalf("work: %v", err)
	}
	if len(ag.Requests) != 1 {
		t.Errorf("the agent ran %d times while a review was still pending", len(ag.Requests))
	}
	if len(rev.settled) != 0 {
		t.Error("a job still running was closed")
	}
}

func TestThePromptStatesTheTicketAndTheBar(t *testing.T) {
	// What the agent is judged by has to be IN the prompt: an agent that never
	// saw the gate chain cannot write to it.
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	req := request()
	req.Body = "the walking skeleton, end to end"
	req.Criteria = []string{"AC-x", "AC-y"}
	req.Commands = map[string]string{"test": "go test ./...", "lint": "golangci-lint run"}
	loop := devloop.Loop{Agent: ag, Store: store, Now: fakes.NewClock(time.Unix(0, 0))}
	if _, err := loop.Work(context.Background(), req); err != nil {
		t.Fatalf("work: %v", err)
	}
	got := ag.Requests[0].Prompt
	for _, want := range []string{
		req.Title, "the walking skeleton, end to end", "AC-x, AC-y",
		"Write the test first", "go test ./...", "golangci-lint run",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the prompt omits %q:\n%s", want, got)
		}
	}
}

func TestATicketWithNothingExtraStillProducesAPrompt(t *testing.T) {
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	bare := devloop.Request{Run: "run-1", Ticket: "KRI-1", Title: "Create a short link"}
	loop := devloop.Loop{Agent: ag, Store: store, Now: fakes.NewClock(time.Unix(0, 0))}
	if _, err := loop.Work(context.Background(), bare); err != nil {
		t.Fatalf("work: %v", err)
	}
	got := ag.Requests[0].Prompt
	if strings.Contains(got, "It satisfies:") || strings.Contains(got, "judged by these commands") {
		t.Errorf("the prompt invented sections the ticket did not have:\n%s", got)
	}
}

func TestTheRoundBoundDefaultsRatherThanRunningForever(t *testing.T) {
	// A zero MaxRounds is a caller that did not choose, not a caller asking
	// for an unbounded loop.
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	c := &fakeCommitter{}
	rev := &fakeReviewer{
		verdicts: []string{
			reviewbridge.VerdictFindings, reviewbridge.VerdictFindings,
			reviewbridge.VerdictFindings, reviewbridge.VerdictFindings,
			reviewbridge.VerdictFindings, reviewbridge.VerdictFindings,
			reviewbridge.VerdictFindings, reviewbridge.VerdictFindings,
		},
		findings: "- **Severity**: High",
	}
	if _, err := pairLoop(store, ag, c, rev, 0).Work(context.Background(), request()); err == nil {
		t.Fatal("an unbounded loop ran to completion")
	}
	if len(c.shas) != 5 {
		t.Errorf("ran %d rounds, want the default 5", len(c.shas))
	}
}

func TestACommitFailureStopsTheRound(t *testing.T) {
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	c := &fakeCommitter{err: errors.New("index.lock exists")}
	rev := &fakeReviewer{}
	_, err := pairLoop(store, ag, c, rev, 3).Work(context.Background(), request())
	if err == nil || !strings.Contains(err.Error(), "index.lock") {
		t.Fatalf("got %v, want git's own complaint", err)
	}
	if rev.submits != 0 {
		t.Error("a review was submitted for work that was never committed")
	}
}

func TestTheResponseNamesTheCommitThatAddressedTheFindings(t *testing.T) {
	// "Addressed" with no commit is not a trail. The honest answer names the
	// commit, which is why a findings round settles only after the fix lands.
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	c := &fakeCommitter{}
	rev := &fakeReviewer{
		verdicts: []string{reviewbridge.VerdictFindings, reviewbridge.VerdictClean},
		findings: "- **Severity**: Low",
	}
	if _, err := pairLoop(store, ag, c, rev, 5).Work(context.Background(), request()); err != nil {
		t.Fatalf("work: %v", err)
	}
	if len(rev.responses) != 2 {
		t.Fatalf("settled %d rounds: %v", len(rev.responses), rev.responses)
	}
	if !strings.Contains(rev.responses[0], c.shas[1]) {
		t.Errorf("the findings round was answered %q, which names no fix", rev.responses[0])
	}
	if !strings.Contains(rev.responses[1], c.shas[1]) {
		t.Errorf("the clean round was answered %q", rev.responses[1])
	}
}

func TestAStallLeavesItsLastRoundOpen(t *testing.T) {
	// The unanswered round is the evidence of what was asked for and never
	// addressed, so it must not be closed on the way out.
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	c := &fakeCommitter{}
	rev := &fakeReviewer{
		verdicts: []string{
			reviewbridge.VerdictFindings, reviewbridge.VerdictFindings,
			reviewbridge.VerdictFindings,
		},
		findings: "- **Severity**: High",
	}
	if _, err := pairLoop(store, ag, c, rev, 2).Work(context.Background(), request()); err == nil {
		t.Fatal("a review that never went clean read as success")
	}
	// Round one was answered by round two's commit; round two never was.
	if len(rev.settled) != 1 || rev.settled[0] != "run-1-0-1-1" {
		t.Errorf("settled %v, want only the round a later commit answered", rev.settled)
	}
}

func TestRoundsAreNumberedFromOne(t *testing.T) {
	// The round number is what the operator matches a commit to a review by.
	// Off by one and the trail points at the wrong round.
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
	if len(c.messages) != 2 {
		t.Fatalf("made %d commits", len(c.messages))
	}
	for i, want := range []string{"(round 1)", "(round 2)"} {
		if !strings.HasSuffix(c.messages[i], want) {
			t.Errorf("commit %d is %q, want it to end %q", i, c.messages[i], want)
		}
	}
	if rev.settled[0] != "run-1-0-1-1" {
		t.Errorf("the first round is %q", rev.settled[0])
	}
	if got.Rounds != 2 {
		t.Errorf("recorded %d rounds", got.Rounds)
	}
}

func TestAFailureNamesTheRoundItHappenedIn(t *testing.T) {
	// "commit failed" without a round leaves the operator counting commits to
	// work out where it stopped.
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	c := &failOnRound{n: 2}
	rev := &fakeReviewer{
		verdicts: []string{reviewbridge.VerdictFindings, reviewbridge.VerdictFindings},
		findings: "- **Severity**: Low",
	}
	_, err := pairLoop(store, ag, c, rev, 5).Work(context.Background(), request())
	if err == nil || !strings.Contains(err.Error(), "commit round 2") {
		t.Fatalf("got %v, want the round named", err)
	}
}

func TestAnAgentFailureNamesItsRound(t *testing.T) {
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply(), Err: nil}
	c := &fakeCommitter{}
	rev := &fakeReviewer{
		verdicts: []string{reviewbridge.VerdictFindings},
		findings: "- **Severity**: Low",
	}
	// One reply, no repeat: the fix invocation has nothing to return.
	_, err := pairLoop(store, ag, c, rev, 5).Work(context.Background(), request())
	if err == nil || !strings.Contains(err.Error(), "round 1") {
		t.Fatalf("got %v, want the round named", err)
	}
}

// failOnRound commits until round n, then fails.
type failOnRound struct {
	n     int
	calls int
}

func (f *failOnRound) Commit(context.Context, string, string) (string, error) {
	f.calls++
	if f.calls >= f.n {
		return "", errors.New("index.lock exists")
	}
	return "sha-" + strconv.Itoa(f.calls), nil
}

// fakeArchitect records impasses and can hand back a direction.
type fakeArchitect struct {
	resolved  []architect.Intervention
	direction string
	resumeErr error
	resumed   int
	delivered int
}

func (f *fakeArchitect) Resolve(
	_ context.Context, build, ticket, trigger string, findings []string, _ string,
) (architect.Intervention, error) {
	history, err := json.Marshal(findings)
	if err != nil {
		return architect.Intervention{}, err
	}
	in := architect.Intervention{
		Build: build, Trigger: trigger, Findings: history,
		Direction: f.direction, State: architect.StateDirected,
	}
	f.resolved = append(f.resolved, in)
	return in, nil
}

func (f *fakeArchitect) Resume(context.Context, string) (architect.Intervention, bool, error) {
	f.resumed++
	if f.resumeErr != nil {
		return architect.Intervention{}, false, f.resumeErr
	}
	if f.direction == "" {
		return architect.Intervention{}, false, nil
	}
	return architect.Intervention{Direction: f.direction, State: architect.StateDirected}, true, nil
}

func (f *fakeArchitect) MarkResumed(context.Context, string) error {
	f.delivered++
	return nil
}

func findingsLoop(store *memStore, ag *fakes.Agent, c *fakeCommitter, rev *fakeReviewer,
	sa *fakeArchitect, limit int) devloop.Loop {
	loop := pairLoop(store, ag, c, rev, 0)
	loop.Architect = sa
	loop.MaxRounds = limit
	return loop
}

func findingRounds(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = reviewbridge.VerdictFindings
	}
	return out
}

func TestTheImpasseReachesTheArchitectAtTheRoundLimit(t *testing.T) {
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	c := &fakeCommitter{}
	rev := &fakeReviewer{verdicts: findingRounds(6), findings: "- **Severity**: High"}
	sa := &fakeArchitect{}
	_, err := findingsLoop(store, ag, c, rev, sa, 4).Work(context.Background(), request())
	if err == nil {
		t.Fatal("a run that hit the round limit read as success")
	}
	if len(sa.resolved) != 1 {
		t.Fatalf("the architect was asked %d times", len(sa.resolved))
	}
	if sa.resolved[0].Trigger != architect.TriggerRoundLimit {
		t.Errorf("the trigger is %q", sa.resolved[0].Trigger)
	}
	var handed []string
	if err := json.Unmarshal(sa.resolved[0].Findings, &handed); err != nil {
		t.Fatalf("the findings history is unreadable: %v", err)
	}
	if len(handed) != 4 {
		t.Errorf("handed over %d findings for a limit of 4", len(handed))
	}
}

func TestBelowTheLimitTheLoopDoesNotPause(t *testing.T) {
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	c := &fakeCommitter{}
	rev := &fakeReviewer{
		verdicts: []string{
			reviewbridge.VerdictFindings, reviewbridge.VerdictFindings,
			reviewbridge.VerdictClean,
		},
		findings: "- **Severity**: Low",
	}
	sa := &fakeArchitect{}
	if _, err := findingsLoop(store, ag, c, rev, sa, 4).Work(context.Background(), request()); err != nil {
		t.Fatalf("work: %v", err)
	}
	if len(sa.resolved) != 0 {
		t.Error("two consecutive findings rounds paused the run")
	}
}

func TestACleanPassResetsTheCounter(t *testing.T) {
	// Consecutive rounds, not rounds: a clean pass mid-count means the loop is
	// converging, and counting through it would pause a run that is working.
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	c := &fakeCommitter{}
	rev := &fakeReviewer{
		verdicts: []string{
			reviewbridge.VerdictFindings, reviewbridge.VerdictFindings,
			reviewbridge.VerdictClean,
		},
		findings: "- **Severity**: Low",
	}
	sa := &fakeArchitect{}
	got, err := findingsLoop(store, ag, c, rev, sa, 3).Work(context.Background(), request())
	if err != nil {
		t.Fatalf("work: %v", err)
	}
	if len(sa.resolved) != 0 {
		t.Error("a run that reached a clean pass was sent to the architect")
	}
	if got.Rounds != 3 {
		t.Errorf("ran %d rounds", got.Rounds)
	}
}

func TestTheSnapshottedLimitGovernsTheRun(t *testing.T) {
	// The limit the run was CREATED under, not the one configured now: a
	// config change affects only future runs.
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	c := &fakeCommitter{}
	rev := &fakeReviewer{verdicts: findingRounds(8), findings: "- **Severity**: High"}
	sa := &fakeArchitect{}
	// The loop's default has since been lowered to 2; the run carries 4.
	req := request()
	req.RoundLimit = 4
	if _, err := findingsLoop(store, ag, c, rev, sa, 2).Work(context.Background(), req); err == nil {
		t.Fatal("expected the impasse")
	}
	if len(c.shas) != 4 {
		t.Errorf("ran %d rounds, want the 4 the run was created under", len(c.shas))
	}
}

func TestAnArchitectDirectionReachesTheDevAgent(t *testing.T) {
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	c := &fakeCommitter{}
	rev := &fakeReviewer{
		verdicts: []string{reviewbridge.VerdictFindings, reviewbridge.VerdictClean},
		findings: "- **Severity**: Low",
	}
	sa := &fakeArchitect{direction: "Split the handler; the store is not the problem."}
	if _, err := findingsLoop(store, ag, c, rev, sa, 4).Work(context.Background(), request()); err != nil {
		t.Fatalf("work: %v", err)
	}
	if sa.resumed == 0 {
		t.Fatal("the loop never looked for a recorded direction")
	}
	if len(ag.Requests) < 2 {
		t.Fatalf("the agent was invoked %d times", len(ag.Requests))
	}
	if !strings.Contains(ag.Requests[1].Prompt, "Split the handler") {
		t.Errorf("the direction did not reach the agent:\n%s", ag.Requests[1].Prompt)
	}
}

func TestAResumeFailureStopsTheLoop(t *testing.T) {
	// Running on without a recorded direction would repeat the impasse the
	// architect already answered.
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	c := &fakeCommitter{}
	rev := &fakeReviewer{verdicts: []string{reviewbridge.VerdictClean}}
	sa := &fakeArchitect{resumeErr: errors.New("store unavailable")}
	if _, err := findingsLoop(store, ag, c, rev, sa, 4).Work(context.Background(), request()); err == nil {
		t.Fatal("an unreadable intervention read as no intervention")
	}
}

func TestEveryFixRoundResumesTheSameSession(t *testing.T) {
	// The fix is a correction to work THIS agent did. A fresh session re-reads
	// its own findings with no memory of what it wrote, and the transcript
	// splits across session ids so only the last one reaches the catalog.
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	c := &fakeCommitter{}
	rev := &fakeReviewer{
		verdicts: []string{reviewbridge.VerdictFindings, reviewbridge.VerdictClean},
		findings: "- **Severity**: Low",
	}
	if _, err := pairLoop(store, ag, c, rev, 5).Work(context.Background(), request()); err != nil {
		t.Fatalf("work: %v", err)
	}
	if len(ag.Requests) < 2 {
		t.Fatalf("the agent ran %d times; there was no fix round", len(ag.Requests))
	}
	if ag.Requests[0].Resume != "" {
		t.Error("the first invocation asked to resume something")
	}
	for i, req := range ag.Requests[1:] {
		if req.Resume == "" {
			t.Errorf("fix round %d started a fresh session", i+1)
		}
	}
}

func TestASecondPassOverTheSameRunDoesNotOverwriteItsRounds(t *testing.T) {
	// A run re-enters the dev loop after a gate failure or a rework. Round ids
	// that restart at 1 upsert over the first pass's rows, replacing its job
	// ids and verdicts while keeping its commit — a review trail that points
	// at the wrong work.
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	rev := &fakeReviewer{verdicts: []string{reviewbridge.VerdictClean}, findings: ""}
	loop := pairLoop(store, ag, &fakeCommitter{}, rev, 5)

	first := request()
	first.Attempt = 1
	if _, err := loop.Work(context.Background(), first); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	second := request()
	second.Attempt = 2
	rev.verdicts = []string{reviewbridge.VerdictClean}
	rev.round = 0
	if _, err := loop.Work(context.Background(), second); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(rev.settled) != 2 {
		t.Fatalf("settled %v", rev.settled)
	}
	if rev.settled[0] == rev.settled[1] {
		t.Errorf("both passes used round id %q", rev.settled[0])
	}
}

func TestAPendingReviewIsNotACompletedSession(t *testing.T) {
	// It reported success: the session was stamped ended and the orchestrator
	// advanced the run to the gates, with a review job still running and
	// nothing that would ever read its verdict.
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	rev := &fakeReviewer{verdicts: []string{reviewbridge.VerdictPending}}
	_, err := pairLoop(store, ag, &fakeCommitter{}, rev, 5).
		Work(context.Background(), request())
	if !errors.Is(err, devloop.ErrReviewPending) {
		t.Fatalf("got %v, want ErrReviewPending", err)
	}
	for _, row := range store.rows {
		if row.Run == "run-1" && row.Ended {
			t.Error("the session was stamped ended with its review still running")
		}
	}
}

func TestASuccessfulArchitectHandoffIsNotAMalfunction(t *testing.T) {
	// It returned a plain error, which the table reads as a failed dev-loop
	// stage and parks the run in awaiting-operator — terminal, with no path
	// back. The direction the architect just recorded could never be
	// consumed by anything.
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	rev := &fakeReviewer{
		verdicts: []string{reviewbridge.VerdictFindings, reviewbridge.VerdictFindings},
		findings: "- **Severity**: High",
	}
	loop := pairLoop(store, ag, &fakeCommitter{}, rev, 2)
	sa := &recordingArchitect{direction: "extract the port"}
	loop.Architect = sa

	_, err := loop.Work(context.Background(), request())
	if !errors.Is(err, devloop.ErrArchitectDirected) {
		t.Fatalf("got %v, want ErrArchitectDirected", err)
	}
	if sa.resolved != 1 {
		t.Errorf("the architect was asked %d times", sa.resolved)
	}
}

func TestAnArchitectFailureIsStillAFailure(t *testing.T) {
	// The control: "the architect could not be reached" is not "the architect
	// gave direction", and must not read as a run that may continue.
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	rev := &fakeReviewer{
		verdicts: []string{reviewbridge.VerdictFindings, reviewbridge.VerdictFindings},
		findings: "- **Severity**: High",
	}
	loop := pairLoop(store, ag, &fakeCommitter{}, rev, 2)
	loop.Architect = &recordingArchitect{err: errors.New("architect unreachable")}

	_, err := loop.Work(context.Background(), request())
	if err == nil || errors.Is(err, devloop.ErrArchitectDirected) {
		t.Fatalf("got %v, want a plain failure", err)
	}
}

// recordingArchitect stands in for the SA.
type recordingArchitect struct {
	direction string
	err       error
	resolved  int
	delivered int
	resume    architect.Intervention
	found     bool
}

func (a *recordingArchitect) Resolve(
	_ context.Context, build, _, trigger string, findings []string, _ string,
) (architect.Intervention, error) {
	a.resolved++
	if a.err != nil {
		return architect.Intervention{}, a.err
	}
	return architect.Intervention{
		Build: build, Trigger: trigger, Direction: a.direction,
		State: architect.StateDirected,
	}, nil
}

func (a *recordingArchitect) Resume(
	context.Context, string,
) (architect.Intervention, bool, error) {
	return a.resume, a.found, nil
}

func (a *recordingArchitect) MarkResumed(context.Context, string) error {
	a.delivered++
	a.found = false
	return nil
}

func TestASessionThatFailedMidLoopStillReachesTheCatalog(t *testing.T) {
	// AC-thread-no-loss: every outcome reaches the thread catalog, crashed
	// included. Capture ran only after the whole loop succeeded, so a commit,
	// agent or review failure left ImportState "none" with no transcript ref
	// — and RecoverImports skips exactly those rows, so the conversation was
	// nowhere, forever.
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	rev := &fakeReviewer{
		verdicts: []string{reviewbridge.VerdictFindings}, findings: "- **Severity**: High",
	}
	loop := pairLoop(store, ag, &failingCommitter{after: 1}, rev, 5)
	threads := &recordingThreads{}
	loop.Threads = threads
	req := request()
	req.Workspace = t.TempDir()

	if _, err := loop.Work(context.Background(), req); err == nil {
		t.Fatal("a failed commit read as a completed session")
	}
	if len(threads.imported) != 1 {
		t.Fatalf("imported %d transcripts for a crashed session", len(threads.imported))
	}
	if !strings.Contains(threads.imported[0], "Implement this ticket") {
		t.Errorf("the imported transcript is %q", threads.imported[0])
	}
}

func TestASessionStillRunningIsNotImportedYet(t *testing.T) {
	// The control. A pending review or an architect handoff is not an
	// outcome: the session continues, and importing a partial transcript
	// under the session's own key would return that thread for every later
	// import and lose everything after it.
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	rev := &fakeReviewer{verdicts: []string{reviewbridge.VerdictPending}}
	loop := pairLoop(store, ag, &fakeCommitter{}, rev, 5)
	threads := &recordingThreads{}
	loop.Threads = threads
	req := request()
	req.Workspace = t.TempDir()

	if _, err := loop.Work(context.Background(), req); !errors.Is(err, devloop.ErrReviewPending) {
		t.Fatalf("got %v", err)
	}
	if len(threads.imported) != 0 {
		t.Errorf("a session still running was imported: %d", len(threads.imported))
	}
}

// failingCommitter commits successfully n times, then refuses.
type failingCommitter struct {
	after int
	made  int
}

func (c *failingCommitter) Commit(_ context.Context, _, _ string) (string, error) {
	c.made++
	if c.made > c.after {
		return "", errors.New("the worktree is gone")
	}
	return fmt.Sprintf("sha-%d", c.made), nil
}

// recordingThreads keeps every transcript handed to the catalog.
type recordingThreads struct{ imported []string }

func (r *recordingThreads) Import(
	_ context.Context, _ string, transcript json.RawMessage, _, _, _, _ string,
) (string, error) {
	r.imported = append(r.imported, string(transcript))
	return fmt.Sprintf("thread-%d", len(r.imported)), nil
}

func TestARecordedDirectionReachesTheFirstPrompt(t *testing.T) {
	// The architect answered on the previous pass. Its direction reached only
	// the FIX prompt, so a pass whose first review came back clean never gave
	// the agent the direction at all — the impasse it was asked about was
	// resolved by nobody, and the intervention stayed open forever.
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	rev := &fakeReviewer{verdicts: []string{reviewbridge.VerdictClean}}
	loop := pairLoop(store, ag, &fakeCommitter{}, rev, 5)
	loop.Architect = &recordingArchitect{
		found:  true,
		resume: architect.Intervention{Direction: "extract the port boundary"},
	}
	if _, err := loop.Work(context.Background(), request()); err != nil {
		t.Fatalf("work: %v", err)
	}
	if len(ag.Requests) == 0 {
		t.Fatal("the agent never ran")
	}
	if !strings.Contains(ag.Requests[0].Prompt, "extract the port boundary") {
		t.Errorf("the first prompt does not carry the direction:\n%s", ag.Requests[0].Prompt)
	}
}

func TestWithNoDirectionTheFirstPromptSaysNothingAboutOne(t *testing.T) {
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	rev := &fakeReviewer{verdicts: []string{reviewbridge.VerdictClean}}
	loop := pairLoop(store, ag, &fakeCommitter{}, rev, 5)
	if _, err := loop.Work(context.Background(), request()); err != nil {
		t.Fatalf("work: %v", err)
	}
	if strings.Contains(ag.Requests[0].Prompt, "architect") {
		t.Errorf("a run with no direction was told about one:\n%s", ag.Requests[0].Prompt)
	}
}

func TestAPassThatCameBackResumesItsSession(t *testing.T) {
	// A pending review and an architect handoff both come back. Nothing was
	// persisted on the way out, so the next pass started a fresh agent
	// session, made another commit, and submitted the same code again under a
	// new id — the previous conversation and its round trail lost.
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	c := &fakeCommitter{}
	rev := &fakeReviewer{verdicts: []string{reviewbridge.VerdictPending}}
	loop := pairLoop(store, ag, c, rev, 5)

	if _, err := loop.Work(context.Background(), request()); !errors.Is(err, devloop.ErrReviewPending) {
		t.Fatalf("first pass: %v", err)
	}
	saved, found := sessionFor(store, "run-1")
	if !found || saved.SessionID == "" {
		t.Fatalf("the session was not persisted on the way out: %+v", saved)
	}
	firstID, firstCommits := saved.SessionID, len(saved.Commits)
	invocations := len(ag.Requests)

	// The second pass. The review has finished this time.
	rev.round, rev.verdicts = 0, []string{reviewbridge.VerdictClean}
	if _, err := loop.Work(context.Background(), request()); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(ag.Requests) != invocations {
		t.Errorf("the second pass invoked the agent again: %d then %d",
			invocations, len(ag.Requests))
	}
	got, _ := sessionFor(store, "run-1")
	if got.SessionID != firstID {
		t.Errorf("the session id changed from %q to %q", firstID, got.SessionID)
	}
	if len(c.messages) <= firstCommits {
		t.Log("the resumed pass committed again, which a new round does")
	}
}

func TestAFinishedSessionIsNotResumed(t *testing.T) {
	// The control. A run that settled and comes back — after a gate failure
	// or a rework — is new work, and resuming the finished conversation would
	// hand the agent a session that already reported itself done.
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	rev := &fakeReviewer{verdicts: []string{reviewbridge.VerdictClean}}
	loop := pairLoop(store, ag, &fakeCommitter{}, rev, 5)

	if _, err := loop.Work(context.Background(), request()); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	invocations := len(ag.Requests)
	rev.round = 0
	second := request()
	second.Attempt = 2
	if _, err := loop.Work(context.Background(), second); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(ag.Requests) == invocations {
		t.Error("a settled session was resumed instead of starting new work")
	}
}

func sessionFor(store *memStore, run string) (devloop.Session, bool) {
	got, found, _ := store.Find(context.Background(), run)
	return got, found
}

func TestAResumedPassPollsItsPendingRoundInsteadOfCommittingAgain(t *testing.T) {
	// The previous pass left a review job running. Committing again and
	// submitting under the same round id would replace the stored job and
	// orphan the review the run is actually waiting for.
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	c := &fakeCommitter{}
	rev := &fakeReviewer{verdicts: []string{reviewbridge.VerdictPending}}
	loop := pairLoop(store, ag, c, rev, 5)

	if _, err := loop.Work(context.Background(), request()); !errors.Is(err, devloop.ErrReviewPending) {
		t.Fatalf("first pass: %v", err)
	}
	commits, submits := len(c.messages), rev.submits
	saved, _ := sessionFor(store, "run-1")
	if saved.Pending.ID == "" {
		t.Fatalf("the round being waited on was not recorded: %+v", saved.Pending)
	}
	if len(saved.Turns) == 0 {
		t.Error("the conversation so far was not persisted")
	}

	// It has finished this time.
	rev.round, rev.verdicts = 0, []string{reviewbridge.VerdictClean}
	if _, err := loop.Work(context.Background(), request()); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(c.messages) != commits {
		t.Errorf("the resumed pass committed again: %d then %d", commits, len(c.messages))
	}
	if rev.submits != submits {
		t.Errorf("the resumed pass submitted a second job: %d then %d", submits, rev.submits)
	}
	if len(rev.settled) != 1 || rev.settled[0] != "run-1-0-1-1" {
		t.Errorf("settled %v, want the round that was pending", rev.settled)
	}
	got, _ := sessionFor(store, "run-1")
	if got.Pending.ID != "" {
		t.Errorf("the round is still marked pending: %+v", got.Pending)
	}
}

func TestAResumedPassContinuesTheRoundNumbering(t *testing.T) {
	// A resumed pass that begins at round zero reuses the round ids the
	// previous pass already recorded, replacing their jobs and verdicts.
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	rev := &fakeReviewer{
		verdicts: []string{reviewbridge.VerdictPending}, findings: "- **Severity**: High",
	}
	loop := pairLoop(store, ag, &fakeCommitter{}, rev, 5)

	if _, err := loop.Work(context.Background(), request()); !errors.Is(err, devloop.ErrReviewPending) {
		t.Fatalf("first pass: %v", err)
	}
	// The pending round comes back with findings, so the loop fixes and runs
	// a SECOND round — which must not reuse the first round's id.
	rev.round = 0
	rev.verdicts = []string{reviewbridge.VerdictFindings, reviewbridge.VerdictClean}
	if _, err := loop.Work(context.Background(), request()); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(rev.settled) != 2 {
		t.Fatalf("settled %v", rev.settled)
	}
	if rev.settled[0] == rev.settled[1] {
		t.Errorf("the resumed round reused id %q", rev.settled[0])
	}
	if rev.settled[1] != "run-1-0-1-2" {
		t.Errorf("the resumed round is %q, want the second", rev.settled[1])
	}
}

func TestAResumedPassKeepsEveryTurnItHasHad(t *testing.T) {
	// Turns appended during the pair loop lived only in memory, so a pass that
	// came back after a fix round lost the correction exchange entirely.
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	rev := &fakeReviewer{
		verdicts: []string{reviewbridge.VerdictFindings, reviewbridge.VerdictPending},
		findings: "- **Severity**: High",
	}
	loop := pairLoop(store, ag, &fakeCommitter{}, rev, 5)
	if _, err := loop.Work(context.Background(), request()); !errors.Is(err, devloop.ErrReviewPending) {
		t.Fatalf("work: %v", err)
	}
	saved, _ := sessionFor(store, "run-1")
	if len(saved.Turns) != 2 {
		t.Fatalf("persisted %d turns, want the implement prompt and the fix", len(saved.Turns))
	}
	if !strings.Contains(saved.Turns[1].Prompt, "Severity") {
		t.Errorf("the fix exchange is not the one persisted: %q", saved.Turns[1].Prompt)
	}
}

func TestAResumedPassKeepsTheWholeConversation(t *testing.T) {
	// Turns held only in memory would leave the eventual import carrying only
	// what happened after the last resume — often nothing at all, for a
	// pending-then-clean review.
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	rev := &fakeReviewer{verdicts: []string{reviewbridge.VerdictPending}}
	loop := pairLoop(store, ag, &fakeCommitter{}, rev, 5)
	threads := &recordingThreads{}
	loop.Threads = threads
	req := request()
	req.Workspace = t.TempDir()

	if _, err := loop.Work(context.Background(), req); !errors.Is(err, devloop.ErrReviewPending) {
		t.Fatalf("first pass: %v", err)
	}
	rev.round, rev.verdicts = 0, []string{reviewbridge.VerdictClean}
	if _, err := loop.Work(context.Background(), req); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(threads.imported) != 1 {
		t.Fatalf("imported %d transcripts", len(threads.imported))
	}
	if !strings.Contains(threads.imported[0], "Implement this ticket") {
		t.Errorf("the original conversation is missing from the import:\n%s",
			threads.imported[0])
	}
}

func TestAnInterventionClosesOnDeliveryNotOnLookup(t *testing.T) {
	// A pass that read the direction and never reached a prompt closed the
	// intervention anyway, so the direction the impasse was raised to get was
	// given to nobody and could never be given again.
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	rev := &fakeReviewer{verdicts: []string{reviewbridge.VerdictClean}}
	loop := pairLoop(store, ag, &fakeCommitter{}, rev, 5)
	sa := &recordingArchitect{
		found:  true,
		resume: architect.Intervention{Direction: "extract the port boundary"},
	}
	loop.Architect = sa

	if _, err := loop.Work(context.Background(), request()); err != nil {
		t.Fatalf("work: %v", err)
	}
	if sa.delivered != 1 {
		t.Errorf("the intervention was closed %d times", sa.delivered)
	}
	if !strings.Contains(ag.Requests[0].Prompt, "extract the port boundary") {
		t.Error("it was closed without the direction reaching a prompt")
	}
}

func TestNoDirectionClosesNothing(t *testing.T) {
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	rev := &fakeReviewer{verdicts: []string{reviewbridge.VerdictClean}}
	loop := pairLoop(store, ag, &fakeCommitter{}, rev, 5)
	sa := &recordingArchitect{}
	loop.Architect = sa
	if _, err := loop.Work(context.Background(), request()); err != nil {
		t.Fatalf("work: %v", err)
	}
	if sa.delivered != 0 {
		t.Errorf("an intervention that does not exist was closed %d times", sa.delivered)
	}
}

func TestAfterAnArchitectHandoffTheNextPassStartsFresh(t *testing.T) {
	// The impasse spends the session's rounds. Resuming it would leave no
	// budget at all — the loop would do nothing and report a clean pass. The
	// next pass opens a NEW session whose first prompt carries the direction.
	store := &memStore{}
	ag := &fakes.Agent{Replies: devReply(), Repeat: true}
	rev := &fakeReviewer{
		verdicts: []string{reviewbridge.VerdictFindings, reviewbridge.VerdictFindings},
		findings: "- **Severity**: High",
	}
	loop := pairLoop(store, ag, &fakeCommitter{}, rev, 2)
	sa := &recordingArchitect{direction: "extract the port boundary"}
	loop.Architect = sa

	if _, err := loop.Work(context.Background(), request()); !errors.Is(err, devloop.ErrArchitectDirected) {
		t.Fatalf("first pass: %v", err)
	}
	ended, _ := sessionFor(store, "run-1")
	if !ended.Ended {
		t.Error("the handed-off session was left open, so the next pass resumes it")
	}
	invocations := len(ag.Requests)

	// The next pass. The architect's direction is now recorded.
	sa.found = true
	sa.resume = architect.Intervention{Direction: sa.direction}
	rev.round, rev.verdicts = 0, []string{reviewbridge.VerdictClean}
	if _, err := loop.Work(context.Background(), request()); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(ag.Requests) <= invocations {
		t.Fatal("the second pass invoked nothing: it resumed a spent session")
	}
	if !strings.Contains(ag.Requests[invocations].Prompt, "extract the port boundary") {
		t.Errorf("the fresh session's first prompt lacks the direction:\n%s",
			ag.Requests[invocations].Prompt)
	}
}
