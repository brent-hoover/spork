// Shared AVSpec analyzer. One brain for both verify.mjs (the gate) and
// interview.mjs (the authoring CLI). Pure: no console, no process.exit.
//
// analyze(specDir) -> {
//   fatal: string|null,          // missing manifest/schema; nothing else ran
//   schemaValid: bool,
//   status, strict, pass,
//   counts: {error, todo, warn},
//   findings: [{severity, code, message, ids, question?, fix?}],
//   manifest,                    // parsed avspec.yaml (or null on fatal)
// }

import fs from "node:fs";
import path from "node:path";
import * as YAML from "js-yaml";
import Ajv2020 from "ajv/dist/2020.js";

export function analyze(specDir) {
  const dir = path.resolve(specDir);
  const findings = [];
  const add = (severity, code, message, extra = {}) =>
    findings.push({ severity, code, message, ids: extra.ids || [], ...(extra.question ? { question: extra.question } : {}), ...(extra.fix ? { fix: extra.fix } : {}) });
  const error = (c, m, e) => add("error", c, m, e);
  const todo = (c, m, e) => add("todo", c, m, e);
  const warn = (c, m, e) => add("warn", c, m, e);
  const readYaml = (p) => YAML.load(fs.readFileSync(p, "utf8"));
  const exists = (rel) => fs.existsSync(path.join(dir, rel));

  const schemaPath = path.join(dir, "avspec.schema.yaml");
  const manifestPath = path.join(dir, "avspec.yaml");
  for (const [label, p] of [["schema", schemaPath], ["manifest", manifestPath]])
    if (!fs.existsSync(p)) return fatal(`missing ${label}: ${p}`);

  const schema = readYaml(schemaPath);
  const m = readYaml(manifestPath);
  const status = m?.metadata?.status ?? "draft";
  const strict = status === "ready" || status === "built";

  const ajv = new Ajv2020({ allErrors: true, strict: false });
  const validate = ajv.compile(schema);
  if (!validate(m)) {
    for (const e of validate.errors)
      error("SCHEMA", `${e.instancePath || "/"} ${e.message}`, { question: `The manifest shape is invalid at ${e.instancePath || "/"}: ${e.message}. Fix it before continuing.` });
    return finalize(false); // invalid shape — skip graph checks
  }

  const list = (k) => (Array.isArray(m[k]) ? m[k] : []);
  const idset = (arr) => new Set(arr.map((x) => x.id));
  const requirements = list("requirements");
  const contracts = list("contracts");
  const components = list("components");
  const decisions = list("decisions");
  const tasks = list("tasks");
  const acEntries = requirements.flatMap((r) => (r.acceptance || []).map((a) => ({ ...a, req: r.id, reqTitle: r.title })));
  const AC = idset(acEntries), CTR = idset(contracts), CMP = idset(components), TSK = idset(tasks);

  for (const [name, arr] of [["requirement", requirements], ["contract", contracts], ["component", components], ["decision", decisions], ["task", tasks], ["acceptance", acEntries]]) {
    const seen = new Set();
    for (const x of arr) { if (seen.has(x.id)) error("DUP_ID", `duplicate ${name} id: ${x.id}`, { ids: [x.id] }); seen.add(x.id); }
  }

  if (requirements.length === 0)
    todo("NO_REQUIREMENTS", "no requirements defined", { question: "What must the system do? State the first requirement (a WHAT + WHY, no implementation)." });
  for (const r of requirements)
    if (!(r.acceptance || []).length)
      todo("REQ_NO_AC", `${r.id} has no acceptance criteria`, { ids: [r.id], question: `Define at least one acceptance criterion (EARS) for ${r.id} — "${r.title}". What observable behavior proves it's done?` });

  const satisfied = new Set(tasks.flatMap((t) => t.satisfies || []));
  for (const a of acEntries) {
    if (!satisfied.has(a.id)) todo("AC_UNSATISFIED", `${a.id} (in ${a.req}) is satisfied by no task`, { ids: [a.id], question: `Which build task delivers ${a.id} ("${a.ears}")? Add a TSK-* that satisfies it.` });
    if (!a.test) todo("AC_NO_TEST", `${a.id} has no test mapping`, { ids: [a.id], question: `How is ${a.id} proven automatically? Give a .feature scenario ref (verification/acceptance/${a.req}.feature#<scenario>) or a test path.` });
  }

  for (const t of tasks) {
    for (const a of t.satisfies || []) if (!AC.has(a)) error("DANGLING", `${t.id} satisfies unknown ${a}`, { ids: [t.id, a] });
    for (const ref of t.touches || []) {
      if (ref.startsWith("CMP-") && !CMP.has(ref)) error("DANGLING", `${t.id} touches unknown ${ref}`, { ids: [t.id, ref] });
      if (ref.startsWith("CTR-") && !CTR.has(ref)) error("DANGLING", `${t.id} touches unknown ${ref}`, { ids: [t.id, ref] });
    }
    for (const d of t.depends_on || []) if (!TSK.has(d)) error("DANGLING", `${t.id} depends_on unknown ${d}`, { ids: [t.id, d] });
  }
  for (const c of components) {
    for (const d of c.depends_on || []) if (!CMP.has(d)) error("DANGLING", `${c.id} depends_on unknown ${d}`, { ids: [c.id, d] });
    for (const i of c.interfaces || []) if (!CTR.has(i.contract)) error("DANGLING", `${c.id}.${i.name} references unknown ${i.contract}`, { ids: [c.id, i.contract] });
  }

  detectCycles("task", tasks);
  detectCycles("component", components);

  for (const key of ["constitution", "requirements", "design", "tasks"])
    if (m.artifacts?.[key] && !exists(m.artifacts[key]))
      todo("ARTIFACT_MISSING", `artifact file not yet created: ${m.artifacts[key]} (${key})`, { question: `Create the ${key} artifact at ${m.artifacts[key]}.` });
  for (const c of contracts)
    if (!exists(c.path)) todo("CONTRACT_MISSING", `contract file missing: ${c.path} (${c.id})`, { ids: [c.id], question: `Author the contract ${c.id} (${c.type}) at ${c.path}.` });
  for (const d of decisions)
    if (!exists(d.path)) todo("ADR_MISSING", `ADR file missing: ${d.path} (${d.id})`, { ids: [d.id], question: `Write the decision record ${d.id} at ${d.path}.` });
  for (const a of acEntries) {
    if (!a.test) continue;
    const filePart = a.test.split("#")[0].split("::")[0];
    if (filePart.includes("/") && !exists(filePart)) todo("TEST_MISSING", `${a.id} test file not found: ${filePart}`, { ids: [a.id], question: `Create the test file ${filePart} with the scenario for ${a.id}.` });
  }

  mirror(m.artifacts?.requirements, [...idset(requirements), ...AC]);
  mirror(m.artifacts?.design, [...CMP]);
  mirror(m.artifacts?.tasks, [...TSK]);

  const rulesRel = "verification/architecture-rules.yaml";
  if (exists(rulesRel)) archRules(readYaml(path.join(dir, rulesRel)));
  else warn("NO_ARCH_RULES", `no ${rulesRel} — spec-level dependency check skipped`);

  return finalize(true);

  // --- inner helpers -----------------------------------------------------
  function detectCycles(kind, arr) {
    const graph = new Map(arr.map((x) => [x.id, x.depends_on || []]));
    const state = new Map();
    const dfs = (n, stack) => {
      if (state.get(n) === 2) return;
      if (state.get(n) === 1) { error("CYCLE", `${kind} dependency cycle: ${[...stack, n].join(" -> ")}`, { ids: [...stack, n] }); return; }
      state.set(n, 1);
      for (const nb of graph.get(n) || []) if (graph.has(nb)) dfs(nb, [...stack, n]);
      state.set(n, 2);
    };
    for (const x of arr) dfs(x.id, []);
  }
  function mirror(rel, ids) {
    if (!rel || !exists(rel)) return;
    const text = fs.readFileSync(path.join(dir, rel), "utf8");
    for (const id of ids) if (!text.includes(id)) warn("MIRROR", `${id} not mentioned in ${rel}`, { ids: [id] });
  }
  function archRules(rules) {
    const layerOf = new Map();
    for (const [layer, comps] of Object.entries(rules.layers || {})) for (const c of comps || []) layerOf.set(c, layer);
    const allow = new Map();
    for (const a of rules.allow || []) allow.set(a.from, new Set(a.to || []));
    for (const c of components) {
      const from = layerOf.get(c.id);
      if (from === undefined) { todo("LAYER_UNPLACED", `${c.id} is not placed in any layer`, { ids: [c.id], question: `Which layer does ${c.id} belong to (presentation / domain / data)? Place it in architecture-rules.yaml.` }); continue; }
      for (const dep of c.depends_on || []) {
        const to = layerOf.get(dep);
        if (to === undefined || from === to) continue;
        if (!allow.get(from)?.has(to)) error("ARCH_VIOLATION", `${c.id}(${from}) -> ${dep}(${to}) is not an allowed dependency`, { ids: [c.id, dep] });
      }
    }
  }
  function finalize(schemaValid) {
    const counts = {
      error: findings.filter((f) => f.severity === "error").length,
      todo: findings.filter((f) => f.severity === "todo").length,
      warn: findings.filter((f) => f.severity === "warn").length,
    };
    const pass = counts.error + (strict ? counts.todo : 0) === 0;
    return { fatal: null, schemaValid, status, strict, pass, counts, findings, manifest: m };
  }
  function fatal(message) {
    return { fatal: message, schemaValid: false, status: null, strict: false, pass: false, counts: { error: 0, todo: 0, warn: 0 }, findings: [], manifest: null };
  }
}

// Ordering used by both the gate output and the interview queue:
// errors before todos before warns; within a severity, by authoring layer.
const CODE_ORDER = ["SCHEMA", "DUP_ID", "DANGLING", "CYCLE", "ARCH_VIOLATION",
  "NO_REQUIREMENTS", "REQ_NO_AC", "AC_UNSATISFIED", "AC_NO_TEST", "LAYER_UNPLACED",
  "ARTIFACT_MISSING", "CONTRACT_MISSING", "TEST_MISSING", "ADR_MISSING",
  "MIRROR", "NO_ARCH_RULES"];
const SEV_RANK = { error: 0, todo: 1, warn: 2 };

export function ordered(findings) {
  return [...findings].sort((a, b) =>
    (SEV_RANK[a.severity] - SEV_RANK[b.severity]) ||
    (idx(a.code) - idx(b.code)));
  function idx(c) { const i = CODE_ORDER.indexOf(c); return i < 0 ? 99 : i; }
}
