// Command gobco-hook prepares one test package to persist an instrumented
// package's coverage counters however it terminates.
//
// It exists because doing this with grep and perl is not sound, and two
// reviews (2013, 2014) were right about that. A textual rewrite of
// `os.Exit(` misses an aliased import — `stdos "os"`, on one line or in a
// block — a dot import, an exit reached through a function value
// (`exit := os.Exit`), and `syscall.Exit`. Each of those leaves the
// process ending without a flush, and the failure is SILENT: the ticker
// has already written a report with the full condition list in it, so the
// stale result passes every check the gate makes.
//
// So the work is done on the syntax tree, where an alias is just a name
// and a function value is visibly not a call:
//
//   - every call to os.Exit, under whatever name os is imported, becomes
//     a call to gobcoExit;
//   - a TestMain is renamed to gobcoInnerTestMain so the caller can wrap
//     it and flush on the paths that RETURN;
//   - anything whose termination cannot be proven safe — os.Exit used as
//     a value, a dot import of os, syscall.Exit, runtime.Goexit — is
//     REFUSED, because measuring less must never be quiet.
//
// Usage: go run gobco-hook.go <manifest>
//
// The manifest is a file holding one Go source path per line. They are
// NOT passed as arguments because `go run` folds every leading .go
// argument into the program's own source list, and every file here ends
// in _test.go.
//
// Prints "testmain" or "none" on the first line and the package name on
// the second. Exits 1 with a diagnostic if the package cannot be hooked.
package main

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
)

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
	renamed := false
	pkg := ""
	type parsed struct {
		path    string
		file    *ast.File
		changed bool
	}
	var files []parsed

	for _, path := range paths {
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gobco-hook: parse %s: %v\n", path, err)
			os.Exit(2)
		}
		pkg = file.Name.Name

		// What is "os" called here, and is anything imported in a way
		// that defeats analysis?
		osName := ""
		for _, imp := range file.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			name := ""
			if imp.Name != nil {
				name = imp.Name.Name
			}
			switch {
			case p == "os" && name == ".":
				refuse(fset, imp.Pos(), "dot-imports \"os\"; its Exit calls cannot be identified")
			case p == "syscall" && name == ".":
				refuse(fset, imp.Pos(), "dot-imports \"syscall\"")
			case p == "os" && name != "" && name != "_":
				osName = name
			case p == "os":
				osName = "os"
			}
		}

		changed, rewroteExit := false, false
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.FuncDecl:
				if node.Recv == nil && node.Name.Name == "TestMain" {
					node.Name.Name = "gobcoInnerTestMain"
					renamed, changed = true, true
				}
			case *ast.CallExpr:
				if sel, ok := node.Fun.(*ast.SelectorExpr); ok && isExit(sel, osName) {
					// A call, so rewriting it is enough.
					node.Fun = ast.NewIdent("gobcoExit")
					changed, rewroteExit = true, true
					return true
				}
			case *ast.SelectorExpr:
				// Reached only when NOT the Fun of a CallExpr handled
				// above, i.e. the exit is being taken as a value and
				// could be called from anywhere.
				if isExit(node, osName) {
					refuse(fset, node.Pos(), "uses an exit function as a value; the call site cannot be rewritten")
				}
				if pkgOf(node) == "syscall" && node.Sel.Name == "Exit" {
					refuse(fset, node.Pos(), "calls syscall.Exit, which no flush can precede")
				}
				if pkgOf(node) == "runtime" && node.Sel.Name == "Goexit" {
					refuse(fset, node.Pos(), "calls runtime.Goexit")
				}
			}
			return true
		})
		// Rewriting away the last os.Exit can leave "os" imported and
		// unused, which does not compile. A blank reference is cheaper
		// and safer than deciding whether the import is still needed —
		// and it uses whatever name os carries here.
		if rewroteExit {
			file.Decls = append(file.Decls, &ast.GenDecl{
				Tok: token.VAR,
				Specs: []ast.Spec{&ast.ValueSpec{
					Names: []*ast.Ident{ast.NewIdent("_")},
					Values: []ast.Expr{&ast.SelectorExpr{
						X: ast.NewIdent(osName), Sel: ast.NewIdent("Exit")}},
				}},
			})
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

	if renamed {
		fmt.Println("testmain")
	} else {
		fmt.Println("none")
	}
	fmt.Println(pkg)
}

// isExit reports whether sel is os.Exit under the name os carries here.
// syscall.Exit is deliberately NOT included: it is refused rather than
// rewritten, because it terminates without running anything.
func isExit(sel *ast.SelectorExpr, osName string) bool {
	return osName != "" && sel.Sel.Name == "Exit" && pkgOf(sel) == osName
}

func pkgOf(sel *ast.SelectorExpr) string {
	id, ok := sel.X.(*ast.Ident)
	if !ok {
		return ""
	}
	return id.Name
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
