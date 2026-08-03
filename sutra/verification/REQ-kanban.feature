Feature: Kanban board

  Scenario: columns mirror statuses
    Given project "SUT" has issues in several statuses
    When the board for "SUT" is opened
    Then there is a column for each of open, in-progress, blocked, deferred, complete
    And each issue's card sits in its status's column

  Scenario: drag is a real transition
    Given issue SUT-1 has no approved review
    When its card is dragged to the "complete" column
    Then the card snaps back
    And the close-requires-review error is shown

  Scenario: cards carry the essentials
    Given issue SUT-1 has an assignee and labels
    When the board is viewed
    Then SUT-1's card shows number, title, assignee, and labels
    And clicking it opens the issue
