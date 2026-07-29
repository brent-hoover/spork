# ADR-0001 — Use base62 random codes

- **Status:** accepted
- **Date:** 2026-07-27
- **Relates to:** CMP-002, REQ-001

## Context

Short codes must be URL-safe, compact, and non-enumerable. Sequential IDs are
enumerable; UUIDs are long. Base62 (`[0-9A-Za-z]`) over a random 7-char space
gives ~3.5e12 values, compact and unguessable.

## Decision

We will generate 7-character base62 codes at random and retry on the rare
collision.

## Consequences

Codes are short and opaque. Requires a uniqueness check on insert (CMP-003),
introducing a small retry loop — acceptable at expected volume.
