package specverify_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"kriya/internal/specverify"
)

// repoRoot walks up to the directory holding both avspec/ and kriya/.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for range 6 {
		if _, err := os.Stat(filepath.Join(dir, "avspec", "pyproject.toml")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Skip("avspec/ not found above the working directory")
	return ""
}

// realCLI is how this repo invokes avspec: under uv, from avspec/.
func realCLI(t *testing.T) specverify.CLI {
	t.Helper()
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skip("uv not on PATH; the integration ring needs it because avspec is Python")
	}
	return specverify.CLI{Argv: []string{"uv", "run", "avspec"}, WorkDir: filepath.Join(repoRoot(t), "avspec")}
}

// TestAgainstTheRealVerifier pins the contract to the actual tool rather than
// to a stub of it. The stub tests prove the exec and exit-code handling; this
// proves the payload shape and exit codes are what the stubs claim.
func TestAgainstTheRealVerifier(t *testing.T) {
	c := realCLI(t)
	root := repoRoot(t)

	t.Run("a ready spec", func(t *testing.T) {
		r, err := c.Verify(context.Background(), filepath.Join(root, "kriya"))
		if err != nil {
			t.Fatalf("verify kriya: %v", err)
		}
		if r.Status != "ready" || !r.OK {
			t.Errorf("kriya's own spec: status=%q ok=%v, want ready/true", r.Status, r.OK)
		}
		if r.Counts.Error != 0 || r.Counts.Todo != 0 {
			t.Errorf("expected no blocking findings, got %+v", r.Counts)
		}
	})

	t.Run("a directory with no manifest exits 1 and still reports", func(t *testing.T) {
		r, err := c.Verify(context.Background(), t.TempDir())
		if err != nil {
			t.Fatalf("a refusal must be a report, not an error: %v", err)
		}
		if r.OK {
			t.Error("OK should be false with no manifest")
		}
		if len(r.Findings) == 0 {
			t.Error("expected at least one finding explaining the refusal")
		}
	})

	t.Run("a draft spec is OK and exits zero", func(t *testing.T) {
		dir := t.TempDir()
		manifest := "avspec: \"0.3\"\nproject:\n  name: draft-probe\n  status: draft\n"
		if err := os.WriteFile(filepath.Join(dir, "avspec.yaml"), []byte(manifest), 0o644); err != nil {
			t.Fatalf("write manifest: %v", err)
		}
		r, err := c.Verify(context.Background(), dir)
		if err != nil {
			t.Fatalf("verify draft: %v", err)
		}
		// The trap, confirmed against the real tool: exit 0, OK true, and only
		// Status and Counts reveal that intake must refuse it.
		if !r.OK || r.Status != "draft" {
			t.Errorf("got status=%q ok=%v, want draft/true", r.Status, r.OK)
		}
		if r.Counts.Todo == 0 {
			t.Error("a draft spec should carry todo findings")
		}
	})
}
