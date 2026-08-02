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
    "entity": "ENT-",
    "app": "APP-",
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
    if manifest.data:
        out.extend(("entity", e.id) for e in manifest.data.entities)
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
        elif not obj_id[len(prefix) :].strip():
            yield Finding(
                code="PREFIX_MISMATCH",
                severity=Severity.ERROR,
                ref=obj_id,
                message=(
                    f"A {kind} id must have a non-empty suffix after {prefix!r}; got {obj_id!r}."
                ),
            )


def _dangling(ref: str, owner: str, field: str) -> Finding:
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
    entities = {e.id: e for e in manifest.data.entities} if manifest.data else {}
    for module in manifest.modules:
        if module.boundaries:
            for target in module.boundaries.may_import:
                if target not in module_ids:
                    yield _dangling(target, module.id, "boundaries.may_import")
        for owned in module.owns:
            if owned not in entities:
                yield _dangling(owned, module.id, "owns")
        if module.ui is None:
            continue
        view_ids = {v.id for v in module.ui.views}
        action_ids = {a.id for a in module.ui.actions}
        if module.ui.entry is not None and module.ui.entry not in view_ids:
            yield _dangling(module.ui.entry, module.id, "ui.entry")
        for view in module.ui.views:
            for act in view.actions:
                if act not in action_ids:
                    yield _dangling(act, view.id, "actions")
            for nav in view.navigates_to:
                if nav not in view_ids:
                    yield _dangling(nav, view.id, "navigates_to")
            for ac in view.satisfies:
                if ac not in ac_ids:
                    yield _dangling(ac, view.id, "satisfies")
            for shown in view.shows:
                if not shown.startswith("ENT-"):
                    continue
                entity_ref, _, field_name = shown.partition(".")
                entity = entities.get(entity_ref)
                if entity is None or not field_name or field_name not in {
                    f.name for f in entity.fields
                }:
                    yield _dangling(shown, view.id, "shows")
        for action in module.ui.actions:
            if action.invokes is not None:
                contract_ref = action.invokes.split("#", 1)[0]
                if contract_ref not in contract_ids:
                    yield _dangling(contract_ref, action.id, "invokes")
    if manifest.data:
        for entity in manifest.data.entities:
            for relation in entity.relations:
                if relation.to not in entities:
                    yield _dangling(relation.to, entity.id, "relations.to")
            for field in entity.fields:
                if field.ref is not None and field.ref not in entities:
                    yield _dangling(field.ref, entity.id, "fields.ref")


@rule
def entity_multi_owner(spec: Spec) -> Iterable[Finding]:
    owners: dict[str, list[str]] = {}
    for module in spec.manifest.modules:
        for entity_id in module.owns:
            owners.setdefault(entity_id, []).append(module.id)
    for entity_id, module_ids in owners.items():
        if len(module_ids) > 1:
            yield Finding(
                code="ENT_MULTI_OWNER",
                severity=Severity.ERROR,
                ref=entity_id,
                message=f"{entity_id} is owned by more than one module: {', '.join(module_ids)}.",
            )


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
