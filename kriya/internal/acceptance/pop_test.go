//go:build acceptance

package acceptance

import (
	"context"
	"errors"
	"fmt"

	"github.com/cucumber/godog"

	"kriya/internal/orchestrator"
)

// plan is a head plan's tickets, some workable and some not.
//
// It behaves as sutra's work stack does on the point these scenarios turn on:
// only a workable ticket pops, and a replayed key returns the SAME claim.
type plan struct {
	workable []string
	blocked  []string
	handed   int
	byKey    map[string]string
}

func newPlan(workable, blocked []string) *plan {
	return &plan{workable: workable, blocked: blocked, byKey: map[string]string{}}
}

func (p *plan) Pop(_ context.Context, key string) (string, string, error) {
	if claimed, ok := p.byKey[key]; ok {
		return claimed, "T-" + claimed, nil
	}
	if p.handed >= len(p.workable) {
		return "", "", nil
	}
	id := p.workable[p.handed]
	p.handed++
	p.byKey[key] = id
	return id, "T-" + id, nil
}

// unblock moves a blocked ticket into the workable set.
func (p *plan) unblock() error {
	if len(p.blocked) == 0 {
		return errors.New("nothing is blocked")
	}
	p.workable = append(p.workable, p.blocked[0])
	p.blocked = p.blocked[1:]
	return nil
}

// settledPops is the durable count of pops that finished.
type settledPops struct{ n map[string]int }

func (s *settledPops) Current(_ context.Context, target string) (int, error) {
	return s.n[target], nil
}

func (s *settledPops) Advance(_ context.Context, target string, to int) error {
	s.n[target] = to
	return nil
}

// popWorld is one pop-loop scenario's state.
type popWorld struct {
	plan  *plan
	built []string
	loop  orchestrator.Loop
	got   orchestrator.Result
	err   error
}

func (w *world) newPop(workable, blocked []string) *popWorld {
	p := &popWorld{plan: newPlan(workable, blocked)}
	p.loop = orchestrator.Loop{
		Pops: p.plan, Ordinals: &settledPops{n: map[string]int{}},
		TargetKey: "/target", MaxTickets: 16,
		Build: func(_ context.Context, issue, title string) (orchestrator.BuildRun, error) {
			p.built = append(p.built, issue)
			return orchestrator.BuildRun{
				ID: "run-" + issue, Ticket: title, State: orchestrator.StateClosed,
			}, nil
		},
	}
	w.pop = p
	return p
}

func registerPopLoop(sc *godog.ScenarioContext, w *world) {
	sc.Step(`^a head plan with workable tickets$`, func() error {
		w.newPop([]string{"i-1", "i-2", "i-3"}, []string{"i-blocked"})
		return nil
	})

	sc.Step(`^kriya keeps popping and building$`, func() error {
		p := w.pop
		p.got, p.err = p.loop.Run(context.Background())
		if p.err != nil {
			return p.err
		}
		if len(p.built) != 3 {
			return fmt.Errorf("built %v, want every workable ticket", p.built)
		}
		return nil
	})

	sc.Step(`^every remaining ticket is blocked or in flight$`, func() error {
		// The workable set is exhausted; one ticket remains blocked.
		p := w.pop
		if p.plan.handed != len(p.plan.workable) {
			return errors.New("workable tickets remain")
		}
		if len(p.plan.blocked) == 0 {
			return errors.New("nothing is blocked, so the claim is untested")
		}
		return nil
	})

	sc.Step(`^kriya idles without exiting$`, func() error {
		p := w.pop
		got, err := p.loop.Run(context.Background())
		if err != nil {
			return fmt.Errorf("an exhausted work stack was treated as a failure: %w", err)
		}
		if !got.Idle {
			return errors.New("the loop did not report idling")
		}
		if len(got.Built) != 0 {
			return fmt.Errorf("built %d runs with nothing workable", len(got.Built))
		}
		return nil
	})

	sc.Step(`^a ticket unblocks$`, func() error {
		return w.pop.plan.unblock()
	})

	sc.Step(`^popping resumes$`, func() error {
		p := w.pop
		before := len(p.built)
		got, err := p.loop.Run(context.Background())
		if err != nil {
			return err
		}
		if len(p.built) != before+1 {
			return fmt.Errorf("built %v after the unblock", p.built)
		}
		if len(got.Built) != 1 {
			return fmt.Errorf("the resumed pass reported %d runs", len(got.Built))
		}
		// It idles again afterwards, which is right: the pass ends when
		// nothing more is workable, and that is not exiting.
		if !got.Idle {
			return errors.New("the pass did not end by running out of work")
		}
		return nil
	})
}
