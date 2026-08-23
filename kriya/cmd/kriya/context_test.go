package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kriya/internal/planner"
	"kriya/internal/specverify"
)

func TestTheContextSpecCarriesEachModulesLaw(t *testing.T) {
	got := specForContext(
		[]specverify.Module{{
			ID: "MOD-api", Name: "api",
			MayImport: []string{"MOD-store"},
			Commands:  map[string]string{"test": "go test ./..."},
			Contracts: []specverify.Contract{
				{ID: "CTR-api", Type: "openapi", Path: "contracts/api.yaml"},
			},
		}},
		[]specverify.ConstitutionEntry{{ID: "CON-a", Statement: "Tests come first."}},
		map[string]string{"contracts/api.yaml": "openapi: 3.1.0"},
	)
	if len(got.Modules) != 1 {
		t.Fatalf("mapped %d modules", len(got.Modules))
	}
	m := got.Modules[0]
	if m.ID != "MOD-api" || len(m.MayImport) != 1 || m.MayImport[0] != "MOD-store" {
		t.Errorf("boundary came through as %+v", m)
	}
	if len(m.Contracts) != 1 || m.Contracts[0].Path != "contracts/api.yaml" {
		t.Errorf("contracts came through as %+v", m.Contracts)
	}
	if m.Commands["test"] != "go test ./..." {
		t.Errorf("commands came through as %v", m.Commands)
	}
	if len(got.Constitution) != 1 || got.Constitution[0].ID != "CON-a" {
		t.Errorf("constitution came through as %+v", got.Constitution)
	}
	if got.Artifacts["contracts/api.yaml"] == "" {
		t.Error("the artifact set did not travel")
	}
}

func TestAnEmptySpecMapsToAnEmptyContext(t *testing.T) {
	got := specForContext(nil, nil, nil)
	if len(got.Modules) != 0 || len(got.Constitution) != 0 {
		t.Errorf("mapped %+v", got)
	}
}

func TestATicketNamingItsModuleScopesToIt(t *testing.T) {
	snap := planner.Snapshot{
		ResolvedCommands: map[string]map[string]string{"MOD-api": {"test": "t"}},
		Law: []specverify.Module{
			{ID: "MOD-api", Name: "api"}, {ID: "MOD-store", Name: "store"},
		},
	}
	got := modulesFor(snap, "MOD-api")
	if len(got) != 1 || got[0] != "MOD-api" {
		t.Errorf("scoped to %v", got)
	}
}

func TestATicketNamingNoModuleSeesThemAll(t *testing.T) {
	// A context assembled for nothing would show the agent no law at all.
	snap := planner.Snapshot{
		ResolvedCommands: map[string]map[string]string{"MOD-api": {"test": "t"}},
		Law: []specverify.Module{
			{ID: "MOD-store", Name: "store"}, {ID: "MOD-api", Name: "api"},
		},
	}
	got := modulesFor(snap, "Create a short link")
	if len(got) != 2 || got[0] != "MOD-api" || got[1] != "MOD-store" {
		t.Errorf("scoped to %v, want every module in a stable order", got)
	}
}

func TestTheOperatorsInstructionsAreReadVerbatim(t *testing.T) {
	dir := t.TempDir()
	body := "# House rules\n\nNever swallow an exception.\n"
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := operatorInstructions(dir); got != body {
		t.Errorf("read %q", got)
	}
}

func TestAGENTSmdIsTheFallback(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("agents"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := operatorInstructions(dir); got != "agents" {
		t.Errorf("read %q", got)
	}
}

func TestAProjectWithNoInstructionsIsNotAnError(t *testing.T) {
	if got := operatorInstructions(t.TempDir()); got != "" {
		t.Errorf("read %q from a project carrying neither file", got)
	}
}

func TestAnUnreadableInstructionFileDoesNotStopTheBuild(t *testing.T) {
	// Silent context loss is the risk; a build that refuses to start because
	// an optional file is unreadable is worse than one that reports it.
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "CLAUDE.md"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if got := operatorInstructions(dir); got != "" {
		t.Errorf("read %q from a directory named CLAUDE.md", got)
	}
}

func TestTheSnapshotRoundTripsItsLaw(t *testing.T) {
	// The law is pinned alongside the files because context assembly is judged
	// against what was ADMITTED.
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := planner.SQLSnapshots{DB: db}
	want := planner.Snapshot{
		Hash: "hash-1",
		Law: []specverify.Module{{
			ID: "MOD-api", Name: "api", MayImport: []string{"MOD-store"},
			Contracts: []specverify.Contract{{ID: "CTR-api", Type: "openapi", Path: "c.yaml"}},
		}},
		Constitution: []specverify.ConstitutionEntry{{ID: "CON-a", Statement: "s"}},
	}
	if err := store.Put(t.Context(), want); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := store.Get(t.Context(), "hash-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Law) != 1 || got.Law[0].MayImport[0] != "MOD-store" {
		t.Errorf("law came back %+v", got.Law)
	}
	if len(got.Constitution) != 1 || got.Constitution[0].ID != "CON-a" {
		t.Errorf("constitution came back %+v", got.Constitution)
	}
	if _, err := json.Marshal(got); err != nil {
		t.Errorf("the snapshot no longer encodes: %v", err)
	}
	if !strings.HasPrefix(got.Hash, "hash-1") {
		t.Errorf("hash came back %q", got.Hash)
	}
}
