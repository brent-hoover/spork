Feature: Spec intake
  Kriya builds only from a spec that is provably ready, and only from
  exactly the spec it validated. Intake verifies the avspec, resolves
  every module's effective commands, and pins both into an immutable
  SpecSnapshot. A spec change enters the pipeline only through a new
  intake, which feeds the plan-supersession path.

  Scenario: ready spec is pinned
    Given a project "shorty" whose avspec verifies status "ready" with zero errors and zero todo findings
    And every module's effective stack resolves non-empty test, lint, typecheck, arch, coverage, and mutation commands
    When the operator points kriya at the project
    Then intake succeeds and a SpecSnapshot is pinned
    And the snapshot holds the full artifact set and every module's resolved commands

  Scenario: draft spec is refused
    Given a project whose avspec verifies status "draft"
    When the operator points kriya at the project
    Then intake is refused and the response carries the verify findings
    And no snapshot is pinned and nothing is enqueued

  Scenario: erroring spec is refused
    Given a project whose avspec claims status "ready" but verify reports an error finding
    When the operator points kriya at the project
    Then intake is refused and the response carries the error finding
    And no snapshot is pinned and nothing is enqueued

  Scenario: a ready claim with todo findings is refused
    Given a project whose avspec claims status "ready" but verify reports a todo finding
    When the operator points kriya at the project
    Then intake is refused and the response carries the todo finding
    And no snapshot is pinned and nothing is enqueued

  Scenario: missing module command refuses intake
    Given a project whose avspec verifies status "ready" with zero errors
    And module "web" resolves a blank effective "mutation" command
    When the operator points kriya at the project
    Then intake is refused naming module "web" and command "mutation"
    And no snapshot is pinned and nothing is enqueued

  Scenario: complete module override resolves to the module's own commands
    Given a ready spec whose module "web" overrides every stack command
    When intake pins the SpecSnapshot
    Then the snapshot records module "web" with exactly its own commands
    And no field of module "web" falls back to the project stack

  Scenario: partial module override falls back per field
    Given a ready spec whose module "web" overrides only the "test" command
    When intake pins the SpecSnapshot
    Then the snapshot records module "web" with its own "test" command
    And every other command for module "web" is the project stack's

  Scenario: working tree edits do not change a pinned build
    Given intake pinned SpecSnapshot "S1" for a project and a build bound to "S1"
    When the working-tree spec's lint command is edited
    And the build runs its gate chain and recovery later replays a step
    Then every executed command is the one resolved into "S1"
    And the working-tree edit is never consulted

  Scenario: an intake crash never allocates a second generation
    Given an intake attempt was recorded pending under its idempotency token and its generation landed on the mapping
    And the crash hit before the plan was created
    When the intake retries under the same token
    Then it resumes the recorded attempt and reuses its generation — no duplicate supersession
    Given a deliberate same-hash re-intake arrives under a new token
    Then it allocates the next generation

  Scenario: a delayed older plan can never regress the mapping
    Given intake generation 2 pinned the project's mapping snapshot
    When a delayed plan seed carrying generation 1 arrives
    Then the mapping is untouched — the older generation loses the upsert
    Given no mapping exists yet
    When a generation-1 plan seed inserts first and the generation-2 intake write arrives after
    Then the mapping converges on generation 2
    And unplanned binds always read the newest intake's snapshot
    Given a re-intake pins the same content hash a prior intake pinned
    Then it allocates a fresh, higher generation for the target
    And a plan still carrying the prior intake's generation cannot regress the mapping, even naming the same hash

  Scenario: amended spec pins a new snapshot
    Given SpecSnapshot "S1" exists for a project with builds referencing it
    And the spec is amended and verifies status "ready" again
    When the operator re-runs intake
    Then a new SpecSnapshot "S2" is pinned and handed to decomposition through the supersession path
    And "S1" is unchanged and still referenced by its builds
