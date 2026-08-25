package planner_test

import (
	"context"
	"errors"
	"testing"

	"kriya/internal/planner"
)

func TestRecoveryChecksTheEpochBeforeReplayingSutra(t *testing.T) {
	// A subtree or reversed-approval conflict is CACHED under the persisted
	// key. Replaying sutra first means every startup receives that conflict
	// before reaching the stamp — and since only a stale claim is tolerated,
	// recovery fails forever and kriya never boots again.
	claims, epics, advances := newMemClaims(), newEpicDesk(), newMemAdvances()
	epics.revision = 9 // the claim captured 7: the close will conflict
	claim := submittedClaim()
	claim.State = planner.CompletionClosing
	claim.ApprovalEvent, claim.CloseKey = "event-9", planner.CloseKey("/spec", 0, "event-9")
	if err := claims.Upsert(context.Background(), claim); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// The world moved on: the epoch advanced past the claim's.
	if _, err := (planner.Epochs{Store: advances}).
		OnEvent(context.Background(), "/spec", planner.CauseTicketReopen, "E1"); err != nil {
		t.Fatalf("advance: %v", err)
	}

	_, err := closer(claims, epics, advances).
		RecoverCloses(context.Background(), "", func(planner.CompletionClaim) (string, error) {
			return "epic-1", nil
		})
	if !errors.Is(err, planner.ErrStaleClaim) {
		t.Fatalf("got %v, want the claim recognised as stale", err)
	}
	if len(epics.calls) != 0 {
		t.Errorf("sutra was called for a claim already known stale: %+v", epics.calls)
	}
}

func TestAStaleClosingClaimIsSettledRatherThanRetriedForever(t *testing.T) {
	// A claim whose epoch moved will never stamp. Leaving it in closing means
	// recovery retries it on every startup for the life of the database.
	claims, epics, advances := newMemClaims(), newEpicDesk(), newMemAdvances()
	claim := submittedClaim()
	claim.State = planner.CompletionClosing
	claim.ApprovalEvent, claim.CloseKey = "event-9", planner.CloseKey("/spec", 0, "event-9")
	if err := claims.Upsert(context.Background(), claim); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := (planner.Epochs{Store: advances}).
		OnEvent(context.Background(), "/spec", planner.CauseTicketReopen, "E1"); err != nil {
		t.Fatalf("advance: %v", err)
	}
	c := closer(claims, epics, advances)
	if _, err := c.RecoverCloses(context.Background(), "",
		func(planner.CompletionClaim) (string, error) { return "epic-1", nil }); !errors.Is(err, planner.ErrStaleClaim) {
		t.Fatalf("first recovery: %v", err)
	}
	// The second startup finds nothing to replay: the stale claim was
	// settled, and a fresh attempt at the new epoch is what comes next.
	n, err := c.RecoverCloses(context.Background(), "",
		func(planner.CompletionClaim) (string, error) { return "epic-1", nil })
	if err != nil {
		t.Fatalf("second recovery: %v", err)
	}
	if n != 0 {
		t.Errorf("the second startup replayed %d stale closes", n)
	}
}

func TestAClosingClaimAtItsOwnEpochStillReplays(t *testing.T) {
	// The control: only a claim the world moved past is skipped. One whose
	// epoch still holds is a close that must be finished.
	claims, epics := newMemClaims(), newEpicDesk()
	epics.revision = 7
	claim := submittedClaim()
	claim.State = planner.CompletionClosing
	claim.ApprovalEvent, claim.CloseKey = "event-9", planner.CloseKey("/spec", 0, "event-9")
	if err := claims.Upsert(context.Background(), claim); err != nil {
		t.Fatalf("seed: %v", err)
	}
	n, err := closer(claims, epics, newMemAdvances()).
		RecoverCloses(context.Background(), "", func(planner.CompletionClaim) (string, error) {
			return "epic-1", nil
		})
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if n != 1 || len(epics.calls) != 1 {
		t.Errorf("recovered %d, called sutra %d times", n, len(epics.calls))
	}
	if claims.rows["/spec"].State != planner.CompletionComplete {
		t.Errorf("the claim settled as %q", claims.rows["/spec"].State)
	}
}
