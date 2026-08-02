"""Finding primitives and stable ordering."""

from __future__ import annotations

from collections.abc import Iterable
from dataclasses import dataclass, field
from typing import Literal

Severity = Literal["error", "todo", "warn"]


@dataclass(frozen=True)
class Finding:
    severity: Severity
    code: str
    message: str
    ids: tuple[str, ...] = field(default_factory=tuple)
    question: str | None = None
    fix: str | None = None


CODE_ORDER: tuple[str, ...] = (
    "SCHEMA",
    "DUP_ID",
    "DANGLING",
    "CYCLE",
    "ARCH_VIOLATION",
    "NO_COMPONENTS",
    "NO_BOUNDARIES",
    "NO_CONTRACTS",
    "CMP_NO_CONTRACT",
    "NO_CONSTITUTION",
    "NO_REQUIREMENTS",
    "REQ_NO_AC",
    "AC_UNSATISFIED",
    "AC_NO_TEST",
    "LAYER_UNPLACED",
    "NO_TASKS",
    "ARTIFACT_MISSING",
    "CONTRACT_MISSING",
    "TEST_MISSING",
    "ADR_MISSING",
    "NO_STACK",
    "STACK_INCOMPLETE",
    "CMP_NO_STACK",
    "MIRROR",
    "NO_ARCH_RULES",
)
SEVERITY_RANK: dict[Severity, int] = {"error": 0, "todo": 1, "warn": 2}


def ordered(findings: Iterable[Finding]) -> list[Finding]:
    return sorted(findings, key=lambda item: (SEVERITY_RANK[item.severity], code_rank(item.code)))


def code_rank(code: str) -> int:
    try:
        return CODE_ORDER.index(code)
    except ValueError:
        return len(CODE_ORDER)
