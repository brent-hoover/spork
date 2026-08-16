package api

import (
	"bufio"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestEveryArchiveGuardHasAScenario ties each place that enforces
// AC-project-archive's read-only rule to the scenario that exercises it.
//
// The AC names a SET — "archived projects are read-only" — and the way a
// set claim rots is that someone adds a member without adding a scenario.
// That is defect species 1, and it is why the suite spent the whole build
// with one of twenty-three doors covered and nothing said so.
//
// Counting the call sites was the first attempt and review 2012 was right
// to reject it: a count says the number is unchanged, not that anything is
// tested. So each guardWritable call carries a `// door:` comment naming
// the door or doors it serves, and this test requires a three-way match:
// every guard site is annotated, every door it names appears in the
// feature table, and every door in the feature table is named by at least
// one guard site. The acceptance step closes the loop from the other end,
// failing if the feature table and its step registry disagree.
//
// The relation is many-to-many by nature — one site can serve three doors
// (guardReviewProject covers verdict, consume, and resubmit) and one door
// can be enforced at two sites (createReview checks twice) — so this
// asserts coverage in both directions rather than a bijection.
const doorTable = "../../verification/REQ-projects.feature"

func TestEveryArchiveGuardHasAScenario(t *testing.T) {
	guarded, sites := doorsNamedByGuards(t)
	scenario := doorsNamedByFeature(t)

	for door := range guarded {
		if !scenario[door] {
			t.Errorf("guard site names door %q, which no row of the feature table exercises.\n"+
				"Add the row to %s and the entry to acceptance/archive_steps_test.go.",
				door, doorTable)
		}
	}
	for door := range scenario {
		if !guarded[door] {
			t.Errorf("the feature table exercises door %q, which no guardWritable call claims.\n"+
				"Either the guard is missing, or its `// door:` comment is.", door)
		}
	}
	if len(sites) == 0 {
		t.Fatal("found no guardWritable call sites at all; this test is not looking where it thinks")
	}
}

// doorsNamedByGuards walks the package's own source for guardWritable
// calls and reads the `// door:` comment immediately above each. A site
// without one fails: an unannotated guard is a guard nothing has been
// shown to exercise.
func doorsNamedByGuards(t *testing.T) (map[string]bool, []string) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	doors := map[string]bool{}
	var sites []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		// Comment groups by the line they end on, so a call can find the
		// annotation directly above it.
		above := map[int]string{}
		for _, group := range file.Comments {
			above[fset.Position(group.End()).Line] = group.Text()
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			id, ok := call.Fun.(*ast.Ident)
			if !ok || id.Name != "guardWritable" {
				return true
			}
			pos := fset.Position(call.Pos())
			sites = append(sites, pos.String())
			text, ok := above[pos.Line-1]
			if !ok {
				t.Errorf("%s: guardWritable call has no `// door:` comment above it; "+
					"name the door it enforces so the archived-project scenario can be checked against it", pos)
				return true
			}
			named := false
			for _, line := range strings.Split(text, "\n") {
				rest, found := strings.CutPrefix(strings.TrimSpace(line), "door:")
				if !found {
					continue
				}
				for _, d := range strings.Split(rest, ",") {
					if d = strings.TrimSpace(d); d != "" {
						doors[d] = true
						named = true
					}
				}
			}
			if !named {
				t.Errorf("%s: the comment above guardWritable names no door", pos)
			}
			return true
		})
	}
	sort.Strings(sites)
	return doors, sites
}

// doorsNamedByFeature reads the door column out of the archived-project
// scenario's table. It reads the feature file rather than a Go copy of it
// so that the thing being compared is the specification itself.
func doorsNamedByFeature(t *testing.T) map[string]bool {
	t.Helper()
	f, err := os.Open(doorTable)
	if err != nil {
		t.Fatalf("open %s: %v", doorTable, err)
	}
	defer func() { _ = f.Close() }()
	doors := map[string]bool{}
	inTable := false
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case strings.HasPrefix(line, "| door"):
			inTable = true
		case inTable && strings.HasPrefix(line, "|"):
			doors[strings.TrimSpace(strings.Trim(line, "|"))] = true
		case inTable:
			inTable = false
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read %s: %v", doorTable, err)
	}
	if len(doors) == 0 {
		t.Fatalf("%s holds no door table; the archived-project scenario is gone or renamed", doorTable)
	}
	return doors
}
