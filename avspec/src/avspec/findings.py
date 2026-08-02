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
