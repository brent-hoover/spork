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

// sessionRoutes finds a run by the session its review was stamped with, or by
// the review itself for a verdict that carries no session.
type sessionRoutes struct {
	bySession map[string]orchestrator.BuildRun
	byReview  map[string]orchestrator.BuildRun
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

func (s *sessionRoutes) ByReview(
	_ context.Context, review string,
) (orchestrator.BuildRun, bool, error) {
	if s.err != nil {
		return orchestrator.BuildRun{}, false, s.err
	}
	run, ok := s.byReview[review]
	return run, ok, nil
}

// reviewRevisions answers what revision a review is at. The verdict event
// carries none, so the review is asked.
type reviewRevisions struct {
	at    int
	err   error
	asked []string
}

func (r *reviewRevisions) Revision(_ context.Context, review string) (int, error) {
	r.asked = append(r.asked, review)
	return r.at, r.err
}

func router(feed *verdictFeed, cursors *memCursors, routes *sessionRoutes, store *memStore) orchestrator.Router {
	return orchestrator.Router{
		Feed: feed, Cursors: cursors, Routes: routes,
		Reviews: &reviewRevisions{at: 2}, Store: store, Name: "verdicts",
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
			Verdict: orchestrator.VerdictChangesRequested,
		}}},
		next: map[string]string{"": "cursor-1"},
	}
	store := newMemStore()
	routes := &sessionRoutes{bySession: map[string]orchestrator.BuildRun{
		"sess-42": submittedFor("sess-42"),
	}}
	got, _, err := router(feed, newMemCursors(), routes, store).Consume(context.Background())
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
			Verdict: orchestrator.VerdictApproved,
		}}},
		next: map[string]string{"": "cursor-1"},
	}
	store := newMemStore()
	routes := &sessionRoutes{bySession: map[string]orchestrator.BuildRun{
		"sess-42": submittedFor("sess-42"),
	}}
	got, _, err := router(feed, newMemCursors(), routes, store).Consume(context.Background())
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
	r := router(feed, cursors, &sessionRoutes{}, newMemStore())
	got, next, err := r.Consume(context.Background())
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("routed %+v", got)
	}
	if err := r.Advance(context.Background(), next); err != nil {
		t.Fatalf("advance: %v", err)
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
	for i := range 2 {
		_, next, err := r.Consume(context.Background())
		if err != nil {
			t.Fatalf("consume %d: %v", i, err)
		}
		if err := r.Advance(context.Background(), next); err != nil {
			t.Fatalf("advance %d: %v", i, err)
		}
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
		Feed: feed, Cursors: cursors, Routes: routes,
		Reviews: &reviewRevisions{at: 2}, Store: store, Name: "verdicts",
	}
	if _, _, err := r.Consume(context.Background()); err == nil {
		t.Fatal("a verdict that could not be recorded read as routed")
	}
	if cursors.at["verdicts"] != "" {
		t.Errorf("the cursor advanced to %q past an unrouted verdict", cursors.at["verdicts"])
	}
}

func TestAnUnreadableFeedStopsTheConsumer(t *testing.T) {
	feed := &verdictFeed{err: errors.New("tracker unavailable")}
	cursors := newMemCursors()
	if _, _, err := router(feed, cursors, &sessionRoutes{}, newMemStore()).
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
	if _, _, err := router(feed, cursors, routes, newMemStore()).
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
	cursors := newMemCursors()
	cursors.err = errors.New("disk full")
	r := router(&verdictFeed{}, cursors, &sessionRoutes{}, newMemStore())
	if err := r.Advance(context.Background(), "cursor-1"); err == nil {
		t.Fatal("a cursor that was never saved read as advanced")
	}
	// Advancing nothing writes nothing, so it cannot fail.
	if err := r.Advance(context.Background(), ""); err != nil {
		t.Errorf("advancing an empty cursor failed: %v", err)
	}
}

func TestAnEmptyFeedIsNotAnError(t *testing.T) {
	feed := &verdictFeed{pages: map[string][]orchestrator.VerdictEvent{}}
	got, _, err := router(feed, newMemCursors(), &sessionRoutes{}, newMemStore()).
		Consume(context.Background())
	if err != nil {
		t.Fatalf("an empty feed was treated as a failure: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("routed %+v from an empty feed", got)
	}
}

func TestAVerdictPayloadIsParsed(t *testing.T) {
	// The KIND is the verdict. The payload carries the review and the session
	// and neither the verdict nor a revision, so reading either from it would
	// find nothing.
	got, err := orchestrator.ParseVerdict("event-9", orchestrator.KindChangesRequested,
		json.RawMessage(`{"review":"review-1","issue":"i-1","session":"sess-42"}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := orchestrator.VerdictEvent{
		ID: "event-9", Review: "review-1", Session: "sess-42",
		Verdict: orchestrator.VerdictChangesRequested,
	}
	if got != want {
		t.Errorf("parsed %+v", got)
	}
	approved, err := orchestrator.ParseVerdict("event-10", orchestrator.KindApproved,
		json.RawMessage(`{"review":"review-1"}`))
	if err != nil {
		t.Fatalf("parse approved: %v", err)
	}
	if approved.Verdict != orchestrator.VerdictApproved {
		t.Errorf("an approval parsed as %q", approved.Verdict)
	}
}

func TestAnEventThatIsNotAVerdictIsRefused(t *testing.T) {
	if _, err := orchestrator.ParseVerdict("e-1", "review.created",
		json.RawMessage(`{"review":"review-1"}`)); err == nil {
		t.Fatal("a non-verdict event parsed as a verdict")
	}
}

func TestTheRevisionComesFromTheReview(t *testing.T) {
	// The event carries none. A resubmission fenced on a guessed revision
	// would be refused, or worse advance one the human never saw.
	feed := &verdictFeed{
		pages: map[string][]orchestrator.VerdictEvent{"": {{
			ID: "event-9", Review: "review-1", Session: "sess-42",
			Verdict: orchestrator.VerdictChangesRequested,
		}}},
		next: map[string]string{"": "cursor-1"},
	}
	store := newMemStore()
	routes := &sessionRoutes{bySession: map[string]orchestrator.BuildRun{
		"sess-42": submittedFor("sess-42"),
	}}
	revisions := &reviewRevisions{at: 4}
	r := orchestrator.Router{
		Feed: feed, Cursors: newMemCursors(), Routes: routes,
		Reviews: revisions, Store: store, Name: "verdicts",
	}
	if _, _, err := r.Consume(context.Background()); err != nil {
		t.Fatalf("consume: %v", err)
	}
	if len(revisions.asked) != 1 || revisions.asked[0] != "review-1" {
		t.Errorf("asked %v", revisions.asked)
	}
	if store.rows["run-sess-42"].ReviewRevision != 4 {
		t.Errorf("recorded revision %d", store.rows["run-sess-42"].ReviewRevision)
	}
}

func TestAnUnreadableRevisionStopsTheConsumer(t *testing.T) {
	feed := &verdictFeed{
		pages: map[string][]orchestrator.VerdictEvent{"": {{
			ID: "event-9", Review: "review-1", Session: "sess-42",
			Verdict: orchestrator.VerdictChangesRequested,
		}}},
		next: map[string]string{"": "cursor-1"},
	}
	routes := &sessionRoutes{bySession: map[string]orchestrator.BuildRun{
		"sess-42": submittedFor("sess-42"),
	}}
	cursors := newMemCursors()
	r := orchestrator.Router{
		Feed: feed, Cursors: cursors, Routes: routes,
		Reviews: &reviewRevisions{err: errors.New("tracker unavailable")},
		Store:   newMemStore(), Name: "verdicts",
	}
	if _, _, err := r.Consume(context.Background()); err == nil {
		t.Fatal("an unreadable revision read as zero")
	}
	if cursors.at["verdicts"] != "" {
		t.Error("the cursor advanced past a verdict that was never routed")
	}
}

func TestAnUnreadablePayloadIsAnError(t *testing.T) {
	if _, err := orchestrator.ParseVerdict("event-9", orchestrator.KindApproved, json.RawMessage("not json")); err == nil {
		t.Fatal("an unparseable payload read as a verdict")
	}
}

func TestAnApprovalCarriesTheRevisionItApproved(t *testing.T) {
	// The consume fence names the revision the approval is against. An initial
	// submission never recorded one, so the run's field was zero and the
	// tracker refused the consumption instead of merging.
	feed := &verdictFeed{
		pages: map[string][]orchestrator.VerdictEvent{"": {{
			ID: "event-9", Review: "review-1", Session: "sess-1",
			Verdict: orchestrator.VerdictApproved,
		}}},
		next: map[string]string{"": "c-1"},
	}
	run := submittedFor("sess-1")
	run.ReviewRevision = 0
	routes := &sessionRoutes{bySession: map[string]orchestrator.BuildRun{"sess-1": run}}

	routed, _, err := router(feed, newMemCursors(), routes, &memStore{}).
		Consume(context.Background())
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if len(routed) != 1 {
		t.Fatalf("routed %d verdicts", len(routed))
	}
	// The router's stub review is at revision 2.
	if routed[0].Revision != 2 {
		t.Errorf("the approval carries revision %d, want the review's own", routed[0].Revision)
	}
	if routed[0].Reworked {
		t.Error("an approval was routed as rework")
	}
}

func TestTheCursorDoesNotAdvanceUntilTheCallerHasActed(t *testing.T) {
	// Consume routes; the caller enqueues merges and drives runs. A cursor
	// advanced inside Consume is advanced before any of that, so an enqueue
	// or drive failure loses the events permanently — the feed never offers
	// them again and the reviews wait forever.
	feed := &verdictFeed{
		pages: map[string][]orchestrator.VerdictEvent{"": {{
			ID: "event-9", Review: "review-1", Session: "sess-1",
			Verdict: orchestrator.VerdictApproved,
		}}},
		next: map[string]string{"": "c-1"},
	}
	cursors := newMemCursors()
	routes := &sessionRoutes{
		bySession: map[string]orchestrator.BuildRun{"sess-1": submittedFor("sess-1")},
	}
	r := router(feed, cursors, routes, &memStore{})

	routed, next, err := r.Consume(context.Background())
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if len(routed) != 1 {
		t.Fatalf("routed %d", len(routed))
	}
	if cursors.at["verdicts"] != "" {
		t.Errorf("the cursor advanced to %q before the caller acted", cursors.at["verdicts"])
	}
	if next != "c-1" {
		t.Fatalf("Consume reported next cursor %q", next)
	}

	// The caller acted; now it advances.
	if err := r.Advance(context.Background(), next); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if cursors.at["verdicts"] != "c-1" {
		t.Errorf("the cursor is at %q after the caller acted", cursors.at["verdicts"])
	}
}
