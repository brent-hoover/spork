package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// archiveDoors is the number of places that enforce AC-project-archive's
// read-only rule. Each one is exercised by name in REQ-projects.feature's
// "every door into an archived project refuses the write".
//
// The count is asserted rather than described because the AC names a SET,
// and the way a set claim rots is that someone adds a member without
// adding a scenario — defect species 1, the reason that scenario exists.
// The suite passed for the whole build with one of twenty-three doors
// covered, and nothing said so.
//
// If this test fails you have added or removed a guardWritable call. Add
// the matching row to the feature table and the matching entry to
// acceptance/archive_steps_test.go's `doors`, then update this number.
// Do not simply update the number.
const archiveDoors = 23

func TestEveryArchiveGuardHasAScenario(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	found := 0
	sites := []string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
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
			found++
			sites = append(sites, fset.Position(call.Pos()).String())
			return true
		})
	}
	if found != archiveDoors {
		t.Fatalf("guardWritable is called from %d places, expected %d.\n"+
			"A door was added or removed without updating the archived-project scenario.\n"+
			"Add its row to verification/REQ-projects.feature and its entry to\n"+
			"acceptance/archive_steps_test.go, then update archiveDoors.\nSites:\n  %s",
			found, archiveDoors, strings.Join(sites, "\n  "))
	}
}
