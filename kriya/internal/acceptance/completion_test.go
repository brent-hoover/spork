//go:build acceptance

package acceptance

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cucumber/godog"

	"kriya/internal/fakes"
	"kriya/internal/orchestrator"
	"kriya/internal/planner"
)

// detectWorld is one completion-detection scenario's state.
type detectWorld struct {
	plans    *memPlans
	tickets  *memPlanTickets
	live     *liveQueue
	stalls   *memStalls
	detector planner.Detector
	got      planner.Detection
	err      error
	// recorded is the stall this scenario produced, if any.
	recorded orchestrator.Stall
}

// memPlans holds plan rows.
type memPlans struct{ rows map[string]planner.Plan }

func (m *memPlans) Upsert(_ context.Context, p planner.Plan) error {
	m.rows[p.TargetKey] = p
	return nil
}

func (m *memPlans) Find(_ context.Context, key string) (planner.Plan, bool, error) {
	p, ok := m.rows[key]
	return p, ok, nil
}

func (m *memPlans) ByKey(_ context.Context, key string) (planner.Plan, bool, error) {
	for _, p := range m.rows {
		if p.Key == key {
			return p, true, nil
		}
	}
	return planner.Plan{}, false, nil
}

// memPlanTickets holds a target's planned ticket set.
type memPlanTickets struct{ rows []planner.Ticket }

func (m *memPlanTickets) ForTarget(context.Context, string) ([]planner.Ticket, error) {
	return m.rows, nil
}

// liveQueue is what the tracker currently holds, which is the universe
// completion is judged against — never kriya's own idea of it.
type liveQueue struct {
	rows      []planner.LiveIssue
	watermark string
}

func (l *liveQueue) Active(context.Context, string) ([]planner.LiveIssue, string, error) {
	return l.rows, l.watermark, nil
}

// memStalls holds the operator inbox's stall rows.
type memStalls struct{ rows map[string]orchestrator.Stall }

func (m *memStalls) Insert(_ context.Context, s orchestrator.Stall) error {
	if existing, ok := m.rows[s.Key]; ok {
		existing.State, existing.Resolved = orchestrator.StallOpen, time.Time{}
		m.rows[s.Key] = existing
		return nil
	}
	m.rows[s.Key] = s
	return nil
}

func (m *memStalls) Resolve(_ context.Context, key string, at time.Time) error {
	s, ok := m.rows[key]
	if !ok {
		return nil
	}
	s.State, s.Resolved = orchestrator.StallResolved, at
	m.rows[key] = s
	return nil
}

func (m *memStalls) Open(context.Context) ([]orchestrator.Stall, error) {
	var out []orchestrator.Stall
	for _, s := range m.rows {
		if s.State == orchestrator.StallOpen {
			out = append(out, s)
		}
	}
	return out, nil
}

func (w *world) newDetect() *detectWorld {
	d := &detectWorld{
		plans:   &memPlans{rows: map[string]planner.Plan{}},
		tickets: &memPlanTickets{},
		live:    &liveQueue{watermark: "w-1"},
		stalls:  &memStalls{rows: map[string]orchestrator.Stall{}},
	}
	d.detector = planner.Detector{Plans: d.plans, Tickets: d.tickets, Issues: d.live}
	w.detect = d
	return d
}

// plan seeds a target's plan in a given state with a ticket set.
func (d *detectWorld) plan(completed bool, issues ...string) error {
	d.tickets.rows = nil
	for _, id := range issues {
		d.tickets.rows = append(d.tickets.rows, planner.Ticket{Title: id, IssueID: id})
	}
	return d.plans.Upsert(context.Background(), planner.Plan{
		Key: "plan-key", TargetKey: "/spec", SpecHash: "hash1",
		State: planner.PlanActive, Completed: completed, Tickets: len(issues),
	})
}

func (d *detectWorld) detect() error {
	d.got, d.err = d.detector.Detect(context.Background(), "/spec", "project-1")
	return d.err
}

// refused asserts detection did not arm and named the cause.
func (d *detectWorld) refused(naming ...string) error {
	if d.err != nil {
		return d.err
	}
	if d.got.Armed {
		return errors.New("completion armed")
	}
	for _, want := range naming {
		if !strings.Contains(d.got.Reason, want) {
			return fmt.Errorf("the reason %q does not name %q", d.got.Reason, want)
		}
	}
	return nil
}

func registerCompletionDetection(sc *godog.ScenarioContext, w *world) {
	sc.Step(`^an activated head plan still mid-phase, without its completed stamp$`, func() error {
		d := w.newDetect()
		return d.plan(false, "issue-1")
	})

	sc.Step(`^every ticket created so far is complete$`, func() error {
		// Complete means ABSENT from the active queue: the tracker's own
		// answer, not a flag kriya keeps.
		w.detect.live.rows = nil
		return nil
	})

	sc.Step(`^completion detection runs$`, func() error {
		_ = w.detect.detect()
		return nil
	})

	sc.Step(`^no completion attempt starts — the ticket set is not yet whole$`, func() error {
		// The missing STAMP, not the state: a whole plan is active too, so
		// naming the state alone would not tell the operator them apart.
		return w.detect.refused("completed=false")
	})

	sc.Step(`^decomposition passes its final assignment barrier and stamps completed$`, func() error {
		return w.detect.plan(true, "issue-1")
	})

	sc.Step(`^completion detection arms$`, func() error {
		d := w.detect
		if err := d.detect(); err != nil {
			return err
		}
		if !d.got.Armed {
			return fmt.Errorf("detection did not arm: %s", d.got.Reason)
		}
		// The watermark travels with the answer. A claim captured against a
		// read is only as good as a feed drained to what that read saw.
		if d.got.Watermark == "" {
			return errors.New("the detection carries no feed watermark")
		}
		return nil
	})

	sc.Step(`^the activated head plan's every ticket is complete$`, func() error {
		d := w.newDetect()
		if err := d.plan(true, "issue-1", "issue-2"); err != nil {
			return err
		}
		d.live.rows = nil
		return nil
	})

	sc.Step(`^a mapped unplanned ticket for the target — parented under the epic at bind time — is still open$`,
		func() error {
			w.detect.live.rows = append(w.detect.live.rows,
				planner.LiveIssue{ID: "issue-99", Status: "open"})
			return nil
		})

	sc.Step(`^a mapped unplanned ticket for the target sits in status "([^"]*)", never popped and never bound$`,
		func(status string) error {
			w.detect.live.rows = append(w.detect.live.rows,
				planner.LiveIssue{ID: "issue-99", Status: status})
			return nil
		})

	sc.Step(`^no completion attempt starts and popping continues$`, func() error {
		return w.detect.refused("issue-99")
	})

	sc.Step(`^sutra's close gate would refuse the epic anyway — the unplanned ticket is a descendant$`,
		func() error {
			// Which is WHY detection refuses rather than submitting and
			// finding out: an armed attempt here would open a completion
			// review that can never close, and spend a human's attention on
			// it.
			return w.detect.refused("issue-99", "unplanned")
		})

	sc.Step(`^no completion attempt starts — blocked is active work, resolved through the live queue and mappings$`,
		func() error {
			return w.detect.refused("issue-99", "blocked")
		})

	registerStall(sc, w)
}

func registerStall(sc *godog.ScenarioContext, w *world) {
	sc.Step(`^no ticket is workable, none are in flight, and the epic cannot close$`, func() error {
		d := w.newDetect()
		// A whole plan, and an unplanned ticket nothing can move: nothing to
		// pop, nothing running, and an epic sutra would refuse to close.
		if err := d.plan(true, "issue-1"); err != nil {
			return err
		}
		d.live.rows = []planner.LiveIssue{{ID: "issue-99", Status: "blocked"}}
		if err := d.detect(); err != nil {
			return err
		}
		if d.got.Armed {
			return errors.New("the scenario's condition is not a stall: completion armed")
		}
		recorded, err := orchestrator.Stalls{
			Store: d.stalls, Now: fakes.NewClock(time.Unix(0, 0)),
		}.Record(context.Background(), "/spec", 0, d.got.Reason)
		d.recorded = recorded
		return err
	})

	sc.Step(`^a durable Stall row records the condition and its cause$`, func() error {
		d := w.detect
		got, ok := d.stalls.rows[d.recorded.Key]
		if !ok {
			return errors.New("no stall row survives; the condition is nowhere")
		}
		if got.Cause == "" {
			return errors.New("the stall records no cause")
		}
		// The CAUSE, not a generic "stalled": the operator has to know what
		// to go and unblock.
		if !strings.Contains(got.Cause, "issue-99") {
			return fmt.Errorf("the cause is %q — it names nothing to act on", got.Cause)
		}
		return nil
	})

	sc.Step(`^the stall appears in the operator inbox from that row$`, func() error {
		open, err := w.detect.stalls.Open(context.Background())
		if err != nil {
			return err
		}
		if len(open) != 1 {
			return fmt.Errorf("the inbox lists %d stalls", len(open))
		}
		return nil
	})

	sc.Step(`^resolution stamps the row rather than deleting it$`, func() error {
		d := w.detect
		s := orchestrator.Stalls{Store: d.stalls, Now: fakes.NewClock(time.Unix(0, 0))}
		if err := s.Resolve(context.Background(), d.recorded.Key); err != nil {
			return err
		}
		got, ok := d.stalls.rows[d.recorded.Key]
		if !ok {
			return errors.New("resolution deleted the row; the history is gone")
		}
		if got.State != orchestrator.StallResolved {
			return fmt.Errorf("the row is %q after resolution", got.State)
		}
		if got.Cause == "" {
			return errors.New("resolution blanked the cause")
		}
		open, _ := d.stalls.Open(context.Background())
		if len(open) != 0 {
			return errors.New("a resolved stall is still in the inbox")
		}
		return nil
	})

	sc.Step(`^kriya neither spins nor declares the build done$`, func() error {
		d := w.detect
		// Not done: detection refused, and it said why.
		if d.got.Armed {
			return errors.New("the build declared itself done over a stall")
		}
		// Not spinning: the condition is a ROW. Re-detecting converges on it
		// rather than filling the inbox, which is what makes a poll safe.
		s := orchestrator.Stalls{Store: d.stalls, Now: fakes.NewClock(time.Unix(0, 0))}
		for range 3 {
			if _, err := s.Record(context.Background(), "/spec", 0, d.got.Reason); err != nil {
				return err
			}
		}
		if len(d.stalls.rows) != 1 {
			return fmt.Errorf("polling produced %d stall rows", len(d.stalls.rows))
		}
		return nil
	})
}
