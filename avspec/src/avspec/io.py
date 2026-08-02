"""Manifest loading and saving."""

from __future__ import annotations

from collections.abc import Mapping
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from pydantic import ValidationError
from ruamel.yaml import YAML

from avspec.model import Manifest


@dataclass(frozen=True)
class LoadResult:
    spec_dir: Path
    raw: dict[str, Any] | None
    manifest: Manifest | None
    errors: tuple[str, ...] = ()


yaml = YAML()
yaml.default_flow_style = False
yaml.width = 120


def load_manifest(spec_dir: Path) -> LoadResult:
    resolved = spec_dir.resolve()
    manifest_path = resolved / "avspec.yaml"
    if not manifest_path.exists():
        return LoadResult(
            spec_dir=resolved,
            raw=None,
            manifest=None,
            errors=(f"missing manifest: {manifest_path}",),
        )

    data = yaml.load(manifest_path.read_text()) or {}
    if not isinstance(data, dict):
        return LoadResult(
            spec_dir=resolved,
            raw=None,
            manifest=None,
            errors=("manifest root must be a mapping",),
        )

    try:
        return LoadResult(spec_dir=resolved, raw=data, manifest=Manifest.model_validate(data))
    except ValidationError as exc:
        return LoadResult(
            spec_dir=resolved,
            raw=data,
            manifest=None,
            errors=tuple(_format_validation_error(error) for error in exc.errors()),
        )


def save_raw_manifest(spec_dir: Path, data: dict[str, Any]) -> None:
    manifest_path = spec_dir.resolve() / "avspec.yaml"
    with manifest_path.open("w") as handle:
        yaml.dump(data, handle)


def _format_validation_error(error: Mapping[str, Any]) -> str:
    loc = "/" + "/".join(str(part) for part in error["loc"])
    return f"{loc} {error['msg']}"
