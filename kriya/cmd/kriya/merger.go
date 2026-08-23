package main

import (
	"context"

	"kriya/internal/workspace"
)

// repoMerger binds the git seam to one repository and default branch.
//
// The merge queue asks "can this land, and land it against exactly this base";
// which repository and which branch that means is the composition root's to
// know, not the queue's.
type repoMerger struct {
	git    workspace.ShellGit
	repo   string
	branch string
}

func (m repoMerger) Preflight(ctx context.Context, commit, base string) (bool, error) {
	return m.git.Preflight(ctx, m.repo, commit, base)
}

func (m repoMerger) Head(ctx context.Context) (string, error) {
	return m.git.DefaultBranchCommit(ctx, m.repo, m.branch)
}

func (m repoMerger) Merge(ctx context.Context, commit, expectedBase string) (string, error) {
	return m.git.MergeCAS(ctx, m.repo, m.branch, commit, expectedBase)
}
