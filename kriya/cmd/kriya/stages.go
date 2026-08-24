package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"path/filepath"
	"sort"

	kctx "kriya/internal/context"
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
	loop      worker
	runner    gates.Runner
	snap      planner.Snapshot
	po        owner.Owner
	submitter orchestrator.Submitter
	queue     orchestrator.Queue
	completer orchestrator.Completer
	learnings kctx.Recorder

	commandsFor func(module string) map[string]string
	// ticketFor recovers the whole ticket — body, criteria and issue — from
	// the plan. A pop hands back an id and a title, and a run built from
	// those alone reaches the dev agent with nothing to implement against
	// and the product owner with nothing to validate against.
	ticketFor    func(title string) planner.Ticket
	sessionFor   func(ctx context.Context, run string) (string, error)
	actor        string
	instructions string
}

// worker is the dev loop as the stage uses it.
//
// An interface at the composition root rather than the concrete Loop, because
// what the stage assembles — the ticket, the actor, the generated toolset — is
// only observable in the request it hands over.
type worker interface {
	Work(ctx context.Context, req devloop.Request) (devloop.Session, error)
}

func buildStages(d deps) orchestrator.Stages {
	ws, runner := d.ws, d.runner
	snap, commandsFor := d.snap, d.commandsFor
	submitter, ticketFor, sessionFor := d.submitter, d.ticketFor, d.sessionFor
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
			// The branch travels on the run for the same reason the issue
			// does: recovery replays a submission from what the RUN recorded,
			// and a branch re-derived after a workspace was pruned is no
			// branch at all.
			run.Branch = w.Branch
			return run, nil
		},

		orchestrator.StageDevLoop: func(ctx context.Context, run orchestrator.BuildRun) (orchestrator.BuildRun, error) {
			// The default branch may have moved while this run was elsewhere —
			// a merge that lost its CAS, a completion whose head advanced.
			// Rerunning the chain against a base it has moved past just fails
			// the same way, so the branch integrates first and the new base is
			// what the next chain freezes.
			w, err := ws.Integrate(ctx, run.ID)
			if err != nil {
				return run, err
			}
			run.GatedBase = w.Base
			ticket := ticketFor(run.Ticket)
			modules := modulesFor(snap, run.Ticket)
			session, err := d.loop.Work(ctx, devloop.Request{
				Run: run.ID, Ticket: run.Ticket, Title: ticket.Title,
				Body: ticket.Body, Criteria: ticket.Criteria,
				Issue: ticket.IssueID, Actor: d.actor,
				// The gate-chain round this pass belongs to, which scopes the
				// review round ids. Without it a second pass over the same run
				// overwrites the first pass's rounds.
				Attempt:   run.Attempt,
				Workspace: w.Path, Commands: commandsFor(run.Ticket),
				// What the agent may RUN, generated from the touched modules'
				// own commands. Listing them as content tells it what it is
				// judged by; this is what lets it run them.
				AllowRules: devloop.AllowRules(commandsForAll(commandsFor, modules)...),
				// The law comes from the PINNED snapshot, never the working
				// tree: the agent is shown the spec the build was admitted
				// against.
				Spec:         specForContext(snap.Law, snap.Constitution, snap.Content),
				Modules:      modules,
				Instructions: d.instructions,
				ProjectKey:   projectKeyOf(run.Plan),
				// The limit the RUN was created under, not the one configured
				// now: a config change affects only future runs.
				RoundLimit: run.RoundLimit,
			})
			if errors.Is(err, devloop.ErrArchitectDirected) {
				// The architect answered and its direction is recorded. The
				// run has neither advanced nor failed: the next pass reads
				// that direction and starts from it, which is the whole point
				// of asking. Parking here would strand it.
				return run, orchestrator.ErrWaiting
			}
			if errors.Is(err, devloop.ErrReviewPending) {
				// The review job has not finished. The run has neither
				// advanced nor failed: it comes back, rather than reaching
				// the gates with a review still running or parking for an
				// operator who can do nothing about it.
				return run, orchestrator.ErrWaiting
			}
			if err != nil {
				return run, err
			}
			// The branch head the session produced. Everything after this —
			// gates, validation, review, merge — is about THIS commit, not the
			// base the branch was cut from.
			if n := len(session.Commits); n > 0 {
				run.Head = session.Commits[n-1]
			}
			return run, nil
		},

		orchestrator.StageValidate: validateStage(ws, d.po, ticketFor, snap, d.learnings),

		orchestrator.StageSubmit: submitStage(ws, submitter, sessionFor),

		orchestrator.StageMerge: mergeStage(d.queue),

		orchestrator.StageComplete: completeStage(ws, d.completer),

		orchestrator.StageGates: gateStage(ws, runner, commandsFor, snap, d.learnings),
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

// gateStage runs the whole chain once per module the ticket touched.
//
// PER MODULE, because that is what the gates require: each module carries its
// own six commands, and one chain labelled with the run's ticket runs one
// module's commands and records them under a name no module has.
func gateStage(
	ws workspace.Manager, runner gates.Runner, commandsFor func(string) map[string]string,
	snap planner.Snapshot, learnings kctx.Recorder,
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
		modules := modulesFor(snap, run.Ticket)
		if len(modules) == 0 {
			// Running no gates must never read as passing them. Intake refuses
			// a snapshot with no modules, so reaching here is a defect.
			return run, fmt.Errorf("no module for run %s to gate", run.ID)
		}
		for _, module := range modules {
			commands := commandsFor(module)
			if len(commands) == 0 {
				// Intake refuses a module missing any of the six, so a module
				// the snapshot resolved nothing for is a defect rather than a
				// failing gate — and running no gates must never read as a
				// module that passed them.
				return run, fmt.Errorf("module %s resolved no gate commands", module)
			}
			failed, err := runModuleChain(ctx, runner, run, module, w.Path, commands, learnings)
			if err != nil || failed != nil {
				return run, chainOutcome(err, failed)
			}
		}
		return run, nil
	}
}

// chainOutcome turns one module's chain into the stage's result.
func chainOutcome(err error, failed *gates.Result) error {
	if err != nil {
		return err
	}
	// A failing gate is a RESULT, and the table sends it back to the dev loop
	// — the findings are the next instruction. Returning a malfunction here
	// would park the run instead.
	return fmt.Errorf("gate %s failed for %s", failed.Gate, failed.Module)
}

// runModuleChain runs one module's chain and reports the first gate that
// failed, recording its diagnosis as a learning.
func runModuleChain(
	ctx context.Context, runner gates.Runner, run orchestrator.BuildRun,
	module, dir string, commands map[string]string, learnings kctx.Recorder,
) (*gates.Result, error) {
	results, err := runner.RunChain(ctx, run.ID, module, run.Head, run.GatedBase,
		dir, commands, run.Attempt)
	if errors.Is(err, gates.ErrBaseMoved) {
		// A RESULT, not a malfunction: the run integrates the new base and
		// the complete chain reruns against it. Recording a mixed-base pass
		// would say nothing about either base.
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	for _, result := range results {
		if result.Passed {
			continue
		}
		// The diagnosis is the lesson, recorded HERE rather than in a
		// post-mortem — the same moment-of-correction rule the pair loop
		// follows.
		if err := recordLearning(ctx, learnings, run, kctx.Capture{
			Lesson:     string(result.Detail),
			Module:     result.Module,
			Pattern:    "gate-failure/" + result.Gate,
			SourceKind: kctx.SourceGateFailure,
			SourceRef:  result.Gate,
		}); err != nil {
			return nil, err
		}
		return &result, nil
	}
	return nil, nil
}

// commandsForAll resolves every touched module's gate commands.
func commandsForAll(
	commandsFor func(string) map[string]string, modules []string,
) []map[string]string {
	out := make([]map[string]string, 0, len(modules))
	for _, module := range modules {
		if cmds := commandsFor(module); len(cmds) > 0 {
			out = append(out, cmds)
		}
	}
	return out
}

// validateStage runs the product owner over a fully gated run.
func validateStage(
	ws workspace.Manager, po owner.Owner, ticketFor func(string) planner.Ticket,
	snap planner.Snapshot, learnings kctx.Recorder,
) func(context.Context, orchestrator.BuildRun) (orchestrator.BuildRun, error) {
	return func(ctx context.Context, run orchestrator.BuildRun) (orchestrator.BuildRun, error) {
		w, found, err := ws.Store.Find(ctx, run.ID)
		if err != nil {
			return run, err
		}
		if !found {
			return run, fmt.Errorf("no workspace for run %s", run.ID)
		}
		ticket := ticketFor(run.Ticket)
		// The module the gate results were recorded under, which is what the
		// PO reads to confirm the chain passed at this commit. A first build
		// touches one module; a ticket spanning several is validated against
		// the first, in the same order the chain ran them.
		modules := modulesFor(snap, run.Ticket)
		if len(modules) == 0 {
			return run, fmt.Errorf("no module for run %s to validate", run.ID)
		}
		v, err := po.Validate(ctx, owner.Request{
			Build: run.ID, Module: modules[0], Ticket: run.Ticket,
			Commit: run.Head, Attempt: run.Attempt,
			Criteria: ticket.Criteria, Workspace: w.Path,
			SystemFile: systemFileFor(w.Path),
		})
		if err != nil {
			return run, err
		}
		if !v.Passed() {
			if err := recordLearning(ctx, learnings, run, kctx.Capture{
				Lesson:     v.Notes,
				Module:     modules[0],
				Pattern:    "po-rejection/" + v.Verdict,
				SourceKind: kctx.SourcePORejection,
				SourceRef:  v.Verdict,
			}); err != nil {
				return run, err
			}
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
		if run.ReviewID != "" && run.ReviewState == orchestrator.SubmitSubmitted &&
			run.ReviewCommit == run.Head && run.ReviewVerdictEvent == "" {
			// The review for THIS head already exists and no verdict is
			// waiting to be answered. That is the crash window between
			// recording the review and the table writing the advanced state:
			// the run comes back in submitting with a landed submission, and
			// resubmitting would advance a revision nothing reworked against
			// a verdict event there has not been.
			//
			// The verdict event is what separates this from rework that
			// produced no new commit — a round that only answered a question,
			// or a fix that reverted to what was there. That is a real rework
			// at an unchanged head, and the human's findings are still
			// unanswered.
			return run, nil
		}
		if run.ReviewID != "" {
			// A review exists and a verdict is unanswered, or the head has
			// moved. Either way this is rework reaching the human again:
			// resubmission advances its revision rather than opening a second
			// review over the same ticket.
			return submitter.Resubmit(ctx, run, orchestrator.Rework{
				Branch: w.Branch, Session: session, Summary: run.Ticket,
				Revision: run.ReviewRevision, VerdictEvent: run.ReviewVerdictEvent,
			})
		}
		// The run's OWN issue, which is what recovery replays under. A stage
		// that looked it up again could submit against a different one than
		// the replay of the same submission would.
		return submitter.Submit(ctx, run, orchestrator.Submission{
			Issue: run.Issue, Branch: w.Branch, Session: session,
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
			return releaseReview(run),
				fmt.Errorf("run %s is merging with no attempt enqueued", run.ID)
		}
		attempt, err := q.Run(ctx, key)
		if errors.Is(err, orchestrator.ErrWaiting) {
			// Another attempt holds the section. The run has neither advanced
			// nor failed, so it stays in merging and the caller comes back.
			return run, err
		}
		if err != nil {
			return run, err
		}
		if attempt.State != orchestrator.AttemptMerged {
			// A RESULT: the attempt aborted, and the table sends the run back
			// to the dev loop with the cause.
			return releaseReview(run),
				fmt.Errorf("merge attempt %s: %s", attempt.State, attempt.Note)
		}
		return run, nil
	}
}

// releaseReview unbinds a review that can no longer be advanced.
//
// An aborted merge returns the run to the dev loop, which integrates the moved
// base and produces a NEW head. That head is work no human has seen, so it
// needs its own review — and the approved one is spent: resubmitting it fences
// on an APPROVAL event where sutra expects a changes-requested one, which it
// refuses forever.
//
// The commit and the session stay. They are the history of what was reviewed,
// and the next submission overwrites them with its own.
func releaseReview(run orchestrator.BuildRun) orchestrator.BuildRun {
	run.ReviewID = ""
	run.ReviewKey = ""
	run.ReviewState = orchestrator.SubmitNone
	run.ReviewRevision = 0
	run.ReviewVerdictEvent = ""
	return run
}

// completeStage closes a merged run's ticket through the tracker's own gate.
func completeStage(
	ws workspace.Manager, c orchestrator.Completer,
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
			Issue: run.Issue, Branch: w.Branch, Merged: run.ReviewCommit,
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

// ticketFromStore reads the whole ticket back from the plan.
//
// From the STORE, not from whatever the pop happened to return: a pop carries
// an id and a title, and a run built from those alone reaches the dev agent
// with nothing to implement against and the product owner with nothing to
// validate against.
//
// The popped ticket is the fallback, so a plan whose row cannot be read still
// carries its issue id — losing the issue would submit a review against
// nothing and close no ticket.
func ticketFromStore(db *sql.DB, target string, popped planner.Ticket) func(string) planner.Ticket {
	return func(string) planner.Ticket {
		t, found, err := (planner.SQLTickets{DB: db}).Find(
			context.Background(), target, popped.IssueID)
		if err != nil || !found {
			return popped
		}
		t.IssueID = popped.IssueID
		return t
	}
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

// projectKeyOf scopes a project learning to its target.
//
// The target path, which is stable for the life of a target — the same thing
// the merge queue scopes attempts by.
func projectKeyOf(plan string) string { return plan }

// recordLearning fills in what every capture from a run shares.
//
// The run, the project and the TRIGGERING commit are the run's, not the
// caller's: a capture site that had to supply them could get them wrong, and
// a lesson anchored on the wrong commit points at code that was never the
// problem.
func recordLearning(
	ctx context.Context, learnings kctx.Recorder, run orchestrator.BuildRun, c kctx.Capture,
) error {
	if learnings == nil {
		return nil
	}
	if c.Lesson == "" {
		// Nothing to teach. A blank lesson is noise every future run reads
		// past, and the write would refuse it anyway.
		return nil
	}
	c.Scope = kctx.ScopeProject
	c.ProjectKey = projectKeyOf(run.Plan)
	c.SourceRun = run.ID
	c.SourceCommit = run.Head
	if err := learnings.Record(ctx, c); err != nil {
		return fmt.Errorf("record learning for %s: %w", run.Ticket, err)
	}
	return nil
}
