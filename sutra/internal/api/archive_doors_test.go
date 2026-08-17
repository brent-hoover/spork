package api

import (
	"bufio"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
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

// knownSites is every guardWritable call site, named by the function that
// holds it and its position within that function — stable when lines move,
// changing only when a site is added, removed, or relocated.
//
// The inventory is here because the door mapping ALONE is not enough, and
// reviews 2017/2018 caught why: several sites serve one door, so deleting
// one of createReview's two guards leaves the door still claimed by the
// other, and the acceptance request is still refused. Mapping proves each
// door is exercised; the inventory proves no site quietly disappeared.
// Neither substitutes for the other.
var knownSites = map[string]int{
	"createDocument":          1,
	"saveDocVersion":          1,
	"linkDocumentToIssue":     1,
	"unlinkDocumentFromIssue": 1,
	"resolveCommentAnchor":    3, // one per anchor kind
	"mutateLabel":             1, // attach and detach share it
	"createIssue":             1,
	"updateIssue":             1,
	"updateIssueStatus":       1,
	"assignIssue":             1,
	"addIssueRelation":        2, // both ends
	"removeIssueRelation":     2, // both ends
	"guardReviewProject":      1, // verdict, consume, and resubmit route through it
	"createReview":            2, // prepare stage and transactional stage
	"resubmitReview":          1,
	"resolveAnchor":           2, // project anchor and issue anchor
	"guardCurrentAnchor":      1, // the anchor being LEFT
}

func TestEveryArchiveGuardHasAScenario(t *testing.T) {
	guarded, byFunc := doorsNamedByGuards(t)
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

	for fn, want := range knownSites {
		if got := byFunc[fn]; got != want {
			t.Errorf("%s holds %d guardWritable calls, expected %d.\n"+
				"A guard was added or removed. If removed, check FIRST whether another site "+
				"still refuses the same door — that is what makes the loss invisible to the "+
				"scenario. Then update knownSites.", fn, got, want)
		}
	}
	for fn, got := range byFunc {
		if _, known := knownSites[fn]; !known {
			t.Errorf("%s holds %d guardWritable calls and is not in knownSites; "+
				"add it, with the door its guards enforce", fn, got)
		}
	}
}

// doorsNamedByGuards walks the package's own source for guardWritable
// calls and reads the `// door:` comment immediately above each. A site
// without one fails: an unannotated guard is a guard nothing has been
// shown to exercise.
func doorsNamedByGuards(t *testing.T) (map[string]bool, map[string]int) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	doors := map[string]bool{}
	byFunc := map[string]int{}
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
		// Walk per function, so each site is attributed to the function
		// that holds it rather than to the file.
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				id, ok := call.Fun.(*ast.Ident)
				if !ok || id.Name != "guardWritable" {
					return true
				}
				pos := fset.Position(call.Pos())
				byFunc[fn.Name.Name]++
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
	}
	return doors, byFunc
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
