package orchestrator_test

import (
	"testing"

	"kriya/internal/orchestrator"
)

func TestASpikeNeverEntersTheGateChain(t *testing.T) {
	// A spike's deliverable is a documented finding, not code. The gate chain
	// would fail it for having no tests — which is not a finding about the
	// risk, it is a finding about the ticket being the wrong shape.
	seen := map[orchestrator.State]bool{}
	state := orchestrator.StateResearchLoop
	for range 8 {
		transition, ok := orchestrator.Lookup(state)
		if !ok {
			break
		}
		if seen[state] {
			t.Fatalf("the research path loops at %q", state)
		}
		seen[state] = true
		if transition.Stage == orchestrator.StageGates ||
			transition.Stage == orchestrator.StageValidate {
			t.Errorf("the research path reaches %q", transition.Stage)
		}
		state = transition.OnOK
	}
	if state != orchestrator.StateFindingSubmitted {
		t.Errorf("the research path ends at %q, not awaiting a human", state)
	}
}

func TestTheResearchPathParksRatherThanLooping(t *testing.T) {
	// There is no dev loop to send a failed research stage back to: nothing
	// an agent could retry blindly. The operator sees the cause.
	transition, ok := orchestrator.Lookup(orchestrator.StateResearchLoop)
	if !ok {
		t.Fatal("researching has no transition")
	}
	if transition.OnFail != orchestrator.StateAwaitingOperator {
		t.Errorf("a failed research stage goes to %q", transition.OnFail)
	}
}

func TestFindingSubmittedWaitsForAHuman(t *testing.T) {
	// Like review-submitted: the table cannot express waiting, so the state
	// has no transition at all and an approval moves the run from outside it.
	if _, ok := orchestrator.Lookup(orchestrator.StateFindingSubmitted); ok {
		t.Error("finding-submitted has a transition; the run would not wait")
	}
}

func TestTheImplementationPathIsUnchanged(t *testing.T) {
	// The control: adding a research branch must not reroute code work.
	transition, ok := orchestrator.Lookup(orchestrator.StateQueued)
	if !ok {
		t.Fatal("queued has no transition")
	}
	if transition.OnOK != orchestrator.StateDevLoop {
		t.Errorf("a queued run now goes to %q", transition.OnOK)
	}
}
