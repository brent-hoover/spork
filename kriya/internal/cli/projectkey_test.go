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
