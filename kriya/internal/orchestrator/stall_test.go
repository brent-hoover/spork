package orchestrator_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"kriya/internal/clock"
	"kriya/internal/fakes"
	"kriya/internal/orchestrator"
)

// memStalls is an in-memory StallStore.
type memStalls struct {
	rows map[string]orchestrator.Stall
	err  error
}

func newMemStalls() *memStalls { return &memStalls{rows: map[string]orchestrator.Stall{}} }

func (m *memStalls) Insert(_ context.Context, s orchestrator.Stall) error {
	if m.err != nil {
		return m.err
	}
	existing, ok := m.rows[s.Key]
	if !ok {
		m.rows[s.Key] = s
		return nil
	}
	// A condition that came back is the SAME condition: the row reopens
	// rather than a second one appearing, and its cause stands.
	existing.State = orchestrator.StallOpen
	existing.Resolved = time.Time{}
	m.rows[s.Key] = existing
	return nil
}

func (m *memStalls) Resolve(_ context.Context, key string, at time.Time) error {
	if m.err != nil {
		return m.err
	}
	s, ok := m.rows[key]
	if !ok {
		return nil
	}
	s.State, s.Resolved = orchestrator.StallResolved, at
	m.rows[key] = s
	return nil
}

func (m *memStalls) Open(_ context.Context) ([]orchestrator.Stall, error) {
	if m.err != nil {
		return nil, m.err
	}
	var out []orchestrator.Stall
	for _, s := range m.rows {
		if s.State == orchestrator.StallOpen {
			out = append(out, s)
		}
	}
	return out, nil
}

func stalls(store *memStalls) orchestrator.Stalls {
	return orchestrator.Stalls{Store: store, Now: fakes.NewClock(time.Unix(0, 0))}
}

func TestAStallIsRecordedWithItsCause(t *testing.T) {
	// "kriya neither spins nor declares the build done": the condition is
	// durable, and the cause is what makes it actionable rather than a
	// mystery the operator has to reconstruct.
	store := newMemStalls()
	got, err := stalls(store).Record(context.Background(), "/spec", 3,
		"outstanding work: issue-9 (unplanned, blocked)")
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if got.Cause == "" {
		t.Error("a stall was recorded with no cause")
	}
	if got.State != orchestrator.StallOpen {
		t.Errorf("the stall is in state %q", got.State)
	}
	open, err := store.Open(context.Background())
	if err != nil || len(open) != 1 {
		t.Fatalf("the inbox lists %d stalls: %v", len(open), err)
	}
}

func TestRedetectingTheSameStallConvergesOnOneRow(t *testing.T) {
	// The key is deterministic from (target, epoch), so polls, concurrent
	// detectors and restart recovery all land on one row rather than filling
	// the inbox with the same condition.
	store := newMemStalls()
	s := stalls(store)
	for range 5 {
		if _, err := s.Record(context.Background(), "/spec", 3, "nothing workable"); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	if len(store.rows) != 1 {
		t.Errorf("recorded %d rows for one condition", len(store.rows))
	}
}

func TestALaterEpochsStallIsANewRow(t *testing.T) {
	// A stall is about a moment in the target's life. After an epoch advance
	// — a supersession, a reopen — the same-looking condition is a different
	// one, and reusing the row would hide it behind a resolved stamp.
	store := newMemStalls()
	s := stalls(store)
	if _, err := s.Record(context.Background(), "/spec", 3, "nothing workable"); err != nil {
		t.Fatalf("record: %v", err)
	}
	if _, err := s.Record(context.Background(), "/spec", 4, "nothing workable"); err != nil {
		t.Fatalf("record: %v", err)
	}
	if len(store.rows) != 2 {
		t.Errorf("two epochs produced %d rows", len(store.rows))
	}
}

func TestADifferentTargetsStallIsANewRow(t *testing.T) {
	store := newMemStalls()
	s := stalls(store)
	if _, err := s.Record(context.Background(), "/one", 1, "nothing workable"); err != nil {
		t.Fatalf("record: %v", err)
	}
	if _, err := s.Record(context.Background(), "/two", 1, "nothing workable"); err != nil {
		t.Fatalf("record: %v", err)
	}
	if len(store.rows) != 2 {
		t.Errorf("two targets produced %d rows", len(store.rows))
	}
}

func TestResolutionStampsTheRowRatherThanDeletingIt(t *testing.T) {
	// The history is the point: an operator asking "has this happened before"
	// gets an answer, and a deleted row cannot give one.
	store := newMemStalls()
	s := stalls(store)
	recorded, err := s.Record(context.Background(), "/spec", 3, "nothing workable")
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := s.Resolve(context.Background(), recorded.Key); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	got := store.rows[recorded.Key]
	if got.State != orchestrator.StallResolved {
		t.Errorf("the row is in state %q after resolution", got.State)
	}
	if got.Resolved.IsZero() {
		t.Error("the row records no resolution time")
	}
	if got.TargetKey != "/spec" {
		t.Errorf("resolution blanked the target: %q", got.TargetKey)
	}
	if got.Cause != recorded.Cause {
		t.Error("resolution rewrote the cause")
	}
	open, _ := store.Open(context.Background())
	if len(open) != 0 {
		t.Errorf("the inbox still lists %d stalls", len(open))
	}
}

func TestAResolvedStallReopensWhenTheConditionReturns(t *testing.T) {
	// The same condition at the same epoch is the SAME stall. A second row
	// would tell the operator it is new, and leaving it resolved would hide
	// that the build is stuck again.
	store := newMemStalls()
	s := stalls(store)
	recorded, err := s.Record(context.Background(), "/spec", 3, "nothing workable")
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := s.Resolve(context.Background(), recorded.Key); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := s.Record(context.Background(), "/spec", 3, "nothing workable"); err != nil {
		t.Fatalf("re-record: %v", err)
	}
	if len(store.rows) != 1 {
		t.Errorf("the returning condition produced %d rows", len(store.rows))
	}
	open, _ := store.Open(context.Background())
	if len(open) != 1 {
		t.Error("the returning condition is not in the inbox")
	}
}

func TestAnUnwritableStallIsAFailure(t *testing.T) {
	// A stall nobody recorded is a build that looks like it is still working.
	store := newMemStalls()
	store.err = errors.New("disk full")
	if _, err := stalls(store).Record(context.Background(), "/spec", 3, "nothing"); err == nil {
		t.Fatal("a stall that was never written read as recorded")
	}
}

func TestAStallWithNoCauseIsRefused(t *testing.T) {
	// "a durable Stall row records the condition AND ITS CAUSE". A blank one
	// tells the operator only that something is wrong.
	if _, err := stalls(newMemStalls()).Record(context.Background(), "/spec", 3, ""); err == nil {
		t.Fatal("a stall with no cause was recorded")
	}
}

func TestTheSQLStallStoreRoundTrips(t *testing.T) {
	db := sqlOrchDB(t)
	store := orchestrator.SQLStalls{DB: db}
	s := orchestrator.Stalls{Store: store, Now: clock.System{}}
	recorded, err := s.Record(context.Background(), "/spec", 3, "nothing workable")
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	open, err := store.Open(context.Background())
	if err != nil || len(open) != 1 {
		t.Fatalf("listed %d open stalls: %v", len(open), err)
	}
	if open[0].Cause != "nothing workable" || open[0].TargetKey != "/spec" {
		t.Errorf("read back %+v", open[0])
	}
	// Re-detection converges rather than rewriting the operator's row.
	if _, err := s.Record(context.Background(), "/spec", 3, "something else"); err != nil {
		t.Fatalf("re-record: %v", err)
	}
	open, _ = store.Open(context.Background())
	if len(open) != 1 || open[0].Cause != "nothing workable" {
		t.Errorf("re-detection produced %+v", open)
	}
	if err := s.Resolve(context.Background(), recorded.Key); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if open, _ := store.Open(context.Background()); len(open) != 0 {
		t.Errorf("the inbox still lists %d", len(open))
	}
}
