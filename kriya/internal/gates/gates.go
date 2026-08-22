// Package gates runs a target module's quality gates.
//
// This package does not know what a linter or a coverage tool is. It resolves
// the module's command from the pinned SpecSnapshot, runs it at the head
// commit, and records the outcome. It counts no arms and infers no threshold —
// the passing bar is the TARGET's to declare in its own command.
package gates

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"

	"kriya/internal/clock"
)

// The gate chain, in the order CON-gate-chain fixes it.
//
// test first because coverage is judged only over a passing suite; mutation
// last because it is the most expensive and only meaningful once everything
// else holds.
var Chain = []string{"test", "structure", "typing", "arch", "coverage", "mutation"}

// commandFor maps a gate to the stack command that implements it.
//
// structure and typing are the spec's names for what the stack calls lint and
// typecheck; GateResult records the GATE name, so a result stays readable
// against the requirement rather than against one project's tooling.
var commandFor = map[string]string{
	"test": "test", "structure": "lint", "typing": "typecheck",
	"arch": "arch", "coverage": "coverage", "mutation": "mutation",
}

// Result is one gate's outcome, pinned to the commit it ran at.
type Result struct {
	Build  string
	Module string
	Gate   string
	Commit string
	Passed bool
	// Detail carries the tool's own output. AC-coverage-recorded wants the
	// uncovered arms "named in detail for the dev agent", and they are IN that
	// output — branch-coverage.sh prints them — so the requirement is met by
	// capture rather than by parsing. Parsing would also mean kriya deciding
	// what a coverage tool means, which AC-coverage-floor forbids.
	Detail  json.RawMessage
	Attempt int
}

// Store persists gate results.
type Store interface {
	// Upsert records a result, replacing any for the same build, module, gate
	// and attempt. Reruns must never duplicate.
	Upsert(ctx context.Context, r Result) error
	// Passed reports whether a gate passed at this exact commit. A stale pass
	// from an older commit never satisfies the chain.
	Passed(ctx context.Context, build, module, gate, commit string) (bool, error)
}

// detail is the envelope stored on a Result.
type detail struct {
	ExitCode int    `json:"exit_code"`
	Command  string `json:"command"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	Duration string `json:"duration"`
}

// Runner executes gates against a workspace.
type Runner struct {
	Store Store
	Now   clock.Clock
	// Limit caps captured output. A mutation run can print megabytes, and a
	// result nobody can open is a result nobody reads.
	Limit int
}

// Run executes one gate and records the outcome.
//
// A failure is a RESULT, not an error: a gate that fails is the chain working,
// and returning it as an error would make "the linter found something" look
// like "the linter would not start". Only the second is an error.
func (r Runner) Run(ctx context.Context, build, module, gate, commit, dir string, commands map[string]string) (Result, error) {
	name, ok := commandFor[gate]
	if !ok {
		return Result{}, fmt.Errorf("unknown gate %q", gate)
	}
	command := commands[name]
	if command == "" {
		// Intake refuses a module missing any of the six, so reaching here
		// means the snapshot and the chain disagree — a defect, not a failing
		// gate.
		return Result{}, fmt.Errorf("module %s resolved no %s command", module, name)
	}

	started := r.Now.Now()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = dir
	out, runErr := cmd.Output()

	var stderr []byte
	var exitCode int
	var ee *exec.ExitError
	switch {
	case runErr == nil:
	case asExitError(runErr, &ee):
		stderr, exitCode = ee.Stderr, ee.ExitCode()
	default:
		// The command could not start at all.
		return Result{}, fmt.Errorf("run %s gate for %s: %w", gate, module, runErr)
	}

	env := detail{
		ExitCode: exitCode, Command: command,
		Stdout:   truncate(string(out), r.limit()),
		Stderr:   truncate(string(stderr), r.limit()),
		Duration: r.Now.Now().Sub(started).String(),
	}
	body, err := json.Marshal(env)
	if err != nil {
		return Result{}, fmt.Errorf("marshal gate detail: %w", err)
	}
	result := Result{
		Build: build, Module: module, Gate: gate, Commit: commit,
		Passed: runErr == nil, Detail: body,
	}
	if err := r.Store.Upsert(ctx, result); err != nil {
		return Result{}, err
	}
	return result, nil
}

func (r Runner) limit() int {
	if r.Limit > 0 {
		return r.Limit
	}
	return 64 << 10
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	// Keep the TAIL: a tool's verdict is at the end, and a head-truncated
	// capture reliably discards the one line the dev agent needs.
	return "…truncated…\n" + s[len(s)-limit:]
}

// Chain runs the gates in order, stopping at the first failure.
//
// Stopping is not an optimisation: coverage is judged only over a passing
// suite, and running mutation against code whose tests fail measures nothing
// while costing the most.
func (r Runner) RunChain(ctx context.Context, build, module, commit, dir string, commands map[string]string) ([]Result, error) {
	var out []Result
	for _, gate := range Chain {
		result, err := r.Run(ctx, build, module, gate, commit, dir, commands)
		if err != nil {
			return out, err
		}
		out = append(out, result)
		if !result.Passed {
			return out, nil
		}
	}
	return out, nil
}
