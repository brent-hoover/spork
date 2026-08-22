// Package workspace manages the worktree and branch lifecycle per ticket.
//
// Every BuildRun works in its own worktree on a run-scoped branch, so two runs
// cannot see each other's uncommitted changes and a retired run's branch
// survives untouched until it is merged or the operator disposes of it.
package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"

	"kriya/internal/clock"
)

// States a workspace passes through.
const (
	StatePending = "pending"
	StateReady   = "ready"
	StateRemoved = "removed"
)

// Workspace is one run's isolated checkout.
type Workspace struct {
	// Run scopes the workspace. Two runs for the SAME ticket get different
	// branches: a reopened ticket must not collide with its predecessor's
	// branch, which survives with unmerged commits until disposal.
	Run    string
	Ticket string
	Path   string
	Branch string
	// Base is the commit the branch was cut from, recorded so a merge can
	// check the base has not moved under it.
	Base    string
	State   string
	Cleanup string
}

// Store persists workspaces.
type Store interface {
	Upsert(ctx context.Context, w Workspace) error
	Find(ctx context.Context, run string) (Workspace, bool, error)
	// Pending lists workspaces a crash left mid-creation.
	Pending(ctx context.Context) ([]Workspace, error)
}

// Git is the slice of git the workspace needs.
//
// An interface this package declares, so the lifecycle is testable without a
// repository and the shell-out lives in one place.
type Git interface {
	// DefaultBranchCommit resolves the base a new branch is cut from.
	DefaultBranchCommit(ctx context.Context, repo, branch string) (string, error)
	// AddWorktree creates a worktree at path on a new branch cut from base.
	AddWorktree(ctx context.Context, repo, path, branch, base string) error
	// RemoveWorktree removes a worktree, refusing if it is dirty.
	RemoveWorktree(ctx context.Context, repo, path string) error
	// HasUnmergedCommits reports whether branch holds commits absent from base.
	HasUnmergedCommits(ctx context.Context, repo, branch, base string) (bool, error)
}

// Manager creates and disposes workspaces.
type Manager struct {
	// Repo is the target project's git repository.
	Repo string
	// Root is where worktrees are created.
	Root          string
	DefaultBranch string
	Store         Store
	Git           Git
	Now           clock.Clock
}

// branchName is deterministic and run-scoped.
//
// The ticket alone would collide: a reopened ticket's new run must not reuse
// the predecessor's branch, which is quarantined with unmerged commits. The
// run id is hashed rather than embedded raw because it is a uuid and git
// branch names are read by humans.
func branchName(ticket, run string) string {
	sum := sha256.Sum256([]byte(run))
	return "kriya/" + sanitise(ticket) + "/" + hex.EncodeToString(sum[:])[:8]
}

func sanitise(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, s)
}

// Ensure returns the workspace for a run, creating it if needed.
//
// The row is persisted in state pending BEFORE git creates anything: the path,
// branch, and base are deterministic, so a crash between the write and the
// worktree leaves a record recovery can act on rather than an orphan directory
// nothing knows about.
func (m Manager) Ensure(ctx context.Context, run, ticket string) (Workspace, error) {
	if existing, found, err := m.Store.Find(ctx, run); err != nil {
		return Workspace{}, fmt.Errorf("find workspace: %w", err)
	} else if found && existing.State == StateReady {
		return existing, nil
	}

	base, err := m.Git.DefaultBranchCommit(ctx, m.Repo, m.DefaultBranch)
	if err != nil {
		return Workspace{}, fmt.Errorf("resolve %s: %w", m.DefaultBranch, err)
	}
	branch := branchName(ticket, run)
	w := Workspace{
		Run: run, Ticket: ticket,
		Path:   filepath.Join(m.Root, sanitise(ticket)+"-"+branchSuffix(branch)),
		Branch: branch, Base: base, State: StatePending,
	}
	if err := m.Store.Upsert(ctx, w); err != nil {
		return Workspace{}, fmt.Errorf("record pending workspace: %w", err)
	}
	if err := m.Git.AddWorktree(ctx, m.Repo, w.Path, w.Branch, w.Base); err != nil {
		return Workspace{}, fmt.Errorf("create worktree: %w", err)
	}
	w.State = StateReady
	if err := m.Store.Upsert(ctx, w); err != nil {
		return Workspace{}, fmt.Errorf("record ready workspace: %w", err)
	}
	return w, nil
}

func branchSuffix(branch string) string {
	parts := strings.Split(branch, "/")
	return parts[len(parts)-1]
}

// Disposal lands with the orchestrator, which is what ends a run.
//
// It is deliberately absent rather than written ahead: kriya's lint gate runs
// deadcode with no allowlist, so a method whose only caller does not exist yet
// is unreachable production code and fails the gate. The behaviour it must
// have is already specified — cleanup can NEVER destroy unmerged work, and a
// refusal records its reason on the workspace row leaving the run's terminal
// state untouched — and REQ-workspaces pins it.

// Recover finishes workspaces a crash left mid-creation.
//
// Recovery stage 5. A row stamped pending has a worktree that may or may not
// exist — the row is written first precisely so this is answerable — and
// AddWorktree is idempotent enough to retry: git refuses a path that already
// holds one, which is the same outcome as having created it.
func (m Manager) Recover(ctx context.Context) (int, error) {
	pending, err := m.Store.Pending(ctx)
	if err != nil {
		return 0, fmt.Errorf("list pending workspaces: %w", err)
	}
	for _, w := range pending {
		if _, err := m.Ensure(ctx, w.Run, w.Ticket); err != nil {
			return 0, fmt.Errorf("recover workspace %s: %w", w.Run, err)
		}
	}
	return len(pending), nil
}
