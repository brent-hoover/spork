// Package agent is the coding-agent seam: claude -p run as a subprocess.
//
// NOT a declared avspec module. It sits outside devloop because PM, SA, and
// PO all invoke agents, and planner, architect, and owner may not import
// devloop.
//
// Owns the agent_invocation table even though ENT-agent-invocation is
// declared under MOD-orchestrator — no module that invokes an agent can
// legally import orchestrator, so the row would otherwise have no writer.
// See feature-work/kriya-build/spec-gaps.md.
//
// Invoked with --safe-mode: it keeps subscription auth while disabling
// CLAUDE.md, skills, plugins, hooks, and MCP servers, so a target repo's
// hooks cannot execute.
package agent
