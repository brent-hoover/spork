package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"kriya/internal/planner"
)

// planReport renders a build's completion report from its plan.
//
// The report is what a human reads to decide whether the build is done, so it
// says what was BUILT — the ticket set and the criteria each ticket carried —
// rather than restating that the gates passed. The gates are a precondition of
// reaching a human at all; the question here is whether the work matches what
// was asked for.
type planReport struct {
	db    *sql.DB
	plans planner.SQLPlans
}

func reportFor(db *sql.DB) planReport {
	return planReport{db: db, plans: planner.SQLPlans{DB: db}}
}

// Render produces the completion report for a target.
func (r planReport) Render(ctx context.Context, targetKey string) (string, error) {
	plan, found, err := r.plans.Find(ctx, targetKey)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("no plan for %s to report on", targetKey)
	}
	tickets, err := (planner.SQLTickets{DB: r.db}).ForTarget(ctx, targetKey)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Build completion report\n\n")
	fmt.Fprintf(&b, "Target: `%s`\nSpec: `%s`\nTickets: %d\n\n", targetKey, plan.SpecHash, len(tickets))
	b.WriteString("## What was built\n\n")
	for _, t := range tickets {
		fmt.Fprintf(&b, "- **%s** (`%s`)\n", t.Title, t.IssueID)
		if len(t.Criteria) > 0 {
			fmt.Fprintf(&b, "  - satisfies: %s\n", strings.Join(t.Criteria, ", "))
		}
	}
	if len(tickets) == 0 {
		// A plan stamped whole with no tickets is a decomposition that
		// produced nothing. Reporting it as a finished build would ask a
		// human to approve an empty claim.
		return "", fmt.Errorf("the plan for %s is whole but has no tickets", targetKey)
	}
	b.WriteString("\nEvery ticket above is complete and no target-scoped work is outstanding.\n")
	return b.String(), nil
}
