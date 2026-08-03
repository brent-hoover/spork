# Adapters

Frontends that drive the AVSpec core. The core exposes two commands —
`avspec verify` (the gate) and `avspec next` (the gap queue) — and every
adapter is a thin driver over them.

- `guided-qa/` — a Claude skill that interviews a human and writes the spec.
  The verifier decides what to ask; the agent conducts the conversation.
- `build-from-spec/` — a Claude skill for the other direction: an agent
  handed a `ready` spec builds the application from it. Boundaries are
  law, feature files are the definition of done, and spec defects are
  reported, never improvised around.
