package devloop_test

import "kriya/internal/agent"

// devReply is what a satisfied dev agent returns.
func devReply() []agent.Result {
	return []agent.Result{{SessionID: "dev-session-1", Model: "test-model", Text: "done"}}
}
