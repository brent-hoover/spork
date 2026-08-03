Feature: Repo-anchored projects

  Scenario: init anchors a project to a repo
    Given a repository directory with no sutra project
    When "sutra init --key SUT --name Sutra" is run there
    Then project "SUT" exists with the repo's path recorded
    And a local marker file links the directory to "SUT"

  Scenario: repo context is implicit
    Given a repository initialized for project "SUT"
    When "sutra issue list" is run inside it
    Then it lists SUT's issues without a project flag
    When "sutra issue list" is run outside any initialized repo
    Then it requires an explicit project flag

  Scenario: content scopes to its project
    Given projects "SUT" and "OTH" both have issues and docs
    When SUT's issues and docs are listed
    Then nothing from "OTH" appears

  Scenario: archive hides without deleting
    Given project "OTH" is archived
    Then "OTH" is absent from default project listings
    And writing to "OTH" is rejected
    And reading "OTH"'s content still works
