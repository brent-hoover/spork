package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
)

// Verdict kinds kriya acts on.
const (
	VerdictChangesRequested = "changes-requested"
	VerdictApproved         = "approved"
)

// VerdictEvent is a review verdict as the feed reports it.
type VerdictEvent struct {
	// ID is the immutable event id. It is what a resubmission answers and
	// what a merge attempt's identity includes.
	ID string
	// Review is the review the verdict landed on.
	Review string
	// Session is the dev session the review was stamped with. Feedback routes
	// back by it — to the same agent instance where possible.
	Session string
	Verdict string
}

// Feed reads review verdicts from the tracker.
type Feed interface {
	// Since returns verdicts after a cursor, and the cursor to resume from.
	Since(ctx context.Context, cursor string) ([]VerdictEvent, string, error)
}

// Cursors persists how far the feed has been consumed.
type Cursors interface {
	Current(ctx context.Context, name string) (string, error)
	Advance(ctx context.Context, name, cursor string) error
}

// Routes finds the run a verdict belongs to.
type Routes interface {
	// BySession returns the run whose review was stamped with this session.
	// AC-rework-routing routes by SESSION, not by review: the session is what
	// identifies the agent instance that wrote the code.
	BySession(ctx context.Context, session string) (BuildRun, bool, error)
}

// Revisions reads a review's current revision.
//
// The verdict event does not carry one, so the review is asked. A resubmission
// fenced on a guessed revision would be refused, or worse advance a revision
// the human never saw.
type Revisions interface {
	Revision(ctx context.Context, review string) (int, error)
}

// Router turns review verdicts into work.
type Router struct {
	Feed    Feed
	Cursors Cursors
	Routes  Routes
	Reviews Revisions
	Store   Store
	// Name scopes the cursor, so two consumers of one feed do not advance
	// each other's position.
	Name string
}

// Routed is one verdict's outcome.
type Routed struct {
	Run     BuildRun
	Verdict VerdictEvent
	// Revision is the review's revision at the moment the verdict landed.
	//
	// From the REVIEW, because the event payload carries none — and the
	// consume and resubmit fences both name it. An initial submission records
	// no revision on the run, so reading it from there enqueued approvals at
	// revision zero and the tracker refused every one.
	Revision int
	// Reworked reports that the run was returned to the pair loop.
	Reworked bool
}

// Consume reads new verdicts and routes each to its run.
//
// It returns the cursor to resume from WITHOUT advancing it. Routing is only
// half the work — the caller still enqueues merges and drives runs — and a
// cursor advanced here is advanced before any of that, so a failure in the
// caller loses the events permanently: the feed never offers them again and
// the reviews wait forever. The caller calls Advance once it has acted.
//
// Re-delivery is the safe direction. Every act these events lead to is keyed:
// an approval enqueues under the event's own key and collides rather than
// duplicating, and a rework rewrites the same fields with the same values.
func (r Router) Consume(ctx context.Context) ([]Routed, string, error) {
	cursor, err := r.Cursors.Current(ctx, r.Name)
	if err != nil {
		return nil, "", fmt.Errorf("read feed cursor: %w", err)
	}
	verdicts, next, err := r.Feed.Since(ctx, cursor)
	if err != nil {
		return nil, "", fmt.Errorf("read verdicts: %w", err)
	}

	var out []Routed
	for _, v := range verdicts {
		routed, err := r.route(ctx, v)
		if err != nil {
			return out, "", err
		}
		if routed != nil {
			out = append(out, *routed)
		}
	}
	if next == cursor {
		// Nothing new. Reported as no cursor at all, so a caller that advances
		// unconditionally writes nothing.
		return out, "", nil
	}
	return out, next, nil
}

// Advance moves the cursor past a page the caller has finished acting on.
//
// An empty cursor advances nothing: there was no new page.
func (r Router) Advance(ctx context.Context, cursor string) error {
	if cursor == "" {
		return nil
	}
	if err := r.Cursors.Advance(ctx, r.Name, cursor); err != nil {
		return fmt.Errorf("advance feed cursor: %w", err)
	}
	return nil
}

// route sends one verdict to its run.
//
// A verdict for a session kriya does not know is SKIPPED, not an error: the
// feed is shared, and another consumer's reviews are none of kriya's business.
func (r Router) route(ctx context.Context, v VerdictEvent) (*Routed, error) {
	run, found, err := r.Routes.BySession(ctx, v.Session)
	if err != nil {
		return nil, fmt.Errorf("route verdict %s: %w", v.ID, err)
	}
	if !found {
		return nil, nil
	}
	// The revision comes from the REVIEW, not the event — the payload has none
	// — and both fences name it: a resubmission fenced on a stale revision is
	// refused, and an approval consumed at revision zero is refused too.
	revision, err := r.Reviews.Revision(ctx, v.Review)
	if err != nil {
		return nil, fmt.Errorf("read revision of %s: %w", v.Review, err)
	}
	if v.Verdict != VerdictChangesRequested {
		// An approval is the merge queue's business, not the pair loop's.
		return &Routed{Run: run, Verdict: v, Revision: revision}, nil
	}
	// The event is persisted BEFORE the run moves: a resubmission answers
	// this verdict, and one that could not name it would be answering
	// whatever the review said last.
	run.ReviewVerdictEvent = v.ID
	run.ReviewRevision = revision
	run.State = StateDevLoop
	if err := r.Store.Upsert(ctx, run); err != nil {
		return nil, fmt.Errorf("record reworking run: %w", err)
	}
	return &Routed{Run: run, Verdict: v, Revision: revision, Reworked: true}, nil
}

// Event kinds the tracker emits for a verdict.
//
// The KIND is the verdict. The payload carries the review, its issue and the
// session — never the verdict itself and never a revision, so neither can be
// read from it.
const (
	KindApproved         = "review.approved"
	KindChangesRequested = "review.changes-requested"
)

// verdictPayload is what a review verdict event carries.
type verdictPayload struct {
	Review  string `json:"review"`
	Session string `json:"session"`
}

// ParseVerdict reads a feed event into this package's shape.
//
// The verdict comes from the KIND, and the revision from the review itself:
// the payload has neither. Exported because the composition root maps the
// tracker's events, and the mapping is the one place that knows both shapes.
func ParseVerdict(id, kind string, payload json.RawMessage) (VerdictEvent, error) {
	var p verdictPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return VerdictEvent{}, fmt.Errorf("parse verdict event %s: %w", id, err)
	}
	var verdict string
	switch kind {
	case KindApproved:
		verdict = VerdictApproved
	case KindChangesRequested:
		verdict = VerdictChangesRequested
	default:
		return VerdictEvent{}, fmt.Errorf("event %s is not a verdict: %q", id, kind)
	}
	return VerdictEvent{ID: id, Review: p.Review, Session: p.Session, Verdict: verdict}, nil
}
