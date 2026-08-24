package orchestrator_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"kriya/internal/orchestrator"
)

// ticketAPI behaves as sutra does: a keyed close lands once, and a replay
// under the same key returns the original success rather than a close-used
// conflict.
type ticketAPI struct {
	closed map[string]bool
	calls  []closeCall
	err    error
	// reversed makes the close conflict, as a reversed approval would.
	reversed bool
}

type closeCall struct {
	issue, review, event, key string
	revision                  int
}

func newTicketAPI() *ticketAPI { return &ticketAPI{closed: map[string]bool{}} }

func (t *ticketAPI) Complete(
	_ context.Context, issue, review string, revision int, event, key string,
) error {
	t.calls = append(t.calls, closeCall{
		issue: issue, review: review, event: event, key: key, revision: revision,
	})
	if t.err != nil {
		return t.err
	}
	if t.closed[key] {
		// Replayed key: the original success, never a close-used conflict.
		return nil
	}
	if t.reversed {
		return errors.New("close conflicted: the approval was reversed")
	}
	t.closed[key] = true
	return nil
}

// branchHead reports where a branch stands, and records the order of calls
// against the session ending.
type branchHead struct {
	head string
	err  error
	// terminatedFirst records whether the session had ended by the time the
	// head was read.
	terminatedFirst bool
	ender           *sessionEnd
}

func (b *branchHead) Head(context.Context, string) (string, error) {
	if b.ender != nil {
		b.terminatedFirst = b.ender.ended
	}
	if b.err != nil {
		return "", b.err
	}
	return b.head, nil
}

type sessionEnd struct {
	ended bool
	err   error
}

func (s *sessionEnd) Terminate(context.Context, string) error {
	if s.err != nil {
		return s.err
	}
	s.ended = true
	return nil
}

func completer(store *memStore, tickets *ticketAPI, head *branchHead, ender *sessionEnd) orchestrator.Completer {
	head.ender = ender
	return orchestrator.Completer{
		Store: store, Tickets: tickets, Branches: head, Sessions: ender, Actor: "actor-1",
	}
}

func mergedRun() orchestrator.BuildRun {
	return orchestrator.BuildRun{
		ID: "run-1", Ticket: "KRI-1", State: orchestrator.StateMerged,
		ReviewID: "review-1", ReviewRevision: 2, ReviewVerdictEvent: "event-9",
		ReviewCommit: "C2",
	}
}

func completion() orchestrator.Completion {
	return orchestrator.Completion{Issue: "issue-7", Branch: "kriya/KRI-1/abcd", Merged: "C2"}
}

func TestTheSessionEndsBeforeTheHeadIsRead(t *testing.T) {
	// The dev session is the branch's only in-protocol writer, so a check
	// racing it proves nothing about the moment after.
	store, tickets := newMemStore(), newTicketAPI()
	head, ender := &branchHead{head: "C2"}, &sessionEnd{}
	if _, err := completer(store, tickets, head, ender).
		Complete(context.Background(), mergedRun(), completion()); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if !head.terminatedFirst {
		t.Error("the head was read while the session could still write to the branch")
	}
}

func TestTheCloseNamesTheReviewRevisionAndEvent(t *testing.T) {
	// sutra stamps the review close-used against exactly these, so a stale
	// revision rejects and the merge-time consumption never blocks the close.
	store, tickets := newMemStore(), newTicketAPI()
	got, err := completer(store, tickets, &branchHead{head: "C2"}, &sessionEnd{}).
		Complete(context.Background(), mergedRun(), completion())
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if len(tickets.calls) != 1 {
		t.Fatalf("closed %d times", len(tickets.calls))
	}
	call := tickets.calls[0]
	if call.issue != "issue-7" || call.review != "review-1" ||
		call.revision != 2 || call.event != "event-9" {
		t.Errorf("closed with %+v", call)
	}
	if got.CompletionState != orchestrator.CompleteClosed {
		t.Errorf("the run is in %q", got.CompletionState)
	}
	if got.CompletedHead != "C2" {
		t.Errorf("recorded head %q", got.CompletedHead)
	}
}

func TestTheCloseKeyIsPersistedBeforeTheCall(t *testing.T) {
	store, tickets := newMemStore(), newTicketAPI()
	tickets.err = errors.New("sutra unreachable")
	_, err := completer(store, tickets, &branchHead{head: "C2"}, &sessionEnd{}).
		Complete(context.Background(), mergedRun(), completion())
	if err == nil {
		t.Fatal("a close that never landed read as success")
	}
	row := store.rows["run-1"]
	if row.CompletionState != orchestrator.CompleteCompleting || row.CloseKey == "" {
		t.Errorf("the crash window is invisible: %+v", row)
	}
}

func TestAReplayedCloseReturnsTheOriginalSuccess(t *testing.T) {
	// Never a close-used conflict: the key is what makes the replay the same
	// request rather than a second one.
	store, tickets := newMemStore(), newTicketAPI()
	c := completer(store, tickets, &branchHead{head: "C2"}, &sessionEnd{})
	got, err := c.Complete(context.Background(), mergedRun(), completion())
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	// Rewind: the close landed, the state was never written.
	got.CompletionState = orchestrator.CompleteCompleting
	if err := store.Upsert(context.Background(), got); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	n, err := c.RecoverCompletions(context.Background(),
		func(orchestrator.BuildRun) orchestrator.Completion { return completion() })
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if n != 1 {
		t.Errorf("recovered %d completions", n)
	}
	if store.rows["run-1"].CompletionState != orchestrator.CompleteClosed {
		t.Errorf("the run is in %q", store.rows["run-1"].CompletionState)
	}
	if len(tickets.closed) != 1 {
		t.Errorf("%d closes landed", len(tickets.closed))
	}
}

func TestReApprovalYieldsAFreshCloseKey(t *testing.T) {
	// A close that conflicted because its approval was reversed must not
	// replay its cached conflict when the review is reapproved at the same
	// revision.
	store, tickets := newMemStore(), newTicketAPI()
	tickets.reversed = true
	c := completer(store, tickets, &branchHead{head: "C2"}, &sessionEnd{})
	if _, err := c.Complete(context.Background(), mergedRun(), completion()); err == nil {
		t.Fatal("a conflicted close read as success")
	}
	conflicted := store.rows["run-1"].CloseKey

	tickets.reversed = false
	reapproved := mergedRun()
	reapproved.ReviewVerdictEvent = "event-10"
	got, err := c.Complete(context.Background(), reapproved, completion())
	if err != nil {
		t.Fatalf("complete after reapproval: %v", err)
	}
	if got.CloseKey == conflicted {
		t.Error("the retried close reused the conflicted key")
	}
}

func TestAnAdvancedHeadStopsTheCompletion(t *testing.T) {
	// Unreviewed commits landed. The ticket is NOT completed and the run goes
	// back to the pair loop for them.
	store, tickets := newMemStore(), newTicketAPI()
	_, err := completer(store, tickets, &branchHead{head: "C3"}, &sessionEnd{}).
		Complete(context.Background(), mergedRun(), completion())
	if !errors.Is(err, orchestrator.ErrHeadAdvanced) {
		t.Fatalf("got %v, want ErrHeadAdvanced", err)
	}
	if len(tickets.calls) != 0 {
		t.Error("the ticket was closed over unreviewed commits")
	}
	if store.rows["run-1"].CompletionState == orchestrator.CompleteClosed {
		t.Error("the run was recorded closed")
	}
}

func TestARunWithNoReviewCannotComplete(t *testing.T) {
	store, tickets := newMemStore(), newTicketAPI()
	run := mergedRun()
	run.ReviewID = ""
	if _, err := completer(store, tickets, &branchHead{head: "C2"}, &sessionEnd{}).
		Complete(context.Background(), run, completion()); err == nil {
		t.Fatal("a run with no review closed its ticket")
	}
	if len(tickets.calls) != 0 {
		t.Error("sutra was called with no review to name")
	}
}

func TestAnUnendableSessionStopsTheCompletion(t *testing.T) {
	store, tickets := newMemStore(), newTicketAPI()
	ender := &sessionEnd{err: errors.New("store unavailable")}
	if _, err := completer(store, tickets, &branchHead{head: "C2"}, ender).
		Complete(context.Background(), mergedRun(), completion()); err == nil {
		t.Fatal("the head was checked with the session still live")
	}
	if len(tickets.calls) != 0 {
		t.Error("the ticket closed without the session ending")
	}
}

func TestAnUnreadableHeadStopsTheCompletion(t *testing.T) {
	store, tickets := newMemStore(), newTicketAPI()
	head := &branchHead{err: errors.New("git unavailable")}
	if _, err := completer(store, tickets, head, &sessionEnd{}).
		Complete(context.Background(), mergedRun(), completion()); err == nil {
		t.Fatal("an unreadable head read as an unmoved one")
	}
	if len(tickets.calls) != 0 {
		t.Error("the ticket closed without knowing where the branch stood")
	}
}

func TestACommitAfterCompletionIsDetected(t *testing.T) {
	// Against the RECORDED head: the point is that the branch is no longer
	// where completion left it.
	store, tickets := newMemStore(), newTicketAPI()
	head := &branchHead{head: "C2"}
	c := completer(store, tickets, head, &sessionEnd{})
	got, err := c.Complete(context.Background(), mergedRun(), completion())
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	quiet, err := c.Advanced(context.Background(), got, "kriya/KRI-1/abcd")
	if err != nil {
		t.Fatalf("advanced: %v", err)
	}
	if quiet {
		t.Error("an unchanged branch reported as advanced")
	}
	head.head = "C3"
	advanced, err := c.Advanced(context.Background(), got, "kriya/KRI-1/abcd")
	if err != nil {
		t.Fatalf("advanced: %v", err)
	}
	if !advanced {
		t.Error("a commit landing after completion was not detected")
	}
}

func TestAnUncompletedRunIsNeverReportedAsAdvanced(t *testing.T) {
	// Nothing recorded a head, so there is nothing to compare against and no
	// claim to make.
	store, tickets := newMemStore(), newTicketAPI()
	c := completer(store, tickets, &branchHead{head: "C3"}, &sessionEnd{})
	advanced, err := c.Advanced(context.Background(), mergedRun(), "kriya/KRI-1/abcd")
	if err != nil {
		t.Fatalf("advanced: %v", err)
	}
	if advanced {
		t.Error("a run that never completed reported as advanced")
	}
}

func TestAnUnreadableCompletionListStopsRecovery(t *testing.T) {
	store := &failingRuns{listErr: errors.New("store unavailable")}
	c := orchestrator.Completer{
		Store: store, Tickets: newTicketAPI(),
		Branches: &branchHead{head: "C2"}, Sessions: &sessionEnd{}, Actor: "a",
	}
	if _, err := c.RecoverCompletions(context.Background(),
		func(orchestrator.BuildRun) orchestrator.Completion { return completion() }); err == nil {
		t.Fatal("an unreadable store read as no completions in flight")
	}
}

func TestTheCloseKeyIsStableForOneApproval(t *testing.T) {
	store, tickets := newMemStore(), newTicketAPI()
	c := completer(store, tickets, &branchHead{head: "C2"}, &sessionEnd{})
	first, err := c.Complete(context.Background(), mergedRun(), completion())
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	second, err := c.Complete(context.Background(), mergedRun(), completion())
	if err != nil {
		t.Fatalf("complete again: %v", err)
	}
	if first.CloseKey != second.CloseKey {
		t.Error("two closes of one approval keyed differently")
	}
	if len(tickets.closed) != 1 {
		t.Errorf("%d closes landed for one approval", len(tickets.closed))
	}
	if !strings.HasPrefix(first.CloseKey, second.CloseKey[:8]) {
		t.Error("the keys diverge")
	}
}
