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
	_, err := in.Decompose(context.Background(), target(), snapshotWith(twoCriteria), 1, "actor")
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

func TestRepeatingOneLayerIsStillOneLayer(t *testing.T) {
	// Counting entries rather than distinct names let ["store","store"] pass
	// as an end-to-end slice while touching exactly the one layer the rule
	// exists to refuse.
	mustReject(t, `{"tickets":[
	  {"title":"add the column","body":"","kind":"implementation","skeleton":true,
	   "criteria":["AC-valid-url"],"layers":["store","store"]}]}`,
		"touches 1 layer(s)")
}

func TestABlankLayerNameCountsForNothing(t *testing.T) {
	mustReject(t, `{"tickets":[
	  {"title":"add the column","body":"","kind":"implementation","skeleton":true,
	   "criteria":["AC-valid-url"],"layers":["store",""]}]}`,
		"touches 1 layer(s)")
}

func TestAWhitespaceLayerNameCountsForNothing(t *testing.T) {
	// The schema requires only minLength 1, so " " is a valid string and an
	// empty layer name. Checking for "" alone let it through as a second
	// layer, and a one-layer stripe passed as an end-to-end slice.
	mustReject(t, `{"tickets":[
	  {"title":"add the column","body":"","kind":"implementation","skeleton":true,
	   "criteria":["AC-valid-url"],"layers":["store","  "]}]}`,
		"touches 1 layer(s)")
}

func TestALayerIsTheSameLayerWithSpacesAroundIt(t *testing.T) {
	// Coverage compared normalized layer names against raw ones, so a stray
	// space would make " http " a layer the skeleton does not touch — and a
	// skeleton that does cover the plan would be refused.
	_, err := decomposeReply(t, `{"tickets":[
	  {"title":"skeleton","body":"","kind":"implementation","skeleton":true,
	   "criteria":["AC-valid-url"],"layers":[" http ","store"]},
	  {"title":"later","body":"","kind":"implementation",
	   "criteria":["AC-bad-url"],"layers":["http","store"]}]}`)
	if err != nil {
		t.Fatalf("a skeleton covering the plan was refused over whitespace: %v", err)
	}
}

func TestThePMIsToldTheSkeletonGatingRule(t *testing.T) {
	// A rule kriya enforces but never states is one the PM can violate while
	// obeying every instruction it was given — and decomposition has no
	// retry path, so the build simply fails.
	in, _, ag := decomposer(t, `{"tickets":[
	  {"title":"skeleton","body":"","kind":"implementation","skeleton":true,
	   "criteria":["AC-valid-url"],"layers":["http","store"]}]}`)
	if _, err := in.Decompose(context.Background(), target(), snapshotWith(twoCriteria), 1, "actor"); err != nil {
		t.Fatalf("decompose: %v", err)
	}
	prompt := ag.Requests[0].Prompt
	for _, want := range []string{"gate the skeleton", "gates every other implementation ticket"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the PM prompt never says %q", want)
		}
	}
}

func TestAPlanOfNothingButResearchIsRefused(t *testing.T) {
	// A decomposition that produces only spikes builds nothing. It would
	// reach the completion protocol having shipped no code, where it is
	// indistinguishable from a build that succeeded.
	mustReject(t, `{"tickets":[
	  {"title":"spike: one","body":"","kind":"spike","criteria":["AC-valid-url"]},
	  {"title":"spike: two","body":"","kind":"spike","criteria":["AC-bad-url"]}]}`,
		"no implementation work among them")
}

func TestASpikeMayNotBlockTheSkeletonAloneMustBlockTheRest(t *testing.T) {
	// Assigning the skeleton first is not enough: sutra SKIPS blocked work,
	// so a spike gating the skeleton while another slice is free hands that
	// other slice out first. The skeleton is then assigned first and worked
	// second — and only when a risk is in play, which is when the guarantee
	// matters most.
	mustReject(t, `{"tickets":[
	  {"title":"spike: which store","body":"","kind":"spike",
	   "criteria":["AC-valid-url"],"blocks":["AC-valid-url"]},
	  {"title":"skeleton","body":"","kind":"implementation","skeleton":true,
	   "criteria":["AC-valid-url"],"layers":["http","store"]},
	  {"title":"free slice","body":"","kind":"implementation",
	   "criteria":["AC-bad-url"],"layers":["http","store"]}]}`,
		"which would then be worked first")
}

func TestASpikeBlockingTheWholeImplementationPlanIsFine(t *testing.T) {
	// The rule is about BYPASS, not about blocking. A risk the skeleton waits
	// on is a risk the whole implementation plan waits on, and that plan is
	// legal: nothing can be worked ahead of the skeleton.
	_, err := decomposeReply(t, `{"tickets":[
	  {"title":"spike: which store","body":"","kind":"spike",
	   "criteria":["AC-valid-url"],"blocks":["AC-valid-url","AC-bad-url"]},
	  {"title":"skeleton","body":"","kind":"implementation","skeleton":true,
	   "criteria":["AC-valid-url"],"layers":["http","store"]},
	  {"title":"later slice","body":"","kind":"implementation",
	   "criteria":["AC-bad-url"],"layers":["http","store"]}]}`)
	if err != nil {
		t.Fatalf("a plan whose spike blocks all implementation work was refused: %v", err)
	}
}

func TestAnUnblockedSkeletonNeedsNoBlockingElsewhere(t *testing.T) {
	// The common shape: the skeleton is free, a spike gates later work. The
	// bypass rule must not fire here, or every risk-carrying plan is refused.
	_, err := decomposeReply(t, `{"tickets":[
	  {"title":"spike: which store","body":"","kind":"spike",
	   "criteria":["AC-bad-url"],"blocks":["AC-bad-url"]},
	  {"title":"skeleton","body":"","kind":"implementation","skeleton":true,
	   "criteria":["AC-valid-url"],"layers":["http","store"]},
	  {"title":"later slice","body":"","kind":"implementation",
	   "criteria":["AC-bad-url"],"layers":["http","store"]}]}`)
	if err != nil {
		t.Fatalf("a plan with a free skeleton and a gated later slice was refused: %v", err)
	}
}
