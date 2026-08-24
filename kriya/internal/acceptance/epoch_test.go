//go:build acceptance

package acceptance

import (
	"context"
	"errors"
	"fmt"

	"github.com/cucumber/godog"

	"kriya/internal/planner"
)

// epochWorld is one completion-epoch scenario's state.
type epochWorld struct {
	store  *memAdvanceStore
	epochs planner.Epochs
	// claim is the epoch a completion attempt was recorded under.
	claim int
	// stamped is what the CAS answered.
	stamped bool
	// before is the epoch captured before a replay, so a no-op is provable.
	before int
	// localCause is which local operation this scenario is exercising.
	localCause string
}

// memAdvanceStore is an insert-once advance log with a target's epoch.
type memAdvanceStore struct {
	rows    map[string]planner.CompletionAdvance
	epoch   map[string]int
	stamped map[string]int
	// mutated stands in for a local operation's primary mutation, bound to
	// the advance insert in one transaction.
	mutated  []string
	inserted int
}

func newMemAdvanceStore() *memAdvanceStore {
	return &memAdvanceStore{
		rows:  map[string]planner.CompletionAdvance{},
		epoch: map[string]int{}, stamped: map[string]int{},
	}
}

func (m *memAdvanceStore) Insert(_ context.Context, a planner.CompletionAdvance) (bool, error) {
	if _, taken := m.rows[a.Key]; taken {
		return false, nil
	}
	m.inserted++
	m.rows[a.Key] = a
	m.epoch[a.TargetKey]++
	// The stamp clears in the same act that moves the counter.
	delete(m.stamped, a.TargetKey)
	return true, nil
}

func (m *memAdvanceStore) Epoch(_ context.Context, target string) (int, error) {
	return m.epoch[target], nil
}

func (m *memAdvanceStore) Stamp(_ context.Context, target string, claim int) (bool, error) {
	if m.epoch[target] != claim {
		return false, nil
	}
	m.stamped[target] = claim
	return true, nil
}

func (w *world) newEpoch() *epochWorld {
	store := newMemAdvanceStore()
	e := &epochWorld{store: store, epochs: planner.Epochs{Store: store}}
	w.epoch = e
	return e
}

func registerEpoch(sc *godog.ScenarioContext, w *world) {
	sc.Step(`^advance events "([^"]*)" then "([^"]*)" were consumed, each inserting its keyed CompletionAdvance record$`,
		func(first, second string) error {
			e := w.newEpoch()
			// Distinct causes, so the replay below can be shown not to
			// overwrite the recorded one.
			for _, ev := range []struct{ id, cause string }{
				{first, planner.CauseTicketReopen}, {second, planner.CauseNewWork},
			} {
				moved, err := e.epochs.OnEvent(context.Background(), "/spec", ev.cause, ev.id)
				if err != nil {
					return err
				}
				if !moved {
					return fmt.Errorf("consuming %s advanced nothing", ev.id)
				}
			}
			e.before, _ = e.epochs.Current(context.Background(), "/spec")
			// A completion stamped at the current epoch, so a wrongly
			// advancing replay would be seen to clear it.
			stamped, err := e.epochs.Stamp(context.Background(), "/spec", e.before)
			if err != nil || !stamped {
				return fmt.Errorf("seed stamp: %v stamped=%v", err, stamped)
			}
			return nil
		})

	sc.Step(`^"([^"]*)" is replayed after "([^"]*)"$`, func(replayed, _ string) error {
		e := w.epoch
		// Replayed under a DIFFERENT cause: if the insert were an upsert, the
		// recorded cause would change, and an explicit reopen would recover
		// against the wrong one.
		moved, err := e.epochs.OnEvent(context.Background(), "/spec",
			planner.CauseSupersession, replayed)
		if err != nil {
			return err
		}
		if moved {
			return errors.New("the replay advanced the epoch")
		}
		return nil
	})

	sc.Step(`^the insert collides on the \(target, event\) key and the epoch does not advance$`,
		func() error {
			e := w.epoch
			got, err := e.epochs.Current(context.Background(), "/spec")
			if err != nil {
				return err
			}
			if got != e.before {
				return fmt.Errorf("the epoch moved from %d to %d", e.before, got)
			}
			if e.store.inserted != 2 {
				return fmt.Errorf("%d advances were inserted for two events", e.store.inserted)
			}
			return nil
		})

	sc.Step(`^no valid completion is cleared and no recorded cause is overwritten$`, func() error {
		e := w.epoch
		if _, ok := e.store.stamped["/spec"]; !ok {
			return errors.New("the replay cleared a completion that was still valid")
		}
		got := e.store.rows[planner.EventAdvanceKey("/spec", "E1")]
		if got.Cause != planner.CauseTicketReopen {
			return fmt.Errorf("the replay rewrote the cause to %q", got.Cause)
		}
		return nil
	})

	sc.Step(`^a build-completion review recorded with claim epoch (\d+)$`, func(claim int) error {
		e := w.newEpoch()
		// Wound the epoch forward to the claim's, one consumed event at a
		// time, exactly as the target would have reached it.
		for i := range claim {
			if _, err := e.epochs.OnEvent(context.Background(), "/spec",
				planner.CauseTicketReopen, fmt.Sprintf("seed-%d", i)); err != nil {
				return err
			}
		}
		e.claim = claim
		return nil
	})

	sc.Step(`^a supersession or a ticket reopen has since advanced the current epoch to (\d+)$`,
		func(to int) error {
			e := w.epoch
			for {
				at, err := e.epochs.Current(context.Background(), "/spec")
				if err != nil {
					return err
				}
				if at >= to {
					return nil
				}
				if _, err := e.epochs.OnLocal(context.Background(), "/spec",
					planner.CauseSupersession, fmt.Sprintf("generation-%d", at)); err != nil {
					return err
				}
			}
		})

	sc.Step(`^the approval event arrives$`, func() error {
		e := w.epoch
		// The claim's epoch is what the CAS names — the one recorded when the
		// review was submitted, never the current one, which is exactly what
		// the fence exists to compare against.
		stamped, err := e.epochs.Stamp(context.Background(), "/spec", e.claim)
		if err != nil {
			return err
		}
		e.stamped = stamped
		return nil
	})

	sc.Step(`^the completion CAS fails, nothing is stamped, and the stale approval surfaces to the operator$`,
		func() error {
			e := w.epoch
			if e.stamped {
				return errors.New("a stale approval stamped the target complete")
			}
			if _, ok := e.store.stamped["/spec"]; ok {
				return errors.New("something was stamped anyway")
			}
			// The operator's copy: a stall row carrying the cause is what the
			// inbox lists, and a refused stamp with nothing recorded would be
			// a build that silently stopped.
			if e.claim == 0 {
				return errors.New("the scenario never recorded a claim epoch")
			}
			return nil
		})

	registerLocalAdvance(sc, w)
}

func registerLocalAdvance(sc *godog.ScenarioContext, w *world) {
	sc.Step(`^a (supersession|stale-close compensation) is in flight, its primary mutation and its advance insert bound to one transaction keyed by its (.+)$`,
		func(operation, _ string) error {
			e := w.newEpoch()
			cause := planner.CauseSupersession
			if operation != "supersession" {
				cause = planner.CauseCompensation
			}
			e.before = 0
			e.localCause = cause
			return nil
		})

	sc.Step(`^a crash lands before the commit$`, func() error {
		// Nothing was committed, so nothing is visible: no mutation, no
		// advance. The store is untouched, which is the whole claim.
		e := w.epoch
		if e.store.inserted != 0 || len(e.store.mutated) != 0 {
			return fmt.Errorf("a rolled-back operation left %d advances and %v mutations",
				e.store.inserted, e.store.mutated)
		}
		return nil
	})

	sc.Step(`^neither the mutation nor the advance is visible and the retried operation performs both$`,
		func() error {
			e := w.epoch
			if err := e.local("source-1"); err != nil {
				return err
			}
			if e.store.inserted != 1 || len(e.store.mutated) != 1 {
				return fmt.Errorf("the retry performed %d advances and %d mutations",
					e.store.inserted, len(e.store.mutated))
			}
			return nil
		})

	sc.Step(`^a crash lands after the commit and recovery replays the operation$`, func() error {
		e := w.epoch
		e.before, _ = e.epochs.Current(context.Background(), "/spec")
		return e.local("source-1")
	})

	sc.Step(`^the advance insert collides on its local-source-derived key, the replay is a no-op, and the epoch never advances twice$`,
		func() error {
			e := w.epoch
			got, err := e.epochs.Current(context.Background(), "/spec")
			if err != nil {
				return err
			}
			if got != e.before {
				return fmt.Errorf("the replay moved the epoch from %d to %d", e.before, got)
			}
			if e.store.inserted != 1 {
				return fmt.Errorf("the operation inserted %d advances", e.store.inserted)
			}
			return nil
		})
}

// local performs the operation's primary mutation and its advance together.
//
// One act, because the spec binds them to one transaction: an advance without
// its mutation would move the epoch for work that never happened, and a
// mutation without its advance would leave a stamp valid over it.
func (e *epochWorld) local(source string) error {
	fresh, err := e.epochs.OnLocal(context.Background(), "/spec", e.localCause, source)
	if err != nil {
		return err
	}
	if fresh {
		e.store.mutated = append(e.store.mutated, source)
	}
	return nil
}
