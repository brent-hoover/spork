"""Typer CLI: verify (the CI gate) and next (the interview's brain)."""

from __future__ import annotations

import json
from dataclasses import asdict
from pathlib import Path

import typer

from avspec.analysis import Report, analyze
from avspec.findings import Severity
from avspec.loading import MANIFEST_NAME, load
from avspec.model import Commands, Manifest, Stack

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


def _effective_commands(project: Stack | None, module: Stack | None) -> dict[str, str | None]:
    """Resolve a module's commands: its own override per field, project stack otherwise.

    Per FIELD, not per block — a module that overrides only `test` still
    inherits the project's `lint`. Missing entries stay None rather than being
    dropped, so a consumer can tell "not declared" from "declared blank".
    """
    base = project.commands if project and project.commands else Commands()
    over = module.commands if module and module.commands else Commands()
    return {
        field: getattr(over, field) or getattr(base, field)
        for field in Commands.model_fields
    }


def _artifacts(spec_dir: Path, manifest: Manifest) -> list[str]:
    """Every file the spec references, relative to spec_dir, sorted and deduped.

    This is the content set a consumer must pin to make a build reproducible:
    the manifest, each acceptance criterion's feature file, and each module's
    contract documents. Feature references carry a `#scenario` anchor, which
    is stripped — the file is the artifact.
    """
    paths = {MANIFEST_NAME}
    for requirement in manifest.requirements:
        for criterion in requirement.acceptance:
            if criterion.test:
                paths.add(criterion.test.split("#", 1)[0])
    for module in manifest.modules:
        for contract in module.contracts:
            paths.add(contract.path)
    return sorted(paths)


@app.command()
def resolve(
    spec_dir: Path = typer.Argument(Path(".")),  # noqa: B008
) -> None:
    """Print the resolved build model as JSON — effective commands and artifacts.

    Output is ALWAYS JSON and there is no --json flag, unlike verify and next.
    Those two have a human format worth reading; this command exists only for
    machine consumption, so a second format would be dead weight.

    A build engine needs two things the manifest states only indirectly: what
    each module's commands actually resolve to after per-field override, and
    which files make up the spec. Emitting them here keeps ONE parser for the
    avspec format; a consumer that re-parsed avspec.yaml could disagree with
    avspec about its own format.

    Exits 1 if the manifest cannot be loaded, printing the findings.
    """
    result = load(spec_dir)
    if result.manifest is None:
        failure = {"ok": False, "findings": [asdict(f) for f in result.findings]}
        typer.echo(json.dumps(failure, indent=2))
        raise typer.Exit(1)

    manifest = result.manifest
    project_stack = manifest.project.stack
    typer.echo(
        json.dumps(
            {
                "ok": True,
                "avspec": manifest.avspec,
                "project": {"name": manifest.project.name, "status": manifest.project.status},
                "commands": _effective_commands(project_stack, None),
                "modules": [
                    {
                        "id": module.id,
                        "name": module.name,
                        "commands": _effective_commands(project_stack, module.stack),
                    }
                    for module in manifest.modules
                ],
                "artifacts": _artifacts(spec_dir, manifest),
            },
            indent=2,
        )
    )
