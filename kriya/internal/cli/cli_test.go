package cli_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"kriya/internal/cli"
	"kriya/internal/fakes"
	"kriya/internal/planner"
	"kriya/internal/specverify"
)

// completeModule declares all six gates, so intake's command check passes and
// these tests exercise only what they are about.
func completeModule() specverify.Module {
	cmds := map[string]string{}
	for _, name := range specverify.RequiredCommands {
		cmds[name] = "run-" + name
	}
	return specverify.Module{ID: "MOD-a", Name: "a", Commands: cmds}
}

func run(t *testing.T, r specverify.Report) (string, error) {
	t.Helper()
	var out bytes.Buffer
	v := fakes.NewVerifier("/spec", r).WithModules("/spec", completeModule())
	in := planner.Intaker{Verify: v}
	err := cli.Build(context.Background(), &out, in, "/spec")
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
