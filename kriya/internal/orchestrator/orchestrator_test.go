package orchestrator_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"kriya/internal/fakes"
	"kriya/internal/orchestrator"
)

type memStore struct {
	rows map[string]orchestrator.BuildRun
}

func newMemStore() *memStore { return &memStore{rows: map[string]orchestrator.BuildRun{}} }

func (m *memStore) Upsert(_ context.Context, r orchestrator.BuildRun) error {
	m.rows[r.ID] = r
	return nil
}

func (m *memStore) Find(_ context.Context, id string) (orchestrator.BuildRun, bool, error) {
	r, ok := m.rows[id]
	return r, ok, nil
}

// recordingStages logs which stages ran, so a test asserts the traversal
// rather than only the final state.
func recordingStages(log *[]orchestrator.Stage, failures map[orchestrator.Stage]error) orchestrator.Stages {
	s := orchestrator.Stages{}
	for _, stage := range []orchestrator.Stage{
		orchestrator.StageWorkspace, orchestrator.StageDevLoop, orchestrator.StageGates,
	} {
		stage := stage
		s[stage] = func(_ context.Context, run orchestrator.BuildRun) (orchestrator.BuildRun, error) {
			*log = append(*log, stage)
			if err, bad := failures[stage]; bad {
				return run, err
			}
			return run, nil
		}
	}
	return s
}

func orch(store *memStore, stages orchestrator.Stages) orchestrator.Orchestrator {
	return orchestrator.Orchestrator{Store: store, Stages: stages, Now: fakes.NewClock(time.Unix(0, 0))}
}

func seed(t *testing.T, store *memStore, state orchestrator.State) string {
	t.Helper()
	run := orchestrator.BuildRun{ID: "run-1", Ticket: "KRI-1", State: state}
	if err := store.Upsert(context.Background(), run); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return run.ID
}

func TestEveryStateInTheTableHasExactlyOneTransition(t *testing.T) {
	// The table IS the machine. Two rows for one state would make the next
	// step depend on iteration order, which is precisely the judgement
	// CON-deterministic-orchestrator forbids the orchestrator from having.
	seen := map[orchestrator.State]bool{}
	for _, tr := range orchestrator.Table {
		if seen[tr.From] {
			t.Errorf("state %q has more than one transition", tr.From)
		}
		seen[tr.From] = true
	}
}

func TestAdvanceFollowsTheTable(t *testing.T) {
	store := newMemStore()
	var log []orchestrator.Stage
	id := seed(t, store, orchestrator.StateQueued)
	run, err := orch(store, recordingStages(&log, nil)).Advance(context.Background(), id)
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if run.State != orchestrator.StateDevLoop {
		t.Errorf("state is %q, want dev-loop", run.State)
	}
	if len(log) != 1 || log[0] != orchestrator.StageWorkspace {
		t.Errorf("ran %v, want the workspace stage", log)
	}
}

func TestAFailingGateChainReturnsToTheDevLoop(t *testing.T) {
	// The findings ARE the next instruction, and the loop is how they get
	// acted on. Parking would need an operator to relay what the tools said.
	store := newMemStore()
	var log []orchestrator.Stage
	id := seed(t, store, orchestrator.StateGates)
	stages := recordingStages(&log, map[orchestrator.Stage]error{
		orchestrator.StageGates: errors.New("coverage below the floor"),
	})
	run, err := orch(store, stages).Advance(context.Background(), id)
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if run.State != orchestrator.StateDevLoop {
		t.Errorf("state is %q, want dev-loop", run.State)
	}
}

func TestAStageMalfunctionParksWithItsCause(t *testing.T) {
	// A stage ERROR is distinct from a stage reporting failure: the run parks
	// with the durable cause so the inbox can say WHY, rather than looping on
	// something no dev agent can fix.
	store := newMemStore()
	var log []orchestrator.Stage
	id := seed(t, store, orchestrator.StateQueued)
	stages := recordingStages(&log, map[orchestrator.Stage]error{
		orchestrator.StageWorkspace: errors.New("the worktree vanished"),
	})
	run, err := orch(store, stages).Advance(context.Background(), id)
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if run.State != orchestrator.StateAwaitingOperator {
		t.Errorf("state is %q, want awaiting-operator", run.State)
	}
	if !strings.Contains(run.Error, "worktree vanished") {
		t.Errorf("the cause was not recorded: %q", run.Error)
	}
}

func TestATerminalStateIsNotAnError(t *testing.T) {
	// A caller driving to completion needs to see it stop, not be told it
	// broke.
	store := newMemStore()
	var log []orchestrator.Stage
	id := seed(t, store, orchestrator.StatePOValidation)
	run, err := orch(store, recordingStages(&log, nil)).Advance(context.Background(), id)
	if err != nil {
		t.Fatalf("a terminal state must not error: %v", err)
	}
	if run.State != orchestrator.StatePOValidation {
		t.Errorf("state changed to %q", run.State)
	}
	if len(log) != 0 {
		t.Errorf("a terminal state ran %v", log)
	}
}

func TestDriveWalksTheWholeTraversal(t *testing.T) {
	store := newMemStore()
	var log []orchestrator.Stage
	id := seed(t, store, orchestrator.StateQueued)
	run, err := orch(store, recordingStages(&log, nil)).Drive(context.Background(), id, 10)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if run.State != orchestrator.StatePOValidation {
		t.Errorf("settled in %q, want po-validation", run.State)
	}
	want := []orchestrator.Stage{
		orchestrator.StageWorkspace, orchestrator.StageDevLoop, orchestrator.StageGates,
	}
	if len(log) != len(want) {
		t.Fatalf("ran %v, want %v", log, want)
	}
	for i := range want {
		if log[i] != want[i] {
			t.Errorf("step %d ran %q, want %q", i, log[i], want[i])
		}
	}
}

func TestDriveRefusesToLoopForever(t *testing.T) {
	// A table that fails to advance is a defect. Looping would hide it behind
	// a hang, which is the worst way to discover it.
	store := newMemStore()
	var log []orchestrator.Stage
	id := seed(t, store, orchestrator.StateGates)
	stages := recordingStages(&log, map[orchestrator.Stage]error{})
	// gates -> po-validation is terminal, so force the cycle the guard exists
	// for by failing gates every round: gates -> dev-loop -> gates -> ...
	stages[orchestrator.StageGates] = func(_ context.Context, run orchestrator.BuildRun) (orchestrator.BuildRun, error) {
		return run, errors.New("always fails")
	}
	if _, err := orch(store, stages).Drive(context.Background(), id, 6); err == nil {
		t.Fatal("a non-settling run must be reported, not looped on")
	}
}

func TestAMissingStageImplementationIsAnError(t *testing.T) {
	// The table naming a stage nothing implements is a wiring defect, and a
	// silent skip would advance the run as though the work had happened.
	store := newMemStore()
	id := seed(t, store, orchestrator.StateQueued)
	if _, err := orch(store, orchestrator.Stages{}).Advance(context.Background(), id); err == nil {
		t.Fatal("a stage with no implementation must be an error")
	}
}
