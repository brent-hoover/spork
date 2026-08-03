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
