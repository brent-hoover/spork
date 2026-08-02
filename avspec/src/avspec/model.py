"""Pydantic models for the AVSpec 0.3 manifest — the canonical format definition."""

from __future__ import annotations

from typing import Literal

from pydantic import BaseModel, ConfigDict, Field


class StrictModel(BaseModel):
    model_config = ConfigDict(extra="forbid")


class Commands(StrictModel):
    install: str | None = None
    test: str | None = None
    lint: str | None = None
    typecheck: str | None = None
    arch: str | None = None


class Language(StrictModel):
    name: str
    version: str | None = None


class Stack(StrictModel):
    languages: list[str | Language] = Field(default_factory=list)
    package_manager: str | None = None
    frameworks: list[str] = Field(default_factory=list)
    bdd: str | None = None
    commands: Commands | None = None


class Project(StrictModel):
    name: str
    description: str | None = None
    status: Literal["draft", "ready", "built"] = "draft"
    stack: Stack | None = None


class ConstitutionEntry(StrictModel):
    id: str
    statement: str


class AcceptanceCriterion(StrictModel):
    id: str
    statement: str
    test: str | None = None  # "relative/path.feature#scenario name"


class Requirement(StrictModel):
    id: str
    title: str
    rationale: str | None = None
    acceptance: list[AcceptanceCriterion] = Field(default_factory=list)


class Contract(StrictModel):
    id: str
    type: str  # openapi | asyncapi | jsonschema — free-form, never an enum
    path: str


class Boundaries(StrictModel):
    may_import: list[str] = Field(default_factory=list)


class View(StrictModel):
    id: str
    name: str
    route: str | None = None  # web
    invocation: str | None = None  # cli / tui
    purpose: str | None = None
    shows: list[str] = Field(default_factory=list)
    actions: list[str] = Field(default_factory=list)
    navigates_to: list[str] = Field(default_factory=list)
    satisfies: list[str] = Field(default_factory=list)


class Action(StrictModel):
    id: str
    name: str
    invokes: str | None = None  # "CTR-<id>#<operation>"


class UI(StrictModel):
    kind: Literal["web", "cli", "tui", "none"]
    entry: str | None = None
    views: list[View] = Field(default_factory=list)
    actions: list[Action] = Field(default_factory=list)


class Module(StrictModel):
    id: str
    name: str
    responsibility: str | None = None
    stack: Stack | None = None
    boundaries: Boundaries | None = None
    contracts: list[Contract] = Field(default_factory=list)
    ui: UI | None = None


class Manifest(StrictModel):
    avspec: Literal["0.3"]
    project: Project
    constitution: list[ConstitutionEntry] = Field(default_factory=list)
    requirements: list[Requirement] = Field(default_factory=list)
    modules: list[Module] = Field(default_factory=list)
