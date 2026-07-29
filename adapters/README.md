# Adapters

The AVSpec core is plain files + a manifest — any agent can read it with no
adapter. Adapters are **thin, optional** per-tool conveniences that generate or
drive the same files; they never become the source of truth.

- `claude/` — a `CLAUDE.md` fragment and a suggested slash-command flow for
  Claude Code / Claude.
- Add `copilot/`, `cursor/`, `kiro/` etc. the same way: each just wires that
  tool's command surface to "read the four layers, run `tools/verify.mjs`, build
  tasks in order." No adapter may add required fields to the format.

Rule: if removing every adapter still leaves a buildable, verifiable spec, the
core stayed clean. It must.
