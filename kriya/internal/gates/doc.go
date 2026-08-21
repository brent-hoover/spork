// Package gates is the quality-gate runner — structural lint, typing, branch coverage, mutation
// testing via the target's stack commands.
//
// Owns GateResult.
//
// This package does not know what a linter or a coverage tool is. It resolves
// the module's command from the pinned SpecSnapshot, runs it at the head
// commit, and records the outcome. It counts no arms and infers no
// threshold — the passing bar is the target's to declare.
package gates
