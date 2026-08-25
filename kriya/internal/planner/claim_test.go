package planner_test

import (
	"context"
	"errors"
	"testing"

	"kriya/internal/planner"
)

// memClaims persists completion claims.
type memClaims struct {
	rows map[string]planner.CompletionClaim
	err  error
	// failAfter makes the Nth write onward fail, for the crash windows
	// between one persisted step and the next.
	failAfter int
	writes    int
}

func newMemClaims() *memClaims {
	return &memClaims{rows: map[string]planner.CompletionClaim{}}
}

func (m *memClaims) Upsert(_ context.Context, c planner.CompletionClaim) error {
	if m.err != nil {
		return m.err
	}
	m.writes++
	if m.failAfter > 0 && m.writes > m.failAfter {
		return errors.New("disk full")
	}
	m.rows[c.TargetKey] = c
	return nil
}

func (m *memClaims) Find(_ context.Context, key string) (planner.CompletionClaim, bool, error) {
	c, ok := m.rows[key]
	return c, ok, m.err
}

func (m *memClaims) Submitting(ctx context.Context) ([]planner.CompletionClaim, error) {
	return m.inState(ctx, planner.CompletionSubmitting)
}

func (m *memClaims) Closing(ctx context.Context) ([]planner.CompletionClaim, error) {
	return m.inState(ctx, planner.CompletionClosing)
}

func (m *memClaims) inState(_ context.Context, state string) ([]planner.CompletionClaim, error) {
	if m.err != nil {
		return nil, m.err
	}
	var out []planner.CompletionClaim
	for _, c := range m.rows {
		if c.State == state {
			out = append(out, c)
		}
	}
	return out, nil
}

// docCatalog stands in for sutra's documents, honouring idempotency keys.
type docCatalog struct {
	byKey    map[string]string
	versions int
	calls    []string
	err      error
}

func newDocCatalog() *docCatalog { return &docCatalog{byKey: map[string]string{}} }

func (d *docCatalog) Create(
	_ context.Context, _, _, _, _, key string,
) (string, string, error) {
	d.calls = append(d.calls, key)
	if d.err != nil {
		return "", "", d.err
	}
	if version, ok := d.byKey[key]; ok {
		// A replayed key returns the ORIGINAL document and version. No second
		// version appends.
		return "doc-1", version, nil
	}
	d.versions++
	version := "ver-" + itoa(d.versions)
	d.byKey[key] = version
	return "doc-1", version, nil
}

// reviewDesk stands in for sutra's review API for document reviews.
type reviewDesk struct {
	byKey    map[string]string
	created  int
	versions []string
	// keys records every key presented, which is what a replay has to get
	// right: a re-derived key opens a SECOND review rather than replaying.
	keys []string
	err  error
}

func newReviewDesk() *reviewDesk { return &reviewDesk{byKey: map[string]string{}} }

func (r *reviewDesk) Create(
	_ context.Context, _, _, docVersion, key string,
) (string, int, error) {
	r.keys = append(r.keys, key)
	if r.err != nil {
		return "", 0, r.err
	}
	r.versions = append(r.versions, docVersion)
	if id, ok := r.byKey[key]; ok {
		return id, 1, nil
	}
	r.created++
	id := "review-" + itoa(r.created)
	r.byKey[key] = id
	return id, 1, nil
}

// staticReport renders the same report every time it is asked.
type staticReport struct {
	body  string
	asked int
	err   error
}

func (s *staticReport) Render(context.Context, string) (string, error) {
	s.asked++
	return s.body, s.err
}

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return "many"
}

func claimer(
	claims *memClaims, docs planner.Documents, reviews *reviewDesk, report *staticReport,
	advances *memAdvances,
) planner.Claimer {
	return planner.Claimer{
		Claims: claims, Reports: report, Documents: docs, Reviews: reviews,
		Epochs: planner.Epochs{Store: advances},
	}
}

func TestASubmissionRecordsItsWholeClaimBeforeAnyCall(t *testing.T) {
	// "kriya records review-submitting with the deterministic submission key,
	// the doc key, and the pending report reference BEFORE any sutra call".
	// The document call failing is what proves the row exists anyway.
	claims, docs := newMemClaims(), newDocCatalog()
	docs.err = errors.New("sutra unreachable")
	c := claimer(claims, docs, newReviewDesk(), &staticReport{body: "# Done"}, newMemAdvances())

	if _, err := c.Submit(context.Background(), "/spec", "p-1", "epic-1", 0, 7); err == nil {
		t.Fatal("expected the document creation to fail")
	}
	got, found := claims.rows["/spec"]
	if !found {
		t.Fatal("no claim row survived; there is nothing to recover from")
	}
	if got.State != planner.CompletionSubmitting {
		t.Errorf("the claim is in state %q", got.State)
	}
	for name, value := range map[string]string{
		"submission key": got.SubmissionKey,
		"report key":     got.ReportKey,
		"pending report": got.PendingReport,
	} {
		if value == "" {
			t.Errorf("the %s was not persisted before the call", name)
		}
	}
	if got.SubtreeRevision != 7 {
		t.Errorf("the captured subtree revision is %d", got.SubtreeRevision)
	}
}

func TestACrashBeforeTheDocumentRecoversExactlyOnce(t *testing.T) {
	// The keyed doc mutation replays and creates the version, and the keyed
	// review creation follows. Exactly one of each exists.
	claims, docs, reviews := newMemClaims(), newDocCatalog(), newReviewDesk()
	report := &staticReport{body: "# Done"}
	broken := newDocCatalog()
	broken.err = errors.New("crash")
	if _, err := claimer(claims, broken, reviews, report, newMemAdvances()).
		Submit(context.Background(), "/spec", "p-1", "epic-1", 0, 7); err == nil {
		t.Fatal("expected the crash")
	}

	c := claimer(claims, docs, reviews, report, newMemAdvances())
	n, err := c.Recover(context.Background(), func(planner.CompletionClaim) (string, string, error) {
		return "p-1", "epic-1", nil
	})
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if n != 1 {
		t.Errorf("recovered %d claims", n)
	}
	if docs.versions != 1 {
		t.Errorf("%d document versions exist", docs.versions)
	}
	// The REPLAY presented the persisted key. A re-derived one would append a
	// second version beside the first rather than returning it.
	all := append(append([]string{}, broken.calls...), docs.calls...)
	if len(all) < 2 {
		t.Fatalf("the document was attempted %d times", len(all))
	}
	if all[0] != all[len(all)-1] {
		t.Errorf("the replay presented doc key %q, not the original %q",
			all[len(all)-1], all[0])
	}
	if all[0] != claims.rows["/spec"].ReportKey {
		t.Errorf("the call used %q but the row holds %q", all[0], claims.rows["/spec"].ReportKey)
	}
	if reviews.created != 1 {
		t.Errorf("%d reviews exist", reviews.created)
	}
	if claims.rows["/spec"].State != planner.CompletionSubmitted {
		t.Errorf("the claim settled as %q", claims.rows["/spec"].State)
	}
}

func TestACrashBetweenDocumentAndReviewAppendsNoSecondVersion(t *testing.T) {
	// sutra returns the SAME version for the replayed key. A recovery that
	// re-rendered or re-keyed would append another and review the wrong one.
	claims, docs, reviews := newMemClaims(), newDocCatalog(), newReviewDesk()
	report := &staticReport{body: "# Done"}
	reviews.err = errors.New("crash after the document existed")
	if _, err := claimer(claims, docs, reviews, report, newMemAdvances()).
		Submit(context.Background(), "/spec", "p-1", "epic-1", 0, 7); err == nil {
		t.Fatal("expected the crash")
	}
	if docs.versions != 1 {
		t.Fatalf("the document was not created: %d versions", docs.versions)
	}
	recordedVersion := claims.rows["/spec"].ReportVersion
	if recordedVersion == "" {
		t.Fatal("the version was not recorded before the review call")
	}

	reviews.err = nil
	if _, err := claimer(claims, docs, reviews, report, newMemAdvances()).
		Recover(context.Background(), func(planner.CompletionClaim) (string, string, error) {
			return "p-1", "epic-1", nil
		}); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if docs.versions != 1 {
		t.Errorf("the replay appended a second version: %d", docs.versions)
	}
	if reviews.created != 1 {
		t.Errorf("%d reviews exist", reviews.created)
	}
	if len(reviews.versions) == 0 || reviews.versions[len(reviews.versions)-1] != recordedVersion {
		t.Errorf("the review names %v, not the recorded version %q",
			reviews.versions, recordedVersion)
	}
}

func TestTheReportIsReplayedNeverRerendered(t *testing.T) {
	// A replay that regenerated the report could write different bytes under
	// the same key — and sutra would return the FIRST bytes, so the review
	// and kriya's record would disagree about what a human approved.
	claims, docs, reviews := newMemClaims(), newDocCatalog(), newReviewDesk()
	report := &staticReport{body: "# Done"}
	docs.err = errors.New("crash")
	if _, err := claimer(claims, docs, reviews, report, newMemAdvances()).
		Submit(context.Background(), "/spec", "p-1", "epic-1", 0, 7); err == nil {
		t.Fatal("expected the crash")
	}
	rendered := report.asked

	docs.err = nil
	if _, err := claimer(claims, docs, reviews, report, newMemAdvances()).
		Recover(context.Background(), func(planner.CompletionClaim) (string, string, error) {
			return "p-1", "epic-1", nil
		}); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if report.asked != rendered {
		t.Errorf("recovery re-rendered the report: asked %d then %d", rendered, report.asked)
	}
}

func TestTheSubmissionKeyIsScopedToTheEpoch(t *testing.T) {
	// "recovery can never adopt the prior epoch's approved review — its key
	// names the old epoch."
	first := planner.SubmissionKey("/spec", 3)
	if first == planner.SubmissionKey("/spec", 4) {
		t.Error("two epochs share a submission key; a spent review could replay")
	}
	if first == planner.SubmissionKey("/other", 3) {
		t.Error("two targets share a submission key")
	}
	// The report key is derived from it, so it rotates with the epoch too.
	if planner.ReportKey(first) == planner.ReportKey(planner.SubmissionKey("/spec", 4)) {
		t.Error("two epochs share a report key; a stale document could replay")
	}
}

func TestAnEmptyReportIsRefused(t *testing.T) {
	// A review with nothing to read is a human asked to approve a blank page.
	c := claimer(newMemClaims(), newDocCatalog(), newReviewDesk(),
		&staticReport{body: ""}, newMemAdvances())
	if _, err := c.Submit(context.Background(), "/spec", "p-1", "epic-1", 0, 7); err == nil {
		t.Fatal("an empty completion report was submitted")
	}
}

func TestADocumentWithNoVersionIsRefused(t *testing.T) {
	// The review's deliverable IS the version. Submitting without one would
	// open a review over nothing.
	c := claimer(newMemClaims(), emptyVersionDocs{}, newReviewDesk(),
		&staticReport{body: "# Done"}, newMemAdvances())
	if _, err := c.Submit(context.Background(), "/spec", "p-1", "epic-1", 0, 7); err == nil {
		t.Fatal("a review was opened over a document with no version")
	}
}

// emptyVersionDocs returns a document that points at nothing.
type emptyVersionDocs struct{}

func (emptyVersionDocs) Create(
	context.Context, string, string, string, string, string,
) (string, string, error) {
	return "doc-1", "", nil
}

func TestAnUnwritableClaimIsAFailure(t *testing.T) {
	claims := newMemClaims()
	claims.err = errors.New("disk full")
	c := claimer(claims, newDocCatalog(), newReviewDesk(),
		&staticReport{body: "# Done"}, newMemAdvances())
	if _, err := c.Submit(context.Background(), "/spec", "p-1", "epic-1", 0, 7); err == nil {
		t.Fatal("a claim that was never written read as recorded")
	}
}

func TestRecoveryReplaysThePersistedKeyNotAFreshlyDerivedOne(t *testing.T) {
	// The submission key is epoch-scoped. If recovery re-derived it after the
	// epoch advanced, the replay would present a DIFFERENT key and open a
	// second review — for a claim whose CAS is already doomed. It must
	// present the key the row holds.
	claims, docs, reviews := newMemClaims(), newDocCatalog(), newReviewDesk()
	report := &staticReport{body: "# Done"}
	advances := newMemAdvances()
	reviews.err = errors.New("crash before the review landed")
	if _, err := claimer(claims, docs, reviews, report, advances).
		Submit(context.Background(), "/spec", "p-1", "epic-1", 0, 7); err == nil {
		t.Fatal("expected the crash")
	}
	persisted := claims.rows["/spec"].SubmissionKey

	// The world moves: a ticket reopens, and the epoch advances past the
	// claim's.
	if _, err := (planner.Epochs{Store: advances}).
		OnEvent(context.Background(), "/spec", planner.CauseTicketReopen, "E1"); err != nil {
		t.Fatalf("advance: %v", err)
	}

	reviews.err = nil
	if _, err := claimer(claims, docs, reviews, report, advances).
		Recover(context.Background(), func(planner.CompletionClaim) (string, string, error) {
			return "p-1", "epic-1", nil
		}); err != nil {
		t.Fatalf("recover: %v", err)
	}
	got := claims.rows["/spec"]
	if got.SubmissionKey != persisted {
		t.Errorf("recovery re-derived the key: %q became %q", persisted, got.SubmissionKey)
	}
	// What the REPLAY presented. The row keeping its key means nothing if the
	// call went out under a different one.
	if len(reviews.keys) == 0 {
		t.Fatal("the review was never attempted")
	}
	if last := reviews.keys[len(reviews.keys)-1]; last != persisted {
		t.Errorf("the replay presented key %q, not the persisted %q", last, persisted)
	}
	// The claim keeps the epoch it was made at, so the stamp's CAS can still
	// see that it is stale.
	if got.Epoch != 0 {
		t.Errorf("the claim adopted epoch %d instead of the one it was made at", got.Epoch)
	}
	if reviews.created != 1 {
		t.Errorf("%d reviews exist after the replay", reviews.created)
	}
}

func TestARecoveredClaimKeepsItsCapturedSubtreeRevision(t *testing.T) {
	// The close is fenced on the revision the CLAIM captured. A replay that
	// re-captured it would fence against a subtree that has since moved, and
	// close an epic over work nobody reviewed.
	claims, docs, reviews := newMemClaims(), newDocCatalog(), newReviewDesk()
	report := &staticReport{body: "# Done"}
	reviews.err = errors.New("crash")
	if _, err := claimer(claims, docs, reviews, report, newMemAdvances()).
		Submit(context.Background(), "/spec", "p-1", "epic-1", 0, 7); err == nil {
		t.Fatal("expected the crash")
	}
	reviews.err = nil
	if _, err := claimer(claims, docs, reviews, report, newMemAdvances()).
		Recover(context.Background(), func(planner.CompletionClaim) (string, string, error) {
			return "p-1", "epic-1", nil
		}); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if got := claims.rows["/spec"].SubtreeRevision; got != 7 {
		t.Errorf("the recovered claim fences on revision %d, not the captured 7", got)
	}
}
