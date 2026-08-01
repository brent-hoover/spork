---
name: avspec-guided-qa
description: >-
  Interview a user to author an AVSpec (Agent-Verifiable Architecture Spec) from
  scratch, or fill the gaps in a draft one. Use when someone wants to write a
  machine-verifiable architecture spec through guided questions rather than by
  editing templates by hand — "help me write a spec", "interview me for the
  architecture", "fill in my avspec". Drives every question and the definition of
  done from the AVSpec verifier's --json gap report.
---

# AVSpec Guided-QA

You are authoring an AVSpec by interview. The **verifier is the brain**: it tells
you what to ask and when you're done. You are a thin driver over it. Full rationale
is in `DESIGN.md` next to this file; the operational loop is below.

## Setup

1. Confirm the spec directory (default: current dir). It must contain
   `avspec.yaml`, `avspec.schema.yaml`, and `tools/verify.mjs` (copy the AVSpec
   scaffold in if starting fresh; a minimal manifest with `status: draft`,
   `metadata`, `mode: greenfield`, and the four `artifacts:` paths is enough to
   begin).
2. Run the verifier in JSON mode to read the current state:
   `node tools/verify.mjs <dir> --json`.

## Seed pass (only if the manifest is near-empty)

If there are no requirements yet, elicit a first draft in layer order before
looping. Ask in this sequence, one focused question at a time:

1. **Framing** — project name, one sentence on what it does.
2. **Constitution** — implementation language(s); confirm test-first and the
   dependency direction (presentation → domain → data). Offer these as defaults;
   most users accept. Write `principles`/`constraints` + `stack`.
3. **Requirements** — "What must it do?" Capture each as a `REQ-*` (title + why).
   Loop "anything else it must do?" until they're out.

Then stop seeding and switch to the gap loop — it will pull out acceptance
criteria, design, and tasks as todos.

## Gap loop (the main engine)

Repeat until `counts.todo === 0`:

1. Run `node tools/verify.mjs <dir> --json`.
2. Pick the next finding by priority:
   - **all `error` findings first** — these are inconsistencies (dangling ref,
     cycle, architecture violation, duplicate id). Show the user, confirm the
     fix, edit the manifest.
   - then **`todo` findings, grouped by requirement**, walking
     `REQ_NO_AC → AC_UNSATISFIED → AC_NO_TEST → *_MISSING`.
3. Ask the finding's `question` verbatim-in-spirit (rephrase naturally; keep the
   IDs). Ask **one** thing at a time.
4. Write the answer into `avspec.yaml` (the source of truth) **and** expand the
   matching Markdown artifact so the IDs mirror (avoids `MIRROR` warnings).
   - New `AC-*`: write it in EARS; propose a `.feature` scenario name and set
     `test:` to `verification/acceptance/<REQ>.feature#<scenario>`, then create
     that scenario file.
   - New `TSK-*`: set `satisfies`, `touches`, `depends_on`.
   - New `CMP-*`: set responsibility, `depends_on`, interfaces→`CTR-*`, and place
     it in `verification/architecture-rules.yaml`.
5. Re-run and continue. Mention `warn` findings once; don't block on them.

For synthesis steps (proposing a component decomposition or a task breakdown),
present your proposal and get a yes before writing it.

## Finishing

When `counts.todo === 0` and there are no `[NEEDS CLARIFICATION]` markers left,
tell the user the spec is complete and offer to set `metadata.status: ready`.
Run the verifier once more in text mode to show the green `PASS (status: ready)`.
The spec is now ready to hand to a build agent.

## Hard rules

- Never invent requirements, criteria, or constraints — draft only from answers.
- On ambiguity, write `[NEEDS CLARIFICATION: …]` and leave the field incomplete
  (so a todo persists) rather than guessing.
- Only the verifier decides "done." Never set `status: ready` while any `todo`
  or `[NEEDS CLARIFICATION]` marker remains.
- Don't add fields the schema doesn't define. If the format is missing something,
  say so — it's a change to the schema + verifier, not something to smuggle into
  the manifest.
