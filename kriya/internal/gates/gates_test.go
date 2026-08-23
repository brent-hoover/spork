package gates_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"kriya/internal/fakes"
	"kriya/internal/gates"
)

type memStore struct {
	rows []gates.Result
}

func (m *memStore) Upsert(_ context.Context, r gates.Result) error {
	for i, existing := range m.rows {
		if existing.Build == r.Build && existing.Module == r.Module &&
			existing.Gate == r.Gate && existing.Attempt == r.Attempt {
			m.rows[i] = r
			return nil
		}
	}
	m.rows = append(m.rows, r)
	return nil
}

func (m *memStore) Passed(_ context.Context, build, module, gate, commit string) (bool, error) {
	for _, r := range m.rows {
		if r.Build == build && r.Module == module && r.Gate == gate && r.Commit == commit {
			return r.Passed, nil
		}
	}
	return false, nil
}

func runner(store *memStore) gates.Runner {
	return gates.Runner{Store: store, Now: fakes.NewClock(time.Unix(0, 0))}
}

// allPassing declares every command as a no-op that succeeds.
func allPassing() map[string]string {
	return map[string]string{
		"test": "true", "lint": "true", "typecheck": "true",
		"arch": "true", "coverage": "true", "mutation": "true",
	}
}

func TestAPassingGateIsRecorded(t *testing.T) {
	store := &memStore{}
	r, err := runner(store).Run(context.Background(), "b1", "MOD-a", "test", "c1", t.TempDir(), allPassing())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !r.Passed {
		t.Error("a succeeding command must pass the gate")
	}
	if len(store.rows) != 1 {
		t.Errorf("recorded %d results, want 1", len(store.rows))
	}
}

func TestAFailingGateIsAResultNotAnError(t *testing.T) {
	// A gate that fails is the chain working. Returning it as an error would
	// make "the linter found something" indistinguishable from "the linter
	// would not start", and only the second is a malfunction.
	store := &memStore{}
	cmds := allPassing()
	cmds["lint"] = "echo problems-here >&2; exit 1"
	r, err := runner(store).Run(context.Background(), "b1", "MOD-a", "structure", "c1", t.TempDir(), cmds)
	if err != nil {
		t.Fatalf("a failing gate must not be an error: %v", err)
	}
	if r.Passed {
		t.Error("the gate should have failed")
	}
	var d struct {
		ExitCode int    `json:"exit_code"`
		Stderr   string `json:"stderr"`
	}
	if err := json.Unmarshal(r.Detail, &d); err != nil {
		t.Fatalf("detail: %v", err)
	}
	if d.ExitCode != 1 {
		t.Errorf("exit code %d, want 1", d.ExitCode)
	}
	if !strings.Contains(d.Stderr, "problems-here") {
		t.Error("the tool's own output must reach the detail for the dev agent")
	}
}

func TestTheToolsOutputIsCapturedForTheDevAgent(t *testing.T) {
	// AC-coverage-recorded wants the uncovered arms named in detail. They are
	// IN the tool's output, so capture is the mechanism — kriya parses nothing,
	// which is also what AC-coverage-floor requires of it.
	store := &memStore{}
	cmds := allPassing()
	cmds["coverage"] = "echo 'uncovered: foo.go:12 arm false'"
	r, err := runner(store).Run(context.Background(), "b1", "MOD-a", "branch-coverage", "c1", t.TempDir(), cmds)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(string(r.Detail), "foo.go:12") {
		t.Error("the tool's stdout did not reach the detail")
	}
}

func TestTheChainStopsAtTheFirstFailure(t *testing.T) {
	// Coverage is judged only over a passing suite, and mutation against
	// failing tests measures nothing at the highest cost.
	store := &memStore{}
	cmds := allPassing()
	cmds["test"] = "exit 1"
	results, err := runner(store).RunChain(context.Background(), "b1", "MOD-a", "c1", t.TempDir(), cmds)
	if err != nil {
		t.Fatalf("chain: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("ran %d gates after a failing test, want 1", len(results))
	}
	if results[0].Gate != "test" {
		t.Errorf("first gate was %q, want test", results[0].Gate)
	}
}

func TestAFullChainRunsEveryGateInOrder(t *testing.T) {
	store := &memStore{}
	results, err := runner(store).RunChain(context.Background(), "b1", "MOD-a", "c1", t.TempDir(), allPassing())
	if err != nil {
		t.Fatalf("chain: %v", err)
	}
	if len(results) != len(gates.Chain) {
		t.Fatalf("ran %d gates, want %d", len(results), len(gates.Chain))
	}
	for i, want := range gates.Chain {
		if results[i].Gate != want {
			t.Errorf("gate %d was %q, want %q", i, results[i].Gate, want)
		}
	}
	if gates.Chain[0] != "test" {
		t.Error("test must run first: coverage is judged only over a passing suite")
	}
	if gates.Chain[len(gates.Chain)-1] != "mutation" {
		t.Error("mutation must run last")
	}
}

func TestARerunReplacesRatherThanDuplicates(t *testing.T) {
	// "results upsert as GateResults pinned to the commit, so reruns never
	// duplicate".
	store := &memStore{}
	r := runner(store)
	for range 3 {
		if _, err := r.Run(context.Background(), "b1", "MOD-a", "test", "c1", t.TempDir(), allPassing()); err != nil {
			t.Fatalf("run: %v", err)
		}
	}
	if len(store.rows) != 1 {
		t.Errorf("three runs left %d rows, want 1", len(store.rows))
	}
}

func TestAStalePassNeverSatisfiesTheChain(t *testing.T) {
	// "a passing result recorded at an older commit never satisfies the chain".
	store := &memStore{}
	if _, err := runner(store).Run(context.Background(), "b1", "MOD-a", "test", "old-commit", t.TempDir(), allPassing()); err != nil {
		t.Fatalf("run: %v", err)
	}
	passed, err := store.Passed(context.Background(), "b1", "MOD-a", "test", "new-commit")
	if err != nil {
		t.Fatalf("passed: %v", err)
	}
	if passed {
		t.Error("a pass at an older commit satisfied the chain at a newer one")
	}
}

func TestAMissingCommandIsADefectNotAFailingGate(t *testing.T) {
	// Intake refuses a module missing any of the six, so reaching here means
	// the snapshot and the chain disagree.
	store := &memStore{}
	cmds := allPassing()
	delete(cmds, "mutation")
	if _, err := runner(store).Run(context.Background(), "b1", "MOD-a", "mutation", "c1", t.TempDir(), cmds); err == nil {
		t.Fatal("a missing command must be an error, not a failed gate")
	}
}

func TestOutputIsTruncatedFromTheHead(t *testing.T) {
	// A tool's verdict is at the END, so a head-truncated capture reliably
	// discards the one line the dev agent needs.
	store := &memStore{}
	r := gates.Runner{Store: store, Now: fakes.NewClock(time.Unix(0, 0)), Limit: 64}
	cmds := allPassing()
	cmds["test"] = "for i in $(seq 1 200); do echo padding-line-$i; done; echo FINAL-VERDICT"
	got, err := r.Run(context.Background(), "b1", "MOD-a", "test", "c1", t.TempDir(), cmds)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(string(got.Detail), "FINAL-VERDICT") {
		t.Error("truncation discarded the tail, which is where the verdict is")
	}
	if !strings.Contains(string(got.Detail), "truncated") {
		t.Error("truncation was not marked, so the reader cannot tell output is missing")
	}
}

func TestOutputAtTheLimitIsKeptWhole(t *testing.T) {
	// Truncating output that fits would put an ellipsis in front of a complete
	// capture and cost the dev agent the first line of it.
	runner := gates.Runner{Store: &memStore{}, Now: fakes.NewClock(time.Unix(0, 0)), Limit: 64}
	result, err := runner.Run(context.Background(), "run-1", "engine", "test", "sha-a", t.TempDir(),
		map[string]string{"test": "printf '%064d' 0"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Contains(string(result.Detail), "truncated") {
		t.Errorf("output of exactly the limit was truncated: %s", result.Detail)
	}
}

func TestOutputOverTheLimitKeepsItsTail(t *testing.T) {
	// A tool's verdict is at the end, and a head-truncated capture reliably
	// discards the one line the dev agent needs.
	runner := gates.Runner{Store: &memStore{}, Now: fakes.NewClock(time.Unix(0, 0)), Limit: 32}
	result, err := runner.Run(context.Background(), "run-1", "engine", "test", "sha-a", t.TempDir(),
		map[string]string{"test": "printf '%063d' 0; echo VERDICT"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	detail := string(result.Detail)
	if !strings.Contains(detail, "truncated") {
		t.Fatalf("output over the limit was not truncated: %s", detail)
	}
	if !strings.Contains(detail, "VERDICT") {
		t.Errorf("truncation discarded the tail: %s", detail)
	}
}
