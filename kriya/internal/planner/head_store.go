package planner

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// HeadMigration is the durable head row.
//
// target_key is the PRIMARY KEY, and that uniqueness IS the bootstrap CAS: a
// target with no head yet has two concurrent first decompositions racing an
// INSERT, and exactly one can win. No separate "is there a head" read is
// needed, and no window exists between reading and inserting.
const HeadMigration = `
CREATE TABLE plan_head (
    target_key TEXT PRIMARY KEY,
    current    TEXT NOT NULL,
    generation INTEGER NOT NULL DEFAULT 0,
    fence      INTEGER NOT NULL DEFAULT 0
)`

// SQLHeads holds plan heads in SQLite.
type SQLHeads struct {
	DB *sql.DB
	// Advances binds a replacement to the completion epoch it must move. Nil
	// runs the CAS on its own, which is what a test of the CAS itself wants;
	// production wires it, because a supersession that did not advance the
	// epoch leaves the OLD plan's completion claim current — free to stamp
	// the target done, or to suppress a fresh review, over work the new head
	// has not built.
	Advances AdvanceStore
}

// errAlreadyHead rolls the advance transaction back for a candidate that
// already owns the head.
//
// A sentinel rather than a verdict returned from inside the callback, because
// the ROLLBACK is the point: a resumed plan re-enters the CAS, and inserting
// an advance for a head that does not move would advance the epoch and kill a
// perfectly good completion claim.
var errAlreadyHead = errors.New("the candidate already owns the head")

// errLostCAS rolls the advance back for a candidate that lost.
//
// A loser moves no head, so it must move no epoch either. Its landing is
// written afterwards, in its own transaction: until that write, the candidate
// is simply still pending, and a recovery re-enters the CAS and lands it then
// — idempotent, and never a candidate durably lost with nothing saying so.
type errLostCAS struct {
	head    PlanHead
	landing string
}

func (e *errLostCAS) Error() string {
	return fmt.Sprintf("lost the replacement CAS to head plan %s", e.head.Current)
}

// Head reads a target's head row.
func (s SQLHeads) Head(ctx context.Context, targetKey string) (PlanHead, bool, error) {
	h := PlanHead{TargetKey: targetKey}
	err := s.DB.QueryRowContext(ctx,
		`SELECT current, generation, fence FROM plan_head WHERE target_key = ?`, targetKey).
		Scan(&h.Current, &h.Generation, &h.Fence)
	if errors.Is(err, sql.ErrNoRows) {
		return PlanHead{}, false, nil
	}
	if err != nil {
		return PlanHead{}, false, fmt.Errorf("read plan head: %w", err)
	}
	return h, true, nil
}

// Replace performs the whole replacement in ONE transaction.
//
// Everything the spec calls atomic happens here together: the eligibility
// check, the predecessor's supersession, the head move, the generation bump
// and the fence raise. Split across transactions, a crash between any two
// would leave a head pointing at a plan that never superseded its predecessor
// — and pops admitted against a plan whose predecessor was never retired.
func (s SQLHeads) Replace(ctx context.Context, candidate Plan) (Replacement, error) {
	if s.Advances == nil {
		return s.replaceAlone(ctx, candidate)
	}

	var verdict Replacement
	// The advance and the head move are ONE fact. Committed separately, a
	// crash between them leaves either an unadvanced epoch over replaced
	// work, or an advance for a replacement that never happened — and the
	// second is worse, because the epoch cannot be walked back.
	//
	// Keyed on the CANDIDATE, so a resumed plan re-entering the CAS presents
	// the same key and advances nothing a second time.
	fresh, err := (Epochs{Store: s.Advances}).OnLocalWith(ctx,
		candidate.TargetKey, CauseSupersession, candidate.Key,
		func(tx Tx) error {
			var err error
			verdict, err = replaceWithin(ctx, tx, candidate)
			return err
		})

	if errors.Is(err, errAlreadyHead) {
		return verdict, nil
	}
	var lost *errLostCAS
	if errors.As(err, &lost) {
		return s.landLoser(ctx, candidate, lost)
	}
	if err != nil {
		return Replacement{}, err
	}
	if !fresh {
		// A REPLAY of this candidate's advance, so the mutation did not run —
		// that is what the insert-once key is for. This candidate has already
		// been through the CAS, and the durable state says how it went.
		return s.settled(ctx, candidate)
	}
	return verdict, nil
}

// settled reports the verdict a candidate already reached.
//
// Read rather than re-run: re-running would need a fresh advance key, which
// would move the epoch for a head that is not moving — killing a completion
// claim on behalf of a plan that changed nothing.
func (s SQLHeads) settled(ctx context.Context, candidate Plan) (Replacement, error) {
	head, found, err := s.Head(ctx, candidate.TargetKey)
	if err != nil {
		return Replacement{}, err
	}
	if found && head.Current == candidate.Key {
		return Replacement{Won: true, Head: head}, nil
	}
	// It lost the first time. The landing it took then is the answer now;
	// re-deriving one could park a candidate the head has since outrun.
	plan, found, err := (SQLPlans{DB: s.DB}).ByKey(ctx, candidate.Key)
	if err != nil {
		return Replacement{}, err
	}
	if !found {
		return Replacement{}, fmt.Errorf(
			"plan %s advanced the epoch but has no row", Short(candidate.Key))
	}
	return Replacement{Head: head, Landing: plan.State}, nil
}

// replaceAlone runs the CAS with no epoch to move.
func (s SQLHeads) replaceAlone(ctx context.Context, candidate Plan) (Replacement, error) {
	verdict, err := s.attempt(ctx, candidate)

	// The transaction is CLOSED before anything else touches the database.
	// Landing a loser while it was still open deadlocked against it —
	// SQLITE_BUSY on the connection the caller was already holding.
	if errors.Is(err, errAlreadyHead) {
		return verdict, nil
	}
	var lost *errLostCAS
	if errors.As(err, &lost) {
		return s.landLoser(ctx, candidate, lost)
	}
	if err != nil {
		return Replacement{}, err
	}
	return verdict, nil
}

// attempt runs one CAS in its own transaction and closes it either way.
func (s SQLHeads) attempt(ctx context.Context, candidate Plan) (Replacement, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Replacement{}, fmt.Errorf("begin replacement: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	verdict, err := replaceWithin(ctx, txExec{tx: tx}, candidate)
	if err != nil {
		return verdict, err
	}
	if err := tx.Commit(); err != nil {
		return Replacement{}, fmt.Errorf("commit replacement: %w", err)
	}
	return verdict, nil
}

// landLoser records a losing candidate's terminal or parked state.
func (s SQLHeads) landLoser(
	ctx context.Context, candidate Plan, lost *errLostCAS,
) (Replacement, error) {
	// REVALIDATED, because the deciding transaction has already rolled back
	// and the head can have moved since. Landing the stale verdict could park
	// a candidate awaiting-operator that the head has by now outrun — an
	// operator offered a retry the CAS can only reject — or, worse, park one
	// that concurrently BECAME the head.
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Replacement{}, fmt.Errorf("begin landing: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	head, found, err := headWithin(ctx, txExec{tx: tx}, candidate.TargetKey)
	if err != nil {
		return Replacement{}, err
	}
	if found && head.Current == candidate.Key {
		// It became the head while the verdict was in flight. Landing it now
		// would bury the current head as a loser.
		return Replacement{Won: true, Head: head}, nil
	}

	landing := lost.landing
	if found {
		var headGeneration int
		err := tx.QueryRowContext(ctx,
			`SELECT generation FROM decomposition_plan WHERE decomposition_key = ?`,
			head.Current).Scan(&headGeneration)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return Replacement{}, fmt.Errorf("read the current head plan: %w", err)
		}
		if err == nil {
			landing = LandingFor(candidate.Generation, headGeneration)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE decomposition_plan SET state = ?, error = ?
		   WHERE decomposition_key = ? AND state IN (?, ?)`,
		landing, lost.Error(), candidate.Key,
		PlanPending, PlanAwaitingOperator); err != nil {
		return Replacement{}, fmt.Errorf("land the losing candidate: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Replacement{}, fmt.Errorf("commit landing: %w", err)
	}
	return Replacement{Head: head, Landing: landing}, nil
}

// replaceWithin is the replacement's body, inside the caller's transaction.
func replaceWithin(ctx context.Context, tx Tx, candidate Plan) (Replacement, error) {
	head, found, err := headWithin(ctx, tx, candidate.TargetKey)
	if err != nil {
		return Replacement{}, err
	}
	if !found {
		return bootstrapWithin(ctx, tx, candidate)
	}

	// ALREADY the head. A resumed plan re-enters the CAS — it must, since a
	// crash can land between the head move and activation — and comparing it
	// against itself makes it an equal-generation loser: buried historical,
	// with the head left pointing at a terminal plan. Owning the head is a
	// win, idempotently.
	if head.Current == candidate.Key {
		return Replacement{Won: true, Head: head}, errAlreadyHead
	}

	// The head's PLAN's intake generation, not the head row's own counter.
	// The two are different numbers and comparing the wrong one would let a
	// stale intake win against a head that had merely moved often.
	var headGeneration int
	var headState string
	err = tx.QueryRowContext(ctx,
		`SELECT generation, state FROM decomposition_plan WHERE decomposition_key = ?`,
		head.Current).Scan(&headGeneration, &headState)
	if errors.Is(err, sql.ErrNoRows) {
		return Replacement{}, fmt.Errorf("head of %s names plan %s, which does not exist",
			candidate.TargetKey, head.Current)
	}
	if err != nil {
		return Replacement{}, fmt.Errorf("read the head plan: %w", err)
	}

	// The head's plan must be ACTIVE. Replacing a head whose plan is itself
	// mid-replacement would interleave two supersessions over one predecessor.
	if headState != PlanActive || !Eligible(candidate.Generation, headGeneration) {
		// The verdict travels as an error so the advance rolls back: a
		// candidate that moved no head must move no epoch. Its landing is
		// written afterwards, in its own transaction.
		return Replacement{}, &errLostCAS{
			head: head, landing: LandingFor(candidate.Generation, headGeneration),
		}
	}
	return moveHeadWithin(ctx, tx, candidate, head)
}

// moveHeadWithin performs the winning replacement's writes.
//
// Every one of them in the caller's transaction: the predecessor's
// supersession, the successor's predecessor pointer, the head move, the
// generation bump and the fence. A crash between any two would leave a head
// pointing at a plan that never superseded its predecessor.
func moveHeadWithin(
	ctx context.Context, tx Tx, candidate Plan, head PlanHead,
) (Replacement, error) {
	if _, err := tx.ExecContext(ctx,
		`UPDATE decomposition_plan SET state = ?, superseded_by = ?
		   WHERE decomposition_key = ? AND state = ?`,
		PlanSuperseded, candidate.Key, head.Current, PlanActive); err != nil {
		return Replacement{}, fmt.Errorf("supersede the predecessor: %w", err)
	}
	// The successor's predecessor pointer, in the SAME transaction. Written
	// afterwards, a crash in between leaves a successor that cannot find the
	// plan it replaced — and a superseded plan whose tickets nothing will
	// ever retire, holding the epic open forever.
	if _, err := tx.ExecContext(ctx,
		`UPDATE decomposition_plan SET predecessor = ? WHERE decomposition_key = ?`,
		head.Current, candidate.Key); err != nil {
		return Replacement{}, fmt.Errorf("point the successor at its predecessor: %w", err)
	}
	// The fence rises HERE, with the head move, and is lowered by a separate
	// activation once retirement has finished.
	//
	// TRANSFERRED from the predecessor, not stacked on top of it. A plain
	// increment leaks: an active-but-incomplete head still holds its own
	// outstanding contribution, so replacing it made the fence 2 — and the
	// predecessor can never lower it again, because Activate only matches the
	// CURRENT head. The successor's activation takes it to 1 and pops are
	// refused forever. There is one head, so there is at most one outstanding
	// contribution, and the new head inherits it rather than adding a second.
	if _, err := tx.ExecContext(ctx,
		`UPDATE plan_head
		    SET current = ?, generation = generation + 1,
		        fence = CASE WHEN fence > 0 THEN fence ELSE fence + 1 END
		   WHERE target_key = ? AND current = ?`,
		candidate.Key, candidate.TargetKey, head.Current); err != nil {
		return Replacement{}, fmt.Errorf("move the head: %w", err)
	}
	// The GLOBAL fence rises only when this target did not already hold a
	// contribution — the same transfer, expressed across targets. Raising it
	// unconditionally would count one unactivated head twice, and the
	// activation that follows would leave admissions refused everywhere.
	fence := head.Fence
	raise := 1
	if fence > 0 {
		raise = 0
	}
	if fence == 0 {
		fence = 1
	}
	if err := raiseWithin(ctx, tx, raise); err != nil {
		return Replacement{}, err
	}
	return Replacement{
		Won: true,
		Head: PlanHead{
			TargetKey: candidate.TargetKey, Current: candidate.Key,
			Generation: head.Generation + 1, Fence: fence,
		},
		Predecessor: head.Current,
	}, nil
}

// bootstrapWithin installs the first head, the insert itself being the CAS.
func bootstrapWithin(ctx context.Context, tx Tx, candidate Plan) (Replacement, error) {
	// ON CONFLICT DO NOTHING, then read back: a concurrent first
	// decomposition that beat us leaves its own row, and the read tells us
	// whose it is. With no predecessor the retirement set is empty, so the
	// fence starts raised and the ordinary activation lowers it — one path,
	// not a special case that skips the fence.
	res, err := tx.ExecContext(ctx,
		`INSERT INTO plan_head (target_key, current, generation, fence)
		 VALUES (?, ?, 1, 1) ON CONFLICT(target_key) DO NOTHING`,
		candidate.TargetKey, candidate.Key)
	if err != nil {
		return Replacement{}, fmt.Errorf("install the first head: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return Replacement{}, fmt.Errorf("install the first head: %w", err)
	}
	if rows == 1 {
		if err := raiseWithin(ctx, tx, 1); err != nil {
			return Replacement{}, err
		}
		return Replacement{Won: true, Head: PlanHead{
			TargetKey: candidate.TargetKey, Current: candidate.Key, Generation: 1, Fence: 1,
		}}, nil
	}
	// Lost the insert race. Re-read and take the ordinary verdict.
	return replaceWithin(ctx, tx, candidate)
}

// headWithin reads the head row inside a transaction.
func headWithin(ctx context.Context, tx Tx, targetKey string) (PlanHead, bool, error) {
	h := PlanHead{TargetKey: targetKey}
	err := tx.QueryRowContext(ctx,
		`SELECT current, generation, fence FROM plan_head WHERE target_key = ?`, targetKey).
		Scan(&h.Current, &h.Generation, &h.Fence)
	if errors.Is(err, sql.ErrNoRows) {
		return PlanHead{}, false, nil
	}
	if err != nil {
		return PlanHead{}, false, fmt.Errorf("read plan head: %w", err)
	}
	return h, true, nil
}

// Activate lowers the pop fence for a head that has finished retiring.
//
// Its OWN transaction. Lowering the fence in the head-moving transaction
// would admit pops in the window before retirement ran — against a plan whose
// predecessor still holds live tickets.
func (s SQLHeads) Activate(ctx context.Context, key string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin activation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// The plan's state is checked HERE, in the same statement, not by the
	// caller. Every retry path reaches activation, and one that replayed it
	// for a parked, superseded or mid-phase plan would admit pops against a
	// head that is not ready — which is the whole thing the fence prevents.
	res, err := tx.ExecContext(ctx,
		`UPDATE plan_head SET fence = fence - 1
		   WHERE current = ? AND fence > 0
		     AND EXISTS (SELECT 1 FROM decomposition_plan
		                  WHERE decomposition_key = ? AND state = ? AND completed = 1)`,
		key, key, PlanActive)
	if err != nil {
		return fmt.Errorf("activate plan %s: %w", key, err)
	}
	moved, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("activate plan %s: %w", key, err)
	}
	// The global counter falls only when this head's own contribution did.
	// A replayed activation moves neither, which is what keeps the counter
	// from going negative and admitting pops against a later replacement.
	if moved == 1 {
		if err := lowerWithin(ctx, txExec{tx: tx}); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit activation: %w", err)
	}
	return nil
}
