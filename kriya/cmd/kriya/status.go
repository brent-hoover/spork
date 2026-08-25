package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"

	"kriya/internal/cli"
	"kriya/internal/gates"
	"kriya/internal/orchestrator"
	"kriya/internal/planner"
)

// status is the `kriya status` command surface.
func status(ctx context.Context, db *sql.DB, args []string) error {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	asJSON := flags.Bool("json", false, "emit the same state machine-readably")
	if err := flags.Parse(args); err != nil {
		return err
	}
	report, err := gatherStatus(ctx, db)
	if err != nil {
		return err
	}
	return cli.Status(os.Stdout, report, *asJSON)
}

// gatherStatus assembles the operator's picture from every store that holds
// part of it.
//
// HERE rather than in the command surface, which may not import the gate
// runner — and should not need to. What an operator wants is one picture, and
// the composition root is the one place that can see all of it.
func gatherStatus(ctx context.Context, db *sql.DB) (cli.StatusReport, error) {
	all, err := (planner.SQLTargets{DB: db}).All(ctx)
	if err != nil {
		return cli.StatusReport{}, err
	}
	report := cli.StatusReport{}
	for _, target := range all {
		got, err := targetStatus(ctx, db, target)
		if err != nil {
			return cli.StatusReport{}, err
		}
		report.Targets = append(report.Targets, got)
	}
	stalls, err := (orchestrator.SQLStalls{DB: db}).Open(ctx)
	if err != nil {
		return cli.StatusReport{}, err
	}
	for _, stall := range stalls {
		report.Stalls = append(report.Stalls, cli.StallStatus{
			TargetKey: stall.TargetKey, Cause: stall.Cause,
		})
	}
	return report, nil
}

// targetStatus gathers one target's plan, runs and completion.
func targetStatus(
	ctx context.Context, db *sql.DB, target planner.BuildTarget,
) (cli.TargetStatus, error) {
	out := cli.TargetStatus{TargetKey: target.TargetKey, Epic: target.EpicID}
	if plan, found, err := (planner.SQLPlans{DB: db}).Find(ctx, target.TargetKey); err != nil {
		return cli.TargetStatus{}, err
	} else if found {
		out.Plan, out.Tickets = plan.State, plan.Tickets
	}

	completion, err := completionStatus(ctx, db, target.TargetKey)
	if err != nil {
		return cli.TargetStatus{}, err
	}
	out.Completion = completion

	runs, err := (orchestrator.SQLStore{DB: db}).ForPlan(ctx, target.TargetKey)
	if err != nil {
		return cli.TargetStatus{}, err
	}
	for _, run := range runs {
		got, err := runStatus(ctx, db, run)
		if err != nil {
			return cli.TargetStatus{}, err
		}
		out.Runs = append(out.Runs, got)
	}
	return out, nil
}

// completionStatus reads a target's durable declaration.
//
// The EPOCH comes from the counter, not the claim: a claim settled at an
// earlier epoch is exactly what an operator needs to see distinguished from
// one that still holds.
func completionStatus(
	ctx context.Context, db *sql.DB, targetKey string,
) (cli.CompletionStatus, error) {
	advances := planner.SQLAdvances{DB: db}
	epoch, err := advances.Epoch(ctx, targetKey)
	if err != nil {
		return cli.CompletionStatus{}, err
	}
	stamped, _, err := advances.Stamped(ctx, targetKey)
	if err != nil {
		return cli.CompletionStatus{}, err
	}
	out := cli.CompletionStatus{Complete: stamped, Epoch: epoch}
	claim, found, err := (planner.SQLClaims{DB: db}).Find(ctx, targetKey)
	if err != nil {
		return cli.CompletionStatus{}, err
	}
	if found {
		out.Review, out.ReportVersion = claim.ReviewID, claim.ReportVersion
		out.ReopenOwed = claim.ReopenOwed
		if !stamped {
			out.State = claim.State
		}
	}
	return out, nil
}

// runStatus reads one run's position, including how far through the gate chain
// its current attempt got.
func runStatus(
	ctx context.Context, db *sql.DB, run orchestrator.BuildRun,
) (cli.RunStatus, error) {
	out := cli.RunStatus{
		Ticket: run.Ticket, State: string(run.State), Attempt: run.Attempt,
		Started: run.Started, Review: run.ReviewID,
	}
	if run.Head == "" || run.Attempt == 0 {
		// Nothing has been gated yet. An empty gate list says that more
		// honestly than six rows all reading FAILED.
		return out, nil
	}
	store := gates.SQLStore{DB: db}
	for _, gate := range gates.Chain {
		// Outcome, not Passed: the chain treats absence and failure alike —
		// neither is a pass — but a report that showed six FAILED rows for a
		// chain stopped at its second gate would misstate where the run is.
		passed, ran, err := store.Outcome(ctx, run.ID, run.Ticket, gate, run.Head, run.Attempt)
		if err != nil {
			return cli.RunStatus{}, fmt.Errorf("read %s result for %s: %w", gate, run.ID, err)
		}
		out.Gates = append(out.Gates, cli.GateStatus{Gate: gate, Ran: ran, Passed: passed})
	}
	return out, nil
}
