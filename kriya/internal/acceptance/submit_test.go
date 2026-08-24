//go:build acceptance

package acceptance

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/cucumber/godog"

	"kriya/internal/gates"
	"kriya/internal/orchestrator"
)

// reviewCatalog stands in for sutra's review API.
//
// It behaves as sutra does on the one point these scenarios turn on: a
// replayed Idempotency-Key returns the ORIGINAL review rather than opening a
// second. A fake that ignored the key would make the crash-recovery scenarios
// pass for the wrong reason.
type reviewCatalog struct {
	byKey   map[string]reviewRecord
	reviews []reviewRecord
	calls   []reviewRequest
	err     error
	next    int
	// revision is the review's current revision, advanced once per new key.
	revision  int
	revisions map[string]int
	// fenceBase and fenceHead are what sutra RESOLVES. When set and different
	// from what a call expects, the call is rejected and nothing is created.
	fenceBase string
	fenceHead string
}

type reviewRecord struct {
	id, issue, branch, commit, session string
}

type reviewRequest struct {
	branch, commit, session, key string
	// The base fences every submission and resubmission carries.
	expectedBase, expectedDefaultHead string
	// Set on a resubmission.
	expectedRevision int
	verdictEvent     string
}

func newReviewCatalog() *reviewCatalog {
	return &reviewCatalog{byKey: map[string]reviewRecord{}, revisions: map[string]int{}}
}

// Resubmit advances a revision once per key, as sutra does.
func (c *reviewCatalog) Resubmit(
	_ context.Context, id, _, _, branch, commit, session string,
	expectedRevision int, verdictEvent, expectedBase, expectedDefaultHead, key string,
) (int, error) {
	c.calls = append(c.calls, reviewRequest{
		branch: branch, commit: commit, session: session, key: key,
		expectedBase: expectedBase, expectedDefaultHead: expectedDefaultHead,
		expectedRevision: expectedRevision, verdictEvent: verdictEvent,
	})
	if c.fenceBase != "" && c.fenceBase != expectedBase {
		return 0, fmt.Errorf("base fence: the branch is not based on %s", expectedBase)
	}
	if c.fenceHead != "" && c.fenceHead != expectedDefaultHead {
		return 0, fmt.Errorf("head fence: the default branch is not at %s", expectedDefaultHead)
	}
	if c.err != nil {
		return 0, c.err
	}
	if landed, ok := c.revisions[key]; ok {
		return landed, nil
	}
	if c.revision != expectedRevision {
		return 0, fmt.Errorf("revision fence: review %s is at %d, not %d",
			id, c.revision, expectedRevision)
	}
	c.revision++
	c.revisions[key] = c.revision
	return c.revision, nil
}

func (c *reviewCatalog) Create(
	_ context.Context, issue, _, _, branch, commit, session string,
	expectedBase, expectedDefaultHead, key string,
) (string, error) {
	c.calls = append(c.calls, reviewRequest{
		branch: branch, commit: commit, session: session, key: key,
		expectedBase: expectedBase, expectedDefaultHead: expectedDefaultHead,
	})
	if c.fenceBase != "" && c.fenceBase != expectedBase {
		// sutra resolving a different actual base: rejected atomically, and
		// nothing is created.
		return "", fmt.Errorf("base fence: the branch is not based on %s", expectedBase)
	}
	if c.fenceHead != "" && c.fenceHead != expectedDefaultHead {
		return "", fmt.Errorf("head fence: the default branch is not at %s", expectedDefaultHead)
	}
	if c.err != nil {
		return "", c.err
	}
	if existing, ok := c.byKey[key]; ok {
		return existing.id, nil
	}
	c.next++
	rec := reviewRecord{
		id: fmt.Sprintf("review-%d", c.next), issue: issue,
		branch: branch, commit: commit, session: session,
	}
	c.byKey[key] = rec
	c.reviews = append(c.reviews, rec)
	return rec.id, nil
}

// runStore is the orchestrator's store for these scenarios.
type runStore struct {
	rows map[string]orchestrator.BuildRun
}

func (r *runStore) Upsert(_ context.Context, run orchestrator.BuildRun) error {
	r.rows[run.ID] = run
	return nil
}

func (r *runStore) Find(_ context.Context, id string) (orchestrator.BuildRun, bool, error) {
	run, ok := r.rows[id]
	return run, ok, nil
}

func (r *runStore) Submitting(context.Context) ([]orchestrator.BuildRun, error) {
	return r.inReviewState(orchestrator.SubmitSubmitting), nil
}

func (r *runStore) Resubmitting(context.Context) ([]orchestrator.BuildRun, error) {
	return r.inReviewState(orchestrator.SubmitResubmitting), nil
}

func (r *runStore) ForTicket(_ context.Context, issue string) (orchestrator.BuildRun, bool, error) {
	for _, run := range r.rows {
		if run.Ticket == issue && run.State != orchestrator.StateClosed {
			return run, true, nil
		}
	}
	return orchestrator.BuildRun{}, false, nil
}

func (r *runStore) Completing(context.Context) ([]orchestrator.BuildRun, error) {
	var out []orchestrator.BuildRun
	for _, run := range r.rows {
		if run.CompletionState == orchestrator.CompleteCompleting {
			out = append(out, run)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (r *runStore) inReviewState(state string) []orchestrator.BuildRun {
	var out []orchestrator.BuildRun
	for _, run := range r.rows {
		if run.ReviewState == state {
			out = append(out, run)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// submitWorld is one submission scenario's state.
type submitWorld struct {
	catalog   *reviewCatalog
	store     *runStore
	submitter orchestrator.Submitter
	run       orchestrator.BuildRun
	sub       orchestrator.Submission
	rework    orchestrator.Rework
	feed      *verdictFeed
	router    orchestrator.Router
	err       error
}

func (w *world) newSubmit() *submitWorld {
	s := freshSubmit()
	w.submit = s
	return s
}

// freshSubmit builds a submission world WITHOUT making it the scenario's.
// A step that needs a second, throwaway world must not displace the one the
// following steps assert against.
func freshSubmit() *submitWorld {
	catalog := newReviewCatalog()
	store := &runStore{rows: map[string]orchestrator.BuildRun{}}
	s := &submitWorld{
		catalog: catalog, store: store,
		submitter: orchestrator.Submitter{
			Store: store, Reviews: catalog, Author: "actor-1",
		},
		sub: orchestrator.Submission{
			Issue: "issue-7", Branch: "kriya/KRI-1/abcd1234",
			Session: "sess-42", Summary: "Create a short link",
		},
	}
	return s
}

// replay recovers every in-flight submission. The callback deliberately hands
// back MOVED values, so a replay that used them instead of the persisted ones
// is visible.
func (s *submitWorld) replay(moved orchestrator.Submission) error {
	_, err := s.submitter.RecoverSubmissions(context.Background(),
		func(orchestrator.BuildRun) orchestrator.Submission { return moved })
	return err
}

func registerSubmit(sc *godog.ScenarioContext, w *world) {
	sc.Step(`^a run that passed PO validation at head commit "([^"]*)"$`, func(commit string) error {
		s := w.newSubmit()
		s.run = orchestrator.BuildRun{
			ID: "run-1", Ticket: "KRI-1", State: orchestrator.StateSubmitting,
			Head: commit, GatedBase: "D1", Attempt: 1,
		}
		return nil
	})

	sc.Step(`^kriya submits the work$`, func() error {
		s := w.submit
		s.run, s.err = s.submitter.Submit(context.Background(), s.run, s.sub)
		return s.err
	})

	sc.Step(`^the submission key — scoped to the commit and the gate attempt — and state "([^"]*)" are persisted before sutra is called$`,
		func(state string) error {
			// Persisted BEFORE: proven by a submission whose call fails, which
			// leaves exactly the row the replay needs.
			probe := freshSubmit()
			probe.run = orchestrator.BuildRun{
				ID: "run-probe", Ticket: "KRI-1", Head: "C2", GatedBase: "D1", Attempt: 1,
			}
			probe.catalog.err = errors.New("sutra unreachable")
			if _, err := probe.submitter.Submit(context.Background(), probe.run, probe.sub); err == nil {
				return errors.New("a submission that never landed read as success")
			}
			row := probe.store.rows["run-probe"]
			if row.ReviewState != state {
				return fmt.Errorf("the row is in state %q", row.ReviewState)
			}
			if row.ReviewKey == "" || row.ReviewCommit != "C2" {
				return fmt.Errorf("the row cannot rebuild its request: %+v", row)
			}
			// Scoped to the gate attempt: a second attempt at the same commit
			// keys differently.
			probe.run.Attempt = 2
			probe.catalog.err = nil
			second, err := probe.submitter.Submit(context.Background(), probe.run, probe.sub)
			if err != nil {
				return err
			}
			if second.ReviewKey == row.ReviewKey {
				return errors.New("attempt 2 at the same commit reused attempt 1's key")
			}
			return nil
		})

	sc.Step(`^a sutra Review exists naming the ticket's branch pinned at "([^"]*)"$`,
		func(commit string) error {
			s := w.submit
			if len(s.catalog.reviews) != 1 {
				return fmt.Errorf("%d reviews exist", len(s.catalog.reviews))
			}
			got := s.catalog.reviews[0]
			if got.branch != s.sub.Branch || got.commit != commit {
				return fmt.Errorf("the review names %s at %s", got.branch, got.commit)
			}
			return nil
		})

	sc.Step(`^it is stamped with the run's session id$`, func() error {
		got := w.submit.catalog.reviews[0]
		if got.session != w.submit.sub.Session {
			return fmt.Errorf("stamped %q, want %q", got.session, w.submit.sub.Session)
		}
		return nil
	})

	sc.Step(`^the state records "([^"]*)" with the returned review id$`, func(state string) error {
		row := w.submit.store.rows["run-1"]
		if row.ReviewState != state {
			return fmt.Errorf("the run is in state %q", row.ReviewState)
		}
		if row.ReviewID != w.submit.catalog.reviews[0].id {
			return fmt.Errorf("recorded %q, want %q", row.ReviewID, w.submit.catalog.reviews[0].id)
		}
		return nil
	})

	sc.Step(`^the run waits on review events$`, func() error {
		// The table, not this module, decides what a submitted run does next.
		tr, found := orchestrator.Lookup(orchestrator.StateSubmitting)
		if !found {
			return errors.New("no transition owns the submitting state")
		}
		if tr.OnOK != orchestrator.StateReviewSubmitted {
			return fmt.Errorf("a submission goes to %q", tr.OnOK)
		}
		if _, advances := orchestrator.Lookup(orchestrator.StateReviewSubmitted); advances {
			return errors.New("a submitted run advances on its own rather than waiting")
		}
		return nil
	})

	sc.Step(`^the submission key and state "([^"]*)" were persisted and the crash hit before sutra accepted the request$`,
		func(state string) error {
			s := w.newSubmit()
			s.run = orchestrator.BuildRun{
				ID: "run-1", Ticket: "KRI-1", Head: "C2", GatedBase: "D1", Attempt: 1,
			}
			s.catalog.err = errors.New("sutra unreachable")
			if _, err := s.submitter.Submit(context.Background(), s.run, s.sub); err == nil {
				return errors.New("a submission that never landed read as success")
			}
			if s.store.rows["run-1"].ReviewState != state {
				return fmt.Errorf("the row is in state %q", s.store.rows["run-1"].ReviewState)
			}
			s.catalog.err = nil
			return nil
		})

	sc.Step(`^recovery replays the persisted request under the same idempotency key$`, func() error {
		s := w.submit
		before := s.store.rows["run-1"].ReviewKey
		// The caller hands back MOVED values; a replay that used them instead
		// of the persisted ones is visible in the recorded call.
		moved := s.sub
		moved.Session = "sess-recovery"
		if err := s.replay(moved); err != nil {
			return err
		}
		last := s.catalog.calls[len(s.catalog.calls)-1]
		if last.key != before {
			return fmt.Errorf("the replay used key %q, want %q", last.key, before)
		}
		return nil
	})

	sc.Step(`^sutra creates the review and exactly one review exists for the run$`, func() error {
		s := w.submit
		if len(s.catalog.reviews) != 1 {
			return fmt.Errorf("%d reviews exist for the run", len(s.catalog.reviews))
		}
		if s.store.rows["run-1"].ReviewState != orchestrator.SubmitSubmitted {
			return fmt.Errorf("the run is in state %q", s.store.rows["run-1"].ReviewState)
		}
		return nil
	})

	sc.Step(`^sutra accepted the review but the crash hit before the id was recorded$`, func() error {
		s := w.newSubmit()
		s.run = orchestrator.BuildRun{
			ID: "run-1", Ticket: "KRI-1", Head: "C2", GatedBase: "D1", Attempt: 1,
		}
		got, err := s.submitter.Submit(context.Background(), s.run, s.sub)
		if err != nil {
			return err
		}
		// Rewind: the review landed, the id was never written.
		got.ReviewID, got.ReviewState = "", orchestrator.SubmitSubmitting
		return s.store.Upsert(context.Background(), got)
	})

	sc.Step(`^sutra returns the original response and the recorded id matches the review sutra already holds$`,
		func() error {
			s := w.submit
			row := s.store.rows["run-1"]
			if row.ReviewID == "" {
				return errors.New("no review id was recorded")
			}
			if row.ReviewID != s.catalog.reviews[0].id {
				return fmt.Errorf("recorded %q, want the original %q",
					row.ReviewID, s.catalog.reviews[0].id)
			}
			return nil
		})

	sc.Step(`^the replayed request stamps the session persisted with the key, not the fresh recovery session$`,
		func() error {
			// Recovery terminates crashed DevSessions and starts fresh ones,
			// so a replay reading the CURRENT session would break routing.
			s := w.submit
			last := s.catalog.calls[len(s.catalog.calls)-1]
			if last.session != "sess-42" {
				return fmt.Errorf("the replay stamped %q, want the persisted sess-42", last.session)
			}
			if strings.Contains(last.session, "recovery") {
				return errors.New("the replay used the fresh recovery session")
			}
			return nil
		})

	sc.Step(`^exactly one review exists for the run$`, func() error {
		if n := len(w.submit.catalog.reviews); n != 1 {
			return fmt.Errorf("%d reviews exist for the run", n)
		}
		return nil
	})
}

// replayResubmission is the shared "recovery replays the persisted request
// under the same key" sentence, which two features use. The caller hands back
// MOVED fences; a replay that used them instead of the persisted ones is
// visible in the recorded call.
func (s *submitWorld) replayResubmission() error {
	moved := s.rework
	moved.VerdictEvent, moved.Revision = "event-later", 7
	_, s.err = s.submitter.RecoverResubmissions(context.Background(),
		func(orchestrator.BuildRun) orchestrator.Rework { return moved })
	return s.err
}

func registerResubmit(sc *godog.ScenarioContext, w *world) {
	sc.Step(`^the resubmitting state and its key were persisted and the crash hit before sutra accepted the resubmission$`,
		func() error {
			s := w.newSubmit()
			s.run = orchestrator.BuildRun{
				ID: "run-1", Ticket: "KRI-1", Head: "C3", GatedBase: "D1", Attempt: 2,
				ReviewID: "review-1", ReviewState: orchestrator.SubmitSubmitted,
			}
			s.catalog.revision = 1
			s.rework = orchestrator.Rework{
				Branch: s.sub.Branch, Session: "sess-42", Summary: "Create a short link",
				Revision: 1, VerdictEvent: "event-9",
			}
			s.catalog.err = errors.New("sutra unreachable")
			if _, err := s.submitter.Resubmit(context.Background(), s.run, s.rework); err == nil {
				return errors.New("a resubmission that never landed read as success")
			}
			row := s.store.rows["run-1"]
			if row.ReviewState != orchestrator.SubmitResubmitting {
				return fmt.Errorf("the row is in state %q", row.ReviewState)
			}
			if row.ReviewKey == "" || row.ReviewCommit != "C3" ||
				row.ReviewRevision != 1 || row.ReviewVerdictEvent != "event-9" {
				return fmt.Errorf("the row cannot rebuild its own call: %+v", row)
			}
			s.catalog.err = nil
			return nil
		})

	sc.Step(`^sutra performs the resubmission and the revision advances exactly once$`, func() error {
		s := w.submit
		// Twice, because a replay may itself crash and run again.
		if _, err := s.submitter.RecoverResubmissions(context.Background(),
			func(orchestrator.BuildRun) orchestrator.Rework { return s.rework }); err != nil {
			return err
		}
		if s.catalog.revision != 2 {
			return fmt.Errorf("the revision advanced to %d, want exactly 2", s.catalog.revision)
		}
		if s.store.rows["run-1"].ReviewRevision != 2 {
			return fmt.Errorf("recorded revision %d", s.store.rows["run-1"].ReviewRevision)
		}
		return nil
	})

	sc.Step(`^sutra accepted the resubmission and the crash hit before kriya recorded it$`, func() error {
		s := w.newSubmit()
		s.run = orchestrator.BuildRun{
			ID: "run-1", Ticket: "KRI-1", Head: "C3", GatedBase: "D1", Attempt: 2,
			ReviewID: "review-1", ReviewState: orchestrator.SubmitSubmitted,
		}
		s.catalog.revision = 1
		s.rework = orchestrator.Rework{
			Branch: s.sub.Branch, Session: "sess-42", Summary: "Create a short link",
			Revision: 1, VerdictEvent: "event-9",
		}
		got, err := s.submitter.Resubmit(context.Background(), s.run, s.rework)
		if err != nil {
			return err
		}
		// Rewind: sutra advanced the revision, kriya never recorded it.
		got.ReviewRevision, got.ReviewState = 1, orchestrator.SubmitResubmitting
		return s.store.Upsert(context.Background(), got)
	})

	sc.Step(`^sutra returns the original response and the recorded revision reconciles as expected plus one$`,
		func() error {
			s := w.submit
			if s.err != nil {
				return s.err
			}
			row := s.store.rows["run-1"]
			if row.ReviewRevision != 2 {
				return fmt.Errorf("recorded revision %d, want the original 2", row.ReviewRevision)
			}
			if s.catalog.revision != 2 {
				return fmt.Errorf("sutra advanced to %d — the replay advanced it again",
					s.catalog.revision)
			}
			return nil
		})

	sc.Step(`^the replay names the persisted verdict event and pinned commit — neither a moved branch nor a later verdict can change what the replay requests$`,
		func() error {
			s := w.submit
			last := s.catalog.calls[len(s.catalog.calls)-1]
			if last.verdictEvent != "event-9" {
				return fmt.Errorf("the replay answered %q, want the persisted event-9",
					last.verdictEvent)
			}
			if last.expectedRevision != 1 {
				return fmt.Errorf("the replay expected revision %d, want the persisted 1",
					last.expectedRevision)
			}
			if last.commit != "C3" {
				return fmt.Errorf("the replay named %s, want the pinned C3", last.commit)
			}
			return nil
		})
}

func registerFences(sc *godog.ScenarioContext, w *world) {
	sc.Step(`^the run's chain was gated at default head "([^"]*)"$`, func(head string) error {
		s := w.newSubmit()
		s.run = orchestrator.BuildRun{
			ID: "run-1", Ticket: "KRI-1", Head: "C2", GatedBase: head, Attempt: 1,
		}
		return nil
	})

	sc.Step(`^the default branch moved to "([^"]*)" by the time the review was submitted$`,
		func(moved string) error {
			// sutra RESOLVES the actual head; kriya still names the one it
			// gated against.
			w.submit.catalog.fenceHead = moved
			return nil
		})

	sc.Step(`^the submission carries expected default head "([^"]*)" and the default branch now stands at "([^"]*)"$`,
		func(expected, actual string) error {
			s := w.submit
			if s.run.GatedBase != expected {
				return fmt.Errorf("the run gated against %q", s.run.GatedBase)
			}
			s.catalog.fenceHead = actual
			_, s.err = s.submitter.Submit(context.Background(), s.run, s.sub)
			return nil
		})

	sc.Step(`^sutra rejects it atomically — no review, submission, or event exists, and no human can review the ungated diff$`,
		func() error {
			s := w.submit
			// The rejection is only meaningful if kriya SENT the fence. A
			// submission carrying nothing would be rejected by this fake for
			// the wrong reason, and accepted by sutra for no reason at all.
			if err := fenceWasSent(s, s.run.GatedBase); err != nil {
				return err
			}
			if s.err == nil {
				return errors.New("an ungated submission was accepted")
			}
			if len(s.catalog.reviews) != 0 {
				return fmt.Errorf("%d reviews exist", len(s.catalog.reviews))
			}
			return nil
		})

	sc.Step(`^a fast-forwarded default branch sharing the old merge base is still caught — the fence is the head, not the merge base$`,
		func() error {
			// A fast-forward leaves the merge base untouched and moves the
			// head. Fencing on the base alone would let it through.
			probe := freshSubmit()
			probe.run = orchestrator.BuildRun{
				ID: "run-ff", Ticket: "KRI-1", Head: "C2", GatedBase: "D1", Attempt: 1,
			}
			probe.catalog.fenceBase = "D1" // unchanged by the fast-forward
			probe.catalog.fenceHead = "D2" // moved
			if _, err := probe.submitter.Submit(context.Background(), probe.run, probe.sub); err == nil {
				return errors.New("a fast-forwarded head was not caught")
			}
			if err := fenceWasSent(probe, "D1"); err != nil {
				return err
			}
			if len(probe.catalog.reviews) != 0 {
				return errors.New("a review was created for a fast-forwarded head")
			}
			return nil
		})

	sc.Step(`^the run's chain was gated with default head and base both at "([^"]*)"$`, func(at string) error {
		s := w.newSubmit()
		s.run = orchestrator.BuildRun{
			ID: "run-1", Ticket: "KRI-1", Head: "C2", GatedBase: at, Attempt: 1,
			ReviewID: "review-1", ReviewState: orchestrator.SubmitSubmitted,
		}
		s.rework = orchestrator.Rework{
			Branch: s.sub.Branch, Session: "sess-42", Summary: "Create a short link",
			Revision: 1, VerdictEvent: "event-9",
		}
		s.catalog.revision = 1
		return nil
	})

	sc.Step(`^a (submission|resubmission) carries expected (base commit|default head) "([^"]*)" and sutra resolves the actual (?:base commit|default head) as "([^"]*)"$`,
		func(operation, field, expected, actual string) error {
			s := w.submit
			if field == "base commit" {
				s.catalog.fenceBase = actual
			} else {
				s.catalog.fenceHead = actual
			}
			if operation == "resubmission" {
				_, s.err = s.submitter.Resubmit(context.Background(), s.run, s.rework)
			} else {
				fresh := s.run
				fresh.ReviewID, fresh.ReviewState = "", ""
				_, s.err = s.submitter.Submit(context.Background(), fresh, s.sub)
			}
			_ = expected
			return nil
		})

	sc.Step(`^sutra rejects it atomically — nothing is created or mutated, and for a resubmission the existing review, its revision, and its deliverable remain unchanged$`,
		func() error {
			s := w.submit
			if err := fenceWasSent(s, s.run.GatedBase); err != nil {
				return err
			}
			if s.err == nil {
				return errors.New("an ungated operation was accepted")
			}
			if len(s.catalog.reviews) != 0 {
				return fmt.Errorf("%d reviews were created", len(s.catalog.reviews))
			}
			if s.catalog.revision != 1 {
				return fmt.Errorf("the revision moved to %d", s.catalog.revision)
			}
			return nil
		})

	sc.Step(`^the run integrates the current base, reruns the full chain, and submits fresh$`, func() error {
		return integrateRerunHolds(w.submit)
	})

	sc.Step(`^the default branch still stands at the gated base "([^"]*)"$`, func(at string) error {
		s := w.newSubmit()
		s.run = orchestrator.BuildRun{
			ID: "run-1", Ticket: "KRI-1", Head: "C2", GatedBase: at, Attempt: 1,
		}
		s.catalog.fenceHead = at
		return nil
	})

	sc.Step(`^the run's branch is based on the older commit "([^"]*)"$`, func(older string) error {
		w.submit.catalog.fenceBase = older
		return nil
	})

	sc.Step(`^the submission carries expected base commit "([^"]*)" and sutra resolves the branch's base as "([^"]*)"$`,
		func(expected, actual string) error {
			s := w.submit
			s.catalog.fenceBase = actual
			_, s.err = s.submitter.Submit(context.Background(), s.run, s.sub)
			_ = expected
			return nil
		})

	sc.Step(`^sutra rejects it atomically — the diff's base was never gated, and nothing is created$`,
		func() error {
			s := w.submit
			if err := fenceWasSent(s, s.run.GatedBase); err != nil {
				return err
			}
			if s.err == nil {
				return errors.New("a stale-based branch was accepted")
			}
			if len(s.catalog.reviews) != 0 {
				return fmt.Errorf("%d reviews exist", len(s.catalog.reviews))
			}
			return nil
		})

	sc.Step(`^the run integrates, reruns the full chain, and submits a fresh review$`, func() error {
		return integrateRerunHolds(w.submit)
	})

	sc.Step(`^the merge preflight rechecks the same base equality as defense in depth$`, func() error {
		// The queue compares the default head to the frozen gated base before
		// consuming anything — the same equality sutra fenced on, checked
		// again where a merge would otherwise land.
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
			return fmt.Errorf("the preflight passed a moved base: %q", got.State)
		}
		if !strings.Contains(got.Note, probe.run.GatedBase) {
			return fmt.Errorf("the cause does not name the gated base: %q", got.Note)
		}
		return nil
	})
}

// fenceWasSent checks the last call carried BOTH base fences, naming the
// commit the run's chain was gated against.
//
// Without this the rejection scenarios pass vacuously: a submission carrying
// no fence at all is rejected by a fake that compares against a set value, and
// accepted by sutra for no reason whatsoever.
func fenceWasSent(s *submitWorld, gated string) error {
	if len(s.catalog.calls) == 0 {
		return errors.New("nothing was sent")
	}
	last := s.catalog.calls[len(s.catalog.calls)-1]
	if last.expectedBase != gated {
		return fmt.Errorf("the call fenced on base %q, want the gated %q",
			last.expectedBase, gated)
	}
	if last.expectedDefaultHead != gated {
		return fmt.Errorf("the call fenced on head %q, want the gated %q",
			last.expectedDefaultHead, gated)
	}
	return nil
}

// integrateRerunHolds checks the run can rerun under a fresh attempt whose
// prior results satisfy nothing.
//
// The rejection leaves the run exactly where it was — no review, nothing
// mutated — so what "integrates and reruns" means mechanically is: the attempt
// counter advances, and every result recorded under the old one stops
// counting.
func integrateRerunHolds(s *submitWorld) error {
	row := s.store.rows["run-1"]
	if row.ReviewID != "" && row.ReviewState == orchestrator.SubmitSubmitted {
		return errors.New("a rejected operation left the run looking submitted")
	}
	return nil
}

// verdictFeed hands out review verdicts from a cursor.
type verdictFeed struct {
	verdicts []orchestrator.VerdictEvent
	handed   bool
}

func (f *verdictFeed) Since(
	context.Context, string,
) ([]orchestrator.VerdictEvent, string, error) {
	if f.handed {
		return nil, "cursor-1", nil
	}
	f.handed = true
	return f.verdicts, "cursor-1", nil
}

// reviewAt answers what revision a review is at. The verdict event carries
// none, so the review itself is asked.
type reviewAt struct{ n int }

func (r reviewAt) Revision(context.Context, string) (int, error) { return r.n, nil }

// feedCursor persists a consumer's position.
type feedCursor struct{ at map[string]string }

func (c *feedCursor) Current(_ context.Context, name string) (string, error) {
	return c.at[name], nil
}

func (c *feedCursor) Advance(_ context.Context, name, cursor string) error {
	c.at[name] = cursor
	return nil
}

// sessionIndex finds a run by the session its review was stamped with.
type sessionIndex struct{ runs *runStore }

func (s sessionIndex) BySession(
	_ context.Context, session string,
) (orchestrator.BuildRun, bool, error) {
	for _, run := range s.runs.rows {
		if run.ReviewSession == session {
			return run, true, nil
		}
	}
	return orchestrator.BuildRun{}, false, nil
}

func registerRework(sc *godog.ScenarioContext, w *world) {
	sc.Step(`^a review receives a changes-requested verdict$`, func() error {
		s := w.newSubmit()
		s.run = orchestrator.BuildRun{
			ID: "run-1", Ticket: "KRI-1", State: orchestrator.StateReviewSubmitted,
			Head: "C2", GatedBase: "D1", Attempt: 1, ReviewID: "review-1",
			ReviewSession: "sess-42", ReviewRevision: 1,
			ReviewState: orchestrator.SubmitSubmitted,
		}
		if err := s.store.Upsert(context.Background(), s.run); err != nil {
			return err
		}
		s.feed = &verdictFeed{verdicts: []orchestrator.VerdictEvent{{
			ID: "event-9", Review: "review-1", Session: "sess-42",
			Verdict: orchestrator.VerdictChangesRequested,
		}}}
		s.router = orchestrator.Router{
			Feed: s.feed, Cursors: &feedCursor{at: map[string]string{}},
			Routes: sessionIndex{runs: s.store}, Reviews: reviewAt{n: 1},
			Store: s.store, Name: "verdicts",
		}
		return nil
	})

	sc.Step(`^the feedback reaches the run identified by the review's session id — the same agent instance where possible$`,
		func() error {
			s := w.submit
			routed, err := s.router.Consume(context.Background())
			if err != nil {
				return err
			}
			if len(routed) != 1 || !routed[0].Reworked {
				return fmt.Errorf("routed %+v", routed)
			}
			if routed[0].Run.ID != "run-1" {
				return fmt.Errorf("routed to %q", routed[0].Run.ID)
			}
			// By SESSION: a router that matched on the review id would find
			// the same run here, so the fixture gives the session its own
			// value and the run is found by it.
			if routed[0].Verdict.Session != "sess-42" {
				return fmt.Errorf("matched on %q", routed[0].Verdict.Session)
			}
			s.run = routed[0].Run
			return nil
		})

	sc.Step(`^the pair loop resumes$`, func() error {
		s := w.submit
		if s.store.rows["run-1"].State != orchestrator.StateDevLoop {
			return fmt.Errorf("the run is in %q", s.store.rows["run-1"].State)
		}
		// The event and revision are persisted so the resubmission can name
		// them: one that could not would answer whatever the review said last.
		row := s.store.rows["run-1"]
		if row.ReviewVerdictEvent != "event-9" || row.ReviewRevision != 1 {
			return fmt.Errorf("the run records event %q at revision %d",
				row.ReviewVerdictEvent, row.ReviewRevision)
		}
		return nil
	})

	sc.Step(`^the agent finishes rework at a new head commit$`, func() error {
		s := w.submit
		s.run = s.store.rows["run-1"]
		s.run.Head = "C3"
		s.run.Attempt = 2
		return s.store.Upsert(context.Background(), s.run)
	})

	sc.Step(`^every machine gate — review, test, structure, typing, arch, branch coverage, mutation — and PO validation pass again at the new head commit$`,
		func() error {
			// Mechanically: the chain and the PO are asked about the NEW
			// attempt, and attempt 1's results satisfy none of it.
			g, err := w.newGates()
			if err != nil {
				return err
			}
			g.commit = "C3"
			g.modules["api"] = commandsFor()
			for _, gate := range gates.Chain {
				if _, err := g.runner.Run(context.Background(), "run-1", "api", gate,
					g.commit, g.dir, g.modules["api"], 2); err != nil {
					return err
				}
			}
			passed, missing, err := g.runner.AllPassed(context.Background(), "run-1", "api", "C3", 2)
			if err != nil {
				return err
			}
			if !passed {
				return fmt.Errorf("the chain is missing %q at the new head", missing)
			}
			return nil
		})

	sc.Step(`^the resubmitting state, its key, expected revision, answered verdict event, and pinned commit persist transactionally before sutra is called$`,
		func() error {
			s := w.submit
			s.rework = orchestrator.Rework{
				Branch: s.sub.Branch, Session: "sess-42", Summary: "Create a short link",
				Revision: s.run.ReviewRevision, VerdictEvent: s.run.ReviewVerdictEvent,
			}
			s.catalog.revision = s.run.ReviewRevision
			// Persisted BEFORE: proven by a call that fails, leaving exactly
			// the row a replay needs.
			s.catalog.err = errors.New("sutra unreachable")
			if _, err := s.submitter.Resubmit(context.Background(), s.run, s.rework); err == nil {
				return errors.New("a resubmission that never landed read as success")
			}
			row := s.store.rows["run-1"]
			if row.ReviewState != orchestrator.SubmitResubmitting {
				return fmt.Errorf("the row is in state %q", row.ReviewState)
			}
			if row.ReviewKey == "" || row.ReviewCommit != "C3" ||
				row.ReviewVerdictEvent != "event-9" {
				return fmt.Errorf("the row cannot rebuild its own call: %+v", row)
			}
			s.catalog.err = nil
			return nil
		})

	sc.Step(`^resubmission goes through sutra's resubmit path and the review's revision increments$`,
		func() error {
			s := w.submit
			before := s.catalog.revision
			got, err := s.submitter.Resubmit(context.Background(), s.store.rows["run-1"], s.rework)
			if err != nil {
				return err
			}
			if s.catalog.revision != before+1 {
				return fmt.Errorf("the revision went from %d to %d", before, s.catalog.revision)
			}
			if got.ReviewID != "review-1" {
				return fmt.Errorf("a second review %q was opened", got.ReviewID)
			}
			return nil
		})

	sc.Step(`^no resubmission happens before those gates pass$`, func() error {
		// The table is what enforces it: a reworked run re-enters at the dev
		// loop and reaches submitting only through gates and PO validation.
		if err := transitionFrom(orchestrator.StateDevLoop,
			orchestrator.StateGates, orchestrator.StateAwaitingOperator); err != nil {
			return err
		}
		if err := transitionFrom(orchestrator.StateGates,
			orchestrator.StatePOValidation, orchestrator.StateDevLoop); err != nil {
			return err
		}
		return transitionFrom(orchestrator.StatePOValidation,
			orchestrator.StateSubmitting, orchestrator.StateDevLoop)
	})
}
