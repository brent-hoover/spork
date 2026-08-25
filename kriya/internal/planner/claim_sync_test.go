package planner_test

import (
	"context"
	"errors"
	"testing"
)

// syncSpy records the watermark a claim accounted for.
type syncSpy struct {
	at  []string
	err error
}

func (s *syncSpy) SyncTo(_ context.Context, watermark string) error {
	s.at = append(s.at, watermark)
	return s.err
}

func TestASubmissionAccountsForTheEventsItsDetectionSaw(t *testing.T) {
	// Part of the PROTOCOL, not something the caller does afterwards. Every
	// ticket completing during the build emitted a status change, and a
	// caller who forgot this step would have each of them read as a reopen
	// the moment the claim existed — so no build could ever complete.
	spy := &syncSpy{}
	c := claimer(newMemClaims(), newDocCatalog(), newReviewDesk(),
		&staticReport{body: "# Done"}, newMemAdvances())
	c.Work = spy
	if _, err := c.Submit(context.Background(), "/spec", "p-1", "epic-1", 0, 7, "42"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if len(spy.at) != 1 || spy.at[0] != "42" {
		t.Errorf("the claim accounted for %v, not its detection's watermark", spy.at)
	}
}

func TestAFailedSyncStopsTheSubmission(t *testing.T) {
	// An unaccounted feed position means the very next pass destroys this
	// claim. Opening the review anyway would spend a human's attention on
	// something already doomed.
	spy := &syncSpy{err: errors.New("cursor unwritable")}
	claims := newMemClaims()
	docs, reviews := newDocCatalog(), newReviewDesk()
	c := claimer(claims, docs, reviews, &staticReport{body: "# Done"}, newMemAdvances())
	c.Work = spy
	if _, err := c.Submit(context.Background(), "/spec", "p-1", "epic-1", 0, 7, "42"); err == nil {
		t.Fatal("a submission proceeded with an unaccounted feed position")
	}
	if reviews.created != 0 {
		t.Errorf("%d reviews were opened anyway", reviews.created)
	}
	// The write-ahead row stands, so recovery can finish it once the cursor
	// is writable again.
	if claims.rows["/spec"].SubmissionKey == "" {
		t.Error("no claim row survived; there is nothing to recover from")
	}
}

func TestASubmissionWithNoWorkSeamStillWorks(t *testing.T) {
	// Nil skips it, which is what a module-level test of the protocol wants.
	c := claimer(newMemClaims(), newDocCatalog(), newReviewDesk(),
		&staticReport{body: "# Done"}, newMemAdvances())
	if _, err := c.Submit(context.Background(), "/spec", "p-1", "epic-1", 0, 7, "42"); err != nil {
		t.Fatalf("submit: %v", err)
	}
}

func TestTheSyncHappensBeforeTheExternalCalls(t *testing.T) {
	// Before anything can read the feed again. A sync after the review opened
	// leaves a window in which the watcher consumes the build's own events
	// and kills a claim a human may already be looking at.
	spy := &syncSpy{}
	docs := newDocCatalog()
	c := claimer(newMemClaims(), docs, newReviewDesk(),
		&staticReport{body: "# Done"}, newMemAdvances())
	c.Work = &orderedSync{spy: spy, docs: docs}
	if _, err := c.Submit(context.Background(), "/spec", "p-1", "epic-1", 0, 7, "42"); err != nil {
		t.Fatalf("submit: %v", err)
	}
}

// orderedSync fails if the document already exists when the sync runs.
type orderedSync struct {
	spy  *syncSpy
	docs *docCatalog
}

func (o *orderedSync) SyncTo(ctx context.Context, watermark string) error {
	if o.docs.versions != 0 {
		return errors.New("the sync ran after the report document existed")
	}
	return o.spy.SyncTo(ctx, watermark)
}
