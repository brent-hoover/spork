package planner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
)

// Completion lifecycle states.
//
// Each one is recorded BEFORE the call it names, so recovery resumes the exact
// step rather than guessing which side of a crash it is on.
const (
	CompletionNone         = "none"
	CompletionSubmitting   = "review-submitting"
	CompletionSubmitted    = "review-submitted"
	CompletionResubmitting = "review-resubmitting"
	CompletionClosing      = "closing"
	CompletionComplete     = "complete"
	// CompletionStale is where a claim rests once its epoch has moved past
	// it. Terminal for THIS attempt: it can never stamp, and leaving it in
	// closing would have recovery retry it on every startup for the life of
	// the database.
	CompletionStale = "stale"
)

// CompletionClaim is one attempt to declare a target's build complete.
//
// The WHOLE field set rotates as one unit at review-submitting, before any
// external call, and is immutable within the attempt. The submission key is
// derived from (target, completion epoch), so it is EPOCH-SCOPED: after a
// reopen or a supersession advances the epoch, recompletion is a new attempt
// with a new key, a new review and fresh human approval. Recovery can never
// adopt a prior epoch's approved review, because the key names the old epoch —
// an old approval never authorizes newly added work.
type CompletionClaim struct {
	TargetKey string
	State     string
	// Epoch is the completion epoch this claim is bound to. The stamp's CAS
	// compares against it.
	Epoch int
	// SubmissionKey is the review creation's Idempotency-Key, derived from
	// (target, epoch). It rides the request, so recovery replays the persisted
	// request under the same key and sutra returns the original review.
	SubmissionKey string
	// ReportKey is the document mutation's own key, persisted in the same
	// transaction — so a crash after the document exists but before the review
	// recovers the EXACT version rather than appending another.
	ReportKey string
	// PendingReport is the prepared report content, persisted before the
	// document exists. A replay that regenerated it could write different
	// bytes under the same key.
	PendingReport string
	// ReportDoc and ReportVersion are what the document mutation returned.
	// The review names the version.
	ReportDoc      string
	ReportVersion  string
	ReviewID       string
	ReviewRevision int
	// SubtreeRevision is the epic's revision at capture. The close is fenced
	// on it: history that moved invalidates the claim even when current state
	// still matches.
	SubtreeRevision int64
	// ApprovalEvent is the verdict event the close is spending. It is IN the
	// close key, so a reapproval after a reversal-induced conflict issues
	// under a fresh one rather than replaying the cached conflict.
	ApprovalEvent string
	// CloseKey is the epic close's Idempotency-Key, persisted with the
	// closing state before the call. A close that landed replays to its
	// original success under it — never a close-used conflict, since this
	// very key stamped it.
	CloseKey string
	// Watermark is the feed position the detection that armed this attempt
	// saw. Everything at or before it is accounted for by that detection —
	// including the build's OWN completion events, which would otherwise read
	// as reopens and destroy the claim the moment it existed.
	Watermark string
	// ReopenOwed records that this claim's close may be standing over a
	// newer epoch. It is set when a stale claim is settled without having
	// resolved whether its close landed: the epic could be closed with no
	// compensating reopen, and "we do not know" must not read as "nothing
	// happened". The operator inbox is what lists it.
	ReopenOwed bool
}

// ClaimStore persists completion claims.
type ClaimStore interface {
	Upsert(ctx context.Context, c CompletionClaim) error
	Find(ctx context.Context, targetKey string) (CompletionClaim, bool, error)
	// Submitting lists claims a crash left mid-submission.
	Submitting(ctx context.Context) ([]CompletionClaim, error)
	// Closing lists claims a crash left mid-close.
	Closing(ctx context.Context) ([]CompletionClaim, error)
}

// Reports renders a build's completion report.
type Reports interface {
	Render(ctx context.Context, targetKey string) (string, error)
}

// Documents is the slice of the tracker a completion report needs.
type Documents interface {
	// Create returns the document and the version it points at. A replayed
	// key returns the ORIGINAL of both — no second version appends.
	Create(ctx context.Context, projectID, title, issue, content, key string) (doc, version string, err error)
}

// CompletionReviews opens the review a human approves.
type CompletionReviews interface {
	Create(ctx context.Context, issue, summary, docVersion, key string) (id string, revision int, err error)
}

// SubmissionKey is deterministic per (target, completion epoch).
//
// EPOCH-SCOPED, which is the whole point: a recompletion after a reopen or a
// supersession presents a different key, so it gets a new review and fresh
// human approval rather than replaying a spent one.
func SubmissionKey(targetKey string, epoch int) string {
	return claimKey("kriya-completion-submit:" + targetKey + ":" + strconv.Itoa(epoch))
}

// ReportKey is the report document mutation's key for one attempt.
func ReportKey(submissionKey string) string {
	return claimKey("kriya-completion-report:" + submissionKey)
}

func claimKey(material string) string {
	sum := sha256.Sum256([]byte(material))
	return hex.EncodeToString(sum[:])
}

// CloseKey is deterministic per (target, epoch, approval event).
//
// The APPROVAL EVENT is in it because a reapproval after a reversal-induced
// conflict must issue under a fresh key: replaying the old one would return
// the cached conflict forever, and the build could never finish.
func CloseKey(targetKey string, epoch int, approvalEvent string) string {
	return claimKey("kriya-completion-close:" + targetKey + ":" +
		strconv.Itoa(epoch) + ":" + approvalEvent)
}

// Synced marks the feed position a claim accounts for.
//
// Part of the protocol rather than something the caller does afterwards: the
// events a detection already saw the effects of include the build's OWN
// completion events, and a caller who forgot this step would have every one of
// them read as a reopen the moment the claim existed.
type Synced interface {
	SyncTo(ctx context.Context, watermark string) error
}

// Epics closes a build's umbrella issue.
type Epics interface {
	// Close completes the epic, fenced on the subtree revision the claim
	// captured. sutra's no-open-children gate is the authoritative check.
	Close(ctx context.Context, epic, review string, revision int,
		verdictEvent string, expectedSubtreeRevision int64, key string) error
}

// Claimer submits the review a human approves to finish a build.
type Claimer struct {
	Claims    ClaimStore
	Reports   Reports
	Documents Documents
	Reviews   CompletionReviews
	Epics     Epics
	Epochs    Epochs
	// Work marks the feed position this claim accounts for. Nil skips it,
	// which is what a module-level test of the protocol itself wants.
	Work Synced
}

// Submit opens the build-completion review for an armed target.
//
// The order is the protocol. The claim's whole field set is recorded FIRST,
// before anything external exists; then the report document, whose returned
// version is persisted before the review-create call; then the review. Every
// step names a key persisted in the step before it, so recovery from any point
// replays exactly what was promised.
func (c Claimer) Submit(
	ctx context.Context, targetKey, projectID, epicID string,
	epoch int, subtreeRevision int64, watermark string,
) (CompletionClaim, error) {
	// The epoch is the CALLER'S, captured with the detection that armed this
	// attempt. Re-reading it here would bind the claim to an epoch nothing
	// checked for completion: work that advanced it between the detection and
	// this call would be covered by a review nobody looked at it for.
	report, err := c.Reports.Render(ctx, targetKey)
	if err != nil {
		return CompletionClaim{}, fmt.Errorf("render completion report for %s: %w", targetKey, err)
	}
	if report == "" {
		// A review with nothing to read is a human asked to approve a blank
		// page.
		return CompletionClaim{}, fmt.Errorf("the completion report for %s is empty", targetKey)
	}

	submission := SubmissionKey(targetKey, epoch)
	claim := CompletionClaim{
		TargetKey: targetKey, State: CompletionSubmitting, Epoch: epoch,
		SubmissionKey: submission, ReportKey: ReportKey(submission),
		PendingReport: report, SubtreeRevision: subtreeRevision,
		Watermark: watermark,
	}
	if err := c.Claims.Upsert(ctx, claim); err != nil {
		return CompletionClaim{}, fmt.Errorf("record submitting claim for %s: %w", targetKey, err)
	}
	// Before the external calls, and before anything can read the feed again:
	// everything up to this watermark is accounted for by the detection that
	// armed the attempt, the build's own completion events included.
	if c.Work != nil {
		if err := c.Work.SyncTo(ctx, watermark); err != nil {
			return CompletionClaim{}, err
		}
	}
	return c.finish(ctx, claim, projectID, epicID)
}

// finish performs the calls the write-ahead row promised.
//
// Shared with recovery so a replay takes exactly the same path as a fresh
// submission — two implementations of one protocol would leave only one
// tested, and this is the protocol whose crash windows the spec enumerates.
func (c Claimer) finish(
	ctx context.Context, claim CompletionClaim, projectID, epicID string,
) (CompletionClaim, error) {
	// The keyed doc mutation, replayed UNCONDITIONALLY rather than skipped
	// when a version is already recorded. The key is what makes it safe —
	// sutra returns the original version for a request that landed and
	// performs it otherwise — and trusting kriya's own record instead would
	// mean a row written before the call ever landed could never self-heal.
	doc, version, err := c.Documents.Create(ctx, projectID,
		"Build completion report", epicID, claim.PendingReport, claim.ReportKey)
	if err != nil {
		return CompletionClaim{}, fmt.Errorf(
			"create completion report for %s: %w", claim.TargetKey, err)
	}
	if version == "" {
		return CompletionClaim{}, fmt.Errorf(
			"the completion report for %s has no version to review", claim.TargetKey)
	}
	claim.ReportDoc, claim.ReportVersion = doc, version
	// Recorded BEFORE the review call: a crash between them must recover the
	// exact version, not append another.
	if err := c.Claims.Upsert(ctx, claim); err != nil {
		return CompletionClaim{}, fmt.Errorf("record completion report: %w", err)
	}

	id, revision, err := c.Reviews.Create(ctx, epicID,
		"Build complete", claim.ReportVersion, claim.SubmissionKey)
	if err != nil {
		return CompletionClaim{}, fmt.Errorf(
			"submit completion review for %s: %w", claim.TargetKey, err)
	}
	claim.ReviewID, claim.ReviewRevision = id, revision
	claim.State = CompletionSubmitted
	if err := c.Claims.Upsert(ctx, claim); err != nil {
		return CompletionClaim{}, fmt.Errorf("record submitted completion review: %w", err)
	}
	return claim, nil
}

// Recover replays completion submissions a crash left in flight.
//
// Under the PERSISTED keys and the persisted report, never re-rendered: a
// replay that regenerated the report could write different bytes under the
// same key, and one that re-derived the submission key after an epoch advance
// would open a second review for a claim that is already spent.
func (c Claimer) Recover(
	ctx context.Context, project func(CompletionClaim) (projectID, epicID string, err error),
) (int, error) {
	pending, err := c.Claims.Submitting(ctx)
	if err != nil {
		return 0, fmt.Errorf("list submitting claims: %w", err)
	}
	for _, claim := range pending {
		projectID, epicID, err := project(claim)
		if err != nil {
			return 0, err
		}
		if _, err := c.finish(ctx, claim, projectID, epicID); err != nil {
			return 0, err
		}
	}
	return len(pending), nil
}

// RecoverCloses replays epic closes a crash left in flight.
//
// Under the PERSISTED close key, so a close that landed replays to its
// original success. A claim whose epoch has since moved surfaces as
// ErrStaleClaim rather than stamping — the close is real, the stamp is not.
func (c Claimer) RecoverCloses(
	ctx context.Context, targetKey string, epic func(CompletionClaim) (string, error),
) (int, error) {
	pending, err := c.Claims.Closing(ctx)
	if err != nil {
		return 0, fmt.Errorf("list closing claims: %w", err)
	}
	var stale error
	var replayed int
	for _, claim := range pending {
		if targetKey != "" && claim.TargetKey != targetKey {
			// Another target's close. Startup consumes work events for the
			// target it was invoked for and no other, so replaying a foreign
			// close here would do it with an epoch nothing had refreshed:
			// its cached conflict comes back and recovery fails for a build
			// this command is not even about.
			continue
		}
		replayed++
		// The epoch FIRST, before sutra. A subtree or reversed-approval
		// conflict is cached under the persisted key, so replaying the call
		// would return that conflict on every startup — and since only a
		// stale claim is tolerated, recovery would fail forever.
		current, err := c.Epochs.Current(ctx, claim.TargetKey)
		if err != nil {
			return 0, err
		}
		if current != claim.Epoch {
			// The close was ISSUED — the row reached closing before the call
			// — and whether it landed is unknown from here. A reopen is owed
			// until something resolves it.
			if err := c.settleStale(ctx, claim, true); err != nil {
				return 0, err
			}
			stale = fmt.Errorf("%s: %w", claim.TargetKey, ErrStaleClaim)
			continue
		}
		epicID, err := epic(claim)
		if err != nil {
			return 0, err
		}
		if _, err := c.finishClose(ctx, claim, epicID); err != nil {
			if errors.Is(err, ErrStaleClaim) {
				// The epoch moved between the check and the stamp. The close
				// landed; a compensating reopen is what returns the target to
				// work, and stopping here would leave every other target's
				// close unrecovered.
				stale = err
				continue
			}
			return 0, err
		}
	}
	return replayed, stale
}

// settleStale parks a claim whose epoch has moved past it.
//
// Terminal for THIS attempt. A fresh attempt at the new epoch is what comes
// next, under a key that names it — and leaving this one in closing would have
// every startup retry a close that can never stamp.
//
// owed records that the close's outcome is UNRESOLVED. A close that landed
// before an ambiguous failure leaves the epic closed over a newer epoch with
// nothing reopening it, and settling silently would turn "we do not know" into
// "nothing happened". The compensating reopen is the operator's to drive until
// the compensation lifecycle lands.
func (c Claimer) settleStale(ctx context.Context, claim CompletionClaim, owed bool) error {
	claim.State = CompletionStale
	claim.ReopenOwed = owed
	if err := c.Claims.Upsert(ctx, claim); err != nil {
		return fmt.Errorf("record stale claim for %s: %w", claim.TargetKey, err)
	}
	return nil
}

// ErrNotArmed reports a completion attempt on a target that may not have one.
var ErrNotArmed = errors.New("completion is not armed for this target")

// ErrStaleClaim reports an approval spent against a claim the world moved past.
//
// A distinct error because the operator acts on it: the approval was real, the
// human meant it, and it covered a build that is no longer this one. Nothing is
// stamped, and a fresh attempt with fresh approval is required.
var ErrStaleClaim = errors.New("the completion claim's epoch has moved")

// Close completes a target's epic on its approval, then stamps the target.
//
// Write-ahead: the closing state, the approval event and the close key are
// persisted BEFORE the call, so a crash after the close landed replays under
// the same key — sutra returns the original success rather than a close-used
// conflict, since this very key stamped it.
//
// The stamp that follows is a compare-and-swap on the epoch. sutra's gate
// proves the EPIC was closable; the CAS proves the claim is still the current
// one. Both are needed: a reopen between them is refused by sutra, and an
// advance between them is refused by the CAS.
func (c Claimer) Close(
	ctx context.Context, targetKey, epicID, approvalEvent string,
) (CompletionClaim, error) {
	claim, found, err := c.Claims.Find(ctx, targetKey)
	if err != nil {
		return CompletionClaim{}, fmt.Errorf("read completion claim for %s: %w", targetKey, err)
	}
	if !found || claim.ReviewID == "" {
		return CompletionClaim{}, fmt.Errorf("%s has no completion review to close against", targetKey)
	}
	if approvalEvent == "" {
		// The close key embeds it. Without one, a reapproval after a conflict
		// would replay the cached conflict forever.
		return CompletionClaim{}, fmt.Errorf("the approval for %s names no verdict event", targetKey)
	}

	if claim.State != CompletionClosing || claim.ApprovalEvent != approvalEvent {
		claim.ApprovalEvent = approvalEvent
		claim.CloseKey = CloseKey(targetKey, claim.Epoch, approvalEvent)
		claim.State = CompletionClosing
		if err := c.Claims.Upsert(ctx, claim); err != nil {
			return CompletionClaim{}, fmt.Errorf("record closing claim: %w", err)
		}
	}
	return c.finishClose(ctx, claim, epicID)
}

// finishClose performs the close the write-ahead row promised, and stamps.
//
// Shared with recovery, so a replay takes the same path as a first attempt.
func (c Claimer) finishClose(
	ctx context.Context, claim CompletionClaim, epicID string,
) (CompletionClaim, error) {
	if err := c.Epics.Close(ctx, epicID, claim.ReviewID, claim.ReviewRevision,
		claim.ApprovalEvent, claim.SubtreeRevision, claim.CloseKey); err != nil {
		return CompletionClaim{}, fmt.Errorf("close epic %s: %w", epicID, err)
	}
	// The epic is closed. The stamp is a SEPARATE fence: sutra proved the epic
	// was closable, and this proves the claim is still the current one.
	stamped, err := c.Epochs.Stamp(ctx, claim.TargetKey, claim.Epoch)
	if err != nil {
		return CompletionClaim{}, err
	}
	if !stamped {
		// The epoch advanced while the approval was in flight. The close
		// landed — sutra's gate was satisfied at the time — and a
		// compensating reopen is what returns the target to work. The
		// operator hears about it; nothing is stamped, and the claim parks
		// terminally so recovery does not retry it forever.
		// The close LANDED — sutra returned success a moment ago — so the
		// epic is closed over an epoch this claim no longer owns, and the
		// compensating reopen is owed rather than merely possible.
		if err := c.settleStale(ctx, claim, true); err != nil {
			return CompletionClaim{}, err
		}
		return claim, fmt.Errorf("%s: %w", claim.TargetKey, ErrStaleClaim)
	}
	claim.State = CompletionComplete
	if err := c.Claims.Upsert(ctx, claim); err != nil {
		return CompletionClaim{}, fmt.Errorf("record completed claim: %w", err)
	}
	return claim, nil
}
