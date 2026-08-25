package planner_test

import (
	"context"
	"testing"

	"kriya/internal/planner"
)

func TestRecoveryLeavesAnotherTargetsCloseAlone(t *testing.T) {
	// Startup consumes work events for the target it was invoked for and no
	// other. Replaying a foreign close would do it with an epoch nothing had
	// refreshed: its cached conflict comes back and recovery fails for a
	// build this command is not even about.
	claims, epics, advances := newMemClaims(), newEpicDesk(), newMemAdvances()
	epics.revision = 7
	for _, target := range []string{"/mine", "/theirs"} {
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
		RecoverCloses(context.Background(), "/mine",
			func(planner.CompletionClaim) (string, error) { return "epic-1", nil })
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if n != 1 {
		t.Errorf("replayed %d closes for one target", n)
	}
	if claims.rows["/theirs"].State != planner.CompletionClosing {
		t.Errorf("another target's close was replayed: %q", claims.rows["/theirs"].State)
	}
	if len(epics.calls) != 1 {
		t.Errorf("sutra was called %d times", len(epics.calls))
	}
}

func TestAnUnscopedRecoveryReplaysEverything(t *testing.T) {
	// The control: an empty target means "every one", which is what a
	// recovery with no build in hand wants.
	claims, epics := newMemClaims(), newEpicDesk()
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
	n, err := closer(claims, epics, newMemAdvances()).
		RecoverCloses(context.Background(), "",
			func(planner.CompletionClaim) (string, error) { return "epic-1", nil })
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if n != 2 {
		t.Errorf("an unscoped recovery replayed %d closes", n)
	}
}
