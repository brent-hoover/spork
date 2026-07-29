# Requirements

WHAT the system must do and WHY — no implementation detail. Each requirement
owns one or more acceptance criteria in **EARS** notation. Every `AC-*` must map
to an executable test before the spec can reach `status: ready`.

> IDs here mirror `avspec.yaml` under `requirements:`. Unresolved
> `[NEEDS CLARIFICATION]` markers block `ready`.

**EARS quick reference** — write each criterion as one of:
- *Ubiquitous:* "The <system> shall <response>."
- *Event:* "When <trigger>, the <system> shall <response>."
- *State:* "While <state>, the <system> shall <response>."
- *Option:* "Where <feature>, the <system> shall <response>."
- *Unwanted:* "If <condition>, then the <system> shall <response>."

---

## REQ-001 — <requirement title>

**Why:** <the user/business reason this exists>

**Acceptance criteria:**

- **AC-001** — When <trigger>, the system shall <observable response>.
  → test: `verification/acceptance/REQ-001.feature#<scenario>`
- **AC-002** — If <error condition>, then the system shall <handled response>.
  → test: `verification/acceptance/REQ-001.feature#<scenario>`

## REQ-002 — <requirement title>

**Why:** <reason>

**Acceptance criteria:**

- **AC-003** — The system shall <ubiquitous response>.
  → test: `tests/<path>::<test_id>`
