package orchestrator_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"kriya/internal/orchestrator"
)

// verdictFeed hands out pages from a cursor.
type verdictFeed struct {
	pages map[string][]orchestrator.VerdictEvent
	next  map[string]string
	asked []string
	err   error
}

func (f *verdictFeed) Since(
	_ context.Context, cursor string,
) ([]orchestrator.VerdictEvent, string, error) {
	f.asked = append(f.asked, cursor)
	if f.err != nil {
		return nil, "", f.err
	}
	return f.pages[cursor], f.next[cursor], nil
}

// memCursors persists a consumer's feed position.
type memCursors struct {
	at  map[string]string
	err error
}

func newMemCursors() *memCursors { return &memCursors{at: map[string]string{}} }

func (m *memCursors) Current(_ context.Context, name string) (string, error) {
	return m.at[name], nil
}

func (m *memCursors) Advance(_ context.Context, name, cursor string) error {
	if m.err != nil {
		return m.err
	}
	m.at[name] = cursor
	return nil
}

// sessionRoutes finds a run by the session its review was stamped with.
type sessionRoutes struct {
	bySession map[string]orchestrator.BuildRun
	err       error
}

func (s *sessionRoutes) BySession(
	_ context.Context, session string,
) (orchestrator.BuildRun, bool, error) {
	if s.err != nil {
		return orchestrator.BuildRun{}, false, s.err
	}
	run, ok := s.bySession[session]
	return run, ok, nil
}

func router(feed *verdictFeed, cursors *memCursors, routes *sessionRoutes, store *memStore) orchestrator.Router {
	return orchestrator.Router{
		Feed: feed, Cursors: cursors, Routes: routes, Store: store, Name: "verdicts",
	}
}

func submittedFor(session string) orchestrator.BuildRun {
	return orchestrator.BuildRun{
		ID: "run-" + session, Ticket: "KRI-1", State: orchestrator.StateReviewSubmitted,
		ReviewID: "review-1", ReviewSession: session, ReviewRevision: 1,
	}
}

func TestAChangesRequestedVerdictReturnsItsRunToThePairLoop(t *testing.T) {
	// Routed by SESSION: it is what identifies the agent instance that wrote
	// the code, which is where the feedback belongs.
	feed := &verdictFeed{
		pages: map[string][]orchestrator.VerdictEvent{"": {{
			ID: "event-9", Review: "review-1", Session: "sess-42",
			Verdict: orchestrator.VerdictChangesRequested, Revision: 2,
		}}},
		next: map[string]string{"": "cursor-1"},
	}
	store := newMemStore()
	routes := &sessionRoutes{bySession: map[string]orchestrator.BuildRun{
		"sess-42": submittedFor("sess-42"),
	}}
	got, err := router(feed, newMemCursors(), routes, store).Consume(context.Background())
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if len(got) != 1 || !got[0].Reworked {
		t.Fatalf("routed %+v", got)
	}
	run := store.rows["run-sess-42"]
	if run.State != orchestrator.StateDevLoop {
		t.Errorf("the run is in %q", run.State)
	}
	// The event and revision are persisted so a resubmission can name them.
	if run.ReviewVerdictEvent != "event-9" || run.ReviewRevision != 2 {
		t.Errorf("the run records event %q at revision %d",
			run.ReviewVerdictEvent, run.ReviewRevision)
	}
}

func TestAnApprovalIsNotThePairLoopsBusiness(t *testing.T) {
	feed := &verdictFeed{
		pages: map[string][]orchestrator.VerdictEvent{"": {{
			ID: "event-9", Review: "review-1", Session: "sess-42",
			Verdict: orchestrator.VerdictApproved, Revision: 2,
		}}},
		next: map[string]string{"": "cursor-1"},
	}
	store := newMemStore()
	routes := &sessionRoutes{bySession: map[string]orchestrator.BuildRun{
		"sess-42": submittedFor("sess-42"),
	}}
	got, err := router(feed, newMemCursors(), routes, store).Consume(context.Background())
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if len(got) != 1 || got[0].Reworked {
		t.Fatalf("routed %+v", got)
	}
	if _, moved := store.rows["run-sess-42"]; moved {
		t.Error("an approval moved the run out of review-submitted")
	}
}

func TestAVerdictForAnUnknownSessionIsSkipped(t *testing.T) {
	// The feed is shared: another consumer's reviews are none of kriya's
	// business, and erroring on them would stall the cursor forever.
	feed := &verdictFeed{
		pages: map[string][]orchestrator.VerdictEvent{"": {{
			ID: "event-9", Session: "somebody-elses",
			Verdict: orchestrator.VerdictChangesRequested,
		}}},
		next: map[string]string{"": "cursor-1"},
	}
	cursors := newMemCursors()
	got, err := router(feed, cursors, &sessionRoutes{}, newMemStore()).Consume(context.Background())
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("routed %+v", got)
	}
	if cursors.at["verdicts"] != "cursor-1" {
		t.Error("the cursor stalled on a verdict that was not kriya's")
	}
}

func TestTheCursorResumesWhereItLeftOff(t *testing.T) {
	feed := &verdictFeed{
		pages: map[string][]orchestrator.VerdictEvent{
			"":         {{ID: "e-1", Session: "sess-42", Verdict: orchestrator.VerdictApproved}},
			"cursor-1": {{ID: "e-2", Session: "sess-42", Verdict: orchestrator.VerdictApproved}},
		},
		next: map[string]string{"": "cursor-1", "cursor-1": "cursor-2"},
	}
	cursors := newMemCursors()
	routes := &sessionRoutes{bySession: map[string]orchestrator.BuildRun{
		"sess-42": submittedFor("sess-42"),
	}}
	r := router(feed, cursors, routes, newMemStore())
	if _, err := r.Consume(context.Background()); err != nil {
		t.Fatalf("consume: %v", err)
	}
	if _, err := r.Consume(context.Background()); err != nil {
		t.Fatalf("consume again: %v", err)
	}
	if len(feed.asked) != 2 || feed.asked[1] != "cursor-1" {
		t.Errorf("the feed was asked from %v", feed.asked)
	}
	if cursors.at["verdicts"] != "cursor-2" {
		t.Errorf("the cursor is at %q", cursors.at["verdicts"])
	}
}

func TestTheCursorAdvancesOnlyAfterEveryVerdictIsRouted(t *testing.T) {
	// A cursor advanced first would lose the verdicts that had not been acted
	// on, and a verdict nobody acted on is a review waiting forever.
	feed := &verdictFeed{
		pages: map[string][]orchestrator.VerdictEvent{"": {
			{ID: "e-1", Session: "sess-42", Verdict: orchestrator.VerdictChangesRequested},
			{ID: "e-2", Session: "sess-42", Verdict: orchestrator.VerdictChangesRequested},
		}},
		next: map[string]string{"": "cursor-1"},
	}
	cursors := newMemCursors()
	routes := &sessionRoutes{bySession: map[string]orchestrator.BuildRun{
		"sess-42": submittedFor("sess-42"),
	}}
	store := &failingRuns{after: 1}
	r := orchestrator.Router{
		Feed: feed, Cursors: cursors, Routes: routes, Store: store, Name: "verdicts",
	}
	if _, err := r.Consume(context.Background()); err == nil {
		t.Fatal("a verdict that could not be recorded read as routed")
	}
	if cursors.at["verdicts"] != "" {
		t.Errorf("the cursor advanced to %q past an unrouted verdict", cursors.at["verdicts"])
	}
}

func TestAnUnreadableFeedStopsTheConsumer(t *testing.T) {
	feed := &verdictFeed{err: errors.New("tracker unavailable")}
	cursors := newMemCursors()
	if _, err := router(feed, cursors, &sessionRoutes{}, newMemStore()).
		Consume(context.Background()); err == nil {
		t.Fatal("an unreachable tracker read as an empty feed")
	}
	if cursors.at["verdicts"] != "" {
		t.Error("the cursor advanced past a feed nobody read")
	}
}

func TestAnUnreadableRouteStopsTheConsumer(t *testing.T) {
	// "I could not find the run" is not "there is no run", and skipping would
	// drop a verdict permanently once the cursor moved.
	feed := &verdictFeed{
		pages: map[string][]orchestrator.VerdictEvent{"": {{ID: "e-1", Session: "sess-42"}}},
		next:  map[string]string{"": "cursor-1"},
	}
	routes := &sessionRoutes{err: errors.New("store unavailable")}
	cursors := newMemCursors()
	if _, err := router(feed, cursors, routes, newMemStore()).
		Consume(context.Background()); err == nil {
		t.Fatal("an unreadable route read as an unknown session")
	}
	if cursors.at["verdicts"] != "" {
		t.Error("the cursor advanced past a verdict that was never routed")
	}
}

func TestAnUnadvanceableCursorIsAFailure(t *testing.T) {
	// A cursor that could not advance means the next pass re-reads the same
	// verdicts, which is survivable — but reporting success would hide that
	// the position was never saved.
	feed := &verdictFeed{
		pages: map[string][]orchestrator.VerdictEvent{"": {}},
		next:  map[string]string{"": "cursor-1"},
	}
	cursors := newMemCursors()
	cursors.err = errors.New("disk full")
	if _, err := router(feed, cursors, &sessionRoutes{}, newMemStore()).
		Consume(context.Background()); err == nil {
		t.Fatal("a cursor that was never saved read as advanced")
	}
}

func TestAnEmptyFeedIsNotAnError(t *testing.T) {
	feed := &verdictFeed{pages: map[string][]orchestrator.VerdictEvent{}}
	got, err := router(feed, newMemCursors(), &sessionRoutes{}, newMemStore()).
		Consume(context.Background())
	if err != nil {
		t.Fatalf("an empty feed was treated as a failure: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("routed %+v from an empty feed", got)
	}
}

func TestAVerdictPayloadIsParsed(t *testing.T) {
	got, err := orchestrator.ParseVerdict("event-9", json.RawMessage(
		`{"review":"review-1","session":"sess-42","verdict":"changes-requested","revision":2}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := orchestrator.VerdictEvent{
		ID: "event-9", Review: "review-1", Session: "sess-42",
		Verdict: orchestrator.VerdictChangesRequested, Revision: 2,
	}
	if got != want {
		t.Errorf("parsed %+v", got)
	}
}

func TestAnUnreadablePayloadIsAnError(t *testing.T) {
	if _, err := orchestrator.ParseVerdict("event-9", json.RawMessage("not json")); err == nil {
		t.Fatal("an unparseable payload read as a verdict")
	}
}
