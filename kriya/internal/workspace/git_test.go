package workspace_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"kriya/internal/workspace"
)

// tempRepo builds a real repository with one commit.
//
// Real git, not a fake: ShellGit's whole job is what git actually does, and a
// fake git would only prove the test agrees with itself.
func tempRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "kriya@example.test"},
		{"config", "user.name", "kriya"},
		{"commit", "-q", "--allow-empty", "-m", "root"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

func head(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	return string(out[:len(out)-1])
}

func TestCommitCapturesTheAgentsWork(t *testing.T) {
	dir := tempRepo(t)
	before := head(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "new.go"), []byte("package p\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	sha, err := workspace.ShellGit{}.Commit(context.Background(), dir, "round 1")
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if sha == before {
		t.Error("a dirty tree produced no new commit")
	}
	if sha != head(t, dir) {
		t.Errorf("returned %s but HEAD is %s", sha, head(t, dir))
	}
}

func TestACleanTreeReturnsHeadRatherThanFailing(t *testing.T) {
	// An agent that changed nothing is a stall the round bound catches, not a
	// crash in the committer.
	dir := tempRepo(t)
	sha, err := workspace.ShellGit{}.Commit(context.Background(), dir, "round 2")
	if err != nil {
		t.Fatalf("a clean tree must not be an error: %v", err)
	}
	if sha != head(t, dir) {
		t.Errorf("returned %s, want HEAD %s", sha, head(t, dir))
	}
}

func TestCommitOutsideARepositoryFails(t *testing.T) {
	if _, err := (workspace.ShellGit{}).Commit(context.Background(), t.TempDir(), "x"); err == nil {
		t.Fatal("committing outside a repository must fail loudly")
	}
}

func TestUnmergedCommitsAreDetected(t *testing.T) {
	// The whole point of the check is refusing to delete work, so it has to
	// answer against real git history.
	dir := tempRepo(t)
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("checkout", "-q", "-b", "topic")
	run("commit", "-q", "--allow-empty", "-m", "work")

	g := workspace.ShellGit{}
	unmerged, err := g.HasUnmergedCommits(context.Background(), dir, "topic", "main")
	if err != nil {
		t.Fatalf("has unmerged: %v", err)
	}
	if !unmerged {
		t.Error("a branch ahead of main reported as safe to delete")
	}

	unmerged, err = g.HasUnmergedCommits(context.Background(), dir, "main", "topic")
	if err != nil {
		t.Fatalf("has unmerged: %v", err)
	}
	if unmerged {
		t.Error("a branch behind its base reported as holding work")
	}
}

func TestAnUnanswerableQuestionIsNotAnAnswer(t *testing.T) {
	// The safe answer when the question cannot be answered is "do not delete",
	// and the caller must see the failure rather than a false.
	dir := tempRepo(t)
	if _, err := (workspace.ShellGit{}).HasUnmergedCommits(
		context.Background(), dir, "no-such-branch", "main"); err == nil {
		t.Fatal("an unknown branch answered the question")
	}
}

func TestTheDefaultBranchCommitIsResolved(t *testing.T) {
	dir := tempRepo(t)
	got, err := workspace.ShellGit{}.DefaultBranchCommit(context.Background(), dir, "main")
	if err != nil {
		t.Fatalf("resolve main: %v", err)
	}
	if got != head(t, dir) {
		t.Errorf("resolved %s, want %s", got, head(t, dir))
	}
}

func TestGitFailuresCarryGitsOwnComplaint(t *testing.T) {
	_, err := workspace.ShellGit{}.DefaultBranchCommit(context.Background(), tempRepo(t), "absent")
	if err == nil {
		t.Fatal("resolving a branch that does not exist must fail")
	}
	if !strings.Contains(err.Error(), "rev-parse") {
		t.Errorf("error %q does not say what was attempted", err)
	}
}

func TestWorktreesAreCreatedAndRemoved(t *testing.T) {
	repo := tempRepo(t)
	path := filepath.Join(t.TempDir(), "wt")
	g := workspace.ShellGit{}
	if err := g.AddWorktree(context.Background(), repo, path, "topic", "main"); err != nil {
		t.Fatalf("add worktree: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("worktree was not created: %v", err)
	}
	if err := g.RemoveWorktree(context.Background(), repo, path); err != nil {
		t.Fatalf("remove worktree: %v", err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("the worktree survived removal")
	}
}

func TestADirtyWorktreeIsNotRemoved(t *testing.T) {
	// git refuses while it is dirty, and that refusal is what the caller
	// records rather than overrides.
	repo := tempRepo(t)
	path := filepath.Join(t.TempDir(), "wt")
	g := workspace.ShellGit{}
	if err := g.AddWorktree(context.Background(), repo, path, "topic", "main"); err != nil {
		t.Fatalf("add worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "dirty.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := g.RemoveWorktree(context.Background(), repo, path); err == nil {
		t.Fatal("a worktree holding uncommitted work was removed")
	}
}
