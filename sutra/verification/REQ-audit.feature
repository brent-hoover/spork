Feature: Issue audit history
  The event stream is the audit trail — one stream, consumed as both
  notifications and per-issue history.

  # AC-audit-mutations enumerates six mutation kinds. Each row asserts the
  # event lands on SUT-1's own subject — an event recorded against the wrong
  # subject (or none) is missing from this issue's history whatever else it
  # proves. Comments arrive by three anchors and each resolves its subject
  # separately, so all three are rows here.
  Scenario Outline: every mutation is recorded
    Given issue SUT-1 exists
    When <actor> applies <mutation> to SUT-1
    Then an event exists for SUT-1 with actor <actor>, kind "<kind>", and a timestamp

    Examples:
      | actor      | mutation                          | kind                 |
      | "operator" | its creation                      | issue.created        |
      | "claude"   | a status change                   | issue.status-changed |
      | "claude"   | an assignment                     | issue.assigned       |
      | "claude"   | a label                           | issue.labeled        |
      | "claude"   | a comment                         | comment.created      |
      | "claude"   | a comment on one of its documents | comment.created      |
      | "claude"   | a comment on one of its reviews   | comment.created      |
      | "claude"   | a document link                   | doc.linked           |
      | "claude"   | a relation to another issue       | issue.relation-added |

  Scenario: history reads back in order
    Given issue SUT-1 was created, assigned, and closed in that order
    When the audit history of SUT-1 is requested
    Then the events are returned in chronological order
    And each event shows actor, change kind, and time

  Scenario: events are append-only
    Given an event exists for issue SUT-1
    When any API operation attempts to modify or delete it
    Then the operation is rejected
