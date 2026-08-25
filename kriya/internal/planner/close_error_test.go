package planner_test

import (
	"context"
	"errors"
	"testing"

	"kriya/internal/planner"
)

func TestAnUnreadableClaimStopsAClose(t *testing.T) {
	// Closing without knowing what the claim promised would name a review at
	// a revision nobody recorded.
	claims := newMemClaims()
	claims.err = errors.New("disk full")
	if _, err := closer(claims, newEpicDesk(), newMemAdvances()).
		Close(context.Background(), "/spec", "epic-1", "event-9"); err == nil {
		t.Fatal("a close proceeded on an unreadable claim")
	}
}

func TestAnUnwritableClosingStateStopsTheCall(t *testing.T) {
	// The close key must be on disk BEFORE the call. Without it, a crash
	// after the close landed replays under a freshly derived key and hits
	// sutra's close-used conflict — a build that can never be finished.
	claims, epics := newMemClaims(), newEpicDesk()
	epics.revision = 7
	if err := claims.Upsert(context.Background(), submittedClaim()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	claims.failAfter = 1
	if _, err := closer(claims, epics, newMemAdvances()).
		Close(context.Background(), "/spec", "epic-1", "event-9"); err == nil {
		t.Fatal("the close ran with no key on disk")
	}
	if len(epics.calls) != 0 {
		t.Errorf("the epic was closed anyway: %+v", epics.calls)
	}
}

func TestAnUnstampableTargetStopsTheClose(t *testing.T) {
	// "I could not tell whether the claim is current" is not "it is". The
	// epic is closed by this point, and reporting success would leave the
	// target complete with nothing recording it.
	claims, epics := newMemClaims(), newEpicDesk()
	epics.revision = 7
	advances := newMemAdvances()
	if err := claims.Upsert(context.Background(), submittedClaim()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	advances.err = errors.New("disk full")
	if _, err := closer(claims, epics, advances).
		Close(context.Background(), "/spec", "epic-1", "event-9"); err == nil {
		t.Fatal("an unstampable target read as complete")
	}
}

func TestRecoveryReplaysEveryClosingClaim(t *testing.T) {
	claims, epics, advances := newMemClaims(), newEpicDesk(), newMemAdvances()
	epics.revision = 7
	for _, target := range []string{"/a", "/b"} {
		claim := submittedClaim()
		claim.TargetKey = target
		claim.State = planner.CompletionClosing
		claim.ApprovalEvent = "event-9"
		claim.CloseKey = planner.CloseKey(target, 0, "event-9")
		if err := claims.Upsert(context.Background(), claim); err != nil {
			t.Fatalf("seed %s: %v", target, err)
		}
	}
	n, err := closer(claims, epics, advances).
		RecoverCloses(context.Background(), "", func(planner.CompletionClaim) (string, error) {
			return "epic-1", nil
		})
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if n != 2 {
		t.Errorf("recovered %d closes", n)
	}
	for _, target := range []string{"/a", "/b"} {
		if claims.rows[target].State != planner.CompletionComplete {
			t.Errorf("%s settled as %q", target, claims.rows[target].State)
		}
	}
}

func TestRecoveryFinishesTheOthersWhenOneIsStale(t *testing.T) {
	// Stopping at the first stale claim would leave every later target's
	// close unreplayed — and those closes are real, landed calls whose
	// results kriya has not recorded.
	claims, epics, advances := newMemClaims(), newEpicDesk(), newMemAdvances()
	epics.revision = 7
	// /a's epoch has moved; /b's has not.
	if _, err := (planner.Epochs{Store: advances}).
		OnEvent(context.Background(), "/a", planner.CauseTicketReopen, "E1"); err != nil {
		t.Fatalf("advance: %v", err)
	}
	for _, target := range []string{"/a", "/b"} {
		claim := submittedClaim()
		claim.TargetKey = target
		claim.State = planner.CompletionClosing
		claim.ApprovalEvent, claim.CloseKey = "event-9", planner.CloseKey(target, 0, "event-9")
		if err := claims.Upsert(context.Background(), claim); err != nil {
			t.Fatalf("seed %s: %v", target, err)
		}
	}
	_, err := closer(claims, epics, advances).
		RecoverCloses(context.Background(), "", func(planner.CompletionClaim) (string, error) {
			return "epic-1", nil
		})
	if !errors.Is(err, planner.ErrStaleClaim) {
		t.Fatalf("got %v, want the stale claim reported", err)
	}
	if claims.rows["/b"].State != planner.CompletionComplete {
		t.Errorf("the healthy target was left at %q", claims.rows["/b"].State)
	}
	if claims.rows["/a"].State == planner.CompletionComplete {
		t.Error("the stale target stamped complete")
	}
}

func TestAnUnlistableClosingSetStopsRecovery(t *testing.T) {
	claims := newMemClaims()
	claims.err = errors.New("disk full")
	if _, err := closer(claims, newEpicDesk(), newMemAdvances()).
		RecoverCloses(context.Background(), "", func(planner.CompletionClaim) (string, error) {
			return "epic-1", nil
		}); err == nil {
		t.Fatal("an unreadable claim store read as nothing closing")
	}
}

func TestARecoveredCloseThatCannotResolveItsEpicStops(t *testing.T) {
	claims := newMemClaims()
	claim := submittedClaim()
	claim.State = planner.CompletionClosing
	claim.ApprovalEvent, claim.CloseKey = "event-9", "close-key"
	if err := claims.Upsert(context.Background(), claim); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := closer(claims, newEpicDesk(), newMemAdvances()).
		RecoverCloses(context.Background(), "", func(planner.CompletionClaim) (string, error) {
			return "", errors.New("no build target for this claim")
		}); err == nil {
		t.Fatal("a claim with no epic was replayed anyway")
	}
}
