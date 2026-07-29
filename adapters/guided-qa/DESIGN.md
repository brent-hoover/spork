# Guided-QA authoring adapter — design

One of two authoring paths for an AVSpec. Both converge on the same artifacts and
the same gates:

- **Manual** — an expert fills `templates/`, edits `avspec.yaml`, runs the
  verifier to `PASS`.
- **Guided-QA** (this doc) — an interview drafts the spec *for* the user, using
  the verifier itself to decide what to ask next and when it's done.

## Core principle: the verifier is the brain

The interview logic is **not** hand-written in the adapter. It's derived from the
verifier's structured output. `node tools/verify.mjs <dir> --json` returns a list
of findings; every `todo` finding already carries the exact `question` to ask and
the `ids` it concerns. So the adapter is a thin driver: ask → write the manifest →
re-verify → repeat until no `todo` remains, then flip `status` to `ready`.

This is why the adapter can be a Claude skill *or* a CLI *or* a web form with no
divergence in behavior: they all consume the same JSON and write the same files.
Removing every adapter still leaves a buildable, verifiable spec.

```
        ┌─────────────────────────────────────────────┐
        │  driver (skill / CLI / form)                 │
        │   1. run verify.mjs --json                   │
        │   2. pick next finding (error > todo, by     │
        │      layer order)                            │
        │   3. ask finding.question                    │
        │   4. write user's answer into avspec.yaml    │
        │      (+ expand the Markdown artifact)         │
        │   5. goto 1                                    │
        └─────────────────────────────────────────────┘
             ▲                              │
             │ findings[] (JSON)            │ edits
             │                              ▼
                       avspec.yaml  ←— the single source of truth / state
```

## Two question sources

The verifier's gap list is reactive — it can only ask about IDs that already
exist. To go from an empty repo to a first draft you also need **forward
elicitation**: a proactive, layer-ordered scaffold that seeds the manifest.
Guided-QA runs forward elicitation once, then hands off to the gap loop.

### Forward elicitation order (seed pass)

Ordered by the layers' data dependency — you can't ask about acceptance before a
requirement exists, or tasks before components:

| Stage | Elicits | Notes |
|------|---------|-------|
| 0 · Framing | `metadata.name`, one-line description, confirm `mode: greenfield` | |
| 1 · Constitution | languages (→ `stack`), test-first, dependency direction, other `CON-*` | Offer sensible defaults; most projects accept them. |
| 2 · Requirements | `REQ-*` title + why, looped ("another?") | Pure WHAT/WHY, no design. |
| 3 · Acceptance | per REQ, one+ `AC-*` in EARS + its `test` mapping | Propose the EARS shape; propose a `.feature` scenario name so `test` fills in. |
| 4 · Design | `CMP-*` (propose a decomposition from the requirements, user edits), responsibilities, `depends_on`, interfaces → `CTR-*`, layer placement | Contracts authored or stubbed here. |
| 5 · Tasks | propose `TSK-*` from AC × components, user confirms order/`depends_on`/`[P]` | |
| 6 · Decisions | capture `ADR-*` for anything contested in stages 1/4 | Optional. |

After the seed pass, everything the seed left incomplete surfaces as a `todo` —
so stages 3–6 don't have to be exhaustive on the first pass; the gap loop mops up.

### Gap-closing loop (drain pass)

Drive purely from `--json`. Priority:

1. **`error` findings first** — the spec is *inconsistent* (dangling ref, cycle,
   architecture violation, duplicate ID). These aren't questions so much as
   "you referenced X which doesn't exist" — surface, confirm the fix, edit.
2. **`todo` findings next**, ordered to avoid ping-pong: group by requirement and
   walk `REQ_NO_AC → AC_UNSATISFIED → AC_NO_TEST → *_MISSING`. Closing a
   higher-level gap often creates the next one, so batch per requirement.
3. **`warn`** — mention once (e.g. Markdown/manifest mirror drift); never block.

Stop when `counts.todo === 0`. Then, and only then, offer to set `status: ready`.

## Gap → question taxonomy

Every question is emitted by the verifier; the driver never invents its own. The
mapping (finding `code` → intent):

| code | severity | the question asks… |
|------|----------|--------------------|
| `SCHEMA` | error | fix a malformed manifest shape |
| `DUP_ID` | error | resolve a duplicated identifier |
| `DANGLING` | error | a reference points at a non-existent ID — fix or create it |
| `CYCLE` | error | break a dependency cycle (tasks or components) |
| `ARCH_VIOLATION` | error | a component dependency crosses a forbidden layer edge |
| `NO_REQUIREMENTS` | todo | state the first requirement |
| `REQ_NO_AC` | todo | add an acceptance criterion (EARS) to a requirement |
| `AC_UNSATISFIED` | todo | add a task that delivers this acceptance criterion |
| `AC_NO_TEST` | todo | give the executable test that proves this criterion |
| `LAYER_UNPLACED` | todo | assign a component to a layer |
| `ARTIFACT_MISSING` / `CONTRACT_MISSING` / `ADR_MISSING` / `TEST_MISSING` | todo | author the referenced file |

Adding a new gate to the verifier automatically adds a new interview question —
the adapter needs no change.

## State & resumability

The manifest **is** the state; the driver is stateless. Consequences:

- Stop and resume any time — re-running the verifier reconstructs the exact
  remaining question set from `avspec.yaml`.
- `status: draft` tolerates open `todo`s; `ready`/`built` do not (the ready-gate).
  The driver may only write `status: ready` when `counts.todo === 0`.
- Uncertainty is recorded in-band as `[NEEDS CLARIFICATION: …]` in the Markdown
  and as a missing/partial field in the manifest — never as a silent guess. A
  spec with any `[NEEDS CLARIFICATION]` marker may not reach `ready`.

## Anti-hallucination rules (normative for the driver)

1. **Never invent requirements, criteria, or constraints.** Draft only from the
   user's answers. Proposals (a component decomposition, a task list) must be
   presented as proposals and confirmed before they're written.
2. **Mark, don't guess.** If an answer is ambiguous, write
   `[NEEDS CLARIFICATION: …]` and keep the related field incomplete so a `todo`
   persists — rather than inventing a plausible value.
3. **Confirm before write on synthesis steps** (stages 4–5), where the driver
   proposes structure the user didn't dictate verbatim.
4. **The verifier is the sole arbiter of "done."** No "looks complete to me."
   `ready` requires `counts.todo === 0` and zero `[NEEDS CLARIFICATION]` markers.

## Skill vs CLI

Both are drivers over the same `--json` brain; pick per surface:

- **Claude skill** (`SKILL.md`, shipped alongside this doc) — conversational,
  best when a human is in the loop and answers are prose. It runs the verifier,
  asks the next question in natural language, edits the files, re-runs.
- **CLI wizard** (`tools/interview.mjs`, built) — the deterministic, no-LLM
  driver; better for CI-adjacent or scripted "fill from a YAML of answers" use.
  Reuses the shared `tools/lib/analyze.mjs` brain.

Neither may add a required field to the format. If the format needs a new field,
it goes in the schema + verifier, and both drivers inherit it for free.

## CLI reference (`tools/interview.mjs`)

The CLI owns the *mechanical* half of guided-QA — showing the queue and applying
structured answers — where an LLM isn't needed. (The skill remains the better
*conversational* driver for turning prose answers into structured edits.)

```bash
# show the ordered question queue (errors first, then todos by layer)
node tools/interview.mjs <dir> next [--json]

# merge a structured answer-patch into avspec.yaml; --scaffold also creates
# any referenced-but-missing files (artifact/contract/ADR/.feature stubs)
node tools/interview.mjs <dir> apply <patch.yaml> [--scaffold]

# interactive readline wizard (needs a TTY)
node tools/interview.mjs <dir> ask
```

`analyze.mjs` is the single brain shared by `verify.mjs` and `interview.mjs`, so
the queue, the gate, and the wizard can never disagree. Adding a gate to the
analyzer adds a question to all three at once.

Answer-patch shape (all sections optional; IDs auto-assigned when omitted) — see
`patch.example.yaml`:

```yaml
requirements: [{ id?, title, rationale?, acceptance: [{ id?, ears, test? }] }]
components:   [{ id?, responsibility, depends_on?, interfaces?: [{ name, contract }] }]
contracts:    [{ id?, type, path }]
decisions:    [{ id?, title, status, path }]
tasks:        [{ id?, title, satisfies, touches?, depends_on?, parallelizable? }]
tests:        { AC-001: "verification/acceptance/REQ-001.feature#scenario" }
layers:       { presentation: [CMP-001], domain: [CMP-002], data: [CMP-003] }
```

Note: `apply` rewrites `avspec.yaml` via a YAML serializer, so hand-written
comments in the manifest are not preserved (the manifest is machine-authoritative
data; keep prose in the Markdown artifacts). Manual and CLI editing interleave
freely — re-run `next`/`verify` after either.
