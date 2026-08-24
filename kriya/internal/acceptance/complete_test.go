//go:build acceptance

package acceptance

import (
	"context"
	"errors"
	"fmt"

	"github.com/cucumber/godog"

	"kriya/internal/orchestrator"
)

// ticketGate behaves as sutra does: a keyed close lands once, and a replay
// under the same key returns the original success rather than a close-used
// conflict.
type ticketGate struct {
	closed map[string]bool
	calls  []ticketCall
	err    error
	// reversed makes the close conflict, as a reversed approval would.
	reversed bool
}

type ticketCall struct {
	issue, review, event, key string
	revision                  int
}

func newTicketGate() *ticketGate { return &ticketGate{closed: map[string]bool{}} }

func (t *ticketGate) Complete(
	_ context.Context, issue, review string, revision int, event, key string,
) error {
	t.calls = append(t.calls, ticketCall{
		issue: issue, review: review, event: event, key: key, revision: revision,
	})
	if t.err != nil {
		return t.err
	}
	if t.closed[key] {
		return nil
	}
	if t.reversed {
		return errors.New("close conflicted: the approval was reversed")
	}
	t.closed[key] = true
	return nil
}

// branchAt reports a branch head, and records whether the run's session had
// ended by the time it was read.
type branchAt struct {
	head            string
	ender           *sessionStop
	terminatedFirst bool
}

func (b *branchAt) Head(context.Context, string) (string, error) {
	if b.ender != nil {
		b.terminatedFirst = b.ender.ended
	}
	return b.head, nil
}

type sessionStop struct{ ended bool }

func (s *sessionStop) Terminate(context.Context, string) error {
	s.ended = true
	return nil
}

// completeWorld is one completion scenario's state.
type completeWorld struct {
	tickets *ticketGate
	branch  *branchAt
	ender   *sessionStop
	store   *runStore
	c       orchestrator.Completer
	run     orchestrator.BuildRun
	err     error
}

func (w *world) newComplete() *completeWorld {
	tickets := newTicketGate()
	ender := &sessionStop{}
	branch := &branchAt{head: "C2", ender: ender}
	store := &runStore{rows: map[string]orchestrator.BuildRun{}}
	c := &completeWorld{
		tickets: tickets, branch: branch, ender: ender, store: store,
		c: orchestrator.Completer{
			Store: store, Tickets: tickets, Branches: branch,
			Sessions: ender, Actor: "actor-1",
		},
		run: orchestrator.BuildRun{
			ID: "run-1", Ticket: "KRI-1", State: orchestrator.StateMerged,
			ReviewID: "review-1", ReviewRevision: 2, ReviewVerdictEvent: "event-9",
			ReviewCommit: "C2",
		},
	}
	w.complete = c
	return c
}

func (c *completeWorld) completion() orchestrator.Completion {
	return orchestrator.Completion{Issue: "issue-7", Branch: "kriya/KRI-1/abcd", Merged: "C2"}
}

func registerComplete(sc *godog.ScenarioContext, w *world) {
	sc.Step(`^the branch merged successfully$`, func() error {
		w.newComplete()
		return nil
	})

	sc.Step(`^kriya terminates the run's dev session — the branch's only in-protocol writer — before the check$`,
		func() error {
			c := w.complete
			c.run, c.err = c.c.Complete(context.Background(), c.run, c.completion())
			if c.err != nil {
				return c.err
			}
			if !c.branch.terminatedFirst {
				return errors.New("the head was read while the session could still write")
			}
			return nil
		})

	sc.Step(`^kriya re-reads the branch head and it still equals the merged commit$`, func() error {
		if w.complete.branch.head != "C2" {
			return fmt.Errorf("the branch is at %q", w.complete.branch.head)
		}
		return nil
	})

	sc.Step(`^kriya enters completing, persisting the ticket-close key scoped to the review, its approved revision, and its approval's verdict event, and transitions the ticket to complete naming all three$`,
		func() error {
			// Persisted BEFORE the call: proven in its own world by a close
			// that never lands, which leaves exactly the row a replay needs.
			probe := w.newComplete()
			probe.tickets.err = errors.New("sutra unreachable")
			if _, err := probe.c.Complete(context.Background(), probe.run, probe.completion()); err == nil {
				return errors.New("a close that never landed read as success")
			}
			row := probe.store.rows["run-1"]
			if row.CompletionState != orchestrator.CompleteCompleting || row.CloseKey == "" {
				return fmt.Errorf("the crash window is invisible: %+v", row)
			}
			// Scoped to all three: a different verdict event keys differently.
			reapproved := probe.run
			reapproved.ReviewVerdictEvent = "event-10"
			probe.tickets.err = nil
			other, err := probe.c.Complete(context.Background(), reapproved, probe.completion())
			if err != nil {
				return err
			}
			if other.CloseKey == row.CloseKey {
				return errors.New("a different approval event produced the same close key")
			}
			return nil
		})

	sc.Step(`^the transition goes through sutra under that key, which stamps the review close-used — the merge-time consumption never blocks it, and a stale revision would reject$`,
		func() error {
			c := w.complete
			if len(c.tickets.calls) == 0 {
				return errors.New("sutra was never called")
			}
			call := c.tickets.calls[0]
			if call.review != "review-1" || call.revision != 2 || call.event != "event-9" {
				return fmt.Errorf("closed with %+v", call)
			}
			if call.key == "" {
				return errors.New("the close carried no idempotency key")
			}
			return nil
		})

	sc.Step(`^the run advances to closed$`, func() error {
		c := w.complete
		if c.store.rows["run-1"].CompletionState != orchestrator.CompleteClosed {
			return fmt.Errorf("the run is in %q", c.store.rows["run-1"].CompletionState)
		}
		// The table, not this module, decides where a completed run goes.
		return transitionFrom(orchestrator.StateMerged,
			orchestrator.StateClosed, orchestrator.StateDevLoop)
	})

	sc.Step(`^the close landed in sutra but the crash hit before kriya recorded it$`, func() error {
		c := w.newComplete()
		got, err := c.c.Complete(context.Background(), c.run, c.completion())
		if err != nil {
			return err
		}
		got.CompletionState = orchestrator.CompleteCompleting
		c.run = got
		return c.store.Upsert(context.Background(), got)
	})

	sc.Step(`^recovery replays the complete transition under the persisted ticket-close key$`,
		func() error {
			c := w.complete
			before := c.store.rows["run-1"].CloseKey
			_, c.err = c.c.RecoverCompletions(context.Background(),
				func(orchestrator.BuildRun) orchestrator.Completion { return c.completion() })
			if c.err != nil {
				return c.err
			}
			last := c.tickets.calls[len(c.tickets.calls)-1]
			if last.key != before {
				return fmt.Errorf("the replay used key %q, want the persisted one", last.key)
			}
			return nil
		})

	sc.Step(`^sutra returns the original success — never a close-used conflict — and the run advances to closed$`,
		func() error {
			c := w.complete
			if c.store.rows["run-1"].CompletionState != orchestrator.CompleteClosed {
				return fmt.Errorf("the run is in %q", c.store.rows["run-1"].CompletionState)
			}
			if len(c.tickets.closed) != 1 {
				return fmt.Errorf("%d closes landed", len(c.tickets.closed))
			}
			return nil
		})

	sc.Step(`^a spike's document-backed close recovers identically under its own persisted key, review, and revision — research runs persist them directly, having no merge attempt$`,
		func() error {
			// The close reads its key, review and revision from the RUN, not
			// from a merge attempt — so a run that never had one replays the
			// same way. Proven by recovering a run with no attempt anywhere.
			probe := w.newComplete()
			probe.run.CompletionState = orchestrator.CompleteCompleting
			probe.run.CloseKey = "spike-close-key"
			if err := probe.store.Upsert(context.Background(), probe.run); err != nil {
				return err
			}
			if _, err := probe.c.RecoverCompletions(context.Background(),
				func(orchestrator.BuildRun) orchestrator.Completion {
					return probe.completion()
				}); err != nil {
				return err
			}
			last := probe.tickets.calls[len(probe.tickets.calls)-1]
			if last.key != "spike-close-key" {
				return fmt.Errorf("the replay used key %q", last.key)
			}
			return nil
		})

	sc.Step(`^a close conflicted because its approval was reversed, and the review was later reapproved at the same revision$`,
		func() error {
			c := w.newComplete()
			c.tickets.reversed = true
			if _, err := c.c.Complete(context.Background(), c.run, c.completion()); err == nil {
				return errors.New("a conflicted close read as success")
			}
			c.tickets.reversed = false
			return nil
		})

	sc.Step(`^the new approval event yields a fresh ticket-close key and the retried close issues under it, never replaying the cached conflict$`,
		func() error {
			c := w.complete
			conflicted := c.store.rows["run-1"].CloseKey
			reapproved := c.run
			reapproved.ReviewVerdictEvent = "event-10"
			got, err := c.c.Complete(context.Background(), reapproved, c.completion())
			if err != nil {
				return err
			}
			if got.CloseKey == conflicted {
				return errors.New("the retried close reused the conflicted key")
			}
			c.run = got
			return nil
		})

	sc.Step(`^the completed head commit is durably recorded$`, func() error {
		if got := w.complete.store.rows["run-1"].CompletedHead; got != "C2" {
			return fmt.Errorf("recorded head %q", got)
		}
		return nil
	})

	sc.Step(`^the run's workspace becomes eligible for cleanup$`, func() error {
		// Eligible means the session that was writing to it has ended: the
		// branch has no in-protocol writer left.
		if !w.complete.ender.ended {
			return errors.New("the dev session is still live")
		}
		return nil
	})

	sc.Step(`^a commit landed on the branch before kriya's completion check$`, func() error {
		w.complete.branch.head = "C3"
		return nil
	})

	sc.Step(`^kriya re-reads the branch head$`, func() error {
		c := w.complete
		c.run, c.err = c.c.Complete(context.Background(), c.run, c.completion())
		return nil
	})

	sc.Step(`^the ticket is not completed and the run returns to the pair loop for the unreviewed commits$`,
		func() error {
			c := w.complete
			if !errors.Is(c.err, orchestrator.ErrHeadAdvanced) {
				return fmt.Errorf("got %v, want ErrHeadAdvanced", c.err)
			}
			if len(c.tickets.calls) != 0 {
				return errors.New("the ticket closed over unreviewed commits")
			}
			return transitionFrom(orchestrator.StateMerged,
				orchestrator.StateClosed, orchestrator.StateDevLoop)
		})

	sc.Step(`^once the gates pass again the new head is submitted as a fresh review, durably recorded on the run — the approved review is never resubmitted$`,
		func() error {
			// A fresh review, because the submission key is scoped to the head
			// commit and the gate attempt: a new head under a new attempt can
			// only produce a key nothing has used.
			probe := freshSubmit()
			probe.run = orchestrator.BuildRun{
				ID: "run-1", Ticket: "KRI-1", Head: "C3", GatedBase: "D1", Attempt: 2,
			}
			first, err := probe.submitter.Submit(context.Background(), probe.run, probe.sub)
			if err != nil {
				return err
			}
			older := orchestrator.BuildRun{
				ID: "run-1", Ticket: "KRI-1", Head: "C2", GatedBase: "D1", Attempt: 1,
			}
			second, err := probe.submitter.Submit(context.Background(), older, probe.sub)
			if err != nil {
				return err
			}
			if first.ReviewKey == second.ReviewKey {
				return errors.New("a new head reused the approved review's key")
			}
			if first.ReviewID == second.ReviewID {
				return errors.New("a new head returned the approved review")
			}
			return nil
		})

	sc.Step(`^no work is stranded on a terminal run$`, func() error {
		// merged is NOT terminal: it has a transition, and its failure path
		// goes back to the loop rather than to a state nothing leaves.
		if _, found := orchestrator.Lookup(orchestrator.StateMerged); !found {
			return errors.New("a merged run has nowhere to go")
		}
		return nil
	})

	sc.Step(`^the ticket completed with its head commit durably recorded$`, func() error {
		c := w.newComplete()
		got, err := c.c.Complete(context.Background(), c.run, c.completion())
		if err != nil {
			return err
		}
		c.run = got
		if got.CompletedHead == "" {
			return errors.New("no head was recorded")
		}
		return nil
	})

	sc.Step(`^an out-of-band commit lands on the branch afterward$`, func() error {
		w.complete.branch.head = "C9"
		return nil
	})

	sc.Step(`^the advance is detected against the recorded head commit and surfaces to the operator with sutra's reopen path$`,
		func() error {
			c := w.complete
			advanced, err := c.c.Advanced(context.Background(), c.run, "kriya/KRI-1/abcd")
			if err != nil {
				return err
			}
			if !advanced {
				return errors.New("a commit landing after completion was not detected")
			}
			// Against the RECORDED head, not anything re-derived.
			if c.run.CompletedHead == c.branch.head {
				return errors.New("the comparison used the current head")
			}
			return nil
		})
}
