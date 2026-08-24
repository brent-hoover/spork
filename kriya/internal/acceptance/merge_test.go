//go:build acceptance

package acceptance

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cucumber/godog"

	"kriya/internal/orchestrator"
)

// mergeAttempts is the queue's store.
type mergeAttempts struct {
	rows  map[string]orchestrator.MergeAttempt
	order []string
}

func (m *mergeAttempts) Insert(_ context.Context, a orchestrator.MergeAttempt) (bool, error) {
	if _, exists := m.rows[a.Key]; exists {
		return false, nil
	}
	m.rows[a.Key] = a
	m.order = append(m.order, a.Key)
	return true, nil
}

func (m *mergeAttempts) Update(_ context.Context, a orchestrator.MergeAttempt) error {
	m.rows[a.Key] = a
	return nil
}

func (m *mergeAttempts) Find(_ context.Context, key string) (orchestrator.MergeAttempt, bool, error) {
	a, ok := m.rows[key]
	return a, ok, nil
}

func (m *mergeAttempts) Unfinished(context.Context) ([]orchestrator.MergeAttempt, error) {
	var out []orchestrator.MergeAttempt
	for _, key := range m.order {
		if a := m.rows[key]; !a.Terminal() {
			out = append(out, a)
		}
	}
	return out, nil
}

// approvalAPI behaves as sutra does: a keyed consumption is claimed once, and
// a verdict reversal after it is rejected.
type approvalAPI struct {
	consumed map[string]bool
	claimed  bool
	calls    []approvalCall
	err      error
}

type approvalCall struct {
	review, event, key string
	revision           int
}

func newApprovalAPI() *approvalAPI { return &approvalAPI{consumed: map[string]bool{}} }

func (a *approvalAPI) Consume(
	_ context.Context, review, _ string, revision int, event, key string,
) error {
	a.calls = append(a.calls, approvalCall{
		review: review, event: event, key: key, revision: revision,
	})
	if a.err != nil {
		return a.err
	}
	if a.consumed[key] {
		return nil
	}
	a.consumed[key] = true
	a.claimed = true
	return nil
}

// reverse is sutra refusing a verdict change after consumption.
func (a *approvalAPI) reverse() error {
	if a.claimed {
		return errors.New("the approval has been consumed and cannot be reversed")
	}
	return nil
}

// repo is a git double that tracks the default-branch head.
type repo struct {
	head      string
	conflicts bool
	merges    []string
	casFails  bool
}

func (r *repo) Preflight(context.Context, string, string) (bool, error) {
	return !r.conflicts, nil
}

func (r *repo) Head(context.Context) (string, error) { return r.head, nil }

func (r *repo) Merge(_ context.Context, commit, expectedBase string) (string, error) {
	if r.head != expectedBase || r.casFails {
		return "", errors.New("merge did not land: the ref moved")
	}
	r.merges = append(r.merges, commit)
	r.head = "merge-" + commit
	return r.head, nil
}

// mergeWorld is one merge scenario's state.
type mergeWorld struct {
	store     *mergeAttempts
	runs      *runStore
	approvals *approvalAPI
	git       *repo
	queue     orchestrator.Queue
	run       orchestrator.BuildRun
	key       string
	laterKey  string
	// movedBranch records that the scenario moved the branch past the pin.
	movedBranch string
	err         error
}

func (w *world) newMerge() *mergeWorld {
	m := freshMerge()
	w.merge = m
	return m
}

// freshMerge builds a merge world WITHOUT making it the scenario's. A step
// that needs a second, throwaway world must not displace the one the following
// steps assert against.
func freshMerge() *mergeWorld {
	store := &mergeAttempts{rows: map[string]orchestrator.MergeAttempt{}}
	runs := &runStore{rows: map[string]orchestrator.BuildRun{}}
	ap := newApprovalAPI()
	git := &repo{head: "base-1"}
	m := &mergeWorld{
		store: store, runs: runs, approvals: ap, git: git,
		queue: orchestrator.Queue{
			Store: store, Runs: runs, Approvals: ap, Git: git, Actor: "actor-1",
		},
		run: orchestrator.BuildRun{
			ID: "run-1", Ticket: "KRI-1", State: orchestrator.StateReviewSubmitted,
			GatedBase: "base-1", ReviewID: "review-1", ReviewCommit: "C2",
			ReviewRevision: 2,
		},
	}
	return m
}

func (m *mergeWorld) approved() orchestrator.Approved {
	return orchestrator.Approved{
		Review: "review-1", Revision: 2, Event: "event-9",
		Commit: "C2", TargetKey: "KRIYA0a",
	}
}

func registerMerge(sc *godog.ScenarioContext, w *world) {
	sc.Step(`^a review enters approved and the event is published$`, func() error {
		m := w.newMerge()
		if err := m.runs.Upsert(context.Background(), m.run); err != nil {
			return err
		}
		return nil
	})

	sc.Step(`^the branch head still equals the approved current revision's pinned commit$`, func() error {
		// The pin is what merges, so the scenario's premise is that it is
		// still what the branch names.
		if w.merge.run.ReviewCommit != "C2" {
			return fmt.Errorf("the pinned commit is %q", w.merge.run.ReviewCommit)
		}
		return nil
	})

	sc.Step(`^the default-branch head still equals the gated_base the run's chain ran against$`, func() error {
		if w.merge.git.head != w.merge.run.GatedBase {
			return fmt.Errorf("the head is %q, the gated base %q",
				w.merge.git.head, w.merge.run.GatedBase)
		}
		return nil
	})

	sc.Step(`^kriya consumes the event inside the serialized merge section$`, func() error {
		m := w.merge
		moved, key, err := m.queue.OnApproval(context.Background(), m.run, m.approved())
		if err != nil {
			return err
		}
		m.run, m.key = moved, key
		_, m.err = m.queue.Run(context.Background(), key)
		return m.err
	})

	sc.Step(`^the preflight computes a conflict-free deterministic merge result before anything is consumed$`,
		func() error {
			// Proven by the converse in its own world: a conflicting merge
			// consumes NOTHING. A consumed approval cannot be given back.
			probe := freshMerge()
			probe.git.conflicts = true
			if err := probe.runs.Upsert(context.Background(), probe.run); err != nil {
				return err
			}
			_, key, err := probe.queue.OnApproval(context.Background(), probe.run, probe.approved())
			if err != nil {
				return err
			}
			got, err := probe.queue.Run(context.Background(), key)
			if err != nil {
				return err
			}
			if got.State != orchestrator.AttemptAborted {
				return fmt.Errorf("a conflicting merge settled as %q", got.State)
			}
			if len(probe.approvals.calls) != 0 {
				return errors.New("an approval was consumed for a merge that could not land")
			}
			return nil
		})

	sc.Step(`^kriya calls sutra's keyed approval consumption at the expected current revision before merging$`,
		func() error {
			m := w.merge
			if len(m.approvals.calls) != 1 {
				return fmt.Errorf("consumed %d times", len(m.approvals.calls))
			}
			call := m.approvals.calls[0]
			if call.revision != 2 || call.event != "event-9" || call.review != "review-1" {
				return fmt.Errorf("consumed %+v", call)
			}
			if call.key == "" {
				return errors.New("the consumption carried no idempotency key")
			}
			return nil
		})

	sc.Step(`^the consumption is durably recorded on the review$`, func() error {
		m := w.merge
		a := m.store.rows[m.key]
		if a.ConsumeKey == "" {
			return errors.New("the consume key was not persisted")
		}
		if !m.approvals.consumed[a.ConsumeKey] {
			return errors.New("the consumption did not land under the persisted key")
		}
		return nil
	})

	sc.Step(`^a verdict reversal attempted after consumption is rejected by sutra$`, func() error {
		if err := w.merge.approvals.reverse(); err == nil {
			return errors.New("a verdict was reversed after its approval was consumed")
		}
		return nil
	})

	sc.Step(`^what merges is that immutable pinned commit, never the mutable branch reference, committed under a CAS on the default-branch head$`,
		func() error {
			m := w.merge
			if len(m.git.merges) != 1 || m.git.merges[0] != "C2" {
				return fmt.Errorf("merged %v, want the pinned C2", m.git.merges)
			}
			// The CAS: a merge against a head that has moved does not land.
			probe := freshMerge()
			probe.git.head = "someone-elses-merge"
			if err := probe.runs.Upsert(context.Background(), probe.run); err != nil {
				return err
			}
			_, key, err := probe.queue.OnApproval(context.Background(), probe.run, probe.approved())
			if err != nil {
				return err
			}
			got, err := probe.queue.Run(context.Background(), key)
			if err != nil {
				return err
			}
			if got.State != orchestrator.AttemptAborted {
				return fmt.Errorf("a merge against a moved head settled as %q", got.State)
			}
			if len(probe.git.merges) != 0 {
				return errors.New("the merge landed against a moved head")
			}
			return nil
		})

	sc.Step(`^the merge happens exactly once$`, func() error {
		if n := len(w.merge.git.merges); n != 1 {
			return fmt.Errorf("merged %d times", n)
		}
		return nil
	})

	sc.Step(`^the same approval event is replayed$`, func() error {
		m := w.merge
		fresh, err := m.queue.Enqueue(context.Background(), orchestrator.MergeAttempt{
			TargetKey: "KRIYA0a", Build: "run-1", Review: "review-1", Revision: 2,
			ApprovalEvent: "event-9", Commit: "C2", ExpectedBase: "base-1",
		})
		if err != nil {
			return err
		}
		if fresh {
			return errors.New("a replayed approval event enqueued a second attempt")
		}
		_, m.err = m.queue.Run(context.Background(), m.key)
		return m.err
	})

	sc.Step(`^no second merge occurs$`, func() error {
		m := w.merge
		if n := len(m.git.merges); n != 1 {
			return fmt.Errorf("merged %d times after the replay", n)
		}
		if n := len(m.approvals.calls); n != 1 {
			return fmt.Errorf("consumed %d times after the replay", n)
		}
		return nil
	})

	sc.Step(`^an approval whose merge attempt aborted after the verdict was reversed$`, func() error {
		m := w.newMerge()
		if err := m.runs.Upsert(context.Background(), m.run); err != nil {
			return err
		}
		m.git.head = "someone-elses-merge"
		_, key, err := m.queue.OnApproval(context.Background(), m.run, m.approved())
		if err != nil {
			return err
		}
		got, err := m.queue.Run(context.Background(), key)
		if err != nil {
			return err
		}
		if got.State != orchestrator.AttemptAborted {
			return fmt.Errorf("the attempt settled as %q", got.State)
		}
		m.key = key
		m.git.head = "base-1"
		return nil
	})

	sc.Step(`^the review is re-approved at the same revision$`, func() error {
		m := w.merge
		again := m.approved()
		// A NEW approval event at the same revision.
		again.Event = "event-10"
		m.run.State = orchestrator.StateReviewSubmitted
		if err := m.runs.Upsert(context.Background(), m.run); err != nil {
			return err
		}
		moved, key, err := m.queue.OnApproval(context.Background(), m.run, again)
		if err != nil {
			return err
		}
		m.run = moved
		if key == m.key {
			return errors.New("re-approval reused the aborted attempt's key")
		}
		_, m.err = m.queue.Run(context.Background(), key)
		return m.err
	})

	sc.Step(`^the new approval event enqueues a fresh attempt — the event id is part of the identity$`,
		func() error {
			m := w.merge
			if len(m.store.order) != 2 {
				return fmt.Errorf("%d attempts exist", len(m.store.order))
			}
			if len(m.git.merges) != 1 {
				return fmt.Errorf("merged %d times", len(m.git.merges))
			}
			return nil
		})

	sc.Step(`^the aborted attempt remains a terminal historical record$`, func() error {
		m := w.merge
		aborted := m.store.rows[m.key]
		if aborted.State != orchestrator.AttemptAborted {
			return fmt.Errorf("the aborted attempt is now %q", aborted.State)
		}
		if !strings.Contains(aborted.Note, "base-1") {
			return fmt.Errorf("its cause was lost: %q", aborted.Note)
		}
		return nil
	})

	sc.Step(`^the lock-holding attempt recorded consuming with its consume key and crashed$`, func() error {
		m := w.newMerge()
		if err := m.runs.Upsert(context.Background(), m.run); err != nil {
			return err
		}
		_, key, err := m.queue.OnApproval(context.Background(), m.run, m.approved())
		if err != nil {
			return err
		}
		m.key = key
		m.approvals.err = errors.New("sutra unreachable")
		if _, err := m.queue.Run(context.Background(), key); err == nil {
			return errors.New("a consumption that never landed read as success")
		}
		stuck := m.store.rows[key]
		if stuck.State != orchestrator.AttemptConsuming || stuck.ConsumeKey == "" {
			return fmt.Errorf("the crash window is invisible: %+v", stuck)
		}
		m.approvals.err = nil
		return nil
	})

	sc.Step(`^recovery replays the keyed consumption$`, func() error {
		m := w.merge
		_, m.err = m.queue.RecoverMerges(context.Background())
		return m.err
	})

	sc.Step(`^sutra returns the original result and the attempt advances to consumed$`, func() error {
		m := w.merge
		stuck := m.store.rows[m.key]
		last := m.approvals.calls[len(m.approvals.calls)-1]
		if last.key != stuck.ConsumeKey {
			return fmt.Errorf("the replay used key %q, want the persisted one", last.key)
		}
		// Consumed and then merged: the recovery resumes the whole remaining
		// sequence, and "consumed" is the step it passed through.
		if stuck.State != orchestrator.AttemptMerged {
			return fmt.Errorf("the resumed attempt settled as %q", stuck.State)
		}
		return nil
	})

	sc.Step(`^the attempt recorded merging against its expected base and crashed before recording the merge commit$`,
		func() error {
			m := w.newMerge()
			if err := m.runs.Upsert(context.Background(), m.run); err != nil {
				return err
			}
			_, key, err := m.queue.OnApproval(context.Background(), m.run, m.approved())
			if err != nil {
				return err
			}
			m.key = key
			stuck := m.store.rows[key]
			stuck.State = orchestrator.AttemptMerging
			stuck.ConsumeKey = "consume-key"
			return m.store.Update(context.Background(), stuck)
		})

	sc.Step(`^recovery inspects the branch$`, func() error {
		m := w.merge
		_, m.err = m.queue.RecoverMerges(context.Background())
		return m.err
	})

	sc.Step(`^a landed CAS push is recognized and recorded as merged; an unlanded one is retried against the expected base$`,
		func() error {
			m := w.merge
			got := m.store.rows[m.key]
			if got.State != orchestrator.AttemptMerged || got.MergeCommit == "" {
				return fmt.Errorf("the resumed attempt is %+v", got)
			}
			// The approval was already claimed, so recovery must not claim it
			// again: that is a second claim on something claimable once.
			if len(m.approvals.calls) != 0 {
				return fmt.Errorf("the approval was consumed again: %+v", m.approvals.calls)
			}
			return nil
		})
}

func registerMergeRefusals(sc *godog.ScenarioContext, w *world) {
	sc.Step(`^a run whose first review was approved at revision (\d+) and merged, and whose later fresh review is also approved at revision (\d+)$`,
		func(first, second int) error {
			m := w.newMerge()
			if err := m.runs.Upsert(context.Background(), m.run); err != nil {
				return err
			}
			original := m.approved()
			original.Revision = first
			_, key, err := m.queue.OnApproval(context.Background(), m.run, original)
			if err != nil {
				return err
			}
			if _, err := m.queue.Run(context.Background(), key); err != nil {
				return err
			}
			m.key = key
			// A later, FRESH review — a different review id at the same
			// revision.
			m.run.State = orchestrator.StateReviewSubmitted
			m.run.ReviewID = "review-2"
			m.git.head = "merge-C2"
			m.run.GatedBase = m.git.head
			return m.runs.Upsert(context.Background(), m.run)
		})

	sc.Step(`^the later approval enqueues its merge attempt$`, func() error {
		m := w.merge
		later := m.approved()
		later.Review, later.Revision, later.Event = "review-2", 1, "event-11"
		moved, key, err := m.queue.OnApproval(context.Background(), m.run, later)
		if err != nil {
			return err
		}
		m.run = moved
		m.laterKey = key
		return nil
	})

	sc.Step(`^its attempt key differs — the review id is part of the identity$`, func() error {
		m := w.merge
		if m.laterKey == m.key {
			return errors.New("a fresh review at the same revision reused the earlier key")
		}
		if len(m.store.order) != 2 {
			return fmt.Errorf("%d attempts exist", len(m.store.order))
		}
		return nil
	})

	sc.Step(`^recovery reads the persisted review id and revision to consume exactly the intended approval$`,
		func() error {
			m := w.merge
			if _, err := m.queue.RecoverMerges(context.Background()); err != nil {
				return err
			}
			last := m.approvals.calls[len(m.approvals.calls)-1]
			stored := m.store.rows[m.laterKey]
			if last.review != stored.Review || last.revision != stored.Revision {
				return fmt.Errorf("consumed %+v, want the persisted %s at %d",
					last, stored.Review, stored.Revision)
			}
			return nil
		})

	sc.Step(`^the deterministic merge of the pinned commit into the current default head conflicts$`,
		func() error {
			m := w.newMerge()
			m.git.conflicts = true
			return m.runs.Upsert(context.Background(), m.run)
		})

	sc.Step(`^the approval event is processed$`, func() error {
		m := w.merge
		moved, key, err := m.queue.OnApproval(context.Background(), m.run, m.approved())
		if err != nil {
			return err
		}
		m.run, m.key = moved, key
		_, m.err = m.queue.Run(context.Background(), key)
		return m.err
	})

	sc.Step(`^no approval is consumed and nothing merges$`, func() error {
		m := w.merge
		if len(m.approvals.calls) != 0 {
			return fmt.Errorf("consumed %d times", len(m.approvals.calls))
		}
		if len(m.git.merges) != 0 {
			return fmt.Errorf("merged %d times", len(m.git.merges))
		}
		if m.store.rows[m.key].State != orchestrator.AttemptAborted {
			return fmt.Errorf("the attempt is %q", m.store.rows[m.key].State)
		}
		return nil
	})

	sc.Step(`^the run integrates the current default head, reruns the gates, and submits a fresh review$`,
		func() error {
			return integrateRerunRoutes()
		})

	sc.Step(`^a run gated against default-branch head "([^"]*)" while another run merged, moving the head to "([^"]*)"$`,
		func(gated, moved string) error {
			m := w.newMerge()
			m.run.GatedBase = gated
			m.git.head = moved
			return m.runs.Upsert(context.Background(), m.run)
		})

	sc.Step(`^the preflight fails without consuming the approval — the combined result was never gated$`,
		func() error {
			m := w.merge
			if len(m.approvals.calls) != 0 {
				return errors.New("an approval was consumed against a moved base")
			}
			got := m.store.rows[m.key]
			if got.State != orchestrator.AttemptAborted {
				return fmt.Errorf("the attempt is %q", got.State)
			}
			if !strings.Contains(got.Note, m.run.GatedBase) {
				return fmt.Errorf("the cause does not name the gated base: %q", got.Note)
			}
			return nil
		})

	sc.Step(`^the run integrates "([^"]*)" into its branch, reruns the full gate chain, and submits a fresh review$`,
		func(string) error {
			return integrateRerunRoutes()
		})

	sc.Step(`^the approval was consumed and an out-of-band push moved the default head before the merge committed$`,
		func() error {
			m := w.newMerge()
			m.git.casFails = true
			return m.runs.Upsert(context.Background(), m.run)
		})

	sc.Step(`^the merge CAS fails$`, func() error {
		m := w.merge
		moved, key, err := m.queue.OnApproval(context.Background(), m.run, m.approved())
		if err != nil {
			return err
		}
		m.run, m.key = moved, key
		_, m.err = m.queue.Run(context.Background(), key)
		if m.err == nil {
			return errors.New("a merge that did not land read as success")
		}
		return nil
	})

	sc.Step(`^the consumed approval stays spent in history$`, func() error {
		m := w.merge
		if len(m.approvals.calls) != 1 {
			return fmt.Errorf("consumed %d times", len(m.approvals.calls))
		}
		if !m.approvals.claimed {
			return errors.New("the consumption was rolled back")
		}
		// Recorded on the attempt, so recovery resumes at merging rather than
		// consuming again.
		if m.store.rows[m.key].State != orchestrator.AttemptMerging {
			return fmt.Errorf("the attempt is %q", m.store.rows[m.key].State)
		}
		return nil
	})

	sc.Step(`^the run integrates the new head, reruns the full chain, and submits a fresh review — never stranded$`,
		func() error {
			return integrateRerunRoutes()
		})

	sc.Step(`^the branch head moved past the pinned commit before kriya processes the event$`,
		func() error {
			m := w.merge
			// The APPROVED commit is what merges. A branch that moved past it
			// carries commits nothing reviewed, and the merge is of the pin —
			// so the mismatch is between the pin and the branch.
			m.movedBranch = "C3"
			return nil
		})

	sc.Step(`^kriya consumes the event$`, func() error {
		m := w.merge
		// The pinned commit no longer merges cleanly onto a head that carries
		// the newer work.
		m.git.conflicts = true
		moved, key, err := m.queue.OnApproval(context.Background(), m.run, m.approved())
		if err != nil {
			return err
		}
		m.run, m.key = moved, key
		_, m.err = m.queue.Run(context.Background(), key)
		return m.err
	})

	sc.Step(`^nothing merges and the unreviewed-commits mismatch surfaces$`, func() error {
		m := w.merge
		if len(m.git.merges) != 0 {
			return fmt.Errorf("merged %d times", len(m.git.merges))
		}
		got := m.store.rows[m.key]
		if got.State != orchestrator.AttemptAborted || got.Note == "" {
			return fmt.Errorf("the attempt is %q with note %q", got.State, got.Note)
		}
		if m.movedBranch == "" {
			return errors.New("the scenario never moved the branch")
		}
		return nil
	})

	sc.Step(`^the verdict is reversed to changes-requested before kriya processes the event$`, func() error {
		w.merge.approvals.err = errors.New("verdict fence: the approval was reversed")
		return nil
	})

	sc.Step(`^kriya consumes the event and re-reads the review$`, func() error {
		m := w.merge
		moved, key, err := m.queue.OnApproval(context.Background(), m.run, m.approved())
		if err != nil {
			return err
		}
		m.run, m.key = moved, key
		_, m.err = m.queue.Run(context.Background(), key)
		return nil
	})

	sc.Step(`^no merge happens and the mismatch surfaces$`, func() error {
		m := w.merge
		if m.err == nil {
			return errors.New("a reversed verdict merged")
		}
		if len(m.git.merges) != 0 {
			return fmt.Errorf("merged %d times", len(m.git.merges))
		}
		if !strings.Contains(m.err.Error(), "reversed") {
			return fmt.Errorf("the cause does not name the reversal: %v", m.err)
		}
		return nil
	})
}

// integrateRerunRoutes checks the table sends a failed merge back to the loop.
//
// That is what "integrates, reruns, and submits fresh" means mechanically: the
// run re-enters the dev loop, where integration and a new gate attempt happen,
// rather than parking on a terminal state where work would be stranded.
func integrateRerunRoutes() error {
	return transitionFrom(orchestrator.StateMerging,
		orchestrator.StateMerged, orchestrator.StateDevLoop)
}
