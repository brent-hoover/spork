"""Typer CLI: verify (the CI gate) and next (the interview's brain)."""

from __future__ import annotations

import json
from dataclasses import asdict
from pathlib import Path

import typer

from avspec.analysis import Report, analyze
from avspec.findings import Severity

app = typer.Typer(no_args_is_help=True, add_completion=False)


def _payload(report: Report) -> dict:
    return {
        "status": report.status,
        "ok": report.ok,
        "counts": report.counts,
        "findings": [asdict(f) for f in report.findings],
    }


def _print_human(report: Report) -> None:
    for finding in report.findings:
        ref = finding.ref or ""
        typer.echo(f"{finding.severity.value:<6} {finding.code:<22} {ref:<18} {finding.message}")
    counts = report.counts
    typer.echo(
        f"status={report.status} errors={counts['error']} "
        f"todos={counts['todo']} ok={report.ok}"
    )


@app.command()
def verify(
    spec_dir: Path = typer.Argument(Path(".")),  # noqa: B008
    as_json: bool = typer.Option(False, "--json", help="Machine-readable report."),
) -> None:
    """Verify a spec directory. Exit 0 iff the spec passes for its declared status."""
    report = analyze(spec_dir)
    if as_json:
        typer.echo(json.dumps(_payload(report), indent=2))
    else:
        _print_human(report)
    raise typer.Exit(0 if report.ok else 1)


@app.command(name="next")
def next_(
    spec_dir: Path = typer.Argument(Path(".")),  # noqa: B008
    as_json: bool = typer.Option(False, "--json", help="Machine-readable queue."),
) -> None:
    """Print the gap queue in authoring order — errors first, then todos."""
    report = analyze(spec_dir)
    actionable = [f for f in report.findings if f.severity in (Severity.ERROR, Severity.TODO)]
    if as_json:
        payload = {"status": report.status, "findings": [asdict(f) for f in actionable]}
        typer.echo(json.dumps(payload, indent=2))
    else:
        for finding in actionable:
            text = finding.question or finding.message
            typer.echo(f"[{finding.severity.value}] {finding.code}: {text}")
    raise typer.Exit(0)
