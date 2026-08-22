package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"kriya/internal/orchestrator"
	"kriya/internal/planner"
)

// projectKey derives a sutra project key from the target directory.
//
// Eight characters of sanitised basename is not enough: `foo-bar` and
// `foobar` sanitise identically, two directories can share a basename, and
// long names collide on their prefix. sutra enforces unique project keys, so a
// collision is not a cosmetic clash — the second target fails permanently with
// a 409 and there is no way forward.
//
// The key is therefore the basename truncated to leave room for a short digest
// of the FULL path. Readable at a glance, and distinct for distinct targets.
func projectKey(dir string) string {
	base := strings.Map(func(r rune) rune {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return -1
	}, strings.ToUpper(filepath.Base(dir)))
	if len(base) > 8 {
		base = base[:8]
	}
	if base == "" {
		base = "TARGET"
	}
	sum := sha256.Sum256([]byte(dir))
	return base + hex.EncodeToString(sum[:])[:6]
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
// Drive starts and advances a BuildRun for one ticket. Nil when no repository
// is configured, in which case a build stops after decomposition.
type Drive func(ctx context.Context, ticket planner.Ticket) (orchestrator.BuildRun, error)

func Build(ctx context.Context, out io.Writer, in planner.Intaker, dir, actor, token string, drive Drive) error {
	w := &errWriter{w: out}
	snapshot, err := in.AdmitAndPin(ctx, dir, token)

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
		tickets, decErr := in.Decompose(ctx, target, snapshot, actor)
		if decErr != nil {
			return decErr
		}
		w.printf("  %d tracer tickets\n", len(tickets))
		for _, t := range tickets {
			w.printf("    %s  [%s]\n", t.Title, strings.Join(t.Criteria, " "))
		}
		if drive == nil {
			// No repository to work in. Decomposition is the whole of intake,
			// and refusing to guess a repository is deliberate: the workspace
			// manager creates git worktrees, and inferring the wrong
			// repository would cut branches in it.
			w.printf("  no repository configured; not starting a run\n")
			break
		}
		run, driveErr := drive(ctx, tickets[0])
		w.printf("  run %s settled in %s\n", run.ID, run.State)
		if run.Error != "" {
			w.printf("    %s\n", run.Error)
		}
		if driveErr != nil {
			return driveErr
		}
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
