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

// popKey is deterministic per (target, pop ordinal).
func popKey(targetKey string, ordinal int) string {
	sum := sha256.Sum256([]byte("kriya-pop:" + targetKey + ":" + strconv.Itoa(ordinal)))
	return hex.EncodeToString(sum[:])
}

// Loop pops and builds until nothing is workable.
type Loop struct {
	Pops     Popper
	Build    Builder
	Ordinals Ordinals
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
