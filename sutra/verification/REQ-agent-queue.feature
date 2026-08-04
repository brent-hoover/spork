Feature: Agent work stack
  Issues assigned to an agent identity form its work stack; popping is the
  atomic handoff from tracker to working agent.

  Scenario: pop claims the issue
    Given issue SUT-1 is assigned to agent "claude" with status "open"
    When "claude" pops its work stack
    Then it receives SUT-1
    And SUT-1 has status "in-progress"
    And an "issue.status-changed" event with subject SUT-1, actor "claude", and a timestamp is recorded

  Scenario: concurrent pops never collide
    Given issues SUT-1 and SUT-2 are assigned to agent "claude" with status "open"
    When two instances of "claude" pop concurrently
    Then one instance receives SUT-1 and the other receives SUT-2

  Scenario: oldest issue comes first
    Given issue SUT-1 was assigned to "claude" before issue SUT-2
    When "claude" pops its work stack
    Then it receives SUT-1

  Scenario: blocker is worked first
    Given issue SUT-1 is assigned to "claude"
    And SUT-1 is blocked by issue SUT-2, also assigned to "claude" and open
    When "claude" pops its work stack
    Then it receives SUT-2

  Scenario: externally blocked issues are skipped
    Given issue SUT-1 is assigned to "claude"
    And SUT-1 is blocked by an open issue assigned to "human-brent"
    And issue SUT-3 is assigned to "claude" and unblocked
    When "claude" pops its work stack
    Then it receives SUT-3

  Scenario: non-open statuses are never handed out
    Given issues assigned to "claude" with statuses "blocked", "deferred", "in-progress", and "complete"
    And issue SUT-9 assigned to "claude" with status "open"
    When "claude" pops its work stack
    Then it receives SUT-9
    And every non-open issue retains its original status and stays assigned to "claude"
    Given SUT-9 is no longer assigned to "claude"
    When "claude" pops its work stack
    Then it receives an explicit empty result

  Scenario: empty stack is not an error
    Given agent "claude" has no open assigned issues
    When "claude" pops its work stack
    Then it receives an explicit empty result

  Scenario: archived project issues are never handed out
    Given issue SUT-4 is assigned to "claude" in an archived project
    And issue SUT-5 is assigned to "claude" in an active project
    When "claude" pops its work stack
    Then it receives SUT-5
    And SUT-4 is unmutated

  Scenario: same-key replay claims nothing new
    Given "claude" popped its work stack with idempotency key "K" and received SUT-1
    When the pop is replayed with idempotency key "K"
    Then the response is identical to the original
    And no additional issue is claimed
