---
name: avspec-guided-qa
description: >-
  Interview a user to author an AVSpec 0.3 (Agent-Verifiable Architecture
  Spec) from scratch, or fill the gaps in a draft one. Use when someone wants
  a machine-verifiable architecture spec through guided questions — "help me
  write a spec", "interview me for the architecture", "fill in my avspec".
  Drives every question and the definition of done from `avspec next --json`.
---

# AVSpec Guided-QA

You are authoring an AVSpec by interview. **The verifier is the brain**: it
decides what to ask about and when the spec is done. You are the mouth — free
to dig deeper, reframe questions naturally, batch related follow-ups, and
propose sensible defaults. You never decide completeness; `avspec verify` does.

## Setup

1. Confirm the spec directory (default: current dir).
2. If `avspec.yaml` does not exist, create the minimal manifest:

   ```yaml
   avspec: "0.3"
   project:
     name: <ask>
     description: <ask — one sentence>
     status: draft
   ```

3. Run `avspec next <dir> --json` to read the current gap queue.
   (In this repo: `uv run avspec next <dir> --json`.)

## The authoring flow

The queue enforces this order; conduct the conversation to match it:

1. **Vision + non-negotiables** — 1–3 sentences into `project.description`.
   Apply the user's standing constitution defaults (test-first, one-way
   dependency flow, no secrets in source, idempotent mutations) with a
   one-line confirmation, not a question each; spend the actual question on
   THIS system's project-specific principles.
2. **Functionality inventory** — always, for every project. Broad
   capability bullets, not user stories yet. For a well-known app type
   (issue tracker, shop, CMS), PROPOSE the genre's baseline for the user to
   prune and extend rather than asking open-ended. Each survivor becomes a
   `REQ-*` with a title (rationale optional for now).
3. **Modules** — if the inventory is long, group like functionality.
   Heuristics: capabilities sharing the same core nouns (data ownership)
   live together; things that change together live together; a capability
   that merely *calls* another belongs behind a contract. Propose a
   grouping; the user reshapes it.
4. **Data** — the entities, their fields (neutral types), relations, and
   which module owns each. Entity ownership is also the strongest check on
   the module grouping from step 3.
5. **Apps** — one deployable or several. A critical, deliberate decision:
   crossing a process boundary changes failure semantics, transactionality,
   and enforcement. One app is a valid answer; split for forcing reasons
   (runtime shape, deploy cadence, isolation, scaling, language). Imports
   never cross apps — only contracts do.
6. **Boundaries and contracts** — what each module may import; how modules
   (and apps) communicate.
7. **User stories / acceptance criteria** — turn every REQ into testable
   ACs, then UI views/actions where a surface exists.
8. **Tests** — a Gherkin scenario reference per AC.
9. **Stack** — asked last; implementation details follow shape.
10. **Validate** — `avspec verify` green at `ready`. Building the app is
    out of scope for the interview (that is the harness's job).

## The loop

Repeat until `avspec next` returns zero findings:

1. Run `avspec next <dir> --json`.
2. Take the FIRST finding (the queue is already in authoring order,
   errors before todos). Its `question` field is your prompt — rephrase it
   naturally, keep any IDs verbatim. If `question` is null, the finding is an
   inconsistency, not a gap to ask about — explain and fix what `message`
   describes, then continue.
3. Ask ONE question at a time. Digging deeper on an answer is encouraged;
   moving to a different finding before writing the current answer is not.
4. Write the answer into `avspec.yaml` (and create any referenced files —
   `.feature` scenarios, contract stubs). Mint numeric IDs (`REQ-001`
   style) unless the user offers slugs. Prefixes are fixed:
   CON- REQ- AC- MOD- CTR- VIEW- ACT-.
5. Re-run and continue.

If the user skips a question, note it and move to the next finding; re-ask
skipped ones at the end rather than spinning on them.

## Writing answers

- `NO_CONSTITUTION` → offer defaults and confirm: dependency direction
  between the modules they expect, test-first, no secrets in source.
- `NO_REQUIREMENTS` → loop "what must it do?" until the user is out;
  one REQ per answer with title and rationale.
- `REQ_NO_AC` → elicit testable statements (EARS or given/when/then prose).
- `AC_NO_TEST` → propose a scenario name; set the ref in the required
  `path/to/file.feature#scenario name` format and create the file with
  that scenario sketched in Gherkin.
- `TEST_REF_INVALID` / `TEST_SCENARIO_MISSING` → the ref is malformed
  (missing `#scenario name`, or the path isn't a `.feature` file) or the
  named scenario isn't in the file; fix the ref or add the scenario.
- `NO_MODULES` → ask for the units of the system, one responsibility each.
- `NO_DATA` → elicit the core entities, their fields, and their relations;
  an explicit "no persistent data" (`data: {entities: []}`) is valid.
- `ENT_NO_FIELDS` → ask what fields the entity has.
- `ENT_UNOWNED` / `ENT_MULTI_OWNER` → ask which single module reads and
  writes this entity's state; ownership must be exactly one module.
- `MOD_NO_BOUNDARIES` → ask which modules each may import. An empty list is
  a valid, explicit answer.
- UI findings (`UI_NO_VIEWS`, `UI_NO_ENTRY`, `VIEW_NO_AC`,
  `VIEW_UNREACHABLE`, `ACTION_ORPHAN`) → the UI spec is functionality only:
  screens, what each shows, what each can do, where each navigates. Never
  ask about layout or styling.
- `NO_APPS` → ask which deployable application(s) the modules belong to.
  One app is a valid, explicit answer; only split into more when there's a
  forcing reason (different runtime shape, deploy cadence, isolation,
  scaling).
- `MOD_NO_APP` / `MOD_MULTI_APP` → ask which single app runs this module.
- `BOUNDARY_CROSS_APP` → a module imports one living in a different app;
  imports can't cross a process boundary — replace it with a contract.
- `NO_STACK` → asked last: stack is an implementation detail, chosen once
  requirements and modules are known. Fill `project.stack`: languages,
  package_manager, frameworks, bdd, store, and the install/test/lint
  commands. Free-form strings.

## Done

When `avspec next` is empty at `status: draft`, ask the user whether to
promote to `status: ready`, set it, and run `avspec verify <dir>` — it must
exit 0. Show the user the final summary line.
