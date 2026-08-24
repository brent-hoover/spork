package gates_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
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

func (m *memStore) Passed(
	_ context.Context, build, module, gate, commit string, attempt int,
) (bool, error) {
	for _, r := range m.rows {
		if r.Build == build && r.Module == module && r.Gate == gate &&
			r.Commit == commit && r.Attempt == attempt {
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
	r, err := runner(store).Run(context.Background(), "b1", "MOD-a", "test", "c1", t.TempDir(), allPassing(), 0)
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
	r, err := runner(store).Run(context.Background(), "b1", "MOD-a", "structure", "c1", t.TempDir(), cmds, 0)
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
	r, err := runner(store).Run(context.Background(), "b1", "MOD-a", "branch-coverage", "c1", t.TempDir(), cmds, 0)
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
	results, err := runner(store).RunChain(context.Background(), "b1", "MOD-a", "c1", "D1", t.TempDir(), cmds, 0)
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
	results, err := runner(store).RunChain(context.Background(), "b1", "MOD-a", "c1", "D1", t.TempDir(), allPassing(), 0)
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
		if _, err := r.Run(context.Background(), "b1", "MOD-a", "test", "c1", t.TempDir(), allPassing(), 0); err != nil {
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
	if _, err := runner(store).Run(context.Background(), "b1", "MOD-a", "test", "old-commit", t.TempDir(), allPassing(), 0); err != nil {
		t.Fatalf("run: %v", err)
	}
	passed, err := store.Passed(context.Background(), "b1", "MOD-a", "test", "new-commit", 0)
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
	if _, err := runner(store).Run(context.Background(), "b1", "MOD-a", "mutation", "c1", t.TempDir(), cmds, 0); err == nil {
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
	got, err := r.Run(context.Background(), "b1", "MOD-a", "test", "c1", t.TempDir(), cmds, 0)
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
		map[string]string{"test": "printf '%064d' 0"}, 0)
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
		map[string]string{"test": "printf '%063d' 0; echo VERDICT"}, 0)
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

func TestTheReviewGatesOnlyMutation(t *testing.T) {
	// The other five tell the dev agent what to fix. Waiting for a review
	// before running them would stall the loop at the point it most needs
	// their output.
	store := &memStore{}
	review := &noReview{}
	runner := gates.Runner{Store: store, Review: review, Now: fakes.NewClock(time.Unix(0, 0))}
	results, err := runner.RunChain(context.Background(), "run-1", "MOD-a", "sha-a", "D1",
		t.TempDir(), allPassing(), 0)
	if err != nil {
		t.Fatalf("run chain: %v", err)
	}
	var ran []string
	for _, r := range results {
		ran = append(ran, r.Gate)
	}
	want := []string{"test", "structure", "typing", "arch", "branch-coverage"}
	if strings.Join(ran, ",") != strings.Join(want, ",") {
		t.Fatalf("ran %v, want the five that precede the review check", ran)
	}
	if len(review.asked) != 1 {
		t.Errorf("the review was consulted %d times, want once — before mutation", len(review.asked))
	}
}

// noReview reports that nothing has passed review.
type noReview struct{ asked []string }

func (n *noReview) PassedAt(_ context.Context, _, commit string) (bool, error) {
	n.asked = append(n.asked, commit)
	return false, nil
}

func TestAReviewThatCannotBeReadStopsTheChain(t *testing.T) {
	// "I could not read the verdict" is not "the review passed".
	store := &memStore{}
	runner := gates.Runner{
		Store: store, Review: brokenReview{}, Now: fakes.NewClock(time.Unix(0, 0)),
	}
	_, err := runner.RunChain(context.Background(), "run-1", "MOD-a", "sha-a", "D1",
		t.TempDir(), allPassing(), 0)
	if err == nil {
		t.Fatal("an unreadable review verdict was treated as a pass")
	}
}

type brokenReview struct{}

func (brokenReview) PassedAt(context.Context, string, string) (bool, error) {
	return false, errors.New("store unavailable")
}

func TestAnUnknownGateIsRefused(t *testing.T) {
	// The chain naming a gate nothing maps to a command is a defect, and
	// running nothing would record a pass for work never checked.
	_, err := runner(&memStore{}).Run(context.Background(), "b1", "MOD-a", "vibes",
		"c1", t.TempDir(), allPassing(), 0)
	if err == nil {
		t.Fatal("an unknown gate was run")
	}
}

func TestAModuleMissingAGateCommandIsADefect(t *testing.T) {
	// Intake refuses a module missing any of the six, so reaching here means
	// the snapshot and the chain disagree.
	cmds := allPassing()
	delete(cmds, "coverage")
	_, err := runner(&memStore{}).Run(context.Background(), "b1", "MOD-a", "branch-coverage",
		"c1", t.TempDir(), cmds, 0)
	if err == nil {
		t.Fatal("a module with no coverage command was gated anyway")
	}
}

func TestAnUnrunnableCommandIsAnErrorNotAFailure(t *testing.T) {
	// A gate that could not run and a gate that ran and failed are different
	// outcomes: the first is a broken build engine, the second is work to do.
	_, err := runner(&memStore{}).Run(context.Background(), "b1", "MOD-a", "test",
		"c1", filepath.Join(t.TempDir(), "absent"), allPassing(), 0)
	if err == nil {
		t.Fatal("a gate run in a directory that does not exist reported a result")
	}
}

func TestAResultThatCannotBeRecordedIsAFailure(t *testing.T) {
	// A gate whose result vanished is one the chain cannot read, and the run
	// would re-run it forever.
	_, err := gates.Runner{Store: failingGateStore{}, Now: fakes.NewClock(time.Unix(0, 0))}.
		Run(context.Background(), "b1", "MOD-a", "test", "c1", t.TempDir(), allPassing(), 0)
	if err == nil {
		t.Fatal("a result that could not be recorded read as recorded")
	}
}

// failingGateStore refuses every write and read.
type failingGateStore struct{}

func (failingGateStore) Upsert(context.Context, gates.Result) error {
	return errors.New("disk full")
}

func (failingGateStore) Passed(
	context.Context, string, string, string, string, int,
) (bool, error) {
	return false, errors.New("store unavailable")
}

func TestAnUnreadableGateResultStopsAllPassed(t *testing.T) {
	runner := gates.Runner{Store: failingGateStore{}, Now: fakes.NewClock(time.Unix(0, 0))}
	if _, _, err := runner.AllPassed(context.Background(), "b1", "MOD-a", "c1", 0); err == nil {
		t.Fatal("an unreadable gate result read as a pass")
	}
}

func TestAllPassedNamesTheEarliestGap(t *testing.T) {
	// A caller told "mutation" when test also failed would fix the wrong
	// thing.
	store := &memStore{}
	r := runner(store)
	for _, gate := range []string{"test", "structure"} {
		if _, err := r.Run(context.Background(), "b1", "MOD-a", gate, "c1",
			t.TempDir(), allPassing(), 0); err != nil {
			t.Fatalf("run %s: %v", gate, err)
		}
	}
	passed, missing, err := r.AllPassed(context.Background(), "b1", "MOD-a", "c1", 0)
	if err != nil {
		t.Fatalf("all passed: %v", err)
	}
	if passed {
		t.Error("a chain with four gates outstanding reported as complete")
	}
	if missing != "typing" {
		t.Errorf("named %q as the gap, want the earliest one", missing)
	}
}

func TestAMissingReviewIsReportedAsTheGap(t *testing.T) {
	// The review comes FIRST in the constitution's chain, so a missing one is
	// the gap even when every gate result is present.
	store := &memStore{}
	r := gates.Runner{Store: store, Review: &noReview{}, Now: fakes.NewClock(time.Unix(0, 0))}
	for _, gate := range gates.Chain {
		if _, err := r.Run(context.Background(), "b1", "MOD-a", gate, "c1",
			t.TempDir(), allPassing(), 0); err != nil {
			t.Fatalf("run %s: %v", gate, err)
		}
	}
	passed, missing, err := r.AllPassed(context.Background(), "b1", "MOD-a", "c1", 0)
	if err != nil {
		t.Fatalf("all passed: %v", err)
	}
	if passed || missing != "review" {
		t.Errorf("reported passed=%v missing=%q", passed, missing)
	}
}

func TestEveryGatePassedIsACompleteChain(t *testing.T) {
	store := &memStore{}
	r := runner(store)
	for _, gate := range gates.Chain {
		if _, err := r.Run(context.Background(), "b1", "MOD-a", gate, "c1",
			t.TempDir(), allPassing(), 0); err != nil {
			t.Fatalf("run %s: %v", gate, err)
		}
	}
	passed, missing, err := r.AllPassed(context.Background(), "b1", "MOD-a", "c1", 0)
	if err != nil {
		t.Fatalf("all passed: %v", err)
	}
	if !passed || missing != "" {
		t.Errorf("reported passed=%v missing=%q", passed, missing)
	}
}

// movingBase reports a different head after n reads.
type movingBase struct {
	head  string
	moved string
	after int
	reads int
	err   error
}

func (m *movingBase) Head(context.Context) (string, error) {
	m.reads++
	if m.err != nil {
		return "", m.err
	}
	if m.after > 0 && m.reads > m.after {
		return m.moved, nil
	}
	return m.head, nil
}

func TestAChainAbortsWhenItsBaseMovesMidRun(t *testing.T) {
	// A chain whose ground moved between the third and fourth gate would
	// otherwise record a mixed-base pass — some results against the old base,
	// some against the new — which says nothing about either.
	store := &memStore{}
	base := &movingBase{head: "D1", moved: "D2", after: 3}
	r := gates.Runner{Store: store, Base: base, Now: fakes.NewClock(time.Unix(0, 0))}
	// The work commit is C2 and the frozen base is D1: distinct, because a
	// runner that checked the COMMIT against the default head would report
	// "moved" on every chain that has any work in it.
	results, err := r.RunChain(context.Background(), "b1", "MOD-a", "C2", "D1",
		t.TempDir(), allPassing(), 1)
	if !errors.Is(err, gates.ErrBaseMoved) {
		t.Fatalf("got %v, want ErrBaseMoved", err)
	}
	if len(results) != 3 {
		t.Errorf("ran %d gates before aborting", len(results))
	}
	if len(store.rows) != 3 {
		t.Errorf("recorded %d results for an aborted chain", len(store.rows))
	}
}

func TestAChainOnAStableBaseRunsThrough(t *testing.T) {
	store := &memStore{}
	base := &movingBase{head: "D1"}
	r := gates.Runner{Store: store, Base: base, Now: fakes.NewClock(time.Unix(0, 0))}
	results, err := r.RunChain(context.Background(), "b1", "MOD-a", "C2", "D1",
		t.TempDir(), allPassing(), 1)
	if err != nil {
		t.Fatalf("run chain: %v", err)
	}
	if len(results) != len(gates.Chain) {
		t.Errorf("ran %d gates", len(results))
	}
	// Checked BEFORE each gate, not once at the start.
	if base.reads != len(gates.Chain) {
		t.Errorf("read the head %d times for %d gates", base.reads, len(gates.Chain))
	}
}

func TestAnUnreadableBaseStopsTheChain(t *testing.T) {
	// "I could not read the head" is not "the head has not moved".
	store := &memStore{}
	base := &movingBase{err: errors.New("git unavailable")}
	r := gates.Runner{Store: store, Base: base, Now: fakes.NewClock(time.Unix(0, 0))}
	if _, err := r.RunChain(context.Background(), "b1", "MOD-a", "C2", "D1",
		t.TempDir(), allPassing(), 1); err == nil {
		t.Fatal("an unreadable head read as an unmoved one")
	}
	if len(store.rows) != 0 {
		t.Error("a gate ran without knowing where the base stood")
	}
}

func TestAPriorAttemptsPassSatisfiesNothing(t *testing.T) {
	// Integrating a new base can leave the branch head unchanged: the commit
	// is the same, the ground it sits on is not.
	store := &memStore{}
	r := runner(store)
	for _, gate := range gates.Chain {
		if _, err := r.Run(context.Background(), "b1", "MOD-a", gate, "c1",
			t.TempDir(), allPassing(), 1); err != nil {
			t.Fatalf("run %s: %v", gate, err)
		}
	}
	passed, _, err := r.AllPassed(context.Background(), "b1", "MOD-a", "c1", 1)
	if err != nil {
		t.Fatalf("all passed: %v", err)
	}
	if !passed {
		t.Fatal("attempt 1's own results did not satisfy it")
	}
	passed, missing, err := r.AllPassed(context.Background(), "b1", "MOD-a", "c1", 2)
	if err != nil {
		t.Fatalf("all passed: %v", err)
	}
	if passed {
		t.Error("attempt 1's results satisfied attempt 2 at the same commit")
	}
	if missing != "test" {
		t.Errorf("named %q as the gap", missing)
	}
}
