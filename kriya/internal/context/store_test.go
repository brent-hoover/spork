package context_test

import (
	stdctx "context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	// Imported under its OWN name, with the standard library aliased instead.
	// gobco builds the black-box coverage bridge by scanning these files for an
	// import whose NAME matches the package under test; an aliased subject
	// leaves it with an empty import path and instrumentation aborts.
	"kriya/internal/context"
)

func sqlDB(t *testing.T, migrate bool) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if !migrate {
		return db
	}
	for _, stmt := range strings.Split(context.Migration, ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("migrate: %v", err)
		}
	}
	return db
}

func TestABundleSurvivesTheRoundTrip(t *testing.T) {
	s := context.SQLBundles{DB: sqlDB(t, true)}
	want := context.Bundle{
		Build:   "run-1",
		Content: json.RawMessage(`{"criteria":["AC-x"]}`),
		Created: time.Date(2026, 8, 22, 9, 0, 0, 0, time.UTC),
	}
	if err := s.Put(stdctx.Background(), want); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, found, err := s.Get(stdctx.Background(), "run-1")
	if err != nil || !found {
		t.Fatalf("get: %v found=%v", err, found)
	}
	if string(got.Content) != string(want.Content) || !got.Created.Equal(want.Created) {
		t.Errorf("read back %+v", got)
	}
}

func TestReassemblingARunReplacesItsBundle(t *testing.T) {
	s := context.SQLBundles{DB: sqlDB(t, true)}
	b := context.Bundle{Build: "run-1", Content: json.RawMessage(`{"n":1}`), Created: time.Unix(0, 0).UTC()}
	if err := s.Put(stdctx.Background(), b); err != nil {
		t.Fatalf("put: %v", err)
	}
	b.Content = json.RawMessage(`{"n":2}`)
	if err := s.Put(stdctx.Background(), b); err != nil {
		t.Fatalf("re-put: %v", err)
	}
	got, _, err := s.Get(stdctx.Background(), "run-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got.Content) != `{"n":2}` {
		t.Errorf("read back %s", got.Content)
	}
}

func TestARunWithNoBundleIsNotFound(t *testing.T) {
	s := context.SQLBundles{DB: sqlDB(t, true)}
	_, found, err := s.Get(stdctx.Background(), "run-absent")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if found {
		t.Error("a run that was never assembled was found")
	}
}

func TestABundleStoreThatCannotBeReadIsNotEmpty(t *testing.T) {
	s := context.SQLBundles{DB: sqlDB(t, false)}
	if _, _, err := s.Get(stdctx.Background(), "run-1"); err == nil {
		t.Error("a missing table read as a run with no bundle")
	}
	if err := s.Put(stdctx.Background(), context.Bundle{Build: "run-1"}); err == nil {
		t.Error("a write to a missing table reported success")
	}
}

func seedLearning(t *testing.T, db *sql.DB, module, pattern, lesson string) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO learning (scope, lesson, module, pattern, source_kind, created)
		 VALUES ('global', ?, ?, ?, 'operator', '2026-08-22T09:00:00Z')`,
		lesson, module, pattern)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func TestEitherTagIsEnoughToMatch(t *testing.T) {
	// A lesson about a failure pattern is worth having on a module that has
	// not hit it yet — that is what feeding it forward is for.
	db := sqlDB(t, true)
	seedLearning(t, db, "MOD-api", "pagination", "by module")
	seedLearning(t, db, "MOD-other", "off-by-one", "by pattern")
	seedLearning(t, db, "MOD-other", "rounding", "neither")

	got, err := context.SQLLearnings{DB: db}.Matching(stdctx.Background(),
		[]string{"MOD-api"}, []string{"off-by-one"})
	if err != nil {
		t.Fatalf("matching: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("matched %+v", got)
	}
	if got[0].Lesson != "by module" || got[1].Lesson != "by pattern" {
		t.Errorf("matched %+v", got)
	}
}

func TestMatchingOnModulesAloneWorks(t *testing.T) {
	db := sqlDB(t, true)
	seedLearning(t, db, "MOD-api", "pagination", "by module")
	seedLearning(t, db, "MOD-other", "pagination", "wrong module")
	got, err := context.SQLLearnings{DB: db}.Matching(stdctx.Background(), []string{"MOD-api"}, nil)
	if err != nil {
		t.Fatalf("matching: %v", err)
	}
	if len(got) != 1 || got[0].Lesson != "by module" {
		t.Errorf("matched %+v", got)
	}
}

func TestMatchingOnPatternsAloneWorks(t *testing.T) {
	db := sqlDB(t, true)
	seedLearning(t, db, "MOD-api", "off-by-one", "by pattern")
	seedLearning(t, db, "MOD-api", "rounding", "wrong pattern")
	got, err := context.SQLLearnings{DB: db}.Matching(stdctx.Background(), nil, []string{"off-by-one"})
	if err != nil {
		t.Fatalf("matching: %v", err)
	}
	if len(got) != 1 || got[0].Lesson != "by pattern" {
		t.Errorf("matched %+v", got)
	}
}

func TestAskingForNothingReturnsNothingWithoutQuerying(t *testing.T) {
	// An unfiltered query would return every learning ever recorded, which is
	// the opposite of matching.
	s := context.SQLLearnings{DB: sqlDB(t, false)}
	got, err := s.Matching(stdctx.Background(), nil, nil)
	if err != nil {
		t.Fatalf("matching: %v", err)
	}
	if got != nil {
		t.Errorf("returned %+v", got)
	}
}

func TestALearningStoreThatCannotBeReadIsNotEmpty(t *testing.T) {
	s := context.SQLLearnings{DB: sqlDB(t, false)}
	if _, err := s.Matching(stdctx.Background(), []string{"MOD-api"}, nil); err == nil {
		t.Error("a missing table read as nothing having been learned")
	}
}

func TestAnUnparseableTimestampFails(t *testing.T) {
	db := sqlDB(t, true)
	_, err := db.Exec(
		`INSERT INTO context_bundle (build, content, created) VALUES ('run-1', '{}', 'yesterday')`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, _, err := (context.SQLBundles{DB: db}).Get(stdctx.Background(), "run-1"); err == nil {
		t.Fatal("a bundle with an unreadable timestamp was accepted")
	}
}
