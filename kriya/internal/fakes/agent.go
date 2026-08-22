package fakes

import (
	"context"
	"encoding/json"
	"errors"

	"kriya/internal/agent"
)

// Agent is a programmable agent.Agent.
type Agent struct {
	// Replies is consumed in order, one per Run.
	Replies []agent.Result
	Err     error
	// Requests records what was asked, so a test can assert an agent was
	// invoked with the context and permissions the ticket called for.
	Requests []agent.Request
}

// NewAgent returns an Agent replying with structured once.
func NewAgent(structured string) *Agent {
	return &Agent{Replies: []agent.Result{{
		SessionID:  "session-1",
		Model:      "test-model",
		Structured: json.RawMessage(structured),
	}}}
}

// Run returns the next programmed reply.
func (a *Agent) Run(_ context.Context, req agent.Request) (agent.Result, error) {
	a.Requests = append(a.Requests, req)
	if a.Err != nil {
		return agent.Result{}, a.Err
	}
	if len(a.Replies) == 0 {
		return agent.Result{}, errors.New("fakes: no reply programmed")
	}
	next := a.Replies[0]
	a.Replies = a.Replies[1:]
	return next, nil
}
