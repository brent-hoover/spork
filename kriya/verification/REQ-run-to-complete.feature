Feature: Run to complete
  A build runs until the current head plan's every ticket is complete.
  The finish line moves with supersession, idling is not exiting, and a
  stall is reported with its cause instead of spinning.

  Scenario: the loop runs until nothing is workable
    Given a head plan with workable tickets
    Then kriya keeps popping and building
    Given every remaining ticket is blocked or in flight
    Then kriya idles without exiting
    When a ticket unblocks
    Then popping resumes

  Scenario: completion never fires against a partial plan
    Given an activated head plan still mid-phase, without its completed stamp
    And every ticket created so far is complete
    When completion detection runs
    Then no completion attempt starts — the ticket set is not yet whole
    When decomposition passes its final assignment barrier and stamps completed
    Then completion detection arms

  Scenario: a rejected completion review reworks within its attempt
    Given a build-completion review receives changes-requested
    When kriya records review-resubmitting — persisting the revision-scoped resubmission key and the pending report reference before any external call
    And kriya regenerates the completion report
    Then the resubmission goes through sutra's resubmit path with a new document version
    And completion_report_doc and completion_revision update transactionally within the same attempt — key and epoch unchanged — and the pending fields reconcile
    And the epic closes on the new revision's approval, never stranding the target

  Scenario: an initial submission crash before the report document recovers exactly once
    Given review-submitting rotated the claim set with its doc key and pending report, and the crash hit before the document version existed
    When recovery runs
    Then the keyed doc mutation replays and creates the version, records it, and the keyed review creation follows
    And exactly one document version and one review exist

  Scenario: an initial submission crash between document and review recovers exactly once
    Given the report document was created under its key and the crash hit before the review-create call
    When recovery replays the keyed doc mutation
    Then sutra returns the same version — no second version appends
    And the keyed review creation then runs, exactly once

  Scenario: a rework crash before the document version recovers exactly once
    Given review-resubmitting was recorded with its keys and the crash hit before the new document version existed
    When recovery runs
    Then the keyed doc mutation replays and creates the version, and the keyed resubmission follows
    And exactly one document version and one resubmission exist

  Scenario: a rework crash between document and resubmission recovers exactly once
    Given the document version was created under its key and the crash hit before the review resubmission
    When recovery replays the keyed doc mutation
    Then sutra returns the same version — no second version appends
    And the keyed resubmission then runs, exactly once

  Scenario: a rework crash after resubmission recovers exactly once
    Given sutra accepted the resubmission and the crash hit before completion_report_doc and completion_revision updated
    When recovery replays the keyed resubmission
    Then sutra returns the original response and the fields reconcile from it
    And nothing double-submits and nothing is lost

  Scenario: an open unplanned ticket blocks completion
    Given the activated head plan's every ticket is complete
    And a mapped unplanned ticket for the target — parented under the epic at bind time — is still open
    When completion detection runs
    Then no completion attempt starts and popping continues
    And sutra's close gate would refuse the epic anyway — the unplanned ticket is a descendant

  Scenario: a blocked unplanned ticket blocks completion
    Given the activated head plan's every ticket is complete
    And a mapped unplanned ticket for the target sits in status "blocked", never popped and never bound
    When completion detection runs
    Then no completion attempt starts — blocked is active work, resolved through the live queue and mappings

  Scenario: a reopened detached ticket still reopens the epic
    Given a target ticket was detached after the epic closed and is then reopened
    When kriya consumes the reopen event and checks its operation id against the sibling events
    Then no status-changed event for the epic with a reopened status appears under the operation — a relation event naming the epic is not evidence — and the advance records the operation, cascade_verified as unreached, and reopen_owed true
    And until the owed reopen executes, completion reads fall back to live target work — the complete-looking epic alone never reports done
    And kriya issues the explicit durable keyed reopen, the epoch advances, and popping resumes
    And the epic is never left complete while kriya's stamp is cleared

  Scenario: a detached ticket cannot enable a stale completion
    Given a planned ticket was detached from the epic through the relation API and later reopened
    Then the detach advanced the epic's subtree revision when it happened
    And any completion claim recorded before the detach fails its revision fence
    And the detached ticket still blocks completion detection through its target scope

  Scenario: reattachment reconciliation closes the claim-after-detach race
    Given a target ticket was detached and then completed
    When a completion attempt begins
    Then reattachment reconciliation re-parents it beneath the epic before the claim's subtree revision is captured
    And a later reopen of that ticket is a descendant reopen — the cascade reopens the epic and the revision moves
    Given the ticket is instead detached after the claim was captured
    Then the removal advanced the epic's revision and the claim fails its fence

  Scenario: a post-close detachment rotates the completion claim
    Given the epic closed and a complete target ticket is then detached
    When kriya consumes the detachment event, whose payload names the removed relation's kind and the detached child
    Then the completion epoch advances with cause detachment and the stamp clears
    And recompletion runs as a fresh epoch-scoped attempt — new keys, a new review, fresh approval — never replaying the spent one

  Scenario: a detach between reattachment and capture aborts the attempt
    Given reattachment reconciliation completed and a target ticket was then detached before the revision capture
    When the post-capture membership verification runs
    Then the ticket reads as missing from the epic's subtree — the capture already includes the detach's increment
    And the attempt aborts and reconciliation re-runs, so no claim is ever captured over a hole in the subtree

  Scenario: completion is the head plan done and the epic closable
    Given the activated head plan bears its completed stamp
    And every ticket of the activated head plan is complete
    And no other target-scoped ticket is open, in progress, or blocked
    Then kriya records review-submitting with the deterministic submission key, the doc key, and the pending report reference before any sutra call
    And the keyed doc mutation creates the report document and its returned version is persisted before the review-create call
    And kriya submits a build-completion review on the umbrella epic with that recorded document version as its deliverable
    And a crash before the review id lands is recovered by replaying the persisted request under the same submission key — sutra returns the original review if it landed and creates it otherwise, never a second one
    And retired tickets resting deferred under the epic do not block its close
    When the human approves the review
    Then the epic close is attempted through sutra naming the attempt's completion review at its recorded revision with its approval's verdict event, under the closing state's persisted idempotency key, and sutra's no-open-children gate is the authoritative check — a stale revision rejects instead of spending a later approval
    And a crash after the close succeeds but before kriya records it recovers by replaying under the same key — sutra returns the original success, never a close-used conflict
    And after the close succeeds, completion is stamped on the BuildTarget row — a CAS requiring the current epoch to still equal the immutable claim epoch recorded at submission — and popping stops for that target

  Scenario: a reopen-and-recomplete before the event is consumed still fences
    Given a completion review was submitted with the epic's subtree revision recorded as 7
    And a child ticket reopens and recompletes before kriya consumes any reopen event, advancing the revision to 9
    When the approval event arrives and kriya attempts the epic close with expected subtree revision 7
    Then sutra rejects the close — history moved even though current state matches
    And kriya advances its epoch and a fresh attempt with fresh approval is required

  Scenario: reapproval after a conflicted close gets a fresh key
    Given an epic close conflicted because the completion approval was reversed mid-flight
    When the review is reapproved
    Then the new approval event yields a fresh close key
    And the retried close issues under it — never replaying the cached conflict

  Scenario: a stale approval never stamps an unfinished target
    Given a build-completion review recorded with claim epoch 3
    And a supersession or a ticket reopen has since advanced the current epoch to 4
    When the approval event arrives
    Then the completion CAS fails, nothing is stamped, and the stale approval surfaces to the operator

  Scenario: a reopen between validation and stamping cannot slip through
    Given kriya's advisory read saw every head-plan ticket complete
    And a child ticket reopens in sutra before the epic close executes
    When kriya attempts the epic close
    Then sutra refuses it server-atomically under its no-open-children gate
    And nothing is stamped and the epoch advances when kriya observes the reopen

  Scenario: a reopen after the close heals the stamp
    Given the epic close succeeded and the completion stamp has not yet landed
    When a child ticket reopens in sutra
    Then sutra's cascade reopens the epic in the same transaction — the epic is never closed over an open child
    And a stamp that lands in the window is cleared and the epoch advanced when the reopen event arrives
    And popping resumes from the epic reopen itself, not from the mirror healing

  Scenario: a stale close is serialized or compensated, never left standing
    Given an epic close is in flight for claim epoch 3
    When a supersession advances the epoch to 4
    Then the supersession first resolves the in-flight close's outcome before issuing its reopen
    And if the stale close landed, a durable compensating reopen is driven through the same lifecycle
    And the current target's epic is never left closed by a claim whose CAS failed

  Scenario: completion clears when work returns
    Given a build target whose completion is stamped
    When a supersession installs a new head plan or a completed ticket reopens
    Then the completion epoch advances and the completion stamp is atomically cleared
    And a ticket-reopen advance issues no sutra call only when its recorded verification shows the epic's own status-changed reopen event under the same operation
    And a supersession advance calls the keyed reopen only when the durable lifecycle shows a close ever ran
    And popping resumes for the target

  Scenario: any epic reopen forces a fresh completion
    Given a completed target whose epic reopened because deferred work activated
    Then the completion epoch advances and the stamp clears when the reopen event is consumed
    And recompletion runs under a new epoch-scoped key with a fresh review — the spent one cannot replay
    Given a completed target whose epic reopened because an active subtree was attached
    Then the same epoch advance, clear, and fresh-review requirement apply
    And a cascade-driven advance skips the external reopen call only with recorded reached verification — the epic's own status-changed event under the operation — whatever the prior completion state

  Scenario: replayed advance events are no-ops in any order
    Given advance events "E1" then "E2" were consumed, each inserting its keyed CompletionAdvance record
    When "E1" is replayed after "E2"
    Then the insert collides on the (target, event) key and the epoch does not advance
    And no valid completion is cleared and no recorded cause is overwritten

  Scenario: a verdict ABA cannot poison the rework keys
    Given a completion rework got a cached conflict, the review was then approved, and a later changes-requested arrived at the same revision
    When the new rework begins
    Then the new changes-requested event rotates both the resubmission and report keys
    And the retried mutations issue under fresh keys — never replaying the stale document or the cached conflict

  Scenario: supersession moves the finish line
    Given a build mid-flight when a supersession activates a new head plan
    Then completion is judged against the new head's ticket set
    And a superseded plan's ticket set never satisfies completion

  Scenario: state is inspectable from the CLI
    Given a build with runs in flight and a completed build alongside it
    When the operator runs the status command
    Then builds, runs, and their gate positions are shown, including the completed build's durable completion declaration
    When the status command runs with --json
    Then the same state is emitted machine-readably for agents and scripts

  Scenario: recompletion needs a fresh review
    Given a completed target whose epoch advanced after a ticket reopened and new work merged
    When every head-plan ticket is complete again
    Then a new completion attempt rotates the whole claim-field set with a key derived from the new epoch
    And a new build-completion review is submitted for fresh human approval
    And recovery can never adopt the prior epoch's approved review — its key names the old epoch

  Scenario: a detached reopen-and-recomplete cannot slip a stale completion
    Given a detached target ticket reopened and recompleted before kriya consumed its events
    And the live epic reads complete with no visible active target work
    When completion is read from an untrusted stamp
    Then done is withheld until the feed is processed through the watermark captured atomically with the live read
    And draining consumes the reopen, advances the epoch, and kills the stale claim — completion needs a fresh review

  Scenario: an unattributed ticket in a multi-target project pauses completion
    Given a project with two target mappings and an unbound blocked ticket attributed to neither
    When completion detection runs for either target
    Then it refuses to arm — the active ticket is ambiguous
    And a durable AttributionAmbiguity row upserts on its key and the inbox lists it
    When the operator resolves it through the attribution action
    Then the selected target and the parenting mutation's key persist with state resolving before sutra is called
    And a crash after the relation lands is recovered by replaying the keyed mutation, stamping resolved with the outcome
    And the row stamps resolved and the other target's completion detection arms normally

  Scenario: a previously bound unplanned ticket is never ambiguous
    Given a multi-mapping project where an unplanned ticket was once bound to target "A", then detached and reopened
    When completion detection attributes active work
    Then the ticket attributes to "A" through its BuildRun's persisted target binding
    And no ambiguity row is created and target "B" is unaffected

  Scenario: new work created after completion wakes the target
    Given a completed target whose stamp is trusted
    When a new active issue is created and attributes to that target
    Then the epoch advances with cause new-work, keyed by the creating event, and the stamp clears
    And the ticket is parented under the epic through the keyed mutation — attaching active work beneath the complete epic cascades the reopen
    And kriya verifies the cascade through the mutation's operation id, issuing the explicit keyed reopen only if the epic provably went unreached
    And popping resumes; nothing strands
    Given the creation is ambiguous in a multi-mapping project
    Then no epoch advances — the open ambiguity row suppresses completion for every candidate target
    When the operator resolves the attribution
    Then only the selected target advances, through its parenting attachment's cascade, and the other candidates resume untouched

  Scenario: a stall surfaces with its cause
    Given no ticket is workable, none are in flight, and the epic cannot close
    Then a durable Stall row records the condition and its cause
    And the stall appears in the operator inbox from that row
    And resolution stamps the row rather than deleting it
    And kriya neither spins nor declares the build done
