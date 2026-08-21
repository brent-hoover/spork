package planner_test

import (
	"context"
	"errors"
	"testing"

	"kriya/internal/fakes"
	"kriya/internal/planner"
	"kriya/internal/specverify"
)

func admit(t *testing.T, r specverify.Report) error {
	t.Helper()
	in := planner.Intaker{Verify: fakes.NewVerifier("/spec", r)}
	return in.Admit(context.Background(), "/spec")
}

func TestAReadySpecIsAdmitted(t *testing.T) {
	if err := admit(t, specverify.Report{Status: "ready", OK: true}); err != nil {
		t.Fatalf("a clean ready spec must be admitted, got: %v", err)
	}
}

func TestADraftSpecIsRefused(t *testing.T) {
	// The trap: avspec reports OK=true and exits 0 for a draft, because todos
	// do not block at draft. Only Status reveals it.
	err := admit(t, specverify.Report{
		Status: "draft", OK: true,
		Counts: specverify.Counts{Todo: 5},
	})
	var refusal *planner.Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("a draft spec must be refused, got: %v", err)
	}
	if refusal.Status != "draft" {
		t.Errorf("refusal should name the status, got %q", refusal.Status)
	}
}

func TestAnUnverifiableSpecIsRefusedWithItsFindings(t *testing.T) {
	err := admit(t, specverify.Report{
		Status: "unknown", OK: false,
		Counts:   specverify.Counts{Error: 1},
		Findings: []specverify.Finding{{Code: "MANIFEST_MISSING", Severity: "error"}},
	})
	var refusal *planner.Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("expected a refusal, got: %v", err)
	}
	if len(refusal.Findings) != 1 || refusal.Findings[0].Code != "MANIFEST_MISSING" {
		t.Errorf("AC-intake-refuse requires the findings travel with the refusal, got %+v", refusal.Findings)
	}
}

func TestAReadyClaimWithTodoFindingsIsRefused(t *testing.T) {
	// A spec may claim ready and still carry todos; todos block at ready.
	err := admit(t, specverify.Report{
		Status: "ready", OK: true,
		Counts: specverify.Counts{Todo: 2},
	})
	var refusal *planner.Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("ready + todos must be refused, got: %v", err)
	}
}

func TestAReadyClaimWithErrorFindingsIsRefused(t *testing.T) {
	err := admit(t, specverify.Report{
		Status: "ready", OK: true,
		Counts: specverify.Counts{Error: 1},
	})
	var refusal *planner.Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("ready + errors must be refused, got: %v", err)
	}
}

func TestAVerifierMalfunctionIsNotARefusal(t *testing.T) {
	// A broken verifier must not read as a clean refusal: the operator has to
	// know the difference between "your spec is wrong" and "I could not look".
	v := fakes.NewVerifier("/spec", specverify.Report{})
	v.Err = errors.New("avspec exited 2")
	in := planner.Intaker{Verify: v}
	err := in.Admit(context.Background(), "/spec")
	var refusal *planner.Refusal
	if errors.As(err, &refusal) {
		t.Fatal("a verifier malfunction must not be reported as a refusal")
	}
	if err == nil {
		t.Fatal("expected an error")
	}
}
