package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"

	"kriya/internal/planner"
)

// Popper claims the next workable ticket.
type Popper interface {
	// Pop returns the claimed ticket's id and title, both empty when nothing
	// is workable. Empty is NOT an error: idling is the ordinary state of a
	// plan whose remaining tickets are blocked or in flight.
	Pop(ctx context.Context, key string) (id, title string, err error)
}

// Builder runs one ticket to a terminal state.
type Builder func(ctx context.Context, issue, title string) (BuildRun, error)

// Ordinals persists how many pops a target has SETTLED.
//
// The number is what makes a pop key both replayable and fresh. A crash
// between the pop and the build leaves it unadvanced, so the replay presents
// the same key and the tracker returns the same claim rather than taking a
// second ticket and abandoning the first. A build that settles advances it, so
// the next pop is a new claim rather than a replay of a finished one.
type Ordinals interface {
	Current(ctx context.Context, targetKey string) (int, error)
	Advance(ctx context.Context, targetKey string, to int) error
}

// diagnose records a stall when an idle build cannot finish either.
//
// A build that CAN complete is idling on its way to the finish line, not
// stuck: recording a stall for it would put a healthy build in the inbox.
func (l Loop) diagnose(ctx context.Context) error {
	if l.Finish == nil || l.Stalls == nil {
		return nil
	}
	armed, reason, err := l.Finish.Detect(ctx, l.TargetKey)
	if err != nil {
		// "I could not tell whether the build is done" is not "it is fine".
		// Shrugging here idles forever with nothing recorded.
		return fmt.Errorf("detect completion for %s: %w", l.TargetKey, err)
	}
	if armed {
		return nil
	}
	epoch, err := l.epoch(ctx)
	if err != nil {
		return err
	}
	if err := l.Stalls.Record(ctx, l.TargetKey, epoch, reason); err != nil {
		return fmt.Errorf("record stall for %s: %w", l.TargetKey, err)
	}
	return nil
}

// epoch reads the target's completion epoch, or zero when nothing tracks it.
func (l Loop) epoch(ctx context.Context) (int, error) {
	if l.Epochs == nil {
		return 0, nil
	}
	epoch, err := l.Epochs.Current(ctx, l.TargetKey)
	if err != nil {
		return 0, fmt.Errorf("read completion epoch for %s: %w", l.TargetKey, err)
	}
	return epoch, nil
}

// popKey is deterministic per (target, pop ordinal).
func popKey(targetKey string, ordinal int) string {
	sum := sha256.Sum256([]byte("kriya-pop:" + targetKey + ":" + strconv.Itoa(ordinal)))
	return hex.EncodeToString(sum[:])
}

// Finishable answers whether a target's build may attempt to finish.
//
// The BUILD's finish line, not a ticket's: orchestrator.Completion is the
// close of one ticket after its merge, and this is the question of whether the
// whole target is done.
//
// Asked ONLY at the idle, because that is the only moment the answer changes
// anything: a loop with work in hand is plainly not finished, and asking on
// every pop would put a tracker round-trip in the hot path of a build that is
// obviously still going.
type Finishable interface {
	// Detect reports whether completion may be attempted, and what is holding
	// it back when it may not.
	Detect(ctx context.Context, targetKey string) (armed bool, reason string, err error)
}

// StallRecorder records a build that can neither proceed nor finish.
type StallRecorder interface {
	Record(ctx context.Context, targetKey string, epoch int, cause string) error
}

// Epoch reads a target's completion epoch, which scopes the stall key.
//
// A stall is about a moment in the target's life: after a supersession or a
// reopen the same-looking condition is a different one, and reusing the row
// would hide it behind an earlier resolution.
type Epoch interface {
	Current(ctx context.Context, targetKey string) (int, error)
}

// Admitter guards BuildRun admission against head transitions.
//
// The planner owns the fence and exposes this guard; the orchestrator invokes
// it and never writes the row itself. Read returns the version a later Admit
// must present — carrying it is the whole mechanism, because a read followed
// by an unconditional write is exactly the read-assert the fence exists to
// avoid.
type Admitter interface {
	Read(ctx context.Context) (planner.Fence, error)
	Admit(ctx context.Context, readVersion int) error
}

// Reserver writes a run and admits it in one transaction.
type Reserver interface {
	Reserve(ctx context.Context, run BuildRun, guard Admission, readVersion int) error
}

// Binder attaches a claim's result to a reservation.
type Binder interface {
	Bind(ctx context.Context, runID, issue, title string) error
}

// Settler settles a reservation whose claim returned nothing.
type Settler interface {
	SettleEmpty(ctx context.Context, runID string) error
}

// Admission is the planner's fence guard, invoked inside a transaction.
//
// The planner owns the fence and writes it; the orchestrator supplies only the
// transaction the write joins.
type Admission interface {
	AdmitWithin(ctx context.Context, tx planner.Tx, readVersion int) error
}

// Loop pops and builds until nothing is workable.
type Loop struct {
	Pops     Popper
	Build    Builder
	Ordinals Ordinals
	// Admit guards each pop against an unactivated head. Nil admits
	// everything, which is what a module-level test of the loop itself
	// wants; production wires it, because a pop admitted while any head is
	// unactivated is work started before its predecessor retired.
	Admit Admitter
	// Reserve writes the pop's BuildRun in the SAME transaction as the
	// admission, before the claim goes out. Nil skips the write-ahead, which
	// leaves a crash between the claim and the binding unrecoverable — the
	// claim exists in the tracker and nothing here owns it.
	Reserve Reserver
	// Finish and Stalls turn an idle into a diagnosis. Nil asks nothing and
	// records nothing, which is what a module-level test of the pop loop
	// itself wants.
	Finish Finishable
	Stalls StallRecorder
	// Epochs scopes the stall key. Nil records every stall at epoch zero,
	// which is right for a target nothing has ever advanced.
	Epochs Epoch
	// TargetKey scopes the pop keys to one target.
	TargetKey string
	// MaxTickets bounds one pass. Reaching it is not completion — it is this
	// pass ending — and the caller comes back.
	MaxTickets int
}

// Result is one pass of the loop.
type Result struct {
	// Built is every run this pass produced, in the order they were popped.
	Built []BuildRun
	// Idle reports that nothing was workable. Idling is not exiting: the
	// remaining tickets are blocked or in flight, and popping resumes when one
	// unblocks.
	Idle bool
}

// Run pops and builds until the tracker has nothing workable to give.
//
// It stops on the first build that does not settle: a run parked for the
// operator is a signal, and popping past it would bury it under more work.
func (l Loop) Run(ctx context.Context) (Result, error) {
	limit := l.MaxTickets
	if limit <= 0 {
		limit = 32
	}
	ordinal, err := l.Ordinals.Current(ctx, l.TargetKey)
	if err != nil {
		return Result{}, fmt.Errorf("read pop ordinal: %w", err)
	}

	var out Result
	for range limit {
		// BEFORE the claim: the run is written and the fence taken in one
		// transaction, and only then is the tracker asked. Admitted after,
		// the ticket is already claimed and refusing then strands it; written
		// after, a crash in the window leaves a claim nothing owns.
		key := popKey(l.TargetKey, ordinal)
		reserved, admitted, err := l.reserve(ctx, key)
		if err != nil {
			return out, err
		}
		if !admitted {
			// A head is mid-supersession. Idling is right: the work is not
			// gone, it is waiting for retirement to finish.
			out.Idle = true
			return out, nil
		}
		issue, title, err := l.Pops.Pop(ctx, key)
		if err != nil {
			return out, fmt.Errorf("pop: %w", err)
		}
		if issue == "" {
			// The claim returned an explicitly empty result: nothing was
			// claimed, so the reservation owns nothing. It settles as no-work
			// rather than lingering as a queued run recovery would try to
			// bind — and no work is invented for it.
			if err := l.settleEmpty(ctx, reserved); err != nil {
				return out, err
			}
			return l.idle(ctx, out, ordinal)
		}
		if err := l.bind(ctx, reserved, issue, title); err != nil {
			return out, err
		}
		run, buildErr := l.Build(ctx, issue, title)
		out.Built = append(out.Built, run)
		if buildErr != nil {
			// The ordinal does NOT advance. A build that did not settle is one
			// the next pass must resume on the same claim.
			return out, fmt.Errorf("build %s: %w", title, buildErr)
		}
		ordinal++
		if err := l.Ordinals.Advance(ctx, l.TargetKey, ordinal); err != nil {
			return out, fmt.Errorf("advance pop ordinal: %w", err)
		}
	}
	return out, nil
}

// idle ends a pass that found nothing workable.
//
// The plan is not finished — its remaining tickets are blocked or in flight —
// so the loop idles rather than reporting completion.
//
// The ordinal ADVANCES anyway. The tracker settles an empty pop under its key
// like any other response, so leaving the ordinal where it is would replay
// "nothing workable" forever, even after a ticket unblocks. Nothing was
// claimed, so nothing is lost by moving past it.
func (l Loop) idle(ctx context.Context, out Result, ordinal int) (Result, error) {
	out.Idle = true
	if err := l.Ordinals.Advance(ctx, l.TargetKey, ordinal+1); err != nil {
		return out, fmt.Errorf("advance pop ordinal: %w", err)
	}
	// Nothing workable is either a build about to finish or a build that is
	// STUCK, and the two look identical from here. Asking is what separates
	// them, and the answer is durable: a stall the operator can act on rather
	// than a process quietly spinning.
	if err := l.diagnose(ctx); err != nil {
		return out, err
	}
	return out, nil
}

// reserve writes the pop's run and takes an admission slot together.
//
// One transaction, because they are one fact. It returns the reserved run so
// the caller can bind it to whatever the claim returns — the run exists
// BEFORE the claim, which is what makes a crash in that window recoverable
// rather than an orphaned claim.
func (l Loop) reserve(ctx context.Context, key string) (BuildRun, bool, error) {
	if l.Reserve == nil || l.Admit == nil {
		admitted, err := l.admit(ctx)
		return BuildRun{}, admitted, err
	}
	guard, ok := l.Admit.(Admission)
	if !ok {
		return BuildRun{}, false, fmt.Errorf(
			"the admitter %T cannot join a reservation transaction", l.Admit)
	}

	run := BuildRun{ID: key, Plan: l.TargetKey, PopKey: key, State: StateReserved}
	for range 2 {
		fence, err := l.Admit.Read(ctx)
		if err != nil {
			return BuildRun{}, false, fmt.Errorf("read the pop fence: %w", err)
		}
		err = l.Reserve.Reserve(ctx, run, guard, fence.Version)
		if err == nil {
			return run, true, nil
		}
		var fenced *planner.ErrFenced
		if !errors.As(err, &fenced) {
			return BuildRun{}, false, fmt.Errorf("reserve the pop: %w", err)
		}
		if !fenced.StaleVersion {
			return BuildRun{}, false, nil
		}
	}
	// Two lost races in a row is contention, not a closed fence.
	return BuildRun{}, false, nil
}

// bind attaches the claim's result to the reservation.
//
// The run existed BEFORE the claim, so this is the write that turns "a claim
// may have happened" into "this run owns that ticket". State is left QUEUED:
// what kind of work it is, and therefore which path it takes, is the builder's
// to decide from the plan.
func (l Loop) bind(ctx context.Context, reserved BuildRun, issue, title string) error {
	if l.Reserve == nil || reserved.ID == "" {
		return nil
	}
	binder, ok := l.Reserve.(Binder)
	if !ok {
		return nil
	}
	if err := binder.Bind(ctx, reserved.ID, issue, title); err != nil {
		return fmt.Errorf("bind the reserved pop to %s: %w", issue, err)
	}
	return nil
}

// settleEmpty moves a reservation that claimed nothing to no-work.
func (l Loop) settleEmpty(ctx context.Context, reserved BuildRun) error {
	if l.Reserve == nil || reserved.ID == "" {
		return nil
	}
	settler, ok := l.Reserve.(Settler)
	if !ok {
		return nil
	}
	if err := settler.SettleEmpty(ctx, reserved.ID); err != nil {
		return fmt.Errorf("settle the empty pop %s: %w", Short(reserved.ID), err)
	}
	return nil
}

// The run's ID is the POP KEY, deliberately. The key is stable across
// attempts — same target, same ordinal — so a retried reservation lands on the
// same row. A fresh identifier per attempt inserted a SECOND run for one
// idempotent pop, and ForTicket then picked the newest bare reservation,
// abandoning the original run and everything it had done.

// admit takes an admission slot, retrying once on a lost race.
//
// A stale version means another writer moved the fence between the read and
// the CAS — a race worth retrying at once, since the fence may well still be
// clear. A nonzero counter is not: it means a head is unactivated, and the
// answer will not change until one activates.
func (l Loop) admit(ctx context.Context) (bool, error) {
	if l.Admit == nil {
		return true, nil
	}
	for range 2 {
		fence, err := l.Admit.Read(ctx)
		if err != nil {
			return false, fmt.Errorf("read the pop fence: %w", err)
		}
		err = l.Admit.Admit(ctx, fence.Version)
		if err == nil {
			return true, nil
		}
		var fenced *planner.ErrFenced
		if !errors.As(err, &fenced) {
			return false, fmt.Errorf("pop admission: %w", err)
		}
		if !fenced.StaleVersion {
			return false, nil
		}
	}
	// Two lost races in a row is contention, not a closed fence. Idling lets
	// the caller come back rather than spinning here.
	return false, nil
}

// Short truncates an identifier for a message, safely.
func Short(id string) string { return planner.Short(id) }
