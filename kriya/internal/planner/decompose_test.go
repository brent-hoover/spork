package planner_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"kriya/internal/fakes"
	"kriya/internal/planner"
)

func snapshotWith(manifest string) planner.Snapshot {
	return planner.Snapshot{
		Hash:    "hash1234567890abcdef",
		Content: map[string]string{"avspec.yaml": manifest},
	}
}

const twoCriteria = `
requirements:
  - id: REQ-create
    acceptance:
      - id: AC-valid-url
      - id: AC-bad-url
`

func decomposer(t *testing.T, reply string) (planner.Intaker, *countingTracker, *fakes.Agent) {
	t.Helper()
	tr := &countingTracker{}
	ag := fakes.NewAgent(reply)
	return planner.Intaker{Targets: newMemTargets(), Tracker: tr, Agent: ag}, tr, ag
}

func target() planner.BuildTarget {
	return planner.BuildTarget{
		TargetKey: "/spec", SpecHash: "hash1234567890abcdef",
		ProjectID: "p1", EpicID: "e1", EpicState: planner.EpicCreated,
	}
}

func TestTicketsAreCreatedAndParentedUnderTheEpic(t *testing.T) {
	in, tr, _ := decomposer(t, `{"tickets":[
		{"title":"walking skeleton","body":"thin slice","kind":"implementation","skeleton":true,"criteria":["AC-valid-url"],"layers":["http","store"]},
		{"title":"reject bad urls","body":"","kind":"implementation","criteria":["AC-bad-url"],"layers":["http","store"]}]}`)
	tickets, err := in.Decompose(context.Background(), target(), snapshotWith(twoCriteria), "actor")
	if err != nil {
		t.Fatalf("decompose: %v", err)
	}
	if len(tickets) != 2 {
		t.Fatalf("expected two tickets, got %d", len(tickets))
	}
	if tr.issues != 2 {
		t.Errorf("expected two issues created, got %d", tr.issues)
	}
	if tr.relations != 2 {
		t.Errorf("every ticket must be parented under the epic; got %d relations", tr.relations)
	}
}

func TestACitationOutsideTheSnapshotIsRejected(t *testing.T) {
	// "every ticket's acceptance criteria cite REQ and AC ids present in the
	// snapshot". An invented id produces a ticket whose completion can never
	// be verified against anything, so it must not reach the tracker.
	in, tr, _ := decomposer(t, `{"tickets":[
		{"title":"invented","body":"","kind":"implementation","criteria":["AC-does-not-exist"],"layers":["http","store"]}]}`)
	_, err := in.Decompose(context.Background(), target(), snapshotWith(twoCriteria), "actor")
	if err == nil {
		t.Fatal("a citation absent from the snapshot must be rejected")
	}
	if !strings.Contains(err.Error(), "AC-does-not-exist") {
		t.Errorf("the error should name the bad citation, got %v", err)
	}
	if tr.issues != 0 {
		t.Errorf("nothing may be created when a citation is invalid; got %d issues", tr.issues)
	}
}

func TestTheAgentIsAskedForStructuredOutput(t *testing.T) {
	// Parsing an agent's prose is guessing, and a guess about which criteria a
	// ticket covers is a build that verifies the wrong thing.
	in, _, ag := decomposer(t, `{"tickets":[{"title":"t","body":"","kind":"implementation","skeleton":true,"criteria":["AC-bad-url"],"layers":["http","store"]}]}`)
	if _, err := in.Decompose(context.Background(), target(), snapshotWith(twoCriteria), "actor"); err != nil {
		t.Fatalf("decompose: %v", err)
	}
	req := ag.Requests[0]
	if len(req.Schema) == 0 {
		t.Error("the PM must be constrained by a schema")
	}
	if req.Role != "pm" {
		t.Errorf("role should be pm, got %q", req.Role)
	}
	// The prompt must carry the manifest CONTENT, not a path: the agent has to
	// decompose the snapshot, and a path would let it read a working tree that
	// may already differ from what intake validated.
	if !strings.Contains(req.Prompt, "AC-valid-url") {
		t.Error("the prompt should carry the snapshot's criteria")
	}
}

func TestASnapshotWithNoCriteriaCannotBeDecomposed(t *testing.T) {
	in, _, _ := decomposer(t, `{"tickets":[]}`)
	_, err := in.Decompose(context.Background(), target(), snapshotWith("project: {}\n"), "actor")
	if err == nil {
		t.Fatal("a snapshot citing no criteria has nothing to decompose against")
	}
}

func TestEveryTicketIsAssignedToTheActorThatWillPopIt(t *testing.T) {
	// sutra's work stack offers only issues ASSIGNED to the popping identity.
	// An unassigned ticket is one nothing ever claims: the build decomposes,
	// reports its tickets, and idles forever.
	in, tr, _ := decomposer(t, `{"tickets":[
		{"title":"walking skeleton","body":"","kind":"implementation","skeleton":true,"criteria":["AC-valid-url"],"layers":["http","store"]},
		{"title":"reject bad urls","body":"","kind":"implementation","criteria":["AC-bad-url"],"layers":["http","store"]}]}`)
	tickets, err := in.Decompose(context.Background(), target(), snapshotWith(twoCriteria), "actor")
	if err != nil {
		t.Fatalf("decompose: %v", err)
	}
	if len(tr.assigned) != len(tickets) {
		t.Fatalf("assigned %d of %d tickets", len(tr.assigned), len(tickets))
	}
	for _, assignee := range tr.assigned {
		if assignee != "actor" {
			t.Errorf("a ticket was assigned to %q", assignee)
		}
	}
}

func TestATicketThatCannotBeAssignedFailsDecomposition(t *testing.T) {
	// Reporting success would leave a ticket nothing can ever pop, which looks
	// exactly like a plan whose work is blocked.
	in, tr, _ := decomposer(t, `{"tickets":[
		{"title":"walking skeleton","body":"","kind":"implementation","skeleton":true,"criteria":["AC-valid-url"],"layers":["http","store"]}]}`)
	tr.assignErr = errors.New("tracker unavailable")
	if _, err := in.Decompose(context.Background(), target(),
		snapshotWith(twoCriteria), "actor"); err == nil {
		t.Fatal("a ticket nothing can pop read as decomposed")
	}
}
