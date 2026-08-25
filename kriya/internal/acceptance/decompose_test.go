//go:build acceptance

package acceptance

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/cucumber/godog"

	"kriya/internal/fakes"
	"kriya/internal/planner"
)

// registerDecompose wires REQ-decompose.
func registerDecompose(sc *godog.ScenarioContext, w *world) {
	sc.Given(`^no epic exists yet for the build target$`, func() error {
		if err := w.copyLinkshort(); err != nil {
			return err
		}
		w.citeCriteria("AC-valid-url")
		if _, found, err := w.targets.Find(context.Background(), w.dir); err != nil || found {
			return fmt.Errorf("the target already has a row")
		}
		return nil
	})
	sc.Given(`^a pinned SpecSnapshot for project "([^"]*)"$`, func(string) error {
		if err := w.copyLinkshort(); err != nil {
			return err
		}
		// A plan with SHAPE, not the one-ticket reply the other scenarios
		// use: a spike, a skeleton, and a later slice reaching a third
		// layer. Every claim this scenario makes — the exemption, the
		// skeleton's coverage, its place in the pop order — is vacuous
		// against a plan holding one ticket of one kind.
		w.tracerPlan()
		return nil
	})

	sc.When(`^the first decomposition runs$`, func() error { return w.runOnce() })
	// Shared with the risk-first feature, which says the same sentence about a
	// snapshot carrying a risk. It dispatches on whichever world the
	// scenario's Given built: registering it twice would be ambiguous.
	sc.When(`^the PM agent decomposes it$`, func() error {
		if w.spike != nil {
			return w.spike.decompose()
		}
		return w.runOnce()
	})
	sc.When(`^a later decomposition supersedes the plan$`, func() error {
		// A second run under a NEW token: a deliberate re-intake, which is
		// what supersession is. The epic must not be recreated.
		w.token = "token-2"
		w.tracker.reset()
		return w.runOnce()
	})

	sc.Then(`^a BuildTarget row is written ahead of the epic's creation and records the epic id when it returns$`,
		func() error { return w.assertTargetRecordsEpic() })
	sc.Then(`^an umbrella epic is created exactly once — a recovered replay reuses the recorded row — and every ticket is parented under it$`,
		func() error { return w.assertOneEpicEveryTicketParented() })
	sc.Then(`^carried-forward and new tickets sit under the same epic with no re-parenting$`,
		func() error { return w.assertEpicUnchangedAcrossRuns() })
	sc.Then(`^every ticket's acceptance criteria cite REQ and AC ids present in the snapshot$`,
		func() error { return w.assertCitationsAreInTheSnapshot() })
	sc.Then(`^every implementation ticket is a thin end-to-end slice$`,
		func() error { return w.assertSlicesAreEndToEnd() })
	sc.Then(`^spike and research tickets are exempt from the slice rule$`,
		func() error { return w.assertSpikesAreExempt() })
	sc.Then(`^the walking skeleton — an implementation ticket touching every layer — is the first implementation ticket workable once blocking risks retire$`,
		func() error { return w.assertSkeletonLeadsTheImplementationPhase() })

	sc.Given(`^a decomposition producing tickets, relations, and assignments$`, func() error {
		if err := w.copyLinkshort(); err != nil {
			return err
		}
		w.tracerPlan()
		return nil
	})
	sc.When(`^the plan executes$`, func() error { return w.runOnce() })
	sc.Then(`^every ticket is created before any relation is wired$`,
		func() error { return w.assertPhaseOrder() })
	sc.Then(`^every relation is wired before any assignment is made$`,
		func() error { return w.assertPhaseOrder() })
	sc.Then(`^no ticket is workable before the relation phase completes$`,
		func() error { return w.assertNothingWorkableBeforeWiring() })
	sc.Then(`^completed is stamped only after the final assignment barrier$`,
		func() error { return w.assertCompletedFollowsTheLastAssignment() })
}

// tracerPlan programs the fake PM with a plan that has shape.
func (w *world) tracerPlan() {
	w.agent = fakes.NewAgent(`{"tickets":[
	  {"title":"spike: is the code space dense enough","body":"",
	   "kind":"spike","criteria":["AC-known-code"],"blocks":["AC-known-code","AC-unknown-code"]},
	  {"title":"create a short link end to end","body":"","skeleton":true,
	   "kind":"implementation","criteria":["AC-valid-url","REQ-create-link"],
	   "layers":["http","store","cli"]},
	  {"title":"resolve a short link","body":"",
	   "kind":"implementation","criteria":["AC-unknown-code","REQ-resolve-link"],
	   "layers":["http","store"]}]}`)
	w.agent.Repeat = true
}

// implementations returns the implementation tickets the plan produced.
func (w *world) implementations() []planner.Ticket {
	var out []planner.Ticket
	for _, t := range w.tickets {
		if t.Kind == planner.KindImplementation {
			out = append(out, t)
		}
	}
	return out
}

func (w *world) assertSlicesAreEndToEnd() error {
	impls := w.implementations()
	if len(impls) < 2 {
		return fmt.Errorf("the plan holds %d implementation tickets: too few to tell a slice from a plan", len(impls))
	}
	for _, t := range impls {
		// END-TO-END is the checkable half: a ticket naming one layer is a
		// horizontal stripe. THIN is not mechanically checkable and kriya
		// does not pretend to check it — it instructs the PM and stops there.
		if len(t.Layers) < 2 {
			return fmt.Errorf("implementation ticket %q names layers %v: that is a layer, not a slice",
				t.Title, t.Layers)
		}
	}
	return nil
}

func (w *world) assertSpikesAreExempt() error {
	var spikes int
	for _, t := range w.tickets {
		if t.Kind != planner.KindSpike {
			continue
		}
		spikes++
		// The exemption is only observable on a spike that would FAIL the
		// slice rule. One that happened to name two layers proves nothing.
		if len(t.Layers) >= 2 {
			return fmt.Errorf("spike %q names %d layers, so its exemption was never exercised",
				t.Title, len(t.Layers))
		}
		if t.Skeleton {
			return fmt.Errorf("spike %q is marked the walking skeleton", t.Title)
		}
	}
	if spikes == 0 {
		return fmt.Errorf("the plan holds no spike, so the exemption was never exercised")
	}
	return nil
}

func (w *world) assertSkeletonLeadsTheImplementationPhase() error {
	var skeleton planner.Ticket
	layers := map[string]bool{}
	for _, t := range w.implementations() {
		for _, l := range t.Layers {
			layers[l] = true
		}
		if t.Skeleton {
			if skeleton.Title != "" {
				return fmt.Errorf("two tickets are the walking skeleton")
			}
			skeleton = t
		}
	}
	if skeleton.Title == "" {
		return fmt.Errorf("no implementation ticket is the walking skeleton")
	}
	// "touching every layer" — every layer the PLAN touches. A layer only a
	// later ticket reaches is one whose wiring nothing has demonstrated.
	for l := range layers {
		if !slices.Contains(skeleton.Layers, l) {
			return fmt.Errorf("the skeleton %q does not touch layer %q, which the plan does", skeleton.Title, l)
		}
	}
	// "the FIRST implementation ticket workable": sutra offers assigned work
	// FIFO, so being first is a fact about assignment order, not about the
	// order the PM happened to list its tickets in.
	first := ""
	for _, issue := range w.tracker.assignOrder {
		if title := w.tracker.titleOf(issue); title != "" && w.kindOf(title) == planner.KindImplementation {
			first = title
			break
		}
	}
	if first != skeleton.Title {
		return fmt.Errorf("the first implementation ticket assigned was %q, not the skeleton %q",
			first, skeleton.Title)
	}
	return nil
}

// kindOf names the kind of the ticket with a title.
func (w *world) kindOf(title string) string {
	for _, t := range w.tickets {
		if t.Title == title {
			return t.Kind
		}
	}
	return ""
}

// assertPhaseOrder requires the plan to have visited each phase exactly once.
//
// One assertion serves both barrier steps because they are one fact: a phase
// that repeats is a phase that was interleaved with the next, and that is
// what both sentences forbid from their own side.
func (w *world) assertPhaseOrder() error {
	got := w.tracker.phases()
	want := []string{"create", "relation", "assign"}
	if !slices.Equal(got, want) {
		return fmt.Errorf("the plan ran phases %v, want %v — a repeated phase is an interleaved one", got, want)
	}
	return nil
}

func (w *world) assertNothingWorkableBeforeWiring() error {
	// Assignment is what makes a ticket poppable, so "workable" is a claim
	// about assignment: nothing may be assigned until the last blocking
	// relation is wired.
	lastRelation, firstAssign := -1, -1
	for n, c := range w.tracker.calls {
		if c == "relation" {
			lastRelation = n
		}
		if c == "assign" && firstAssign < 0 {
			firstAssign = n
		}
	}
	if lastRelation < 0 {
		return fmt.Errorf("no blocking relation was wired, so the barrier was never under test")
	}
	if firstAssign < 0 {
		return fmt.Errorf("nothing was assigned, so nothing was ever workable")
	}
	if firstAssign < lastRelation {
		return fmt.Errorf("a ticket was assigned at step %d, before the relation phase finished at step %d",
			firstAssign, lastRelation)
	}
	return nil
}

func (w *world) assertCompletedFollowsTheLastAssignment() error {
	plan, found, err := w.plans.Find(context.Background(), w.dir)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("no plan row for %s", w.dir)
	}
	if plan.State != planner.PlanCompleted {
		return fmt.Errorf("the plan is %q, not %q", plan.State, planner.PlanCompleted)
	}
	// The stamp is what arms completion detection, so it must count the whole
	// ticket set — a stamp carrying fewer tickets than were assigned would
	// arm detection against a plan it cannot see all of.
	if plan.Tickets != len(w.tickets) {
		return fmt.Errorf("the stamp records %d tickets but %d were filed", plan.Tickets, len(w.tickets))
	}
	if len(w.tracker.assignOrder) != len(w.tickets) {
		return fmt.Errorf("%d tickets but %d assignments: the final barrier was stamped over incomplete work",
			len(w.tickets), len(w.tracker.assignOrder))
	}
	return nil
}

// runOnce points kriya at the project and requires it to succeed.
func (w *world) runOnce() error {
	if err := w.run(); err != nil {
		return err
	}
	if w.err != nil {
		return fmt.Errorf("build failed: %w", w.err)
	}
	return nil
}

func (w *world) target() (planner.BuildTarget, error) {
	t, found, err := w.targets.Find(context.Background(), w.dir)
	if err != nil {
		return planner.BuildTarget{}, err
	}
	if !found {
		return planner.BuildTarget{}, fmt.Errorf("no BuildTarget row for %s", w.dir)
	}
	return t, nil
}

func (w *world) assertTargetRecordsEpic() error {
	t, err := w.target()
	if err != nil {
		return err
	}
	if t.EpicID == "" {
		return fmt.Errorf("the row records no epic id")
	}
	if t.EpicState != planner.EpicCreated {
		return fmt.Errorf("epic_state is %q, want %q", t.EpicState, planner.EpicCreated)
	}
	// Written AHEAD: the row must carry everything a replay needs to present
	// the same idempotency key, or recovery creates a duplicate instead of
	// replaying.
	for name, value := range map[string]string{
		"project key": t.ProjectKey, "name": t.Name, "actor": t.Actor, "spec hash": t.SpecHash,
	} {
		if value == "" {
			return fmt.Errorf("the row records no %s, so a replay could not rebuild its call", name)
		}
	}
	return nil
}

func (w *world) assertOneEpicEveryTicketParented() error {
	if w.tracker.epics != 1 {
		return fmt.Errorf("created %d epics, want exactly one", w.tracker.epics)
	}
	tickets := w.tracker.issues - w.tracker.epics
	if tickets < 1 {
		return fmt.Errorf("no tracer tickets were created")
	}
	// One parent_of per ticket, and no more: a ticket parented twice would
	// bump the epic's subtree revision for nothing.
	if w.tracker.relations != tickets {
		return fmt.Errorf("%d tickets but %d parent relations", tickets, w.tracker.relations)
	}
	return nil
}

func (w *world) assertEpicUnchangedAcrossRuns() error {
	t, err := w.target()
	if err != nil {
		return err
	}
	if t.EpicID != w.firstEpicID {
		return fmt.Errorf("the epic changed across runs: %q then %q", w.firstEpicID, t.EpicID)
	}
	// No re-parenting of the epic itself, and no second epic.
	if w.tracker.epics != 0 {
		return fmt.Errorf("the later decomposition created %d epics", w.tracker.epics)
	}
	return nil
}

func (w *world) assertCitationsAreInTheSnapshot() error {
	snap, err := w.store.only()
	if err != nil {
		return err
	}
	manifest, ok := snap.Content["avspec.yaml"]
	if !ok {
		return fmt.Errorf("the snapshot holds no manifest to cite against")
	}
	if len(w.tickets) == 0 {
		return fmt.Errorf("no tickets were produced")
	}
	for _, t := range w.tickets {
		if len(t.Criteria) == 0 {
			return fmt.Errorf("ticket %q cites nothing", t.Title)
		}
		for _, id := range t.Criteria {
			// Against the SNAPSHOT, not the working tree: an edit after
			// intake must not retroactively make a bad citation look valid.
			if !strings.Contains(manifest, id) {
				return fmt.Errorf("ticket %q cites %q, absent from the pinned manifest", t.Title, id)
			}
		}
	}
	return nil
}
