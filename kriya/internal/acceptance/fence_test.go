//go:build acceptance

package acceptance

import (
	"context"
	"errors"
	"fmt"

	"github.com/cucumber/godog"

	"kriya/internal/planner"
)

// fenceWorld is one pop-fence scenario's state.
//
// Against a REAL SQLite fence, because the claim under test is that admission
// and a head transition are write-write conflicts on one row. A fake would be
// a second implementation of the very thing in question.
type fenceWorld struct {
	head  *headWorld
	fence planner.SQLFence
	// stale is the version an agent read before the replacement moved it.
	stale int
	err   error
}

func (w *world) newFence() (*fenceWorld, error) {
	h, err := w.newHead(w.tempDir)
	if err != nil {
		return nil, err
	}
	f := &fenceWorld{head: h, fence: planner.SQLFence{DB: h.db}}
	w.fence = f
	return f, nil
}

func registerFence(sc *godog.ScenarioContext, w *world) {
	sc.Given(`^an agent read the fence at version V with the unactivated-heads counter at zero$`,
		func() error {
			f, err := w.newFence()
			if err != nil {
				return err
			}
			ctx := context.Background()
			// A head that is already activated, so the counter is clear.
			plan := f.head.plan("hash-h1", 1)
			plan.State, plan.Completed = planner.PlanActive, true
			if err := f.head.plans.Upsert(ctx, plan); err != nil {
				return err
			}
			if verdict, err := f.head.heads.Replace(ctx, plan); err != nil || !verdict.Won {
				return fmt.Errorf("install the head: %v won=%v", err, verdict.Won)
			}
			if err := f.head.heads.Activate(ctx, plan.Key); err != nil {
				return err
			}

			got, err := f.fence.Read(ctx)
			if err != nil {
				return err
			}
			if got.UnactivatedHeads != 0 {
				return fmt.Errorf("the counter is %d, not zero", got.UnactivatedHeads)
			}
			f.stale = got.Version
			return nil
		})

	sc.When(`^a replacement transaction concurrently increments the counter and the fence version$`,
		func() error {
			f := w.fence
			ctx := context.Background()
			candidate := f.head.plan("hash-h2", 2)
			if err := f.head.plans.Upsert(ctx, candidate); err != nil {
				return err
			}
			verdict, err := f.head.heads.Replace(ctx, candidate)
			if err != nil || !verdict.Won {
				return fmt.Errorf("the replacement lost: %v won=%v", err, verdict.Won)
			}
			got, err := f.fence.Read(ctx)
			if err != nil {
				return err
			}
			if got.UnactivatedHeads != 1 {
				return fmt.Errorf("the counter is %d after a replacement", got.UnactivatedHeads)
			}
			if got.Version <= f.stale {
				return fmt.Errorf("the replacement did not bump the version past %d", f.stale)
			}
			return nil
		})

	sc.When(`^the agent attempts admission using its stale read$`, func() error {
		w.fence.err = w.fence.fence.Admit(context.Background(), w.fence.stale)
		return nil
	})

	sc.Then(`^the admission CAS on the fence version fails — a plain read-assert would have admitted it$`,
		func() error {
			f := w.fence
			if f.err == nil {
				return fmt.Errorf("a stale admission was accepted")
			}
			// The agent's own read said the counter was zero, and it still
			// says zero in that stale snapshot. A read-assert would check
			// exactly that and commit. Only the CONDITIONAL WRITE on the
			// version catches the concurrent replacement.
			var fenced *planner.ErrFenced
			if !errors.As(f.err, &fenced) {
				return fmt.Errorf("admission failed with %v, not a fence refusal", f.err)
			}
			return nil
		})

	sc.When(`^the agent retries against the fresh fence while the head is still unactivated$`,
		func() error {
			f := w.fence
			got, err := f.fence.Read(context.Background())
			if err != nil {
				return err
			}
			f.stale = got.Version
			f.err = f.fence.Admit(context.Background(), got.Version)
			return nil
		})

	sc.Then(`^admission is refused because the counter is nonzero$`, func() error {
		f := w.fence
		var fenced *planner.ErrFenced
		if !errors.As(f.err, &fenced) {
			return fmt.Errorf("the retry was accepted: %v", f.err)
		}
		if fenced.StaleVersion {
			return fmt.Errorf("the retry was refused for a stale version, not the counter")
		}
		if fenced.UnactivatedHeads != 1 {
			return fmt.Errorf("the refusal names %d unactivated heads", fenced.UnactivatedHeads)
		}
		return nil
	})

	sc.When(`^the head activates and decrements the counter$`, func() error {
		f := w.fence
		ctx := context.Background()
		candidate := f.head.plan("hash-h2", 2)
		candidate.State, candidate.Completed = planner.PlanActive, true
		if err := f.head.plans.Upsert(ctx, candidate); err != nil {
			return err
		}
		if err := f.head.heads.Activate(ctx, candidate.Key); err != nil {
			return err
		}
		got, err := f.fence.Read(ctx)
		if err != nil {
			return err
		}
		if got.UnactivatedHeads != 0 {
			return fmt.Errorf("the counter is %d after activation", got.UnactivatedHeads)
		}
		f.stale = got.Version
		return nil
	})

	sc.Then(`^the retried pop succeeds$`, func() error {
		f := w.fence
		if err := f.fence.Admit(context.Background(), f.stale); err != nil {
			return fmt.Errorf("admission was refused after activation: %w", err)
		}
		// And it WROTE: the version moved, so a concurrent replacement
		// racing this admission would have lost its own CAS.
		got, err := f.fence.Read(context.Background())
		if err != nil {
			return err
		}
		if got.Version <= f.stale {
			return fmt.Errorf("admission did not bump the version, so it was a read-assert")
		}
		return nil
	})
}
