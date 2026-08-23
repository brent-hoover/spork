package architect_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"kriya/internal/agent"
	"kriya/internal/architect"
	"kriya/internal/fakes"
)

type memStore struct {
	rows   map[string]architect.Intervention
	writes []architect.Intervention
	err    error
}

func newMemStore() *memStore {
	return &memStore{rows: map[string]architect.Intervention{}}
}

func (m *memStore) Upsert(_ context.Context, i architect.Intervention) error {
	if m.err != nil {
		return m.err
	}
	m.rows[i.Build] = i
	m.writes = append(m.writes, i)
	return nil
}

func (m *memStore) Find(_ context.Context, build string) (architect.Intervention, bool, error) {
	if m.err != nil {
		return architect.Intervention{}, false, m.err
	}
	i, ok := m.rows[build]
	return i, ok, nil
}

func saReply(direction string) []agent.Result {
	return []agent.Result{{SessionID: "sa-1", Model: "test-model", Text: direction}}
}

func resolver(store *memStore, ag *fakes.Agent) architect.Architect {
	return architect.Architect{Agent: ag, Store: store, Now: fakes.NewClock(time.Unix(0, 0))}
}

func TestTheImpasseIsRecordedBeforeTheArchitectIsAsked(t *testing.T) {
	// An impasse that existed only in memory would let a restarted loop run
	// straight back into the same wall.
	store := newMemStore()
	ag := &fakes.Agent{Err: errors.New("agent died")}
	_, err := resolver(store, ag).Resolve(context.Background(), "run-1", "KRI-1",
		architect.TriggerRoundLimit, []string{"- **Severity**: High"}, "")
	if err == nil {
		t.Fatal("an architect that never answered read as success")
	}
	row := store.rows["run-1"]
	if row.State != architect.StateOpen {
		t.Errorf("the row is in state %q", row.State)
	}
	if !strings.Contains(string(row.Findings), "Severity") {
		t.Errorf("the findings history was not persisted: %s", row.Findings)
	}
}

func TestTheDirectionIsRecordedOnTheRow(t *testing.T) {
	store := newMemStore()
	ag := &fakes.Agent{Replies: saReply("Split the handler; the store is not the problem.")}
	got, err := resolver(store, ag).Resolve(context.Background(), "run-1", "KRI-1",
		architect.TriggerRoundLimit, []string{"finding one", "finding two"}, "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.State != architect.StateDirected {
		t.Errorf("state is %q", got.State)
	}
	if !strings.Contains(store.rows["run-1"].Direction, "Split the handler") {
		t.Errorf("the direction was not recorded: %q", store.rows["run-1"].Direction)
	}
	if store.rows["run-1"].Trigger != architect.TriggerRoundLimit {
		t.Errorf("the trigger is %q", store.rows["run-1"].Trigger)
	}
}

func TestTheArchitectGetsEveryFindingInOrder(t *testing.T) {
	// Its job is to see what dev and review could not agree on, and a summary
	// is precisely what hides that.
	store := newMemStore()
	ag := &fakes.Agent{Replies: saReply("direction")}
	_, err := resolver(store, ag).Resolve(context.Background(), "run-1", "KRI-1",
		architect.TriggerAgentDeclared,
		[]string{"first finding", "second finding", "third finding"}, "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	prompt := ag.Requests[0].Prompt
	first := strings.Index(prompt, "first finding")
	second := strings.Index(prompt, "second finding")
	third := strings.Index(prompt, "third finding")
	if first < 0 || second < 0 || third < 0 {
		t.Fatalf("a finding is missing from the prompt:\n%s", prompt)
	}
	if first >= second || second >= third {
		t.Error("the findings did not reach the architect in order")
	}
	// Numbered from one, and the numbers are how the architect refers back to
	// a round when it answers.
	for _, want := range []string{"round 1", "round 2", "round 3"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt has no %q:\n%s", want, prompt)
		}
	}
}

func TestTheArchitectRunsAsItsOwnRole(t *testing.T) {
	store := newMemStore()
	ag := &fakes.Agent{Replies: saReply("direction")}
	if _, err := resolver(store, ag).Resolve(context.Background(), "run-1", "KRI-1",
		architect.TriggerRoundLimit, nil, ""); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if ag.Requests[0].Role != agent.RoleSA {
		t.Errorf("ran as %q", ag.Requests[0].Role)
	}
}

func TestTheArchitectGetsNoToolThatCouldChangeTheWorktree(t *testing.T) {
	// AC-sa-directs requires the worktree contents and branch head to be
	// identical before and after. Handing it nothing that could change them is
	// what makes that true rather than hoped for.
	store := newMemStore()
	ag := &fakes.Agent{Replies: saReply("direction")}
	if _, err := resolver(store, ag).Resolve(context.Background(), "run-1", "KRI-1",
		architect.TriggerRoundLimit, nil, ""); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	rules := strings.Join(ag.Requests[0].AllowRules, ",")
	if rules == "" {
		t.Fatal("no permission rules were passed, so the agent gets defaults")
	}
	for _, mutating := range []string{"Write", "Edit", "Bash", "NotebookEdit", "MultiEdit"} {
		if strings.Contains(rules, mutating) {
			t.Errorf("the architect was granted %s", mutating)
		}
	}
	if ag.Requests[0].Workspace != "" {
		t.Errorf("the architect was pointed at a workspace: %q", ag.Requests[0].Workspace)
	}
}

func TestTheAllowRulesCannotBeMutatedByACaller(t *testing.T) {
	first := architect.AllowRules()
	first[0] = "Bash"
	if architect.AllowRules()[0] == "Bash" {
		t.Error("a caller widened the architect's toolset")
	}
}

func TestResumeReadsTheRowRatherThanMemory(t *testing.T) {
	// A run that restarted between the direction and the resume must still get
	// it.
	store := newMemStore()
	if err := store.Upsert(context.Background(), architect.Intervention{
		Build: "run-1", Trigger: architect.TriggerRoundLimit,
		Direction: "Split the handler.", State: architect.StateDirected,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, found, err := resolver(store, &fakes.Agent{}).Resume(context.Background(), "run-1")
	if err != nil || !found {
		t.Fatalf("resume: %v found=%v", err, found)
	}
	if got.Direction != "Split the handler." {
		t.Errorf("resumed with %q", got.Direction)
	}
	if store.rows["run-1"].State != architect.StateResumed {
		t.Errorf("the row is in state %q after resume", store.rows["run-1"].State)
	}
}

func TestAnUndirectedInterventionDoesNotResume(t *testing.T) {
	// Open means the architect has not answered. Resuming on it would put an
	// empty direction into the dev agent's context and look like advice.
	store := newMemStore()
	if err := store.Upsert(context.Background(), architect.Intervention{
		Build: "run-1", State: architect.StateOpen,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, found, err := resolver(store, &fakes.Agent{}).Resume(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if found {
		t.Error("an unanswered impasse resumed")
	}
}

func TestARunWithNoInterventionResumesCleanly(t *testing.T) {
	_, found, err := resolver(newMemStore(), &fakes.Agent{}).Resume(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if found {
		t.Error("a run that never hit an impasse found one")
	}
}

func TestAStoreFailureSurfaces(t *testing.T) {
	store := newMemStore()
	store.err = errors.New("disk full")
	a := resolver(store, &fakes.Agent{Replies: saReply("direction")})
	if _, err := a.Resolve(context.Background(), "run-1", "KRI-1",
		architect.TriggerRoundLimit, nil, ""); err == nil {
		t.Error("an impasse that could not be recorded read as recorded")
	}
	if _, _, err := a.Resume(context.Background(), "run-1"); err == nil {
		t.Error("a store that could not be read read as no intervention")
	}
}

func TestFindingsAreValidJSONOnTheRow(t *testing.T) {
	store := newMemStore()
	ag := &fakes.Agent{Replies: saReply("direction")}
	if _, err := resolver(store, ag).Resolve(context.Background(), "run-1", "KRI-1",
		architect.TriggerRoundLimit, []string{"a", "b"}, ""); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	var back []string
	if err := json.Unmarshal(store.rows["run-1"].Findings, &back); err != nil {
		t.Fatalf("the findings history is not readable: %v", err)
	}
	if len(back) != 2 || back[0] != "a" {
		t.Errorf("read back %v", back)
	}
}
