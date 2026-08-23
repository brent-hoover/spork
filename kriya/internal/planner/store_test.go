package planner_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"kriya/internal/planner"
)

func sqlDB(t *testing.T, schemas ...string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, schema := range schemas {
		for _, stmt := range strings.Split(schema, ";") {
			if strings.TrimSpace(stmt) == "" {
				continue
			}
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("migrate: %v", err)
			}
		}
	}
	return db
}

func TestASnapshotSurvivesTheRoundTrip(t *testing.T) {
	s := planner.SQLSnapshots{DB: sqlDB(t, planner.Migration)}
	want := planner.Snapshot{
		Hash:    "hash-1",
		Content: map[string]string{"avspec.yaml": "body"},
		ResolvedCommands: map[string]map[string]string{
			"engine": {"test": "go test ./..."},
		},
		Created: time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC),
	}
	if err := s.Put(context.Background(), want); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := s.Get(context.Background(), "hash-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Content["avspec.yaml"] != "body" {
		t.Errorf("content came back %v", got.Content)
	}
	if got.ResolvedCommands["engine"]["test"] != "go test ./..." {
		t.Errorf("commands came back %v", got.ResolvedCommands)
	}
	if !got.Created.Equal(want.Created) {
		t.Errorf("created came back %s", got.Created)
	}
}

func TestPinningTheSameContentTwiceIsNotAnError(t *testing.T) {
	// Snapshots are content-addressed, so an identical hash is the same
	// snapshot and a re-intake of unchanged files must not fail.
	s := planner.SQLSnapshots{DB: sqlDB(t, planner.Migration)}
	snap := planner.Snapshot{Hash: "hash-1", Created: time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)}
	if err := s.Put(context.Background(), snap); err != nil {
		t.Fatalf("first put: %v", err)
	}
	if err := s.Put(context.Background(), snap); err != nil {
		t.Fatalf("second put: %v", err)
	}
	n, err := s.Count(context.Background())
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("pinned %d snapshots for one hash", n)
	}
}

func TestAnUnpinnedHashIsAnError(t *testing.T) {
	// Building against a snapshot nothing pinned would build against a spec
	// that was never admitted.
	s := planner.SQLSnapshots{DB: sqlDB(t, planner.Migration)}
	if _, err := s.Get(context.Background(), "hash-absent"); err == nil {
		t.Fatal("a hash that was never pinned read as a snapshot")
	}
}

func TestASnapshotStoreThatCannotBeReadIsNotEmpty(t *testing.T) {
	s := planner.SQLSnapshots{DB: sqlDB(t)}
	if _, err := s.Count(context.Background()); err == nil {
		t.Error("a missing table counted as zero snapshots")
	}
	if err := s.Put(context.Background(), planner.Snapshot{Hash: "h"}); err == nil {
		t.Error("a write to a missing table reported success")
	}
}

func TestATargetSurvivesEverythingRecoveryNeeds(t *testing.T) {
	// A write-ahead row that cannot rebuild its own call is not write-ahead:
	// a recovery that reconstructed a different actor would present a
	// different idempotency key and create a duplicate.
	s := planner.SQLTargets{DB: sqlDB(t, planner.TargetMigration)}
	want := planner.BuildTarget{
		TargetKey: "key-1", SpecHash: "hash-1", ProjectID: "p-1", EpicID: "e-1",
		EpicState: planner.EpicPending, ProjectKey: "KRIYA0a1b2c",
		Name: "kriya", Actor: "actor-1",
	}
	if err := s.Upsert(context.Background(), want); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, found, err := s.Find(context.Background(), "key-1")
	if err != nil || !found {
		t.Fatalf("find: %v found=%v", err, found)
	}
	if got != want {
		t.Errorf("read back %+v", got)
	}
}

func TestAFirstIntakeHasNoRowRatherThanAnError(t *testing.T) {
	s := planner.SQLTargets{DB: sqlDB(t, planner.TargetMigration)}
	_, found, err := s.Find(context.Background(), "key-absent")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if found {
		t.Error("a target that was never recorded was found")
	}
}

func TestPendingTargetsComeBackInADeclaredOrder(t *testing.T) {
	s := planner.SQLTargets{DB: sqlDB(t, planner.TargetMigration)}
	for _, target := range []planner.BuildTarget{
		{TargetKey: "key-b", EpicState: planner.EpicPending},
		{TargetKey: "key-a", EpicState: planner.EpicPending},
		{TargetKey: "key-c", EpicState: "ready"},
	} {
		if err := s.Upsert(context.Background(), target); err != nil {
			t.Fatalf("upsert %s: %v", target.TargetKey, err)
		}
	}
	got, err := s.Pending(context.Background())
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(got) != 2 || got[0].TargetKey != "key-a" || got[1].TargetKey != "key-b" {
		t.Fatalf("listed %+v", got)
	}
}

func TestATargetStoreThatCannotBeReadIsNotEmpty(t *testing.T) {
	// A short list reads exactly like "nothing left to recover".
	s := planner.SQLTargets{DB: sqlDB(t)}
	if _, err := s.Pending(context.Background()); err == nil {
		t.Error("a missing table listed as no pending targets")
	}
	if _, _, err := s.Find(context.Background(), "key-1"); err == nil {
		t.Error("a missing table read as a target that does not exist")
	}
	if err := s.Upsert(context.Background(), planner.BuildTarget{TargetKey: "k"}); err == nil {
		t.Error("a write to a missing table reported success")
	}
}
