#!/usr/bin/env node
// AVSpec verifier — the CI gate. Thin CLI over tools/lib/analyze.mjs.
//
// Severity model (see analyze.mjs):
//   error  well-formedness/consistency — always fails, even in draft.
//   todo   completeness gap — fails only at status ready/built (the ready-gate);
//          tolerated while draft. Each todo carries the question to close it.
//   warn   advisory — never fails.
//
// Usage: node tools/verify.mjs [specDir] [--json]   (exit 0 = pass, 1 = fail)

import process from "node:process";
import { analyze } from "./lib/analyze.mjs";

const argv = process.argv.slice(2);
const asJson = argv.includes("--json");
const specDir = argv.find((a) => !a.startsWith("--")) || ".";

const r = analyze(specDir);
if (r.fatal) { console.error(`FATAL: ${r.fatal}`); process.exit(1); }

if (asJson) {
  console.log(JSON.stringify({ status: r.status, strict: r.strict, pass: r.pass, counts: r.counts, findings: r.findings }, null, 2));
  process.exit(r.pass ? 0 : 1);
}

const errs = r.findings.filter((f) => f.severity === "error");
const todos = r.findings.filter((f) => f.severity === "todo");
const warns = r.findings.filter((f) => f.severity === "warn");
for (const f of errs) console.error(`  ERROR  [${f.code}] ${f.message}`);
for (const f of todos) console.log(`  ${r.strict ? "TODO*" : "todo "}  [${f.code}] ${f.message}`);
for (const f of warns) console.log(`  warn   [${f.code}] ${f.message}`);
console.log("");
if (r.pass) {
  console.log(`PASS (status: ${r.status}) — ${errs.length} errors, ${todos.length} open todos, ${warns.length} warnings.`);
  if (!r.strict && todos.length) console.log(`  ${todos.length} todo(s) remain; they must be closed before status: ready.`);
  process.exit(0);
}
console.error(`FAIL (status: ${r.status}) — ${errs.length} error(s)` + (r.strict ? ` + ${todos.length} unmet todo(s) blocking ready.` : "."));
process.exit(1);
