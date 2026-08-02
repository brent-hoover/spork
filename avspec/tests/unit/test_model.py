import pytest
from pydantic import ValidationError

from avspec.model import Language, Manifest

MINIMAL = {"avspec": "0.3", "project": {"name": "demo"}}


def test_minimal_manifest_validates() -> None:
    m = Manifest.model_validate(MINIMAL)
    assert m.project.status == "draft"
    assert m.modules == []


def test_unknown_key_is_rejected() -> None:
    with pytest.raises(ValidationError):
        Manifest.model_validate({**MINIMAL, "componets": []})


def test_wrong_version_is_rejected() -> None:
    with pytest.raises(ValidationError):
        Manifest.model_validate({**MINIMAL, "avspec": "0.2"})


def test_full_module_round_trip() -> None:
    data = {
        **MINIMAL,
        "constitution": [{"id": "CON-layering", "statement": "api -> domain -> data"}],
        "requirements": [
            {
                "id": "REQ-create",
                "title": "Create",
                "acceptance": [
                    {"id": "AC-ok", "statement": "shall work", "test": "verification/x.feature#ok"}
                ],
            }
        ],
        "modules": [
            {
                "id": "MOD-web",
                "name": "web",
                "responsibility": "UI",
                "boundaries": {"may_import": []},
                "contracts": [{"id": "CTR-api", "type": "openapi", "path": "contracts/api.yaml"}],
                "ui": {
                    "kind": "web",
                    "entry": "VIEW-home",
                    "views": [
                        {
                            "id": "VIEW-home",
                            "name": "Home",
                            "route": "/",
                            "shows": ["code"],
                            "actions": ["ACT-go"],
                            "satisfies": ["AC-ok"],
                        }
                    ],
                    "actions": [{"id": "ACT-go", "name": "go", "invokes": "CTR-api#go"}],
                },
            }
        ],
    }
    m = Manifest.model_validate(data)
    assert m.modules[0].ui is not None
    assert m.modules[0].ui.views[0].satisfies == ["AC-ok"]


def test_blank_title_is_rejected() -> None:
    data = {
        **MINIMAL,
        "requirements": [{"id": "REQ-a", "title": "   "}],
    }
    with pytest.raises(ValidationError):
        Manifest.model_validate(data)


def test_blank_project_name_is_rejected() -> None:
    with pytest.raises(ValidationError):
        Manifest.model_validate({**MINIMAL, "project": {"name": ""}})


def test_blank_language_entry_is_rejected() -> None:
    with pytest.raises(ValidationError):
        Manifest.model_validate(
            {
                **MINIMAL,
                "project": {"name": "demo", "stack": {"languages": ["   "]}},
            }
        )


def test_blank_contract_type_is_rejected() -> None:
    data = {
        **MINIMAL,
        "modules": [
            {
                "id": "MOD-api",
                "name": "api",
                "contracts": [{"id": "CTR-x", "type": "  ", "path": "contracts/x.yaml"}],
            }
        ],
    }
    with pytest.raises(ValidationError):
        Manifest.model_validate(data)


def test_stack_accepts_bare_and_versioned_languages() -> None:
    m = Manifest.model_validate(
        {
            **MINIMAL,
            "project": {
                "name": "demo",
                "stack": {
                    "languages": ["python", {"name": "go", "version": "1.23"}]
                },
            },
        }
    )
    assert m.project.stack is not None
    langs = m.project.stack.languages
    assert langs[0] == "python"
    assert isinstance(langs[1], Language) and langs[1].version == "1.23"
