package workspace

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// ShellGit runs git as a subprocess.
//
// Shelling out rather than a library: it matches the gate runner's shape, the
// worktree commands kriya needs are ones go-git does not fully support, and
// what recovery can observe is exactly what git itself reports.
type ShellGit struct{}

func (ShellGit) run(ctx context.Context, repo string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if ok := asExitError(err, &ee); ok {
			return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// DefaultBranchCommit resolves the commit a new branch is cut from.
func (g ShellGit) DefaultBranchCommit(ctx context.Context, repo, branch string) (string, error) {
	return g.run(ctx, repo, "rev-parse", branch)
}

// AddWorktree creates a worktree at path on a new branch cut from base.
func (g ShellGit) AddWorktree(ctx context.Context, repo, path, branch, base string) error {
	_, err := g.run(ctx, repo, "worktree", "add", "-b", branch, path, base)
	return err
}

// RemoveWorktree removes a worktree. git refuses while it is dirty, which is
// the refusal the caller records rather than overrides.
func (g ShellGit) RemoveWorktree(ctx context.Context, repo, path string) error {
	_, err := g.run(ctx, repo, "worktree", "remove", path)
	return err
}

// HasUnmergedCommits reports whether branch holds commits absent from base.
func (g ShellGit) HasUnmergedCommits(ctx context.Context, repo, branch, base string) (bool, error) {
	out, err := g.run(ctx, repo, "rev-list", "--count", base+".."+branch)
	if err != nil {
		return false, err
	}
	// Any count but zero means work would be lost. Parsing failure is treated
	// as unmerged: the safe answer when the question cannot be answered is
	// "do not delete".
	return out != "0", nil
}

// Commit turns everything in dir into one commit and returns its sha.
//
// dir is a worktree, not the repository root: the dev loop commits where the
// agent worked. A clean tree returns the current HEAD rather than an error —
// an agent that changed nothing is a review round that finds the same thing
// again, which the loop's round bound catches, not a crash.
func (g ShellGit) Commit(ctx context.Context, dir, message string) (string, error) {
	dirty, err := g.run(ctx, dir, "status", "--porcelain")
	if err != nil {
		return "", err
	}
	if dirty != "" {
		if _, err := g.run(ctx, dir, "add", "-A"); err != nil {
			return "", err
		}
		if _, err := g.run(ctx, dir, "commit", "-m", message); err != nil {
			return "", err
		}
	}
	return g.run(ctx, dir, "rev-parse", "HEAD")
}
