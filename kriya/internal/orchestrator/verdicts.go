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
	Session  string
	Verdict  string
	Revision int
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

// Router turns review verdicts into work.
type Router struct {
	Feed    Feed
	Cursors Cursors
	Routes  Routes
	Store   Store
	// Name scopes the cursor, so two consumers of one feed do not advance
	// each other's position.
	Name string
}

// Routed is one verdict's outcome.
type Routed struct {
	Run     BuildRun
	Verdict VerdictEvent
	// Reworked reports that the run was returned to the pair loop.
	Reworked bool
}

// Consume reads new verdicts and routes each to its run.
//
// The cursor advances only after every verdict in a page has been routed. A
// cursor advanced first would lose the ones that had not been acted on yet,
// and a verdict nobody acted on is a review waiting forever.
func (r Router) Consume(ctx context.Context) ([]Routed, error) {
	cursor, err := r.Cursors.Current(ctx, r.Name)
	if err != nil {
		return nil, fmt.Errorf("read feed cursor: %w", err)
	}
	verdicts, next, err := r.Feed.Since(ctx, cursor)
	if err != nil {
		return nil, fmt.Errorf("read verdicts: %w", err)
	}

	var out []Routed
	for _, v := range verdicts {
		routed, err := r.route(ctx, v)
		if err != nil {
			return out, err
		}
		if routed != nil {
			out = append(out, *routed)
		}
	}
	if next != "" && next != cursor {
		if err := r.Cursors.Advance(ctx, r.Name, next); err != nil {
			return out, fmt.Errorf("advance feed cursor: %w", err)
		}
	}
	return out, nil
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
	if v.Verdict != VerdictChangesRequested {
		// An approval is the merge queue's business, not the pair loop's.
		return &Routed{Run: run, Verdict: v}, nil
	}
	// The event and revision are persisted BEFORE the run moves: a
	// resubmission answers this verdict, and one that could not name it would
	// be answering whatever the review said last.
	run.ReviewVerdictEvent = v.ID
	run.ReviewRevision = v.Revision
	run.State = StateDevLoop
	if err := r.Store.Upsert(ctx, run); err != nil {
		return nil, fmt.Errorf("record reworking run: %w", err)
	}
	return &Routed{Run: run, Verdict: v, Reworked: true}, nil
}

// verdictPayload is what a review verdict event carries.
type verdictPayload struct {
	Review   string `json:"review"`
	Session  string `json:"session"`
	Verdict  string `json:"verdict"`
	Revision int    `json:"revision"`
}

// ParseVerdict reads a feed event's payload.
//
// Exported because the composition root maps the tracker's events onto this
// package's shape, and the mapping is the one place that knows both.
func ParseVerdict(id string, payload json.RawMessage) (VerdictEvent, error) {
	var p verdictPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return VerdictEvent{}, fmt.Errorf("parse verdict event %s: %w", id, err)
	}
	return VerdictEvent{
		ID: id, Review: p.Review, Session: p.Session,
		Verdict: p.Verdict, Revision: p.Revision,
	}, nil
}
