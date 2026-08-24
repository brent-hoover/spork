package cli

import (
	"context"
	"fmt"
	"io"

	kctx "kriya/internal/context"
)

// Learn is the `kriya learn add` entry point.
//
// AC-learn-manual says the operator can add a learning BY HAND, with the same
// scoping and tagging as a captured one. A command is what makes that true:
// without it the claim rests on whatever writes to the store directly, which
// is nothing the operator has.
func Learn(ctx context.Context, out io.Writer, recorder kctx.Recorder, c kctx.Capture) error {
	w := &errWriter{w: out}
	if err := recorder.Record(ctx, c); err != nil {
		// The write is what enforces ENT-learning's conditional rules, so its
		// refusal is the operator's answer — printed, not just returned, since
		// a refusal they cannot see is one they cannot act on.
		w.printf("refused: %v\n", err)
		return fmt.Errorf("%w", err)
	}
	w.printf("recorded a %s learning for %s (%s)\n", c.Scope, c.Module, c.Pattern)
	return w.err
}
