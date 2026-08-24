package planner_test

import (
	"context"
	"errors"
	"fmt"
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
	projects, issues, relations int
	failIssue                   error
	// keys records the idempotency keys presented, which is what makes a
	// replay safe on sutra's side.
	keys []string
	// assigned records which identity each ticket went to. sutra's pop offers
	// only issues assigned to the popping identity.
	assigned  map[string]string
	assignErr error
}

func (c *countingTracker) CreateProject(_ context.Context, _, _, _, idem string) (string, error) {
	c.projects++
	c.keys = append(c.keys, idem)
	return "project-1", nil
}

func (c *countingTracker) AddRelation(_ context.Context, _, _, _, _, idem string) error {
	c.relations++
	c.keys = append(c.keys, idem)
	return nil
}

func (c *countingTracker) CreateIssue(_ context.Context, _, _, _, _, idem string) (string, error) {
	c.issues++
	c.keys = append(c.keys, idem)
	if c.failIssue != nil {
		return "", c.failIssue
	}
	// Distinct ids, because two tickets are two issues: a fake handing back
	// one id would hide anything that keys work by issue.
	if c.issues == 1 {
		return "epic-1", nil
	}
	return fmt.Sprintf("issue-%d", c.issues), nil
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

func TestTheKeyTracksResourceIdentityNotTheActor(t *testing.T) {
	// The key hashes what identifies the RESOURCE — step, target, spec hash —
	// and deliberately not the actor. Two attempts to create one project are
	// the same operation whoever ran them, and a project is identified by its
	// key, not its creator.
	//
	// An earlier version DID include the actor, on the reasoning that sutra
	// settles a rejected request under its key so a changed request needs a
	// new one. That produced the opposite bug on the next real run: a changed
	// actor presented a fresh key and tried to create a SECOND project under
	// an already-taken key, which sutra refused with a 409. The cure for
	// "kriya poisoned a key with an invalid request" is to not send an invalid
	// request — the actor is validated at startup — not to vary the key with
	// everything.
	base := keysFor(t, "/spec", "hash1234567890", "actor-1")
	changedActor := keysFor(t, "/spec", "hash1234567890", "actor-2")
	for i := range base {
		if base[i] != changedActor[i] {
			t.Errorf("a different actor changed key %d: %q vs %q", i, base[i], changedActor[i])
		}
	}
	for name, got := range map[string][]string{
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

func (t *memTargets) Pending(_ context.Context) ([]planner.BuildTarget, error) {
	var out []planner.BuildTarget
	for _, b := range t.rows {
		if b.EpicState == planner.EpicPending {
			out = append(out, b)
		}
	}
	return out, nil
}

func TestRecoveryFinishesATargetLeftPending(t *testing.T) {
	// The crash window: the row is written, the epic call never returns.
	store := newMemTargets()
	failing := &countingTracker{failIssue: errors.New("crash")}
	if _, err := epicIntaker(store, failing).EnsureEpic(
		context.Background(), "/spec", "hash1234567890", "SHORT", "shorty", "actor-1"); err == nil {
		t.Fatal("expected the epic call to fail")
	}

	healthy := &countingTracker{}
	in := epicIntaker(store, healthy)
	n, err := in.RecoverTargets(context.Background())
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if n != 1 {
		t.Errorf("expected one target recovered, got %d", n)
	}
	row, _, _ := store.Find(context.Background(), "/spec")
	if row.EpicState != planner.EpicCreated {
		t.Errorf("the target is still %q after recovery", row.EpicState)
	}
	if healthy.projects != 0 {
		t.Error("recovery created a second project instead of reusing the recorded one")
	}
}

func TestRecoveryReplaysUnderTheOriginalActor(t *testing.T) {
	// The key was derived from the actor the original call used. Replaying
	// under a different one presents a different key and creates a duplicate
	// rather than reusing the original — so the row has to carry it.
	store := newMemTargets()
	failing := &countingTracker{failIssue: errors.New("crash")}
	if _, err := epicIntaker(store, failing).EnsureEpic(
		context.Background(), "/spec", "hash1234567890", "SHORT", "shorty", "actor-1"); err == nil {
		t.Fatal("expected failure")
	}
	firstKeys := append([]string{}, failing.keys...)

	healthy := &countingTracker{}
	if _, err := epicIntaker(store, healthy).RecoverTargets(context.Background()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	// The epic key presented at recovery must match the one the original
	// attempt used.
	if len(healthy.keys) == 0 || healthy.keys[len(healthy.keys)-1] != firstKeys[len(firstKeys)-1] {
		t.Errorf("recovery presented %v, original attempt ended with %v", healthy.keys, firstKeys)
	}
}

func TestRecoveryDoesNotAdoptChangedInputs(t *testing.T) {
	// The keys were derived from the recorded values. Recomputing them from
	// current inputs would present a different key for a call that may already
	// have landed, creating a second project or epic rather than replaying the
	// first. A changed spec is a new plan, not an overwrite of a pending row.
	store := newMemTargets()
	failing := &countingTracker{failIssue: errors.New("crash")}
	if _, err := epicIntaker(store, failing).EnsureEpic(
		context.Background(), "/spec", "originalhash1234", "SHORT", "shorty", "actor-1"); err == nil {
		t.Fatal("expected failure")
	}
	originalKeys := append([]string{}, failing.keys...)

	// A later call naming a DIFFERENT spec hash must not change the pending
	// row's keys.
	healthy := &countingTracker{}
	if _, err := epicIntaker(store, healthy).EnsureEpic(
		context.Background(), "/spec", "a-completely-different-hash", "SHORT", "shorty", "actor-1"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if healthy.keys[len(healthy.keys)-1] != originalKeys[len(originalKeys)-1] {
		t.Errorf("the epic key changed with the spec hash: %q then %q",
			originalKeys[len(originalKeys)-1], healthy.keys[len(healthy.keys)-1])
	}
	row, _, _ := store.Find(context.Background(), "/spec")
	if row.SpecHash != "originalhash1234" {
		t.Errorf("the pending row adopted a new spec hash: %q", row.SpecHash)
	}
}

// AssignIssue puts a ticket on the popping identity's work stack. Without it
// the tracker's pop never offers it to anyone.
func (c *countingTracker) AssignIssue(_ context.Context, issue, assignee, _, _ string) error {
	if c.assignErr != nil {
		return c.assignErr
	}
	if c.assigned == nil {
		c.assigned = map[string]string{}
	}
	c.assigned[issue] = assignee
	return nil
}
