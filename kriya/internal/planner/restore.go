package planner

import (
	"context"
	"fmt"
)

// Restorer returns a parked plan to the operator's control.
//
// FORWARD only. Every parked state has exactly one way out and it always
// moves the plan onward — there is no undo, because the tracker mutations a
// plan already issued cannot be taken back and a plan rewound past them would
// reissue work that exists.
type Restorer struct {
	Plans PlanStore
	Heads HeadStore
	Steps StepStore
}

// Restore returns a parked plan to the state its durable progress supports.
//
// Which state that is depends on how far it got, and the durable rows say:
//
//   - Parked BEFORE activation, it returns to PENDING. Its predecessor rows
//     are reset from step progress and retirement resumes.
//   - Parked AFTER activation, it returns to ACTIVE, with ticket-row states
//     recomputed from per-step progress rather than assumed.
//
// A plan is "after activation" exactly when it holds the head: the head move
// is what activation follows, so a candidate that never took the head cannot
// have been activated.
func (r Restorer) Restore(ctx context.Context, key string) (Plan, error) {
	plan, found, err := r.Plans.ByKey(ctx, key)
	if err != nil {
		return Plan{}, err
	}
	if !found {
		return Plan{}, fmt.Errorf("no plan %s to restore", Short(key))
	}
	if plan.State != PlanAwaitingOperator {
		return Plan{}, fmt.Errorf("plan %s is %s, not parked for the operator", Short(key), plan.State)
	}

	head, hasHead, err := r.Heads.Head(ctx, plan.TargetKey)
	if err != nil {
		return Plan{}, err
	}
	if hasHead && head.Current == plan.Key {
		return r.restoreHead(ctx, plan)
	}
	return r.retryCandidate(ctx, plan, head, hasHead)
}

// restoreHead returns a parked HEAD plan to pending or active.
func (r Restorer) restoreHead(ctx context.Context, plan Plan) (Plan, error) {
	steps, err := r.Steps.ForPlan(ctx, plan.Key)
	if err != nil {
		return Plan{}, fmt.Errorf("read the sequence of plan %s: %w", Short(plan.Key), err)
	}

	// RECOMPUTED from per-step progress, never assumed. A plan restored as
	// whole because it once looked whole would arm completion detection over
	// a ticket set that is still missing its last assignments.
	plan.Completed = allComplete(steps)
	plan.State = PlanActive
	if !plan.Completed && !anyIssued(steps) {
		// Nothing has been issued at all, so it never reached its phases.
		// Pending is where retirement resumes from.
		plan.State = PlanPending
	}
	plan.Error = ""
	if err := r.Plans.Upsert(ctx, plan); err != nil {
		return Plan{}, err
	}
	return plan, nil
}

// retryCandidate re-enters a parked NON-HEAD candidate into the CAS.
func (r Restorer) retryCandidate(
	ctx context.Context, plan Plan, head PlanHead, hasHead bool,
) (Plan, error) {
	if hasHead {
		current, found, err := r.Plans.ByKey(ctx, head.Current)
		if err != nil {
			return Plan{}, err
		}
		// Already outrun: it transitions durably to terminal historical the
		// moment the ineligibility is OBSERVED, rather than being handed back
		// to the operator as a retry the CAS can only reject again.
		if found && !Eligible(plan.Generation, current.Generation) {
			plan.State, plan.Error = PlanHistorical,
				fmt.Sprintf("the head reached intake generation %d", current.Generation)
			if err := r.Plans.Upsert(ctx, plan); err != nil {
				return Plan{}, err
			}
			return plan, nil
		}
	}

	// Still eligible: re-enter the CAS. On winning it atomically repoints its
	// predecessor at the head it beat, which is what makes the retry a
	// genuine replacement rather than a second head.
	verdict, err := r.Heads.Replace(ctx, plan)
	if err != nil {
		return Plan{}, fmt.Errorf("retry the replacement for %s: %w", Short(plan.Key), err)
	}
	if !verdict.Won {
		// It lost again, and Replace has already landed it.
		return r.reread(ctx, plan.Key)
	}
	plan.State, plan.Error, plan.Predecessor = PlanPending, "", verdict.Predecessor
	if err := r.Plans.Upsert(ctx, plan); err != nil {
		return Plan{}, err
	}
	return plan, nil
}

// reread returns a plan's durable state after something else wrote it.
func (r Restorer) reread(ctx context.Context, key string) (Plan, error) {
	plan, found, err := r.Plans.ByKey(ctx, key)
	if err != nil {
		return Plan{}, err
	}
	if !found {
		return Plan{}, fmt.Errorf("plan %s vanished while being restored", Short(key))
	}
	return plan, nil
}

// allComplete reports whether every step of a sequence has landed.
func allComplete(steps []Step) bool {
	if len(steps) == 0 {
		return false
	}
	for _, s := range steps {
		if s.State != StepComplete {
			return false
		}
	}
	return true
}

// anyIssued reports whether any step has been sent to the tracker.
func anyIssued(steps []Step) bool {
	for _, s := range steps {
		if s.State != StepPending {
			return true
		}
	}
	return false
}
