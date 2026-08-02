# AVSpec 0.1 pressure test - spork-suite

The earlier ready spec was speculative and has been removed. This directory is
now a draft seed for eliciting the real product shape.

Current observed limitation: AVSpec 0.1 guided-QA is a gap-closing loop over the
verifier, not a full forward-elicitation interview. With an empty draft it asks
for the first requirement, then acceptance, task, and test mappings. It does not
yet proactively ask for stack, principles, constraints, components, contracts,
data model, configuration, application surfaces, or deployment shape.

The Python rewrite changes the first interview level:

1. complete module/app/service list
2. architecture layers and allowed dependency directions
3. API or message contracts at those boundaries
4. the top-level suite constitution (`principles` and `constraints`)
5. high-level suite capabilities
6. acceptance criteria, tasks, tests, and later implementation details
7. per-module implementation stacks through `components[].stack`

This keeps the interview at the suite/module level before it asks for acceptance
tests. Stack is no longer treated as a suite-wide discovery prerequisite; each
module can declare its own stack during implementation planning.
