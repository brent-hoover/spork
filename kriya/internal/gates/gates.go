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
	"errors"
	"fmt"
	"os/exec"

	"kriya/internal/clock"
)

// The gate chain, in the order CON-gate-chain fixes it.
//
// test first because coverage is judged only over a passing suite; mutation
// last because it is the most expensive and only meaningful once everything
// else holds.
var Chain = []string{"test", "structure", "typing", "arch", "branch-coverage", "mutation"}

// commandFor maps a gate to the stack command that implements it.
//
// structure and typing are the spec's names for what the stack calls lint and
// typecheck; GateResult records the GATE name, so a result stays readable
// against the requirement rather than against one project's tooling.
var commandFor = map[string]string{
	"test": "test", "structure": "lint", "typing": "typecheck",
	"arch": "arch", "branch-coverage": "coverage", "mutation": "mutation",
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
	// Passed reports whether a gate passed at this exact commit UNDER THIS
	// ATTEMPT. Both, because integrating a new base can leave the branch head
	// unchanged: the commit is the same, the ground it sits on is not, and a
	// result from the previous attempt says nothing about the new one.
	Passed(ctx context.Context, build, module, gate, commit string, attempt int) (bool, error)
}

// detail is the envelope stored on a Result.
type detail struct {
	ExitCode int    `json:"exit_code"`
	Command  string `json:"command"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	Duration string `json:"duration"`
}

// Base reports the default branch's current head.
//
// Declared here rather than imported for the same reason ReviewPass is: the
// chain has to be able to ask whether the ground moved under it, and a gate
// package that imported the workspace would invert the dependency the
// composition root keeps flat.
type Base interface {
	Head(ctx context.Context) (string, error)
}

// ReviewPass reports whether the pair loop's review passed at a commit.
//
// Declared here rather than imported: the constitution puts the review pass
// FIRST in the chain, so the gate runner has to be able to ask — but a gate
// package that imported the review bridge would invert the dependency the
// composition root exists to keep flat.
type ReviewPass interface {
	PassedAt(ctx context.Context, run, commit string) (bool, error)
}

// Runner executes gates against a workspace.
type Runner struct {
	Store Store
	Now   clock.Clock
	// Limit caps captured output. A mutation run can print megabytes, and a
	// result nobody can open is a result nobody reads.
	Limit int
	// Review answers whether the pair loop's review passed. Nil runs the
	// chain without that precondition, which is what a module-level test of
	// the other five gates wants.
	Review ReviewPass
	// Base answers where the default branch stands. Nil skips the mid-chain
	// check, which a module-level test of one gate does not need.
	Base Base
}

// ErrBaseMoved reports that the ground moved under a running chain.
//
// A distinct error because the caller acts on it: the run integrates the new
// base and reruns, rather than parking for an operator who can do nothing
// about it.
var ErrBaseMoved = errors.New("the default branch moved mid-chain")

// Run executes one gate and records the outcome.
//
// A failure is a RESULT, not an error: a gate that fails is the chain working,
// and returning it as an error would make "the linter found something" look
// like "the linter would not start". Only the second is an error.
func (r Runner) Run(
	ctx context.Context, build, module, gate, commit, dir string,
	commands map[string]string, attempt int,
) (Result, error) {
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
		Passed: runErr == nil, Detail: body, Attempt: attempt,
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
func (r Runner) RunChain(
	ctx context.Context, build, module, commit, dir string,
	commands map[string]string, attempt int,
) ([]Result, error) {
	var out []Result
	for _, gate := range Chain {
		// Checked BEFORE each gate, not once at the start: a chain whose
		// ground moved between the third and fourth gate would otherwise
		// record a mixed-base pass — some results against the old base, some
		// against the new — which says nothing about either.
		if err := r.baseHolds(ctx, commit); err != nil {
			return out, err
		}
		// AC-mutation-after-review: mutation is the most expensive gate and
		// only meaningful once everything else holds, the review pass
		// included. Every earlier gate stops the chain by failing; the review
		// is not a gate kriya runs, so it is asked about instead.
		if gate == "mutation" && r.Review != nil {
			passed, err := r.Review.PassedAt(ctx, build, commit)
			if err != nil {
				return out, fmt.Errorf("read review verdict at %s: %w", commit, err)
			}
			if !passed {
				return out, nil
			}
		}
		result, err := r.Run(ctx, build, module, gate, commit, dir, commands, attempt)
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

// baseHolds reports whether the frozen base is still the default head.
func (r Runner) baseHolds(ctx context.Context, frozen string) error {
	if r.Base == nil {
		return nil
	}
	head, err := r.Base.Head(ctx)
	if err != nil {
		return fmt.Errorf("resolve default head: %w", err)
	}
	if head != frozen {
		return fmt.Errorf("%w: %s is now %s", ErrBaseMoved, frozen, head)
	}
	return nil
}

// AllPassed reports whether every gate in the chain passed at a commit, and
// names the first that did not.
//
// The chain is walked in ORDER so the name returned is the earliest gap. A
// caller told "mutation" when test also failed would fix the wrong thing.
func (r Runner) AllPassed(
	ctx context.Context, build, module, commit string, attempt int,
) (bool, string, error) {
	for _, gate := range Chain {
		passed, err := r.Store.Passed(ctx, build, module, gate, commit, attempt)
		if err != nil {
			return false, "", fmt.Errorf("read %s result: %w", gate, err)
		}
		if !passed {
			return false, gate, nil
		}
	}
	if r.Review == nil {
		return true, "", nil
	}
	// The review is not a gate kriya runs, so it is asked about — and it comes
	// FIRST in the constitution's chain, which is why a missing review is
	// reported as the gap even when every gate result is present.
	reviewed, err := r.Review.PassedAt(ctx, build, commit)
	if err != nil {
		return false, "", fmt.Errorf("read review verdict at %s: %w", commit, err)
	}
	if !reviewed {
		return false, "review", nil
	}
	return true, "", nil
}
