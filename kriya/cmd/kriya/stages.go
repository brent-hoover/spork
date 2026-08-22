package main

import (
	"context"
	"fmt"

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
