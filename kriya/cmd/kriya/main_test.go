package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTheDSNTurnsOnWhatSQLiteLeavesOff(t *testing.T) {
	// foreign_keys is OFF by default, so REFERENCES clauses would parse and
	// enforce nothing.
	got := dsn("/tmp/x.db")
	for _, want := range []string{"foreign_keys(1)", "busy_timeout(5000)"} {
		if !strings.Contains(got, want) {
			t.Errorf("dsn %q is missing %s", got, want)
		}
	}
}

func TestOpenStoreAppliesEveryModuleSchema(t *testing.T) {
	t.Setenv("KRIYA_DB", filepath.Join(t.TempDir(), "kriya.db"))
	db, err := openStore(context.Background())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, table := range []string{"spec_snapshot", "build_run", "dev_session", "review_round"} {
		if !tableExists(t, db, table) {
			t.Errorf("%s was never created", table)
		}
	}
}

func TestOpenStoreFailsWhenTheSchemaCannotBeApplied(t *testing.T) {
	// A directory is not a database. The failure must surface at open, not as
	// a missing table halfway through a build.
	t.Setenv("KRIYA_DB", t.TempDir())
	if _, err := openStore(context.Background()); err == nil {
		t.Fatal("opening a directory as a database must fail")
	}
}

func TestRunRefusesWithoutACommand(t *testing.T) {
	t.Setenv("KRIYA_DB", filepath.Join(t.TempDir(), "kriya.db"))
	if err := run(context.Background(), nil); err == nil {
		t.Fatal("no arguments must produce usage, not silence")
	}
}

func TestRunRejectsAnUnknownCommand(t *testing.T) {
	t.Setenv("KRIYA_DB", filepath.Join(t.TempDir(), "kriya.db"))
	err := run(context.Background(), []string{"deploy"})
	if err == nil || !strings.Contains(err.Error(), "deploy") {
		t.Fatalf("got %v, want the unknown command named", err)
	}
}

func TestBuildNeedsExactlyOneTarget(t *testing.T) {
	t.Setenv("KRIYA_DB", filepath.Join(t.TempDir(), "kriya.db"))
	for _, args := range [][]string{{"build"}, {"build", "a", "b"}} {
		if err := run(context.Background(), args); err == nil {
			t.Errorf("%v was accepted", args)
		}
	}
}

func TestBuildRefusesAnActorThatIsNotAnIdentity(t *testing.T) {
	// Validated BEFORE any mutation: sutra settles a rejected request under
	// its idempotency key, so sending one kriya knew was invalid would poison
	// that key permanently.
	t.Setenv("KRIYA_DB", filepath.Join(t.TempDir(), "kriya.db"))
	t.Setenv("KRIYA_ACTOR", "brent")
	err := run(context.Background(), []string{"build", t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "KRIYA_ACTOR") {
		t.Fatalf("got %v, want a refusal naming KRIYA_ACTOR", err)
	}
}

func TestBuildRefusesWithoutTiers(t *testing.T) {
	t.Setenv("KRIYA_DB", filepath.Join(t.TempDir(), "kriya.db"))
	t.Setenv("KRIYA_ACTOR", "6f1b3a4e-0000-4000-8000-000000000001")
	t.Setenv("KRIYA_TIERS", "")
	err := run(context.Background(), []string{"build", t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "KRIYA_TIERS") {
		t.Fatalf("got %v, want a refusal naming KRIYA_TIERS", err)
	}
}

func writeTiers(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tiers.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write tiers: %v", err)
	}
	return path
}

func TestLoadTiersReadsTheConfiguredFile(t *testing.T) {
	// No model identifier appears in kriya's source (AC-tier-config), so the
	// file is the only place a role can get one.
	t.Setenv("KRIYA_TIERS", writeTiers(t, `{"roles":{"pm":"heavy"},"models":{"heavy":"a-model"}}`))
	tiers, err := loadTiers()
	if err != nil {
		t.Fatalf("load tiers: %v", err)
	}
	if err := tiers.Validate("pm"); err != nil {
		t.Errorf("pm did not survive the round trip: %v", err)
	}
}

func TestLoadTiersFailsOnAMissingFile(t *testing.T) {
	t.Setenv("KRIYA_TIERS", filepath.Join(t.TempDir(), "absent.json"))
	if _, err := loadTiers(); err == nil {
		t.Fatal("a named file that does not exist must fail loudly")
	}
}

func TestLoadTiersFailsOnUnparseableConfig(t *testing.T) {
	t.Setenv("KRIYA_TIERS", writeTiers(t, "roles: pm"))
	if _, err := loadTiers(); err == nil {
		t.Fatal("a tiers file that is not JSON must fail loudly")
	}
}

func TestBuildRefusesATierlessPM(t *testing.T) {
	// AC-tier-explicit: a missing tier fails at startup, not halfway through a
	// decomposition.
	t.Setenv("KRIYA_DB", filepath.Join(t.TempDir(), "kriya.db"))
	t.Setenv("KRIYA_ACTOR", "6f1b3a4e-0000-4000-8000-000000000001")
	t.Setenv("KRIYA_TIERS", writeTiers(t, `{"roles":{"dev":"heavy"},"models":{"heavy":"a-model"}}`))
	if err := run(context.Background(), []string{"build", t.TempDir()}); err == nil {
		t.Fatal("a build with no PM tier must refuse before it starts")
	}
}

func TestVerifierDefaultsToAvspecOnPath(t *testing.T) {
	// An earlier version always used `uv run avspec`, which resolves its
	// environment from the WORKING DIRECTORY — so it worked only where uv
	// could find the project and failed obscurely everywhere else.
	t.Setenv("KRIYA_AVSPEC_DIR", "")
	v := verifier()
	if len(v.Argv) != 1 || v.Argv[0] != "avspec" {
		t.Errorf("default argv %v", v.Argv)
	}
	if v.WorkDir != "" {
		t.Errorf("default pinned a working directory: %q", v.WorkDir)
	}
}

func TestVerifierUsesTheNamedUVProject(t *testing.T) {
	t.Setenv("KRIYA_AVSPEC_DIR", "/opt/avspec")
	v := verifier()
	if strings.Join(v.Argv, " ") != "uv run avspec" || v.WorkDir != "/opt/avspec" {
		t.Errorf("argv %v in %q", v.Argv, v.WorkDir)
	}
}

func TestSutraURLDefaultsToTheLocalTracker(t *testing.T) {
	t.Setenv("KRIYA_SUTRA_URL", "")
	if got := sutraURL(); got != "http://127.0.0.1:7357" {
		t.Errorf("default %q", got)
	}
	t.Setenv("KRIYA_SUTRA_URL", "http://sutra.internal")
	if got := sutraURL(); got != "http://sutra.internal" {
		t.Errorf("override %q", got)
	}
}

func TestLatestSnapshotHashIsEmptyBeforeAnyIntake(t *testing.T) {
	// A target nothing has mapped has no pinned snapshot, and reporting one
	// would build against a spec that was never admitted.
	db := openTemp(t)
	if err := applyMigrations(context.Background(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if got := latestSnapshotHash(context.Background(), db, "/nowhere"); got != "" {
		t.Errorf("got %q for an unmapped target", got)
	}
}

func TestNoRepositoryMeansNoDriver(t *testing.T) {
	// A build engine with nowhere to work says so and stops, rather than
	// cutting branches in whatever repository it is standing in.
	if driver(nil, workspaceManagerForTest(t), tiersForTest(t), bridgeForTest(t), "", "/target") != nil {
		t.Fatal("a driver was returned with no repository configured")
	}
}

func TestRecoveryStepsRunInDeclaredOrder(t *testing.T) {
	db := openTemp(t)
	if err := applyMigrations(context.Background(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	steps := recoverySteps(intakerForTest(db), workspaceManagerForTest(t), bridgeForTest(t))
	var stages []string
	for _, s := range steps {
		stages = append(stages, s.Stage.String())
	}
	got := strings.Join(stages, ",")
	if got != "targets,workspaces,review-rounds" {
		t.Errorf("stages %s — the order is declared, not discovered", got)
	}
}

func TestReviewRecoverySurfacesRatherThanBlocks(t *testing.T) {
	// One ambiguous review must not block every future build.
	db := openTemp(t)
	if err := applyMigrations(context.Background(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	_, err := db.Exec(
		`INSERT INTO enqueue_attempt (round, run, commit_sha, state, job_id, note)
		 VALUES ('r-1','run-1','abc123','unresolved',0,'connection reset')`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	steps := recoverySteps(intakerForTest(db), workspaceManagerForTest(t), bridgeWithStore(db))
	for _, s := range steps {
		if s.Owner != "reviewbridge" {
			continue
		}
		if err := s.Run(context.Background()); err != nil {
			t.Fatalf("an unresolved review blocked recovery: %v", err)
		}
		return
	}
	t.Fatal("no reviewbridge recovery step is registered")
}

func TestReviewRecoveryPropagatesAStoreFailure(t *testing.T) {
	// Errors should never pass silently: a store kriya cannot read is not the
	// same as a store with nothing in it.
	db := openTemp(t)
	steps := recoverySteps(intakerForTest(db), workspaceManagerForTest(t), bridgeWithStore(db))
	for _, s := range steps {
		if s.Owner != "reviewbridge" {
			continue
		}
		if err := s.Run(context.Background()); err == nil {
			t.Fatal("a missing enqueue_attempt table read as no unresolved reviews")
		}
		return
	}
	t.Fatal("no reviewbridge recovery step is registered")
}

func TestMainErrorsCarryTheirCause(t *testing.T) {
	t.Setenv("KRIYA_DB", t.TempDir())
	err := run(context.Background(), []string{"build", "."})
	if err == nil {
		t.Fatal("a build on an unopenable store must fail")
	}
	if errors.Unwrap(err) == nil && !strings.Contains(err.Error(), "open store") {
		t.Errorf("error %q lost its cause", err)
	}
}
