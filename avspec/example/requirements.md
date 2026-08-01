# Requirements — linkshort

## REQ-001 — Create a short link

**Why:** Users need a compact code that maps to a long URL.

- **AC-001** — When a valid URL is POSTed to `/links`, the system shall persist
  it and return 201 with a short code.
  → test: `verification/acceptance/REQ-001.feature#valid url is shortened`
- **AC-002** — If the submitted URL is malformed, then the system shall return
  400 and persist nothing.
  → test: `verification/acceptance/REQ-001.feature#malformed url is rejected`

## REQ-002 — Resolve a short link

**Why:** A short code must send the visitor to the original URL.

- **AC-003** — When a request targets a known short code, the system shall
  respond 302 to the original URL.
  → test: `verification/acceptance/REQ-002.feature#known code redirects`
- **AC-004** — If a request targets an unknown short code, then the system shall
  respond 404.
  → test: `verification/acceptance/REQ-002.feature#unknown code is not found`
