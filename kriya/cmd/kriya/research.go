package main

import (
	"context"
	"fmt"
	"strings"

	"kriya/internal/agent"
	"kriya/internal/orchestrator"
	"kriya/internal/planner"
)

// spikeResearch asks an agent to answer a risk and document what it found.
//
// READ-ONLY tools, deliberately. A spike's job is to answer a question, and a
// spike that changed code would be doing implementation work under a ticket
// nothing gates — the gate chain is skipped precisely because there is
// supposed to be no code.
type spikeResearch struct {
	agent   agent.Agent
	ticket  func(string) planner.Ticket
	spec    string
	workdir string
}

// researchAllowRules are the spike agent's tools.
//
// The same read-only set the SA and PO get, for the same reason: the
// deliverable is a document, and nothing that could change the worktree
// belongs in reach of a ticket whose output nothing gates.
var researchAllowRules = []string{"Read", "Grep", "Glob"}

func (s spikeResearch) Research(
	ctx context.Context, run orchestrator.BuildRun,
) (string, error) {
	ticket := s.ticket(run.Ticket)
	res, err := s.agent.Run(ctx, agent.Request{
		Role:       agent.RoleSA,
		Prompt:     researchPrompt(ticket, s.spec, run),
		Workspace:  s.workdir,
		AllowRules: researchAllowRules,
	})
	if err != nil {
		return "", fmt.Errorf("research agent on %s: %w", run.Ticket, err)
	}
	return res.Text, nil
}

// researchPrompt states the risk and what a finding has to contain.
//
// A finding is EVIDENCE, not an opinion: the risk is retired on it, and
// dependent work starts on the strength of it. Asking for the answer without
// asking what supports it produces something nobody can check.
func researchPrompt(ticket planner.Ticket, spec string, run orchestrator.BuildRun) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Answer this risk and document what you found.\n\nRisk: %s\n", ticket.Title)
	if ticket.Body != "" {
		fmt.Fprintf(&b, "\n%s\n", ticket.Body)
	}
	if len(ticket.Criteria) > 0 {
		fmt.Fprintf(&b, "\nIt answers: %s\n", strings.Join(ticket.Criteria, ", "))
	}
	if len(ticket.Blocks) > 0 {
		fmt.Fprintf(&b, "\nWork waiting on this answer: %s\n", strings.Join(ticket.Blocks, ", "))
	}
	if run.ReviewVerdictEvent != "" {
		// A revision. The human rejected the previous finding, and their
		// reasons are on the review — the agent is being asked to answer the
		// same risk better, not a different one.
		b.WriteString("\nThis is a REVISION: a previous finding was rejected. " +
			"Read the review's comments and answer what they raised.\n")
	}
	b.WriteString("\nWrite a finding, not an opinion: state what you did, what you " +
		"observed, and what it means for the work waiting on it. A risk is " +
		"retired on this evidence and dependent work starts on its strength, " +
		"so anything you could not establish must say so.\n")
	if spec != "" {
		fmt.Fprintf(&b, "\nThe pinned specification:\n\n%s\n", spec)
	}
	b.WriteString("\nYou have READ-ONLY tools. A spike answers a question; it does " +
		"not implement the answer.\n")
	return b.String()
}
