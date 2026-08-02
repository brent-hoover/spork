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
    assert result.manifest is not None
    assert result.manifest.project.name == "demo"
