package main

import (
	"io"
	"net/http"
	"net/http/httptest"

	"context"
	"errors"
	"kriya/internal/agent"
	"kriya/internal/clock"
	"kriya/internal/devloop"
	"kriya/internal/orchestrator"
	"kriya/internal/planner"
	"kriya/internal/recovery"
	"kriya/internal/reviewbridge"
	"kriya/internal/trackerclient"
	"kriya/internal/workspace"
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
	steps := recoverySteps(intakerForTest(db), workspaceManagerForTest(t), bridgeForTest(t), loopForTest(db), submitterForTest(db), queueForTest(t, db), completerForTest(t, db), "actor-1", planner.Claimer{}, noTargets, noWork, "/target",
		orchestrator.Researcher{}, noFindingProject)
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
	steps := recoverySteps(intakerForTest(db), workspaceManagerForTest(t), bridgeWithStore(db), loopForTest(db), submitterForTest(db), queueForTest(t, db), completerForTest(t, db), "actor-1", planner.Claimer{}, noTargets, noWork, "/target",
		orchestrator.Researcher{}, noFindingProject)
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
	steps := recoverySteps(intakerForTest(db), workspaceManagerForTest(t), bridgeWithStore(db), loopForTest(db), submitterForTest(db), queueForTest(t, db), completerForTest(t, db), "actor-1", planner.Claimer{}, noTargets, noWork, "/target",
		orchestrator.Researcher{}, noFindingProject)
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

func TestTheWholeTicketIsRecoveredFromThePlan(t *testing.T) {
	// A pop carries an id and a title. Everything the dev agent implements
	// against and the product owner validates against — the body and the
	// acceptance criteria — is read back from the plan.
	full := planner.Ticket{
		Title: "Create a short link", Body: "Accept a URL and return a code.",
		Criteria: []string{"AC-valid-url"}, IssueID: "issue-7",
		Plan: "plan-1", Ordinal: 7,
	}
	popped := planner.Ticket{Title: full.Title, IssueID: "issue-7", Plan: "plan-1", Ordinal: 7}

	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := (planner.SQLTickets{DB: db}).Put(t.Context(), "/target", full); err != nil {
		t.Fatalf("put ticket: %v", err)
	}
	got := ticketFromStore(db, "/target", popped)(full.Title)
	if got.Body != full.Body {
		t.Errorf("the body came back as %q", got.Body)
	}
	if len(got.Criteria) != 1 || got.Criteria[0] != "AC-valid-url" {
		t.Errorf("the criteria came back as %v", got.Criteria)
	}
	if got.IssueID != "issue-7" {
		t.Errorf("the issue came back as %q", got.IssueID)
	}

	// A plan row that cannot be read still yields the ISSUE: submitting a
	// review against nothing and closing no ticket is worse than reaching the
	// dev agent with a thin ticket.
	unrecorded := planner.Ticket{Title: "Redirect", IssueID: "issue-8", Plan: "plan-1", Ordinal: 8}
	if got := ticketFromStore(db, "/target", unrecorded)("Redirect"); got.IssueID != "issue-8" {
		t.Errorf("an unrecorded ticket lost its issue: %q", got.IssueID)
	}
}

func TestLearnAddRecordsAnOperatorLearning(t *testing.T) {
	// Through the real command surface: an operator has no other way in, and a
	// test that wrote to the store directly would prove nothing about whether
	// they do.
	t.Setenv("KRIYA_DB", filepath.Join(t.TempDir(), "kriya.db"))
	err := run(t.Context(), []string{"learn", "add",
		"--module", "MOD-api", "--pattern", "error-handling",
		"--lesson", "never swallow an exception", "--project", "/target"})
	if err != nil {
		t.Fatalf("learn add: %v", err)
	}
}

func TestLearnAddWithoutAProjectIsCrossProject(t *testing.T) {
	// The scope follows the project key: "which project" and "is it
	// project-scoped" are one decision, and two flags could contradict.
	t.Setenv("KRIYA_DB", filepath.Join(t.TempDir(), "kriya.db"))
	if err := run(t.Context(), []string{"learn", "add",
		"--module", "MOD-api", "--pattern", "p", "--lesson", "a global lesson"}); err != nil {
		t.Fatalf("learn add: %v", err)
	}
}

func TestLearnRefusesAnIncompleteEntry(t *testing.T) {
	// The write enforces the rules; the command surfaces the refusal.
	t.Setenv("KRIYA_DB", filepath.Join(t.TempDir(), "kriya.db"))
	if err := run(t.Context(), []string{"learn", "add", "--module", "MOD-api"}); err == nil {
		t.Fatal("a learning with no lesson was recorded")
	}
}

func TestLearnNeedsASubcommand(t *testing.T) {
	t.Setenv("KRIYA_DB", filepath.Join(t.TempDir(), "kriya.db"))
	for _, args := range [][]string{{"learn"}, {"learn", "remove"}} {
		if err := run(t.Context(), args); err == nil {
			t.Errorf("%v was accepted", args)
		}
	}
}

func TestAPoppedTicketResumesItsExistingRun(t *testing.T) {
	// Starting a second run would abandon the first along with its review, its
	// attempt count and everything the tracker already points at.
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := orchestrator.SQLStore{DB: db}
	existing := orchestrator.BuildRun{
		ID: "run-existing", Ticket: "Create a short link", Issue: "issue-7",
		Plan: "/target", State: orchestrator.StateDevLoop, ReviewID: "review-1", Attempt: 3,
	}
	if err := store.Upsert(t.Context(), existing); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := resumeOrStart(t.Context(), store,
		planner.Ticket{Title: "Create a short link", IssueID: "issue-7", Plan: "plan-1", Ordinal: 7}, "/target")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got.ID != "run-existing" || got.Attempt != 3 {
		t.Errorf("started %+v instead of resuming", got)
	}
	// A DIFFERENT issue sharing the title starts its own run. Titles are not
	// unique, and resuming on one makes the second claim drive the first
	// issue's work while orphaning the issue it actually claimed.
	other, err := resumeOrStart(t.Context(), store,
		planner.Ticket{Title: "Create a short link", IssueID: "issue-8", Plan: "plan-1", Ordinal: 8}, "/target")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if other.ID == "run-existing" {
		t.Error("issue-8 resumed the run that belongs to issue-7")
	}
}

func TestAFreshTicketStartsANewRun(t *testing.T) {
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := orchestrator.SQLStore{DB: db}
	got, err := resumeOrStart(t.Context(), store,
		planner.Ticket{Title: "Redirect a short code", IssueID: "issue-new", Plan: "plan-1", Ordinal: 0}, "/target")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if got.ID == "" || got.State != orchestrator.StateQueued {
		t.Errorf("started %+v", got)
	}
	// The ISSUE, recorded at creation. Recovery replays a submission and a
	// close from what the run holds, and a title is not an issue id.
	if got.Issue != "issue-new" {
		t.Errorf("the run recorded issue %q", got.Issue)
	}
	// And it is durable before anything drives it: a run only in memory is
	// one a crash loses along with its claim.
	if _, found, err := store.Find(t.Context(), got.ID); err != nil || !found {
		t.Errorf("the new run was not recorded: %v found=%v", err, found)
	}
}

func TestWithoutARepositoryOnlyTheTrackerStageRecovers(t *testing.T) {
	// Every other stage shells out to git in the manager's Repo, and an empty
	// one is the process's own working directory — so recovering a pending
	// workspace would cut branches and worktrees in whatever repository kriya
	// happened to be launched from.
	all := recoverySteps(planner.Intaker{}, workspace.Manager{}, reviewbridge.Bridge{},
		devloop.Loop{}, orchestrator.Submitter{}, orchestrator.Queue{},
		orchestrator.Completer{}, "actor-1", planner.Claimer{}, noTargets, noWork, "/target",
		orchestrator.Researcher{}, noFindingProject)
	if len(all) < 2 {
		t.Fatalf("only %d recovery steps exist, so nothing is being skipped", len(all))
	}
	kept := repositoryIndependent(all)
	if len(kept) != 1 || kept[0].Stage != recovery.StageTargets {
		t.Fatalf("kept %d steps: %+v", len(kept), kept)
	}
}

func TestAnApprovedVerdictIsEnqueuedRatherThanRead(t *testing.T) {
	// The cursor advances past a consumed verdict and never offers it again,
	// so a verdict routed and then dropped is a run that waits forever.
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	run := orchestrator.BuildRun{
		ID: "run-1", Ticket: "T-1", Issue: "issue-7", Plan: "/target",
		State: orchestrator.StateReviewSubmitted, ReviewID: "review-1",
		ReviewCommit: "C2", ReviewRevision: 1, Branch: "kriya/T-1/abcd",
	}
	if err := (orchestrator.SQLStore{DB: db}).Upsert(t.Context(), run); err != nil {
		t.Fatalf("seed: %v", err)
	}
	queue := queueForTest(t, db)
	routed := []orchestrator.Routed{{
		Run: run,
		Verdict: orchestrator.VerdictEvent{
			ID: "event-9", Review: "review-1", Session: "sess-1",
			Verdict: orchestrator.VerdictApproved,
		},
	}}
	// The drive that follows fails against a repository this test does not
	// populate; what is asserted is that the approval was ENQUEUED first.
	if err := actOnVerdicts(t.Context(), db, workspace.Manager{}, agent.Tiers{},
		reviewbridge.Bridge{}, queue, routed, "/target", "actor-1"); err != nil {
		t.Logf("drive: %v", err)
	}

	key, found, err := queue.AttemptFor(t.Context(), "run-1")
	if err != nil {
		t.Fatalf("attempt for: %v", err)
	}
	if !found || key == "" {
		t.Fatal("the approval was read and dropped: no merge attempt exists")
	}
}

func TestAReworkedVerdictEnqueuesNothing(t *testing.T) {
	// The control. A changes-requested run goes back to the pair loop; there
	// is nothing to merge, and enqueuing one would try to land rejected work.
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	run := orchestrator.BuildRun{
		ID: "run-2", Ticket: "T-2", Issue: "issue-8", Plan: "/target",
		State: orchestrator.StateDevLoop, ReviewID: "review-2", ReviewCommit: "C2",
	}
	if err := (orchestrator.SQLStore{DB: db}).Upsert(t.Context(), run); err != nil {
		t.Fatalf("seed: %v", err)
	}
	queue := queueForTest(t, db)
	_ = actOnVerdicts(t.Context(), db, workspace.Manager{}, agent.Tiers{},
		reviewbridge.Bridge{}, queue, []orchestrator.Routed{{
			Run: run, Reworked: true,
			Verdict: orchestrator.VerdictEvent{
				ID: "event-3", Review: "review-2", Session: "sess-2",
				Verdict: orchestrator.VerdictChangesRequested,
			},
		}}, "/target", "actor-1")

	if _, found, err := queue.AttemptFor(t.Context(), "run-2"); err != nil {
		t.Fatalf("attempt for: %v", err)
	} else if found {
		t.Error("rejected work was enqueued for merging")
	}
}

func TestAPoppedTicketFromAnotherTargetIsRefused(t *testing.T) {
	// A pop is identity-wide. Building another target's ticket here would run
	// THIS target's gate commands over the wrong codebase, against a snapshot
	// that never described it.
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := (planner.SQLTickets{DB: db}).Put(t.Context(), "/theirs",
		planner.Ticket{Title: "Add billing", IssueID: "issue-9", Plan: "plan-1", Ordinal: 9}); err != nil {
		t.Fatalf("put: %v", err)
	}
	build := buildOne(db, workspace.Manager{}, agent.Tiers{},
		reviewbridge.Bridge{}, "/mine", "actor-1")

	_, err := build(t.Context(), "issue-9", "Add billing")
	if err == nil {
		t.Fatal("another target's ticket was built here")
	}
	if !strings.Contains(err.Error(), "issue-9") || !strings.Contains(err.Error(), "/mine") {
		t.Errorf("the refusal does not name the issue and the target: %v", err)
	}
}

func TestAnotherTargetsVerdictIsLeftForItsOwnBuild(t *testing.T) {
	// The verdict cursor is actor-wide, but this queue's repository, snapshot
	// and target key belong to one target. Driving a foreign run here would
	// merge its commit into the wrong repository.
	db := openTemp(t)
	if err := applyMigrations(t.Context(), db, migrations()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	foreign := orchestrator.BuildRun{
		ID: "run-foreign", Ticket: "T-9", Issue: "issue-9", Plan: "/theirs",
		State: orchestrator.StateReviewSubmitted, ReviewID: "review-9",
		ReviewCommit: "C9", Branch: "kriya/T-9/zzzz",
	}
	if err := (orchestrator.SQLStore{DB: db}).Upsert(t.Context(), foreign); err != nil {
		t.Fatalf("seed: %v", err)
	}
	queue := queueForTest(t, db)
	if err := actOnVerdicts(t.Context(), db, workspace.Manager{}, agent.Tiers{},
		reviewbridge.Bridge{}, queue, []orchestrator.Routed{{
			Run: foreign, Revision: 1,
			Verdict: orchestrator.VerdictEvent{
				ID: "event-9", Review: "review-9", Session: "sess-9",
				Verdict: orchestrator.VerdictApproved,
			},
		}}, "/mine", "actor-1"); err != nil {
		t.Fatalf("act: %v", err)
	}
	if _, found, err := queue.AttemptFor(t.Context(), "run-foreign"); err != nil {
		t.Fatalf("attempt for: %v", err)
	} else if found {
		t.Error("another target's approval was enqueued in this target's queue")
	}
}

func TestEachTargetKeepsItsOwnVerdictCursor(t *testing.T) {
	// The feed is actor-wide and each target skips what is not its own. One
	// shared cursor would have the first target to read a page advance past
	// every other target's verdicts, which are then never offered again.
	db := openTemp(t)
	mine := verdictRouter(db, "actor-1", "/mine")
	theirs := verdictRouter(db, "actor-1", "/theirs")
	if mine.Name == theirs.Name {
		t.Fatalf("both targets consume under cursor %q", mine.Name)
	}
	// And the same target under the same actor keeps one position.
	if again := verdictRouter(db, "actor-1", "/mine"); again.Name != mine.Name {
		t.Errorf("the same target read under %q then %q", mine.Name, again.Name)
	}
}

// noTargets is a claim resolver for tests that never recover a claim.
func noTargets(planner.CompletionClaim) (string, string, error) { return "", "", nil }

// noWork is a work-event consumer for tests that never consume one.
func noWork(context.Context) error { return nil }

// noFindingProject resolves nothing, for tests that recover no findings.
func noFindingProject(orchestrator.BuildRun) (string, error) { return "", nil }
