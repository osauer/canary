// Command app-log-audit rejects production Canary app log emitters that can
// write physical lines without an explicit severity field.
package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type violation struct {
	path    string
	line    int
	message string
}

func main() {
	root := flag.String("root", ".", "repository root")
	flag.Parse()
	violations, err := scanRoot(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "app-log-audit: %v\n", err)
		os.Exit(2)
	}
	if len(violations) == 0 {
		fmt.Println("app-log-audit: production app logging is leveled")
		return
	}
	for _, item := range violations {
		fmt.Fprintf(os.Stderr, "%s:%d: %s\n", item.path, item.line, item.message)
	}
	fmt.Fprintln(os.Stderr, "app-log-audit: use log/slog; production app log records must stay single-line and carry an explicit level")
	os.Exit(1)
}

func scanRoot(root string) ([]violation, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	paths := []string{filepath.Join(root, "cmd", "canary", "app.go")}
	err = filepath.WalkDir(filepath.Join(root, "internal", "app"), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		return nil, err
	}

	fset := token.NewFileSet()
	var violations []violation
	serveFound := false
	for _, path := range paths {
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return nil, fmt.Errorf("parse %s: %w", path, parseErr)
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil, relErr
		}
		imports := importPaths(file)
		if name, ok := importedName(imports, "log"); ok {
			violations = append(violations, at(fset, rel, file.Pos(), fmt.Sprintf("production app imports standard log as %q", name)))
		}

		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := selector.X.(*ast.Ident)
			if !ok {
				return true
			}
			if imports[pkg.Name] == "log" && isRawLogMethod(selector.Sel.Name) {
				violations = append(violations, at(fset, rel, call.Pos(), "standard log call can emit an unlevelled production line"))
			}
			if imports[pkg.Name] == "log/slog" && isSlogMethod(selector.Sel.Name) && firstStringContainsNewline(call.Args) {
				violations = append(violations, at(fset, rel, call.Pos(), "slog message contains a newline and would emit an unlevelled continuation line"))
			}
			return true
		})

		if rel == filepath.Join("cmd", "canary", "app.go") {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Name.Name != "runAppServeWithIO" {
					continue
				}
				serveFound = true
				ast.Inspect(fn.Body, func(node ast.Node) bool {
					call, ok := node.(*ast.CallExpr)
					if !ok || !isRawPrintCall(call, imports) {
						return true
					}
					violations = append(violations, at(fset, rel, call.Pos(), "runAppServeWithIO writes directly instead of the leveled app logger"))
					return true
				})
			}
		}
	}
	if !serveFound {
		violations = append(violations, violation{path: filepath.Join("cmd", "canary", "app.go"), message: "runAppServeWithIO not found; production serve-path log audit has no anchor"})
	}
	sort.Slice(violations, func(i, j int) bool {
		if violations[i].path != violations[j].path {
			return violations[i].path < violations[j].path
		}
		return violations[i].line < violations[j].line
	})
	return violations, nil
}

func importPaths(file *ast.File) map[string]string {
	paths := make(map[string]string, len(file.Imports))
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		name := filepath.Base(path)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		paths[name] = path
	}
	return paths
}

func importedName(imports map[string]string, path string) (string, bool) {
	for name, importedPath := range imports {
		if importedPath == path {
			return name, true
		}
	}
	return "", false
}

func isRawLogMethod(name string) bool {
	switch name {
	case "Print", "Printf", "Println", "Fatal", "Fatalf", "Fatalln", "Panic", "Panicf", "Panicln":
		return true
	default:
		return false
	}
}

func isSlogMethod(name string) bool {
	switch name {
	case "Debug", "Info", "Warn", "Error", "Log":
		return true
	default:
		return false
	}
}

func firstStringContainsNewline(args []ast.Expr) bool {
	if len(args) == 0 {
		return false
	}
	lit, ok := args[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	value, err := strconv.Unquote(lit.Value)
	return err == nil && strings.ContainsAny(value, "\r\n")
}

func isRawPrintCall(call *ast.CallExpr, imports map[string]string) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := selector.X.(*ast.Ident)
	if !ok || imports[pkg.Name] != "fmt" {
		return false
	}
	switch selector.Sel.Name {
	case "Print", "Printf", "Println", "Fprint", "Fprintf", "Fprintln":
		return true
	default:
		return false
	}
}

func at(fset *token.FileSet, path string, pos token.Pos, message string) violation {
	return violation{path: filepath.ToSlash(path), line: fset.Position(pos).Line, message: message}
}
