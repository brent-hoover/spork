package orchestrator_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"kriya/internal/orchestrator"
)

// workStack hands out tickets in order, then nothing.
//
// It behaves as sutra does on the point these tests turn on: a replayed key
// returns the SAME claim rather than taking a second ticket.
type workStack struct {
	tickets []string
	handed  int
	byKey   map[string]string
	keys    []string
	err     error
}

func newWorkStack(tickets ...string) *workStack {
	return &workStack{tickets: tickets, byKey: map[string]string{}}
}

func (s *workStack) Pop(_ context.Context, key string) (string, string, error) {
	s.keys = append(s.keys, key)
	if s.err != nil {
		return "", "", s.err
	}
	if claimed, ok := s.byKey[key]; ok {
		return claimed, "T-" + claimed, nil
	}
	if s.handed >= len(s.tickets) {
		// Nothing workable. Not an error.
		return "", "", nil
	}
	id := s.tickets[s.handed]
	s.handed++
	s.byKey[key] = id
	return id, "T-" + id, nil
}

// builds records what the loop was asked to build.
type builds struct {
	seen   []string
	failAt int
	park   bool
}

func (b *builds) build(_ context.Context, issue, title string) (orchestrator.BuildRun, error) {
	b.seen = append(b.seen, issue)
	run := orchestrator.BuildRun{ID: "run-" + issue, Ticket: title, State: orchestrator.StateClosed}
	if b.failAt > 0 && len(b.seen) == b.failAt {
		if b.park {
			run.State = orchestrator.StateAwaitingOperator
			run.Error = "the worktree vanished"
		}
		return run, errors.New("build failed")
	}
	return run, nil
}

// memOrdinals is the settled-pop counter.
type memOrdinals struct {
	settled map[string]int
	err     error
}

func newMemOrdinals() *memOrdinals { return &memOrdinals{settled: map[string]int{}} }

func (m *memOrdinals) Current(_ context.Context, target string) (int, error) {
	return m.settled[target], m.err
}

func (m *memOrdinals) Advance(_ context.Context, target string, to int) error {
	if m.err != nil {
		return m.err
	}
	m.settled[target] = to
	return nil
}

func popLoop(stack *workStack, b *builds) orchestrator.Loop {
	return orchestrator.Loop{
		Pops: stack, Build: b.build, Ordinals: newMemOrdinals(),
		TargetKey: "/target", MaxTickets: 8,
	}
}

func TestTheLoopKeepsPoppingAndBuilding(t *testing.T) {
	stack, b := newWorkStack("i-1", "i-2", "i-3"), &builds{}
	got, err := popLoop(stack, b).Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(b.seen) != 3 {
		t.Errorf("built %v", b.seen)
	}
	if len(got.Built) != 3 {
		t.Errorf("reported %d runs", len(got.Built))
	}
}

func TestNothingWorkableIsIdlingNotExiting(t *testing.T) {
	// The plan is not finished — its remaining tickets are blocked or in
	// flight — so an empty pop must not read as an error or as completion.
	stack, b := newWorkStack(), &builds{}
	got, err := popLoop(stack, b).Run(context.Background())
	if err != nil {
		t.Fatalf("an empty work stack was treated as a failure: %v", err)
	}
	if !got.Idle {
		t.Error("the loop did not report idling")
	}
	if len(b.seen) != 0 {
		t.Errorf("built %v with nothing workable", b.seen)
	}
}

func TestPoppingResumesWhenATicketUnblocks(t *testing.T) {
	stack, b := newWorkStack(), &builds{}
	loop := popLoop(stack, b)
	first, err := loop.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !first.Idle {
		t.Fatal("the loop did not idle on an empty stack")
	}
	// A ticket unblocks.
	stack.tickets = append(stack.tickets, "i-9")
	second, err := loop.Run(context.Background())
	if err != nil {
		t.Fatalf("run again: %v", err)
	}
	if len(second.Built) != 1 || b.seen[0] != "i-9" {
		t.Errorf("built %v after the unblock", b.seen)
	}
}

func TestEachPopCarriesItsOwnKey(t *testing.T) {
	// A crash between the pop and the build leaves a ticket claimed. A replay
	// under the SAME key returns that claim rather than taking a second ticket
	// and abandoning the first.
	stack, b := newWorkStack("i-1", "i-2"), &builds{}
	if _, err := popLoop(stack, b).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	seen := map[string]bool{}
	for _, key := range stack.keys {
		if key == "" {
			t.Fatal("a pop carried no idempotency key")
		}
		if seen[key] {
			t.Errorf("key %q was used twice in one pass", key)
		}
		seen[key] = true
	}
}

func TestACrashBeforeTheBuildSettlesReplaysTheSameClaim(t *testing.T) {
	// The ordinal does not advance for a build that did not settle, so the
	// next pass presents the same key and gets the same ticket back rather
	// than taking a second one and abandoning the first.
	stack := newWorkStack("i-1", "i-2")
	ordinals := newMemOrdinals()
	first := &builds{failAt: 1}
	loop := orchestrator.Loop{
		Pops: stack, Build: first.build, Ordinals: ordinals,
		TargetKey: "/target", MaxTickets: 8,
	}
	if _, err := loop.Run(context.Background()); err == nil {
		t.Fatal("a failed build read as success")
	}
	if ordinals.settled["/target"] != 0 {
		t.Errorf("the ordinal advanced to %d for a build that never settled",
			ordinals.settled["/target"])
	}

	second := &builds{}
	loop.Build = second.build
	if _, err := loop.Run(context.Background()); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(second.seen) == 0 || second.seen[0] != "i-1" {
		t.Errorf("the replay claimed %v, want i-1 again", second.seen)
	}
	if stack.handed != 2 {
		t.Errorf("the tracker handed out %d distinct tickets", stack.handed)
	}
}

func TestASettledPassDoesNotReplayItself(t *testing.T) {
	// The ordinal advances on every settled build, so a later pass asks for
	// NEW work rather than re-claiming what it already finished.
	stack := newWorkStack("i-1", "i-2")
	ordinals := newMemOrdinals()
	first := &builds{}
	loop := orchestrator.Loop{
		Pops: stack, Build: first.build, Ordinals: ordinals,
		TargetKey: "/target", MaxTickets: 8,
	}
	if _, err := loop.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	// Two builds and the empty pop that ended the pass.
	if ordinals.settled["/target"] != 3 {
		t.Errorf("the ordinal is %d after two settled builds and an empty pop",
			ordinals.settled["/target"])
	}

	second := &builds{}
	loop.Build = second.build
	got, err := loop.Run(context.Background())
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(second.seen) != 0 {
		t.Errorf("the second pass rebuilt %v", second.seen)
	}
	if !got.Idle {
		t.Error("the second pass did not idle")
	}
}

func TestAnUnreadableOrdinalStopsThePass(t *testing.T) {
	// Popping under an unknown ordinal could replay a finished claim or take a
	// ticket nothing will come back for.
	stack, b := newWorkStack("i-1"), &builds{}
	ordinals := newMemOrdinals()
	ordinals.err = errors.New("store unavailable")
	loop := orchestrator.Loop{
		Pops: stack, Build: b.build, Ordinals: ordinals,
		TargetKey: "/target", MaxTickets: 8,
	}
	if _, err := loop.Run(context.Background()); err == nil {
		t.Fatal("an unreadable ordinal read as zero")
	}
	if len(stack.keys) != 0 {
		t.Error("a pop was made under an unknown ordinal")
	}
}

func TestAParkedRunStopsThePass(t *testing.T) {
	// A run parked for the operator is a signal, and popping past it would
	// bury it under more work.
	stack, b := newWorkStack("i-1", "i-2", "i-3"), &builds{failAt: 2, park: true}
	got, err := popLoop(stack, b).Run(context.Background())
	if err == nil {
		t.Fatal("a failed build read as success")
	}
	if len(b.seen) != 2 {
		t.Errorf("built %v after a parked run", b.seen)
	}
	// The parked run is still reported: the operator needs to see it.
	if len(got.Built) != 2 {
		t.Errorf("reported %d runs", len(got.Built))
	}
	if got.Built[1].State != orchestrator.StateAwaitingOperator {
		t.Errorf("the parked run is in %q", got.Built[1].State)
	}
}

func TestAPopFailureStopsThePass(t *testing.T) {
	// "I could not ask for work" is not "there is no work".
	stack, b := newWorkStack("i-1"), &builds{}
	stack.err = errors.New("tracker unavailable")
	got, err := popLoop(stack, b).Run(context.Background())
	if err == nil {
		t.Fatal("an unreachable tracker read as an empty work stack")
	}
	if got.Idle {
		t.Error("a failed pop reported as idling")
	}
}

func TestAPassIsBounded(t *testing.T) {
	// Reaching the bound is not completion — it is this pass ending — and the
	// caller comes back.
	stack := newWorkStack("a", "b", "c", "d", "e")
	b := &builds{}
	loop := popLoop(stack, b)
	loop.MaxTickets = 2
	got, err := loop.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(b.seen) != 2 {
		t.Errorf("built %v under a bound of 2", b.seen)
	}
	if got.Idle {
		t.Error("a bounded pass reported as idling, which would say the plan is waiting")
	}
}

func TestAnUnsetBoundStillBounds(t *testing.T) {
	// A loop with no bound would pop forever against a tracker that keeps
	// finding work, and never return to whatever drives it.
	stack := newWorkStack()
	for i := range 100 {
		stack.tickets = append(stack.tickets, fmt.Sprintf("i-%d", i))
	}
	b := &builds{}
	loop := orchestrator.Loop{
		Pops: stack, Build: b.build, Ordinals: newMemOrdinals(), TargetKey: "/target",
	}
	if _, err := loop.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(b.seen) == 0 || len(b.seen) >= 100 {
		t.Errorf("built %d tickets with no bound set", len(b.seen))
	}
}

func TestAnEmptyPopStillAdvancesTheOrdinal(t *testing.T) {
	// The tracker settles an empty pop under its key like any other response,
	// so a stalled ordinal replays "nothing workable" forever — even after a
	// ticket unblocks. Nothing was claimed, so nothing is lost by moving past.
	stack, b := newWorkStack(), &builds{}
	ordinals := newMemOrdinals()
	loop := orchestrator.Loop{
		Pops: stack, Build: b.build, Ordinals: ordinals,
		TargetKey: "/target", MaxTickets: 8,
	}
	got, err := loop.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !got.Idle {
		t.Fatal("the loop did not idle")
	}
	if ordinals.settled["/target"] != 1 {
		t.Errorf("the ordinal is %d after an empty pop", ordinals.settled["/target"])
	}

	// A ticket unblocks. The next pass must present a NEW key, or the tracker
	// hands back the settled empty response again.
	stack.tickets = append(stack.tickets, "i-9")
	before := len(stack.keys)
	if _, err := loop.Run(context.Background()); err != nil {
		t.Fatalf("run again: %v", err)
	}
	if stack.keys[before] == stack.keys[before-1] {
		t.Error("the resumed pass reused the empty pop's key")
	}
	if len(b.seen) != 1 || b.seen[0] != "i-9" {
		t.Errorf("built %v after the unblock", b.seen)
	}
}

// stubDetector answers the completion question the idle path asks.
type stubDetector struct {
	armed  bool
	reason string
	err    error
	asked  int
}

func (d *stubDetector) Detect(context.Context, string) (bool, string, error) {
	d.asked++
	return d.armed, d.reason, d.err
}

// stubStalls records what the idle path decided to report.
type stubStalls struct {
	causes []string
	err    error
}

func (s *stubStalls) Record(_ context.Context, _ string, _ int, cause string) error {
	if s.err != nil {
		return s.err
	}
	s.causes = append(s.causes, cause)
	return nil
}

func idleLoop(det *stubDetector, st *stubStalls) orchestrator.Loop {
	return orchestrator.Loop{
		Pops:      newWorkStack(),
		Build:     func(context.Context, string, string) (orchestrator.BuildRun, error) { panic("no build") },
		Ordinals:  newMemOrdinals(),
		TargetKey: "/spec",
		Finish:    det,
		Stalls:    st,
	}
}

func TestAnIdleLoopWithCompletionArmedRecordsNoStall(t *testing.T) {
	// Armed means the ticket set is whole and nothing is outstanding. The
	// build is finishing, not stuck, and a stall row would put a healthy
	// build in the operator's inbox.
	det, st := &stubDetector{armed: true}, &stubStalls{}
	got, err := idleLoop(det, st).Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !got.Idle {
		t.Error("an empty pop did not idle")
	}
	if len(st.causes) != 0 {
		t.Errorf("recorded a stall for a build that can complete: %v", st.causes)
	}
}

func TestAnIdleLoopThatCannotCompleteRecordsTheStallWithItsCause(t *testing.T) {
	// Nothing workable, nothing in flight, and the epic cannot close. That is
	// the stall condition exactly, and it is durable rather than a message
	// somebody has to be watching for.
	det := &stubDetector{reason: "outstanding work: issue-9 (unplanned, blocked)"}
	st := &stubStalls{}
	if _, err := idleLoop(det, st).Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if det.asked != 1 {
		t.Errorf("completion was asked %d times", det.asked)
	}
	if len(st.causes) != 1 {
		t.Fatalf("recorded %d stalls", len(st.causes))
	}
	if !strings.Contains(st.causes[0], "issue-9") {
		t.Errorf("the recorded cause is %q — it does not say what is holding the build", st.causes[0])
	}
}

func TestAPoppingLoopAsksNothingAboutCompletion(t *testing.T) {
	// The question is only interesting when there is nothing to do. Asking on
	// every pop would put a tracker round-trip in the hot path of a build
	// that is plainly still working.
	det, st := &stubDetector{}, &stubStalls{}
	l := idleLoop(det, st)
	l.Pops = newWorkStack("issue-1")
	l.Build = func(context.Context, string, string) (orchestrator.BuildRun, error) {
		return orchestrator.BuildRun{ID: "run-1"}, nil
	}
	if _, err := l.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if det.asked != 1 {
		t.Errorf("completion was asked %d times; want once, at the idle", det.asked)
	}
}

func TestAnUnreadableCompletionStopsTheLoop(t *testing.T) {
	// "I could not tell whether the build is done" is not "it is fine". A
	// loop that shrugged would idle forever with nothing recorded.
	det := &stubDetector{err: errors.New("tracker unavailable")}
	if _, err := idleLoop(det, &stubStalls{}).Run(context.Background()); err == nil {
		t.Fatal("an unreadable completion state read as an armed one")
	}
}

func TestAStallThatCannotBeRecordedStopsTheLoop(t *testing.T) {
	// A stall nobody wrote is a build that looks like it is still working.
	det := &stubDetector{reason: "nothing workable"}
	st := &stubStalls{err: errors.New("disk full")}
	if _, err := idleLoop(det, st).Run(context.Background()); err == nil {
		t.Fatal("a stall that was never written read as recorded")
	}
}

func TestALoopWithNoCompletionSeamStillIdles(t *testing.T) {
	// Nil asks nothing and records nothing — which is what a module-level
	// test of the pop loop itself wants.
	l := orchestrator.Loop{
		Pops:      newWorkStack(),
		Build:     func(context.Context, string, string) (orchestrator.BuildRun, error) { panic("no build") },
		Ordinals:  newMemOrdinals(),
		TargetKey: "/spec",
	}
	got, err := l.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !got.Idle {
		t.Error("the loop did not idle")
	}
}
