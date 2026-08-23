package context_test

import (
	stdctx "context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	// Imported under its OWN name, with the standard library aliased instead.
	// gobco builds the black-box coverage bridge by scanning these files for an
	// import whose NAME matches the package under test; an aliased subject
	// leaves it with an empty import path and instrumentation aborts.
	"kriya/internal/context"
	"kriya/internal/fakes"
)

type memBundles struct{ rows map[string]context.Bundle }

func newMemBundles() *memBundles { return &memBundles{rows: map[string]context.Bundle{}} }

func (m *memBundles) Put(_ stdctx.Context, b context.Bundle) error {
	m.rows[b.Build] = b
	return nil
}

func (m *memBundles) Get(_ stdctx.Context, build string) (context.Bundle, bool, error) {
	b, ok := m.rows[build]
	return b, ok, nil
}

type memLearnings struct {
	rows  []context.Learning
	asked [][]string
}

func (m *memLearnings) Matching(_ stdctx.Context, modules, patterns []string) ([]context.Learning, error) {
	m.asked = append(m.asked, modules, patterns)
	var out []context.Learning
	for _, l := range m.rows {
		if contains(modules, l.Module) || contains(patterns, l.Pattern) {
			out = append(out, l)
		}
	}
	return out, nil
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func spec() context.Spec {
	return context.Spec{
		Constitution: []context.Principle{{ID: "CON-tests", Statement: "Tests come first."}},
		Modules: []context.Module{
			{
				ID: "MOD-api", Name: "api", MayImport: []string{"MOD-store"},
				Commands:  map[string]string{"test": "go test ./api/...", "lint": "lint-api"},
				Contracts: []context.Contract{{ID: "CTR-api", Type: "openapi", Path: "contracts/api.yaml"}},
			},
			{
				ID: "MOD-store", Name: "store",
				Commands: map[string]string{"test": "go test ./store/..."},
			},
			{
				ID: "MOD-billing", Name: "billing", MayImport: []string{"MOD-ledger"},
				Commands:  map[string]string{"test": "go test ./billing/...", "mutation": "gremlins"},
				Contracts: []context.Contract{{ID: "CTR-billing", Type: "openapi", Path: "contracts/billing.yaml"}},
			},
		},
		Artifacts: map[string]string{
			"avspec.yaml":            "the whole manifest, which must not be dumped",
			"contracts/api.yaml":     "openapi: 3.1.0  # api",
			"contracts/billing.yaml": "openapi: 3.1.0  # billing",
		},
	}
}

func ticket() context.Ticket {
	return context.Ticket{
		Title:    "Create a short link",
		Criteria: []string{"AC-valid-url"},
		Modules:  []string{"MOD-api", "MOD-store"},
		Patterns: []string{"off-by-one"},
	}
}

func assemble(t *testing.T, learnings context.LearningStore, instructions string) (map[string]any, *memBundles) {
	t.Helper()
	store := newMemBundles()
	a := context.Assembler{Store: store, Learnings: learnings, Now: fakes.NewClock(time.Unix(0, 0))}
	bundle, err := a.Assemble(stdctx.Background(), "run-1", spec(), ticket(), instructions)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	var content map[string]any
	if err := json.Unmarshal(bundle.Content, &content); err != nil {
		t.Fatalf("decode content: %v", err)
	}
	return content, store
}

func TestTheTicketsCriteriaAndItsModulesLawAreIncluded(t *testing.T) {
	content, _ := assemble(t, nil, "")
	if got := content["criteria"]; !strings.Contains(toJSON(t, got), "AC-valid-url") {
		t.Errorf("criteria are %v", got)
	}
	if got := toJSON(t, content["constitution"]); !strings.Contains(got, "Tests come first") {
		t.Errorf("constitution is %s", got)
	}
	touched := toJSON(t, content["touched_modules"])
	for _, want := range []string{"MOD-api", "MOD-store", "MOD-store", "CTR-api", "openapi: 3.1.0  # api"} {
		if !strings.Contains(touched, want) {
			t.Errorf("touched modules omit %q: %s", want, touched)
		}
	}
}

func TestItIsNotADumpOfTheWholeSpec(t *testing.T) {
	// A manifest pasted whole is not context; it is the thing context exists
	// to replace.
	content, _ := assemble(t, nil, "")
	whole := toJSON(t, content)
	if strings.Contains(whole, "the whole manifest") {
		t.Error("the manifest was dumped into the context")
	}
}

func TestAnUntouchedModuleAppearsOnlyAsItsContract(t *testing.T) {
	content, _ := assemble(t, nil, "")
	reachable := toJSON(t, content["reachable_modules"])
	if !strings.Contains(reachable, "openapi: 3.1.0  # billing") {
		t.Errorf("billing's contract is missing: %s", reachable)
	}
	for _, internal := range []string{"MOD-ledger", "gremlins", "go test ./billing/..."} {
		if strings.Contains(toJSON(t, content), internal) {
			t.Errorf("billing's internals leaked: %s", internal)
		}
	}
}

func TestTheToolsetIsTheTicketsModules(t *testing.T) {
	content, _ := assemble(t, nil, "")
	tools := toJSON(t, content["tools"])
	if !strings.Contains(tools, "go test ./api/...") || !strings.Contains(tools, "lint-api") {
		t.Errorf("the touched modules' commands are missing: %s", tools)
	}
	if strings.Contains(tools, "billing") {
		t.Errorf("a tool irrelevant to the ticket is wired in: %s", tools)
	}
}

func TestTheOperatorsInstructionsRideAlongVerbatim(t *testing.T) {
	instructions := "# House rules\n\nNever use a bare except.\n"
	content, _ := assemble(t, nil, instructions)
	if content["instructions"] != instructions {
		t.Errorf("instructions came through as %q", content["instructions"])
	}
	playbook, _ := content["playbook"].(string)
	for _, want := range []string{"pair-programming loop", "roborev", "Conventional commit"} {
		if !strings.Contains(playbook, want) {
			t.Errorf("the playbook does not cover %q", want)
		}
	}
}

func TestOnlyRelevantLearningsAreInjected(t *testing.T) {
	learnings := &memLearnings{rows: []context.Learning{
		{Lesson: "api pagination is one-based", Module: "MOD-api", Pattern: "pagination"},
		{Lesson: "loops here run to len, not len-1", Module: "MOD-store", Pattern: "off-by-one"},
		{Lesson: "billing rounds half-even", Module: "MOD-billing", Pattern: "rounding"},
	}}
	content, _ := assemble(t, learnings, "")
	got := toJSON(t, content["learnings"])
	for _, want := range []string{"api pagination is one-based", "loops here run to len"} {
		if !strings.Contains(got, want) {
			t.Errorf("a matching learning is missing: %q in %s", want, got)
		}
	}
	if strings.Contains(got, "billing rounds half-even") {
		t.Errorf("an unrelated module's learning was injected: %s", got)
	}
}

func TestMatchingHappensInTheStoreNotAfterwards(t *testing.T) {
	// A caller that filtered afterwards would have loaded every learning ever
	// recorded first.
	learnings := &memLearnings{}
	if _, _ = assemble(t, learnings, ""); len(learnings.asked) != 2 {
		t.Fatalf("the store was asked %v", learnings.asked)
	}
	if !contains(learnings.asked[0], "MOD-api") || !contains(learnings.asked[1], "off-by-one") {
		t.Errorf("the store was asked for %v", learnings.asked)
	}
}

func TestTheBundleIsRecordedAgainstTheRun(t *testing.T) {
	// A reviewer must be able to reconstruct exactly what the agent knew when
	// it acted.
	content, store := assemble(t, nil, "")
	saved, found, err := store.Get(stdctx.Background(), "run-1")
	if err != nil || !found {
		t.Fatalf("get: %v found=%v", err, found)
	}
	var reconstructed map[string]any
	if err := json.Unmarshal(saved.Content, &reconstructed); err != nil {
		t.Fatalf("decode saved: %v", err)
	}
	if toJSON(t, reconstructed) != toJSON(t, content) {
		t.Error("the recorded bundle differs from what the agent was given")
	}
}

func TestTheSystemFileLandsUnderTheWorkspace(t *testing.T) {
	work := t.TempDir()
	content, _ := assemble(t, nil, "")
	path, err := context.SystemFile(work, context.Bundle{
		Build: "run-1", Content: json.RawMessage(toJSON(t, content)),
	})
	if err != nil {
		t.Fatalf("system file: %v", err)
	}
	if filepath.Dir(path) != filepath.Join(work, ".kriya") {
		t.Errorf("written to %s", path)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(string(body), "AC-valid-url") {
		t.Error("the file does not hold the assembled context")
	}
}

func TestAnUnwritableWorkspaceFails(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := context.SystemFile(file, context.Bundle{}); err == nil {
		t.Fatal("writing into a file as though it were a workspace succeeded")
	}
}

func toJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
