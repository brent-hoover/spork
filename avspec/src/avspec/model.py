"""Pydantic models for the AVSpec 0.3 manifest — the canonical format definition."""

from __future__ import annotations

from typing import Annotated, Literal

from pydantic import AfterValidator, BaseModel, ConfigDict, Field, model_validator


def _reject_blank(v: str) -> str:
    if not v.strip():
        raise ValueError("must not be blank")
    return v


NonBlankStr = Annotated[str, AfterValidator(_reject_blank)]


class StrictModel(BaseModel):
    model_config = ConfigDict(extra="forbid")


class Commands(StrictModel):
    install: NonBlankStr | None = None
    test: NonBlankStr | None = None
    lint: NonBlankStr | None = None
    typecheck: NonBlankStr | None = None
    arch: NonBlankStr | None = None
    coverage: NonBlankStr | None = None  # branch coverage — every conditional arm
    mutation: NonBlankStr | None = None  # mutation testing — proves the tests test


class Language(StrictModel):
    name: NonBlankStr
    version: str | None = None


class Stack(StrictModel):
    languages: list[NonBlankStr | Language] = Field(default_factory=list)
    package_manager: str | None = None
    frameworks: list[str] = Field(default_factory=list)
    bdd: str | None = None
    commands: Commands | None = None
    store: str | None = None  # free-form (postgres, sqlite, none, ...) — implementation detail


class Project(StrictModel):
    name: NonBlankStr
    description: str | None = None
    status: Literal["draft", "ready", "built"] = "draft"
    stack: Stack | None = None


class ConstitutionEntry(StrictModel):
    id: NonBlankStr
    statement: NonBlankStr


class AcceptanceCriterion(StrictModel):
    id: NonBlankStr
    statement: NonBlankStr
    test: str | None = None  # "relative/path.feature#scenario name"


class Requirement(StrictModel):
    id: NonBlankStr
    title: NonBlankStr
    rationale: str | None = None
    acceptance: list[AcceptanceCriterion] = Field(default_factory=list)


class Contract(StrictModel):
    id: NonBlankStr
    type: NonBlankStr  # openapi | asyncapi | jsonschema — free-form, never an enum
    path: NonBlankStr


class Boundaries(StrictModel):
    may_import: list[str] = Field(default_factory=list)


class View(StrictModel):
    id: NonBlankStr
    name: NonBlankStr
    route: str | None = None  # web
    invocation: str | None = None  # cli / tui
    purpose: str | None = None
    shows: list[str] = Field(default_factory=list)
    actions: list[str] = Field(default_factory=list)
    navigates_to: list[str] = Field(default_factory=list)
    satisfies: list[str] = Field(default_factory=list)


class Action(StrictModel):
    id: NonBlankStr
    name: NonBlankStr
    invokes: str | None = None  # "CTR-<id>#<operation>"


class UI(StrictModel):
    kind: Literal["web", "cli", "tui", "none"]
    entry: str | None = None
    views: list[View] = Field(default_factory=list)
    actions: list[Action] = Field(default_factory=list)


class Module(StrictModel):
    id: NonBlankStr
    name: NonBlankStr
    responsibility: str | None = None
    stack: Stack | None = None
    boundaries: Boundaries | None = None
    contracts: list[Contract] = Field(default_factory=list)
    ui: UI | None = None
    owns: list[str] = Field(default_factory=list)  # ENT-* this module owns


class EntityField(StrictModel):
    name: NonBlankStr
    type: Literal[
        "string", "integer", "decimal", "boolean", "datetime", "date", "uuid", "json", "enum", "ref"
    ]
    required: bool = False
    unique: bool = False
    values: list[str] = Field(default_factory=list)  # enum only
    ref: str | None = None  # ENT-* target, ref type only

    @model_validator(mode="after")
    def _check_ref_integrity(self) -> EntityField:
        if self.type == "ref" and not (self.ref and self.ref.strip()):
            raise ValueError("EntityField.type == 'ref' requires a non-blank 'ref' target.")
        if self.type != "ref" and self.ref is not None:
            raise ValueError(
                f"EntityField.ref is only valid when type == 'ref'; got {self.type!r}."
            )
        return self


class Relation(StrictModel):
    to: NonBlankStr  # ENT-*
    kind: Literal["one_to_one", "one_to_many", "many_to_many"]
    name: str | None = None


class Entity(StrictModel):
    id: NonBlankStr
    name: NonBlankStr
    description: str | None = None
    fields: list[EntityField] = Field(default_factory=list)
    relations: list[Relation] = Field(default_factory=list)


class Data(StrictModel):
    entities: list[Entity] = Field(default_factory=list)


class App(StrictModel):
    id: NonBlankStr
    name: NonBlankStr
    modules: list[str] = Field(default_factory=list)  # MOD-*


class Manifest(StrictModel):
    avspec: Literal["0.3"]
    project: Project
    constitution: list[ConstitutionEntry] = Field(default_factory=list)
    requirements: list[Requirement] = Field(default_factory=list)
    modules: list[Module] = Field(default_factory=list)
    data: Data | None = None
    apps: list[App] = Field(default_factory=list)
