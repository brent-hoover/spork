package context

import (
	stdctx "context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"kriya/internal/clock"
)

// Playbook is kriya's process doctrine, handed to every dev agent.
//
// Embedded rather than assembled from the target's spec: it is how KRIYA
// works — the pair loop, the gate chain, who drives roborev — and it is the
// same for every target. AC-context-instructions wants it alongside the
// spec-derived material, not in place of it.
//
//go:embed playbook.md
var Playbook string

// Contract is an interface a module publishes.
type Contract struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Path string `json:"path"`
	// Body is the contract file's content, from the pinned snapshot.
	Body string `json:"body,omitempty"`
}

// Module is one module's law as the snapshot pinned it.
type Module struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	MayImport []string          `json:"may_import,omitempty"`
	Contracts []Contract        `json:"contracts,omitempty"`
	Commands  map[string]string `json:"commands,omitempty"`
}

// Principle is one constitution entry.
type Principle struct {
	ID        string `json:"id"`
	Statement string `json:"statement"`
}

// Spec is the pinned snapshot, in the terms this package needs.
//
// Declared here rather than imported from the planner: MOD-context's declared
// boundary permits only the tracker client, and the composition root is what
// maps a snapshot onto it. The mapping being explicit is the point — it is
// where "what the agent sees" is decided.
type Spec struct {
	Modules      []Module
	Constitution []Principle
	// Artifacts maps a spec-relative path to its pinned content.
	Artifacts map[string]string
}

// Ticket is the work the context is assembled for.
type Ticket struct {
	Title    string
	Criteria []string
	// Modules are the module ids the ticket touches.
	Modules []string
	// Patterns are the failure patterns this ticket is prone to, used to
	// match learnings alongside the modules.
	Patterns []string
	// ProjectKey scopes which project's learnings this ticket may see.
	ProjectKey string
}

// Learning is a lesson earned on an earlier run.
type Learning struct {
	Lesson  string `json:"lesson"`
	Module  string `json:"module"`
	Pattern string `json:"pattern"`
}

// Bundle is exactly what a dev agent was given.
type Bundle struct {
	Build   string
	Content json.RawMessage
	Created time.Time
}

// BundleStore persists bundles.
type BundleStore interface {
	Put(ctx stdctx.Context, b Bundle) error
	Get(ctx stdctx.Context, build string) (Bundle, bool, error)
}

// LearningStore reads the lessons already earned.
type LearningStore interface {
	// Matching returns global learnings and this project's, tagged with any of
	// these modules or patterns. Matching is the store's, not the caller's: a
	// caller that filtered afterwards would have loaded every project's first.
	Matching(ctx stdctx.Context, projectKey string, modules, patterns []string) ([]Learning, error)
}

// content is the assembled bundle, as the agent receives it.
type content struct {
	Ticket       string      `json:"ticket"`
	Criteria     []string    `json:"criteria"`
	Constitution []Principle `json:"constitution"`
	// Touched carries each touched module whole: its boundary, its contracts,
	// and the commands it is judged by.
	Touched []Module `json:"touched_modules"`
	// Reachable carries every OTHER module as its contract alone. The agent
	// sees the interfaces it may call, never internals it may not import.
	Reachable    []Module   `json:"reachable_modules"`
	Instructions string     `json:"instructions,omitempty"`
	Playbook     string     `json:"playbook"`
	Learnings    []Learning `json:"learnings,omitempty"`
	// Tools are the touched modules' resolved gate commands, keyed by module.
	Tools map[string]map[string]string `json:"tools"`
}

// Assembler builds a dev agent's context from a pinned snapshot.
type Assembler struct {
	Store     BundleStore
	Learnings LearningStore
	Now       clock.Clock
}

// Assemble produces the bundle and records it against the run.
//
// Recorded BEFORE it is returned: AC-context-recorded wants a reviewer able to
// reconstruct exactly what the agent knew when it acted, and a bundle written
// after the agent ran would be missing for precisely the runs worth
// reconstructing.
func (a Assembler) Assemble(
	ctx stdctx.Context, build string, spec Spec, ticket Ticket, instructions string,
) (Bundle, error) {
	touched := map[string]bool{}
	for _, id := range ticket.Modules {
		touched[id] = true
	}

	c := content{
		Ticket:       ticket.Title,
		Criteria:     ticket.Criteria,
		Constitution: spec.Constitution,
		Instructions: instructions,
		Playbook:     Playbook,
		Tools:        map[string]map[string]string{},
	}
	for _, module := range spec.Modules {
		if touched[module.ID] {
			whole := module
			whole.Contracts = withBodies(module.Contracts, spec.Artifacts)
			c.Touched = append(c.Touched, whole)
			c.Tools[module.ID] = module.Commands
			continue
		}
		// Contract only. No boundary, no commands, no owned entities: those
		// are internals of a module this ticket may call but not import.
		c.Reachable = append(c.Reachable, Module{
			ID: module.ID, Name: module.Name,
			Contracts: withBodies(module.Contracts, spec.Artifacts),
		})
	}

	if a.Learnings != nil {
		learned, err := a.Learnings.Matching(ctx, ticket.ProjectKey,
			ticket.Modules, ticket.Patterns)
		if err != nil {
			return Bundle{}, fmt.Errorf("read learnings: %w", err)
		}
		c.Learnings = learned
	}

	body, err := json.Marshal(c)
	if err != nil {
		return Bundle{}, fmt.Errorf("encode context: %w", err)
	}
	bundle := Bundle{Build: build, Content: body, Created: a.Now.Now()}
	if err := a.Store.Put(ctx, bundle); err != nil {
		return Bundle{}, fmt.Errorf("record context bundle: %w", err)
	}
	return bundle, nil
}

// withBodies attaches each contract's pinned content.
//
// From the SNAPSHOT, never the working tree: the agent is shown the contract
// the build was admitted against, and a contract edited since would describe
// an interface nothing validated.
func withBodies(contracts []Contract, artifacts map[string]string) []Contract {
	if len(contracts) == 0 {
		return nil
	}
	// Declared order, not sorted: the spec lists a module's contracts in a
	// fixed order and that order is already deterministic, so sorting would
	// add a comparison that can never change the output.
	out := make([]Contract, 0, len(contracts))
	for _, c := range contracts {
		c.Body = artifacts[c.Path]
		out = append(out, c)
	}
	return out
}

// SystemFile writes a bundle where the agent can be pointed at it.
//
// A file rather than prompt text: this is CONTENT — the ticket's law, the
// contracts, the learnings — and content stated as instruction competes with
// the instruction. It lands under the workspace's own .kriya directory so a
// reviewer opening the worktree finds exactly what the agent was given.
func SystemFile(workspace string, b Bundle) (string, error) {
	dir := filepath.Join(workspace, ".kriya")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("make %s: %w", dir, err)
	}
	path := filepath.Join(dir, "context.json")
	if err := os.WriteFile(path, b.Content, 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}

// Learning source kinds, exactly as ENT-learning declares them.
const (
	SourceReviewFinding = "review-finding"
	SourceGateFailure   = "gate-failure"
	SourceSADirection   = "sa-direction"
	SourcePORejection   = "po-rejection"
	SourceOperator      = "operator"
)

// Learning scopes.
const (
	ScopeProject = "project"
	ScopeGlobal  = "global"
)

// Capture is a learning as it is recorded.
//
// Separate from Learning, which is what the feed-forward path reads: what a
// capture must carry to be traceable is more than what a later run needs to
// act on it, and conflating the two would either lose the provenance or push
// it into every context bundle.
type Capture struct {
	Scope string
	// ProjectKey is present exactly when Scope is project.
	ProjectKey string
	Lesson     string
	Module     string
	Pattern    string
	SourceKind string
	// SourceRun is absent exactly when SourceKind is operator.
	SourceRun string
	// SourceCommit is the TRIGGERING head — the commit that was reviewed,
	// gated or validated when the lesson arose. Code-backed corrections only.
	SourceCommit string
	// SourceDoc is the finding document version a research-backed correction
	// arose from. Exactly one of SourceCommit and SourceDoc is present on a
	// captured learning, matching the run kind.
	SourceDoc string
	// SourceRef is the finding, gate result or event that produced it.
	SourceRef string
	// SourceSession is the AGENT SESSION the correction happened in, not the
	// run: a run has many sessions, and the transcript is findable only by the
	// session's own id.
	SourceSession string
}

// Recorder persists captured learnings.
type Recorder interface {
	Record(ctx stdctx.Context, c Capture) error
}

// Validate checks what the format cannot express.
//
// ENT-learning's conditional requirements are enforced at WRITE time because
// the schema has no way to say "present exactly when": a project learning
// without its key, or a captured one without its run, would be unmatched or
// untraceable forever after, and the write is the last moment anything knows.
func (c Capture) Validate() error {
	if err := c.validateScope(); err != nil {
		return err
	}
	if c.Lesson == "" || c.Module == "" || c.Pattern == "" {
		return fmt.Errorf("a learning needs a lesson, a module and a pattern")
	}
	return c.validateProvenance()
}

// validateScope checks project_key is present exactly when scope is project.
func (c Capture) validateScope() error {
	switch c.Scope {
	case ScopeProject:
		if c.ProjectKey == "" {
			return fmt.Errorf("a project learning needs its project key")
		}
	case ScopeGlobal:
		if c.ProjectKey != "" {
			return fmt.Errorf("a global learning must carry no project key")
		}
	default:
		return fmt.Errorf("unknown learning scope %q", c.Scope)
	}
	return nil
}

// validateProvenance checks the reference fields the source kind implies.
func (c Capture) validateProvenance() error {
	switch c.SourceKind {
	case SourceOperator:
		// The operator IS the origin. A run or an anchor would claim a
		// provenance the entry does not have.
		// EVERY captured-origin reference, not only the anchors: an operator
		// entry pointing at a finding or a session claims a provenance it
		// does not have, and a reader following it finds someone else's.
		if c.SourceRun != "" || c.SourceCommit != "" || c.SourceDoc != "" ||
			c.SourceRef != "" || c.SourceSession != "" {
			return fmt.Errorf("an operator learning carries no run, anchor, finding or session")
		}
		return nil
	case SourceReviewFinding, SourceGateFailure, SourceSADirection, SourcePORejection:
	default:
		return fmt.Errorf("unknown learning source %q", c.SourceKind)
	}
	if c.SourceRun == "" || c.SourceRef == "" {
		return fmt.Errorf("a captured learning needs its run and the finding that produced it")
	}
	// Exactly one anchor, matching the run kind: a code correction points at
	// the triggering commit, a research one at the finding document version.
	if (c.SourceCommit == "") == (c.SourceDoc == "") {
		return fmt.Errorf(
			"a captured learning needs exactly one of a triggering commit or a finding document")
	}
	return nil
}
