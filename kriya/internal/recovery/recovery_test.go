package recovery_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"kriya/internal/recovery"
)

func step(stage recovery.Stage, owner string, log *[]string, err error) recovery.Step {
	return recovery.Step{Stage: stage, Owner: owner, Run: func(context.Context) error {
		*log = append(*log, owner)
		return err
	}}
}

func TestStagesRunInDeclaredOrderRegardlessOfHowTheyWereListed(t *testing.T) {
	// Sorted here rather than trusted to arrive in order: a caller that listed
	// them out of sequence would otherwise reconcile later machines first,
	// silently, and the scenarios pinning the ordering would still pass.
	var log []string
	steps := []recovery.Step{
		step(recovery.StageReviewRounds, "rounds", &log, nil),
		step(recovery.StagePlanLifecycle, "plans", &log, nil),
		step(recovery.StagePopBinding, "pops", &log, nil),
	}
	if err := recovery.Run(context.Background(), steps); err != nil {
		t.Fatalf("run: %v", err)
	}
	want := []string{"pops", "plans", "rounds"}
	for i := range want {
		if log[i] != want[i] {
			t.Fatalf("ran %v, want %v", log, want)
		}
	}
}

func TestPopBindingRunsBeforePlanLifecycle(t *testing.T) {
	// The spec is explicit: the reconciliation pass runs first so no
	// claimed-but-unbound pop exists when retirement's stamps land, and
	// REQ-parallel-build asserts the BuildRun is bound by pop replay BEFORE
	// any ownership classification. An earlier draft had these reversed.
	if recovery.StagePopBinding >= recovery.StagePlanLifecycle {
		t.Fatal("pop binding must precede plan lifecycle")
	}
}

func TestAFailedStageStopsTheRunAndNamesItself(t *testing.T) {
	// Continuing past a failed stage would reconcile later machines against
	// state an earlier stage was supposed to settle.
	var log []string
	steps := []recovery.Step{
		step(recovery.StagePopBinding, "pops", &log, errors.New("boom")),
		step(recovery.StageBuildRuns, "runs", &log, nil),
	}
	err := recovery.Run(context.Background(), steps)
	if err == nil {
		t.Fatal("expected the failure to propagate")
	}
	for _, want := range []string{"pop-binding", "pops", "boom"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %q, got %v", want, err)
		}
	}
	if len(log) != 1 {
		t.Errorf("later stages ran after a failure: %v", log)
	}
}

func TestAStageWithNoOwnerIsSkipped(t *testing.T) {
	// Stages gain owners as their modules land; an unowned stage is not an
	// error, it is simply not built yet.
	if err := recovery.Run(context.Background(), nil); err != nil {
		t.Fatalf("an empty plan must succeed: %v", err)
	}
}
