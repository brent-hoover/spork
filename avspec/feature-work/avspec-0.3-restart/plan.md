# AVSpec 0.3 Slice 1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the AVSpec 0.3 core — Pydantic models, a rule-registry analyzer whose todos fire on absence, `avspec verify` / `avspec next` commands, the hand-written linkshort conformance example, and the rewritten agent-led interview skill.

**Architecture:** Pydantic v2 models in `model.py` are the canonical format definition. `analysis.py` runs an ordered registry of pure rule functions `(Spec) -> Iterable[Finding]`; `verify` and `next` are two presentations of the same report. The interview is a Claude skill driving `avspec next --json` — the verifier is the brain, the agent is the mouth.

**Tech Stack:** Python 3.12+, uv, Pydantic v2, ruamel.yaml, typer, pytest, pytest-bdd, ruff, ty.

**Source design:** `feature-work/avspec-0.3-restart/design.md`

## Global Constraints

- Work happens in a **git worktree** (`core:worktree` skill → `.worktrees/avspec-0.3-slice1`), branched from `develop`.
- All paths below are relative to the `avspec/` subdirectory of the repo.
- **Exactly three runtime dependencies:** `pydantic`, `ruamel.yaml`, `typer`. Dev: `pytest`, `pytest-bdd`, `ruff`, `ty`. Nothing else without explicit approval. **PyYAML is forbidden.**
- Format version literal is `"0.3"`. Manifest filename is `avspec.yaml`.
- ID prefixes, exactly: `CON-`, `REQ-`, `AC-`, `MOD-`, `CTR-`, `VIEW-`, `ACT-`. Suffix style is free (numeric or slug).
- No enums for stack tooling: `package_manager`, `frameworks`, `bdd`, `commands.*`, `type` on contracts are free-form strings.
- Severities: `error` always fails; `todo` fails only at `status: ready` or `built`; `warn` never fails. Todos fire on **absence**.
- Type hints on all new code. No bare `except`. Conventional commits, no co-branding.
- A spec directory contains zero Python and carries no schema file.
- Commit at the end of every task (reviewable chunks — Roborev gates progress).

---

## File Structure

| File | Responsibility |
|---|---|
| `pyproject.toml` | uv project, deps, ruff/pytest config, `avspec` entry point |
| `README.md` | One-paragraph project description (also fixes the build: pyproject references it) |
| `src/avspec/__init__.py` | `__version__` only |
| `src/avspec/findings.py` | `Finding`, `Severity`, `CODE_ORDER`, `ordered()` — imports nothing else from avspec |
| `src/avspec/model.py` | Pydantic v2 models — canonical 0.3 format definition |
| `src/avspec/loading.py` | ruamel read + Pydantic validation → `LoadResult` |
| `src/avspec/analysis.py` | `Spec`, `Report`, `analyze()` |
| `src/avspec/rules/__init__.py` | `RULES` registry, `@rule` decorator, registration imports |
| `src/avspec/rules/wellformed.py` | Errors: duplicates, prefixes, dangling refs, boundary cycles |
| `src/avspec/rules/completeness.py` | Absence todos + contract file checks |
| `src/avspec/rules/ui.py` | View/action todos including reachability |
| `src/avspec/cli.py` | typer app: `verify`, `next` |
| `tests/conftest.py` | `MINIMAL` manifest dict, `write_manifest()`, `make_spec()` |
| `tests/unit/test_*.py` | One test module per source module |
| `tests/features/*.feature` + `tests/steps/test_cli_steps.py` | CLI behavior via pytest-bdd |
| `examples/linkshort/` | Hand-written 0.3 spec; CI conformance fixture, must verify at `ready` |
| `adapters/guided-qa/SKILL.md` | Agent-led interview skill (rewritten) |
| `ci/verify.yml` | uv-based CI: ruff, ty, pytest, `avspec verify examples/linkshort` |

Note on imports: rule modules import `Spec` from `avspec.analysis` at module top; `analysis.analyze()` imports `RULES` *inside the function* to avoid a circular import at load time.

---

### Task 1: Clean slate and project scaffold

**Files:**
- Delete: `tools/`, `example/`, `avspec.schema.yaml`, `src/avspec/*.py` (all seven old modules), `tests/test_analysis.py`, `tests/test_package.py`, `adapters/guided-qa/patch.example.yaml`, `adapters/guided-qa/DESIGN.md`
- Move: `feature-work/avspec-0.2-rewrite/` → `feature-work/archive/avspec-0.2-rewrite/`
- Create: `README.md`, `pyproject.toml` (rewrite), `src/avspec/__init__.py`, `tests/unit/test_package.py`
- Leave untouched: `templates/` (rewritten in a later slice), `verification/`, `ci/verify.yml` (rewritten in Task 10)

**Interfaces:**
- Produces: an importable `avspec` package with `__version__ = "0.3.0"`; `uv run pytest` green.

- [ ] **Step 1: Delete and archive**

```bash
git rm -r tools example avspec.schema.yaml src/avspec tests/test_analysis.py tests/test_package.py \
  adapters/guided-qa/patch.example.yaml adapters/guided-qa/DESIGN.md
mkdir -p feature-work/archive
git mv feature-work/avspec-0.2-rewrite feature-work/archive/avspec-0.2-rewrite
```

- [ ] **Step 2: Write `README.md`**

```markdown
# AVSpec

Agent-Verifiable Architecture Spec. A machine-checkable format
(modules, boundaries, contracts, constitution), a verifier that gates CI
(`avspec verify`), and an agent-led interview that authors specs
(`avspec next` is its brain). A spec directory contains zero Python —
plain YAML, Markdown, Gherkin, and standard contract formats.

Design: `feature-work/avspec-0.3-restart/design.md`
```

- [ ] **Step 3: Rewrite `pyproject.toml`**

```toml
[project]
name = "avspec"
version = "0.3.0"
description = "Agent-Verifiable Architecture Spec format, verifier, and interview brain"
readme = "README.md"
requires-python = ">=3.12"
dependencies = [
    "pydantic>=2.11",
    "ruamel.yaml>=0.18",
    "typer>=0.16",
]

[project.scripts]
avspec = "avspec.cli:app"

[dependency-groups]
dev = [
    "pytest>=8.4",
    "pytest-bdd>=8.1",
    "ruff>=0.12",
    "ty>=0.0.1a16",
]

[build-system]
requires = ["hatchling"]
build-backend = "hatchling.build"

[tool.hatch.build.targets.wheel]
packages = ["src/avspec"]

[tool.ruff]
line-length = 100
target-version = "py312"
src = ["src", "tests"]
extend-exclude = ["feature-work", "examples", "templates"]

[tool.ruff.lint]
select = ["E", "F", "I", "UP", "B", "SIM"]

[tool.pytest.ini_options]
testpaths = ["tests"]
addopts = "-q"
```

- [ ] **Step 4: Create the package and a failing smoke test**

`src/avspec/__init__.py`:

```python
"""AVSpec — Agent-Verifiable Architecture Spec."""

__version__ = "0.3.0"
```

`tests/unit/__init__.py` is not needed; `tests/unit/test_package.py`:

```python
from avspec import __version__


def test_version() -> None:
    assert __version__ == "0.3.0"
```

- [ ] **Step 5: Sync and run**

Run: `uv sync && uv run pytest`
Expected: 1 passed. (Before this task, `uv run pytest` failed with "Readme file does not exist".)

- [ ] **Step 6: Lint, type-check, commit**

```bash
uv run ruff check . && uv run ty check
git add -A
git commit -m "chore: clean out 0.1/0.2 artifacts, scaffold avspec 0.3 package"
```

---

### Task 2: Findings module

**Files:**
- Create: `src/avspec/findings.py`
- Test: `tests/unit/test_findings.py`

**Interfaces:**
- Produces: `Severity` (StrEnum: `ERROR`/`TODO`/`WARN`, values `"error"`/`"todo"`/`"warn"`), frozen dataclass `Finding(code: str, severity: Severity, message: str, ref: str | None = None, question: str | None = None)`, `CODE_ORDER: tuple[str, ...]`, `ordered(findings: Iterable[Finding]) -> list[Finding]`.

- [ ] **Step 1: Write the failing tests**

`tests/unit/test_findings.py`:

```python
from avspec.findings import CODE_ORDER, Finding, Severity, ordered


def f(code: str, sev: Severity, ref: str = "") -> Finding:
    return Finding(code=code, severity=sev, message="m", ref=ref or None)


def test_errors_sort_before_todos() -> None:
    todo = f("NO_STACK", Severity.TODO)
    err = f("DUPLICATE_ID", Severity.ERROR)
    assert ordered([todo, err]) == [err, todo]


def test_todos_follow_authoring_order() -> None:
    later = f("VIEW_NO_AC", Severity.TODO)
    earlier = f("NO_REQUIREMENTS", Severity.TODO)
    assert ordered([later, earlier]) == [earlier, later]


def test_unknown_code_sorts_last_within_severity() -> None:
    known = f("TEST_FILE_MISSING", Severity.TODO)
    unknown = f("SOMETHING_NEW", Severity.TODO)
    assert ordered([unknown, known]) == [known, unknown]


def test_ties_break_on_ref() -> None:
    b = f("REQ_NO_AC", Severity.TODO, ref="REQ-b")
    a = f("REQ_NO_AC", Severity.TODO, ref="REQ-a")
    assert ordered([b, a]) == [a, b]


def test_code_order_is_unique() -> None:
    assert len(CODE_ORDER) == len(set(CODE_ORDER))
```

- [ ] **Step 2: Run to verify failure**

Run: `uv run pytest tests/unit/test_findings.py -v`
Expected: FAIL — `ModuleNotFoundError: avspec.findings`

- [ ] **Step 3: Implement `src/avspec/findings.py`**

```python
"""Finding, Severity, and ordering. Imports nothing else from avspec."""

from __future__ import annotations

from collections.abc import Iterable
from dataclasses import dataclass
from enum import StrEnum


class Severity(StrEnum):
    ERROR = "error"
    TODO = "todo"
    WARN = "warn"


@dataclass(frozen=True)
class Finding:
    code: str
    severity: Severity
    message: str
    ref: str | None = None
    question: str | None = None


# Authoring order: you cannot answer later questions before earlier ones exist.
CODE_ORDER: tuple[str, ...] = (
    "NO_STACK",
    "NO_CONSTITUTION",
    "NO_REQUIREMENTS",
    "REQ_NO_AC",
    "AC_NO_TEST",
    "NO_MODULES",
    "MOD_NO_RESPONSIBILITY",
    "MOD_NO_BOUNDARIES",
    "CONTRACT_FILE_MISSING",
    "UI_NO_ENTRY",
    "VIEW_NO_AC",
    "VIEW_UNREACHABLE",
    "ACTION_ORPHAN",
    "TEST_FILE_MISSING",
)

_SEVERITY_RANK = {Severity.ERROR: 0, Severity.TODO: 1, Severity.WARN: 2}


def ordered(findings: Iterable[Finding]) -> list[Finding]:
    """Errors first, then todos in authoring order, then warns; ties break on ref."""

    def key(finding: Finding) -> tuple[int, int, str]:
        try:
            position = CODE_ORDER.index(finding.code)
        except ValueError:
            position = len(CODE_ORDER)
        return (_SEVERITY_RANK[finding.severity], position, finding.ref or "")

    return sorted(findings, key=key)
```

- [ ] **Step 4: Run to verify pass**

Run: `uv run pytest tests/unit/test_findings.py -v`
Expected: 5 passed

- [ ] **Step 5: Lint, type-check, commit**

```bash
uv run ruff check . && uv run ty check
git add src/avspec/findings.py tests/unit/test_findings.py
git commit -m "feat: findings model with severity and authoring order"
```

---

### Task 3: Manifest models

**Files:**
- Create: `src/avspec/model.py`
- Test: `tests/unit/test_model.py`

**Interfaces:**
- Produces (all Pydantic v2, `extra="forbid"`): `Manifest(avspec: Literal["0.3"], project: Project, constitution: list[ConstitutionEntry], requirements: list[Requirement], modules: list[Module])`; `Project(name, description=None, status: Literal["draft","ready","built"]="draft", stack: Stack|None)`; `Stack(languages: list[str|Language], package_manager, frameworks, bdd, commands: Commands|None)`; `Requirement(id, title, rationale=None, acceptance: list[AcceptanceCriterion])`; `AcceptanceCriterion(id, statement, test: str|None)`; `Module(id, name, responsibility=None, stack=None, boundaries: Boundaries|None, contracts: list[Contract], ui: UI|None)`; `Boundaries(may_import: list[str])`; `Contract(id, type: str, path: str)`; `UI(kind: Literal["web","cli","tui","none"], entry: str|None, views: list[View], actions: list[Action])`; `View(id, name, route=None, invocation=None, purpose=None, shows, actions, navigates_to, satisfies: list[str])`; `Action(id, name, invokes: str|None)`; `ConstitutionEntry(id, statement)`.

- [ ] **Step 1: Write the failing tests**

`tests/unit/test_model.py`:

```python
import pytest
from pydantic import ValidationError

from avspec.model import Manifest

MINIMAL = {"avspec": "0.3", "project": {"name": "demo"}}


def test_minimal_manifest_validates() -> None:
    m = Manifest.model_validate(MINIMAL)
    assert m.project.status == "draft"
    assert m.modules == []


def test_unknown_key_is_rejected() -> None:
    with pytest.raises(ValidationError):
        Manifest.model_validate({**MINIMAL, "componets": []})


def test_wrong_version_is_rejected() -> None:
    with pytest.raises(ValidationError):
        Manifest.model_validate({**MINIMAL, "avspec": "0.2"})


def test_full_module_round_trip() -> None:
    data = {
        **MINIMAL,
        "constitution": [{"id": "CON-layering", "statement": "api -> domain -> data"}],
        "requirements": [
            {
                "id": "REQ-create",
                "title": "Create",
                "acceptance": [
                    {"id": "AC-ok", "statement": "shall work", "test": "verification/x.feature#ok"}
                ],
            }
        ],
        "modules": [
            {
                "id": "MOD-web",
                "name": "web",
                "responsibility": "UI",
                "boundaries": {"may_import": []},
                "contracts": [{"id": "CTR-api", "type": "openapi", "path": "contracts/api.yaml"}],
                "ui": {
                    "kind": "web",
                    "entry": "VIEW-home",
                    "views": [
                        {
                            "id": "VIEW-home",
                            "name": "Home",
                            "route": "/",
                            "shows": ["code"],
                            "actions": ["ACT-go"],
                            "satisfies": ["AC-ok"],
                        }
                    ],
                    "actions": [{"id": "ACT-go", "name": "go", "invokes": "CTR-api#go"}],
                },
            }
        ],
    }
    m = Manifest.model_validate(data)
    assert m.modules[0].ui.views[0].satisfies == ["AC-ok"]


def test_stack_accepts_bare_and_versioned_languages() -> None:
    m = Manifest.model_validate(
        {**MINIMAL, "project": {"name": "demo", "stack": {"languages": ["python", {"name": "go", "version": "1.23"}]}}}
    )
    langs = m.project.stack.languages
    assert langs[0] == "python" and langs[1].version == "1.23"
```

- [ ] **Step 2: Run to verify failure**

Run: `uv run pytest tests/unit/test_model.py -v`
Expected: FAIL — `ModuleNotFoundError: avspec.model`

- [ ] **Step 3: Implement `src/avspec/model.py`**

```python
"""Pydantic models for the AVSpec 0.3 manifest — the canonical format definition."""

from __future__ import annotations

from typing import Literal

from pydantic import BaseModel, ConfigDict, Field


class StrictModel(BaseModel):
    model_config = ConfigDict(extra="forbid")


class Commands(StrictModel):
    install: str | None = None
    test: str | None = None
    lint: str | None = None
    typecheck: str | None = None
    arch: str | None = None


class Language(StrictModel):
    name: str
    version: str | None = None


class Stack(StrictModel):
    languages: list[str | Language] = Field(default_factory=list)
    package_manager: str | None = None
    frameworks: list[str] = Field(default_factory=list)
    bdd: str | None = None
    commands: Commands | None = None


class Project(StrictModel):
    name: str
    description: str | None = None
    status: Literal["draft", "ready", "built"] = "draft"
    stack: Stack | None = None


class ConstitutionEntry(StrictModel):
    id: str
    statement: str


class AcceptanceCriterion(StrictModel):
    id: str
    statement: str
    test: str | None = None  # "relative/path.feature#scenario name"


class Requirement(StrictModel):
    id: str
    title: str
    rationale: str | None = None
    acceptance: list[AcceptanceCriterion] = Field(default_factory=list)


class Contract(StrictModel):
    id: str
    type: str  # openapi | asyncapi | jsonschema — free-form, never an enum
    path: str


class Boundaries(StrictModel):
    may_import: list[str] = Field(default_factory=list)


class View(StrictModel):
    id: str
    name: str
    route: str | None = None  # web
    invocation: str | None = None  # cli / tui
    purpose: str | None = None
    shows: list[str] = Field(default_factory=list)
    actions: list[str] = Field(default_factory=list)
    navigates_to: list[str] = Field(default_factory=list)
    satisfies: list[str] = Field(default_factory=list)


class Action(StrictModel):
    id: str
    name: str
    invokes: str | None = None  # "CTR-<id>#<operation>"


class UI(StrictModel):
    kind: Literal["web", "cli", "tui", "none"]
    entry: str | None = None
    views: list[View] = Field(default_factory=list)
    actions: list[Action] = Field(default_factory=list)


class Module(StrictModel):
    id: str
    name: str
    responsibility: str | None = None
    stack: Stack | None = None
    boundaries: Boundaries | None = None
    contracts: list[Contract] = Field(default_factory=list)
    ui: UI | None = None


class Manifest(StrictModel):
    avspec: Literal["0.3"]
    project: Project
    constitution: list[ConstitutionEntry] = Field(default_factory=list)
    requirements: list[Requirement] = Field(default_factory=list)
    modules: list[Module] = Field(default_factory=list)
```

- [ ] **Step 4: Run to verify pass**

Run: `uv run pytest tests/unit/test_model.py -v`
Expected: 5 passed

- [ ] **Step 5: Lint, type-check, commit**

```bash
uv run ruff check . && uv run ty check
git add src/avspec/model.py tests/unit/test_model.py
git commit -m "feat: pydantic models for the 0.3 manifest"
```

---

### Task 4: Loading

**Files:**
- Create: `src/avspec/loading.py`
- Test: `tests/unit/test_loading.py`, `tests/conftest.py`

**Interfaces:**
- Consumes: `Manifest` (Task 3), `Finding`/`Severity` (Task 2).
- Produces: `MANIFEST_NAME = "avspec.yaml"`; frozen dataclass `LoadResult(manifest: Manifest | None, findings: list[Finding])`; `load(spec_dir: Path) -> LoadResult`. Error codes: `MANIFEST_MISSING`, `MANIFEST_UNPARSEABLE`, `MANIFEST_INVALID` (one finding per Pydantic error, message `"<dotted.loc>: <msg>"`).
- Also produces the shared test helpers in `tests/conftest.py` used by every later task.

- [ ] **Step 1: Write `tests/conftest.py`**

```python
from pathlib import Path

from ruamel.yaml import YAML

from avspec.analysis import Spec  # noqa: F401  (re-exported for step files)  -- added in Task 5
from avspec.model import Manifest

MINIMAL: dict = {"avspec": "0.3", "project": {"name": "demo", "status": "draft"}}


def write_manifest(spec_dir: Path, data: dict) -> None:
    spec_dir.mkdir(parents=True, exist_ok=True)
    yaml = YAML()
    with (spec_dir / "avspec.yaml").open("w", encoding="utf-8") as fh:
        yaml.dump(data, fh)


def make_spec(spec_dir: Path, data: dict):
    from avspec.analysis import Spec

    return Spec(dir=spec_dir, manifest=Manifest.model_validate(data))
```

Until Task 5 exists, comment the `Spec` import out — Step 4 of Task 5 uncomments it. (`write_manifest`/`MINIMAL` are needed now; `make_spec` from Task 6 on.)

- [ ] **Step 2: Write the failing tests**

`tests/unit/test_loading.py`:

```python
from pathlib import Path

from avspec.findings import Severity
from avspec.loading import load
from tests.conftest import MINIMAL, write_manifest


def test_missing_manifest(tmp_path: Path) -> None:
    result = load(tmp_path)
    assert result.manifest is None
    assert [f.code for f in result.findings] == ["MANIFEST_MISSING"]
    assert result.findings[0].severity is Severity.ERROR


def test_unparseable_yaml(tmp_path: Path) -> None:
    (tmp_path / "avspec.yaml").write_text("a: [unclosed", encoding="utf-8")
    result = load(tmp_path)
    assert [f.code for f in result.findings] == ["MANIFEST_UNPARSEABLE"]


def test_invalid_manifest_reports_location(tmp_path: Path) -> None:
    write_manifest(tmp_path, {"avspec": "0.3", "project": {"nam": "typo"}})
    result = load(tmp_path)
    assert result.manifest is None
    assert all(f.code == "MANIFEST_INVALID" for f in result.findings)
    assert any("project" in f.message for f in result.findings)


def test_valid_manifest_loads(tmp_path: Path) -> None:
    write_manifest(tmp_path, MINIMAL)
    result = load(tmp_path)
    assert result.findings == []
    assert result.manifest.project.name == "demo"
```

If `from tests.conftest import ...` fails with import errors, add empty `tests/__init__.py` and `tests/unit/__init__.py` files — pytest's rootdir handling otherwise resolves `tests` as a namespace package.

- [ ] **Step 3: Run to verify failure**

Run: `uv run pytest tests/unit/test_loading.py -v`
Expected: FAIL — `ModuleNotFoundError: avspec.loading`

- [ ] **Step 4: Implement `src/avspec/loading.py`**

```python
"""Read avspec.yaml and validate it into a Manifest."""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path

from pydantic import ValidationError
from ruamel.yaml import YAML
from ruamel.yaml.error import YAMLError

from avspec.findings import Finding, Severity
from avspec.model import Manifest

MANIFEST_NAME = "avspec.yaml"


@dataclass(frozen=True)
class LoadResult:
    manifest: Manifest | None
    findings: list[Finding]


def load(spec_dir: Path) -> LoadResult:
    path = spec_dir / MANIFEST_NAME
    if not path.is_file():
        return LoadResult(
            None,
            [
                Finding(
                    code="MANIFEST_MISSING",
                    severity=Severity.ERROR,
                    message=f"{MANIFEST_NAME} not found in {spec_dir}",
                    question="Is this the right spec directory, or should avspec.yaml be created?",
                )
            ],
        )
    yaml = YAML(typ="rt")
    try:
        data = yaml.load(path.read_text(encoding="utf-8"))
    except YAMLError as exc:
        return LoadResult(
            None,
            [
                Finding(
                    code="MANIFEST_UNPARSEABLE",
                    severity=Severity.ERROR,
                    message=f"{MANIFEST_NAME}: {exc}",
                )
            ],
        )
    try:
        manifest = Manifest.model_validate(data)
    except ValidationError as exc:
        findings = [
            Finding(
                code="MANIFEST_INVALID",
                severity=Severity.ERROR,
                message=f"{'.'.join(str(part) for part in err['loc'])}: {err['msg']}",
            )
            for err in exc.errors()
        ]
        return LoadResult(None, findings)
    return LoadResult(manifest, [])
```

- [ ] **Step 5: Run to verify pass**

Run: `uv run pytest tests/unit/test_loading.py -v`
Expected: 4 passed

- [ ] **Step 6: Lint, type-check, commit**

```bash
uv run ruff check . && uv run ty check
git add src/avspec/loading.py tests/unit/test_loading.py tests/conftest.py
git commit -m "feat: manifest loading with structured error findings"
```

---
### Task 5: Analyzer skeleton and rule registry

**Files:**
- Create: `src/avspec/analysis.py`, `src/avspec/rules/__init__.py`
- Modify: `tests/conftest.py` (uncomment the `Spec` import)
- Test: `tests/unit/test_analysis.py`

**Interfaces:**
- Consumes: `load()` (Task 4), `ordered()` (Task 2).
- Produces: frozen dataclasses `Spec(dir: Path, manifest: Manifest)` and `Report(status: str, findings: list[Finding])` with properties `counts: dict[str, int]` (keys `"error"`,`"todo"`,`"warn"`) and `ok: bool`; `analyze(spec_dir: Path) -> Report`; in `avspec.rules`: `RULES: list[Rule]`, decorator `rule(fn)` where `Rule = Callable[[Spec], Iterable[Finding]]`.

- [ ] **Step 1: Write the failing tests**

`tests/unit/test_analysis.py`:

```python
from pathlib import Path

from avspec.analysis import Report, analyze
from avspec.findings import Finding, Severity
from tests.conftest import MINIMAL, write_manifest


def test_missing_manifest_is_error_report(tmp_path: Path) -> None:
    report = analyze(tmp_path)
    assert report.status == "unknown"
    assert report.counts["error"] == 1
    assert not report.ok


def test_draft_with_todos_is_ok() -> None:
    todo = Finding(code="NO_STACK", severity=Severity.TODO, message="m")
    assert Report(status="draft", findings=[todo]).ok


def test_ready_with_todos_is_not_ok() -> None:
    todo = Finding(code="NO_STACK", severity=Severity.TODO, message="m")
    assert not Report(status="ready", findings=[todo]).ok


def test_error_always_fails() -> None:
    err = Finding(code="DUPLICATE_ID", severity=Severity.ERROR, message="m")
    assert not Report(status="draft", findings=[err]).ok


def test_analyze_runs_registry_on_valid_manifest(tmp_path: Path) -> None:
    write_manifest(tmp_path, MINIMAL)
    report = analyze(tmp_path)
    assert report.status == "draft"
    # Registry is empty until Task 6; a valid minimal manifest yields no findings yet.
    assert report.counts["error"] == 0
```

- [ ] **Step 2: Run to verify failure**

Run: `uv run pytest tests/unit/test_analysis.py -v`
Expected: FAIL — `ModuleNotFoundError: avspec.analysis`

- [ ] **Step 3: Implement**

`src/avspec/analysis.py`:

```python
"""Analyzer: run the rule registry over a spec directory."""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path

from avspec.findings import Finding, Severity, ordered
from avspec.loading import load
from avspec.model import Manifest


@dataclass(frozen=True)
class Spec:
    dir: Path
    manifest: Manifest


@dataclass(frozen=True)
class Report:
    status: str
    findings: list[Finding]

    @property
    def counts(self) -> dict[str, int]:
        return {
            severity.value: sum(1 for f in self.findings if f.severity is severity)
            for severity in Severity
        }

    @property
    def ok(self) -> bool:
        if self.counts["error"]:
            return False
        return not (self.status in ("ready", "built") and self.counts["todo"])


def analyze(spec_dir: Path) -> Report:
    result = load(spec_dir)
    if result.manifest is None:
        return Report(status="unknown", findings=ordered(result.findings))
    # Imported here, not at module top: rule modules import Spec from this module.
    from avspec.rules import RULES

    spec = Spec(dir=spec_dir, manifest=result.manifest)
    findings: list[Finding] = []
    for run in RULES:
        findings.extend(run(spec))
    return Report(status=result.manifest.project.status, findings=ordered(findings))
```

`src/avspec/rules/__init__.py` (rule modules are added by Tasks 6–8; their imports are appended then):

```python
"""Rule registry. A rule is a pure function (Spec) -> Iterable[Finding]."""

from __future__ import annotations

from collections.abc import Callable, Iterable
from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from avspec.analysis import Spec
    from avspec.findings import Finding

Rule = Callable[["Spec"], Iterable["Finding"]]

RULES: list[Rule] = []


def rule(fn: Rule) -> Rule:
    """Register a rule. A rule module not imported below silently does nothing."""
    RULES.append(fn)
    return fn
```

Uncomment the `Spec` import in `tests/conftest.py`.

- [ ] **Step 4: Run to verify pass**

Run: `uv run pytest tests/unit/test_analysis.py -v`
Expected: 5 passed

- [ ] **Step 5: Lint, type-check, commit**

```bash
uv run ruff check . && uv run ty check
git add src/avspec/analysis.py src/avspec/rules/__init__.py tests/unit/test_analysis.py tests/conftest.py
git commit -m "feat: analyzer skeleton with rule registry and report semantics"
```

---

### Task 6: Well-formedness rules (errors)

**Files:**
- Create: `src/avspec/rules/wellformed.py`
- Modify: `src/avspec/rules/__init__.py` (append registration import)
- Test: `tests/unit/test_wellformed.py`

**Interfaces:**
- Consumes: `Spec`, `Finding`, `Severity`, `rule`.
- Produces error findings: `DUPLICATE_ID`, `PREFIX_MISMATCH`, `DANGLING_REF`, `BOUNDARY_CYCLE`. Reference resolution rules: `boundaries.may_import` → module ids; `view.satisfies` → acceptance ids; `view.actions` → action ids in the same module; `view.navigates_to` → view ids in the same module; `ui.entry` → view ids in the same module; `action.invokes` (the part before `#`) → contract ids anywhere in the manifest (cross-module invocation is the point of contracts).

- [ ] **Step 1: Write the failing tests**

`tests/unit/test_wellformed.py`:

```python
import copy
from pathlib import Path

from avspec.rules import wellformed
from tests.conftest import MINIMAL, make_spec


def codes(findings) -> list[str]:
    return sorted(f.code for f in findings)


def base() -> dict:
    return copy.deepcopy(MINIMAL)


def test_duplicate_ids(tmp_path: Path) -> None:
    data = base()
    data["requirements"] = [{"id": "REQ-a", "title": "A"}, {"id": "REQ-a", "title": "B"}]
    assert codes(wellformed.duplicate_ids(make_spec(tmp_path, data))) == ["DUPLICATE_ID"]


def test_prefix_mismatch(tmp_path: Path) -> None:
    data = base()
    data["modules"] = [{"id": "CMP-oops", "name": "x"}]
    assert codes(wellformed.prefix_mismatch(make_spec(tmp_path, data))) == ["PREFIX_MISMATCH"]


def test_dangling_may_import(tmp_path: Path) -> None:
    data = base()
    data["modules"] = [{"id": "MOD-a", "name": "a", "boundaries": {"may_import": ["MOD-ghost"]}}]
    assert codes(wellformed.dangling_refs(make_spec(tmp_path, data))) == ["DANGLING_REF"]


def test_dangling_ui_refs(tmp_path: Path) -> None:
    data = base()
    data["modules"] = [
        {
            "id": "MOD-web",
            "name": "web",
            "ui": {
                "kind": "web",
                "entry": "VIEW-ghost",
                "views": [
                    {
                        "id": "VIEW-a",
                        "name": "A",
                        "actions": ["ACT-ghost"],
                        "navigates_to": ["VIEW-ghost"],
                        "satisfies": ["AC-ghost"],
                    }
                ],
                "actions": [{"id": "ACT-a", "name": "a", "invokes": "CTR-ghost#op"}],
            },
        }
    ]
    found = codes(wellformed.dangling_refs(make_spec(tmp_path, data)))
    assert found == ["DANGLING_REF"] * 5  # entry, action ref, navigation, satisfies, invokes


def test_boundary_cycle(tmp_path: Path) -> None:
    data = base()
    data["modules"] = [
        {"id": "MOD-a", "name": "a", "boundaries": {"may_import": ["MOD-b"]}},
        {"id": "MOD-b", "name": "b", "boundaries": {"may_import": ["MOD-a"]}},
    ]
    assert "BOUNDARY_CYCLE" in codes(wellformed.boundary_cycle(make_spec(tmp_path, data)))


def test_clean_spec_has_no_errors(tmp_path: Path) -> None:
    data = base()
    data["modules"] = [
        {"id": "MOD-a", "name": "a", "boundaries": {"may_import": ["MOD-b"]}},
        {"id": "MOD-b", "name": "b", "boundaries": {"may_import": []}},
    ]
    spec = make_spec(tmp_path, data)
    assert (
        codes(wellformed.duplicate_ids(spec))
        + codes(wellformed.prefix_mismatch(spec))
        + codes(wellformed.dangling_refs(spec))
        + codes(wellformed.boundary_cycle(spec))
        == []
    )
```

- [ ] **Step 2: Run to verify failure**

Run: `uv run pytest tests/unit/test_wellformed.py -v`
Expected: FAIL — `ImportError: cannot import name 'wellformed'`

- [ ] **Step 3: Implement `src/avspec/rules/wellformed.py`**

```python
"""Well-formedness rules — inconsistencies. Always fail."""

from __future__ import annotations

from collections.abc import Iterable

from avspec.analysis import Spec
from avspec.findings import Finding, Severity
from avspec.model import Manifest, Module
from avspec.rules import rule

PREFIXES: dict[str, str] = {
    "constitution entry": "CON-",
    "requirement": "REQ-",
    "acceptance criterion": "AC-",
    "module": "MOD-",
    "contract": "CTR-",
    "view": "VIEW-",
    "action": "ACT-",
}


def _all_ids(manifest: Manifest) -> list[tuple[str, str]]:
    """(kind, id) for every identified object in the manifest."""
    out: list[tuple[str, str]] = [("constitution entry", c.id) for c in manifest.constitution]
    for req in manifest.requirements:
        out.append(("requirement", req.id))
        out.extend(("acceptance criterion", ac.id) for ac in req.acceptance)
    for module in manifest.modules:
        out.append(("module", module.id))
        out.extend(("contract", c.id) for c in module.contracts)
        if module.ui:
            out.extend(("view", v.id) for v in module.ui.views)
            out.extend(("action", a.id) for a in module.ui.actions)
    return out


@rule
def duplicate_ids(spec: Spec) -> Iterable[Finding]:
    seen: set[str] = set()
    for _, obj_id in _all_ids(spec.manifest):
        if obj_id in seen:
            yield Finding(
                code="DUPLICATE_ID",
                severity=Severity.ERROR,
                ref=obj_id,
                message=f"ID {obj_id} is declared more than once.",
            )
        seen.add(obj_id)


@rule
def prefix_mismatch(spec: Spec) -> Iterable[Finding]:
    for kind, obj_id in _all_ids(spec.manifest):
        prefix = PREFIXES[kind]
        if not obj_id.startswith(prefix):
            yield Finding(
                code="PREFIX_MISMATCH",
                severity=Severity.ERROR,
                ref=obj_id,
                message=f"A {kind} id must start with {prefix!r}; got {obj_id!r}.",
            )


def _dangling(ref: str, known: set[str], owner: str, field: str) -> Finding:
    return Finding(
        code="DANGLING_REF",
        severity=Severity.ERROR,
        ref=owner,
        message=f"{owner}.{field} references {ref!r}, which does not exist.",
    )


@rule
def dangling_refs(spec: Spec) -> Iterable[Finding]:
    manifest = spec.manifest
    module_ids = {m.id for m in manifest.modules}
    ac_ids = {ac.id for req in manifest.requirements for ac in req.acceptance}
    contract_ids = {c.id for m in manifest.modules for c in m.contracts}
    for module in manifest.modules:
        if module.boundaries:
            for target in module.boundaries.may_import:
                if target not in module_ids:
                    yield _dangling(target, module_ids, module.id, "boundaries.may_import")
        if module.ui is None:
            continue
        view_ids = {v.id for v in module.ui.views}
        action_ids = {a.id for a in module.ui.actions}
        if module.ui.entry is not None and module.ui.entry not in view_ids:
            yield _dangling(module.ui.entry, view_ids, module.id, "ui.entry")
        for view in module.ui.views:
            for act in view.actions:
                if act not in action_ids:
                    yield _dangling(act, action_ids, view.id, "actions")
            for nav in view.navigates_to:
                if nav not in view_ids:
                    yield _dangling(nav, view_ids, view.id, "navigates_to")
            for ac in view.satisfies:
                if ac not in ac_ids:
                    yield _dangling(ac, ac_ids, view.id, "satisfies")
        for action in module.ui.actions:
            if action.invokes is not None:
                contract_ref = action.invokes.split("#", 1)[0]
                if contract_ref not in contract_ids:
                    yield _dangling(contract_ref, contract_ids, action.id, "invokes")


@rule
def boundary_cycle(spec: Spec) -> Iterable[Finding]:
    graph: dict[str, list[str]] = {
        m.id: (m.boundaries.may_import if m.boundaries else []) for m in spec.manifest.modules
    }
    done: set[str] = set()

    def visit(node: str, stack: tuple[str, ...]) -> Iterable[Finding]:
        if node in stack:
            cycle = " -> ".join((*stack[stack.index(node) :], node))
            yield Finding(
                code="BOUNDARY_CYCLE",
                severity=Severity.ERROR,
                ref=node,
                message=f"Module import cycle: {cycle}.",
            )
            return
        if node in done:
            return
        done.add(node)
        for dep in graph.get(node, []):
            yield from visit(dep, (*stack, node))

    for module_id in graph:
        yield from visit(module_id, ())
```

Append to `src/avspec/rules/__init__.py` (bottom of file):

```python
# Imported for registration side effects — a rule module missing here does nothing.
from avspec.rules import wellformed  # noqa: E402, F401
```

- [ ] **Step 4: Run to verify pass**

Run: `uv run pytest tests/unit/test_wellformed.py tests/unit/test_analysis.py -v`
Expected: all pass (the analysis test on the minimal manifest still yields no errors — minimal is well-formed)

- [ ] **Step 5: Lint, type-check, commit**

```bash
uv run ruff check . && uv run ty check
git add src/avspec/rules/wellformed.py src/avspec/rules/__init__.py tests/unit/test_wellformed.py
git commit -m "feat: well-formedness rules — duplicates, prefixes, dangling refs, cycles"
```

---

### Task 7: Completeness rules (todos fire on absence)

**Files:**
- Create: `src/avspec/rules/completeness.py`
- Modify: `src/avspec/rules/__init__.py` (append registration import)
- Test: `tests/unit/test_completeness.py`

**Interfaces:**
- Consumes: `Spec`, `Finding`, `Severity`, `rule`.
- Produces todo findings: `NO_STACK`, `NO_CONSTITUTION`, `NO_REQUIREMENTS`, `REQ_NO_AC`, `AC_NO_TEST`, `TEST_FILE_MISSING`, `NO_MODULES`, `MOD_NO_RESPONSIBILITY`, `MOD_NO_BOUNDARIES`, `CONTRACT_FILE_MISSING`; error finding `CONTRACT_UNPARSEABLE`. Every todo carries a `question` — these strings are the interview's prompts.

- [ ] **Step 1: Write the failing tests**

`tests/unit/test_completeness.py`:

```python
import copy
from pathlib import Path

from avspec.findings import Severity
from avspec.rules import completeness
from tests.conftest import MINIMAL, make_spec


def codes(findings) -> list[str]:
    return sorted(f.code for f in findings)


def base() -> dict:
    return copy.deepcopy(MINIMAL)


def test_empty_spec_fires_all_absence_todos(tmp_path: Path) -> None:
    spec = make_spec(tmp_path, base())
    found = (
        codes(completeness.stack_declared(spec))
        + codes(completeness.constitution_present(spec))
        + codes(completeness.requirements_present(spec))
        + codes(completeness.modules_present(spec))
    )
    assert found == ["NO_CONSTITUTION", "NO_MODULES", "NO_REQUIREMENTS", "NO_STACK"]


def test_every_todo_has_a_question(tmp_path: Path) -> None:
    spec = make_spec(tmp_path, base())
    for run in (
        completeness.stack_declared,
        completeness.constitution_present,
        completeness.requirements_present,
        completeness.modules_present,
    ):
        for finding in run(spec):
            assert finding.question, f"{finding.code} has no question"


def test_req_no_ac_and_ac_no_test(tmp_path: Path) -> None:
    data = base()
    data["requirements"] = [
        {"id": "REQ-empty", "title": "no acs"},
        {"id": "REQ-untested", "title": "x", "acceptance": [{"id": "AC-u", "statement": "s"}]},
    ]
    spec = make_spec(tmp_path, data)
    assert codes(completeness.requirements_present(spec)) == ["AC_NO_TEST", "REQ_NO_AC"]


def test_test_file_missing_and_present(tmp_path: Path) -> None:
    data = base()
    data["requirements"] = [
        {
            "id": "REQ-x",
            "title": "x",
            "acceptance": [{"id": "AC-x", "statement": "s", "test": "verification/x.feature#ok"}],
        }
    ]
    spec = make_spec(tmp_path, data)
    assert codes(completeness.test_files_exist(spec)) == ["TEST_FILE_MISSING"]
    (tmp_path / "verification").mkdir(parents=True)
    (tmp_path / "verification" / "x.feature").write_text("Feature: x", encoding="utf-8")
    assert codes(completeness.test_files_exist(spec)) == []


def test_module_todos(tmp_path: Path) -> None:
    data = base()
    data["modules"] = [{"id": "MOD-bare", "name": "bare"}]
    spec = make_spec(tmp_path, data)
    assert codes(completeness.modules_present(spec)) == [
        "MOD_NO_BOUNDARIES",
        "MOD_NO_RESPONSIBILITY",
    ]


def test_empty_may_import_is_a_valid_answer(tmp_path: Path) -> None:
    data = base()
    data["modules"] = [
        {"id": "MOD-leaf", "name": "leaf", "responsibility": "r", "boundaries": {"may_import": []}}
    ]
    assert codes(completeness.modules_present(make_spec(tmp_path, data))) == []


def test_contract_file_missing_unparseable_and_ok(tmp_path: Path) -> None:
    data = base()
    data["modules"] = [
        {
            "id": "MOD-api",
            "name": "api",
            "responsibility": "r",
            "boundaries": {"may_import": []},
            "contracts": [{"id": "CTR-x", "type": "openapi", "path": "contracts/x.yaml"}],
        }
    ]
    spec = make_spec(tmp_path, data)
    assert codes(completeness.contract_files(spec)) == ["CONTRACT_FILE_MISSING"]
    contract = tmp_path / "contracts" / "x.yaml"
    contract.parent.mkdir(parents=True)
    contract.write_text("a: [unclosed", encoding="utf-8")
    found = list(completeness.contract_files(spec))
    assert codes(found) == ["CONTRACT_UNPARSEABLE"]
    assert found[0].severity is Severity.ERROR
    contract.write_text("openapi: 3.0.3", encoding="utf-8")
    assert codes(completeness.contract_files(spec)) == []
```

- [ ] **Step 2: Run to verify failure**

Run: `uv run pytest tests/unit/test_completeness.py -v`
Expected: FAIL — `ImportError: cannot import name 'completeness'`

- [ ] **Step 3: Implement `src/avspec/rules/completeness.py`**

```python
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
            "No stack is declared for the project.",
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
            try:
                yaml.load(path.read_text(encoding="utf-8"))
            except YAMLError as exc:
                yield Finding(
                    code="CONTRACT_UNPARSEABLE",
                    severity=Severity.ERROR,
                    ref=contract.id,
                    message=f"{contract.path} is not parseable: {exc}",
                )
```

Append to `src/avspec/rules/__init__.py` registration imports:

```python
from avspec.rules import completeness, wellformed  # noqa: E402, F401
```

(Replace the Task 6 import line — keep one combined, alphabetized import.)

- [ ] **Step 4: Run to verify pass**

Run: `uv run pytest tests/unit -v`
Expected: all pass. Note `tests/unit/test_analysis.py::test_analyze_runs_registry_on_valid_manifest` still passes — todos are not errors.

- [ ] **Step 5: Lint, type-check, commit**

```bash
uv run ruff check . && uv run ty check
git add src/avspec/rules/completeness.py src/avspec/rules/__init__.py tests/unit/test_completeness.py
git commit -m "feat: completeness rules — todos fire on absence, with interview questions"
```

---

### Task 8: UI rules

**Files:**
- Create: `src/avspec/rules/ui.py`
- Modify: `src/avspec/rules/__init__.py` (append registration import)
- Test: `tests/unit/test_ui_rules.py`

**Interfaces:**
- Consumes: `Spec`, `Finding`, `Severity`, `rule`.
- Produces todo findings: `UI_NO_ENTRY` (views exist, no entry declared), `VIEW_NO_AC` (view satisfies nothing), `VIEW_UNREACHABLE` (not reachable from entry via `navigates_to`; only checked when entry is set and valid), `ACTION_ORPHAN` (action no view references).

- [ ] **Step 1: Write the failing tests**

`tests/unit/test_ui_rules.py`:

```python
import copy
from pathlib import Path

from avspec.rules import ui as ui_rules
from tests.conftest import MINIMAL, make_spec


def codes(findings) -> list[str]:
    return sorted(f.code for f in findings)


def web_module(ui_block: dict) -> dict:
    data = copy.deepcopy(MINIMAL)
    data["modules"] = [
        {
            "id": "MOD-web",
            "name": "web",
            "responsibility": "UI",
            "boundaries": {"may_import": []},
            "ui": ui_block,
        }
    ]
    return data


def view(view_id: str, **overrides) -> dict:
    base = {"id": view_id, "name": view_id, "satisfies": ["AC-x"]}
    base.update(overrides)
    return base


def test_no_entry(tmp_path: Path) -> None:
    data = web_module({"kind": "web", "views": [view("VIEW-a")]})
    assert "UI_NO_ENTRY" in codes(ui_rules.ui_shape(make_spec(tmp_path, data)))


def test_view_without_ac(tmp_path: Path) -> None:
    data = web_module({"kind": "web", "entry": "VIEW-a", "views": [view("VIEW-a", satisfies=[])]})
    assert "VIEW_NO_AC" in codes(ui_rules.ui_shape(make_spec(tmp_path, data)))


def test_unreachable_view(tmp_path: Path) -> None:
    data = web_module(
        {"kind": "web", "entry": "VIEW-a", "views": [view("VIEW-a"), view("VIEW-island")]}
    )
    found = ui_rules.ui_shape(make_spec(tmp_path, data))
    unreachable = [f for f in found if f.code == "VIEW_UNREACHABLE"]
    assert [f.ref for f in unreachable] == ["VIEW-island"]


def test_navigation_makes_reachable(tmp_path: Path) -> None:
    data = web_module(
        {
            "kind": "web",
            "entry": "VIEW-a",
            "views": [view("VIEW-a", navigates_to=["VIEW-b"]), view("VIEW-b")],
        }
    )
    assert "VIEW_UNREACHABLE" not in codes(ui_rules.ui_shape(make_spec(tmp_path, data)))


def test_orphan_action(tmp_path: Path) -> None:
    data = web_module(
        {
            "kind": "web",
            "entry": "VIEW-a",
            "views": [view("VIEW-a")],
            "actions": [{"id": "ACT-unused", "name": "unused"}],
        }
    )
    assert "ACTION_ORPHAN" in codes(ui_rules.ui_shape(make_spec(tmp_path, data)))


def test_kind_none_is_quiet(tmp_path: Path) -> None:
    data = web_module({"kind": "none"})
    assert codes(ui_rules.ui_shape(make_spec(tmp_path, data))) == []
```

- [ ] **Step 2: Run to verify failure**

Run: `uv run pytest tests/unit/test_ui_rules.py -v`
Expected: FAIL — `ImportError: cannot import name 'ui'`

- [ ] **Step 3: Implement `src/avspec/rules/ui.py`**

```python
"""UI rules — the screen graph must be complete, connected, and traceable."""

from __future__ import annotations

from collections.abc import Iterable

from avspec.analysis import Spec
from avspec.findings import Finding, Severity
from avspec.rules import rule


def _todo(code: str, message: str, question: str, ref: str | None = None) -> Finding:
    return Finding(code=code, severity=Severity.TODO, message=message, question=question, ref=ref)


@rule
def ui_shape(spec: Spec) -> Iterable[Finding]:
    for module in spec.manifest.modules:
        ui = module.ui
        if ui is None or ui.kind == "none":
            continue
        view_ids = {v.id for v in ui.views}
        if ui.views and ui.entry is None:
            yield _todo(
                "UI_NO_ENTRY",
                f"{module.id} declares views but no entry point.",
                f"Which view is the entry point of {module.id} — "
                "the first screen a user lands on?",
                ref=module.id,
            )
        for view in ui.views:
            if not view.satisfies:
                yield _todo(
                    "VIEW_NO_AC",
                    f"{view.id} satisfies no acceptance criterion.",
                    f"Which acceptance criteria does {view.id} ({view.name}) serve? "
                    "A view no requirement needs should be removed.",
                    ref=view.id,
                )
        if ui.entry is not None and ui.entry in view_ids:
            nav = {v.id: v.navigates_to for v in ui.views}
            reachable: set[str] = set()
            frontier = [ui.entry]
            while frontier:
                current = frontier.pop()
                if current in reachable:
                    continue
                reachable.add(current)
                frontier.extend(t for t in nav.get(current, []) if t in view_ids)
            for view in ui.views:
                if view.id not in reachable:
                    yield _todo(
                        "VIEW_UNREACHABLE",
                        f"{view.id} cannot be reached from {ui.entry}.",
                        f"How does a user get to {view.id} ({view.name})? "
                        "Add it to some view's navigates_to, or remove it.",
                        ref=view.id,
                    )
        referenced = {act for v in ui.views for act in v.actions}
        for action in ui.actions:
            if action.id not in referenced:
                yield _todo(
                    "ACTION_ORPHAN",
                    f"{action.id} is not offered by any view.",
                    f"Which view offers {action.id} ({action.name})? "
                    "Add it to that view's actions, or remove it.",
                    ref=action.id,
                )
```

Update the registration import in `src/avspec/rules/__init__.py` to:

```python
from avspec.rules import completeness, ui, wellformed  # noqa: E402, F401
```

- [ ] **Step 4: Run to verify pass**

Run: `uv run pytest tests/unit -v`
Expected: all pass

- [ ] **Step 5: Lint, type-check, commit**

```bash
uv run ruff check . && uv run ty check
git add src/avspec/rules/ui.py src/avspec/rules/__init__.py tests/unit/test_ui_rules.py
git commit -m "feat: ui rules — entry, traceability, reachability, orphan actions"
```

---
### Task 9: CLI — `verify` and `next` (pytest-bdd)

**Files:**
- Create: `src/avspec/cli.py`, `tests/features/verify.feature`, `tests/features/next.feature`, `tests/steps/test_cli_steps.py`
- Test: the feature files are the tests

**Interfaces:**
- Consumes: `analyze()`, `Report` (Task 5), `Severity` (Task 2).
- Produces: `app` (typer application, entry point `avspec = "avspec.cli:app"` from Task 1). `avspec verify [dir] [--json]` exits 0 iff `report.ok`. `avspec next [dir] [--json]` always exits 0 and lists errors+todos in `ordered()` order. JSON shapes: verify → `{"status", "ok", "counts", "findings": [{code, severity, message, ref, question}]}`; next → `{"status", "findings": [...]}` filtered to errors and todos.

- [ ] **Step 1: Write the failing feature files**

`tests/features/verify.feature`:

```gherkin
Feature: avspec verify
  The CI gate. Exit 0 iff the spec passes for its declared status.

  Scenario: a directory without a manifest fails
    Given an empty spec directory
    When I run avspec verify --json
    Then the exit code is 1
    And the report contains a "MANIFEST_MISSING" finding with severity "error"

  Scenario: a draft spec with only todos passes
    Given a minimal draft spec
    When I run avspec verify --json
    Then the exit code is 0
    And the report contains a "NO_REQUIREMENTS" finding with severity "todo"

  Scenario: a ready spec with todos fails
    Given a minimal spec with status "ready"
    When I run avspec verify --json
    Then the exit code is 1
```

`tests/features/next.feature`:

```gherkin
Feature: avspec next
  The interview's brain: the gap queue in authoring order.

  Scenario: the stack question comes first on an empty spec
    Given a minimal draft spec
    When I run avspec next --json
    Then the exit code is 0
    And the first finding code is "NO_STACK"
    And every finding has a question
```

`tests/steps/test_cli_steps.py`:

```python
import json
from pathlib import Path

import pytest
from pytest_bdd import given, parsers, scenarios, then, when
from typer.testing import CliRunner

from avspec.cli import app
from tests.conftest import MINIMAL, write_manifest

scenarios("../features")


@pytest.fixture()
def spec_dir(tmp_path: Path) -> Path:
    return tmp_path / "spec"


@pytest.fixture()
def cli_result() -> dict:
    return {}


@given("an empty spec directory")
def empty_dir(spec_dir: Path) -> None:
    spec_dir.mkdir()


@given("a minimal draft spec")
def minimal_spec(spec_dir: Path) -> None:
    write_manifest(spec_dir, MINIMAL)


@given(parsers.parse('a minimal spec with status "{status}"'))
def spec_with_status(spec_dir: Path, status: str) -> None:
    data = {**MINIMAL, "project": {**MINIMAL["project"], "status": status}}
    write_manifest(spec_dir, data)


@when(parsers.parse("I run avspec {command} --json"))
def run_cli(cli_result: dict, spec_dir: Path, command: str) -> None:
    result = CliRunner().invoke(app, [command, str(spec_dir), "--json"])
    cli_result["exit_code"] = result.exit_code
    cli_result["payload"] = json.loads(result.stdout)


@then(parsers.parse("the exit code is {code:d}"))
def check_exit(cli_result: dict, code: int) -> None:
    assert cli_result["exit_code"] == code


@then(parsers.parse('the report contains a "{code}" finding with severity "{severity}"'))
def check_finding(cli_result: dict, code: str, severity: str) -> None:
    findings = cli_result["payload"]["findings"]
    assert any(f["code"] == code and f["severity"] == severity for f in findings)


@then(parsers.parse('the first finding code is "{code}"'))
def check_first(cli_result: dict, code: str) -> None:
    assert cli_result["payload"]["findings"][0]["code"] == code


@then("every finding has a question")
def check_questions(cli_result: dict) -> None:
    assert all(f["question"] for f in cli_result["payload"]["findings"])
```

- [ ] **Step 2: Run to verify failure**

Run: `uv run pytest tests/steps -v`
Expected: FAIL — `ModuleNotFoundError: avspec.cli`

- [ ] **Step 3: Implement `src/avspec/cli.py`**

```python
"""Typer CLI: verify (the CI gate) and next (the interview's brain)."""

from __future__ import annotations

import json
from dataclasses import asdict
from pathlib import Path

import typer

from avspec.analysis import Report, analyze
from avspec.findings import Severity

app = typer.Typer(no_args_is_help=True, add_completion=False)


def _payload(report: Report) -> dict:
    return {
        "status": report.status,
        "ok": report.ok,
        "counts": report.counts,
        "findings": [asdict(f) for f in report.findings],
    }


def _print_human(report: Report) -> None:
    for finding in report.findings:
        ref = finding.ref or ""
        typer.echo(f"{finding.severity.value:<6} {finding.code:<22} {ref:<18} {finding.message}")
    counts = report.counts
    typer.echo(
        f"status={report.status} errors={counts['error']} "
        f"todos={counts['todo']} ok={report.ok}"
    )


@app.command()
def verify(
    spec_dir: Path = typer.Argument(Path(".")),
    as_json: bool = typer.Option(False, "--json", help="Machine-readable report."),
) -> None:
    """Verify a spec directory. Exit 0 iff the spec passes for its declared status."""
    report = analyze(spec_dir)
    if as_json:
        typer.echo(json.dumps(_payload(report), indent=2))
    else:
        _print_human(report)
    raise typer.Exit(0 if report.ok else 1)


@app.command(name="next")
def next_(
    spec_dir: Path = typer.Argument(Path(".")),
    as_json: bool = typer.Option(False, "--json", help="Machine-readable queue."),
) -> None:
    """Print the gap queue in authoring order — errors first, then todos."""
    report = analyze(spec_dir)
    actionable = [f for f in report.findings if f.severity in (Severity.ERROR, Severity.TODO)]
    if as_json:
        typer.echo(
            json.dumps({"status": report.status, "findings": [asdict(f) for f in actionable]}, indent=2)
        )
    else:
        for finding in actionable:
            typer.echo(f"[{finding.severity.value}] {finding.code}: {finding.question or finding.message}")
    raise typer.Exit(0)
```

- [ ] **Step 4: Run to verify pass**

Run: `uv run pytest -v`
Expected: all unit tests and all four scenarios pass

- [ ] **Step 5: Smoke the installed entry point**

Run: `uv run avspec verify examples 2>&1 | head -3; uv run avspec --help | head -5`
Expected: verify prints a MANIFEST_MISSING error line (examples/ has no manifest yet); help lists `verify` and `next`.

- [ ] **Step 6: Lint, type-check, commit**

```bash
uv run ruff check . && uv run ty check
git add src/avspec/cli.py tests/features tests/steps
git commit -m "feat: verify and next commands with json output, bdd-tested"
```

---

### Task 10: linkshort conformance example and CI

**Files:**
- Create: `examples/linkshort/avspec.yaml`, `examples/linkshort/contracts/links.openapi.yaml`, `examples/linkshort/verification/REQ-create-link.feature`, `examples/linkshort/verification/REQ-resolve-link.feature`
- Modify: `ci/verify.yml` (full rewrite)
- Test: `tests/unit/test_examples.py`

**Interfaces:**
- Consumes: `analyze()` (Task 5).
- Produces: a complete 0.3 spec that must reach `ok` at `status: ready` — the standing conformance fixture for every future rule change.

- [ ] **Step 1: Write the failing conformance test**

`tests/unit/test_examples.py`:

```python
from pathlib import Path

from avspec.analysis import analyze

LINKSHORT = Path(__file__).parents[2] / "examples" / "linkshort"


def test_linkshort_is_ready_and_clean() -> None:
    report = analyze(LINKSHORT)
    blockers = [f for f in report.findings if f.severity.value in ("error", "todo")]
    assert blockers == [], [f"{f.code}: {f.message}" for f in blockers]
    assert report.status == "ready"
    assert report.ok
```

Run: `uv run pytest tests/unit/test_examples.py -v`
Expected: FAIL — MANIFEST_MISSING (directory doesn't exist yet)

- [ ] **Step 2: Write `examples/linkshort/avspec.yaml`**

```yaml
avspec: "0.3"

project:
  name: linkshort
  description: A minimal URL shortener, specified end to end.
  status: ready
  stack:
    languages: [{ name: typescript, version: "5" }]
    package_manager: pnpm
    frameworks: [fastify]
    bdd: cucumber-js
    commands:
      install: pnpm install
      test: pnpm test
      lint: pnpm lint

constitution:
  - id: CON-layering
    statement: Dependencies flow api -> domain -> data; no back-edges.
  - id: CON-test-first
    statement: No endpoint is implemented before the test mapped to its AC exists and fails.

requirements:
  - id: REQ-create-link
    title: Create a short link
    rationale: Users need a compact code that maps to a long URL.
    acceptance:
      - id: AC-valid-url
        statement: When a valid URL is POSTed to /links, the system shall persist it and return 201 with a short code.
        test: verification/REQ-create-link.feature#valid url is shortened
      - id: AC-bad-url
        statement: If the submitted URL is malformed, then the system shall return 400 and persist nothing.
        test: verification/REQ-create-link.feature#malformed url is rejected
  - id: REQ-resolve-link
    title: Resolve a short link
    rationale: A short code must send the visitor to the original URL.
    acceptance:
      - id: AC-known-code
        statement: When a request targets a known short code, the system shall respond 302 to the original URL.
        test: verification/REQ-resolve-link.feature#known code redirects
      - id: AC-unknown-code
        statement: If a request targets an unknown short code, then the system shall respond 404.
        test: verification/REQ-resolve-link.feature#unknown code is not found

modules:
  - id: MOD-api
    name: api
    responsibility: HTTP edge — validates requests and maps them to the domain service.
    boundaries: { may_import: [MOD-domain] }
    contracts:
      - id: CTR-links
        type: openapi
        path: contracts/links.openapi.yaml
  - id: MOD-domain
    name: domain
    responsibility: Generates codes and orchestrates create and resolve.
    boundaries: { may_import: [MOD-data] }
  - id: MOD-data
    name: data
    responsibility: Persists and looks up code-to-URL mappings.
    boundaries: { may_import: [] }
  - id: MOD-web
    name: web-ui
    responsibility: Browser front end for creating links.
    boundaries: { may_import: [] }
    ui:
      kind: web
      entry: VIEW-create
      views:
        - id: VIEW-create
          name: Create link
          route: /
          purpose: Paste a URL, get a short code.
          shows: [target_url, short_code]
          actions: [ACT-create]
          satisfies: [AC-valid-url, AC-bad-url]
      actions:
        - id: ACT-create
          name: create_link
          invokes: CTR-links#createLink
```

- [ ] **Step 3: Write the contract and feature files**

`examples/linkshort/contracts/links.openapi.yaml`:

```yaml
openapi: 3.0.3
info:
  title: linkshort
  version: 0.1.0
paths:
  /links:
    post:
      operationId: createLink
      summary: Create a short link
      responses:
        "201": { description: Created }
        "400": { description: Malformed URL }
  /{code}:
    get:
      operationId: resolveLink
      summary: Resolve a short code
      parameters:
        - { name: code, in: path, required: true, schema: { type: string } }
      responses:
        "302": { description: Redirect to the original URL }
        "404": { description: Unknown code }
```

`examples/linkshort/verification/REQ-create-link.feature`:

```gherkin
Feature: Create a short link

  Scenario: valid url is shortened
    Given the service is running
    When I POST "https://example.com/a/very/long/path" to /links
    Then the response status is 201
    And the body contains a short code

  Scenario: malformed url is rejected
    Given the service is running
    When I POST "not-a-url" to /links
    Then the response status is 400
    And no link is persisted
```

`examples/linkshort/verification/REQ-resolve-link.feature`:

```gherkin
Feature: Resolve a short link

  Scenario: known code redirects
    Given a link exists for "https://example.com" with code "abc123"
    When I GET /abc123
    Then the response status is 302
    And the Location header is "https://example.com"

  Scenario: unknown code is not found
    Given no link exists with code "nope"
    When I GET /nope
    Then the response status is 404
```

- [ ] **Step 4: Run to verify pass**

Run: `uv run pytest tests/unit/test_examples.py -v && uv run avspec verify examples/linkshort`
Expected: test passes; CLI prints `status=ready errors=0 todos=0 ok=True`, exit 0.

- [ ] **Step 5: Rewrite `ci/verify.yml`**

```yaml
name: verify
on:
  push:
    branches: [develop, main]
  pull_request:

jobs:
  verify:
    runs-on: ubuntu-latest
    defaults:
      run:
        working-directory: avspec
    steps:
      - uses: actions/checkout@v4
      - uses: astral-sh/setup-uv@v5
      - run: uv sync
      - run: uv run ruff check .
      - run: uv run ty check
      - run: uv run pytest
      - run: uv run avspec verify examples/linkshort
```

- [ ] **Step 6: Full suite, lint, type-check, commit**

```bash
uv run pytest && uv run ruff check . && uv run ty check
git add examples ci/verify.yml tests/unit/test_examples.py
git commit -m "feat: linkshort conformance example and uv-based ci gate"
```

---

### Task 11: Rewrite the guided-qa interview skill

**Files:**
- Modify: `adapters/guided-qa/SKILL.md` (full rewrite)
- Modify: `adapters/README.md` (update the one-paragraph pointer to say the skill drives `avspec next`)

**Interfaces:**
- Consumes: the `avspec next --json` payload shape from Task 9 (`{"status", "findings": [{code, severity, message, ref, question}]}`) and the finding codes from Tasks 4–8. If any of those changed during implementation, this document must match reality — check against `uv run avspec next examples/linkshort --json` output, not against this plan.

- [ ] **Step 1: Rewrite `adapters/guided-qa/SKILL.md`**

```markdown
---
name: avspec-guided-qa
description: >-
  Interview a user to author an AVSpec 0.3 (Agent-Verifiable Architecture
  Spec) from scratch, or fill the gaps in a draft one. Use when someone wants
  a machine-verifiable architecture spec through guided questions — "help me
  write a spec", "interview me for the architecture", "fill in my avspec".
  Drives every question and the definition of done from `avspec next --json`.
---

# AVSpec Guided-QA

You are authoring an AVSpec by interview. **The verifier is the brain**: it
decides what to ask about and when the spec is done. You are the mouth — free
to dig deeper, reframe questions naturally, batch related follow-ups, and
propose sensible defaults. You never decide completeness; `avspec verify` does.

## Setup

1. Confirm the spec directory (default: current dir).
2. If `avspec.yaml` does not exist, create the minimal manifest:

   ```yaml
   avspec: "0.3"
   project:
     name: <ask>
     description: <ask — one sentence>
     status: draft
   ```

3. Run `avspec next <dir> --json` to read the current gap queue.
   (In this repo: `uv run avspec next <dir> --json`.)

## The loop

Repeat until `avspec next` returns zero findings:

1. Run `avspec next <dir> --json`.
2. Take the FIRST finding (the queue is already in authoring order,
   errors before todos). Its `question` field is your prompt — rephrase it
   naturally, keep any IDs verbatim.
3. Ask ONE question at a time. Digging deeper on an answer is encouraged;
   moving to a different finding before writing the current answer is not.
4. Write the answer into `avspec.yaml` (and create any referenced files —
   `.feature` scenarios, contract stubs). Mint numeric IDs (`REQ-001`
   style) unless the user offers slugs. Prefixes are fixed:
   CON- REQ- AC- MOD- CTR- VIEW- ACT-.
5. Re-run and continue.

If the user skips a question, note it and move to the next finding; re-ask
skipped ones at the end rather than spinning on them.

## Writing answers

- `NO_STACK` → fill `project.stack`: languages, package_manager, frameworks,
  bdd, and the install/test/lint commands. Free-form strings.
- `NO_CONSTITUTION` → offer defaults and confirm: dependency direction
  between the modules they expect, test-first, no secrets in source.
- `NO_REQUIREMENTS` → loop "what must it do?" until the user is out;
  one REQ per answer with title and rationale.
- `REQ_NO_AC` → elicit testable statements (EARS or given/when/then prose).
- `AC_NO_TEST` → propose a scenario name; set
  `test: verification/<REQ>.feature#<scenario>` and create the file with
  that scenario sketched in Gherkin.
- `NO_MODULES` → ask for the units of the system, one responsibility each.
- `MOD_NO_BOUNDARIES` → ask which modules each may import. An empty list is
  a valid, explicit answer.
- UI findings (`UI_NO_ENTRY`, `VIEW_NO_AC`, `VIEW_UNREACHABLE`,
  `ACTION_ORPHAN`) → the UI spec is functionality only: screens, what each
  shows, what each can do, where each navigates. Never ask about layout or
  styling.

## Done

When `avspec next` is empty at `status: draft`, ask the user whether to
promote to `status: ready`, set it, and run `avspec verify <dir>` — it must
exit 0. Show the user the final summary line.
```

- [ ] **Step 2: Update `adapters/README.md`**

Replace the body with:

```markdown
# Adapters

Frontends that drive the AVSpec core. The core exposes two commands —
`avspec verify` (the gate) and `avspec next` (the gap queue) — and every
adapter is a thin driver over them.

- `guided-qa/` — a Claude skill that interviews a human and writes the spec.
  The verifier decides what to ask; the agent conducts the conversation.
```

- [ ] **Step 3: Validate the skill against reality**

Run: `uv run avspec next examples/linkshort --json`
Expected: `{"status": "ready", "findings": []}` — and every code named in SKILL.md exists in `src/avspec/findings.py` `CODE_ORDER` or the wellformed/loading error codes. Fix any drift in SKILL.md, not in the code.

- [ ] **Step 4: Commit**

```bash
git add adapters
git commit -m "docs: rewrite guided-qa skill to drive avspec next"
```

---

### Task 12: Slice milestone check

**Files:** none — verification only.

- [ ] **Step 1: Full gate**

Run: `uv run pytest && uv run ruff check . && uv run ty check && uv run avspec verify examples/linkshort`
Expected: everything green, exit 0.

- [ ] **Step 2: Empty-directory behavior (milestone item 2)**

```bash
mkdir -p /tmp/avspec-empty && uv run avspec verify /tmp/avspec-empty --json; echo "exit=$?"
```

Expected: JSON report with a `MANIFEST_MISSING` error, `exit=1`.

- [ ] **Step 3: Live interview dry run (milestone item 3)**

Invoke the `avspec-guided-qa` skill against a fresh temp directory and take a toy project (e.g. a todo CLI) through at least stack, constitution, one requirement with an AC and test file, and two modules with boundaries. Confirm the loop consumes `avspec next` findings in order and the todo count decreases every round. This is a manual checkpoint with the user — the design's milestone item 3 is only *fully* met when a real session takes a spec to `ready`.

- [ ] **Step 4: Roborev**

Ensure every commit in the branch has a Roborev pass before merge — no progress past this slice without it.

---

## Self-review notes (already applied)

- `AC_UNSATISFIED` from the design's draft rule list is **dropped** for slice 1: with no `tasks` section in 0.3, "which module implements this AC" has no manifest home, and inferring it would be advisory noise. `AC_NO_TEST` + `TEST_FILE_MISSING` + `VIEW_NO_AC` cover the traceable ground. Revisit if a `tasks`/`implements` edge is added.
- The design's `UI_ENTRY_MISSING` error is split: an entry naming an unknown view is a `DANGLING_REF` **error** (wellformed); views-but-no-entry is the `UI_NO_ENTRY` **todo** (absence). Same ground covered, cleaner severity semantics.
- `MANIFEST_UNPARSEABLE` was added alongside the design's `MANIFEST_MISSING`/`MANIFEST_INVALID` — broken YAML must fail loudly, not crash.
- Type consistency verified: `Finding(code, severity, message, ref, question)` field order matches every construction site; `asdict()` output therefore matches the JSON contract in Task 9 and SKILL.md.


