Feature: Structural lint gate
  Hard structural limits on the target app, enforced by each module's
  own snapshot-resolved lint command. Findings cannot be waived at gate
  time and stale results never satisfy the chain.

  Scenario: the lint command runs per touched module
    Given a run at head commit "C2" whose ticket touched modules "api" and "store"
    When the structure gate runs
    Then each module's snapshot-resolved lint command executes against "C2"
    And a nonzero exit from "store" fails the gate for "store"
    And a clean exit from "api" passes the gate for "api"

  Scenario: structural findings cannot be waived
    Given the structure gate failed for module "store"
    Then no waiver mechanism exists at gate time
    And the remedies are fixing the code or an operator-approved lint-config change in the target project via spec amendment and re-intake
    Given a passing structure result recorded at an older commit
    Then it does not satisfy the gate at the current head

  Scenario: failures feed back and results upsert
    Given the structure gate fails with findings
    Then the tool's findings are handed to the dev agent and the loop continues
    And the result is upserted as a GateResult with gate "structure", the module, the commit, and detail
    When the gate reruns for the same build, module, and commit
    Then the stored result is replaced, never duplicated
