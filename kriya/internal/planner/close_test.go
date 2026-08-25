package planner_test

import (
	"context"
	"errors"
	"testing"

	"kriya/internal/planner"
)

// epicDesk stands in for sutra's issue-status API, honouring keys and fences.
type epicDesk struct {
	closedByKey map[string]bool
	// revision is the epic's CURRENT subtree revision. A close naming a
	// different one is refused, exactly as sutra refuses it.
	revision int64
	calls    []closeCall
	err      error
}

type closeCall struct {
	key      string
	review   string
	revision int
	event    string
	fence    int64
}

func newEpicDesk() *epicDesk {
	return &epicDesk{closedByKey: map[string]bool{}}
}

func (e *epicDesk) Close(
	_ context.Context, _, review string, revision int,
	verdictEvent string, expectedSubtreeRevision int64, key string,
) error {
	e.calls = append(e.calls, closeCall{
		key: key, review: review, revision: revision,
		event: verdictEvent, fence: expectedSubtreeRevision,
	})
	if e.err != nil {
		return e.err
	}
	if e.closedByKey[key] {
		// Replayed under the key that stamped it: the original success, never
		// a close-used conflict.
		return nil
	}
	if expectedSubtreeRevision != e.revision {
		return errors.New("conflict: expected subtree_revision does not match")
	}
	e.closedByKey[key] = true
	return nil
}

func closer(
	claims *memClaims, epics *epicDesk, advances *memAdvances,
) planner.Claimer {
	return planner.Claimer{
		Claims: claims, Epics: epics,
		Epochs: planner.Epochs{Store: advances},
	}
}

func submittedClaim() planner.CompletionClaim {
	return planner.CompletionClaim{
		TargetKey: "/spec", State: planner.CompletionSubmitted, Epoch: 0,
		SubmissionKey: "sub-0", ReportVersion: "ver-1",
		ReviewID: "review-1", ReviewRevision: 1, SubtreeRevision: 7,
	}
}

func TestACloseIsWrittenAheadWithItsKey(t *testing.T) {
	// A crash after the close landed replays under the SAME key — sutra
	// returns the original success rather than a close-used conflict, since
	// this very key stamped it.
	claims, epics := newMemClaims(), newEpicDesk()
	epics.revision, epics.err = 7, errors.New("crash mid-close")
	if err := claims.Upsert(context.Background(), submittedClaim()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := closer(claims, epics, newMemAdvances()).
		Close(context.Background(), "/spec", "epic-1", "event-9"); err == nil {
		t.Fatal("expected the crash")
	}
	got := claims.rows["/spec"]
	if got.State != planner.CompletionClosing {
		t.Errorf("the claim is in state %q", got.State)
	}
	if got.CloseKey == "" || got.ApprovalEvent != "event-9" {
		t.Errorf("the close was not written ahead: %+v", got)
	}
}

func TestACloseThatLandedReplaysToItsOriginalSuccess(t *testing.T) {
	claims, epics, advances := newMemClaims(), newEpicDesk(), newMemAdvances()
	epics.revision = 7
	if err := claims.Upsert(context.Background(), submittedClaim()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	c := closer(claims, epics, advances)
	if _, err := c.Close(context.Background(), "/spec", "epic-1", "event-9"); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Replayed, as recovery would.
	if _, err := c.Close(context.Background(), "/spec", "epic-1", "event-9"); err != nil {
		t.Fatalf("replayed close: %v", err)
	}
	if len(epics.calls) != 2 {
		t.Fatalf("the close ran %d times", len(epics.calls))
	}
	if epics.calls[0].key != epics.calls[1].key {
		t.Errorf("the replay presented a different key: %v", epics.calls)
	}
	if claims.rows["/spec"].State != planner.CompletionComplete {
		t.Errorf("the claim settled as %q", claims.rows["/spec"].State)
	}
}

func TestTheCloseIsFencedOnTheCapturedSubtreeRevision(t *testing.T) {
	// "sutra rejects the close — history moved even though current state
	// matches". A child that reopened and recompleted before kriya consumed
	// either event advances the revision and leaves state identical.
	claims, epics := newMemClaims(), newEpicDesk()
	epics.revision = 9 // the world moved; the claim captured 7
	if err := claims.Upsert(context.Background(), submittedClaim()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := closer(claims, epics, newMemAdvances()).
		Close(context.Background(), "/spec", "epic-1", "event-9"); err == nil {
		t.Fatal("a close over moved history succeeded")
	}
	if len(epics.calls) != 1 || epics.calls[0].fence != 7 {
		t.Errorf("the close fenced on %v, not the captured 7", epics.calls)
	}
}

func TestAStaleClaimStampsNothingEvenWhenTheCloseLands(t *testing.T) {
	// The epoch advanced while the approval was in flight. sutra's gate was
	// satisfied at the time, so the close LANDED — and the stamp must still
	// refuse: the human approved a build that is no longer this one.
	claims, epics, advances := newMemClaims(), newEpicDesk(), newMemAdvances()
	epics.revision = 7
	if err := claims.Upsert(context.Background(), submittedClaim()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// A ticket reopens: the epoch moves past the claim's.
	if _, err := (planner.Epochs{Store: advances}).
		OnEvent(context.Background(), "/spec", planner.CauseTicketReopen, "E1"); err != nil {
		t.Fatalf("advance: %v", err)
	}
	_, err := closer(claims, epics, advances).
		Close(context.Background(), "/spec", "epic-1", "event-9")
	if !errors.Is(err, planner.ErrStaleClaim) {
		t.Fatalf("got %v, want ErrStaleClaim", err)
	}
	if _, ok := advances.stamped["/spec"]; ok {
		t.Error("a stale approval stamped the target")
	}
	if claims.rows["/spec"].State == planner.CompletionComplete {
		t.Error("the claim settled complete over a moved epoch")
	}
}

func TestAReapprovalAfterAConflictGetsAFreshKey(t *testing.T) {
	// "the new approval event yields a fresh close key ... never replaying
	// the cached conflict." A reversal-induced conflict is cached under the
	// old key, and reusing it would return that conflict forever.
	claims, epics := newMemClaims(), newEpicDesk()
	epics.revision = 7
	if err := claims.Upsert(context.Background(), submittedClaim()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	c := closer(claims, epics, newMemAdvances())

	epics.err = errors.New("conflict: the approval was reversed mid-flight")
	if _, err := c.Close(context.Background(), "/spec", "epic-1", "event-9"); err == nil {
		t.Fatal("expected the conflict")
	}
	firstKey := claims.rows["/spec"].CloseKey

	// Reapproved: a NEW verdict event.
	epics.err = nil
	if _, err := c.Close(context.Background(), "/spec", "epic-1", "event-10"); err != nil {
		t.Fatalf("retried close: %v", err)
	}
	got := claims.rows["/spec"]
	if got.CloseKey == firstKey {
		t.Error("the retry reused the key the conflict was cached under")
	}
	if got.ApprovalEvent != "event-10" {
		t.Errorf("the retry answered %q", got.ApprovalEvent)
	}
	if last := epics.calls[len(epics.calls)-1]; last.key == firstKey {
		t.Error("the call went out under the conflicted key")
	}
}

func TestACloseNeedsAnApprovalEvent(t *testing.T) {
	claims := newMemClaims()
	if err := claims.Upsert(context.Background(), submittedClaim()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := closer(claims, newEpicDesk(), newMemAdvances()).
		Close(context.Background(), "/spec", "epic-1", ""); err == nil {
		t.Fatal("a close with no verdict event was accepted")
	}
}

func TestATargetWithNoReviewCannotClose(t *testing.T) {
	if _, err := closer(newMemClaims(), newEpicDesk(), newMemAdvances()).
		Close(context.Background(), "/spec", "epic-1", "event-9"); err == nil {
		t.Fatal("a target with no completion review closed its epic")
	}
}

func TestTheCloseNamesTheReviewAtItsRecordedRevision(t *testing.T) {
	// sutra's gate checks the review it is being closed against. Naming a
	// different revision would spend an approval the human did not give.
	claims, epics := newMemClaims(), newEpicDesk()
	epics.revision = 7
	claim := submittedClaim()
	claim.ReviewRevision = 3
	if err := claims.Upsert(context.Background(), claim); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := closer(claims, epics, newMemAdvances()).
		Close(context.Background(), "/spec", "epic-1", "event-9"); err != nil {
		t.Fatalf("close: %v", err)
	}
	got := epics.calls[0]
	if got.review != "review-1" || got.revision != 3 || got.event != "event-9" {
		t.Errorf("the close named %+v", got)
	}
}
