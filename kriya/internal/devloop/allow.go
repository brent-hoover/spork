package devloop

import (
	"slices"
	"sort"
	"strings"
)

// editRules are the tools the dev agent needs to change code.
//
// The SA and PO are read-only by design; this role is the one that must write,
// so its rules are deliberately not theirs.
var editRules = []string{"Read", "Write", "Edit", "Grep", "Glob"}

// AllowRules is the permission set for one ticket's dev agent.
//
// Generated from the TOUCHED modules' resolved commands, so what the agent may
// run is the ticket's business rather than the machine's. There is no bare
// Bash rule: "tools irrelevant to the ticket are absent" has to be
// mechanically true, and a wildcard permits every command that exists.
//
// A module that resolved no commands grants no shell at all. Intake refuses
// such a module, so reaching here means something is already wrong, and the
// safe reading of "no commands" is "no shell" rather than "any shell".
func AllowRules(commands ...map[string]string) []string {
	var bash []string
	for _, module := range commands {
		for _, command := range module {
			command = strings.TrimSpace(command)
			if command == "" {
				continue
			}
			rule := "Bash(" + command + ")"
			if !slices.Contains(bash, rule) {
				bash = append(bash, rule)
			}
		}
	}
	// Sorted because map iteration is random, and rules that reorder between
	// two runs of the same ticket make the invocation irreproducible.
	sort.Strings(bash)
	return append(append([]string(nil), editRules...), bash...)
}
