# AVSpec 0.3 — Restart Design

**Date:** 2026-08-02
**Status:** Approved
**Source of truth:** `tech-spec.md`

## Problem

Agent-driven development devolves into big-ball-of-mud architecture because prose
architecture docs depend on the agent *choosing* to follow them. AVSpec makes good
architecture a **verifiable signal**: a machine-checkable spec format, a verifier that
gates CI, and an interview that produces the spec.

Two prior attempts stalled. Post-mortem:

1. **Wrong model.** The 0.2 format (components/entities/views) did not match how the
   author thinks about architecture. The right vocabulary is
   **project / module / boundary / contract / constitution**.
2. **TUI interview.** A questionary wizard was rigid where the work (graph-wiring,
   follow-up questions) needs flexibility. The interview must be **agent-led**.
3. **Lost the thread.** Code drifted from the plan until restarting was cheaper than
   reconciling. Mitigation: small slices, each ending in a verifiable milestone.

## Decisions

| Decision | Choice |
|---|---|
| Source of truth | `tech-spec.md`; 0.2 design mined for ideas, not binding |
| Vocabulary | project, module, boundary, contract, constitution |
| Format version | `0.3` (0.1 Node and 0.2 design both retired) |
| Interview | Agent-led Claude skill; the verifier is the brain |
| First slice | `verify` + `next` + interview skill + linkshort example |
| IDs | Required, prefix + unique; numeric (`REQ-001`) or slug (`REQ-create-link`) |
| UI | Per-module views+actions graph; functionality only, never layout |
| Implementation | Python 3.12+, uv, ruff, ty, pytest, pytest-bdd |
| Runtime deps (slice 1) | `pydantic`, `ruamel.yaml`, `typer` — exactly three |
| Examples | linkshort (small), ATS (medium), spork itself (large) |

## Format 0.3

One `avspec.yaml` manifest per project; YAML everywhere. Referenced artifacts are
plain YAML, Markdown, Gherkin, and standard contract formats — a spec directory
contains zero Python. Existing specs are reused, never reinvented: **OpenAPI /
AsyncAPI / JSON Schema** for contracts, **Gherkin** for acceptance tests.

```yaml
avspec: "0.3"

project:
  name: linkshort
  description: A minimal URL shortener.
  status: draft            # draft | ready | built
  stack:                   # project-level default
    languages: [{ name: typescript, version: "5" }]
    package_manager: pnpm
    frameworks: [fastify]
    bdd: cucumber-js
    commands:              # how the verifier / CI invokes tooling — free-form strings
      install: pnpm install
      test: pnpm test
      lint: pnpm lint
      arch: pnpm depcruise # hook for code-level boundary enforcement (later slice)

constitution:              # principles and constraints — the "clean code limitations"
  - id: CON-layering
    statement: Dependencies flow api -> domain -> data; no back-edges.

requirements:
  - id: REQ-create-link
    title: Create a short link
    rationale: Users need a compact code that maps to a long URL.
    acceptance:
      - id: AC-valid-url
        statement: When a valid URL is POSTed to /links, the system shall persist it and return 201 with a short code.
        test: verification/REQ-create-link.feature#valid url is shortened

modules:
  - id: MOD-domain
    name: domain
    responsibility: Generates codes and orchestrates create/resolve.
    # stack:               # optional per-module override, same shape as project.stack
    boundaries:
      may_import: [MOD-data]   # explicit allow-list; anything else is a violation
    contracts:
      - id: CTR-shorten
        type: openapi          # openapi | asyncapi | jsonschema
        path: contracts/shorten.openapi.yaml
```

Notes:

- Modules may be parts of one app or separate deployables; `boundaries` governs
  imports, `contracts` governs communication, and the distinction between in-process
  and networked is a stack/deployment concern, not a format concern.
- IDs must be unique and carry their type prefix (`REQ-`, `AC-`, `CON-`, `MOD-`,
  `CTR-`, `VIEW-`, `ACT-`). Style after the prefix is free. The verifier cares only
  about prefix and uniqueness; the interview mints numeric IDs by default.
- No enums for stack tooling — `package_manager`, `frameworks`, `bdd`, `commands.*`
  are free-form strings so no implementation-language assumption calcifies.
- Acceptance statements may be EARS or given/when/then prose; the `test:` reference
  (`path#scenario`) is what the verifier checks.
- The 0.2 `data` and `config` sections are deliberately out of slice 1; they return
  later as ordinary new rules + models if the examples show agents stalling without
  them.

## UI model — resolving the tech-spec open question

**The UI spec covers functionality only, never design.** A module that has a
user-facing surface declares it:

```yaml
modules:
  - id: MOD-web
    name: web-ui
    boundaries: { may_import: [] }   # talks to the API via contract, not imports
    ui:
      kind: web                      # web | cli | tui | none
      entry: VIEW-create
      views:
        - id: VIEW-create
          name: Create link
          route: /                   # `invocation:` when kind is cli
          purpose: Paste a URL, get a short code.
          shows: [short_code, target_url]   # what facts appear — not how
          actions: [ACT-create]
          navigates_to: [VIEW-stats]
          satisfies: [AC-valid-url]
      actions:
        - id: ACT-create
          name: create_link
          invokes: CTR-shorten#createLink   # ties the UI to a contract operation
```

Modeled: what a view shows, what it can do, where it navigates, which acceptance
criteria it satisfies. Not modeled: layout, styling, element hierarchy — prose in
design docs. The boundary is drawn at what can be mechanically verified. With no
`data` section in slice 1, `shows:` entries are free-form strings; if `data`
returns later they upgrade to checked `ENT-*.field` references. Per-module
declaration means multiple surfaces (admin panel, public web, CLI) are just multiple
modules.

## The verifier core

```
src/avspec/
  model.py       # Pydantic v2 models — the canonical format definition
  findings.py    # Finding, Severity, ordering — no other imports
  loading.py     # ruamel.yaml round-trip read -> validated Manifest
  rules/         # one pure function (spec) -> Iterable[Finding] per check, registry
  cli.py         # typer: verify, next
tests/
  features/      # pytest-bdd — CLI behavior
  unit/          # one fixture + one assertion per rule
```

### Severities

- `error` — inconsistency (duplicate ID, dangling reference, boundary cycle,
  action invoking a nonexistent contract operation). Always fails.
- `todo` — incompleteness. Fails only at `status: ready` or `built`.
  **Todos fire on absence**: an empty spec is a wall of todos, never a pass.
- `warn` — advisory. Never fails.

### Commands

- `avspec verify [dir] [--json]` — the CI gate. Exit 0/1.
- `avspec next [dir] --json` — the same findings ordered by authoring order
  (constitution → requirements → acceptance → modules → boundaries → contracts →
  ui → tests), each carrying a human-readable `question`. This is the interview's
  brain.

### Initial rule set (draft — final list fixed during planning)

Errors: `DUPLICATE_ID`, `DANGLING_REF`, `BOUNDARY_CYCLE`, `PREFIX_MISMATCH`,
`UI_ENTRY_MISSING`, `ACTION_UNRESOLVED` (contract-reading version may start as todo).

Todos: `NO_CONSTITUTION`, `NO_REQUIREMENTS`, `REQ_NO_AC`, `AC_NO_TEST`,
`TEST_FILE_MISSING`, `NO_MODULES`, `NO_STACK`, `MOD_NO_RESPONSIBILITY`,
`MOD_NO_BOUNDARIES`, `AC_UNSATISFIED`, `VIEW_UNREACHABLE`, `VIEW_NO_AC`,
`ACTION_ORPHAN`.

Contract files are checked for existence and parseability in slice 1; deep
OpenAPI/AsyncAPI validation and operation lookup is a later rule
(`openapi-spec-validator` joins the dependency list only then). Code-level boundary
enforcement runs through `stack.commands.arch` in a later slice — slice 1 verifies
the spec, not the code.

## The interview

A rewrite of `adapters/guided-qa` (whose loop was already correct — it drives the
retired Node verifier). The skill:

1. Runs `avspec next --json`.
2. Asks about the top gap conversationally — free to dig, reframe, batch related
   follow-ups, and propose defaults.
3. Writes the answer into `avspec.yaml` and companion artifacts.
4. Re-runs. Done = zero todos at `status: ready`.

The skill never decides completeness; the verifier does. Because answers are plain
YAML edits, any agent (or a human with an editor) can drive the same loop.

## Examples — small, medium, large

| Example | Size | Role |
|---|---|---|
| `examples/linkshort/` | Small | Hand-written in slice 1. Must pass `verify` at `ready`; CI conformance fixture. |
| ATS (applicant tracking) | Medium | Authored **via the interview** — first real e2e test; exercises multi-module boundaries and multiple UI surfaces. |
| spork (this repo's parent, an agent harness) | Large | Dogfood. Authored via the interview; any friction is a format defect. |

## Cleanup

- Delete Node tooling: `tools/*.mjs`, `tools/lib/`, `ci/verify.yml` contents replaced.
- Delete current `src/avspec/*` (implements neither prior design) and `example/`
  (0.1 format), `avspec.schema.yaml` (spec dirs no longer carry a schema).
- Archive `feature-work/avspec-0.2-rewrite/` (reference; not binding).
- Rewrite `templates/` to 0.3 vocabulary as the interview needs them.

## Testing

BDD-first, per the operator's standing rule: `.feature` files drive `verify` and
`next` behavior through pytest-bdd before implementation; each rule additionally
gets one unit test over a minimal fixture. `examples/linkshort/` is a conformance
fixture in CI.

## Milestone (slice 1 definition of done)

1. `avspec verify examples/linkshort` exits 0 at `status: ready`.
2. `avspec verify` on an empty directory reports the seed todos, exit 1 at `ready`.
3. The guided-qa skill, driving `avspec next`, takes an empty directory to a
   verifying spec in a live session.

## Risks

| Risk | Mitigation |
|---|---|
| Drift from plan (the "lost thread" failure) | Small slices; each ends at a verifiable milestone; Roborev pass gates progress |
| Format still wrong for big projects | spork dogfood spec is the canary before any 1.0 claim |
| Skill quality varies by model/session | The verifier is deterministic; a bad interview still cannot produce a false `ready` |
| UI model too shallow | Layout stays prose by design; revisit only if agents stall building the examples |
