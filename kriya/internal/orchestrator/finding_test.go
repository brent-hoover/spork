package orchestrator_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"kriya/internal/orchestrator"
)

// findingDocs stands in for sutra's documents, honouring idempotency keys.
type findingDocs struct {
	byKey    map[string]string
	versions int
	keys     []string
	err      error
}

func newFindingDocs() *findingDocs { return &findingDocs{byKey: map[string]string{}} }

func (d *findingDocs) Create(
	_ context.Context, _, _, _, content, key string,
) (string, string, error) {
	d.keys = append(d.keys, key)
	if d.err != nil {
		return "", "", d.err
	}
	if v, ok := d.byKey[key]; ok {
		// A replayed key returns the ORIGINAL version. The content it was
		// first called with is what sutra holds, whatever this call carried.
		return "doc-1", v, nil
	}
	d.versions++
	v := fmt.Sprintf("ver-%d", d.versions)
	d.byKey[key] = v
	return "doc-1", v, nil
}

// findingReviews stands in for sutra's review API over document deliverables.
type findingReviews struct {
	byKey     map[string]string
	created   int
	revision  int
	versions  []string
	resubmits []string
	err       error
	fenceErr  error
}

func newFindingReviews() *findingReviews {
	return &findingReviews{byKey: map[string]string{}, revision: 1}
}

func (r *findingReviews) Create(
	_ context.Context, _, _, docVersion, key string,
) (string, int, error) {
	if r.err != nil {
		return "", 0, r.err
	}
	r.versions = append(r.versions, docVersion)
	if id, ok := r.byKey[key]; ok {
		return id, r.revision, nil
	}
	r.created++
	id := fmt.Sprintf("review-%d", r.created)
	r.byKey[key] = id
	return id, r.revision, nil
}

func (r *findingReviews) Resubmit(
	_ context.Context, _, _, docVersion string, expected int, _, _ string,
) (int, error) {
	if r.fenceErr != nil {
		return 0, r.fenceErr
	}
	r.resubmits = append(r.resubmits, docVersion)
	return expected + 1, nil
}

// spikeFinding renders a fixed finding, counting how often it was asked.
type spikeFinding struct {
	body  string
	asked int
	err   error
}

func (s *spikeFinding) Research(context.Context, orchestrator.BuildRun) (string, error) {
	s.asked++
	return s.body, s.err
}

func researcher(
	store *memStore, docs *findingDocs, reviews *findingReviews, f *spikeFinding,
) orchestrator.Researcher {
	return orchestrator.Researcher{Store: store, Findings: f, Docs: docs, Reviews: reviews}
}

func spikeRun() orchestrator.BuildRun {
	return orchestrator.BuildRun{
		ID: "run-1", Ticket: "spike: headless?", Issue: "issue-7",
		Kind: orchestrator.KindSpike, State: orchestrator.StateResearchLoop,
	}
}

func TestAFindingIsWrittenAheadOfItsDocument(t *testing.T) {
	// The key and the prepared finding land BEFORE the document exists, so a
	// crash between them replays the exact bytes under the exact key rather
	// than writing different ones sutra would then refuse to hold.
	store, docs := newMemStore(), newFindingDocs()
	docs.err = errors.New("crash before the document existed")
	r := researcher(store, docs, newFindingReviews(), &spikeFinding{body: "# headless works"})

	if _, err := r.Submit(context.Background(), spikeRun(), "p-1"); err == nil {
		t.Fatal("expected the crash")
	}
	got := store.rows["run-1"]
	if got.FindingKey == "" || got.PendingFinding == "" {
		t.Errorf("the finding was not written ahead: %+v", got)
	}
	if got.ReviewState != orchestrator.SubmitSubmitting {
		t.Errorf("the run is in review state %q", got.ReviewState)
	}
}

func TestACrashBeforeTheFindingDocumentRecoversExactlyOnce(t *testing.T) {
	store := newMemStore()
	docs, reviews := newFindingDocs(), newFindingReviews()
	f := &spikeFinding{body: "# headless works"}
	broken := newFindingDocs()
	broken.err = errors.New("crash")
	if _, err := researcher(store, broken, reviews, f).
		Submit(context.Background(), spikeRun(), "p-1"); err == nil {
		t.Fatal("expected the crash")
	}

	n, err := researcher(store, docs, reviews, f).
		RecoverFindings(context.Background(), func(orchestrator.BuildRun) (string, error) {
			return "p-1", nil
		})
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if n != 1 {
		t.Errorf("recovered %d findings", n)
	}
	if docs.versions != 1 || reviews.created != 1 {
		t.Errorf("%d document versions and %d reviews exist", docs.versions, reviews.created)
	}
	if store.rows["run-1"].ReviewState != orchestrator.SubmitSubmitted {
		t.Errorf("the run settled as %q", store.rows["run-1"].ReviewState)
	}
	// The finding was REPLAYED, not re-rendered: different bytes under a key
	// sutra has settled would leave the two disagreeing about what a human
	// approved.
	if f.asked != 1 {
		t.Errorf("the finding was researched %d times", f.asked)
	}
}

func TestACrashBetweenDocumentAndReviewAppendsNoSecondVersion(t *testing.T) {
	store := newMemStore()
	docs, reviews := newFindingDocs(), newFindingReviews()
	f := &spikeFinding{body: "# headless works"}
	reviews.err = errors.New("crash after the document existed")
	if _, err := researcher(store, docs, reviews, f).
		Submit(context.Background(), spikeRun(), "p-1"); err == nil {
		t.Fatal("expected the crash")
	}
	if docs.versions != 1 {
		t.Fatalf("%d document versions exist", docs.versions)
	}
	recorded := store.rows["run-1"].FindingVersion
	if recorded == "" {
		t.Fatal("the version was not recorded before the review call")
	}

	reviews.err = nil
	if _, err := researcher(store, docs, reviews, f).
		RecoverFindings(context.Background(), func(orchestrator.BuildRun) (string, error) {
			return "p-1", nil
		}); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if docs.versions != 1 {
		t.Errorf("the replay appended a second version: %d", docs.versions)
	}
	if docs.keys[0] != docs.keys[len(docs.keys)-1] {
		t.Errorf("the replay presented a different key: %v", docs.keys)
	}
	if reviews.versions[len(reviews.versions)-1] != recorded {
		t.Errorf("the review names %v, not the recorded version %q", reviews.versions, recorded)
	}
}

func TestARevisedFindingRotatesItsDocumentKey(t *testing.T) {
	// "a new document version is created under a fresh revision-scoped doc
	// key". A key that did not rotate would replay the previous version, and
	// the human would approve the finding they had already rejected.
	store := newMemStore()
	docs, reviews := newFindingDocs(), newFindingReviews()
	f := &spikeFinding{body: "# headless works"}
	r := researcher(store, docs, reviews, f)

	run, err := r.Submit(context.Background(), spikeRun(), "p-1")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	firstKey, firstVersion := run.FindingKey, run.FindingVersion

	// Changes requested: the run returns to research and revises.
	run.ReviewVerdictEvent = "event-9"
	f.body = "# headless works, with caveats"
	revised, err := r.Submit(context.Background(), run, "p-1")
	if err != nil {
		t.Fatalf("revise: %v", err)
	}
	if revised.FindingKey == firstKey {
		t.Error("the revision reused the rejected finding's document key")
	}
	if revised.FindingVersion == firstVersion {
		t.Error("the revision reused the rejected version")
	}
	if docs.versions != 2 {
		t.Errorf("%d document versions exist after a revision", docs.versions)
	}
	// It rides the resubmission machinery: one review, its revision advanced.
	if reviews.created != 1 {
		t.Errorf("the revision opened %d reviews", reviews.created)
	}
	if revised.ReviewRevision != run.ReviewRevision+1 {
		t.Errorf("the revision advanced to %d", revised.ReviewRevision)
	}
	if revised.ReviewVerdictEvent != "" {
		t.Error("the answered verdict is still pending")
	}
}

func TestARevisionWithNoVerdictEventIsRefused(t *testing.T) {
	// Without it sutra cannot fence the call, and a revision could answer a
	// verdict the human has since replaced.
	store := newMemStore()
	r := researcher(store, newFindingDocs(), newFindingReviews(),
		&spikeFinding{body: "# finding"})
	run, err := r.Submit(context.Background(), spikeRun(), "p-1")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	run.ReviewVerdictEvent = ""
	if _, err := r.Submit(context.Background(), run, "p-1"); err == nil {
		t.Fatal("a revision with no verdict event was accepted")
	}
}

func TestAFenceThatMovedStopsTheRevision(t *testing.T) {
	// sutra advances by exactly one. Anything else means the review moved
	// under the fence, which is what the fence exists to catch.
	store := newMemStore()
	reviews := newFindingReviews()
	r := researcher(store, newFindingDocs(), reviews, &spikeFinding{body: "# finding"})
	run, err := r.Submit(context.Background(), spikeRun(), "p-1")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	run.ReviewVerdictEvent = "event-9"
	reviews.fenceErr = errors.New("conflict: the review has moved on")
	if _, err := r.Submit(context.Background(), run, "p-1"); err == nil {
		t.Fatal("a refused resubmission read as an advance")
	}
}

func TestAnEmptyFindingIsRefused(t *testing.T) {
	// A risk retired on no evidence at all is the one thing risk-first exists
	// to prevent.
	store := newMemStore()
	r := researcher(store, newFindingDocs(), newFindingReviews(), &spikeFinding{body: ""})
	if _, err := r.Submit(context.Background(), spikeRun(), "p-1"); err == nil {
		t.Fatal("an empty finding was submitted for review")
	}
}

func TestASpikeWithNoIssueCannotSubmit(t *testing.T) {
	// The review hangs off the spike's own issue; without it there is nothing
	// for a human to approve against.
	store := newMemStore()
	run := spikeRun()
	run.Issue = ""
	r := researcher(store, newFindingDocs(), newFindingReviews(), &spikeFinding{body: "x"})
	if _, err := r.Submit(context.Background(), run, "p-1"); err == nil {
		t.Fatal("a spike with no issue submitted a finding")
	}
}

func TestRecoveryLeavesACodeSubmissionAlone(t *testing.T) {
	// Its own recovery replays it, under its own keys and against a branch
	// rather than a document. Replaying it here would submit a diff as a
	// finding.
	store := &memStore{rows: map[string]orchestrator.BuildRun{
		"run-code": {
			ID: "run-code", Ticket: "T-1", Issue: "issue-8",
			ReviewState: orchestrator.SubmitSubmitting,
		},
	}}
	docs := newFindingDocs()
	n, err := researcher(store, docs, newFindingReviews(), &spikeFinding{body: "x"}).
		RecoverFindings(context.Background(), func(orchestrator.BuildRun) (string, error) {
			return "p-1", nil
		})
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if n != 0 {
		t.Errorf("replayed %d code submissions as findings", n)
	}
	if docs.versions != 0 {
		t.Errorf("a document was created for a code submission")
	}
}

func TestTheFindingKeyIsScopedToTheRevision(t *testing.T) {
	if orchestrator.FindingKey("run-1", 1) == orchestrator.FindingKey("run-1", 2) {
		t.Error("two revisions share a finding document key")
	}
	if orchestrator.FindingKey("run-1", 1) == orchestrator.FindingKey("run-2", 1) {
		t.Error("two runs share a finding document key")
	}
}
