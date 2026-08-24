package planner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

// Causes an epoch advance can have.
//
// The cause is not decoration: it selects how recovery reconciles the advance.
// A ticket-reopen may already have been cascaded by sutra and need no external
// call; a supersession must issue a keyed reopen only if a close ever ran.
const (
	CauseSupersession       = "supersession"
	CauseTicketReopen       = "ticket-reopen"
	CauseDeferredActivation = "deferred-activation"
	CauseSubtreeAttachment  = "subtree-attachment"
	CauseDetachment         = "detachment"
	CauseNewWork            = "new-work"
	CauseCompensation       = "compensation"
)

// declaredCauses is the closed set. A cause outside it would strand the row:
// recovery reads this field to decide what to reconcile.
var declaredCauses = map[string]bool{
	CauseSupersession: true, CauseTicketReopen: true, CauseDeferredActivation: true,
	CauseSubtreeAttachment: true, CauseDetachment: true, CauseNewWork: true,
	CauseCompensation: true,
}

// CompletionAdvance is an insert-once record of one epoch advance.
//
// INSERT-ONCE is the whole mechanism. The key carries the advance's identity —
// the consumed event, or the local operation that caused it — so a replay in
// any order collides and advances nothing: every event is consumed at most
// once, a valid completion is never cleared twice, and the cause an explicit
// reopen needs to recover is never overwritten by a later replay naming
// something else.
type CompletionAdvance struct {
	Key       string
	TargetKey string
	Cause     string
	// Event is the consumed event id, for event-consumption advances. The key
	// derives from it.
	Event string
	// LocalSource is the operation's own immutable identity, for advances that
	// consume no event: a supersession's new PlanHead generation, or a stale
	// close's operation key.
	LocalSource string
}

// AdvanceStore persists advances and the epoch they move.
//
// Insert reports whether the row was NEW. The counter moves in the same
// transaction, so an advance that collided cannot have moved it — which is
// what makes a replay a true no-op rather than a race.
//
// InsertWith binds a local operation's PRIMARY MUTATION to that same
// transaction. A supersession's replacement CAS and its epoch advance are one
// fact: committed separately, a crash between them leaves either an unadvanced
// epoch over replaced work, or an advance for a replacement that never
// happened. The callback runs inside the transaction and its error rolls
// everything back.
type AdvanceStore interface {
	Insert(ctx context.Context, a CompletionAdvance) (fresh bool, err error)
	InsertWith(ctx context.Context, a CompletionAdvance, mutate func(Tx) error) (bool, error)
	Epoch(ctx context.Context, targetKey string) (int, error)
	// Stamp records completion only while the target's epoch still equals the
	// claim's. It reports whether it landed.
	Stamp(ctx context.Context, targetKey string, claimEpoch int) (bool, error)
}

// Tx is the handle a bound mutation writes through.
//
// Narrow deliberately: what a caller needs inside the advance's transaction is
// to execute its own statements, not to commit or roll back — the advance owns
// that, because the two are one fact.
type Tx interface {
	ExecContext(ctx context.Context, query string, args ...any) (Result, error)
}

// Result is the part of a SQL result a bound mutation reads.
type Result interface {
	RowsAffected() (int64, error)
}

// Epochs advances a target's completion epoch and stamps its completion.
type Epochs struct{ Store AdvanceStore }

// EventAdvanceKey is deterministic per (target, consumed event).
func EventAdvanceKey(targetKey, event string) string {
	return advanceKey("kriya-advance-event:" + targetKey + ":" + event)
}

// LocalAdvanceKey is deterministic per (target, cause, local source).
//
// The cause is in it because two different local operations on one target —
// a supersession and a compensation — are different advances even if their
// sources ever collided.
func LocalAdvanceKey(targetKey, cause, source string) string {
	return advanceKey("kriya-advance-local:" + targetKey + ":" + cause + ":" + source)
}

func advanceKey(material string) string {
	sum := sha256.Sum256([]byte(material))
	return hex.EncodeToString(sum[:])
}

// OnEvent advances the epoch for a consumed tracker event.
//
// Reports whether it moved. False is the ordinary answer for a replay, not a
// failure: the point of the key is that consuming twice is safe.
func (e Epochs) OnEvent(ctx context.Context, targetKey, cause, event string) (bool, error) {
	if event == "" {
		// The key IS the event. Without one there is nothing to collide on,
		// and every replay would advance again.
		return false, errors.New("an event-consumption advance needs its event")
	}
	return e.insert(ctx, CompletionAdvance{
		Key: EventAdvanceKey(targetKey, event), TargetKey: targetKey,
		Cause: cause, Event: event,
	})
}

// OnLocal advances the epoch for an operation that consumed no event.
//
// The source is the operation's own immutable identity — a supersession's new
// PlanHead generation, a compensating close's operation key — so a crash
// replay of that operation collides rather than advancing twice.
func (e Epochs) OnLocal(ctx context.Context, targetKey, cause, source string) (bool, error) {
	return e.OnLocalWith(ctx, targetKey, cause, source, nil)
}

// OnLocalWith advances the epoch and performs the operation's own mutation in
// ONE transaction.
//
// They are one fact. Committed separately, a crash between them leaves either
// an unadvanced epoch over work that was replaced, or an advance for a
// replacement that never happened — and the second is worse, because the epoch
// cannot be walked back.
//
// The mutation runs only when the advance is FRESH: a replay has already done
// it, and doing it again is the double-application the key exists to prevent.
func (e Epochs) OnLocalWith(
	ctx context.Context, targetKey, cause, source string, mutate func(Tx) error,
) (bool, error) {
	if source == "" {
		return false, errors.New("a local-operation advance needs its source")
	}
	a := CompletionAdvance{
		Key: LocalAdvanceKey(targetKey, cause, source), TargetKey: targetKey,
		Cause: cause, LocalSource: source,
	}
	if !declaredCauses[a.Cause] {
		return false, fmt.Errorf("%q is not a declared advance cause", a.Cause)
	}
	fresh, err := e.Store.InsertWith(ctx, a, mutate)
	if err != nil {
		return false, fmt.Errorf("record %s advance for %s: %w", a.Cause, a.TargetKey, err)
	}
	return fresh, nil
}

func (e Epochs) insert(ctx context.Context, a CompletionAdvance) (bool, error) {
	if !declaredCauses[a.Cause] {
		return false, fmt.Errorf("%q is not a declared advance cause", a.Cause)
	}
	fresh, err := e.Store.Insert(ctx, a)
	if err != nil {
		// An advance nobody recorded is a stamp that stays valid over work
		// that came back.
		return false, fmt.Errorf("record %s advance for %s: %w", a.Cause, a.TargetKey, err)
	}
	return fresh, nil
}

// Stamp records a target complete, under a compare-and-swap on its epoch.
//
// The CAS is the fence a human's approval is spent through: the claim named an
// epoch, and anything that advanced it since — a reopen, a supersession, a
// creation — means the approval was for work that is no longer all of it.
func (e Epochs) Stamp(ctx context.Context, targetKey string, claimEpoch int) (bool, error) {
	stamped, err := e.Store.Stamp(ctx, targetKey, claimEpoch)
	if err != nil {
		return false, fmt.Errorf("stamp completion for %s: %w", targetKey, err)
	}
	return stamped, nil
}

// Current reads a target's completion epoch.
func (e Epochs) Current(ctx context.Context, targetKey string) (int, error) {
	epoch, err := e.Store.Epoch(ctx, targetKey)
	if err != nil {
		return 0, fmt.Errorf("read completion epoch for %s: %w", targetKey, err)
	}
	return epoch, nil
}
