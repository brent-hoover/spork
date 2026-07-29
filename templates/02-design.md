# Design

HOW the system is structured. A C4-style component model plus arc42-lite
crosscutting sections. Components and their dependencies mirror `avspec.yaml`
under `components:`; every exposed interface names a contract (`CTR-*`) that
exists in `contracts/`.

## Context

<One paragraph: the system, its users, and the external systems it talks to.
This is the C4 "System Context" level in prose.>

## Components (C4 container/component level)

- **CMP-001 — <name>.** Responsibility: <single responsibility>.
  - Interfaces: `<InterfaceName>` → **CTR-001** (`contracts/<file>`)
  - Depends on: CMP-002
- **CMP-002 — <name>.** Responsibility: <single responsibility>.
  - Interfaces: none (internal)
  - Depends on: —

> The verifier checks: every `depends_on` and `interface.contract` resolves, and
> that the dependency graph matches `verification/architecture-rules.yaml`.

## Runtime (key scenarios)

<For the 1–3 most important flows, a short step list or sequence description
referencing CMP-* and AC-* IDs. e.g. "REQ-001 happy path: CMP-001 receives …,
calls CMP-002 via CTR-001, returns … (satisfies AC-001).">

## Deployment

<Target topology in brief: processes, datastores, external services. Greenfield —
describe the intended shape.>

## Crosscutting concepts (arc42-lite)

- **Error handling:** <convention>
- **AuthN/AuthZ:** <approach>
- **Observability:** <logging/metrics/tracing conventions>
- **Persistence:** <datastore + access pattern>

## Decisions

Significant choices are recorded as ADRs and mirror `avspec.yaml` under
`decisions:`.

- **ADR-0001 — <title>** (accepted) → `decisions/0001-<slug>.md`
