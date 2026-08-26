package planner

import (
	"context"
	"fmt"
)

// Binding is the plan and row a popped ticket belongs to.
//
// The PLAN matters as much as the row: it names the snapshot the work is
// built against, and therefore which acceptance criteria the product owner
// validates. A ticket bound to the wrong plan is one built against a spec
// that never described it.
type Binding struct {
	Plan   Plan
	Ticket Ticket
	// WalkedPast is every row nearer the head than the one that bound, in
	// the order they were walked. Each is stamped consumed by the binding.
	WalkedPast []Ticket
}

// Binder resolves which plan owns a popped ticket.
type Binder struct {
	Plans   PlanStore
	Heads   HeadStore
	Tickets TicketStore
}

// chainLink is one plan on the supersession chain and its row for the ticket.
type chainLink struct {
	plan Plan
	// row is the plan's ticket for this issue, present only when it has one.
	row   Ticket
	found bool
	// depth counts hops from the head: the head is zero, its predecessor one.
	depth int
}

// Bind applies the state-aware precedence algorithm.
//
// The rules are ORDERED, and the order is what makes them a decision rather
// than four opinions. Each is tried in turn:
//
//  1. The DEEPEST unstamped superseded row. Mid-retirement the oldest
//     unsettled row is the one whose decomposition still owns the work — a
//     nearer row may already have been consumed, and binding to it would
//     claim a plan that has finished with the ticket.
//  2. The NEAREST predecessor row while the head is unactivated. An
//     unactivated head never binds: its ticket set is still growing, and its
//     predecessor has not retired, so the work belongs to whoever last owned
//     it.
//  3. The activated head's own row. The ordinary case.
//  4. The NEAREST consumed row's plan, for a ticket living only in consumed
//     history — the decomposition that produced its acceptance criteria,
//     never a head that omitted it.
//
// Every row nearer the head than the one that binds is stamped consumed, so a
// later pop does not walk it again.
func (b Binder) Bind(ctx context.Context, targetKey, issue string) (Binding, error) {
	chain, head, err := b.walk(ctx, targetKey, issue)
	if err != nil {
		return Binding{}, err
	}
	if len(chain) == 0 {
		return Binding{}, fmt.Errorf(
			"issue %s belongs to no plan of %s", issue, targetKey)
	}

	chosen, err := choose(chain, head)
	if err != nil {
		return Binding{}, err
	}
	binding := Binding{Plan: chosen.plan, Ticket: chosen.row}
	for _, link := range chain {
		if link.depth < chosen.depth && link.found && !link.row.Consumed {
			binding.WalkedPast = append(binding.WalkedPast, link.row)
		}
	}
	if err := b.stamp(ctx, binding.WalkedPast); err != nil {
		return Binding{}, err
	}
	return binding, nil
}

// choose applies the four rules in order.
func choose(chain []chainLink, head PlanHead) (chainLink, error) {
	// 1. The deepest unstamped superseded row.
	if deepest, ok := deepestUnstampedSuperseded(chain); ok {
		return deepest, nil
	}

	// An unactivated head never binds: its ticket set is still growing and
	// its predecessor has not retired, so the work belongs to whoever last
	// owned it.
	eligible := chain
	if head.Fence > 0 {
		eligible = withoutHead(chain)
	}

	// 2. The nearest UNSTAMPED row. The chain is walked head-first, so the
	//    first match is nearest.
	for _, link := range eligible {
		if !link.row.Consumed {
			return link, nil
		}
	}
	// 3 and 4 collapse: the nearest row that exists at all. For an activated
	//    head that lists the ticket that is the head's own row; otherwise it
	//    is the nearest consumed row's plan — the decomposition that produced
	//    the ticket's acceptance criteria, never a head that omitted it.
	if len(eligible) > 0 {
		return eligible[0], nil
	}
	return chainLink{}, fmt.Errorf(
		"every plan holding the ticket is an unactivated head, which never binds")
}

// deepestUnstampedSuperseded finds the oldest row retirement has not settled.
//
// DEEPEST rather than nearest: mid-retirement the oldest unsettled row is the
// one whose decomposition still owns the work — a nearer row may already have
// been consumed, and binding to it would claim a plan that has finished with
// the ticket.
func deepestUnstampedSuperseded(chain []chainLink) (chainLink, bool) {
	var deepest chainLink
	found := false
	for _, link := range chain {
		if link.row.Consumed || link.plan.State != PlanSuperseded {
			continue
		}
		if !found || link.depth > deepest.depth {
			deepest, found = link, true
		}
	}
	return deepest, found
}

// withoutHead drops the head's own link from the chain.
func withoutHead(chain []chainLink) []chainLink {
	out := make([]chainLink, 0, len(chain))
	for _, link := range chain {
		if link.depth > 0 {
			out = append(out, link)
		}
	}
	return out
}

// walk collects the supersession chain from the head down, head-first.
func (b Binder) walk(
	ctx context.Context, targetKey, issue string,
) ([]chainLink, PlanHead, error) {
	head, found, err := b.Heads.Head(ctx, targetKey)
	if err != nil {
		return nil, PlanHead{}, err
	}
	if !found {
		return nil, PlanHead{}, fmt.Errorf("%s has no plan head to bind against", targetKey)
	}

	var chain []chainLink
	key := head.Current
	// Bounded by the chain's own length. A predecessor pointer that cycled
	// would otherwise walk forever, and a corrupt chain must fail rather than
	// hang.
	seen := map[string]bool{}
	for depth := 0; key != "" && !seen[key]; depth++ {
		seen[key] = true
		plan, ok, err := b.Plans.ByKey(ctx, key)
		if err != nil {
			return nil, PlanHead{}, err
		}
		if !ok {
			return nil, PlanHead{}, fmt.Errorf("the chain names plan %s, which has no row", Short(key))
		}
		row, has, err := b.rowFor(ctx, plan, issue)
		if err != nil {
			return nil, PlanHead{}, err
		}
		chain = append(chain, chainLink{plan: plan, row: row, found: has, depth: depth})
		key = plan.Predecessor
	}
	return withRows(chain), head, nil
}

// withRows drops links that hold no row for the ticket.
//
// Kept as a separate step so the DEPTH of the rows that remain still counts
// hops along the real chain: a plan that omitted the ticket must not make its
// predecessor look nearer the head than it is.
func withRows(chain []chainLink) []chainLink {
	out := make([]chainLink, 0, len(chain))
	for _, link := range chain {
		if link.found {
			out = append(out, link)
		}
	}
	return out
}

// rowFor finds a plan's row for one issue.
func (b Binder) rowFor(ctx context.Context, plan Plan, issue string) (Ticket, bool, error) {
	rows, err := b.Tickets.ForPlan(ctx, plan.Key)
	if err != nil {
		return Ticket{}, false, err
	}
	for _, row := range rows {
		if row.IssueID == issue {
			return row, true, nil
		}
	}
	return Ticket{}, false, nil
}

// stamp consumes every row the binding walked past.
//
// Neutrally: the disposition is left empty, because a binding makes no
// judgement about the WORK — only about which plan owns it. Retirement is what
// decides whether a row was retired, carried forward, bound or completed.
func (b Binder) stamp(ctx context.Context, walked []Ticket) error {
	for _, row := range walked {
		if err := b.Tickets.Consume(ctx, row.Plan, row.Ordinal, ""); err != nil {
			return fmt.Errorf("stamp walked-past row %d of plan %s: %w",
				row.Ordinal, Short(row.Plan), err)
		}
	}
	return nil
}
