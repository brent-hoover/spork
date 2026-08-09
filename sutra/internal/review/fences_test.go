package review

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRevalidateFencesNothingPinned pins the one decision every caller
// delegates here: a submission that supplied no fence has nothing to
// recheck, so no git command runs at all. The repository is deliberately
// one that cannot be read — if the short-circuit is missing or reversed,
// resolveHead fails and a submission that made no claim about the
// repository is refused over it.
func TestRevalidateFencesNothingPinned(t *testing.T) {
	unreadable := Repo{Path: "/nonexistent/sutra-fence-repo", DefaultBranch: "main"}
	sha := strings.Repeat("a", 40)
	pinned := strings.Repeat("b", 40)

	t.Run("no fences consult no repository", func(t *testing.T) {
		if err := RevalidateFences(unreadable, sha, Fences{}); err != nil {
			t.Fatalf("unpinned submission rejected: %v", err)
		}
	})
	// The complements: either fence alone must reach git, or the
	// short-circuit above would be indistinguishable from skipping
	// revalidation entirely.
	t.Run("a head fence reaches the repository", func(t *testing.T) {
		if err := RevalidateFences(unreadable, sha, Fences{ExpectedDefaultHead: &pinned}); err == nil {
			t.Fatal("head fence accepted against an unreadable repository")
		}
	})
	t.Run("a base fence reaches the repository", func(t *testing.T) {
		if err := RevalidateFences(unreadable, sha, Fences{ExpectedBaseCommit: &pinned}); err == nil {
			t.Fatal("base fence accepted against an unreadable repository")
		}
	})
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commitFile writes content and commits it, returning the new sha.
func commitFile(t *testing.T, dir, content, message string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-m", message)
	return gitRun(t, dir, "rev-parse", "HEAD")
}

// liveRepo builds a one-commit repository on the default branch and
// returns it with its head sha.
func liveRepo(t *testing.T) (Repo, string) {
	t.Helper()
	dir := t.TempDir()
	gitRun(t, dir, "init", "-b", "main")
	return Repo{Path: dir, DefaultBranch: "main"}, commitFile(t, dir, "x\n", "one")
}

// TestDiffSizeLimitIsInclusive pins WHICH size is the first one refused.
//
// The only other test of this bound sets the limit to 16 bytes against a
// diff of hundreds, where the refusal happens the same way whether the
// comparison admits its own limit or not. A bound tested only from far
// away is a bound whose edge is unpinned: "reject over the limit" and
// "reject at the limit" agree everywhere except at one value.
//
// So the two legs sit one byte apart, on either side of the real diff's
// exact length — the limit that must still serve it, and the largest
// limit that must not.
func TestDiffSizeLimitIsInclusive(t *testing.T) {
	dir := t.TempDir()
	gitRun(t, dir, "init", "-b", "main")
	base := commitFile(t, dir, "one\n", "base")
	head := commitFile(t, dir, "two\n", "head")

	full, err := Diff(dir, base, head)
	if err != nil {
		t.Fatalf("unbounded diff: %v", err)
	}
	exact := int64(len(full))
	if exact == 0 {
		t.Fatal("empty diff cannot pin a size bound")
	}

	old := MaxDiffBytes
	t.Cleanup(func() { MaxDiffBytes = old })

	MaxDiffBytes = exact
	got, err := Diff(dir, base, head)
	if err != nil {
		t.Fatalf("a diff of exactly the limit must be served, got: %v", err)
	}
	if int64(len(got)) != exact {
		t.Fatalf("diff at the limit truncated: %d bytes, want %d", len(got), exact)
	}

	MaxDiffBytes = exact - 1
	if _, err := Diff(dir, base, head); err == nil {
		t.Fatalf("a diff one byte over the limit must be refused")
	}
}

// TestRevalidateFencesAgainstALiveRepository exercises the two fence
// comparisons themselves.
//
// TestRevalidateFencesNothingPinned above reaches this function only
// through a repository that cannot be read, where resolveHead fails and
// returns before either comparison runs. Every "rejected" it observes is
// really the repository being unreadable, so it holds for an
// implementation that abandons the fences the moment the repository
// answers — which is the one case fences exist for.
//
// A live repository separates them: the head resolves, so a refusal can
// only come from the pinned value disagreeing with what git reports, and
// an acceptance can only come from them agreeing.
func TestRevalidateFencesAgainstALiveRepository(t *testing.T) {
	repo, head := liveRepo(t)
	stale := strings.Repeat("c", 40)

	t.Run("matching head fence is accepted", func(t *testing.T) {
		if err := RevalidateFences(repo, head, Fences{ExpectedDefaultHead: &head}); err != nil {
			t.Fatalf("head fence naming the live head rejected: %v", err)
		}
	})
	t.Run("moved head fence conflicts", func(t *testing.T) {
		if err := RevalidateFences(repo, head, Fences{ExpectedDefaultHead: &stale}); err == nil {
			t.Fatal("head fence naming a commit the branch does not point at was accepted")
		}
	})
	t.Run("matching base fence is accepted", func(t *testing.T) {
		if err := RevalidateFences(repo, head, Fences{ExpectedBaseCommit: &head}); err != nil {
			t.Fatalf("base fence naming the real merge base rejected: %v", err)
		}
	})
	t.Run("moved base fence conflicts", func(t *testing.T) {
		if err := RevalidateFences(repo, head, Fences{ExpectedBaseCommit: &stale}); err == nil {
			t.Fatal("base fence naming a commit that is not the merge base was accepted")
		}
	})
}
