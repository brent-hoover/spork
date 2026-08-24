package workspace_test

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"kriya/internal/fakes"
	"kriya/internal/workspace"
)

// memStore is workspace's own port, so its double lives here.
type memStore struct {
	rows map[string]workspace.Workspace
}

func newMemStore() *memStore { return &memStore{rows: map[string]workspace.Workspace{}} }

func (m *memStore) Upsert(_ context.Context, w workspace.Workspace) error {
	m.rows[w.Run] = w
	return nil
}

func (m *memStore) Find(_ context.Context, run string) (workspace.Workspace, bool, error) {
	w, ok := m.rows[run]
	return w, ok, nil
}

func (m *memStore) Pending(context.Context) ([]workspace.Workspace, error) {
	return m.inState(workspace.StatePending), nil
}

func (m *memStore) Created(context.Context) ([]workspace.Workspace, error) {
	return m.inState(workspace.StateCreated), nil
}

// inState lists rows in a state that are still expected on disk. Order is by
// run, because recovery replays in a declared order.
func (m *memStore) inState(state string) []workspace.Workspace {
	var out []workspace.Workspace
	for _, w := range m.rows {
		if w.State == state && !w.Removed() {
			out = append(out, w)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Run < out[j].Run })
	return out
}

// fakeGit records what was asked and can be made to fail.
type fakeGit struct {
	added, removed int
	base           string
	unmerged       bool
	addErr         error
	removeErr      error
	branches       []string
	// integrated records what was merged into the branch, so a test can see
	// whether it took the moved head.
	integrated   []string
	integrateErr error
}

func (g *fakeGit) DefaultBranchCommit(context.Context, string, string) (string, error) {
	if g.base == "" {
		g.base = "base-commit"
	}
	return g.base, nil
}

func (g *fakeGit) AddWorktree(_ context.Context, _, _, branch, _ string) error {
	if g.addErr != nil {
		return g.addErr
	}
	g.added++
	g.branches = append(g.branches, branch)
	return nil
}

func (g *fakeGit) RemoveWorktree(context.Context, string, string) error {
	if g.removeErr != nil {
		return g.removeErr
	}
	g.removed++
	return nil
}

// Integrate records what was merged in, so a test can see whether the branch
// took the moved head.
func (g *fakeGit) Integrate(_ context.Context, _, _, commit string) error {
	if g.integrateErr != nil {
		return g.integrateErr
	}
	g.integrated = append(g.integrated, commit)
	return nil
}

func (g *fakeGit) HasUnmergedCommits(context.Context, string, string, string) (bool, error) {
	return g.unmerged, nil
}

func manager(store *memStore, git *fakeGit) workspace.Manager {
	return workspace.Manager{
		Repo: "/repo", Root: "/wt", DefaultBranch: "main",
		Store: store, Git: git, Now: fakes.NewClock(time.Unix(0, 0)),
	}
}

func TestEachRunGetsItsOwnBranchAndPath(t *testing.T) {
	store, git := newMemStore(), &fakeGit{}
	m := manager(store, git)
	a, err := m.Ensure(context.Background(), "run-1", "KRI-1")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	b, err := m.Ensure(context.Background(), "run-2", "KRI-2")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if a.Branch == b.Branch {
		t.Errorf("both runs share branch %q", a.Branch)
	}
	if a.Path == b.Path {
		t.Errorf("both runs share path %q", a.Path)
	}
}

func TestASuccessorRunForTheSameTicketNeverCollides(t *testing.T) {
	// A reopened ticket's new run must not reuse the predecessor's branch,
	// which survives quarantined with unmerged commits until disposal.
	store, git := newMemStore(), &fakeGit{}
	m := manager(store, git)
	first, _ := m.Ensure(context.Background(), "run-1", "KRI-7")
	second, err := m.Ensure(context.Background(), "run-2", "KRI-7")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if first.Branch == second.Branch {
		t.Fatalf("the successor reused branch %q", first.Branch)
	}
	// The predecessor's row is untouched.
	kept, found, _ := store.Find(context.Background(), "run-1")
	if !found || kept.State != workspace.StateCreated {
		t.Error("the predecessor's workspace was disturbed")
	}
}

func TestTheRowIsPersistedBeforeGitCreatesAnything(t *testing.T) {
	// A crash between the write and the worktree must leave a record recovery
	// can act on, not an orphan directory nothing knows about. Failing the git
	// call proves the row exists anyway.
	store, git := newMemStore(), &fakeGit{addErr: errors.New("git exploded")}
	if _, err := manager(store, git).Ensure(context.Background(), "run-1", "KRI-1"); err == nil {
		t.Fatal("expected the worktree creation to fail")
	}
	row, found, _ := store.Find(context.Background(), "run-1")
	if !found {
		t.Fatal("no workspace row survived; there is nothing to recover from")
	}
	if row.State != workspace.StatePending {
		t.Errorf("state is %q, want pending", row.State)
	}
	// The row must carry everything the creation needs, or recovery cannot
	// finish it. Inverted on the first draft — it returned when a value was
	// EMPTY, so the test passed by giving up.
	for name, value := range map[string]string{
		"path": row.Path, "branch": row.Branch, "base": row.Base,
	} {
		if value == "" {
			t.Errorf("the pending row records no %s", name)
		}
	}
}

func TestEnsureIsIdempotent(t *testing.T) {
	store, git := newMemStore(), &fakeGit{}
	m := manager(store, git)
	first, _ := m.Ensure(context.Background(), "run-1", "KRI-1")
	again, err := m.Ensure(context.Background(), "run-1", "KRI-1")
	if err != nil {
		t.Fatalf("ensure again: %v", err)
	}
	if git.added != 1 {
		t.Errorf("created %d worktrees for one run", git.added)
	}
	if again.Branch != first.Branch {
		t.Error("the second call produced a different branch")
	}
}

func TestABranchNameSurvivesAnAwkwardTicket(t *testing.T) {
	// Branch names are read by humans, and a ticket title is arbitrary text.
	store, git := newMemStore(), &fakeGit{}
	w, err := manager(store, git).Ensure(context.Background(), "run-1",
		"walking skeleton: intake → tickets (v2)")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !strings.HasPrefix(w.Branch, "kriya/walking-skeleton") {
		t.Errorf("branch %q lost the ticket it belongs to", w.Branch)
	}
	for _, bad := range []string{" ", ":", "→", "(", ")"} {
		if strings.Contains(w.Branch, bad) {
			t.Errorf("branch %q kept %q, which git will not accept", w.Branch, bad)
		}
	}
	if strings.Contains(w.Branch, "run-1") {
		t.Errorf("branch %q embeds the run id raw", w.Branch)
	}
}

func TestTwoRunsOnOneTicketGetSeparateBranches(t *testing.T) {
	// A retry must not land on the predecessor's branch, which is quarantined
	// with unmerged commits.
	store, git := newMemStore(), &fakeGit{}
	m := manager(store, git)
	first, err := m.Ensure(context.Background(), "run-1", "same ticket")
	if err != nil {
		t.Fatalf("ensure first: %v", err)
	}
	second, err := m.Ensure(context.Background(), "run-2", "same ticket")
	if err != nil {
		t.Fatalf("ensure second: %v", err)
	}
	if first.Branch == second.Branch || first.Path == second.Path {
		t.Errorf("two runs share %q at %q", first.Branch, first.Path)
	}
}

// fakeProbe answers from a set of paths that "exist".
type fakeProbe struct {
	here  map[string]bool
	err   error
	asked []string
}

func (p *fakeProbe) Exists(path string) (bool, error) {
	p.asked = append(p.asked, path)
	if p.err != nil {
		return false, p.err
	}
	return p.here[path], nil
}

func managerWith(store *memStore, git *fakeGit, probe *fakeProbe) workspace.Manager {
	m := manager(store, git)
	m.Probe = probe
	return m
}

func TestRecoveryAdoptsAWorktreeThatIsAlreadyThere(t *testing.T) {
	// The row promised this path and something is at it, so creation in fact
	// succeeded and only the write did not. Recreating would fail on git's own
	// refusal for no reason.
	store, git := newMemStore(), &fakeGit{}
	w, err := manager(store, git).Ensure(context.Background(), "run-1", "KRI-1")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	// Rewind to the crash window: the row says pending, the worktree exists.
	w.State = workspace.StatePending
	if err := store.Upsert(context.Background(), w); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	before := git.added
	probe := &fakeProbe{here: map[string]bool{w.Path: true}}

	if _, err := managerWith(store, git, probe).Recover(context.Background()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if git.added != before {
		t.Error("an existing worktree was recreated instead of adopted")
	}
	got, _, err := store.Find(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got.State != workspace.StateCreated {
		t.Errorf("the adopted row is in state %q", got.State)
	}
}

func TestRecoveryRecreatesAPendingWorktreeThatIsAbsent(t *testing.T) {
	// Creation was never proven, so nothing was lost.
	store, git := newMemStore(), &fakeGit{}
	if err := store.Upsert(context.Background(), workspace.Workspace{
		Run: "run-1", Ticket: "KRI-1", Path: "/gone", Branch: "kriya/KRI-1/abcd1234",
		Base: "base-sha", State: workspace.StatePending,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	probe := &fakeProbe{here: map[string]bool{}}
	if _, err := managerWith(store, git, probe).Recover(context.Background()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if git.added != 1 {
		t.Errorf("created %d worktrees, want 1", git.added)
	}
}

func TestAVanishedCreatedWorktreeSurfacesRatherThanRecreating(t *testing.T) {
	// Proven state has vanished, and silently recreating it would hide
	// whatever destroyed it.
	store, git := newMemStore(), &fakeGit{}
	if err := store.Upsert(context.Background(), workspace.Workspace{
		Run: "run-1", Ticket: "KRI-1", Path: "/gone", Branch: "kriya/KRI-1/abcd1234",
		Base: "base-sha", State: workspace.StateCreated,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	probe := &fakeProbe{here: map[string]bool{}}
	_, err := managerWith(store, git, probe).Recover(context.Background())
	if err == nil {
		t.Fatal("a vanished worktree was passed over in silence")
	}
	if !strings.Contains(err.Error(), "/gone") {
		t.Errorf("the error %q does not name the path", err)
	}
	if git.added != 0 {
		t.Error("the vanished worktree was silently recreated")
	}
}

func TestACreatedWorktreeStillThereIsNotADiscrepancy(t *testing.T) {
	store, git := newMemStore(), &fakeGit{}
	w, err := manager(store, git).Ensure(context.Background(), "run-1", "KRI-1")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	probe := &fakeProbe{here: map[string]bool{w.Path: true}}
	if _, err := managerWith(store, git, probe).Recover(context.Background()); err != nil {
		t.Fatalf("recover: %v", err)
	}
}

func TestAProbeThatCannotAnswerIsAnError(t *testing.T) {
	// "I could not look" is not "it is not there".
	store, git := newMemStore(), &fakeGit{}
	if err := store.Upsert(context.Background(), workspace.Workspace{
		Run: "run-1", Ticket: "KRI-1", Path: "/somewhere", State: workspace.StatePending,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	probe := &fakeProbe{err: errors.New("permission denied")}
	if _, err := managerWith(store, git, probe).Recover(context.Background()); err == nil {
		t.Fatal("an unanswerable probe read as an absent worktree")
	}
	if git.added != 0 {
		t.Error("a worktree was created on the strength of a failed probe")
	}
}

func TestRecoveryProbesTheRecordedPath(t *testing.T) {
	// No orphaned worktree is undiscoverable, which only holds if the path
	// recovery looks at is the one the row promised.
	store, git := newMemStore(), &fakeGit{}
	if err := store.Upsert(context.Background(), workspace.Workspace{
		Run: "run-1", Ticket: "KRI-1", Path: "/promised/path", State: workspace.StatePending,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	probe := &fakeProbe{here: map[string]bool{}}
	if _, err := managerWith(store, git, probe).Recover(context.Background()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(probe.asked) != 1 || probe.asked[0] != "/promised/path" {
		t.Errorf("probed %v", probe.asked)
	}
}

func TestIntegrationTakesTheMovedDefaultHead(t *testing.T) {
	// A run whose merge or completion failed comes back to the pair loop, and
	// rerunning the chain against a base the default branch has moved past
	// just fails the same way.
	store, git := newMemStore(), &fakeGit{}
	m := manager(store, git)
	if _, err := m.Ensure(context.Background(), "run-1", "KRI-1"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	git.base = "D2"

	got, err := m.Integrate(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("integrate: %v", err)
	}
	if len(git.integrated) != 1 || git.integrated[0] != "D2" {
		t.Errorf("integrated %v", git.integrated)
	}
	if got.Base != "D2" {
		t.Errorf("the recorded base is %q — the next chain would freeze the old one", got.Base)
	}
	// Durable, because the next chain reads it back rather than being handed it.
	row, _, err := store.Find(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if row.Base != "D2" {
		t.Errorf("the stored base is %q", row.Base)
	}
}

func TestAnUnmovedHeadIntegratesNothing(t *testing.T) {
	// Merging a branch into itself makes an empty commit and a new head that
	// reviews identically.
	store, git := newMemStore(), &fakeGit{}
	m := manager(store, git)
	if _, err := m.Ensure(context.Background(), "run-1", "KRI-1"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if _, err := m.Integrate(context.Background(), "run-1"); err != nil {
		t.Fatalf("integrate: %v", err)
	}
	if len(git.integrated) != 0 {
		t.Errorf("integrated %v against an unmoved head", git.integrated)
	}
}

func TestAConflictingIntegrationSurfaces(t *testing.T) {
	// A conflict is exactly the kind of thing the pair loop exists to resolve,
	// but nothing can resolve one it was never told about.
	store, git := newMemStore(), &fakeGit{}
	m := manager(store, git)
	if _, err := m.Ensure(context.Background(), "run-1", "KRI-1"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	git.base = "D2"
	git.integrateErr = errors.New("CONFLICT in handler.go")
	if _, err := m.Integrate(context.Background(), "run-1"); err == nil {
		t.Fatal("a conflicting integration read as integrated")
	}
	row, _, err := store.Find(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if row.Base == "D2" {
		t.Error("the base advanced despite the integration failing")
	}
}

func TestIntegratingAWorkspaceThatDoesNotExistFails(t *testing.T) {
	store, git := newMemStore(), &fakeGit{}
	if _, err := manager(store, git).Integrate(context.Background(), "run-absent"); err == nil {
		t.Fatal("a run with no workspace integrated something")
	}
}
