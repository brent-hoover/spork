"""Pydantic models for the AVSpec manifest."""

from __future__ import annotations

from typing import Any, Literal

from pydantic import BaseModel, ConfigDict, Field


class StrictModel(BaseModel):
    model_config = ConfigDict(extra="forbid")


class Metadata(StrictModel):
    name: str
    status: Literal["draft", "ready", "built"]
    description: str | None = None


class LanguageSpec(StrictModel):
    name: str
    version: str | None = None


class Commands(StrictModel):
    install: str | None = None
    test: str | None = None
    lint: str | None = None
    typecheck: str | None = None
    arch: str | None = None


class Stack(StrictModel):
    languages: list[str | LanguageSpec] = Field(default_factory=list)
    package_manager: str | None = None
    frameworks: list[str] = Field(default_factory=list)
    bdd: str | None = None
    commands: Commands | None = None
    fitness_function: str | None = None


class Artifacts(StrictModel):
    constitution: str
    requirements: str
    design: str
    tasks: str


class Principle(StrictModel):
    id: str
    statement: str


class Constraint(StrictModel):
    id: str
    statement: str
    gate: bool = True


class AcceptanceCriterion(StrictModel):
    id: str
    ears: str
    test: str | None = None


class Requirement(StrictModel):
    id: str
    title: str
    rationale: str | None = None
    acceptance: list[AcceptanceCriterion] = Field(default_factory=list)


class Contract(StrictModel):
    id: str
    type: Literal["openapi", "jsonschema", "asyncapi"]
    path: str


class Interface(StrictModel):
    name: str
    contract: str


class Component(StrictModel):
    id: str
    responsibility: str
    depends_on: list[str] = Field(default_factory=list)
    interfaces: list[Interface] = Field(default_factory=list)
    owns: list[str] = Field(default_factory=list)
    stack: Stack | None = None


class Decision(StrictModel):
    id: str
    title: str
    status: Literal["proposed", "accepted", "superseded", "deprecated"]
    path: str


class Task(StrictModel):
    id: str
    title: str
    satisfies: list[str]
    touches: list[str] = Field(default_factory=list)
    depends_on: list[str] = Field(default_factory=list)
    parallelizable: bool = False


class FieldDef(StrictModel):
    name: str
    type: str
    required: bool = True
    unique: bool = False


class Relation(StrictModel):
    to: str
    kind: str
    name: str | None = None


class Entity(StrictModel):
    id: str
    name: str
    description: str | None = None
    fields: list[FieldDef] = Field(default_factory=list)
    relations: list[Relation] = Field(default_factory=list)


class DataModel(StrictModel):
    store: str
    entities: list[Entity] = Field(default_factory=list)


class ConfigEntry(StrictModel):
    id: str
    name: str
    type: str
    required: bool = True
    secret: bool = False
    description: str | None = None


class View(StrictModel):
    id: str
    name: str
    purpose: str
    route: str | None = None
    invocation: str | None = None
    displays: list[str] = Field(default_factory=list)
    actions: list[str] = Field(default_factory=list)
    navigates_to: list[str] = Field(default_factory=list)
    satisfies: list[str] = Field(default_factory=list)


class Action(StrictModel):
    id: str
    name: str
    invokes: str | None = None
    writes: list[str] = Field(default_factory=list)


class UI(StrictModel):
    kind: Literal["web", "cli", "tui", "none"]
    entry: str | None = None
    views: list[View] = Field(default_factory=list)
    actions: list[Action] = Field(default_factory=list)


class Manifest(StrictModel):
    avspec: str
    metadata: Metadata
    mode: Literal["greenfield"]
    artifacts: Artifacts
    stack: Stack | None = None
    principles: list[Principle] = Field(default_factory=list)
    constraints: list[Constraint] = Field(default_factory=list)
    requirements: list[Requirement] = Field(default_factory=list)
    contracts: list[Contract] = Field(default_factory=list)
    components: list[Component] = Field(default_factory=list)
    decisions: list[Decision] = Field(default_factory=list)
    tasks: list[Task] = Field(default_factory=list)
    data: DataModel | None = None
    config: list[ConfigEntry] = Field(default_factory=list)
    ui: UI | None = None

    def model_dump_yaml(self) -> dict[str, Any]:
        return self.model_dump(mode="json", exclude_none=True, exclude_defaults=True)
