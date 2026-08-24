package agent_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kriya/internal/agent"
)

// stubClaude writes an executable standing in for `claude -p`.
//
// A real subprocess, because what is being tested is the command line: the
// --safe-mode flag that stops a target repository's hooks from executing is
// invisible to any test that stubs the Agent interface instead.
func stubClaude(t *testing.T, script string) (bin, argvFile string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "claude")
	argvFile = filepath.Join(dir, "argv")
	body := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > " +
		argvFile + "\n" + script + "\n"
	if err := os.WriteFile(bin, []byte(body), 0o700); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return bin, argvFile
}

func argv(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read argv: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}

const okEnvelope = `echo '{"session_id":"s-1","result":"done","modelUsage":{"model-b":{}}}'`

func TestEverySessionRunsInSafeMode(t *testing.T) {
	// A -p session shows no trust dialog, so without --safe-mode, building an
	// arbitrary spec runs arbitrary code from it.
	bin, argvFile := stubClaude(t, okEnvelope)
	c := agent.Claude{Bin: bin, Tiers: tiers()}
	if _, err := c.Run(context.Background(), agent.Request{Role: agent.RolePM, Prompt: "go"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := argv(t, argvFile)
	if !contains(got, "--safe-mode") {
		t.Fatalf("argv %v omits --safe-mode", got)
	}
	if !contains(got, "--model") || !contains(got, "model-b") {
		t.Errorf("argv %v does not name the resolved model", got)
	}
}

func TestOptionalFlagsAppearOnlyWhenAsked(t *testing.T) {
	bin, argvFile := stubClaude(t, okEnvelope)
	c := agent.Claude{Bin: bin, Tiers: tiers()}
	if _, err := c.Run(context.Background(), agent.Request{Role: agent.RolePM, Prompt: "go"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, flag := range []string{"--add-dir", "--allowedTools", "--append-system-prompt-file", "--json-schema"} {
		if contains(argv(t, argvFile), flag) {
			t.Errorf("%s was sent for a request that asked for nothing", flag)
		}
	}
}

func TestAFullRequestCarriesEveryFlag(t *testing.T) {
	bin, argvFile := stubClaude(t, okEnvelope)
	work := t.TempDir()
	system := filepath.Join(t.TempDir(), "system.md")
	if err := os.WriteFile(system, []byte("context"), 0o600); err != nil {
		t.Fatalf("write system file: %v", err)
	}
	c := agent.Claude{Bin: bin, Tiers: tiers()}
	_, err := c.Run(context.Background(), agent.Request{
		Role: agent.RolePM, Prompt: "go", Workspace: work,
		AllowRules: []string{"Bash(go test:*)", "Read"},
		SystemFile: system, Schema: json.RawMessage(`{"type":"object"}`),
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	got := argv(t, argvFile)
	for _, want := range []string{
		"--add-dir", work, "--allowedTools", "Bash(go test:*),Read",
		"--append-system-prompt-file", system, "--json-schema", `{"type":"object"}`,
	} {
		if !contains(got, want) {
			t.Errorf("argv %v is missing %q", got, want)
		}
	}
}

func TestAnAgentErrorEnvelopeIsAFailure(t *testing.T) {
	bin, _ := stubClaude(t, `echo '{"is_error":true,"result":"model refused","session_id":"s-1"}'`)
	c := agent.Claude{Bin: bin, Tiers: tiers()}
	_, err := c.Run(context.Background(), agent.Request{Role: agent.RolePM, Prompt: "go"})
	if err == nil || !strings.Contains(err.Error(), "model refused") {
		t.Fatalf("got %v, want the agent's own explanation", err)
	}
}

func TestAReplyWithNoSessionIDIsRejected(t *testing.T) {
	// Without it a run's conversation is unfindable from its ticket.
	bin, _ := stubClaude(t, `echo '{"result":"done"}'`)
	c := agent.Claude{Bin: bin, Tiers: tiers()}
	if _, err := c.Run(context.Background(), agent.Request{Role: agent.RolePM, Prompt: "go"}); err == nil {
		t.Fatal("a reply with no session id was accepted")
	}
}

func TestANonZeroExitStillYieldsItsEnvelope(t *testing.T) {
	// Failing on the exit code alone discards the reason the agent gave.
	bin, _ := stubClaude(t, `echo '{"is_error":true,"result":"ran out of turns","session_id":"s-1"}'
exit 1`)
	c := agent.Claude{Bin: bin, Tiers: tiers()}
	_, err := c.Run(context.Background(), agent.Request{Role: agent.RolePM, Prompt: "go"})
	if err == nil || !strings.Contains(err.Error(), "ran out of turns") {
		t.Fatalf("got %v, want the envelope's explanation rather than the exit code", err)
	}
}

func TestASilentNonZeroExitReportsTheCode(t *testing.T) {
	bin, _ := stubClaude(t, "exit 3")
	c := agent.Claude{Bin: bin, Tiers: tiers()}
	_, err := c.Run(context.Background(), agent.Request{Role: agent.RolePM, Prompt: "go"})
	if err == nil || !strings.Contains(err.Error(), "exited 3") {
		t.Fatalf("got %v, want the exit code", err)
	}
}

func TestAMissingBinaryFailsLoudly(t *testing.T) {
	c := agent.Claude{Bin: filepath.Join(t.TempDir(), "absent"), Tiers: tiers()}
	if _, err := c.Run(context.Background(), agent.Request{Role: agent.RolePM, Prompt: "go"}); err == nil {
		t.Fatal("a missing agent binary must fail")
	}
}

func TestAnUnroutableRoleNeverReachesTheBinary(t *testing.T) {
	bin, argvFile := stubClaude(t, okEnvelope)
	c := agent.Claude{Bin: bin, Tiers: tiers()}
	if _, err := c.Run(context.Background(), agent.Request{Role: agent.RolePO, Prompt: "go"}); err == nil {
		t.Fatal("a role with no tier must refuse before spending anything")
	}
	if _, err := os.Stat(argvFile); err == nil {
		t.Error("the binary ran for a role that has no configured tier")
	}
}

func TestUnparseableOutputIsAFailure(t *testing.T) {
	bin, _ := stubClaude(t, "echo not-json")
	c := agent.Claude{Bin: bin, Tiers: tiers()}
	if _, err := c.Run(context.Background(), agent.Request{Role: agent.RolePM, Prompt: "go"}); err == nil {
		t.Fatal("output that is not an envelope was accepted")
	}
}

func TestTheModelRecordedIsTheOneThatRan(t *testing.T) {
	// AC-tier-observed: what ran, not what was asked for.
	bin, _ := stubClaude(t, `echo '{"session_id":"s-1","modelUsage":{"other-model":{}}}'`)
	c := agent.Claude{Bin: bin, Tiers: tiers()}
	res, err := c.Run(context.Background(), agent.Request{Role: agent.RolePM, Prompt: "go"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Model != "other-model" {
		t.Errorf("recorded %q, want what actually billed", res.Model)
	}
}

func TestTheRequestedModelIsTheFallback(t *testing.T) {
	bin, _ := stubClaude(t, `echo '{"session_id":"s-1"}'`)
	c := agent.Claude{Bin: bin, Tiers: tiers()}
	res, err := c.Run(context.Background(), agent.Request{Role: agent.RolePM, Prompt: "go"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Model != "model-b" {
		t.Errorf("recorded %q, want the requested model when the envelope omits usage", res.Model)
	}
}

func TestSeveralModelsAreAllRecorded(t *testing.T) {
	// Picking one would imply it was the only one that ran.
	bin, _ := stubClaude(t, `echo '{"session_id":"s-1","modelUsage":{"z-model":{},"a-model":{}}}'`)
	c := agent.Claude{Bin: bin, Tiers: tiers()}
	res, err := c.Run(context.Background(), agent.Request{Role: agent.RolePM, Prompt: "go"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Model != "a-model+z-model" {
		t.Errorf("recorded %q, want every model that ran, in a stable order", res.Model)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func TestAResumedSessionContinuesTheConversation(t *testing.T) {
	// Each pair-loop round is a correction to work the SAME agent did. A
	// fresh session re-reads its own findings with no memory of what it
	// wrote, and the transcript splits across session ids so only the last
	// one reaches the thread catalog.
	bin, argvFile := stubClaude(t, okEnvelope)
	c := agent.Claude{Bin: bin, Tiers: tiers()}
	if _, err := c.Run(context.Background(), agent.Request{
		Role: agent.RoleDev, Prompt: "fix it", Resume: "s-1",
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := argv(t, argvFile)
	if !contains(got, "--resume") || !contains(got, "s-1") {
		t.Errorf("argv %v does not resume the session", got)
	}
}

func TestAnUnresumedRequestSendsNoResumeFlag(t *testing.T) {
	bin, argvFile := stubClaude(t, okEnvelope)
	c := agent.Claude{Bin: bin, Tiers: tiers()}
	if _, err := c.Run(context.Background(),
		agent.Request{Role: agent.RoleDev, Prompt: "go"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if contains(argv(t, argvFile), "--resume") {
		t.Error("a first invocation asked to resume something")
	}
}
