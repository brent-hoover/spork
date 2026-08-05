Feature: Mutation gate
  Mutation testing proves the tests test. It runs after the review
  pass, and a surviving mutant is a named test gap that blocks the
  chain until killed.

  Scenario: mutation waits for the review pass
    Given a run whose head commit "C2" has not yet passed the review gate
    Then the mutation gate does not run
    When the review gate passes at "C2"
    Then each touched module's snapshot-resolved mutation command runs against "C2"

  Scenario: surviving mutants are named test gaps
    Given the mutation command reports surviving mutants for module "store"
    Then the gate fails for "store"
    And the survivors are returned to the dev agent as named test gaps to kill
    And the result upserts as a GateResult with gate "mutation" pinned to the commit
    And a run with zero survivors passes the gate
