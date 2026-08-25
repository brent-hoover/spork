//go:build acceptance

package acceptance

import (
	"context"
	"fmt"

	"github.com/cucumber/godog"

	"kriya/internal/planner"
)

// resolveWorld is one same-key-retry scenario's state.
type resolveWorld struct{ plans *countingPlans }

// countingPlans is a PlanStore that counts writes.
//
// "with no mutation" is the claim under test in four of the six outcomes, and
// it cannot be checked by reading the row back: an idempotent rewrite of the
// same values leaves a row that looks untouched. Counting the writes is what
// makes the claim observable.
type countingPlans struct {
	rows   map[string]planner.Plan
	writes int
}

func newCountingPlans() *countingPlans {
	return &countingPlans{rows: map[string]planner.Plan{}}
}

func (c *countingPlans) Upsert(_ context.Context, p planner.Plan) error {
	c.writes++
	c.rows[p.Key] = p
	return nil
}

func (c *countingPlans) Find(_ context.Context, targetKey string) (planner.Plan, bool, error) {
	for _, p := range c.rows {
		if p.TargetKey == targetKey && p.State == planner.PlanActive {
			return p, true, nil
		}
	}
	return planner.Plan{}, false, nil
}

func (c *countingPlans) ByKey(_ context.Context, key string) (planner.Plan, bool, error) {
	p, ok := c.rows[key]
	return p, ok, nil
}

// seed installs a plan under key "K" without counting it as a write.
func (r *resolveWorld) seed(p planner.Plan) {
	p.Key, p.TargetKey = "K", "/spec"
	r.plans.rows["K"] = p
	r.plans.writes = 0
}

// resolve runs the real resolution against the seeded plan.
func (r *resolveWorld) resolve() (planner.Resolution, error) {
	return planner.Resolve(context.Background(), r.plans, "K")
}

// outcome seeds one state and reports what resolving it did.
func (r *resolveWorld) outcome(p planner.Plan) (planner.Resolution, error) {
	r.seed(p)
	res, err := r.resolve()
	if err != nil {
		return planner.Resolution{}, err
	}
	if res.Fresh {
		return planner.Resolution{}, fmt.Errorf("key K resolved to no plan at all")
	}
	if res.Plan.State != p.State {
		return planner.Resolution{}, fmt.Errorf(
			"resolving a %s plan returned a %s one", p.State, res.Plan.State)
	}
	return res, nil
}

// untouched requires the resolution to have written nothing.
func (r *resolveWorld) untouched(what string) error {
	if r.plans.writes != 0 {
		return fmt.Errorf("resolving %s wrote to the store %d time(s)", what, r.plans.writes)
	}
	return nil
}

// reports requires an outcome that hands the plan back rather than resuming.
func (r *resolveWorld) reports(p planner.Plan, what string) error {
	res, err := r.outcome(p)
	if err != nil {
		return err
	}
	if res.Resume {
		return fmt.Errorf("resolving %s asked the caller to resume it", what)
	}
	return r.untouched(what)
}

// resumes requires an outcome that carries the plan's phases forward.
func (r *resolveWorld) resumes(p planner.Plan, what string) error {
	res, err := r.outcome(p)
	if err != nil {
		return err
	}
	if !res.Resume {
		return fmt.Errorf("resolving %s did not resume it", what)
	}
	// Resuming is not itself a mutation: the caller carries the phases
	// forward from durable per-step progress, and resolution only reads.
	return r.untouched(what)
}

func registerResolve(sc *godog.ScenarioContext, w *world) {
	sc.Given(`^a plan exists for decomposition key "([^"]*)"$`, func(string) error {
		w.resolve = &resolveWorld{plans: newCountingPlans()}
		return nil
	})
	sc.When(`^a request with key "([^"]*)" arrives again$`, func(string) error {
		// Resolution happens per state below: one request cannot arrive
		// against six different plans at once, and each Then is a claim
		// about one of them.
		if w.resolve == nil {
			return fmt.Errorf("no plan was seeded")
		}
		return nil
	})

	sc.Then(`^a pending plan resumes from its durable progress$`, func() error {
		return w.resolve.resumes(planner.Plan{State: planner.PlanPending}, "a pending plan")
	})
	sc.Then(`^an active plan without completed resumes its remaining phases$`, func() error {
		return w.resolve.resumes(
			planner.Plan{State: planner.PlanActive, Completed: false},
			"an active plan without the completed stamp")
	})
	sc.Then(`^an active plan bearing completed returns as the current already-complete result with no mutation$`,
		func() error {
			res, err := w.resolve.outcome(
				planner.Plan{State: planner.PlanActive, Completed: true, Tickets: 3})
			if err != nil {
				return err
			}
			if res.Resume {
				return fmt.Errorf("a plan already stamped whole was resumed")
			}
			if !res.Plan.Completed {
				return fmt.Errorf("the result does not carry the completed stamp")
			}
			return w.resolve.untouched("a completed plan")
		})
	sc.Then(`^a superseded plan returns the historical result with no mutation$`, func() error {
		res, err := w.resolve.outcome(planner.Plan{
			State: planner.PlanSuperseded, SupersededBy: "plan-2"})
		if err != nil {
			return err
		}
		if res.Resume {
			return fmt.Errorf("a superseded plan was resumed")
		}
		// The caller learns where the head went. Without it the retry knows
		// only that its plan lost, which is not an answer it can act on.
		if res.Plan.SupersededBy != "plan-2" {
			return fmt.Errorf("the historical result names no replacement")
		}
		return w.resolve.untouched("a superseded plan")
	})
	sc.Then(`^an awaiting-operator plan is preserved untouched for the operator$`, func() error {
		res, err := w.resolve.outcome(planner.Plan{
			State: planner.PlanAwaitingOperator, Error: "the tracker refused the epic"})
		if err != nil {
			return err
		}
		if res.Resume {
			return fmt.Errorf("a parked plan was silently resumed past whatever stopped it")
		}
		if res.Plan.Error == "" {
			return fmt.Errorf("the parked plan reports no cause for the operator to act on")
		}
		return w.resolve.untouched("a parked plan")
	})
	sc.Then(`^a plan in terminal historical returns that terminal result with no mutation$`, func() error {
		return w.resolve.reports(planner.Plan{State: planner.PlanHistorical},
			"a terminal historical plan")
	})
}
