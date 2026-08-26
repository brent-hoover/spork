package planner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Migration is planner's schema. The composition root sequences it with the
// other modules'; each module owns its own tables.
const Migration = `
CREATE TABLE spec_snapshot (
    hash              TEXT PRIMARY KEY,
    content           TEXT NOT NULL,
    resolved_commands TEXT NOT NULL,
    created           TEXT NOT NULL
)`

// LawMigration pins each module's boundary and the project's constitution
// alongside the files.
//
// A second migration rather than an edit to the first: the ledger records
// which migrations ran, and rewriting an applied one leaves every existing
// database claiming to have columns it does not have.
const LawMigration = `
ALTER TABLE spec_snapshot ADD COLUMN law TEXT NOT NULL DEFAULT '[]';
ALTER TABLE spec_snapshot ADD COLUMN constitution TEXT NOT NULL DEFAULT '[]'`

// TargetMigration is planner's second table. Separate from the first because
// a module's schema evolves across milestones and each migration is recorded
// by its own id; folding it into the first would never run on a database that
// already applied it.
const TargetMigration = `
CREATE TABLE build_target (
    target_key TEXT PRIMARY KEY,
    spec_hash  TEXT NOT NULL,
    project_id TEXT NOT NULL DEFAULT '',
    epic_id    TEXT NOT NULL DEFAULT '',
    epic_state TEXT NOT NULL,
    project_key TEXT NOT NULL DEFAULT '',
    name        TEXT NOT NULL DEFAULT '',
    actor       TEXT NOT NULL DEFAULT ''
)`

// SQLSnapshots stores snapshots in SQLite.
type SQLSnapshots struct{ DB *sql.DB }

// Put stores a snapshot. Pinning the same content twice is not an error:
// snapshots are content-addressed, so an identical hash is the same snapshot,
// and a re-intake of unchanged files must not fail.
func (s SQLSnapshots) Put(ctx context.Context, snap Snapshot) error {
	content, err := json.Marshal(snap.Content)
	if err != nil {
		return fmt.Errorf("marshal content: %w", err)
	}
	commands, err := json.Marshal(snap.ResolvedCommands)
	if err != nil {
		return fmt.Errorf("marshal resolved commands: %w", err)
	}
	law, err := json.Marshal(snap.Law)
	if err != nil {
		return fmt.Errorf("marshal law: %w", err)
	}
	constitution, err := json.Marshal(snap.Constitution)
	if err != nil {
		return fmt.Errorf("marshal constitution: %w", err)
	}
	_, err = s.DB.ExecContext(ctx,
		`INSERT INTO spec_snapshot (hash, content, resolved_commands, law, constitution, created)
		 VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(hash) DO NOTHING`,
		snap.Hash, string(content), string(commands), string(law), string(constitution),
		snap.Created.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("insert snapshot: %w", err)
	}
	return nil
}

// Get reads a snapshot by hash.
func (s SQLSnapshots) Get(ctx context.Context, hash string) (Snapshot, error) {
	var content, commands, law, constitution, created string
	err := s.DB.QueryRowContext(ctx,
		`SELECT content, resolved_commands, law, constitution, created
		   FROM spec_snapshot WHERE hash = ?`, hash).
		Scan(&content, &commands, &law, &constitution, &created)
	if err != nil {
		return Snapshot{}, fmt.Errorf("read snapshot %s: %w", hash, err)
	}
	snap := Snapshot{Hash: hash}
	if err := json.Unmarshal([]byte(content), &snap.Content); err != nil {
		return Snapshot{}, fmt.Errorf("unmarshal content: %w", err)
	}
	if err := json.Unmarshal([]byte(commands), &snap.ResolvedCommands); err != nil {
		return Snapshot{}, fmt.Errorf("unmarshal resolved commands: %w", err)
	}
	if err := json.Unmarshal([]byte(law), &snap.Law); err != nil {
		return Snapshot{}, fmt.Errorf("unmarshal law: %w", err)
	}
	if err := json.Unmarshal([]byte(constitution), &snap.Constitution); err != nil {
		return Snapshot{}, fmt.Errorf("unmarshal constitution: %w", err)
	}
	if snap.Created, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return Snapshot{}, fmt.Errorf("parse created: %w", err)
	}
	return snap, nil
}

// Count reports how many snapshots are pinned. It exists for the acceptance
// assertion that a refused intake pins nothing.
func (s SQLSnapshots) Count(ctx context.Context) (int, error) {
	var n int
	if err := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM spec_snapshot`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count snapshots: %w", err)
	}
	return n, nil
}

// SQLTargets stores build targets in SQLite.
type SQLTargets struct{ DB *sql.DB }

// Upsert writes a target, replacing any row with the same key.
func (s SQLTargets) Upsert(ctx context.Context, t BuildTarget) error {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO build_target
		   (target_key, spec_hash, project_id, epic_id, epic_state, project_key, name, actor)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(target_key) DO UPDATE SET
		   spec_hash = excluded.spec_hash,
		   project_id = excluded.project_id,
		   epic_id = excluded.epic_id,
		   epic_state = excluded.epic_state,
		   project_key = excluded.project_key,
		   name = excluded.name,
		   actor = excluded.actor`,
		t.TargetKey, t.SpecHash, t.ProjectID, t.EpicID, t.EpicState,
		t.ProjectKey, t.Name, t.Actor)
	if err != nil {
		return fmt.Errorf("upsert build target: %w", err)
	}
	return nil
}

// Find reads a target by key.
func (s SQLTargets) Find(ctx context.Context, targetKey string) (BuildTarget, bool, error) {
	t := BuildTarget{TargetKey: targetKey}
	err := s.DB.QueryRowContext(ctx,
		`SELECT spec_hash, project_id, epic_id, epic_state, project_key, name, actor
		 FROM build_target WHERE target_key = ?`,
		targetKey).Scan(&t.SpecHash, &t.ProjectID, &t.EpicID, &t.EpicState,
		&t.ProjectKey, &t.Name, &t.Actor)
	if errors.Is(err, sql.ErrNoRows) {
		// Absence is not failure: the first intake of a target has no row.
		return BuildTarget{}, false, nil
	}
	if err != nil {
		return BuildTarget{}, false, fmt.Errorf("read build target: %w", err)
	}
	return t, true, nil
}

// Pending lists targets a crash left mid-creation.
func (s SQLTargets) Pending(ctx context.Context) ([]BuildTarget, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT target_key, spec_hash, project_id, epic_id, epic_state, project_key, name, actor
		 FROM build_target WHERE epic_state = ? ORDER BY target_key`, EpicPending)
	if err != nil {
		return nil, fmt.Errorf("query pending targets: %w", err)
	}
	return scanTargets(rows, "pending")
}

// All lists every target, for the operator's picture of the machine.
func (s SQLTargets) All(ctx context.Context) ([]BuildTarget, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT target_key, spec_hash, project_id, epic_id, epic_state, project_key, name, actor
		 FROM build_target ORDER BY target_key`)
	if err != nil {
		return nil, fmt.Errorf("query targets: %w", err)
	}
	return scanTargets(rows, "all")
}

// scanTargets reads a target cursor to exhaustion.
func scanTargets(rows *sql.Rows, what string) ([]BuildTarget, error) {
	defer func() { _ = rows.Close() }()

	var out []BuildTarget
	for rows.Next() {
		var t BuildTarget
		if err := rows.Scan(&t.TargetKey, &t.SpecHash, &t.ProjectID, &t.EpicID,
			&t.EpicState, &t.ProjectKey, &t.Name, &t.Actor); err != nil {
			return nil, fmt.Errorf("scan %s target: %w", what, err)
		}
		out = append(out, t)
	}
	// Checked, because a cursor that fails mid-iteration otherwise returns a
	// SHORT list that reads exactly like "nothing left to recover".
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s targets: %w", what, err)
	}
	return out, nil
}

// TicketMigration records the tickets a decomposition produced.
//
// Without it a popped ticket is an id and a title, and its acceptance criteria
// — which are the whole of what the product owner validates against — exist
// only in the tracker's issue body. Recording them here is what lets a run
// built from a pop be validated at all.
const TicketMigration = `
CREATE TABLE planned_ticket (
    issue      TEXT PRIMARY KEY,
    target_key TEXT NOT NULL,
    title      TEXT NOT NULL,
    body       TEXT NOT NULL DEFAULT '',
    criteria   TEXT NOT NULL DEFAULT '[]'
)`

// PlanMigration records a decomposition's own lifecycle.
//
// The completed stamp is what arms build-completion detection: a plan
// mid-decomposition has a ticket set that is still growing, and "every ticket
// created so far is complete" says nothing about a build that is done.
const PlanMigration = `
CREATE TABLE plan (
    target_key TEXT PRIMARY KEY,
    spec_hash  TEXT NOT NULL,
    state      TEXT NOT NULL,
    tickets    INTEGER NOT NULL DEFAULT 0
)`

// TicketDeferAttemptMigration counts the deferral keys a row has spent.
//
// Its own migration because TicketConsumedMigration has shipped. sutra settles
// a REJECTED request under its idempotency key, so a conflicted deferral
// poisons that key permanently. Without a durable counter the attempt number
// restarted at zero on every retirement pass, re-presenting the same poisoned
// keys forever — and the ticket could never be deferred once its status
// stabilised, leaving the successor fenced indefinitely.
const TicketDeferAttemptMigration = `
ALTER TABLE plan_ticket ADD COLUMN defer_attempt INTEGER NOT NULL DEFAULT 0`

// TicketConsumedMigration records a retirement's verdict on a row.
//
// Its own migration because TicketOrdinalMigration has shipped. A consumed
// row no longer participates in binding, so the absence of these columns is
// not merely missing information — it is a row that looks live forever.
const TicketConsumedMigration = `
ALTER TABLE plan_ticket ADD COLUMN consumed INTEGER NOT NULL DEFAULT 0;
ALTER TABLE plan_ticket ADD COLUMN disposition TEXT NOT NULL DEFAULT ''`

// TicketOrdinalMigration re-keys planned tickets on (plan, ordinal).
//
// The issue id cannot be the identity of a WRITE-AHEAD row: the whole point
// is that the row exists before the create call, so there is no issue id yet.
// Keyed on the issue, the write-ahead insert produced a row with an empty id
// that the post-create write could never find again — one orphan row per
// ticket, and a resume replaying against ticket definitions it could not
// match to its sequence.
//
// The ordinal is the identity the sequence already uses to address tickets.
// The issue keeps a unique index for pop lookups, partial so that the many
// rows still awaiting their id do not collide on the empty string.
//
// A new table because SQLite cannot re-key one in place. Existing rows carry
// across with ordinal 0 and their recorded plan, which is honest: they were
// written when a ticket's identity WAS its issue, and every one of them has
// an issue id.
const TicketOrdinalMigration = `
CREATE TABLE plan_ticket (
    plan       TEXT NOT NULL,
    ordinal    INTEGER NOT NULL,
    issue      TEXT NOT NULL DEFAULT '',
    target_key TEXT NOT NULL,
    title      TEXT NOT NULL,
    body       TEXT NOT NULL DEFAULT '',
    criteria   TEXT NOT NULL DEFAULT '[]',
    kind       TEXT NOT NULL DEFAULT 'implementation',
    blocks     TEXT NOT NULL DEFAULT '[]',
    layers     TEXT NOT NULL DEFAULT '[]',
    skeleton   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (plan, ordinal)
);
CREATE UNIQUE INDEX idx_plan_ticket_issue ON plan_ticket(issue) WHERE issue <> '';
CREATE INDEX idx_plan_ticket_target ON plan_ticket(target_key);
INSERT INTO plan_ticket
    (plan, ordinal, issue, target_key, title, body, criteria, kind, blocks, layers, skeleton)
  SELECT plan, 0, issue, target_key, title, body, criteria, kind, blocks, layers, skeleton
    FROM planned_ticket;
DROP TABLE planned_ticket`

// StepMigration is a plan's durable mutation sequence.
//
// Persisted BEFORE any tracker call, which is what makes a resume a replay
// rather than a second decomposition. Keyed on (plan, seq): the sequence is
// the plan, and replaying it in seq order reproduces the phase barriers.
const StepMigration = `
CREATE TABLE plan_step (
    plan    TEXT NOT NULL,
    seq     INTEGER NOT NULL,
    ordinal INTEGER NOT NULL,
    other   INTEGER NOT NULL DEFAULT -1,
    kind    TEXT NOT NULL,
    key     TEXT NOT NULL,
    state   TEXT NOT NULL DEFAULT 'pending',
    issue   TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (plan, seq)
)`

// A new TABLE rather than columns on the old one: `plan` is keyed on
// target_key, and the spec makes the DECOMPOSITION KEY the identity — a
// target accumulates plans as decompositions supersede each other, so one row
// per target cannot hold the set that supersession and retirement walk.
// SQLite cannot re-key a table in place, so this creates the right shape and
// carries the existing rows across.
//
// Legacy rows get key 'legacy:<target>': unique, so the carry-over cannot
// collide, and structurally distinguishable from a derived key, which is 64
// hex characters. Such a row therefore resolves to nothing and the next
// decomposition creates a proper plan rather than replaying a keyless one.
// The two old states map onto the new model: decomposing was active without
// the completed stamp, and completed was active with it.
// PlanPredecessorMigration records which plan a successor beat.
//
// Its own migration because PlanKeyMigration has shipped. Without it a
// successor cannot find the plan it replaced, so retirement has nothing to
// walk and a superseded plan's tickets hold the epic open forever.
const PlanPredecessorMigration = `
ALTER TABLE decomposition_plan ADD COLUMN predecessor TEXT NOT NULL DEFAULT ''`

// PlanKeyMigration re-keys plans on their decomposition key.
const PlanKeyMigration = `
CREATE TABLE decomposition_plan (
    decomposition_key TEXT PRIMARY KEY,
    target_key        TEXT NOT NULL,
    spec_hash         TEXT NOT NULL,
    generation        INTEGER NOT NULL DEFAULT 0,
    state             TEXT NOT NULL,
    completed         INTEGER NOT NULL DEFAULT 0,
    tickets           INTEGER NOT NULL DEFAULT 0,
    superseded_by     TEXT NOT NULL DEFAULT '',
    error             TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_decomposition_plan_target ON decomposition_plan(target_key);
INSERT INTO decomposition_plan
    (decomposition_key, target_key, spec_hash, state, completed, tickets)
  SELECT 'legacy:' || target_key, target_key, spec_hash, 'active',
         CASE WHEN state = 'completed' THEN 1 ELSE 0 END, tickets
    FROM plan;
DROP TABLE plan`

// TicketKindMigration records what kind of ticket the plan produced.
//
// Its own migration: planned_ticket has shipped. Without the kind a popped
// spike is indistinguishable from implementation work, so it goes through the
// code gate chain — and fails it, because a spike produces a document.
const TicketKindMigration = `
ALTER TABLE planned_ticket ADD COLUMN kind TEXT NOT NULL DEFAULT 'implementation';
ALTER TABLE planned_ticket ADD COLUMN blocks TEXT NOT NULL DEFAULT '[]'`

// TicketPlanMigration binds a ticket to the decomposition that produced it.
//
// Its own migration, because TicketSliceMigration has shipped. Existing rows
// get an empty plan, which is honest: they were written when a target had one
// plan and nothing recorded which. They still answer target-scoped reads.
const TicketPlanMigration = `
ALTER TABLE planned_ticket ADD COLUMN plan TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_planned_ticket_plan ON planned_ticket(plan)`

// TicketSliceMigration records the plan shape a tracer bullet declares.
//
// Its own migration, because TicketKindMigration has shipped: a database that
// ran it must gain these columns by ALTER rather than by a rewrite of an
// applied migration. Without them a ticket read back after decomposition
// carries no layers and is never the skeleton, so anything reasoning about
// plan shape from the store — recovery above all — sees a different plan than
// the one that was filed.
const TicketSliceMigration = `
ALTER TABLE planned_ticket ADD COLUMN layers TEXT NOT NULL DEFAULT '[]';
ALTER TABLE planned_ticket ADD COLUMN skeleton INTEGER NOT NULL DEFAULT 0`

// SQLPlans persists plans in SQLite.
type SQLPlans struct{ DB *sql.DB }

// Upsert writes a plan row.
func (s SQLPlans) Upsert(ctx context.Context, p Plan) error {
	res, err := s.DB.ExecContext(ctx,
		`INSERT INTO decomposition_plan
		   (decomposition_key, target_key, spec_hash, generation, state,
		    completed, tickets, superseded_by, error, predecessor)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(decomposition_key) DO UPDATE SET
		   target_key = excluded.target_key, spec_hash = excluded.spec_hash,
		   generation = excluded.generation, state = excluded.state,
		   completed = excluded.completed, tickets = excluded.tickets,
		   superseded_by = excluded.superseded_by, error = excluded.error,
		   predecessor = excluded.predecessor
		 WHERE decomposition_plan.state NOT IN (?, ?)`,
		p.Key, p.TargetKey, p.SpecHash, p.Generation, p.State,
		p.Completed, p.Tickets, p.SupersededBy, p.Error, p.Predecessor,
		PlanSuperseded, PlanHistorical)
	if err != nil {
		return fmt.Errorf("upsert plan: %w", err)
	}
	// SUPERSEDED and HISTORICAL are terminal, and the guard above makes them
	// so. Without it, a decomposition whose head was replaced mid-phase went
	// on writing itself active and completed — erasing the supersession, and
	// then failing to activate because it was no longer current, leaking a
	// fence increment that nothing would ever lower.
	//
	// Loudly, not silently: a caller that just wrote nothing must not carry
	// on stamping a plan whole.
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("upsert plan: %w", err)
	}
	if rows == 0 {
		return &TerminalPlanError{Key: p.Key}
	}
	return nil
}

// TerminalPlanError says a write was refused because the plan has ended.
//
// Its own type because the caller's response is specific: a decomposition
// that learns its plan was superseded mid-phase must STOP, not retry. Nothing
// went wrong with the store.
type TerminalPlanError struct{ Key string }

func (e *TerminalPlanError) Error() string {
	return fmt.Sprintf("plan %s has ended and cannot be written to", Short(e.Key))
}

// Claim inserts a plan only if nothing has claimed its key.
//
// ON CONFLICT DO NOTHING, and the affected-row count is the verdict. Resolve
// reads before this writes, so two concurrent first requests for one key can
// both find no row and both believe they are fresh — and an upsert would let
// both decompose, each overwriting the other's lifecycle row. Exactly one
// insert can succeed, and the loser re-resolves into an ordinary same-key
// retry of the winner.
func (s SQLPlans) Claim(ctx context.Context, p Plan) (bool, error) {
	res, err := s.DB.ExecContext(ctx,
		`INSERT INTO decomposition_plan
		   (decomposition_key, target_key, spec_hash, generation, state,
		    completed, tickets, superseded_by, error, predecessor)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(decomposition_key) DO NOTHING`,
		p.Key, p.TargetKey, p.SpecHash, p.Generation, p.State,
		p.Completed, p.Tickets, p.SupersededBy, p.Error, p.Predecessor)
	if err != nil {
		return false, fmt.Errorf("claim plan key: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim plan key: %w", err)
	}
	return rows == 1, nil
}

// planColumns is the read list both queries share, so a column added to one
// read path cannot be missed by the other.
const planColumns = `decomposition_key, target_key, spec_hash, generation,
	state, completed, tickets, superseded_by, error, predecessor`

func scanPlan(row interface{ Scan(...any) error }) (Plan, error) {
	var p Plan
	err := row.Scan(&p.Key, &p.TargetKey, &p.SpecHash, &p.Generation,
		&p.State, &p.Completed, &p.Tickets, &p.SupersededBy, &p.Error, &p.Predecessor)
	return p, err
}

// Find reads a target's current plan.
//
// The ACTIVE one, and by state rather than by recency: a target accumulates
// superseded plans, and the newest row is whichever was written last — which
// during a supersession is the predecessor being stamped, not the head.
func (s SQLPlans) Find(ctx context.Context, targetKey string) (Plan, bool, error) {
	p, err := scanPlan(s.DB.QueryRowContext(ctx,
		`SELECT `+planColumns+` FROM decomposition_plan
		   WHERE target_key = ? AND state = ? LIMIT 1`, targetKey, PlanActive))
	if errors.Is(err, sql.ErrNoRows) {
		return Plan{}, false, nil
	}
	if err != nil {
		return Plan{}, false, fmt.Errorf("read plan: %w", err)
	}
	return p, true, nil
}

// ByKey resolves a decomposition key.
func (s SQLPlans) ByKey(ctx context.Context, key string) (Plan, bool, error) {
	p, err := scanPlan(s.DB.QueryRowContext(ctx,
		`SELECT `+planColumns+` FROM decomposition_plan WHERE decomposition_key = ?`, key))
	if errors.Is(err, sql.ErrNoRows) {
		return Plan{}, false, nil
	}
	if err != nil {
		return Plan{}, false, fmt.Errorf("read plan by key: %w", err)
	}
	return p, true, nil
}

// SQLTickets persists planned tickets in SQLite.
type SQLTickets struct{ DB *sql.DB }

// Put records a ticket a decomposition produced.
//
// A ticket with no plan is REFUSED rather than stored. Rows are keyed on
// (plan, ordinal), so an unidentified ticket lands on (”, 0) — and the next
// one overwrites it. Silently keeping one ticket out of a plan of six is
// exactly the failure this refuses to have.
func (s SQLTickets) Put(ctx context.Context, targetKey string, t Ticket) error {
	if t.Plan == "" {
		return fmt.Errorf("ticket %q names no plan, so it has no identity to be stored under", t.Title)
	}
	blocks, err := json.Marshal(t.Blocks)
	if err != nil {
		return fmt.Errorf("marshal blocks: %w", err)
	}
	criteria, err := json.Marshal(t.Criteria)
	if err != nil {
		return fmt.Errorf("marshal criteria: %w", err)
	}
	layers, err := json.Marshal(t.Layers)
	if err != nil {
		return fmt.Errorf("marshal layers: %w", err)
	}
	_, err = s.DB.ExecContext(ctx,
		`INSERT INTO plan_ticket
		   (plan, ordinal, issue, target_key, title, body, criteria, kind, blocks, layers, skeleton)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(plan, ordinal) DO UPDATE SET
		   issue = excluded.issue, target_key = excluded.target_key,
		   title = excluded.title, body = excluded.body,
		   criteria = excluded.criteria, kind = excluded.kind,
		   blocks = excluded.blocks, layers = excluded.layers,
		   skeleton = excluded.skeleton`,
		t.Plan, t.Ordinal, t.IssueID, targetKey, t.Title, t.Body, string(criteria),
		kindOf(t), string(blocks), string(layers), t.Skeleton)
	if err != nil {
		return fmt.Errorf("record planned ticket: %w", err)
	}
	return nil
}

// kindOf defaults a ticket with no kind to implementation.
//
// The column is an enum, and a zero-valued Ticket has none. Implementation is
// the safe default: a spike misfiled as one fails the gate chain loudly, where
// an implementation ticket misfiled as a spike would SKIP the gates and land
// unreviewed code.
func kindOf(t Ticket) string {
	if t.Kind == "" {
		return KindImplementation
	}
	return t.Kind
}

// ForTarget lists a target's whole planned ticket set.
//
// Ordered by issue, so a detector's report of what is blocking reads the same
// way twice.
func (s SQLTickets) ForTarget(ctx context.Context, targetKey string) ([]Ticket, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT `+ticketColumns+` FROM plan_ticket
		   WHERE target_key = ? ORDER BY plan, ordinal`, targetKey)
	if err != nil {
		return nil, fmt.Errorf("query planned tickets: %w", err)
	}
	return scanTickets(rows)
}

// scanTickets drains a ticket query. One drain for every ticket read path, so
// a column added to the select list cannot be decoded by one and dropped by
// another — which is how layers and skeleton became write-only columns.
func scanTickets(rows *sql.Rows) ([]Ticket, error) {
	defer func() { _ = rows.Close() }()

	var out []Ticket
	for rows.Next() {
		var t Ticket
		var criteria, blocks, layers string
		if err := rows.Scan(&t.IssueID, &t.Title, &t.Body, &criteria,
			&t.Kind, &blocks, &layers, &t.Skeleton, &t.Plan, &t.Ordinal,
			&t.Consumed, &t.Disposition, &t.DeferAttempt); err != nil {
			return nil, fmt.Errorf("scan planned ticket: %w", err)
		}
		if err := decodeTicketLists(&t, criteria, blocks, layers); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	// Checked, because a cursor failing mid-iteration otherwise returns a
	// SHORT list — which here reads as a plan with fewer tickets than it has,
	// and a build that completes over the ones it could not see.
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate planned tickets: %w", err)
	}
	return out, nil
}

// ticketColumns is the read list every ticket query shares.
const ticketColumns = `issue, title, body, criteria, kind, blocks, layers, skeleton,
	plan, ordinal, consumed, disposition, defer_attempt`

// Consume stamps a predecessor row settled by a successor's retirement.
//
// The stamp is written even when the disposition needed no tracker call —
// "the row whose ticket was never created retires with no sutra call" is
// still a retirement, and an unstamped row is one a later pass walks again.
func (s SQLTickets) Consume(
	ctx context.Context, decompositionKey string, ordinal int, disposition string,
) error {
	if _, err := s.DB.ExecContext(ctx,
		`UPDATE plan_ticket SET consumed = 1, disposition = ?
		   WHERE plan = ? AND ordinal = ?`,
		disposition, decompositionKey, ordinal); err != nil {
		return fmt.Errorf("consume ticket %d: %w", ordinal, err)
	}
	return nil
}

// BumpDeferAttempt spends another deferral key for a row.
//
// Incremented BEFORE the call it is for, and returned, so the number handed
// back has never been presented to sutra. Incrementing afterwards would leave
// a crash between the call and the write re-presenting the key that call
// already settled.
func (s SQLTickets) BumpDeferAttempt(
	ctx context.Context, decompositionKey string, ordinal int,
) (int, error) {
	var attempt int
	err := s.DB.QueryRowContext(ctx,
		`UPDATE plan_ticket SET defer_attempt = defer_attempt + 1
		   WHERE plan = ? AND ordinal = ?
		 RETURNING defer_attempt`, decompositionKey, ordinal).Scan(&attempt)
	if err != nil {
		return 0, fmt.Errorf("spend a defer key for ticket %d: %w", ordinal, err)
	}
	return attempt, nil
}

// ForPlan lists ONE decomposition's tickets.
//
// A same-key retry answers from here. Scoped to the plan rather than the
// target because a target accumulates plans, and the union of every
// generation's tickets is not any one decomposition's output.
func (s SQLTickets) ForPlan(ctx context.Context, decompositionKey string) ([]Ticket, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT `+ticketColumns+` FROM plan_ticket
		   WHERE plan = ? ORDER BY ordinal`, decompositionKey)
	if err != nil {
		return nil, fmt.Errorf("query plan tickets: %w", err)
	}
	return scanTickets(rows)
}

// Find reads a ticket by the issue it became, within one target's plan.
//
// SCOPED to the target, because a pop is identity-wide: sutra offers whatever
// is assigned to the popping identity, from any plan. A ticket this target's
// decomposition did not produce is one whose code lives in another repository,
// and building it here would work the wrong codebase.
func (s SQLTickets) Find(ctx context.Context, targetKey, issue string) (Ticket, bool, error) {
	t := Ticket{IssueID: issue}
	var criteria, blocks, layers string
	err := s.DB.QueryRowContext(ctx,
		`SELECT title, body, criteria, kind, blocks, layers, skeleton, plan, ordinal,
		        consumed, disposition, defer_attempt
		   FROM plan_ticket WHERE issue = ? AND target_key = ?`, issue, targetKey).
		Scan(&t.Title, &t.Body, &criteria, &t.Kind, &blocks, &layers, &t.Skeleton,
			&t.Plan, &t.Ordinal, &t.Consumed, &t.Disposition, &t.DeferAttempt)
	if errors.Is(err, sql.ErrNoRows) {
		return Ticket{}, false, nil
	}
	if err != nil {
		return Ticket{}, false, fmt.Errorf("read planned ticket: %w", err)
	}
	if err := decodeTicketLists(&t, criteria, blocks, layers); err != nil {
		return Ticket{}, false, err
	}
	return t, true, nil
}

// decodeTicketLists fills a ticket's three JSON-encoded list columns.
//
// One function for all three so a column added later cannot be decoded in one
// read path and quietly dropped in the other — which is exactly how layers and
// skeleton reached the store as write-only columns in the first place.
func decodeTicketLists(t *Ticket, criteria, blocks, layers string) error {
	for _, list := range []struct {
		name string
		raw  string
		into *[]string
	}{
		{"criteria", criteria, &t.Criteria},
		{"blocks", blocks, &t.Blocks},
		{"layers", layers, &t.Layers},
	} {
		if err := json.Unmarshal([]byte(list.raw), list.into); err != nil {
			return fmt.Errorf("unmarshal %s: %w", list.name, err)
		}
	}
	return nil
}
