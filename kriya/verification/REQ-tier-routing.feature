Feature: Model-tier routing
  Which model each agent role runs on is configuration. Missing
  configuration fails loudly, changes need no code, and every run
  records what actually ran.

  Scenario: roles map to tiers in configuration
    Given kriya's routing configuration
    Then each role — PM, SA, PO, dev — maps to a model tier there
    And no model identifier is hardcoded in kriya's source

  Scenario: a missing tier fails loudly
    Given a routing configuration with no entry for the SA role
    When kriya starts
    Then startup fails naming the unconfigured role
    And no silent default is applied

  Scenario: config changes take effect and runs record models
    Given the dev role is routed to tier "fast"
    When the operator changes the dev role to tier "deep" and a new run starts
    Then the dev agent runs on tier "deep" with no code change
    And every agent invocation durably records its role, configured tier, and the resolved model that actually ran
    And a plan-scoped PM invocation is recorded even though no BuildRun exists yet
