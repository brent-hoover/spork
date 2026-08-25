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
	"flag"
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
		return errors.New(
			"usage: kriya build <project> | kriya status [--json] | kriya learn add [flags]")
	}
	switch args[0] {
	case "build":
		if len(args) != 2 {
			return errors.New("usage: kriya build <project>")
		}
		return build(ctx, db, args[1])
	case "status":
		return status(ctx, db, args[1:])
	case "learn":
		return learn(ctx, db, args[1:])
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

// intaker wires the planner for one target.
func intaker(db *sql.DB, tiers agent.Tiers, target string) planner.Intaker {
	return planner.Intaker{
		Verify:    verifier(),
		Snapshots: planner.SQLSnapshots{DB: db},
		Targets:   planner.SQLTargets{DB: db},
		Attempts:  planner.SQLAttempts{DB: db},
		Tracker:   sutraTracker{c: trackerclient.New(sutraURL())},
		Tickets:   planner.SQLTickets{DB: db},
		Plans:     planner.SQLPlans{DB: db},
		Agent: agent.Recording{
			Inner:  agent.Claude{Tiers: tiers},
			Ledger: agent.Ledger{DB: db, Now: clock.System{}},
			Tiers:  tiers,
			Now:    clock.System{},
			Scope:  agent.Scope{Plan: target},
		},
		Now: clock.System{},
	}
}

// learn is the `kriya learn` command surface.
//
// One subcommand today. It is a subcommand rather than a flag on build because
// adding a learning is not part of a build: an operator writes one from their
// own experience, at a moment of their choosing.
func learn(ctx context.Context, db *sql.DB, args []string) error {
	if len(args) == 0 || args[0] != "add" {
		return errors.New("usage: kriya learn add --module M --pattern P --lesson L [--project KEY]")
	}
	flags := flag.NewFlagSet("learn add", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	var (
		lesson  = flags.String("lesson", "", "what was learned, and how to avoid it")
		module  = flags.String("module", "", "the module the lesson concerns")
		pattern = flags.String("pattern", "", "the failure pattern the lesson concerns")
		project = flags.String("project", "", "scope to this project; omit for a cross-project lesson")
	)
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	// The scope follows the project key rather than being asked for
	// separately: "which project" and "is it project-scoped" are one decision,
	// and two flags could contradict each other.
	scope := kctx.ScopeGlobal
	if *project != "" {
		scope = kctx.ScopeProject
	}
	return cli.Learn(ctx, os.Stdout, kctx.SQLLearnings{DB: db, Now: clock.System{}},
		kctx.Capture{
			Scope: scope, ProjectKey: *project, Lesson: *lesson,
			Module: *module, Pattern: *pattern, SourceKind: kctx.SourceOperator,
		})
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

	in := intaker(db, tiers, target)

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
	// The repository-touching stages are SKIPPED when none is configured.
	// Every one of them shells out to git with the manager's Repo as the
	// working directory, and an empty one is the process's own — so recovering
	// a pending workspace would cut branches and worktrees in whatever
	// repository kriya happened to be launched from. Only the target
	// reconciliation, which touches the tracker and this database, is
	// repository-independent.
	steps := recoverySteps(in, ws, reviews, recoveryLoop(db), submitterOn(db, actor),
		mergeQueue(db, ws, actor), completerOn(db, ws, actor), actor,
		completionClaimer(db, actor, target), targetsFor(db),
		func(ctx context.Context) error { return consumeWork(ctx, db, target, actor) },
		target)
	if repo == "" {
		steps = repositoryIndependent(steps)
	}
	if err := recovery.Run(ctx, steps); err != nil {
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

// recoverInFlight replays everything a crash left mid-flight between an
// approval and a closed ticket.
//
// Every replay is built from the RUN's persisted fields, never re-derived: a
// replay must reproduce the original request, and a re-derived one could act
// on a different issue, a moved branch, or a fresh session that feedback does
// not route to.
func recoverInFlight(
	queue orchestrator.Queue, submitter orchestrator.Submitter, completer orchestrator.Completer,
) func(context.Context) error {
	return func(ctx context.Context) error {
		// Attempts first: a merge left mid-flight holds the serialized
		// section, and a submission replayed ahead of it would open a review
		// for work that is already landing.
		if _, err := queue.RecoverMerges(ctx); err != nil {
			return err
		}
		if _, err := submitter.RecoverSubmissions(ctx,
			func(run orchestrator.BuildRun) orchestrator.Submission {
				return orchestrator.Submission{
					Issue: run.Issue, Branch: run.Branch,
					Session: run.ReviewSession, Summary: run.Ticket,
				}
			}); err != nil {
			return err
		}
		if _, err := completer.RecoverCompletions(ctx,
			func(run orchestrator.BuildRun) orchestrator.Completion {
				return orchestrator.Completion{
					Issue: run.Issue, Branch: run.Branch, Merged: run.CompletedHead,
				}
			}); err != nil {
			return err
		}
		_, err := submitter.RecoverResubmissions(ctx,
			func(run orchestrator.BuildRun) orchestrator.Rework {
				return orchestrator.Rework{
					Branch: run.Branch, Session: run.ReviewSession, Summary: run.Ticket,
					Revision: run.ReviewRevision, VerdictEvent: run.ReviewVerdictEvent,
				}
			})
		return err
	}
}

// repositoryIndependent keeps only the recovery steps that need no worktree.
//
// The target stage reconciles the tracker and this database. Every other stage
// runs git in the configured repository, and there is none.
func repositoryIndependent(steps []recovery.Step) []recovery.Step {
	var out []recovery.Step
	for _, step := range steps {
		if step.Stage == recovery.StageTargets {
			out = append(out, step)
		}
	}
	return out
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
	// The pop loop is the outer loop: it claims the next workable ticket and
	// builds it, until the tracker has nothing to give. Idling is not exiting.
	return func(ctx context.Context) (orchestrator.Result, error) {
		// Verdicts first. A changes-requested review returns its run to the
		// pair loop, and a pass that popped new work before reading them would
		// leave a rejected run waiting behind tickets it does not need.
		// Work returning FIRST. A ticket that reopened invalidates any
		// completion claim, and a pass that popped or claimed before reading
		// them would act on a target it believes is finished.
		if err := consumeWork(ctx, db, target, actor); err != nil {
			return orchestrator.Result{}, err
		}
		router := verdictRouter(db, actor, target)
		routed, next, err := router.Consume(ctx)
		if err != nil {
			return orchestrator.Result{}, err
		}
		// ACTED ON before the cursor moves. The feed never offers a consumed
		// page again, so a verdict routed and then dropped is a run that waits
		// forever — approved work that never merges, rework that never
		// restarts. Re-reading a page is the safe direction: every act these
		// events lead to is keyed.
		if err := actOnVerdicts(ctx, db, ws, tiers, reviews,
			mergeQueue(db, ws, actor), routed, target, actor); err != nil {
			return orchestrator.Result{}, err
		}
		if err := router.Advance(ctx, next); err != nil {
			return orchestrator.Result{}, err
		}
		result, err := orchestrator.Loop{
			Pops:      sutraPops{c: trackerclient.New(sutraURL()), identity: actor},
			Build:     buildOne(db, ws, tiers, reviews, target, actor),
			Ordinals:  orchestrator.SQLOrdinals{DB: db},
			TargetKey: target,
			// Asked at the idle: nothing workable is either a build about to
			// finish or one that is stuck, and they look identical from the
			// loop. The answer is durable either way.
			Finish: completionDetector(db),
			Stalls: stallRecorder(db),
			Epochs: planner.Epochs{Store: planner.SQLAdvances{DB: db}},
		}.Run(ctx)
		if err != nil || !result.Idle {
			return result, err
		}
		// Idle AND armed is the finish line. The completion review is
		// submitted here rather than inside the loop because the loop's job is
		// to pop and build: whether the build is DONE is a different question,
		// and it is asked once, when there is nothing left to pop.
		if err := claimCompletion(ctx, db, target, actor); err != nil {
			return result, err
		}
		// The review is open. Look once: if the human has already approved
		// it, the epic closes and the target stamps. POLLING, and only here —
		// the event-driven consumption is the feed's, and a poll that observes
		// the same approval issues the same close under the same key.
		return result, observeCompletionApproval(ctx, db, target, actor)
	}
}

// claimCompletion submits the build-completion review when a target is armed.
//
// Every crash window in the protocol is recovered by the same path a fresh
// submission takes, so this is only ever the FIRST attempt: recovery drives
// the rest from the persisted claim.
func claimCompletion(ctx context.Context, db *sql.DB, target, actor string) error {
	// The cheapest question first, and the one that short-circuits hardest:
	// an attempt already in flight or settled FOR THIS EPOCH needs no tracker
	// round-trip at all, and a second submission would open a second review
	// for one claim.
	current, err := claimIsCurrent(ctx, db, target)
	if err != nil {
		return err
	}
	if current {
		return nil
	}
	got, err := completionDetector(db).Detection(ctx, target)
	if err != nil || !got.Armed {
		return err
	}
	row, found, err := (planner.SQLTargets{DB: db}).Find(ctx, target)
	if err != nil || !found {
		return err
	}
	// The epic's revision at CAPTURE. The close is fenced on it, so history
	// that moved invalidates the claim even when current state still matches.
	epic, err := trackerclient.New(sutraURL()).GetIssue(ctx, row.EpicID)
	if err != nil {
		return fmt.Errorf("read epic %s: %w", row.EpicID, err)
	}
	// The epoch and the WATERMARK the detection was about. Re-reading either
	// here would bind the claim to state nothing checked for completion.
	_, err = completionClaimer(db, actor, target).Submit(
		ctx, target, row.ProjectID, row.EpicID,
		got.Epoch, epic.SubtreeRevision, got.Watermark)
	return err
}

// observeCompletionApproval closes the epic when its review is approved.
//
// Nothing happens on any other verdict: a completion review awaiting a human,
// or one sent back for rework, leaves the target exactly where it is.
func observeCompletionApproval(ctx context.Context, db *sql.DB, target, actor string) error {
	claim, found, err := (planner.SQLClaims{DB: db}).Find(ctx, target)
	if err != nil || !found {
		return err
	}
	if !awaitsClose(claim) {
		return nil
	}
	rv, err := trackerclient.New(sutraURL()).GetReview(ctx, claim.ReviewID)
	if err != nil {
		return fmt.Errorf("read completion review %s: %w", claim.ReviewID, err)
	}
	if rv.State != "approved" {
		return nil
	}
	row, found, err := (planner.SQLTargets{DB: db}).Find(ctx, target)
	if err != nil || !found {
		return err
	}
	_, err = completionClaimer(db, actor, target).Close(ctx, target, row.EpicID, rv.LatestVerdictEvent)
	if errors.Is(err, planner.ErrStaleClaim) {
		// The approval was real and it covered a build that is no longer this
		// one. The operator hears about it rather than the build silently
		// declaring itself done or silently stopping.
		fmt.Fprintf(os.Stderr,
			"kriya: %s was approved at completion epoch %d, which has since moved; "+
				"the epic closed but nothing is stamped — a fresh completion review is required\n",
			target, claim.Epoch)
		return nil
	}
	return err
}

// awaitsClose reports whether a claim is waiting on a human.
//
// SUBMITTED claims are polled for their first approval; CLOSING ones are too,
// because Close persists that state before calling sutra — a close that
// errored rests there and needs either its key replayed or a reapproval
// observed under a new verdict event. Polling only submitted claims left one
// stranded with nothing to resume it.
func awaitsClose(claim planner.CompletionClaim) bool {
	return claim.State == planner.CompletionSubmitted ||
		claim.State == planner.CompletionClosing
}

// claimIsCurrent reports whether a completion attempt for THIS epoch stands.
//
// The epoch comparison is the whole of it. A claim settled at epoch 3 says
// nothing about epoch 4: work came back and was redone, and the human's old
// approval was for a smaller build. Treating any claim as final would leave
// the target complete-looking forever with new work merged into it — which is
// exactly what "recompletion needs a fresh review" forbids.
func claimIsCurrent(ctx context.Context, db *sql.DB, target string) (bool, error) {
	claim, found, err := (planner.SQLClaims{DB: db}).Find(ctx, target)
	if err != nil {
		return false, err
	}
	if !found || claim.State == planner.CompletionNone {
		return false, nil
	}
	epoch, err := (planner.Epochs{Store: planner.SQLAdvances{DB: db}}).Current(ctx, target)
	if err != nil {
		return false, err
	}
	return claim.Epoch == epoch, nil
}

// targetsFor resolves a claim's project and epic from the target's own row.
//
// From the ROW, not the caller: a replay must reproduce the original request,
// and a project re-derived somewhere else is a second place for the two to
// disagree.
func targetsFor(db *sql.DB) func(planner.CompletionClaim) (string, string, error) {
	return func(claim planner.CompletionClaim) (string, string, error) {
		row, found, err := (planner.SQLTargets{DB: db}).Find(
			context.Background(), claim.TargetKey)
		if err != nil {
			return "", "", err
		}
		if !found {
			return "", "", fmt.Errorf("no build target for claim %s", claim.TargetKey)
		}
		return row.ProjectID, row.EpicID, nil
	}
}

// completionClaimer wires the build-completion claim protocol.
func completionClaimer(db *sql.DB, actor, target string) planner.Claimer {
	c := trackerclient.New(sutraURL())
	return planner.Claimer{
		// Wired HERE, not by the caller. Every ticket completing during a
		// build emits a status change, and a caller who forgot this seam
		// would have each of them read as a reopen the moment the claim
		// existed — so no build could ever complete. The epic is not needed:
		// the sync only moves a cursor.
		Work:      workWatcher(db, planner.BuildTarget{}, target, actor),
		Claims:    planner.SQLClaims{DB: db},
		Reports:   reportFor(db),
		Documents: sutraDocs{c: c, actor: actor},
		Reviews:   sutraCompletionReviews{c: c, actor: actor},
		Epics:     sutraEpics{c: c, actor: actor},
		Epochs:    planner.Epochs{Store: planner.SQLAdvances{DB: db}},
	}
}

// actOnVerdicts drives every run a consumed verdict moved.
//
// A rework is already in dev-loop and only needs driving. An approval is the
// merge queue's business: it is enqueued under the approval event's own key,
// so the same event observed twice enqueues once.
func actOnVerdicts(
	ctx context.Context, db *sql.DB, ws workspace.Manager, tiers agent.Tiers,
	reviews reviewbridge.Bridge, queue orchestrator.Queue,
	routed []orchestrator.Routed, target, actor string,
) error {
	// This target's runs only. The cursor is actor-wide, but this queue's
	// repository, snapshot and target key belong to one target — driving a
	// foreign run here would merge its commit into the wrong repository. Its
	// own build reads the same feed.
	var mine []orchestrator.Routed
	for _, r := range routed {
		if r.Run.Plan == target {
			mine = append(mine, r)
		}
	}
	if len(mine) == 0 {
		return nil
	}

	// Approvals are made DURABLE FIRST, every one of them, before anything is
	// driven. The cursor has already advanced past these events and will never
	// offer them again, so an approval still only in memory when a later step
	// fails is one nothing can recover: the run waits on a merge nobody
	// enqueued.
	for i, r := range mine {
		if r.Reworked {
			// Back in the pair loop. There is nothing to merge, and enqueuing
			// one would try to land work the human rejected.
			continue
		}
		// The revision the ROUTER read from the review. An initial submission
		// records none on the run, so taking it from there enqueued every
		// approval at revision zero and the tracker refused the consumption.
		moved, _, err := queue.OnApproval(ctx, r.Run, orchestrator.Approved{
			Review: r.Verdict.Review, Revision: r.Revision,
			Event: r.Verdict.ID, Commit: r.Run.ReviewCommit, TargetKey: target,
		})
		if err != nil {
			return err
		}
		mine[i].Run = moved
	}

	snap, err := snapshotFor(ctx, db, target)
	if err != nil {
		return err
	}
	for _, r := range mine {
		o := orchestrator.Orchestrator{
			Store: orchestrator.SQLStore{DB: db},
			Stages: buildStages(stageDeps(db, ws,
				loopFor(db, tiers, reviews, r.Run.Ticket),
				tiers, planner.Ticket{Title: r.Run.Ticket, IssueID: r.Run.Issue},
				snap, target, actor)),
			Now: clock.System{},
		}
		if _, err := o.Drive(ctx, r.Run.ID, 16); err != nil {
			return err
		}
	}
	return nil
}

// completionDetector answers whether a target's build may attempt to finish.
//
// The project id comes from the target's own row rather than the caller: the
// detector is asked about a TARGET, and re-deriving the project somewhere else
// is a second place for the two to disagree.
func completionDetector(db *sql.DB) buildFinish {
	return buildFinish{
		targets: planner.SQLTargets{DB: db},
		detect: planner.Detector{
			Plans:   planner.SQLPlans{DB: db},
			Tickets: planner.SQLTickets{DB: db},
			Issues:  sutraIssues{c: trackerclient.New(sutraURL())},
			Epochs:  &planner.Epochs{Store: planner.SQLAdvances{DB: db}},
		},
	}
}

// buildFinish adapts the detector to the pop loop's seam.
type buildFinish struct {
	targets planner.SQLTargets
	detect  planner.Detector
}

func (f buildFinish) Detect(ctx context.Context, targetKey string) (bool, string, error) {
	got, err := f.Detection(ctx, targetKey)
	if err != nil {
		return false, "", err
	}
	return got.Armed, got.Reason, nil
}

// Detection is the whole answer, including the epoch a claim must bind to.
func (f buildFinish) Detection(
	ctx context.Context, targetKey string,
) (planner.Detection, error) {
	target, found, err := f.targets.Find(ctx, targetKey)
	if err != nil {
		return planner.Detection{}, err
	}
	if !found {
		// Nothing has been planned for it. Not armed, and not a stall either
		// — the operator has not started this build.
		return planner.Detection{}, nil
	}
	// The target's own epic, excused from its own completion check: it is
	// open for exactly as long as the build runs, so counting it as
	// outstanding work would make completion unable to arm at all.
	detect := f.detect
	detect.Epic = target.EpicID
	return detect.Detect(ctx, targetKey, target.ProjectID)
}

// stallRecorder records a build that can neither proceed nor finish.
func stallRecorder(db *sql.DB) stallWriter {
	return stallWriter{s: orchestrator.Stalls{
		Store: orchestrator.SQLStalls{DB: db}, Now: clock.System{},
	}}
}

// stallWriter adapts the stall recorder to the pop loop's seam, which wants
// only the error: the row itself is the inbox's business, not the loop's.
type stallWriter struct{ s orchestrator.Stalls }

func (w stallWriter) Record(ctx context.Context, targetKey string, epoch int, cause string) error {
	_, err := w.s.Record(ctx, targetKey, epoch, cause)
	return err
}

// consumeWork advances a target's completion epoch when its work returns.
//
// Nothing else moves the epoch, and without it a stamped target stays complete
// forever: a stale approval is never fenced and recompletion never happens.
func consumeWork(ctx context.Context, db *sql.DB, target, actor string) error {
	row, found, err := (planner.SQLTargets{DB: db}).Find(ctx, target)
	if err != nil || !found {
		return err
	}
	w := workWatcher(db, row, target, actor)
	_, next, err := w.Consume(ctx)
	if err != nil {
		return err
	}
	// The cursor moves only after every advance in the page landed. Losing an
	// event here is losing the fact that work came back.
	return w.Advance(ctx, next)
}

// workWatcher wires the epoch's feed consumer.
func workWatcher(
	db *sql.DB, row planner.BuildTarget, target, actor string,
) planner.Work {
	c := trackerclient.New(sutraURL())
	return planner.Work{
		Feed:      sutraWorkFeed{c: c},
		Epochs:    planner.Epochs{Store: planner.SQLAdvances{DB: db}},
		Tickets:   planner.SQLTickets{DB: db},
		Claims:    planner.SQLClaims{DB: db},
		Cursors:   orchestrator.SQLCursors{DB: db},
		TargetKey: target, Epic: row.EpicID, Actor: actor,
	}
}

// verdictRouter consumes review verdicts and routes each to its run.
//
// The cursor is named for the actor AND the target. The feed is actor-wide and
// each target skips what is not its own — so one shared cursor would have the
// first target to read a page advance past every other target's verdicts,
// which are then never offered again. Each target keeps its own position and
// reads the whole feed.
func verdictRouter(db *sql.DB, actor, target string) orchestrator.Router {
	return orchestrator.Router{
		Feed:    sutraFeed{c: trackerclient.New(sutraURL())},
		Cursors: orchestrator.SQLCursors{DB: db},
		Routes:  orchestrator.SQLStore{DB: db},
		Reviews: sutraRevisions{c: trackerclient.New(sutraURL())},
		Store:   orchestrator.SQLStore{DB: db},
		Name:    "verdicts:" + actor + ":" + target,
	}
}

// loopFor wires the pair loop for one ticket.
//
// Extracted because the pop loop and the verdict router both need it: a run
// returning from a changes-requested verdict re-enters the dev loop, and a
// second wiring of the same collaborators would be a second thing to keep in
// step with this one.
func loopFor(
	db *sql.DB, tiers agent.Tiers, reviews reviewbridge.Bridge, title string,
) devloop.Loop {
	return devloop.Loop{
		Agent: recorder(db, tiers, title),
		Store: devloop.SQLStore{DB: db},
		Context: kctx.Assembler{
			Store:     kctx.SQLBundles{DB: db},
			Learnings: kctx.SQLLearnings{DB: db, Now: clock.System{}},
			Now:       clock.System{},
		},
		Learnings: kctx.SQLLearnings{DB: db, Now: clock.System{}},
		Threads:   sutraThreads{c: trackerclient.New(sutraURL())},
		Architect: architect.Architect{
			Agent: recorder(db, tiers, title),
			Store: architect.SQLStore{DB: db},
			Now:   clock.System{},
		},
		Commit: workspace.ShellGit{},
		Review: reviews,
		Now:    clock.System{},
	}
}

// snapshotFor reads the target's pinned snapshot.
func snapshotFor(ctx context.Context, db *sql.DB, target string) (planner.Snapshot, error) {
	return planner.SQLSnapshots{DB: db}.Get(ctx, latestSnapshotHash(ctx, db, target))
}

// buildOne runs a single popped ticket to a terminal state.
func buildOne(
	db *sql.DB, ws workspace.Manager, tiers agent.Tiers, reviews reviewbridge.Bridge,
	target, actor string,
) orchestrator.Builder {
	return func(ctx context.Context, issue, title string) (orchestrator.BuildRun, error) {
		// A pop is IDENTITY-WIDE: sutra offers whatever is assigned to the
		// popping identity, from any plan. A ticket this target's
		// decomposition did not produce belongs to another repository, and
		// building it here would run this target's gate commands over the
		// wrong codebase. Refused loudly rather than skipped: the claim is
		// already made, and silently dropping it would strand the ticket.
		ticket, planned, err := (planner.SQLTickets{DB: db}).Find(ctx, target, issue)
		if err != nil {
			return orchestrator.BuildRun{}, err
		}
		if !planned {
			return orchestrator.BuildRun{}, fmt.Errorf(
				"issue %s (%s) is not in %s's plan; it was popped for this identity "+
					"but belongs to another target", issue, title, target)
		}
		ticket.IssueID = issue
		snap, err := snapshotFor(ctx, db, target)
		if err != nil {
			return orchestrator.BuildRun{}, err
		}
		o := orchestrator.Orchestrator{
			Store: orchestrator.SQLStore{DB: db},
			Stages: buildStages(stageDeps(db, ws,
				loopFor(db, tiers, reviews, ticket.Title),
				tiers, ticket, snap, target, actor)),
			Now: clock.System{},
		}
		run, err := resumeOrStart(ctx, o.Store, ticket, target)
		if err != nil {
			return orchestrator.BuildRun{}, err
		}
		settled, err := o.Drive(ctx, run.ID, 16)
		if err != nil || settled.State != orchestrator.StateReviewSubmitted {
			return settled, err
		}
		// The run is waiting on a human. Look once: if the verdict is already
		// in, the approval is enqueued and the table takes over again.
		// POLLING, deliberately, and only here — the event-driven consumption
		// REQ-run-to-complete describes is M5's, and a poll that observes the
		// same approval enqueues the same attempt under the same key.
		// The target path scopes the queue. It is stable for the life of a
		// target, which is all the attempt key needs of it.
		merged, err := observeApproval(ctx, o, mergeQueue(db, ws, actor),
			trackerclient.New(sutraURL()), settled, target)
		if err != nil || merged.CompletionState != orchestrator.CompleteClosed {
			return merged, err
		}
		// Closed. An out-of-band commit landing on the branch now is work
		// nothing reviewed, and the operator hears about it rather than kriya
		// merging it silently.
		return reportAdvance(ctx, completerOn(db, ws, actor), ws, merged)
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

// mergeQueue lands approved work against the target repository.
func mergeQueue(db *sql.DB, ws workspace.Manager, actor string) orchestrator.Queue {
	return orchestrator.Queue{
		Store:     orchestrator.SQLAttempts{DB: db},
		Runs:      orchestrator.SQLStore{DB: db},
		Approvals: sutraApprovals{c: trackerclient.New(sutraURL())},
		Git:       repoMerger{git: workspace.ShellGit{}, repo: ws.Repo, branch: ws.DefaultBranch},
		// The repository and branch the CAS targets: two targets sharing them
		// share a queue head, because that is what is actually contended.
		Resource: ws.Repo + "#" + ws.DefaultBranch,
		Actor:    actor,
	}
}

// completerOn closes tickets against the configured tracker.
func completerOn(db *sql.DB, ws workspace.Manager, actor string) orchestrator.Completer {
	return orchestrator.Completer{
		Store:    orchestrator.SQLStore{DB: db},
		Tickets:  sutraTickets{c: trackerclient.New(sutraURL()), actor: actor},
		Branches: branchHeads{git: workspace.ShellGit{}, repo: ws.Repo},
		Sessions: sessionEnder{db: db},
		Actor:    actor,
	}
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
	loop devloop.Loop, submitter orchestrator.Submitter,
	queue orchestrator.Queue, completer orchestrator.Completer, actor string,
	claimer planner.Claimer, targets func(planner.CompletionClaim) (string, string, error),
	work func(context.Context) error, target string,
) []recovery.Step {
	return []recovery.Step{
		{Stage: recovery.StageTargets, Owner: "planner", Run: func(ctx context.Context) error {
			if _, err := in.RecoverTargets(ctx); err != nil {
				return err
			}
			// A completion submission a crash left in flight, replayed under
			// its PERSISTED keys. The claim is epoch-scoped, so a replay after
			// an advance still presents the old key — which is what keeps it
			// from adopting a spent review — and the stamp's CAS refuses it
			// later.
			// Work returning FIRST. A close that cached a conflict after a
			// reopen would otherwise be replayed here with its epoch still
			// unchanged: the conflict comes back, recovery fails, and the
			// advance that would have made the claim stale never runs.
			if err := work(ctx); err != nil {
				return err
			}
			if _, err := claimer.Recover(ctx, targets); err != nil {
				return err
			}
			// And a close a crash left mid-flight, replayed under its
			// PERSISTED key: a close that landed returns its original
			// success, never a close-used conflict, because that very key is
			// what stamped it.
			_, err := claimer.RecoverCloses(ctx, target, func(c planner.CompletionClaim) (string, error) {
				_, epic, err := targets(c)
				return epic, err
			})
			if errors.Is(err, planner.ErrStaleClaim) {
				// Recovered, and stale. Reported rather than fatal: the close
				// landed and every other target's was replayed too, and this
				// one needs a fresh review rather than a halted startup.
				fmt.Fprintln(os.Stderr, "kriya:", err)
				return nil
			}
			if err != nil {
				// A close that cannot be replayed — a cached conflict from a
				// reversed approval, say — must not halt startup either. The
				// claim rests in closing, and the driver re-polls it: a
				// reapproval rotates the key and the retry escapes the cache.
				fmt.Fprintln(os.Stderr, "kriya: completion close deferred:", err)
			}
			return nil
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
		{Stage: recovery.StageMerges, Owner: "orchestrator",
			Run: recoverInFlight(queue, submitter, completer)},
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

// stageDeps wires every collaborator a run's stages delegate to.
func stageDeps(
	db *sql.DB, ws workspace.Manager, loop devloop.Loop, tiers agent.Tiers,
	ticket planner.Ticket, snap planner.Snapshot, target, actor string,
) deps {
	runner := gates.Runner{
		Store:  gates.SQLStore{DB: db},
		Review: reviewbridge.SQLStore{DB: db},
		// The chain checks before every gate that the ground has not moved.
		Base: repoMerger{git: workspace.ShellGit{}, repo: ws.Repo, branch: ws.DefaultBranch},
		Now:  clock.System{},
	}
	return deps{
		ws: ws, loop: loop, runner: runner, snap: snap,
		po: owner.Owner{
			Agent: recorder(db, tiers, ticket.Title),
			Store: owner.SQLStore{DB: db},
			Gates: runner,
			Now:   clock.System{},
		},
		submitter:    submitterOn(db, actor),
		learnings:    kctx.SQLLearnings{DB: db, Now: clock.System{}},
		completer:    completerOn(db, ws, actor),
		queue:        mergeQueue(db, ws, actor),
		commandsFor:  commandsFromSnapshot(snap),
		ticketFor:    ticketFromStore(db, target, ticket),
		actor:        actor,
		sessionFor:   sessionFromStore(db),
		instructions: operatorInstructions(target),
	}
}

// observeApproval enqueues an approval that has already arrived.
//
// Nothing happens unless the review is approved: any other verdict leaves the
// run exactly where it was, waiting, which is what a submitted run does.
func observeApproval(
	ctx context.Context, o orchestrator.Orchestrator, queue orchestrator.Queue,
	reviews *trackerclient.Client, run orchestrator.BuildRun, targetKey string,
) (orchestrator.BuildRun, error) {
	rv, err := reviews.GetReview(ctx, run.ReviewID)
	if err != nil {
		return run, fmt.Errorf("read review %s: %w", run.ReviewID, err)
	}
	if rv.State != "approved" {
		return run, nil
	}
	moved, _, err := queue.OnApproval(ctx, run, orchestrator.Approved{
		Review: rv.ID, Revision: rv.Revision, Event: rv.LatestVerdictEvent,
		Commit: run.ReviewCommit, TargetKey: targetKey,
	})
	if err != nil {
		return run, err
	}
	return o.Drive(ctx, moved.ID, 4)
}

// reportAdvance surfaces a commit that landed after the ticket closed.
//
// Detected against the RECORDED head, not against anything re-derived: the
// point is that the branch is no longer where completion left it.
func reportAdvance(
	ctx context.Context, c orchestrator.Completer, ws workspace.Manager,
	run orchestrator.BuildRun,
) (orchestrator.BuildRun, error) {
	w, found, err := ws.Store.Find(ctx, run.ID)
	if err != nil || !found {
		return run, err
	}
	advanced, err := c.Advanced(ctx, run, w.Branch)
	if err != nil {
		return run, err
	}
	if advanced {
		fmt.Fprintf(os.Stderr,
			"kriya: %s advanced past the completed head %s; reopen it in sutra to review the new work\n",
			w.Branch, run.CompletedHead)
	}
	return run, nil
}

// resumeOrStart returns the run this ticket already has, or a fresh one.
//
// A pop can hand back a ticket a run already exists for — after a rework moved
// it to dev-loop, or after a crash that left the claim standing. Starting a
// second run would abandon the first along with its review, its attempt count
// and everything the tracker already points at.
func resumeOrStart(
	ctx context.Context, store orchestrator.Store, ticket planner.Ticket, target string,
) (orchestrator.BuildRun, error) {
	existing, found, err := store.ForTicket(ctx, ticket.IssueID)
	if err != nil {
		return orchestrator.BuildRun{}, err
	}
	if found {
		return existing, nil
	}
	run := orchestrator.BuildRun{
		ID: uuid.NewString(), Ticket: ticket.Title,
		// Recorded at creation, because recovery replays a submission and a
		// close from what the RUN holds: a replay that looked the issue up
		// again could act on a different one.
		Issue: ticket.IssueID,
		Plan:  target, State: orchestrator.StateQueued,
		// Snapshotted here, at creation. Changing KRIYA_ROUND_LIMIT later
		// affects only runs created after the change.
		RoundLimit: roundLimit(),
	}
	if err := store.Upsert(ctx, run); err != nil {
		return orchestrator.BuildRun{}, err
	}
	return run, nil
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
