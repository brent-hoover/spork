from pathlib import Path

from ruamel.yaml import YAML

from avspec.analysis import Spec  # noqa: F401  (re-exported for step files)
from avspec.model import Manifest

MINIMAL: dict = {"avspec": "0.3", "project": {"name": "demo", "status": "draft"}}


def write_manifest(spec_dir: Path, data: dict) -> None:
    spec_dir.mkdir(parents=True, exist_ok=True)
    yaml = YAML()
    with (spec_dir / "avspec.yaml").open("w", encoding="utf-8") as fh:
        yaml.dump(data, fh)


def make_spec(spec_dir: Path, data: dict):
    return Spec(dir=spec_dir, manifest=Manifest.model_validate(data))
