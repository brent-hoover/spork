package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
)

// Completion states a run passes through after its merge lands.
const (
	CompleteNone       = "none"
	CompleteCompleting = "completing"
	CompleteClosed     = "closed"
)

// Tickets closes a ticket through the tracker's own gate.
type Tickets interface {
	// Complete transitions the ticket, naming the review, its approved
	// revision and its approval's verdict event. The key makes a replay
	// return the original success rather than a close-used conflict.
	Complete(ctx context.Context, issue, review string, revision int,
		verdictEvent, key string) error
}

// Branches answers where a branch stands.
type Branches interface {
	// Head resolves a branch's current commit.
	Head(ctx context.Context, branch string) (string, error)
}

// Sessions ends the run's dev session.
type Sessions interface {
	// Terminate ends the session that has been writing to the branch. Called
	// BEFORE the head check, because the session is the branch's only
	// in-protocol writer and a check racing it proves nothing.
	Terminate(ctx context.Context, run string) error
}

// Completer closes a merged run's ticket.
type Completer struct {
	Store    Store
	Tickets  Tickets
	Branches Branches
	Sessions Sessions
	Actor    string
}

// completionKey is scoped to the review, its approved revision, and the
// approval's verdict event.
//
// The verdict event is in it so that a close which conflicted because its
// approval was reversed does not replay its cached conflict when the review is
// reapproved at the same revision: the new approval yields a FRESH key.
func completionKey(review string, revision int, verdictEvent string) string {
	sum := sha256.Sum256([]byte(
		"kriya-ticket-close:" + review + ":" + strconv.Itoa(revision) + ":" + verdictEvent))
	return hex.EncodeToString(sum[:])
}

// Completion is what closing a ticket needs beyond the run's own fields.
type Completion struct {
	Issue  string
	Branch string
	// Merged is the APPROVED commit — the one the merge landed. The run's
	// branch must still point at it for the ticket to close; anything past it
	// is work nothing reviewed.
	Merged string
}

// ErrHeadAdvanced reports that the branch moved before the completion check.
//
// Its own error because the caller acts on it: the run returns to the pair
// loop for the unreviewed commits rather than parking.
var ErrHeadAdvanced = fmt.Errorf("the branch advanced before completion")

// Complete closes a merged run's ticket.
//
// The dev session is terminated FIRST. It is the branch's only in-protocol
// writer, so checking the head while it may still be committing would prove
// nothing about the moment after.
func (c Completer) Complete(ctx context.Context, run BuildRun, cm Completion) (BuildRun, error) {
	if run.ReviewID == "" {
		return BuildRun{}, fmt.Errorf("run %s has no review to close against", run.ID)
	}
	if err := c.Sessions.Terminate(ctx, run.ID); err != nil {
		return BuildRun{}, fmt.Errorf("terminate session for %s: %w", run.ID, err)
	}
	head, err := c.Branches.Head(ctx, cm.Branch)
	if err != nil {
		return BuildRun{}, fmt.Errorf("read %s: %w", cm.Branch, err)
	}
	if head != cm.Merged {
		// Unreviewed commits landed. The ticket is NOT completed and the run
		// goes back to the pair loop for them; the approved review is never
		// resubmitted, so a fresh one covers the new head.
		return run, fmt.Errorf("%w: %s is at %s, not the merged %s",
			ErrHeadAdvanced, cm.Branch, head, cm.Merged)
	}

	run.CloseKey = completionKey(run.ReviewID, run.ReviewRevision, run.ReviewVerdictEvent)
	run.CompletedHead = head
	run.CompletionState = CompleteCompleting
	if err := c.Store.Upsert(ctx, run); err != nil {
		return BuildRun{}, fmt.Errorf("record completing run: %w", err)
	}
	return c.finish(ctx, run, cm)
}

// finish performs the close the write-ahead row promised.
func (c Completer) finish(ctx context.Context, run BuildRun, cm Completion) (BuildRun, error) {
	err := c.Tickets.Complete(ctx, cm.Issue, run.ReviewID, run.ReviewRevision,
		run.ReviewVerdictEvent, run.CloseKey)
	if err != nil {
		return BuildRun{}, fmt.Errorf("close ticket for %s: %w", run.ID, err)
	}
	run.CompletionState = CompleteClosed
	// The RUN closes too, not only its completion marker. A run left in merged
	// with a closed ticket is one the table would try to complete again, and
	// one nothing reports as finished.
	run.State = StateClosed
	if err := c.Store.Upsert(ctx, run); err != nil {
		return BuildRun{}, fmt.Errorf("record closed run: %w", err)
	}
	return run, nil
}

// RecoverCompletions replays closes a crash left in flight.
//
// Under the PERSISTED key: sutra returns the original success for a close that
// landed — never a close-used conflict — so the run reaches closed either way.
func (c Completer) RecoverCompletions(ctx context.Context, cm func(BuildRun) Completion) (int, error) {
	pending, err := c.Store.Completing(ctx)
	if err != nil {
		return 0, fmt.Errorf("list completing runs: %w", err)
	}
	for _, run := range pending {
		if _, err := c.finish(ctx, run, cm(run)); err != nil {
			return 0, err
		}
	}
	return len(pending), nil
}

// Advanced reports whether a closed run's branch moved after completion.
//
// Checked against the RECORDED head: an out-of-band commit landing on a closed
// run's branch is work nothing reviewed, and it surfaces to the operator with
// the tracker's reopen path rather than being merged silently.
func (c Completer) Advanced(ctx context.Context, run BuildRun, branch string) (bool, error) {
	if run.CompletionState != CompleteClosed || run.CompletedHead == "" {
		return false, nil
	}
	head, err := c.Branches.Head(ctx, branch)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", branch, err)
	}
	return head != run.CompletedHead, nil
}
