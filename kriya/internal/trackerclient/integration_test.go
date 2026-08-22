package trackerclient_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"kriya/internal/trackerclient"
)

// startSutra builds and runs a real sutra, returning its base URL.
//
// The integration ring drives the real tracker, not a fake: sutra's fences —
// idempotent replay, atomic pop, verdict-ABA — exist specifically for kriya,
// and a fake would prove kriya against a restatement of kriya's own
// assumptions rather than against the contract.
func startSutra(t *testing.T) string {
	t.Helper()
	root := repoRoot(t)
	bin := filepath.Join(t.TempDir(), "sutra")
	build := exec.Command("go", "build", "-o", bin, "./cmd/sutra")
	build.Dir = filepath.Join(root, "sutra")
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("cannot build sutra: %v\n%s", err, out)
	}

	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	cmd := exec.Command(bin, "-addr", addr, "-db", filepath.Join(t.TempDir(), "sutra.db"))
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sutra: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	base := "http://" + addr
	// A context deadline rather than time.Now: the clock ban applies to tests
	// too, and it should — reaching for wall time here is the same habit that
	// makes domain code untestable. This wants a deadline anyway.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/projects", nil)
		if err != nil {
			t.Fatalf("build readiness request: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			return base
		}
		select {
		case <-ctx.Done():
			t.Fatalf("sutra did not become ready at %s", base)
		case <-ticker.C:
		}
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for range 6 {
		if _, err := os.Stat(filepath.Join(dir, "sutra", "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Skip("sutra/ not found above the working directory")
	return ""
}

func TestAgainstRealSutra(t *testing.T) {
	ctx := context.Background()
	c := trackerclient.New(startSutra(t))

	actor, err := c.CreateIdentity(ctx, "kriya-pm", "agent", "Kriya PM", "id-1")
	if err != nil {
		t.Fatalf("create identity: %v", err)
	}
	if actor.ID == "" {
		t.Fatal("identity has no id")
	}

	project, err := c.CreateProject(ctx, "SHORT", "shorty", actor.ID, "proj-1")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	epic, err := c.CreateIssue(ctx, project.ID, "Build shorty", "umbrella", actor.ID, "epic-1")
	if err != nil {
		t.Fatalf("create epic: %v", err)
	}
	ticket, err := c.CreateIssue(ctx, project.ID, "Create a short link", "", actor.ID, "tkt-1")
	if err != nil {
		t.Fatalf("create ticket: %v", err)
	}

	if err := c.AddRelation(ctx, epic.ID, "parent_of", ticket.ID, actor.ID, "rel-1"); err != nil {
		t.Fatalf("parent the ticket under the epic: %v", err)
	}

	t.Run("a replayed mutation returns the original rather than acting twice", func(t *testing.T) {
		// The property kriya's whole write-ahead discipline depends on: a
		// crash between sending and recording is safe to replay.
		again, err := c.CreateIssue(ctx, project.ID, "Create a short link", "", actor.ID, "tkt-1")
		if err != nil {
			t.Fatalf("replay: %v", err)
		}
		if again.ID != ticket.ID {
			t.Errorf("replay created a second issue: %s then %s", ticket.ID, again.ID)
		}
	})

	t.Run("a non-2xx carries its status", func(t *testing.T) {
		_, err := c.CreateProject(ctx, "SHORT", "duplicate", actor.ID, "proj-2")
		if err == nil {
			t.Fatal("a duplicate project key must fail")
		}
		var apiErr *trackerclient.APIError
		if !asAPIError(err, &apiErr) {
			t.Fatalf("expected an APIError, got %T: %v", err, err)
		}
		if apiErr.Status != http.StatusConflict {
			t.Errorf("expected 409 so a replay is distinguishable from a sick tracker, got %d", apiErr.Status)
		}
	})
}
