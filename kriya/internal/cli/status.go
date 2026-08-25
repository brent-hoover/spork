package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// StatusReport is everything the status command shows.
//
// PLAIN DATA, assembled by the composition root. The command surface may not
// import the gate runner, and it should not need to: what an operator wants is
// a picture, and gathering it is the one place that knows every store.
type StatusReport struct {
	Targets []TargetStatus `json:"targets"`
	Stalls  []StallStatus  `json:"stalls,omitempty"`
}

// TargetStatus is one build target and what is happening to it.
type TargetStatus struct {
	TargetKey string `json:"target_key"`
	Epic      string `json:"epic,omitempty"`
	// Plan is the decomposition's state. A target whose plan is still
	// decomposing has a ticket set that is still growing.
	Plan       string           `json:"plan,omitempty"`
	Tickets    int              `json:"tickets"`
	Runs       []RunStatus      `json:"runs,omitempty"`
	Completion CompletionStatus `json:"completion"`
}

// RunStatus is one BuildRun's position.
type RunStatus struct {
	Ticket string `json:"ticket"`
	State  string `json:"state"`
	// Attempt is the gate-chain round. Results pin to it, so a run on attempt
	// three has had two chains fail.
	Attempt int       `json:"attempt"`
	Started time.Time `json:"started,omitzero"`
	Review  string    `json:"review,omitempty"`
	// Gates is how far through the chain this attempt got. The run's state
	// says it is IN the gates; this says which ones it has passed.
	Gates []GateStatus `json:"gates,omitempty"`
}

// GateStatus is one gate's outcome for a run's current attempt.
type GateStatus struct {
	Gate   string `json:"gate"`
	Passed bool   `json:"passed"`
}

// CompletionStatus is a target's durable completion declaration.
//
// The EPOCH is part of it: a build that completed and then had work return is
// not the same as one that never completed, and the epoch is what tells them
// apart.
type CompletionStatus struct {
	Complete bool `json:"complete"`
	// State is the claim's lifecycle position when it is not yet complete —
	// submitting, submitted, closing — so an operator can see a build waiting
	// on them rather than one that has stopped.
	State         string `json:"state,omitempty"`
	Epoch         int    `json:"epoch"`
	Review        string `json:"review,omitempty"`
	ReportVersion string `json:"report_version,omitempty"`
	// ReopenOwed reports a close whose outcome nothing resolved: the epic may
	// be closed over work that returned, and a compensating reopen is due.
	ReopenOwed bool `json:"reopen_owed,omitempty"`
}

// StallStatus is a build that can neither proceed nor finish.
type StallStatus struct {
	TargetKey string `json:"target_key"`
	Cause     string `json:"cause"`
}

// Status renders the report, as text or as JSON.
//
// The SAME state either way. A script and a human reading different things
// about one build is how an operator ends up debugging the tool instead of the
// build.
func Status(out io.Writer, report StatusReport, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return fmt.Errorf("encode status: %w", err)
		}
		return nil
	}
	return writeText(out, report)
}

// writeText renders the operator's view.
func writeText(out io.Writer, report StatusReport) error {
	w := &errWriter{w: out}
	if len(report.Targets) == 0 {
		// Nothing at all reads as a broken command; "no builds" reads as an
		// answer.
		w.printf("no builds\n")
		return w.err
	}
	for _, target := range report.Targets {
		writeTarget(w, target)
	}
	if len(report.Stalls) > 0 {
		w.printf("\nstalled:\n")
		for _, stall := range report.Stalls {
			w.printf("  %s — %s\n", stall.TargetKey, stall.Cause)
		}
	}
	return w.err
}

// writeTarget renders one target and its runs.
func writeTarget(w *errWriter, target TargetStatus) {
	w.printf("%s\n", target.TargetKey)
	w.printf("  plan: %s (%d tickets)\n", planOf(target), target.Tickets)
	w.printf("  completion: %s\n", completionOf(target.Completion))
	if len(target.Runs) == 0 {
		w.printf("  no runs\n")
		return
	}
	for _, run := range target.Runs {
		writeRun(w, run)
	}
}

// writeRun renders one run's position, including how far through the gates it
// got — "in gates" says nothing about that on its own.
func writeRun(w *errWriter, run RunStatus) {
	w.printf("  %s\n", run.Ticket)
	w.printf("    state: %s (attempt %d)%s\n", run.State, run.Attempt, startedAt(run))
	if run.Review != "" {
		w.printf("    review: %s\n", run.Review)
	}
	for _, gate := range run.Gates {
		w.printf("    gate %-15s %s\n", gate.Gate, passedOf(gate.Passed))
	}
}

// planOf names a plan's state, or says there is none.
func planOf(target TargetStatus) string {
	if target.Plan == "" {
		return "none"
	}
	return target.Plan
}

// completionOf renders a target's durable declaration.
func completionOf(c CompletionStatus) string {
	switch {
	case c.Complete && c.ReopenOwed:
		return fmt.Sprintf("complete at epoch %d, REOPEN OWED — review %s", c.Epoch, c.Review)
	case c.Complete:
		return fmt.Sprintf("complete at epoch %d — review %s", c.Epoch, c.Review)
	case c.State != "":
		return fmt.Sprintf("%s at epoch %d", c.State, c.Epoch)
	default:
		return fmt.Sprintf("not claimed (epoch %d)", c.Epoch)
	}
}

// startedAt renders a start time, or nothing when the run has none.
//
// Nothing rather than year one: a run recorded before the column existed has
// no start time, and printing the zero instant would be a lie about it.
func startedAt(run RunStatus) string {
	if run.Started.IsZero() {
		return ""
	}
	return ", started " + run.Started.UTC().Format(time.RFC3339)
}

func passedOf(passed bool) string {
	if passed {
		return "passed"
	}
	return "FAILED"
}
