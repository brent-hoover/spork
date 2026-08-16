// Command gobco-hook prepares a test directory to persist an instrumented
// package's coverage counters however its binary terminates.
//
// It exists because doing this with grep and perl is not sound, and two
// rounds of review were right about that. A textual rewrite of `os.Exit(`
// misses an aliased import on one line or in a block, a dot import, an
// exit reached through a function value, and syscall.Exit. Each of those
// ends the process with no flush, and the failure is SILENT: the ticker
// has already written a report carrying the complete condition list, so a
// stale result passes every check the gate makes.
//
// The work is therefore done on the syntax tree:
//
//   - every call to os.Exit, under whatever name os is imported, becomes
//     a call to gobcoExit;
//   - a TestMain is renamed to gobcoInnerTestMain so the caller can wrap
//     it and flush on the paths that RETURN;
//   - anything whose termination cannot be proven safe — an exit used as
//     a value, a dot import, syscall.Exit or runtime.Goexit under ANY
//     alias — is REFUSED, because measuring less must never be quiet.
//
// Two things make the analysis binding-aware rather than name-matching,
// both from review 2019/2020. Aliases are resolved from each file's own
// import specs, so `sx "syscall"` is recognised as syscall. And a
// selector is only treated as a package reference when its identifier
// resolves to no local declaration — the parser records that — so a
// local variable named os with an Exit method is left alone. (An import
// name cannot be shadowed at package level: Go forbids declaring the
// same identifier in both the file and package blocks.)
//
// A test directory can hold TWO packages, `foo` and `foo_test`, and they
// need separate treatment: a helper emitted into one is invisible to the
// other. So the report is per package.
//
// Usage: go run gobco-hook.go <manifest>
//
// The manifest is a file holding one Go source path per line. They are
// NOT passed as arguments because `go run` folds every leading .go
// argument into the program's own source list, and every file here ends
// in _test.go.
//
// Prints one line per package involved:
//
//	<package> <testmain|none> <exits|noexits>
//
// Exits 1 with a diagnostic if the directory cannot be hooked.
package main

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strconv"
	"strings"
)

// terminators are the packages whose exits cannot be preceded by a flush.
// os is handled separately: its Exit is rewritten rather than refused.
var terminators = map[string]string{"syscall": "Exit", "runtime": "Goexit"}

type pkgState struct {
	hasTestMain bool
	rewroteExit bool
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "gobco-hook: expected one manifest path")
		os.Exit(2)
	}
	paths, err := readManifest(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "gobco-hook: %v\n", err)
		os.Exit(2)
	}
	if len(paths) == 0 {
		fmt.Fprintln(os.Stderr, "gobco-hook: manifest names no files")
		os.Exit(2)
	}

	fset := token.NewFileSet()
	packages := map[string]*pkgState{}
	type parsed struct {
		path    string
		file    *ast.File
		changed bool
	}
	var files []parsed

	for _, path := range paths {
		// Object resolution is left ON — it is what tells a package
		// qualifier apart from a local variable of the same name.
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gobco-hook: parse %s: %v\n", path, err)
			os.Exit(2)
		}
		pkgName := file.Name.Name
		state := packages[pkgName]
		if state == nil {
			state = &pkgState{}
			packages[pkgName] = state
		}

		// name -> import path, for the packages that matter here.
		imports := map[string]string{}
		for _, imp := range file.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			if p != "os" && p != "syscall" && p != "runtime" {
				continue
			}
			name := p[strings.LastIndex(p, "/")+1:]
			if imp.Name != nil {
				name = imp.Name.Name
			}
			switch name {
			case ".":
				refuse(fset, imp.Pos(), fmt.Sprintf("dot-imports %q; its calls cannot be identified by name", p))
			case "_":
				continue
			}
			imports[name] = p
		}

		// pkgPath resolves a selector's qualifier to an import path, and
		// returns "" when the qualifier is anything else — a local
		// variable, a field, a call result.
		pkgPath := func(sel *ast.SelectorExpr) string {
			id, ok := sel.X.(*ast.Ident)
			if !ok || id.Obj != nil { // Obj set => declared locally, not a package
				return ""
			}
			return imports[id.Name]
		}

		changed := false
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.FuncDecl:
				if node.Recv == nil && node.Name.Name == "TestMain" {
					node.Name.Name = "gobcoInnerTestMain"
					state.hasTestMain, changed = true, true
				}
			case *ast.CallExpr:
				sel, ok := node.Fun.(*ast.SelectorExpr)
				if ok && pkgPath(sel) == "os" && sel.Sel.Name == "Exit" {
					node.Fun = ast.NewIdent("gobcoExit")
					changed, state.rewroteExit = true, true
					return true
				}
			case *ast.SelectorExpr:
				// Reached only when this selector is NOT the Fun of a
				// call rewritten above — so an exit seen here is either
				// taken as a value or belongs to a package no flush can
				// precede.
				path := pkgPath(node)
				if path == "os" && node.Sel.Name == "Exit" {
					refuse(fset, node.Pos(), "uses os.Exit as a value; the call site cannot be rewritten")
				}
				if fn, ok := terminators[path]; ok && node.Sel.Name == fn {
					refuse(fset, node.Pos(), fmt.Sprintf("calls %s.%s, which no flush can precede", path, fn))
				}
			}
			return true
		})

		// Rewriting away the last os.Exit can leave the import unused,
		// which does not compile. A blank reference is cheaper and safer
		// than deciding whether the import is still needed, and it uses
		// whatever name the file gave it.
		if changed && state.rewroteExit {
			for name, path := range imports {
				if path == "os" {
					file.Decls = append(file.Decls, &ast.GenDecl{
						Tok: token.VAR,
						Specs: []ast.Spec{&ast.ValueSpec{
							Names: []*ast.Ident{ast.NewIdent("_")},
							Values: []ast.Expr{&ast.SelectorExpr{
								X: ast.NewIdent(name), Sel: ast.NewIdent("Exit")}},
						}},
					})
					break
				}
			}
		}
		files = append(files, parsed{path, file, changed})
	}

	for _, p := range files {
		if !p.changed {
			continue
		}
		out, err := os.Create(p.path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gobco-hook: rewrite %s: %v\n", p.path, err)
			os.Exit(2)
		}
		if err := format.Node(out, fset, p.file); err != nil {
			fmt.Fprintf(os.Stderr, "gobco-hook: format %s: %v\n", p.path, err)
			os.Exit(2)
		}
		if err := out.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "gobco-hook: close %s: %v\n", p.path, err)
			os.Exit(2)
		}
	}

	names := make([]string, 0, len(packages))
	for name := range packages {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		state := packages[name]
		fmt.Println(name,
			pick(state.hasTestMain, "testmain", "none"),
			pick(state.rewroteExit, "exits", "noexits"))
	}
}

func pick(cond bool, yes, no string) string {
	if cond {
		return yes
	}
	return no
}

func refuse(fset *token.FileSet, pos token.Pos, why string) {
	fmt.Fprintf(os.Stderr, "gobco-hook: %s: this driver %s\n", fset.Position(pos), why)
	os.Exit(1)
}

// readManifest returns the source paths named in the file, one per line,
// ignoring blanks.
func readManifest(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); line != "" {
			out = append(out, line)
		}
	}
	return out, scanner.Err()
}
