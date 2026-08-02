import copy
from pathlib import Path

from avspec.rules import ui as ui_rules
from tests.conftest import MINIMAL, make_spec


def codes(findings) -> list[str]:
    return sorted(f.code for f in findings)


def web_module(ui_block: dict) -> dict:
    data = copy.deepcopy(MINIMAL)
    data["modules"] = [
        {
            "id": "MOD-web",
            "name": "web",
            "responsibility": "UI",
            "boundaries": {"may_import": []},
            "ui": ui_block,
        }
    ]
    return data


def view(view_id: str, **overrides) -> dict:
    base = {"id": view_id, "name": view_id, "satisfies": ["AC-x"]}
    base.update(overrides)
    return base


def test_no_views(tmp_path: Path) -> None:
    data = web_module({"kind": "web"})
    assert "UI_NO_VIEWS" in codes(ui_rules.ui_shape(make_spec(tmp_path, data)))


def test_no_entry(tmp_path: Path) -> None:
    data = web_module({"kind": "web", "views": [view("VIEW-a")]})
    assert "UI_NO_ENTRY" in codes(ui_rules.ui_shape(make_spec(tmp_path, data)))


def test_view_without_ac(tmp_path: Path) -> None:
    data = web_module({"kind": "web", "entry": "VIEW-a", "views": [view("VIEW-a", satisfies=[])]})
    assert "VIEW_NO_AC" in codes(ui_rules.ui_shape(make_spec(tmp_path, data)))


def test_unreachable_view(tmp_path: Path) -> None:
    data = web_module(
        {"kind": "web", "entry": "VIEW-a", "views": [view("VIEW-a"), view("VIEW-island")]}
    )
    found = ui_rules.ui_shape(make_spec(tmp_path, data))
    unreachable = [f for f in found if f.code == "VIEW_UNREACHABLE"]
    assert [f.ref for f in unreachable] == ["VIEW-island"]


def test_navigation_makes_reachable(tmp_path: Path) -> None:
    data = web_module(
        {
            "kind": "web",
            "entry": "VIEW-a",
            "views": [view("VIEW-a", navigates_to=["VIEW-b"]), view("VIEW-b")],
        }
    )
    assert "VIEW_UNREACHABLE" not in codes(ui_rules.ui_shape(make_spec(tmp_path, data)))


def test_orphan_action(tmp_path: Path) -> None:
    data = web_module(
        {
            "kind": "web",
            "entry": "VIEW-a",
            "views": [view("VIEW-a")],
            "actions": [{"id": "ACT-unused", "name": "unused"}],
        }
    )
    assert "ACTION_ORPHAN" in codes(ui_rules.ui_shape(make_spec(tmp_path, data)))


def test_kind_none_is_quiet(tmp_path: Path) -> None:
    data = web_module({"kind": "none"})
    assert codes(ui_rules.ui_shape(make_spec(tmp_path, data))) == []
