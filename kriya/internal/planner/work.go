package planner

import (
	"context"
	"fmt"
)

// Event kinds the tracker emits that can return work to a target.
const (
	KindStatusChanged = "issue.status-changed"
	KindCreated       = "issue.created"
)

// WorkEvent is one lifecycle event as the feed reports it.
//
// The payload is deliberately absent: sutra emits status changes with none, so
// what an event MEANS is a question about the issue's live state, not about
// the event. Operation is the server transaction id — the evidence a cascade
// verification correlates against.
type WorkEvent struct {
	ID        string
	Kind      string
	Subject   string
	Operation string
}

// WorkFeed reads lifecycle events from the tracker.
type WorkFeed interface {
	Since(ctx context.Context, cursor string) ([]WorkEvent, string, error)
}

// IssueStates answers what an issue's status currently is.
type IssueStates interface {
	Status(ctx context.Context, id string) (string, error)
}

// Cursors persists how far a consumer has read.
//
// Declared here rather than imported: what this needs is a durable position,
// and the module that stores one is the composition root's business.
type Cursors interface {
	Current(ctx context.Context, name string) (string, error)
	Advance(ctx context.Context, name, cursor string) error
}

// Work advances a target's completion epoch when its work returns.
//
// The epoch is what makes an approval spendable exactly once against exactly
// the work it was given for, and nothing else moves it: without this consumer
// a stamped target stays complete forever, a stale approval is never fenced,
// and recompletion never happens.
type Work struct {
	Feed    WorkFeed
	Issues  IssueStates
	Epochs  Epochs
	Tickets TargetTickets
	Claims  ClaimStore
	Cursors Cursors
	// TargetKey and Epic scope the feed to this build. The feed is
	// project-wide, and an issue this target never planned belongs to
	// somebody else's.
	TargetKey string
	Epic      string
	// Actor scopes the cursor alongside the target, so two identities
	// consuming one project do not advance each other's position.
	Actor string
}

// Name is the cursor this consumer reads under.
func (w Work) Name() string { return "work:" + w.Actor + ":" + w.TargetKey }

// Consume reads new lifecycle events and advances the epoch for each that
// returns work to this target.
//
// It returns the cursor to resume from WITHOUT advancing it. Every advance is
// keyed by its event, so re-reading a page is safe; losing one is not.
func (w Work) Consume(ctx context.Context) (advanced int, next string, err error) {
	cursor, err := w.Cursors.Current(ctx, w.Name())
	if err != nil {
		return 0, "", fmt.Errorf("read work cursor: %w", err)
	}
	events, next, err := w.Feed.Since(ctx, cursor)
	if err != nil {
		return 0, "", fmt.Errorf("read work events: %w", err)
	}
	for _, event := range events {
		moved, err := w.consume(ctx, event)
		if err != nil {
			return advanced, "", err
		}
		if moved {
			advanced++
		}
	}
	if next == cursor {
		// Nothing new. Reported as no cursor at all, so a caller that
		// advances unconditionally writes nothing.
		return advanced, "", nil
	}
	return advanced, next, nil
}

// Advance moves the cursor past a page the caller has finished acting on.
func (w Work) Advance(ctx context.Context, cursor string) error {
	if cursor == "" {
		return nil
	}
	if err := w.Cursors.Advance(ctx, w.Name(), cursor); err != nil {
		return fmt.Errorf("advance work cursor: %w", err)
	}
	return nil
}

// consume decides whether one event returns work to this target.
func (w Work) consume(ctx context.Context, event WorkEvent) (bool, error) {
	if event.Kind != KindStatusChanged {
		// Creations and relation changes are attribution's business, and
		// attribution is a lifecycle of its own. Advancing on them without it
		// would rotate the epoch for work that may belong to another target.
		return false, nil
	}
	cause, scoped, err := w.scope(ctx, event.Subject)
	if err != nil || !scoped {
		return false, err
	}
	// Only a target with something to invalidate. Mid-build, tickets move
	// between statuses constantly, and advancing on each would rotate the
	// epoch — and every key derived from it — for no reason.
	claimed, err := w.hasClaim(ctx)
	if err != nil || !claimed {
		return false, err
	}
	// The event carries no payload, so the LIVE status is what says whether
	// this was a reopen. "I could not read it" is not "it is still complete":
	// skipping would leave a stamp standing over work that came back.
	status, err := w.Issues.Status(ctx, event.Subject)
	if err != nil {
		return false, fmt.Errorf("read status of %s: %w", event.Subject, err)
	}
	if !activeStatuses[status] {
		return false, nil
	}
	return w.Epochs.OnEvent(ctx, w.TargetKey, cause, event.ID)
}

// scope reports whether an issue belongs to this target, and what its return
// to activity means.
//
// The EPIC's own reopen is a different cause from a ticket's: deferred work
// activating or an active subtree being attached, which recovery reconciles
// differently from a child coming back.
func (w Work) scope(ctx context.Context, issue string) (cause string, scoped bool, err error) {
	if issue == w.Epic {
		return CauseDeferredActivation, true, nil
	}
	tickets, err := w.Tickets.ForTarget(ctx, w.TargetKey)
	if err != nil {
		return "", false, fmt.Errorf("read planned tickets: %w", err)
	}
	for _, t := range tickets {
		if t.IssueID == issue {
			return CauseTicketReopen, true, nil
		}
	}
	return "", false, nil
}

// hasClaim reports whether this target has a completion to invalidate.
func (w Work) hasClaim(ctx context.Context) (bool, error) {
	claim, found, err := w.Claims.Find(ctx, w.TargetKey)
	if err != nil {
		return false, fmt.Errorf("read completion claim: %w", err)
	}
	if !found {
		return false, nil
	}
	// A claim awaiting a human is exactly what returning work must
	// invalidate: they are being asked about a build that has changed
	// underneath them.
	switch claim.State {
	case CompletionNone, CompletionStale:
		return false, nil
	}
	return true, nil
}
