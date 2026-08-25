package cli_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"kriya/internal/cli"
)

func report() cli.StatusReport {
	return cli.StatusReport{
		Targets: []cli.TargetStatus{
			{
				TargetKey: "/linkshort", Epic: "epic-1", Plan: "completed", Tickets: 3,
				Runs: []cli.RunStatus{
					{
						Ticket: "Create a short link", State: "gates", Attempt: 2,
						Started: time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC),
						Gates: []cli.GateStatus{
							{Gate: "test", Passed: true},
							{Gate: "structure", Passed: true},
							{Gate: "typing", Passed: false},
						},
					},
					{
						Ticket: "Redirect", State: "review-submitted", Attempt: 1,
						Started: time.Date(2026, 8, 24, 9, 30, 0, 0, time.UTC),
						Review:  "review-4",
					},
				},
			},
			{
				TargetKey: "/paste", Epic: "epic-2", Plan: "completed", Tickets: 2,
				Completion: cli.CompletionStatus{
					Complete: true, Epoch: 3, Review: "review-9", ReportVersion: "ver-2",
				},
			},
		},
		Stalls: []cli.StallStatus{
			{TargetKey: "/paste", Cause: "outstanding work: issue-9 (unplanned, blocked)"},
		},
	}
}

func TestTheStatusTextShowsBuildsRunsAndGatePositions(t *testing.T) {
	var out bytes.Buffer
	if err := cli.Status(&out, report(), false); err != nil {
		t.Fatalf("status: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"/linkshort", "Create a short link", "gates", "attempt 2",
		// The gate POSITION, not just the run's state: "in gates" says
		// nothing about how far through them it is.
		"test", "structure", "typing",
		"Redirect", "review-submitted", "review-4",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the status omits %q:\n%s", want, got)
		}
	}
}

func TestACompletedBuildShowsItsDurableDeclaration(t *testing.T) {
	// "including the completed build's durable completion declaration". A
	// build that says only "no runs" is indistinguishable from one that never
	// started.
	var out bytes.Buffer
	if err := cli.Status(&out, report(), false); err != nil {
		t.Fatalf("status: %v", err)
	}
	got := out.String()
	for _, want := range []string{"/paste", "complete", "epoch 3", "review-9"} {
		if !strings.Contains(got, want) {
			t.Errorf("the completion declaration omits %q:\n%s", want, got)
		}
	}
}

func TestAStallIsShownWithItsCause(t *testing.T) {
	var out bytes.Buffer
	if err := cli.Status(&out, report(), false); err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(out.String(), "issue-9") {
		t.Errorf("the stall's cause is missing:\n%s", out.String())
	}
}

func TestTheJSONFormCarriesTheSameState(t *testing.T) {
	// "the same state is emitted machine-readably for agents and scripts" —
	// the SAME state, so a script and a human never disagree about a build.
	var out bytes.Buffer
	if err := cli.Status(&out, report(), true); err != nil {
		t.Fatalf("status: %v", err)
	}
	var got cli.StatusReport
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("the JSON form does not parse: %v\n%s", err, out.String())
	}
	if len(got.Targets) != 2 {
		t.Fatalf("decoded %d targets", len(got.Targets))
	}
	if got.Targets[0].Runs[0].Ticket != "Create a short link" {
		t.Errorf("decoded %+v", got.Targets[0].Runs[0])
	}
	if len(got.Targets[0].Runs[0].Gates) != 3 {
		t.Errorf("the gate positions did not survive: %+v", got.Targets[0].Runs[0].Gates)
	}
	if !got.Targets[1].Completion.Complete || got.Targets[1].Completion.Epoch != 3 {
		t.Errorf("the completion declaration did not survive: %+v", got.Targets[1].Completion)
	}
	if len(got.Stalls) != 1 {
		t.Errorf("the stalls did not survive: %+v", got.Stalls)
	}
}

func TestAnEmptyStatusSaysSoRatherThanPrintingNothing(t *testing.T) {
	// Nothing at all reads as a broken command. "No builds" reads as an
	// answer.
	var out bytes.Buffer
	if err := cli.Status(&out, cli.StatusReport{}, false); err != nil {
		t.Fatalf("status: %v", err)
	}
	if strings.TrimSpace(out.String()) == "" {
		t.Error("an empty status printed nothing at all")
	}
}

func TestTheEmptyJSONFormIsStillValidJSON(t *testing.T) {
	// A script parsing it must not have to special-case "no builds".
	var out bytes.Buffer
	if err := cli.Status(&out, cli.StatusReport{}, true); err != nil {
		t.Fatalf("status: %v", err)
	}
	var got cli.StatusReport
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("the empty JSON form does not parse: %v\n%s", err, out.String())
	}
}

func TestARunWithNoStartTimeSaysSoRatherThanYearOne(t *testing.T) {
	// A run recorded before the column existed has no start time. Printing
	// year one would be a lie about it.
	r := cli.StatusReport{Targets: []cli.TargetStatus{{
		TargetKey: "/t", Runs: []cli.RunStatus{{Ticket: "T", State: "queued"}},
	}}}
	var out bytes.Buffer
	if err := cli.Status(&out, r, false); err != nil {
		t.Fatalf("status: %v", err)
	}
	if strings.Contains(out.String(), "0001-01-01") {
		t.Errorf("an unset start time printed as year one:\n%s", out.String())
	}
}
