package planner_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"kriya/internal/fakes"
	"kriya/internal/planner"
	"kriya/internal/specverify"
)

// complete is a module whose effective stack declares all six gates.
func complete(id string) specverify.Module {
	cmds := map[string]string{}
	for _, name := range specverify.RequiredCommands {
		cmds[name] = "run-" + name
	}
	return specverify.Module{ID: id, Name: id, Commands: cmds}
}

// admit runs a full intake against a spec directory holding only a manifest,
// and reports the outcome. There is no admit-without-pinning entry point by
// design, so these tests exercise the real path.
func admit(t *testing.T, r specverify.Report) error {
	t.Helper()
	dir := specDir(t, "avspec: \"0.3\"\n")
	v := fakes.NewVerifier(dir, r)
	v.Models = map[string]specverify.Model{dir: {
		OK:        true,
		Modules:   []specverify.Module{complete("MOD-a")},
		Artifacts: []string{"avspec.yaml"},
	}}
	in := planner.Intaker{Verify: v, Snapshots: newMemSnapshots(), Attempts: newMemAttempts(), Now: fakes.NewClock(time.Unix(0, 0))}
	_, err := in.AdmitAndPin(context.Background(), dir, "token-1")
	return err
}

// admitModel runs intake with a programmed model, for the command checks.
func admitModel(t *testing.T, mods ...specverify.Module) error {
	t.Helper()
	dir := specDir(t, "avspec: \"0.3\"\n")
	v := fakes.NewVerifier(dir, specverify.Report{Status: "ready", OK: true})
	v.Models = map[string]specverify.Model{dir: {
		OK: true, Modules: mods, Artifacts: []string{"avspec.yaml"},
	}}
	in := planner.Intaker{Verify: v, Snapshots: newMemSnapshots(), Attempts: newMemAttempts(), Now: fakes.NewClock(time.Unix(0, 0))}
	_, err := in.AdmitAndPin(context.Background(), dir, "token-1")
	return err
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
	dir := specDir(t, "x")
	v := fakes.NewVerifier(dir, specverify.Report{})
	v.Err = errors.New("avspec exited 2")
	in := planner.Intaker{Verify: v, Snapshots: newMemSnapshots(), Attempts: newMemAttempts(), Now: fakes.NewClock(time.Unix(0, 0))}
	_, err := in.AdmitAndPin(context.Background(), dir, "token-1")
	var refusal *planner.Refusal
	if errors.As(err, &refusal) {
		t.Fatal("a verifier malfunction must not be reported as a refusal")
	}
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestAModuleMissingAGateCommandRefusesIntake(t *testing.T) {
	// AC-intake-commands: refused "even when avspec verify alone reports
	// ready". avspec does not require commands at all, so this check is
	// kriya's alone — and a missing one is a gate that silently never runs.
	partial := complete("MOD-a")
	delete(partial.Commands, "mutation")
	err := admitModel(t, complete("MOD-ok"), partial)

	var refusal *planner.Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("expected a refusal, got: %v", err)
	}
	for _, want := range []string{"MOD-a", "mutation"} {
		if !strings.Contains(refusal.Reason, want) {
			t.Errorf("refusal must name the module and the command; %q missing from %q", want, refusal.Reason)
		}
	}
}

func TestEveryRequiredCommandIsChecked(t *testing.T) {
	// Table-driven so a command dropped from the check is caught, rather than
	// one representative standing in for six.
	for _, missing := range specverify.RequiredCommands {
		t.Run(missing, func(t *testing.T) {
			m := complete("MOD-a")
			delete(m.Commands, missing)
			err := admitModel(t, m)
			var refusal *planner.Refusal
			if !errors.As(err, &refusal) {
				t.Fatalf("missing %q must refuse intake, got: %v", missing, err)
			}
			if !strings.Contains(refusal.Reason, missing) {
				t.Errorf("refusal should name %q, got %q", missing, refusal.Reason)
			}
		})
	}
}

func TestASpecWithNoModulesIsRefused(t *testing.T) {
	// A spec with no modules verifies ready and has no gate chain to run.
	err := admitModel(t)
	var refusal *planner.Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("expected a refusal, got: %v", err)
	}
}

func TestInstallIsNotRequired(t *testing.T) {
	// AC-intake-commands names six, and install is not among them.
	m := complete("MOD-a")
	delete(m.Commands, "install")
	if err := admitModel(t, m); err != nil {
		t.Fatalf("install is not a required gate, got: %v", err)
	}
}
