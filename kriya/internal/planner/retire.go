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
	StatusOpen       = "open"
	StatusComplete   = "complete"
	StatusDeferred   = "deferred"
	StatusInProgress = "in-progress"
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
	// Tracker replays a creation step whose outcome was never recorded. The
	// call goes out under the step's PERSISTED key, so sutra returns the
	// original issue if the first attempt landed and creates it if not —
	// either way retirement learns the id it must defer.
	Tracker Tracker
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
	ctx context.Context, predecessor Plan, projectID string,
	carried map[string]bool, actor string,
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
		disposition, err := r.retireOne(ctx, predecessor, ticket, projectID, carried, live, actor)
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
	ctx context.Context, predecessor Plan, ticket Ticket, projectID string,
	carried map[string]bool, live map[string]bool, actor string,
) (string, error) {
	if ticket.IssueID == "" {
		// An empty id is NOT proof the ticket does not exist. The create may
		// have landed and crashed before its returned id was recorded — the
		// step is marked issued precisely to say "this may have happened".
		// Assuming it never did leaves a real, open sutra issue holding the
		// epic forever while its row is stamped retired.
		issue, err := r.settleCreate(ctx, predecessor, ticket, projectID, actor)
		if err != nil {
			return "", err
		}
		if issue == "" {
			// Its creation step is still pending: nothing was ever sent, so
			// there is nothing in sutra to defer.
			return DispositionRetired, nil
		}
		ticket.IssueID = issue
	}
	if live[ticket.IssueID] {
		// A popped build finishes under its pinned snapshot. Deferring it
		// would cancel work already in progress.
		return DispositionBound, nil
	}
	if carried[ticket.IssueID] {
		return DispositionCarriedForward, nil
	}

	return r.deferTicket(ctx, predecessor, ticket, actor)
}

// deferTicket reads a ticket's status and defers it, re-reading on conflict.
//
// The read and the transition cannot be atomic, so the status can change
// between them. Two things follow, and both are handled by RE-READING rather
// than by assuming:
//
//   - A conflict means the status moved. The fresh read may now say
//     in-progress, which is a build that started while this was in flight —
//     deferring it would cancel that build.
//   - sutra settles a REJECTED request under its idempotency key, so the
//     retry must present a NEW one. Retrying under the key that conflicted
//     replays the cached 409 forever, whatever the ticket's status by then.
func (r Retirement) deferTicket(
	ctx context.Context, predecessor Plan, ticket Ticket, actor string,
) (string, error) {
	// Bounded: one re-read after a conflict. A ticket whose status keeps
	// moving is one something else is actively working, and looping here
	// would fight it. A later retirement pass picks it up with a fresh
	// attempt number, so nothing is lost by stopping.
	for range 2 {
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
		case StatusInProgress:
			// A build is working it, whatever the run table said a moment
			// ago. sutra's own status is the authority on that, and it is
			// fresher than any read kriya made first.
			return DispositionBound, nil
		}

		// The key is spent DURABLY before the call it is for. sutra settles a
		// rejected request under its key, so an attempt counter living only
		// in this loop restarted at zero on the next retirement pass and
		// re-presented keys sutra had already refused — forever.
		attempt, err := r.Tickets.BumpDeferAttempt(ctx, predecessor.Key, ticket.Ordinal)
		if err != nil {
			return "", err
		}
		// The expectation is the status just OBSERVED, whatever it is.
		// Expecting "open" unconditionally made a blocked ticket conflict on
		// every attempt, against a state it would never return to.
		if err := r.Defer.Defer(ctx, ticket.IssueID, status, actor,
			DeferKey(predecessor.Key, ticket.Ordinal, attempt)); err == nil {
			return DispositionRetired, nil
		}
	}
	return "", fmt.Errorf("defer %s: its status kept moving under the transition", ticket.IssueID)
}

// settleCreate replays a creation step whose outcome was never recorded.
//
// It returns the issue the step produced, or empty when the step was never
// sent. The call goes out under the step's PERSISTED key, so sutra returns the
// original issue if the first attempt landed — which is exactly the case that
// makes an empty recorded id a lie rather than a fact.
func (r Retirement) settleCreate(
	ctx context.Context, predecessor Plan, ticket Ticket, projectID, actor string,
) (string, error) {
	if r.Steps == nil {
		// No durable sequence to consult. Nothing can be said about whether
		// the create landed, so nothing is claimed.
		return "", nil
	}
	steps, err := r.Steps.ForPlan(ctx, predecessor.Key)
	if err != nil {
		return "", fmt.Errorf("read the sequence of plan %s: %w", Short(predecessor.Key), err)
	}
	for _, step := range steps {
		if step.Kind != StepCreate || step.Ordinal != ticket.Ordinal {
			continue
		}
		if step.Issue != "" {
			return step.Issue, nil
		}
		if step.State == StepPending {
			// Never sent. There is genuinely nothing in sutra.
			return "", nil
		}
		// ISSUED, with no recorded result. Replay it to its terminal answer.
		if r.Tracker == nil {
			// A missing tracker is NOT proof the issue does not exist. Treated
			// as one, the row is consumed and its real, open sutra issue holds
			// the epic forever. Refusing names the misconfiguration instead.
			return "", fmt.Errorf(
				"ticket %d of plan %s has an issued creation step and no tracker to replay it",
				ticket.Ordinal, Short(predecessor.Key))
		}
		issue, err := r.Tracker.CreateIssue(ctx, projectID, ticket.Title, ticket.Body, actor, step.Key)
		if err != nil {
			return "", fmt.Errorf("replay the creation of ticket %d: %w", ticket.Ordinal, err)
		}
		// Recorded, so a later pass does not replay it again.
		if err := r.Steps.Mark(ctx, predecessor.Key, step.Seq, StepComplete, issue); err != nil {
			return "", err
		}
		return issue, nil
	}
	return "", nil
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
// The ATTEMPT is in it for a second reason: sutra settles a rejected request
// under its key, so a conditional transition that conflicted would replay that
// cached 409 on every retry — forever, whatever the ticket's status by then.
func DeferKey(predecessorKey string, ordinal, attempt int) string {
	return idempotencyKey(fmt.Sprintf("defer-%d-%d", ordinal, attempt), predecessorKey, "")
}
