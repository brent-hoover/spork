package devloop_test

import (
	"slices"
	"strings"
	"testing"

	"kriya/internal/devloop"
)

func apiCommands() map[string]string {
	return map[string]string{
		"test": "go test ./...", "lint": "golangci-lint run",
		"typecheck": "go vet ./...", "arch": "arch-go",
		"coverage": "coverage.sh", "mutation": "gremlins unleash",
	}
}

func TestTheDevAgentMayRunTheCommandsItsWorkIsJudgedBy(t *testing.T) {
	rules := devloop.AllowRules(apiCommands())
	for _, want := range []string{
		"Bash(go test ./...)", "Bash(golangci-lint run)", "Bash(go vet ./...)",
		"Bash(arch-go)", "Bash(coverage.sh)", "Bash(gremlins unleash)",
	} {
		if !slices.Contains(rules, want) {
			t.Errorf("%s is not permitted: %v", want, rules)
		}
	}
}

func TestTheDevAgentMayWriteCode(t *testing.T) {
	// The SA and PO are read-only by design. The dev agent is the one role
	// that must change the worktree, so its rules are not theirs.
	rules := devloop.AllowRules(apiCommands())
	for _, want := range []string{"Read", "Write", "Edit", "Grep", "Glob"} {
		if !slices.Contains(rules, want) {
			t.Errorf("%s is not permitted: %v", want, rules)
		}
	}
}

func TestToolsIrrelevantToTheTicketAreAbsent(t *testing.T) {
	// AC-context-toolset. A bare Bash rule would permit every command on the
	// machine, which is the opposite of a ticket-scoped toolset.
	rules := devloop.AllowRules(apiCommands())
	for _, rule := range rules {
		if rule == "Bash" || strings.HasPrefix(rule, "Bash(*") {
			t.Fatalf("%q permits everything", rule)
		}
	}
	// A command belonging to a module this ticket did NOT touch is not in
	// the set, because only the touched modules' commands were passed.
	if slices.Contains(rules, "Bash(pytest)") {
		t.Error("an untouched module's command was permitted")
	}
}

func TestTheSameCommandInTwoModulesIsPermittedOnce(t *testing.T) {
	// Two Go modules commonly share `go test ./...`. A duplicate rule is not
	// wrong, but it is noise in an argument the agent is handed verbatim.
	rules := devloop.AllowRules(apiCommands(), apiCommands())
	seen := map[string]int{}
	for _, r := range rules {
		seen[r]++
	}
	for rule, n := range seen {
		if n > 1 {
			t.Errorf("%q appears %d times", rule, n)
		}
	}
}

func TestAModuleWithNoResolvedCommandsGrantsNoBashAtAll(t *testing.T) {
	// Intake refuses such a module, so reaching here means something is
	// wrong — and the safe reading of "no commands" is "no shell", never
	// "any shell".
	rules := devloop.AllowRules(map[string]string{})
	for _, rule := range rules {
		if strings.HasPrefix(rule, "Bash") {
			t.Errorf("%q was granted with no commands resolved", rule)
		}
	}
	if len(rules) == 0 {
		t.Error("the agent cannot even read the code")
	}
}

func TestABlankCommandGrantsNothing(t *testing.T) {
	rules := devloop.AllowRules(map[string]string{"test": "", "lint": "  "})
	for _, rule := range rules {
		if strings.HasPrefix(rule, "Bash") {
			t.Errorf("%q was granted for a blank command", rule)
		}
	}
}

func TestTheRulesAreStable(t *testing.T) {
	// Map iteration is random. Rules that reorder between two runs of the
	// same ticket make the agent's own invocation non-reproducible, and a
	// recorded ledger entry impossible to compare against a replay.
	first := devloop.AllowRules(apiCommands())
	for range 8 {
		if got := devloop.AllowRules(apiCommands()); !slices.Equal(first, got) {
			t.Fatalf("the order moved:\n%v\n%v", first, got)
		}
	}
}
