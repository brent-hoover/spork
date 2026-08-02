import json
from pathlib import Path

import pytest
from pytest_bdd import given, parsers, scenarios, then, when
from tests.conftest import MINIMAL, write_manifest
from typer.testing import CliRunner

from avspec.cli import app

scenarios("../features")


@pytest.fixture()
def spec_dir(tmp_path: Path) -> Path:
    return tmp_path / "spec"


@pytest.fixture()
def cli_result() -> dict:
    return {}


@given("an empty spec directory")
def empty_dir(spec_dir: Path) -> None:
    spec_dir.mkdir()


@given("a minimal draft spec")
def minimal_spec(spec_dir: Path) -> None:
    write_manifest(spec_dir, MINIMAL)


@given(parsers.parse('a minimal spec with status "{status}"'))
def spec_with_status(spec_dir: Path, status: str) -> None:
    data = {**MINIMAL, "project": {**MINIMAL["project"], "status": status}}
    write_manifest(spec_dir, data)


@when(parsers.parse("I run avspec {command} --json"))
def run_cli(cli_result: dict, spec_dir: Path, command: str) -> None:
    result = CliRunner().invoke(app, [command, str(spec_dir), "--json"])
    cli_result["exit_code"] = result.exit_code
    cli_result["payload"] = json.loads(result.stdout)


@then(parsers.parse("the exit code is {code:d}"))
def check_exit(cli_result: dict, code: int) -> None:
    assert cli_result["exit_code"] == code


@then(parsers.parse('the report contains a "{code}" finding with severity "{severity}"'))
def check_finding(cli_result: dict, code: str, severity: str) -> None:
    findings = cli_result["payload"]["findings"]
    assert any(f["code"] == code and f["severity"] == severity for f in findings)


@then(parsers.parse('the first finding code is "{code}"'))
def check_first(cli_result: dict, code: str) -> None:
    assert cli_result["payload"]["findings"][0]["code"] == code


@then("every finding has a question")
def check_questions(cli_result: dict) -> None:
    assert all(f["question"] for f in cli_result["payload"]["findings"])
