package cli_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kriya/internal/cli"
	"kriya/internal/fakes"
	"kriya/internal/planner"
	"kriya/internal/specverify"
)

// completeModule declares all six gates, so intake's command check passes and
// these tests exercise only what they are about.
// store is a minimal SnapshotStore; these tests are about cli output, not
// persistence, so it records without asserting.
type store struct{ n int }

func newStore() *store { return &store{} }

func (s *store) Put(context.Context, planner.Snapshot) error { s.n++; return nil }
func (s *store) Get(context.Context, string) (planner.Snapshot, error) {
	return planner.Snapshot{}, errors.New("not needed")
}
func (s *store) Count(context.Context) (int, error) { return s.n, nil }

// targets and stubTracker keep these tests about cli output; the epic path
// has its own tests in planner.
type targets struct {
	rows map[string]planner.BuildTarget
}

func newTargets() *targets { return &targets{rows: map[string]planner.BuildTarget{}} }

func (t *targets) Upsert(_ context.Context, b planner.BuildTarget) error {
	t.rows[b.TargetKey] = b
	return nil
}

func (t *targets) Find(_ context.Context, key string) (planner.BuildTarget, bool, error) {
	b, ok := t.rows[key]
	return b, ok, nil
}

func (t *targets) Pending(context.Context) ([]planner.BuildTarget, error) { return nil, nil }

type stubTracker struct{}

func (stubTracker) CreateProject(context.Context, string, string, string, string) (string, error) {
	return "project-1", nil
}

func (stubTracker) AddRelation(context.Context, string, string, string, string, string) error {
	return nil
}

func (stubTracker) CreateIssue(context.Context, string, string, string, string, string) (string, error) {
	return "issue-1", nil
}

// attempts is a minimal AttemptStore; these tests are about cli output.
type attempts struct{ n int }

func newAttempts() *attempts { return &attempts{} }

func (a *attempts) Reserve(context.Context, string, string) (planner.IntakeAttempt, error) {
	a.n++
	return planner.IntakeAttempt{Generation: a.n}, nil
}
func (a *attempts) Complete(context.Context, string, string) error     { return nil }
func (a *attempts) MapSpec(context.Context, planner.SpecMapping) error { return nil }
func (a *attempts) Mapping(context.Context, string) (planner.SpecMapping, bool, error) {
	return planner.SpecMapping{}, false, nil
}

func completeModule() specverify.Module {
	cmds := map[string]string{}
	for _, name := range specverify.RequiredCommands {
		cmds[name] = "run-" + name
	}
	return specverify.Module{ID: "MOD-a", Name: "a", Commands: cmds}
}

func run(t *testing.T, r specverify.Report) (string, error) {
	t.Helper()
	dir := t.TempDir()
	// The manifest must carry a criterion id: decomposition validates every
	// ticket's citations against the snapshot, so a snapshot with none has
	// nothing a ticket could legitimately cite.
	manifest := "requirements:\n  - id: REQ-x\n    acceptance:\n      - id: AC-x\n"
	if err := os.WriteFile(filepath.Join(dir, "avspec.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	v := fakes.NewVerifier(dir, r)
	v.Models = map[string]specverify.Model{dir: {
		OK:        true,
		Modules:   []specverify.Module{completeModule()},
		Artifacts: []string{"avspec.yaml"},
	}}
	var out bytes.Buffer
	in := planner.Intaker{
		Verify:    v,
		Snapshots: newStore(),
		Attempts:  newAttempts(),
		Targets:   newTargets(),
		Tracker:   stubTracker{},
		Agent:     fakes.NewAgent(`{"tickets":[{"title":"walking skeleton","body":"b","criteria":["AC-x"]}]}`),
		Now:       fakes.NewClock(time.Unix(0, 0)),
	}
	err := cli.Build(context.Background(), &out, in, dir, "actor-1", "token-1")
	return out.String(), err
}

func TestBuildReportsAReadySpec(t *testing.T) {
	out, err := run(t, specverify.Report{Status: "ready", OK: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "ready") {
		t.Errorf("output should say the spec is ready, got %q", out)
	}
}

func TestBuildPrintsTheFindingsBehindARefusal(t *testing.T) {
	out, err := run(t, specverify.Report{
		Status: "draft", OK: true,
		Counts: specverify.Counts{Todo: 1},
		Findings: []specverify.Finding{
			{Code: "REQ_ABSENT", Severity: "todo", Message: "no requirements yet"},
		},
	})
	if err == nil {
		t.Fatal("a refusal must be returned, not swallowed")
	}
	// AC-intake-refuse: refused WITH the verify findings. A bare status line
	// would leave the operator nothing to act on.
	for _, want := range []string{"refused", "REQ_ABSENT", "no requirements yet", "todo"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q; got:\n%s", want, out)
		}
	}
}
