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
# Stack comes last — it is an implementation detail, chosen only once what the
# system must do and what shape it takes are already pinned down.
CODE_ORDER: tuple[str, ...] = (
    "NO_CONSTITUTION",
    "NO_REQUIREMENTS",
    "REQ_NO_AC",
    "AC_NO_TEST",
    "NO_MODULES",
    "MOD_NO_RESPONSIBILITY",
    "NO_DATA",
    "ENT_NO_FIELDS",
    "ENT_UNOWNED",
    "NO_APPS",
    "MOD_NO_APP",
    "MOD_NO_BOUNDARIES",
    "CONTRACT_FILE_MISSING",
    "UI_NO_VIEWS",
    "UI_NO_ENTRY",
    "VIEW_NO_AC",
    "VIEW_UNREACHABLE",
    "ACTION_ORPHAN",
    "TEST_REF_INVALID",
    "TEST_FILE_MISSING",
    "TEST_SCENARIO_MISSING",
    "NO_STACK",
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
