package daemon

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

// Every dispatched method is declared in package api, and every declaration
// is dispatched: a client can only send what the daemon serves, through a
// declared request type.
func TestDispatchMethodsMatchAPIDeclarations(t *testing.T) {
	t.Parallel()
	dispatched := dispatchMethodValues(t, rpcMethodConstants(t))
	declared := rpc.DeclaredMethods()
	for method := range dispatched {
		if _, ok := rpc.RequestType(method); !ok {
			t.Errorf("dispatched method %q has no declaration in internal/rpc/api", method)
		}
	}
	for _, method := range declared {
		if !dispatched[method] {
			t.Errorf("declared method %q has no daemon dispatch case", method)
		}
	}
}

// ownRequestDecoders decode a request into a private wire struct that tracks
// which fields were sent, with unknown fields already refused; the request type
// they accept is the declared one by field names, not by Go type.
var ownRequestDecoders = []string{
	"resolveRequestInstrument",   // reads only the contract it resolves, for every method
	"decodeWatchlistMutation",    // watchlist.replace, .add, .remove
	"decodeHoldingLocksReplace",  // holding_locks.replace
	"handleCashSweepPrioritySet", // cash_sweep.priority.set
	"handleSettingsUpdate",       // settings.update: one value per key
}

// Every read of a request's params names the method whose request it decodes,
// through decodeFor or paramsFor, so the compiler checks the type against the
// declaration. A new handler that decodes req.Params any other way fails here.
func TestRequestParamsDecodeThroughTheirDeclaration(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || slices.Contains(ownRequestDecoders, fn.Name.Name) {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) < 2 || exprText(call.Args[0]) != "req.Params" {
					return true
				}
				if exprText(call.Fun) == "decodeFor" {
					return true
				}
				if inner, ok := call.Args[len(call.Args)-1].(*ast.CallExpr); ok && exprText(inner.Fun) == "paramsFor" {
					return true
				}
				t.Errorf("%s: %s decodes req.Params without naming its method: use decodeFor or paramsFor", fset.Position(call.Pos()), fn.Name.Name)
				return true
			})
		}
	}
}

func exprText(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprText(v.X) + "." + v.Sel.Name
	}
	return ""
}

// A request field the method does not declare is refused before any handler
// runs, for a method with a request and for one that takes none.
func TestDispatchRefusesAnUndeclaredRequestField(t *testing.T) {
	t.Parallel()
	srv := &Server{logger: NewLogger(&bytes.Buffer{}, "error")}
	for _, tc := range []struct{ method, params, field string }{
		{rpc.MethodPositionsList, `{"symbol":"AAPL","bogus":1}`, "bogus"},
		{rpc.MethodTradingStatus, `{"account":"U1"}`, "account"},
	} {
		res := dispatchRaw(t, srv, tc.method, tc.params)
		if res.Ok || res.Error == nil || res.Error.Code != rpc.CodeBadRequest || !strings.Contains(res.Error.Message, `unknown field "`+tc.field+`"`) {
			t.Errorf("%s %s: %+v", tc.method, tc.params, res.Error)
		}
	}
}
