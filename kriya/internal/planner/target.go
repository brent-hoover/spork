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
}

// TargetStore persists build targets.
type TargetStore interface {
	Upsert(ctx context.Context, t BuildTarget) error
	Find(ctx context.Context, targetKey string) (BuildTarget, bool, error)
}

// Tracker is the slice of the issue tracker planner needs.
//
// An interface planner declares, so planner does not depend on the concrete
// client and tests can drive it without a server.
type Tracker interface {
	CreateProject(ctx context.Context, key, name, actor, idempotencyKey string) (projectID string, err error)
	CreateIssue(ctx context.Context, projectID, title, body, actor, idempotencyKey string) (issueID string, err error)
}

// idempotencyKey derives a stable key for one step of one target.
//
// It hashes every input that shapes the request, not just the target. An
// earlier version keyed on the target path alone, which broke on the first
// real run: sutra settles a rejected request under its key — deliberately, so
// a DIFFERENT request cannot reuse a key and mutate — so when the actor
// changed between runs, the second request was a genuine key reuse and sutra
// correctly replayed the original rejection forever. The key must therefore
// change when the request changes, and stay identical when it does not, which
// is what makes a crash replay safe and a corrected retry possible.
func idempotencyKey(step, targetKey, specHash, actor string) string {
	h := sha256.New()
	for _, part := range []string{step, targetKey, specHash, actor} {
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

	target := BuildTarget{TargetKey: targetKey, SpecHash: specHash, EpicState: EpicPending}
	if found {
		target = existing
		target.SpecHash = specHash
	}
	if target.ProjectID == "" {
		if err := i.Targets.Upsert(ctx, target); err != nil {
			return BuildTarget{}, fmt.Errorf("write target ahead of project: %w", err)
		}
		projectID, err := i.Tracker.CreateProject(ctx, projectKey, name, actor, idempotencyKey("project", targetKey, specHash, actor))
		if err != nil {
			return BuildTarget{}, fmt.Errorf("create project: %w", err)
		}
		target.ProjectID = projectID
		if err := i.Targets.Upsert(ctx, target); err != nil {
			return BuildTarget{}, fmt.Errorf("record project: %w", err)
		}
	}

	epicID, err := i.Tracker.CreateIssue(ctx, target.ProjectID,
		"Build "+name, "Umbrella epic for spec "+specHash[:12], actor, idempotencyKey("epic", targetKey, specHash, actor))
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
