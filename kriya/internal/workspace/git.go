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

// mergeTree computes the merged tree in memory and reports whether it is
// conflict-free.
//
// `git merge-tree --write-tree` writes nothing to any ref and touches no
// working tree: it prints the resulting tree and exits non-zero on conflicts.
// That is what makes it safe to run BEFORE consuming an approval, which cannot
// be given back.
func (g ShellGit) mergeTree(ctx context.Context, repo, commit, base string) (string, bool, error) {
	cmd := exec.CommandContext(ctx, "git", "merge-tree", "--write-tree", base, commit)
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if ok := asExitError(err, &ee); ok {
			// A non-zero exit is git reporting conflicts, which is an ANSWER.
			// Only a failure to run at all is an error.
			return "", false, nil
		}
		return "", false, fmt.Errorf("git merge-tree: %w", err)
	}
	tree := strings.TrimSpace(string(out))
	if tree == "" {
		return "", false, fmt.Errorf("git merge-tree produced no tree for %s into %s", commit, base)
	}
	return tree, true, nil
}

// Preflight reports whether a commit merges cleanly into base, publishing
// nothing.
func (g ShellGit) Preflight(ctx context.Context, repo, commit, base string) (bool, error) {
	_, clean, err := g.mergeTree(ctx, repo, commit, base)
	return clean, err
}

// MergeCAS lands commit on branch, but only while branch still points at
// expectedBase.
//
// The compare-and-swap is the whole point: between the preflight and here
// another merge may have landed, and merging anyway would put work on a base
// no gate chain ever ran against. update-ref with an old-value IS the swap —
// git refuses when the ref has moved, which is exactly the case this catches.
func (g ShellGit) MergeCAS(
	ctx context.Context, repo, branch, commit, expectedBase string,
) (string, error) {
	tree, clean, err := g.mergeTree(ctx, repo, commit, expectedBase)
	if err != nil {
		return "", err
	}
	if !clean {
		return "", fmt.Errorf("merge of %s into %s conflicts", commit, expectedBase)
	}
	merged, err := g.run(ctx, repo, "commit-tree", tree,
		"-p", expectedBase, "-p", commit, "-m", "Merge "+commit)
	if err != nil {
		return "", err
	}
	if _, err := g.run(ctx, repo, "update-ref",
		"refs/heads/"+branch, merged, expectedBase); err != nil {
		return "", fmt.Errorf("merge did not land: %w", err)
	}
	return merged, nil
}
