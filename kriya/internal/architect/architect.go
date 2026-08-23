package architect

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"kriya/internal/agent"
	"kriya/internal/clock"
)

// Triggers that bring an impasse to the architect.
const (
	TriggerRoundLimit    = "round-limit"
	TriggerAgentDeclared = "agent-declared"
)

// An intervention's lifecycle.
//
// Recovery and resume read this row, never transient state — which is the
// whole reason the lifecycle is persisted rather than inferred from how many
// rounds happen to have run.
const (
	StateOpen      = "open"
	StateDirected  = "directed"
	StateResumed   = "resumed"
	StateEscalated = "escalated"
)

// Intervention is one run's record of an impasse and its resolution.
type Intervention struct {
	Build   string
	Trigger string
	// Findings is the history handed to the SA, verbatim.
	Findings json.RawMessage
	// Direction is the SA's answer, put into the dev agent's context on resume.
	Direction string
	State     string
	// Outcome records how the resumed run fared.
	Outcome string
}

// Store persists interventions.
type Store interface {
	Upsert(ctx context.Context, i Intervention) error
	Find(ctx context.Context, build string) (Intervention, bool, error)
}

// Architect resolves impasses. It directs; it never writes code.
type Architect struct {
	Agent agent.Agent
	Store Store
	Now   clock.Clock
}

// allowRules is the SA's toolset.
//
// Read-only, deliberately: AC-sa-directs requires that the worktree contents
// and branch head are identical before and after an SA invocation, and the
// cheapest way to guarantee that is to hand it nothing that could change them.
// No Write, no Edit, no Bash.
var allowRules = []string{"Read", "Grep", "Glob"}

// AllowRules returns the SA's permitted tools.
func AllowRules() []string { return append([]string(nil), allowRules...) }

// Resolve records the impasse, asks the architect, and records its direction.
//
// The row is written BEFORE the agent is invoked, so an impasse that crashed
// mid-resolution is one a resumed run can still see — an impasse that existed
// only in memory would let the loop restart straight into the same wall.
func (a Architect) Resolve(
	ctx context.Context, build, ticket, trigger string, findings []string, systemFile string,
) (Intervention, error) {
	history, err := json.Marshal(findings)
	if err != nil {
		return Intervention{}, fmt.Errorf("encode findings history: %w", err)
	}
	in := Intervention{
		Build: build, Trigger: trigger, Findings: history, State: StateOpen,
	}
	if err := a.Store.Upsert(ctx, in); err != nil {
		return Intervention{}, fmt.Errorf("record impasse: %w", err)
	}

	res, err := a.Agent.Run(ctx, agent.Request{
		Role:       agent.RoleSA,
		Prompt:     prompt(ticket, trigger, findings),
		SystemFile: systemFile,
		AllowRules: AllowRules(),
	})
	if err != nil {
		return Intervention{}, fmt.Errorf("architect on %s: %w", ticket, err)
	}
	in.Direction = res.Text
	in.State = StateDirected
	if err := a.Store.Upsert(ctx, in); err != nil {
		return Intervention{}, fmt.Errorf("record direction: %w", err)
	}
	return in, nil
}

// Resume stamps an intervention as acted on and returns its direction.
//
// Read from the row rather than carried in memory: a run that restarted
// between the direction and the resume must still get it.
func (a Architect) Resume(ctx context.Context, build string) (Intervention, bool, error) {
	in, found, err := a.Store.Find(ctx, build)
	if err != nil {
		return Intervention{}, false, fmt.Errorf("find intervention: %w", err)
	}
	if !found || in.State != StateDirected {
		return Intervention{}, false, nil
	}
	in.State = StateResumed
	if err := a.Store.Upsert(ctx, in); err != nil {
		return Intervention{}, false, fmt.Errorf("record resumed intervention: %w", err)
	}
	return in, true, nil
}

// prompt states the impasse.
//
// The findings go over VERBATIM and in order. The architect's job is to see
// what dev and review could not agree on, and a summary is precisely the thing
// that hides it.
func prompt(ticket, trigger string, findings []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The pair loop on %q cannot converge (%s).\n\n", ticket, trigger)
	b.WriteString("Below is every finding since the last clean pass, in order.\n")
	b.WriteString("Give the dev agent a direction. Do NOT write code.\n")
	b.WriteString("If resolving this would change the ticket's acceptance criteria\n")
	b.WriteString("or the spec, say so and name the change instead of deciding it.\n\n")
	for i, finding := range findings {
		fmt.Fprintf(&b, "--- round %d ---\n%s\n\n", i+1, finding)
	}
	return b.String()
}
