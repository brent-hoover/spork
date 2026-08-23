"""`avspec inspect` — a spec's full resolved detail.

verify judges a spec; inspect shows it.

Kriya needs each module's effective commands and the spec's artifact set to
pin a reproducible snapshot. Emitting them here keeps ONE parser for the
avspec format; a consumer that re-parsed avspec.yaml could disagree with
avspec about its own format.
"""

from __future__ import annotations

import json
from pathlib import Path

from typer.testing import CliRunner

from avspec.cli import app
from tests.conftest import write_manifest


def inspect(spec_dir: Path) -> tuple[int, dict]:
    result = CliRunner().invoke(app, ["inspect", str(spec_dir)])
    return result.exit_code, json.loads(result.stdout)


def test_a_module_inherits_the_project_stack(tmp_path: Path) -> None:
    write_manifest(
        tmp_path,
        {
            "avspec": "0.3",
            "project": {
                "name": "p",
                "status": "draft",
                "stack": {"commands": {"test": "pt", "lint": "pl"}},
            },
            "modules": [{"id": "MOD-a", "name": "a"}],
        },
    )
    code, payload = inspect(tmp_path)
    assert code == 0
    assert payload["modules"][0]["commands"]["test"] == "pt"
    assert payload["modules"][0]["commands"]["lint"] == "pl"


def test_override_is_per_field_not_per_block(tmp_path: Path) -> None:
    """A module overriding only `test` still inherits `lint`.

    Per-block replacement would silently drop every command the module did
    not restate, which a consumer would read as "not declared".
    """
    write_manifest(
        tmp_path,
        {
            "avspec": "0.3",
            "project": {
                "name": "p",
                "status": "draft",
                "stack": {"commands": {"test": "pt", "lint": "pl"}},
            },
            "modules": [
                {"id": "MOD-a", "name": "a", "stack": {"commands": {"test": "mt"}}},
            ],
        },
    )
    _, payload = inspect(tmp_path)
    commands = payload["modules"][0]["commands"]
    assert commands["test"] == "mt", "the override must win"
    assert commands["lint"] == "pl", "the un-overridden field must be inherited"


def test_an_undeclared_command_is_null_not_absent(tmp_path: Path) -> None:
    """Null, so a consumer can tell "not declared" from "declared blank"."""
    write_manifest(
        tmp_path,
        {
            "avspec": "0.3",
            "project": {"name": "p", "status": "draft", "stack": {"commands": {"test": "pt"}}},
            "modules": [{"id": "MOD-a", "name": "a"}],
        },
    )
    _, payload = inspect(tmp_path)
    commands = payload["modules"][0]["commands"]
    assert "arch" in commands
    assert commands["arch"] is None


def test_artifacts_list_every_referenced_file(tmp_path: Path) -> None:
    write_manifest(
        tmp_path,
        {
            "avspec": "0.3",
            "project": {"name": "p", "status": "draft"},
            "requirements": [
                {
                    "id": "REQ-a",
                    "title": "t",
                    "acceptance": [
                        {"id": "AC-a", "statement": "s", "test": "verification/a.feature#one"},
                        {"id": "AC-b", "statement": "s", "test": "verification/a.feature#two"},
                    ],
                }
            ],
            "modules": [
                {
                    "id": "MOD-a",
                    "name": "a",
                    "contracts": [{"id": "CTR-a", "type": "openapi", "path": "contracts/a.yaml"}],
                }
            ],
        },
    )
    _, payload = inspect(tmp_path)
    # The scenario anchor is stripped and the file deduped: two criteria
    # pointing into one feature file are one artifact.
    assert payload["artifacts"] == [
        "avspec.yaml",
        "contracts/a.yaml",
        "verification/a.feature",
    ]


def test_a_missing_manifest_exits_one_with_findings(tmp_path: Path) -> None:
    code, payload = inspect(tmp_path)
    assert code == 1
    assert payload["ok"] is False
    assert payload["findings"], "a refusal must explain itself"


def test_a_module_carries_its_own_law(tmp_path: Path) -> None:
    """A build engine assembling an agent's context needs the module's law.

    Which imports a module may make, and which contracts it publishes, are
    stated in the manifest but not resolved anywhere a consumer can read
    without parsing avspec.yaml itself.
    """
    write_manifest(
        tmp_path,
        {
            "avspec": "0.3",
            "project": {"name": "p", "status": "draft"},
            "modules": [
                {
                    "id": "MOD-a",
                    "name": "a",
                    "boundaries": {"may_import": ["MOD-b"]},
                    "contracts": [
                        {"id": "CTR-a", "type": "openapi", "path": "contracts/a.yaml"}
                    ],
                },
                {"id": "MOD-b", "name": "b"},
            ],
        },
    )
    (tmp_path / "contracts").mkdir()
    (tmp_path / "contracts" / "a.yaml").write_text("openapi: 3.1.0\n")

    code, payload = inspect(tmp_path)
    assert code == 0
    first, second = payload["modules"]
    assert first["may_import"] == ["MOD-b"]
    assert first["contracts"] == [
        {"id": "CTR-a", "type": "openapi", "path": "contracts/a.yaml"}
    ]
    # Absent is an empty list, never a missing key: a consumer that had to
    # distinguish "declared nothing" from "the field is not there" would be
    # deciding what an unstated boundary means, which is avspec's job.
    assert second["may_import"] == []
    assert second["contracts"] == []


def test_the_constitution_is_emitted_whole(tmp_path: Path) -> None:
    """The constitution is law the whole project is judged by, so it travels
    with the model rather than being read out of the manifest separately."""
    write_manifest(
        tmp_path,
        {
            "avspec": "0.3",
            "project": {"name": "p", "status": "draft"},
            "constitution": [
                {"id": "CON-one", "statement": "Tests come first."},
                {"id": "CON-two", "statement": "No untyped code lands."},
            ],
            "modules": [{"id": "MOD-a", "name": "a"}],
        },
    )
    code, payload = inspect(tmp_path)
    assert code == 0
    assert payload["constitution"] == [
        {"id": "CON-one", "statement": "Tests come first."},
        {"id": "CON-two", "statement": "No untyped code lands."},
    ]


def test_a_spec_with_no_constitution_emits_an_empty_list(tmp_path: Path) -> None:
    write_manifest(
        tmp_path,
        {
            "avspec": "0.3",
            "project": {"name": "p", "status": "draft"},
            "modules": [{"id": "MOD-a", "name": "a"}],
        },
    )
    code, payload = inspect(tmp_path)
    assert code == 0
    assert payload["constitution"] == []
