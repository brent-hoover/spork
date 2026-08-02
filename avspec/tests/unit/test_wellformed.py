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
        == []
    )
