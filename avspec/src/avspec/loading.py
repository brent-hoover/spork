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
