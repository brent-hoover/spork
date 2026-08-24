package planner

import (
	"context"
	"encoding/json"
	"fmt"

	"kriya/internal/agent"
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
        "required": ["title", "body", "criteria"],
        "properties": {
          "title": {"type": "string", "minLength": 1},
          "body": {"type": "string"},
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
	// Criteria are the AC ids this ticket satisfies. They are validated
	// against the snapshot, not trusted.
	Criteria []string `json:"criteria"`
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
func (i Intaker) Decompose(ctx context.Context, target BuildTarget, snap Snapshot, actor string) ([]Ticket, error) {
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
	if err := validateCitations(reply.Tickets, known); err != nil {
		return nil, err
	}

	for n, ticket := range reply.Tickets {
		issueID, err := i.Tracker.CreateIssue(ctx, target.ProjectID, ticket.Title, ticket.Body, actor,
			idempotencyKey(fmt.Sprintf("ticket-%d", n), target.TargetKey, target.SpecHash))
		if err != nil {
			return nil, fmt.Errorf("create ticket %d: %w", n, err)
		}
		reply.Tickets[n].IssueID = issueID
		// Recorded HERE, because this is the only moment the ticket's criteria
		// and the issue they became are both in hand. A pop later returns an
		// id and a title, and a run built from those alone reaches the product
		// owner with nothing to validate against.
		if i.Tickets != nil {
			if err := i.Tickets.Put(ctx, target.TargetKey, reply.Tickets[n]); err != nil {
				return nil, fmt.Errorf("record ticket %d: %w", n, err)
			}
		}
		if err := i.Tracker.AddRelation(ctx, target.EpicID, "parent_of", issueID, actor,
			idempotencyKey(fmt.Sprintf("parent-%d", n), target.TargetKey, target.SpecHash)); err != nil {
			return nil, fmt.Errorf("parent ticket %d under the epic: %w", n, err)
		}
	}
	return reply.Tickets, nil
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
