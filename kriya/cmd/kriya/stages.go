package main

import (
	"context"
	"fmt"

	"sort"

	"kriya/internal/devloop"
	"kriya/internal/gates"
	"kriya/internal/orchestrator"
	"kriya/internal/planner"
	"kriya/internal/workspace"
)

// buildStages maps each orchestrator stage to the module that performs it.
//
// The mapping lives here, in the composition root, for the same reason the
// recovery ordering does: the orchestrator must reach the modules, and an
// interface would make every module reach back. Reading this function tells
// you which module does what, and reading orchestrator.Table tells you when.
func buildStages(
	ws workspace.Manager,
	loop devloop.Loop,
	runner gates.Runner,
	snap planner.Snapshot,
	commandsFor func(module string) map[string]string,
	instructions string,
) orchestrator.Stages {
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
			})
			return run, err
		},

		orchestrator.StageGates: func(ctx context.Context, run orchestrator.BuildRun) (orchestrator.BuildRun, error) {
			w, found, err := ws.Store.Find(ctx, run.ID)
			if err != nil {
				return run, err
			}
			if !found {
				return run, fmt.Errorf("no workspace for run %s", run.ID)
			}
			results, err := runner.RunChain(ctx, run.ID, run.Ticket, run.GatedBase, w.Path, commandsFor(run.Ticket))
			if err != nil {
				return run, err
			}
			run.Attempt++
			for _, result := range results {
				if !result.Passed {
					// A failing gate is a RESULT, and the table sends it back
					// to the dev loop — the findings are the next instruction.
					// Returning an error here would park the run instead.
					return run, fmt.Errorf("gate %s failed for %s", result.Gate, result.Module)
				}
			}
			return run, nil
		},
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
