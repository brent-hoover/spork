Feature: Typing gate
  Untyped code never lands. Each touched module's snapshot-resolved
  typecheck command runs at the head commit; results pin to the commit
  they ran against.

  Scenario: the typecheck command runs per touched module
    Given a run at head commit "C2" whose ticket touched modules "api" and "store"
    When the typing gate runs
    Then each module's snapshot-resolved typecheck command executes against "C2"
    And any nonzero exit fails that module's gate

  Scenario: typing results pin to the commit
    Given the typing gate ran for module "api" at commit "C2"
    Then the result is upserted as a GateResult with gate "typing" pinned to "C2"
    And a passing result recorded at an older commit never satisfies the chain
    And a failure returns the tool's output to the dev agent
