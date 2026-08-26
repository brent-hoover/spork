package main

import (
	"context"
	"errors"
	"testing"

	"kriya/internal/devloop"
	"kriya/internal/orchestrator"
	"kriya/internal/planner"
	"kriya/internal/recovery"
	"kriya/internal/reviewbridge"
	"kriya/internal/workspace"
)

func TestWorkEventsAreConsumedBeforeClaimsAreReplayed(t *testing.T) {
	// A close that cached a conflict after a reopen would otherwise be
	// replayed with its epoch still unchanged: the conflict comes back,
	// recovery fails, and the advance that would have made the claim stale
	// never runs. The ordering is what breaks that cycle.
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var order []string
	steps := recoverySteps(intakerForTest(db), workspace.Manager{}, reviewbridge.Bridge{},
		devloop.Loop{}, orchestrator.Submitter{}, orchestrator.Queue{},
		orchestrator.Completer{}, "actor-1",
		planner.Claimer{Claims: recordingClaims{&order}}, noTargets,
		func(context.Context) error {
			order = append(order, "work")
			return nil
		}, noWork, "/target",
		orchestrator.Researcher{}, noFindingProject)
	for _, step := range steps {
		if step.Stage != recovery.StageTargets {
			continue
		}
		_ = step.Run(t.Context())
	}
	if len(order) < 2 {
		t.Fatalf("the target stage ran %v", order)
	}
	if order[0] != "work" {
		t.Errorf("claims were replayed before work events were consumed: %v", order)
	}
}

// recordingClaims notes when the claim store is asked for what is in flight.
type recordingClaims struct{ order *[]string }

func (r recordingClaims) Upsert(context.Context, planner.CompletionClaim) error { return nil }

func (r recordingClaims) Find(
	context.Context, string,
) (planner.CompletionClaim, bool, error) {
	return planner.CompletionClaim{}, false, nil
}

func (r recordingClaims) Submitting(context.Context) ([]planner.CompletionClaim, error) {
	*r.order = append(*r.order, "claims")
	return nil, nil
}

func (r recordingClaims) Closing(context.Context) ([]planner.CompletionClaim, error) {
	*r.order = append(*r.order, "closes")
	return nil, nil
}

func TestAnUnreplayableCloseDoesNotHaltStartup(t *testing.T) {
	// A cached conflict from a reversed approval cannot be escaped by
	// replaying the same key. The claim rests in closing and the driver
	// re-polls it — a reapproval rotates the key and the retry escapes the
	// cache. Halting startup would leave the operator with a kriya that will
	// not boot until they edit the database.
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	steps := recoverySteps(intakerForTest(db), workspace.Manager{}, reviewbridge.Bridge{},
		devloop.Loop{}, orchestrator.Submitter{}, orchestrator.Queue{},
		orchestrator.Completer{}, "actor-1",
		planner.Claimer{Claims: refusingClaims{}}, noTargets, noWork, noWork, "/target",
		orchestrator.Researcher{}, noFindingProject)
	for _, step := range steps {
		if step.Stage != recovery.StageTargets {
			continue
		}
		if err := step.Run(t.Context()); err != nil {
			t.Errorf("an unreplayable close halted startup: %v", err)
		}
	}
}

// refusingClaims hands back a close that will not replay.
type refusingClaims struct{}

func (refusingClaims) Upsert(context.Context, planner.CompletionClaim) error { return nil }

func (refusingClaims) Find(context.Context, string) (planner.CompletionClaim, bool, error) {
	return planner.CompletionClaim{}, false, nil
}

func (refusingClaims) Submitting(context.Context) ([]planner.CompletionClaim, error) {
	return nil, nil
}

func (refusingClaims) Closing(context.Context) ([]planner.CompletionClaim, error) {
	return nil, errors.New("the close store is unreachable")
}
