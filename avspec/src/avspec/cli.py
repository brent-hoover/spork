"""AVSpec command line interface."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Annotated

import typer
from rich.console import Console

from avspec import __version__
from avspec.analysis import Report, analyze
from avspec.findings import ordered
from avspec.interview import ask as ask_interview

app = typer.Typer(name="avspec", help="Author and verify Agent-Verifiable Architecture Specs.")
console = Console()


@app.command()
def version() -> None:
    """Print the AVSpec version."""
    typer.echo(__version__)


@app.command()
def verify(
    spec_dir: Annotated[Path, typer.Argument(help="Spec directory.")] = Path("."),
    as_json: Annotated[bool, typer.Option("--json", help="Emit machine-readable JSON.")] = False,
) -> None:
    """Run the AVSpec verifier."""
    report = analyze(spec_dir)
    if report.fatal:
        console.print(f"FATAL: {report.fatal}", style="red")
        raise typer.Exit(1)
    if as_json:
        typer.echo(_report_json(report))
        raise typer.Exit(0 if report.pass_ else 1)

    findings = ordered(report.findings)
    for finding in findings:
        if finding.severity == "error":
            console.print(f"  ERROR  [{finding.code}] {finding.message}", style="red")
        elif finding.severity == "todo":
            label = "TODO*" if report.strict else "todo "
            console.print(f"  {label}  [{finding.code}] {finding.message}", style="yellow")
        else:
            console.print(f"  warn   [{finding.code}] {finding.message}", style="dim")
    console.print("")

    if report.pass_:
        console.print(
            f"PASS (status: {report.status}) - {report.counts['error']} errors, "
            f"{report.counts['todo']} open todos, {report.counts['warn']} warnings."
        )
        if not report.strict and report.counts["todo"]:
            console.print(
                f"  {report.counts['todo']} todo(s) remain; "
                "they must be closed before status: ready."
            )
        raise typer.Exit(0)

    console.print(
        f"FAIL (status: {report.status}) - {report.counts['error']} error(s)"
        + (f" + {report.counts['todo']} unmet todo(s) blocking ready." if report.strict else "."),
        style="red",
    )
    raise typer.Exit(1)


@app.command("next")
def next_command(
    spec_dir: Annotated[Path, typer.Argument(help="Spec directory.")] = Path("."),
    as_json: Annotated[bool, typer.Option("--json", help="Emit machine-readable JSON.")] = False,
) -> None:
    """Print the ordered interview queue."""
    report = analyze(spec_dir)
    if report.fatal:
        console.print(f"FATAL: {report.fatal}", style="red")
        raise typer.Exit(1)

    findings = ordered(report.findings)
    blocking = [finding for finding in findings if finding.severity == "error"]
    todos = [finding for finding in findings if finding.severity == "todo"]
    warnings = [finding for finding in findings if finding.severity == "warn"]
    queue = [*blocking, *todos]

    if as_json:
        typer.echo(
            json.dumps(
                {
                    "status": report.status,
                    "pass": report.pass_,
                    "counts": report.counts,
                    "queue": [
                        {
                            "n": index,
                            "severity": finding.severity,
                            "code": finding.code,
                            "ids": list(finding.ids),
                            "ask": finding.question or finding.message,
                        }
                        for index, finding in enumerate(queue, start=1)
                    ],
                },
                indent=2,
            )
        )
        raise typer.Exit(0 if report.pass_ else 1)

    name = report.manifest.metadata.name if report.manifest else spec_dir.name
    console.print(
        f"\n{name} - status: {report.status} - {len(todos)} open question(s), "
        f"{len(blocking)} blocking error(s)\n"
    )
    if blocking:
        console.print("must fix first:")
        for index, finding in enumerate(blocking, start=1):
            console.print(f"  {index}. [{finding.code}] {finding.message}")
        console.print("")
    if todos:
        console.print("open questions:")
        for index, finding in enumerate(todos, start=1 + len(blocking)):
            console.print(f"  {index}. {finding.question or finding.message}")
        console.print("")
    if not blocking and not todos:
        console.print(
            f"Nothing open. {'Spec passes.' if report.strict else 'Ready to set status: ready.'}"
        )
    if warnings:
        console.print(f"({len(warnings)} warning(s) - non-blocking)", style="dim")
    raise typer.Exit(0 if report.pass_ else 1)


@app.command()
def ask(spec_dir: Annotated[Path, typer.Argument(help="Spec directory.")] = Path(".")) -> None:
    """Run the module-first AVSpec interview."""
    raise typer.Exit(ask_interview(spec_dir))


def _report_json(report: Report) -> str:
    return json.dumps(
        {
            "status": report.status,
            "strict": report.strict,
            "pass": report.pass_,
            "counts": report.counts,
            "findings": [
                {
                    "severity": finding.severity,
                    "code": finding.code,
                    "message": finding.message,
                    "ids": list(finding.ids),
                    **({"question": finding.question} if finding.question else {}),
                    **({"fix": finding.fix} if finding.fix else {}),
                }
                for finding in report.findings
            ],
        },
        indent=2,
    )
