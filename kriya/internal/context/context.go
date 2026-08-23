package context

import (
	stdctx "context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	// Matching returns learnings tagged with any of these modules or
	// patterns. Matching is the store's, not the caller's: a caller that
	// filtered afterwards would have loaded everything first.
	Matching(ctx stdctx.Context, modules, patterns []string) ([]Learning, error)
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
		learned, err := a.Learnings.Matching(ctx, ticket.Modules, ticket.Patterns)
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
	out := make([]Contract, 0, len(contracts))
	for _, c := range contracts {
		c.Body = artifacts[c.Path]
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
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
