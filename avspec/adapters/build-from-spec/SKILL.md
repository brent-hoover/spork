---
name: avspec-build-from-spec
description: >-
  Build an application from a ready AVSpec (Agent-Verifiable Architecture
  Spec). Use when handed a spec directory containing avspec.yaml with
  status ready and asked to implement it — "build this from the spec",
  "implement the avspec", "pick up this ticket" in a spec-carrying repo.
  The spec is the requirements; the verifier and the spec's own commands
  are the definition of done.
---

# AVSpec Build-From-Spec

You have a `ready` spec. `ready` means: buildable without further
prompting. Everything you need is in the spec directory — do not
re-interview the human. If something genuinely blocks you, that is a spec
defect: report it, don't improvise around it.

## Read order

1. `avspec.yaml` — the manifest. Confirm `avspec verify <dir>` exits 0
   before you start; a failing spec is not yours to build yet.
2. `project.description` + `constitution` — the envelope facts and the
   rules every line of code must obey.
3. `project.stack` — languages, frameworks, store, and the exact
   `commands` for install/test/lint/typecheck — plus `coverage` (branch
   coverage, every conditional arm) and `mutation` (proves the tests
   test) when declared. A module may carry its own `stack:` — the
   module's EFFECTIVE stack is its override where present, the project
   stack otherwise, per field. Use these commands, not your habits.
4. `data.entities` — the schema. Field types are the neutral vocabulary
   (string, integer, decimal, boolean, datetime, date, uuid, json, enum,
   ref); map them to the stack's types yourself.
5. `modules` + `apps` — the shape you are filling in.
6. `requirements[].acceptance` + `verification/*.feature` — the
   executable definition of done.
7. `contracts/` — API surfaces. Operations referenced by `ui.actions`
   (`CTR-x#operation`) must exist there as you flesh them out.

## Boundaries are law

- A module imports ONLY what its `boundaries.may_import` lists. No
  exceptions, no "just this once" — the architecture rule in
  `stack.commands.arch` (when present) and code review will catch it.
- `owns: [ENT-*]` decides where an entity's persistence code lives.
  Exactly one module reads/writes each entity's state; everyone else goes
  through that module's interface.
- Modules in different apps NEVER import each other — they communicate
  through the declared contracts only.
- The UI (`ui.views` / `ui.actions`) implements what's declared: views,
  what they show, where they navigate, which operations actions invoke.
  Layout and styling are yours; the graph is not.

## Build loop (test-first)

The spec's constitution almost certainly demands test-first; honor it
mechanically:

1. Wire the `verification/*.feature` files to the declared `bdd` runner
   first. Every `AC-*` has a `test:` ref (`path#scenario`) — those
   scenarios are the acceptance suite. They should all fail before
   implementation and all pass at the end.
2. Work module by module, dependency-leaves first (modules with
   `may_import: []`), then up the import graph. The manifest's boundary
   graph is your build order.
3. After every change run the EFFECTIVE commands of the module you are
   in — its stack override where present, the project stack otherwise:
   `test`, `lint`, `typecheck` (and `arch` if declared). Green before
   moving on.
   `coverage` and `mutation`, where declared, are the FINAL gates: run
   each declared one after the acceptance suite passes and before
   declaring any module or the build done — they are slower, but
   skipping a declared gate is skipping the definition of done. A spec
   that declares neither simply has no final gate beyond the suite.
4. Re-run `avspec verify <dir>` whenever you touch the spec directory
   itself (new contract operations, etc.). It must stay green.

## Definition of done

- Every scenario referenced by every `AC-*` passes under the declared
  runner.
- Every module's EFFECTIVE stack commands exit 0 — every declared command, including `coverage` and `mutation` where present.
- `avspec verify <dir>` exits 0.
- No import violates a boundary; no entity is touched outside its owner.

Only then may `project.status` move to `built` — and that flip is the
human's call to make, not yours to assume.

## When the spec is wrong

Reality wins, but the spec leads. If implementation reveals a defect —
a missing entity field, an impossible boundary, a contract operation the
UI needs but nobody declared:

1. STOP building the affected part.
2. Amend `avspec.yaml` (and its artifacts) first; run `avspec verify`.
3. Report the change to the human with one line of why.
4. Then write the code that matches the amended spec.

The spec never trails the code. A divergence you don't record is the
big-ball-of-mud seed this whole system exists to prevent.
