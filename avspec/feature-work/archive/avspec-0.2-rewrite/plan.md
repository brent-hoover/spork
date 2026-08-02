# AVSpec 0.2 Core Implementation Plan (Plan 1 of 3)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the Python core of AVSpec 0.2 — Pydantic models for the extended format, a rule-registry analyzer whose todos fire on absence, and the `avspec verify` / `avspec next` commands.

**Architecture:** Pydantic v2 models are the canonical format definition. `loading.py` keeps two representations of a spec: a validated `Manifest` for analysis and a ruamel round-trip document for mutation (Plan 2 needs the latter to rewrite `avspec.yaml` without destroying comments). `analysis.py` runs an ordered registry of pure rule functions `(Spec) -> Iterable[Finding]`; every rule is one function plus one test, and all CLI commands read the same registry.

**Tech Stack:** Python 3.12+, uv, Pydantic v2, ruamel.yaml, openapi-spec-validator, typer, pytest, pytest-bdd, ruff, ty.

**Source design:** `feature-work/avspec-0.2-rewrite/design.md`

## Global Constraints

- Python `>=3.12`. Package management with `uv` — never pip, poetry, or pipenv.
- **Exactly five runtime dependencies**, no others without approval: `pydantic`, `ruamel.yaml`, `openapi-spec-validator`, `questionary`, `typer`. Dev: `pytest`, `pytest-bdd`, `ruff`, `ty`.
- **PyYAML is forbidden.** `ruamel.yaml` only — round-trip comment and key-order preservation is required.
- **Never hand-author JSON Schema.** Pydantic models are canonical; `avspec.schema.json` is generated (Plan 2).
- **Neutral type vocabulary** for entity fields and config entries, exactly: `string`, `integer`, `decimal`, `boolean`, `datetime`, `date`, `uuid`, `json`, `enum`, `ref`. Never native Python type names.
- **No enums for stack tooling.** `package_manager`, `bdd`, `frameworks`, `commands.*`, and `data.store` are free-form strings. 0.1's `fitness_function` enum is removed entirely.
- **Three severities, unchanged semantics:** `error` always fails; `todo` fails only at `status: ready` or `built`; `warn` never fails.
- Type hints on all new code. Fail loudly — no bare `except`, no swallowed exceptions.
- Conventional commit messages. No co-branding lines.
- A spec directory contains zero Python. Spec directories no longer carry `avspec.schema.yaml`.

---

## File Structure

| File | Responsibility |
|---|---|
| `pyproject.toml` | uv project, deps, ruff/pytest config, `avspec` entry point |
| `src/avspec/__init__.py` | `__version__` only |
| `src/avspec/findings.py` | `Finding`, `Severity`, `CODE_ORDER`, `ordered()` — no other imports |
| `src/avspec/model.py` | Pydantic v2 models — the canonical format definition |
| `src/avspec/loading.py` | ruamel read/write + Pydantic validation → `LoadResult` |
| `src/avspec/analysis.py` | `Spec`, `Report`, `analyze()`, rule registry plumbing |
| `src/avspec/rules/__init__.py` | `RULES` registry, `@rule` decorator, submodule imports |
| `src/avspec/rules/wellformed.py` | Errors: duplicate IDs, dangling refs, cycles, ownership, bindings |
| `src/avspec/rules/completeness.py` | Absence and traceability todos |
| `src/avspec/rules/ui.py` | View/action todos including reachability |
| `src/avspec/rules/contracts_rules.py` | `ACTION_UNRESOLVED`, `CONTRACT_EMPTY` |
| `src/avspec/rules/content.py` | Stub markers, placeholders, missing files, MIRROR |
| `src/avspec/rules/architecture.py` | Layer placement and dependency violations |
| `src/avspec/contracts.py` | Contract parsing, validation, operation lookup |
| `src/avspec/cli/__init__.py` | typer app, subcommand registration |
| `src/avspec/cli/verify.py` | `avspec verify` |
| `src/avspec/cli/next_.py` | `avspec next` |
| `tests/conftest.py` | `spec_dir` fixture — writes a minimal valid spec to `tmp_path` |
| `tests/unit/test_*.py` | One test module per rule module |
| `tests/features/*.feature` | CLI behavior, pytest-bdd |

Rules live in separate modules by family so a subagent can hold one file in context. `rules/__init__.py` imports every submodule for registration side effects — a rule module that isn't imported there silently does nothing.

---

## Task 1: Project scaffold

**Files:**
- Create: `pyproject.toml`, `src/avspec/__init__.py`, `src/avspec/cli/__init__.py`, `tests/unit/test_package.py`
- Create: `.python-version`

**Interfaces:**
- Consumes: nothing
- Produces: `avspec.__version__: str`; `avspec.cli.app: typer.Typer`; console script `avspec`

- [ ] **Step 1: Initialize the uv project**

```bash
cd /Users/brent/Projects/personal/avspec
uv init --lib --name avspec --python 3.12 --no-workspace --vcs none
```

If `uv init` refuses because files exist, skip it — Step 2 writes `pyproject.toml` outright.

- [ ] **Step 2: Write `pyproject.toml`**

```toml
[project]
name = "avspec"
version = "0.2.0"
description = "Agent-Verifiable Architecture Spec — format, verifier, and guided-QA authoring CLI"
readme = "README.md"
requires-python = ">=3.12"
license = { text = "MIT" }
dependencies = [
    "pydantic>=2.9",
    "ruamel.yaml>=0.18",
    "openapi-spec-validator>=0.7",
    "questionary>=2.0",
    "typer>=0.12",
]

[project.scripts]
avspec = "avspec.cli:app"

[dependency-groups]
dev = [
    "pytest>=8.3",
    "pytest-bdd>=8.1",
    "ruff>=0.6",
    "ty",
]

[build-system]
requires = ["hatchling"]
build-backend = "hatchling.build"

[tool.hatch.build.targets.wheel]
packages = ["src/avspec"]

[tool.ruff]
line-length = 100
src = ["src", "tests"]

[tool.ruff.lint]
select = ["E", "F", "I", "UP", "B", "SIM"]

[tool.pytest.ini_options]
testpaths = ["tests"]
addopts = "-q"
```

- [ ] **Step 3: Write the package skeleton**

`src/avspec/__init__.py`:

```python
"""AVSpec — Agent-Verifiable Architecture Spec."""

__version__ = "0.2.0"
```

`src/avspec/cli/__init__.py`:

```python
"""AVSpec command line interface."""

from __future__ import annotations

import typer

from avspec import __version__

app = typer.Typer(
    name="avspec",
    help="Author and verify Agent-Verifiable Architecture Specs.",
    no_args_is_help=True,
)


@app.command()
def version() -> None:
    """Print the AVSpec version."""
    typer.echo(__version__)
```

- [ ] **Step 4: Write the failing test**

`tests/unit/test_package.py`:

```python
from typer.testing import CliRunner

from avspec import __version__
from avspec.cli import app


def test_version_constant_matches_pyproject() -> None:
    assert __version__ == "0.2.0"


def test_version_command_prints_version() -> None:
    result = CliRunner().invoke(app, ["version"])
    assert result.exit_code == 0
    assert result.stdout.strip() == "0.2.0"
```

- [ ] **Step 5: Sync and run**

```bash
uv sync
uv run pytest tests/unit/test_package.py -v
```

Expected: both tests PASS.

- [ ] **Step 6: Verify lint and type-check run clean**

```bash
uv run ruff check .
uv run ruff format --check .
```

Expected: no errors. If `ruff format --check` fails, run `uv run ruff format .` and re-run.

- [ ] **Step 7: Commit**

```bash
git add pyproject.toml uv.lock .python-version src/avspec tests/unit/test_package.py
git commit -m "feat: scaffold Python package with typer CLI entry point"
```

---

## Task 2: Findings module

**Files:**
- Create: `src/avspec/findings.py`, `tests/unit/test_findings.py`

**Interfaces:**
- Consumes: nothing (this module must import nothing from `avspec`)
- Produces:
  - `Severity = Literal["error", "todo", "warn"]`
  - `Finding(severity, code, message, ids=(), question=None)` — frozen dataclass
  - `CODE_ORDER: tuple[str, ...]`
  - `ordered(findings: Iterable[Finding]) -> list[Finding]`

`CODE_ORDER` is **authoring order**, not alphabetical. Task 17 in Plan 2 walks this list to decide which question to ask next, so a code placed wrongly here makes the interview ask about components before they exist.

- [ ] **Step 1: Write the failing test**

`tests/unit/test_findings.py`:

```python
from avspec.findings import CODE_ORDER, Finding, ordered


def _f(severity: str, code: str) -> Finding:
    return Finding(severity=severity, code=code, message=code)


def test_errors_sort_before_todos_before_warns() -> None:
    result = ordered([_f("warn", "MIRROR"), _f("todo", "NO_STACK"), _f("error", "DUP_ID")])
    assert [f.severity for f in result] == ["error", "todo", "warn"]


def test_within_a_severity_authoring_order_wins() -> None:
    result = ordered([_f("todo", "NO_TASKS"), _f("todo", "NO_STACK"), _f("todo", "REQ_NO_AC")])
    assert [f.code for f in result] == ["NO_STACK", "REQ_NO_AC", "NO_TASKS"]


def test_components_are_asked_about_before_ui_and_tasks() -> None:
    assert CODE_ORDER.index("NO_COMPONENTS") < CODE_ORDER.index("NO_VIEWS")
    assert CODE_ORDER.index("NO_VIEWS") < CODE_ORDER.index("NO_TASKS")


def test_entity_ownership_is_asked_after_components_exist() -> None:
    assert CODE_ORDER.index("NO_COMPONENTS") < CODE_ORDER.index("ENT_UNOWNED")


def test_unknown_codes_sort_last_without_raising() -> None:
    result = ordered([_f("todo", "WAT"), _f("todo", "NO_STACK")])
    assert [f.code for f in result] == ["NO_STACK", "WAT"]


def test_findings_are_hashable_and_frozen() -> None:
    f = _f("todo", "NO_STACK")
    assert {f, f} == {f}
```

- [ ] **Step 2: Run to verify it fails**

```bash
uv run pytest tests/unit/test_findings.py -v
```

Expected: FAIL — `ModuleNotFoundError: No module named 'avspec.findings'`.

- [ ] **Step 3: Implement**

`src/avspec/findings.py`:

```python
"""Findings — the single vocabulary shared by the verifier and the interview."""

from __future__ import annotations

from collections.abc import Iterable
from dataclasses import dataclass, field
from typing import Literal

Severity = Literal["error", "todo", "warn"]


@dataclass(frozen=True)
class Finding:
    """One thing the analyzer noticed about a spec.

    `question` is the prompt the interview asks to close a todo. Errors carry
    no question: they are fixed by editing the manifest, not by answering.
    """

    severity: Severity
    code: str
    message: str
    ids: tuple[str, ...] = field(default=())
    question: str | None = None


def error(code: str, message: str, *ids: str) -> Finding:
    return Finding(severity="error", code=code, message=message, ids=ids)


def todo(code: str, message: str, *ids: str, question: str) -> Finding:
    return Finding(severity="todo", code=code, message=message, ids=ids, question=question)


def warn(code: str, message: str, *ids: str) -> Finding:
    return Finding(severity="warn", code=code, message=message, ids=ids)


# Authoring order. The interview walks this to pick the next question, so the
# sequence must match how a person actually builds a spec: you cannot be asked
# which component owns an entity before any component exists.
CODE_ORDER: tuple[str, ...] = (
    # --- errors: well-formedness -------------------------------------------
    "SCHEMA",
    "DUP_ID",
    "DANGLING",
    "CYCLE",
    "ENT_MULTI_OWNER",
    "DISPLAY_UNKNOWN_FIELD",
    "ACTION_UNRESOLVED",
    "ARCH_VIOLATION",
    # --- todos: foundation --------------------------------------------------
    "NO_STACK",
    "STACK_INCOMPLETE",
    "NO_PRINCIPLES",
    # --- todos: requirements ------------------------------------------------
    "NO_REQUIREMENTS",
    "REQ_NO_AC",
    # --- todos: data --------------------------------------------------------
    "DATA_UNDECLARED",
    # --- todos: design ------------------------------------------------------
    "NO_COMPONENTS",
    "CMP_NO_CONTRACT",
    "ENT_UNOWNED",
    "LAYER_UNPLACED",
    # --- todos: ui ----------------------------------------------------------
    "UI_UNDECLARED",
    "NO_VIEWS",
    "NO_ENTRY",
    "VIEW_NO_AC",
    "VIEW_UNREACHABLE",
    "ACTION_ORPHAN",
    # --- todos: tasks and tests ---------------------------------------------
    "NO_TASKS",
    "AC_UNSATISFIED",
    "AC_NO_OWNER",
    "AC_NO_TEST",
    # --- todos: files -------------------------------------------------------
    "ARTIFACT_MISSING",
    "CONTRACT_MISSING",
    "CONTRACT_EMPTY",
    "TEST_MISSING",
    "PLACEHOLDER",
    "ADR_MISSING",
    "STUB_CONTENT",
    "MIRROR",
    # --- warns --------------------------------------------------------------
    "NO_ARCH_RULES",
)

_SEV_RANK: dict[str, int] = {"error": 0, "todo": 1, "warn": 2}


def _code_index(code: str) -> int:
    try:
        return CODE_ORDER.index(code)
    except ValueError:
        return len(CODE_ORDER)


def ordered(findings: Iterable[Finding]) -> list[Finding]:
    """Sort by severity, then by authoring order. Unknown codes sort last."""
    return sorted(findings, key=lambda f: (_SEV_RANK[f.severity], _code_index(f.code)))
```

- [ ] **Step 4: Run to verify it passes**

```bash
uv run pytest tests/unit/test_findings.py -v
```

Expected: all six PASS.

- [ ] **Step 5: Commit**

```bash
git add src/avspec/findings.py tests/unit/test_findings.py
git commit -m "feat: add Finding vocabulary and authoring-order sort"
```

---

## Task 3: Models for the 0.1 sections

**Files:**
- Create: `src/avspec/model.py`, `tests/unit/test_model_core.py`

**Interfaces:**
- Consumes: nothing
- Produces: `Base`, ID aliases (`PrId`, `ConId`, `ReqId`, `AcId`, `CtrId`, `CmpId`, `AdrId`, `TskId`, `EntId`, `CfgId`, `ViewId`, `ActId`, `TouchRef`), `Metadata`, `Artifacts`, `Principle`, `Constraint`, `Acceptance`, `Requirement`, `Contract`, `Interface`, `Component`, `Decision`, `Task`, `Manifest`

Task 4 extends `Manifest` and `Component` in place. Define the ID aliases for the 0.2 sections now (`EntId`, `CfgId`, `ViewId`, `ActId`) so `TouchRef` is correct from the start and Task 4 only adds models.

- [ ] **Step 1: Write the failing test**

`tests/unit/test_model_core.py`:

```python
import pytest
from pydantic import ValidationError

from avspec.model import Component, Manifest, Requirement, Task

MINIMAL: dict = {
    "avspec": "0.2",
    "metadata": {"name": "linkshort", "status": "draft"},
    "mode": "greenfield",
    "artifacts": {
        "constitution": "constitution.md",
        "requirements": "requirements.md",
        "design": "design.md",
        "tasks": "tasks.md",
    },
}


def test_minimal_manifest_validates() -> None:
    m = Manifest.model_validate(MINIMAL)
    assert m.metadata.name == "linkshort"
    assert m.metadata.status == "draft"
    assert m.requirements == []


def test_unknown_top_level_key_is_rejected() -> None:
    with pytest.raises(ValidationError, match="Extra inputs are not permitted"):
        Manifest.model_validate(MINIMAL | {"wat": 1})


def test_status_is_constrained() -> None:
    with pytest.raises(ValidationError):
        Manifest.model_validate({**MINIMAL, "metadata": {"name": "x", "status": "shipped"}})


def test_id_patterns_are_enforced() -> None:
    with pytest.raises(ValidationError):
        Requirement.model_validate({"id": "REQ-1", "title": "too few digits"})
    assert Requirement.model_validate({"id": "REQ-001", "title": "ok"}).id == "REQ-001"


def test_task_must_satisfy_at_least_one_ac() -> None:
    with pytest.raises(ValidationError):
        Task.model_validate({"id": "TSK-001", "title": "x", "satisfies": []})


def test_task_touches_accepts_every_02_reference_kind() -> None:
    t = Task.model_validate(
        {
            "id": "TSK-001",
            "title": "x",
            "satisfies": ["AC-001"],
            "touches": ["CMP-001", "CTR-001", "ENT-001", "CFG-001", "VIEW-001", "ACT-001"],
        }
    )
    assert len(t.touches) == 6


def test_task_touches_rejects_unknown_prefix() -> None:
    with pytest.raises(ValidationError):
        Task.model_validate({"id": "TSK-001", "title": "x", "satisfies": ["AC-001"], "touches": ["REQ-001"]})


def test_component_defaults_are_empty_lists_not_none() -> None:
    c = Component.model_validate({"id": "CMP-001", "responsibility": "r"})
    assert c.depends_on == [] and c.interfaces == []
```

- [ ] **Step 2: Run to verify it fails**

```bash
uv run pytest tests/unit/test_model_core.py -v
```

Expected: FAIL — `ModuleNotFoundError: No module named 'avspec.model'`.

- [ ] **Step 3: Implement**

`src/avspec/model.py`:

```python
"""Canonical definition of the AVSpec manifest format, version 0.2.

These models ARE the format. `avspec.schema.json` is generated from them for
non-Python consumers; it is never hand-authored.
"""

from __future__ import annotations

from typing import Annotated, Literal

from pydantic import BaseModel, ConfigDict, Field, StringConstraints


class Base(BaseModel):
    """Reject unknown keys everywhere — a typo must fail, not be ignored."""

    model_config = ConfigDict(extra="forbid")


def _id(prefix: str) -> object:
    return StringConstraints(pattern=rf"^{prefix}-[0-9]{{3,}}$")


PrId = Annotated[str, _id("PR")]
ConId = Annotated[str, _id("CON")]
ReqId = Annotated[str, _id("REQ")]
AcId = Annotated[str, _id("AC")]
CtrId = Annotated[str, _id("CTR")]
CmpId = Annotated[str, _id("CMP")]
AdrId = Annotated[str, _id("ADR")]
TskId = Annotated[str, _id("TSK")]
EntId = Annotated[str, _id("ENT")]
CfgId = Annotated[str, _id("CFG")]
ViewId = Annotated[str, _id("VIEW")]
ActId = Annotated[str, _id("ACT")]

TouchRef = Annotated[
    str, StringConstraints(pattern=r"^(CMP|CTR|ENT|CFG|VIEW|ACT)-[0-9]{3,}$")
]

NonEmpty = Annotated[str, StringConstraints(min_length=1)]

Status = Literal["draft", "ready", "built"]


class Metadata(Base):
    name: NonEmpty
    description: str | None = None
    status: Status


class Artifacts(Base):
    """Paths to the four human-readable layers, relative to the manifest."""

    constitution: NonEmpty
    requirements: NonEmpty
    design: NonEmpty
    tasks: NonEmpty


class Principle(Base):
    id: PrId
    statement: NonEmpty


class Constraint(Base):
    id: ConId
    statement: NonEmpty
    gate: bool = True


class Acceptance(Base):
    id: AcId
    ears: NonEmpty
    test: str | None = None


class Requirement(Base):
    id: ReqId
    title: NonEmpty
    rationale: str | None = None
    acceptance: list[Acceptance] = Field(default_factory=list)


class Contract(Base):
    id: CtrId
    type: Literal["openapi", "jsonschema", "asyncapi"]
    path: NonEmpty


class Interface(Base):
    name: NonEmpty
    contract: CtrId


class Component(Base):
    id: CmpId
    responsibility: NonEmpty
    depends_on: list[CmpId] = Field(default_factory=list)
    interfaces: list[Interface] = Field(default_factory=list)


class Decision(Base):
    id: AdrId
    title: NonEmpty
    status: Literal["proposed", "accepted", "superseded", "deprecated"]
    path: NonEmpty


class Task(Base):
    id: TskId
    title: NonEmpty
    satisfies: list[AcId] = Field(min_length=1)
    touches: list[TouchRef] = Field(default_factory=list)
    depends_on: list[TskId] = Field(default_factory=list)
    parallelizable: bool = False


class Manifest(Base):
    avspec: Annotated[str, StringConstraints(pattern=r"^\d+\.\d+$")]
    metadata: Metadata
    mode: Literal["greenfield"]
    artifacts: Artifacts
    principles: list[Principle] = Field(default_factory=list)
    constraints: list[Constraint] = Field(default_factory=list)
    requirements: list[Requirement] = Field(default_factory=list)
    contracts: list[Contract] = Field(default_factory=list)
    components: list[Component] = Field(default_factory=list)
    decisions: list[Decision] = Field(default_factory=list)
    tasks: list[Task] = Field(default_factory=list)
```

- [ ] **Step 4: Run to verify it passes**

```bash
uv run pytest tests/unit/test_model_core.py -v
```

Expected: all eight PASS.

- [ ] **Step 5: Commit**

```bash
git add src/avspec/model.py tests/unit/test_model_core.py
git commit -m "feat: add Pydantic models for AVSpec 0.1 manifest sections"
```

---

## Task 4: Models for the 0.2 sections

**Files:**
- Modify: `src/avspec/model.py`
- Create: `tests/unit/test_model_02.py`

**Interfaces:**
- Consumes: everything from Task 3
- Produces: `FieldType`, `Language`, `Commands`, `Stack`, `EntityField`, `Relation`, `Entity`, `Data`, `ConfigEntry`, `Action`, `View`, `Ui`, `DisplayRef`, `InvokeRef`; `Component.owns: list[EntId]`; `Manifest.stack`, `.data`, `.config`, `.ui`

- [ ] **Step 1: Write the failing test**

`tests/unit/test_model_02.py`:

```python
import pytest
from pydantic import ValidationError

from avspec.model import Component, Data, Manifest, Stack, Ui, View

MINIMAL: dict = {
    "avspec": "0.2",
    "metadata": {"name": "linkshort", "status": "draft"},
    "mode": "greenfield",
    "artifacts": {
        "constitution": "constitution.md",
        "requirements": "requirements.md",
        "design": "design.md",
        "tasks": "tasks.md",
    },
}


def test_stack_commands_are_free_form_strings() -> None:
    s = Stack.model_validate(
        {
            "languages": [{"name": "go", "version": "1.23"}],
            "package_manager": "go",
            "frameworks": ["chi"],
            "bdd": "godog",
            "commands": {"install": "go mod download", "test": "go test ./...", "arch": "go-arch-lint check"},
        }
    )
    assert s.commands is not None
    assert s.commands.test == "go test ./..."
    assert s.languages[0].name == "go"


def test_fitness_function_enum_is_gone() -> None:
    with pytest.raises(ValidationError, match="Extra inputs are not permitted"):
        Stack.model_validate({"fitness_function": "import-linter"})


def test_entity_field_types_use_the_neutral_vocabulary() -> None:
    d = Data.model_validate(
        {
            "store": "postgres",
            "entities": [
                {
                    "id": "ENT-001",
                    "name": "Link",
                    "fields": [{"name": "code", "type": "string", "required": True, "unique": True}],
                }
            ],
        }
    )
    assert d.entities[0].fields[0].type == "string"


def test_native_python_type_names_are_rejected() -> None:
    with pytest.raises(ValidationError):
        Data.model_validate(
            {"store": "sqlite", "entities": [{"id": "ENT-001", "name": "L", "fields": [{"name": "c", "type": "str"}]}]}
        )


def test_store_none_is_the_explicit_opt_out() -> None:
    assert Data.model_validate({"store": "none"}).entities == []


def test_component_owns_entities() -> None:
    c = Component.model_validate({"id": "CMP-001", "responsibility": "r", "owns": ["ENT-001"]})
    assert c.owns == ["ENT-001"]


def test_view_display_ref_must_be_entity_dot_field() -> None:
    v = View.model_validate(
        {"id": "VIEW-001", "name": "Create", "purpose": "p", "displays": ["ENT-001.target_url"]}
    )
    assert v.displays == ["ENT-001.target_url"]
    with pytest.raises(ValidationError):
        View.model_validate({"id": "VIEW-001", "name": "C", "purpose": "p", "displays": ["ENT-001"]})


def test_action_invoke_ref_must_be_contract_hash_operation() -> None:
    u = Ui.model_validate(
        {
            "kind": "web",
            "entry": "VIEW-001",
            "views": [{"id": "VIEW-001", "name": "C", "purpose": "p", "route": "/"}],
            "actions": [{"id": "ACT-001", "name": "create_link", "invokes": "CTR-001#createLink"}],
        }
    )
    assert u.actions[0].invokes == "CTR-001#createLink"
    with pytest.raises(ValidationError):
        Ui.model_validate({"kind": "web", "actions": [{"id": "ACT-001", "name": "x", "invokes": "CTR-001"}]})


def test_ui_kind_none_is_the_explicit_opt_out() -> None:
    assert Ui.model_validate({"kind": "none"}).views == []


def test_full_02_manifest_validates() -> None:
    m = Manifest.model_validate(
        MINIMAL
        | {
            "stack": {"languages": [{"name": "python", "version": "3.12"}], "commands": {"test": "uv run pytest"}},
            "data": {"store": "sqlite", "entities": [{"id": "ENT-001", "name": "Link"}]},
            "config": [{"id": "CFG-001", "name": "DATABASE_URL", "type": "string", "secret": True}],
            "ui": {"kind": "none"},
        }
    )
    assert m.stack is not None and m.data is not None and m.ui is not None
    assert m.config[0].secret is True


def test_02_sections_are_absent_by_default_not_empty() -> None:
    m = Manifest.model_validate(MINIMAL)
    assert m.stack is None and m.data is None and m.ui is None
    assert m.config == []
```

The `is None` distinction in the last test is load-bearing: `DATA_UNDECLARED` and `UI_UNDECLARED` (Task 8, Task 9) fire on absence, and `store: none` / `kind: none` are how an author opts out deliberately. An empty default would erase that difference.

- [ ] **Step 2: Run to verify it fails**

```bash
uv run pytest tests/unit/test_model_02.py -v
```

Expected: FAIL — `ImportError: cannot import name 'Stack' from 'avspec.model'`.

- [ ] **Step 3: Add the 0.2 models**

Insert into `src/avspec/model.py`, after the `NonEmpty` alias:

```python
DisplayRef = Annotated[
    str, StringConstraints(pattern=r"^ENT-[0-9]{3,}\.[A-Za-z_][A-Za-z0-9_]*$")
]
InvokeRef = Annotated[str, StringConstraints(pattern=r"^CTR-[0-9]{3,}#.+$")]

# Language-independent by design. Mapping these onto str/string/String is the
# building agent's job, so native type names must never appear here.
FieldType = Literal[
    "string", "integer", "decimal", "boolean", "datetime", "date", "uuid", "json", "enum", "ref"
]
```

Then add these model classes before `Manifest`:

```python
class Language(Base):
    name: NonEmpty
    version: str | None = None


class Commands(Base):
    """How to drive this project's toolchain. Free-form so AVSpec never needs
    to know what Go, Rust, or TypeScript tooling looks like."""

    install: str | None = None
    test: str | None = None
    lint: str | None = None
    arch: str | None = None


class Stack(Base):
    languages: list[Language] = Field(default_factory=list)
    package_manager: str | None = None
    frameworks: list[str] = Field(default_factory=list)
    bdd: str | None = None
    commands: Commands | None = None


class EntityField(Base):
    name: NonEmpty
    type: FieldType
    required: bool = False
    unique: bool = False
    description: str | None = None


class Relation(Base):
    to: EntId
    kind: Literal["one_to_one", "one_to_many", "many_to_many"]
    name: NonEmpty


class Entity(Base):
    id: EntId
    name: NonEmpty
    description: str | None = None
    fields: list[EntityField] = Field(default_factory=list)
    relations: list[Relation] = Field(default_factory=list)


class Data(Base):
    """`store` is free-form; the literal value "none" is the explicit opt-out
    for an app with no persistence."""

    store: NonEmpty
    entities: list[Entity] = Field(default_factory=list)


class ConfigEntry(Base):
    id: CfgId
    name: NonEmpty
    type: FieldType
    required: bool = False
    secret: bool = False
    description: str | None = None


class Action(Base):
    id: ActId
    name: NonEmpty
    invokes: InvokeRef | None = None
    writes: list[EntId] = Field(default_factory=list)


class View(Base):
    id: ViewId
    name: NonEmpty
    purpose: NonEmpty
    route: str | None = None
    invocation: str | None = None
    displays: list[DisplayRef] = Field(default_factory=list)
    actions: list[ActId] = Field(default_factory=list)
    navigates_to: list[ViewId] = Field(default_factory=list)
    satisfies: list[AcId] = Field(default_factory=list)


class Ui(Base):
    kind: Literal["web", "cli", "tui", "none"]
    entry: ViewId | None = None
    views: list[View] = Field(default_factory=list)
    actions: list[Action] = Field(default_factory=list)
```

- [ ] **Step 4: Extend `Component` and `Manifest`**

Add to `Component`:

```python
    owns: list[EntId] = Field(default_factory=list)
```

Add to `Manifest`, after `artifacts`:

```python
    stack: Stack | None = None
    data: Data | None = None
    config: list[ConfigEntry] = Field(default_factory=list)
    ui: Ui | None = None
```

- [ ] **Step 5: Run both model test modules**

```bash
uv run pytest tests/unit/test_model_core.py tests/unit/test_model_02.py -v
```

Expected: all PASS. `test_task_touches_accepts_every_02_reference_kind` from Task 3 still passes — `TouchRef` already covered the new prefixes.

- [ ] **Step 6: Commit**

```bash
git add src/avspec/model.py tests/unit/test_model_02.py
git commit -m "feat: add stack, data, config, and ui models for AVSpec 0.2"
```

---

## Task 5: Loading and validation

**Files:**
- Create: `src/avspec/loading.py`, `tests/unit/test_loading.py`, `tests/conftest.py`

**Interfaces:**
- Consumes: `avspec.model.Manifest`, `avspec.findings.Finding`
- Produces:
  - `MANIFEST_NAME: str = "avspec.yaml"`
  - `LoadResult(manifest: Manifest | None, raw: Any | None, findings: list[Finding], fatal: str | None)`
  - `load(spec_dir: Path) -> LoadResult`
  - `read_yaml(path: Path) -> Any`
  - `write_yaml(path: Path, document: Any) -> None`
  - pytest fixture `spec_dir` and helper `write_spec(tmp_path, **overrides) -> Path`

Two representations, deliberately: `manifest` is validated and used by every rule; `raw` is the ruamel round-trip document that Plan 2 mutates so comments survive.

- [ ] **Step 1: Write the shared test fixture**

`tests/conftest.py`:

```python
from __future__ import annotations

from pathlib import Path
from typing import Any

import pytest
from ruamel.yaml import YAML

_yaml = YAML()
_yaml.default_flow_style = False

MINIMAL_SPEC: dict[str, Any] = {
    "avspec": "0.2",
    "metadata": {"name": "linkshort", "status": "draft"},
    "mode": "greenfield",
    "artifacts": {
        "constitution": "constitution.md",
        "requirements": "requirements.md",
        "design": "design.md",
        "tasks": "tasks.md",
    },
}


def write_spec(root: Path, **overrides: Any) -> Path:
    """Write a minimal valid avspec.yaml into `root`, merged with overrides."""
    root.mkdir(parents=True, exist_ok=True)
    document = {**MINIMAL_SPEC, **overrides}
    with (root / "avspec.yaml").open("w", encoding="utf-8") as handle:
        _yaml.dump(document, handle)
    return root


@pytest.fixture
def spec_dir(tmp_path: Path) -> Path:
    return write_spec(tmp_path / "spec")
```

- [ ] **Step 2: Write the failing test**

`tests/unit/test_loading.py`:

```python
from pathlib import Path

from conftest import write_spec

from avspec.loading import load, read_yaml, write_yaml


def test_loads_a_valid_manifest(spec_dir: Path) -> None:
    result = load(spec_dir)
    assert result.fatal is None
    assert result.findings == []
    assert result.manifest is not None
    assert result.manifest.metadata.name == "linkshort"


def test_missing_manifest_is_fatal(tmp_path: Path) -> None:
    result = load(tmp_path)
    assert result.fatal is not None
    assert "avspec.yaml" in result.fatal
    assert result.manifest is None


def test_no_schema_file_is_required(spec_dir: Path) -> None:
    assert not (spec_dir / "avspec.schema.yaml").exists()
    assert load(spec_dir).fatal is None


def test_invalid_manifest_yields_schema_findings_not_an_exception(tmp_path: Path) -> None:
    root = write_spec(tmp_path / "s", metadata={"name": "x", "status": "shipped"})
    result = load(root)
    assert result.manifest is None
    assert [f.code for f in result.findings] == ["SCHEMA"]
    assert "metadata.status" in result.findings[0].message


def test_schema_finding_reports_every_error_not_just_the_first(tmp_path: Path) -> None:
    root = write_spec(tmp_path / "s", metadata={"status": "draft"}, mode="brownfield")
    result = load(root)
    assert len(result.findings) >= 2
    assert all(f.severity == "error" for f in result.findings)


def test_malformed_yaml_is_fatal_not_a_traceback(tmp_path: Path) -> None:
    root = tmp_path / "s"
    root.mkdir()
    (root / "avspec.yaml").write_text("metadata: [unclosed\n", encoding="utf-8")
    result = load(root)
    assert result.fatal is not None
    assert result.manifest is None


def test_round_trip_preserves_comments_and_key_order(tmp_path: Path) -> None:
    path = tmp_path / "doc.yaml"
    original = "# leading note\nb: 2\na: 1  # trailing note\n"
    path.write_text(original, encoding="utf-8")

    document = read_yaml(path)
    document["c"] = 3
    write_yaml(path, document)

    written = path.read_text(encoding="utf-8")
    assert "# leading note" in written
    assert "# trailing note" in written
    assert written.index("b:") < written.index("a:")
```

`test_round_trip_preserves_comments_and_key_order` is the reason PyYAML is banned. If it fails, the interview in Plan 2 will silently destroy a user's hand-written comments on every answer.

- [ ] **Step 3: Run to verify it fails**

```bash
uv run pytest tests/unit/test_loading.py -v
```

Expected: FAIL — `ModuleNotFoundError: No module named 'avspec.loading'`.

- [ ] **Step 4: Implement**

`src/avspec/loading.py`:

```python
"""Read, validate, and write spec manifests.

Two representations are kept on purpose:

* `manifest` — a validated `Manifest`, what every analyzer rule reads.
* `raw`      — the ruamel round-trip document, what the interview mutates so
  a user's comments and key order survive being rewritten.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from pydantic import ValidationError
from ruamel.yaml import YAML
from ruamel.yaml.error import YAMLError

from avspec.findings import Finding, error
from avspec.model import Manifest

MANIFEST_NAME = "avspec.yaml"

_yaml = YAML()
_yaml.preserve_quotes = True
_yaml.default_flow_style = False
_yaml.width = 120


def read_yaml(path: Path) -> Any:
    with path.open(encoding="utf-8") as handle:
        return _yaml.load(handle)


def write_yaml(path: Path, document: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w", encoding="utf-8") as handle:
        _yaml.dump(document, handle)


@dataclass
class LoadResult:
    manifest: Manifest | None = None
    raw: Any | None = None
    findings: list[Finding] = field(default_factory=list)
    fatal: str | None = None


def _location(err: dict[str, Any]) -> str:
    return ".".join(str(part) for part in err["loc"]) or "/"


def load(spec_dir: Path) -> LoadResult:
    """Load and validate `<spec_dir>/avspec.yaml`.

    A fatal result means nothing else can run. A SCHEMA finding means the file
    parsed as YAML but is not a valid manifest — the graph rules are skipped
    because they would report noise on a shape they cannot trust.
    """
    path = Path(spec_dir) / MANIFEST_NAME
    if not path.is_file():
        return LoadResult(fatal=f"missing manifest: {path}")

    try:
        raw = read_yaml(path)
    except YAMLError as exc:
        return LoadResult(fatal=f"{path} is not valid YAML: {exc}")

    if not isinstance(raw, dict):
        return LoadResult(fatal=f"{path} must contain a mapping at the top level")

    try:
        manifest = Manifest.model_validate(raw)
    except ValidationError as exc:
        findings = [
            error("SCHEMA", f"{_location(err)}: {err['msg']}", _location(err))
            for err in exc.errors()
        ]
        return LoadResult(raw=raw, findings=findings)

    return LoadResult(manifest=manifest, raw=raw)
```

- [ ] **Step 5: Run to verify it passes**

```bash
uv run pytest tests/unit/test_loading.py -v
```

Expected: all seven PASS. If `from conftest import write_spec` fails to resolve, confirm `[tool.pytest.ini_options] testpaths = ["tests"]` is present and run pytest from the repo root.

- [ ] **Step 6: Commit**

```bash
git add src/avspec/loading.py tests/unit/test_loading.py tests/conftest.py
git commit -m "feat: add round-trip manifest loading with Pydantic validation"
```

---

## Task 6: Analyzer skeleton and rule registry

**Files:**
- Create: `src/avspec/analysis.py`, `src/avspec/rules/__init__.py`, `tests/unit/test_analysis.py`

**Interfaces:**
- Consumes: `loading.load`, `findings.ordered`
- Produces:
  - `Spec(dir: Path, manifest: Manifest, raw: Any)` with helper `Spec.path(rel: str) -> Path` and `Spec.exists(rel: str) -> bool`
  - `Report(fatal, status, strict, passed, counts, findings, manifest)`
  - `analyze(spec_dir: Path) -> Report`
  - `avspec.rules.RULES: list[Rule]`, `@rule` decorator, `Rule = Callable[[Spec], Iterable[Finding]]`

Every later task registers rules against this registry. `rules/__init__.py` must import each rule submodule explicitly — an unimported module registers nothing and fails silently.

- [ ] **Step 1: Write the failing test**

`tests/unit/test_analysis.py`:

```python
from pathlib import Path

from conftest import write_spec

from avspec import rules
from avspec.analysis import Spec, analyze
from avspec.findings import Finding


def test_fatal_when_no_manifest(tmp_path: Path) -> None:
    report = analyze(tmp_path)
    assert report.fatal is not None
    assert report.passed is False
    assert report.findings == []


def test_draft_tolerates_todos(spec_dir: Path, monkeypatch) -> None:
    monkeypatch.setattr(
        rules, "RULES", [lambda _s: [Finding("todo", "NO_STACK", "no stack", question="q")]]
    )
    report = analyze(spec_dir)
    assert report.status == "draft"
    assert report.strict is False
    assert report.counts["todo"] == 1
    assert report.passed is True


def test_ready_fails_on_todos(tmp_path: Path, monkeypatch) -> None:
    root = write_spec(tmp_path / "s", metadata={"name": "x", "status": "ready"})
    monkeypatch.setattr(
        rules, "RULES", [lambda _s: [Finding("todo", "NO_STACK", "no stack", question="q")]]
    )
    report = analyze(root)
    assert report.strict is True
    assert report.passed is False


def test_errors_fail_even_in_draft(spec_dir: Path, monkeypatch) -> None:
    monkeypatch.setattr(rules, "RULES", [lambda _s: [Finding("error", "DUP_ID", "dup")]])
    assert analyze(spec_dir).passed is False


def test_warnings_never_fail(spec_dir: Path, monkeypatch) -> None:
    monkeypatch.setattr(rules, "RULES", [lambda _s: [Finding("warn", "MIRROR", "m")]])
    report = analyze(spec_dir)
    assert report.counts["warn"] == 1
    assert report.passed is True


def test_schema_errors_short_circuit_the_rules(tmp_path: Path, monkeypatch) -> None:
    root = write_spec(tmp_path / "s", metadata={"name": "x", "status": "bogus"})

    def _boom(_spec: Spec) -> list[Finding]:
        raise AssertionError("rules must not run on an invalid manifest")

    monkeypatch.setattr(rules, "RULES", [_boom])
    report = analyze(root)
    assert [f.code for f in report.findings] == ["SCHEMA"]
    assert report.passed is False


def test_findings_come back_in_authoring_order(spec_dir: Path, monkeypatch) -> None:
    monkeypatch.setattr(
        rules,
        "RULES",
        [
            lambda _s: [Finding("todo", "NO_TASKS", "t", question="q")],
            lambda _s: [Finding("error", "DUP_ID", "d")],
            lambda _s: [Finding("todo", "NO_STACK", "s", question="q")],
        ],
    )
    assert [f.code for f in analyze(spec_dir).findings] == ["DUP_ID", "NO_STACK", "NO_TASKS"]


def test_spec_path_helpers_are_relative_to_the_spec_dir(spec_dir: Path) -> None:
    (spec_dir / "design.md").write_text("hi", encoding="utf-8")
    report = analyze(spec_dir)
    spec = Spec(dir=spec_dir, manifest=report.manifest, raw=None)
    assert spec.exists("design.md") is True
    assert spec.exists("nope.md") is False
    assert spec.path("design.md") == spec_dir / "design.md"
```

Rename `test_findings_come back_in_authoring_order` to `test_findings_come_back_in_authoring_order` when typing it — the space is not valid Python.

- [ ] **Step 2: Run to verify it fails**

```bash
uv run pytest tests/unit/test_analysis.py -v
```

Expected: FAIL — `ModuleNotFoundError: No module named 'avspec.analysis'`.

- [ ] **Step 3: Implement the registry**

`src/avspec/rules/__init__.py`:

```python
"""Rule registry.

Every rule is a pure function `(Spec) -> Iterable[Finding]`. Import each rule
submodule below — a module that is not imported here registers nothing and
fails silently.
"""

from __future__ import annotations

from collections.abc import Callable, Iterable
from typing import TYPE_CHECKING

from avspec.findings import Finding

if TYPE_CHECKING:
    from avspec.analysis import Spec

Rule = Callable[["Spec"], Iterable[Finding]]

RULES: list[Rule] = []


def rule(fn: Rule) -> Rule:
    RULES.append(fn)
    return fn
```

Leave the submodule imports out for now; each later task appends its own line at the bottom of this file.

- [ ] **Step 4: Implement the analyzer**

`src/avspec/analysis.py`:

```python
"""Run the rule registry over a spec directory."""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
from typing import Any

from avspec import rules as rules_module
from avspec.findings import Finding, ordered
from avspec.loading import load
from avspec.model import Manifest

STRICT_STATUSES = frozenset({"ready", "built"})


@dataclass
class Spec:
    """What a rule receives: the validated manifest plus its location on disk."""

    dir: Path
    manifest: Manifest
    raw: Any

    def path(self, rel: str) -> Path:
        return self.dir / rel

    def exists(self, rel: str) -> bool:
        return self.path(rel).exists()

    def read(self, rel: str) -> str:
        return self.path(rel).read_text(encoding="utf-8")


@dataclass
class Report:
    fatal: str | None
    status: str | None
    strict: bool
    passed: bool
    counts: dict[str, int]
    findings: list[Finding]
    manifest: Manifest | None


def _counts(findings: list[Finding]) -> dict[str, int]:
    return {
        severity: sum(1 for f in findings if f.severity == severity)
        for severity in ("error", "todo", "warn")
    }


def _report(
    findings: list[Finding], status: str | None, manifest: Manifest | None
) -> Report:
    strict = status in STRICT_STATUSES
    counts = _counts(findings)
    passed = counts["error"] + (counts["todo"] if strict else 0) == 0
    return Report(
        fatal=None,
        status=status,
        strict=strict,
        passed=passed,
        counts=counts,
        findings=ordered(findings),
        manifest=manifest,
    )


def analyze(spec_dir: Path) -> Report:
    """Load a spec and run every registered rule against it."""
    result = load(Path(spec_dir))

    if result.fatal is not None:
        return Report(
            fatal=result.fatal,
            status=None,
            strict=False,
            passed=False,
            counts=_counts([]),
            findings=[],
            manifest=None,
        )

    if result.manifest is None:
        # The shape is invalid. Graph rules would report noise on a manifest
        # they cannot trust, so stop at the schema errors.
        status = (result.raw or {}).get("metadata", {}).get("status")
        return _report(result.findings, status if isinstance(status, str) else None, None)

    spec = Spec(dir=Path(spec_dir), manifest=result.manifest, raw=result.raw)
    findings = [f for fn in rules_module.RULES for f in fn(spec)]
    return _report(findings, result.manifest.metadata.status, result.manifest)
```

`analyze` reads `rules_module.RULES` through the module object rather than importing the list directly, so the `monkeypatch.setattr(rules, "RULES", ...)` in the tests takes effect.

- [ ] **Step 5: Run to verify it passes**

```bash
uv run pytest tests/unit/test_analysis.py -v
```

Expected: all eight PASS.

- [ ] **Step 6: Commit**

```bash
git add src/avspec/analysis.py src/avspec/rules/__init__.py tests/unit/test_analysis.py
git commit -m "feat: add rule registry and analyzer with draft/ready severity gating"
```

---

## Task 7: Well-formedness rules

**Files:**
- Create: `src/avspec/rules/wellformed.py`, `tests/unit/test_rules_wellformed.py`
- Modify: `src/avspec/rules/__init__.py`

**Interfaces:**
- Consumes: `Spec`, `rule`, `error`
- Produces: rules emitting `DUP_ID`, `DANGLING`, `CYCLE`, `ENT_MULTI_OWNER`, `DISPLAY_UNKNOWN_FIELD`

All five are errors: they fail even in `draft`, because they describe a spec that contradicts itself rather than one that is merely unfinished.

- [ ] **Step 1: Write the failing test**

`tests/unit/test_rules_wellformed.py`:

```python
from pathlib import Path

from conftest import write_spec

from avspec.analysis import analyze


def codes(root: Path) -> list[str]:
    return [f.code for f in analyze(root).findings]


def test_duplicate_requirement_ids_are_an_error(tmp_path: Path) -> None:
    root = write_spec(
        tmp_path / "s",
        requirements=[{"id": "REQ-001", "title": "a"}, {"id": "REQ-001", "title": "b"}],
    )
    assert "DUP_ID" in codes(root)


def test_duplicate_acceptance_ids_across_requirements_are_an_error(tmp_path: Path) -> None:
    root = write_spec(
        tmp_path / "s",
        requirements=[
            {"id": "REQ-001", "title": "a", "acceptance": [{"id": "AC-001", "ears": "e"}]},
            {"id": "REQ-002", "title": "b", "acceptance": [{"id": "AC-001", "ears": "e"}]},
        ],
    )
    assert "DUP_ID" in codes(root)


def test_task_satisfying_an_unknown_ac_is_dangling(tmp_path: Path) -> None:
    root = write_spec(tmp_path / "s", tasks=[{"id": "TSK-001", "title": "t", "satisfies": ["AC-999"]}])
    assert "DANGLING" in codes(root)


def test_task_touching_an_unknown_entity_is_dangling(tmp_path: Path) -> None:
    root = write_spec(
        tmp_path / "s",
        requirements=[{"id": "REQ-001", "title": "a", "acceptance": [{"id": "AC-001", "ears": "e"}]}],
        tasks=[{"id": "TSK-001", "title": "t", "satisfies": ["AC-001"], "touches": ["ENT-404"]}],
    )
    assert "DANGLING" in codes(root)


def test_interface_referencing_an_unknown_contract_is_dangling(tmp_path: Path) -> None:
    root = write_spec(
        tmp_path / "s",
        components=[
            {"id": "CMP-001", "responsibility": "r", "interfaces": [{"name": "http", "contract": "CTR-404"}]}
        ],
    )
    assert "DANGLING" in codes(root)


def test_component_dependency_cycle_is_an_error(tmp_path: Path) -> None:
    root = write_spec(
        tmp_path / "s",
        components=[
            {"id": "CMP-001", "responsibility": "r", "depends_on": ["CMP-002"]},
            {"id": "CMP-002", "responsibility": "r", "depends_on": ["CMP-001"]},
        ],
    )
    assert "CYCLE" in codes(root)


def test_task_dependency_cycle_is_an_error(tmp_path: Path) -> None:
    root = write_spec(
        tmp_path / "s",
        requirements=[{"id": "REQ-001", "title": "a", "acceptance": [{"id": "AC-001", "ears": "e"}]}],
        tasks=[
            {"id": "TSK-001", "title": "a", "satisfies": ["AC-001"], "depends_on": ["TSK-002"]},
            {"id": "TSK-002", "title": "b", "satisfies": ["AC-001"], "depends_on": ["TSK-001"]},
        ],
    )
    assert "CYCLE" in codes(root)


def test_an_acyclic_diamond_is_not_reported(tmp_path: Path) -> None:
    root = write_spec(
        tmp_path / "s",
        components=[
            {"id": "CMP-001", "responsibility": "r", "depends_on": ["CMP-002", "CMP-003"]},
            {"id": "CMP-002", "responsibility": "r", "depends_on": ["CMP-004"]},
            {"id": "CMP-003", "responsibility": "r", "depends_on": ["CMP-004"]},
            {"id": "CMP-004", "responsibility": "r"},
        ],
    )
    assert "CYCLE" not in codes(root)


def test_entity_owned_by_two_components_is_an_error(tmp_path: Path) -> None:
    root = write_spec(
        tmp_path / "s",
        data={"store": "sqlite", "entities": [{"id": "ENT-001", "name": "Link"}]},
        components=[
            {"id": "CMP-001", "responsibility": "r", "owns": ["ENT-001"]},
            {"id": "CMP-002", "responsibility": "r", "owns": ["ENT-001"]},
        ],
    )
    found = [f for f in analyze(root).findings if f.code == "ENT_MULTI_OWNER"]
    assert found and "ENT-001" in found[0].ids


def test_view_displaying_an_unknown_field_is_an_error(tmp_path: Path) -> None:
    root = write_spec(
        tmp_path / "s",
        data={
            "store": "sqlite",
            "entities": [{"id": "ENT-001", "name": "Link", "fields": [{"name": "code", "type": "string"}]}],
        },
        ui={
            "kind": "web",
            "entry": "VIEW-001",
            "views": [{"id": "VIEW-001", "name": "V", "purpose": "p", "displays": ["ENT-001.nope"]}],
        },
    )
    assert "DISPLAY_UNKNOWN_FIELD" in codes(root)


def test_view_displaying_a_known_field_is_clean(tmp_path: Path) -> None:
    root = write_spec(
        tmp_path / "s",
        data={
            "store": "sqlite",
            "entities": [{"id": "ENT-001", "name": "Link", "fields": [{"name": "code", "type": "string"}]}],
        },
        ui={
            "kind": "web",
            "entry": "VIEW-001",
            "views": [{"id": "VIEW-001", "name": "V", "purpose": "p", "displays": ["ENT-001.code"]}],
        },
    )
    assert "DISPLAY_UNKNOWN_FIELD" not in codes(root)
```

- [ ] **Step 2: Run to verify it fails**

```bash
uv run pytest tests/unit/test_rules_wellformed.py -v
```

Expected: FAIL — every assertion looking for a code finds an empty list, because no rules are registered yet.

- [ ] **Step 3: Implement**

`src/avspec/rules/wellformed.py`:

```python
"""Well-formedness rules. All errors: the spec contradicts itself."""

from __future__ import annotations

from collections.abc import Iterable, Iterator

from avspec.analysis import Spec
from avspec.findings import Finding, error
from avspec.rules import rule


def _acceptance(spec: Spec) -> Iterator[tuple[str, str]]:
    """Yield (acceptance id, owning requirement id)."""
    for req in spec.manifest.requirements:
        for ac in req.acceptance:
            yield ac.id, req.id


@rule
def duplicate_ids(spec: Spec) -> Iterable[Finding]:
    m = spec.manifest
    groups: list[tuple[str, list[str]]] = [
        ("requirement", [r.id for r in m.requirements]),
        ("acceptance criterion", [ac for ac, _ in _acceptance(spec)]),
        ("contract", [c.id for c in m.contracts]),
        ("component", [c.id for c in m.components]),
        ("decision", [d.id for d in m.decisions]),
        ("task", [t.id for t in m.tasks]),
        ("principle", [p.id for p in m.principles]),
        ("constraint", [c.id for c in m.constraints]),
        ("config entry", [c.id for c in m.config]),
        ("entity", [e.id for e in (m.data.entities if m.data else [])]),
        ("view", [v.id for v in (m.ui.views if m.ui else [])]),
        ("action", [a.id for a in (m.ui.actions if m.ui else [])]),
    ]
    for label, ids in groups:
        seen: set[str] = set()
        for identifier in ids:
            if identifier in seen:
                yield error("DUP_ID", f"duplicate {label} id: {identifier}", identifier)
            seen.add(identifier)


@rule
def dangling_references(spec: Spec) -> Iterable[Finding]:
    m = spec.manifest
    acceptance = {ac for ac, _ in _acceptance(spec)}
    components = {c.id for c in m.components}
    contracts = {c.id for c in m.contracts}
    tasks = {t.id for t in m.tasks}
    entities = {e.id for e in (m.data.entities if m.data else [])}
    config = {c.id for c in m.config}
    views = {v.id for v in (m.ui.views if m.ui else [])}
    actions = {a.id for a in (m.ui.actions if m.ui else [])}

    known: dict[str, set[str]] = {
        "AC": acceptance,
        "CMP": components,
        "CTR": contracts,
        "TSK": tasks,
        "ENT": entities,
        "CFG": config,
        "VIEW": views,
        "ACT": actions,
    }

    def check(owner: str, ref: str, what: str) -> Finding | None:
        prefix = ref.split("-", 1)[0]
        if ref not in known[prefix]:
            return error("DANGLING", f"{owner} {what} unknown {ref}", owner, ref)
        return None

    for task in m.tasks:
        for ref in task.satisfies:
            if found := check(task.id, ref, "satisfies"):
                yield found
        for ref in task.touches:
            if found := check(task.id, ref, "touches"):
                yield found
        for ref in task.depends_on:
            if found := check(task.id, ref, "depends_on"):
                yield found

    for component in m.components:
        for ref in component.depends_on:
            if found := check(component.id, ref, "depends_on"):
                yield found
        for ref in component.owns:
            if found := check(component.id, ref, "owns"):
                yield found
        for interface in component.interfaces:
            if interface.contract not in contracts:
                yield error(
                    "DANGLING",
                    f"{component.id}.{interface.name} references unknown {interface.contract}",
                    component.id,
                    interface.contract,
                )

    for entity in m.data.entities if m.data else []:
        for relation in entity.relations:
            if found := check(entity.id, relation.to, f"relation {relation.name} targets"):
                yield found

    if m.ui is not None:
        if m.ui.entry is not None and m.ui.entry not in views:
            yield error("DANGLING", f"ui.entry names unknown {m.ui.entry}", m.ui.entry)
        for view in m.ui.views:
            for ref in view.actions:
                if found := check(view.id, ref, "action"):
                    yield found
            for ref in view.navigates_to:
                if found := check(view.id, ref, "navigates_to"):
                    yield found
            for ref in view.satisfies:
                if found := check(view.id, ref, "satisfies"):
                    yield found
        for action in m.ui.actions:
            for ref in action.writes:
                if found := check(action.id, ref, "writes"):
                    yield found


def _cycles(kind: str, graph: dict[str, list[str]]) -> Iterator[Finding]:
    """Iterative DFS. `visiting` holds the current path; a back-edge into it is
    a cycle."""
    state: dict[str, int] = {}  # 0 unvisited, 1 visiting, 2 done
    reported: set[frozenset[str]] = set()

    def walk(node: str, path: list[str]) -> Iterator[Finding]:
        if state.get(node) == 2:
            return
        if state.get(node) == 1:
            cycle = path[path.index(node) :]
            key = frozenset(cycle)
            if key not in reported:
                reported.add(key)
                yield error(
                    "CYCLE",
                    f"{kind} dependency cycle: {' -> '.join([*cycle, node])}",
                    *cycle,
                )
            return
        state[node] = 1
        for neighbour in graph.get(node, []):
            if neighbour in graph:
                yield from walk(neighbour, [*path, node])
        state[node] = 2

    for start in graph:
        yield from walk(start, [])


@rule
def cycles(spec: Spec) -> Iterable[Finding]:
    m = spec.manifest
    yield from _cycles("component", {c.id: list(c.depends_on) for c in m.components})
    yield from _cycles("task", {t.id: list(t.depends_on) for t in m.tasks})


@rule
def entity_single_owner(spec: Spec) -> Iterable[Finding]:
    owners: dict[str, list[str]] = {}
    for component in spec.manifest.components:
        for entity_id in component.owns:
            owners.setdefault(entity_id, []).append(component.id)
    for entity_id, holders in owners.items():
        if len(holders) > 1:
            yield error(
                "ENT_MULTI_OWNER",
                f"{entity_id} is owned by more than one component: {', '.join(holders)}",
                entity_id,
                *holders,
            )


@rule
def display_bindings_resolve(spec: Spec) -> Iterable[Finding]:
    m = spec.manifest
    if m.ui is None:
        return
    fields: dict[str, set[str]] = {
        e.id: {f.name for f in e.fields} for e in (m.data.entities if m.data else [])
    }
    for view in m.ui.views:
        for ref in view.displays:
            entity_id, _, field_name = ref.partition(".")
            if entity_id not in fields:
                continue  # already reported by dangling_references
            if field_name not in fields[entity_id]:
                yield error(
                    "DISPLAY_UNKNOWN_FIELD",
                    f"{view.id} displays {ref}, but {entity_id} has no field {field_name!r}",
                    view.id,
                    entity_id,
                )
```

`display_bindings_resolve` skips unknown entities so a single typo yields one finding, not two.

- [ ] **Step 4: Register the module**

Append to `src/avspec/rules/__init__.py`:

```python
from avspec.rules import wellformed  # noqa: E402,F401  (registration side effect)
```

Add `__all__ = ["RULES", "Rule", "rule"]` above that import so ruff's unused-import rule stays satisfied.

- [ ] **Step 5: Run to verify it passes**

```bash
uv run pytest tests/unit/test_rules_wellformed.py -v
```

Expected: all eleven PASS.

- [ ] **Step 6: Run the whole suite — the registry is now global state**

```bash
uv run pytest -v
```

Expected: all PASS. `tests/unit/test_analysis.py` monkeypatches `RULES`, so real rules cannot leak into it.

- [ ] **Step 7: Commit**

```bash
git add src/avspec/rules/wellformed.py src/avspec/rules/__init__.py tests/unit/test_rules_wellformed.py
git commit -m "feat: add well-formedness rules for ids, references, cycles, and bindings"
```

---

## Task 8: Completeness rules

**Files:**
- Create: `src/avspec/rules/completeness.py`, `tests/unit/test_rules_completeness.py`
- Modify: `src/avspec/rules/__init__.py`

**Interfaces:**
- Consumes: `Spec`, `rule`, `todo`
- Produces: rules emitting `NO_STACK`, `STACK_INCOMPLETE`, `NO_PRINCIPLES`, `NO_REQUIREMENTS`, `REQ_NO_AC`, `DATA_UNDECLARED`, `NO_COMPONENTS`, `CMP_NO_CONTRACT`, `ENT_UNOWNED`, `NO_TASKS`, `AC_UNSATISFIED`, `AC_NO_OWNER`, `AC_NO_TEST`

This is the task that fixes the root defect: **todos fire on absence.** Every finding here carries a `question`, because the interview in Plan 2 uses it verbatim.

- [ ] **Step 1: Write the failing test**

`tests/unit/test_rules_completeness.py`:

```python
from pathlib import Path

from conftest import write_spec

from avspec.analysis import analyze

FULL_STACK = {
    "languages": [{"name": "python", "version": "3.12"}],
    "package_manager": "uv",
    "commands": {"install": "uv sync", "test": "uv run pytest", "arch": "uv run lint-imports"},
}


def codes(root: Path) -> list[str]:
    return [f.code for f in analyze(root).findings]


def test_empty_spec_reports_absence_not_silence(tmp_path: Path) -> None:
    """The 0.1 defect: a spec with nothing in it emitted no todos at all."""
    found = codes(write_spec(tmp_path / "s"))
    for expected in ("NO_STACK", "NO_PRINCIPLES", "NO_REQUIREMENTS", "DATA_UNDECLARED", "NO_COMPONENTS", "NO_TASKS"):
        assert expected in found


def test_every_todo_carries_a_question(tmp_path: Path) -> None:
    for finding in analyze(write_spec(tmp_path / "s")).findings:
        if finding.severity == "todo":
            assert finding.question, f"{finding.code} has no question"


def test_stack_without_commands_test_is_incomplete(tmp_path: Path) -> None:
    root = write_spec(tmp_path / "s", stack={"languages": [{"name": "go"}]})
    assert "NO_STACK" not in codes(root)
    assert "STACK_INCOMPLETE" in codes(root)


def test_complete_stack_is_clean(tmp_path: Path) -> None:
    found = codes(write_spec(tmp_path / "s", stack=FULL_STACK))
    assert "NO_STACK" not in found and "STACK_INCOMPLETE" not in found


def test_data_declared_as_none_closes_the_todo(tmp_path: Path) -> None:
    assert "DATA_UNDECLARED" not in codes(write_spec(tmp_path / "s", data={"store": "none"}))


def test_requirement_without_acceptance_is_reported(tmp_path: Path) -> None:
    root = write_spec(tmp_path / "s", requirements=[{"id": "REQ-001", "title": "Shorten"}])
    assert "NO_REQUIREMENTS" not in codes(root)
    assert "REQ_NO_AC" in codes(root)


def test_acceptance_with_no_task_and_no_test_is_reported(tmp_path: Path) -> None:
    root = write_spec(
        tmp_path / "s",
        requirements=[{"id": "REQ-001", "title": "S", "acceptance": [{"id": "AC-001", "ears": "e"}]}],
    )
    found = codes(root)
    assert "AC_UNSATISFIED" in found
    assert "AC_NO_TEST" in found


def test_ac_owned_by_a_component_via_its_task_is_clean(tmp_path: Path) -> None:
    root = write_spec(
        tmp_path / "s",
        requirements=[{"id": "REQ-001", "title": "S", "acceptance": [{"id": "AC-001", "ears": "e"}]}],
        components=[{"id": "CMP-001", "responsibility": "r"}],
        tasks=[{"id": "TSK-001", "title": "t", "satisfies": ["AC-001"], "touches": ["CMP-001"]}],
    )
    assert "AC_NO_OWNER" not in codes(root)


def test_ac_owned_by_a_view_alone_is_clean(tmp_path: Path) -> None:
    """UI-only criteria must not demand a backend component that needn't exist."""
    root = write_spec(
        tmp_path / "s",
        requirements=[{"id": "REQ-001", "title": "S", "acceptance": [{"id": "AC-001", "ears": "e"}]}],
        ui={
            "kind": "web",
            "entry": "VIEW-001",
            "views": [{"id": "VIEW-001", "name": "V", "purpose": "p", "satisfies": ["AC-001"]}],
        },
        tasks=[{"id": "TSK-001", "title": "t", "satisfies": ["AC-001"]}],
    )
    assert "AC_NO_OWNER" not in codes(root)


def test_ac_with_a_task_touching_nothing_has_no_owner(tmp_path: Path) -> None:
    root = write_spec(
        tmp_path / "s",
        requirements=[{"id": "REQ-001", "title": "S", "acceptance": [{"id": "AC-001", "ears": "e"}]}],
        tasks=[{"id": "TSK-001", "title": "t", "satisfies": ["AC-001"]}],
    )
    assert "AC_NO_OWNER" in codes(root)


def test_entity_owned_by_no_component_is_reported(tmp_path: Path) -> None:
    root = write_spec(
        tmp_path / "s",
        data={"store": "sqlite", "entities": [{"id": "ENT-001", "name": "Link"}]},
        components=[{"id": "CMP-001", "responsibility": "r"}],
    )
    assert "ENT_UNOWNED" in codes(root)


def test_depended_on_component_without_an_interface_is_reported(tmp_path: Path) -> None:
    root = write_spec(
        tmp_path / "s",
        contracts=[{"id": "CTR-001", "type": "openapi", "path": "contracts/api.yaml"}],
        components=[
            {"id": "CMP-001", "responsibility": "api", "depends_on": ["CMP-002"]},
            {"id": "CMP-002", "responsibility": "store"},
        ],
    )
    found = [f for f in analyze(root).findings if f.code == "CMP_NO_CONTRACT"]
    assert found and found[0].ids == ("CMP-002",)


def test_leaf_component_nothing_depends_on_is_exempt(tmp_path: Path) -> None:
    root = write_spec(tmp_path / "s", components=[{"id": "CMP-001", "responsibility": "r"}])
    assert "CMP_NO_CONTRACT" not in codes(root)
```

- [ ] **Step 2: Run to verify it fails**

```bash
uv run pytest tests/unit/test_rules_completeness.py -v
```

Expected: FAIL — `ModuleNotFoundError: No module named 'avspec.rules.completeness'`.

- [ ] **Step 3: Implement**

`src/avspec/rules/completeness.py`:

```python
"""Completeness rules — todos that fire on ABSENCE, not only on breakage.

This is the core fix over 0.1, where a spec containing nothing produced no
findings and could reach `status: ready`. Every todo carries the question the
interview asks to close it.
"""

from __future__ import annotations

from collections.abc import Iterable

from avspec.analysis import Spec
from avspec.findings import Finding, todo
from avspec.rules import rule


@rule
def stack_declared(spec: Spec) -> Iterable[Finding]:
    stack = spec.manifest.stack
    if stack is None or not stack.languages:
        yield todo(
            "NO_STACK",
            "no stack declared",
            question=(
                "What is this built with? Give the language and version — without it an "
                "agent picks the framework for you."
            ),
        )
        return

    missing = [
        name
        for name, value in (
            ("commands.install", stack.commands.install if stack.commands else None),
            ("commands.test", stack.commands.test if stack.commands else None),
        )
        if not value
    ]
    if missing:
        yield todo(
            "STACK_INCOMPLETE",
            f"stack is missing: {', '.join(missing)}",
            question=(
                "How is this project installed and tested? Give the exact shell commands "
                f"for {', '.join(missing)} — the conformance gate runs what you declare."
            ),
        )


@rule
def principles_declared(spec: Spec) -> Iterable[Finding]:
    if not spec.manifest.principles:
        yield todo(
            "NO_PRINCIPLES",
            "constitution declares no principles",
            question=(
                "What rules must every part of this system honour? State the first "
                "principle (PR-*) — these become gates, not suggestions."
            ),
        )


@rule
def requirements_declared(spec: Spec) -> Iterable[Finding]:
    m = spec.manifest
    if not m.requirements:
        yield todo(
            "NO_REQUIREMENTS",
            "no requirements defined",
            question="What must the system do? State the first requirement — a WHAT and WHY, no implementation.",
        )
        return
    for req in m.requirements:
        if not req.acceptance:
            yield todo(
                "REQ_NO_AC",
                f"{req.id} has no acceptance criteria",
                req.id,
                question=(
                    f"Define at least one acceptance criterion (EARS) for {req.id} — "
                    f'"{req.title}". What observable behaviour proves it is done?'
                ),
            )


@rule
def data_declared(spec: Spec) -> Iterable[Finding]:
    if spec.manifest.data is None:
        yield todo(
            "DATA_UNDECLARED",
            "no data section — declare entities, or store: none",
            question=(
                "What does this system persist? Declare the entities and their store. "
                'If it persists nothing, set store: "none" explicitly.'
            ),
        )


@rule
def components_declared(spec: Spec) -> Iterable[Finding]:
    m = spec.manifest
    if not m.components:
        yield todo(
            "NO_COMPONENTS",
            "no components defined",
            question=(
                "What are the parts of this system? Name the first component (CMP-*) "
                "and the one thing it is responsible for."
            ),
        )
        return

    depended_on = {dep for c in m.components for dep in c.depends_on}
    for component in m.components:
        if component.id in depended_on and not component.interfaces:
            yield todo(
                "CMP_NO_CONTRACT",
                f"{component.id} is depended on but exposes no interface",
                component.id,
                question=(
                    f"Other components depend on {component.id}. What interface does it "
                    "expose, and which contract (CTR-*) defines it?"
                ),
            )


@rule
def entities_owned(spec: Spec) -> Iterable[Finding]:
    m = spec.manifest
    if m.data is None or not m.components:
        return  # covered by data_declared / components_declared
    owned = {entity_id for c in m.components for entity_id in c.owns}
    for entity in m.data.entities:
        if entity.id not in owned:
            yield todo(
                "ENT_UNOWNED",
                f"{entity.id} ({entity.name}) is owned by no component",
                entity.id,
                question=(
                    f"Which component owns {entity.id} ({entity.name})? Exactly one "
                    "component must be accountable for each entity."
                ),
            )


@rule
def tasks_declared(spec: Spec) -> Iterable[Finding]:
    if not spec.manifest.tasks:
        yield todo(
            "NO_TASKS",
            "no build tasks defined",
            question="What is the build order? Add the first task (TSK-*) and the acceptance criterion it delivers.",
        )


@rule
def acceptance_is_deliverable(spec: Spec) -> Iterable[Finding]:
    m = spec.manifest
    satisfied = {ac for task in m.tasks for ac in task.satisfies}
    view_satisfied = {ac for view in (m.ui.views if m.ui else []) for ac in view.satisfies}

    owners: dict[str, set[str]] = {}
    for task in m.tasks:
        components = {ref for ref in task.touches if ref.startswith("CMP-")}
        for ac in task.satisfies:
            owners.setdefault(ac, set()).update(components)

    for req in m.requirements:
        for ac in req.acceptance:
            if ac.id not in satisfied:
                yield todo(
                    "AC_UNSATISFIED",
                    f"{ac.id} (in {req.id}) is satisfied by no task",
                    ac.id,
                    question=f'Which build task delivers {ac.id} ("{ac.ears}")? Add a TSK-* that satisfies it.',
                )
            if not owners.get(ac.id) and ac.id not in view_satisfied:
                yield todo(
                    "AC_NO_OWNER",
                    f"{ac.id} traces to no component or view",
                    ac.id,
                    question=(
                        f"Which component or view is accountable for {ac.id}? Add a CMP-* "
                        "to the task's `touches`, or have a VIEW-* satisfy it."
                    ),
                )
            if not ac.test:
                yield todo(
                    "AC_NO_TEST",
                    f"{ac.id} has no test mapping",
                    ac.id,
                    question=(
                        f"How is {ac.id} proven automatically? Give a .feature scenario "
                        f"reference (verification/acceptance/{req.id}.feature#<scenario>) or a test path."
                    ),
                )
```

- [ ] **Step 4: Register the module**

Append to `src/avspec/rules/__init__.py`:

```python
from avspec.rules import completeness  # noqa: E402,F401  (registration side effect)
```

- [ ] **Step 5: Run to verify it passes**

```bash
uv run pytest tests/unit/test_rules_completeness.py -v
```

Expected: all thirteen PASS.

- [ ] **Step 6: Commit**

```bash
git add src/avspec/rules/completeness.py src/avspec/rules/__init__.py tests/unit/test_rules_completeness.py
git commit -m "feat: add completeness rules that fire on absence"
```

---

## Task 9: UI rules

**Files:**
- Create: `src/avspec/rules/ui.py`, `tests/unit/test_rules_ui.py`
- Modify: `src/avspec/rules/__init__.py`

**Interfaces:**
- Consumes: `Spec`, `rule`, `todo`
- Produces: rules emitting `UI_UNDECLARED`, `NO_VIEWS`, `NO_ENTRY`, `VIEW_NO_AC`, `VIEW_UNREACHABLE`, `ACTION_ORPHAN`

- [ ] **Step 1: Write the failing test**

`tests/unit/test_rules_ui.py`:

```python
from pathlib import Path

from conftest import write_spec

from avspec.analysis import analyze


def codes(root: Path) -> list[str]:
    return [f.code for f in analyze(root).findings]


def test_absent_ui_section_is_reported(tmp_path: Path) -> None:
    assert "UI_UNDECLARED" in codes(write_spec(tmp_path / "s"))


def test_kind_none_closes_the_todo_and_suppresses_the_rest(tmp_path: Path) -> None:
    found = codes(write_spec(tmp_path / "s", ui={"kind": "none"}))
    assert "UI_UNDECLARED" not in found
    assert "NO_VIEWS" not in found
    assert "NO_ENTRY" not in found


def test_web_ui_with_no_views_is_reported(tmp_path: Path) -> None:
    found = codes(write_spec(tmp_path / "s", ui={"kind": "web"}))
    assert "NO_VIEWS" in found


def test_views_without_an_entry_point_are_reported(tmp_path: Path) -> None:
    root = write_spec(
        tmp_path / "s",
        ui={"kind": "web", "views": [{"id": "VIEW-001", "name": "V", "purpose": "p", "route": "/"}]},
    )
    assert "NO_ENTRY" in codes(root)


def test_view_satisfying_no_acceptance_criterion_is_reported(tmp_path: Path) -> None:
    root = write_spec(
        tmp_path / "s",
        ui={
            "kind": "web",
            "entry": "VIEW-001",
            "views": [{"id": "VIEW-001", "name": "V", "purpose": "p", "route": "/"}],
        },
    )
    assert "VIEW_NO_AC" in codes(root)


def test_unreachable_view_is_reported(tmp_path: Path) -> None:
    root = write_spec(
        tmp_path / "s",
        requirements=[{"id": "REQ-001", "title": "S", "acceptance": [{"id": "AC-001", "ears": "e"}]}],
        ui={
            "kind": "web",
            "entry": "VIEW-001",
            "views": [
                {"id": "VIEW-001", "name": "A", "purpose": "p", "route": "/", "satisfies": ["AC-001"]},
                {"id": "VIEW-002", "name": "B", "purpose": "p", "route": "/b", "satisfies": ["AC-001"]},
            ],
        },
    )
    found = [f for f in analyze(root).findings if f.code == "VIEW_UNREACHABLE"]
    assert found and found[0].ids == ("VIEW-002",)


def test_view_reachable_through_two_hops_is_clean(tmp_path: Path) -> None:
    root = write_spec(
        tmp_path / "s",
        requirements=[{"id": "REQ-001", "title": "S", "acceptance": [{"id": "AC-001", "ears": "e"}]}],
        ui={
            "kind": "web",
            "entry": "VIEW-001",
            "views": [
                {
                    "id": "VIEW-001", "name": "A", "purpose": "p", "route": "/",
                    "navigates_to": ["VIEW-002"], "satisfies": ["AC-001"],
                },
                {
                    "id": "VIEW-002", "name": "B", "purpose": "p", "route": "/b",
                    "navigates_to": ["VIEW-003"], "satisfies": ["AC-001"],
                },
                {"id": "VIEW-003", "name": "C", "purpose": "p", "route": "/c", "satisfies": ["AC-001"]},
            ],
        },
    )
    assert "VIEW_UNREACHABLE" not in codes(root)


def test_action_referenced_by_no_view_is_reported(tmp_path: Path) -> None:
    root = write_spec(
        tmp_path / "s",
        requirements=[{"id": "REQ-001", "title": "S", "acceptance": [{"id": "AC-001", "ears": "e"}]}],
        ui={
            "kind": "web",
            "entry": "VIEW-001",
            "views": [{"id": "VIEW-001", "name": "A", "purpose": "p", "route": "/", "satisfies": ["AC-001"]}],
            "actions": [{"id": "ACT-001", "name": "orphan"}],
        },
    )
    assert "ACTION_ORPHAN" in codes(root)


def test_cli_ui_uses_the_same_rules(tmp_path: Path) -> None:
    found = codes(write_spec(tmp_path / "s", ui={"kind": "cli"}))
    assert "NO_VIEWS" in found
```

- [ ] **Step 2: Run to verify it fails**

```bash
uv run pytest tests/unit/test_rules_ui.py -v
```

Expected: FAIL — `ModuleNotFoundError: No module named 'avspec.rules.ui'`.

- [ ] **Step 3: Implement**

`src/avspec/rules/ui.py`:

```python
"""UI completeness rules.

`kind: none` is the explicit opt-out for API-only builds and suppresses every
other rule here.
"""

from __future__ import annotations

from collections.abc import Iterable

from avspec.analysis import Spec
from avspec.findings import Finding, todo
from avspec.rules import rule


@rule
def ui_declared(spec: Spec) -> Iterable[Finding]:
    if spec.manifest.ui is None:
        yield todo(
            "UI_UNDECLARED",
            "no ui section — declare views, or kind: none",
            question=(
                "What does a user interact with? Declare the UI kind (web, cli, tui). "
                'If this is API-only, set kind: "none" explicitly.'
            ),
        )


@rule
def views_declared(spec: Spec) -> Iterable[Finding]:
    ui = spec.manifest.ui
    if ui is None or ui.kind == "none":
        return

    if not ui.views:
        yield todo(
            "NO_VIEWS",
            f"ui.kind is {ui.kind} but no views are defined",
            question=(
                "What is the first thing a user sees? Name the view, its route or "
                "invocation, and what it is for."
            ),
        )
        return

    if ui.entry is None:
        yield todo(
            "NO_ENTRY",
            "ui.entry is not set",
            question="Which view does a user land on first? That is ui.entry.",
        )

    for view in ui.views:
        if not view.satisfies:
            yield todo(
                "VIEW_NO_AC",
                f"{view.id} ({view.name}) satisfies no acceptance criterion",
                view.id,
                question=(
                    f"Which acceptance criterion does {view.id} ({view.name}) deliver? "
                    "A view that proves nothing is a view nothing asked for."
                ),
            )


@rule
def views_reachable(spec: Spec) -> Iterable[Finding]:
    ui = spec.manifest.ui
    if ui is None or ui.kind == "none" or ui.entry is None or not ui.views:
        return

    navigation = {view.id: list(view.navigates_to) for view in ui.views}
    reached: set[str] = set()
    frontier = [ui.entry]
    while frontier:
        current = frontier.pop()
        if current in reached or current not in navigation:
            continue
        reached.add(current)
        frontier.extend(navigation[current])

    for view in ui.views:
        if view.id not in reached:
            yield todo(
                "VIEW_UNREACHABLE",
                f"{view.id} ({view.name}) is not reachable from {ui.entry}",
                view.id,
                question=(
                    f"How does a user get to {view.id} ({view.name})? Add it to some "
                    "view's navigates_to, or make it the entry point."
                ),
            )


@rule
def actions_are_used(spec: Spec) -> Iterable[Finding]:
    ui = spec.manifest.ui
    if ui is None or ui.kind == "none":
        return
    used = {action_id for view in ui.views for action_id in view.actions}
    for action in ui.actions:
        if action.id not in used:
            yield todo(
                "ACTION_ORPHAN",
                f"{action.id} ({action.name}) is offered by no view",
                action.id,
                question=f"Which view offers {action.id} ({action.name})? Add it to that view's actions.",
            )
```

- [ ] **Step 4: Register the module**

Append to `src/avspec/rules/__init__.py`:

```python
from avspec.rules import ui  # noqa: E402,F401  (registration side effect)
```

- [ ] **Step 5: Run to verify it passes**

```bash
uv run pytest tests/unit/test_rules_ui.py -v
```

Expected: all nine PASS.

- [ ] **Step 6: Commit**

```bash
git add src/avspec/rules/ui.py src/avspec/rules/__init__.py tests/unit/test_rules_ui.py
git commit -m "feat: add ui rules including view reachability"
```

---

## Task 10: Contract parsing and resolution

**Files:**
- Create: `src/avspec/contracts.py`, `src/avspec/rules/contracts_rules.py`, `src/avspec/asyncapi-3.0.0.schema.json`, `tests/unit/test_contracts.py`
- Modify: `src/avspec/rules/__init__.py`, `pyproject.toml`

**Interfaces:**
- Consumes: `Spec`, `rule`, `error`, `todo`, `avspec.loading.read_yaml`
- Produces:
  - `STUB_MARKER: str = "AVSPEC:STUB"`
  - `ContractDoc(id: str, type: str, path: Path, document: Any | None, is_stub: bool, parse_error: str | None)`
  - `load_contract(spec: Spec, contract: Contract) -> ContractDoc`
  - `operation_ids(doc: ContractDoc) -> set[str]`
  - `is_empty(doc: ContractDoc) -> bool`
  - rules emitting `ACTION_UNRESOLVED`, `CONTRACT_EMPTY`

This is gate 3, which 0.1 documented but never implemented — `analyze.mjs` only checked that contract files existed.

`operation_ids` covers OpenAPI `operationId` values, JSON Schema `$defs` keys, and AsyncAPI `operations` keys, so `invokes: CTR-001#name` resolves for all three contract types.

- [ ] **Step 1: Vendor the AsyncAPI schema**

```bash
curl -fsSL https://raw.githubusercontent.com/asyncapi/spec-json-schemas/master/schemas/3.0.0.json \
  -o src/avspec/asyncapi-3.0.0.schema.json
```

Add to `pyproject.toml` so it ships in the wheel:

```toml
[tool.hatch.build.targets.wheel.force-include]
"src/avspec/asyncapi-3.0.0.schema.json" = "avspec/asyncapi-3.0.0.schema.json"
```

If the download fails, write a minimal placeholder `{"type": "object"}` and note it — AsyncAPI validation degrades to "is a mapping", which is acceptable for this plan.

- [ ] **Step 2: Write the failing test**

`tests/unit/test_contracts.py`:

```python
from pathlib import Path

from conftest import write_spec

from avspec.analysis import analyze

OPENAPI = """\
openapi: 3.1.0
info: { title: shorten, version: "1.0.0" }
paths:
  /links:
    post:
      operationId: createLink
      responses: { "201": { description: created } }
"""

OPENAPI_EMPTY = """\
openapi: 3.1.0
info: { title: shorten, version: "1.0.0" }
paths: {}
"""

OPENAPI_STUB = """\
# AVSPEC:STUB
openapi: 3.1.0
info: { title: shorten, version: "0.0.0" }
paths: {}
"""


def _spec(tmp_path: Path, contract_body: str, invokes: str) -> Path:
    root = write_spec(
        tmp_path / "s",
        contracts=[{"id": "CTR-001", "type": "openapi", "path": "contracts/api.yaml"}],
        requirements=[{"id": "REQ-001", "title": "S", "acceptance": [{"id": "AC-001", "ears": "e"}]}],
        ui={
            "kind": "web",
            "entry": "VIEW-001",
            "views": [
                {"id": "VIEW-001", "name": "V", "purpose": "p", "route": "/",
                 "actions": ["ACT-001"], "satisfies": ["AC-001"]}
            ],
            "actions": [{"id": "ACT-001", "name": "create", "invokes": invokes}],
        },
    )
    target = root / "contracts" / "api.yaml"
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(contract_body, encoding="utf-8")
    return root


def codes(root: Path) -> list[str]:
    return [f.code for f in analyze(root).findings]


def test_action_invoking_a_real_operation_resolves(tmp_path: Path) -> None:
    assert "ACTION_UNRESOLVED" not in codes(_spec(tmp_path, OPENAPI, "CTR-001#createLink"))


def test_action_invoking_a_missing_operation_is_an_error(tmp_path: Path) -> None:
    root = _spec(tmp_path, OPENAPI, "CTR-001#deleteLink")
    found = [f for f in analyze(root).findings if f.code == "ACTION_UNRESOLVED"]
    assert found and found[0].severity == "error"
    assert "deleteLink" in found[0].message


def test_unresolved_against_a_stub_contract_is_only_a_todo(tmp_path: Path) -> None:
    root = _spec(tmp_path, OPENAPI_STUB, "CTR-001#deleteLink")
    found = [f for f in analyze(root).findings if f.code == "ACTION_UNRESOLVED"]
    assert found and found[0].severity == "todo"
    assert found[0].question


def test_openapi_with_no_paths_is_reported_empty(tmp_path: Path) -> None:
    assert "CONTRACT_EMPTY" in codes(_spec(tmp_path, OPENAPI_EMPTY, "CTR-001#createLink"))


def test_populated_openapi_is_not_reported_empty(tmp_path: Path) -> None:
    assert "CONTRACT_EMPTY" not in codes(_spec(tmp_path, OPENAPI, "CTR-001#createLink"))


def test_stub_contract_is_not_also_reported_empty(tmp_path: Path) -> None:
    """A stub already reports STUB_CONTENT; CONTRACT_EMPTY would be a duplicate."""
    assert "CONTRACT_EMPTY" not in codes(_spec(tmp_path, OPENAPI_STUB, "CTR-001#createLink"))


def test_missing_contract_file_does_not_raise(tmp_path: Path) -> None:
    root = write_spec(
        tmp_path / "s",
        contracts=[{"id": "CTR-001", "type": "openapi", "path": "contracts/absent.yaml"}],
    )
    report = analyze(root)
    assert report.fatal is None
```

- [ ] **Step 3: Run to verify it fails**

```bash
uv run pytest tests/unit/test_contracts.py -v
```

Expected: FAIL — `ModuleNotFoundError: No module named 'avspec.contracts'`.

- [ ] **Step 4: Implement the contracts module**

`src/avspec/contracts.py`:

```python
"""Contract loading and operation resolution — gate 3.

0.1 checked only that contract files existed. Nothing parsed them, so the
README's contract gate was never real.
"""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
from typing import Any

from ruamel.yaml.error import YAMLError

from avspec.loading import read_yaml
from avspec.model import Contract

STUB_MARKER = "AVSPEC:STUB"


@dataclass
class ContractDoc:
    id: str
    type: str
    path: Path
    document: Any | None = None
    is_stub: bool = False
    parse_error: str | None = None

    @property
    def exists(self) -> bool:
        return self.document is not None or self.parse_error is not None


def load_contract(spec_dir: Path, contract: Contract) -> ContractDoc:
    path = spec_dir / contract.path
    doc = ContractDoc(id=contract.id, type=contract.type, path=path)
    if not path.is_file():
        return doc

    text = path.read_text(encoding="utf-8")
    doc.is_stub = STUB_MARKER in text

    try:
        loaded = read_yaml(path)
    except YAMLError as exc:
        doc.parse_error = str(exc)
        return doc

    if not isinstance(loaded, dict):
        doc.parse_error = "contract must contain a mapping at the top level"
        return doc

    doc.document = loaded
    return doc


def operation_ids(doc: ContractDoc) -> set[str]:
    """Names an action's `invokes` may reference, per contract type."""
    document = doc.document
    if not isinstance(document, dict):
        return set()

    if doc.type == "openapi":
        names: set[str] = set()
        paths = document.get("paths") or {}
        if isinstance(paths, dict):
            for item in paths.values():
                if not isinstance(item, dict):
                    continue
                for operation in item.values():
                    if isinstance(operation, dict) and "operationId" in operation:
                        names.add(str(operation["operationId"]))
        return names

    if doc.type == "asyncapi":
        operations = document.get("operations") or {}
        return set(map(str, operations)) if isinstance(operations, dict) else set()

    defs = document.get("$defs") or document.get("definitions") or {}
    return set(map(str, defs)) if isinstance(defs, dict) else set()


def is_empty(doc: ContractDoc) -> bool:
    """A contract that parses but declares nothing an implementer could build."""
    document = doc.document
    if not isinstance(document, dict):
        return False
    if doc.type == "openapi":
        return not (document.get("paths") or {})
    if doc.type == "asyncapi":
        return not (document.get("channels") or {})
    return not (document.get("$defs") or document.get("definitions") or document.get("properties") or {})
```

- [ ] **Step 5: Implement the rules**

`src/avspec/rules/contracts_rules.py`:

```python
"""Rules that read inside contract files."""

from __future__ import annotations

from collections.abc import Iterable

from avspec.analysis import Spec
from avspec.contracts import ContractDoc, is_empty, load_contract, operation_ids
from avspec.findings import Finding, error, todo
from avspec.rules import rule


def _docs(spec: Spec) -> dict[str, ContractDoc]:
    return {c.id: load_contract(spec.dir, c) for c in spec.manifest.contracts}


@rule
def contracts_are_populated(spec: Spec) -> Iterable[Finding]:
    for doc in _docs(spec).values():
        if doc.document is None or doc.is_stub:
            continue  # missing and stub files are reported by content rules
        if is_empty(doc):
            yield todo(
                "CONTRACT_EMPTY",
                f"{doc.id} parses but declares no operations ({doc.path.name})",
                doc.id,
                question=f"What operations does {doc.id} expose? The contract file is still empty.",
            )


@rule
def actions_resolve(spec: Spec) -> Iterable[Finding]:
    ui = spec.manifest.ui
    if ui is None:
        return
    docs = _docs(spec)

    for action in ui.actions:
        if action.invokes is None:
            continue
        contract_id, _, operation = action.invokes.partition("#")
        doc = docs.get(contract_id)
        if doc is None or doc.document is None:
            continue  # dangling contract id / missing file reported elsewhere
        if operation in operation_ids(doc):
            continue

        message = f"{action.id} invokes {action.invokes}, but {contract_id} has no operation {operation!r}"
        if doc.is_stub:
            yield todo(
                "ACTION_UNRESOLVED",
                f"{message} (contract is still a stub)",
                action.id,
                contract_id,
                question=(
                    f"Author {contract_id} so it declares {operation!r} — {action.id} calls it."
                ),
            )
        else:
            yield error("ACTION_UNRESOLVED", message, action.id, contract_id)
```

- [ ] **Step 6: Register the module**

Append to `src/avspec/rules/__init__.py`:

```python
from avspec.rules import contracts_rules  # noqa: E402,F401  (registration side effect)
```

- [ ] **Step 7: Run to verify it passes**

```bash
uv run pytest tests/unit/test_contracts.py -v
```

Expected: all seven PASS.

- [ ] **Step 8: Commit**

```bash
git add src/avspec/contracts.py src/avspec/rules/contracts_rules.py src/avspec/asyncapi-3.0.0.schema.json pyproject.toml src/avspec/rules/__init__.py tests/unit/test_contracts.py
git commit -m "feat: implement contract gate with operation resolution"
```

---

## Task 11: Content rules

**Files:**
- Create: `src/avspec/rules/content.py`, `tests/unit/test_rules_content.py`
- Modify: `src/avspec/rules/__init__.py`

**Interfaces:**
- Consumes: `Spec`, `rule`, `todo`, `contracts.STUB_MARKER`
- Produces: rules emitting `ARTIFACT_MISSING`, `CONTRACT_MISSING`, `ADR_MISSING`, `TEST_MISSING`, `STUB_CONTENT`, `PLACEHOLDER`, `MIRROR`

Two behaviours this task must get right, because they are the whole point of the design:

1. **A stub does not close its own todo.** A file containing `AVSPEC:STUB` reports `STUB_CONTENT` instead of counting as authored.
2. **`MIRROR` is a todo, not a warning.** Every `REQ-*`, `AC-*`, `CMP-*`, `ENT-*`, `VIEW-*`, and `TSK-*` must appear in its Markdown artifact.

- [ ] **Step 1: Write the failing test**

`tests/unit/test_rules_content.py`:

```python
from pathlib import Path

from conftest import write_spec

from avspec.analysis import analyze

ARTIFACTS = ("constitution.md", "requirements.md", "design.md", "tasks.md")


def codes(root: Path) -> list[str]:
    return [f.code for f in analyze(root).findings]


def _with_artifacts(root: Path, **bodies: str) -> Path:
    for name in ARTIFACTS:
        (root / name).write_text(bodies.get(name.removesuffix(".md"), f"# {name}\n"), encoding="utf-8")
    return root


def test_missing_artifact_files_are_reported(tmp_path: Path) -> None:
    found = codes(write_spec(tmp_path / "s"))
    assert found.count("ARTIFACT_MISSING") == 4


def test_present_artifacts_are_not_reported_missing(tmp_path: Path) -> None:
    root = _with_artifacts(write_spec(tmp_path / "s"))
    assert "ARTIFACT_MISSING" not in codes(root)


def test_a_stub_artifact_does_not_close_its_todo(tmp_path: Path) -> None:
    root = write_spec(tmp_path / "s")
    _with_artifacts(root, design="<!-- AVSPEC:STUB -->\n# Design\n")
    found = codes(root)
    assert "ARTIFACT_MISSING" not in found
    assert "STUB_CONTENT" in found


def test_missing_contract_and_adr_files_are_reported(tmp_path: Path) -> None:
    root = _with_artifacts(
        write_spec(
            tmp_path / "s",
            contracts=[{"id": "CTR-001", "type": "openapi", "path": "contracts/api.yaml"}],
            decisions=[{"id": "ADR-001", "title": "d", "status": "accepted", "path": "decisions/0001.md"}],
        )
    )
    found = codes(root)
    assert "CONTRACT_MISSING" in found
    assert "ADR_MISSING" in found


def test_missing_test_file_is_reported(tmp_path: Path) -> None:
    root = _with_artifacts(
        write_spec(
            tmp_path / "s",
            requirements=[
                {
                    "id": "REQ-001",
                    "title": "S",
                    "acceptance": [
                        {"id": "AC-001", "ears": "e", "test": "verification/acceptance/REQ-001.feature#ac-001"}
                    ],
                }
            ],
        )
    )
    assert "TEST_MISSING" in codes(root)


def test_feature_file_with_placeholders_is_reported(tmp_path: Path) -> None:
    root = _with_artifacts(
        write_spec(
            tmp_path / "s",
            requirements=[
                {
                    "id": "REQ-001",
                    "title": "S",
                    "acceptance": [
                        {"id": "AC-001", "ears": "e", "test": "verification/acceptance/REQ-001.feature#ac-001"}
                    ],
                }
            ],
        )
    )
    feature = root / "verification" / "acceptance" / "REQ-001.feature"
    feature.parent.mkdir(parents=True, exist_ok=True)
    feature.write_text(
        "Feature: REQ-001\n\n  Scenario: ac-001\n    Given <precondition>\n    Then <observable outcome>\n",
        encoding="utf-8",
    )
    found = codes(root)
    assert "TEST_MISSING" not in found
    assert "PLACEHOLDER" in found


def test_authored_feature_file_is_clean(tmp_path: Path) -> None:
    root = _with_artifacts(
        write_spec(
            tmp_path / "s",
            requirements=[
                {
                    "id": "REQ-001",
                    "title": "S",
                    "acceptance": [
                        {"id": "AC-001", "ears": "e", "test": "verification/acceptance/REQ-001.feature#ac-001"}
                    ],
                }
            ],
        )
    )
    feature = root / "verification" / "acceptance" / "REQ-001.feature"
    feature.parent.mkdir(parents=True, exist_ok=True)
    feature.write_text(
        "Feature: REQ-001\n\n  Scenario: ac-001\n    Given a target URL\n    Then a short code is returned\n",
        encoding="utf-8",
    )
    found = codes(root)
    assert "PLACEHOLDER" not in found and "TEST_MISSING" not in found


def test_mirror_is_a_todo_not_a_warning(tmp_path: Path) -> None:
    root = _with_artifacts(write_spec(tmp_path / "s", requirements=[{"id": "REQ-001", "title": "S"}]))
    mirror = [f for f in analyze(root).findings if f.code == "MIRROR"]
    assert mirror and all(f.severity == "todo" for f in mirror)
    assert "REQ-001" in mirror[0].ids


def test_ids_mentioned_in_prose_close_mirror(tmp_path: Path) -> None:
    root = write_spec(
        tmp_path / "s",
        requirements=[{"id": "REQ-001", "title": "S"}],
        data={"store": "sqlite", "entities": [{"id": "ENT-001", "name": "Link"}]},
        components=[{"id": "CMP-001", "responsibility": "r", "owns": ["ENT-001"]}],
    )
    _with_artifacts(
        root,
        requirements="# Requirements\n\n## REQ-001 Shorten\n",
        design="# Design\n\n## CMP-001\n\nOwns ENT-001.\n",
    )
    assert "MIRROR" not in codes(root)


def test_mirror_is_skipped_when_the_artifact_is_absent(tmp_path: Path) -> None:
    """Absence is already ARTIFACT_MISSING; MIRROR would be noise on top."""
    root = write_spec(tmp_path / "s", requirements=[{"id": "REQ-001", "title": "S"}])
    assert "MIRROR" not in codes(root)
```

- [ ] **Step 2: Run to verify it fails**

```bash
uv run pytest tests/unit/test_rules_content.py -v
```

Expected: FAIL — `ModuleNotFoundError: No module named 'avspec.rules.content'`.

- [ ] **Step 3: Implement**

`src/avspec/rules/content.py`:

```python
"""Content rules — do the referenced files exist, and do they say anything?

A generated stub must not close the todo that demanded it. That is the whole
reason STUB_CONTENT exists, and MIRROR is the backstop for an author who
deletes the marker without writing anything.
"""

from __future__ import annotations

import re
from collections.abc import Iterable, Iterator

from avspec.analysis import Spec
from avspec.contracts import STUB_MARKER
from avspec.findings import Finding, todo
from avspec.rules import rule

PLACEHOLDER_PATTERN = re.compile(r"<(precondition|action|observable outcome)>")


def _artifact_paths(spec: Spec) -> Iterator[tuple[str, str]]:
    artifacts = spec.manifest.artifacts
    yield "constitution", artifacts.constitution
    yield "requirements", artifacts.requirements
    yield "design", artifacts.design
    yield "tasks", artifacts.tasks


@rule
def referenced_files_exist(spec: Spec) -> Iterable[Finding]:
    m = spec.manifest

    for label, rel in _artifact_paths(spec):
        if not spec.exists(rel):
            yield todo(
                "ARTIFACT_MISSING",
                f"artifact file not yet created: {rel} ({label})",
                question=f"Create the {label} artifact at {rel}.",
            )

    for contract in m.contracts:
        if not spec.exists(contract.path):
            yield todo(
                "CONTRACT_MISSING",
                f"contract file missing: {contract.path} ({contract.id})",
                contract.id,
                question=f"Author the contract {contract.id} ({contract.type}) at {contract.path}.",
            )

    for decision in m.decisions:
        if not spec.exists(decision.path):
            yield todo(
                "ADR_MISSING",
                f"ADR file missing: {decision.path} ({decision.id})",
                decision.id,
                question=f"Write the decision record {decision.id} at {decision.path}.",
            )

    for req in m.requirements:
        for ac in req.acceptance:
            if not ac.test:
                continue  # AC_NO_TEST covers this
            file_part = ac.test.split("#")[0].split("::")[0]
            if "/" in file_part and not spec.exists(file_part):
                yield todo(
                    "TEST_MISSING",
                    f"{ac.id} test file not found: {file_part}",
                    ac.id,
                    question=f"Create the test file {file_part} with the scenario for {ac.id}.",
                )


def _referenced_files(spec: Spec) -> Iterator[tuple[str, str]]:
    """(relative path, label) for every file a stub marker could hide in."""
    for label, rel in _artifact_paths(spec):
        yield rel, label
    for contract in spec.manifest.contracts:
        yield contract.path, contract.id
    for decision in spec.manifest.decisions:
        yield decision.path, decision.id
    for req in spec.manifest.requirements:
        for ac in req.acceptance:
            if ac.test:
                yield ac.test.split("#")[0].split("::")[0], ac.id


@rule
def files_are_not_stubs(spec: Spec) -> Iterable[Finding]:
    seen: set[str] = set()
    for rel, label in _referenced_files(spec):
        if rel in seen or "/" not in rel and not spec.exists(rel):
            seen.add(rel)
            continue
        seen.add(rel)
        if not spec.exists(rel):
            continue
        if STUB_MARKER in spec.read(rel):
            yield todo(
                "STUB_CONTENT",
                f"{rel} is still a generated stub ({label})",
                label,
                question=(
                    f"Write the real content of {rel}, then delete its {STUB_MARKER} "
                    "marker line. A stub does not count as authored."
                ),
            )


@rule
def features_have_no_placeholders(spec: Spec) -> Iterable[Finding]:
    seen: set[str] = set()
    for req in spec.manifest.requirements:
        for ac in req.acceptance:
            if not ac.test:
                continue
            rel = ac.test.split("#")[0].split("::")[0]
            if rel in seen or not rel.endswith(".feature") or not spec.exists(rel):
                continue
            seen.add(rel)
            text = spec.read(rel)
            if STUB_MARKER in text:
                continue  # STUB_CONTENT already covers it
            if PLACEHOLDER_PATTERN.search(text):
                yield todo(
                    "PLACEHOLDER",
                    f"{rel} still contains scaffold placeholders",
                    ac.id,
                    question=f"Replace the <precondition>/<action>/<observable outcome> placeholders in {rel}.",
                )


@rule
def ids_are_mirrored_in_prose(spec: Spec) -> Iterable[Finding]:
    """Every ID must be discussed in its Markdown artifact.

    This is the backstop that makes deleting a stub marker insufficient — the
    honour system needs one check that is not on the honour system.
    """
    m = spec.manifest
    requirement_ids = [r.id for r in m.requirements] + [
        ac.id for r in m.requirements for ac in r.acceptance
    ]
    design_ids = (
        [c.id for c in m.components]
        + [e.id for e in (m.data.entities if m.data else [])]
        + [v.id for v in (m.ui.views if m.ui else [])]
    )
    task_ids = [t.id for t in m.tasks]

    for rel, ids in (
        (m.artifacts.requirements, requirement_ids),
        (m.artifacts.design, design_ids),
        (m.artifacts.tasks, task_ids),
    ):
        if not ids or not spec.exists(rel):
            continue  # absence is ARTIFACT_MISSING; do not pile on
        text = spec.read(rel)
        if STUB_MARKER in text:
            continue  # STUB_CONTENT already covers it
        for identifier in ids:
            if identifier not in text:
                yield todo(
                    "MIRROR",
                    f"{identifier} is not mentioned in {rel}",
                    identifier,
                    question=f"Write up {identifier} in {rel}. The manifest indexes it; the prose has to explain it.",
                )
```

- [ ] **Step 4: Register the module**

Append to `src/avspec/rules/__init__.py`:

```python
from avspec.rules import content  # noqa: E402,F401  (registration side effect)
```

- [ ] **Step 5: Run to verify it passes**

```bash
uv run pytest tests/unit/test_rules_content.py -v
```

Expected: all ten PASS.

- [ ] **Step 6: Commit**

```bash
git add src/avspec/rules/content.py src/avspec/rules/__init__.py tests/unit/test_rules_content.py
git commit -m "feat: add content rules — stubs no longer close their own todos"
```

---

## Task 12: Architecture rules

**Files:**
- Create: `src/avspec/rules/architecture.py`, `tests/unit/test_rules_architecture.py`
- Modify: `src/avspec/rules/__init__.py`

**Interfaces:**
- Consumes: `Spec`, `rule`, `error`, `todo`, `warn`, `avspec.loading.read_yaml`
- Produces:
  - `ARCH_RULES_REL: str = "verification/architecture-rules.yaml"`
  - rules emitting `LAYER_UNPLACED`, `ARCH_VIOLATION`, `NO_ARCH_RULES`

`architecture-rules.yaml` stays plain YAML rather than a Pydantic model — it is authored by hand, read in exactly one place, and Plan 2's `mergeLayers` equivalent rewrites it.

- [ ] **Step 1: Write the failing test**

`tests/unit/test_rules_architecture.py`:

```python
from pathlib import Path

from conftest import write_spec

from avspec.analysis import analyze

LAYERED = """\
layers:
  presentation: [CMP-001]
  domain: [CMP-002]
  data: [CMP-003]
allow:
  - { from: presentation, to: [domain] }
  - { from: domain, to: [data] }
"""


def codes(root: Path) -> list[str]:
    return [f.code for f in analyze(root).findings]


def _rules(root: Path, body: str) -> Path:
    target = root / "verification" / "architecture-rules.yaml"
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(body, encoding="utf-8")
    return root


def test_absent_rules_file_is_a_warning_only(tmp_path: Path) -> None:
    report = analyze(write_spec(tmp_path / "s"))
    found = [f for f in report.findings if f.code == "NO_ARCH_RULES"]
    assert found and found[0].severity == "warn"


def test_unplaced_component_is_a_todo(tmp_path: Path) -> None:
    root = _rules(
        write_spec(tmp_path / "s", components=[{"id": "CMP-009", "responsibility": "r"}]), LAYERED
    )
    found = [f for f in analyze(root).findings if f.code == "LAYER_UNPLACED"]
    assert found and found[0].severity == "todo" and found[0].ids == ("CMP-009",)


def test_allowed_dependency_direction_is_clean(tmp_path: Path) -> None:
    root = _rules(
        write_spec(
            tmp_path / "s",
            components=[
                {"id": "CMP-001", "responsibility": "r", "depends_on": ["CMP-002"]},
                {"id": "CMP-002", "responsibility": "r", "depends_on": ["CMP-003"]},
                {"id": "CMP-003", "responsibility": "r"},
            ],
        ),
        LAYERED,
    )
    assert "ARCH_VIOLATION" not in codes(root)


def test_back_edge_is_an_error(tmp_path: Path) -> None:
    root = _rules(
        write_spec(
            tmp_path / "s",
            components=[
                {"id": "CMP-001", "responsibility": "r"},
                {"id": "CMP-002", "responsibility": "r"},
                {"id": "CMP-003", "responsibility": "r", "depends_on": ["CMP-001"]},
            ],
        ),
        LAYERED,
    )
    found = [f for f in analyze(root).findings if f.code == "ARCH_VIOLATION"]
    assert found and found[0].severity == "error"


def test_skip_layer_dependency_is_an_error(tmp_path: Path) -> None:
    root = _rules(
        write_spec(
            tmp_path / "s",
            components=[
                {"id": "CMP-001", "responsibility": "r", "depends_on": ["CMP-003"]},
                {"id": "CMP-002", "responsibility": "r"},
                {"id": "CMP-003", "responsibility": "r"},
            ],
        ),
        LAYERED,
    )
    assert "ARCH_VIOLATION" in codes(root)


def test_same_layer_dependency_is_allowed(tmp_path: Path) -> None:
    root = _rules(
        write_spec(
            tmp_path / "s",
            components=[
                {"id": "CMP-002", "responsibility": "r", "depends_on": ["CMP-004"]},
                {"id": "CMP-004", "responsibility": "r"},
            ],
        ),
        "layers:\n  domain: [CMP-002, CMP-004]\nallow: []\n",
    )
    assert "ARCH_VIOLATION" not in codes(root)


def test_malformed_rules_file_does_not_crash_the_analyzer(tmp_path: Path) -> None:
    root = _rules(write_spec(tmp_path / "s"), "layers: [not, a, mapping]\n")
    report = analyze(root)
    assert report.fatal is None
```

- [ ] **Step 2: Run to verify it fails**

```bash
uv run pytest tests/unit/test_rules_architecture.py -v
```

Expected: FAIL — `ModuleNotFoundError: No module named 'avspec.rules.architecture'`.

- [ ] **Step 3: Implement**

`src/avspec/rules/architecture.py`:

```python
"""Layer placement and dependency-direction rules.

`verification/architecture-rules.yaml` is hand-authored and read in exactly
one place, so it stays plain YAML rather than a Pydantic model.
"""

from __future__ import annotations

from collections.abc import Iterable
from typing import Any

from ruamel.yaml.error import YAMLError

from avspec.analysis import Spec
from avspec.findings import Finding, error, todo, warn
from avspec.loading import read_yaml
from avspec.rules import rule

ARCH_RULES_REL = "verification/architecture-rules.yaml"


def _load_rules(spec: Spec) -> dict[str, Any] | None:
    if not spec.exists(ARCH_RULES_REL):
        return None
    try:
        loaded = read_yaml(spec.path(ARCH_RULES_REL))
    except YAMLError:
        return {}
    return loaded if isinstance(loaded, dict) else {}


@rule
def architecture(spec: Spec) -> Iterable[Finding]:
    rules = _load_rules(spec)
    if rules is None:
        yield warn("NO_ARCH_RULES", f"no {ARCH_RULES_REL} — dependency direction is unchecked")
        return

    raw_layers = rules.get("layers")
    layers: dict[str, list[str]] = raw_layers if isinstance(raw_layers, dict) else {}

    layer_of: dict[str, str] = {}
    for layer, members in layers.items():
        for component_id in members or []:
            layer_of[str(component_id)] = str(layer)

    allowed: dict[str, set[str]] = {}
    for entry in rules.get("allow") or []:
        if isinstance(entry, dict) and "from" in entry:
            allowed[str(entry["from"])] = {str(t) for t in entry.get("to") or []}

    for component in spec.manifest.components:
        source = layer_of.get(component.id)
        if source is None:
            yield todo(
                "LAYER_UNPLACED",
                f"{component.id} is not placed in any layer",
                component.id,
                question=(
                    f"Which layer does {component.id} belong to "
                    f"({' / '.join(layers) or 'presentation / domain / data'})?"
                ),
            )
            continue

        for dependency in component.depends_on:
            target = layer_of.get(dependency)
            if target is None or target == source:
                continue
            if target not in allowed.get(source, set()):
                yield error(
                    "ARCH_VIOLATION",
                    f"{component.id}({source}) -> {dependency}({target}) is not an allowed dependency",
                    component.id,
                    dependency,
                )
```

- [ ] **Step 4: Register the module**

Append to `src/avspec/rules/__init__.py`:

```python
from avspec.rules import architecture  # noqa: E402,F401  (registration side effect)
```

- [ ] **Step 5: Run the whole suite**

```bash
uv run pytest -v
```

Expected: all PASS. Earlier rule tests now also see a `NO_ARCH_RULES` warning; every one of them asserts on specific codes rather than exact list equality, so none break.

- [ ] **Step 6: Commit**

```bash
git add src/avspec/rules/architecture.py src/avspec/rules/__init__.py tests/unit/test_rules_architecture.py
git commit -m "feat: add layer placement and dependency direction rules"
```

---

## Task 13: `avspec verify` and `avspec next`

**Files:**
- Create: `src/avspec/cli/verify.py`, `src/avspec/cli/next_.py`
- Create: `tests/features/verify.feature`, `tests/features/test_verify_steps.py`
- Create: `tests/unit/test_cli.py`
- Modify: `src/avspec/cli/__init__.py`

**Interfaces:**
- Consumes: `analyze`, `Report`, `Finding`
- Produces: `avspec verify [DIR] [--json]` and `avspec next [DIR] [--json]` registered on `app`

Exit codes are the contract CI depends on: **0 = pass, 1 = fail, 2 = fatal.**

`next_.py` is named with a trailing underscore because `next` is a builtin; the typer command is still `next`.

- [ ] **Step 1: Write the BDD feature**

`tests/features/verify.feature`:

```gherkin
Feature: avspec verify
  The CI gate. A draft may carry open questions; a ready spec may not.

  Scenario: a draft spec with open questions still passes
    Given a minimal draft spec
    When I run "verify"
    Then the exit code is 0
    And the output contains "PASS"
    And the output contains "open todo"

  Scenario: the same spec marked ready fails
    Given a minimal draft spec
    And the spec status is "ready"
    When I run "verify"
    Then the exit code is 1
    And the output contains "FAIL"

  Scenario: a directory with no manifest is fatal
    Given an empty directory
    When I run "verify"
    Then the exit code is 2
    And the output contains "missing manifest"

  Scenario: JSON output carries the findings
    Given a minimal draft spec
    When I run "verify --json"
    Then the exit code is 0
    And the JSON field "status" is "draft"
    And the JSON findings include "NO_STACK"

  Scenario: next lists the open questions in authoring order
    Given a minimal draft spec
    When I run "next"
    Then the exit code is 0
    And the first question mentions "language"
```

- [ ] **Step 2: Write the step definitions**

`tests/features/test_verify_steps.py`:

```python
from __future__ import annotations

import json
from pathlib import Path

import pytest
from conftest import write_spec
from pytest_bdd import given, parsers, scenarios, then, when
from typer.testing import CliRunner

from avspec.cli import app

scenarios("verify.feature")


@pytest.fixture
def context(tmp_path: Path) -> dict:
    return {"root": tmp_path / "spec", "result": None}


@given("a minimal draft spec")
def _minimal(context: dict) -> None:
    write_spec(context["root"])


@given("an empty directory")
def _empty(context: dict) -> None:
    context["root"].mkdir(parents=True, exist_ok=True)


@given(parsers.parse('the spec status is "{status}"'))
def _status(context: dict, status: str) -> None:
    write_spec(context["root"], metadata={"name": "linkshort", "status": status})


@when(parsers.parse('I run "{command}"'))
def _run(context: dict, command: str) -> None:
    args = [*command.split(), str(context["root"])]
    context["result"] = CliRunner(mix_stderr=True).invoke(app, args)


@then(parsers.parse("the exit code is {code:d}"))
def _exit_code(context: dict, code: int) -> None:
    assert context["result"].exit_code == code, context["result"].output


@then(parsers.parse('the output contains "{text}"'))
def _contains(context: dict, text: str) -> None:
    assert text in context["result"].output


@then(parsers.parse('the JSON field "{field}" is "{value}"'))
def _json_field(context: dict, field: str, value: str) -> None:
    assert json.loads(context["result"].stdout)[field] == value


@then(parsers.parse('the JSON findings include "{code}"'))
def _json_findings(context: dict, code: str) -> None:
    payload = json.loads(context["result"].stdout)
    assert code in [f["code"] for f in payload["findings"]]


@then(parsers.parse('the first question mentions "{text}"'))
def _first_question(context: dict, text: str) -> None:
    assert text in context["result"].output.split("\n\n")[1]
```

- [ ] **Step 3: Write the unit test for exit-code plumbing**

`tests/unit/test_cli.py`:

```python
import json
from pathlib import Path

from conftest import write_spec
from typer.testing import CliRunner

from avspec.cli import app

runner = CliRunner(mix_stderr=True)


def test_verify_json_is_parseable_and_reports_counts(spec_dir: Path) -> None:
    result = runner.invoke(app, ["verify", str(spec_dir), "--json"])
    payload = json.loads(result.stdout)
    assert payload["pass"] is True
    assert payload["counts"]["todo"] > 0
    assert payload["strict"] is False


def test_next_json_numbers_the_queue(spec_dir: Path) -> None:
    result = runner.invoke(app, ["next", str(spec_dir), "--json"])
    payload = json.loads(result.stdout)
    assert payload["queue"][0]["n"] == 1
    assert payload["queue"][0]["ask"]


def test_next_on_a_complete_spec_says_so(tmp_path: Path, monkeypatch) -> None:
    from avspec import rules

    monkeypatch.setattr(rules, "RULES", [])
    root = write_spec(tmp_path / "s")
    result = runner.invoke(app, ["next", str(root)])
    assert result.exit_code == 0
    assert "Nothing open" in result.output


def test_dir_defaults_to_cwd(spec_dir: Path, monkeypatch) -> None:
    monkeypatch.chdir(spec_dir)
    assert runner.invoke(app, ["verify"]).exit_code == 0
```

- [ ] **Step 4: Run to verify they fail**

```bash
uv run pytest tests/features tests/unit/test_cli.py -v
```

Expected: FAIL — no `verify` or `next` command registered.

- [ ] **Step 5: Implement `verify`**

`src/avspec/cli/verify.py`:

```python
"""`avspec verify` — the CI gate.

Exit codes: 0 pass, 1 fail, 2 fatal.
"""

from __future__ import annotations

import json
from dataclasses import asdict
from pathlib import Path
from typing import Annotated

import typer

from avspec.analysis import Report, analyze

EXIT_PASS = 0
EXIT_FAIL = 1
EXIT_FATAL = 2


def report_payload(report: Report) -> dict:
    return {
        "status": report.status,
        "strict": report.strict,
        "pass": report.passed,
        "counts": report.counts,
        "findings": [asdict(f) | {"ids": list(f.ids)} for f in report.findings],
    }


def verify(
    directory: Annotated[Path, typer.Argument(help="Spec directory.")] = Path("."),
    as_json: Annotated[bool, typer.Option("--json", help="Emit machine-readable output.")] = False,
) -> None:
    """Verify a spec. Exits non-zero when it does not pass for its status."""
    report = analyze(directory)

    if report.fatal is not None:
        if as_json:
            typer.echo(json.dumps({"fatal": report.fatal}, indent=2))
        else:
            typer.echo(f"FATAL: {report.fatal}", err=True)
        raise typer.Exit(EXIT_FATAL)

    if as_json:
        typer.echo(json.dumps(report_payload(report), indent=2))
        raise typer.Exit(EXIT_PASS if report.passed else EXIT_FAIL)

    for finding in report.findings:
        if finding.severity == "error":
            typer.echo(f"  ERROR  [{finding.code}] {finding.message}", err=True)
        elif finding.severity == "todo":
            label = "TODO*" if report.strict else "todo "
            typer.echo(f"  {label}  [{finding.code}] {finding.message}")
        else:
            typer.echo(f"  warn   [{finding.code}] {finding.message}")

    typer.echo("")
    counts = report.counts
    if report.passed:
        typer.echo(
            f"PASS (status: {report.status}) — {counts['error']} errors, "
            f"{counts['todo']} open todos, {counts['warn']} warnings."
        )
        if not report.strict and counts["todo"]:
            typer.echo(f"  {counts['todo']} todo(s) must be closed before status: ready.")
        raise typer.Exit(EXIT_PASS)

    blocking = f" + {counts['todo']} unmet todo(s) blocking ready" if report.strict else ""
    typer.echo(f"FAIL (status: {report.status}) — {counts['error']} error(s){blocking}.", err=True)
    raise typer.Exit(EXIT_FAIL)
```

- [ ] **Step 6: Implement `next`**

`src/avspec/cli/next_.py`:

```python
"""`avspec next` — the ordered question queue.

Module name has a trailing underscore because `next` is a builtin; the
command is still `next`.
"""

from __future__ import annotations

import json
from pathlib import Path
from typing import Annotated

import typer

from avspec.analysis import analyze
from avspec.cli.verify import EXIT_FAIL, EXIT_FATAL, EXIT_PASS


def next_(
    directory: Annotated[Path, typer.Argument(help="Spec directory.")] = Path("."),
    as_json: Annotated[bool, typer.Option("--json", help="Emit machine-readable output.")] = False,
) -> None:
    """Show what the spec still needs, in the order it should be answered."""
    report = analyze(directory)

    if report.fatal is not None:
        typer.echo(f"FATAL: {report.fatal}", err=True)
        raise typer.Exit(EXIT_FATAL)

    errors = [f for f in report.findings if f.severity == "error"]
    todos = [f for f in report.findings if f.severity == "todo"]
    warnings = [f for f in report.findings if f.severity == "warn"]

    if as_json:
        queue = [
            {
                "n": index,
                "severity": f.severity,
                "code": f.code,
                "ids": list(f.ids),
                "ask": f.question or f.message,
            }
            for index, f in enumerate([*errors, *todos], start=1)
        ]
        typer.echo(
            json.dumps(
                {"status": report.status, "pass": report.passed, "counts": report.counts, "queue": queue},
                indent=2,
            )
        )
        raise typer.Exit(EXIT_PASS if report.passed else EXIT_FAIL)

    name = report.manifest.metadata.name if report.manifest else "spec"
    typer.echo(
        f"\n{name} — status: {report.status} — "
        f"{len(todos)} open question(s), {len(errors)} blocking error(s)\n"
    )

    number = 1
    if errors:
        typer.echo("must fix first (the spec contradicts itself):")
        for finding in errors:
            typer.echo(f"  {number}. [{finding.code}] {finding.message}")
            number += 1
        typer.echo("")

    if todos:
        typer.echo("open questions:")
        for finding in todos:
            typer.echo(f"  {number}. {finding.question}")
            number += 1
        typer.echo("")

    if not errors and not todos:
        tail = "Spec passes." if report.strict else "Ready to set status: ready."
        typer.echo(f"Nothing open. {tail}")

    if warnings:
        typer.echo(f"({len(warnings)} warning(s) — non-blocking)")

    raise typer.Exit(EXIT_PASS if report.passed else EXIT_FAIL)
```

- [ ] **Step 7: Register both commands**

Replace the body of `src/avspec/cli/__init__.py` below the `app = typer.Typer(...)` block with:

```python
from avspec.cli.next_ import next_  # noqa: E402
from avspec.cli.verify import verify  # noqa: E402

app.command("version")(_version)
app.command("verify")(verify)
app.command("next")(next_)
```

and rename the existing `version` function to `_version`, keeping its body:

```python
def _version() -> None:
    """Print the AVSpec version."""
    typer.echo(__version__)
```

Remove the `@app.command()` decorator from it — registration now happens explicitly below.

- [ ] **Step 8: Run to verify they pass**

```bash
uv run pytest tests/features tests/unit/test_cli.py -v
```

Expected: all five scenarios and all four unit tests PASS.

- [ ] **Step 9: Run the full suite, lint, and type-check**

```bash
uv run pytest
uv run ruff check .
uv run ruff format --check .
uv run ty check src tests
```

Expected: all green. `ty` is pre-1.0 — if it reports errors in third-party stubs rather than in this code, note them and move on; do not silence real findings in `src/avspec`.

- [ ] **Step 10: Verify against a real directory by hand**

```bash
mkdir -p /tmp/avspec-smoke && cd /tmp/avspec-smoke
cat > avspec.yaml <<'YAML'
avspec: "0.2"
metadata: { name: smoke, status: draft }
mode: greenfield
artifacts:
  constitution: constitution.md
  requirements: requirements.md
  design: design.md
  tasks: tasks.md
YAML
cd - && uv run avspec next /tmp/avspec-smoke
uv run avspec verify /tmp/avspec-smoke; echo "exit=$?"
```

Expected: `next` lists questions beginning with the stack question; `verify` prints `PASS (status: draft)` with `exit=0`. Confirm the first question mentions the language — that proves `CODE_ORDER` is in authoring order.

- [ ] **Step 11: Commit**

```bash
git add src/avspec/cli tests/features tests/unit/test_cli.py
git commit -m "feat: add avspec verify and avspec next commands"
```

---

## Plan 1 Complete

`avspec verify` and `avspec next` fully analyze an AVSpec 0.2 spec. `status: ready` now requires a declared stack, entities with owners, a UI or an explicit opt-out, tasks tracing acceptance criteria to components or views, contracts whose operations actually resolve, and prose that mentions every ID.

**Deferred to Plan 2 (Authoring):** `patch.py`, `scaffold.py`, `tui.py`, and the `init` / `apply` / `scaffold` / `ask` / `schema --emit` commands.

**Deferred to Plan 3 (Migration):** rewriting `example/` (non-Python) and `example-draft/`, adding the Python example, the AVSpec self-spec, deleting `tools/*.mjs` and `package.json`, updating `ci/verify.yml`, `templates/`, and the README.

---

## Self-Review

**Spec coverage.** Every 0.2 format section maps to Task 3 or 4; every new finding code in the design maps to a rule in Tasks 7–12; `verify`/`next` land in Task 13. The design's `NO_ARCH_RULES` warning is Task 12. Gate 3 is Task 10. Deliberate omissions carried forward correctly: no NFR section, no `AC_NO_VIEW` rule.

**Placeholder scan.** No TBDs. Every code step contains runnable code. Two known typos are called out inline for the implementer to fix while typing: the space in `test_findings_come back_in_authoring_order` (Task 6, Step 1) and the `@app.command()` decorator removal (Task 13, Step 7).

**Type consistency.** `Report.passed` is used throughout — never `pass_` or `pass`, which are reserved or misleading. `Spec.dir` / `.manifest` / `.raw` are consistent across Tasks 6–12. `STUB_MARKER` is defined once in `contracts.py` and imported by `content.py`, not redefined. `ordered()` is applied once, inside `_report`, so no command re-sorts.

**One risk accepted.** `RULES` is module-level global state. Tests that need isolation monkeypatch `avspec.rules.RULES`; rule tests instead assert on membership (`"CODE" in codes(...)`) rather than exact list equality, so adding a rule in a later task never breaks an earlier task's tests.
