# Adapters

Frontends that drive the AVSpec core. The core exposes two commands —
`avspec verify` (the gate) and `avspec next` (the gap queue) — and every
adapter is a thin driver over them.

- `guided-qa/` — a Claude skill that interviews a human and writes the spec.
  The verifier decides what to ask; the agent conducts the conversation.
