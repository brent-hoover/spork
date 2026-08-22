package agent

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"kriya/internal/clock"
)

// Migration is the agent_invocation ledger.
//
// This package owns the table even though ENT-agent-invocation is declared
// under MOD-orchestrator. No module that invokes an agent can legally import
// orchestrator — planner may import only trackerclient, and architect and
// owner may not import orchestrator at all — so under the declared boundaries
// the row would have no writer. Every invocation passes through this seam, so
// the execution ledger belongs to it. See feature-work/kriya-build/spec-gaps.md.
const Migration = `
CREATE TABLE agent_invocation (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    role    TEXT NOT NULL,
    tier    TEXT NOT NULL,
    model   TEXT NOT NULL,
    session TEXT NOT NULL,
    plan    TEXT NOT NULL DEFAULT '',
    build   TEXT NOT NULL DEFAULT '',
    started TEXT NOT NULL,
    ended   TEXT NOT NULL,
    CHECK ((plan = '') <> (build = ''))
)`

// Invocation is one recorded agent execution.
type Invocation struct {
	Role    Role
	Tier    string
	Model   string
	Session string
	// Exactly one of Plan or Build is set. The CHECK constraint enforces it in
	// the database because the format cannot express conditional requiredness,
	// and a row scoped to neither or both is unattributable.
	Plan    string
	Build   string
	Started time.Time
	Ended   time.Time
}

// Ledger records invocations.
type Ledger struct {
	DB  *sql.DB
	Now clock.Clock
}

// Record writes one invocation.
func (l Ledger) Record(ctx context.Context, inv Invocation) error {
	_, err := l.DB.ExecContext(ctx,
		`INSERT INTO agent_invocation (role, tier, model, session, plan, build, started, ended)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		string(inv.Role), inv.Tier, inv.Model, inv.Session, inv.Plan, inv.Build,
		inv.Started.UTC().Format(time.RFC3339Nano), inv.Ended.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("record agent invocation: %w", err)
	}
	return nil
}

// Recording wraps an Agent so every invocation is written to the ledger.
//
// A wrapper rather than a call at each site: AC-tier-observed says EVERY
// invocation records its role, tier, and resolved model, and a rule enforced
// at each call site is a rule that will be missed at the next one.
type Recording struct {
	Inner  Agent
	Ledger Ledger
	Tiers  Tiers
	Now    clock.Clock
	// Scope names the plan or build the invocation belongs to.
	Scope Scope
}

// Scope attributes an invocation to exactly one of a plan or a build.
type Scope struct {
	Plan  string
	Build string
}

// Run invokes the agent and records what ran.
func (r Recording) Run(ctx context.Context, req Request) (Result, error) {
	tier, _, err := r.Tiers.Resolve(req.Role)
	if err != nil {
		return Result{}, err
	}
	started := r.Now.Now()
	res, runErr := r.Inner.Run(ctx, req)
	if runErr != nil {
		return Result{}, runErr
	}
	inv := Invocation{
		Role: req.Role, Tier: tier, Model: res.Model, Session: res.SessionID,
		Plan: r.Scope.Plan, Build: r.Scope.Build,
		Started: started, Ended: r.Now.Now(),
	}
	if err := r.Ledger.Record(ctx, inv); err != nil {
		return Result{}, err
	}
	return res, nil
}
