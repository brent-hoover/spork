package main

import (
	"io"
	"net/http"
	"net/http/httptest"

	"context"
	"errors"
	"kriya/internal/clock"
	"kriya/internal/devloop"
	"kriya/internal/orchestrator"
	"kriya/internal/planner"
	"kriya/internal/trackerclient"
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
	if driver(nil, workspaceManagerForTest(t), tiersForTest(t), bridgeForTest(t), "", "/target", "actor-1") != nil {
		t.Fatal("a driver was returned with no repository configured")
	}
}

func TestRecoveryStepsRunInDeclaredOrder(t *testing.T) {
	db := openTemp(t)
	if err := applyMigrations(context.Background(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	steps := recoverySteps(intakerForTest(db), workspaceManagerForTest(t), bridgeForTest(t), loopForTest(db), submitterForTest(db), queueForTest(t, db), "actor-1")
	var stages []string
	for _, s := range steps {
		stages = append(stages, s.Stage.String())
	}
	got := strings.Join(stages, ",")
	if got != "targets,workspaces,dev-sessions,merges,review-rounds" {
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
	steps := recoverySteps(intakerForTest(db), workspaceManagerForTest(t), bridgeWithStore(db), loopForTest(db), submitterForTest(db), queueForTest(t, db), "actor-1")
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
	steps := recoverySteps(intakerForTest(db), workspaceManagerForTest(t), bridgeWithStore(db), loopForTest(db), submitterForTest(db), queueForTest(t, db), "actor-1")
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

func TestTheRoundLimitIsReadFromTheEnvironment(t *testing.T) {
	t.Setenv("KRIYA_ROUND_LIMIT", "")
	if got := roundLimit(); got != 0 {
		t.Errorf("an unset limit read as %d", got)
	}
	t.Setenv("KRIYA_ROUND_LIMIT", "7")
	if got := roundLimit(); got != 7 {
		t.Errorf("read %d", got)
	}
}

func TestAnUnusableRoundLimitFallsBackRatherThanRefusing(t *testing.T) {
	// A build engine that refused to start over a typo in an optional tuning
	// knob would be worse than one that says so and carries on.
	for _, raw := range []string{"lots", "0", "-3"} {
		t.Setenv("KRIYA_ROUND_LIMIT", raw)
		if got := roundLimit(); got != 0 {
			t.Errorf("KRIYA_ROUND_LIMIT=%q read as %d", raw, got)
		}
	}
}

func TestAnUnapprovedReviewLeavesTheRunWaiting(t *testing.T) {
	// Any verdict but approved leaves the run exactly where it was, which is
	// what a submitted run does.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"review-1","state":"changes-requested","revision":2}`)
	}))
	defer srv.Close()

	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	run := orchestrator.BuildRun{
		ID: "run-1", Ticket: "KRI-1", State: orchestrator.StateReviewSubmitted,
		GatedBase: "base-1", ReviewID: "review-1", ReviewCommit: "C2",
	}
	got, err := observeApproval(t.Context(),
		orchestrator.Orchestrator{Store: orchestrator.SQLStore{DB: db}, Now: clock.System{}},
		queueForTest(t, db), trackerclient.New(srv.URL), run, "/target")
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if got.State != orchestrator.StateReviewSubmitted {
		t.Errorf("the run moved to %q", got.State)
	}
}

func TestAnUnreadableReviewStopsTheObservation(t *testing.T) {
	// "I could not read the verdict" is not "there is no verdict".
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	_, err := observeApproval(t.Context(),
		orchestrator.Orchestrator{Store: orchestrator.SQLStore{DB: db}, Now: clock.System{}},
		queueForTest(t, db), trackerclient.New(srv.URL),
		orchestrator.BuildRun{ID: "run-1", ReviewID: "review-1"}, "/target")
	if err == nil {
		t.Fatal("an unreadable review read as unapproved")
	}
}

func TestASessionIsReadFromTheRunThatDidTheWork(t *testing.T) {
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := (devloop.SQLStore{DB: db}).Upsert(t.Context(), devloop.Session{
		Run: "run-1", Ticket: "KRI-1", SessionID: "sess-42",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := sessionFromStore(db)(t.Context(), "run-1")
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	if got != "sess-42" {
		t.Errorf("read %q", got)
	}
	// A run with no session is an error, not an empty string: a review stamped
	// with nothing routes feedback nowhere.
	if _, err := sessionFromStore(db)(t.Context(), "run-absent"); err == nil {
		t.Error("a run with no session read as one with a blank session")
	}
}

func TestATicketsIssueAndCriteriaAreLookedUpByTitle(t *testing.T) {
	tickets := []planner.Ticket{
		{Title: "Create a short link", Criteria: []string{"AC-valid-url"}, IssueID: "issue-7"},
		{Title: "Redirect", Criteria: []string{"AC-redirect"}, IssueID: "issue-8"},
	}
	if got := issuesFromTickets(tickets)("Redirect"); got != "issue-8" {
		t.Errorf("resolved %q", got)
	}
	if got := criteriaFromTickets(tickets)("Create a short link"); len(got) != 1 ||
		got[0] != "AC-valid-url" {
		t.Errorf("resolved %v", got)
	}
	// An unknown title resolves to nothing rather than to another ticket's.
	if got := issuesFromTickets(tickets)("Unknown"); got != "" {
		t.Errorf("an unknown ticket resolved to %q", got)
	}
}
