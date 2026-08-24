package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
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
	// The epoch is zero until the completion lifecycle lands with
	// REQ-run-to-complete's claim protocol; the key it scopes is already
	// derived from it, so a later epoch's stall is already a distinct row.
	if err := l.Stalls.Record(ctx, l.TargetKey, 0, reason); err != nil {
		return fmt.Errorf("record stall for %s: %w", l.TargetKey, err)
	}
	return nil
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

// Loop pops and builds until nothing is workable.
type Loop struct {
	Pops     Popper
	Build    Builder
	Ordinals Ordinals
	// Finish and Stalls turn an idle into a diagnosis. Nil asks nothing and
	// records nothing, which is what a module-level test of the pop loop
	// itself wants.
	Finish Finishable
	Stalls StallRecorder
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
		issue, title, err := l.Pops.Pop(ctx, popKey(l.TargetKey, ordinal))
		if err != nil {
			return out, fmt.Errorf("pop: %w", err)
		}
		if issue == "" {
			// Nothing workable. The plan is not finished — its remaining
			// tickets are blocked or in flight — so the loop idles rather than
			// reporting completion.
			//
			// The ordinal ADVANCES anyway. The tracker settles an empty pop
			// under its key like any other response, so leaving the ordinal
			// where it is would replay "nothing workable" forever, even after
			// a ticket unblocks. Nothing was claimed, so nothing is lost by
			// moving past it.
			out.Idle = true
			ordinal++
			if err := l.Ordinals.Advance(ctx, l.TargetKey, ordinal); err != nil {
				return out, fmt.Errorf("advance pop ordinal: %w", err)
			}
			// Nothing workable is either a build about to finish or a build
			// that is STUCK, and the two look identical from here. Asking is
			// what separates them, and the answer is durable: a stall the
			// operator can act on rather than a process quietly spinning.
			if err := l.diagnose(ctx); err != nil {
				return out, err
			}
			return out, nil
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
