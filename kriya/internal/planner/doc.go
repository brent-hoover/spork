// Package planner is the PM agent: spec intake, tracer-bullet decomposition, risk-first
// ticketing.
//
// Owns Plan, PlanHead, PlannedTicket, SpecMapping, SpecSnapshot, PopFence,
// BuildTarget, IntakeAttempt, and CompletionAdvance.
//
// Declares the PopBinder interface it needs but does not implement: plan
// retirement must bind claimed-but-unbound pops on orchestrator-owned
// BuildRun rows, and planner may not import orchestrator. See
// feature-work/kriya-build/spec-gaps.md.
package planner
