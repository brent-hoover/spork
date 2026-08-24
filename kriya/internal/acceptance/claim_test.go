//go:build acceptance

package acceptance

import (
	"context"
	"errors"
	"fmt"

	"github.com/cucumber/godog"

	"kriya/internal/planner"
)

// claimWorld is one completion-submission scenario's state.
type claimWorld struct {
	claims  *memCompletionClaims
	docs    *docShelf
	reviews *reviewCounter
	report  *fixedReport
	claimer planner.Claimer
	err     error
}

// memCompletionClaims persists completion claims.
type memCompletionClaims struct {
	rows map[string]planner.CompletionClaim
}

func (m *memCompletionClaims) Upsert(_ context.Context, c planner.CompletionClaim) error {
	m.rows[c.TargetKey] = c
	return nil
}

func (m *memCompletionClaims) Find(
	_ context.Context, key string,
) (planner.CompletionClaim, bool, error) {
	c, ok := m.rows[key]
	return c, ok, nil
}

func (m *memCompletionClaims) Submitting(context.Context) ([]planner.CompletionClaim, error) {
	var out []planner.CompletionClaim
	for _, c := range m.rows {
		if c.State == planner.CompletionSubmitting {
			out = append(out, c)
		}
	}
	return out, nil
}

// docShelf stands in for sutra's documents, honouring idempotency keys the way
// sutra does: a replayed key returns the ORIGINAL version.
type docShelf struct {
	byKey    map[string]string
	versions int
	keys     []string
	fail     error
}

func (d *docShelf) Create(
	_ context.Context, _, _, _, _, key string,
) (string, string, error) {
	d.keys = append(d.keys, key)
	if d.fail != nil {
		return "", "", d.fail
	}
	if v, ok := d.byKey[key]; ok {
		return "doc-1", v, nil
	}
	d.versions++
	v := fmt.Sprintf("ver-%d", d.versions)
	d.byKey[key] = v
	return "doc-1", v, nil
}

// reviewCounter stands in for sutra's review creation.
type reviewCounter struct {
	byKey    map[string]string
	created  int
	keys     []string
	versions []string
	fail     error
}

func (r *reviewCounter) Create(
	_ context.Context, _, _, docVersion, key string,
) (string, int, error) {
	r.keys = append(r.keys, key)
	if r.fail != nil {
		return "", 0, r.fail
	}
	r.versions = append(r.versions, docVersion)
	if id, ok := r.byKey[key]; ok {
		return id, 1, nil
	}
	r.created++
	id := fmt.Sprintf("review-%d", r.created)
	r.byKey[key] = id
	return id, 1, nil
}

// fixedReport renders the same bytes every time, so a re-render is invisible
// in the content and visible only in the count.
type fixedReport struct{ rendered int }

func (f *fixedReport) Render(context.Context, string) (string, error) {
	f.rendered++
	return "# Build complete\n", nil
}

func (w *world) newClaim() *claimWorld {
	c := &claimWorld{
		claims:  &memCompletionClaims{rows: map[string]planner.CompletionClaim{}},
		docs:    &docShelf{byKey: map[string]string{}},
		reviews: &reviewCounter{byKey: map[string]string{}},
		report:  &fixedReport{},
	}
	c.claimer = planner.Claimer{
		Claims: c.claims, Reports: c.report, Documents: c.docs, Reviews: c.reviews,
		Epochs: planner.Epochs{Store: newMemAdvanceStore()},
	}
	w.claim = c
	return c
}

func (c *claimWorld) recover() error {
	_, err := c.claimer.Recover(context.Background(),
		func(planner.CompletionClaim) (string, string, error) { return "p-1", "epic-1", nil })
	return err
}

func registerClaim(sc *godog.ScenarioContext, w *world) {
	sc.Step(`^review-submitting rotated the claim set with its doc key and pending report, and the crash hit before the document version existed$`,
		func() error {
			c := w.newClaim()
			c.docs.fail = errors.New("crash before the document existed")
			_, c.err = c.claimer.Submit(context.Background(), "/spec", "p-1", "epic-1", 7)
			if c.err == nil {
				return errors.New("the scenario's crash did not happen")
			}
			// The claim set is on disk BEFORE the call that failed. That is
			// what makes the crash recoverable at all.
			row := c.claims.rows["/spec"]
			if row.SubmissionKey == "" || row.ReportKey == "" || row.PendingReport == "" {
				return fmt.Errorf("the claim set was not rotated before the call: %+v", row)
			}
			if c.docs.versions != 0 {
				return errors.New("a document version exists; the crash was in the wrong place")
			}
			c.docs.fail = nil
			return nil
		})

	sc.Step(`^the report document was created under its key and the crash hit before the review-create call$`,
		func() error {
			c := w.newClaim()
			c.reviews.fail = errors.New("crash after the document existed")
			_, c.err = c.claimer.Submit(context.Background(), "/spec", "p-1", "epic-1", 7)
			if c.err == nil {
				return errors.New("the scenario's crash did not happen")
			}
			if c.docs.versions != 1 {
				return fmt.Errorf("%d document versions exist", c.docs.versions)
			}
			// Recorded BEFORE the review call: the replay has to name the
			// exact version, not append another.
			if c.claims.rows["/spec"].ReportVersion == "" {
				return errors.New("the version was not recorded before the review call")
			}
			c.reviews.fail = nil
			return nil
		})

	sc.Step(`^recovery replays the keyed doc mutation$`, func() error { return w.claim.recover() })

	sc.Step(`^the keyed doc mutation replays and creates the version, records it, and the keyed review creation follows$`,
		func() error {
			c := w.claim
			if c.docs.versions != 1 {
				return fmt.Errorf("%d document versions exist", c.docs.versions)
			}
			row := c.claims.rows["/spec"]
			if row.ReportVersion == "" {
				return errors.New("the version was not recorded")
			}
			if row.State != planner.CompletionSubmitted {
				return fmt.Errorf("the claim settled as %q", row.State)
			}
			// The REPLAY presented the persisted key both times. A re-derived
			// one would append a second version beside the first.
			if len(c.docs.keys) < 2 || c.docs.keys[0] != c.docs.keys[len(c.docs.keys)-1] {
				return fmt.Errorf("the doc keys presented were %v", c.docs.keys)
			}
			return nil
		})

	sc.Step(`^sutra returns the same version — no second version appends$`, func() error {
		c := w.claim
		if c.docs.versions != 1 {
			return fmt.Errorf("the replay appended: %d versions exist", c.docs.versions)
		}
		if len(c.docs.keys) < 2 {
			return fmt.Errorf("the doc mutation ran %d times; it was not replayed", len(c.docs.keys))
		}
		if c.docs.keys[0] != c.docs.keys[1] {
			return fmt.Errorf("the replay presented a different key: %v", c.docs.keys)
		}
		// And the report was REPLAYED, not re-rendered: different bytes under
		// the same key would leave sutra holding the first and kriya
		// believing the second.
		if c.report.rendered != 1 {
			return fmt.Errorf("the report was rendered %d times", c.report.rendered)
		}
		return nil
	})

	sc.Step(`^the keyed review creation then runs, exactly once$`, func() error {
		c := w.claim
		if c.reviews.created != 1 {
			return fmt.Errorf("%d reviews exist", c.reviews.created)
		}
		row := c.claims.rows["/spec"]
		if last := c.reviews.keys[len(c.reviews.keys)-1]; last != row.SubmissionKey {
			return fmt.Errorf("the replay presented key %q, not the persisted %q",
				last, row.SubmissionKey)
		}
		if c.reviews.versions[len(c.reviews.versions)-1] != row.ReportVersion {
			return fmt.Errorf("the review names %v, not the recorded version %q",
				c.reviews.versions, row.ReportVersion)
		}
		return nil
	})

	sc.Step(`^exactly one document version and one review exist$`, func() error {
		c := w.claim
		if c.docs.versions != 1 {
			return fmt.Errorf("%d document versions exist", c.docs.versions)
		}
		if c.reviews.created != 1 {
			return fmt.Errorf("%d reviews exist", c.reviews.created)
		}
		return nil
	})
}
