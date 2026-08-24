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

func (m *memStore) ForTicket(_ context.Context, issue string) (orchestrator.BuildRun, bool, error) {
	for _, run := range m.rows {
		if run.Ticket == issue && run.State != orchestrator.StateClosed {
			return run, true, nil
		}
	}
	return orchestrator.BuildRun{}, false, nil
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
		orchestrator.PopMigration, orchestrator.CursorMigration,
		orchestrator.MergeMigration, orchestrator.ResourceMigration,
		orchestrator.HeadMigration,
		orchestrator.IssueMigration,
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
		ID: "run-1", Ticket: "T-1", Issue: "issue-7", Branch: "kriya/KRI-1/abcd",
		Plan: "/target", State: orchestrator.StateGates,
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

func TestARunWithNoCompletionStateStoresTheDeclaredDefault(t *testing.T) {
	// The column is an enum. A zero-valued BuildRun must not put "" in it, or
	// every later read finds a value the spec does not declare.
	s := sqlOrchStore(t)
	if err := s.Upsert(context.Background(), orchestrator.BuildRun{ID: "run-1"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, _, err := s.Find(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got.CompletionState != orchestrator.CompleteNone {
		t.Errorf("stored %q, want %q", got.CompletionState, orchestrator.CompleteNone)
	}
}

func TestASetCompletionStateIsStoredAsGiven(t *testing.T) {
	s := sqlOrchStore(t)
	if err := s.Upsert(context.Background(), orchestrator.BuildRun{
		ID: "run-1", CompletionState: orchestrator.CompleteCompleting, CloseKey: "key-1",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	open, err := s.Completing(context.Background())
	if err != nil {
		t.Fatalf("completing: %v", err)
	}
	if len(open) != 1 || open[0].CloseKey != "key-1" {
		t.Fatalf("completing listed %+v", open)
	}
}

func TestACompletingListThatCannotBeReadIsNotEmpty(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "bare.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := (orchestrator.SQLStore{DB: db}).Completing(context.Background()); err == nil {
		t.Error("a missing table listed as no completions in flight")
	}
}

func TestPopOrdinalsSurviveTheRoundTrip(t *testing.T) {
	// A target that has settled no pops has no row, which is zero rather than
	// an error: it is genuinely where a target that has done nothing stands.
	s := orchestrator.SQLOrdinals{DB: sqlOrchStore(t).DB}
	if _, err := s.Current(context.Background(), "/nowhere"); err != nil {
		t.Fatalf("current: %v", err)
	}
	if err := s.Advance(context.Background(), "/target", 3); err != nil {
		t.Fatalf("advance: %v", err)
	}
	got, err := s.Current(context.Background(), "/target")
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if got != 3 {
		t.Errorf("read %d", got)
	}
	if err := s.Advance(context.Background(), "/target", 4); err != nil {
		t.Fatalf("re-advance: %v", err)
	}
	if again, _ := s.Current(context.Background(), "/target"); again != 4 {
		t.Errorf("read %d after re-advancing", again)
	}
}

func TestFeedCursorsSurviveTheRoundTrip(t *testing.T) {
	// No row is the START of the feed, which is where a consumer that has read
	// nothing genuinely is.
	s := orchestrator.SQLCursors{DB: sqlOrchStore(t).DB}
	got, err := s.Current(context.Background(), "verdicts")
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if got != "" {
		t.Errorf("a consumer that read nothing is at %q", got)
	}
	if err := s.Advance(context.Background(), "verdicts", "cursor-1"); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if again, _ := s.Current(context.Background(), "verdicts"); again != "cursor-1" {
		t.Errorf("read %q", again)
	}
	// Two consumers do not advance each other's position.
	if err := s.Advance(context.Background(), "approvals", "cursor-9"); err != nil {
		t.Fatalf("advance other: %v", err)
	}
	if mine, _ := s.Current(context.Background(), "verdicts"); mine != "cursor-1" {
		t.Errorf("another consumer moved this one to %q", mine)
	}
}

func TestARunIsFoundByItsReviewSession(t *testing.T) {
	// Feedback routes by session, so the lookup has to work that way.
	s := sqlOrchStore(t)
	if err := s.Upsert(context.Background(), orchestrator.BuildRun{
		ID: "run-1", Ticket: "KRI-1", ReviewSession: "sess-42", ReviewID: "review-1",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, found, err := s.BySession(context.Background(), "sess-42")
	if err != nil || !found {
		t.Fatalf("by session: %v found=%v", err, found)
	}
	if got.ID != "run-1" {
		t.Errorf("found %q", got.ID)
	}
	_, found, err = s.BySession(context.Background(), "sess-unknown")
	if err != nil {
		t.Fatalf("by session: %v", err)
	}
	if found {
		t.Error("a session no run was stamped with found one")
	}
}

func TestTheseStoresFailRatherThanReadAsEmpty(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "bare.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := (orchestrator.SQLOrdinals{DB: db}).Current(context.Background(), "/t"); err == nil {
		t.Error("a missing pop_ordinal table read as zero")
	}
	if err := (orchestrator.SQLOrdinals{DB: db}).Advance(context.Background(), "/t", 1); err == nil {
		t.Error("a write to a missing pop_ordinal table reported success")
	}
	if _, err := (orchestrator.SQLCursors{DB: db}).Current(context.Background(), "v"); err == nil {
		t.Error("a missing feed_cursor table read as the start of the feed")
	}
	if err := (orchestrator.SQLCursors{DB: db}).Advance(context.Background(), "v", "c"); err == nil {
		t.Error("a write to a missing feed_cursor table reported success")
	}
	if _, _, err := (orchestrator.SQLStore{DB: db}).BySession(context.Background(), "s"); err == nil {
		t.Error("a missing build_run table read as an unknown session")
	}
}

func sqlMergeAttempts(t *testing.T) orchestrator.SQLAttempts {
	t.Helper()
	return orchestrator.SQLAttempts{DB: sqlOrchStore(t).DB}
}

func TestAMergeAttemptSurvivesTheRoundTrip(t *testing.T) {
	s := sqlMergeAttempts(t)
	want := orchestrator.MergeAttempt{
		Key: "key-1", TargetKey: "KRIYA0a", Build: "run-1", Review: "review-1",
		Revision: 2, ApprovalEvent: "event-9", Commit: "C2", ExpectedBase: "base-1",
		State: orchestrator.AttemptQueued,
	}
	fresh, err := s.Insert(context.Background(), want)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if !fresh {
		t.Error("the first insert of an attempt was not fresh")
	}
	got, found, err := s.Find(context.Background(), "key-1")
	if err != nil || !found {
		t.Fatalf("find: %v found=%v", err, found)
	}
	if got != want {
		t.Errorf("read back %+v", got)
	}
}

func TestAReplayedApprovalCollidesOnTheKey(t *testing.T) {
	// The key is the PRIMARY KEY, so nothing above has to check first: the
	// uniqueness IS the check.
	s := sqlMergeAttempts(t)
	a := orchestrator.MergeAttempt{
		Key: "key-1", TargetKey: "KRIYA0a", Build: "run-1", Review: "review-1",
		ApprovalEvent: "event-9", State: orchestrator.AttemptQueued,
	}
	if _, err := s.Insert(context.Background(), a); err != nil {
		t.Fatalf("insert: %v", err)
	}
	fresh, err := s.Insert(context.Background(), a)
	if err != nil {
		t.Fatalf("re-insert: %v", err)
	}
	if fresh {
		t.Error("a replayed approval event enqueued a second attempt")
	}
}

func TestUnfinishedAttemptsComeBackInQueueOrder(t *testing.T) {
	// The queue is a queue: a target's approvals must land in the order they
	// arrived, which is insertion order and not key order.
	s := sqlMergeAttempts(t)
	for _, key := range []string{"zzz", "aaa", "mmm"} {
		if _, err := s.Insert(context.Background(), orchestrator.MergeAttempt{
			Key: key, TargetKey: "KRIYA0a", Build: "run-" + key,
			State: orchestrator.AttemptQueued,
		}); err != nil {
			t.Fatalf("insert %s: %v", key, err)
		}
	}
	settled := orchestrator.MergeAttempt{
		Key: "done", TargetKey: "KRIYA0a", Build: "run-done",
		State: orchestrator.AttemptQueued,
	}
	if _, err := s.Insert(context.Background(), settled); err != nil {
		t.Fatalf("insert: %v", err)
	}
	settled.State = orchestrator.AttemptMerged
	if err := s.Update(context.Background(), settled); err != nil {
		t.Fatalf("update: %v", err)
	}

	got, err := s.Unfinished(context.Background())
	if err != nil {
		t.Fatalf("unfinished: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("listed %d attempts", len(got))
	}
	for i, want := range []string{"zzz", "aaa", "mmm"} {
		if got[i].Key != want {
			t.Errorf("position %d is %q, want %q", i, got[i].Key, want)
		}
	}
}

func TestAnUnknownAttemptIsNotFound(t *testing.T) {
	s := sqlMergeAttempts(t)
	_, found, err := s.Find(context.Background(), "no-such-key")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if found {
		t.Error("an attempt nobody enqueued was found")
	}
}

func TestAnAttemptStoreThatCannotBeReadIsNotEmpty(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "bare.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := orchestrator.SQLAttempts{DB: db}
	if _, err := s.Unfinished(context.Background()); err == nil {
		t.Error("a missing table listed as no attempts in flight")
	}
	if _, _, err := s.Find(context.Background(), "key-1"); err == nil {
		t.Error("a missing table read as an unknown attempt")
	}
	if _, err := s.Insert(context.Background(), orchestrator.MergeAttempt{Key: "k"}); err == nil {
		t.Error("an insert into a missing table reported success")
	}
	if err := s.Update(context.Background(), orchestrator.MergeAttempt{Key: "k"}); err == nil {
		t.Error("an update on a missing table reported success")
	}
}

func TestARunIsFoundByItsTicketWhileUnsettled(t *testing.T) {
	// A pop can hand back a ticket a run already exists for — after a rework,
	// or after a crash that left the claim standing. Starting a second run
	// would abandon the first with its review and its attempt count.
	s := sqlOrchStore(t)
	if err := s.Upsert(context.Background(), orchestrator.BuildRun{
		ID: "run-1", Ticket: "Create a short link", Issue: "issue-7",
		State: orchestrator.StateDevLoop, ReviewID: "review-1", Attempt: 3,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, found, err := s.ForTicket(context.Background(), "issue-7")
	if err != nil || !found {
		t.Fatalf("for ticket: %v found=%v", err, found)
	}
	if got.ID != "run-1" || got.Attempt != 3 || got.ReviewID != "review-1" {
		t.Errorf("found %+v", got)
	}
}

func TestASettledRunIsNotResumed(t *testing.T) {
	// A closed run is history. A new claim on the same issue is genuinely new
	// work, and resuming the old one would reopen something finished.
	s := sqlOrchStore(t)
	for _, state := range []orchestrator.State{
		orchestrator.StateClosed, orchestrator.StateMerged, orchestrator.StateCancelled,
	} {
		if err := s.Upsert(context.Background(), orchestrator.BuildRun{
			ID: "run-" + string(state), Ticket: "settled " + string(state),
			Issue: "issue-settled-" + string(state), State: state,
		}); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		_, found, err := s.ForTicket(context.Background(), "issue-settled-"+string(state))
		if err != nil {
			t.Fatalf("for ticket: %v", err)
		}
		if found {
			t.Errorf("a run in %q was offered for resumption", state)
		}
	}
}

func TestATicketWithNoRunIsNotFound(t *testing.T) {
	s := sqlOrchStore(t)
	_, found, err := s.ForTicket(context.Background(), "issue-fresh")
	if err != nil {
		t.Fatalf("for ticket: %v", err)
	}
	if found {
		t.Error("a ticket nothing has built found a run")
	}
}

func TestForTicketFailsRatherThanReadingAsFresh(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "bare.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, _, err := (orchestrator.SQLStore{DB: db}).
		ForTicket(context.Background(), "issue-7"); err == nil {
		t.Error("a missing table read as a ticket with no run")
	}
}

func TestTwoIssuesWithOneTitleDoNotShareARun(t *testing.T) {
	// Titles are not unique — a decomposition can produce "add tests" twice,
	// for two different modules. Keying resumption on the title makes the
	// second claim drive and submit the FIRST issue's work, and orphans the
	// issue it actually claimed.
	s := sqlOrchStore(t)
	first := orchestrator.BuildRun{
		ID: "run-1", Ticket: "add tests", Issue: "issue-7", Plan: "/target",
		State: orchestrator.StateDevLoop,
	}
	if err := s.Upsert(context.Background(), first); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, found, err := s.ForTicket(context.Background(), "issue-8")
	if err != nil {
		t.Fatalf("for ticket: %v", err)
	}
	if found {
		t.Errorf("issue-8 resumed run %q, which belongs to issue-7", got.ID)
	}
	// And its own issue does find it.
	got, found, err = s.ForTicket(context.Background(), "issue-7")
	if err != nil || !found {
		t.Fatalf("issue-7 did not find its own run: %v found=%v", err, found)
	}
	if got.ID != "run-1" {
		t.Errorf("resumed %q", got.ID)
	}
}
