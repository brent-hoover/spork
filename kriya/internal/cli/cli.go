package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"kriya/internal/planner"
)

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
func Build(ctx context.Context, out io.Writer, in planner.Intaker, dir string) error {
	w := &errWriter{w: out}
	err := in.Admit(ctx, dir)

	var refusal *planner.Refusal
	switch {
	case err == nil:
		w.printf("%s: ready\n", dir)
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
