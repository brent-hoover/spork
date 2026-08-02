import copy
from pathlib import Path

from avspec.rules import wellformed
from tests.conftest import MINIMAL, make_spec


def codes(findings) -> list[str]:
    return sorted(f.code for f in findings)


def base() -> dict:
    return copy.deepcopy(MINIMAL)


def test_duplicate_ids(tmp_path: Path) -> None:
    data = base()
    data["requirements"] = [{"id": "REQ-a", "title": "A"}, {"id": "REQ-a", "title": "B"}]
    assert codes(wellformed.duplicate_ids(make_spec(tmp_path, data))) == ["DUPLICATE_ID"]


def test_prefix_mismatch(tmp_path: Path) -> None:
    data = base()
    data["modules"] = [{"id": "CMP-oops", "name": "x"}]
    assert codes(wellformed.prefix_mismatch(make_spec(tmp_path, data))) == ["PREFIX_MISMATCH"]


def test_prefix_mismatch_bare_prefix(tmp_path: Path) -> None:
    data = base()
    data["modules"] = [{"id": "MOD-", "name": "x"}]
    assert codes(wellformed.prefix_mismatch(make_spec(tmp_path, data))) == ["PREFIX_MISMATCH"]


def test_prefix_mismatch_whitespace_only_suffix(tmp_path: Path) -> None:
    data = base()
    data["modules"] = [{"id": "MOD-   ", "name": "x"}]
    assert codes(wellformed.prefix_mismatch(make_spec(tmp_path, data))) == ["PREFIX_MISMATCH"]


def test_dangling_may_import(tmp_path: Path) -> None:
    data = base()
    data["modules"] = [{"id": "MOD-a", "name": "a", "boundaries": {"may_import": ["MOD-ghost"]}}]
    assert codes(wellformed.dangling_refs(make_spec(tmp_path, data))) == ["DANGLING_REF"]


def test_dangling_ui_refs(tmp_path: Path) -> None:
    data = base()
    data["modules"] = [
        {
            "id": "MOD-web",
            "name": "web",
            "ui": {
                "kind": "web",
                "entry": "VIEW-ghost",
                "views": [
                    {
                        "id": "VIEW-a",
                        "name": "A",
                        "actions": ["ACT-ghost"],
                        "navigates_to": ["VIEW-ghost"],
                        "satisfies": ["AC-ghost"],
                    }
                ],
                "actions": [{"id": "ACT-a", "name": "a", "invokes": "CTR-ghost#op"}],
            },
        }
    ]
    found = codes(wellformed.dangling_refs(make_spec(tmp_path, data)))
    assert found == ["DANGLING_REF"] * 5  # entry, action ref, navigation, satisfies, invokes


def test_entity_prefix_mismatch(tmp_path: Path) -> None:
    data = base()
    data["data"] = {"entities": [{"id": "E-oops", "name": "x"}]}
    assert codes(wellformed.prefix_mismatch(make_spec(tmp_path, data))) == ["PREFIX_MISMATCH"]


def test_entity_duplicate_id(tmp_path: Path) -> None:
    data = base()
    data["data"] = {
        "entities": [{"id": "ENT-a", "name": "A"}, {"id": "ENT-a", "name": "B"}]
    }
    assert codes(wellformed.duplicate_ids(make_spec(tmp_path, data))) == ["DUPLICATE_ID"]


def test_dangling_module_owns(tmp_path: Path) -> None:
    data = base()
    data["modules"] = [{"id": "MOD-a", "name": "a", "owns": ["ENT-ghost"]}]
    assert codes(wellformed.dangling_refs(make_spec(tmp_path, data))) == ["DANGLING_REF"]


def test_dangling_relation_to(tmp_path: Path) -> None:
    data = base()
    data["data"] = {
        "entities": [
            {"id": "ENT-a", "name": "A", "relations": [{"to": "ENT-ghost", "kind": "one_to_one"}]}
        ]
    }
    assert codes(wellformed.dangling_refs(make_spec(tmp_path, data))) == ["DANGLING_REF"]


def test_dangling_field_ref(tmp_path: Path) -> None:
    data = base()
    data["data"] = {
        "entities": [
            {
                "id": "ENT-a",
                "name": "A",
                "fields": [{"name": "owner", "type": "ref", "ref": "ENT-ghost"}],
            }
        ]
    }
    assert codes(wellformed.dangling_refs(make_spec(tmp_path, data))) == ["DANGLING_REF"]


def test_field_ref_to_real_entity_is_clean(tmp_path: Path) -> None:
    data = base()
    data["data"] = {
        "entities": [
            {
                "id": "ENT-a",
                "name": "A",
                "fields": [{"name": "owner", "type": "ref", "ref": "ENT-b"}],
            },
            {"id": "ENT-b", "name": "B"},
        ]
    }
    assert codes(wellformed.dangling_refs(make_spec(tmp_path, data))) == []


def test_view_shows_entity_field_checked(tmp_path: Path) -> None:
    data = base()
    data["data"] = {
        "entities": [
            {"id": "ENT-link", "name": "Link", "fields": [{"name": "code", "type": "string"}]}
        ]
    }
    data["modules"] = [
        {
            "id": "MOD-web",
            "name": "web",
            "ui": {
                "kind": "web",
                "entry": "VIEW-a",
                "views": [
                    {
                        "id": "VIEW-a",
                        "name": "A",
                        "shows": ["ENT-link.code", "ENT-link.ghost_field", "free text ok"],
                    }
                ],
            },
        }
    ]
    found = codes(wellformed.dangling_refs(make_spec(tmp_path, data)))
    assert found == ["DANGLING_REF"]  # only the ghost field, not the valid ref or free text


def test_view_shows_unknown_entity_is_dangling(tmp_path: Path) -> None:
    data = base()
    data["modules"] = [
        {
            "id": "MOD-web",
            "name": "web",
            "ui": {
                "kind": "web",
                "entry": "VIEW-a",
                "views": [{"id": "VIEW-a", "name": "A", "shows": ["ENT-ghost.field"]}],
            },
        }
    ]
    assert codes(wellformed.dangling_refs(make_spec(tmp_path, data))) == ["DANGLING_REF"]


def test_entity_multi_owner(tmp_path: Path) -> None:
    data = base()
    data["data"] = {"entities": [{"id": "ENT-a", "name": "A"}]}
    data["modules"] = [
        {"id": "MOD-x", "name": "x", "owns": ["ENT-a"]},
        {"id": "MOD-y", "name": "y", "owns": ["ENT-a"]},
    ]
    assert codes(wellformed.entity_multi_owner(make_spec(tmp_path, data))) == ["ENT_MULTI_OWNER"]


def test_entity_single_owner_is_clean(tmp_path: Path) -> None:
    data = base()
    data["data"] = {"entities": [{"id": "ENT-a", "name": "A"}]}
    data["modules"] = [{"id": "MOD-x", "name": "x", "owns": ["ENT-a"]}]
    assert codes(wellformed.entity_multi_owner(make_spec(tmp_path, data))) == []


def test_app_prefix_mismatch(tmp_path: Path) -> None:
    data = base()
    data["apps"] = [{"id": "A-oops", "name": "x"}]
    assert codes(wellformed.prefix_mismatch(make_spec(tmp_path, data))) == ["PREFIX_MISMATCH"]


def test_app_duplicate_id(tmp_path: Path) -> None:
    data = base()
    data["apps"] = [{"id": "APP-a", "name": "A"}, {"id": "APP-a", "name": "B"}]
    assert codes(wellformed.duplicate_ids(make_spec(tmp_path, data))) == ["DUPLICATE_ID"]


def test_dangling_app_modules(tmp_path: Path) -> None:
    data = base()
    data["apps"] = [{"id": "APP-a", "name": "a", "modules": ["MOD-ghost"]}]
    assert codes(wellformed.dangling_refs(make_spec(tmp_path, data))) == ["DANGLING_REF"]


def test_module_multi_app(tmp_path: Path) -> None:
    data = base()
    data["modules"] = [{"id": "MOD-x", "name": "x"}]
    data["apps"] = [
        {"id": "APP-a", "name": "a", "modules": ["MOD-x"]},
        {"id": "APP-b", "name": "b", "modules": ["MOD-x"]},
    ]
    assert codes(wellformed.module_multi_app(make_spec(tmp_path, data))) == ["MOD_MULTI_APP"]


def test_module_single_app_is_clean(tmp_path: Path) -> None:
    data = base()
    data["modules"] = [{"id": "MOD-x", "name": "x"}]
    data["apps"] = [{"id": "APP-a", "name": "a", "modules": ["MOD-x"]}]
    assert codes(wellformed.module_multi_app(make_spec(tmp_path, data))) == []


def test_boundary_cross_app(tmp_path: Path) -> None:
    data = base()
    data["modules"] = [
        {"id": "MOD-a", "name": "a", "boundaries": {"may_import": ["MOD-b"]}},
        {"id": "MOD-b", "name": "b", "boundaries": {"may_import": []}},
    ]
    data["apps"] = [
        {"id": "APP-a", "name": "a", "modules": ["MOD-a"]},
        {"id": "APP-b", "name": "b", "modules": ["MOD-b"]},
    ]
    found = codes(wellformed.boundary_cross_app(make_spec(tmp_path, data)))
    assert found == ["BOUNDARY_CROSS_APP"]


def test_boundary_same_app_is_clean(tmp_path: Path) -> None:
    data = base()
    data["modules"] = [
        {"id": "MOD-a", "name": "a", "boundaries": {"may_import": ["MOD-b"]}},
        {"id": "MOD-b", "name": "b", "boundaries": {"may_import": []}},
    ]
    data["apps"] = [{"id": "APP-a", "name": "a", "modules": ["MOD-a", "MOD-b"]}]
    assert codes(wellformed.boundary_cross_app(make_spec(tmp_path, data))) == []


def test_boundary_cross_app_only_fires_when_both_assigned(tmp_path: Path) -> None:
    data = base()
    data["modules"] = [
        {"id": "MOD-a", "name": "a", "boundaries": {"may_import": ["MOD-b"]}},
        {"id": "MOD-b", "name": "b", "boundaries": {"may_import": []}},
    ]
    data["apps"] = [{"id": "APP-a", "name": "a", "modules": ["MOD-a"]}]  # MOD-b unassigned
    assert codes(wellformed.boundary_cross_app(make_spec(tmp_path, data))) == []


def test_boundary_cycle(tmp_path: Path) -> None:
    data = base()
    data["modules"] = [
        {"id": "MOD-a", "name": "a", "boundaries": {"may_import": ["MOD-b"]}},
        {"id": "MOD-b", "name": "b", "boundaries": {"may_import": ["MOD-a"]}},
    ]
    assert "BOUNDARY_CYCLE" in codes(wellformed.boundary_cycle(make_spec(tmp_path, data)))


def test_clean_spec_has_no_errors(tmp_path: Path) -> None:
    data = base()
    data["modules"] = [
        {"id": "MOD-a", "name": "a", "boundaries": {"may_import": ["MOD-b"]}},
        {"id": "MOD-b", "name": "b", "boundaries": {"may_import": []}},
    ]
    spec = make_spec(tmp_path, data)
    assert (
        codes(wellformed.duplicate_ids(spec))
        + codes(wellformed.prefix_mismatch(spec))
        + codes(wellformed.dangling_refs(spec))
        + codes(wellformed.boundary_cycle(spec))
        + codes(wellformed.entity_multi_owner(spec))
        + codes(wellformed.module_multi_app(spec))
        + codes(wellformed.boundary_cross_app(spec))
        == []
    )
