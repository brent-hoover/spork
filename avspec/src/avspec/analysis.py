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
