package planner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// Epic states, mirroring ENT-build-target.epic_state.
const (
	EpicPending = "pending"
	EpicCreated = "created"
)

// BuildTarget is the tracker-side identity of a spec kriya builds.
//
// Only the epic half lives here. completion_state and CompletionAdvance are
// a completion concern and land in M4; the epic is written at decomposition,
// which is why it is here now.
type BuildTarget struct {
	TargetKey string
	SpecHash  string
	ProjectID string
	EpicID    string
	EpicState string
	// ProjectKey, Name and Actor are persisted because RECOVERY MUST REPLAY
	// THE SAME REQUEST. The idempotency key is derived from the inputs that
	// shape the call, so a recovery that reconstructed them differently —
	// a different actor, say — would present a different key and create a
	// duplicate instead of replaying. A write-ahead row that cannot rebuild
	// its own call is not write-ahead.
	ProjectKey string
	Name       string
	Actor      string
}

// TargetStore persists build targets.
type TargetStore interface {
	Upsert(ctx context.Context, t BuildTarget) error
	Find(ctx context.Context, targetKey string) (BuildTarget, bool, error)
	// Pending lists targets a crash left mid-creation.
	Pending(ctx context.Context) ([]BuildTarget, error)
}

// Tracker is the slice of the issue tracker planner needs.
//
// An interface planner declares, so planner does not depend on the concrete
// client and tests can drive it without a server.
type Tracker interface {
	CreateProject(ctx context.Context, key, name, actor, idempotencyKey string) (projectID string, err error)
	CreateIssue(ctx context.Context, projectID, title, body, actor, idempotencyKey string) (issueID string, err error)
	AddRelation(ctx context.Context, issueID, kind, to, actor, idempotencyKey string) error
}

// idempotencyKey derives a stable key for one step of one target.
//
// It hashes RESOURCE IDENTITY — the step, the target, and the spec it was
// pinned from — and deliberately NOT the actor. Two attempts to create one
// project are the same operation regardless of who ran them, and the project
// is identified by its key, not by its creator.
//
// A previous version did include the actor, reasoning that sutra settles a
// rejected request under its key so a changed request must get a new key.
// That produced the opposite bug on the next real run: changing the actor
// made kriya present a fresh key and try to create a SECOND project with an
// already-taken key, which sutra correctly refused with a 409. The lesson is
// that the fix for "kriya poisoned a key with an invalid request" is to not
// send an invalid request — the actor is now validated at startup, before any
// mutation — rather than to make the key vary with everything.
func idempotencyKey(step, targetKey, specHash string) string {
	h := sha256.New()
	for _, part := range []string{step, targetKey, specHash} {
		_, _ = fmt.Fprintf(h, "%d:%s", len(part), part)
	}
	return step + "-" + hex.EncodeToString(h.Sum(nil))[:32]
}

// EnsureEpic writes the BuildTarget ahead of creating the epic, then records
// the epic id when the call returns.
//
// Write-ahead, not write-after: AC-decompose-epic requires "an umbrella epic
// is created exactly once — a recovered replay reuses the recorded row". A
// crash between the call and the write must be replayable, and it is, because
// the row and its idempotency key exist before sutra is ever asked. The key
// is derived from the target rather than random, so the replay presents the
// same key and sutra returns the original epic instead of making a second.
func (i Intaker) EnsureEpic(ctx context.Context, targetKey, specHash, projectKey, name, actor string) (BuildTarget, error) {
	existing, found, err := i.Targets.Find(ctx, targetKey)
	if err != nil {
		return BuildTarget{}, fmt.Errorf("find build target: %w", err)
	}
	if found && existing.EpicState == EpicCreated {
		return existing, nil
	}

	target := BuildTarget{
		TargetKey: targetKey, SpecHash: specHash, EpicState: EpicPending,
		ProjectKey: projectKey, Name: name, Actor: actor,
	}
	if found {
		// The EXISTING row wins, spec hash included. The idempotency keys were
		// derived from those values, so recomputing them from current inputs
		// would present a different key for a call that may already have
		// landed — creating a second project or epic rather than replaying the
		// first. A re-intake of a changed spec is a new plan, which is the
		// supersession path, not an in-place overwrite of a pending row.
		target = existing
	}
	if target.ProjectID == "" {
		if err := i.Targets.Upsert(ctx, target); err != nil {
			return BuildTarget{}, fmt.Errorf("write target ahead of project: %w", err)
		}
		// Every value comes from the TARGET, not the parameters: the row is the
		// record of what the original call sent, and the key must match it.
		projectID, err := i.Tracker.CreateProject(ctx, target.ProjectKey, target.Name, target.Actor,
			idempotencyKey("project", target.TargetKey, target.SpecHash))
		if err != nil {
			return BuildTarget{}, fmt.Errorf("create project: %w", err)
		}
		target.ProjectID = projectID
		if err := i.Targets.Upsert(ctx, target); err != nil {
			return BuildTarget{}, fmt.Errorf("record project: %w", err)
		}
	}

	epicID, err := i.Tracker.CreateIssue(ctx, target.ProjectID,
		"Build "+target.Name, "Umbrella epic for spec "+target.SpecHash[:12], target.Actor,
		idempotencyKey("epic", target.TargetKey, target.SpecHash))
	if err != nil {
		return BuildTarget{}, fmt.Errorf("create epic: %w", err)
	}
	target.EpicID = epicID
	target.EpicState = EpicCreated
	if err := i.Targets.Upsert(ctx, target); err != nil {
		return BuildTarget{}, fmt.Errorf("record epic: %w", err)
	}
	return target, nil
}

// RecoverTargets completes any BuildTarget left mid-creation by a crash.
//
// The write-ahead row is the whole point: a target stamped pending has a
// project or an epic that may or may not exist on the tracker side, and the
// derived idempotency key makes finding out safe — replaying the call returns
// the original if it landed and creates it if it did not. Recovery therefore
// finishes the job rather than guessing what happened.
func (i Intaker) RecoverTargets(ctx context.Context) (int, error) {
	pending, err := i.Targets.Pending(ctx)
	if err != nil {
		return 0, fmt.Errorf("list pending targets: %w", err)
	}
	for _, t := range pending {
		// t.Actor, not a caller-supplied one: the key was derived from the
		// actor the original call used, and replaying under a different one
		// would create a duplicate rather than reuse the original.
		if _, err := i.EnsureEpic(ctx, t.TargetKey, t.SpecHash, t.ProjectKey, t.Name, t.Actor); err != nil {
			return 0, fmt.Errorf("recover target %s: %w", t.TargetKey, err)
		}
	}
	return len(pending), nil
}
