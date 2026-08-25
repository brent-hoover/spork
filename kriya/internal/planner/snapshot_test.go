package planner_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"kriya/internal/fakes"
	"kriya/internal/planner"
	"kriya/internal/specverify"
)

// memSnapshots is an in-memory SnapshotStore.
//
// It lives here rather than in internal/fakes because SnapshotStore is
// planner's OWN port, not a seam. internal/fakes doubles the seams — clock,
// specverify, trackerclient, agent — that several packages share, and arch-go
// forbids it importing any module. A double for a module's own port belongs
// with that module's tests.
type memSnapshots struct{ stored map[string]planner.Snapshot }

func newMemSnapshots() *memSnapshots {
	return &memSnapshots{stored: map[string]planner.Snapshot{}}
}

func (s *memSnapshots) Put(_ context.Context, snap planner.Snapshot) error {
	s.stored[snap.Hash] = snap
	return nil
}

func (s *memSnapshots) Get(_ context.Context, hash string) (planner.Snapshot, error) {
	snap, ok := s.stored[hash]
	if !ok {
		return planner.Snapshot{}, errors.New("no snapshot " + hash)
	}
	return snap, nil
}

func (s *memSnapshots) Count(_ context.Context) (int, error) { return len(s.stored), nil }

func specDir(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "avspec.yaml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return dir
}

func intaker(t *testing.T, dir string, artifacts []string) (planner.Intaker, *memSnapshots) {
	t.Helper()
	cmds := map[string]string{}
	for _, name := range specverify.RequiredCommands {
		cmds[name] = "run-" + name
	}
	v := fakes.NewVerifier(dir, specverify.Report{Status: "ready", OK: true})
	v.Models = map[string]specverify.Model{dir: {
		OK:        true,
		Modules:   []specverify.Module{{ID: "MOD-a", Name: "a", Commands: cmds}},
		Artifacts: artifacts,
	}}
	store := newMemSnapshots()
	return planner.Intaker{
		Verify:    v,
		Snapshots: store, Attempts: newMemAttempts(),
		Now: fakes.NewClock(time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)),
	}, store
}

func TestAPinnedSnapshotHoldsTheArtifactsAndCommands(t *testing.T) {
	dir := specDir(t, "avspec: \"0.3\"\n")
	in, store := intaker(t, dir, []string{"avspec.yaml"})
	intake, err := in.AdmitAndPin(context.Background(), dir, "token-1")
	if err != nil {
		t.Fatalf("admit and pin: %v", err)
	}
	if intake.Snapshot.Content["avspec.yaml"] != "avspec: \"0.3\"\n" {
		t.Errorf("manifest not pinned, got %q", intake.Snapshot.Content["avspec.yaml"])
	}
	if intake.Snapshot.ResolvedCommands["MOD-a"]["mutation"] != "run-mutation" {
		t.Error("resolved commands not pinned")
	}
	if n, _ := store.Count(context.Background()); n != 1 {
		t.Errorf("expected one stored snapshot, got %d", n)
	}
}

func TestTheSnapshotIsTheAuthorityAfterWorkingTreeEdits(t *testing.T) {
	// AC-intake-snapshot-authority: edits after intake cannot change what a
	// pinned build executes. Reading through the store rather than the tree is
	// the whole mechanism, so the test edits the tree and reads the store.
	dir := specDir(t, "original\n")
	in, store := intaker(t, dir, []string{"avspec.yaml"})
	intake, err := in.AdmitAndPin(context.Background(), dir, "token-1")
	if err != nil {
		t.Fatalf("pin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "avspec.yaml"), []byte("edited\n"), 0o644); err != nil {
		t.Fatalf("edit: %v", err)
	}
	got, err := store.Get(context.Background(), intake.Snapshot.Hash)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Content["avspec.yaml"] != "original\n" {
		t.Errorf("the working-tree edit reached the snapshot: %q", got.Content["avspec.yaml"])
	}
}

func TestCommandsChangeTheHashEvenWhenFilesDoNot(t *testing.T) {
	// Two intakes of identical files whose modules resolve different commands
	// are different builds. Hashing only artifacts would collide them, and a
	// build could execute commands intake never validated.
	dir := specDir(t, "same\n")
	a, _ := intaker(t, dir, []string{"avspec.yaml"})
	first, err := a.AdmitAndPin(context.Background(), dir, "token-a")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	b, _ := intaker(t, dir, []string{"avspec.yaml"})
	v, ok := b.Verify.(*fakes.Verifier)
	if !ok {
		t.Fatal("expected the fake verifier")
	}
	m := v.Models[dir]
	m.Modules[0].Commands["mutation"] = "a-different-command"
	v.Models[dir] = m
	second, err := b.AdmitAndPin(context.Background(), dir, "token-b")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first.Snapshot.Hash == second.Snapshot.Hash {
		t.Error("identical files with different resolved commands hashed the same")
	}
}

func TestIdenticalInputPinsIdenticalHash(t *testing.T) {
	dir := specDir(t, "same\n")
	a, _ := intaker(t, dir, []string{"avspec.yaml"})
	first, err := a.AdmitAndPin(context.Background(), dir, "token-a")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	b, _ := intaker(t, dir, []string{"avspec.yaml"})
	second, err := b.AdmitAndPin(context.Background(), dir, "token-b")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first.Snapshot.Hash != second.Snapshot.Hash {
		t.Error("content addressing must be stable across intakes")
	}
}

func TestAMissingArtifactIsAnError(t *testing.T) {
	dir := specDir(t, "avspec: \"0.3\"\n")
	in, store := intaker(t, dir, []string{"avspec.yaml", "verification/absent.feature"})
	if _, err := in.AdmitAndPin(context.Background(), dir, "token-1"); err == nil {
		t.Fatal("a referenced artifact that does not exist must fail intake")
	}
	if n, _ := store.Count(context.Background()); n != 0 {
		t.Error("nothing may be pinned when the artifact set is incomplete")
	}
}

func TestARefusedIntakePinsNothing(t *testing.T) {
	// AC-intake-refuse: no snapshot is pinned and nothing is enqueued.
	dir := specDir(t, "avspec: \"0.3\"\n")
	in, store := intaker(t, dir, []string{"avspec.yaml"})
	v, ok := in.Verify.(*fakes.Verifier)
	if !ok {
		t.Fatal("expected the fake verifier")
	}
	v.Reports[dir] = specverify.Report{Status: "draft", OK: true}
	if _, err := in.AdmitAndPin(context.Background(), dir, "token-1"); err == nil {
		t.Fatal("a draft spec must be refused")
	}
	if n, _ := store.Count(context.Background()); n != 0 {
		t.Errorf("a refused intake pinned %d snapshots", n)
	}
}

func TestAnArtifactSetWithoutTheManifestIsRefused(t *testing.T) {
	// A set carrying some other readable file but not avspec.yaml would pin a
	// "full artifact set" with no spec in it, and every later read of that
	// snapshot would find nothing to read. Non-empty is not the same as
	// complete.
	dir := specDir(t, "avspec: \"0.3\"\n")
	if err := os.WriteFile(filepath.Join(dir, "other.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	in, store := intaker(t, dir, []string{"other.txt"})
	if _, err := in.AdmitAndPin(context.Background(), dir, "token-1"); err == nil {
		t.Fatal("an artifact set without the manifest must be refused")
	}
	if n, _ := store.Count(context.Background()); n != 0 {
		t.Error("nothing may be pinned")
	}
}

// pinWith admits a spec whose model carries the given law, and returns the
// snapshot's hash.
func pinWith(t *testing.T, law []specverify.Module, constitution []specverify.ConstitutionEntry) string {
	t.Helper()
	dir := specDir(t, "avspec: \"0.3\"\n")
	cmds := map[string]string{}
	for _, name := range specverify.RequiredCommands {
		cmds[name] = "run-" + name
	}
	for i := range law {
		law[i].Commands = cmds
	}
	v := fakes.NewVerifier(dir, specverify.Report{Status: "ready", OK: true})
	v.Models = map[string]specverify.Model{dir: {
		OK: true, Modules: law, Constitution: constitution,
		Artifacts: []string{"avspec.yaml"},
	}}
	in := planner.Intaker{
		Verify:    v,
		Snapshots: newMemSnapshots(), Attempts: newMemAttempts(),
		Now: fakes.NewClock(time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)),
	}
	intake, err := in.AdmitAndPin(context.Background(), dir, "token-1")
	if err != nil {
		t.Fatalf("admit and pin: %v", err)
	}
	return intake.Snapshot.Hash
}

func TestTheHashCoversTheModuleLaw(t *testing.T) {
	// Two intakes of identical files whose modules declare different
	// boundaries are different builds: an agent told a different law would
	// write different code from the same spec.
	narrow := pinWith(t, []specverify.Module{{ID: "MOD-a", Name: "a"}}, nil)
	wide := pinWith(t, []specverify.Module{
		{ID: "MOD-a", Name: "a", MayImport: []string{"MOD-anything"}},
	}, nil)
	if narrow == wide {
		t.Error("widening a module's boundary did not change the snapshot hash")
	}
}

func TestTheHashCoversAModulesContracts(t *testing.T) {
	bare := pinWith(t, []specverify.Module{{ID: "MOD-a", Name: "a"}}, nil)
	published := pinWith(t, []specverify.Module{{
		ID: "MOD-a", Name: "a",
		Contracts: []specverify.Contract{{ID: "CTR-a", Type: "openapi", Path: "c.yaml"}},
	}}, nil)
	if bare == published {
		t.Error("publishing a contract did not change the snapshot hash")
	}
}

func TestTheHashCoversTheConstitution(t *testing.T) {
	none := pinWith(t, []specverify.Module{{ID: "MOD-a", Name: "a"}}, nil)
	amended := pinWith(t, []specverify.Module{{ID: "MOD-a", Name: "a"}},
		[]specverify.ConstitutionEntry{{ID: "CON-a", Statement: "Tests come first."}})
	if none == amended {
		t.Error("adding a constitution entry did not change the snapshot hash")
	}
}

func TestTheLawTravelsOnThePinnedSnapshot(t *testing.T) {
	dir := specDir(t, "avspec: \"0.3\"\n")
	cmds := map[string]string{}
	for _, name := range specverify.RequiredCommands {
		cmds[name] = "run-" + name
	}
	v := fakes.NewVerifier(dir, specverify.Report{Status: "ready", OK: true})
	v.Models = map[string]specverify.Model{dir: {
		OK: true, Artifacts: []string{"avspec.yaml"},
		Modules: []specverify.Module{{
			ID: "MOD-a", Name: "a", Commands: cmds, MayImport: []string{"MOD-b"},
			Contracts: []specverify.Contract{{ID: "CTR-a", Type: "openapi", Path: "c.yaml"}},
		}},
		Constitution: []specverify.ConstitutionEntry{{ID: "CON-a", Statement: "s"}},
	}}
	in := planner.Intaker{
		Verify:    v,
		Snapshots: newMemSnapshots(), Attempts: newMemAttempts(),
		Now: fakes.NewClock(time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)),
	}
	intake, err := in.AdmitAndPin(context.Background(), dir, "token-1")
	if err != nil {
		t.Fatalf("admit and pin: %v", err)
	}
	if len(intake.Snapshot.Law) != 1 || intake.Snapshot.Law[0].MayImport[0] != "MOD-b" {
		t.Errorf("law pinned as %+v", intake.Snapshot.Law)
	}
	if len(intake.Snapshot.Constitution) != 1 || intake.Snapshot.Constitution[0].ID != "CON-a" {
		t.Errorf("constitution pinned as %+v", intake.Snapshot.Constitution)
	}
}
