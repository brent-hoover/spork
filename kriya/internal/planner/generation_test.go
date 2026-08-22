package planner_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"kriya/internal/planner"
)

func attempts(t *testing.T) planner.SQLAttempts {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "k.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, stmt := range splitStatements(planner.AttemptMigration) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("migrate: %v", err)
		}
	}
	return planner.SQLAttempts{DB: db}
}

func splitStatements(s string) []string {
	var out []string
	for _, part := range splitOn(s, ';') {
		if trimmed := trimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func TestTheSameTokenReusesItsGeneration(t *testing.T) {
	// "it resumes the recorded attempt and reuses its generation — no
	// duplicate supersession". A crash before the plan was created must not
	// cost a generation on retry.
	s := attempts(t)
	first, err := s.Reserve(context.Background(), "token-a", "/spec")
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	again, err := s.Reserve(context.Background(), "token-a", "/spec")
	if err != nil {
		t.Fatalf("reserve again: %v", err)
	}
	if again.Generation != first.Generation {
		t.Errorf("retry allocated generation %d, want %d", again.Generation, first.Generation)
	}
}

func TestANewTokenAllocatesTheNextGeneration(t *testing.T) {
	// "a deliberate same-hash re-intake arrives under a new token … it
	// allocates the next generation". The token, not the content, decides.
	s := attempts(t)
	first, _ := s.Reserve(context.Background(), "token-a", "/spec")
	second, err := s.Reserve(context.Background(), "token-b", "/spec")
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if second.Generation != first.Generation+1 {
		t.Errorf("got generation %d, want %d", second.Generation, first.Generation+1)
	}
}

func TestNoTwoAttemptsShareAGeneration(t *testing.T) {
	// The CAS increment serializes racing intakes: one reserves N, the loser
	// re-reads and reserves N+1.
	s := attempts(t)
	seen := map[int]string{}
	for _, token := range []string{"a", "b", "c", "d", "e"} {
		got, err := s.Reserve(context.Background(), token, "/spec")
		if err != nil {
			t.Fatalf("reserve %s: %v", token, err)
		}
		if other, clash := seen[got.Generation]; clash {
			t.Fatalf("tokens %s and %s both hold generation %d", other, token, got.Generation)
		}
		seen[got.Generation] = token
	}
}

func TestGenerationsAreScopedToTheTarget(t *testing.T) {
	s := attempts(t)
	a, _ := s.Reserve(context.Background(), "token-a", "/one")
	b, _ := s.Reserve(context.Background(), "token-b", "/two")
	if a.Generation != 1 || b.Generation != 1 {
		t.Errorf("each target starts at 1, got %d and %d", a.Generation, b.Generation)
	}
}

func TestAnOlderGenerationCannotRegressTheMapping(t *testing.T) {
	// "the mapping is untouched — the older generation loses the upsert".
	// Unplanned binds read this, so a delayed seed would otherwise point every
	// later bind at a superseded snapshot.
	s := attempts(t)
	ctx := context.Background()
	if err := s.MapSpec(ctx, planner.SpecMapping{TargetKey: "/spec", Generation: 2, SnapshotHash: "newer"}); err != nil {
		t.Fatalf("map: %v", err)
	}
	if err := s.MapSpec(ctx, planner.SpecMapping{TargetKey: "/spec", Generation: 1, SnapshotHash: "older"}); err != nil {
		t.Fatalf("map older: %v", err)
	}
	got, _, err := s.Mapping(ctx, "/spec")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Generation != 2 || got.SnapshotHash != "newer" {
		t.Errorf("the older generation won: %+v", got)
	}
}

func TestTheMappingConvergesOnTheNewerGenerationWhicheverArrivesFirst(t *testing.T) {
	// "a generation-1 plan seed inserts first and the generation-2 intake
	// write arrives after … the mapping converges on generation 2". Arrival
	// order must not decide the outcome; the fence must.
	s := attempts(t)
	ctx := context.Background()
	if err := s.MapSpec(ctx, planner.SpecMapping{TargetKey: "/spec", Generation: 1, SnapshotHash: "older"}); err != nil {
		t.Fatalf("map: %v", err)
	}
	if err := s.MapSpec(ctx, planner.SpecMapping{TargetKey: "/spec", Generation: 2, SnapshotHash: "newer"}); err != nil {
		t.Fatalf("map: %v", err)
	}
	got, _, _ := s.Mapping(ctx, "/spec")
	if got.Generation != 2 || got.SnapshotHash != "newer" {
		t.Errorf("did not converge on the newer generation: %+v", got)
	}
}

func TestASameHashReIntakeStillAllocatesAHigherGeneration(t *testing.T) {
	// "a re-intake pins the same content hash a prior intake pinned … it
	// allocates a fresh, higher generation", and a plan still carrying the
	// prior generation cannot regress the mapping even naming that same hash.
	s := attempts(t)
	ctx := context.Background()
	first, _ := s.Reserve(ctx, "token-a", "/spec")
	if err := s.Complete(ctx, "token-a", "samehash"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if err := s.MapSpec(ctx, planner.SpecMapping{TargetKey: "/spec", Generation: first.Generation, SnapshotHash: "samehash"}); err != nil {
		t.Fatalf("map: %v", err)
	}
	second, _ := s.Reserve(ctx, "token-b", "/spec")
	if second.Generation <= first.Generation {
		t.Fatalf("re-intake got generation %d, want higher than %d", second.Generation, first.Generation)
	}
	if err := s.MapSpec(ctx, planner.SpecMapping{TargetKey: "/spec", Generation: second.Generation, SnapshotHash: "samehash"}); err != nil {
		t.Fatalf("map: %v", err)
	}
	// The stale plan, naming the same hash under the older generation.
	if err := s.MapSpec(ctx, planner.SpecMapping{TargetKey: "/spec", Generation: first.Generation, SnapshotHash: "samehash"}); err != nil {
		t.Fatalf("map stale: %v", err)
	}
	got, _, _ := s.Mapping(ctx, "/spec")
	if got.Generation != second.Generation {
		t.Errorf("a stale plan naming the same hash regressed the mapping to %d", got.Generation)
	}
}
