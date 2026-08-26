//go:build acceptance

package acceptance

import (
	"context"
	"fmt"

	"github.com/cucumber/godog"

	"kriya/internal/planner"
)

// bindWorld is one binding-precedence scenario's state.
type bindWorld struct {
	head    *headWorld
	tickets *worldTickets
	binder  planner.Binder
	got     planner.Binding
}

func (w *world) newBind() (*bindWorld, error) {
	h, err := w.newHead(w.tempDir)
	if err != nil {
		return nil, err
	}
	b := &bindWorld{head: h, tickets: newWorldTickets()}
	b.binder = planner.Binder{Plans: h.plans, Heads: h.heads, Tickets: b.tickets}
	w.bind = b
	return b, nil
}

// link writes a plan at a point on the chain, with or without the ticket.
func (b *bindWorld) link(key, predecessor, state string, generation int, holds bool) error {
	ctx := context.Background()
	plan := planner.Plan{
		Key: key, TargetKey: "/spec", SpecHash: "hash-" + key,
		Generation: generation, State: state, Predecessor: predecessor,
	}
	if err := b.head.plans.Upsert(ctx, plan); err != nil {
		return err
	}
	if !holds {
		return nil
	}
	return b.tickets.Put(ctx, "/spec", planner.Ticket{
		Title: "the work", IssueID: "issue-1", Plan: key, Ordinal: 0,
	})
}

// installHead points the head row at a plan, fenced or not.
func (b *bindWorld) installHead(key string, unactivated bool) error {
	fence := 0
	if unactivated {
		fence = 1
	}
	_, err := b.head.db.ExecContext(context.Background(),
		`INSERT INTO plan_head (target_key, current, generation, fence)
		 VALUES (?, ?, 1, ?)
		 ON CONFLICT(target_key) DO UPDATE SET current = excluded.current, fence = excluded.fence`,
		"/spec", key, fence)
	return err
}

// consume stamps a plan's row already settled.
func (b *bindWorld) consume(key string) error {
	return b.tickets.Consume(context.Background(), key, 0, planner.DispositionRetired)
}

func (b *bindWorld) bind() error {
	got, err := b.binder.Bind(context.Background(), "/spec", "issue-1")
	if err != nil {
		return err
	}
	b.got = got
	return nil
}

func registerBinding(sc *godog.ScenarioContext, w *world) {
	sc.Given(`^a supersession chain with a superseded plan holding an unstamped row for the ticket$`,
		func() error {
			b, err := w.newBind()
			if err != nil {
				return err
			}
			// TWO unstamped superseded rows at different depths, under an
			// active head. With only one, "deepest" and "nearest" name the
			// same row and the rule is untestable — which is exactly what a
			// planted control revealed about the first version of this.
			if err := b.link("plan-oldest", "", planner.PlanSuperseded, 1, true); err != nil {
				return err
			}
			if err := b.link("plan-middle", "plan-oldest", planner.PlanSuperseded, 2, true); err != nil {
				return err
			}
			if err := b.link("plan-head", "plan-middle", planner.PlanActive, 3, true); err != nil {
				return err
			}
			return b.installHead("plan-head", false)
		})

	sc.Then(`^a pop mid-retirement binds the deepest unstamped superseded row$`, func() error {
		b := w.bind
		if err := b.bind(); err != nil {
			return err
		}
		if b.got.Plan.Key != "plan-oldest" {
			return fmt.Errorf("the pop bound %q, not the deepest unstamped superseded row",
				b.got.Plan.Key)
		}
		// And the rows nearer the head are stamped, so a later pop does not
		// walk them again.
		walked, err := b.tickets.ForPlan(context.Background(), "plan-head")
		if err != nil {
			return err
		}
		if len(walked) != 1 || !walked[0].Consumed {
			return fmt.Errorf("the head's row was not stamped by the binding")
		}
		return nil
	})

	sc.Given(`^the head has not yet activated and no unstamped superseded row exists$`, func() error {
		b, err := w.newBind()
		if err != nil {
			return err
		}
		if err := b.link("plan-old", "", planner.PlanSuperseded, 1, true); err != nil {
			return err
		}
		if err := b.link("plan-head", "plan-old", planner.PlanActive, 2, true); err != nil {
			return err
		}
		// The superseded row is settled, so rule one does not apply.
		if err := b.consume("plan-old"); err != nil {
			return err
		}
		return b.installHead("plan-head", true)
	})

	sc.Then(`^the pop binds the nearest predecessor row on the chain and never the unactivated head$`,
		func() error {
			b := w.bind
			if err := b.bind(); err != nil {
				return err
			}
			if b.got.Plan.Key == "plan-head" {
				return fmt.Errorf("the pop bound the unactivated head")
			}
			if b.got.Plan.Key != "plan-old" {
				return fmt.Errorf("the pop bound %q, not the nearest predecessor", b.got.Plan.Key)
			}
			return nil
		})

	sc.Given(`^the head is activated and lists the ticket$`, func() error {
		b, err := w.newBind()
		if err != nil {
			return err
		}
		if err := b.link("plan-head", "", planner.PlanActive, 1, true); err != nil {
			return err
		}
		return b.installHead("plan-head", false)
	})

	sc.Then(`^the pop binds the head's own row$`, func() error {
		b := w.bind
		if err := b.bind(); err != nil {
			return err
		}
		if b.got.Plan.Key != "plan-head" {
			return fmt.Errorf("the pop bound %q, not the activated head", b.got.Plan.Key)
		}
		if b.got.Ticket.IssueID != "issue-1" {
			return fmt.Errorf("the binding carries ticket %q", b.got.Ticket.IssueID)
		}
		return nil
	})

	sc.Given(`^the ticket's only membership is consumed rows$`, func() error {
		b, err := w.newBind()
		if err != nil {
			return err
		}
		// The head OMITS the ticket entirely — the amended spec dropped it —
		// and the only rows that hold it are consumed history.
		if err := b.link("plan-old", "", planner.PlanSuperseded, 1, true); err != nil {
			return err
		}
		if err := b.link("plan-head", "plan-old", planner.PlanActive, 2, false); err != nil {
			return err
		}
		if err := b.consume("plan-old"); err != nil {
			return err
		}
		return b.installHead("plan-head", false)
	})

	sc.Then(`^the pop binds the nearest consumed row's plan — the decomposition that produced its acceptance criteria — never a head that omitted it$`,
		func() error {
			b := w.bind
			if err := b.bind(); err != nil {
				return err
			}
			if b.got.Plan.Key != "plan-old" {
				return fmt.Errorf("the pop bound %q, not the consumed row's plan", b.got.Plan.Key)
			}
			// The SNAPSHOT is what makes this matter: the work is built against
			// the spec that described it, not one that dropped it.
			if b.got.Plan.SpecHash != "hash-plan-old" {
				return fmt.Errorf("the binding names snapshot %q", b.got.Plan.SpecHash)
			}
			return nil
		})

	sc.Then(`^every walked-past row is stamped by the binding$`, func() error {
		b := w.bind
		// Nothing was walked past here — the consumed row is the deepest and
		// the head holds no row at all — so the claim under test is that the
		// binding stamps what it walks and nothing else. The first scenario
		// proves the stamping; this proves it does not over-reach.
		for _, key := range []string{"plan-old", "plan-head"} {
			rows, err := b.tickets.ForPlan(context.Background(), key)
			if err != nil {
				return err
			}
			for _, row := range rows {
				if key == "plan-head" && row.Consumed {
					return fmt.Errorf("the binding stamped a row it never walked")
				}
			}
		}
		if len(b.got.WalkedPast) != 0 {
			return fmt.Errorf("the binding reports walking past %d rows", len(b.got.WalkedPast))
		}
		return nil
	})
}
