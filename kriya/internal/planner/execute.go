package planner

import (
	"context"
	"fmt"
)

// executeSequence replays a plan's durable sequence against the tracker.
//
// Every step is marked ISSUED before its call and COMPLETE after. That window
// is the crash window, and marking it is what tells recovery the difference
// between "never sent" and "may have landed" — a step recovered as issued is
// replayed under its persisted key, where sutra returns the original result
// rather than creating a second resource.
//
// A step already complete is skipped. That is the whole of resumption: the
// sequence is the plan, so replaying it from the top does the remaining work
// and nothing else.
func (i Intaker) executeSequence(
	ctx context.Context, target BuildTarget, plan Plan, tickets []Ticket, steps []Step, actor string,
) error {
	issues := make(map[int]string, len(tickets))
	for _, step := range steps {
		if step.Kind == StepCreate && step.Issue != "" {
			issues[step.Ordinal] = step.Issue
		}
	}

	for _, step := range steps {
		if step.State == StepComplete {
			continue
		}
		if err := i.markStep(ctx, plan, step.Seq, StepIssued, step.Issue); err != nil {
			return err
		}
		issue, err := i.runStep(ctx, target, tickets, issues, step, actor)
		if err != nil {
			return fmt.Errorf("%s step %d of plan %s: %w", step.Kind, step.Seq, Short(plan.Key), err)
		}
		if step.Kind == StepCreate {
			issues[step.Ordinal] = issue
			tickets[step.Ordinal].IssueID = issue
			tickets[step.Ordinal].Plan = plan.Key
			if err := i.recordTicket(ctx, target, tickets[step.Ordinal]); err != nil {
				return err
			}
		}
		if err := i.markStep(ctx, plan, step.Seq, StepComplete, issue); err != nil {
			return err
		}
	}
	return nil
}

// runStep performs one mutation. The key is the PERSISTED one, always.
func (i Intaker) runStep(
	ctx context.Context, target BuildTarget, tickets []Ticket,
	issues map[int]string, step Step, actor string,
) (string, error) {
	switch step.Kind {
	case StepCreate:
		ticket := tickets[step.Ordinal]
		return i.Tracker.CreateIssue(ctx, target.ProjectID, ticket.Title, ticket.Body, actor, step.Key)
	case StepParent:
		return "", i.Tracker.AddRelation(ctx, target.EpicID, "parent_of", issues[step.Ordinal], actor, step.Key)
	case StepBlock:
		return "", i.Tracker.AddRelation(ctx, issues[step.Ordinal], "blocks", issues[step.Other], actor, step.Key)
	case StepAssign:
		// Assigned to the actor that will pop it: the tracker's work stack
		// offers only issues assigned to the popping identity.
		return "", i.Tracker.AssignIssue(ctx, issues[step.Ordinal], actor, actor, step.Key)
	}
	// A kind kriya does not know is a sequence it cannot replay. Skipping it
	// would report a plan whole with a mutation silently never made.
	return "", fmt.Errorf("unknown mutation kind %q", step.Kind)
}

// markStep records a step's durable state, when there is a store for it.
func (i Intaker) markStep(ctx context.Context, plan Plan, seq int, state, issue string) error {
	if i.Steps == nil {
		return nil
	}
	if err := i.Steps.Mark(ctx, plan.Key, seq, state, issue); err != nil {
		return fmt.Errorf("mark step %d of plan %s %s: %w", seq, Short(plan.Key), state, err)
	}
	return nil
}

// recordTicket persists one created ticket.
//
// HERE, because this is the only moment the ticket's criteria and the issue
// they became are both in hand. A pop later returns an id and a title, and a
// run built from those alone reaches the product owner with nothing to
// validate against.
func (i Intaker) recordTicket(ctx context.Context, target BuildTarget, ticket Ticket) error {
	if i.Tickets == nil {
		return nil
	}
	if err := i.Tickets.Put(ctx, target.TargetKey, ticket); err != nil {
		return fmt.Errorf("record ticket %s: %w", ticket.Title, err)
	}
	return nil
}
