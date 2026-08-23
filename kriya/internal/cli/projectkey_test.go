package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestTheKeyIsReadableAtAGlance(t *testing.T) {
	got := projectKey("/home/brent/Projects/kriya")
	if !strings.HasPrefix(got, "KRIYA") {
		t.Errorf("key %q does not start from the directory name", got)
	}
}

func TestDirectoriesThatSanitiseAlikeStillDiffer(t *testing.T) {
	// sutra enforces unique project keys, so a collision is not cosmetic: the
	// second target fails permanently with a 409 and there is no way forward.
	a := projectKey("/repos/foo-bar")
	b := projectKey("/repos/foobar")
	if a == b {
		t.Fatalf("foo-bar and foobar both key to %q", a)
	}
}

func TestALongNameKeepsRoomForItsDigest(t *testing.T) {
	got := projectKey("/repos/a-very-long-project-name-indeed")
	if len(got) != 14 {
		t.Errorf("key %q is %d characters; the digest needs its six", got, len(got))
	}
	other := projectKey("/elsewhere/a-very-long-project-name-indeed")
	if got == other {
		t.Error("two paths sharing a long basename collided on their prefix")
	}
}

func TestTwoPathsSharingABasenameStillDiffer(t *testing.T) {
	if projectKey("/a/svc") == projectKey("/b/svc") {
		t.Error("the digest covers only the basename, not the full path")
	}
}

func TestANameWithNothingUsableStillKeys(t *testing.T) {
	// A directory named entirely in punctuation sanitises to nothing, and an
	// empty key is one sutra would reject.
	got := projectKey(filepath.Join(t.TempDir(), "..."))
	if !strings.HasPrefix(got, "TARGET") {
		t.Errorf("key %q has no usable stem", got)
	}
}

func TestEveryUsableCharacterSurvives(t *testing.T) {
	// The boundaries are the whole rule: a range that excluded 'A', 'Z', '0'
	// or '9' would silently drop characters and make two distinct directories
	// key alike more often.
	if got := projectKey("/repos/AZ09"); !strings.HasPrefix(got, "AZ09") {
		t.Errorf("key %q dropped a character from AZ09", got)
	}
	if got := projectKey("/repos/az09"); !strings.HasPrefix(got, "AZ09") {
		t.Errorf("key %q did not fold lowercase up", got)
	}
}

func TestEightCharactersAreKeptWhole(t *testing.T) {
	if got := projectKey("/repos/ABCDEFGH"); !strings.HasPrefix(got, "ABCDEFGH") {
		t.Errorf("key %q truncated a name that fits", got)
	}
	if got := projectKey("/repos/ABCDEFGHI"); !strings.HasPrefix(got, "ABCDEFGH") {
		t.Errorf("key %q did not truncate a name that does not fit", got)
	}
}
