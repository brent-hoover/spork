package main

import "testing"

// TestCLIDispatch pins the entrypoint routing: CLI subcommands never
// start the daemon (review 1817).
func TestCLIDispatch(t *testing.T) {
	for _, cmd := range []string{"init", "issue", "api"} {
		if !isCLICommand(cmd) {
			t.Fatalf("%q must dispatch to the CLI", cmd)
		}
	}
	for _, cmd := range []string{"", "-addr", "serve", "--db"} {
		if isCLICommand(cmd) {
			t.Fatalf("%q must start the daemon", cmd)
		}
	}
}
