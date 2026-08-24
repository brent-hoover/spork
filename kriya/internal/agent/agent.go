package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Role is which agent is being invoked. Every invocation records one.
type Role string

// The four roles CON-role-separation defines.
const (
	RolePM  Role = "pm"
	RoleSA  Role = "sa"
	RolePO  Role = "po"
	RoleDev Role = "dev"
)

// Request is one agent invocation.
type Request struct {
	Role Role
	// Prompt is the task. Context that is content rather than instruction
	// belongs in SystemFile.
	Prompt string
	// Workspace is the directory the agent may read and write.
	Workspace string
	// AllowRules are Claude Code permission rules, generated per ticket. No
	// bare Bash: what an agent may run is the ticket's business.
	AllowRules []string
	// SystemFile is a context bundle appended to the system prompt.
	SystemFile string
	// Resume continues an existing session rather than starting one.
	//
	// Every pair-loop round after the first is a correction to work THIS
	// agent did. A fresh session re-reads its own findings with no memory of
	// what it wrote, and the transcript splits across session ids so only the
	// last one reaches the thread catalog.
	Resume string
	// Schema, when set, constrains the reply to this JSON Schema and fills
	// Result.Structured. Asking for prose and parsing it is how a build
	// engine ends up guessing what an agent meant.
	Schema json.RawMessage
}

// Result is what an invocation produced.
type Result struct {
	// SessionID stamps the DevSession and the sutra thread the transcript
	// imports into, so a run's conversation is findable from its ticket.
	SessionID string
	// Model is what ACTUALLY ran, not what was requested — AC-tier-observed
	// wants the resolved model, because a fallback or a stale config makes
	// those differ exactly when it matters.
	Model      string
	Text       string
	Structured json.RawMessage
}

// Agent runs one agent invocation.
type Agent interface {
	Run(ctx context.Context, req Request) (Result, error)
}

// Tiers maps each role to a tier name, and each tier to a model.
//
// No model identifier appears in kriya's source (AC-tier-config), and a role
// with no configured tier fails at startup rather than defaulting
// (AC-tier-explicit) — a silent default is how a build quietly runs on the
// wrong model for a week.
type Tiers struct {
	Roles  map[Role]string   `json:"roles"`
	Models map[string]string `json:"models"`
}

// Resolve returns the model configured for a role.
func (t Tiers) Resolve(role Role) (tier, model string, err error) {
	tier, ok := t.Roles[role]
	if !ok || tier == "" {
		return "", "", fmt.Errorf("no tier configured for role %q", role)
	}
	model, ok = t.Models[tier]
	if !ok || model == "" {
		return "", "", fmt.Errorf("tier %q for role %q names no model", tier, role)
	}
	return tier, model, nil
}

// Validate reports the first role that cannot be resolved.
func (t Tiers) Validate(roles ...Role) error {
	if len(t.Roles) == 0 {
		return errors.New("no role tiers configured")
	}
	for _, role := range roles {
		if _, _, err := t.Resolve(role); err != nil {
			return err
		}
	}
	return nil
}
