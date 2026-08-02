"""Module-first AVSpec interview."""

from __future__ import annotations

from pathlib import Path
from typing import Any

import typer
from rich.console import Console

from avspec.analysis import analyze
from avspec.findings import Finding, ordered
from avspec.io import load_manifest, save_raw_manifest, yaml

console = Console()


def ask(spec_dir: Path) -> int:
    """Run a module-first interview until the next manual stop point."""
    resolved = spec_dir.resolve()
    while True:
        report = analyze(resolved)
        if report.fatal:
            console.print(f"FATAL: {report.fatal}", style="red")
            return 1

        findings = ordered(report.findings)
        error = next((finding for finding in findings if finding.severity == "error"), None)
        if error:
            console.print(
                f"\nBlocking error - fix the manifest, then rerun: [{error.code}] {error.message}"
            )
            return 1

        todo = next((finding for finding in findings if finding.severity == "todo"), None)
        if todo is None:
            console.print(f"\nAll questions answered (status: {report.status}).")
            if not report.strict:
                console.print("Set metadata.status: ready after reviewing the generated artifacts.")
            return 0

        loaded = load_manifest(resolved)
        if loaded.raw is None:
            console.print("No manifest data loaded.", style="red")
            return 1

        changed = _handle_todo(resolved, loaded.raw, todo)
        if not changed:
            console.print("No change made; stopping so the interview does not loop.")
            return 1
        save_raw_manifest(resolved, loaded.raw)
        sync_artifacts(resolved, loaded.raw)


def _handle_todo(spec_dir: Path, manifest: dict[str, Any], finding: Finding) -> bool:
    console.print(f"\n[{finding.code}] {finding.question or finding.message}", style="bold")
    match finding.code:
        case "NO_COMPONENTS":
            return _ask_modules(manifest)
        case "NO_BOUNDARIES":
            return _ask_boundaries(spec_dir, manifest)
        case "NO_CONTRACTS" | "CMP_NO_CONTRACT":
            return _ask_contracts(manifest, finding.ids[0] if finding.ids else None)
        case "NO_CONSTITUTION":
            return _ask_constitution(manifest)
        case "NO_REQUIREMENTS":
            return _ask_capabilities(manifest)
        case "REQ_NO_AC":
            return _ask_acceptance(manifest, finding.ids[0])
        case "AC_UNSATISFIED":
            return _ask_task(manifest, finding.ids[0])
        case "AC_NO_TEST":
            return _ask_test(manifest, finding.ids[0])
        case "CMP_NO_STACK":
            return _ask_module_stack(manifest, finding.ids[0])
        case "NO_STACK" | "STACK_INCOMPLETE":
            return _ask_stack(manifest)
        case "LAYER_UNPLACED":
            return _ask_layer(spec_dir, finding.ids[0])
        case code if code.endswith("_MISSING"):
            return _scaffold_missing(spec_dir, manifest, finding)
        case _:
            console.print(
                "No interactive handler for this finding yet; edit the manifest directly."
            )
            return False


def _ask_stack(manifest: dict[str, Any]) -> bool:
    stack = _prompt_stack("suite default")
    if stack is None:
        return False
    manifest["stack"] = stack
    manifest["avspec"] = "0.2"
    return True


def _ask_module_stack(manifest: dict[str, Any], component_id: str) -> bool:
    component = next(
        (item for item in manifest.get("components") or [] if item["id"] == component_id),
        None,
    )
    if component is None:
        return False
    stack = _prompt_stack(f"{component_id} {_component_label(component)}")
    if stack is None:
        return False
    component["stack"] = stack
    manifest["avspec"] = "0.2"
    return True


def _prompt_stack(label: str) -> dict[str, Any] | None:
    console.print(f"Implementation stack for {label}.")
    languages = _csv_prompt("Implementation languages, comma-separated")
    if not languages:
        return None

    stack: dict[str, Any] = {"languages": languages}
    package_manager = typer.prompt(
        "Package manager or build tool", default="", show_default=False
    ).strip()
    frameworks = _csv_prompt("Frameworks/runtime libraries, comma-separated", required=False)
    bdd = typer.prompt("BDD/test style, if known", default="", show_default=False).strip()

    commands: dict[str, str] = {}
    for key in ("install", "lint", "typecheck", "test", "arch"):
        value = typer.prompt(f"Command for {key}, if known", default="", show_default=False).strip()
        if value:
            commands[key] = value

    if package_manager:
        stack["package_manager"] = package_manager
    if frameworks:
        stack["frameworks"] = frameworks
    if bdd:
        stack["bdd"] = bdd
    if commands:
        stack["commands"] = commands

    return stack


def _ask_modules(manifest: dict[str, Any]) -> bool:
    console.print("Define modules first. Use apps, services, shared packages, or integrations.")
    components = _ensure_list(manifest, "components")
    before = len(components)

    while True:
        name = typer.prompt("Module name (blank when done)", default="", show_default=False).strip()
        if not name:
            break
        responsibility = typer.prompt("What is this module responsible for?").strip()
        if not responsibility:
            continue
        component_id = next_id("CMP-", [item["id"] for item in components])
        components.append({"id": component_id, "responsibility": f"{name} - {responsibility}"})
        console.print(f"  added {component_id}: {name}")

    if len(components) == before:
        return False

    _ask_dependencies(components)
    return True


def _ask_boundaries(spec_dir: Path, manifest: dict[str, Any]) -> bool:
    components = manifest.get("components") or []
    if not components:
        return False

    console.print("Assign each module to an architecture layer.")
    layers: dict[str, list[str]] = {}
    for component in components:
        group = typer.prompt(
            f"Architecture layer for {component['id']} {_component_label(component)}",
            default="application",
            show_default=True,
        ).strip()
        layers.setdefault(group or "application", []).append(component["id"])

    allow: list[dict[str, Any]] = []
    groups = list(layers)
    console.print("Architecture layers: " + ", ".join(groups))
    for group in groups:
        targets = _csv_prompt(f"{group} may depend on which layers?", required=False)
        if targets:
            allow.append({"from": group, "to": targets})

    _write_rules(spec_dir, {"layers": layers, "allow": allow})
    return True


def _ask_contracts(manifest: dict[str, Any], component_id: str | None) -> bool:
    components = manifest.get("components") or []
    if not components:
        return False
    contracts = _ensure_list(manifest, "contracts")
    before = len(contracts)

    targets = [
        component
        for component in components
        if component_id is None or component["id"] == component_id
    ]
    for component in targets:
        console.print(f"\nModule boundary: {component['id']} - {_component_label(component)}")
        exposes = typer.confirm("Does this module expose an API or message contract?", default=True)
        if not exposes:
            component.setdefault("interfaces", [])
            continue
        interface_name = typer.prompt(
            "Interface name", default=_default_interface_name(component), show_default=True
        ).strip()
        contract_type = typer.prompt("Contract type", default="openapi", show_default=True).strip()
        contract_id = next_id("CTR-", [item["id"] for item in contracts])
        contract_path = typer.prompt(
            "Contract path",
            default=f"contracts/{interface_name.lower().replace(' ', '-')}.openapi.yaml",
            show_default=True,
        ).strip()
        contracts.append({"id": contract_id, "type": contract_type, "path": contract_path})
        component.setdefault("interfaces", []).append(
            {"name": interface_name, "contract": contract_id}
        )

    return len(contracts) > before


def _ask_dependencies(components: list[dict[str, Any]]) -> None:
    console.print("\nDeclare module dependencies by ID. Leave blank when none.")
    valid_ids = {component["id"] for component in components}
    for component in components:
        label = _component_label(component)
        deps = _csv_prompt(f"{component['id']} {label} depends on", required=False)
        selected = [dep for dep in deps if dep in valid_ids and dep != component["id"]]
        if selected:
            component["depends_on"] = selected


def _ask_constitution(manifest: dict[str, Any]) -> bool:
    console.print(
        "Capture the suite constitution before capabilities. "
        "These are top-level principles or constraints that every module must honor."
    )
    principles = _ensure_list(manifest, "principles")
    constraints = _ensure_list(manifest, "constraints")
    before = len(principles) + len(constraints)

    while True:
        statement = typer.prompt(
            "Principle or constraint (blank when done)", default="", show_default=False
        ).strip()
        if not statement:
            break
        kind = (
            typer.prompt("Type: principle or constraint", default="principle", show_default=True)
            .strip()
            .lower()
        )
        if kind.startswith("c"):
            constraints.append(
                {
                    "id": next_id("CON-", [item["id"] for item in constraints]),
                    "statement": statement,
                }
            )
        else:
            principles.append(
                {"id": next_id("PR-", [item["id"] for item in principles]), "statement": statement}
            )

    return len(principles) + len(constraints) > before


def _ask_capabilities(manifest: dict[str, Any]) -> bool:
    components = manifest.get("components") or []
    if components:
        console.print(
            "Now define suite capabilities. These are still high-level; no acceptance tests yet."
        )
        for component in components:
            console.print(f"  {component['id']} - {_component_label(component)}")
    requirements = _ensure_list(manifest, "requirements")
    before = len(requirements)

    while True:
        title = typer.prompt(
            "Capability title (blank when done)", default="", show_default=False
        ).strip()
        if not title:
            break
        rationale = typer.prompt(
            "Why does this capability matter?", default="", show_default=False
        ).strip()
        requirement: dict[str, Any] = {
            "id": next_id("REQ-", [item["id"] for item in requirements]),
            "title": title,
            "acceptance": [],
        }
        if rationale:
            requirement["rationale"] = rationale
        requirements.append(requirement)

    return len(requirements) > before


def _ask_acceptance(manifest: dict[str, Any], requirement_id: str) -> bool:
    requirement = _find_requirement(manifest, requirement_id)
    if requirement is None:
        return False
    console.print(
        "Acceptance comes after modules and capabilities. "
        f"Requirement: {requirement_id} - {requirement['title']}"
    )
    acceptance = requirement.setdefault("acceptance", [])
    before = len(acceptance)

    while True:
        ears = typer.prompt(
            "Observable criterion in EARS form (blank when done)", default="", show_default=False
        ).strip()
        if not ears:
            break
        ac_id = next_id("AC-", all_ac_ids(manifest))
        acceptance.append({"id": ac_id, "ears": ears})
        console.print(f"  added {ac_id}")

    return len(acceptance) > before


def _ask_task(manifest: dict[str, Any], ac_id: str) -> bool:
    title = typer.prompt(f"Task that delivers {ac_id}", default="", show_default=False).strip()
    if not title:
        return False
    touches = _csv_prompt("Touched module/component/contract IDs, comma-separated", required=False)
    task: dict[str, Any] = {
        "id": next_id("TSK-", [item["id"] for item in manifest.get("tasks") or []]),
        "title": title,
        "satisfies": [ac_id],
    }
    if touches:
        task["touches"] = touches
    _ensure_list(manifest, "tasks").append(task)
    return True


def _ask_test(manifest: dict[str, Any], ac_id: str) -> bool:
    hit = _find_acceptance(manifest, ac_id)
    if hit is None:
        return False
    requirement_id, acceptance = hit
    default = f"verification/acceptance/{requirement_id}.feature#{ac_id.lower()}"
    test_ref = typer.prompt("Executable test reference", default=default, show_default=True).strip()
    acceptance["test"] = test_ref
    return True


def _ask_layer(spec_dir: Path, component_id: str) -> bool:
    layer = typer.prompt("Layer", default="application", show_default=True).strip()
    if not layer:
        return False
    _merge_layers(spec_dir, {layer: [component_id]})
    return True


def _scaffold_missing(spec_dir: Path, manifest: dict[str, Any], finding: Finding) -> bool:
    if not typer.confirm("Scaffold missing referenced files now?", default=True):
        return False
    made = scaffold(spec_dir, manifest)
    if made:
        console.print("scaffolded: " + ", ".join(made))
    return bool(made)


def scaffold(spec_dir: Path, manifest: dict[str, Any]) -> list[str]:
    made: list[str] = []

    def write(rel: str, body: str) -> None:
        path = spec_dir / rel
        if path.exists():
            return
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(body)
        made.append(rel)

    for contract in manifest.get("contracts") or []:
        rel = contract.get("path")
        if not rel:
            continue
        if contract.get("type") == "openapi":
            write(
                rel,
                "openapi: 3.1.0\n"
                f'info: {{ title: {Path(rel).name}, version: "0.0.0" }}\n'
                "paths: {}\n",
            )
        elif contract.get("type") == "asyncapi":
            write(
                rel,
                "asyncapi: 3.0.0\n"
                f'info: {{ title: {Path(rel).name}, version: "0.0.0" }}\n'
                "channels: {}\n",
            )
        else:
            write(
                rel,
                "# AVSPEC:STUB\n"
                '$schema: "https://json-schema.org/draft/2020-12/schema"\n'
                "type: object\n",
            )

    for requirement in manifest.get("requirements") or []:
        for acceptance in requirement.get("acceptance") or []:
            test_ref = acceptance.get("test")
            if not test_ref or "/" not in test_ref:
                continue
            rel = test_ref.split("#", maxsplit=1)[0].split("::", maxsplit=1)[0]
            if not rel.endswith(".feature"):
                continue
            write(
                rel,
                f"Feature: {requirement['id']}\n\n"
                f"  # {acceptance['id']}: {acceptance.get('ears', '')}\n"
                f"  Scenario: {test_ref.split('#', maxsplit=1)[-1]}\n"
                "    Given <precondition>\n"
                "    When <action>\n"
                "    Then <observable outcome>\n",
            )

    return made


def sync_artifacts(spec_dir: Path, manifest: dict[str, Any]) -> None:
    artifacts = manifest.get("artifacts") or {}
    if artifacts.get("constitution"):
        (spec_dir / artifacts["constitution"]).write_text(_constitution_markdown(manifest))
    if artifacts.get("requirements"):
        (spec_dir / artifacts["requirements"]).write_text(_requirements_markdown(manifest))
    if artifacts.get("design"):
        (spec_dir / artifacts["design"]).write_text(_design_markdown(manifest))
    if artifacts.get("tasks"):
        (spec_dir / artifacts["tasks"]).write_text(_tasks_markdown(manifest))


def _constitution_markdown(manifest: dict[str, Any]) -> str:
    lines = ["# Constitution - " + manifest["metadata"]["name"], ""]
    lines.append("## Principles")
    principles = manifest.get("principles") or []
    lines.extend(
        [f"- **{item['id']}** - {item['statement']}" for item in principles]
        or ["No principles captured yet."]
    )
    lines.extend(["", "## Constraints"])
    constraints = manifest.get("constraints") or []
    lines.extend(
        [f"- **{item['id']}** - {item['statement']}" for item in constraints]
        or ["No constraints captured yet."]
    )
    return "\n".join(lines) + "\n"


def _requirements_markdown(manifest: dict[str, Any]) -> str:
    lines = ["# Requirements - " + manifest["metadata"]["name"], ""]
    requirements = manifest.get("requirements") or []
    if not requirements:
        lines.append("No capabilities captured yet.")
        return "\n".join(lines) + "\n"
    for requirement in requirements:
        lines.extend([f"## {requirement['id']} - {requirement['title']}", ""])
        if requirement.get("rationale"):
            lines.extend([f"**Why:** {requirement['rationale']}", ""])
        acceptance = requirement.get("acceptance") or []
        if not acceptance:
            lines.extend(["Acceptance criteria not captured yet.", ""])
            continue
        for item in acceptance:
            lines.append(f"- **{item['id']}** - {item['ears']}")
            if item.get("test"):
                lines.append(f"  Test: `{item['test']}`")
        lines.append("")
    return "\n".join(lines)


def _design_markdown(manifest: dict[str, Any]) -> str:
    lines = ["# Design - " + manifest["metadata"]["name"], ""]
    description = manifest["metadata"].get("description")
    if description:
        lines.extend(["## Context", "", description, ""])
    lines.extend(["## Modules", ""])
    components = manifest.get("components") or []
    if not components:
        lines.append("No modules captured yet.")
        return "\n".join(lines) + "\n"
    for component in components:
        lines.append(f"- **{component['id']}** - {_component_label(component)}")
        deps = ", ".join(component.get("depends_on") or []) or "none"
        lines.append(f"  - Depends on: {deps}")
        if component.get("stack"):
            languages = ", ".join(component["stack"].get("languages") or [])
            lines.append(f"  - Stack: {languages or 'not captured'}")
    lines.append("")
    return "\n".join(lines)


def _tasks_markdown(manifest: dict[str, Any]) -> str:
    lines = ["# Tasks - " + manifest["metadata"]["name"], ""]
    tasks = manifest.get("tasks") or []
    if not tasks:
        lines.append("No tasks captured yet.")
        return "\n".join(lines) + "\n"
    for task in tasks:
        lines.append(f"- **{task['id']}** - {task['title']}")
        lines.append(f"  Satisfies: {', '.join(task.get('satisfies') or [])}")
        if task.get("touches"):
            lines.append(f"  Touches: {', '.join(task['touches'])}")
    return "\n".join(lines) + "\n"


def _merge_layers(spec_dir: Path, layers: dict[str, list[str]]) -> None:
    rules_path = spec_dir / "verification/architecture-rules.yaml"
    rules_path.parent.mkdir(parents=True, exist_ok=True)
    rules = yaml.load(rules_path.read_text()) if rules_path.exists() else {}
    if not isinstance(rules, dict):
        rules = {}
    rules.setdefault("layers", {})
    for layer, component_ids in layers.items():
        current = rules["layers"].setdefault(layer, [])
        for component_id in component_ids:
            if component_id not in current:
                current.append(component_id)
    rules.setdefault(
        "allow",
        [
            {"from": "application", "to": ["platform", "integration"]},
            {"from": "platform", "to": ["integration"]},
        ],
    )
    with rules_path.open("w") as handle:
        yaml.dump(rules, handle)


def _write_rules(spec_dir: Path, rules: dict[str, Any]) -> None:
    rules_path = spec_dir / "verification/architecture-rules.yaml"
    rules_path.parent.mkdir(parents=True, exist_ok=True)
    with rules_path.open("w") as handle:
        yaml.dump(rules, handle)


def _ensure_list(manifest: dict[str, Any], key: str) -> list[dict[str, Any]]:
    value = manifest.setdefault(key, [])
    if not isinstance(value, list):
        raise TypeError(f"{key} must be a list")
    return value


def _csv_prompt(prompt: str, *, required: bool = True) -> list[str]:
    while True:
        raw = typer.prompt(prompt, default="", show_default=False).strip()
        values = [item.strip() for item in raw.split(",") if item.strip()]
        if values or not required:
            return values


def next_id(prefix: str, existing: list[str]) -> str:
    numbers = [
        int(item.removeprefix(prefix))
        for item in existing
        if item.startswith(prefix) and item.removeprefix(prefix).isdigit()
    ]
    width = max(
        [3, *[len(item.removeprefix(prefix)) for item in existing if item.startswith(prefix)]]
    )
    return prefix + str((max(numbers) if numbers else 0) + 1).zfill(width)


def all_ac_ids(manifest: dict[str, Any]) -> list[str]:
    return [
        acceptance["id"]
        for requirement in manifest.get("requirements") or []
        for acceptance in requirement.get("acceptance") or []
    ]


def _find_requirement(manifest: dict[str, Any], requirement_id: str) -> dict[str, Any] | None:
    return next(
        (item for item in manifest.get("requirements") or [] if item["id"] == requirement_id), None
    )


def _find_acceptance(manifest: dict[str, Any], ac_id: str) -> tuple[str, dict[str, Any]] | None:
    for requirement in manifest.get("requirements") or []:
        for acceptance in requirement.get("acceptance") or []:
            if acceptance["id"] == ac_id:
                return requirement["id"], acceptance
    return None


def _component_label(component: dict[str, Any]) -> str:
    return str(component.get("responsibility") or "(no responsibility)")


def _default_interface_name(component: dict[str, Any]) -> str:
    raw = _component_label(component).split("-", maxsplit=1)[0].strip()
    words = [word for word in raw.replace("_", " ").split(" ") if word]
    if not words:
        return f"{component['id']}API"
    return "".join(word[:1].upper() + word[1:] for word in words) + "API"
