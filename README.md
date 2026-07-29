# AVSpec — Agent-Verifiable Architecture Spec

A standard, machine-verifiable format you generate once and hand to any coding
agent to build from. It reuses the now-common spec-driven skeleton
(**constitution → requirements → design → tasks**, à la GitHub Spec-Kit and
Kiro) but adds the two things those tools leave to "discipline": a **formal,
schema-validated manifest** that defines what a *valid* spec is, and a
**CI-gated verification layer** that mechanically fails when the delivered code
drifts from the spec.

Scope of this version: **greenfield** builds, **tool-agnostic** core (any agent
can consume the plain files; per-tool adapters are thin and optional).

---

## Why another format

Most spec-driven tools treat the spec as prose the agent is *trusted* to follow.
Nothing checks the agent's output against the spec. AVSpec's design goal is the
opposite: **every requirement is traceable to an executable check, and a machine
— not a reviewer — decides whether the build conforms.**

Two properties make that possible:

1. **Stable IDs on everything.** Every principle, requirement, acceptance
   criterion, component, contract, and task has a stable identifier. Cross-layer
   links are by ID, so a rename or deletion is detectable.
2. **A traceability graph that must be complete.** The verifier walks
   `requirement → acceptance criterion → task → test → code/contract` and fails
   on any dangling or unsatisfied node — *before* a line of code exists, and
   again after.

---

## Anatomy

```
avspec/
├── avspec.yaml              # the manifest — the machine-readable spine
├── avspec.schema.yaml       # schema (JSON Schema draft 2020-12, written in YAML)
├── templates/
│   ├── 00-constitution.md   # principles & hard constraints (project-wide)
│   ├── 01-requirements.md    # WHAT + WHY, acceptance criteria in EARS
│   ├── 02-design.md         # HOW — C4/arc42-lite component model
│   ├── 03-tasks.md          # ordered, ID-linked work items
│   └── adr-template.md      # one decision record per file
├── contracts/               # OpenAPI / JSON Schema / AsyncAPI — hard-validated
├── verification/
│   ├── architecture-rules.yaml   # allowed/forbidden dependencies (fitness fns)
│   └── acceptance/          # EARS criteria → Gherkin/tests, one .feature per req
├── adapters/                # optional thin per-tool bindings (neutral core stays clean)
│   └── claude/              # e.g. CLAUDE.md + slash-command that runs the flow
├── ci/verify.yml            # GitHub Actions gate
└── tools/
    ├── lib/analyze.mjs      # shared analyzer — one brain for verify + interview
    ├── verify.mjs           # the runnable verifier (CI gate)
    └── interview.mjs        # guided-QA CLI (next / apply / ask)
```

The `example/` directory is a filled, passing instance you can run the verifier
against today.

---

## The four layers

**Constitution** (`00-constitution.md`) — project-wide principles and *hard*
constraints that every downstream artifact must honor: allowed languages, the
test-first rule, dependency direction, "no secrets in code," etc. These become
gates, not suggestions. IDs: `PR-*` (principle), `CON-*` (constraint).

**Requirements** (`01-requirements.md`) — WHAT and WHY only, no implementation.
Each requirement (`REQ-*`) owns one or more **acceptance criteria** (`AC-*`)
written in **EARS** notation so they translate cleanly to executable tests.
`[NEEDS CLARIFICATION]` markers are allowed but block `status: ready`.

**Design** (`02-design.md`) — HOW. A C4-style component model (`CMP-*`) with
responsibilities and the interfaces between them, plus arc42-lite crosscutting
sections. Every component that exposes an interface references a **contract**
(`CTR-*`) in `contracts/`. Design decisions link to ADRs (`ADR-*`).

**Tasks** (`03-tasks.md`) — the agent's build plan. Ordered work items (`TSK-*`),
each declaring which `AC-*` it satisfies and which `CMP-*`/`CTR-*` it touches.
`[P]` marks safe-to-parallelize tasks.

---

## The verification layer (what makes it "verifiable")

Four gates, run by `tools/verify.mjs` locally and in CI:

1. **Schema gate.** `avspec.yaml` validates against `avspec.schema.yaml`. If the
   manifest is malformed, nothing else runs. This is what makes AVSpec a
   *format* rather than a template.

2. **Traceability gate.** The verifier builds the ID graph and asserts
   completeness:
   - every `REQ-*` has ≥1 `AC-*`;
   - every `AC-*` is satisfied by ≥1 `TSK-*` **and** mapped to a test
     (`verification/acceptance/*.feature` or a test file path);
   - every `TSK-*` references real `AC-*`/`CMP-*` IDs;
   - every `CMP-*` interface references a real `CTR-*`;
   - no dangling references, no orphans.
   This runs with **zero code written** — it verifies the *spec* is buildable and
   complete before an agent starts.

3. **Contract gate.** Every file in `contracts/` validates against its own
   standard (OpenAPI 3.x, JSON Schema, AsyncAPI). Interfaces named in the design
   must resolve to a real operation/schema in a contract.

4. **Conformance gate** (post-build). The declared `architecture-rules.yaml`
   dependency rules run as a language-appropriate fitness function
   (dependency-cruiser / import-linter / ArchUnit / deptrac — chosen per stack),
   and the acceptance tests must pass. A build that violates a declared
   dependency or fails an acceptance test fails CI.

Gates 1–3 verify the **spec**. Gate 4 verifies the **code against the spec**.
Together they close the loop Spec-Kit leaves open.

---

## How an agent consumes it

The intended handoff, tool-agnostic:

1. Agent reads `avspec.yaml` to discover the artifacts and their order.
2. Agent reads constitution → requirements → design → tasks.
3. Agent runs `node tools/verify.mjs` and refuses to start if gates 1–3 fail
   (the spec isn't ready).
4. Agent implements `TSK-*` in dependency order, writing code + the tests each
   `AC-*` maps to.
5. CI runs all four gates on the PR. Green = conformant to spec.

No slash-commands are required — they're an optional convenience in
`adapters/`. The contract is the files.

---

## Relationship to existing tools

- **Skeleton & names** borrowed from **Spec-Kit** (constitution/spec→requirements/
  plan→design/tasks) for familiarity and rough interop.
- **EARS** acceptance notation borrowed from **Kiro**'s requirements style.
- **C4 + arc42** for the design layer; **ADRs** (Nygard) for decisions.
- **New here:** the JSON-Schema manifest, the enforced traceability graph, and
  the four-gate CI verifier — i.e. the machine-verifiability.

`avspec` is a working name; rename freely.

---

## Authoring a spec — two paths, one output

Both paths produce the same files and must pass the same gates; the format is
agnostic to how it was written.

- **Manual** — copy `templates/`, fill `avspec.yaml` (authoritative), run the
  verifier until `PASS`.
- **Guided-QA** — an interview drafts it for you, using the verifier's gap report
  to decide what to ask and when it's done. Two drivers over one shared analyzer
  (`tools/lib/analyze.mjs`): a conversational **Claude skill**
  (`adapters/guided-qa/SKILL.md`) and a deterministic **CLI**
  (`tools/interview.mjs`) for offline/scripted use. Both ask the next open
  `todo`'s `question`, write the answer, and repeat until no todo remains. See
  `adapters/guided-qa/DESIGN.md`.

The verifier supports draft authoring: **well-formedness** (shapes, ID patterns)
is always enforced, but **completeness** (every AC has a test, every AC is
satisfied by a task, …) is reported as `todo` and only *fails the build* once
`status: ready`. So a partial `draft` is valid and its gaps are the interview.

## Getting started

```bash
npm install                          # ajv + js-yaml

# verify (the CI gate)
node tools/verify.mjs example        # complete spec (status: ready) → PASS
node tools/verify.mjs example-draft --json   # partial draft → JSON gap/question queue

# author by guided-QA (the CLI driver)
node tools/interview.mjs example-draft next          # show the open questions
node tools/interview.mjs <dir> apply <patch.yaml> --scaffold   # apply answers + stub files
node tools/interview.mjs <dir> ask                   # interactive wizard (needs a TTY)
```

To author a new spec: copy the `templates/` into place, fill `avspec.yaml`
(the manifest is authoritative), and run the verifier until it prints `PASS`.
The verifier exits non-zero on failure, so it drops straight into CI
(`ci/verify.yml`).

The `example/` linkshort spec is deliberately small but complete — 2
requirements, 4 acceptance criteria, 3 layered components, 4 tasks — and passes
all spec-level gates. Mutating it (removing a task, adding a back-edge,
mistyping an ID) makes the corresponding gate fail, which is the point.
