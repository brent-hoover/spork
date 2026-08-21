Feature: Branch coverage gate
  Coverage is measured per conditional arm, never as statements. The
  passing bar belongs to the target, declared in its own stack command;
  kriya runs that command and honours its verdict.

  Scenario: the target's declared coverage bar decides the gate
    Given a run at head commit "C2" whose ticket touched module "api"
    When the branch-coverage gate runs the module's snapshot-resolved coverage command
    Then the gate fails for "api" when that command fails
    And the gate passes for "api" when that command succeeds
    And kriya counts no arms and applies no threshold of its own
    And a target declaring a floor is judged against that floor
    And a target declaring no floor is judged against every arm

  Scenario: tests must pass before coverage is judged
    Given a run at head commit "C2" whose ticket touched module "api"
    When the module's test command fails at "C2"
    Then the test gate fails as its own commit-pinned GateResult with gate "test"
    And the chain fails regardless of what coverage would report
    When the test command passes at "C2"
    Then coverage is judged over the passing suite

  Scenario: coverage results pin to the commit
    Given the coverage gate ran for module "api" at commit "C2"
    Then the result upserts as a GateResult with gate "branch-coverage" pinned to "C2"
    And the uncovered arms are named in the result's detail for the dev agent
    And a passing result recorded at an older commit never satisfies the chain
