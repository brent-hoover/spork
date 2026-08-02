import pytest
from pydantic import ValidationError

from avspec.model import App, Data, Entity, EntityField, Language, Manifest, Relation

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


def test_data_section_round_trips() -> None:
    data = {
        **MINIMAL,
        "data": {
            "entities": [
                {
                    "id": "ENT-link",
                    "name": "Link",
                    "fields": [
                        {"name": "code", "type": "string", "required": True, "unique": True},
                        {"name": "owner", "type": "ref", "ref": "ENT-user"},
                    ],
                    "relations": [{"to": "ENT-user", "kind": "one_to_many"}],
                },
                {"id": "ENT-user", "name": "User"},
            ]
        },
        "modules": [{"id": "MOD-data", "name": "data", "owns": ["ENT-link", "ENT-user"]}],
    }
    m = Manifest.model_validate(data)
    assert isinstance(m.data, Data)
    assert m.data.entities[0].fields[0].name == "code"
    assert m.data.entities[0].relations[0].to == "ENT-user"
    assert m.modules[0].owns == ["ENT-link", "ENT-user"]


def test_data_defaults_to_none() -> None:
    m = Manifest.model_validate(MINIMAL)
    assert m.data is None


def test_empty_data_entities_is_valid() -> None:
    m = Manifest.model_validate({**MINIMAL, "data": {"entities": []}})
    assert m.data is not None
    assert m.data.entities == []


def test_entity_field_bad_type_is_rejected() -> None:
    with pytest.raises(ValidationError):
        EntityField.model_validate({"name": "x", "type": "float"})


def test_entity_field_blank_name_is_rejected() -> None:
    with pytest.raises(ValidationError):
        EntityField.model_validate({"name": "   ", "type": "string"})


def test_entity_field_ref_type_without_target_is_rejected() -> None:
    with pytest.raises(ValidationError):
        EntityField.model_validate({"name": "owner", "type": "ref"})


def test_entity_field_ref_on_non_ref_type_is_rejected() -> None:
    with pytest.raises(ValidationError):
        EntityField.model_validate({"name": "owner", "type": "string", "ref": "ENT-user"})


def test_entity_field_valid_ref_is_accepted() -> None:
    field = EntityField.model_validate({"name": "owner", "type": "ref", "ref": "ENT-user"})
    assert field.ref == "ENT-user"


def test_relation_bad_kind_is_rejected() -> None:
    with pytest.raises(ValidationError):
        Relation.model_validate({"to": "ENT-x", "kind": "many_to_many_to_many"})


def test_entity_extra_field_is_rejected() -> None:
    with pytest.raises(ValidationError):
        Entity.model_validate({"id": "ENT-x", "name": "X", "bogus": "nope"})


def test_module_owns_defaults_to_empty() -> None:
    m = Manifest.model_validate({**MINIMAL, "modules": [{"id": "MOD-a", "name": "a"}]})
    assert m.modules[0].owns == []


def test_apps_default_to_empty() -> None:
    m = Manifest.model_validate(MINIMAL)
    assert m.apps == []


def test_apps_round_trip() -> None:
    data = {
        **MINIMAL,
        "modules": [{"id": "MOD-api", "name": "api"}],
        "apps": [{"id": "APP-server", "name": "server", "modules": ["MOD-api"]}],
    }
    m = Manifest.model_validate(data)
    assert isinstance(m.apps[0], App)
    assert m.apps[0].modules == ["MOD-api"]


def test_app_extra_field_is_rejected() -> None:
    with pytest.raises(ValidationError):
        App.model_validate({"id": "APP-x", "name": "X", "bogus": "nope"})


def test_stack_accepts_store() -> None:
    m = Manifest.model_validate(
        {**MINIMAL, "project": {"name": "demo", "stack": {"store": "postgres"}}}
    )
    assert m.project.stack is not None
    assert m.project.stack.store == "postgres"


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
