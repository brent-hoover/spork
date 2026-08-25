package planner_test

import (
	"context"
	"strings"
	"testing"
)

// decomposeReply runs one PM reply through decomposition and returns its error.
func decomposeReply(t *testing.T, reply string) (*countingTracker, error) {
	t.Helper()
	in, tr, _ := decomposer(t, reply)
	_, err := in.Decompose(context.Background(), target(), snapshotWith(twoCriteria), "actor")
	return tr, err
}

// mustReject requires decomposition to refuse a reply, naming why.
func mustReject(t *testing.T, reply, want string) {
	t.Helper()
	_, err := decomposeReply(t, reply)
	if err == nil {
		t.Fatalf("the reply was accepted; want it refused for %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("refused with %q, which does not mention %q", err, want)
	}
}

func TestAnImplementationTicketTouchingOneLayerIsNotASlice(t *testing.T) {
	// "every implementation ticket is a thin end-to-end slice". A ticket
	// naming one layer is the horizontal decomposition tracer bullets exist
	// to prevent — "add the column" ships nothing a user can see.
	mustReject(t, `{"tickets":[
	  {"title":"add the column","body":"","kind":"implementation","skeleton":true,
	   "criteria":["AC-valid-url"],"layers":["store"]}]}`,
		"a slice that touches one layer is a layer")
}

func TestASpikeIsExemptFromTheSliceRule(t *testing.T) {
	// "spike and research tickets are exempt from the slice rule": a spike's
	// deliverable is a finding, so it cuts through no layers and naming some
	// would be a lie.
	_, err := decomposeReply(t, `{"tickets":[
	  {"title":"spike: is it headless","body":"","kind":"spike","criteria":["AC-bad-url"]},
	  {"title":"slice","body":"","kind":"implementation","skeleton":true,
	   "criteria":["AC-valid-url"],"layers":["http","store"]}]}`)
	if err != nil {
		t.Fatalf("a layerless spike was refused: %v", err)
	}
}

func TestAPlanWithNoWalkingSkeletonIsRefused(t *testing.T) {
	// Without one, "the first implementation ticket workable" names nothing.
	mustReject(t, `{"tickets":[
	  {"title":"a","body":"","kind":"implementation","criteria":["AC-valid-url"],"layers":["http","store"]},
	  {"title":"b","body":"","kind":"implementation","criteria":["AC-bad-url"],"layers":["http","store"]}]}`,
		"no ticket is the walking skeleton")
}

func TestTwoWalkingSkeletonsAreRefused(t *testing.T) {
	// "THE walking skeleton". Two of them is two claims about which slice
	// proves the stack connects, and the FIFO order can only honour one.
	mustReject(t, `{"tickets":[
	  {"title":"a","body":"","kind":"implementation","skeleton":true,"criteria":["AC-valid-url"],"layers":["http","store"]},
	  {"title":"b","body":"","kind":"implementation","skeleton":true,"criteria":["AC-bad-url"],"layers":["http","store"]}]}`,
		"both the walking skeleton")
}

func TestASpikeCannotBeTheWalkingSkeleton(t *testing.T) {
	// The skeleton is the slice that proves the stack connects. A spike
	// produces a document and touches no layer, so it can prove nothing.
	mustReject(t, `{"tickets":[
	  {"title":"spike","body":"","kind":"spike","skeleton":true,"criteria":["AC-bad-url"]},
	  {"title":"a","body":"","kind":"implementation","skeleton":true,"criteria":["AC-valid-url"],"layers":["http","store"]}]}`,
		"cannot be the walking skeleton")
}

func TestTheSkeletonMustTouchEveryLayerThePlanTouches(t *testing.T) {
	// "an implementation ticket touching every layer". A layer some later
	// ticket reaches and the skeleton does not is a layer whose wiring
	// nothing has demonstrated before the plan depends on it.
	mustReject(t, `{"tickets":[
	  {"title":"skeleton","body":"","kind":"implementation","skeleton":true,
	   "criteria":["AC-valid-url"],"layers":["http","store"]},
	  {"title":"later","body":"","kind":"implementation",
	   "criteria":["AC-bad-url"],"layers":["http","queue"]}]}`,
		`does not touch layer "queue"`)
}

func TestTheWalkingSkeletonIsAssignedFirst(t *testing.T) {
	// sutra offers assigned work FIFO, so assignment order IS pop order.
	// The skeleton is declared LAST here: an implementation that just walked
	// the slice in order would pass while proving nothing.
	tr, err := decomposeReply(t, `{"tickets":[
	  {"title":"later","body":"","kind":"implementation",
	   "criteria":["AC-bad-url"],"layers":["http","store"]},
	  {"title":"skeleton","body":"","kind":"implementation","skeleton":true,
	   "criteria":["AC-valid-url"],"layers":["http","store"]}]}`)
	if err != nil {
		t.Fatalf("decompose: %v", err)
	}
	if len(tr.assignOrder) != 2 {
		t.Fatalf("assigned %v, want both tickets", tr.assignOrder)
	}
	if got := tr.titleOf(tr.assignOrder[0]); got != "skeleton" {
		t.Errorf("the first assignment was %q, want the walking skeleton", got)
	}
}
