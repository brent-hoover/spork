package main

import (
	"testing"

	"kriya/internal/agent"
	"kriya/internal/planner"
	"kriya/internal/reviewbridge"
	"kriya/internal/workspace"
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

	// Retirement's OWN collaborators, for the same reason: every one is
	// optional, and a forgotten Tracker made an issued-but-unrecorded create
	// look like a ticket that never existed — consumed, while its real sutra
	// issue held the epic open forever.
	if in.Retire == nil {
		t.Fatal("the production intaker retires nothing")
	}
	for name, wired := range map[string]bool{
		"retirement tickets":   in.Retire.Tickets != nil,
		"retirement steps":     in.Retire.Steps != nil,
		"retirement deferrer":  in.Retire.Defer != nil,
		"retirement live work": in.Retire.Live != nil,
		"retirement tracker":   in.Retire.Tracker != nil,
	} {
		if !wired {
			t.Errorf("the production retirement has no %s", name)
		}
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

// TestThePopLoopIsWiredWithEveryCollaborator checks the other composition root.
//
// Same class of silent omission as the intaker's: every collaborator on Loop
// is optional, and a forgotten one changes nothing visible. Without Admit the
// loop pops straight past an unactivated head, and the only symptom is work
// started before its predecessor retired.
func TestThePopLoopIsWiredWithEveryCollaborator(t *testing.T) {
	db := openTemp(t)
	loop := popLoop(db, workspace.Manager{}, agent.Tiers{}, reviewbridge.Bridge{}, "/spec", "actor")

	for name, wired := range map[string]bool{
		"popper":    loop.Pops != nil,
		"builder":   loop.Build != nil,
		"ordinals":  loop.Ordinals != nil,
		"admitter":  loop.Admit != nil,
		"finisher":  loop.Finish != nil,
		"stalls":    loop.Stalls != nil,
		"epochs":    loop.Epochs != nil,
		"targetKey": loop.TargetKey != "",
	} {
		if !wired {
			t.Errorf("the production pop loop has no %s", name)
		}
	}
}
