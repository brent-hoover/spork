"""UI rules — the screen graph must be complete, connected, and traceable."""

from __future__ import annotations

from collections.abc import Iterable

from avspec.analysis import Spec
from avspec.findings import Finding, Severity
from avspec.rules import rule


def _todo(code: str, message: str, question: str, ref: str | None = None) -> Finding:
    return Finding(code=code, severity=Severity.TODO, message=message, question=question, ref=ref)


@rule
def ui_shape(spec: Spec) -> Iterable[Finding]:
    for module in spec.manifest.modules:
        ui = module.ui
        if ui is None or ui.kind == "none":
            continue
        view_ids = {v.id for v in ui.views}
        if ui.views and ui.entry is None:
            yield _todo(
                "UI_NO_ENTRY",
                f"{module.id} declares views but no entry point.",
                f"Which view is the entry point of {module.id} — "
                "the first screen a user lands on?",
                ref=module.id,
            )
        for view in ui.views:
            if not view.satisfies:
                yield _todo(
                    "VIEW_NO_AC",
                    f"{view.id} satisfies no acceptance criterion.",
                    f"Which acceptance criteria does {view.id} ({view.name}) serve? "
                    "A view no requirement needs should be removed.",
                    ref=view.id,
                )
        if ui.entry is not None and ui.entry in view_ids:
            nav = {v.id: v.navigates_to for v in ui.views}
            reachable: set[str] = set()
            frontier = [ui.entry]
            while frontier:
                current = frontier.pop()
                if current in reachable:
                    continue
                reachable.add(current)
                frontier.extend(t for t in nav.get(current, []) if t in view_ids)
            for view in ui.views:
                if view.id not in reachable:
                    yield _todo(
                        "VIEW_UNREACHABLE",
                        f"{view.id} cannot be reached from {ui.entry}.",
                        f"How does a user get to {view.id} ({view.name})? "
                        "Add it to some view's navigates_to, or remove it.",
                        ref=view.id,
                    )
        referenced = {act for v in ui.views for act in v.actions}
        for action in ui.actions:
            if action.id not in referenced:
                yield _todo(
                    "ACTION_ORPHAN",
                    f"{action.id} is not offered by any view.",
                    f"Which view offers {action.id} ({action.name})? "
                    "Add it to that view's actions, or remove it.",
                    ref=action.id,
                )
