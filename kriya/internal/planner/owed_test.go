package planner_test

import (
	"context"
	"errors"
	"testing"

	"kriya/internal/planner"
)

func TestAStaleCloseRecordsThatAReopenIsOwed(t *testing.T) {
	// The close LANDED — sutra returned success a moment before the stamp was
	// refused — so the epic is closed over an epoch this claim no longer
	// owns. Settling silently would turn "the epic is wrongly closed" into
	// nothing at all.
	claims, epics, advances := newMemClaims(), newEpicDesk(), newMemAdvances()
	epics.revision = 7
	if err := claims.Upsert(context.Background(), submittedClaim()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := (planner.Epochs{Store: advances}).
		OnEvent(context.Background(), "/spec", planner.CauseTicketReopen, "E1"); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if _, err := closer(claims, epics, advances).
		Close(context.Background(), "/spec", "epic-1", "event-9"); !errors.Is(err, planner.ErrStaleClaim) {
		t.Fatalf("got %v", err)
	}
	got := claims.rows["/spec"]
	if got.State != planner.CompletionStale {
		t.Errorf("the claim is in state %q", got.State)
	}
	if !got.ReopenOwed {
		t.Error("a landed close over a spent epoch owes no reopen")
	}
}

func TestARecoveredStaleCloseAlsoOwesAReopen(t *testing.T) {
	// Its close was ISSUED — the row reached closing before the call — and
	// whether it landed is unknown from here. "We do not know" must not read
	// as "nothing happened".
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
	if _, err := closer(claims, epics, advances).
		RecoverCloses(context.Background(), func(planner.CompletionClaim) (string, error) {
			return "epic-1", nil
		}); !errors.Is(err, planner.ErrStaleClaim) {
		t.Fatalf("got %v", err)
	}
	if !claims.rows["/spec"].ReopenOwed {
		t.Error("an unresolved close owes no reopen")
	}
	if len(epics.calls) != 0 {
		t.Error("sutra was called for a claim already known stale")
	}
}

func TestACompletedClaimOwesNothing(t *testing.T) {
	// The control: a close that landed for a claim that IS current leaves the
	// epic legitimately closed, and an owed reopen would undo a real
	// completion.
	claims, epics := newMemClaims(), newEpicDesk()
	epics.revision = 7
	if err := claims.Upsert(context.Background(), submittedClaim()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := closer(claims, epics, newMemAdvances()).
		Close(context.Background(), "/spec", "epic-1", "event-9"); err != nil {
		t.Fatalf("close: %v", err)
	}
	got := claims.rows["/spec"]
	if got.State != planner.CompletionComplete {
		t.Fatalf("the claim settled as %q", got.State)
	}
	if got.ReopenOwed {
		t.Error("a legitimate completion owes a reopen")
	}
}
