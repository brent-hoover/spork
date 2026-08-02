# AVSpec 0.2 — Format Extension and Python Rewrite

**Date:** 2026-07-28
**Status:** Approved design, not yet implemented

## Problem

AVSpec 0.1 verifies that a spec is *internally consistent*. It does not verify that a
spec is *sufficient to build from* — the project's actual goal.

Two defects cause this:

1. **The analyzer only reports on things that are present and broken.** Absent-but-required
   content produces no finding. A spec with zero components, no declared stack, and no UI
   emits zero todos and reaches `status: ready`.
2. **The format has nowhere to put what an agent needs.** No data model, no UI, no runtime
   configuration. That information survives only as prose in `design.md`, which nothing checks.

A third defect compounds them: generated stub files *close* the todos that demanded them, so
`ready` is reachable with empty artifacts.

Gate 3 (contract validation) is also documented in the README but not implemented — `analyze.mjs`
only checks that contract files exist.

## Success criteria

- `status: ready` implies an agent can build the app without further prompting.
- The interview CLI takes a user from an empty directory to a `ready` spec, unassisted.
- The format expresses specs for any target language; nothing in a spec directory is Python.

## Decisions

| Decision | Choice |
|---|---|
| Scope | Rewrite analyzer, verifier, and interview together |
| Language | Python (uv, ruff, ty, pytest) |
| Manifest validation | Pydantic v2 models are canonical |
| Published schema | `avspec.schema.json`, generated from the models |
| Interview UX | Rich TUI (`questionary`) |
| Format | Extended to 0.2 |
| NFR section | Deliberately omitted |
| Examples | Two — one Python, one non-Python |
| Self-spec | Yes, AVSpec specified in AVSpec |

### Why Pydantic over hand-authored JSON Schema

JSON Schema is unpleasant to author and its validation errors are poor. Pydantic v2 is the
project owner's stated default for data models. The models become the format's definition;
`avspec schema --emit` generates `avspec.schema.json` so non-Python consumers retain a way to
validate, but nobody authors JSON Schema by hand.

Consequence: **spec directories no longer carry a schema file.** The tool carries the model.
This removes a duplicated file and a fatal-error path.

`jsonschema` remains as a transitive dependency of `openapi-spec-validator`, used only to
validate contract files a user brings. The format supports `type: jsonschema` contracts, so
this cannot be eliminated — but it is never authored by AVSpec.

### Why no non-functional-requirements section

An NFR that isn't measurable cannot be gated. An NFR that is measurable is an acceptance
criterion with a test. A prose NFR block would create a section the verifier can only wave at —
the exact failure mode this project exists to fix.

Performance and security requirements become `REQ-*`/`AC-*`. Project-wide invariants stay `CON-*`.

---

## Format 0.2

Four additions. Each was chosen against one test: *would an agent stall without this?*

### `stack` — promoted to required-for-`ready`

0.1 has `languages: [string]`, optional. That lets an agent pick Django when you meant FastAPI.

```yaml
stack:
  languages: [{ name: go, version: "1.23" }]
  package_manager: go
  frameworks: [chi]
  bdd: godog
  commands:
    install: go mod download
    test:    go test ./...
    lint:    golangci-lint run
    arch:    go-arch-lint check
```

**`commands` is what makes gate 4 language-agnostic.** The verifier runs what the spec declares;
AVSpec never needs to know how Go, Rust, or TypeScript tooling is invoked.

`package_manager`, `bdd`, and `frameworks` are free-form strings, never enums — an enum is where
implementation-language assumptions calcify. **0.1's `fitness_function` enum is removed** for the
same reason; `commands.arch` replaces it, since the only thing AVSpec needed was how to run the check.

### `data` — the entity model

The single biggest determinant of generated code. Entities carry `ENT-*` IDs and join the
traceability graph.

```yaml
data:
  store: postgres                    # postgres | sqlite | redis | none | other
  entities:
    - id: ENT-001
      name: Link
      description: A shortened URL mapping.
      fields:
        - { name: code,       type: string,   required: true, unique: true }
        - { name: target_url, type: string,   required: true }
        - { name: expires_at, type: datetime, required: false }
      relations:
        - { to: ENT-002, kind: one_to_many, name: visits }
```

`store: none` is the explicit opt-out for apps without persistence. Explicit beats absent.

### `config` — the runtime surface

Without it an agent invents environment variable names.

```yaml
config:
  - id: CFG-001
    name: DATABASE_URL
    type: string
    required: true
    secret: true                     # secret: true ⇒ must never appear in code
    description: Postgres connection string.
```

### `ui` — views, actions, and bindings

0.1 models only machine interfaces. An agent handed a web-app spec must invent every screen.

Modeled: what a view shows, what it can do, where it navigates. Not modeled: individual
elements and layout — that stays prose in `design.md`, covered by the MIRROR check. The
boundary is drawn at what can be mechanically verified.

```yaml
ui:
  kind: web                          # web | cli | tui | none
  entry: VIEW-001
  views:
    - id: VIEW-001
      name: Create link
      route: /                       # `invocation:` when kind is cli
      purpose: Paste a URL, get a short code.
      displays: [ENT-001.code, ENT-001.target_url]
      actions: [ACT-001]
      navigates_to: [VIEW-002]
      satisfies: [AC-001]
  actions:
    - id: ACT-001
      name: create_link
      invokes: CTR-001#createLink
      writes: [ENT-001]
```

`kind: none` is the explicit opt-out for API-only builds.

### Neutral type vocabulary

Entity fields and config entries use a fixed, language-independent set:

`string` · `integer` · `decimal` · `boolean` · `datetime` · `date` · `uuid` · `json` · `enum` · `ref`

Mapping to `str` / `string` / `String` is the building agent's job.

### New graph edges

- `components[].owns: [ENT-*]` — every entity must be owned by exactly one component
- `tasks[].touches` accepts `ENT-*`, `CFG-*`, `VIEW-*`, `ACT-*` alongside `CMP-*`/`CTR-*`

---

## The analyzer

The three-severity model is unchanged: `error` always fails, `todo` fails at `ready`/`built`,
`warn` never fails. The structural change is that **todos now fire on absence**.

### Rules as a registry

Each rule is a pure function `(spec) -> Iterable[Finding]`, registered in an ordered table.
Adding a completeness check is one function plus one test, and `verify` / `next` / `ask` all
inherit it. This preserves the one-brain property 0.1 was built on.

### New findings

**Errors** (well-formedness — always fail):

| Code | Catches |
|---|---|
| `ENT_MULTI_OWNER` | An entity owned by two components |
| `DISPLAY_UNKNOWN_FIELD` | `displays: [ENT-001.foo]` where `foo` is not a field |
| `ACTION_UNRESOLVED` | `invokes:` names a real contract with no such operation |

`ACTION_UNRESOLVED` downgrades to a todo while the target contract is still a stub. It is the
first rule that reads *inside* a contract file, deepening gate 3 from "file parses" to "the
operation the UI calls exists."

**Todos** (completeness — fail at `ready`):

| Absence | Traceability | Content | UI |
|---|---|---|---|
| `NO_STACK` | `AC_NO_OWNER` | `STUB_CONTENT` | `UI_UNDECLARED` |
| `STACK_INCOMPLETE` | `ENT_UNOWNED` | `PLACEHOLDER` | `NO_VIEWS` |
| `NO_COMPONENTS` | `CMP_NO_CONTRACT` | `CONTRACT_EMPTY` | `NO_ENTRY` |
| `NO_TASKS` | | `MIRROR` (promoted) | `VIEW_NO_AC` |
| `NO_PRINCIPLES` | | | `VIEW_UNREACHABLE` |
| `DATA_UNDECLARED` | | | `ACTION_ORPHAN` |

Precise definitions for the two non-obvious ones:

- `AC_NO_OWNER` — no task satisfying this AC touches any `CMP-*`, and no view satisfies it. It
  replaces the narrower `AC_NO_COMPONENT` so UI-only criteria don't demand a backend component
  that needn't exist.
- `CMP_NO_CONTRACT` — a component that another component `depends_on` declares no `interfaces`.
  Leaf components that nothing depends on are exempt.

`VIEW_UNREACHABLE` reuses the graph walk that does cycle detection.

`AC_NO_VIEW` was considered and rejected — "user-visible behavior" is not mechanically
detectable, so the rule would be advisory noise or simply wrong.

### Stubs stop closing their own todos

Scaffolded files carry a marker: `<!-- AVSPEC:STUB -->` in Markdown, `# AVSPEC:STUB` in YAML
and `.feature`. While present, the file reads as absent (`STUB_CONTENT`). Deleting the line is
the author asserting the file is real.

Two content checks back this up where they are cheap and unambiguous:

- `PLACEHOLDER` — `.feature` files still containing `<precondition>`-style placeholders
- `CONTRACT_EMPTY` — OpenAPI documents with empty `paths`

### MIRROR promoted from warn to todo

Every `REQ-*`, `CMP-*`, `ENT-*`, and `VIEW-*` must be mentioned in its Markdown artifact before
`ready`.

The stub marker is honor-system; this is the backstop that isn't. Without it, deleting a marker
from an otherwise-empty `design.md` passes. Cost: `ready` is unreachable from the manifest
alone — the prose must exist.

---

## Implementation

### CLI — one binary, seven subcommands

| Command | Purpose |
|---|---|
| `avspec init [dir]` | Create `avspec.yaml` and templates. Fixes "can't start from an empty directory." |
| `avspec verify [dir] [--json]` | The CI gate. Exit 0/1. |
| `avspec next [dir] [--json]` | Ordered question queue. |
| `avspec ask [dir]` | TUI interview. |
| `avspec apply [dir] <patch.yaml>` | Non-interactive patch — the agent/scripting path. |
| `avspec scaffold [dir]` | Write stub files for referenced-but-missing paths. |
| `avspec schema --emit` | Regenerate `avspec.schema.json` from the models. |

`init` also removes 0.1's fatal-error path: `analyze()` previously returned fatal when
`avspec.schema.yaml` was absent, making an empty directory unstartable.

### Layout

```
src/avspec/
  model.py        # Pydantic v2 — canonical format definition
  loading.py      # ruamel read + validate → Manifest | ValidationErrors
  findings.py     # Finding, Severity, CODE_ORDER, ordered()
  analysis.py     # analyze(dir) -> Report; runs the rule table
  rules/          # wellformed · completeness · content · ui · architecture
  contracts.py    # openapi/jsonschema/asyncapi parse + operation lookup
  scaffold.py
  patch.py        # single mutation engine
  tui.py          # questionary prompt handlers
  cli/            # typer app, one module per subcommand
tests/
  features/       # .feature files — CLI behavior via pytest-bdd
  steps/
  unit/           # one fixture + assertion per rule
```

### The interview

**Queue-driven.** `ask` loops: analyze → take the top finding → prompt → apply patch →
re-analyze.

**Handlers mirror rules.** `HANDLERS: dict[str, Handler]` maps finding code → prompt function
returning a patch. Adding a rule and its question is two registrations. Codes without a handler
print "edit the manifest directly" rather than dead-ending the loop.

**`CODE_ORDER` is rewritten as authoring order.** You cannot ask "which component owns
`ENT-001`?" before components exist:

```
stack → principles/constraints → requirements → acceptance
      → data → components → ui → tasks → tests → files
```

**Answers become patches**, applied through the same engine as `avspec apply`. This is the
third leg of the one-brain property: one analyzer, one patch applier, two drivers. The Claude
skill adapter keeps working by emitting patches.

**Multi-select prompts are where the TUI earns its keep** — "which ACs does this task satisfy,"
"which entities does this component own," "which views does this navigate to." That graph-wiring
is most of the work and is miserable in a readline loop.

**`init` is a short scripted interview** — name, description, `stack`, `ui.kind`, `data.store` —
then hands off to the queue.

Every prompt supports **skip** (defer; remembered for the session so the loop doesn't spin) and
**quit** (save and exit).

### Dependencies

Runtime:

- **`pydantic`** — canonical models and validation
- **`ruamel.yaml`** *(not PyYAML)* — the interview rewrites `avspec.yaml` after every answer;
  PyYAML round-trips destroy comments and reorder keys
- **`openapi-spec-validator`** — gate 3 and `invokes:` operation resolution
- **`questionary`** — the TUI (pulls `prompt_toolkit`)
- **`typer`** — subcommand routing (pulls `click`)

Dev: `ruff`, `ty`, `pytest`, `pytest-bdd`.

AsyncAPI has no maintained Python validator. Its JSON Schema is vendored and validated with
`jsonschema` — no extra dependency.

### Language neutrality

A spec directory contains **zero Python** — plain YAML, Markdown, and Gherkin. The tool is
external, installed with `uvx avspec` or pipx, so a Go or TypeScript project never gains a
Python dependency in its repository.

This property belongs in the README, since it is what makes "tool-agnostic core" true rather
than aspirational.

---

## Migration

- **Delete the Node tooling**: `tools/*.mjs`, `package.json`, `package-lock.json`,
  `node_modules/`. `ci/verify.yml` becomes uv + `avspec verify`.
- **No `migrate` command.** Two 0.1 specs exist and both are ours.
- **`example/` rewritten to 0.2** targeting a non-Python stack (TypeScript or Go). Any Python
  assumption remaining in the format shows up immediately as something the example cannot express.
- **Second example added**, Python-stacked, as a reference for the most likely first users.
- **`example-draft/` rewritten** as a partial 0.2 — the interview's test subject.
- **`templates/02-design.md`** gains data and UI sections so MIRROR is satisfiable.
- **Self-spec added**: AVSpec specified in AVSpec 0.2. Any friction authoring it is a real
  format defect.

## Testing

BDD-first. `tests/features/` drives CLI behavior through pytest-bdd; each rule gets a unit test
over a minimal fixture spec. Rules-as-a-registry makes this mechanical — one fixture, one
assertion per rule.

The three example specs are conformance fixtures: `example/` and the Python example must pass
`ready`; `example-draft/` must report a known todo set.

## Risks

| Risk | Mitigation |
|---|---|
| MIRROR makes authoring feel punishing | It only fires at `ready`; drafts are unaffected |
| Stub markers are honor-system | MIRROR plus the two content checks are the backstop |
| Self-spec churns on every format change | Accepted — that churn *is* the signal |
| Three fixture specs to keep passing | They are CI-verified, so drift fails the build |
| `ui` model too shallow for real apps | Layout stays prose; revisit only if agents actually stall |
