# Design — linkshort

## Context

A single HTTP service. External callers create links and follow short codes;
mappings persist in a datastore. No other systems involved.

## Components

- **CMP-001 — HTTP edge.** Validates requests, maps to the domain service.
  - Interfaces: `ShortenHTTP` → **CTR-001** (`contracts/shorten.openapi.yaml`)
  - Depends on: CMP-002
- **CMP-002 — Domain.** Generates codes, orchestrates create/resolve.
  - Depends on: CMP-003
- **CMP-003 — Data.** Persists and looks up code→URL mappings.
  - Depends on: —

## Runtime

- **Create (REQ-001):** CMP-001 validates → CMP-002 generates a base62 code →
  CMP-003 persists → 201 with code (satisfies AC-001; AC-002 on invalid input).
- **Resolve (REQ-002):** CMP-001 → CMP-002 looks up via CMP-003 → 302 (AC-003) or
  404 (AC-004).

## Deployment

One stateless Node process + a key/value store. Config injected at runtime
(CON-004).

## Crosscutting

- **Error handling:** problem+json bodies; 4xx for client errors.
- **Observability:** structured logs keyed by short code.
- **Persistence:** code→URL map, code is the key.

## Decisions

- **ADR-0001 — Use base62 random codes** (accepted) → `decisions/0001-base62-codes.md`
