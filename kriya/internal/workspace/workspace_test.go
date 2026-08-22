package workspace_test

import (
	"context"
	"errors"
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
	var out []workspace.Workspace
	for _, w := range m.rows {
		if w.State == workspace.StatePending {
			out = append(out, w)
		}
	}
	return out, nil
}

// fakeGit records what was asked and can be made to fail.
type fakeGit struct {
	added, removed int
	base           string
	unmerged       bool
	addErr         error
	removeErr      error
	branches       []string
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
	if !found || kept.State != workspace.StateReady {
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
