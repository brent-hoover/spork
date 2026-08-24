package orchestrator_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"kriya/internal/orchestrator"
)

// memAttempts is the merge queue's store.
type memAttempts struct {
	rows  map[string]orchestrator.MergeAttempt
	order []string
	err   error
}

func newMemAttempts() *memAttempts {
	return &memAttempts{rows: map[string]orchestrator.MergeAttempt{}}
}

func (m *memAttempts) Insert(_ context.Context, a orchestrator.MergeAttempt) (bool, error) {
	if m.err != nil {
		return false, m.err
	}
	if _, exists := m.rows[a.Key]; exists {
		return false, nil
	}
	m.rows[a.Key] = a
	m.order = append(m.order, a.Key)
	return true, nil
}

func (m *memAttempts) Update(_ context.Context, a orchestrator.MergeAttempt) error {
	if m.err != nil {
		return m.err
	}
	m.rows[a.Key] = a
	return nil
}

func (m *memAttempts) Find(_ context.Context, key string) (orchestrator.MergeAttempt, bool, error) {
	if m.err != nil {
		return orchestrator.MergeAttempt{}, false, m.err
	}
	a, ok := m.rows[key]
	return a, ok, nil
}

func (m *memAttempts) Unfinished(context.Context) ([]orchestrator.MergeAttempt, error) {
	if m.err != nil {
		return nil, m.err
	}
	var out []orchestrator.MergeAttempt
	for _, key := range m.order {
		if a := m.rows[key]; !a.Terminal() {
			out = append(out, a)
		}
	}
	return out, nil
}

// approvals counts consumptions, keyed as sutra keys them.
type approvals struct {
	consumed map[string]bool
	calls    []consumeCall
	err      error
	// reversed makes a consumption fail as sutra's fence would.
	reversed bool
}

type consumeCall struct {
	review, event, key string
	revision           int
}

func newApprovals() *approvals { return &approvals{consumed: map[string]bool{}} }

func (a *approvals) Consume(
	_ context.Context, review, _ string, revision int, event, key string,
) error {
	a.calls = append(a.calls, consumeCall{
		review: review, event: event, key: key, revision: revision,
	})
	if a.err != nil {
		return a.err
	}
	if a.consumed[key] {
		// Replayed key: the original result, and no second claim.
		return nil
	}
	if a.reversed {
		return errors.New("verdict fence: the approval was reversed")
	}
	a.consumed[key] = true
	return nil
}

// gitDouble records what the queue asked of git.
type gitDouble struct {
	head       string
	conflicts  bool
	merges     []string
	merged     string
	preflights int
	headErr    error
	mergeErr   error
	// casFails makes the merge refuse as a moved ref would.
	casFails bool
	// landed is every commit already reachable from the default head.
	landed map[string]bool
}

func (g *gitDouble) Preflight(context.Context, string, string) (bool, error) {
	g.preflights++
	return !g.conflicts, nil
}

func (g *gitDouble) Head(context.Context) (string, error) {
	if g.headErr != nil {
		return "", g.headErr
	}
	return g.head, nil
}

func (g *gitDouble) Landed(_ context.Context, commit string) (bool, error) {
	return g.landed[commit], nil
}

func (g *gitDouble) Merge(_ context.Context, commit, expectedBase string) (string, error) {
	g.merges = append(g.merges, commit+"@"+expectedBase)
	if g.mergeErr != nil {
		return "", g.mergeErr
	}
	if g.casFails {
		return "", errors.New("merge did not land: the ref moved")
	}
	g.merged = "merge-" + commit
	return g.merged, nil
}

func queue(store *memAttempts, ap *approvals, git *gitDouble) orchestrator.Queue {
	return orchestrator.Queue{
		Store: store, Approvals: ap, Git: git,
		Resource: "/repo#main", Actor: "actor-1",
	}
}

func attempt() orchestrator.MergeAttempt {
	return orchestrator.MergeAttempt{
		TargetKey: "KRIYA0a", Build: "run-1", Review: "review-1", Revision: 2,
		ApprovalEvent: "event-9", Commit: "C2", ExpectedBase: "base-1",
		Resource: "/repo#main",
	}
}

func enqueued(t *testing.T, q orchestrator.Queue, a orchestrator.MergeAttempt) string {
	t.Helper()
	fresh, err := q.Enqueue(context.Background(), a)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if !fresh {
		t.Fatal("the first enqueue of an approval was not fresh")
	}
	return orchestrator.AttemptKey(a.TargetKey, a.Build, a.Review, a.Revision, a.ApprovalEvent)
}

func TestAnApprovalMergesItsPinnedCommitExactlyOnce(t *testing.T) {
	store, ap, git := newMemAttempts(), newApprovals(), &gitDouble{head: "base-1"}
	q := queue(store, ap, git)
	key := enqueued(t, q, attempt())

	got, err := q.Run(context.Background(), key)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got.State != orchestrator.AttemptMerged || got.MergeCommit == "" {
		t.Fatalf("attempt settled as %+v", got)
	}
	if len(git.merges) != 1 || git.merges[0] != "C2@base-1" {
		t.Errorf("merged %v — the pinned commit against the frozen base is what lands", git.merges)
	}
}

func TestThePreflightRunsBeforeAnythingIsConsumed(t *testing.T) {
	// A consumed approval cannot be given back, and a conflict discovered
	// afterwards would strand the review.
	store, ap, git := newMemAttempts(), newApprovals(), &gitDouble{head: "base-1", conflicts: true}
	q := queue(store, ap, git)
	key := enqueued(t, q, attempt())

	got, err := q.Run(context.Background(), key)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got.State != orchestrator.AttemptAborted {
		t.Errorf("a conflicting merge settled as %q", got.State)
	}
	if len(ap.calls) != 0 {
		t.Error("an approval was consumed for a merge that could not land")
	}
	if len(git.merges) != 0 {
		t.Error("a conflicting merge was attempted anyway")
	}
}

func TestAMovedDefaultHeadAbortsBeforeConsuming(t *testing.T) {
	// The base moved under the chain that gated this work, so the gates no
	// longer say anything about what would land.
	store, ap, git := newMemAttempts(), newApprovals(), &gitDouble{head: "someone-elses-merge"}
	q := queue(store, ap, git)
	key := enqueued(t, q, attempt())

	got, err := q.Run(context.Background(), key)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got.State != orchestrator.AttemptAborted {
		t.Errorf("settled as %q", got.State)
	}
	if !strings.Contains(got.Note, "base-1") {
		t.Errorf("the cause does not name the base it expected: %q", got.Note)
	}
	if len(ap.calls) != 0 {
		t.Error("an approval was consumed against a moved base")
	}
}

func TestTheApprovalIsConsumedAtTheExpectedRevision(t *testing.T) {
	store, ap, git := newMemAttempts(), newApprovals(), &gitDouble{head: "base-1"}
	q := queue(store, ap, git)
	key := enqueued(t, q, attempt())
	if _, err := q.Run(context.Background(), key); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(ap.calls) != 1 {
		t.Fatalf("consumed %d times", len(ap.calls))
	}
	call := ap.calls[0]
	if call.revision != 2 || call.event != "event-9" || call.review != "review-1" {
		t.Errorf("consumed %+v", call)
	}
	if call.key == "" {
		t.Error("the consumption carried no idempotency key")
	}
}

func TestReplayingTheSameApprovalEventMergesNothingMore(t *testing.T) {
	store, ap, git := newMemAttempts(), newApprovals(), &gitDouble{head: "base-1"}
	q := queue(store, ap, git)
	key := enqueued(t, q, attempt())
	if _, err := q.Run(context.Background(), key); err != nil {
		t.Fatalf("run: %v", err)
	}

	// The event arrives again.
	fresh, err := q.Enqueue(context.Background(), attempt())
	if err != nil {
		t.Fatalf("re-enqueue: %v", err)
	}
	if fresh {
		t.Error("a replayed approval event enqueued a second attempt")
	}
	if _, err := q.Run(context.Background(), key); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if len(git.merges) != 1 {
		t.Errorf("merged %d times", len(git.merges))
	}
	if len(ap.calls) != 1 {
		t.Errorf("consumed %d times", len(ap.calls))
	}
}

func TestReApprovalAfterReversalEnqueuesItsOwnAttempt(t *testing.T) {
	// The immutable approval-event id is part of the identity, so an aborted
	// attempt never blocks a later approval at the same revision.
	store, ap, git := newMemAttempts(), newApprovals(), &gitDouble{head: "base-1"}
	q := queue(store, ap, git)
	first := attempt()
	firstKey := enqueued(t, q, first)
	// It aborts — say the base had moved.
	aborted := store.rows[firstKey]
	aborted.State = orchestrator.AttemptAborted
	if err := store.Update(context.Background(), aborted); err != nil {
		t.Fatalf("abort: %v", err)
	}

	again := attempt()
	again.ApprovalEvent = "event-10"
	secondKey := enqueued(t, q, again)
	if secondKey == firstKey {
		t.Fatal("re-approval at the same revision reused the aborted attempt's key")
	}
	if _, err := q.Run(context.Background(), secondKey); err != nil {
		t.Fatalf("run: %v", err)
	}
	if store.rows[firstKey].State != orchestrator.AttemptAborted {
		t.Error("the aborted attempt stopped being a terminal historical record")
	}
	if store.rows[secondKey].State != orchestrator.AttemptMerged {
		t.Errorf("the fresh attempt settled as %q", store.rows[secondKey].State)
	}
}

func TestRecoveryResumesAConsumingAttemptAtItsOwnStep(t *testing.T) {
	store, ap, git := newMemAttempts(), newApprovals(), &gitDouble{head: "base-1"}
	q := queue(store, ap, git)
	key := enqueued(t, q, attempt())
	ap.err = errors.New("sutra unreachable")
	if _, err := q.Run(context.Background(), key); err == nil {
		t.Fatal("a consumption that never landed read as success")
	}
	stuck := store.rows[key]
	if stuck.State != orchestrator.AttemptConsuming || stuck.ConsumeKey == "" {
		t.Fatalf("the crash window is invisible: %+v", stuck)
	}

	ap.err = nil
	n, err := q.RecoverMerges(context.Background())
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if n != 1 {
		t.Errorf("recovered %d attempts", n)
	}
	if ap.calls[len(ap.calls)-1].key != stuck.ConsumeKey {
		t.Error("the replay used a different consume key")
	}
	if store.rows[key].State != orchestrator.AttemptMerged {
		t.Errorf("the resumed attempt settled as %q", store.rows[key].State)
	}
}

func TestRecoveryResumesAMergingAttemptWithoutConsumingAgain(t *testing.T) {
	// The approval was already claimed. Consuming again would be a second
	// claim on something that can only be claimed once.
	store, ap, git := newMemAttempts(), newApprovals(), &gitDouble{head: "base-1"}
	q := queue(store, ap, git)
	key := enqueued(t, q, attempt())
	stuck := store.rows[key]
	stuck.State = orchestrator.AttemptMerging
	stuck.ConsumeKey = "consume-key"
	if err := store.Update(context.Background(), stuck); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := q.RecoverMerges(context.Background()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(ap.calls) != 0 {
		t.Errorf("the approval was consumed again: %+v", ap.calls)
	}
	if store.rows[key].State != orchestrator.AttemptMerged {
		t.Errorf("settled as %q", store.rows[key].State)
	}
}

func TestAMergeThatLandedBeforeItWasRecordedIsNotMergedAgain(t *testing.T) {
	// The narrowest crash window there is: the CAS moved the branch and the
	// process died before AttemptMerged was written. Replaying the merge
	// would build a second merge commit and CAS it against a base the first
	// merge already moved past — which fails forever, stranding an approval
	// that was consumed and work that is already on the branch.
	store, ap, git := newMemAttempts(), newApprovals(), &gitDouble{head: "merge-C2"}
	q := queue(store, ap, git)
	key := enqueued(t, q, attempt())
	stuck := store.rows[key]
	stuck.State = orchestrator.AttemptMerging
	stuck.ConsumeKey = "consume-key"
	if err := store.Update(context.Background(), stuck); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// The pinned commit is already reachable from the default head.
	git.landed = map[string]bool{stuck.MergeSource(): true}

	settled, err := q.Run(context.Background(), key)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(git.merges) != 0 {
		t.Errorf("merged again: %v", git.merges)
	}
	if settled.State != orchestrator.AttemptMerged {
		t.Errorf("the attempt settled as %q", settled.State)
	}
	if settled.MergeCommit != "merge-C2" {
		t.Errorf("recorded merge commit %q, want the head the merge produced",
			settled.MergeCommit)
	}
	if store.rows[key].State != orchestrator.AttemptMerged {
		t.Errorf("the durable attempt is %q", store.rows[key].State)
	}
}

func TestAMergeThatHasNotLandedStillMerges(t *testing.T) {
	// The control: "already reachable" must not be assumed of an attempt that
	// simply has not run yet.
	store, ap, git := newMemAttempts(), newApprovals(), &gitDouble{head: "base-1"}
	q := queue(store, ap, git)
	key := enqueued(t, q, attempt())
	if _, err := q.Run(context.Background(), key); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(git.merges) != 1 {
		t.Errorf("merged %d times", len(git.merges))
	}
}

func TestAReversedVerdictStopsTheMerge(t *testing.T) {
	// sutra rejects the consumption, and nothing lands.
	store, ap, git := newMemAttempts(), newApprovals(), &gitDouble{head: "base-1"}
	ap.reversed = true
	q := queue(store, ap, git)
	key := enqueued(t, q, attempt())
	if _, err := q.Run(context.Background(), key); err == nil {
		t.Fatal("a reversed verdict was merged")
	}
	if len(git.merges) != 0 {
		t.Error("the merge happened despite a refused consumption")
	}
}

func TestAFailedCASLeavesNothingMerged(t *testing.T) {
	store, ap, git := newMemAttempts(), newApprovals(), &gitDouble{head: "base-1", casFails: true}
	q := queue(store, ap, git)
	key := enqueued(t, q, attempt())
	if _, err := q.Run(context.Background(), key); err == nil {
		t.Fatal("a merge that did not land read as success")
	}
	if store.rows[key].MergeCommit != "" {
		t.Error("a merge commit was recorded for a merge that did not land")
	}
	if store.rows[key].State != orchestrator.AttemptMerging {
		t.Errorf("the attempt is in state %q, want the step it is stuck at",
			store.rows[key].State)
	}
}

func TestAnUnknownAttemptIsAnError(t *testing.T) {
	q := queue(newMemAttempts(), newApprovals(), &gitDouble{head: "base-1"})
	if _, err := q.Run(context.Background(), "no-such-key"); err == nil {
		t.Fatal("running an attempt nobody enqueued was allowed")
	}
}

func TestAnUnreadableAttemptStoreStopsTheQueue(t *testing.T) {
	store := newMemAttempts()
	store.err = errors.New("store unavailable")
	q := queue(store, newApprovals(), &gitDouble{head: "base-1"})
	if _, err := q.RecoverMerges(context.Background()); err == nil {
		t.Fatal("an unreadable store read as no attempts in flight")
	}
	if _, err := q.Run(context.Background(), "any"); err == nil {
		t.Fatal("an unreadable store read as an unknown attempt")
	}
	if _, err := q.Enqueue(context.Background(), attempt()); err == nil {
		t.Fatal("an unwritable store read as an enqueue")
	}
}

func TestAnUnreadableHeadStopsBeforeConsuming(t *testing.T) {
	// "I could not read the head" is not "the head is where I expected".
	store, ap := newMemAttempts(), newApprovals()
	git := &gitDouble{headErr: errors.New("git unavailable")}
	q := queue(store, ap, git)
	key := enqueued(t, q, attempt())
	if _, err := q.Run(context.Background(), key); err == nil {
		t.Fatal("an unreadable head was treated as the expected base")
	}
	if len(ap.calls) != 0 {
		t.Error("an approval was consumed without a base check")
	}
}

func TestOnlyTheQueueHeadMerges(t *testing.T) {
	// Two merges racing on one default branch would have one landing against
	// a base the other just moved.
	store, ap, git := newMemAttempts(), newApprovals(), &gitDouble{head: "base-1"}
	q := queue(store, ap, git)
	first := attempt()
	firstKey := enqueued(t, q, first)
	second := attempt()
	second.Build, second.Review, second.ApprovalEvent = "run-2", "review-2", "event-10"
	secondKey := enqueued(t, q, second)

	// The second attempt is asked first, and declines: it is not the head. It
	// reports WAITING, which is neither success nor failure — a run told
	// either would be moved somewhere it does not belong.
	waiting, err := q.Run(context.Background(), secondKey)
	if !errors.Is(err, orchestrator.ErrWaiting) {
		t.Fatalf("got %v, want ErrWaiting", err)
	}
	if waiting.State != orchestrator.AttemptQueued {
		t.Errorf("the waiting attempt is in %q, want queued", waiting.State)
	}
	if len(ap.calls) != 0 {
		t.Error("a waiting attempt consumed an approval")
	}
	if len(git.merges) != 0 {
		t.Error("a waiting attempt merged")
	}

	// The head runs.
	if _, err := q.Run(context.Background(), firstKey); err != nil {
		t.Fatalf("run first: %v", err)
	}
	if len(git.merges) != 1 {
		t.Errorf("merged %v", git.merges)
	}
}

func TestTheSecondAttemptPreflightsAgainstTheMovedBase(t *testing.T) {
	// Once the first merge moves the default head, the second's frozen base is
	// stale: it aborts with its approval UNCONSUMED and takes the
	// integrate-rerun-fresh-review path.
	store, ap, git := newMemAttempts(), newApprovals(), &gitDouble{head: "base-1"}
	q := queue(store, ap, git)
	firstKey := enqueued(t, q, attempt())
	second := attempt()
	second.Build, second.Review, second.ApprovalEvent = "run-2", "review-2", "event-10"
	secondKey := enqueued(t, q, second)

	if _, err := q.Run(context.Background(), firstKey); err != nil {
		t.Fatalf("run first: %v", err)
	}
	// The first merge moved the head.
	git.head = git.merged

	got, err := q.Run(context.Background(), secondKey)
	if err != nil {
		t.Fatalf("run second: %v", err)
	}
	if got.State != orchestrator.AttemptAborted {
		t.Errorf("the second attempt settled as %q", got.State)
	}
	if len(ap.calls) != 1 {
		t.Errorf("consumed %d approvals — the second's must be untouched", len(ap.calls))
	}
	if len(git.merges) != 1 {
		t.Errorf("merged %v", git.merges)
	}
}

func TestTwoTargetsSharingARepositoryShareAQueueHead(t *testing.T) {
	// The lock is scoped to what is CONTENDED — the repository and branch the
	// CAS targets — not to the target. Two targets in one repository racing
	// the same branch is exactly the case the ordering exists to prevent.
	store, ap, git := newMemAttempts(), newApprovals(), &gitDouble{head: "base-1"}
	q := queue(store, ap, git)
	enqueued(t, q, attempt())
	other := attempt()
	other.TargetKey, other.Build = "OTHER0b", "run-2"
	other.Review, other.ApprovalEvent = "review-2", "event-10"
	otherKey := enqueued(t, q, other)

	_, err := q.Run(context.Background(), otherKey)
	if !errors.Is(err, orchestrator.ErrWaiting) {
		t.Fatalf("got %v, want ErrWaiting — it shares the branch", err)
	}
	if len(ap.calls) != 0 {
		t.Error("a waiting attempt consumed an approval")
	}
}

func TestADifferentRepositoryDoesNotBlock(t *testing.T) {
	// Nothing is contended between them.
	store, ap, git := newMemAttempts(), newApprovals(), &gitDouble{head: "base-1"}
	q := queue(store, ap, git)
	enqueued(t, q, attempt())

	elsewhere := queue(store, ap, git)
	elsewhere.Resource = "/other-repo#main"
	other := attempt()
	other.TargetKey, other.Build = "OTHER0b", "run-2"
	other.Review, other.ApprovalEvent = "review-2", "event-10"
	other.Resource = "/other-repo#main"
	otherKey := enqueued(t, elsewhere, other)

	got, err := elsewhere.Run(context.Background(), otherKey)
	if err != nil {
		t.Fatalf("run other repository: %v", err)
	}
	if got.State != orchestrator.AttemptMerged {
		t.Errorf("an attempt in another repository settled as %q", got.State)
	}
}

func TestRecoveryDrainsTheQueueRatherThanStoppingAtItsHead(t *testing.T) {
	// Advancing a head settles it, and the attempt behind becomes the new
	// head. Stopping at the first would leave that one queued until something
	// else restarted recovery — and recovery runs once, at startup.
	store, ap, git := newMemAttempts(), newApprovals(), &gitDouble{head: "base-1"}
	q := queue(store, ap, git)
	firstKey := enqueued(t, q, attempt())
	second := attempt()
	second.Build, second.Review, second.ApprovalEvent = "run-2", "review-2", "event-10"
	secondKey := enqueued(t, q, second)

	n, err := q.RecoverMerges(context.Background())
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if n != 2 {
		t.Errorf("resumed %d attempts, want the whole queue drained", n)
	}
	if store.rows[firstKey].State != orchestrator.AttemptMerged {
		t.Errorf("the head settled as %q", store.rows[firstKey].State)
	}
	// The second aborts: the first merge moved the head under it. Aborted is
	// terminal, which is the point — nothing is left queued.
	if !store.rows[secondKey].Terminal() {
		t.Errorf("the second attempt is still %q", store.rows[secondKey].State)
	}
}

func TestRecoveryStopsBehindAnAttemptStillHoldingTheSection(t *testing.T) {
	// An attempt that does not settle keeps the section, and recovery must not
	// do what running work is forbidden from doing.
	store, ap, git := newMemAttempts(), newApprovals(), &gitDouble{head: "base-1", casFails: true}
	q := queue(store, ap, git)
	enqueued(t, q, attempt())
	second := attempt()
	second.Build, second.Review, second.ApprovalEvent = "run-2", "review-2", "event-10"
	secondKey := enqueued(t, q, second)

	if _, err := q.RecoverMerges(context.Background()); err == nil {
		t.Fatal("a merge that did not land read as success")
	}
	if store.rows[secondKey].State != orchestrator.AttemptQueued {
		t.Errorf("the waiting attempt moved to %q", store.rows[secondKey].State)
	}
}

func TestRecoveryLeavesAnotherRepositorysAttemptsAlone(t *testing.T) {
	// The attempt store is shared across every target in one database, but a
	// queue's Git is bound to ONE repository and branch. Advancing a foreign
	// attempt would preflight and CAS against the wrong repository, and abort
	// an attempt whose base has not moved at all.
	store, ap, git := newMemAttempts(), newApprovals(), &gitDouble{head: "base-1"}
	q := queue(store, ap, git)
	foreign := attempt()
	foreign.Resource = "/other-repo#main"
	foreign.Review = "review-other"
	if _, err := q.Enqueue(context.Background(), foreign); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// The enqueue stamps this queue's resource, so put the foreign one back.
	for key, row := range store.rows {
		if row.Review == "review-other" {
			row.Resource = "/other-repo#main"
			store.rows[key] = row
		}
	}

	n, err := q.RecoverMerges(context.Background())
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if n != 0 {
		t.Errorf("recovered %d foreign attempts", n)
	}
	if len(git.merges) != 0 || len(ap.calls) != 0 {
		t.Errorf("touched another repository: merges=%v approvals=%v", git.merges, ap.calls)
	}
	for _, row := range store.rows {
		if row.State == orchestrator.AttemptAborted {
			t.Errorf("a foreign attempt was aborted: %+v", row)
		}
	}
}
