//go:build acceptance

package acceptance

import (
	"context"
	"fmt"

	"github.com/cucumber/godog"

	"kriya/internal/planner"
)

// restoreWorld is one parked-plan scenario's state.
type restoreWorld struct {
	head     *headWorld
	steps    planner.SQLSteps
	heads    *countingHeads
	restorer planner.Restorer
	parked   planner.Plan
	restored planner.Plan
}

// countingHeads records how many times the replacement CAS was entered.
//
// "when the ineligibility is FIRST OBSERVED" is a claim that observing is
// ENOUGH: a candidate the head has outrun is dead the moment anyone looks, and
// the operator's inbox should not have to enter the CAS to find out. Both
// paths reach terminal historical, so the round trip is the only difference
// there is to measure.
type countingHeads struct {
	inner    planner.HeadStore
	attempts int
}

func (c *countingHeads) Replace(
	ctx context.Context, p planner.Plan,
) (planner.Replacement, error) {
	c.attempts++
	return c.inner.Replace(ctx, p)
}

func (c *countingHeads) Head(ctx context.Context, key string) (planner.PlanHead, bool, error) {
	return c.inner.Head(ctx, key)
}

func (c *countingHeads) Activate(ctx context.Context, key string) error {
	return c.inner.Activate(ctx, key)
}

func (w *world) newRestore() (*restoreWorld, error) {
	h, err := w.newHead(w.tempDir)
	if err != nil {
		return nil, err
	}
	if err := migrateSteps(h); err != nil {
		return nil, err
	}
	steps := planner.SQLSteps{DB: h.db}
	heads := &countingHeads{inner: h.heads}
	r := &restoreWorld{
		head:  h,
		steps: steps,
		heads: heads,
		restorer: planner.Restorer{
			Plans: h.plans, Heads: heads, Steps: steps,
		},
	}
	w.restore = r
	return r, nil
}

// park writes a plan awaiting the operator, with the step progress given.
func (r *restoreWorld) park(key string, generation int, states []string) error {
	ctx := context.Background()
	r.parked = planner.Plan{
		Key: key, TargetKey: "/spec", SpecHash: "hash-h1",
		Generation: generation, State: planner.PlanAwaitingOperator,
		Error: "the tracker refused the epic",
	}
	if err := r.head.plans.Upsert(ctx, r.parked); err != nil {
		return err
	}
	steps := make([]planner.Step, len(states))
	for n, state := range states {
		steps[n] = planner.Step{
			Plan: key, Seq: n, Ordinal: n, Other: -1,
			Kind: planner.StepCreate, Key: fmt.Sprintf("key-%d", n), State: state,
		}
	}
	return r.steps.Write(ctx, steps)
}

func registerRestore(sc *godog.ScenarioContext, w *world) {
	sc.Given(`^a head plan parked awaiting-operator before activation$`, func() error {
		r, err := w.newRestore()
		if err != nil {
			return err
		}
		// Nothing issued: it never reached its phases.
		if err := r.park("plan-before", 1,
			[]string{planner.StepPending, planner.StepPending}); err != nil {
			return err
		}
		return r.takeHead()
	})

	sc.Given(`^a head plan parked awaiting-operator after activation$`, func() error {
		r, err := w.newRestore()
		if err != nil {
			return err
		}
		// Part-way through its phases: one step landed, one did not.
		if err := r.park("plan-after", 1,
			[]string{planner.StepComplete, planner.StepIssued}); err != nil {
			return err
		}
		return r.takeHead()
	})

	sc.Given(`^a non-head candidate parked after losing the replacement CAS, still generation-eligible$`,
		func() error {
			r, err := w.newRestore()
			if err != nil {
				return err
			}
			ctx := context.Background()
			// An ACTIVATED head at generation 2, and a parked candidate at 4:
			// strictly newer, so its retry can genuinely win.
			winner := r.head.plan("hash-h1", 2)
			winner.State = planner.PlanActive
			if err := r.head.plans.Upsert(ctx, winner); err != nil {
				return err
			}
			if verdict, err := r.head.heads.Replace(ctx, winner); err != nil || !verdict.Won {
				return fmt.Errorf("install the head: %v won=%v", err, verdict.Won)
			}
			return r.park("plan-eligible", 4, []string{planner.StepPending})
		})

	sc.Given(`^a parked candidate whose intake generation the head has since outrun$`, func() error {
		r, err := w.newRestore()
		if err != nil {
			return err
		}
		ctx := context.Background()
		winner := r.head.plan("hash-h1", 9)
		winner.State = planner.PlanActive
		if err := r.head.plans.Upsert(ctx, winner); err != nil {
			return err
		}
		if verdict, err := r.head.heads.Replace(ctx, winner); err != nil || !verdict.Won {
			return fmt.Errorf("install the head: %v won=%v", err, verdict.Won)
		}
		return r.park("plan-outrun", 3, []string{planner.StepPending})
	})

	sc.When(`^the operator restores it$`, func() error { return w.restore.run() })
	sc.When(`^the operator retries it$`, func() error { return w.restore.run() })

	sc.Then(`^it returns to pending with predecessor rows reset from durable step progress and retirement resumes$`,
		func() error {
			r := w.restore
			if r.restored.State != planner.PlanPending {
				return fmt.Errorf("the plan returned to %q, want pending", r.restored.State)
			}
			if r.restored.Completed {
				return fmt.Errorf("a plan that issued nothing came back stamped whole")
			}
			if r.restored.Error != "" {
				return fmt.Errorf("the restored plan still carries its park cause")
			}
			return nil
		})

	sc.Then(`^it returns to active with ticket-row states recomputed from per-step progress$`,
		func() error {
			r := w.restore
			if r.restored.State != planner.PlanActive {
				return fmt.Errorf("the plan returned to %q, want active", r.restored.State)
			}
			// RECOMPUTED: one step is still issued, so the plan is not whole.
			// Restoring it as complete would arm completion detection over a
			// ticket set still missing its last mutations.
			if r.restored.Completed {
				return fmt.Errorf("a plan with an unfinished step came back stamped whole")
			}
			return nil
		})

	sc.Then(`^it re-enters the replacement CAS and on winning atomically repoints its predecessor to the head it beat$`,
		func() error {
			r := w.restore
			ctx := context.Background()
			head, _, err := r.head.heads.Head(ctx, "/spec")
			if err != nil {
				return err
			}
			if head.Current != r.parked.Key {
				return fmt.Errorf("the retried candidate did not take the head")
			}
			if r.restored.Predecessor != r.head.plan("hash-h1", 2).Key {
				return fmt.Errorf("the retried candidate points at predecessor %q",
					r.restored.Predecessor)
			}
			// The head it beat is superseded, in the same transaction.
			beaten, err := r.head.landed(r.head.plan("hash-h1", 2).Key)
			if err != nil {
				return err
			}
			if beaten.State != planner.PlanSuperseded {
				return fmt.Errorf("the beaten head is %q", beaten.State)
			}
			return nil
		})

	sc.Then(`^it transitions durably to terminal historical when the ineligibility is first observed$`,
		func() error {
			r := w.restore
			// The scenario has no When: OBSERVING the ineligibility is what
			// triggers the transition, so the observation happens here rather
			// than being set up beforehand.
			before := r.heads.attempts
			if err := r.run(); err != nil {
				return err
			}
			if r.heads.attempts != before {
				return fmt.Errorf(
					"observing an outrun candidate entered the replacement CAS %d time(s)",
					r.heads.attempts-before)
			}
			if r.restored.State != planner.PlanHistorical {
				return fmt.Errorf("the outrun candidate is %q, want historical", r.restored.State)
			}
			// DURABLY: read it back rather than trusting the return value.
			got, err := r.head.landed(r.parked.Key)
			if err != nil {
				return err
			}
			if got.State != planner.PlanHistorical {
				return fmt.Errorf("the stored plan is %q", got.State)
			}
			return nil
		})

	sc.Then(`^it leaves the inbox's actionable set — adopting its decomposition again requires a fresh intake$`,
		func() error {
			r := w.restore
			// The inbox lists parked plans. Historical is terminal, so it is
			// not one — and restoring it again must be refused rather than
			// offering an action that cannot work.
			if _, err := r.restorer.Restore(context.Background(), r.parked.Key); err == nil {
				return fmt.Errorf("a terminal historical plan was restored again")
			}
			// And the head is untouched: an ineligible candidate observing
			// its own ineligibility must change nothing else.
			head, _, err := r.head.heads.Head(context.Background(), "/spec")
			if err != nil {
				return err
			}
			if head.Current != r.head.plan("hash-h1", 9).Key {
				return fmt.Errorf("the head moved to %q", head.Current)
			}
			return nil
		})
}

// takeHead makes the parked plan the current head.
func (r *restoreWorld) takeHead() error {
	ctx := context.Background()
	if _, err := r.head.db.ExecContext(ctx,
		`INSERT INTO plan_head (target_key, current, generation, fence)
		 VALUES (?, ?, 1, 1)`, "/spec", r.parked.Key); err != nil {
		return fmt.Errorf("seed the head: %w", err)
	}
	return nil
}

// run restores the parked plan.
func (r *restoreWorld) run() error {
	got, err := r.restorer.Restore(context.Background(), r.parked.Key)
	if err != nil {
		return err
	}
	r.restored = got
	return nil
}
