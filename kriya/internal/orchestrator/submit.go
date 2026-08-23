package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
)

// Review-submission states.
//
// A write-ahead marker around review creation. The key, the pinned commit and
// the session are persisted with `submitting` BEFORE sutra is called, so
// recovery replays the ORIGINAL request rather than rebuilding one.
const (
	SubmitNone         = "none"
	SubmitSubmitting   = "submitting"
	SubmitSubmitted    = "submitted"
	SubmitResubmitting = "resubmitting"
)

// Reviews is the slice of the tracker a submission needs.
type Reviews interface {
	// Create opens a review and returns its id. The key is sent as the
	// Idempotency-Key: a replay returns the original review.
	Create(ctx context.Context, issue, author, summary, branch, commit,
		session, key string) (string, error)
	// Resubmit advances a review to its next revision and returns that
	// revision. expectedRevision and expectedVerdictEvent are fences sutra
	// enforces; the key makes a replay idempotent.
	Resubmit(ctx context.Context, id, author, summary, branch, commit, session string,
		expectedRevision int, expectedVerdictEvent, key string) (int, error)
}

// submissionKey is deterministic per (run, head commit, gate attempt).
//
// The gate attempt is in it deliberately: an integration that leaves the
// commit unchanged still yields a FRESH key, so a replay can never return a
// prior — possibly already consumed — review when a new one is required.
func submissionKey(run, commit string, attempt int) string {
	sum := sha256.Sum256([]byte(
		"kriya-review-submission:" + run + ":" + commit + ":" + strconv.Itoa(attempt)))
	return hex.EncodeToString(sum[:])
}

// Submitter turns a validated run into a sutra review.
type Submitter struct {
	Store   Store
	Reviews Reviews
	// Author is the identity every tracker mutation is recorded against.
	Author string
}

// Submission is what a review needs beyond the run's own fields.
type Submission struct {
	// Issue is the sutra issue the review hangs off.
	Issue string
	// Branch is the run-scoped branch the work is on.
	Branch string
	// Session is the dev session that did the work. Feedback routes back by
	// it, which is why it is pinned rather than read at replay time.
	Session string
	Summary string
}

// Submit opens the review for a run that passed validation.
//
// The key, the pinned commit and the session are written in ONE update before
// sutra is called. Replay reproduces the original request from those fields:
// a branch that moved after the crash cannot smuggle an ungated commit under
// the old key, and a fresh recovery session cannot displace the session
// feedback must route to.
func (s Submitter) Submit(ctx context.Context, run BuildRun, sub Submission) (BuildRun, error) {
	if run.GatedBase == "" {
		return BuildRun{}, fmt.Errorf("run %s has no gated commit to submit", run.ID)
	}
	run.ReviewKey = submissionKey(run.ID, run.GatedBase, run.Attempt)
	run.ReviewCommit = run.GatedBase
	run.ReviewSession = sub.Session
	run.ReviewState = SubmitSubmitting
	if err := s.Store.Upsert(ctx, run); err != nil {
		return BuildRun{}, fmt.Errorf("record pending submission: %w", err)
	}
	return s.finish(ctx, run, sub)
}

// finish performs the call the write-ahead row promised.
//
// Shared with recovery so a replay takes exactly the same path as a fresh
// submission — two implementations of one protocol would leave only one
// tested.
func (s Submitter) finish(ctx context.Context, run BuildRun, sub Submission) (BuildRun, error) {
	id, err := s.Reviews.Create(ctx, sub.Issue, s.Author, sub.Summary,
		sub.Branch, run.ReviewCommit, run.ReviewSession, run.ReviewKey)
	if err != nil {
		return BuildRun{}, fmt.Errorf("submit review for %s: %w", run.ID, err)
	}
	run.ReviewID = id
	run.ReviewState = SubmitSubmitted
	if err := s.Store.Upsert(ctx, run); err != nil {
		return BuildRun{}, fmt.Errorf("record submitted review: %w", err)
	}
	return run, nil
}

// RecoverSubmissions replays submissions a crash left in flight.
//
// Replayed under the SAME key, from the SAME persisted fields: sutra returns
// the original review for a request that landed and creates otherwise, so
// exactly one review exists for the run either way.
func (s Submitter) RecoverSubmissions(ctx context.Context, subs func(BuildRun) Submission) (int, error) {
	pending, err := s.Store.Submitting(ctx)
	if err != nil {
		return 0, fmt.Errorf("list submitting runs: %w", err)
	}
	for _, run := range pending {
		if _, err := s.finish(ctx, run, subs(run)); err != nil {
			return 0, err
		}
	}
	return len(pending), nil
}

// resubmissionKey is derived from the original submission key, the revision
// being advanced to, and the verdict event being answered.
//
// All three, because none alone is enough: the submission key alone would
// collide across revisions, the revision alone across reworks of the same
// revision, and the event is what pins WHICH verdict this rework answers.
func resubmissionKey(submission string, revision int, event string) string {
	sum := sha256.Sum256([]byte(
		"kriya-review-resubmission:" + submission + ":" + strconv.Itoa(revision) + ":" + event))
	return hex.EncodeToString(sum[:])
}

// Rework is what a resubmission needs beyond the run's own fields.
type Rework struct {
	Branch  string
	Session string
	Summary string
	// Revision is the review's current revision — the one the resubmission
	// expects to find and advance from.
	Revision int
	// VerdictEvent is the changes-requested event this rework answers.
	VerdictEvent string
}

// Resubmit advances a reworked review to its next revision.
//
// The resubmitting state, its key, the expected revision, the answered verdict
// event and the pinned commit are persisted TRANSACTIONALLY before sutra is
// called. The replay then names exactly those: neither a moved branch nor a
// later verdict can change what it requests.
func (s Submitter) Resubmit(ctx context.Context, run BuildRun, rw Rework) (BuildRun, error) {
	if run.ReviewID == "" {
		return BuildRun{}, fmt.Errorf("run %s has no review to resubmit", run.ID)
	}
	if run.GatedBase == "" {
		return BuildRun{}, fmt.Errorf("run %s has no gated commit to resubmit", run.ID)
	}
	if rw.VerdictEvent == "" {
		// Without it sutra cannot fence the call, and a rework could answer a
		// verdict the human has since replaced.
		return BuildRun{}, fmt.Errorf("run %s names no verdict event to answer", run.ID)
	}
	run.ReviewKey = resubmissionKey(
		submissionKey(run.ID, run.GatedBase, run.Attempt), rw.Revision+1, rw.VerdictEvent)
	run.ReviewCommit = run.GatedBase
	run.ReviewSession = rw.Session
	run.ReviewRevision = rw.Revision
	run.ReviewVerdictEvent = rw.VerdictEvent
	run.ReviewState = SubmitResubmitting
	if err := s.Store.Upsert(ctx, run); err != nil {
		return BuildRun{}, fmt.Errorf("record pending resubmission: %w", err)
	}
	return s.finishRework(ctx, run, rw)
}

// finishRework performs the call the write-ahead row promised.
func (s Submitter) finishRework(ctx context.Context, run BuildRun, rw Rework) (BuildRun, error) {
	revision, err := s.Reviews.Resubmit(ctx, run.ReviewID, s.Author, rw.Summary,
		rw.Branch, run.ReviewCommit, run.ReviewSession,
		run.ReviewRevision, run.ReviewVerdictEvent, run.ReviewKey)
	if err != nil {
		return BuildRun{}, fmt.Errorf("resubmit review for %s: %w", run.ID, err)
	}
	if revision != run.ReviewRevision+1 {
		// sutra advances by exactly one. Anything else means the review moved
		// under the fence, which is the situation the fence exists to catch.
		return BuildRun{}, fmt.Errorf(
			"review %s advanced to revision %d, expected %d",
			run.ReviewID, revision, run.ReviewRevision+1)
	}
	run.ReviewRevision = revision
	run.ReviewState = SubmitSubmitted
	if err := s.Store.Upsert(ctx, run); err != nil {
		return BuildRun{}, fmt.Errorf("record resubmitted review: %w", err)
	}
	return run, nil
}

// RecoverResubmissions replays resubmissions a crash left in flight.
//
// The replay names the PERSISTED verdict event and pinned commit, so neither a
// moved branch nor a later verdict can change what it requests. sutra's fence
// then makes the revision advance exactly once however many times it runs.
func (s Submitter) RecoverResubmissions(ctx context.Context, rw func(BuildRun) Rework) (int, error) {
	pending, err := s.Store.Resubmitting(ctx)
	if err != nil {
		return 0, fmt.Errorf("list resubmitting runs: %w", err)
	}
	for _, run := range pending {
		if _, err := s.finishRework(ctx, run, rw(run)); err != nil {
			return 0, err
		}
	}
	return len(pending), nil
}
