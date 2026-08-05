Feature: Product-owner validation
  Passing tests is not the bar. The PO — the only agent holding the
  whole spec — confirms each AC is genuinely satisfied and hunts test
  gaming before anything reaches human review.

  Scenario: the PO validates after review and mutation
    Given a run whose head commit has passed the review and mutation gates
    Then PO validation begins
    Given a run missing either gate at the head commit
    Then PO validation does not run and cannot be reordered around the gap

  Scenario: every AC is checked against real behavior
    Given a ticket with three acceptance criteria
    When the PO validates the run
    Then each AC is matched to a test that genuinely exercises it
    And the implementation is judged against the AC's intent within the whole spec
    And an AC satisfied by letter but not intent fails validation

  Scenario: gaming fails validation with evidence
    Given tests that assert tautologies, or an implementation special-casing test inputs, or assertions weakened since an earlier commit
    When the PO validates the run
    Then validation fails naming the gaming evidence found

  Scenario: the verdict is durable and actionable
    Given the PO passes a run
    Then the run advances to review submission and the verdict with its reasons is durably recorded
    Given the PO fails a run
    Then the findings return to the dev agent, the pair loop resumes, and the verdict survives in the run's history
