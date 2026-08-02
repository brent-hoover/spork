from pathlib import Path

from avspec.analysis import analyze

LINKSHORT = Path(__file__).parents[2] / "examples" / "linkshort"


def test_linkshort_is_ready_and_clean() -> None:
    report = analyze(LINKSHORT)
    blockers = [f for f in report.findings if f.severity.value in ("error", "todo")]
    assert blockers == [], [f"{f.code}: {f.message}" for f in blockers]
    assert report.status == "ready"
    assert report.ok
