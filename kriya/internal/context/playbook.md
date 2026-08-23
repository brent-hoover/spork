# How kriya expects you to work

## The pair-programming loop

Every commit you make is reviewed as it lands. Work in small commits — one
coherent change each — rather than accumulating a branch and asking for a
review at the end. A review round is enqueued for each commit you make, and
the findings come back to you verbatim.

When findings arrive: address every one, then stop. Do not restate them, do
not argue with them in the commit message, and do not bundle unrelated work
into the fix. The next commit is what closes the round, and it is answered on
the review job by naming that commit.

The loop exits only when a round on the branch head reports nothing.

## Reviews

You do not drive roborev yourself. Kriya enqueues every review, reads every
verdict, responds on the job, and closes it. Do not run `roborev` commands, and
do not adopt, cancel, or close a job — kriya acts only on jobs it created, and
a job you touch is one it can no longer account for.

## The gate chain

Your work is judged by the target project's own commands, in this order:
review pass, then test, structure, typing, arch, and branch-coverage, then
mutation. The chain is inviolable and there is no waiver. A failing gate comes
back to you with the tool's own output; fixing the code or amending the
project's spec are the only two ways forward.

Mutation runs last and only once everything else holds. A surviving mutant is
a named test gap, not a suggestion.

## Tests come first

No behaviour is implemented before the test mapped to its acceptance criterion
exists and fails. A test that cannot fail is not a test — when you write one
for a claim about what the system does NOT do, prove it by planting the thing
it forbids and watching the test go red.

## Commits

Conventional commit format. The subject says what changed; the body says why,
and records what you verified. No co-authorship trailers.
