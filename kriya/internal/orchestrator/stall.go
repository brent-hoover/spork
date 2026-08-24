package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"kriya/internal/clock"
)

// Stall lifecycle states.
const (
	StallOpen     = "open"
	StallResolved = "resolved"
)

// Stall is a build that can neither proceed nor finish.
//
// Nothing workable, nothing in flight, and the epic cannot close. Durable
// because the alternative is a process that spins or, worse, one that declares
// the build done — and because the operator inbox is a list of rows, not a log
// somebody has to be watching at the right moment.
type Stall struct {
	// Key is deterministic from (target, completion epoch). Detection UPSERTS
	// on it, so a poll, a second detector and a restart all converge on one
	// row rather than filling the inbox with the same condition.
	Key       string
	TargetKey string
	// Cause is what is holding the build back, in the words of whatever
	// detected it. A stall without one tells the operator only that something
	// is wrong.
	Cause    string
	State    string
	Created  time.Time
	Resolved time.Time
}

// StallStore persists stalls.
//
// Insert and Resolve rather than one upsert, because they are different acts:
// re-detection must not rewrite the cause an operator is reading, and
// resolution must not blank it. One method doing both would have to guess.
type StallStore interface {
	// Insert records a stall if its key is absent, and REOPENS a resolved row
	// under the same key — a condition that came back is the same condition,
	// not a new one. An open row is left exactly as it was created.
	Insert(ctx context.Context, s Stall) error
	// Resolve stamps a stall resolved, leaving everything else standing.
	Resolve(ctx context.Context, key string, at time.Time) error
	Open(ctx context.Context) ([]Stall, error)
}

// Stalls records and resolves build stalls.
type Stalls struct {
	Store StallStore
	Now   clock.Clock
}

// StallKey is deterministic from the target and its completion epoch.
//
// The EPOCH is in it because a stall is about a moment in the target's life: a
// supersession or a reopen makes the same-looking condition a different one,
// and reusing the row would hide it behind an earlier resolution.
func StallKey(targetKey string, epoch int) string {
	sum := sha256.Sum256([]byte("kriya-stall:" + targetKey + ":" + strconv.Itoa(epoch)))
	return hex.EncodeToString(sum[:])
}

// Record notes that a target's build can neither proceed nor finish.
func (s Stalls) Record(ctx context.Context, targetKey string, epoch int, cause string) (Stall, error) {
	if cause == "" {
		// "a durable Stall row records the condition AND ITS CAUSE". A blank
		// one is a row the operator can do nothing with.
		return Stall{}, errors.New("a stall needs a cause")
	}
	stall := Stall{
		Key: StallKey(targetKey, epoch), TargetKey: targetKey, Cause: cause,
		State: StallOpen, Created: s.Now.Now(),
	}
	if err := s.Store.Insert(ctx, stall); err != nil {
		return Stall{}, fmt.Errorf("record stall for %s: %w", targetKey, err)
	}
	return stall, nil
}

// Resolve stamps a stall rather than deleting it.
//
// The history is the point: an operator asking whether this has happened
// before gets an answer, and a deleted row cannot give one.
func (s Stalls) Resolve(ctx context.Context, key string) error {
	if err := s.Store.Resolve(ctx, key, s.Now.Now()); err != nil {
		return fmt.Errorf("resolve stall %s: %w", key, err)
	}
	return nil
}
