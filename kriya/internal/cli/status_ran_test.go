package cli_test

import (
	"bytes"
	"strings"
	"testing"

	"kriya/internal/cli"
)

func TestAGateThatHasNotRunDoesNotReadAsFailed(t *testing.T) {
	// A chain stops at its first failure, so the gates after it never ran.
	// Printing them as FAILED claims four more losses that did not happen,
	// and hides where the run actually is.
	r := cli.StatusReport{Targets: []cli.TargetStatus{{
		TargetKey: "/t",
		Runs: []cli.RunStatus{{
			Ticket: "T", State: "gates", Attempt: 1,
			Gates: []cli.GateStatus{
				{Gate: "test", Ran: true, Passed: true},
				{Gate: "structure", Ran: true, Passed: false},
				{Gate: "typing"},
			},
		}},
	}}}
	var out bytes.Buffer
	if err := cli.Status(&out, r, false); err != nil {
		t.Fatalf("status: %v", err)
	}
	got := out.String()
	if strings.Count(got, "FAILED") != 1 {
		t.Errorf("a chain with one failure printed %d:\n%s",
			strings.Count(got, "FAILED"), got)
	}
	if !strings.Contains(got, "not run") {
		t.Errorf("a gate that never ran has no distinct rendering:\n%s", got)
	}
}

func TestTheRanFlagSurvivesTheJSONForm(t *testing.T) {
	// A script deciding whether to wait or to report a failure needs the same
	// distinction a human gets.
	r := cli.StatusReport{Targets: []cli.TargetStatus{{
		TargetKey: "/t",
		Runs:      []cli.RunStatus{{Ticket: "T", Gates: []cli.GateStatus{{Gate: "typing"}}}},
	}}}
	var out bytes.Buffer
	if err := cli.Status(&out, r, true); err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(out.String(), `"ran": false`) {
		t.Errorf("the JSON form cannot tell not-run from failed:\n%s", out.String())
	}
}
