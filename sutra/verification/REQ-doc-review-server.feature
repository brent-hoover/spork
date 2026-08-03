Feature: Doc review in the browser
  Humans review and comment on docs in place, with threads.

  Scenario: doc renders in the browser
    Given document "design" has markdown content
    When it is opened in the web UI
    Then the content renders formatted
    And the shown version is the latest unless one is chosen

  Scenario: comments pin to a spot in a version
    Given document "design" is at version 2
    When a reader comments on its third block
    Then the comment anchors to version 2 at that block
    And viewing version 3 does not silently orphan the comment

  Scenario: discussion sits beside the doc
    Given comments and replies exist on document "design"
    When the doc is open in the web UI
    Then the threaded discussion is visible alongside the content

  Scenario: viewer learns of new versions
    Given "design" is open in a browser
    When a new version is saved
    Then the viewer is notified or refreshed to the new version
