package api_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// senders are the call methods a client sends a request through.
var senders = map[string]bool{"Call": true, "call": true, "callPrivate": true, "callCashPolicy": true}

// Every client request names its api declaration through rpc.Call, so the
// compiler checks the request and result types. A call that names a method
// by its rpc.Method constant, or picks it in a variable outside a sender
// that takes `method string`, bypasses that check and fails here. The daemon,
// the dial transport and package rpc itself are the senders, not clients, and
// canarytest stands in for the daemon in other packages' tests.
func TestClientsSendThroughDeclarations(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	skip := []string{"internal/daemon/", "internal/rpc/", "internal/dial/", "canarytest/", ".claude/", "dist/", "node_modules/"}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if slices.Contains(skip, rel+"/") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			isSender := takesMethodName(fn)
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) < 3 {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || !senders[sel.Sel.Name] {
					return true
				}
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "rpc" {
					return true // rpc.Call, the typed path itself
				}
				switch arg := call.Args[1].(type) {
				case *ast.SelectorExpr:
					if x, ok := arg.X.(*ast.Ident); ok && x.Name == "rpc" && strings.HasPrefix(arg.Sel.Name, "Method") {
						t.Errorf("%s: sends rpc.%s directly: use rpc.Call with api.%s", fset.Position(call.Pos()), arg.Sel.Name, strings.TrimPrefix(arg.Sel.Name, "Method"))
					}
				case *ast.BasicLit:
					t.Errorf("%s: sends method %s by its name: use rpc.Call with its api declaration", fset.Position(call.Pos()), arg.Value)
				case *ast.Ident:
					if !isSender {
						t.Errorf("%s: sends a method chosen in %s: use rpc.Call with its api declaration", fset.Position(call.Pos()), arg.Name)
					}
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// takesMethodName reports whether fn is itself a sender: it takes the method
// name as a string parameter called method.
func takesMethodName(fn *ast.FuncDecl) bool {
	for _, field := range fn.Type.Params.List {
		if id, ok := field.Type.(*ast.Ident); ok && id.Name == "string" {
			for _, name := range field.Names {
				if name.Name == "method" {
					return true
				}
			}
		}
	}
	return false
}
