package reviewbridge_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kriya/internal/reviewbridge"
)

// stubRoborev writes an executable standing in for roborev. The script
// dispatches on $1, which is exactly the subcommand the CLI is supposed to
// send — a stub that ignored it would pass whatever kriya invoked.
func stubRoborev(t *testing.T, script string) reviewbridge.CLI {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "roborev")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return reviewbridge.CLI{Bin: bin}
}

func TestEnqueueReadsTheJobIDRoborevPrints(t *testing.T) {
	c := stubRoborev(t, `case "$1" in review) echo "queued job #47 for $2";; *) exit 9;; esac`)
	id, err := c.Enqueue(context.Background(), t.TempDir(), "abc123")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if id != 47 {
		t.Errorf("read job %d, want 47", id)
	}
}

func TestEnqueueWithoutAJobIDFails(t *testing.T) {
	// No id means no way to act on the job without adopting one by SHA, which
	// R1 forbids.
	c := stubRoborev(t, `echo "submitted"`)
	if _, err := c.Enqueue(context.Background(), t.TempDir(), "abc123"); err == nil {
		t.Fatal("an enqueue with no id must not read as success")
	}
}

func TestEnqueueSurfacesRoborevsOwnComplaint(t *testing.T) {
	c := stubRoborev(t, `echo "not a git repository" >&2; echo "not a git repository"; exit 1`)
	_, err := c.Enqueue(context.Background(), t.TempDir(), "abc123")
	if err == nil || !strings.Contains(err.Error(), "not a git repository") {
		t.Fatalf("got %v, want roborev's own message", err)
	}
}

func TestStatusReportsAJobStillRunning(t *testing.T) {
	c := stubRoborev(t, `case "$1" in list) echo '[{"id":47,"status":"running"}]';; *) exit 9;; esac`)
	status, report, err := c.Status(context.Background(), t.TempDir(), 47)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status != "running" || report != "" {
		t.Errorf("got %q with report %q", status, report)
	}
}

func TestStatusFetchesTheReportOnceDone(t *testing.T) {
	c := stubRoborev(t, `case "$1" in
  list) echo '[{"id":12,"status":"done"},{"id":47,"status":"done"}]';;
  show) echo "- **Severity**: Medium";;
  *) exit 9;;
esac`)
	status, report, err := c.Status(context.Background(), t.TempDir(), 47)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status != "done" || !strings.Contains(report, "Severity") {
		t.Errorf("got %q with report %q", status, report)
	}
}

func TestStatusRefusesAJobRoborevDoesNotKnow(t *testing.T) {
	// Silence here would let a lost job read as a clean review.
	c := stubRoborev(t, `case "$1" in list) echo '[{"id":1,"status":"done"}]';; *) exit 9;; esac`)
	if _, _, err := c.Status(context.Background(), t.TempDir(), 47); err == nil {
		t.Fatal("an unknown job must not read as any status")
	}
}

func TestStatusFailsWhenTheListingCannotBeParsed(t *testing.T) {
	c := stubRoborev(t, `case "$1" in list) echo 'not json';; *) exit 9;; esac`)
	if _, _, err := c.Status(context.Background(), t.TempDir(), 47); err == nil {
		t.Fatal("an unparseable listing was accepted")
	}
}

func TestStatusFailsWhenTheListingCannotBeRead(t *testing.T) {
	c := stubRoborev(t, `exit 4`)
	if _, _, err := c.Status(context.Background(), t.TempDir(), 47); err == nil {
		t.Fatal("a roborev that would not list was treated as having nothing")
	}
}

func TestStatusFailsWhenTheReportCannotBeFetched(t *testing.T) {
	c := stubRoborev(t, `case "$1" in
  list) echo '[{"id":47,"status":"done"}]';;
  show) exit 5;;
esac`)
	if _, _, err := c.Status(context.Background(), t.TempDir(), 47); err == nil {
		t.Fatal("a finished job whose report cannot be read must not read as clean")
	}
}

func TestCloseNamesTheJob(t *testing.T) {
	c := stubRoborev(t, `case "$1" in close) test "$2" = "47" || exit 1;; *) exit 9;; esac`)
	if err := c.Close(context.Background(), t.TempDir(), 47); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestCloseSurfacesAFailure(t *testing.T) {
	c := stubRoborev(t, `echo "no such job"; exit 1`)
	err := c.Close(context.Background(), t.TempDir(), 47)
	if err == nil || !strings.Contains(err.Error(), "no such job") {
		t.Fatalf("got %v, want roborev's own message", err)
	}
}

func TestTheDefaultBinaryIsRoborevOnPath(t *testing.T) {
	// Resolved from PATH, not an absolute path baked into the source: the tool
	// is installed wherever the operator installed it. PATH is redirected at a
	// stub so this test cannot enqueue a real review.
	dir := t.TempDir()
	stub := filepath.Join(dir, "roborev")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho 'queued job #5'\n"), 0o700); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("PATH", dir)
	id, err := reviewbridge.CLI{}.Enqueue(context.Background(), t.TempDir(), "abc123")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if id != 5 {
		t.Errorf("read job %d", id)
	}
}
