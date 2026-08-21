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

Four seams isolate everything kriya does not own: **agents** run as
`claude -p` subprocesses, **reviews** go through the roborev CLI,
**tracking** goes through a sutra HTTP client, and **gates** shell out to
the target's own snapshot-resolved commands. Each seam is one package with
no internal imports, so each can be faked wholesale in tests.

SQLite holds the durable plane. Every externally-visible action is
**write-ahead**: kriya persists its intent and idempotency key *before* the
call, so recovery can replay on either side of the other system's
acceptance. godog drives all 155 scenarios; unit tests run against a fake
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
root and logged it (`sutra/arch-go.yml:22-25`); kriya hits it five times
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
file's header, logged as a spec gap alongside sutra's, and permitted by
`scope.md` — which currently forbids boundary edits. It is the first
implementation commit, before any package exists.

### Two state machines, not one

The design's earlier claim that one transition table covers the build was
wrong. There are two, owned by different modules, and the 44
`run-to-complete` scenarios mostly exercise the second.

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
`AttributionAmbiguity` (orchestrator-owned) adds a further 5-value state.
`Plan.state` adds 5 more.

Both machines are expressed the same way — an explicit table, one row per
transition:

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
explicitly forbidden from importing `devloop` (`arch-go.yml:82`, `:97`).

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
- The permission to run them is **generated allow-rules** in Claude Code's
  permission-rule syntax — `Bash(go test *)`, `Bash(go vet *)`, one per
  resolved command — and nothing else. No bare `Bash`, which is what makes
  "tools irrelevant to the ticket are absent" mechanically true.

**`specverify` → the avspec verifier.** All twelve `REQ-spec-intake`
scenarios turn on running the verifier and consuming its findings
("the response carries the verify findings",
`REQ-spec-intake.feature:18`). It is a **Python** tool kriya shells out to:
`avspec verify <dir> --json` (`avspec/src/avspec/cli.py:38-45`), returning
`{status, ok, counts, findings[]}`. `avspec/src/**` is must-not-touch, so
the report shape is fixed and consumed as-is. **The integration ring
therefore needs a Python/uv environment**, not only Go.

**`reviewbridge` → roborev.** Enqueue, poll, respond, close. R1 settled the
protocol: roborev's enqueue accepts no correlation key and does not dedupe
by SHA, so kriya acts **only** on ids returned by its own enqueue calls —
no adoption of jobs it did not create, no cancellation of unproven ones.
`EnqueueAttempt` is written before the call; unresolved attempts and
orphans surface in the TUI inbox.

**`trackerclient` → sutra.** The one package that speaks sutra's HTTP API.
Every mutation carries an idempotency key persisted before the call.

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

There are **37 `_key` columns** across the 25 entities. That count is the
real measure of this design's surface: each one is a crash window somebody
specified.

Where the external system has no key (roborev), recovery **observes** and
surfaces ambiguity to the operator rather than guessing, per R1.

### Test harness

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

| # | Milestone | Requirements proven |
|---|---|---|
| M1 | Skeleton: packages, arch-go completed, per-module DDL, clock, CI green with zero behavior | none — the gate chain itself |
| M2 | Intake and decomposition: `specverify`, `trackerclient`, `planner` | spec-intake, decompose, risk-first |
| M3 | One ticket end to end: `workspace`, `agent`, `devloop`, `reviewbridge`, `gates`, `context` | workspaces, pair-loop, 4 gate REQs, context-assembly, thread-capture, tier-routing |
| M4 | Review and completion: `owner`, the per-target machine | po-validation, submit-review |
| M5 | The outer loop: full orchestrator table, parallelism, recovery | run-to-complete, parallel-build, sa-agent, learning-loop |
| M6 | `tui`, then the linkshort end-to-end proof run | tui |

## Interfaces

**CLI** — verbatim from the pinned `VIEW-cli-*` and `VIEW-tui-*`
invocations (`avspec.yaml:531-540`):

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
| Idempotency key, unique — **37 columns across the 25 entities** | `decomposition_key`, `pop_key`, `advance_key`, `completion_submission_key`, `reclaim_removal_key`, `ambiguity_key` | Write-ahead replay after a crash |
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

**Drawbacks, honestly:** it proves nothing the spec cares about. 44 of the
155 scenarios are recovery, 24 are review-submission protocol, and 6 are
parallel-build races — the skeleton exercises none of them. Worse, recovery
and idempotency are not features that bolt on: the write-ahead shape has to
be in each seam from the first write, or every call site is retrofitted
later. This option front-loads visible progress and back-loads all the risk.

### Complete

The design above: all twelve modules in dependency order, the state table,
all four seams with write-ahead persistence, both test rings, the full gate
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
  modules but none of `cmd/kriya`, `agent`, `specverify`, `clock`, or
  `acceptance`, and arch-go requires 100% package coverage. The six
  `shouldOnlyDependsOn` modules must also gain the non-module packages
  they use. First commit, before any package exists.
- **Whole-module mutation is unusable on kriya's own code.** Sutra scoped
  its gate to eight domain-core packages for exactly this reason. Kriya's
  command currently takes no directory argument.
- **The `--bare` fork is unresolved and both branches are bad.** With it,
  agents lose subscription billing; without it, every agent inherits the
  host's `~/.claude` — hooks, plugins, MCP servers, `CLAUDE.md`. Must be
  decided before the first real dev-agent run.
- **The two state tables can be undermined without failing any gate.**
  Nothing mechanically prevents a contributor adding judgment to the
  orchestrator. The structural-lint limits are the intended guard; whether
  they are configured strictly enough to catch it is unverified.
- **Sutra's fences are proven by sutra's tests, not by a consumer.** If a
  guarantee is weaker than kriya's recovery assumes, it surfaces here
  first — as a flaky integration test, which is the worst way to learn it.
- **The integration ring needs Python.** `specverify` shells out to
  `avspec verify`, so the suite depends on a working uv environment as
  well as a Go toolchain. A CI image with only Go fails at M2.

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
- [ ] **How does the TUI reach planner-owned actions?** `AC-tui-act`
      requires `ACT-plan-restore`, `ACT-plan-retry`, and
      `ACT-parenting-resolve` to be executable from the TUI, but
      `MOD-tui` may import only orchestrator, architect, and reviewbridge
      (`arch-go.yml:131-135`). The intended answer is orchestrator
      pass-throughs — and the reason a pass-through is not the
      orchestrator "deciding" is that it carries an operator's already-made
      decision to the owning module, adding no branch of its own. Confirm
      that reading before M6.

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
