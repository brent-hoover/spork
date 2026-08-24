package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"encoding/json"
	"errors"
	"os/exec"

	"kriya/internal/agent"
	"kriya/internal/clock"
	"kriya/internal/devloop"
	"kriya/internal/fakes"
	"kriya/internal/gates"
	"kriya/internal/orchestrator"
	"kriya/internal/owner"
	"kriya/internal/planner"
	"kriya/internal/specverify"
	"kriya/internal/workspace"
)

// passingCommands is a gate chain that succeeds without running a build.
func passingCommands(string) map[string]string {
	cmds := map[string]string{}
	for _, name := range []string{"test", "lint", "typecheck", "arch", "coverage", "mutation"} {
		cmds[name] = "true"
	}
	return cmds
}

func stagesForTest(t *testing.T, loop worker, commandsFor func(string) map[string]string) (orchestrator.Stages, workspace.Manager) {
	stages, ws, _ := stagesAndReviews(t, loop, commandsFor)
	return stages, ws
}

// stagesAndReviews is stagesForTest, plus the review recorder, for the tests
// that need to see what a submission carried.
func stagesAndReviews(
	t *testing.T, loop worker, commandsFor func(string) map[string]string,
) (orchestrator.Stages, workspace.Manager, *recordingReviews) {
	t.Helper()
	db := openTemp(t)
	if err := applyMigrations(context.Background(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Every run these stages drive has a session: the submit stage stamps the
	// review with it so feedback routes back to the agent that wrote the code.
	for _, run := range []string{"run-1", "run-2", "run-3", "run-po-1", "run-po-2",
		"run-sub-1", "run-sub-2", "run-sub-3", "run-sub-4", "run-merge-1", "run-complete-1", "run-nowhere"} {
		if err := (devloop.SQLStore{DB: db}).Upsert(context.Background(), devloop.Session{
			Run: run, Ticket: "T-1", SessionID: "sess-" + run,
		}); err != nil {
			t.Fatalf("seed session: %v", err)
		}
	}
	ws := workspaceManagerOn(t, db)
	runner := gates.Runner{Store: gates.SQLStore{DB: db}, Now: clock.System{}}
	reviews := &recordingReviews{}
	return buildStages(deps{
		ws: ws, loop: loop, runner: runner, snap: testSnapshot(commandsFor),
		po: owner.Owner{
			Agent: &fakes.Agent{Replies: poReply(owner.VerdictSatisfied)},
			Store: owner.SQLStore{DB: db}, Gates: runner, Now: clock.System{},
		},
		submitter: orchestrator.Submitter{
			Store:   orchestrator.SQLStore{DB: db},
			Reviews: reviews,
			Author:  "actor-1",
		},
		queue:       queueForTest(t, db),
		completer:   completerForTest(t, db),
		commandsFor: commandsFor,
		ticketFor: func(title string) planner.Ticket {
			return planner.Ticket{
				Title: title, Body: "Accept a URL and return a code.",
				Criteria: []string{"AC-x"}, IssueID: "issue-1",
			}
		},
		actor:      "actor-1",
		sessionFor: sessionFromStore(db),
	}), ws, reviews
}

// testSnapshot is a one-module pinned snapshot.
//
// A real module id, not the ticket title: the gate chain runs once per touched
// module and records its results under the module's own name.
func testSnapshot(commandsFor func(string) map[string]string) planner.Snapshot {
	return planner.Snapshot{
		Law:              []specverify.Module{{ID: "MOD-api"}},
		ResolvedCommands: map[string]map[string]string{"MOD-api": commandsFor("MOD-api")},
	}
}

// recordingReviews stands in for sutra's review API, keeping what each
// submission carried so a test can see what was actually asked for.
type recordingReviews struct{ issues, branches []string }

func (r *recordingReviews) Create(
	_ context.Context, issue, _, _, branch, commit, _, _, _, key string,
) (string, error) {
	r.issues = append(r.issues, issue)
	r.branches = append(r.branches, branch)
	return "review-" + key[:8] + "-" + commit, nil
}

func (r *recordingReviews) Resubmit(
	_ context.Context, _, _, _, _, _, _ string, expectedRevision int, _, _, _, _ string,
) (int, error) {
	return expectedRevision + 1, nil
}

// quietLoop invokes the agent once and commits nothing. A run driven through
// it has no head, which is what a run whose dev session produced nothing
// genuinely is.
func quietLoop() devloop.Loop {
	return devloop.Loop{
		Agent: &fakes.Agent{Repeat: true},
		Store: devloop.SQLStore{},
		Now:   fakes.NewClock(time.Unix(0, 0)),
	}
}

func TestTheWorkspaceStageRecordsWhatTheGatesWillRunAt(t *testing.T) {
	// A gate result from any other commit never satisfies, so the base the
	// workspace was cut from has to travel on the run.
	stages, _ := stagesForTest(t, quietLoop(), passingCommands)
	run := orchestrator.BuildRun{ID: "run-1", Ticket: "T-1"}
	got, err := stages[orchestrator.StageWorkspace](context.Background(), run)
	if err != nil {
		t.Fatalf("workspace stage: %v", err)
	}
	if got.GatedBase != "base-sha" {
		t.Errorf("GatedBase is %q — a stale pass could not be recognised", got.GatedBase)
	}
}

func TestTheDevLoopStageRefusesWithoutAWorkspace(t *testing.T) {
	stages, _ := stagesForTest(t, quietLoop(), passingCommands)
	_, err := stages[orchestrator.StageDevLoop](context.Background(),
		orchestrator.BuildRun{ID: "run-missing", Ticket: "T-1"})
	if err == nil || !strings.Contains(err.Error(), "no workspace") {
		t.Fatalf("got %v, want a refusal naming the missing workspace", err)
	}
}

func TestTheGateStageRefusesWithoutAWorkspace(t *testing.T) {
	stages, _ := stagesForTest(t, quietLoop(), passingCommands)
	_, err := stages[orchestrator.StageGates](context.Background(),
		orchestrator.BuildRun{ID: "run-missing", Ticket: "T-1"})
	if err == nil || !strings.Contains(err.Error(), "no workspace") {
		t.Fatalf("got %v, want a refusal naming the missing workspace", err)
	}
}

func TestAPassingChainCountsAnAttempt(t *testing.T) {
	stages, _ := stagesForTest(t, quietLoop(), passingCommands)
	run := orchestrator.BuildRun{ID: "run-2", Ticket: "T-1"}
	run, err := stages[orchestrator.StageWorkspace](context.Background(), run)
	if err != nil {
		t.Fatalf("workspace stage: %v", err)
	}
	got, err := stages[orchestrator.StageGates](context.Background(), run)
	if err != nil {
		t.Fatalf("gate stage: %v", err)
	}
	if got.Attempt != 1 {
		t.Errorf("attempt is %d after one chain", got.Attempt)
	}
}

func TestAFailingGateIsReportedAsAResult(t *testing.T) {
	// The table sends a failing chain back to the dev loop; the findings are
	// the next instruction.
	failing := func(string) map[string]string {
		cmds := passingCommands("")
		cmds["lint"] = "false"
		return cmds
	}
	stages, _ := stagesForTest(t, quietLoop(), failing)
	run, err := stages[orchestrator.StageWorkspace](context.Background(),
		orchestrator.BuildRun{ID: "run-3", Ticket: "T-1"})
	if err != nil {
		t.Fatalf("workspace stage: %v", err)
	}
	got, err := stages[orchestrator.StageGates](context.Background(), run)
	// The chain names the GATE ("structure"), the snapshot names the command
	// ("lint"). The result must carry the gate, which is what the dev loop is
	// told to fix.
	if err == nil || !strings.Contains(err.Error(), "structure") {
		t.Fatalf("got %v, want the failing gate named", err)
	}
	if got.Attempt != 1 {
		t.Errorf("a failed chain must still count as attempt 1, got %d", got.Attempt)
	}
}

func TestCommandsComeFromTheSnapshotByModuleName(t *testing.T) {
	snap := planner.Snapshot{ResolvedCommands: map[string]map[string]string{
		"engine": {"test": "go test ./..."},
		"tui":    {"test": "go test ./tui/..."},
	}}
	if got := commandsFromSnapshot(snap)("tui")["test"]; got != "go test ./tui/..." {
		t.Errorf("resolved %q for tui", got)
	}
}

func TestASingleModuleTargetResolvesByFallback(t *testing.T) {
	// A first build is usually one module, and falling back to the only entry
	// beats failing on a name mismatch.
	snap := planner.Snapshot{ResolvedCommands: map[string]map[string]string{
		"engine": {"test": "go test ./..."},
	}}
	if got := commandsFromSnapshot(snap)("some-ticket-title")["test"]; got != "go test ./..." {
		t.Errorf("resolved %q", got)
	}
}

func TestAmbiguityResolvesToNothingRatherThanAGuess(t *testing.T) {
	snap := planner.Snapshot{ResolvedCommands: map[string]map[string]string{
		"engine": {"test": "a"},
		"tui":    {"test": "b"},
	}}
	if got := commandsFromSnapshot(snap)("neither"); got != nil {
		t.Errorf("guessed %v for a name matching no module", got)
	}
}

// poReply is a structured PO verdict.
func poReply(verdict string) []agent.Result {
	return []agent.Result{{
		SessionID: "po-1", Model: "test-model",
		Structured: json.RawMessage(
			`{"verdict":"` + verdict + `","notes":"every AC has a test that exercises it"}`),
	}}
}

func TestTheProductOwnerRefusesUngatedCode(t *testing.T) {
	// AC-po-position: validation cannot be skipped OR reordered around a gap,
	// so the precondition is the owner's to check rather than the caller's to
	// remember.
	stages, _ := stagesForTest(t, quietLoop(), passingCommands)
	run, err := stages[orchestrator.StageWorkspace](context.Background(),
		orchestrator.BuildRun{ID: "run-po-1", Ticket: "T-1"})
	if err != nil {
		t.Fatalf("workspace stage: %v", err)
	}
	// No gate has run at this commit.
	_, err = stages[orchestrator.StageValidate](context.Background(), run)
	if err == nil {
		t.Fatal("the product owner validated a run with no gate results")
	}
	if !strings.Contains(err.Error(), "test gate has not passed") {
		t.Errorf("got %v, want the earliest missing gate named", err)
	}
}

func TestAPassingProductOwnerAdvancesTheRun(t *testing.T) {
	stages, _ := stagesForTest(t, quietLoop(), passingCommands)
	run, err := stages[orchestrator.StageWorkspace](context.Background(),
		orchestrator.BuildRun{ID: "run-po-2", Ticket: "T-1"})
	if err != nil {
		t.Fatalf("workspace stage: %v", err)
	}
	if run, err = stages[orchestrator.StageGates](context.Background(), run); err != nil {
		t.Fatalf("gate stage: %v", err)
	}
	if _, err := stages[orchestrator.StageValidate](context.Background(), run); err != nil {
		t.Fatalf("validation stage: %v", err)
	}
}

// runThrough drives a run from the workspace stage as far as the named stage.
func runThrough(
	t *testing.T, stages orchestrator.Stages, run orchestrator.BuildRun,
	through ...orchestrator.Stage,
) (orchestrator.BuildRun, error) {
	t.Helper()
	var err error
	for _, stage := range through {
		if run, err = stages[stage](context.Background(), run); err != nil {
			return run, err
		}
	}
	return run, nil
}

func TestASubmissionOpensAReviewForAValidatedRun(t *testing.T) {
	stages, _, reviews := stagesAndReviews(t, quietLoop(), passingCommands)
	got, err := runThrough(t, stages,
		orchestrator.BuildRun{ID: "run-sub-1", Ticket: "T-1", Issue: "issue-7", Head: "C2"},
		orchestrator.StageWorkspace, orchestrator.StageGates,
		orchestrator.StageValidate, orchestrator.StageSubmit)
	if err != nil {
		t.Fatalf("submit stage: %v", err)
	}
	if got.ReviewID == "" || got.ReviewState != orchestrator.SubmitSubmitted {
		t.Errorf("the run records %q in state %q", got.ReviewID, got.ReviewState)
	}
	// The review names the WORK, not the base the branch was cut from.
	if got.ReviewCommit != got.Head {
		t.Errorf("the review pinned %q, not the head %q", got.ReviewCommit, got.Head)
	}
	if got.ReviewCommit == got.GatedBase {
		t.Error("the review pinned the base the branch was cut from")
	}
	// The review is opened against the RUN's issue and its real branch.
	// Recovery replays from the same two fields, so a submission that named
	// anything else would replay as a different request.
	if len(reviews.issues) != 1 || reviews.issues[0] != "issue-7" {
		t.Errorf("the review was opened against %v, not the run's issue", reviews.issues)
	}
	if len(reviews.branches) != 1 || reviews.branches[0] == "" {
		t.Errorf("the review named branch %v", reviews.branches)
	}
	if got.Branch != reviews.branches[0] {
		t.Errorf("the run records branch %q but submitted %q", got.Branch, reviews.branches[0])
	}
}

func TestARunWithAReviewResubmitsRatherThanOpeningASecond(t *testing.T) {
	stages, _ := stagesForTest(t, quietLoop(), passingCommands)
	run, err := runThrough(t, stages,
		orchestrator.BuildRun{ID: "run-sub-2", Ticket: "T-1", Head: "C2"},
		orchestrator.StageWorkspace, orchestrator.StageGates,
		orchestrator.StageValidate, orchestrator.StageSubmit)
	if err != nil {
		t.Fatalf("submit stage: %v", err)
	}
	first := run.ReviewID
	// Rework has a NEW head: a changes-requested verdict sends the run back
	// through the dev loop, which commits. A resubmission at the same head is
	// a replay of the submission that already landed, not rework.
	run.Head = "C3"
	run.ReviewVerdictEvent = "event-9"
	again, err := stages[orchestrator.StageSubmit](context.Background(), run)
	if err != nil {
		t.Fatalf("resubmit stage: %v", err)
	}
	if again.ReviewID != first {
		t.Errorf("rework opened review %q instead of advancing %q", again.ReviewID, first)
	}
	if again.ReviewRevision != run.ReviewRevision+1 {
		t.Errorf("the revision went to %d", again.ReviewRevision)
	}
}

func TestASubmissionNeedsAWorkspace(t *testing.T) {
	stages, _ := stagesForTest(t, quietLoop(), passingCommands)
	for _, stage := range []orchestrator.Stage{
		orchestrator.StageSubmit, orchestrator.StageMerge, orchestrator.StageComplete,
	} {
		_, err := stages[stage](context.Background(),
			orchestrator.BuildRun{ID: "run-nowhere", Ticket: "T-1"})
		if err == nil {
			t.Errorf("the %s stage ran with no workspace", stage)
		}
	}
}

func TestAMergingRunWithNoAttemptIsADefect(t *testing.T) {
	// The attempt is enqueued when the approval is observed. A run in merging
	// with none is a run nothing can advance.
	stages, _ := stagesForTest(t, quietLoop(), passingCommands)
	run, err := stages[orchestrator.StageWorkspace](context.Background(),
		orchestrator.BuildRun{ID: "run-merge-1", Ticket: "T-1"})
	if err != nil {
		t.Fatalf("workspace stage: %v", err)
	}
	_, err = stages[orchestrator.StageMerge](context.Background(), run)
	if err == nil || !strings.Contains(err.Error(), "no attempt") {
		t.Fatalf("got %v, want a refusal naming the missing attempt", err)
	}
}

func TestACompletionChecksTheApprovedCommit(t *testing.T) {
	// The APPROVED commit is what the branch must still point at; anything
	// past it is work nothing reviewed. A real repository, because what the
	// check reads is git.
	db := openTemp(t)
	if err := applyMigrations(context.Background(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	repo := realRepo(t)
	c := orchestrator.Completer{
		Store:    orchestrator.SQLStore{DB: db},
		Tickets:  noTickets{},
		Branches: branchHeads{git: workspace.ShellGit{}, repo: repo},
		Sessions: sessionEnder{db: db},
		Actor:    "actor-1",
	}
	_, err := c.Complete(context.Background(), orchestrator.BuildRun{
		ID: "run-complete-1", Ticket: "T-1", ReviewID: "review-1",
	}, orchestrator.Completion{
		Issue: "issue-7", Branch: "main", Merged: "not-the-branch-head",
	})
	if !errors.Is(err, orchestrator.ErrHeadAdvanced) {
		t.Fatalf("got %v, want ErrHeadAdvanced", err)
	}
}

// realRepo makes a repository with one commit on main.
func realRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "kriya@example.test"},
		{"config", "user.name", "kriya"},
		{"commit", "-q", "--allow-empty", "-m", "root"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

func TestAWaitingMergeLeavesTheRunWhereItIs(t *testing.T) {
	// A run waiting on the merge lock has neither advanced nor failed. Routing
	// it either way would move it somewhere it does not belong — back to the
	// dev loop with its approval still live, or on to merged without merging.
	db := openTemp(t)
	if err := applyMigrations(context.Background(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := orchestrator.SQLStore{DB: db}
	run := orchestrator.BuildRun{
		ID: "run-waiting", Ticket: "T-1", State: orchestrator.StateMerging,
	}
	if err := store.Upsert(context.Background(), run); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	// Two attempts on one resource: the second waits behind the first.
	attempts := orchestrator.SQLAttempts{DB: db}
	for _, key := range []string{"head", "waiting"} {
		if _, err := attempts.Insert(context.Background(), orchestrator.MergeAttempt{
			Key: key, TargetKey: "T", Build: "run-" + key, Resource: "/repo#main",
			State: orchestrator.AttemptQueued,
		}); err != nil {
			t.Fatalf("insert %s: %v", key, err)
		}
	}
	if err := attempts.Update(context.Background(), orchestrator.MergeAttempt{
		Key: "waiting", TargetKey: "T", Build: "run-waiting", Resource: "/repo#main",
		State: orchestrator.AttemptQueued,
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	o := orchestrator.Orchestrator{
		Store: store,
		Stages: orchestrator.Stages{
			orchestrator.StageMerge: mergeStage(orchestrator.Queue{
				Store: attempts, Approvals: noApprovals{},
				Git:      repoMerger{git: workspace.ShellGit{}, repo: t.TempDir(), branch: "main"},
				Resource: "/repo#main", Actor: "actor-1",
			}),
		},
		Now: clock.System{},
	}
	got, err := o.Advance(context.Background(), "run-waiting")
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if got.State != orchestrator.StateMerging {
		t.Errorf("a waiting run moved to %q", got.State)
	}
}

// recordingLoop captures the request the dev-loop stage builds.
//
// The stage is the only place the ticket, the actor and the toolset are
// assembled, so what it hands over is what has to be asserted. A test that
// built a Request itself would prove nothing about production.
type recordingLoop struct{ got devloop.Request }

func (l *recordingLoop) Work(
	_ context.Context, req devloop.Request,
) (devloop.Session, error) {
	l.got = req
	return devloop.Session{Run: req.Run}, nil
}

// captureDevRequest drives the real dev-loop stage and returns what it built.
func captureDevRequest(t *testing.T) devloop.Request {
	t.Helper()
	loop := &recordingLoop{}
	stages, _ := stagesForTest(t, loop, passingCommands)
	run := orchestrator.BuildRun{ID: "run-1", Ticket: "T-1"}
	run, err := stages[orchestrator.StageWorkspace](context.Background(), run)
	if err != nil {
		t.Fatalf("workspace stage: %v", err)
	}
	if _, err := stages[orchestrator.StageDevLoop](context.Background(), run); err != nil {
		t.Fatalf("dev-loop stage: %v", err)
	}
	if loop.got.Run == "" {
		t.Fatal("the stage never invoked the loop")
	}
	return loop.got
}

func TestTheDevAgentIsGivenTheWholeTicket(t *testing.T) {
	// The body and the acceptance criteria are what the agent implements
	// against. A run carries a title; everything else is read back from the
	// plan by the composition root, and dropping it leaves the agent guessing.
	captured := captureDevRequest(t)
	if captured.Body == "" {
		t.Error("the ticket body never reached the dev agent")
	}
	if len(captured.Criteria) == 0 {
		t.Error("the acceptance criteria never reached the dev agent")
	}
	if captured.Issue == "" {
		t.Error("no issue was named, so the transcript import has no anchor")
	}
	if captured.Actor == "" {
		t.Error("no actor was named, so sutra refuses every mutation")
	}
}

func TestTheDevAgentsToolsetIsScopedToTheTicket(t *testing.T) {
	captured := captureDevRequest(t)
	if len(captured.AllowRules) == 0 {
		t.Fatal("no permission rules were generated: the agent cannot run its gates")
	}
	var sawBash bool
	for _, rule := range captured.AllowRules {
		if rule == "Bash" || strings.HasPrefix(rule, "Bash(*") {
			t.Errorf("%q permits every command on the machine", rule)
		}
		if strings.HasPrefix(rule, "Bash(") {
			sawBash = true
		}
	}
	if !sawBash {
		t.Error("no gate command is permitted, so the agent cannot run one")
	}
}

func TestTheDevAgentIsToldWhichModulesItTouched(t *testing.T) {
	// The module IDS from the snapshot, never the ticket title: context
	// assembly keys the law, the contracts and the learnings by module, and a
	// title matches none of them.
	captured := captureDevRequest(t)
	if len(captured.Modules) != 1 || captured.Modules[0] != "MOD-api" {
		t.Errorf("the touched modules are %v, want the snapshot's own ids", captured.Modules)
	}
}

func TestASubmissionThatLandedBeforeTheCrashIsNotSubmittedTwice(t *testing.T) {
	// The window: sutra created the review and the run recorded it, then the
	// process died before the table wrote the advanced state. The run comes
	// back in "submitting" with a review that already exists — and resubmitting
	// it would advance a revision nothing reworked, against a verdict event
	// there has not been.
	stages, _, reviews := stagesAndReviews(t, quietLoop(), passingCommands)
	run, err := runThrough(t, stages,
		orchestrator.BuildRun{ID: "run-sub-3", Ticket: "T-1", Issue: "issue-7", Head: "C2"},
		orchestrator.StageWorkspace, orchestrator.StageGates,
		orchestrator.StageValidate, orchestrator.StageSubmit)
	if err != nil {
		t.Fatalf("submit stage: %v", err)
	}
	opened := len(reviews.issues)

	again, err := stages[orchestrator.StageSubmit](context.Background(), run)
	if err != nil {
		t.Fatalf("replayed submit stage: %v", err)
	}
	if len(reviews.issues) != opened {
		t.Errorf("the replay opened another review: %v", reviews.issues)
	}
	if again.ReviewID != run.ReviewID {
		t.Errorf("the replay replaced review %q with %q", run.ReviewID, again.ReviewID)
	}
	if again.ReviewRevision != run.ReviewRevision {
		t.Errorf("the replay advanced the revision to %d", again.ReviewRevision)
	}
}

func TestNewWorkAfterAVerdictStillResubmits(t *testing.T) {
	// The control for the test above: a run whose head MOVED since its review
	// was opened has genuine rework to submit, and must not be mistaken for a
	// replay of the submission that already landed.
	stages, _, _ := stagesAndReviews(t, quietLoop(), passingCommands)
	run, err := runThrough(t, stages,
		orchestrator.BuildRun{ID: "run-sub-4", Ticket: "T-1", Issue: "issue-7", Head: "C2"},
		orchestrator.StageWorkspace, orchestrator.StageGates,
		orchestrator.StageValidate, orchestrator.StageSubmit)
	if err != nil {
		t.Fatalf("submit stage: %v", err)
	}
	run.Head = "C3"
	run.ReviewVerdictEvent = "event-9"
	again, err := stages[orchestrator.StageSubmit](context.Background(), run)
	if err != nil {
		t.Fatalf("resubmit stage: %v", err)
	}
	if again.ReviewRevision <= run.ReviewRevision {
		t.Errorf("the reworked head did not advance the revision: %d", again.ReviewRevision)
	}
}
