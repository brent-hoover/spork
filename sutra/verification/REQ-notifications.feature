Feature: Pollable event feed
  Every subscriber lives on this machine; polling a cursor beats
  delivering webhooks nobody can receive.

  Scenario: cursor polling resumes losslessly
    Given events E1, E2, E3 occurred in order
    And a consumer's cursor sits after E1
    When the consumer polls the feed
    Then it receives E2 and E3 in order
    And polling again from the new cursor returns nothing

  Scenario: filters narrow the feed
    Given events of kinds "review.approved" and "issue.updated" exist for several issues
    When a consumer polls with kind "review.approved" and subject SUT-1
    Then it receives only review approvals for SUT-1

  Scenario: watermarks anchor reads to the feed
    Given an issue read returns state with its feed watermark captured atomically
    Then processing the feed through that watermark covers every event that could have affected the returned state
    Given an issue list matches nothing
    Then the empty response still carries its watermark
    Given a work-stack pop claims an issue
    Then the result carries ONE authoritative top-level watermark — the nested issue carries none, so no two drain positions can disagree
    Given a work-stack pop finds the stack empty
    Then the empty result still carries a top-level watermark anchoring the negative answer to the feed

  Scenario: draining to a watermark is bounded and provable
    Given a consumer drains the feed with until set to a captured watermark
    And new events keep arriving concurrently
    Then each page reports drained false until everything through the fixed watermark has been returned
    And the page that completes the drain reports drained true — later concurrent events do not move the bound
