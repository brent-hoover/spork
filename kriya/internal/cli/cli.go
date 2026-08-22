package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"kriya/internal/planner"
)

// projectKey derives a sutra project key from the target directory.
//
// Uppercase and truncated because sutra constrains ProjectKey. It is derived
// rather than configured so a re-intake of the same target reaches the same
// project instead of creating a second.
func projectKey(dir string) string {
	base := strings.ToUpper(filepath.Base(dir))
	base = strings.Map(func(r rune) rune {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return -1
	}, base)
	if len(base) > 8 {
		base = base[:8]
	}
	if base == "" {
		base = "TARGET"
	}
	return base
}

// errWriter defers write-error handling to one place.
//
// A failed write is not cosmetic here: AC-intake-refuse requires the verify
// findings to reach the operator, so output that silently vanished means the
// requirement was not met even though the refusal was correct.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) printf(format string, a ...any) {
	if e.err != nil {
		return
	}
	_, e.err = fmt.Fprintf(e.w, format, a...)
}

// Build is the `kriya build <project>` entry point.
func Build(ctx context.Context, out io.Writer, in planner.Intaker, dir, actor string) error {
	w := &errWriter{w: out}
	snapshot, err := in.AdmitAndPin(ctx, dir)

	var refusal *planner.Refusal
	switch {
	case err == nil:
		w.printf("%s: ready\n", dir)
		w.printf("  snapshot %s (%d artifacts, %d modules)\n",
			snapshot.Hash[:12], len(snapshot.Content), len(snapshot.ResolvedCommands))
		target, epicErr := in.EnsureEpic(ctx, dir, snapshot.Hash, projectKey(dir), filepath.Base(dir), actor)
		if epicErr != nil {
			return epicErr
		}
		w.printf("  epic %s in project %s\n", target.EpicID, target.ProjectID)
	case errors.As(err, &refusal):
		w.printf("refused: %s\n", refusal.Reason)
		for _, f := range refusal.Findings {
			w.printf("  [%s] %s: %s\n", f.Severity, f.Code, f.Message)
		}
	default:
		return err
	}
	// Both matter: the refusal is the outcome, the write error means the
	// operator may not have seen it.
	return errors.Join(err, w.err)
}
