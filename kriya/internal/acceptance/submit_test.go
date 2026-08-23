//go:build acceptance

package acceptance

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/cucumber/godog"

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
}

type reviewRecord struct {
	id, issue, branch, commit, session string
}

type reviewRequest struct {
	branch, commit, session, key string
}

func newReviewCatalog() *reviewCatalog {
	return &reviewCatalog{byKey: map[string]reviewRecord{}}
}

func (c *reviewCatalog) Create(
	_ context.Context, issue, _, _, branch, commit, session, key string,
) (string, error) {
	c.calls = append(c.calls, reviewRequest{
		branch: branch, commit: commit, session: session, key: key,
	})
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
	var out []orchestrator.BuildRun
	for _, run := range r.rows {
		if run.ReviewState == orchestrator.SubmitSubmitting {
			out = append(out, run)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// submitWorld is one submission scenario's state.
type submitWorld struct {
	catalog   *reviewCatalog
	store     *runStore
	submitter orchestrator.Submitter
	run       orchestrator.BuildRun
	sub       orchestrator.Submission
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
			GatedBase: commit, Attempt: 1,
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
				ID: "run-probe", Ticket: "KRI-1", GatedBase: "C2", Attempt: 1,
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
				ID: "run-1", Ticket: "KRI-1", GatedBase: "C2", Attempt: 1,
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
			ID: "run-1", Ticket: "KRI-1", GatedBase: "C2", Attempt: 1,
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
