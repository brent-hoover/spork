"""Well-formedness rules — inconsistencies. Always fail."""

from __future__ import annotations

from collections.abc import Iterable

from avspec.analysis import Spec
from avspec.findings import Finding, Severity
from avspec.model import Manifest
from avspec.rules import rule

PREFIXES: dict[str, str] = {
    "constitution entry": "CON-",
    "requirement": "REQ-",
    "acceptance criterion": "AC-",
    "module": "MOD-",
    "contract": "CTR-",
    "view": "VIEW-",
    "action": "ACT-",
}


def _all_ids(manifest: Manifest) -> list[tuple[str, str]]:
    """(kind, id) for every identified object in the manifest."""
    out: list[tuple[str, str]] = [("constitution entry", c.id) for c in manifest.constitution]
    for req in manifest.requirements:
        out.append(("requirement", req.id))
        out.extend(("acceptance criterion", ac.id) for ac in req.acceptance)
    for module in manifest.modules:
        out.append(("module", module.id))
        out.extend(("contract", c.id) for c in module.contracts)
        if module.ui:
            out.extend(("view", v.id) for v in module.ui.views)
            out.extend(("action", a.id) for a in module.ui.actions)
    return out


@rule
def duplicate_ids(spec: Spec) -> Iterable[Finding]:
    seen: set[str] = set()
    for _, obj_id in _all_ids(spec.manifest):
        if obj_id in seen:
            yield Finding(
                code="DUPLICATE_ID",
                severity=Severity.ERROR,
                ref=obj_id,
                message=f"ID {obj_id} is declared more than once.",
            )
        seen.add(obj_id)


@rule
def prefix_mismatch(spec: Spec) -> Iterable[Finding]:
    for kind, obj_id in _all_ids(spec.manifest):
        prefix = PREFIXES[kind]
        if not obj_id.startswith(prefix):
            yield Finding(
                code="PREFIX_MISMATCH",
                severity=Severity.ERROR,
                ref=obj_id,
                message=f"A {kind} id must start with {prefix!r}; got {obj_id!r}.",
            )


def _dangling(ref: str, known: set[str], owner: str, field: str) -> Finding:
    return Finding(
        code="DANGLING_REF",
        severity=Severity.ERROR,
        ref=owner,
        message=f"{owner}.{field} references {ref!r}, which does not exist.",
    )


@rule
def dangling_refs(spec: Spec) -> Iterable[Finding]:
    manifest = spec.manifest
    module_ids = {m.id for m in manifest.modules}
    ac_ids = {ac.id for req in manifest.requirements for ac in req.acceptance}
    contract_ids = {c.id for m in manifest.modules for c in m.contracts}
    for module in manifest.modules:
        if module.boundaries:
            for target in module.boundaries.may_import:
                if target not in module_ids:
                    yield _dangling(target, module_ids, module.id, "boundaries.may_import")
        if module.ui is None:
            continue
        view_ids = {v.id for v in module.ui.views}
        action_ids = {a.id for a in module.ui.actions}
        if module.ui.entry is not None and module.ui.entry not in view_ids:
            yield _dangling(module.ui.entry, view_ids, module.id, "ui.entry")
        for view in module.ui.views:
            for act in view.actions:
                if act not in action_ids:
                    yield _dangling(act, action_ids, view.id, "actions")
            for nav in view.navigates_to:
                if nav not in view_ids:
                    yield _dangling(nav, view_ids, view.id, "navigates_to")
            for ac in view.satisfies:
                if ac not in ac_ids:
                    yield _dangling(ac, ac_ids, view.id, "satisfies")
        for action in module.ui.actions:
            if action.invokes is not None:
                contract_ref = action.invokes.split("#", 1)[0]
                if contract_ref not in contract_ids:
                    yield _dangling(contract_ref, contract_ids, action.id, "invokes")


@rule
def boundary_cycle(spec: Spec) -> Iterable[Finding]:
    graph: dict[str, list[str]] = {
        m.id: (m.boundaries.may_import if m.boundaries else []) for m in spec.manifest.modules
    }
    done: set[str] = set()

    def visit(node: str, stack: tuple[str, ...]) -> Iterable[Finding]:
        if node in stack:
            cycle = " -> ".join((*stack[stack.index(node) :], node))
            yield Finding(
                code="BOUNDARY_CYCLE",
                severity=Severity.ERROR,
                ref=node,
                message=f"Module import cycle: {cycle}.",
            )
            return
        if node in done:
            return
        done.add(node)
        for dep in graph.get(node, []):
            yield from visit(dep, (*stack, node))

    for module_id in graph:
        yield from visit(module_id, ())
