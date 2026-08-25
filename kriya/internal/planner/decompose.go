package planner

import (
	"context"
	"encoding/json"
	"fmt"

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
	key := DecompositionKey(target.ProjectKey, target.TargetKey, target.SpecHash, generation)

	// FIRST — before the agent, before the tracker, before anything. A
	// request that decomposed before resolving its own key would compete
	// with the plan it is retrying: two decompositions for one request, one
	// superseding the other. The key is unique, so resolving here makes a
	// retry find its own plan and leaves competition possible only between
	// DIFFERENT keys.
	//
	// Ahead of the AGENT specifically, not merely ahead of the tracker. The
	// PM call is the expensive half of a decomposition, and asking it again
	// to then discard its answer is a bill for nothing — and a second,
	// possibly different, plan for a key that already has one.
	if i.Plans != nil {
		resolution, err := Resolve(ctx, i.Plans, key)
		if err != nil {
			return nil, err
		}
		if !resolution.Fresh && !resolution.Resume {
			return i.plannedSet(ctx, resolution.Plan)
		}
	}

	tickets, err := i.proposeTickets(ctx, target, snap)
	if err != nil {
		return nil, err
	}

	// Write-ahead, before the first ticket exists. A crash mid-decomposition
	// must leave a row saying a plan was being built: completion detection
	// reads this stamp, and no row at all is indistinguishable from a target
	// nobody has planned.
	plan := Plan{
		Key: key, TargetKey: target.TargetKey, SpecHash: target.SpecHash,
		Generation: generation, State: PlanPending,
	}
	if err := i.recordPlan(ctx, plan); err != nil {
		return nil, err
	}

	// ACTIVE before the phases, not after. The spec is explicit that
	// activation precedes creation, wiring and assignment — which is what
	// makes "active without completed" a real state that a crash can leave
	// behind, and what a resumed retry recognises.
	plan.State = PlanActive
	if err := i.recordPlan(ctx, plan); err != nil {
		return nil, err
	}
	if err := i.filePlan(ctx, target, tickets, actor); err != nil {
		return nil, err
	}
	// The final assignment barrier is passed: every ticket exists, is
	// recorded, is parented under the epic, is wired and is assigned. Only
	// now is the ticket set whole, and only now may completion detection arm.
	plan.Completed, plan.Tickets = true, len(tickets)
	if err := i.recordPlan(ctx, plan); err != nil {
		return nil, err
	}
	return tickets, nil
}

// filePlan creates every ticket, wires the plan, then assigns it in phases.
//
// The ORDER between the phases is the point. Relations come before assignment,
// because assignment is what makes a ticket poppable and one popped before its
// blocking relation exists is one started ahead of the risk it depends on. And
// spikes are assigned before implementation work, because sutra offers work
// FIFO and no tracker-side priority is assumed — the order IS the guarantee.
func (i Intaker) filePlan(
	ctx context.Context, target BuildTarget, tickets []Ticket, actor string,
) error {
	for n := range tickets {
		if err := i.createTicket(ctx, target, tickets, n, actor); err != nil {
			return err
		}
	}
	if err := i.wireBlocks(ctx, target, tickets, actor); err != nil {
		return err
	}
	if err := i.assign(ctx, target, tickets, KindSpike, actor); err != nil {
		return err
	}
	return i.assign(ctx, target, tickets, KindImplementation, actor)
}

// wireBlocks relates each spike to the tickets waiting on its answer.
//
// By CRITERION, never by a ticket reference the agent supplied: kriya already
// validates ids against the snapshot, and trusting an agent about which of its
// own tickets to block is trusting it about something it can get wrong.
func (i Intaker) wireBlocks(
	ctx context.Context, target BuildTarget, tickets []Ticket, actor string,
) error {
	for n, spike := range tickets {
		if spike.Kind != KindSpike {
			continue
		}
		blocked := make(map[string]bool, len(spike.Blocks))
		for _, id := range spike.Blocks {
			blocked[id] = true
		}
		for m, other := range tickets {
			if n == m || !dependsOn(other, blocked) {
				continue
			}
			// A spike's own criterion appears in its blocks list — the risk's
			// answer is what it produces — and a self-block is a ticket
			// nothing can ever pop. The n == m guard above is what prevents
			// it; another spike sharing the criterion is genuinely blocked.
			if err := i.Tracker.AddRelation(ctx, spike.IssueID, "blocks", other.IssueID, actor,
				idempotencyKey(fmt.Sprintf("blocks-%d-%d", n, m),
					target.TargetKey, target.SpecHash)); err != nil {
				return fmt.Errorf("block ticket %d behind spike %d: %w", m, n, err)
			}
		}
	}
	return nil
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

// assign puts one kind of ticket on the popping identity's work stack.
//
// A PHASE per kind, because the ordering guarantee is what makes risk-first
// real: sutra offers work FIFO, so a spike assigned after an implementation
// ticket pops after it.
func (i Intaker) assign(
	ctx context.Context, target BuildTarget, tickets []Ticket, kind, actor string,
) error {
	for _, n := range assignmentOrder(tickets, kind) {
		ticket := tickets[n]
		// Assigned to the actor that will pop it. The tracker's work stack
		// offers only issues assigned to the popping identity, so an
		// unassigned ticket is one nothing ever claims — the build would
		// decompose, report its tickets, and idle forever.
		if err := i.Tracker.AssignIssue(ctx, ticket.IssueID, actor, actor,
			idempotencyKey(fmt.Sprintf("assign-%d", n),
				target.TargetKey, target.SpecHash)); err != nil {
			return fmt.Errorf("assign ticket %d: %w", n, err)
		}
	}
	return nil
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

// createTicket creates one ticket, records it and parents it.
//
// Assignment is a LATER phase: the whole plan must exist and be wired before
// anything is assigned, because assignment is what makes a ticket poppable and
// a ticket popped before its blocking relation exists is one started ahead of
// the risk it depends on.
func (i Intaker) createTicket(
	ctx context.Context, target BuildTarget, tickets []Ticket, n int, actor string,
) error {
	ticket := tickets[n]
	issueID, err := i.Tracker.CreateIssue(ctx, target.ProjectID, ticket.Title, ticket.Body, actor,
		idempotencyKey(fmt.Sprintf("ticket-%d", n), target.TargetKey, target.SpecHash))
	if err != nil {
		return fmt.Errorf("create ticket %d: %w", n, err)
	}
	tickets[n].IssueID = issueID
	// Recorded HERE, because this is the only moment the ticket's criteria and
	// the issue they became are both in hand. A pop later returns an id and a
	// title, and a run built from those alone reaches the product owner with
	// nothing to validate against.
	if i.Tickets != nil {
		if err := i.Tickets.Put(ctx, target.TargetKey, tickets[n]); err != nil {
			return fmt.Errorf("record ticket %d: %w", n, err)
		}
	}
	if err := i.Tracker.AddRelation(ctx, target.EpicID, "parent_of", issueID, actor,
		idempotencyKey(fmt.Sprintf("parent-%d", n), target.TargetKey, target.SpecHash)); err != nil {
		return fmt.Errorf("parent ticket %d under the epic: %w", n, err)
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
		named := distinct(t.Layers)
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
	for l := range layers {
		if !contains(tickets[skeleton].Layers, l) {
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

// distinct returns the non-blank values of a list, without repeats.
func distinct(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, v := range values {
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
	tickets, err := i.Tickets.ForTarget(ctx, plan.TargetKey)
	if err != nil {
		return nil, fmt.Errorf("read the planned tickets of %s: %w", plan.TargetKey, err)
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
		return nil, fmt.Errorf("snapshot %s cites no acceptance criteria to decompose", target.SpecHash[:12])
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
