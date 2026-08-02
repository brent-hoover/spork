# Technical Spec: tech-spec

**Author:** Brent
**Date:** 2026-08-02
**Status:** Draft

## Problem

What problem are we solving? Why now?

Agent-driven development can quickly devolve into "big ball of mud" architecture as agents add functionality that may duplicate existing functionality or make code difficult to discover.

There are other specs that define the architecture in a way that agents can read
but it depends on "trusting" the agent to follow these instructions, which agents don't always do.

This project strives to make good architecture another "signal" that an agent can use to write good code.


## Terms
 - Project: The overall project that the architect is building
 - Module: a set of code with grouped functionality
 - Contract: A definition of the api for a module
 - Boundaries: What other modules a modules is allowed to import/be imported from providing good separation of concerns


## Goals & Non-Goals

### Goals
The overall goal is that this would be something that you could give an agent and they could build the entire application without further prompting. Also, for the agent to use as features are added, so that the code is well-structured.

The secondary goal is that it's a "discovery" tool for architects and possible non-technical users at some point that gives them something that helps them think through the architecture.

Wherever there is an already existing spec that meets our requirements, we will reuse it rather than reimplement it. But this project needs to be maximized for use by agents, but also readable/editable by humans. 

The author hopes this will help his projects (and eventually others) get "built right the first time" rather than creating a "big ball of mud"

More specifically:

- Provide a format that both agents can use to specify architecture and other "clean code" limitations (linting, etc) that can be verified by running a command, either locally or in CI/CD
- Provide a structured project interview that walks an architect through creating the spec based on their requirements
- Provide module-level BDD specifications that can be executed via a BDD testing tool

At the top level:
- When necessary, create "modules" which can either be part of the same app or separate pieces that communicate with each other.
- Boundaries (which other modules it can import, which modules)
- Contracts - how does each module communicate with each other

Within the app level:
- Define BDD specs with Acceptance Criteria
- Define the stack for each module (when that makes sense, otherwise at the project level)

### Non-Goals
- The "avspec" project does not provide a way for the app to actually get built. That will be the "spork" project (of which avspec is a subproject)

## Proposed Design
- YAML files over JSON files every place it's possible to make them more user-editable
- This version will use Python to provide the verification but tries to be as platform/language agnostic as it can

Open Questions:
- [ ] How does AVSpec define user interfaces?



