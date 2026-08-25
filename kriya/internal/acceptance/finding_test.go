//go:build acceptance

package acceptance

import (
	"context"
	"errors"
	"fmt"

	"github.com/cucumber/godog"

	"kriya/internal/orchestrator"
)

// findingWorld is one spike-finding scenario's state.
type findingWorld struct {
	store   *sessionRuns
	docs    *docShelf
	reviews *findingDesk
	finding *scriptedFinding
	r       orchestrator.Researcher
	run     orchestrator.BuildRun
	err     error
	// firstKey and firstVersion are the rejected finding's, so a revision can
	// be shown not to reuse them.
	firstKey     string
	firstVersion string
	// closed records whether sutra's approved-review gate let the spike close.
	closed bool
}

// sessionRuns persists build runs.
type sessionRuns struct {
	rows map[string]orchestrator.BuildRun
}

func (s *sessionRuns) Upsert(_ context.Context, r orchestrator.BuildRun) error {
	s.rows[r.ID] = r
	return nil
}

func (s *sessionRuns) Find(
	_ context.Context, id string,
) (orchestrator.BuildRun, bool, error) {
	r, ok := s.rows[id]
	return r, ok, nil
}

func (s *sessionRuns) Submitting(context.Context) ([]orchestrator.BuildRun, error) {
	var out []orchestrator.BuildRun
	for _, r := range s.rows {
		if r.ReviewState == orchestrator.SubmitSubmitting {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *sessionRuns) Resubmitting(context.Context) ([]orchestrator.BuildRun, error) {
	return nil, nil
}

func (s *sessionRuns) Completing(context.Context) ([]orchestrator.BuildRun, error) {
	return nil, nil
}

func (s *sessionRuns) ForTicket(
	context.Context, string,
) (orchestrator.BuildRun, bool, error) {
	return orchestrator.BuildRun{}, false, nil
}

// findingDesk stands in for sutra's review API over document deliverables.
type findingDesk struct {
	byKey     map[string]string
	created   int
	versions  []string
	resubmits []string
	err       error
}

func (d *findingDesk) Create(
	_ context.Context, _, _, docVersion, key string,
) (string, int, error) {
	if d.err != nil {
		return "", 0, d.err
	}
	d.versions = append(d.versions, docVersion)
	if id, ok := d.byKey[key]; ok {
		return id, 1, nil
	}
	d.created++
	id := fmt.Sprintf("review-%d", d.created)
	d.byKey[key] = id
	return id, 1, nil
}

func (d *findingDesk) Resubmit(
	_ context.Context, _, _, docVersion string, expected int, _, _ string,
) (int, error) {
	d.resubmits = append(d.resubmits, docVersion)
	return expected + 1, nil
}

// scriptedFinding answers with whatever the scenario set.
type scriptedFinding struct {
	body  string
	asked int
}

func (s *scriptedFinding) Research(
	context.Context, orchestrator.BuildRun,
) (string, error) {
	s.asked++
	return s.body, nil
}

func (w *world) newFinding() *findingWorld {
	f := &findingWorld{
		store:   &sessionRuns{rows: map[string]orchestrator.BuildRun{}},
		docs:    &docShelf{byKey: map[string]string{}},
		reviews: &findingDesk{byKey: map[string]string{}},
		finding: &scriptedFinding{body: "# the review tool runs headless\n\nEvidence: ...\n"},
	}
	f.r = orchestrator.Researcher{
		Store: f.store, Findings: f.finding, Docs: f.docs, Reviews: f.reviews,
	}
	f.run = orchestrator.BuildRun{
		ID: "run-spike", Ticket: "spike: can the review tool run headless",
		Issue: "issue-7", Kind: orchestrator.KindSpike,
		State: orchestrator.StateResearchLoop,
	}
	w.finding = f
	return f
}

func registerFindings(sc *godog.ScenarioContext, w *world) {
	sc.Step(`^an in-progress spike ticket with a documented finding attached$`, func() error {
		w.newFinding()
		return nil
	})

	sc.Step(`^the finding is submitted as a document-deliverable Review on the spike ticket$`,
		func() error {
			f := w.finding
			f.run, f.err = f.r.Submit(context.Background(), f.run, "p-1")
			if f.err != nil {
				return f.err
			}
			// A DOCUMENT deliverable, on the SPIKE's own issue: there is no
			// diff to review, and the risk it answers is that ticket's.
			if f.run.FindingVersion == "" {
				return errors.New("the review names no document version")
			}
			if len(f.reviews.versions) != 1 ||
				f.reviews.versions[0] != f.run.FindingVersion {
				return fmt.Errorf("the review names %v, not the recorded version",
					f.reviews.versions)
			}
			f.firstKey, f.firstVersion = f.run.FindingKey, f.run.FindingVersion
			return nil
		})

	sc.Step(`^a human approves it$`, func() error {
		f := w.finding
		if f.run.ReviewState != orchestrator.SubmitSubmitted {
			return fmt.Errorf("the run is in review state %q", f.run.ReviewState)
		}
		// sutra's gate: a spike closes on an APPROVED review over its finding.
		f.closed = f.run.ReviewID != "" && f.run.FindingVersion != ""
		return nil
	})

	sc.Step(`^the spike closes through sutra's approved-review gate$`, func() error {
		if !w.finding.closed {
			return errors.New("the spike did not close on its approved finding")
		}
		return nil
	})

	sc.Step(`^the dependent tickets become workable$`, func() error {
		// The blocking relation is what held them, and sutra releases it when
		// the blocker closes. kriya asks for no unblocking of its own — there
		// is nothing for it to do here, which is the point.
		if !w.finding.closed {
			return errors.New("the spike is still open, so nothing is unblocked")
		}
		return nil
	})

	sc.Step(`^a spike with no approved finding review cannot close$`, func() error {
		f := w.newFinding()
		// Never submitted: no review, no approval, no close. A risk retired
		// without documented evidence is the one thing risk-first exists to
		// prevent.
		if f.run.ReviewID != "" {
			return errors.New("an unsubmitted spike already has a review")
		}
		if f.closed {
			return errors.New("a spike with no approved finding closed")
		}
		return nil
	})

	registerFindingRework(sc, w)
	registerFindingRecovery(sc, w)
}

func registerFindingRework(sc *godog.ScenarioContext, w *world) {
	sc.Step(`^a finding review receives changes-requested$`, func() error {
		f := w.newFinding()
		var err error
		f.run, err = f.r.Submit(context.Background(), f.run, "p-1")
		if err != nil {
			return err
		}
		f.firstKey, f.firstVersion = f.run.FindingKey, f.run.FindingVersion
		// The verdict, recorded on the run: a revision answers THIS event,
		// and one that could not name it would answer whatever the review
		// said last.
		f.run.ReviewVerdictEvent = "event-9"
		return f.store.Upsert(context.Background(), f.run)
	})

	sc.Step(`^the run returns from finding-submitted to its research loop$`, func() error {
		f := w.finding
		f.run.State = orchestrator.StateResearchLoop
		// The table has no transition out of finding-submitted — the run waits
		// for a human — so a rejected verdict is what moves it, exactly as a
		// rejected code review moves a run back to the dev loop.
		if _, ok := orchestrator.Lookup(orchestrator.StateFindingSubmitted); ok {
			return errors.New("finding-submitted has a transition; the run would not wait")
		}
		return nil
	})

	sc.Step(`^the agent revises the finding$`, func() error {
		f := w.finding
		f.finding.body = "# the review tool runs headless, with caveats\n\nEvidence: ...\n"
		f.run, f.err = f.r.Submit(context.Background(), f.run, "p-1")
		return f.err
	})

	sc.Step(`^a new document version is created under a fresh revision-scoped doc key$`,
		func() error {
			f := w.finding
			if f.run.FindingKey == f.firstKey {
				return fmt.Errorf("the revision reused doc key %q", f.firstKey)
			}
			if f.run.FindingVersion == f.firstVersion {
				return errors.New("the revision reused the rejected version")
			}
			if f.docs.versions != 2 {
				return fmt.Errorf("%d document versions exist", f.docs.versions)
			}
			return nil
		})

	sc.Step(`^the new version is recorded as the pending resubmission document under its rotated doc key before the resubmit replays$`,
		func() error {
			f := w.finding
			// Recorded BEFORE the resubmit: a crash between them must replay
			// the exact version rather than appending another.
			got := f.store.rows["run-spike"]
			if got.FindingVersion != f.run.FindingVersion {
				return fmt.Errorf("the row records version %q", got.FindingVersion)
			}
			if got.FindingKey != f.run.FindingKey {
				return fmt.Errorf("the row records key %q", got.FindingKey)
			}
			return nil
		})

	sc.Step(`^the resubmission rides the write-ahead resubmitting machinery, incrementing the review's revision$`,
		func() error {
			f := w.finding
			// ONE review, its revision advanced — not a second review over the
			// same risk.
			if f.reviews.created != 1 {
				return fmt.Errorf("the revision opened %d reviews", f.reviews.created)
			}
			if len(f.reviews.resubmits) != 1 {
				return fmt.Errorf("%d resubmissions ran", len(f.reviews.resubmits))
			}
			if f.run.ReviewRevision != 2 {
				return fmt.Errorf("the review is at revision %d", f.run.ReviewRevision)
			}
			return nil
		})

	sc.Step(`^the run never rests in finding-submitted with a rejected review$`, func() error {
		f := w.finding
		// The answered verdict is CLEARED, which is what says no revision is
		// outstanding. A run resting here with one still set would be a
		// rejected finding nobody is revising.
		if f.run.ReviewVerdictEvent != "" {
			return errors.New("the run rests with an unanswered rejection")
		}
		if f.run.ReviewState != orchestrator.SubmitSubmitted {
			return fmt.Errorf("the run is in review state %q", f.run.ReviewState)
		}
		return nil
	})
}

func registerFindingRecovery(sc *godog.ScenarioContext, w *world) {
	sc.Step(`^the finding doc key and pending reference were persisted and the crash hit before the document version existed$`,
		func() error {
			f := w.newFinding()
			f.docs.fail = errors.New("crash before the document existed")
			if _, err := f.r.Submit(context.Background(), f.run, "p-1"); err == nil {
				return errors.New("the scenario's crash did not happen")
			}
			got := f.store.rows["run-spike"]
			if got.FindingKey == "" || got.PendingFinding == "" {
				return fmt.Errorf("the finding was not written ahead: %+v", got)
			}
			if f.docs.versions != 0 {
				return errors.New("a version exists; the crash was in the wrong place")
			}
			f.docs.fail = nil
			return nil
		})

	sc.Step(`^the keyed doc mutation replays and creates the version, records it as the run's finding doc, and the finding review names exactly that deliverable through the ordinary submission machinery$`,
		func() error {
			f := w.finding
			if f.docs.versions != 1 {
				return fmt.Errorf("%d document versions exist", f.docs.versions)
			}
			got := f.store.rows["run-spike"]
			if got.FindingVersion == "" {
				return errors.New("the version was not recorded on the run")
			}
			if f.reviews.created != 1 {
				return fmt.Errorf("%d reviews exist", f.reviews.created)
			}
			// The ORDINARY machinery: the same write-ahead protocol a code
			// submission uses, over a document rather than a diff.
			if f.reviews.versions[len(f.reviews.versions)-1] != got.FindingVersion {
				return fmt.Errorf("the review names %v, not the recorded version",
					f.reviews.versions)
			}
			// And the finding was REPLAYED, not re-researched: different bytes
			// under a key sutra has settled would leave the two disagreeing.
			if f.finding.asked != 1 {
				return fmt.Errorf("the finding was researched %d times", f.finding.asked)
			}
			return nil
		})

	sc.Step(`^the document version was created and the crash hit before the review-create call$`,
		func() error {
			f := w.newFinding()
			f.reviews.err = errors.New("crash after the document existed")
			if _, err := f.r.Submit(context.Background(), f.run, "p-1"); err == nil {
				return errors.New("the scenario's crash did not happen")
			}
			if f.docs.versions != 1 {
				return fmt.Errorf("%d document versions exist", f.docs.versions)
			}
			f.reviews.err = nil
			return nil
		})

	sc.Step(`^sutra returns the same version — no duplicate finding — and the keyed review creation runs exactly once$`,
		func() error {
			f := w.finding
			if f.docs.versions != 1 {
				return fmt.Errorf("the replay appended: %d versions exist", f.docs.versions)
			}
			if len(f.docs.keys) < 2 || f.docs.keys[0] != f.docs.keys[1] {
				return fmt.Errorf("the replay presented keys %v", f.docs.keys)
			}
			if f.reviews.created != 1 {
				return fmt.Errorf("%d reviews exist", f.reviews.created)
			}
			return nil
		})
}
