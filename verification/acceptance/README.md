# Acceptance tests

One `.feature` (Gherkin) file per requirement, named `REQ-XXX.feature`. Each
`AC-*` in the manifest maps to a scenario here via `REQ-XXX.feature#<scenario>`,
or to a test file elsewhere via `tests/<path>::<id>`.

The EARS criterion in the requirements layer translates almost mechanically:

```
EARS:  When a valid order is submitted, the system shall persist it and return 201.
Gherkin:
  Scenario: valid order is persisted        # <- AC maps to this scenario name
    Given a valid order payload
    When it is submitted to POST /orders
    Then the response status is 201
    And the order is retrievable by its id
```

These scenarios are what gate 4 runs against the built code. Before code exists
they still serve gate 2: the verifier confirms every `AC-*` points at a real
scenario file.
