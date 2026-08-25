package planner

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// LiveIssue is one issue as the tracker currently holds it.
//
// The LIVE queue, not this database's idea of it: a build's finish line is a
// question about the tracker, and kriya's own rows are a cache that a
// detachment, a reopen or somebody else's creation can outrun.
type LiveIssue struct {
	ID     string
	Status string
	// SubtreeRevision is the fence a completion claim is captured against.
	SubtreeRevision int64
}

// Issues reads a project's active work.
type Issues interface {
	// Active returns every issue that is not complete, and the feed watermark
	// the read saw. The watermark is what a later claim is fenced on: a
	// creation racing the read is behind it, and draining the feed to there
	// is what makes "nothing active" mean anything.
	Active(ctx context.Context, projectID string) ([]LiveIssue, string, error)
}

// TargetTickets lists a target's planned ticket set.
type TargetTickets interface {
	ForTarget(ctx context.Context, targetKey string) ([]Ticket, error)
}

// Detection is the answer to "may a completion attempt start".
type Detection struct {
	// Armed reports that the ticket set is whole and no target-scoped work is
	// outstanding. It is permission to ATTEMPT completion, never a claim that
	// the build is done: the claim is captured, fenced and reviewed after.
	Armed bool
	// Reason names what is holding completion back. Empty when armed.
	Reason string
	// Watermark is the feed position the read saw. A claim captured against
	// it is only as good as a feed drained to it.
	Watermark string
	// Blocking is every issue that must settle first, so an operator sees the
	// whole list rather than the first one found.
	Blocking []string
	// Epoch is the completion epoch this answer is about. A claim binds to
	// it: re-reading the epoch after detection would bind the attempt to one
	// nothing checked for completion.
	Epoch int
}

// Detector answers whether a target's build may attempt completion.
type Detector struct {
	Plans   PlanStore
	Tickets TargetTickets
	Issues  Issues
	// Epochs binds an answer to the epoch it is about. Nil answers about
	// epoch zero, which is what a module-level test of the rules wants.
	Epochs *Epochs
	// Epic is the target's umbrella issue, excused from its own completion
	// check. It is OPEN for exactly as long as the build runs — closing it is
	// what completion means — so counting it as outstanding work would make
	// completion unable to arm, ever. Empty excuses nothing: guessing which
	// issue is the umbrella would excuse real work by accident.
	Epic string
}

// activeStatuses are the tracker statuses that count as outstanding work.
//
// BLOCKED is among them, deliberately: blocked is active work resolved through
// the live queue, not work that has gone away. A build that completed over a
// blocked ticket would close its epic on work nobody did.
var activeStatuses = map[string]bool{
	"open": true, "in-progress": true, "blocked": true, "queued": true,
}

// Detect reports whether completion may be attempted for a target.
//
// The order is the point. The plan's own stamp comes first, because "every
// ticket created so far is complete" says nothing about a ticket set that is
// still growing — and only then is the live queue asked, because a plan that
// is not whole cannot be satisfied by any answer the tracker gives.
func (d Detector) Detect(ctx context.Context, targetKey, projectID string) (Detection, error) {
	plan, found, err := d.Plans.Find(ctx, targetKey)
	if err != nil {
		return Detection{}, fmt.Errorf("read plan for %s: %w", targetKey, err)
	}
	if !found {
		return Detection{Reason: fmt.Sprintf("%s has no plan yet", targetKey)}, nil
	}
	if plan.State != PlanCompleted {
		return Detection{Reason: fmt.Sprintf(
			"the plan for %s is still %s — its ticket set is not yet whole",
			targetKey, plan.State)}, nil
	}

	// The epoch BEFORE the queue. An advance landing between them then leaves
	// the answer bound to the OLDER epoch, whose claim fails the stamp's CAS
	// — the safe direction. Reading it after would bind a claim to an epoch
	// whose queue state was never checked, and that claim would PASS the CAS.
	epoch, err := d.epoch(ctx, targetKey)
	if err != nil {
		return Detection{}, err
	}

	active, watermark, err := d.Issues.Active(ctx, projectID)
	if err != nil {
		// "I could not read the queue" is not "the queue is empty". Arming
		// here would submit a completion review over work nobody looked at.
		return Detection{}, fmt.Errorf("read active issues in %s: %w", projectID, err)
	}

	planned, err := d.plannedIDs(ctx, targetKey)
	if err != nil {
		return Detection{}, err
	}
	blocking, reason := blockers(active, planned, d.Epic)
	if len(blocking) > 0 {
		return Detection{
			Reason: reason, Watermark: watermark, Blocking: blocking, Epoch: epoch,
		}, nil
	}
	return Detection{Armed: true, Watermark: watermark, Epoch: epoch}, nil
}

// epoch reads the completion epoch this answer is about.
func (d Detector) epoch(ctx context.Context, targetKey string) (int, error) {
	if d.Epochs == nil {
		return 0, nil
	}
	return d.Epochs.Current(ctx, targetKey)
}

// plannedIDs is the target's planned ticket set, by issue.
func (d Detector) plannedIDs(ctx context.Context, targetKey string) (map[string]bool, error) {
	tickets, err := d.Tickets.ForTarget(ctx, targetKey)
	if err != nil {
		return nil, fmt.Errorf("read planned tickets for %s: %w", targetKey, err)
	}
	out := make(map[string]bool, len(tickets))
	for _, t := range tickets {
		out[t.IssueID] = true
	}
	return out, nil
}

// blockers names every issue that must settle before completion may arm.
//
// Planned and unplanned alike. An unplanned ticket parented under the epic at
// bind time blocks just as hard: sutra's close gate would refuse the epic
// anyway, because it is a descendant — so arming would submit a completion
// review that can never close.
func blockers(
	active []LiveIssue, planned map[string]bool, epic string,
) (ids []string, reason string) {
	var parts []string
	for _, issue := range active {
		if !activeStatuses[issue.Status] {
			continue
		}
		if epic != "" && issue.ID == epic {
			// The umbrella being completed. Only THIS target's epic is
			// excused — another open epic in the project is work, and
			// completing over it would close an umbrella on an unfinished
			// subtree.
			continue
		}
		ids = append(ids, issue.ID)
		kind := "unplanned"
		if planned[issue.ID] {
			kind = "planned"
		}
		parts = append(parts, fmt.Sprintf("%s (%s, %s)", issue.ID, kind, issue.Status))
	}
	if len(ids) == 0 {
		return nil, ""
	}
	sort.Strings(ids)
	sort.Strings(parts)
	return ids, "outstanding work: " + strings.Join(parts, ", ")
}
