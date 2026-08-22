// Package devloop runs one pair-programming session: a dev agent working in
// an isolated workspace on one ticket.
//
// The full loop — commit cadence, roborev rounds, fix, recommit — lands with
// the review bridge. What is here is the invocation itself: the agent seam,
// the per-ticket toolset, and the session record the transcript imports under.
package devloop

import (
	"context"
	"fmt"
	"strings"

	"kriya/internal/agent"
	"kriya/internal/clock"
)

// Session is one dev-agent invocation against a ticket.
type Session struct {
	Run    string
	Ticket string
	// SessionID stamps the sutra thread the transcript imports into, so a
	// run's conversation is findable from its ticket.
	SessionID string
	Model     string
	Ended     bool
}

// Store persists sessions.
type Store interface {
	Upsert(ctx context.Context, s Session) error
}

// Loop drives a dev agent.
type Loop struct {
	Agent agent.Agent
	Store Store
	Now   clock.Clock
}

// Request is what the loop needs to work a ticket.
type Request struct {
	Run       string
	Ticket    string
	Title     string
	Body      string
	Criteria  []string
	Workspace string
	// Commands are the module's resolved gate commands. They travel as
	// CONTENT so the agent knows what its work will be judged by; permission
	// to run them is granted separately.
	Commands map[string]string
	// AllowRules are the generated permission rules. No bare Bash: what an
	// agent may run is the ticket's business, and "tools irrelevant to the
	// ticket are absent" has to be mechanically true rather than requested.
	AllowRules []string
}

// Work runs one session.
//
// The session is recorded BEFORE the agent is invoked and stamped ended after,
// so a crash mid-session leaves a row recovery can terminate and import the
// transcript from — AC-thread-no-loss requires every outcome to reach the
// thread catalog, crashed included.
func (l Loop) Work(ctx context.Context, req Request) (Session, error) {
	session := Session{Run: req.Run, Ticket: req.Ticket}
	if err := l.Store.Upsert(ctx, session); err != nil {
		return Session{}, fmt.Errorf("record session: %w", err)
	}

	res, err := l.Agent.Run(ctx, agent.Request{
		Role:       agent.RoleDev,
		Prompt:     prompt(req),
		Workspace:  req.Workspace,
		AllowRules: req.AllowRules,
	})
	if err != nil {
		return Session{}, fmt.Errorf("dev agent on %s: %w", req.Ticket, err)
	}

	session.SessionID = res.SessionID
	session.Model = res.Model
	session.Ended = true
	if err := l.Store.Upsert(ctx, session); err != nil {
		return Session{}, fmt.Errorf("record ended session: %w", err)
	}
	return session, nil
}

// prompt states the ticket and the bar it will be judged by.
func prompt(req Request) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Implement this ticket in the workspace.\n\nTitle: %s\n", req.Title)
	if req.Body != "" {
		fmt.Fprintf(&b, "\n%s\n", req.Body)
	}
	if len(req.Criteria) > 0 {
		fmt.Fprintf(&b, "\nIt satisfies: %s\n", strings.Join(req.Criteria, ", "))
	}
	b.WriteString("\nWrite the test first: no behaviour is implemented before the test mapped\n")
	b.WriteString("to its acceptance criterion exists and fails.\n")
	if len(req.Commands) > 0 {
		b.WriteString("\nYour work is judged by these commands, in this order:\n")
		for _, name := range []string{"test", "lint", "typecheck", "arch", "coverage", "mutation"} {
			if cmd := req.Commands[name]; cmd != "" {
				fmt.Fprintf(&b, "  %-9s %s\n", name+":", cmd)
			}
		}
	}
	return b.String()
}
