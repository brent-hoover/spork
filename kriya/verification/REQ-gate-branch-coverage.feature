Feature: Branch coverage gate
  Coverage is per conditional arm and binary. There is no percentage
  threshold — one untested arm fails the gate.

  Scenario: any uncovered arm fails the gate
    Given a run at head commit "C2" whose ticket touched module "api"
    When the branch-coverage gate runs the module's snapshot-resolved coverage command
    Then a single uncovered conditional arm fails the gate for "api"
    And a module with every arm covered passes
    And no overall percentage can compensate for an uncovered arm

  Scenario: coverage results pin to the commit
    Given the coverage gate ran for module "api" at commit "C2"
    Then the result upserts as a GateResult with gate "branch-coverage" pinned to "C2"
    And the uncovered arms are named in the result's detail for the dev agent
    And a passing result recorded at an older commit never satisfies the chain
