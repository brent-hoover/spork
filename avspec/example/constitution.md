# Constitution — linkshort

## Principles

- **PR-001 — Library-first.** The shortening logic is a standalone module usable
  without the HTTP layer.
- **PR-002 — Contracts before code.** No endpoint is implemented before its
  OpenAPI operation exists in `contracts/`.

## Constraints (gated)

- **CON-001 — TypeScript only.** *(conformance gate)*
- **CON-002 — Test-first.** Tests mapped from each `AC-*` exist and fail before
  implementation. *(traceability + CI)*
- **CON-003 — Dependency direction.** presentation → domain → data, no
  back-edges. *(fitness function)*
- **CON-004 — No secrets in source.** *(CI secret scan)*
