// Package owner is the PO agent — AC validation, anti-gaming, vision.
//
// Owns Validation.
package owner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"kriya/internal/agent"
	"kriya/internal/clock"
)

// Verdicts a validation can reach.
const (
	VerdictSatisfied    = "satisfied"
	VerdictNotSatisfied = "not-satisfied"
	VerdictTestsGamed   = "tests-gamed"
)

// Validation is one PO verdict on one gate attempt.
type Validation struct {
	Build string
	// Attempt is the gate attempt this verdict belongs to. A rerun after
	// integration needs a fresh PO pass: the code changed, so a verdict from
	// the previous attempt says nothing about this one.
	Attempt int
	Verdict string
	// Commit is the object id the validation covered.
	Commit string
	Notes  string
}

// Passed reports whether the run may advance.
func (v Validation) Passed() bool { return v.Verdict == VerdictSatisfied }

// Store persists validations.
type Store interface {
	Upsert(ctx context.Context, v Validation) error
	// Find reads the verdict for a build's gate attempt. Keyed by attempt
	// because a verdict from an earlier one does not carry forward.
	Find(ctx context.Context, build string, attempt int) (Validation, bool, error)
}

// Gates answers whether every machine gate passed at a commit.
//
// Declared here rather than imported: AC-po-position makes the gate chain a
// PRECONDITION of validation, so the owner has to be able to ask — but an
// owner that imported the gate runner would invert the dependency the
// composition root keeps flat.
type Gates interface {
	// AllPassed reports whether every gate in the chain passed at this exact
	// commit, and names the first that did not.
	AllPassed(ctx context.Context, build, module, commit string) (bool, string, error)
}

// Owner validates a run against its ticket.
type Owner struct {
	Agent agent.Agent
	Store Store
	Gates Gates
	Now   clock.Clock
}

// allowRules is the PO's toolset.
//
// Read-only. The PO judges; it does not fix. An implementation edited by its
// own validator is one nothing independent has looked at.
var allowRules = []string{"Read", "Grep", "Glob"}

// AllowRules returns the PO's permitted tools.
func AllowRules() []string { return append([]string(nil), allowRules...) }

// Request is one validation.
type Request struct {
	Build   string
	Module  string
	Ticket  string
	Commit  string
	Attempt int
	// Criteria are the acceptance criteria the PO checks, each against actual
	// behaviour rather than against the tests' own claims.
	Criteria []string
	// Workspace is the checkout the PO reads. Read-only tools only.
	Workspace string
	// SystemFile is the assembled context — the whole spec's law, which is
	// what makes the PO the only agent able to judge an AC's INTENT.
	SystemFile string
}

// verdict is what the PO agent returns.
type verdict struct {
	Verdict string `json:"verdict"`
	Notes   string `json:"notes"`
}

// Schema constrains the PO's reply.
//
// A free-text verdict would have to be parsed, and a parse that guessed wrong
// would record "satisfied" for a run the PO rejected.
var Schema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["verdict", "notes"],
  "properties": {
    "verdict": {"type": "string", "enum": ["satisfied", "not-satisfied", "tests-gamed"]},
    "notes": {"type": "string", "minLength": 1}
  }
}`)

// Validate runs the PO over a gated run.
//
// Refuses outright when any machine gate has not passed at the head commit.
// AC-po-position says validation cannot be skipped OR REORDERED around the
// gap, so the precondition is checked here rather than assumed by the caller —
// a caller that forgot would otherwise produce a verdict on ungated code.
func (o Owner) Validate(ctx context.Context, req Request) (Validation, error) {
	passed, missing, err := o.Gates.AllPassed(ctx, req.Build, req.Module, req.Commit)
	if err != nil {
		return Validation{}, fmt.Errorf("read gate results at %s: %w", req.Commit, err)
	}
	if !passed {
		return Validation{}, fmt.Errorf(
			"cannot validate %s at %s: the %s gate has not passed", req.Build, req.Commit, missing)
	}

	res, err := o.Agent.Run(ctx, agent.Request{
		Role:       agent.RolePO,
		Prompt:     prompt(req),
		Workspace:  req.Workspace,
		SystemFile: req.SystemFile,
		Schema:     Schema,
		AllowRules: AllowRules(),
	})
	if err != nil {
		return Validation{}, fmt.Errorf("product owner on %s: %w", req.Ticket, err)
	}
	var reply verdict
	if err := json.Unmarshal(res.Structured, &reply); err != nil {
		return Validation{}, fmt.Errorf("parse validation verdict: %w", err)
	}
	if reply.Notes == "" {
		// A verdict with no reasons is not actionable, and AC-po-verdict wants
		// the reasons to survive in the run's history alongside it.
		return Validation{}, fmt.Errorf("validation of %s carries no reasons", req.Ticket)
	}

	v := Validation{
		Build: req.Build, Attempt: req.Attempt, Verdict: reply.Verdict,
		Commit: req.Commit, Notes: reply.Notes,
	}
	if err := o.Store.Upsert(ctx, v); err != nil {
		return Validation{}, fmt.Errorf("record validation: %w", err)
	}
	return v, nil
}

// prompt states what the PO is judging and what counts as gaming.
//
// Gaming is named EXPLICITLY rather than left to judgement: AC-po-anti-gaming
// lists the three patterns, and a PO asked only "is this good" reliably
// answers yes to a tautology.
func prompt(req Request) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Validate the work on %q at commit %s.\n\n", req.Ticket, req.Commit)
	b.WriteString("Every machine gate has passed. Passing tests is not the bar.\n\n")
	b.WriteString("For EACH acceptance criterion below, decide two things:\n")
	b.WriteString("  1. Is there a test that genuinely exercises it?\n")
	b.WriteString("  2. Does the implementation satisfy its INTENT, in the context of\n")
	b.WriteString("     the whole spec? An AC met by letter and not by intent fails.\n\n")
	for _, id := range req.Criteria {
		fmt.Fprintf(&b, "  - %s\n", id)
	}
	b.WriteString("\nThen hunt gaming explicitly. Any of these fails validation:\n")
	b.WriteString("  - assertions that are tautologies, true by construction\n")
	b.WriteString("  - an implementation that special-cases test inputs\n")
	b.WriteString("  - assertions weakened or deleted since an earlier commit\n\n")
	b.WriteString("Name the evidence you found. A verdict with no reasons is not a verdict.\n")
	return b.String()
}
