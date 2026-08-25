//go:build acceptance

package acceptance

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cucumber/godog"

	"kriya/internal/fakes"
	"kriya/internal/planner"
)

// spikeWorld is one risk-first scenario's state.
type spikeWorld struct {
	tracker *recordingTracker
	tickets []planner.Ticket
	// queue is the identity's work stack in the order sutra would offer it.
	queue []string
	err   error
}

func (w *world) newSpike() *spikeWorld {
	s := &spikeWorld{}
	w.spike = s
	return s
}

// spikeSnapshot carries a risk and two requirements that depend on its answer.
const spikeSnapshot = `
requirements:
  - id: REQ-review
    acceptance:
      - id: AC-headless
      - id: AC-verdict
  - id: REQ-create
    acceptance:
      - id: AC-valid-url
`

// spikeDecomposition is what the PM returns for that snapshot.
const spikeDecomposition = `{"tickets":[
  {"title":"spike: can the review tool run headless","body":"",
   "kind":"spike","criteria":["AC-headless"],"blocks":["AC-headless","AC-verdict"]},
  {"title":"drive the review tool","body":"","skeleton":true,
   "kind":"implementation","criteria":["AC-verdict"],"layers":["http","store"]},
  {"title":"create a short link","body":"",
   "kind":"implementation","criteria":["AC-valid-url"],"layers":["http","store"]}]}`

// decompose runs a real decomposition over the risk snapshot.
func (s *spikeWorld) decompose() error {
	s.tracker = newRecordingTracker()
	in := planner.Intaker{
		Targets: &memTargets{rows: map[string]planner.BuildTarget{}},
		Tracker: s.tracker,
		Agent:   fakes.NewAgent(spikeDecomposition),
	}
	target := planner.BuildTarget{
		TargetKey: "/spec", SpecHash: "hash1234567890abcdef",
		ProjectID: "p1", EpicID: "epic-1", EpicState: planner.EpicCreated,
	}
	snap := planner.Snapshot{
		Hash:    "hash1234567890abcdef",
		Content: map[string]string{"avspec.yaml": spikeSnapshot},
	}
	s.tickets, s.err = in.Decompose(context.Background(), target, snap, "actor-1")
	return s.err
}

// byKind returns the issue ids of one kind of ticket.
func (s *spikeWorld) byKind(kind string) []string {
	var out []string
	for _, t := range s.tickets {
		if t.Kind == kind {
			out = append(out, t.IssueID)
		}
	}
	return out
}

// forCriterion returns the issue covering a criterion.
func (s *spikeWorld) forCriterion(id string) string {
	for _, t := range s.tickets {
		for _, c := range t.Criteria {
			if c == id {
				return t.IssueID
			}
		}
	}
	return ""
}

func registerSpikes(sc *godog.ScenarioContext, w *world) {
	sc.Step(`^a snapshot whose plan carries risk "([^"]*)" with two dependent requirements$`,
		func(string) error {
			w.newSpike()
			return nil
		})

	sc.Step(`^a spike ticket exists for the risk labeled per the risk/spike convention$`,
		func() error {
			s := w.spike
			spikes := s.byKind(planner.KindSpike)
			if len(spikes) != 1 {
				return fmt.Errorf("the decomposition produced %d spikes", len(spikes))
			}
			// The KIND is the convention: it decides which path the ticket
			// takes, and a spike carries a documented finding rather than
			// code — so it never enters the gate chain at all.
			for _, t := range s.tickets {
				if t.Kind == planner.KindSpike && !strings.Contains(t.Title, "spike") {
					return fmt.Errorf("the spike is titled %q", t.Title)
				}
			}
			return nil
		})

	sc.Step(`^the spike blocks every ticket depending on the risk's answer$`, func() error {
		s := w.spike
		spike := s.byKind(planner.KindSpike)[0]
		dependent := s.forCriterion("AC-verdict")
		if dependent == "" {
			return errors.New("the scenario has no dependent ticket")
		}
		if !s.tracker.blocks(spike, dependent) {
			return fmt.Errorf("the spike does not block %s: %v", dependent, s.tracker.wired)
		}
		return nil
	})

	sc.Step(`^tickets not depending on the risk are not blocked by it$`, func() error {
		s := w.spike
		spike := s.byKind(planner.KindSpike)[0]
		independent := s.forCriterion("AC-valid-url")
		if independent == "" {
			return errors.New("the scenario has no independent ticket")
		}
		// Blocking everything would serialize a plan that has parallel work
		// in it, which is the opposite of what risk-first buys.
		if s.tracker.blocks(spike, independent) {
			return errors.New("a ticket depending on no risk was blocked anyway")
		}
		// And a self-block would be a ticket nothing can ever pop.
		if s.tracker.blocks(spike, spike) {
			return errors.New("the spike blocks itself")
		}
		return nil
	})

	registerSpikeOrdering(sc, w)
}

func registerSpikeOrdering(sc *godog.ScenarioContext, w *world) {
	sc.Step(`^decomposition produced a mixed set of spike and implementation tickets$`,
		func() error {
			s := w.newSpike()
			if err := s.decompose(); err != nil {
				return err
			}
			if len(s.byKind(planner.KindSpike)) == 0 ||
				len(s.byKind(planner.KindImplementation)) == 0 {
				return errors.New("the decomposition is not a mixed set")
			}
			return nil
		})

	sc.Step(`^the assignment phase runs$`, func() error {
		// It ran inside the decomposition: assignment is the phase that makes
		// a ticket poppable, and it happens only once the whole plan exists
		// and is wired.
		if len(w.spike.tracker.assignOrder) == 0 {
			return errors.New("nothing was assigned")
		}
		return nil
	})

	sc.Step(`^every spike assignment completes before any implementation assignment begins$`,
		func() error {
			s := w.spike
			spikes := map[string]bool{}
			for _, id := range s.byKind(planner.KindSpike) {
				spikes[id] = true
			}
			seenImplementation := false
			for _, issue := range s.tracker.assignOrder {
				if spikes[issue] {
					if seenImplementation {
						return fmt.Errorf(
							"a spike was assigned after implementation work: %v",
							s.tracker.assignOrder)
					}
					continue
				}
				seenImplementation = true
			}
			return nil
		})

	sc.Step(`^an older assignment from another plan already sits in the identity's queue$`,
		func() error {
			s := w.spike
			// FIFO, so it is ahead of everything this plan assigned.
			s.queue = append([]string{"issue-from-another-plan"}, s.tracker.assignOrder...)
			return nil
		})

	sc.Step(`^an agent pops work under the tracker's FIFO ordering$`, func() error {
		if len(w.spike.queue) == 0 {
			return errors.New("the queue is empty")
		}
		return nil
	})

	sc.Step(`^the pre-existing independent ticket may pop first, which is safe — it depends on none of this plan's risks$`,
		func() error {
			if w.spike.queue[0] != "issue-from-another-plan" {
				return fmt.Errorf("the queue leads with %q", w.spike.queue[0])
			}
			return nil
		})

	sc.Step(`^among this plan's tickets the spike pops ahead of its implementation work$`,
		func() error {
			s := w.spike
			spikes := map[string]bool{}
			for _, id := range s.byKind(planner.KindSpike) {
				spikes[id] = true
			}
			mine := map[string]bool{}
			for _, t := range s.tickets {
				mine[t.IssueID] = true
			}
			seenImplementation := false
			for _, issue := range s.queue {
				if !mine[issue] {
					// Another plan's work. Safe in any position: it depends on
					// none of this plan's risks.
					continue
				}
				if spikes[issue] {
					if seenImplementation {
						return fmt.Errorf("the spike pops after implementation work: %v", s.queue)
					}
					continue
				}
				seenImplementation = true
			}
			return nil
		})

	sc.Step(`^a ticket depending on the risk cannot pop at all while the spike is open, regardless of queue position$`,
		func() error {
			s := w.spike
			spike := s.byKind(planner.KindSpike)[0]
			dependent := s.forCriterion("AC-verdict")
			// The RELATION, not the ordering, is what guarantees this: sutra
			// does not offer blocked work whatever its queue position, so an
			// ordering accident can never start dependent work early.
			if !s.tracker.blocks(spike, dependent) {
				return errors.New("the dependent is not blocked by relation")
			}
			return nil
		})

	sc.Step(`^no tracker-side priority mechanism is assumed$`, func() error {
		s := w.spike
		// Everything that made this work is ORDER and RELATIONS. If kriya had
		// asked sutra to prioritise, there would be a call for it.
		for _, r := range s.tracker.wired {
			if r.kind != "parent_of" && r.kind != "blocks" {
				return fmt.Errorf("the plan used relation kind %q", r.kind)
			}
		}
		return nil
	})
}
