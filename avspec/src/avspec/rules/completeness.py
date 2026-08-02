"""Completeness rules — todos that fire on absence. An empty spec is a wall of todos."""

from __future__ import annotations

from collections.abc import Iterable

from ruamel.yaml import YAML
from ruamel.yaml.error import YAMLError

from avspec.analysis import Spec
from avspec.findings import Finding, Severity
from avspec.rules import rule


def _todo(code: str, message: str, question: str, ref: str | None = None) -> Finding:
    return Finding(code=code, severity=Severity.TODO, message=message, question=question, ref=ref)


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
def test_files_exist(spec: Spec) -> Iterable[Finding]:
    for req in spec.manifest.requirements:
        for ac in req.acceptance:
            if ac.test is None:
                continue
            rel_path = ac.test.split("#", 1)[0]
            if not (spec.dir / rel_path).is_file():
                yield _todo(
                    "TEST_FILE_MISSING",
                    f"{ac.id} references {rel_path}, which does not exist.",
                    f"Create {rel_path} with the scenario that verifies {ac.id}.",
                    ref=ac.id,
                )


_YAML_CONTRACT_TYPES = {"openapi", "asyncapi", "jsonschema"}


@rule
def contract_files(spec: Spec) -> Iterable[Finding]:
    yaml = YAML(typ="safe")
    for module in spec.manifest.modules:
        for contract in module.contracts:
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
