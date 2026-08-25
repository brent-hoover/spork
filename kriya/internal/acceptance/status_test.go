//go:build acceptance

package acceptance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cucumber/godog"

	"kriya/internal/cli"
)

// statusWorld is the CLI-status scenario's state.
type statusWorld struct {
	report cli.StatusReport
	text   string
	json   string
}

func (w *world) newStatus() *statusWorld {
	s := &statusWorld{}
	w.status = s
	return s
}

func registerStatus(sc *godog.ScenarioContext, w *world) {
	sc.Step(`^a build with runs in flight and a completed build alongside it$`, func() error {
		s := w.newStatus()
		s.report = cli.StatusReport{
			Targets: []cli.TargetStatus{
				{
					TargetKey: "/linkshort", Epic: "epic-1",
					Plan: "completed", Tickets: 2,
					Runs: []cli.RunStatus{{
						Ticket: "Create a short link", State: "gates", Attempt: 2,
						Started: time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC),
						Gates: []cli.GateStatus{
							{Gate: "test", Passed: true},
							{Gate: "structure", Passed: true},
							{Gate: "typing", Passed: false},
						},
					}},
				},
				{
					TargetKey: "/paste", Epic: "epic-2", Plan: "completed", Tickets: 1,
					Completion: cli.CompletionStatus{
						Complete: true, Epoch: 3,
						Review: "review-9", ReportVersion: "ver-2",
					},
				},
			},
		}
		return nil
	})

	sc.Step(`^the operator runs the status command$`, func() error {
		var out bytes.Buffer
		if err := cli.Status(&out, w.status.report, false); err != nil {
			return err
		}
		w.status.text = out.String()
		return nil
	})

	sc.Step(`^builds, runs, and their gate positions are shown, including the completed build's durable completion declaration$`,
		func() error {
			got := w.status.text
			for _, want := range []string{
				"/linkshort", "Create a short link", "gates", "attempt 2",
				// The gate POSITION. "In gates" says nothing about how far
				// through them the run got, which is the operator's question.
				"test", "structure", "typing",
				// The completed build's DURABLE declaration — a target with
				// no runs is otherwise indistinguishable from one that never
				// started.
				"/paste", "complete", "epoch 3", "review-9",
			} {
				if !strings.Contains(got, want) {
					return fmt.Errorf("the status omits %q:\n%s", want, got)
				}
			}
			return nil
		})

	sc.Step(`^the status command runs with --json$`, func() error {
		var out bytes.Buffer
		if err := cli.Status(&out, w.status.report, true); err != nil {
			return err
		}
		w.status.json = out.String()
		return nil
	})

	sc.Step(`^the same state is emitted machine-readably for agents and scripts$`,
		func() error {
			s := w.status
			var got cli.StatusReport
			if err := json.Unmarshal([]byte(s.json), &got); err != nil {
				return fmt.Errorf("the JSON form does not parse: %w\n%s", err, s.json)
			}
			// The SAME state. A script and a human disagreeing about a build
			// is how an operator ends up debugging the tool instead.
			if len(got.Targets) != len(s.report.Targets) {
				return fmt.Errorf("decoded %d targets, rendered %d",
					len(got.Targets), len(s.report.Targets))
			}
			if len(got.Targets[0].Runs[0].Gates) != 3 {
				return errors.New("the gate positions did not survive the JSON form")
			}
			if !got.Targets[1].Completion.Complete || got.Targets[1].Completion.Epoch != 3 {
				return fmt.Errorf("the completion declaration came back %+v",
					got.Targets[1].Completion)
			}
			return nil
		})
}
