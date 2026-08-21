package main

import "os"

// main is intentionally empty at M1 step 1. The composition root gains the
// store, module wiring, and recovery sequencing at step 5c; the command
// surface arrives with internal/cli at step 12.
func main() {
	os.Exit(0)
}
