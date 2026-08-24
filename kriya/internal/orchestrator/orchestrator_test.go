package orchestrator_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"kriya/internal/clock"
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

func (m *memStore) Submitting(context.Context) ([]orchestrator.BuildRun, error) {
	return m.inReviewState(orchestrator.SubmitSubmitting), nil
}

func (m *memStore) Resubmitting(context.Context) ([]orchestrator.BuildRun, error) {
	return m.inReviewState(orchestrator.SubmitResubmitting), nil
}

func (m *memStore) Completing(context.Context) ([]orchestrator.BuildRun, error) {
	var out []orchestrator.BuildRun
	for _, r := range m.rows {
		if r.CompletionState == orchestrator.CompleteCompleting {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *memStore) inReviewState(state string) []orchestrator.BuildRun {
	var out []orchestrator.BuildRun
	for _, r := range m.rows {
		if r.ReviewState == state {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// recordingStages logs which stages ran, so a test asserts the traversal
// rather than only the final state.
func recordingStages(log *[]orchestrator.Stage, failures map[orchestrator.Stage]error) orchestrator.Stages {
	s := orchestrator.Stages{}
	for _, stage := range []orchestrator.Stage{
		orchestrator.StageWorkspace, orchestrator.StageDevLoop, orchestrator.StageGates,
		orchestrator.StageValidate, orchestrator.StageSubmit,
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
	id := seed(t, store, orchestrator.StateReviewSubmitted)
	run, err := orch(store, recordingStages(&log, nil)).Advance(context.Background(), id)
	if err != nil {
		t.Fatalf("a terminal state must not error: %v", err)
	}
	if run.State != orchestrator.StateReviewSubmitted {
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
	if run.State != orchestrator.StateReviewSubmitted {
		t.Errorf("settled in %q, want review-submitted", run.State)
	}
	want := []orchestrator.Stage{
		orchestrator.StageWorkspace, orchestrator.StageDevLoop, orchestrator.StageGates,
		orchestrator.StageValidate, orchestrator.StageSubmit,
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
	// Force the cycle the guard exists for by failing gates every round:
	// gates -> dev-loop -> gates -> ...
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

func sqlOrchStore(t *testing.T) orchestrator.SQLStore {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "o.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// The real migration text, as the composition root applies it: a test that
	// rebuilt the DDL by hand would pass while production had other columns.
	for _, schema := range []string{
		orchestrator.Migration, orchestrator.RoundLimitMigration,
		orchestrator.SubmissionMigration, orchestrator.CompletionMigration,
	} {
		for _, stmt := range strings.Split(schema, ";") {
			if strings.TrimSpace(stmt) == "" {
				continue
			}
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("migrate: %v", err)
			}
		}
	}
	return orchestrator.SQLStore{DB: db}
}

func TestARunSurvivesTheRoundTrip(t *testing.T) {
	s := sqlOrchStore(t)
	want := orchestrator.BuildRun{
		ID: "run-1", Ticket: "T-1", Plan: "/target", State: orchestrator.StateGates,
		GatedBase: "base-sha", Attempt: 2, Error: "gate structure failed",
		RoundLimit: 4, ReviewState: orchestrator.SubmitNone,
		CompletionState: orchestrator.CompleteNone,
	}
	if err := s.Upsert(context.Background(), want); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, found, err := s.Find(context.Background(), "run-1")
	if err != nil || !found {
		t.Fatalf("find: %v found=%v", err, found)
	}
	if got != want {
		t.Errorf("read back %+v", got)
	}
}

func TestAnUnknownRunIsNotFound(t *testing.T) {
	s := sqlOrchStore(t)
	_, found, err := s.Find(context.Background(), "run-absent")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if found {
		t.Error("a run that was never recorded was found")
	}
}

func TestARunStoreThatCannotBeReadIsNotEmpty(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "bare.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := orchestrator.SQLStore{DB: db}
	if _, _, err := s.Find(context.Background(), "run-1"); err == nil {
		t.Error("a missing table read as a run that does not exist")
	}
	if err := s.Upsert(context.Background(), orchestrator.BuildRun{ID: "run-1"}); err == nil {
		t.Error("a write to a missing table reported success")
	}
}

func TestAdvanceRefusesARunItCannotFind(t *testing.T) {
	// Advancing a run that is not there would invent state for a build nobody
	// started.
	o := orchestrator.Orchestrator{Store: sqlOrchStore(t), Now: clock.System{}}
	if _, err := o.Advance(context.Background(), "run-absent"); err == nil {
		t.Fatal("an unknown run was advanced")
	}
}

func TestSubmittingRunsComeBackWholeAndInOrder(t *testing.T) {
	// Recovery rebuilds the original request from these fields, so every one
	// of them has to survive the round trip.
	s := sqlOrchStore(t)
	for _, r := range []orchestrator.BuildRun{
		{
			ID: "run-b", Ticket: "T-2", State: orchestrator.StateSubmitting,
			GatedBase: "C2", Attempt: 1, ReviewKey: "key-b", ReviewCommit: "C2",
			ReviewSession: "sess-b", ReviewState: orchestrator.SubmitSubmitting,
		},
		{
			ID: "run-a", Ticket: "T-1", State: orchestrator.StateSubmitting,
			GatedBase: "C1", Attempt: 2, ReviewKey: "key-a", ReviewCommit: "C1",
			ReviewSession: "sess-a", ReviewState: orchestrator.SubmitSubmitting,
		},
		{
			ID: "run-c", Ticket: "T-3", ReviewState: orchestrator.SubmitSubmitted,
			ReviewID: "review-1",
		},
	} {
		if err := s.Upsert(context.Background(), r); err != nil {
			t.Fatalf("upsert %s: %v", r.ID, err)
		}
	}
	got, err := s.Submitting(context.Background())
	if err != nil {
		t.Fatalf("submitting: %v", err)
	}
	if len(got) != 2 || got[0].ID != "run-a" || got[1].ID != "run-b" {
		t.Fatalf("listed %+v", got)
	}
	if got[0].ReviewKey != "key-a" || got[0].ReviewCommit != "C1" ||
		got[0].ReviewSession != "sess-a" || got[0].Attempt != 2 {
		t.Errorf("run-a came back %+v", got[0])
	}
	if got[0].State != orchestrator.StateSubmitting {
		t.Errorf("the run state came back %q", got[0].State)
	}
}

func TestARunWithNoReviewStateStoresTheDeclaredDefault(t *testing.T) {
	// The column is an enum. A zero-valued BuildRun must not put "" in it.
	s := sqlOrchStore(t)
	if err := s.Upsert(context.Background(), orchestrator.BuildRun{ID: "run-1"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, _, err := s.Find(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got.ReviewState != orchestrator.SubmitNone {
		t.Errorf("stored %q, want %q", got.ReviewState, orchestrator.SubmitNone)
	}
}

func TestARunStoreThatCannotBeListedIsNotEmpty(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "bare.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := (orchestrator.SQLStore{DB: db}).Submitting(context.Background()); err == nil {
		t.Error("a missing table listed as no submissions in flight")
	}
}
