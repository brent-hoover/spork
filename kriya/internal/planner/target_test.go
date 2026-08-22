package planner_test

import (
	"context"
	"errors"
	"testing"

	"kriya/internal/planner"
)

type memTargets struct {
	rows map[string]planner.BuildTarget
}

func newMemTargets() *memTargets { return &memTargets{rows: map[string]planner.BuildTarget{}} }

func (t *memTargets) Upsert(_ context.Context, b planner.BuildTarget) error {
	t.rows[b.TargetKey] = b
	return nil
}

func (t *memTargets) Find(_ context.Context, key string) (planner.BuildTarget, bool, error) {
	b, ok := t.rows[key]
	return b, ok, nil
}

// countingTracker records every call so a test can assert how many times the
// tracker was actually asked to create something.
type countingTracker struct {
	projects, issues int
	failIssue        error
	// keys records the idempotency keys presented, which is what makes a
	// replay safe on sutra's side.
	keys []string
}

func (c *countingTracker) CreateProject(_ context.Context, _, _, _, idem string) (string, error) {
	c.projects++
	c.keys = append(c.keys, idem)
	return "project-1", nil
}

func (c *countingTracker) CreateIssue(_ context.Context, _, _, _, _, idem string) (string, error) {
	c.issues++
	c.keys = append(c.keys, idem)
	if c.failIssue != nil {
		return "", c.failIssue
	}
	return "epic-1", nil
}

func epicIntaker(store *memTargets, tr planner.Tracker) planner.Intaker {
	return planner.Intaker{Targets: store, Tracker: tr}
}

func TestTheEpicIsCreatedOnce(t *testing.T) {
	store, tr := newMemTargets(), &countingTracker{}
	in := epicIntaker(store, tr)
	for range 3 {
		if _, err := in.EnsureEpic(context.Background(), "/spec", "hash1234567890", "SHORT", "shorty", "actor"); err != nil {
			t.Fatalf("ensure: %v", err)
		}
	}
	if tr.projects != 1 || tr.issues != 1 {
		t.Errorf("AC-decompose-epic: exactly once, got %d projects and %d epics", tr.projects, tr.issues)
	}
}

func TestTheTargetIsWrittenBeforeTheEpicIsCreated(t *testing.T) {
	// Write-ahead: a crash between the call and the outcome write must leave a
	// row to replay from. Failing the epic call proves the row exists anyway.
	store, tr := newMemTargets(), &countingTracker{failIssue: errors.New("sutra unreachable")}
	in := epicIntaker(store, tr)
	if _, err := in.EnsureEpic(context.Background(), "/spec", "hash1234567890", "SHORT", "shorty", "actor"); err == nil {
		t.Fatal("expected the epic creation to fail")
	}
	row, found, _ := store.Find(context.Background(), "/spec")
	if !found {
		t.Fatal("no build target row survived the failure; there is nothing to replay from")
	}
	if row.EpicState != planner.EpicPending {
		t.Errorf("state should still be pending, got %q", row.EpicState)
	}
	if row.ProjectID == "" {
		t.Error("the project id was not recorded, so a replay would create a second project")
	}
}

func TestAReplayReusesTheRecordedRow(t *testing.T) {
	// "a recovered replay reuses the recorded row" — after a failure the
	// second attempt must not create another project.
	store := newMemTargets()
	failing := &countingTracker{failIssue: errors.New("boom")}
	if _, err := epicIntaker(store, failing).EnsureEpic(
		context.Background(), "/spec", "hash1234567890", "SHORT", "shorty", "actor"); err == nil {
		t.Fatal("expected failure")
	}
	recovered := &countingTracker{}
	if _, err := epicIntaker(store, recovered).EnsureEpic(
		context.Background(), "/spec", "hash1234567890", "SHORT", "shorty", "actor"); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if recovered.projects != 0 {
		t.Errorf("the replay created %d projects; it must reuse the recorded one", recovered.projects)
	}
	if recovered.issues != 1 {
		t.Errorf("the replay should create the epic exactly once, got %d", recovered.issues)
	}
}

func keysFor(t *testing.T, targetKey, specHash, actor string) []string {
	t.Helper()
	tr := &countingTracker{}
	if _, err := epicIntaker(newMemTargets(), tr).EnsureEpic(
		context.Background(), targetKey, specHash, "SHORT", "shorty", actor); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	return tr.keys
}

func TestIdempotencyKeysAreStableForAnIdenticalRequest(t *testing.T) {
	// Stable, so a crash replay presents the SAME key and sutra returns the
	// original rather than acting twice. A random key would make every replay
	// a new mutation.
	first := keysFor(t, "/spec", "hash1234567890", "actor-1")
	second := keysFor(t, "/spec", "hash1234567890", "actor-1")
	if len(first) != 2 {
		t.Fatalf("expected two keyed calls, got %v", first)
	}
	for i := range first {
		if first[i] != second[i] {
			t.Errorf("key %d changed between identical runs: %q then %q", i, first[i], second[i])
		}
	}
}

func TestTheKeyChangesWhenTheRequestChanges(t *testing.T) {
	// sutra settles a REJECTED request under its key, deliberately, so a
	// different request cannot reuse a key and mutate. Keying on the target
	// alone therefore poisoned the key whenever the actor changed: the retry
	// was a genuine key reuse and sutra replayed the original rejection
	// forever. Every input that shapes the request must reach the key.
	base := keysFor(t, "/spec", "hash1234567890", "actor-1")
	for name, got := range map[string][]string{
		"actor":     keysFor(t, "/spec", "hash1234567890", "actor-2"),
		"spec hash": keysFor(t, "/spec", "otherhash12345", "actor-1"),
		"target":    keysFor(t, "/other", "hash1234567890", "actor-1"),
	} {
		for i := range base {
			if base[i] == got[i] {
				t.Errorf("a different %s produced the same key %q", name, got[i])
			}
		}
	}
}

func TestTheTwoStepsDoNotShareAKey(t *testing.T) {
	// The project and the epic are separate mutations; one key for both would
	// make the second replay the first.
	keys := keysFor(t, "/spec", "hash1234567890", "actor-1")
	if keys[0] == keys[1] {
		t.Errorf("project and epic share the key %q", keys[0])
	}
}
