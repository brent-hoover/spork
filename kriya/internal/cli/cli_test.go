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
	if err := os.WriteFile(filepath.Join(dir, "avspec.yaml"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	v := fakes.NewVerifier(dir, r)
	v.Models = map[string]specverify.Model{dir: {
		OK:        true,
		Modules:   []specverify.Module{completeModule()},
		Artifacts: []string{"avspec.yaml"},
	}}
	var out bytes.Buffer
	in := planner.Intaker{Verify: v, Snapshots: newStore(), Now: fakes.NewClock(time.Unix(0, 0))}
	err := cli.Build(context.Background(), &out, in, dir)
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
