package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
)

// A merge attempt's states.
//
// Write-ahead around each side effect: consuming before the keyed sutra
// consumption and consumed after; merging before the CAS push and merged with
// the merge commit after. Recovery reads the exact step and resumes IT, never
// conflating one attempt's step with another's.
const (
	AttemptQueued    = "queued"
	AttemptHolding   = "holding"
	AttemptConsuming = "consuming"
	AttemptConsumed  = "consumed"
	AttemptMerging   = "merging"
	AttemptMerged    = "merged"
	AttemptAborted   = "aborted"
)

// MergeAttempt is one durable entry in a target's merge queue.
type MergeAttempt struct {
	// Key is deterministic per (target, build, review, revision, approval
	// event). The review id disambiguates successive fresh reviews, and the
	// immutable approval-event id disambiguates re-approval after reversal at
	// the same revision — so an aborted attempt never blocks a later approval
	// from enqueueing.
	Key       string
	TargetKey string
	Build     string
	Review    string
	Revision  int
	// ApprovalEvent is the immutable id of the approval that created this
	// attempt. Recovery reads it from the row rather than inferring which
	// approval was meant.
	ApprovalEvent string
	// Commit is the approved revision's pinned commit — what actually merges.
	Commit string
	// Resource is what the merge actually contends for: the repository and
	// branch the CAS targets. Two targets sharing a repository share a queue
	// head, because two merges racing on one branch is the thing the ordering
	// exists to prevent — the TARGET is not what is contended.
	Resource string
	// ExpectedBase is the frozen gated_base the preflight validated. The
	// merge CAS targets exactly this.
	ExpectedBase string
	// ConsumeKey is the Idempotency-Key for the consumption call, persisted on
	// entering consuming so a recovery replays rather than re-decides.
	ConsumeKey string
	// MergeCommit is recorded when the CAS push lands.
	MergeCommit string
	State       string
	// Note records why an attempt aborted.
	Note string
}

// MergeSource is the commit that merges — the immutable pin, never a branch.
func (a MergeAttempt) MergeSource() string { return a.Commit }

// Terminal reports whether an attempt has finished, one way or the other.
func (a MergeAttempt) Terminal() bool {
	return a.State == AttemptMerged || a.State == AttemptAborted
}

// AttemptKey derives the queue identity of an approval.
func AttemptKey(targetKey, build, review string, revision int, approvalEvent string) string {
	sum := sha256.Sum256([]byte("kriya-merge-attempt:" + targetKey + ":" + build + ":" +
		review + ":" + strconv.Itoa(revision) + ":" + approvalEvent))
	return hex.EncodeToString(sum[:])
}

// AttemptStore persists merge attempts.
type AttemptStore interface {
	// Insert enqueues an attempt, reporting whether it was new. A replayed
	// approval event collides on the key instead of duplicating.
	Insert(ctx context.Context, a MergeAttempt) (bool, error)
	Update(ctx context.Context, a MergeAttempt) error
	Find(ctx context.Context, key string) (MergeAttempt, bool, error)
	// Unfinished lists attempts a crash left mid-flight, oldest first.
	Unfinished(ctx context.Context) ([]MergeAttempt, error)
}

// Approvals is the slice of the tracker a merge consumes.
type Approvals interface {
	// Consume claims the approval at the expected revision. The key makes a
	// replay return the original result rather than claiming twice.
	Consume(ctx context.Context, review, actor string, expectedRevision int,
		expectedVerdictEvent, key string) error
}

// Merger performs the merge itself.
//
// An interface this package declares: what it needs is "can this land, and
// land it against exactly this base", and the git shell-out lives elsewhere.
type Merger interface {
	// Preflight computes a conflict-free deterministic merge result WITHOUT
	// publishing anything, and reports whether one exists.
	Preflight(ctx context.Context, commit, base string) (bool, error)
	// Head resolves the default branch's current commit.
	Head(ctx context.Context) (string, error)
	// Merge lands commit under a compare-and-swap on the default-branch head:
	// it must still be expectedBase, or the merge does not happen.
	Merge(ctx context.Context, commit, expectedBase string) (string, error)
	// Landed reports whether commit is already reachable from the default
	// branch. It answers the one question a replayed merge has to ask: the
	// CAS is not idempotent, so an attempt resuming in the merging state
	// must know whether its own merge already happened.
	Landed(ctx context.Context, commit string) (bool, error)
}

// Queue merges approved work, one attempt at a time per target.
type Queue struct {
	Store     AttemptStore
	Runs      Store
	Approvals Approvals
	Git       Merger
	// Resource names the repository and branch this queue merges into. It is
	// what serialization is scoped to.
	Resource string
	Actor    string
}

// Enqueue records an approval event as a merge attempt.
//
// Returns false when the key already exists: a replayed approval event
// collides instead of duplicating, which is what makes "the same approval
// event replayed causes no second merge" true before any merging happens.
func (q Queue) Enqueue(ctx context.Context, a MergeAttempt) (bool, error) {
	a.Key = AttemptKey(a.TargetKey, a.Build, a.Review, a.Revision, a.ApprovalEvent)
	a.State = AttemptQueued
	fresh, err := q.Store.Insert(ctx, a)
	if err != nil {
		return false, fmt.Errorf("enqueue merge attempt: %w", err)
	}
	return fresh, nil
}

// Run advances one attempt to a terminal state.
//
// The steps are ordered so nothing irreversible happens before the reversible
// checks have passed: preflight computes a merge result before ANYTHING is
// consumed, because a consumed approval cannot be given back and a conflict
// discovered afterwards would strand the review.
func (q Queue) Run(ctx context.Context, key string) (MergeAttempt, error) {
	a, found, err := q.Store.Find(ctx, key)
	if err != nil {
		return MergeAttempt{}, fmt.Errorf("find merge attempt: %w", err)
	}
	if !found {
		return MergeAttempt{}, fmt.Errorf("no merge attempt %s", key)
	}
	if a.Terminal() {
		// Already settled. A replay of the approval event must not merge
		// again, and an aborted attempt stays a terminal historical record.
		return a, nil
	}
	head, err := q.head(ctx, a.Resource)
	if err != nil {
		return MergeAttempt{}, err
	}
	if head != a.Key {
		// Another attempt holds the section for this repository and branch.
		// This one WAITS — it has neither advanced nor failed — consuming
		// nothing: two merges racing on one branch would have one landing
		// against a base the other just moved.
		return a, fmt.Errorf("merge attempt %s: %w", a.Key, ErrWaiting)
	}
	return q.advance(ctx, a)
}

// head returns the oldest unfinished attempt's key for a merge resource.
//
// The queue ORDER is the lock. A separate lock row would be a second thing to
// keep in step with the queue, and a crash between taking it and recording the
// step it guards would leave it held by nobody.
func (q Queue) head(ctx context.Context, resource string) (string, error) {
	open, err := q.Store.Unfinished(ctx)
	if err != nil {
		return "", fmt.Errorf("list unfinished merge attempts: %w", err)
	}
	for _, a := range open {
		if a.Resource == resource {
			return a.Key, nil
		}
	}
	return "", nil
}

// advance resumes an attempt from whatever step it is at.
func (q Queue) advance(ctx context.Context, a MergeAttempt) (MergeAttempt, error) {
	var err error
	if a.State == AttemptQueued || a.State == AttemptHolding {
		if a, err = q.preflight(ctx, a); err != nil || a.Terminal() {
			return a, err
		}
	}
	if a.State == AttemptHolding || a.State == AttemptConsuming {
		if a, err = q.consume(ctx, a); err != nil {
			return a, err
		}
	}
	return q.merge(ctx, a)
}

// preflight proves the merge can land before anything is consumed.
func (q Queue) preflight(ctx context.Context, a MergeAttempt) (MergeAttempt, error) {
	head, err := q.Git.Head(ctx)
	if err != nil {
		return a, fmt.Errorf("resolve default head: %w", err)
	}
	if head != a.ExpectedBase {
		// The base moved under the chain that gated this work, so the gates
		// no longer say anything about what would land. Aborting is the
		// honest outcome; the run re-gates against the new base.
		return q.abort(ctx, a, fmt.Sprintf(
			"default head moved from %s to %s", a.ExpectedBase, head))
	}
	clean, err := q.Git.Preflight(ctx, a.MergeSource(), a.ExpectedBase)
	if err != nil {
		return a, fmt.Errorf("preflight merge: %w", err)
	}
	if !clean {
		return q.abort(ctx, a, "the merge does not apply cleanly")
	}
	a.State = AttemptHolding
	if err := q.Store.Update(ctx, a); err != nil {
		return a, fmt.Errorf("record preflighted attempt: %w", err)
	}
	return a, nil
}

// consume claims the approval, write-ahead.
func (q Queue) consume(ctx context.Context, a MergeAttempt) (MergeAttempt, error) {
	if a.State != AttemptConsuming {
		a.ConsumeKey = consumeKey(a.Key)
		a.State = AttemptConsuming
		if err := q.Store.Update(ctx, a); err != nil {
			return a, fmt.Errorf("record consuming attempt: %w", err)
		}
	}
	err := q.Approvals.Consume(ctx, a.Review, q.Actor, a.Revision, a.ApprovalEvent, a.ConsumeKey)
	if err != nil {
		return a, fmt.Errorf("consume approval on %s: %w", a.Review, err)
	}
	a.State = AttemptConsumed
	if err := q.Store.Update(ctx, a); err != nil {
		return a, fmt.Errorf("record consumed attempt: %w", err)
	}
	return a, nil
}

// merge lands the pinned commit under a CAS on the default-branch head.
func (q Queue) merge(ctx context.Context, a MergeAttempt) (MergeAttempt, error) {
	if a.State == AttemptMerging {
		// Resuming. The CAS is not idempotent — it swaps against a base its
		// own success moved past — so merging again would build a second
		// merge commit and fail forever against the old base, stranding a
		// consumed approval and work that is already on the branch.
		settled, done, err := q.alreadyLanded(ctx, a)
		if err != nil || done {
			return settled, err
		}
	}
	if a.State != AttemptMerging {
		a.State = AttemptMerging
		if err := q.Store.Update(ctx, a); err != nil {
			return a, fmt.Errorf("record merging attempt: %w", err)
		}
	}
	// The IMMUTABLE pinned commit, never the mutable branch reference: the
	// branch may have moved since approval, and what the human approved is the
	// commit.
	commit, err := q.Git.Merge(ctx, a.MergeSource(), a.ExpectedBase)
	if err != nil {
		return a, fmt.Errorf("merge %s: %w", a.MergeSource(), err)
	}
	a.MergeCommit = commit
	a.State = AttemptMerged
	if err := q.Store.Update(ctx, a); err != nil {
		return a, fmt.Errorf("record merged attempt: %w", err)
	}
	return a, nil
}

// alreadyLanded settles a resumed attempt whose merge already happened.
//
// The current head is recorded as the merge commit. It is not necessarily the
// commit the first merge produced — another attempt may have landed behind it
// — but it is the commit this work is reachable from, which is what completion
// needs to name.
func (q Queue) alreadyLanded(ctx context.Context, a MergeAttempt) (MergeAttempt, bool, error) {
	landed, err := q.Git.Landed(ctx, a.MergeSource())
	if err != nil {
		return a, false, fmt.Errorf("read whether %s landed: %w", a.MergeSource(), err)
	}
	if !landed {
		return a, false, nil
	}
	head, err := q.Git.Head(ctx)
	if err != nil {
		return a, false, fmt.Errorf("resolve default head: %w", err)
	}
	a.MergeCommit = head
	a.State = AttemptMerged
	if err := q.Store.Update(ctx, a); err != nil {
		return a, false, fmt.Errorf("record merged attempt: %w", err)
	}
	return a, true, nil
}

// abort settles an attempt that cannot proceed.
//
// Terminal and durable: an aborted attempt stays a historical record, and a
// later approval enqueues its OWN attempt rather than reviving this one.
func (q Queue) abort(ctx context.Context, a MergeAttempt, note string) (MergeAttempt, error) {
	a.State = AttemptAborted
	a.Note = note
	if err := q.Store.Update(ctx, a); err != nil {
		return a, fmt.Errorf("record aborted attempt: %w", err)
	}
	return a, nil
}

// RecoverMerges resumes attempts a crash left mid-flight.
func (q Queue) RecoverMerges(ctx context.Context) (int, error) {
	open, err := q.Store.Unfinished(ctx)
	if err != nil {
		return 0, fmt.Errorf("list unfinished merge attempts: %w", err)
	}
	// In queue order, one resource at a time — the same serialization Run
	// enforces — but DRAINING: advancing a head can settle it, and the attempt
	// behind it becomes the new head. Stopping at the first would leave that
	// one queued until something else restarted recovery.
	busy := map[string]bool{}
	var resumed int
	for _, a := range open {
		if busy[a.Resource] {
			continue
		}
		settled, err := q.advance(ctx, a)
		if err != nil {
			return 0, err
		}
		resumed++
		if !settled.Terminal() {
			// It is still holding the section; nothing behind it can run.
			busy[a.Resource] = true
		}
	}
	return resumed, nil
}

// consumeKey derives the consumption's idempotency key from the attempt's own.
func consumeKey(attemptKey string) string {
	sum := sha256.Sum256([]byte("kriya-approval-consume:" + attemptKey))
	return hex.EncodeToString(sum[:])
}

// Approved is what an observed approval carries.
type Approved struct {
	// Review, Revision and Event identify the approval. The event id is
	// immutable and part of the attempt's identity, so re-approval after a
	// reversal enqueues its own attempt.
	Review   string
	Revision int
	Event    string
	// Commit is the approved revision's pinned commit — what merges.
	Commit string
	// TargetKey scopes the queue to one target.
	TargetKey string
}

// OnApproval enqueues an observed approval and moves the run into merging.
//
// The attempt is enqueued BEFORE the run's state changes: an attempt with no
// run pointing at it is a queue entry recovery finishes, while a run in
// merging with no attempt is a run nothing can advance.
func (q Queue) OnApproval(ctx context.Context, run BuildRun, a Approved) (BuildRun, string, error) {
	if run.State != StateReviewSubmitted {
		return run, "", fmt.Errorf(
			"run %s is in %q, not awaiting review", run.ID, run.State)
	}
	attempt := MergeAttempt{
		TargetKey: a.TargetKey, Build: run.ID, Review: a.Review, Revision: a.Revision,
		ApprovalEvent: a.Event, Commit: a.Commit, ExpectedBase: run.GatedBase,
		Resource: q.Resource,
	}
	if _, err := q.Enqueue(ctx, attempt); err != nil {
		return run, "", err
	}
	key := AttemptKey(a.TargetKey, run.ID, a.Review, a.Revision, a.Event)
	// Recorded on the RUN, because completion reads them from there: the
	// ticket close names the review at its approved revision with its
	// approval's verdict event, and an initial submission that never went
	// through a rework has neither on the run otherwise.
	run.ReviewRevision = a.Revision
	run.ReviewVerdictEvent = a.Event
	run.State = StateMerging
	if err := q.Runs.Upsert(ctx, run); err != nil {
		return run, "", fmt.Errorf("record merging run: %w", err)
	}
	return run, key, nil
}

// AttemptFor finds the unfinished attempt for a run.
//
// By run rather than by key: the merge stage knows which run it is advancing,
// and the key lives on the attempt. At most one attempt per run is unfinished
// — an aborted one is terminal, and a fresh approval enqueues its own.
func (q Queue) AttemptFor(ctx context.Context, build string) (string, bool, error) {
	open, err := q.Store.Unfinished(ctx)
	if err != nil {
		return "", false, fmt.Errorf("list unfinished merge attempts: %w", err)
	}
	for _, a := range open {
		if a.Build == build {
			return a.Key, true, nil
		}
	}
	return "", false, nil
}
