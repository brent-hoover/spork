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
| **Total** | | **155** |

Run all scenarios: `go test ./...` (godog is wired through the test binary,
as in sutra's build).

## Approach

### Package layout

One package per avspec module, names mirroring the module with hyphens
dropped — the layout `arch-go.yml` already encodes:

```
kriya/
  cmd/kriya/            composition root — wires modules, owns main()
  internal/
    planner/            PM agent: intake, decomposition, risk-first ticketing
    orchestrator/       the state machine; owns BuildRun
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
    store/              SQLite: schema, migrations, transactions
    acceptance/         godog harness (test-only)
```

`store/` and `acceptance/` are **not avspec modules**, and `arch-go.yml`
does not currently mention them or `cmd/kriya`. arch-go requires 100%
package coverage, so the gate fails the moment the first of them exists.
Adding those three rules is step one of implementation (see Risks).

### The orchestrator is a table, not a function

`CON-deterministic-orchestrator` is the load-bearing constraint: *"If a
change requires the orchestrator to decide anything, the change is wrong."*
The design honours it literally.

`BuildRun.state` has sixteen values. The orchestrator holds one exported
table:

```go
type transition struct {
    from    State
    stage   Stage          // which module runs
    onOK    State
    onFail  State
    guard   Guard          // a named precondition, not inline logic
}
```

The loop is: load run → look up `transition[run.state]` → invoke the named
stage through its module interface → write the resulting state in one
transaction. Every branch a reader might expect (a gate failed, a review
came back with changes, a merge conflicted) is a **row in the table**, not
a code path. Adding a stage is adding a row.

This is also what makes the 44 `run-to-complete` scenarios testable: each
crash window is "kill between the module call and the state write", and the
table says exactly what recovery should find.

### The four seams

Each seam is a package with an empty `may_import`, so nothing internal
leaks into it and it can be replaced wholesale by a fake.

**`devloop` → agents.** A dev agent is a `claude -p` subprocess:

```
claude -p <prompt> \
  --output-format stream-json --verbose \
  --model <resolved from tier config> \
  --allowedTools <assembled per ticket> \
  --append-system-prompt-file <context bundle> \
  --add-dir <workspace>
```

Three ACs get their mechanism from the stream:
`system/init` carries the resolved model → `AC-tier-observed`;
`session_id` stamps every event → `REQ-thread-capture` and
`AC-thread-searchable`; `parent_tool_use_id` reconstructs subagent nesting
→ the TUI's live view. `system/api_retry` events carry `rate_limit` and
`billing_error` categories, which feed `ENT-stall`.

The subprocess boundary is deliberate: parallel dev agents get independent
kill and timeout, and a crashing agent cannot take the orchestrator with
it. `SIGINT` ends a turn cleanly; `SIGTERM` exits 143 leaving the turn
unfinished, which is exactly the crash the recovery scenarios describe.

**`reviewbridge` → roborev.** Enqueue, poll, respond, close. R1 settled the
protocol: roborev's enqueue accepts no correlation key and does not dedupe
by SHA, so kriya acts **only** on ids returned by its own enqueue calls —
no adoption of jobs it did not create, no cancellation of unproven ones.
`EnqueueAttempt` is written before the call so a crash in the window is
visible; unresolved attempts and orphans surface in the TUI inbox.

**`trackerclient` → sutra.** The one package that speaks sutra's HTTP API,
generated from or validated against the pinned OpenAPI contract. Every
mutation carries an idempotency key persisted before the call.

**`gates` → the target's commands.** The gate runner does not know what a
linter or a coverage tool is. It resolves the module's command from the
pinned `SpecSnapshot`, runs it at the head commit, and records the outcome.

### GateResult detail — resolving the deferred question

`ENT-gate-result.detail` is typed `json`, which settles the open question
without new mechanism. The runner captures stdout and stderr and stores a
structured envelope:

```json
{"exit_code": 1, "stdout": "...", "stderr": "...", "duration_ms": 812}
```

`AC-coverage-recorded` asks for uncovered arms "named in detail for the dev
agent". The arms are *in* that captured stdout — `branch-coverage.sh`
prints them — so the requirement is met by capture, not by parsing. Kriya
stays honest to `AC-coverage-floor`: it counts nothing and parses nothing.
A future refinement can add an optional parsed summary alongside the raw
capture, per gate, without changing the contract.

### Recovery model

Every crash window follows one shape:

1. Persist intent + idempotency key (`state = *-ing`, e.g. `submitting`).
2. Perform the external call.
3. Persist the outcome in one transaction.

Recovery replays step 2 under the same key. Sutra's contract guarantees
make the replay safe — that is why those fences exist. Where the external
system has no key (roborev), recovery instead **observes** and surfaces
ambiguity to the operator rather than guessing, per R1.

### Test harness

Two rings, as decided:

- **Unit** — each module against a **fake** `trackerclient` implementing
  the same interface. Fast, hermetic, no network.
- **Integration** — the godog suite against a **real sutra** instance,
  because sutra's fences (verdict-ABA, idempotency replay, atomic pop)
  exist specifically for kriya and a fake would prove the wrong thing.

Sutra is a single binary. The harness starts one per suite against a
temporary SQLite database and tears it down after — the same shape sutra's
own acceptance harness uses for its HTTP server.

## Interfaces

**CLI** (`VIEW-cli-build` and siblings):

```
kriya build <project>          intake a spec and start/resume its build
kriya status [<project>]       current plan, runs, and inbox as text
kriya learn <text>             record an operator learning
kriya tui                      the live TUI
```

**Agent seam** — one Go interface, one real implementation, one fake:

```go
type Agent interface {
    Run(ctx context.Context, req Request) (Result, error)
}
type Request struct {
    Role       Role      // pm | sa | po | dev
    Prompt     string
    Workspace  string
    Tools      []string
    SystemFile string
}
type Result struct {
    SessionID  string   // stamps DevSession and the sutra thread
    Model      string   // resolved model, from system/init
    Transcript string   // path to the durable transcript
    Outcome    Outcome
}
```

**Configuration** — tiers only, no model identifiers in source:

```toml
[tiers]
pm = "opus" ; sa = "opus" ; po = "opus" ; dev = "sonnet"
[models]
opus = "claude-opus-5"
sonnet = "claude-sonnet-5"
```

A role with no configured tier **fails at startup** (`AC-tier-explicit`) —
there is no default.

## Data model

Twenty-five entities become twenty-five SQLite tables, derived from the
avspec `entities` block by the same convention sutra used. Four patterns
carry most of the weight:

| Pattern | Where | Why |
|---|---|---|
| Idempotency key column, unique | `decomposition_key`, `pop_key`, `ticket_close_key`, `review_submission_key`, `finding_doc_key` | Write-ahead replay after a crash |
| Fence column compared under transaction | `PopFence`, `intake_generation`, `close_revision`, `review_resubmission_event` | Reject stale work rather than apply it twice |
| `*-ing` interim states | `submitting`, `resubmitting`, `cancelling`, `merging`, `completing` | Make the crash window visible to recovery |
| Snapshot pinning | `SpecSnapshot`, `spec_hash`, `gated_base` | Working-tree edits never change what a running build executes |

`BuildRun` alone carries 32 fields, most of them the write-ahead and fence
columns above. That width is the price of `AC-intake-snapshot-authority`
and the recovery scenarios; it is not accidental and should not be
normalised away.

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

- **`arch-go.yml` is incomplete and fails closed.** It declares the twelve
  modules but not `cmd/kriya`, `store`, or `acceptance`, and arch-go
  requires 100% package coverage. First commit must add them, using the
  explicit-complement idiom for the empty-allowlist cases.
- **Whole-module mutation is unusable at kriya's size.** Sutra measured
  15h48m over ~1678 mutants and scoped its gate to eight domain-core
  packages. Kriya's command currently takes no directory argument. Expect
  to narrow it, which is a stack-command change to kriya's own spec.
- **The `--bare` fork is unresolved and both branches are bad.** With it,
  agents lose subscription billing; without it, every agent inherits the
  host's `~/.claude`. This must be decided before the first real dev-agent
  run, not discovered.
- **The state table can be undermined without failing any gate.** Nothing
  mechanically prevents a contributor adding judgment to the orchestrator.
  The structural-lint limits are the intended guard; whether they are
  configured strictly enough to catch it is unverified.
- **Sutra's fences are proven by sutra's tests, not by a consumer.** If a
  guarantee is weaker than kriya's recovery assumes, it surfaces here
  first — as a flaky integration test, which is the worst way to learn it.

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
- [ ] **Mutation scope for kriya's own code.** Which packages form the
      "domain core" that the gate runs against. Answerable once module
      sizes are real; sutra's answer was its eight non-wire packages.
- [ ] **Does `trackerclient` generate from the OpenAPI contract or
      hand-roll against it?** Sutra validates responses against the
      contract with kin-openapi at test time; the mirror-image question
      here is whether the client is generated. Affects nothing else, so it
      can be settled at the first `trackerclient` commit.

## Change log

- 2026-08-21: Initial draft (Brent Hoover)
