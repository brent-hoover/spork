package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"path/filepath"
	"sort"

	"kriya/internal/devloop"
	"kriya/internal/gates"
	"kriya/internal/orchestrator"
	"kriya/internal/owner"
	"kriya/internal/planner"
	"kriya/internal/workspace"
)

// buildStages maps each orchestrator stage to the module that performs it.
//
// The mapping lives here, in the composition root, for the same reason the
// recovery ordering does: the orchestrator must reach the modules, and an
// interface would make every module reach back. Reading this function tells
// you which module does what, and reading orchestrator.Table tells you when.
// deps is everything the stages delegate to.
//
// A struct rather than a parameter list: the composition root wires ten
// collaborators, and ten positional arguments is a call nobody can read and a
// place two of the same type can quietly swap.
type deps struct {
	ws        workspace.Manager
	loop      devloop.Loop
	runner    gates.Runner
	snap      planner.Snapshot
	po        owner.Owner
	submitter orchestrator.Submitter
	queue     orchestrator.Queue
	completer orchestrator.Completer

	commandsFor  func(module string) map[string]string
	criteriaFor  func(ticket string) []string
	issueFor     func(ticket string) string
	sessionFor   func(ctx context.Context, run string) (string, error)
	instructions string
}

func buildStages(d deps) orchestrator.Stages {
	ws, loop, runner := d.ws, d.loop, d.runner
	snap, commandsFor, instructions := d.snap, d.commandsFor, d.instructions
	po, criteriaFor := d.po, d.criteriaFor
	submitter, issueFor, sessionFor := d.submitter, d.issueFor, d.sessionFor
	return orchestrator.Stages{
		orchestrator.StageWorkspace: func(ctx context.Context, run orchestrator.BuildRun) (orchestrator.BuildRun, error) {
			w, err := ws.Ensure(ctx, run.ID, run.Ticket)
			if err != nil {
				return run, err
			}
			// The base the workspace was cut from is what the gate chain runs
			// at. Recording it on the run is what lets a stale pass be
			// recognised: a result from any other commit never satisfies.
			run.GatedBase = w.Base
			return run, nil
		},

		orchestrator.StageDevLoop: func(ctx context.Context, run orchestrator.BuildRun) (orchestrator.BuildRun, error) {
			w, found, err := ws.Store.Find(ctx, run.ID)
			if err != nil {
				return run, err
			}
			if !found {
				return run, fmt.Errorf("no workspace for run %s", run.ID)
			}
			_, err = loop.Work(ctx, devloop.Request{
				Run: run.ID, Ticket: run.Ticket, Title: run.Ticket,
				Workspace: w.Path, Commands: commandsFor(run.Ticket),
				// The law comes from the PINNED snapshot, never the working
				// tree: the agent is shown the spec the build was admitted
				// against.
				Spec:         specForContext(snap.Law, snap.Constitution, snap.Content),
				Modules:      modulesFor(snap, run.Ticket),
				Instructions: instructions,
				// The limit the RUN was created under, not the one configured
				// now: a config change affects only future runs.
				RoundLimit: run.RoundLimit,
			})
			return run, err
		},

		orchestrator.StageValidate: validateStage(ws, po, criteriaFor),

		orchestrator.StageSubmit: submitStage(ws, submitter, issueFor, sessionFor),

		orchestrator.StageMerge: mergeStage(d.queue),

		orchestrator.StageComplete: completeStage(ws, d.completer, issueFor),

		orchestrator.StageGates: gateStage(ws, runner, commandsFor),
	}
}

// commandsFromSnapshot resolves a module's gate commands out of the pinned
// snapshot, never the working tree.
func commandsFromSnapshot(snap planner.Snapshot) func(string) map[string]string {
	return func(module string) map[string]string {
		if cmds, ok := snap.ResolvedCommands[module]; ok {
			return cmds
		}
		// A single-module target is the common case for a first build; falling
		// back to the only entry beats failing on a name mismatch.
		if len(snap.ResolvedCommands) == 1 {
			for _, cmds := range snap.ResolvedCommands {
				return cmds
			}
		}
		return nil
	}
}

// modulesFor names the modules a ticket touches.
//
// The snapshot's module set when the ticket names none of them: a first build
// is one module, and a context assembled for nothing would show the agent no
// law at all. Ticket-level module attribution lands with REQ-decompose's
// module tagging.
func modulesFor(snap planner.Snapshot, ticket string) []string {
	if _, ok := snap.ResolvedCommands[ticket]; ok {
		return []string{ticket}
	}
	out := make([]string, 0, len(snap.Law))
	for _, module := range snap.Law {
		out = append(out, module.ID)
	}
	sort.Strings(out)
	return out
}

// gateStage runs the whole chain for a run's module.
func gateStage(
	ws workspace.Manager, runner gates.Runner, commandsFor func(string) map[string]string,
) func(context.Context, orchestrator.BuildRun) (orchestrator.BuildRun, error) {
	return func(ctx context.Context, run orchestrator.BuildRun) (orchestrator.BuildRun, error) {
		w, found, err := ws.Store.Find(ctx, run.ID)
		if err != nil {
			return run, err
		}
		if !found {
			return run, fmt.Errorf("no workspace for run %s", run.ID)
		}
		// Incremented BEFORE the chain runs, so every result carries the
		// attempt it belongs to. Stamping afterwards would record results
		// under the previous attempt and let them satisfy it.
		run.Attempt++
		results, err := runner.RunChain(ctx, run.ID, run.Ticket, run.GatedBase, w.Path,
			commandsFor(run.Ticket), run.Attempt)
		if errors.Is(err, gates.ErrBaseMoved) {
			// A RESULT, not a malfunction: the run integrates the new base and
			// the complete chain reruns against it. Recording a mixed-base
			// pass would say nothing about either base.
			return run, err
		}
		if err != nil {
			return run, err
		}
		for _, result := range results {
			if !result.Passed {
				// A failing gate is a RESULT, and the table sends it back to
				// the dev loop — the findings are the next instruction.
				// Returning an error here would park the run instead.
				return run, fmt.Errorf("gate %s failed for %s", result.Gate, result.Module)
			}
		}
		return run, nil
	}
}

// validateStage runs the product owner over a fully gated run.
func validateStage(
	ws workspace.Manager, po owner.Owner, criteriaFor func(string) []string,
) func(context.Context, orchestrator.BuildRun) (orchestrator.BuildRun, error) {
	return func(ctx context.Context, run orchestrator.BuildRun) (orchestrator.BuildRun, error) {
		w, found, err := ws.Store.Find(ctx, run.ID)
		if err != nil {
			return run, err
		}
		if !found {
			return run, fmt.Errorf("no workspace for run %s", run.ID)
		}
		v, err := po.Validate(ctx, owner.Request{
			Build: run.ID, Module: run.Ticket, Ticket: run.Ticket,
			Commit: run.GatedBase, Attempt: run.Attempt,
			Criteria: criteriaFor(run.Ticket), Workspace: w.Path,
			SystemFile: systemFileFor(w.Path),
		})
		if err != nil {
			return run, err
		}
		if !v.Passed() {
			// A RESULT, not an error: the table sends it back to the dev loop,
			// and the notes are what the agent is told to act on.
			return run, fmt.Errorf("product owner returned %s: %s", v.Verdict, v.Notes)
		}
		return run, nil
	}
}

// submitStage opens the sutra review a validated run has earned.
func submitStage(
	ws workspace.Manager, submitter orchestrator.Submitter,
	issueFor func(string) string,
	sessionFor func(context.Context, string) (string, error),
) func(context.Context, orchestrator.BuildRun) (orchestrator.BuildRun, error) {
	return func(ctx context.Context, run orchestrator.BuildRun) (orchestrator.BuildRun, error) {
		w, found, err := ws.Store.Find(ctx, run.ID)
		if err != nil {
			return run, err
		}
		if !found {
			return run, fmt.Errorf("no workspace for run %s", run.ID)
		}
		session, err := sessionFor(ctx, run.ID)
		if err != nil {
			return run, err
		}
		if run.ReviewID != "" {
			// A review already exists, so this is rework reaching the human
			// again. Resubmission advances its revision rather than opening a
			// second review over the same ticket.
			return submitter.Resubmit(ctx, run, orchestrator.Rework{
				Branch: w.Branch, Session: session, Summary: run.Ticket,
				Revision: run.ReviewRevision, VerdictEvent: run.ReviewVerdictEvent,
			})
		}
		return submitter.Submit(ctx, run, orchestrator.Submission{
			Issue: issueFor(run.Ticket), Branch: w.Branch, Session: session,
			Summary: run.Ticket,
		})
	}
}

// mergeStage lands an approved run's work.
//
// The attempt was enqueued when the approval was observed; this stage runs it.
// Splitting the two is what makes an approval durable before anything acts on
// it — a crash between them leaves a queue entry recovery finishes.
func mergeStage(q orchestrator.Queue) func(context.Context, orchestrator.BuildRun) (orchestrator.BuildRun, error) {
	return func(ctx context.Context, run orchestrator.BuildRun) (orchestrator.BuildRun, error) {
		key, found, err := q.AttemptFor(ctx, run.ID)
		if err != nil {
			return run, err
		}
		if !found {
			return run, fmt.Errorf("run %s is merging with no attempt enqueued", run.ID)
		}
		attempt, err := q.Run(ctx, key)
		if err != nil {
			return run, err
		}
		if attempt.State != orchestrator.AttemptMerged {
			// A RESULT: the attempt aborted, and the run parks with the cause
			// rather than looking merged.
			return run, fmt.Errorf("merge attempt %s: %s", attempt.State, attempt.Note)
		}
		return run, nil
	}
}

// completeStage closes a merged run's ticket through the tracker's own gate.
func completeStage(
	ws workspace.Manager, c orchestrator.Completer, issueFor func(string) string,
) func(context.Context, orchestrator.BuildRun) (orchestrator.BuildRun, error) {
	return func(ctx context.Context, run orchestrator.BuildRun) (orchestrator.BuildRun, error) {
		w, found, err := ws.Store.Find(ctx, run.ID)
		if err != nil {
			return run, err
		}
		if !found {
			return run, fmt.Errorf("no workspace for run %s", run.ID)
		}
		// The APPROVED commit, which is what the run's branch must still point
		// at. A commit landing on it after the merge is work nothing reviewed.
		return c.Complete(ctx, run, orchestrator.Completion{
			Issue: issueFor(run.Ticket), Branch: w.Branch, Merged: run.ReviewCommit,
		})
	}
}

// systemFileFor names the context bundle a workspace holds.
//
// The SAME file the dev agent read. The PO judging an AC's intent needs the
// whole spec's law, and re-assembling it here could show the PO a different
// spec than the one the work was done against.
func systemFileFor(workspace string) string {
	return filepath.Join(workspace, ".kriya", "context.json")
}

// criteriaFromTickets maps a ticket title to the criteria it cites.
func criteriaFromTickets(tickets []planner.Ticket) func(string) []string {
	byTitle := make(map[string][]string, len(tickets))
	for _, t := range tickets {
		byTitle[t.Title] = t.Criteria
	}
	return func(ticket string) []string { return byTitle[ticket] }
}

// issuesFromTickets maps a ticket title to the sutra issue it became.
func issuesFromTickets(tickets []planner.Ticket) func(string) string {
	byTitle := make(map[string]string, len(tickets))
	for _, t := range tickets {
		byTitle[t.Title] = t.IssueID
	}
	return func(ticket string) string { return byTitle[ticket] }
}

// sessionFromStore reads the session that did a run's work.
//
// From the RECORDED session, never a fresh one: a review is stamped with it so
// feedback routes back to the agent instance that wrote the code.
func sessionFromStore(db *sql.DB) func(context.Context, string) (string, error) {
	return func(ctx context.Context, run string) (string, error) {
		var session string
		err := db.QueryRowContext(ctx,
			`SELECT session_id FROM dev_session WHERE run = ?`, run).Scan(&session)
		if err != nil {
			return "", fmt.Errorf("read session for run %s: %w", run, err)
		}
		return session, nil
	}
}
