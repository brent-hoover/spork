---
title: Kriya Build — Design
type: design
status: draft
owner: Brent Hoover
created: 2026-08-21
updated: 2026-08-21
problem: ./problem.md
---

# Kriya Build — Design

## Summary

Kriya becomes a Go module rooted at `kriya/`, one package per avspec module
under `internal/`, wired by a composition root at `cmd/kriya`. The
orchestrator is a **table-driven state machine** over `BuildRun`'s sixteen
states: it reads the next transition from an explicit table, calls one
module, and writes the result. It contains no `if` that decides *what
should happen* — only `if` that decides *whether the write succeeded*.

Five seams isolate everything kriya does not own: **agents** run as
`claude -p` subprocesses, **spec verification** shells out to the Python
`avspec verify`, **reviews** go through the roborev CLI, **tracking** goes
through a sutra HTTP client, and **gates** shell out to the target's own
snapshot-resolved commands. Each seam is one package with
no internal imports, so each can be faked wholesale in tests.

SQLite holds the durable plane. Every externally-visible action is
**write-ahead**: kriya persists its intent and idempotency key *before* the
call, so recovery can replay on either side of the other system's
acceptance. godog drives all 162 scenario runs; unit tests run against a fake
tracker, integration tests against a real sutra.

## BDD Scenarios

The executable definition of done. These files already exist — they were
written during the spec phase and mapped from every acceptance criterion.

| Feature file | Behavior proven | Scenarios |
|---|---|---|
| `verification/REQ-spec-intake.feature` | Verify a ready avspec, refuse anything less; pin a SpecSnapshot with resolved per-module commands | 12 |
| `verification/REQ-decompose.feature` | PM agent decomposes a spec into tracer-bullet tickets; plan supersession and retirement | 10 |
| `verification/REQ-risk-first.feature` | Risk items become spike/research tickets and are retired before dependents build | 6 |
| `verification/REQ-parallel-build.feature` | Pop tickets off sutra work stacks; run parallel agents where relations allow; the global pop fence | 6 |
| `verification/REQ-workspaces.feature` | Isolated worktree/branch per ticket, and its cleanup refusals | 5 |
| `verification/REQ-pair-loop.feature` | Small commits, roborev rounds, fix, recommit until pass | 6 |
| `verification/REQ-sa-agent.feature` | System architect resolves impasses and escalates scope to the operator | 4 |
| `verification/REQ-context-assembly.feature` | Per-ticket context and toolset assembly | 6 |
| `verification/REQ-learning-loop.feature` | Mistakes become persistent learnings fed into later builds | 5 |
| `verification/REQ-gate-structure.feature` | Structural lint limits via each module's effective lint command | 4 |
| `verification/REQ-gate-typing.feature` | Typing enforced per module | 2 |
| `verification/REQ-gate-branch-coverage.feature` | The target's declared coverage bar decides the gate | 3 |
| `verification/REQ-gate-mutation.feature` | Mutation runs last and any survivor fails | 2 |
| `verification/REQ-po-validation.feature` | PO validates ACs are genuinely satisfied and tests are not gaming | 4 |
| `verification/REQ-submit-review.feature` | Submit a sutra Review, merge on approval, update ticket status | 24 |
| `verification/REQ-run-to-complete.feature` | The outer loop: every ticket to completion, with every crash window recovered | 44 |
| `verification/REQ-tier-routing.feature` | Roles map to model tiers in configuration; no hardcoded models | 3 |
| `verification/REQ-thread-capture.feature` | Every run's transcript reaches sutra's thread catalog, session-stamped | 5 |
| `verification/REQ-tui.feature` | One screen of live state; one inbox for everything needing the operator | 4 |
| **Total** | | **155 headers / 162 runs** |

Run all scenarios: `go test ./...` (godog wired through the test binary,
as in sutra's build). Three Scenario Outlines expand —
`REQ-run-to-complete.feature:208` (2 rows), `:290` (4), and
`REQ-submit-review.feature:96` (4) — so godog reports **162** where the
files show 155 headers. Done means 162 passing.

## Approach

### Package layout

One package per avspec module, names mirroring the module with hyphens
dropped — the layout `arch-go.yml` already encodes — plus a small set of
**non-module packages** discussed below:

```
kriya/
  cmd/kriya/            composition root — opens the database, wires modules, owns main()
  internal/
    planner/            PM agent: intake, decomposition, risk-first ticketing
    orchestrator/       the per-ticket state machine; owns BuildRun
    workspace/          worktree/branch lifecycle
    devloop/            one pair-programming session
    reviewbridge/       the roborev seam
    gates/              runs the target's gate commands
    architect/          SA agent: impasse and escalation
    owner/              PO agent: AC validation
    context/            context assembly + learning store
    trackerclient/      the only package that speaks sutra's API
    cli/                command surface
    tui/                bubbletea TUI
    agent/              the `claude -p` seam            (non-module)
    specverify/         the `avspec verify` seam        (non-module)
    clock/              controllable time               (non-module)
    recovery/           sequences each module's Recover  (non-module)
    acceptance/         godog harness                   (non-module, test-only)
```

**Each module owns its own tables.** There is no shared `store` package —
that is sutra's shape (`sutra/internal/` has twelve packages and no store),
and it is what keeps the arch-go allowlists honest. The composition root
opens one SQLite connection, applies each module's DDL, and hands each
module its handle.

### The non-module packages are a spec gap, and arch-go must say so

avspec has no concept of a package that is not a declared module, yet
arch-go requires 100% package coverage. Sutra hit this with its composition
root and logged it (`sutra/arch-go.yml:22-25`); kriya hits it six times
over.

The consequence is concrete: six modules use `shouldOnlyDependsOn`
allowlists — planner (`arch-go.yml:18-20`), orchestrator (`:22-29`),
devloop (`:45-49`), context (`:107-109`), cli (`:125-129`), tui
(`:131-135`) — and an allowlist that omits `**.agent`, `**.specverify`, or
`**.clock` forbids them. The six modules whose `may_import` is empty use
the complement form, so a package absent from their forbidden list is
already permitted; they need no change.

**This means `arch-go.yml` deliberately diverges from avspec's
`may_import` for non-module edges.** That divergence must be stated in the
file's header, logged as a spec gap in `kriya/feature-work/kriya-build/spec-gaps.md`
(sutra's own file is out of scope), and permitted by
`scope.md` — which currently forbids boundary edits. It is the first
implementation commit, before any package exists.

### Four state machines, not one

The design's earlier claim that one transition table covers the build was
wrong, and the first correction still undercounted. There are **four**
write-ahead machines plus the Plan lifecycle, owned by different modules,
and different feature files exercise each.

**Per-ticket, owned by `orchestrator`.** `BuildRun.state` has sixteen
values (`avspec.yaml:1321`), and `review_submission_state` is a
**second, independent axis** (`none | submitting | submitted |
resubmitting`, `:1335`) that recovery must read together with the first.
`submitting` is not a `BuildRun.state`.

**Per-target, owned by `planner`.** `BuildTarget` carries `epic_state`
(2 values) and `completion_state` (`none | review-submitting |
review-submitted | review-resubmitting | closing | complete | reopening`),
driven by `CompletionAdvance` rows which themselves carry `parenting_state`
(5), `reclaim_state` (3), `cascade_verified` (3), and a `cause` enum of 7.
**Merge and consumption, owned by `orchestrator`.** `MergeAttempt`
(`avspec.yaml:1372-1403`) carries its own 7-value state —
`queued | holding | consuming | consumed | merging | merged | aborted` —
with `attempt_key` and `consume_key`. This is what most of
`REQ-submit-review.feature`'s 24 scenarios actually exercise: verdict-ABA
consumption and the merge CAS against `gated_base`.

**Attribution, owned by `orchestrator`.** `AttributionAmbiguity`
(`:1483-1497`) has a 5-value state plus its own `resolution_key` /
`reclaim_removal_key` / `reclaim_state` protocol.

**Plan lifecycle, owned by `planner`.** `Plan.state` adds 5 more
(`:1211`), covering supersession and retirement.

Which file exercises which:

| Machine | Owner | Exercised by |
|---|---|---|
| `BuildRun` + `review_submission_state` | orchestrator | `run-to-complete`, `pair-loop`, the `submit-review` submission windows (`:15`, `:20`, `:27`, `:37`) and its `gate_attempt`/`gated_base` fence scenarios (`:89`, `:96-127`), the `risk-first` research states (`:33-40`, `:51-57`) |
| `BuildTarget` + `CompletionAdvance` | planner | most of `run-to-complete` |
| `MergeAttempt` | orchestrator | about half of `submit-review` (`:46`, `:60`, `:66`, `:74`, `:80`, `:129-159`, `:167`, `:177`, `:185`) |
| `AttributionAmbiguity` | orchestrator | `run-to-complete` new-work scenarios |
| `Plan` | planner | `decompose`, `spec-intake` |

All four machines are expressed the same way — an explicit table, one row
per transition:

```go
type transition struct {
    from   State
    stage  Stage    // which module runs
    onOK   State
    onFail State
    guard  Guard    // a named precondition, not inline logic
}
```

The loop is: load row → look up the transition → invoke the named stage
through its module interface → write the resulting state in one
transaction. Every branch a reader might expect — a gate failed, a review
came back with changes, a merge conflicted, **a research ticket took the
document path instead of the gate chain** — is a row, not a code path.

`CON-deterministic-orchestrator` is satisfied literally: the orchestrator
contains no `if` that decides *what should happen*, only `if` that decides
*whether the write succeeded*.

**The research path is a first-class traversal.** `CON-gate-chain` defines
two delivery paths, and `research-loop` / `finding-submitted` are BuildRun
states for the second. Research and spike tickets carry a **document**
deliverable and traverse finding → document Review → human approval →
close, with no code gates because none apply. Those are ordinary rows in
the same table.

### The five seams

Each seam isolates something kriya does not own, and each is fakeable
wholesale for the unit ring.

**`agent` → coding agents.** A `claude -p` subprocess. This is a
**non-module package, not part of `devloop`**, because PM, SA, and PO all
invoke agents: `AC-tier-observed` requires that "every agent invocation —
plan-scoped PM work included — records its role, the configured tier, and
the resolved model" (`avspec.yaml:433`), and `architect` and `owner` are
explicitly forbidden from importing `devloop` (`arch-go.yml:84`, `:98`).

**`internal/agent` also owns the `agent_invocation` table**, and this is
the second spec gap. `ENT-agent-invocation` is declared under
`MOD-orchestrator` (`avspec.yaml:474`), but
`REQ-tier-routing.feature:22` requires that "a plan-scoped PM invocation is
recorded even though no BuildRun exists yet" — and `planner` may import
only `trackerclient`, while `architect` and `owner` may not import
`orchestrator` at all. Under avspec's ownership rule the row has **no legal
writer**. Since every invocation passes through the seam, the ledger is
naturally the seam's. `sutra/arch-go.yml:108-128` is the precedent for a
non-module package carrying its own arch-go rule; the ownership argument
stands on its own — every invocation passes through the seam. Log it in
`kriya/feature-work/kriya-build/spec-gaps.md`.

```
claude -p <prompt> \
  --output-format stream-json --verbose \
  --model <resolved from tier config> \
  --allowedTools <generated per ticket, see below> \
  --append-system-prompt-file <context bundle> \
  --add-dir <workspace>
```

Three ACs get their mechanism from the stream: `system/init` carries the
resolved model → `AC-tier-observed`; `session_id` stamps every event →
`REQ-thread-capture` and `AC-thread-searchable`; `parent_tool_use_id`
reconstructs subagent nesting → the TUI. `system/api_retry` events carry
an `error` category — `rate_limit`, `billing_error`, `overloaded`, and
others — which feeds `ENT-stall`.

The subprocess boundary is deliberate: parallel dev agents get independent
kill and timeout, and a crashing agent cannot take the orchestrator with
it. `SIGINT` ends a turn cleanly; `SIGTERM` exits 143 leaving the turn
unfinished, which is exactly the crash the recovery scenarios describe.

**How the ticket toolset actually reaches the agent.** `--allowedTools`
names Claude Code tool identifiers, not target commands, so it cannot
carry the six resolved commands directly.
`REQ-context-assembly.feature:26-30` requires both that the touched
modules' resolved commands "are wired in" and that "tools irrelevant to the
ticket are absent". The mechanism is two-part:

- The resolved commands are **content**, written into the context bundle
  passed via `--append-system-prompt-file`.
- The permission to run them is **generated allow-rules**, and they cannot
  name the commands directly. The resolved commands are not binaries:
  kriya's own `lint` is a `mktemp`/`trap`/`&&` pipeline and its `mutation`
  is an `if`/`awk` pipeline (`avspec.yaml:22`, `:26`). A rule like
  `Bash(go test *)` cannot express those. Instead kriya writes **one
  wrapper script per resolved gate command** into the workspace and grants
  exactly `Bash(<workspace>/.kriya/gate-* *)` and nothing else. `context`
  writes those wrappers, `workspace` owns the directory they live in, and
  `gates` runs the same resolved commands directly for its own results. No bare
  `Bash`, which is what makes "tools irrelevant to the ticket are absent"
  (`REQ-context-assembly.feature:30`) mechanically true.
  (Permission-rule prefix matching is space-star — `Bash(git diff *)` —
  where the space is what stops it also matching `git diff-index`.)

**`specverify` → the avspec verifier.** All twelve `REQ-spec-intake`
scenarios need the seam present; **three** consume its **findings** ("the
response carries the verify findings", `REQ-spec-intake.feature:18`) while
the rest turn on command resolution from the pinned snapshot. It is a **Python** tool kriya shells out to:
`avspec verify <dir> --json` (`avspec/src/avspec/cli.py:38-45`), returning
`{status, ok, counts, findings[]}`. `avspec/src/**` is must-not-touch, so
the report shape is fixed and consumed as-is. **A refusal is not an
error**: `avspec verify` exits 1 whenever `ok=False`
(`avspec/src/avspec/cli.py:48`), which is the ordinary outcome for all
three finding-driven refusals (`REQ-spec-intake.feature:18`, `:24`,
`:30`), so the seam must return a `Report`, not a Go `error`, on a
non-zero exit that produced a parseable payload. The fourth refusal
(`:33-38`, a missing module command) is **kriya's own** check against the
pinned snapshot — that spec verifies `ready` and `avspec verify` exits 0. **The integration ring
therefore needs a Python/uv environment**, not only Go.

**`reviewbridge` → roborev.** Enqueue, poll, respond, close. R1 settled the
protocol: roborev's enqueue accepts no correlation key and does not dedupe
by SHA, so kriya acts **only** on ids returned by its own enqueue calls —
no adoption of jobs it did not create, no cancellation of unproven ones.
`EnqueueAttempt` is written before the call; unresolved attempts and
orphans surface in the TUI inbox.

**`trackerclient` → sutra.** The one package that speaks sutra's HTTP API.
Every mutation carries an idempotency key persisted before the call.

**The orchestrator exposes a read facade for the TUI.**
`VIEW-tui-inbox.shows` (`avspec.yaml:559`) names `ENT-plan.state`,
`ENT-plan.error`, `ENT-planned-ticket.error`,
`ENT-workspace.cleanup_error`, and four `ENT-completion-advance` fields;
`VIEW-tui-overview.shows` (`:552`) names `ENT-planned-ticket` fields, and
`VIEW-tui-run` (`:567`) adds `ENT-gate-result` and `ENT-validation`
fields — gates- and owner-owned. All are owned by modules `tui` cannot
reach, and `MOD-tui` may import only
orchestrator, architect, and reviewbridge (`arch-go.yml:131-135`).
Orchestrator imports both planner and workspace, so it carries a read
facade spanning their state. A facade is not the orchestrator "deciding":
it selects and forwards, adding no branch.

**`gates` → the target's commands.** The gate runner does not know what a
linter or a coverage tool is. It resolves the module's command from the
pinned `SpecSnapshot`, runs it at the head commit, records the outcome.

### GateResult detail — resolving the deferred question

`ENT-gate-result.detail` is typed `json` (`avspec.yaml:1654`), which
settles the open question without new mechanism. The runner captures
stdout and stderr into a structured envelope:

```json
{"exit_code": 1, "stdout": "...", "stderr": "...", "duration_ms": 812}
```

`AC-coverage-recorded` asks for uncovered arms "named in detail for the dev
agent". The arms are *in* that captured stdout — `branch-coverage.sh:586`
prints them via `jq -r '.[] | select(.t == 0 or .f == 0)'` — so the
requirement is met by capture, not by parsing. Kriya stays honest to
`AC-coverage-floor`: it counts nothing and parses nothing.

### Recovery: who runs it, and how a test induces a crash

About fifty scenarios are phrased "the crash hit before X … **When recovery
runs**". Recovery is therefore a named component, not a property.

**Each owning module exposes `Recover(ctx) error`**, which reconciles the
rows it owns: resume an unstamped step, terminate a crashed DevSession,
release an abandoned workspace, reconcile an unresolved EnqueueAttempt.

**A sixth non-module package, `internal/recovery`, sequences them.**

The honest justification is not that sequencing is impossible elsewhere.
`orchestrator` cannot import `reviewbridge` directly (`arch-go.yml:22-29`),
but `devloop` can (`:44-49`), so `orchestrator → devloop → reviewbridge`
is a legal path and `devloop.Recover` *could* re-export round and
EnqueueAttempt reconciliation. The reason not to is that doing so puts
cross-module sequencing judgment inside the pair-loop module, and inside
the module `CON-deterministic-orchestrator` most constrains. Sequencing
startup work across every owner is a composition-root concern, so it lives
in a package the composition root owns.

`recovery` imports the owning modules and is imported only by `cmd/kriya`,
which runs it once at startup before any pop. Modules never import
`recovery`, so no module allowlist changes — every arch-go rule is
outbound-only and nothing constrains inbound edges.

**Ordering**, with the owner and machine each stage reconciles:

| # | Stage | Owner | Machine |
|---|---|---|---|
| 1 | **Pop binding sweep** | orchestrator | `BuildRun` — bind claimed-but-unbound pops |
| 2 | Plan lifecycle: retirement and activation | planner | `Plan` |
| 3 | Targets and completion | planner | `BuildTarget` + `CompletionAdvance` |
| 4 | Attribution | orchestrator | `AttributionAmbiguity` |
| 5 | Workspaces | workspace | `Workspace` |
| 6 | Dev sessions | devloop | `DevSession` — terminate crashed, import transcript |
| 7 | Build runs | orchestrator | `BuildRun` + `review_submission_state` |
| 8 | Merges | orchestrator | `MergeAttempt` |
| 9 | Review rounds and enqueue attempts | reviewbridge | `ReviewRound`, `EnqueueAttempt`, orphans |

**Pop binding runs first, before retirement.** The spec is explicit: "the
reconciliation pass below runs first so no claimed-but-unbound pop exists
when those stamps land" (`avspec.yaml:1069-1071`), and
`REQ-parallel-build.feature:47` asserts "the BuildRun is bound by pop
replay **before any ownership classification**". An earlier draft of this
table put plan retirement first and claimed "none depends on a later
one" — that was wrong in exactly the direction the scenario names.

**Retirement can also re-bind mid-pass**, so stage 1 is a sweep, not a
one-shot: "claimed-but-unbound pops are bound WHENEVER discovered, not
only in the initial sweep" (`:1137-1145`). Binding is idempotent, so
re-invoking it from stage 2 is safe.

Rounds settle **last**, after build runs, because a BuildRun parked in a
pair-loop state is waiting on its round's outcome
(`REQ-pair-loop.feature:41-46`): the run must be loaded and its expectation
known before a round can be judged stale, orphaned, or still live.

Apart from stage 2's documented re-invocation of stage 1, no stage depends
on a later one.

### Planner binds an orchestrator-owned row — the third spec gap

Retirement must "replay that pop and bind the row" (`avspec.yaml:1137`),
and stamp disposition `bound` for live builds (`:1066`). But `ENT-build-run`
is **orchestrator-owned** (`:474`) and `planner` may import only
`trackerclient` (`arch-go.yml:17-20`). This is the same shape as the
`agent_invocation` gap: a module is required to act on another module's
rows across a boundary it cannot cross.

The reverse direction is already fine and the spec relies on it —
"BuildRun write-ahead creation invokes the planner's admission guard"
(`:1153`) — because orchestrator *may* import planner.

**Mechanism:** `planner` declares the narrow interface it needs and does
not import its implementer:

```go
// package planner
type PopBinder interface {
    BindClaimedPops(ctx context.Context, plan PlanID) (bound int, err error)
}
```

`orchestrator` implements it — it already owns `BuildRun` — and
`cmd/kriya` wires the two together. Dependency inversion keeps every
arch-go rule intact: planner imports nothing new, and orchestrator already
imports planner. Logged in `spec-gaps.md`.

**How a test induces a crash.** Every seam is injectable — `agent`,
`specverify`, `trackerclient`, `gates`, the roborev bridge, **and git**.
`gates` is faked in the unit ring by necessity, not only for crash
injection: a real chain is the ~20-minute coverage run and the 15h48m
mutation run, so no unit test may invoke one. Git matters because
`REQ-workspaces.feature:23` ("a crash between the pending write and
creation") and `REQ-submit-review.feature:70-72` (recovery inspects the
branch; both a landed and an unlanded CAS push must be producible) require.

The harness suppresses **either side** of a call, because the scenarios
name both windows:

- **Pre-acceptance** — the call never reaches the other system.
  `REQ-submit-review.feature:16` ("the crash hit before sutra accepted the
  request"), `REQ-run-to-complete.feature:210` ("a crash lands before the
  commit").
- **Post-acceptance** — the other system accepted it, but the outcome
  write never landed. `REQ-submit-review.feature:20-25` ("sutra accepted
  it").

Then the store handle is dropped, reopened, and `recovery.Run` is called —
which is exactly what the scenarios mean by "recovery runs". No process is
killed; the crash is the suppressed write, which is deterministic and
faster.

**The post-acceptance window is also provable against real sutra.** In the
integration ring the harness wraps the real `trackerclient`, letting the
call through and suppressing only kriya's outcome write. That is the window
worth proving for real, because it is the one sutra's idempotency fences
exist to make safe.

### Recovery model

**Single-call windows** follow one shape:

1. Persist intent + idempotency key (an interim state on the relevant axis).
2. Perform the external call.
3. Persist the outcome in one transaction.

**Multi-call sequences do not**, and several scenarios pin them. At
`REQ-run-to-complete.feature:30-52` a document mutation and a review
creation are separate keyed calls with a crash window *between* them. For
these, each step carries its own key and its own interim marker, and
recovery resumes at the first step whose key is unstamped — it never
replays a completed step or skips an unstarted one.

There are **37 `_key` columns** across the 25 entities, of which **15 are
declared `unique`**. The set mixes true idempotency keys with identity and
scoping references — `project_key` is explicitly NOT unique — so 37 is a
measure of surface, not a count of crash windows. Each key must be
classified before it is implemented; treating a reference as an
idempotency key would fence work that should proceed.

Where the external system has no key (roborev), recovery **observes** and
surfaces ambiguity to the operator rather than guessing, per R1.

### Test harness

**What `acceptance` drives.** Kriya has no HTTP surface, so sutra's shape
(harness → `api`) does not transfer. The scenarios mix operator-level steps
("the operator points kriya at the project") with module-level ones ("the
context manager assembles the dev agent's context",
`REQ-context-assembly.feature:9`). `acceptance` therefore drives the `cli`
package in-process for operator-level steps and module APIs directly for
module-level ones; its arch-go rule permits importing `cli` and every
module. It is test-only and imported by nothing.

- **Unit** — each module against a **fake** `trackerclient`, `agent`, and
  `specverify`, all three behind interfaces. Fast, hermetic, no network,
  no subprocesses.
- **Integration** — the godog suite against a **real sutra** binary and a
  **real `avspec verify`**, because sutra's fences (verdict-ABA,
  idempotency replay, atomic pop) exist specifically for kriya and a fake
  would prove the wrong thing.

Sutra is a single binary; the harness starts one per suite against a
temporary SQLite database and tears it down after.

### Milestones

Delivery is by merged milestones. Dependency-ordered:

Every one of the eighteen packages appears below. `planner` and
`orchestrator` are each split across milestones, because their machines are
not all needed at once — and `recovery` grows from M2 rather than arriving
at the end, because scenarios in M2, M3, and M4 all say "when recovery
runs".

A requirement marked **(partial)** has some of its scenarios proven here
and the rest in a later milestone.

| # | Milestone | Packages | Requirements proven |
|---|---|---|---|
| M1 | Skeleton | `cmd/kriya`, `acceptance`, `clock`, `arch-go.yml` completed, Go+uv CI | none — the gate chain itself |
| M2 | Intake and decomposition | `specverify`, `trackerclient`, `agent`, `cli`, `planner` (`Plan` lifecycle + `BuildTarget.epic_state`), `recovery` (stages 1-2) | spec-intake **(partial)**, decompose **(partial)**, risk-first **(partial)** |
| M3 | One ticket through the chain | `workspace`, `devloop`, `reviewbridge`, `gates`, `context`, `architect`, `orchestrator` (BuildRun, code path only), `recovery` (+ stages 5-7, 9) | workspaces, pair-loop, gate-structure, gate-typing, gate-branch-coverage, gate-mutation, context-assembly, thread-capture, tier-routing, sa-agent **(partial)** |
| M4 | Review, merge, completion | `owner`, `orchestrator` (MergeAttempt + BuildRun research path), `planner` (`completion_state` + `CompletionAdvance`), `recovery` (+ stages 3, 8) | po-validation, submit-review |
| M5 | The outer loop | `orchestrator` (full table + AttributionAmbiguity), `recovery` (+ stage 4) | run-to-complete, parallel-build, learning-loop, risk-first **(remainder)**, spec-intake **(remainder)**, decompose **(remainder)**, sa-agent **(remainder)** |
| M6 | Operator surface and proof | `tui`, then the linkshort end-to-end run | tui |

**DDL ships with its module**, not in M1 — a package that does not exist
yet cannot carry a schema. M1 completes `arch-go.yml` for every package
including ones not yet written; arch-go matches rules against packages that
exist, so a rule naming an absent package is inert rather than an error,
which is what lets the boundary file land once instead of growing.

Three placements are forced rather than chosen:

- **`agent` is M2, not M3.** `REQ-decompose.feature:11` is "When the PM
  agent decomposes it", so M2 cannot go green without at least the `Agent`
  interface and its fake.
- **`architect` is M3, not M5.** `devloop` imports it
  (`arch-go.yml:45-49`), so it cannot arrive after its importer — which
  also pulls `sa-agent` forward from M5.
- **`cli` is M2.** `kriya build <project>` is how M2's intake scenarios are
  driven at all.
- **`planner` splits M2/M4, and so does `BuildTarget`.** The `Plan`
  lifecycle is M2. `BuildTarget` divides along its two enums:
  `epic_state` is written **write-ahead at decomposition**
  (`REQ-decompose.feature:20`: "a BuildTarget row is written ahead of the
  epic's creation and records the epic id when it returns";
  `avspec.yaml:741-745`), so it is M2. `completion_state` and
  `CompletionAdvance` are genuinely a completion concern and are M4. An
  earlier draft deferred all of `BuildTarget` to M4, which would have left
  M2's decomposition scenarios unprovable.
- **The `orchestrator` split is by BuildRun state, not by feature.** M3
  delivers the **code traversal** — `queued`, `no-work`, `dev-loop`,
  `gates`, `po-validation`, `review-submitted`, `awaiting-operator`,
  `merging`, `merged`, `failed`, `cancelling`, `cancelled`. M4 adds the
  **research path** — `research-loop`, `finding-submitted`, `completing`,
  `closed` — because `REQ-submit-review.feature:171` ("a spike's
  document-backed close recovers identically … research runs persist them
  directly, having no merge attempt") sits inside a scenario M4 claims.
  M5 adds nothing to the state list; it adds parallelism, the full
  transition table, and `AttributionAmbiguity`.
- **`recovery` starts in M2 and grows.** `REQ-pair-loop.feature:42` is
  literally "When recovery runs", `REQ-workspaces.feature:24` and `:35`
  probe and recreate under recovery, and five `submit-review` scenarios
  replay under it. A `recovery` package arriving in M5 would leave M2, M3,
  and M4 unable to go green.

**Three requirements deliberately span milestones**, and the table says so
rather than claiming them early:

- **`risk-first`** — only scenario 1 (`:7`, ticket creation with the risk
  label) is M2. Scenario 2 (`:14-23`) asserts that a dependent ticket
  "cannot pop at all while the spike is open" and that "an agent pops work
  under the tracker's FIFO ordering" — pop machinery this table puts in M5
  with `parallel-build`. The remaining four need the BuildRun research path,
  a sutra document Review, and architect escalation. So the split is 1 in
  M2, 5 in M5.
- **`decompose`** — `:31` ("an agent popping work receives an unblocked
  ticket") is the same pop machinery, so it lands M5 with the rest of
  decomposition proven in M2.
- **`sa-agent`** — `:36` asserts pop admission ("an agent popping in the
  window before the supersession fence cannot claim the obsolete ticket —
  deferred is not poppable"), the same machinery deferred to M5. The rest
  is M3.
- **`spec-intake`** — `:55` needs the gate chain (M3) *and* a
  supersession-era replay that only exists once plan supersession and pop
  admission land in M5. The earlier draft blamed "recovery replays a
  step", which was wrong: BuildRun recovery is stage 7 and ships in M3.

Claiming any of these whole in M2 would have been a milestone that could
not go green.

Two of the six `GateResult.gate` values — `test` and `arch`
(`avspec.yaml:1651`) — have **no dedicated requirement**, but both have
dedicated scenarios asserting their own GateResult:
`REQ-gate-structure.feature:20-26` ("the arch gate checks real imports" …
`GateResult with gate "arch" pinned to "C2"`) and
`REQ-gate-branch-coverage.feature:18` (`the test gate fails as its own
commit-pinned GateResult with gate "test"`). They are first-class M3 work
with their own assertions, not indirect.

## Interfaces

**CLI** — verbatim from the pinned `VIEW-cli-*` and `VIEW-tui-*`
invocations (`avspec.yaml:525`, `:531`, `:537`, `:550`):

```
kriya build <project>      intake a spec and start or resume its build
kriya status [--json]      current plan, runs, and inbox; --json is for agents
kriya learn add            record an operator learning
kriya tui                  the live TUI
```

**Agent seam** — one Go interface in `internal/agent`, one real
implementation, one fake:

```go
type Agent interface {
    Run(ctx context.Context, req Request) (Result, error)
}
type Request struct {
    Role       Role     // pm | sa | po | dev
    Prompt     string
    Workspace  string
    AllowRules []string // Bash(go test *), … generated per ticket
    SystemFile string   // context bundle, via --append-system-prompt-file
}
type Result struct {
    SessionID  string   // from system/init; stamps DevSession and the sutra thread
    Model      string   // resolved model actually used, from system/init
    Transcript string   // path to the durable transcript
    Outcome    Outcome
}
```

**Spec-verifier seam** — `internal/specverify`, wrapping
`avspec verify <dir> --json`:

```go
type Verifier interface {
    Verify(ctx context.Context, dir string) (Report, error)
}
type Report struct {
    Status   string     // ready | draft | …
    OK       bool
    Findings []Finding  // code, severity, message, ref
}
```

**Configuration** — `kriya.toml`, resolved from `$KRIYA_CONFIG`, else
`./kriya.toml`, else `~/.config/kriya/kriya.toml`. Tiers only; no model
identifier appears in source (`AC-tier-config`):

```toml
[tiers]
pm = "opus"
sa = "opus"
po = "opus"
dev = "sonnet"

[models]
opus = "claude-opus-5"
sonnet = "claude-sonnet-5"
```

A role with no configured tier **fails at startup** (`AC-tier-explicit`) —
there is no silent default.

## Data model

Twenty-five entities become twenty-five SQLite tables, each owned by the
module that declares it in `modules[].owns`. Four patterns carry most of
the weight:

| Pattern | Examples | Why |
|---|---|---|
| Idempotency keys — the crash windows | `decomposition_key`, `pop_key`, `advance_key`, `completion_submission_key`, `reclaim_removal_key`, `ambiguity_key` | Write-ahead replay after a crash |
| Identity and scoping keys — **not** crash windows | `project_key` (declared NOT unique), `target_key`, `singleton_key`, `mapping_key` | References and upsert hashes |
| Fence column compared under transaction | `PopFence`, `intake_generation`, `close_revision`, `review_resubmission_event` | Reject stale work rather than apply it twice |
| Interim states on independent axes | `BuildRun.state` (16) **and** `review_submission_state` (4); `BuildTarget.completion_state` (7) **and** `epic_state` (2) | Make each crash window visible; recovery reads the axes together |
| Snapshot pinning | `SpecSnapshot`, `spec_hash`, `gated_base` | Working-tree edits never change what a running build executes |

`BuildRun` alone carries 32 fields and `BuildTarget` 24, most of them the
write-ahead and fence columns above. That width is the price of
`AC-intake-snapshot-authority` and the recovery scenarios; it is not
accidental and should not be normalised away.

**Git access** (`MOD-workspace`, and every merge CAS against `gated_base`)
is by **shelling out to `git`**, not a library. It matches the gate
runner's shape, it is what the recovery scenarios can observe, and the
worktree commands kriya needs are ones `go-git` does not fully support.
The seam is an interface so the unit ring fakes it.

## Alternatives considered

### Simplest

A walking skeleton: intake → decompose → one ticket → one dev agent →
merge, with the gate chain reduced to `test` only, no TUI, no recovery, no
parallelism. Ship in days.

**Drawbacks, honestly:** it proves nothing the spec cares about. The
`run-to-complete` file alone is 44 scenarios and the `submit-review` file
24, and about fifty scenarios across the suite turn on "when recovery
runs" — the skeleton exercises none of them. Worse, recovery
and idempotency are not features that bolt on: the write-ahead shape has to
be in each seam from the first write, or every call site is retrofitted
later. This option front-loads visible progress and back-loads all the risk.

### Complete

The design above: all twelve modules in dependency order, the state table,
all five seams with write-ahead persistence, both test rings, the full gate
chain, and the TUI. Every scenario passes. Delivered as merged milestones.

### Optimal

The Complete design plus a **deterministic interleaving harness** — the
concurrency analogue of sutra's `faultsql`. A controllable clock and a
scheduler that can pause any seam at any await point, so a test can pin a
*specific* interleaving (ticket claimed exactly mid-supersession; pop
arriving between fence check and write) rather than describing it in
Gherkin and trusting the implementation to match. It would also let the
recovery scenarios assert every crash window mechanically instead of one
scenario per window.

**What we trade by not doing it:** the race scenarios are proven as written
rather than exhaustively. Sutra's experience says this matters — the
interleavings in kriya's own spec checklist were found by *review*, not by
tests, and there is no reason to think review found the last one.

### Decision

**Complete, with one element of Optimal pulled forward.**

What pushes above Simplest: the failure modes. Kriya merges code
autonomously, so a mis-ordered gate chain or a duplicated dev session is
silent and expensive. Write-ahead persistence and the state table are not
polish — they are the requirement, and they cannot be retrofitted.

What keeps below Optimal: the interleaving harness is a second system, and
building it before kriya exists repeats R3's lesson — a spike must exercise
the thing under test, not scaffolding around it. The element pulled forward
is a **controllable clock** from day one (no `time.Now()` outside one
package), because that *is* a retrofit if deferred, and it is most of what
the harness would later need.

## Risks

- **Wall-clock per gate chain is the dominant scale fact, and R2 is still
  open.** Kriya runs the whole chain *per ticket, per touched module*.
  Sutra's coverage gate alone takes ~20 minutes and its whole-module
  mutation run measured 15h48m over ~1678 mutants. R2
  (`avspec.yaml:87-91`) asks whether the mutation gate kriya runs **for
  the targets it builds** is viable per-ticket or only periodically — it
  is unanswered, and a wrong answer makes builds unusably slow rather than
  incorrect. Distinct from kriya's own mutation scope below.
- **`arch-go.yml` is incomplete and fails closed.** It declares the twelve
  modules but none of `cmd/kriya`, `agent`, `specverify`, `clock`,
  `recovery`, or `acceptance`, and arch-go requires 100% package
  coverage. The six
  `shouldOnlyDependsOn` modules must also gain the non-module packages
  they use. First commit, before any package exists.
- **Whole-module mutation is unusable on kriya's own code.** Sutra scoped
  its gate to eight domain-core packages for exactly this reason. Kriya's
  command currently takes no directory argument.
- **The `--bare` fork is unresolved and both branches are bad.** With it,
  agents lose subscription billing; without it, every agent inherits the
  host's `~/.claude` — hooks, plugins, MCP servers, `CLAUDE.md`. Must be
  decided before the first real dev-agent run.
- **The four state tables can be undermined without failing any gate.**
  Nothing mechanically prevents a contributor adding judgment to the
  orchestrator. The structural-lint limits are the intended guard; whether
  they are configured strictly enough to catch it is unverified.
- **Sutra's fences are proven by sutra's tests, not by a consumer.** If a
  guarantee is weaker than kriya's recovery assumes, it surfaces here
  first — as a flaky integration test, which is the worst way to learn it.
- **"162 runs" understates the harness surface by roughly an order of
  magnitude.** Several scenarios are multi-cycle — `REQ-run-to-complete
  .feature:234-253` carries three `Given` blocks and fourteen `Then` steps
  under one header, and `REQ-tui.feature:22-54` has fifteen `Given`s.
  Step definitions, not scenario count, are the real cost, and they land in
  M1's `acceptance` package before any of them can pass.
- **CI has no Go job at all.** `.github/workflows/verify.yml` is the
  repo's only workflow and runs uv, ruff, ty, pytest, and
  `avspec verify examples/linkshort` — nothing Go. M1 must add one, and it
  needs **both** toolchains, because `specverify` shells out to
  `avspec verify`: the integration ring depends on a working uv
  environment as well as Go.

## Out of scope

- Kriya building kriya. Bootstrapping is a later question.
- Any tracker other than sutra. `trackerclient` is the seam; it has one
  implementation.
- Modifying roborev, including adding the correlation key or SHA dedupe R1
  found missing.
- Multi-machine, hosted, or multi-operator operation.
- Behavioral spec changes. Stack-command changes to kriya's own spec (the
  mutation scope) remain in scope with their own review.

## Open questions

- [ ] **`--bare` or not.** Reproducibility versus subscription billing;
      see Risks. Blocking before the first real dev-agent run, not before
      implementation starts.
- [ ] **Mutation scope for kriya's own code**, and separately **R2's
      cadence question for targets**. Both answerable once module sizes
      are real; sutra's answer to the first was its eight non-wire
      packages.
- [ ] **Does `trackerclient` generate from the OpenAPI contract or
      hand-roll against it?** Settle at the first `trackerclient` commit;
      it affects nothing else.
- [ ] **What model access does the end-to-end proof assume?** Carried
      from `problem.md`: M6 is a real driven build spending real tokens
      against a rate-limited API, and it is not offline-reproducible.
      `CON-no-secrets` covers credentials but not availability.
- [x] **How does the TUI reach planner-owned reads and actions?**
      *Decided* — a read facade plus action pass-throughs on
      `orchestrator`, which can reach planner, workspace, gates, and owner.
      Remains here as a **verification note, not an open question**:
      confirm the facade covers every `VIEW-tui-*` field before M6. `AC-tui-act`
      requires `ACT-plan-restore`, `ACT-plan-retry`, and
      `ACT-parenting-resolve` to be executable from the TUI, but
      `MOD-tui` may import only orchestrator, architect, and reviewbridge
      (`arch-go.yml:131-135`). The intended answer is orchestrator
      Neither is the orchestrator "deciding": both carry an already-made
      decision or an already-recorded fact to or from the owning module,
      adding no branch.

## Change log

- 2026-08-21: Initial draft (Brent Hoover)
- 2026-08-21: Applied design-reviewer findings — dropped the shared `store`
  package for sutra's per-module shape, moved the agent seam out of
  `devloop` into its own package (PM/SA/PO cannot import devloop), added
  the missing `specverify` seam for the Python verifier, replaced the
  single state table with the two real machines (BuildRun plus the
  BuildTarget completion protocol) and corrected `submitting` to the
  `review_submission_state` axis, added the second crash-window shape for
  multi-call sequences, corrected the key count to 37, restated the CLI
  verbatim from the spec, fixed the TOML sample and gave the config a path,
  pinned git as shell-out, named the ticket-toolset mechanism, added the
  R2/target-runtime risk and the Python-in-CI risk, defined six milestones,
  and corrected the scenario count to 162 with outline expansion
  (Brent Hoover)
- 2026-08-21: Second design-review pass — `internal/agent` now owns the
  `agent_invocation` table (no legal writer existed: the entity is
  orchestrator-owned but planner must record PM invocations and cannot
  import orchestrator), moved the `agent` seam into M2 ahead of its first
  consumer, named all four write-ahead machines after `MergeAttempt` was
  found missing, replaced per-command Bash allow-rules with generated
  wrapper scripts (the resolved commands are shell pipelines, not
  binaries), added the orchestrator read facade the TUI's inbox needs,
  corrected the CI risk (there is no Go job at all), reclassified the 37
  `_key` columns as a surface measure rather than a crash-window count,
  and noted that `avspec verify` exits 1 on an ordinary refusal
  (Brent Hoover)
