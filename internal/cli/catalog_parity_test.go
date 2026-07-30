package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestCatalogFlagParity pins the catalog's flag inventory to the actual
// flag.FlagSet registrations in this package. The CLI reference and TUI
// completion are generated from the catalog, so a flag registered in a
// handler but missing here ships undocumented (restart --remote was the
// first instance), and a catalog flag no handler registers is a phantom.
func TestCatalogFlagParity(t *testing.T) {
	registered := collectRegisteredFlags(t)

	external := map[string]bool{}
	catalogFlags := map[string]map[string]bool{}
	for _, spec := range Catalog() {
		if spec.TUI == TUIExternal {
			// app/mcp/daemon/setup parse outside internal/cli.
			external[spec.Name] = true
			continue
		}
		set := map[string]bool{}
		for _, f := range spec.Flags {
			set[f.Name] = true
		}
		catalogFlags[spec.Name] = set
	}

	for command, got := range registered {
		if external[command] {
			continue
		}
		want, ok := catalogFlags[command]
		if !ok {
			t.Errorf("flagSet scope %q has no catalog entry", command)
			continue
		}
		for name := range got {
			if !want[name] {
				t.Errorf("command %q registers flag --%s but the catalog omits it", command, name)
			}
		}
	}
	for command, want := range catalogFlags {
		got := registered[command]
		for name := range want {
			if got == nil || !got[name] {
				t.Errorf("catalog lists %q flag --%s but no handler registers it", command, name)
			}
		}
	}
}

// collectRegisteredFlags AST-walks the package for `fs := flagSet(env, "scope")`
// declarations and unions every flag registered on such a variable, keyed by
// the scope's first token (the command name). The two helpers that accept a
// *flag.FlagSet (failUnexpectedArgs, restartFlagWasSet) register nothing, so a
// per-function walk is complete.
func collectRegisteredFlags(t *testing.T) map[string]map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	out := map[string]map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok && fn.Body != nil {
				collectFromFunc(fn.Body, out)
			}
		}
	}
	return out
}

func collectFromFunc(body *ast.BlockStmt, out map[string]map[string]bool) {
	scopes := map[string]string{} // fs identifier -> command name
	ast.Inspect(body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.AssignStmt:
			for i, rhs := range node.Rhs {
				scope, ok := flagSetScope(rhs)
				if !ok || i >= len(node.Lhs) {
					continue
				}
				if ident, ok := node.Lhs[i].(*ast.Ident); ok {
					command, _, _ := strings.Cut(scope, " ")
					scopes[ident.Name] = command
				}
			}
		case *ast.CallExpr:
			sel, ok := node.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			recv, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			command, ok := scopes[recv.Name]
			if !ok {
				return true
			}
			flagName, ok := registeredFlagName(sel.Sel.Name, node.Args)
			if !ok {
				return true
			}
			if out[command] == nil {
				out[command] = map[string]bool{}
			}
			out[command][flagName] = true
		}
		return true
	})
}

func flagSetScope(expr ast.Expr) (string, bool) {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return "", false
	}
	fun, ok := call.Fun.(*ast.Ident)
	if !ok || fun.Name != "flagSet" || len(call.Args) != 2 {
		return "", false
	}
	lit, ok := call.Args[1].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	scope, err := strconv.Unquote(lit.Value)
	return scope, err == nil
}

var flagNameFirstArg = []string{"Bool", "String", "Int", "Int64", "Uint", "Uint64", "Float64", "Duration"}

var flagNameSecondArg = []string{"BoolVar", "StringVar", "IntVar", "Int64Var", "UintVar", "Uint64Var", "Float64Var", "DurationVar", "Var", "Func", "TextVar", "BoolFunc"}

func registeredFlagName(method string, args []ast.Expr) (string, bool) {
	pos := -1
	switch {
	case slices.Contains(flagNameFirstArg, method):
		pos = 0
	case slices.Contains(flagNameSecondArg, method):
		pos = 1
	}
	if pos < 0 || pos >= len(args) {
		return "", false
	}
	lit, ok := args[pos].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	name, err := strconv.Unquote(lit.Value)
	return name, err == nil
}
