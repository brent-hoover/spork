"""AVSpec analyzer."""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
from typing import Any

from avspec.findings import Finding
from avspec.io import load_manifest, yaml
from avspec.model import AcceptanceCriterion, Component, Manifest, Requirement, Task


@dataclass(frozen=True)
class Report:
    fatal: str | None
    schema_valid: bool
    status: str | None
    strict: bool
    pass_: bool
    counts: dict[str, int]
    findings: tuple[Finding, ...]
    manifest: Manifest | None
    raw: dict[str, Any] | None
    spec_dir: Path


def analyze(spec_dir: Path) -> Report:
    loaded = load_manifest(spec_dir)
    findings: list[Finding] = []

    if loaded.raw is None and loaded.manifest is None:
        return _fatal(
            loaded.spec_dir, loaded.errors[0] if loaded.errors else "unable to load manifest"
        )

    if loaded.manifest is None:
        for error in loaded.errors:
            findings.append(
                Finding(
                    severity="error",
                    code="SCHEMA",
                    message=error,
                    question=f"The manifest shape is invalid: {error}. Fix it before continuing.",
                )
            )
        return _finalize(
            spec_dir=loaded.spec_dir,
            manifest=None,
            raw=loaded.raw,
            status="draft",
            schema_valid=False,
            findings=findings,
        )

    manifest = loaded.manifest
    _wellformed(manifest, findings)
    _completeness(manifest, loaded.spec_dir, findings)
    _content(manifest, loaded.spec_dir, findings)
    _architecture(manifest, loaded.spec_dir, findings)
    return _finalize(
        spec_dir=loaded.spec_dir,
        manifest=manifest,
        raw=loaded.raw,
        status=manifest.metadata.status,
        schema_valid=True,
        findings=findings,
    )


def _wellformed(manifest: Manifest, findings: list[Finding]) -> None:
    for name, items in (
        ("requirement", manifest.requirements),
        ("contract", manifest.contracts),
        ("component", manifest.components),
        ("decision", manifest.decisions),
        ("task", manifest.tasks),
        ("acceptance", _acceptance_entries(manifest.requirements)),
    ):
        _duplicate_ids(name, [item.id for item in items], findings)

    ac_ids = {entry.id for entry in _acceptance_entries(manifest.requirements)}
    contract_ids = {contract.id for contract in manifest.contracts}
    component_ids = {component.id for component in manifest.components}
    task_ids = {task.id for task in manifest.tasks}

    for task in manifest.tasks:
        for ac_id in task.satisfies:
            if ac_id not in ac_ids:
                findings.append(
                    Finding(
                        "error",
                        "DANGLING",
                        f"{task.id} satisfies unknown {ac_id}",
                        (task.id, ac_id),
                    )
                )
        for ref in task.touches:
            if ref.startswith("CMP-") and ref not in component_ids:
                findings.append(
                    Finding("error", "DANGLING", f"{task.id} touches unknown {ref}", (task.id, ref))
                )
            if ref.startswith("CTR-") and ref not in contract_ids:
                findings.append(
                    Finding("error", "DANGLING", f"{task.id} touches unknown {ref}", (task.id, ref))
                )
        for dep in task.depends_on:
            if dep not in task_ids:
                findings.append(
                    Finding(
                        "error", "DANGLING", f"{task.id} depends_on unknown {dep}", (task.id, dep)
                    )
                )

    for component in manifest.components:
        for dep in component.depends_on:
            if dep not in component_ids:
                findings.append(
                    Finding(
                        "error",
                        "DANGLING",
                        f"{component.id} depends_on unknown {dep}",
                        (component.id, dep),
                    )
                )
        for interface in component.interfaces:
            if interface.contract not in contract_ids:
                findings.append(
                    Finding(
                        "error",
                        "DANGLING",
                        f"{component.id}.{interface.name} references unknown {interface.contract}",
                        (component.id, interface.contract),
                    )
                )

    _detect_cycles("task", manifest.tasks, findings)
    _detect_cycles("component", manifest.components, findings)


def _completeness(manifest: Manifest, spec_dir: Path, findings: list[Finding]) -> None:
    if not manifest.components:
        findings.append(
            Finding(
                "todo",
                "NO_COMPONENTS",
                "no modules/components defined",
                question=(
                    "What modules, apps, or services make up the suite? "
                    "Define modules before requirements or acceptance tests."
                ),
            )
        )
    else:
        rules_path = spec_dir / "verification/architecture-rules.yaml"
        if not rules_path.exists():
            findings.append(
                Finding(
                    "todo",
                    "NO_BOUNDARIES",
                    "module boundary rules are not defined",
                    question=(
                        "What architecture layers contain these modules, "
                        "and what dependency directions are allowed?"
                    ),
                )
            )
        if not manifest.contracts:
            findings.append(
                Finding(
                    "todo",
                    "NO_CONTRACTS",
                    "no API contracts defined",
                    question="Which module boundaries expose APIs or message contracts?",
                )
            )
    if not manifest.principles and not manifest.constraints:
        findings.append(
            Finding(
                "todo",
                "NO_CONSTITUTION",
                "no suite constitution defined",
                question=(
                    "What top-level constitution should govern the whole suite "
                    "and every module boundary?"
                ),
            )
        )

    if manifest.requirements and manifest.components and manifest.stack is None:
        for component in manifest.components:
            if component.stack is not None:
                continue
            findings.append(
                Finding(
                    "todo",
                    "CMP_NO_STACK",
                    f"{component.id} has no implementation stack declared",
                    (component.id,),
                    (
                        f"Implementation planning needs a stack for {component.id}. "
                        "What languages, frameworks, and commands should it use?"
                    ),
                )
            )
    elif manifest.requirements and manifest.stack is None:
        findings.append(
            Finding(
                "todo",
                "NO_STACK",
                "no implementation stack declared",
                question=(
                    "Implementation planning needs stack details. "
                    "What languages, frameworks, and commands should be used?"
                ),
            )
        )
    elif manifest.stack is not None and not manifest.stack.languages:
        findings.append(
            Finding(
                "todo",
                "STACK_INCOMPLETE",
                "stack.languages is empty",
                question="Which implementation language or languages should this suite use?",
            )
        )

    if not manifest.requirements:
        findings.append(
            Finding(
                "todo",
                "NO_REQUIREMENTS",
                "no capabilities defined",
                question=(
                    "What high-level capabilities should the suite provide "
                    "once the module map is known?"
                ),
            )
        )

    for requirement in manifest.requirements:
        if not requirement.acceptance:
            findings.append(
                Finding(
                    "todo",
                    "REQ_NO_AC",
                    f"{requirement.id} has no acceptance criteria",
                    (requirement.id,),
                    (
                        f"Define observable acceptance criteria for {requirement.id} - "
                        f'"{requirement.title}".'
                    ),
                )
            )

    satisfied = {ac_id for task in manifest.tasks for ac_id in task.satisfies}
    for entry in _acceptance_entries(manifest.requirements):
        if entry.id not in satisfied:
            findings.append(
                Finding(
                    "todo",
                    "AC_UNSATISFIED",
                    f"{entry.id} is satisfied by no task",
                    (entry.id,),
                    f"Which build task delivers {entry.id}?",
                )
            )
        if not entry.test:
            findings.append(
                Finding(
                    "todo",
                    "AC_NO_TEST",
                    f"{entry.id} has no test mapping",
                    (entry.id,),
                    f"How is {entry.id} proven automatically?",
                )
            )
        elif _looks_like_path(entry.test) and not (spec_dir / _test_path(entry.test)).exists():
            findings.append(
                Finding(
                    "todo",
                    "TEST_MISSING",
                    f"{entry.id} test file not found: {_test_path(entry.test)}",
                    (entry.id,),
                    f"Create the test file {_test_path(entry.test)}.",
                )
            )

    for key, rel in manifest.artifacts.model_dump().items():
        if rel and not (spec_dir / rel).exists():
            findings.append(
                Finding("todo", "ARTIFACT_MISSING", f"artifact file not yet created: {rel} ({key})")
            )
    for contract in manifest.contracts:
        if not (spec_dir / contract.path).exists():
            findings.append(
                Finding(
                    "todo",
                    "CONTRACT_MISSING",
                    f"contract file missing: {contract.path} ({contract.id})",
                    (contract.id,),
                    f"Author the contract {contract.id} at {contract.path}.",
                )
            )
    for decision in manifest.decisions:
        if not (spec_dir / decision.path).exists():
            findings.append(
                Finding(
                    "todo",
                    "ADR_MISSING",
                    f"ADR file missing: {decision.path} ({decision.id})",
                    (decision.id,),
                    f"Write the decision record {decision.id} at {decision.path}.",
                )
            )


def _content(manifest: Manifest, spec_dir: Path, findings: list[Finding]) -> None:
    _mirror(
        spec_dir,
        manifest.artifacts.requirements,
        [req.id for req in manifest.requirements],
        findings,
    )
    _mirror(
        spec_dir,
        manifest.artifacts.design,
        [component.id for component in manifest.components],
        findings,
    )
    _mirror(spec_dir, manifest.artifacts.tasks, [task.id for task in manifest.tasks], findings)


def _architecture(manifest: Manifest, spec_dir: Path, findings: list[Finding]) -> None:
    rules_path = spec_dir / "verification/architecture-rules.yaml"
    if not rules_path.exists():
        return

    rules = yaml.load(rules_path.read_text()) or {}
    layer_of: dict[str, str] = {}
    for layer, components in (rules.get("layers") or {}).items():
        for component_id in components or []:
            layer_of[component_id] = layer

    allowed: dict[str, set[str]] = {}
    for edge in rules.get("allow") or []:
        allowed[edge["from"]] = set(edge.get("to") or [])

    for component in manifest.components:
        from_layer = layer_of.get(component.id)
        if from_layer is None:
            findings.append(
                Finding(
                    "todo",
                    "LAYER_UNPLACED",
                    f"{component.id} is not placed in any layer",
                    (component.id,),
                    f"Which layer does {component.id} belong to?",
                )
            )
            continue
        for dep in component.depends_on:
            to_layer = layer_of.get(dep)
            if to_layer is None or to_layer == from_layer:
                continue
            if to_layer not in allowed.get(from_layer, set()):
                findings.append(
                    Finding(
                        "error",
                        "ARCH_VIOLATION",
                        (
                            f"{component.id}({from_layer}) -> {dep}({to_layer}) "
                            "is not an allowed dependency"
                        ),
                        (component.id, dep),
                    )
                )


def _duplicate_ids(name: str, ids: list[str], findings: list[Finding]) -> None:
    seen: set[str] = set()
    for item_id in ids:
        if item_id in seen:
            findings.append(
                Finding("error", "DUP_ID", f"duplicate {name} id: {item_id}", (item_id,))
            )
        seen.add(item_id)


def _detect_cycles(kind: str, items: list[Task] | list[Component], findings: list[Finding]) -> None:
    graph = {item.id: item.depends_on for item in items}
    state: dict[str, int] = {}

    def visit(node: str, stack: tuple[str, ...]) -> None:
        if state.get(node) == 2:
            return
        if state.get(node) == 1:
            cycle = (*stack, node)
            findings.append(
                Finding("error", "CYCLE", f"{kind} dependency cycle: {' -> '.join(cycle)}", cycle)
            )
            return
        state[node] = 1
        for dep in graph.get(node, []):
            if dep in graph:
                visit(dep, (*stack, node))
        state[node] = 2

    for item_id in graph:
        visit(item_id, ())


def _acceptance_entries(requirements: list[Requirement]) -> list[AcceptanceCriterion]:
    return [entry for requirement in requirements for entry in requirement.acceptance]


def _mirror(spec_dir: Path, rel: str, ids: list[str], findings: list[Finding]) -> None:
    path = spec_dir / rel
    if not path.exists():
        return
    text = path.read_text()
    for item_id in ids:
        if item_id not in text:
            findings.append(
                Finding("warn", "MIRROR", f"{item_id} not mentioned in {rel}", (item_id,))
            )


def _looks_like_path(test_ref: str) -> bool:
    return "/" in test_ref


def _test_path(test_ref: str) -> str:
    return test_ref.split("#", maxsplit=1)[0].split("::", maxsplit=1)[0]


def _finalize(
    *,
    spec_dir: Path,
    manifest: Manifest | None,
    raw: dict[str, Any] | None,
    status: str,
    schema_valid: bool,
    findings: list[Finding],
) -> Report:
    counts = {
        "error": sum(1 for finding in findings if finding.severity == "error"),
        "todo": sum(1 for finding in findings if finding.severity == "todo"),
        "warn": sum(1 for finding in findings if finding.severity == "warn"),
    }
    strict = status in {"ready", "built"}
    pass_ = counts["error"] + (counts["todo"] if strict else 0) == 0
    return Report(
        fatal=None,
        schema_valid=schema_valid,
        status=status,
        strict=strict,
        pass_=pass_,
        counts=counts,
        findings=tuple(findings),
        manifest=manifest,
        raw=raw,
        spec_dir=spec_dir,
    )


def _fatal(spec_dir: Path, message: str) -> Report:
    return Report(
        fatal=message,
        schema_valid=False,
        status=None,
        strict=False,
        pass_=False,
        counts={"error": 0, "todo": 0, "warn": 0},
        findings=(),
        manifest=None,
        raw=None,
        spec_dir=spec_dir,
    )
