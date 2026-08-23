package main

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"kriya/internal/agent"
	"kriya/internal/clock"
	"kriya/internal/devloop"
	"kriya/internal/fakes"
	"kriya/internal/orchestrator"
	"kriya/internal/planner"
	"kriya/internal/reviewbridge"
	"kriya/internal/specverify"
	"kriya/internal/workspace"
)

// testGit stands in for git so the composition root can be exercised without
// cutting branches anywhere.
type testGit struct{ base string }

func (g testGit) DefaultBranchCommit(context.Context, string, string) (string, error) {
	if g.base == "" {
		return "base-sha", nil
	}
	return g.base, nil
}

// AddWorktree creates the directory, because the gate runner chdirs into it.
// A fake that recorded the call and made nothing would let the gate stage pass
// tests it could never pass in production.
func (testGit) AddWorktree(_ context.Context, _, path, _, _ string) error {
	return os.MkdirAll(path, 0o750)
}
func (testGit) RemoveWorktree(context.Context, string, string) error { return nil }
func (testGit) HasUnmergedCommits(context.Context, string, string, string) (bool, error) {
	return false, nil
}

func workspaceManagerForTest(t *testing.T) workspace.Manager {
	t.Helper()
	db := openTemp(t)
	if err := applyMigrations(context.Background(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return workspaceManagerOn(t, db)
}

func workspaceManagerOn(t *testing.T, db *sql.DB) workspace.Manager {
	t.Helper()
	return workspace.Manager{
		Repo: t.TempDir(), Root: t.TempDir(), DefaultBranch: "main",
		Store: workspace.SQLStore{DB: db}, Git: testGit{}, Now: clock.System{},
	}
}

func tiersForTest(t *testing.T) agent.Tiers {
	t.Helper()
	return agent.Tiers{
		Roles:  map[agent.Role]string{agent.RolePM: "heavy", agent.RoleDev: "heavy"},
		Models: map[string]string{"heavy": "a-model"},
	}
}

func bridgeForTest(t *testing.T) reviewbridge.Bridge {
	t.Helper()
	return bridgeWithStore(openTemp(t))
}

func bridgeWithStore(db *sql.DB) reviewbridge.Bridge {
	return reviewbridge.Bridge{
		Repo: "/repo", Store: reviewbridge.SQLStore{DB: db},
		Rev: reviewbridge.CLI{}, Now: clock.System{},
	}
}

func intakerForTest(db *sql.DB) planner.Intaker {
	return planner.Intaker{
		Verify:    fakes.NewVerifier("/target", specverify.Report{OK: true}),
		Snapshots: planner.SQLSnapshots{DB: db},
		Targets:   planner.SQLTargets{DB: db},
		Attempts:  planner.SQLAttempts{DB: db},
		Now:       clock.System{},
	}
}

// loopForTest is the recovery-shaped loop: a session store and nothing else
// that running work would need.
func loopForTest(db *sql.DB) devloop.Loop {
	return devloop.Loop{Store: devloop.SQLStore{DB: db}, Now: clock.System{}}
}

// submitterForTest opens reviews against a recording double.
func submitterForTest(db *sql.DB) orchestrator.Submitter {
	return orchestrator.Submitter{
		Store:   orchestrator.SQLStore{DB: db},
		Reviews: recordingReviews{},
		Author:  "actor-1",
	}
}

// queueForTest merges against a repository this test owns.
func queueForTest(t *testing.T, db *sql.DB) orchestrator.Queue {
	t.Helper()
	return orchestrator.Queue{
		Store:     orchestrator.SQLAttempts{DB: db},
		Runs:      orchestrator.SQLStore{DB: db},
		Approvals: noApprovals{},
		Git:       repoMerger{git: workspace.ShellGit{}, repo: t.TempDir(), branch: "main"},
		Actor:     "actor-1",
	}
}

// noApprovals consumes nothing; these tests never reach a merge.
type noApprovals struct{}

func (noApprovals) Consume(context.Context, string, string, int, string, string) error {
	return nil
}
