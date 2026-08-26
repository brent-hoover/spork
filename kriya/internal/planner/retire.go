package planner

import (
	"context"
	"fmt"
)

// Dispositions a predecessor row is consumed with.
//
// Every retired row gets exactly one, and which one decides whether sutra is
// touched at all. Guessing here is expensive in both directions: deferring a
// ticket a live build is working cancels real work, and leaving one open that
// nothing will build blocks the successor's completion forever.
const (
	// DispositionRetired is work the successor dropped. Its ticket is
	// deferred in sutra so it stops holding the epic open.
	DispositionRetired = "retired"
	// DispositionCarriedForward is work the successor selected to keep. Its
	// ticket stays exactly as it is.
	DispositionCarriedForward = "carried-forward"
	// DispositionBound is work a LIVE build already claimed. It finishes
	// under its pinned snapshot — deferring it would cancel a run mid-flight.
	DispositionBound = "bound"
	// DispositionCompleted is work that already shipped. There is nothing to
	// defer and nothing to carry.
	DispositionCompleted = "completed"
)

// Tracker issue statuses retirement reasons about.
//
// sutra's own vocabulary, not kriya's: these are compared against what the
// tracker returns and sent back as a conditional transition's expectation, so
// a private spelling would silently never match.
const (
	StatusOpen     = "open"
	StatusComplete = "complete"
	StatusDeferred = "deferred"
)

// Deferrer is the slice of the tracker retirement needs.
type Deferrer interface {
	// Status reads an issue's CURRENT status. Freshly, because the conditional
	// transition's expectation must be what the tracker holds now — a stale
	// "open" expectation against a ticket that has since become blocked
	// conflicts forever, retrying against a state that will never return.
	Status(ctx context.Context, issue string) (string, error)
	// Defer transitions an issue to deferred, expecting `expect`.
	Defer(ctx context.Context, issue, expect, actor, key string) error
}

// LiveWork reports which issues a live build currently holds.
type LiveWork interface {
	Claimed(ctx context.Context, issues []string) (map[string]bool, error)
}

// Retirement consumes a superseded plan's rows before its successor activates.
type Retirement struct {
	Tickets TicketStore
	Steps   StepStore
	Defer   Deferrer
	Live    LiveWork
}

// Run stamps every predecessor row consumed with its disposition.
//
// carried is the set of predecessor ISSUE IDS the successor selected to keep.
// It is an input rather than something inferred here: whether amended work is
// the same work is the successor plan's judgement, and guessing it by title
// would silently carry forward a ticket whose acceptance criteria changed.
// Decomposition passes an empty set today, because a successor currently
// creates all of its own tickets and carries nothing — the disposition exists
// and is stamped only for a selection that genuinely happened.
func (r Retirement) Run(
	ctx context.Context, predecessor Plan, carried map[string]bool, actor string,
) error {
	tickets, err := r.Tickets.ForPlan(ctx, predecessor.Key)
	if err != nil {
		return fmt.Errorf("read the tickets of plan %s: %w", Short(predecessor.Key), err)
	}
	live, err := r.claimed(ctx, tickets)
	if err != nil {
		return err
	}

	for _, ticket := range tickets {
		if ticket.Consumed {
			// Already retired. Re-deferring is harmless under the key, but
			// re-READING every ticket's status is a round trip per row on
			// every replay.
			continue
		}
		disposition, err := r.retireOne(ctx, predecessor, ticket, carried, live, actor)
		if err != nil {
			return err
		}
		if err := r.Tickets.Consume(ctx, predecessor.Key, ticket.Ordinal, disposition); err != nil {
			return fmt.Errorf("stamp ticket %d of plan %s consumed: %w",
				ticket.Ordinal, Short(predecessor.Key), err)
		}
	}
	return nil
}

// retireOne classifies one row and acts on it, returning its disposition.
func (r Retirement) retireOne(
	ctx context.Context, predecessor Plan, ticket Ticket,
	carried map[string]bool, live map[string]bool, actor string,
) (string, error) {
	if ticket.IssueID == "" {
		// Its creation step never landed. There is nothing in sutra to
		// defer, and calling anything here would be a request for an issue
		// that does not exist.
		return DispositionRetired, nil
	}
	if live[ticket.IssueID] {
		// A popped build finishes under its pinned snapshot. Deferring it
		// would cancel work already in progress.
		return DispositionBound, nil
	}
	if carried[ticket.IssueID] {
		return DispositionCarriedForward, nil
	}

	status, err := r.Defer.Status(ctx, ticket.IssueID)
	if err != nil {
		return "", fmt.Errorf("read the status of %s: %w", ticket.IssueID, err)
	}
	switch status {
	case StatusComplete:
		// Already shipped. Nothing to defer.
		return DispositionCompleted, nil
	case StatusDeferred:
		// The fence is already established. A retry here would present a
		// conditional transition expecting "deferred" and move nothing.
		return DispositionRetired, nil
	}

	// The expectation is the status just OBSERVED, whatever it is. Expecting
	// "open" unconditionally made a blocked ticket conflict on every attempt,
	// against a state it would never return to.
	if err := r.Defer.Defer(ctx, ticket.IssueID, status, actor,
		DeferKey(predecessor.Key, ticket.Ordinal)); err != nil {
		return "", fmt.Errorf("defer %s: %w", ticket.IssueID, err)
	}
	return DispositionRetired, nil
}

// claimed asks which of a plan's tickets a live build holds.
func (r Retirement) claimed(ctx context.Context, tickets []Ticket) (map[string]bool, error) {
	ids := make([]string, 0, len(tickets))
	for _, t := range tickets {
		if t.IssueID != "" {
			ids = append(ids, t.IssueID)
		}
	}
	if len(ids) == 0 {
		return map[string]bool{}, nil
	}
	live, err := r.Live.Claimed(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("read live claims: %w", err)
	}
	return live, nil
}

// DeferKey is the idempotency key for retiring one ticket of one plan.
//
// GENERATION-SCOPED through the predecessor's decomposition key, which carries
// the intake generation. A key derived from the ticket alone would be replayed
// by a later retirement of the same ticket under a different plan, and sutra
// would return the earlier result instead of performing the new transition.
func DeferKey(predecessorKey string, ordinal int) string {
	return idempotencyKey(fmt.Sprintf("defer-%d", ordinal), predecessorKey, "")
}
