package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestInitAnnouncesNothingWhenTheMarkerCannotBeWritten pins the other
// half of AC-project-init. The report is written after the marker on
// purpose, and only a marker write that FAILS can tell the two orders
// apart — the acceptance scenario sees a successful init, where a report
// printed first and a report printed last look identical.
//
// The project itself is already created by then; the CLI cannot take that
// back. What it can do is not claim the directory was anchored when it
// was not, so the caller learns the truth from the exit code and stderr
// instead of from a success line that was never earned.
func TestInitAnnouncesNothingWhenTheMarkerCannotBeWritten(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"019fe000-0000-7000-8000-000000000001"}`))
	}))
	defer srv.Close()

	// A regular file where a directory is expected: joining the marker
	// name onto it cannot resolve, so the write fails without needing
	// permission games that root would defeat.
	notADir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var out bytes.Buffer
	c := &client{
		base:  srv.URL,
		env:   Env{Workdir: notADir, Stdout: &out, Getenv: func(string) string { return "" }},
		flags: map[string]string{"key": "SUT", "name": "Sutra", "actor": "operator"},
	}
	if err := c.initProject(); err == nil {
		t.Fatal("init reported success with no marker written")
	}
	if out.Len() != 0 {
		t.Errorf("init announced %q after failing to write the marker", out.String())
	}
}
