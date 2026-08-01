#!/usr/bin/env node
// AVSpec guided-QA CLI — the deterministic (no-LLM) authoring driver.
// Same brain as the verifier (tools/lib/analyze.mjs); the CLI owns the
// mechanical side: show the ordered question queue, and apply structured
// answers as a patch to the manifest.
//
// Commands:
//   node tools/interview.mjs <dir> next [--json]
//       Print the ordered queue: blocking errors first, then the open
//       questions (todos) in authoring-layer order. Exit 0 if the spec passes
//       for its current status, else 1.
//
//   node tools/interview.mjs <dir> apply <patch.yaml> [--scaffold]
//       Merge a structured answer-patch into avspec.yaml (upserts requirements,
//       acceptance, components, contracts, decisions, tasks; sets tests; merges
//       layers). With --scaffold, also create any referenced-but-missing files
//       (artifact stubs, contract stubs, ADR stubs, .feature test stubs).
//       Re-analyzes and prints the todo delta.
//
//   node tools/interview.mjs <dir> ask
//       Interactive readline wizard: walk the todos, prompt for each, write the
//       answer, repeat. (Needs a TTY. The Claude skill is the richer
//       interactive option; this is the offline equivalent.)
//
// Patch shape (all sections optional; ids auto-assigned when omitted):
//   requirements: [{ id?, title, rationale?, acceptance: [{ id?, ears, test? }] }]
//   components:   [{ id?, responsibility, depends_on?, interfaces?: [{name, contract}] }]
//   contracts:    [{ id?, type, path }]
//   decisions:    [{ id?, title, status, path }]
//   tasks:        [{ id?, title, satisfies, touches?, depends_on?, parallelizable? }]
//   tests:        { AC-001: "verification/acceptance/REQ-001.feature#scenario" }
//   layers:       { presentation: [CMP-001], domain: [CMP-002], data: [CMP-003] }

import fs from "node:fs";
import path from "node:path";
import process from "node:process";
import YAML from "js-yaml";
import { analyze, ordered } from "./lib/analyze.mjs";

const argv = process.argv.slice(2);
const flags = new Set(argv.filter((a) => a.startsWith("--")));
const pos = argv.filter((a) => !a.startsWith("--"));
const specDir = path.resolve(pos[0] || ".");
const cmd = pos[1] || "next";

const RULES_REL = "verification/architecture-rules.yaml";
const p = (rel) => path.join(specDir, rel);
const exists = (rel) => fs.existsSync(p(rel));

try {
  if (cmd === "next") cmdNext();
  else if (cmd === "apply") await cmdApply(pos[2]);
  else if (cmd === "ask") await cmdAsk();
  else { console.error(`unknown command: ${cmd}`); process.exit(2); }
} catch (e) {
  console.error(`error: ${e.message}`);
  process.exit(1);
}

// ---- commands ---------------------------------------------------------------
function cmdNext() {
  const r = requireAnalysis();
  const q = ordered(r.findings);
  const errs = q.filter((f) => f.severity === "error");
  const todos = q.filter((f) => f.severity === "todo");
  const warns = q.filter((f) => f.severity === "warn");

  if (flags.has("--json")) {
    console.log(JSON.stringify({ status: r.status, pass: r.pass, counts: r.counts,
      queue: [...errs, ...todos].map((f, i) => ({ n: i + 1, severity: f.severity, code: f.code, ids: f.ids, ask: f.question || f.message })) }, null, 2));
    process.exit(r.pass ? 0 : 1);
  }

  console.log(`\n${r.manifest.metadata.name} — status: ${r.status} — ${todos.length} open question(s), ${errs.length} blocking error(s)\n`);
  let n = 1;
  if (errs.length) {
    console.log("must fix first (spec is inconsistent):");
    for (const f of errs) console.log(`  ${n++}. [${f.code}] ${f.message}`);
    console.log("");
  }
  if (todos.length) {
    console.log("open questions:");
    for (const f of todos) console.log(`  ${n++}. ${f.question}`);
    console.log("");
  }
  if (!errs.length && !todos.length) console.log(`Nothing open. ${r.strict ? "Spec passes." : "Ready to set status: ready."}`);
  if (warns.length) console.log(`(${warns.length} warning(s) — non-blocking)`);
  process.exit(r.pass ? 0 : 1);
}

async function cmdApply(patchPath) {
  if (!patchPath) throw new Error("apply needs a patch file: interview.mjs <dir> apply <patch.yaml>");
  const before = requireAnalysis();
  const m = loadManifest();
  const patch = YAML.load(fs.readFileSync(path.resolve(patchPath), "utf8")) || {};

  const summary = applyPatch(m, patch);
  saveManifest(m);
  if (flags.has("--scaffold")) summary.scaffolded = scaffold(m);

  const after = analyze(specDir);
  console.log(`applied: ${summary.notes.join("; ") || "no manifest changes"}`);
  if (summary.scaffolded?.length) console.log(`scaffolded: ${summary.scaffolded.join(", ")}`);
  console.log(`todos: ${before.counts.todo} -> ${after.counts.todo}   errors: ${before.counts.error} -> ${after.counts.error}`);
  if (after.counts.todo === 0 && after.counts.error === 0 && !after.strict)
    console.log(`\nNo open todos. Set metadata.status: ready, then run: node tools/verify.mjs ${pos[0] || "."}`);
  process.exit(0);
}

async function cmdAsk() {
  const rl = (await import("node:readline/promises")).createInterface({ input: process.stdin, output: process.stdout });
  const ask = (q) => rl.question(q);
  try {
    for (;;) {
      const r = analyze(specDir);
      if (r.fatal) { console.log(r.fatal); break; }
      const q = ordered(r.findings);
      const err = q.find((f) => f.severity === "error");
      if (err) { console.log(`\nBlocking error — fix in the manifest, then rerun: [${err.code}] ${err.message}`); break; }
      const t = q.find((f) => f.severity === "todo");
      if (!t) { console.log(`\nAll questions answered (status: ${r.status}).`); if (!r.strict) console.log("Set metadata.status: ready to finish."); break; }

      const m = loadManifest();
      console.log(`\n${t.question}`);
      if (t.code === "NO_REQUIREMENTS" || t.code === "REQ_NO_AC") {
        const reqId = t.code === "NO_REQUIREMENTS" ? await newRequirement(m, ask) : t.ids[0];
        const ears = (await ask("  EARS criterion: ")).trim();
        if (ears) {
          const acId = nextId("AC-", allAcIds(m));
          const req = m.requirements.find((r2) => r2.id === reqId);
          const def = `verification/acceptance/${reqId}.feature#${acId.toLowerCase()}`;
          const test = (await ask(`  test ref [${def}]: `)).trim() || def;
          (req.acceptance ||= []).push({ id: acId, ears, test });
        }
      } else if (t.code === "AC_UNSATISFIED") {
        const title = (await ask("  task title: ")).trim();
        if (title) {
          const touches = (await ask("  touches (CMP-/CTR- ids, comma) []: ")).split(",").map((s) => s.trim()).filter(Boolean);
          (m.tasks ||= []).push({ id: nextId("TSK-", ids(m.tasks)), title, satisfies: [t.ids[0]], ...(touches.length ? { touches } : {}) });
        }
      } else if (t.code === "AC_NO_TEST") {
        const acId = t.ids[0];
        const ac = allAc(m).find((a) => a.id === acId);
        const def = `verification/acceptance/${ac.req}.feature#${acId.toLowerCase()}`;
        ac.entry.test = (await ask(`  test ref [${def}]: `)).trim() || def;
      } else if (t.code === "LAYER_UNPLACED") {
        const layer = (await ask("  layer (presentation/domain/data): ")).trim();
        if (layer) mergeLayers({ [layer]: [t.ids[0]] });
      } else if (t.code.endsWith("_MISSING")) {
        const yes = (await ask("  scaffold this file now? [Y/n]: ")).trim().toLowerCase();
        if (yes !== "n") { saveManifest(m); scaffold(loadManifest()); continue; }
      } else {
        console.log("  (no interactive handler; edit the manifest directly)"); break;
      }
      saveManifest(m);
    }
  } finally { rl.close(); }
}

// ---- patch application ------------------------------------------------------
function applyPatch(m, patch) {
  const notes = [];
  m.requirements ||= [];
  for (const pr of patch.requirements || []) {
    let req = pr.id && m.requirements.find((r) => r.id === pr.id);
    if (!req) { req = { id: pr.id || nextId("REQ-", ids(m.requirements)), title: pr.title || "(untitled)", ...(pr.rationale ? { rationale: pr.rationale } : {}), acceptance: [] }; m.requirements.push(req); notes.push(`+${req.id}`); }
    for (const pa of pr.acceptance || []) {
      const acId = pa.id || nextId("AC-", allAcIds(m));
      (req.acceptance ||= []).push({ id: acId, ears: pa.ears, ...(pa.test ? { test: pa.test } : {}) });
      notes.push(`+${acId}`);
    }
  }
  for (const [acId, ref] of Object.entries(patch.tests || {})) {
    const hit = allAc(m).find((a) => a.id === acId);
    if (hit) { hit.entry.test = ref; notes.push(`test:${acId}`); } else notes.push(`test:${acId}?missing`);
  }
  for (const pc of patch.components || []) upsert(m, "components", pc, "CMP-", notes);
  for (const pc of patch.contracts || []) upsert(m, "contracts", pc, "CTR-", notes);
  for (const pd of patch.decisions || []) upsert(m, "decisions", pd, "ADR-", notes);
  for (const pt of patch.tasks || []) upsert(m, "tasks", pt, "TSK-", notes);
  if (patch.layers) { mergeLayers(patch.layers); notes.push("layers"); }
  return { notes };
}

function upsert(m, key, entry, prefix, notes) {
  m[key] ||= [];
  const id = entry.id || nextId(prefix, ids(m[key]));
  const cur = m[key].find((x) => x.id === id);
  if (cur) { Object.assign(cur, entry, { id }); notes.push(`~${id}`); }
  else { m[key].push({ id, ...entry }); notes.push(`+${id}`); }
}

function mergeLayers(layers) {
  const rules = exists(RULES_REL) ? (YAML.load(fs.readFileSync(p(RULES_REL), "utf8")) || {}) : {};
  rules.layers ||= {};
  for (const [layer, comps] of Object.entries(layers)) {
    rules.layers[layer] = Array.from(new Set([...(rules.layers[layer] || []), ...comps]));
  }
  if (!rules.allow) rules.allow = [{ from: "presentation", to: ["domain"] }, { from: "domain", to: ["data"] }];
  fs.mkdirSync(path.dirname(p(RULES_REL)), { recursive: true });
  fs.writeFileSync(p(RULES_REL), YAML.dump(rules, { lineWidth: 120 }));
}

// ---- scaffolding ------------------------------------------------------------
function scaffold(m) {
  const made = [];
  const write = (rel, body) => { if (exists(rel)) return; fs.mkdirSync(path.dirname(p(rel)), { recursive: true }); fs.writeFileSync(p(rel), body); made.push(rel); };

  for (const [key, rel] of Object.entries(m.artifacts || {}))
    if (rel && !exists(rel)) write(rel, `# ${key[0].toUpperCase() + key.slice(1)}\n\n<!-- generated stub — expand the ${key} IDs from avspec.yaml here -->\n`);

  for (const c of m.contracts || [])
    if (!exists(c.path)) write(c.path, contractStub(c));

  for (const d of m.decisions || [])
    if (!exists(d.path)) write(d.path, `# ${d.id} — ${d.title}\n\n- **Status:** ${d.status}\n\n## Context\n\n<!-- stub -->\n\n## Decision\n\n## Consequences\n`);

  // group acceptance tests by feature file
  const byFile = new Map();
  for (const a of allAc(m)) {
    if (!a.entry.test) continue;
    const [file, scen] = a.entry.test.split("#");
    if (!file.endsWith(".feature") || !file.includes("/")) continue;
    (byFile.get(file) || byFile.set(file, []).get(file)).push({ ac: a.id, req: a.req, ears: a.entry.ears, scen: scen || a.id });
  }
  for (const [file, scens] of byFile) {
    if (exists(file)) continue;
    const req = scens[0].req;
    let body = `Feature: ${req}\n`;
    for (const s of scens) body += `\n  # ${s.ac}: ${s.ears || ""}\n  Scenario: ${s.scen}\n    Given <precondition>\n    When <action>\n    Then <observable outcome>\n`;
    write(file, body);
  }
  return made;
}

function contractStub(c) {
  if (c.type === "openapi") return `openapi: 3.1.0\ninfo: { title: ${path.basename(c.path)}, version: "0.0.0" }\npaths: {}\n`;
  if (c.type === "asyncapi") return `asyncapi: 3.0.0\ninfo: { title: ${path.basename(c.path)}, version: "0.0.0" }\nchannels: {}\n`;
  return `# JSON Schema stub (${c.id})\n$schema: "https://json-schema.org/draft/2020-12/schema"\ntype: object\n`;
}

// ---- manifest io + id helpers ----------------------------------------------
function loadManifest() { return YAML.load(fs.readFileSync(p("avspec.yaml"), "utf8")); }
function saveManifest(m) { fs.writeFileSync(p("avspec.yaml"), YAML.dump(m, { lineWidth: 120, noRefs: true })); }
function ids(arr) { return (arr || []).map((x) => x.id); }
function allAcIds(m) { return (m.requirements || []).flatMap((r) => (r.acceptance || []).map((a) => a.id)); }
function allAc(m) { return (m.requirements || []).flatMap((r) => (r.acceptance || []).map((a) => ({ id: a.id, req: r.id, entry: a }))); }

function nextId(prefix, existing) {
  const nums = existing.filter((s) => s.startsWith(prefix)).map((s) => parseInt(s.slice(prefix.length), 10)).filter((n) => !Number.isNaN(n));
  const width = Math.max(3, ...existing.filter((s) => s.startsWith(prefix)).map((s) => s.slice(prefix.length).length));
  const n = (nums.length ? Math.max(...nums) : 0) + 1;
  return prefix + String(n).padStart(width, "0");
}

async function newRequirement(m, ask) {
  const title = (await ask("  requirement title: ")).trim() || "(untitled)";
  const id = nextId("REQ-", ids(m.requirements));
  (m.requirements ||= []).push({ id, title, acceptance: [] });
  return id;
}

function requireAnalysis() {
  const r = analyze(specDir);
  if (r.fatal) { console.error(`FATAL: ${r.fatal}`); process.exit(1); }
  return r;
}
