package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"kriya/internal/agent"
)

// Ticket kinds.
//
// A SPIKE answers a risk: its deliverable is a documented finding, it blocks
// what depends on the answer, and it never enters the gate chain. An
// IMPLEMENTATION ticket is a thin end-to-end slice and does.
const (
	KindSpike          = "spike"
	KindImplementation = "implementation"
)

// ticketSchema constrains the PM agent's reply.
//
// A schema rather than prose: a build engine that parses an agent's paragraphs
// is guessing, and a guess about which acceptance criteria a ticket covers is
// a build that verifies the wrong thing. additionalProperties is closed so a
// reply carrying fields kriya does not understand is rejected rather than
// silently truncated.
const ticketSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["tickets"],
  "properties": {
    "tickets": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["title", "body", "kind", "criteria"],
        "properties": {
          "title": {"type": "string", "minLength": 1},
          "body": {"type": "string"},
          "kind": {"enum": ["spike", "implementation"]},
          "skeleton": {"type": "boolean"},
          "layers": {
            "type": "array",
            "items": {"type": "string", "minLength": 1}
          },
          "blocks": {
            "type": "array",
            "items": {
              "type": "string",
              "pattern": "^(REQ|AC)-[A-Za-z0-9-]+$"
            }
          },
          "criteria": {
            "type": "array",
            "minItems": 1,
            "items": {
              "type": "string",
              "pattern": "^(REQ|AC)-[A-Za-z0-9-]+$"
            }
          }
        }
      }
    }
  }
}`

// Ticket is one tracer bullet the PM proposed.
type Ticket struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	// Kind decides which path the ticket takes. A spike carries a DOCUMENT
	// deliverable and skips the gate chain entirely — there is no code to
	// gate — so guessing would send research work through the gates and fail
	// it for having none.
	Kind string `json:"kind"`
	// Blocks are the criteria whose work must wait for this spike's answer.
	// Criteria rather than ticket references: kriya already validates ids
	// against the snapshot, and an agent naming its own tickets would be
	// trusted about something it can get wrong.
	Blocks []string `json:"blocks,omitempty"`
	// Criteria are the AC ids this ticket satisfies. They are validated
	// against the snapshot, not trusted.
	Criteria []string `json:"criteria"`
	// Layers are the horizontal strata this ticket cuts through — "http",
	// "store", "cli". A slice is named by what it crosses, so this is how a
	// thin END-TO-END slice is told apart from a stripe of one layer.
	Layers []string `json:"layers,omitempty"`
	// Skeleton marks the walking skeleton: the one implementation ticket
	// that touches every layer the plan touches. It is assigned first among
	// implementation work, so it is the first thing workable once the
	// blocking risks retire.
	Skeleton bool `json:"skeleton,omitempty"`
	// Plan is the decomposition key that produced this ticket. A target
	// accumulates plans, so without it a ticket set cannot be told apart from
	// the union of every generation's tickets.
	Plan string `json:"-"`
	// Consumed marks a row a successor plan's retirement has settled, and
	// Disposition says how. A consumed row no longer participates in binding:
	// a pop landing on it must reach the decomposition that produced its
	// acceptance criteria, not a head that never planned it.
	Consumed    bool   `json:"-"`
	Disposition string `json:"-"`
	// DeferAttempt is how many deferrals of this row sutra has already
	// settled — successfully or as a conflict. It is DURABLE because sutra
	// settles a rejected request under its key: an attempt counter that
	// restarted at zero on every retirement pass would present the same
	// poisoned keys forever, and the ticket could never be deferred once its
	// status stabilised.
	DeferAttempt int `json:"-"`
	// Ordinal is this ticket's position in its plan. The mutation sequence
	// addresses tickets by it, so a resumed replay needs it to line the two
	// up — a map keyed by title would break on two tickets sharing one.
	Ordinal int `json:"-"`
	// IssueID is the sutra issue this ticket became. Not from the agent —
	// stamped after creation, so a review can hang off the right issue
	// without anything having to look it up by title.
	IssueID string `json:"-"`
}

type ticketReply struct {
	Tickets []Ticket `json:"tickets"`
}

// Decompose asks the PM agent for tracer tickets and creates them in sutra.
//
// Every ticket is parented under the target's epic, and every criterion it
// cites must appear in the pinned snapshot — "every ticket's acceptance
// criteria cite REQ and AC ids present in the snapshot". Citations are checked
// rather than trusted: an agent that invents an AC id produces a ticket whose
// completion can never be verified against anything.
func (i Intaker) Decompose(
	ctx context.Context, target BuildTarget, snap Snapshot, generation int, actor string,
) ([]Ticket, error) {
	// From the SNAPSHOT's hash, never the target row's. EnsureEpic returns
	// the existing target with its ORIGINAL hash on purpose — the epic's
	// idempotency keys were derived from it, and one epic umbrellas a target
	// forever. A plan is the opposite: it identifies the decomposition of the
	// spec version in hand, so after an H1 -> H2 intake the plan must be H2's.
	// Taking the target's hash made the key, the row and every report claim
	// H1 while the tickets came from H2.
	key := DecompositionKey(target.ProjectKey, target.TargetKey, snap.Hash, generation)

	// FIRST — before the agent, before the tracker, before anything. A
	// request that decomposed before resolving its own key would compete
	// with the plan it is retrying: two decompositions for one request, one
	// superseding the other. The key is unique, so resolving here makes a
	// retry find its own plan and leaves competition possible only between
	// DIFFERENT keys.
	//
	// Ahead of the AGENT specifically. The PM call is the expensive half of a
	// decomposition, and asking it again to then discard its answer is a bill
	// for nothing — and a second, possibly different, plan for a key that
	// already has one.
	resolution := Resolution{Fresh: true}
	if i.Plans != nil {
		var err error
		if resolution, err = Resolve(ctx, i.Plans, key); err != nil {
			return nil, err
		}
		if !resolution.Fresh && !resolution.Resume {
			return i.report(ctx, resolution.Plan)
		}
	}

	if resolution.Resume {
		return i.resume(ctx, target, resolution.Plan, actor)
	}
	return i.decomposeFresh(ctx, target, snap, key, generation, actor)
}

// decomposeFresh plans a key nothing has planned yet.
func (i Intaker) decomposeFresh(
	ctx context.Context, target BuildTarget, snap Snapshot, key string, generation int, actor string,
) ([]Ticket, error) {
	tickets, err := i.proposeTickets(ctx, target, snap)
	if err != nil {
		return nil, err
	}

	// Write-ahead, before the first ticket exists. A crash mid-decomposition
	// must leave a row saying a plan was being built: completion detection
	// reads this stamp, and no row at all is indistinguishable from a target
	// nobody has planned.
	plan := Plan{
		Key: key, TargetKey: target.TargetKey, SpecHash: snap.Hash,
		Generation: generation, State: PlanPending,
	}
	// CLAIMED, not written. Resolve is a read, so a concurrent twin can have
	// resolved fresh too; only one insert can win, and the loser becomes an
	// ordinary same-key retry of the winner rather than a second
	// decomposition overwriting the first.
	if i.Plans != nil {
		created, err := i.Plans.Claim(ctx, plan)
		if err != nil {
			return nil, err
		}
		if !created {
			return i.afterLosingTheClaim(ctx, target, key, actor)
		}
	}
	// The PAYLOAD, before any call: the full ordered mutation sequence with
	// its derived keys. This is what makes a resume a replay rather than a
	// second decomposition.
	steps := buildSequence(plan, tickets)
	if err := i.writeSequence(ctx, plan, tickets, steps); err != nil {
		return nil, err
	}

	// The CAS, between the write-ahead payload and activation. A candidate
	// that filed tickets before winning the head would have decomposed
	// against a target another plan already owns.
	if err := i.takeTheHead(ctx, target, plan, actor); err != nil {
		return nil, err
	}
	return i.runPlan(ctx, target, plan, tickets, steps, actor)
}

// resume carries a plan's remaining phases forward from durable state.
//
// The PM is NOT asked again. Its answer is already persisted — as the ticket
// rows and the mutation sequence — and asking again risks a different plan
// whose changed requests would be sent under keys the first answer already
// settled, so sutra returns the originals and the plan becomes a mixture of
// two decompositions.
func (i Intaker) resume(
	ctx context.Context, target BuildTarget, plan Plan, actor string,
) ([]Ticket, error) {
	if i.Steps == nil || i.Tickets == nil {
		return nil, fmt.Errorf("plan %s must resume but nothing durable recorded it", Short(plan.Key))
	}
	steps, err := i.Steps.ForPlan(ctx, plan.Key)
	if err != nil {
		return nil, fmt.Errorf("read the sequence of plan %s: %w", Short(plan.Key), err)
	}
	if len(steps) == 0 {
		return nil, fmt.Errorf("plan %s has no recorded sequence to resume", Short(plan.Key))
	}
	tickets, err := i.plannedSet(ctx, plan)
	if err != nil {
		return nil, err
	}
	if len(tickets) == 0 {
		return nil, fmt.Errorf("plan %s has a sequence but no tickets to replay it against", Short(plan.Key))
	}
	// A plan that had not yet taken the head still must. Winning is
	// idempotent for the plan that already holds it: the CAS finds its own
	// key as head and the request resolves to itself.
	if plan.State == PlanPending {
		if err := i.takeTheHead(ctx, target, plan, actor); err != nil {
			return nil, err
		}
	}
	return i.runPlan(ctx, target, plan, tickets, steps, actor)
}

// runPlan activates a plan, replays its sequence and stamps it whole.
func (i Intaker) runPlan(
	ctx context.Context, target BuildTarget, plan Plan, tickets []Ticket, steps []Step, actor string,
) ([]Ticket, error) {
	// ACTIVE before the phases, not after. The spec is explicit that
	// activation precedes creation, wiring and assignment — which is what
	// makes "active without completed" a real state that a crash can leave
	// behind, and what a resumed retry recognises.
	plan.State = PlanActive
	if err := i.recordPlan(ctx, plan); err != nil {
		return nil, err
	}
	if err := i.executeSequence(ctx, target, plan, tickets, steps, actor); err != nil {
		return nil, err
	}
	// The final assignment barrier is passed: every ticket exists, is
	// recorded, is parented under the epic, is wired and is assigned. Only
	// now is the ticket set whole, and only now may completion detection arm.
	plan.Completed, plan.Tickets = true, len(tickets)
	if err := i.recordPlan(ctx, plan); err != nil {
		return nil, err
	}
	if err := i.activate(ctx, plan); err != nil {
		return nil, err
	}
	return tickets, nil
}

// planStepKey derives the idempotency key for one step of one plan.
//
// Scoped to the DECOMPOSITION KEY, which carries the intake generation.
// Keying on (target, spec hash) instead made an H1 -> H2 -> H1 revert present
// the FIRST plan's keys: sutra would settle each call under them and replay
// the original tickets, so the revert produced no new work at all — the exact
// replay the generation was added to the plan's identity to prevent.
func planStepKey(step, decompositionKey string) string {
	return idempotencyKey(step, decompositionKey, "")
}

// dependsOn reports whether a ticket covers any criterion a spike blocks.
func dependsOn(ticket Ticket, blocked map[string]bool) bool {
	for _, id := range ticket.Criteria {
		if blocked[id] {
			return true
		}
	}
	return false
}

// validateKinds rejects a reply whose kinds or blocks make no sense.
func validateKinds(tickets []Ticket, known map[string]bool) error {
	for _, t := range tickets {
		switch t.Kind {
		case KindSpike, KindImplementation:
		default:
			return fmt.Errorf("ticket %q has no kind: it must be %q or %q",
				t.Title, KindSpike, KindImplementation)
		}
		if t.Kind != KindSpike && len(t.Blocks) > 0 {
			// Blocking is what a RISK does. An implementation ticket
			// declaring it would serialize the plan behind work that answers
			// no question.
			return fmt.Errorf("ticket %q is an implementation ticket and cannot block", t.Title)
		}
		for _, id := range t.Blocks {
			if !known[id] {
				return fmt.Errorf("spike %q blocks %q, which is not in the snapshot", t.Title, id)
			}
		}
	}
	return nil
}

// recordPlan writes the plan row, when there is a store to write it to.
func (i Intaker) recordPlan(ctx context.Context, plan Plan) error {
	if i.Plans == nil {
		return nil
	}
	if err := i.Plans.Upsert(ctx, plan); err != nil {
		return fmt.Errorf("record %s plan for %s: %w", plan.State, plan.TargetKey, err)
	}
	return nil
}

// validateCitations rejects a ticket citing an id the snapshot does not carry.
func validateCitations(tickets []Ticket, known map[string]bool) error {
	for _, t := range tickets {
		for _, id := range t.Criteria {
			if !known[id] {
				return fmt.Errorf("ticket %q cites %q, which is not in the snapshot", t.Title, id)
			}
		}
	}
	return nil
}

// validateSlices holds the PM to the tracer-bullet shape.
//
// What "a thin end-to-end slice" means is only half mechanically checkable,
// and this says which half. END-TO-END is structural: a ticket names the
// layers it cuts through, and one that names fewer than two is a horizontal
// stripe — "add the column", "add the handler" — which is exactly the
// decomposition tracer bullets exist to prevent. THIN is not: no count of
// layers or criteria distinguishes a thin slice from a fat one, so that half
// stays an instruction in the prompt rather than a rule pretending to be
// enforced here.
//
// Spikes are exempt by the spec: a spike's deliverable is a documented
// finding, so it cuts through no layers and declaring some would be a lie.
func validateSlices(tickets []Ticket) error {
	layers := map[string]bool{}
	skeleton := -1
	implementations := 0

	for n, t := range tickets {
		if t.Kind != KindImplementation {
			if t.Skeleton {
				return fmt.Errorf("ticket %q is a %s and cannot be the walking skeleton", t.Title, t.Kind)
			}
			continue
		}
		implementations++
		named := normalizeLayers(t.Layers)
		// DISTINCT: counting entries let ["store", "store"] pass as a slice
		// while touching one layer, which is precisely the stripe the rule
		// exists to refuse.
		if len(named) < 2 {
			return fmt.Errorf(
				"implementation ticket %q touches %d layer(s) %v: a slice that touches one layer is a layer",
				t.Title, len(named), t.Layers)
		}
		for _, l := range named {
			layers[l] = true
		}
		if !t.Skeleton {
			continue
		}
		if skeleton >= 0 {
			return fmt.Errorf("tickets %q and %q are both the walking skeleton",
				tickets[skeleton].Title, t.Title)
		}
		skeleton = n
	}

	// A plan of nothing but research builds nothing. Decomposition turns a
	// snapshot into tracer bullets, and a set with no implementation ticket
	// has none — it would reach the completion protocol having shipped no
	// code, and be indistinguishable there from a build that succeeded.
	if implementations == 0 {
		return fmt.Errorf("the plan has %d tickets and no implementation work among them", len(tickets))
	}
	if skeleton < 0 {
		return fmt.Errorf("no ticket is the walking skeleton: %d implementation tickets and no skeleton among them",
			implementations)
	}
	// Every layer the PLAN touches, not every layer it can imagine. The
	// skeleton is the thinnest slice that proves the whole stack connects,
	// so a layer some later ticket reaches and the skeleton does not is a
	// layer whose wiring nothing has demonstrated.
	// Against the NORMALIZED names on both sides. Comparing normalized layers
	// to raw ones would make " http " and "http" different layers, so a
	// skeleton that does cover the plan would be refused for a stray space.
	covered := normalizeLayers(tickets[skeleton].Layers)
	for l := range layers {
		if !contains(covered, l) {
			return fmt.Errorf("the walking skeleton %q does not touch layer %q, which the plan does",
				tickets[skeleton].Title, l)
		}
	}
	return validateSkeletonLeads(tickets, skeleton)
}

// validateSkeletonLeads refuses a plan where the skeleton can be bypassed.
//
// Assigning it first is not enough. sutra SKIPS blocked work, so a spike
// blocking the skeleton while some other implementation ticket is unblocked
// hands that other ticket out first — the skeleton is assigned first and
// worked second, and the guarantee is gone precisely when a risk is in play,
// which is when it matters most.
//
// The rule is therefore about the blockers, not the order: anything blocking
// the skeleton must block every other implementation ticket too. A risk the
// skeleton waits on is a risk the whole implementation plan waits on, because
// the skeleton is what proves the stack those tickets extend actually
// connects.
func validateSkeletonLeads(tickets []Ticket, skeleton int) error {
	for _, spike := range tickets {
		if spike.Kind != KindSpike {
			continue
		}
		blocked := make(map[string]bool, len(spike.Blocks))
		for _, id := range spike.Blocks {
			blocked[id] = true
		}
		if !dependsOn(tickets[skeleton], blocked) {
			continue
		}
		for n, other := range tickets {
			if n == skeleton || other.Kind != KindImplementation {
				continue
			}
			if !dependsOn(other, blocked) {
				return fmt.Errorf(
					"spike %q blocks the walking skeleton %q but not %q, which would then be worked first",
					spike.Title, tickets[skeleton].Title, other.Title)
			}
		}
	}
	return nil
}

// normalizeLayers reduces a layer list to the distinct layers it names.
//
// Trimmed before the blank check, because the schema requires only minLength
// 1: " " is a valid string and an empty layer name. Untrimmed, ["store", " "]
// counted as two layers and passed as an end-to-end slice while touching one.
func normalizeLayers(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// assignmentOrder lists the tickets of one kind in the order they go out.
//
// FIFO is the whole mechanism: sutra offers assigned work in the order it was
// assigned, so this order IS which ticket an agent gets first. The walking
// skeleton leads the implementation phase for that reason — "the first
// implementation ticket workable once blocking risks retire" is a claim about
// pop order, and pop order is decided here, not by anything the tracker knows.
func assignmentOrder(tickets []Ticket, kind string) []int {
	order := make([]int, 0, len(tickets))
	for n, t := range tickets {
		if t.Kind == kind && t.Skeleton {
			order = append(order, n)
		}
	}
	for n, t := range tickets {
		if t.Kind == kind && !t.Skeleton {
			order = append(order, n)
		}
	}
	return order
}

// plannedSet returns an existing plan's tickets without mutating anything.
//
// The idempotent answer to a same-key retry that must not re-decompose: an
// already-complete plan, a superseded one, a parked one and a terminal one all
// land here. The recorded ticket set is what the caller asked for — the agent
// is never invoked again, and no tracker call is made.
func (i Intaker) plannedSet(ctx context.Context, plan Plan) ([]Ticket, error) {
	if i.Tickets == nil {
		return nil, nil
	}
	// By PLAN. A target accumulates plans as decompositions supersede each
	// other, so answering a retry from the target's tickets would hand back
	// every generation's work as though one decomposition had produced it.
	tickets, err := i.Tickets.ForPlan(ctx, plan.Key)
	if err != nil {
		return nil, fmt.Errorf("read the planned tickets of plan %s: %w", Short(plan.Key), err)
	}
	return tickets, nil
}

// proposeTickets asks the PM for a plan and holds it to the rules.
//
// Every check here rejects rather than repairs. A ticket citing an invented
// criterion, blocking work it does not gate, or naming one layer is a plan
// kriya cannot verify anything against — and silently fixing an agent's reply
// would mean building something nobody proposed.
func (i Intaker) proposeTickets(
	ctx context.Context, target BuildTarget, snap Snapshot,
) ([]Ticket, error) {
	known := knownCriteria(snap)
	if len(known) == 0 {
		return nil, fmt.Errorf("snapshot %s cites no acceptance criteria to decompose", Short(target.SpecHash))
	}

	res, err := i.Agent.Run(ctx, agent.Request{
		Role:   agent.RolePM,
		Prompt: decomposePrompt(snap, known),
		Schema: json.RawMessage(ticketSchema),
	})
	if err != nil {
		return nil, fmt.Errorf("pm decomposition: %w", err)
	}

	var reply ticketReply
	if err := json.Unmarshal(res.Structured, &reply); err != nil {
		return nil, fmt.Errorf("pm decomposition: parse tickets: %w", err)
	}
	for _, check := range []func([]Ticket, map[string]bool) error{
		validateCitations, validateKinds,
	} {
		if err := check(reply.Tickets, known); err != nil {
			return nil, err
		}
	}
	if err := validateSlices(reply.Tickets); err != nil {
		return nil, err
	}
	return reply.Tickets, nil
}

// takeTheHead runs a candidate through the replacement CAS.
//
// A loser is an ERROR to this caller, and the error names where the candidate
// landed. Returning the head plan's tickets instead would be a lie: they
// belong to a different decomposition of a different snapshot, and the caller
// asked what THIS request produced.
func (i Intaker) takeTheHead(ctx context.Context, target BuildTarget, plan Plan, actor string) error {
	if i.Heads == nil || i.Plans == nil {
		return nil
	}
	// The verdict AND the loser's landing come from one transaction, so
	// there is no window in which a candidate has lost and does not yet say
	// so.
	verdict, err := i.Heads.Replace(ctx, plan)
	if err != nil {
		return fmt.Errorf("replace the head of %s: %w", plan.TargetKey, err)
	}
	if !verdict.Won {
		return fmt.Errorf(
			"the decomposition of %s lost the head to plan %s and is now %s",
			plan.TargetKey, Short(verdict.Head.Current), verdict.Landing)
	}
	// RETIREMENT, before the plan activates and before the fence comes down.
	// A successor that activated first would admit pops while its
	// predecessor's tickets were still open — work from a plan nobody is
	// building any more.
	//
	// The verdict's predecessor, or the plan's DURABLE one. An idempotent
	// replacement — a plan re-entering the CAS it already won — moves no head
	// and so names no predecessor, and a retry after partial retirement would
	// then skip the remaining rows entirely and activate over them.
	predecessor := verdict.Predecessor
	if predecessor == "" {
		current, found, err := i.Plans.ByKey(ctx, plan.Key)
		if err != nil {
			return err
		}
		if found {
			predecessor = current.Predecessor
		}
	}
	return i.retire(ctx, target, predecessor, actor)
}

// retire consumes the predecessor's rows, when there is one.
//
// A bootstrap has none, and its retirement set is empty — the same path, not
// a special case that skips the fence.
func (i Intaker) retire(ctx context.Context, target BuildTarget, predecessorKey, actor string) error {
	if i.Retire == nil || predecessorKey == "" {
		return nil
	}
	predecessor, found, err := i.Plans.ByKey(ctx, predecessorKey)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("the head named predecessor %s, which has no row", Short(predecessorKey))
	}
	// Nothing carried: a successor currently creates all of its own tickets.
	// The disposition exists and is stamped only for a selection that
	// genuinely happened.
	if err := i.Retire.Run(ctx, predecessor, target.ProjectID, nil, actor); err != nil {
		return fmt.Errorf("retire plan %s: %w", Short(predecessorKey), err)
	}
	return nil
}

// activate lowers the pop fence the head move raised.
//
// Its own step, after the ticket set is whole. The fence exists precisely to
// refuse pops while a head plan is unactivated, so lowering it any earlier
// would admit work against a plan still being built.
func (i Intaker) activate(ctx context.Context, plan Plan) error {
	if i.Heads == nil {
		return nil
	}
	if err := i.Heads.Activate(ctx, plan.Key); err != nil {
		return err
	}
	return nil
}

// writeSequence persists the payload before any tracker call is made.
//
// Tickets first, then steps. A step naming an ordinal no ticket row backs is
// a step recovery cannot replay, and writing them the other way round leaves
// exactly that after a crash between the two.
func (i Intaker) writeSequence(ctx context.Context, plan Plan, tickets []Ticket, steps []Step) error {
	if i.Tickets != nil {
		for n := range tickets {
			tickets[n].Plan = plan.Key
			tickets[n].Ordinal = n
			if err := i.Tickets.Put(ctx, plan.TargetKey, tickets[n]); err != nil {
				return fmt.Errorf("record planned ticket %d: %w", n, err)
			}
		}
	}
	if i.Steps == nil {
		return nil
	}
	if err := i.Steps.Write(ctx, steps); err != nil {
		return fmt.Errorf("record the sequence of plan %s: %w", Short(plan.Key), err)
	}
	return nil
}

// afterLosingTheClaim re-resolves a key a twin claimed first.
//
// Re-resolution BEFORE any verdict, because finding a row for the SAME key
// means a twin won and this request is a retry of it — never a competitor.
// Treating the lost insert as a loss instead would park or bury a request
// whose work is going ahead perfectly well under another goroutine.
func (i Intaker) afterLosingTheClaim(
	ctx context.Context, target BuildTarget, key, actor string,
) ([]Ticket, error) {
	resolution, err := Resolve(ctx, i.Plans, key)
	if err != nil {
		return nil, err
	}
	if resolution.Fresh {
		// The row was there a moment ago. Gone now means something deleted a
		// plan mid-decomposition, and decomposing again would race the twin
		// that is already running.
		return nil, fmt.Errorf("plan %s was claimed and then vanished", Short(key))
	}
	if resolution.Resume {
		return i.resume(ctx, target, resolution.Plan, actor)
	}
	return i.report(ctx, resolution.Plan)
}

// report answers a request whose plan is already settled.
//
// ONE place, reached by every path that resolves to an existing plan — the
// ordinary same-key retry and the one that lost the claim race to a twin.
// Activation lives here because it is a SEPARATE write from the completed
// stamp: a crash between them leaves a whole plan behind a raised fence, and
// every later retry returning "already complete" without lowering it refuses
// pops forever. Replaying it is idempotent, and SQLHeads.Activate enforces
// for itself that the plan is active and whole — so a parked, superseded or
// mid-phase plan reaching here lowers nothing.
func (i Intaker) report(ctx context.Context, plan Plan) ([]Ticket, error) {
	if err := i.activate(ctx, plan); err != nil {
		return nil, err
	}
	return i.plannedSet(ctx, plan)
}
