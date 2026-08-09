Feature: Repo-anchored projects

  Scenario: init anchors a project to a repo
    Given a repository directory with no sutra project
    When "sutra init --key SUT --name Sutra" is run there
    Then project "SUT" exists with the repo's path recorded
    And a local marker file links the directory to "SUT"
    # The report is the only thing distinguishing a successful init from a
    # silent one, and it is written last on purpose: everything above it
    # has to have happened first.
    And it reports "SUT" and the project it created

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

  # Two live projects, because "default listings" is a set and one member
  # cannot tell a listing that hides archived rows from one that stops after
  # its first row. Projects come back in key order, so "WEB" — the later key
  # — is exactly the row a truncating listing drops. Both live projects are
  # named by steps that CREATE them: a project the scenario only assumes
  # exists leaves the assertion looking for an empty id, which every
  # response body contains.
  Scenario: archive hides without deleting
    Given live project "SUT" exists
    And live project "WEB" exists
    And project "OTH" is archived
    Then default project listings hold "SUT" and "WEB", and not "OTH"
    And writing to "OTH" is rejected
    And reading "OTH"'s content still works
