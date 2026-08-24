package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// Claude runs agents as `claude -p` subprocesses.
//
// A subprocess rather than a library because the Agent SDK is Python and
// TypeScript only, and because parallel dev agents then get independent kill,
// timeout, and crash containment — a crashing agent cannot take the
// orchestrator with it.
type Claude struct {
	// Bin is the executable, "claude" by default.
	Bin   string
	Tiers Tiers
}

// envelope is the `--output-format json` payload.
type envelope struct {
	IsError    bool            `json:"is_error"`
	Result     string          `json:"result"`
	SessionID  string          `json:"session_id"`
	Structured json.RawMessage `json:"structured_output"`
	ModelUsage map[string]any  `json:"modelUsage"`
}

// Run invokes the agent and returns what it produced.
func (c Claude) Run(ctx context.Context, req Request) (Result, error) {
	_, model, err := c.Tiers.Resolve(req.Role)
	if err != nil {
		return Result{}, err
	}
	out, err := c.invoke(ctx, req, model)
	if err != nil {
		return Result{}, err
	}
	return parseEnvelope(out, req.Role, model)
}

// args builds the command line.
//
// --safe-mode, always. It keeps subscription auth while disabling CLAUDE.md,
// skills, plugins, hooks, and MCP servers — so a TARGET repository's
// .claude/settings.json hooks cannot execute. A -p session shows no trust
// dialog, so without this, building an arbitrary spec runs arbitrary code from
// it. --bare would also block that but forces pay-per-token billing. Measured
// 2026-08-21: with no flag a planted SessionStart hook fired; with --safe-mode
// it did not.
func args(req Request, model string) []string {
	out := []string{"-p", req.Prompt, "--safe-mode", "--output-format", "json", "--model", model}
	if req.Resume != "" {
		out = append(out, "--resume", req.Resume)
	}
	if req.Workspace != "" {
		out = append(out, "--add-dir", req.Workspace)
	}
	if len(req.AllowRules) > 0 {
		out = append(out, "--allowedTools", strings.Join(req.AllowRules, ","))
	}
	if req.SystemFile != "" {
		out = append(out, "--append-system-prompt-file", req.SystemFile)
	}
	if len(req.Schema) > 0 {
		out = append(out, "--json-schema", string(req.Schema))
	}
	return out
}

// invoke runs the subprocess and returns its stdout.
func (c Claude) invoke(ctx context.Context, req Request, model string) ([]byte, error) {
	bin := c.Bin
	if bin == "" {
		bin = "claude"
	}
	cmd := exec.CommandContext(ctx, bin, args(req, model)...)
	cmd.Dir = req.Workspace

	out, err := cmd.Output()
	if err == nil {
		return out, nil
	}
	var ee *exec.ExitError
	if !asExitError(err, &ee) {
		return nil, fmt.Errorf("agent %s: run %s: %w", req.Role, bin, err)
	}
	// A non-zero exit still carries a JSON envelope explaining itself, so
	// parse before deciding. Failing on the exit code alone discards the
	// reason the agent gave.
	if len(out) == 0 {
		return nil, fmt.Errorf("agent %s: %s exited %d: %s", req.Role, bin, ee.ExitCode(), ee.Stderr)
	}
	return out, nil
}

// parseEnvelope turns the reply into a Result.
func parseEnvelope(out []byte, role Role, requested string) (Result, error) {
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil {
		return Result{}, fmt.Errorf("agent %s: parse envelope: %w", role, err)
	}
	if env.IsError {
		return Result{}, fmt.Errorf("agent %s: %s", role, env.Result)
	}
	if env.SessionID == "" {
		// The session id stamps the DevSession and the sutra thread; without
		// it a run's conversation is unfindable from its ticket.
		return Result{}, fmt.Errorf("agent %s: reply carries no session id", role)
	}
	return Result{
		SessionID:  env.SessionID,
		Model:      resolvedModel(env.ModelUsage, requested),
		Text:       env.Result,
		Structured: env.Structured,
	}, nil
}

// resolvedModel reports the model that actually ran.
//
// AC-tier-observed wants what ran, not what was asked for. modelUsage is keyed
// by the models actually billed, so it is the truthful source; the requested
// model is the fallback when the envelope omits it.
func resolvedModel(usage map[string]any, requested string) string {
	var names []string
	for name := range usage {
		names = append(names, name)
	}
	switch len(names) {
	case 0:
		return requested
	case 1:
		return names[0]
	default:
		// More than one model ran — subagents, or a fallback. Record them all
		// rather than picking one and implying it was the only one.
		return strings.Join(sorted(names), "+")
	}
}
