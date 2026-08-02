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
    # Rules registered; minimal manifest is well-formed — no errors; todos aren't errors.
    assert report.counts["error"] == 0
