package specverify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Severity levels avspec reports. `error` always blocks; `todo` blocks only
// at status ready or built; `warn` never blocks.
const (
	SeverityError = "error"
	SeverityTodo  = "todo"
	SeverityWarn  = "warn"
)

// Finding is one thing avspec has to say about a spec.
type Finding struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Ref      string `json:"ref"`
}

// Counts is avspec's per-severity tally.
type Counts struct {
	Error int `json:"error"`
	Todo  int `json:"todo"`
	Warn  int `json:"warn"`
}

// Report is avspec's verdict, reported faithfully and judged elsewhere.
//
// OK is NOT "ready". A draft spec verifies OK=true with todo findings and
// exits 0, because todos do not block at status draft. AC-intake-refuse
// requires kriya to refuse a draft, so the intake decision must read Status
// and Counts — trusting OK, or the exit code, accepts drafts. Whether a spec
// is acceptable is the planner's judgment; this package only reports.
type Report struct {
	Status   string    `json:"status"`
	OK       bool      `json:"ok"`
	Counts   Counts    `json:"counts"`
	Findings []Finding `json:"findings"`
}

// Verifier runs the spec verifier over a directory.
type Verifier interface {
	Verify(ctx context.Context, dir string) (Report, error)
	Resolve(ctx context.Context, dir string) (Model, error)
}

// CLI shells out to the avspec command.
//
// Argv is the command prefix, so a caller can supply an interpreter — this
// repo runs avspec under uv, and hardcoding that would tie kriya to one
// installation. WorkDir is where the command runs, which matters because uv
// resolves its environment from the working directory.
type CLI struct {
	Argv    []string
	WorkDir string
}

// Verify runs `avspec verify <dir> --json` and parses its report.
//
// A refusal is not an error. avspec exits 0 when OK and 1 when not, and both
// emit a parseable report — a spec kriya must decline is the ordinary case,
// not a malfunction. Every other outcome IS an error: a signal (ExitCode
// -1), any exit code other than 0 or 1, a command that will not start, or
// output that does not parse. Treating those as reports would let a crashed
// or missing verifier read as a clean refusal.
func (c CLI) Verify(ctx context.Context, dir string) (Report, error) {
	out, err := c.run(ctx, "verify", dir, "--json")
	if err != nil {
		return Report{}, err
	}

	// Required fields are pointers so a payload MISSING them is rejected rather
	// than silently zero-valued. Syntactically valid JSON like
	// {"status":"ready","ok":true} would otherwise be admitted with no counts
	// and no findings — an incompatible or half-broken verifier failing OPEN,
	// which is the one direction this seam must never fail.
	var raw struct {
		Status   *string    `json:"status"`
		OK       *bool      `json:"ok"`
		Counts   *Counts    `json:"counts"`
		Findings *[]Finding `json:"findings"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return Report{}, fmt.Errorf("specverify: parse report: %w", err)
	}
	switch {
	case raw.Status == nil:
		return Report{}, errors.New("specverify: report has no status")
	case raw.OK == nil:
		return Report{}, errors.New("specverify: report has no ok")
	case raw.Counts == nil:
		return Report{}, errors.New("specverify: report has no counts")
	case raw.Findings == nil:
		return Report{}, errors.New("specverify: report has no findings")
	}
	return Report{
		Status:   *raw.Status,
		OK:       *raw.OK,
		Counts:   *raw.Counts,
		Findings: *raw.Findings,
	}, nil
}
