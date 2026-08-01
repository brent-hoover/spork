# Constitution

Project-wide principles and hard constraints. Every requirement, design element,
and task must honor these. Constraints marked as gated are enforced mechanically
by the verifier or CI, not left to review.

> Each entry's ID must also appear in `avspec.yaml` under `principles:` /
> `constraints:`. The manifest is authoritative; this file is the readable form.

## Principles

- **PR-001 — <name>.** <One-sentence principle. e.g. "Library-first: each
  capability is a standalone module with no hidden coupling to the host app.">
- **PR-002 — <name>.** <e.g. "Contracts before code: no interface is implemented
  before its contract exists in `contracts/`.">

## Constraints (gated)

- **CON-001 — Allowed languages.** Implementation uses only: `<lang>`. *(Enforced:
  conformance gate.)*
- **CON-002 — Test-first.** No implementation code is merged before the test
  mapped to its `AC-*` exists and initially fails. *(Enforced: traceability +
  CI.)*
- **CON-003 — Dependency direction.** Dependencies flow one way per
  `verification/architecture-rules.yaml`; forbidden edges fail the build.
  *(Enforced: fitness function.)*
- **CON-004 — No secrets in source.** *(Enforced: CI secret scan.)*

Add project-specific constraints below. Keep each one *checkable* — if you can't
name the gate that enforces it, it's a principle, not a constraint.
