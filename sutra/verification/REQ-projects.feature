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

  # "Read-only" is a claim about EVERY door, and the scenario above samples
  # one of them — creating an issue. That is the shape catalogued as defect
  # species 1: an AC naming a set, discharged by a scenario exercising one
  # member. Twenty-three call sites enforce the freeze and nineteen distinct
  # operations reach them, including the far side of a relation, the second
  # anchor of a thread, and each of a comment's three anchors — every one a
  # place where a check could be missing and nothing else would notice,
  # because a laxer door's failure mode is also a refusal.
  #
  # The project is fully populated BEFORE it freezes: each door needs a real
  # target to aim at, and a door that 404s has not been shown to refuse.
  Scenario: every door into an archived project refuses the write
    Given archived project "OTH" holds an issue, a relation, a label, a document, a review, and a thread
    Then every write into "OTH" is refused
      | door                                        |
      | create an issue                             |
      | update an issue                             |
      | change an issue's status                    |
      | assign an issue                             |
      | add a relation from an archived issue       |
      | add a relation to an archived issue         |
      | remove a relation from an archived issue    |
      | remove a relation to an archived issue      |
      | attach a label                              |
      | detach a label                              |
      | create a document                           |
      | save a document version                     |
      | link a document to an issue                 |
      | unlink a document from an issue             |
      | comment on an issue                         |
      | comment on a document version               |
      | comment on a review                         |
      | open a review                               |
      | record a review verdict                     |
      | consume a review approval                   |
      | resubmit a review                           |
      | import a project-anchored thread            |
      | import an issue-anchored thread             |
      | re-anchor a thread into the archived project|
    And every read of "OTH" still works
