// Package main is the composition root: it opens the store, applies each
// module's DDL, wires the modules together, runs recovery, and owns main().
//
// NOT a declared avspec module — avspec has no composition-root concept, yet
// arch-go requires 100% package coverage. Same gap sutra logged.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"strconv"

	"kriya/internal/agent"
	"kriya/internal/architect"
	"kriya/internal/cli"
	"kriya/internal/clock"
	kctx "kriya/internal/context"
	"kriya/internal/devloop"
	"kriya/internal/gates"
	"kriya/internal/orchestrator"
	"kriya/internal/owner"
	"kriya/internal/planner"
	"kriya/internal/recovery"
	"kriya/internal/reviewbridge"
	"kriya/internal/specverify"
	"kriya/internal/trackerclient"
	"kriya/internal/workspace"
)

// defaultDBPath is used when KRIYA_DB is unset. Configuration moves to
// kriya.toml with the rest of it.
const defaultDBPath = "kriya.db"

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "kriya:", err)
		os.Exit(1)
	}
}

// run opens the store and dispatches.
func run(ctx context.Context, args []string) error {
	db, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	if len(args) == 0 {
		return errors.New("usage: kriya build <project>")
	}
	switch args[0] {
	case "build":
		if len(args) != 2 {
			return errors.New("usage: kriya build <project>")
		}
		return build(ctx, db, args[1])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// dsn builds the connection string.
//
// foreign_keys is OFF by default in SQLite, so REFERENCES clauses in module
// schemas would parse and then enforce nothing. busy_timeout keeps parallel
// dev agents from failing outright on a momentarily locked database.
//
// Tests call this rather than repeating the pragmas: a test that rebuilt the
// same string would pass while production had none, proving only that the
// test agrees with itself.
func dsn(path string) string {
	return "file:" + path + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
}

// openStore opens the one database and applies each module's schema.
func openStore(ctx context.Context) (*sql.DB, error) {
	path := os.Getenv("KRIYA_DB")
	if path == "" {
		path = defaultDBPath
	}
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("open store %s: %w", path, err)
	}
	if err := applyMigrations(ctx, db, migrations()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// build wires the modules for one target, reconciles what a previous run left
// open, and runs the command.
func build(ctx context.Context, db *sql.DB, arg string) error {
	target, err := filepath.Abs(arg)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", arg, err)
	}
	actor := os.Getenv("KRIYA_ACTOR")
	// Validated BEFORE any mutation. sutra settles a rejected request under
	// its idempotency key — deliberately, so a different request cannot reuse
	// a key and mutate — so a request kriya knew was invalid would poison that
	// key permanently. The cure is not to send it.
	if _, err := uuid.Parse(actor); err != nil {
		return fmt.Errorf("KRIYA_ACTOR must be an identity uuid: %w", err)
	}
	tiers, err := loadTiers()
	if err != nil {
		return err
	}
	// Validated at startup: AC-tier-explicit wants a missing tier to fail
	// loudly, not halfway through a decomposition.
	if err := tiers.Validate(agent.RolePM); err != nil {
		return err
	}

	in := planner.Intaker{
		Verify:    verifier(),
		Snapshots: planner.SQLSnapshots{DB: db},
		Targets:   planner.SQLTargets{DB: db},
		Attempts:  planner.SQLAttempts{DB: db},
		Tracker:   sutraTracker{c: trackerclient.New(sutraURL())},
		Agent: agent.Recording{
			Inner:  agent.Claude{Tiers: tiers},
			Ledger: agent.Ledger{DB: db, Now: clock.System{}},
			Tiers:  tiers,
			Now:    clock.System{},
			Scope:  agent.Scope{Plan: target},
		},
		Now: clock.System{},
	}

	repo := os.Getenv("KRIYA_REPO")
	ws := workspace.Manager{
		Repo:          repo,
		Root:          filepath.Join(repo, ".kriya", "worktrees"),
		DefaultBranch: "main",
		Store:         workspace.SQLStore{DB: db},
		Git:           workspace.ShellGit{},
		Probe:         workspace.DirProbe{},
		Now:           clock.System{},
	}

	// Recovery runs BEFORE any new work, in declared stage order. Nothing pops
	// until every crash window a previous run left open is reconciled.
	reviews := reviewbridge.Bridge{
		Repo:  repo,
		Store: reviewbridge.SQLStore{DB: db},
		Rev:   reviewbridge.CLI{},
		Now:   clock.System{},
	}
	if err := recovery.Run(ctx,
		recoverySteps(in, ws, reviews, recoveryLoop(db), submitterOn(db, actor), actor)); err != nil {
		return err
	}
	// Each explicit run is a deliberate re-intake and allocates the next
	// generation. KRIYA_INTAKE_TOKEN names an earlier attempt to resume,
	// which is how a retry after a crash reuses its generation instead of
	// superseding itself.
	token := os.Getenv("KRIYA_INTAKE_TOKEN")
	if token == "" {
		token = uuid.NewString()
	}
	return cli.Build(ctx, os.Stdout, in, target, actor, token,
		driver(db, ws, tiers, reviews, repo, target, actor))
}

// driver returns a cli.Drive, or nil when no repository is configured.
//
// Nil rather than a stub: a build engine with nowhere to work should say so
// and stop, not create worktrees in whatever repository it happens to be
// standing in. KRIYA_REPO moves to the target's declared repo_path once
// intake reads it.
func driver(db *sql.DB, ws workspace.Manager, tiers agent.Tiers, reviews reviewbridge.Bridge, repo, target, actor string) cli.Drive {
	if repo == "" {
		return nil
	}
	return func(ctx context.Context, ticket planner.Ticket) (orchestrator.BuildRun, error) {
		snap, err := planner.SQLSnapshots{DB: db}.Get(ctx, latestSnapshotHash(ctx, db, target))
		if err != nil {
			return orchestrator.BuildRun{}, err
		}
		loop := devloop.Loop{
			Agent: recorder(db, tiers, ticket.Title),
			Store: devloop.SQLStore{DB: db},
			Context: kctx.Assembler{
				Store:     kctx.SQLBundles{DB: db},
				Learnings: kctx.SQLLearnings{DB: db},
				Now:       clock.System{},
			},
			Threads: sutraThreads{c: trackerclient.New(sutraURL())},
			Architect: architect.Architect{
				Agent: recorder(db, tiers, ticket.Title),
				Store: architect.SQLStore{DB: db},
				Now:   clock.System{},
			},
			Commit: workspace.ShellGit{},
			Review: reviews,
			Now:    clock.System{},
		}
		runner := gates.Runner{
			Store:  gates.SQLStore{DB: db},
			Review: reviewbridge.SQLStore{DB: db},
			Now:    clock.System{},
		}
		o := orchestrator.Orchestrator{
			Store: orchestrator.SQLStore{DB: db},
			Stages: buildStages(deps{
				ws: ws, loop: loop, runner: runner, snap: snap,
				po: owner.Owner{
					Agent: recorder(db, tiers, ticket.Title),
					Store: owner.SQLStore{DB: db},
					Gates: runner,
					Now:   clock.System{},
				},
				submitter:    submitterOn(db, actor),
				commandsFor:  commandsFromSnapshot(snap),
				criteriaFor:  criteriaFromTickets([]planner.Ticket{ticket}),
				issueFor:     issuesFromTickets([]planner.Ticket{ticket}),
				sessionFor:   sessionFromStore(db),
				instructions: operatorInstructions(target),
			}),
			Now: clock.System{},
		}
		run := orchestrator.BuildRun{
			ID: uuid.NewString(), Ticket: ticket.Title,
			Plan: target, State: orchestrator.StateQueued,
			// Snapshotted here, at creation. Changing KRIYA_ROUND_LIMIT later
			// affects only runs created after the change.
			RoundLimit: roundLimit(),
		}
		if err := o.Store.Upsert(ctx, run); err != nil {
			return orchestrator.BuildRun{}, err
		}
		return o.Drive(ctx, run.ID, 16)
	}
}

// operatorInstructions reads the target's hand-crafted project file.
//
// AC-context-instructions wants it VERBATIM, so it is read and passed through
// rather than summarised. Absent is not an error — a project may have none —
// but an unreadable one is silent context loss, so it is reported and the
// build continues with what it could read.
func operatorInstructions(target string) string {
	for _, name := range []string{"CLAUDE.md", "AGENTS.md"} {
		body, err := os.ReadFile(filepath.Join(target, name))
		if err == nil {
			return string(body)
		}
		if !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "kriya: cannot read %s: %v\n", name, err)
		}
	}
	return ""
}

// latestSnapshotHash reads the snapshot the newest intake mapped.
func latestSnapshotHash(ctx context.Context, db *sql.DB, target string) string {
	m, found, err := (planner.SQLAttempts{DB: db}).Mapping(ctx, target)
	if err != nil || !found {
		return ""
	}
	return m.SnapshotHash
}

// submitterOn opens reviews against the configured tracker.
func submitterOn(db *sql.DB, actor string) orchestrator.Submitter {
	return orchestrator.Submitter{
		Store:   orchestrator.SQLStore{DB: db},
		Reviews: sutraReviews{c: trackerclient.New(sutraURL())},
		Author:  actor,
	}
}

// recoveryLoop is the dev loop as recovery needs it.
//
// Only the seams an import replay touches: the session store and the thread
// importer. The agent, committer and reviewer belong to running work, and
// recovery starts none — handing it those would let a reconciliation step
// invoke an agent.
func recoveryLoop(db *sql.DB) devloop.Loop {
	return devloop.Loop{
		Store:   devloop.SQLStore{DB: db},
		Threads: sutraThreads{c: trackerclient.New(sutraURL())},
		Now:     clock.System{},
	}
}

// recoverySteps maps modules to the stages they reconcile.
//
// The whole ordering lives here, in the composition root, rather than being
// spread across the modules — which is the point of sequencing being a
// composition-root concern. Stages with no owner yet simply have no step; they
// gain one as their module lands.
func recoverySteps(
	in planner.Intaker, ws workspace.Manager, rb reviewbridge.Bridge,
	loop devloop.Loop, submitter orchestrator.Submitter, actor string,
) []recovery.Step {
	return []recovery.Step{
		{Stage: recovery.StageTargets, Owner: "planner", Run: func(ctx context.Context) error {
			_, err := in.RecoverTargets(ctx)
			return err
		}},
		{Stage: recovery.StageWorkspaces, Owner: "workspace", Run: func(ctx context.Context) error {
			_, err := ws.Recover(ctx)
			return err
		}},
		{Stage: recovery.StageDevSessions, Owner: "devloop", Run: func(ctx context.Context) error {
			// An import that may have landed is replayed under its persisted
			// key. sutra returns the original thread for one that did and
			// creates otherwise, so exactly one thread exists either way.
			_, err := loop.RecoverImports(ctx, "", actor)
			return err
		}},
		{Stage: recovery.StageMerges, Owner: "orchestrator", Run: func(ctx context.Context) error {
			// Replayed from the PERSISTED fields under the persisted key, so
			// a branch that moved cannot smuggle an ungated commit in and a
			// fresh session cannot displace the one feedback routes to.
			if _, err := submitter.RecoverSubmissions(ctx,
				func(run orchestrator.BuildRun) orchestrator.Submission {
					return orchestrator.Submission{
						Issue: run.Ticket, Session: run.ReviewSession, Summary: run.Ticket,
					}
				}); err != nil {
				return err
			}
			_, err := submitter.RecoverResubmissions(ctx,
				func(run orchestrator.BuildRun) orchestrator.Rework {
					return orchestrator.Rework{
						Session: run.ReviewSession, Summary: run.Ticket,
						Revision: run.ReviewRevision, VerdictEvent: run.ReviewVerdictEvent,
					}
				})
			return err
		}},
		{Stage: recovery.StageReviewRounds, Owner: "reviewbridge", Run: func(ctx context.Context) error {
			stuck, err := rb.Recover(ctx)
			if err != nil {
				return err
			}
			// Surfaced, not resolved: kriya cannot prove which roborev job an
			// ambiguous attempt created, and one of them must not block every
			// future build.
			for _, a := range stuck {
				fmt.Fprintf(os.Stderr,
					"kriya: review round %s on %s is unresolved (%s); check roborev\n",
					a.Round, a.Commit, a.Note)
			}
			// Rounds left mid-response ARE reconcilable — the payload was
			// persisted before the call — so unlike an ambiguous enqueue they
			// are finished rather than surfaced.
			_, err = rb.RecoverRounds(ctx)
			return err
		}},
	}
}

// verifier decides how to invoke avspec.
//
// The default is plain `avspec` on PATH, which fails with a clear "executable
// not found" if it is not installed. An earlier version always used
// `uv run avspec`, which resolves its environment from the WORKING DIRECTORY —
// so the advertised command worked only when run from a directory where uv
// could find the avspec project, and failed obscurely everywhere else.
//
// KRIYA_AVSPEC_DIR names a uv project holding avspec, for a checkout that has
// not installed it.
func verifier() specverify.CLI {
	if dir := os.Getenv("KRIYA_AVSPEC_DIR"); dir != "" {
		return specverify.CLI{Argv: []string{"uv", "run", "avspec"}, WorkDir: dir}
	}
	return specverify.CLI{Argv: []string{"avspec"}}
}

// recorder wraps the claude seam in the invocation ledger.
//
// Every role gets the same wrapper: AC-tier-observed wants the model that
// ACTUALLY ran recorded for each invocation, and a role wired without the
// ledger would spend tokens nothing accounted for.
func recorder(db *sql.DB, tiers agent.Tiers, build string) agent.Recording {
	return agent.Recording{
		Inner:  agent.Claude{Tiers: tiers},
		Ledger: agent.Ledger{DB: db, Now: clock.System{}},
		Tiers:  tiers,
		Now:    clock.System{},
		Scope:  agent.Scope{Build: build},
	}
}

// roundLimit is the configured pair-loop round limit.
//
// Read once per run and snapshotted onto it. An unparseable value is reported
// and the default stands: a build engine that refused to start over a typo in
// an optional tuning knob would be worse than one that says so.
func roundLimit() int {
	raw := os.Getenv("KRIYA_ROUND_LIMIT")
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		fmt.Fprintf(os.Stderr, "kriya: ignoring KRIYA_ROUND_LIMIT=%q\n", raw)
		return 0
	}
	return n
}

// sutraURL is where the tracker lives.
func sutraURL() string {
	if u := os.Getenv("KRIYA_SUTRA_URL"); u != "" {
		return u
	}
	return "http://127.0.0.1:7357"
}

// loadTiers reads the role-to-model configuration.
//
// No model identifier appears in kriya's source (AC-tier-config).
func loadTiers() (agent.Tiers, error) {
	path := os.Getenv("KRIYA_TIERS")
	if path == "" {
		return agent.Tiers{}, errors.New("KRIYA_TIERS is unset: no role tiers configured")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return agent.Tiers{}, fmt.Errorf("read tiers %s: %w", path, err)
	}
	var t agent.Tiers
	if err := json.Unmarshal(raw, &t); err != nil {
		return agent.Tiers{}, fmt.Errorf("parse tiers %s: %w", path, err)
	}
	return t, nil
}
