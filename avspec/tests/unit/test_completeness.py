import copy
from pathlib import Path

from avspec.findings import Severity
from avspec.rules import completeness
from tests.conftest import MINIMAL, make_spec


def codes(findings) -> list[str]:
    return sorted(f.code for f in findings)


def base() -> dict:
    return copy.deepcopy(MINIMAL)


def test_empty_spec_fires_all_absence_todos(tmp_path: Path) -> None:
    spec = make_spec(tmp_path, base())
    found = sorted(
        codes(completeness.stack_declared(spec))
        + codes(completeness.constitution_present(spec))
        + codes(completeness.requirements_present(spec))
        + codes(completeness.modules_present(spec))
    )
    assert found == ["NO_CONSTITUTION", "NO_MODULES", "NO_REQUIREMENTS", "NO_STACK"]


def test_every_todo_has_a_question(tmp_path: Path) -> None:
    spec = make_spec(tmp_path, base())
    for run in (
        completeness.stack_declared,
        completeness.constitution_present,
        completeness.requirements_present,
        completeness.modules_present,
    ):
        for finding in run(spec):
            assert finding.question, f"{finding.code} has no question"


def test_req_no_ac_and_ac_no_test(tmp_path: Path) -> None:
    data = base()
    data["requirements"] = [
        {"id": "REQ-empty", "title": "no acs"},
        {"id": "REQ-untested", "title": "x", "acceptance": [{"id": "AC-u", "statement": "s"}]},
    ]
    spec = make_spec(tmp_path, data)
    assert codes(completeness.requirements_present(spec)) == ["AC_NO_TEST", "REQ_NO_AC"]


def test_test_file_missing_and_present(tmp_path: Path) -> None:
    data = base()
    data["requirements"] = [
        {
            "id": "REQ-x",
            "title": "x",
            "acceptance": [{"id": "AC-x", "statement": "s", "test": "verification/x.feature#ok"}],
        }
    ]
    spec = make_spec(tmp_path, data)
    assert codes(completeness.test_files_exist(spec)) == ["TEST_FILE_MISSING"]
    (tmp_path / "verification").mkdir(parents=True)
    (tmp_path / "verification" / "x.feature").write_text("Feature: x", encoding="utf-8")
    assert codes(completeness.test_files_exist(spec)) == []


def test_module_todos(tmp_path: Path) -> None:
    data = base()
    data["modules"] = [{"id": "MOD-bare", "name": "bare"}]
    spec = make_spec(tmp_path, data)
    assert codes(completeness.modules_present(spec)) == [
        "MOD_NO_BOUNDARIES",
        "MOD_NO_RESPONSIBILITY",
    ]


def test_empty_may_import_is_a_valid_answer(tmp_path: Path) -> None:
    data = base()
    data["modules"] = [
        {"id": "MOD-leaf", "name": "leaf", "responsibility": "r", "boundaries": {"may_import": []}}
    ]
    assert codes(completeness.modules_present(make_spec(tmp_path, data))) == []


def test_contract_file_missing_unparseable_and_ok(tmp_path: Path) -> None:
    data = base()
    data["modules"] = [
        {
            "id": "MOD-api",
            "name": "api",
            "responsibility": "r",
            "boundaries": {"may_import": []},
            "contracts": [{"id": "CTR-x", "type": "openapi", "path": "contracts/x.yaml"}],
        }
    ]
    spec = make_spec(tmp_path, data)
    assert codes(completeness.contract_files(spec)) == ["CONTRACT_FILE_MISSING"]
    contract = tmp_path / "contracts" / "x.yaml"
    contract.parent.mkdir(parents=True)
    contract.write_text("a: [unclosed", encoding="utf-8")
    found = list(completeness.contract_files(spec))
    assert codes(found) == ["CONTRACT_UNPARSEABLE"]
    assert found[0].severity is Severity.ERROR
    contract.write_text("openapi: 3.0.3", encoding="utf-8")
    assert codes(completeness.contract_files(spec)) == []
