# Spec gaps — kriya build

Things avspec cannot express that kriya's build must nonetheless do.
Each entry is a deliberate, recorded divergence, not drift. The sutra
build kept the same file for the same purpose.

---

## 2026-08-21 — avspec has no concept of a non-module package

**Found during:** design, before any code existed.

`arch-go` requires 100% package coverage: every Go package in the module
must match a rule. But a real Go program contains packages that are not
domain modules, and avspec's `modules[]` block is the only place boundaries
can be declared. Kriya needs seven such packages:

| Package | Why it is not a module |
|---|---|
| `cmd/kriya` | composition root — wires the modules together |
| `internal/agent` | the `claude -p` seam, used by four modules |
| `internal/specverify` | the `avspec verify` seam |
| `internal/clock` | controllable time, imported everywhere |
| `internal/recovery` | sequences each module's `Recover`; no module can reach them all |
| `internal/fakes` | test doubles for the seams; stripped from the deadcode production pass |
| `internal/acceptance` | the godog harness; test-only |

The consequence is concrete. Six modules use `shouldOnlyDependsOn`
allowlists — planner, orchestrator, devloop, context, cli, tui — and an
allowlist that omits a non-module package **forbids** it. So `arch-go.yml`
must list packages that appear nowhere in `modules[].boundaries.may_import`,
which means the file is no longer a pure mirror of the spec.

A second, quieter observation came out of this: **the two idioms behave
differently under extension.** Adding a package silently FORBIDS it
wherever `shouldOnlyDependsOn` is used and silently PERMITS it wherever the
complement form is used. Six modules originally used the complement form,
so half of kriya's boundaries failed open to any package added later.

**Resolved 2026-08-21** by converting every rule to an allowlist. One
package — `internal/clock` — may import nothing at all, and an empty
allowlist passes silently, so it allows a sentinel pattern matching no
package: enforcing, and stable under extension, leaving no denylist rule in
the file at all. An earlier attempt left clock on a denylist and
reintroduced the same fail-open hole in the one place that could not use a
normal allowlist; roborev caught it.

**Divergence recorded:** `arch-go.yml` enforces avspec's `may_import` for
module-to-module edges, and adds non-module packages that avspec cannot
express. The file header must say so.

**What would close it:** an avspec concept for infrastructure packages —
declared, boundary-checked, but not domain modules. Sutra hit this once
(its composition root); kriya hits it seven times, which suggests it is
structural rather than incidental.

`recovery` is the clearest case, though the claim needs stating carefully.
About fifty scenarios say "when recovery runs", and recovery must
reconcile rows owned by planner, workspace, devloop, orchestrator, and
reviewbridge. `orchestrator` may not import `reviewbridge` directly
(`arch-go.yml:22-29`) — but `devloop` may (`:44-49`), so
`orchestrator → devloop → reviewbridge` is a legal path and sequencing
*could* be forced through it. It is not that the boundaries make
sequencing impossible; it is that every legal route runs cross-module
startup sequencing through a domain module that has no business holding
it, and through the one module `CON-deterministic-orchestrator` most
constrains.

So the gap is narrower than "the spec forbids it" and still real: avspec
can describe modules and their edges, but not a startup concern that
belongs to the composition root and spans every owner.

---

## 2026-08-21 — an entity can be declared by a module that cannot write it

**Found during:** design, second review pass. This one is a real defect the
spec cannot currently express, not merely a missing convenience.

`ENT-agent-invocation` is declared under `MOD-orchestrator`
(`avspec.yaml:474`), and avspec's ownership rule says the owning module
holds the table. But `AC-tier-observed` (`:433`) requires that *"every
agent invocation — plan-scoped PM work included — records its role, the
configured tier, and the resolved model"*, and
`REQ-tier-routing.feature:22` pins the case explicitly: a plan-scoped PM
invocation is recorded **even though no BuildRun exists yet**.

The writers cannot reach the owner:

- `planner` may import only `trackerclient` (`arch-go.yml:17-20`)
- `architect` may not import `orchestrator` (`:82`)
- `owner` may not import `orchestrator` (`:96`)

So under the declared boundaries the row has **no legal writer**. The
entity's own description anticipates the shape — it is deliberately
role-neutral, with exactly one of `plan` or `build` set — but ownership
was assigned to the module that happens to hold the *build* half.

**Divergence recorded:** `internal/agent` owns the `agent_invocation`
table. Every invocation passes through the seam by construction, so the
execution ledger belongs to the seam. `sutra/arch-go.yml:108-128` is the
precedent for a non-module package declaring itself.

**What would close it:** either an avspec ownership form that separates
*who declares* from *who writes*, or a rule that an entity referenced by
modules across a boundary must be owned by something both can reach.

**Generalisation worth keeping:** ownership was assigned by looking at the
entity's most prominent consumer, and the spec's own field list already
said that was wrong — `exactly ONE scope reference is present`, with the
non-build case listed first. A declaration that contradicts its own field
constraints is a defect a verifier could catch, and `avspec verify` cannot
see it today.

---

## 2026-08-21 — a module must act on another module's rows across a boundary it cannot cross

**Found during:** design, fifth review pass. Third instance of the same
family, and the one that proves the family is structural.

Plan retirement must bind in-flight pops: *"a defer conflict re-read that
finds a ticket claimed by a queued BuildRun with an unfilled ticket
replays that pop and binds the row"* (`avspec.yaml:1137-1145`), and
stamps disposition `bound` for live builds (`:1066`).
`REQ-parallel-build.feature:47` pins the ordering — the BuildRun is bound
by pop replay **before any ownership classification**.

But `ENT-build-run` is owned by `MOD-orchestrator` (`:474`), and
`MOD-planner` may import only `MOD-tracker-client` (`arch-go.yml:17-20`).
The retiring module cannot reach the rows retirement is specified to
bind.

The reverse direction is fine and the spec already relies on it —
*"BuildRun write-ahead creation invokes the planner's admission guard"*
(`:1153`) — because orchestrator may import planner. So the boundary is
correctly one-way for **imports** while the **protocol** is two-way.

**Divergence recorded:** `planner` declares a narrow `PopBinder`
interface and does not import its implementer; `orchestrator` implements
it; `cmd/kriya` wires them. Dependency inversion, which arch-go permits
because no new import edge exists.

**What would close it:** avspec can express that A may import B. It cannot
express that A must *call into* B without importing it — an interface A
declares and B satisfies. Every module boundary that carries a two-way
protocol will hit this.

**The pattern across all three gaps.** Each was found the same way: an
acceptance criterion requires a module to touch state the boundaries put
out of reach. `agent_invocation` (entity declared under a module that
cannot write it), `recovery` (a startup concern spanning every owner), and
this one (a two-way protocol under one-way imports). None is visible to
`avspec verify`, because all three are consistent at the level avspec
checks — ids resolve, refs exist, boundaries form a DAG. They are only
visible when someone tries to write the code. That is an argument for the
deferred constitution `check:` attachments: a rule like *"every entity is
writable by every module whose acceptance criteria mutate it"* would have
caught two of the three mechanically.

---

## 2026-08-21 — intake reads the working tree twice, and cannot make that atomic

**Found during:** M2 slice S3, by review.

Intake shells out to `avspec verify` and then to `avspec inspect`. Both read
the target's working tree, in separate subprocesses. If the manifest changes
between them, kriya verifies one version and resolves another — admitting
commands from a spec that was never verified, or verifying a spec whose
commands it never saw.

The same window exists between resolving and reading the artifact files for
the snapshot. `AC-intake-snapshot-authority` guarantees that edits made
*after* intake cannot change a pinned build, and that holds — the snapshot is
the authority from then on. It says nothing about edits made *during* intake,
and nothing in the spec does.

**Narrowed, not closed.** A second bug of the same family WAS closed:
`AdmitAndPin` used to call `Admit` and then resolve a second time, so the
model it validated and the model it pinned were different reads even with no
edit at all. It now resolves once and pins exactly what it validated. What
remains is the genuinely external window between two subprocesses over a
mutable directory.

**What would close it:** avspec emitting verification and resolution from a
single invocation over a single read — `avspec intake <dir>` returning both
the report and the model. Then kriya's remaining exposure is only the
artifact read, which could be closed by having that command return the
artifact contents too, making intake one subprocess and one read.

**Why it is recorded rather than fixed:** the fix belongs in avspec, and the
`inspect` command was already an approved scope exception. Adding a second is
the operator's decision, not something to slip in — and it has a cost beyond
scope: merging would make `verify` carry a payload most callers ignore, and
the response shape would change with the verdict, since a manifest that will
not load has no detail to show. The window it closes is an edit made during
the second or so intake takes, on a single-operator machine.

---

## 2026-08-22 — some acceptance criteria assert an agent's judgement, which no harness can check

**Found during:** M2, writing REQ-decompose step definitions.

Two of the ten decompose scenarios are blocked not by missing machinery but
by steps that are not mechanically decidable:

> the walking skeleton — an implementation ticket touching every layer — is
> the first implementation ticket workable once blocking risks retire

> every implementation ticket is a thin end-to-end slice

These are properties of what the PM agent *produced*. Kriya can require a
shape (the JSON schema does), can require citations to resolve (validation
does), and can order tickets — but "thin", "end-to-end", and "touching every
layer" are judgements about content. A harness cannot decide them without
being a judge itself.

**Three ways this could go, none of them free:**

1. **Assert what kriya guarantees instead** — that the prompt carries the
   instruction and the schema constrains the reply. Cheap, and dishonest as a
   green scenario: the step says the walking skeleton IS first, not that
   kriya asked for it.
2. **Use an LLM judge in the suite.** Kriya already has the shape for this —
   `MOD-owner` is the PO agent, whose entire job is "validates ACs are
   genuinely satisfied and tests are not gaming". But a judge in kriya's OWN
   acceptance suite makes the suite non-deterministic and costly, and a
   flaky gate is worse than an honest gap.
3. **Amend the criteria** to say what is checkable, moving the qualitative
   half into the PO's remit for the target build rather than kriya's suite.

**Decided 2026-08-22: judged elsewhere, not amended.** A middle option was
considered and rejected — have the PM declare `kind` and `walking_skeleton`
on each ticket so kriya could enforce "exactly one skeleton, first among
implementation tickets". It was pitched as a real guard and it is not: all
it enforces is that the agent applied a tag. The agent can tag anything, so
a mislabelled ticket passes exactly as a correct one does. The only genuine
catch is the degenerate case of zero or three skeletons, which is too narrow
to pay for the schema, the validation, and the steps.

The line is the same one avspec already draws: declare what is structural,
judge what is semantic. These steps are semantic.

**Consequence for reporting:** milestones must not count these scenarios as
targets. A suite total of "162 green" is unreachable by construction, and
quoting it as the goal would misreport progress against something that
cannot happen.

The gap is not local: the same shape will recur wherever a criterion describes agent
output rather than kriya's protocol. Measured: **28 steps across the suite carry qualitative wording**
(thin, end-to-end, genuinely, not gaming, sensible, appropriate,
reasonable, quality), concentrated in REQ-decompose and
REQ-po-validation. So this is not two stray steps — it is a category, and
REQ-po-validation is almost entirely made of it, which stands to reason:
judging whether acceptance criteria are genuinely satisfied IS the PO's
job, and a scenario asserting the PO judged well needs a judge to check.

**What it means for "done".** A milestone cannot claim a scenario whose steps
no harness can decide. Those scenarios need naming explicitly, so "162 green"
does not quietly become "the ones we could check".
