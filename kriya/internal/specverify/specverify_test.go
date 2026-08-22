package specverify_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"kriya/internal/specverify"
)

// stub writes a fake avspec that prints body on stdout and exits with code.
// Exercising the real exec path is the point: the exit-code contract is the
// part most likely to be wrong, and a hand-built Report would not test it.
func stub(t *testing.T, body string, code int) specverify.CLI {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "avspec")
	script := "#!/bin/sh\ncat <<'JSON'\n" + body + "\nJSON\nexit " + strconv.Itoa(code) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return specverify.CLI{Argv: []string{path}}
}

const readyJSON = `{"status":"ready","ok":true,"counts":{"error":0,"todo":0,"warn":0},"findings":[]}`
const draftJSON = `{"status":"draft","ok":true,"counts":{"error":0,"todo":5,"warn":0},
	"findings":[{"code":"REQ_ABSENT","severity":"todo","message":"no requirements","ref":"project"}]}`
const refusedJSON = `{"status":"unknown","ok":false,"counts":{"error":1,"todo":0,"warn":0},
	"findings":[{"code":"MANIFEST_MISSING","severity":"error","message":"no avspec.yaml","ref":""}]}`

func TestAReadySpecIsReported(t *testing.T) {
	r, err := stub(t, readyJSON, 0).Verify(context.Background(), "any")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Status != "ready" || !r.OK {
		t.Errorf("got status=%q ok=%v, want ready/true", r.Status, r.OK)
	}
}

func TestARefusalIsAReportNotAnError(t *testing.T) {
	// exit 1 with a parseable payload is the ordinary decline path.
	r, err := stub(t, refusedJSON, 1).Verify(context.Background(), "any")
	if err != nil {
		t.Fatalf("exit 1 must yield a report, got error: %v", err)
	}
	if r.OK {
		t.Error("OK should be false")
	}
	if len(r.Findings) != 1 || r.Findings[0].Code != "MANIFEST_MISSING" {
		t.Errorf("findings not carried through: %+v", r.Findings)
	}
}

func TestADraftIsOKAndExitsZero(t *testing.T) {
	// The trap this seam exists to expose: a draft spec is OK=true and exits
	// 0, so neither signal distinguishes it from ready. Only Status and
	// Counts do, and AC-intake-refuse requires refusing it.
	r, err := stub(t, draftJSON, 0).Verify(context.Background(), "any")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !r.OK {
		t.Error("a draft spec reports OK=true; this seam must not editorialise")
	}
	if r.Status != "draft" || r.Counts.Todo != 5 {
		t.Errorf("got status=%q todo=%d, want draft/5", r.Status, r.Counts.Todo)
	}
}

func TestAnUnexpectedExitCodeIsAnError(t *testing.T) {
	_, err := stub(t, readyJSON, 2).Verify(context.Background(), "any")
	if err == nil {
		t.Fatal("exit 2 must be an error, not a report")
	}
	if !strings.Contains(err.Error(), "exited 2") {
		t.Errorf("error should name the exit code, got: %v", err)
	}
}

func TestUnparseableOutputIsAnError(t *testing.T) {
	_, err := stub(t, "not json at all", 0).Verify(context.Background(), "any")
	if err == nil {
		t.Fatal("unparseable output must be an error")
	}
}

func TestAMissingCommandIsAnError(t *testing.T) {
	c := specverify.CLI{Argv: []string{filepath.Join(t.TempDir(), "does-not-exist")}}
	if _, err := c.Verify(context.Background(), "any"); err == nil {
		t.Fatal("a command that cannot start must be an error")
	}
}

func TestNoCommandConfiguredIsAnError(t *testing.T) {
	if _, err := (specverify.CLI{}).Verify(context.Background(), "any"); err == nil {
		t.Fatal("an empty Argv must be an error")
	}
}

func TestAnIncompleteReportIsRejected(t *testing.T) {
	// Syntactically valid JSON that omits required fields must NOT be
	// admitted with Go zero values: a half-broken or version-mismatched
	// verifier would otherwise fail OPEN, reporting zero findings and zero
	// counts, which reads as a clean spec.
	for name, body := range map[string]string{
		"no counts":        `{"status":"ready","ok":true,"findings":[]}`,
		"no findings":      `{"status":"ready","ok":true,"counts":{"error":0,"todo":0,"warn":0}}`,
		"no ok":            `{"status":"ready","counts":{"error":0,"todo":0,"warn":0},"findings":[]}`,
		"no status":        `{"ok":true,"counts":{"error":0,"todo":0,"warn":0},"findings":[]}`,
		"empty object":     `{}`,
		"counts is empty":  `{"status":"ready","ok":true,"counts":{},"findings":[]}`,
		"counts half full": `{"status":"ready","ok":true,"counts":{"error":0},"findings":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := stub(t, body, 0).Verify(context.Background(), "any"); err == nil {
				t.Fatalf("an incomplete report must be an error, not a zero-valued Report")
			}
		})
	}
}

func TestAnIncompleteModelIsRejected(t *testing.T) {
	for name, body := range map[string]string{
		"no ok":             `{"modules":[]}`,
		"ok but no modules": `{"ok":true,"commands":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := stub(t, body, 0).Inspect(context.Background(), "any"); err == nil {
				t.Fatal("an incomplete model must be an error")
			}
		})
	}
}

func TestAFailedResolveNeedsNoModules(t *testing.T) {
	// ok=false is avspec reporting it could not load the manifest; demanding
	// modules there would turn a legitimate refusal into an error.
	m, err := stub(t, `{"ok":false,"findings":[]}`, 1).Inspect(context.Background(), "any")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.OK {
		t.Error("OK should be false")
	}
}
