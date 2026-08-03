Feature: Issue audit history
  The event stream is the audit trail — one stream, consumed as both
  notifications and per-issue history.

  Scenario: every mutation is recorded
    Given issue SUT-1 exists
    When "claude" changes SUT-1 status to "in-progress"
    Then an event exists for SUT-1 with actor "claude", kind "status-changed", and a timestamp

  Scenario: history reads back in order
    Given issue SUT-1 was created, assigned, and closed in that order
    When the audit history of SUT-1 is requested
    Then the events are returned in chronological order
    And each event shows actor, change kind, and time

  Scenario: events are append-only
    Given an event exists for issue SUT-1
    When any API operation attempts to modify or delete it
    Then the operation is rejected
