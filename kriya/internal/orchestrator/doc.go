// Package orchestrator is the outer loop as a deterministic state machine — pops tickets, advances
// BuildRun states from an explicit state table, delegates every stage; no
// judgment, no agent conversations.
//
// Owns BuildRun, AgentInvocation's build-scoped half, Stall, MergeAttempt,
// and AttributionAmbiguity.
//
// The transition table is the module's whole design: every branch a reader
// might expect is a row, not a code path. If a change requires this package
// to decide anything, the change is wrong (CON-deterministic-orchestrator).
package orchestrator
