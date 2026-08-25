//go:build acceptance

package acceptance

import (
	"context"
	"errors"
	"fmt"

	"github.com/cucumber/godog"

	"kriya/internal/planner"
)

// closeWorld is one epic-close scenario's state.
type closeWorld struct {
	claims   *memCompletionClaims
	epics    *epicGate
	advances *memAdvanceStore
	claimer  planner.Claimer
	err      error
	// firstKey is the key a conflicted close was cached under.
	firstKey string
}

// epicGate stands in for sutra's issue-status API: it honours idempotency
// keys, and it refuses a close whose subtree fence does not match — which is
// the whole of what makes a completion claim provable.
type epicGate struct {
	closedByKey map[string]bool
	revision    int64
	// openChildren is how many tickets under the epic are still open. sutra
	// refuses a close while any remain, and the live proof established that
	// it checks this AFTER the subtree fence — the fence is a fence, and
	// this gate is the authoritative check sitting on top of it.
	openChildren int
	keys         []string
	fences       []int64
	reviews      []string
	revisions    []int
	events       []string
	err          error
}

func (e *epicGate) Close(
	_ context.Context, _, review string, revision int,
	verdictEvent string, expectedSubtreeRevision int64, key string,
) error {
	e.keys = append(e.keys, key)
	e.fences = append(e.fences, expectedSubtreeRevision)
	e.reviews = append(e.reviews, review)
	e.revisions = append(e.revisions, revision)
	e.events = append(e.events, verdictEvent)
	if e.err != nil {
		return e.err
	}
	if e.closedByKey[key] {
		// Replayed under the key that stamped it. The ORIGINAL success —
		// never a close-used conflict, because this key is what closed it.
		return nil
	}
	if expectedSubtreeRevision != e.revision {
		return fmt.Errorf("conflict: expected subtree_revision %d, current is %d",
			expectedSubtreeRevision, e.revision)
	}
	if e.openChildren > 0 {
		return fmt.Errorf("conflict: open-children: %d children are still open", e.openChildren)
	}
	e.closedByKey[key] = true
	return nil
}

func (w *world) newClose() *closeWorld {
	c := &closeWorld{
		claims:   &memCompletionClaims{rows: map[string]planner.CompletionClaim{}},
		epics:    &epicGate{closedByKey: map[string]bool{}},
		advances: newMemAdvanceStore(),
	}
	c.claimer = planner.Claimer{
		Claims: c.claims, Epics: c.epics,
		Epochs: planner.Epochs{Store: c.advances},
	}
	w.close = c
	return c
}

// submitted seeds a claim resting at review-submitted.
func (c *closeWorld) submitted(subtreeRevision int64) error {
	epoch, err := c.claimer.Epochs.Current(context.Background(), "/spec")
	if err != nil {
		return err
	}
	return c.claims.Upsert(context.Background(), planner.CompletionClaim{
		TargetKey: "/spec", State: planner.CompletionSubmitted, Epoch: epoch,
		SubmissionKey: planner.SubmissionKey("/spec", epoch),
		ReportVersion: "ver-1", ReviewID: "review-1", ReviewRevision: 2,
		SubtreeRevision: subtreeRevision,
	})
}

func registerClose(sc *godog.ScenarioContext, w *world) {
	sc.Step(`^a completion review was submitted with the epic's subtree revision recorded as (\d+)$`,
		func(revision int) error {
			c := w.newClose()
			c.epics.revision = int64(revision)
			return c.submitted(int64(revision))
		})

	sc.Step(`^a child ticket reopens and recompletes before kriya consumes any reopen event, advancing the revision to (\d+)$`,
		func(to int) error {
			// Current STATE is identical — the child is complete again — and
			// only the revision moved. That is exactly what the fence is for.
			w.close.epics.revision = int64(to)
			return nil
		})

	sc.Step(`^the approval event arrives and kriya attempts the epic close with expected subtree revision (\d+)$`,
		func(fence int) error {
			c := w.close
			if got := c.claims.rows["/spec"].SubtreeRevision; got != int64(fence) {
				return fmt.Errorf("the claim captured revision %d, not %d", got, fence)
			}
			_, c.err = c.claimer.Close(context.Background(), "/spec", "epic-1", "event-9")
			return nil
		})

	sc.Step(`^sutra rejects the close — history moved even though current state matches$`,
		func() error {
			c := w.close
			if c.err == nil {
				return errors.New("the close succeeded over moved history")
			}
			if len(c.epics.fences) != 1 || c.epics.fences[0] != c.claims.rows["/spec"].SubtreeRevision {
				return fmt.Errorf("the close fenced on %v", c.epics.fences)
			}
			if len(c.epics.closedByKey) != 0 {
				return errors.New("the epic closed anyway")
			}
			return nil
		})

	sc.Step(`^kriya advances its epoch and a fresh attempt with fresh approval is required$`,
		func() error {
			c := w.close
			before, err := c.claimer.Epochs.Current(context.Background(), "/spec")
			if err != nil {
				return err
			}
			// Consuming the reopen it had not yet seen is what advances it.
			moved, err := c.claimer.Epochs.OnEvent(context.Background(), "/spec",
				planner.CauseTicketReopen, "reopen-event")
			if err != nil {
				return err
			}
			if !moved {
				return errors.New("consuming the reopen advanced nothing")
			}
			after, _ := c.claimer.Epochs.Current(context.Background(), "/spec")
			if after == before {
				return errors.New("the epoch did not advance")
			}
			// The next attempt's key names the NEW epoch, so the spent
			// review can never replay into it.
			if planner.SubmissionKey("/spec", after) == c.claims.rows["/spec"].SubmissionKey {
				return errors.New("a fresh attempt would reuse the spent submission key")
			}
			if _, ok := c.advances.stamped["/spec"]; ok {
				return errors.New("the target is stamped complete")
			}
			return nil
		})

	registerReapproval(sc, w)
}

func registerReapproval(sc *godog.ScenarioContext, w *world) {
	sc.Step(`^an epic close conflicted because the completion approval was reversed mid-flight$`,
		func() error {
			c := w.newClose()
			c.epics.revision = 7
			if err := c.submitted(7); err != nil {
				return err
			}
			c.epics.err = errors.New("conflict: the approval was reversed")
			if _, err := c.claimer.Close(context.Background(), "/spec", "epic-1", "event-9"); err == nil {
				return errors.New("the conflicted close succeeded")
			}
			c.firstKey = c.claims.rows["/spec"].CloseKey
			if c.firstKey == "" {
				return errors.New("the conflicted close was never written ahead")
			}
			return nil
		})

	sc.Step(`^the review is reapproved$`, func() error {
		c := w.close
		// A NEW verdict event: reapproval is a different event, and that is
		// what rotates the key.
		c.epics.err = nil
		_, c.err = c.claimer.Close(context.Background(), "/spec", "epic-1", "event-10")
		return nil
	})

	sc.Step(`^the new approval event yields a fresh close key$`, func() error {
		c := w.close
		got := c.claims.rows["/spec"].CloseKey
		if got == c.firstKey {
			return fmt.Errorf("the reapproval reused key %q", got)
		}
		if got != planner.CloseKey("/spec", c.claims.rows["/spec"].Epoch, "event-10") {
			return errors.New("the key does not derive from the new approval event")
		}
		return nil
	})

	sc.Step(`^the retried close issues under it — never replaying the cached conflict$`,
		func() error {
			c := w.close
			if c.err != nil {
				return c.err
			}
			last := c.epics.keys[len(c.epics.keys)-1]
			if last == c.firstKey {
				return errors.New("the retry went out under the conflicted key")
			}
			if !c.epics.closedByKey[last] {
				return errors.New("the epic did not close")
			}
			if c.claims.rows["/spec"].State != planner.CompletionComplete {
				return fmt.Errorf("the claim settled as %q", c.claims.rows["/spec"].State)
			}
			return nil
		})
}
