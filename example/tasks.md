# Tasks — linkshort

- **TSK-001 — Implement LinkStore and the create path returning 201 + code.**
  Satisfies: AC-001. Touches: CMP-001, CMP-002, CMP-003, CTR-001. Depends on: —
- **TSK-002 — Add URL validation returning 400 on malformed input.**
  Satisfies: AC-002. Touches: CMP-001. Depends on: TSK-001.
- **TSK-003 — Implement resolve path with 302 redirect for known codes.**
  Satisfies: AC-003. Touches: CMP-001, CMP-002. Depends on: TSK-001.
- **TSK-004 `[P]` — Return 404 for unknown codes.**
  Satisfies: AC-004. Touches: CMP-001. Depends on: TSK-003.
