package main

import (
	"testing"

	"kriya/internal/agent"
	"kriya/internal/planner"
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

	// The head store's OWN dependency, checked here because it is the same
	// class of silent omission: nil Advances runs the CAS without moving the
	// completion epoch, so a supersession leaves the old plan's claim current
	// over work the new head has not built.
	heads, ok := in.Heads.(planner.SQLHeads)
	if !ok {
		t.Fatalf("the production head store is %T", in.Heads)
	}
	if heads.Advances == nil {
		t.Error("the production head store advances no completion epoch")
	}

	for name, wired := range map[string]bool{
		"snapshots": in.Snapshots != nil,
		"targets":   in.Targets != nil,
		"attempts":  in.Attempts != nil,
		"tracker":   in.Tracker != nil,
		"tickets":   in.Tickets != nil,
		"plans":     in.Plans != nil,
		"heads":     in.Heads != nil,
		"steps":     in.Steps != nil,
		"agent":     in.Agent != nil,
		"clock":     in.Now != nil,
		"verifier":  in.Verify != nil,
	} {
		if !wired {
			t.Errorf("the production intaker has no %s store", name)
		}
	}
}
