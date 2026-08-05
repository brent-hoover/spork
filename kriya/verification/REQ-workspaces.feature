Feature: Isolated workspaces
  Every BuildRun works in its own worktree on a ticket-named branch.
  The workspace is durably recorded, resumable after a crash or rework
  round-trip, and cleanup can never destroy unmerged work.

  Scenario: each run gets an isolated worktree
    Given two BuildRuns start for tickets "KRI-1" and "KRI-2"
    Then each run has its own worktree on a branch named for its ticket
    And each branch is cut from the project's default branch
    And uncommitted changes in one worktree are invisible to the other

  Scenario: the workspace is durably recorded
    Given a BuildRun starts and its worktree is created
    Then the BuildRun durably records the workspace path, branch, and base commit
    And recovery and the TUI locate the run's work from the record alone

  Scenario: a resumed run reattaches to its workspace
    Given a BuildRun crashed with an existing worktree matching its record
    When the run resumes
    Then it reattaches to the same worktree and branch
    And no second workspace is created for the run
    Given the recorded worktree is missing or inconsistent with the record
    Then the run surfaces the discrepancy instead of silently recreating state

  Scenario: cleanup never loses unmerged work
    Given a ticket completes and its branch is merged
    Then the run's worktree is removed
    Given a ticket is retired with unmerged commits on its branch
    Then the worktree may be removed but the branch survives
    And only merge or explicit operator disposal deletes the branch
