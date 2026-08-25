package main

import (
	"testing"

	"kriya/internal/agent"
)

// TestTheIntakerIsWiredWithEveryDurableStore checks the COMPOSITION ROOT.
//
// Every store on the Intaker is optional — nil means "skip this", which is
// what a module-level test of decomposition's own phases wants. That makes
// forgetting one in production silent: decomposition still succeeds, and the
// only symptom is a plan nobody recorded, a head nobody claimed, or a ticket
// set completion detection cannot see.
//
// It asserts against what intaker() BUILDS, never against a struct the test
// populated itself — a test that filled the fields and then checked them
// would pass with the production wiring deleted.
func TestTheIntakerIsWiredWithEveryDurableStore(t *testing.T) {
	db := openTemp(t)
	in := intaker(db, agent.Tiers{}, "/spec")

	for name, wired := range map[string]bool{
		"snapshots": in.Snapshots != nil,
		"targets":   in.Targets != nil,
		"attempts":  in.Attempts != nil,
		"tracker":   in.Tracker != nil,
		"tickets":   in.Tickets != nil,
		"plans":     in.Plans != nil,
		"heads":     in.Heads != nil,
		"agent":     in.Agent != nil,
		"clock":     in.Now != nil,
		"verifier":  in.Verify != nil,
	} {
		if !wired {
			t.Errorf("the production intaker has no %s store", name)
		}
	}
}
