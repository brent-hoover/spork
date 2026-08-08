package review

import (
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
