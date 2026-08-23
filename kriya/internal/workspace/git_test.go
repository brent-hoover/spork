package workspace_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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
