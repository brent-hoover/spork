"""Completeness rules — todos that fire on absence. An empty spec is a wall of todos."""

from __future__ import annotations

from collections.abc import Iterable
from pathlib import Path

from gherkin.errors import CompositeParserException
from gherkin.parser import Parser
from ruamel.yaml import YAML
from ruamel.yaml.error import YAMLError

from avspec.analysis import Spec
from avspec.findings import Finding, Severity
from avspec.rules import rule


def _todo(code: str, message: str, question: str, ref: str | None = None) -> Finding:
    return Finding(code=code, severity=Severity.TODO, message=message, question=question, ref=ref)


def _path_escapes(spec_dir: Path, rel_path: str) -> bool:
    """True if rel_path is absolute or resolves outside spec_dir (e.g. via ..)."""
    if Path(rel_path).is_absolute():
        return True
    resolved = (spec_dir / rel_path).resolve()
    return not resolved.is_relative_to(spec_dir.resolve())


def _path_escape(owner: str, rel_path: str) -> Finding:
    return Finding(
        code="PATH_ESCAPE",
        severity=Severity.ERROR,
        ref=owner,
        message=f"{owner} references {rel_path!r}, which escapes the spec directory.",
    )


# Scenario matching is AST-based, via the gherkin-official parser (an owner-approved
# runtime dependency). This correctly handles localized Gherkin (e.g. "# language: de"
# and "Szenario:") and never mistakes text inside a docstring, comment, or data table
# for a scenario heading, since it walks the parsed feature tree rather than lines.


def _scenario_names(children: list[dict]) -> Iterable[str]:
    """Yield scenario names from Feature/Rule children, recursing into Rule blocks."""
    for child in children:
        if "scenario" in child:
            yield child["scenario"]["name"].strip()
        elif "rule" in child:
            yield from _scenario_names(child["rule"].get("children", []))


def _scenario_exists(feature_path: Path, scenario: str) -> tuple[bool, str | None]:
    """Check whether scenario is defined in feature_path.

    Returns (found, parse_error): parse_error is a one-line summary if the file
    could not be parsed as Gherkin, else None.
    """
    text = feature_path.read_text(encoding="utf-8")
    try:
        document = Parser().parse(text)
    except CompositeParserException as exc:
        return False, str(exc).splitlines()[0]
    feature = document.get("feature")
    if feature is None:
        return False, None
    names = set(_scenario_names(feature.get("children", [])))
    return scenario in names, None


@rule
def stack_declared(spec: Spec) -> Iterable[Finding]:
    stack = spec.manifest.project.stack
    if stack is None or (not stack.languages and stack.commands is None):
        yield _todo(
            "NO_STACK",
            "No languages or commands are declared in the stack.",
            "What languages, package manager, and frameworks does this project use, "
            "and what commands run install, test, and lint?",
        )


@rule
def constitution_present(spec: Spec) -> Iterable[Finding]:
    if not spec.manifest.constitution:
        yield _todo(
            "NO_CONSTITUTION",
            "The constitution is empty.",
            "What principles and constraints must every module obey "
            "(dependency direction, test-first, tooling rules)?",
        )


@rule
def requirements_present(spec: Spec) -> Iterable[Finding]:
    if not spec.manifest.requirements:
        yield _todo(
            "NO_REQUIREMENTS",
            "No requirements are declared.",
            "What must this system do? Each answer becomes a REQ with a title and rationale.",
        )
        return
    for req in spec.manifest.requirements:
        if not req.acceptance:
            yield _todo(
                "REQ_NO_AC",
                f"{req.id} has no acceptance criteria.",
                f"How would you verify {req.id} ({req.title!r}) is done? "
                "Each answer becomes an AC with a testable statement.",
                ref=req.id,
            )
        for ac in req.acceptance:
            if ac.test is None:
                yield _todo(
                    "AC_NO_TEST",
                    f"{ac.id} has no test reference.",
                    f"Which Gherkin scenario verifies {ac.id}? "
                    "Answer as 'path/to/file.feature#scenario name'.",
                    ref=ac.id,
                )


@rule
def modules_present(spec: Spec) -> Iterable[Finding]:
    if not spec.manifest.modules:
        yield _todo(
            "NO_MODULES",
            "No modules are declared.",
            "What are the modules of this system — the units with one responsibility each?",
        )
        return
    for module in spec.manifest.modules:
        if not module.responsibility:
            yield _todo(
                "MOD_NO_RESPONSIBILITY",
                f"{module.id} has no responsibility statement.",
                f"In one sentence, what is {module.id} ({module.name}) responsible for?",
                ref=module.id,
            )
        if module.boundaries is None:
            yield _todo(
                "MOD_NO_BOUNDARIES",
                f"{module.id} declares no boundaries.",
                f"Which modules may {module.id} import? "
                "An empty list is a valid answer and means: none.",
                ref=module.id,
            )


@rule
def data_present(spec: Spec) -> Iterable[Finding]:
    if spec.manifest.data is None:
        yield _todo(
            "NO_DATA",
            "No data section is declared.",
            "What are the core entities, their fields and relations — "
            "or confirm the system has no persistent data.",
        )
        return
    owned = {e for m in spec.manifest.modules for e in m.owns}
    for entity in spec.manifest.data.entities:
        if not entity.fields:
            yield _todo(
                "ENT_NO_FIELDS",
                f"{entity.id} has no fields.",
                f"What fields does {entity.id} ({entity.name}) have?",
                ref=entity.id,
            )
        if entity.id not in owned:
            yield _todo(
                "ENT_UNOWNED",
                f"{entity.id} is owned by no module.",
                f"Which module owns {entity.id} ({entity.name}) — "
                "reads and writes its persisted state?",
                ref=entity.id,
            )


@rule
def apps_present(spec: Spec) -> Iterable[Finding]:
    manifest = spec.manifest
    if not manifest.modules:
        return
    if not manifest.apps:
        yield _todo(
            "NO_APPS",
            "Modules exist but no apps are declared.",
            "Which deployable application(s) do the modules belong to? "
            "One app is a valid, explicit answer; separate apps only for a forcing "
            "reason (different runtime shape, deploy cadence, isolation, scaling).",
        )
        return
    assigned = {mod_id for app in manifest.apps for mod_id in app.modules}
    for module in manifest.modules:
        if module.id not in assigned:
            yield _todo(
                "MOD_NO_APP",
                f"{module.id} is in no app.",
                f"Which app does {module.id} ({module.name}) belong to?",
                ref=module.id,
            )


@rule
def test_files_exist(spec: Spec) -> Iterable[Finding]:
    for req in spec.manifest.requirements:
        for ac in req.acceptance:
            if ac.test is None:
                continue
            has_hash = "#" in ac.test
            rel_path, _, scenario = ac.test.partition("#")
            if _path_escapes(spec.dir, rel_path):
                yield _path_escape(ac.id, ac.test)
                continue
            scenario = scenario.strip()
            if not has_hash or not scenario or not rel_path.endswith(".feature"):
                yield _todo(
                    "TEST_REF_INVALID",
                    f"{ac.id} test ref {ac.test!r} is not "
                    "'path/to/file.feature#scenario name'.",
                    f"Which Gherkin scenario verifies {ac.id}? "
                    "Answer as 'path/to/file.feature#scenario name'.",
                    ref=ac.id,
                )
                continue
            full_path = spec.dir / rel_path
            if not full_path.is_file():
                yield _todo(
                    "TEST_FILE_MISSING",
                    f"{ac.id} references {rel_path}, which does not exist.",
                    f"Create {rel_path} with the scenario that verifies {ac.id}.",
                    ref=ac.id,
                )
                continue
            found, parse_error = _scenario_exists(full_path, scenario)
            if not found and parse_error is not None:
                yield _todo(
                    "TEST_SCENARIO_MISSING",
                    f"{ac.id} references scenario {scenario!r} in {rel_path}, "
                    f"which could not be parsed as Gherkin: {parse_error}",
                    f"Fix the Gherkin syntax in {rel_path}, then confirm "
                    f"the scenario {scenario!r} exists for {ac.id}.",
                    ref=ac.id,
                )
            elif not found:
                yield _todo(
                    "TEST_SCENARIO_MISSING",
                    f"{ac.id} references scenario {scenario!r} in {rel_path}, "
                    "which does not exist.",
                    f"Add the scenario {scenario!r} to {rel_path} for {ac.id}, "
                    "or fix the reference.",
                    ref=ac.id,
                )


_YAML_CONTRACT_TYPES = {"openapi", "asyncapi", "jsonschema"}


@rule
def contract_files(spec: Spec) -> Iterable[Finding]:
    yaml = YAML(typ="safe")
    for module in spec.manifest.modules:
        for contract in module.contracts:
            if _path_escapes(spec.dir, contract.path):
                yield _path_escape(contract.id, contract.path)
                continue
            path = spec.dir / contract.path
            if not path.is_file():
                yield _todo(
                    "CONTRACT_FILE_MISSING",
                    f"{contract.id} references {contract.path}, which does not exist.",
                    f"Create the {contract.type} document at {contract.path} for {contract.id}.",
                    ref=contract.id,
                )
                continue
            if contract.type.lower() not in _YAML_CONTRACT_TYPES:
                continue
            try:
                yaml.load(path.read_text(encoding="utf-8"))
            except YAMLError as exc:
                yield Finding(
                    code="CONTRACT_UNPARSEABLE",
                    severity=Severity.ERROR,
                    ref=contract.id,
                    message=f"{contract.path} is not parseable: {exc}",
                )
