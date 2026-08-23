package architect_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"kriya/internal/architect"
)

func sqlStore(t *testing.T, migrate bool) architect.SQLStore {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if migrate {
		if _, err := db.Exec(architect.Migration); err != nil {
			t.Fatalf("migrate: %v", err)
		}
	}
	return architect.SQLStore{DB: db}
}

func TestAnInterventionSurvivesTheRoundTrip(t *testing.T) {
	s := sqlStore(t, true)
	want := architect.Intervention{
		Build: "run-1", Trigger: architect.TriggerAgentDeclared,
		Findings: json.RawMessage(`["a","b"]`), Direction: "Split the handler.",
		State: architect.StateDirected, Outcome: "merged",
	}
	if err := s.Upsert(context.Background(), want); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, found, err := s.Find(context.Background(), "run-1")
	if err != nil || !found {
		t.Fatalf("find: %v found=%v", err, found)
	}
	if got.Trigger != want.Trigger || got.Direction != want.Direction ||
		got.State != want.State || got.Outcome != want.Outcome {
		t.Errorf("read back %+v", got)
	}
	if string(got.Findings) != `["a","b"]` {
		t.Errorf("findings came back %s", got.Findings)
	}
}

func TestTheLifecycleAdvancesInPlace(t *testing.T) {
	// One row per run: recovery and resume read THAT row, and a second row
	// for the same run would make "which is current?" a question.
	s := sqlStore(t, true)
	i := architect.Intervention{Build: "run-1", Trigger: architect.TriggerRoundLimit,
		State: architect.StateOpen}
	if err := s.Upsert(context.Background(), i); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	i.State, i.Direction = architect.StateDirected, "Split the handler."
	if err := s.Upsert(context.Background(), i); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	got, _, err := s.Find(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got.State != architect.StateDirected || got.Direction == "" {
		t.Errorf("read back %+v", got)
	}
}

func TestAnEmptyFindingsHistoryStoresAsAnEmptyList(t *testing.T) {
	// The column is JSON. A blank string would not parse, and every later read
	// of that row would fail on a run that simply had no findings to hand over.
	s := sqlStore(t, true)
	if err := s.Upsert(context.Background(), architect.Intervention{
		Build: "run-1", Trigger: architect.TriggerAgentDeclared, State: architect.StateOpen,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, _, err := s.Find(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	var back []string
	if err := json.Unmarshal(got.Findings, &back); err != nil {
		t.Fatalf("the stored findings are not JSON: %v", err)
	}
}

func TestARunWithNoInterventionIsNotFound(t *testing.T) {
	s := sqlStore(t, true)
	_, found, err := s.Find(context.Background(), "run-absent")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if found {
		t.Error("a run that never hit an impasse was found")
	}
}

func TestAnInterventionStoreThatCannotBeReadIsNotEmpty(t *testing.T) {
	s := sqlStore(t, false)
	if _, _, err := s.Find(context.Background(), "run-1"); err == nil {
		t.Error("a missing table read as a run with no impasse")
	}
	if err := s.Upsert(context.Background(), architect.Intervention{Build: "run-1"}); err == nil {
		t.Error("a write to a missing table reported success")
	}
}
