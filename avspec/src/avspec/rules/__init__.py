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


# Imported for registration side effects — a rule module missing here does nothing.
from avspec.rules import wellformed  # noqa: E402, F401
