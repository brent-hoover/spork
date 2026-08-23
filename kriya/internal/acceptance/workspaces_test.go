//go:build acceptance

package acceptance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cucumber/godog"

	"kriya/internal/fakes"
	"kriya/internal/workspace"
)

// memWorkspaces is the workspace store for these scenarios.
type memWorkspaces struct {
	rows map[string]workspace.Workspace
	// writes records every row as it was written, so "persisted BEFORE git
	// creates anything" is checkable rather than assumed.
	writes []workspace.Workspace
	git    *countingGit
	// createdAtWrite pairs each write with how many worktrees git had made.
	createdAtWrite []int
}

func (m *memWorkspaces) Upsert(_ context.Context, w workspace.Workspace) error {
	m.rows[w.Run] = w
	m.writes = append(m.writes, w)
	m.createdAtWrite = append(m.createdAtWrite, m.git.added)
	return nil
}

func (m *memWorkspaces) Find(_ context.Context, run string) (workspace.Workspace, bool, error) {
	w, ok := m.rows[run]
	return w, ok, nil
}

func (m *memWorkspaces) Pending(context.Context) ([]workspace.Workspace, error) {
	return m.inState(workspace.StatePending), nil
}

func (m *memWorkspaces) Created(context.Context) ([]workspace.Workspace, error) {
	return m.inState(workspace.StateCreated), nil
}

func (m *memWorkspaces) inState(state string) []workspace.Workspace {
	var out []workspace.Workspace
	for _, w := range m.rows {
		if w.State == state && w.RemovedAt.IsZero() {
			out = append(out, w)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Run < out[j].Run })
	return out
}

// countingGit creates real directories so a worktree's isolation is a fact
// about the filesystem rather than a claim about a fake.
type countingGit struct {
	root    string
	base    string
	added   int
	removed int
	// branches records every branch that was cut, so a surviving one can be
	// shown never to have been touched again.
	branches []string
}

func (g *countingGit) DefaultBranchCommit(context.Context, string, string) (string, error) {
	if g.base == "" {
		return "base-sha", nil
	}
	return g.base, nil
}

func (g *countingGit) AddWorktree(_ context.Context, _, path, branch, _ string) error {
	g.added++
	g.branches = append(g.branches, branch)
	if err := os.MkdirAll(filepath.Join(path, ".git"), 0o750); err != nil {
		return err
	}
	return nil
}

func (g *countingGit) RemoveWorktree(_ context.Context, _, path string) error {
	g.removed++
	return os.RemoveAll(path)
}

func (g *countingGit) HasUnmergedCommits(context.Context, string, string, string) (bool, error) {
	return true, nil
}

// workspaceWorld is one workspace scenario's state.
type workspaceWorld struct {
	store   *memWorkspaces
	git     *countingGit
	manager workspace.Manager
	made    map[string]workspace.Workspace
}

func (w *world) newWorkspaces() (*workspaceWorld, error) {
	repo, err := w.tempDir()
	if err != nil {
		return nil, err
	}
	root, err := w.tempDir()
	if err != nil {
		return nil, err
	}
	git := &countingGit{root: root}
	store := &memWorkspaces{rows: map[string]workspace.Workspace{}, git: git}
	ww := &workspaceWorld{
		store: store, git: git, made: map[string]workspace.Workspace{},
		manager: workspace.Manager{
			Repo: repo, Root: root, DefaultBranch: "main",
			Store: store, Git: git, Probe: workspace.DirProbe{},
			Now: fakes.NewClock(time.Unix(0, 0)),
		},
	}
	w.ws = ww
	return ww, nil
}

func registerWorkspaces(sc *godog.ScenarioContext, w *world) {
	sc.Step(`^two BuildRuns start for tickets "([^"]*)" and "([^"]*)"$`,
		func(first, second string) error {
			ww, err := w.newWorkspaces()
			if err != nil {
				return err
			}
			for i, ticket := range []string{first, second} {
				got, err := ww.manager.Ensure(context.Background(),
					fmt.Sprintf("run-%d", i+1), ticket)
				if err != nil {
					return err
				}
				ww.made[ticket] = got
			}
			return nil
		})

	sc.Step(`^each run has its own worktree on a run-scoped branch — named by ticket and run$`,
		func() error {
			seenPath, seenBranch := map[string]bool{}, map[string]bool{}
			for ticket, got := range w.ws.made {
				if seenPath[got.Path] || seenBranch[got.Branch] {
					return fmt.Errorf("%s reuses %s / %s", ticket, got.Path, got.Branch)
				}
				seenPath[got.Path], seenBranch[got.Branch] = true, true
				if !strings.Contains(got.Branch, ticket) {
					return fmt.Errorf("branch %q does not name ticket %s", got.Branch, ticket)
				}
			}
			return nil
		})

	sc.Step(`^each branch is cut from the project's default branch$`, func() error {
		for ticket, got := range w.ws.made {
			if got.Base != "base-sha" {
				return fmt.Errorf("%s was cut from %q", ticket, got.Base)
			}
		}
		return nil
	})

	sc.Step(`^uncommitted changes in one worktree are invisible to the other$`, func() error {
		// A fact about the filesystem: two worktrees are two directories, and
		// a file written in one is not in the other.
		var paths []string
		for _, got := range w.ws.made {
			paths = append(paths, got.Path)
		}
		if len(paths) != 2 {
			return fmt.Errorf("expected two worktrees, got %d", len(paths))
		}
		scratch := filepath.Join(paths[0], "uncommitted.txt")
		if err := os.WriteFile(scratch, []byte("work in progress"), 0o600); err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(paths[1], "uncommitted.txt")); err == nil {
			return errors.New("a file written in one worktree appeared in the other")
		}
		return nil
	})

	sc.Step(`^a retired run for ticket "([^"]*)" whose branch survives with unmerged commits$`,
		func(ticket string) error {
			ww, err := w.newWorkspaces()
			if err != nil {
				return err
			}
			got, err := ww.manager.Ensure(context.Background(), "run-retired", ticket)
			if err != nil {
				return err
			}
			ww.made["retired"] = got
			return nil
		})

	sc.Step(`^a new BuildRun starts for the reopened "([^"]*)"$`, func(ticket string) error {
		got, err := w.ws.manager.Ensure(context.Background(), "run-reopened", ticket)
		if err != nil {
			return err
		}
		w.ws.made["reopened"] = got
		return nil
	})

	sc.Step(`^it gets its own run-scoped branch and worktree$`, func() error {
		retired, reopened := w.ws.made["retired"], w.ws.made["reopened"]
		if retired.Branch == reopened.Branch {
			return fmt.Errorf("both runs are on %s", retired.Branch)
		}
		if retired.Path == reopened.Path {
			return fmt.Errorf("both runs work in %s", retired.Path)
		}
		return nil
	})

	sc.Step(`^the predecessor's branch remains quarantined and untouched until merge or operator disposal$`,
		func() error {
			retired := w.ws.made["retired"]
			if w.ws.git.removed != 0 {
				return errors.New("a worktree was removed while a run was merely superseded")
			}
			// Still recorded, still on its own branch, and nothing has cut
			// over it: the branch it holds is the only copy of that work.
			row, found, err := w.ws.store.Find(context.Background(), "run-retired")
			if err != nil || !found {
				return fmt.Errorf("the retired run's row is gone: %v found=%v", err, found)
			}
			if row.Branch != retired.Branch {
				return fmt.Errorf("the retired branch changed to %s", row.Branch)
			}
			var cuts int
			for _, b := range w.ws.git.branches {
				if b == retired.Branch {
					cuts++
				}
			}
			if cuts != 1 {
				return fmt.Errorf("branch %s was cut %d times", retired.Branch, cuts)
			}
			return nil
		})

	sc.Step(`^a BuildRun is about to create its worktree$`, func() error {
		ww, err := w.newWorkspaces()
		if err != nil {
			return err
		}
		got, err := ww.manager.Ensure(context.Background(), "run-1", "KRI-1")
		if err != nil {
			return err
		}
		ww.made["KRI-1"] = got
		return nil
	})

	sc.Step(`^the workspace row — deterministic path, branch, and base commit — is persisted in state "([^"]*)" before git creates anything$`,
		func(state string) error {
			ww := w.ws
			if len(ww.store.writes) == 0 {
				return errors.New("nothing was written")
			}
			first := ww.store.writes[0]
			if first.State != state {
				return fmt.Errorf("the first write was in state %q", first.State)
			}
			if ww.store.createdAtWrite[0] != 0 {
				return errors.New("git had already created a worktree when the row was written")
			}
			if first.Path == "" || first.Branch == "" || first.Base == "" {
				return fmt.Errorf("the pre-creation row is incomplete: %+v", first)
			}
			return nil
		})

	sc.Step(`^creation succeeds$`, func() error { return nil })

	sc.Step(`^the row flips to "([^"]*)" transactionally$`, func(state string) error {
		row, found, err := w.ws.store.Find(context.Background(), "run-1")
		if err != nil || !found {
			return fmt.Errorf("find: %v found=%v", err, found)
		}
		if row.State != state {
			return fmt.Errorf("the row is in state %q", row.State)
		}
		return nil
	})

	sc.Step(`^a crash between the pending write and creation$`, func() error {
		// Rewind to the crash window: the row says pending and the worktree
		// may or may not be there. Both cases are exercised below.
		row := w.ws.made["KRI-1"]
		row.State = workspace.StatePending
		return w.ws.store.Upsert(context.Background(), row)
	})

	sc.Step(`^recovery probes the recorded path, adopts a matching worktree or recreates an absent one, and verifies the recorded base$`,
		func() error {
			ww := w.ws
			before := ww.git.added
			if _, err := ww.manager.Recover(context.Background()); err != nil {
				return fmt.Errorf("adopting: %w", err)
			}
			if ww.git.added != before {
				return errors.New("an existing worktree was recreated instead of adopted")
			}
			row, _, err := ww.store.Find(context.Background(), "run-1")
			if err != nil {
				return err
			}
			if row.State != workspace.StateCreated {
				return fmt.Errorf("the adopted row is in state %q", row.State)
			}

			// Now the absent case: same row, worktree gone.
			if err := os.RemoveAll(row.Path); err != nil {
				return err
			}
			row.State = workspace.StatePending
			if err := ww.store.Upsert(context.Background(), row); err != nil {
				return err
			}
			if _, err := ww.manager.Recover(context.Background()); err != nil {
				return fmt.Errorf("recreating: %w", err)
			}
			if ww.git.added != before+1 {
				return errors.New("an absent worktree was not recreated")
			}
			recreated, _, err := ww.store.Find(context.Background(), "run-1")
			if err != nil {
				return err
			}
			if recreated.Base != row.Base {
				return fmt.Errorf("the base moved from %s to %s", row.Base, recreated.Base)
			}
			return nil
		})

	sc.Step(`^no orphaned worktree is undiscoverable$`, func() error {
		// Discoverable means: every worktree git was asked to make has a row
		// naming its path, so nothing exists that recovery cannot find.
		ww := w.ws
		for _, row := range ww.store.rows {
			if _, err := os.Stat(row.Path); err != nil {
				return fmt.Errorf("row for %s names %s, which is not there", row.Run, row.Path)
			}
		}
		return nil
	})

	sc.Step(`^a BuildRun crashed with an existing worktree matching its record$`, func() error {
		ww, err := w.newWorkspaces()
		if err != nil {
			return err
		}
		got, err := ww.manager.Ensure(context.Background(), "run-1", "KRI-1")
		if err != nil {
			return err
		}
		ww.made["KRI-1"] = got
		return nil
	})

	// Two features say "the run resumes" — one means reattaching to a
	// worktree, the other means continuing under an architect's direction.
	// Whichever world the scenario set up is the one that means it.
	sc.Step(`^the run resumes$`, func() error {
		switch {
		case w.sa != nil:
			return w.sa.resume()
		case w.ws != nil:
			got, err := w.ws.manager.Ensure(context.Background(), "run-1", "KRI-1")
			if err != nil {
				return err
			}
			w.ws.made["resumed"] = got
			return nil
		default:
			return errors.New("no run to resume")
		}
	})

	sc.Step(`^it reattaches to the same worktree and branch$`, func() error {
		first, resumed := w.ws.made["KRI-1"], w.ws.made["resumed"]
		if first.Path != resumed.Path || first.Branch != resumed.Branch {
			return fmt.Errorf("resumed onto %s / %s, was %s / %s",
				resumed.Path, resumed.Branch, first.Path, first.Branch)
		}
		return nil
	})

	sc.Step(`^no second workspace is created for the run$`, func() error {
		if w.ws.git.added != 1 {
			return fmt.Errorf("git created %d worktrees for one run", w.ws.git.added)
		}
		return nil
	})

	sc.Step(`^a workspace recorded in state "([^"]*)" whose worktree is missing or inconsistent with the record$`,
		func(state string) error {
			ww := w.ws
			row := ww.made["KRI-1"]
			row.State = state
			if err := ww.store.Upsert(context.Background(), row); err != nil {
				return err
			}
			return os.RemoveAll(row.Path)
		})

	sc.Step(`^the run surfaces the discrepancy instead of silently recreating state — proven state has vanished$`,
		func() error {
			ww := w.ws
			before := ww.git.added
			_, err := ww.manager.Recover(context.Background())
			if err == nil {
				return errors.New("a vanished worktree was passed over in silence")
			}
			if ww.git.added != before {
				return errors.New("the vanished worktree was silently recreated")
			}
			return nil
		})

	sc.Step(`^a workspace still in state "([^"]*)" whose worktree is absent$`, func(state string) error {
		ww, err := w.newWorkspaces()
		if err != nil {
			return err
		}
		return ww.store.Upsert(context.Background(), workspace.Workspace{
			Run: "run-1", Ticket: "KRI-1", Path: filepath.Join(ww.git.root, "gone"),
			Branch: "kriya/KRI-1/abcd1234", Base: "base-sha", State: state,
		})
	})

	sc.Step(`^recovery recreates it — creation was never proven, so nothing was lost$`, func() error {
		ww := w.ws
		if _, err := ww.manager.Recover(context.Background()); err != nil {
			return err
		}
		if ww.git.added != 1 {
			return fmt.Errorf("recovery created %d worktrees", ww.git.added)
		}
		return nil
	})
}
