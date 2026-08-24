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

// topicBranch cuts a branch with one extra commit and returns (base, topic).
func topicBranch(t *testing.T, dir, name, file string) (string, string) {
	t.Helper()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	base := head(t, dir)
	run("checkout", "-q", "-b", name, base)
	if err := os.WriteFile(filepath.Join(dir, file), []byte(name+"\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	run("add", "-A")
	run("commit", "-qm", name)
	topic := head(t, dir)
	run("checkout", "-q", "main")
	return base, topic
}

func TestACleanMergeIsReportedAsSuch(t *testing.T) {
	dir := tempRepo(t)
	base, topic := topicBranch(t, dir, "topic", "new.txt")
	clean, err := workspace.ShellGit{}.Preflight(context.Background(), dir, topic, base)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if !clean {
		t.Error("a merge that applies cleanly was reported as conflicting")
	}
}

func TestAConflictIsAnAnswerNotAnError(t *testing.T) {
	// git exits non-zero to report conflicts. Treating that as a failure would
	// abort the queue on the ordinary case it exists to detect.
	dir := tempRepo(t)
	base, first := topicBranch(t, dir, "first", "same.txt")
	_, second := topicBranch(t, dir, "second", "same.txt")

	clean, err := workspace.ShellGit{}.Preflight(context.Background(), dir, second, first)
	if err != nil {
		t.Fatalf("preflight returned an error for a conflict: %v", err)
	}
	if clean {
		t.Error("two branches editing one file the same way reported as clean")
	}
	_ = base
}

func TestPreflightPublishesNothing(t *testing.T) {
	// It must be safe to run BEFORE consuming an approval, which cannot be
	// given back.
	dir := tempRepo(t)
	base, topic := topicBranch(t, dir, "topic", "new.txt")
	before := head(t, dir)
	if _, err := (workspace.ShellGit{}).Preflight(context.Background(), dir, topic, base); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if head(t, dir) != before {
		t.Error("preflight moved the branch")
	}
	if _, err := os.Stat(filepath.Join(dir, "new.txt")); err == nil {
		t.Error("preflight wrote into the working tree")
	}
}

func TestAMergeLandsThePinnedCommit(t *testing.T) {
	dir := tempRepo(t)
	base, topic := topicBranch(t, dir, "topic", "new.txt")
	merged, err := workspace.ShellGit{}.MergeCAS(context.Background(), dir, "main", topic, base)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if merged == base || merged == topic {
		t.Errorf("the merge commit is %s", merged)
	}
	if head(t, dir) != merged {
		t.Errorf("main is at %s, want the merge commit %s", head(t, dir), merged)
	}
	// Both parents, so the merge really carries the topic's history.
	cmd := exec.Command("git", "rev-list", "--parents", "-n", "1", merged)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("rev-list: %v", err)
	}
	for _, want := range []string{base, topic} {
		if !strings.Contains(string(out), want) {
			t.Errorf("the merge commit does not have %s as a parent: %s", want, out)
		}
	}
}

func TestAMovedBaseRefusesTheMerge(t *testing.T) {
	// Between the preflight and the push another merge may have landed, and
	// merging anyway would put work on a base no gate chain ran against.
	dir := tempRepo(t)
	base, topic := topicBranch(t, dir, "topic", "new.txt")
	// Something else lands on main first.
	_, other := topicBranch(t, dir, "other", "other.txt")
	if _, err := (workspace.ShellGit{}).MergeCAS(context.Background(), dir, "main", other, base); err != nil {
		t.Fatalf("first merge: %v", err)
	}
	moved := head(t, dir)

	_, err := workspace.ShellGit{}.MergeCAS(context.Background(), dir, "main", topic, base)
	if err == nil {
		t.Fatal("a merge against a stale base was allowed")
	}
	if head(t, dir) != moved {
		t.Error("the refused merge moved the branch anyway")
	}
}

func TestAConflictingMergeIsRefused(t *testing.T) {
	dir := tempRepo(t)
	base, first := topicBranch(t, dir, "first", "same.txt")
	_, second := topicBranch(t, dir, "second", "same.txt")
	if _, err := (workspace.ShellGit{}).MergeCAS(context.Background(), dir, "first", second, first); err == nil {
		t.Fatal("a conflicting merge produced a commit")
	}
	_ = base
}

func TestARealIntegrationBringsTheOtherBranchesWorkIn(t *testing.T) {
	// A real merge in the worktree, not a rebase: the branch's commits are
	// already under review, and rewriting them would invalidate every review
	// that named one.
	dir := tempRepo(t)
	base, other := topicBranch(t, dir, "other", "theirs.txt")
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("checkout", "-q", "-b", "mine", base)
	if err := os.WriteFile(filepath.Join(dir, "mine.txt"), []byte("mine\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	run("add", "-A")
	run("commit", "-qm", "mine")
	mine := head(t, dir)

	if err := (workspace.ShellGit{}).Integrate(context.Background(), dir, dir, other); err != nil {
		t.Fatalf("integrate: %v", err)
	}
	if head(t, dir) == mine {
		t.Error("the branch did not move")
	}
	// Both files present: the other branch's work really came in, and this
	// branch's own commit is still an ancestor.
	for _, name := range []string{"mine.txt", "theirs.txt"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s is missing after the integration", name)
		}
	}
	cmd := exec.Command("git", "merge-base", "--is-ancestor", mine, "HEAD")
	cmd.Dir = dir
	if err := cmd.Run(); err != nil {
		t.Error("the branch's own commit is no longer an ancestor — it was rewritten")
	}
}

func TestAConflictingIntegrationCarriesGitsReport(t *testing.T) {
	dir := tempRepo(t)
	base, other := topicBranch(t, dir, "other", "same.txt")
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("checkout", "-q", "-b", "mine", base)
	if err := os.WriteFile(filepath.Join(dir, "same.txt"), []byte("mine\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	run("add", "-A")
	run("commit", "-qm", "mine")

	err := workspace.ShellGit{}.Integrate(context.Background(), dir, dir, other)
	if err == nil {
		t.Fatal("a conflicting integration reported success")
	}
	if !strings.Contains(err.Error(), "human or an agent") {
		t.Errorf("the error does not say who resolves it: %v", err)
	}
}

func TestReachableDistinguishesLandedFromNotLanded(t *testing.T) {
	// Against real git, because the answer IS the exit status: 0 yes, 1 no,
	// anything else a real failure. A "no" read as an error would strand a
	// merge; an error read as a "no" would merge the same work twice.
	dir := tempRepo(t)
	root := head(t, dir)
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("checkout", "-q", "-b", "work")
	git("commit", "-q", "--allow-empty", "-m", "work")
	work := head(t, dir)
	git("checkout", "-q", "main")

	sg := workspace.ShellGit{}
	// The root is on main; the work commit is not.
	if got, err := sg.Reachable(context.Background(), dir, root, "main"); err != nil || !got {
		t.Errorf("the root read as unreachable from main: %v %v", got, err)
	}
	if got, err := sg.Reachable(context.Background(), dir, work, "main"); err != nil || got {
		t.Errorf("unmerged work read as landed: %v %v", got, err)
	}
	// And once it lands, it reads as landed.
	git("merge", "-q", "--no-edit", "work")
	if got, err := sg.Reachable(context.Background(), dir, work, "main"); err != nil || !got {
		t.Errorf("merged work read as unlanded: %v %v", got, err)
	}
}

func TestReachableReportsAFailureRatherThanANo(t *testing.T) {
	// A commit git cannot resolve exits nonzero with a code that is NOT 1.
	// Reading that as "has not landed" would merge work a second time.
	dir := tempRepo(t)
	_, err := workspace.ShellGit{}.Reachable(context.Background(), dir,
		"0000000000000000000000000000000000000000", "main")
	if err == nil {
		t.Error("an unresolvable commit read as a clean 'not landed'")
	}
}
