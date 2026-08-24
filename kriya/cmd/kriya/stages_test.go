package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"encoding/json"

	"kriya/internal/agent"
	"kriya/internal/clock"
	"kriya/internal/devloop"
	"kriya/internal/fakes"
	"kriya/internal/gates"
	"kriya/internal/orchestrator"
	"kriya/internal/owner"
	"kriya/internal/planner"
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

func stagesForTest(t *testing.T, loop devloop.Loop, commandsFor func(string) map[string]string) (orchestrator.Stages, workspace.Manager) {
	t.Helper()
	db := openTemp(t)
	if err := applyMigrations(context.Background(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ws := workspaceManagerOn(t, db)
	runner := gates.Runner{Store: gates.SQLStore{DB: db}, Now: clock.System{}}
	return buildStages(deps{
		ws: ws, loop: loop, runner: runner, snap: planner.Snapshot{},
		po: owner.Owner{
			Agent: &fakes.Agent{Replies: poReply(owner.VerdictSatisfied)},
			Store: owner.SQLStore{DB: db}, Gates: runner, Now: clock.System{},
		},
		submitter: orchestrator.Submitter{
			Store:   orchestrator.SQLStore{DB: db},
			Reviews: recordingReviews{},
			Author:  "actor-1",
		},
		commandsFor: commandsFor,
		criteriaFor: func(string) []string { return []string{"AC-x"} },
		issueFor:    func(string) string { return "issue-1" },
		sessionFor:  sessionFromStore(db),
	}), ws
}

// recordingReviews stands in for sutra's review API.
type recordingReviews struct{}

func (recordingReviews) Create(
	_ context.Context, _, _, _, _, commit, _, _, _, key string,
) (string, error) {
	return "review-" + key[:8] + "-" + commit, nil
}

func (recordingReviews) Resubmit(
	_ context.Context, _, _, _, _, _, _ string, expectedRevision int, _, _, _, _ string,
) (int, error) {
	return expectedRevision + 1, nil
}

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
