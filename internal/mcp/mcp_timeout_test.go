package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestMCPToolCallTimeoutBudgets(t *testing.T) {
	t.Parallel()

	clientBudget := func(method string, headroom time.Duration) time.Duration {
		timing, ok := rpc.LookupMethodTiming(method)
		if !ok {
			t.Fatalf("missing timing for %s", method)
		}
		return timing.ClientTimeout(headroom)
	}
	cases := []struct {
		name string
		args json.RawMessage
		want time.Duration
	}{
		{name: "canary_status", want: clientBudget(rpc.MethodStatusHealth, mcpFastToolHeadroom)},
		{name: "canary_technical", args: json.RawMessage(`{"symbols":["ASTS","IREN"]}`), want: mcpAnalysisToolFloor},
		{name: "canary_proposals", args: json.RawMessage(`{}`), want: mcpDefaultToolFloor},
		{name: "canary_proposals", args: json.RawMessage(`{"refresh":true}`), want: clientBudget(rpc.MethodTradeProposalsRefresh, mcpDefaultHeadroom)},
		{name: "canary_opportunities", args: json.RawMessage(`{"refresh":true}`), want: clientBudget(rpc.MethodOpportunitiesRefresh, mcpDefaultHeadroom)},
		{name: "canary_brief", args: json.RawMessage(`{}`), want: clientBudget(rpc.MethodBriefSnapshot, mcpDefaultHeadroom)},
	}
	for _, tc := range cases {
		t.Run(tc.name+" "+string(tc.args), func(t *testing.T) {
			t.Parallel()
			if got := mcpToolCallTimeout(tc.name, tc.args); got != tc.want {
				t.Fatalf("mcpToolCallTimeout(%q, %s) = %s, want %s", tc.name, tc.args, got, tc.want)
			}
		})
	}
}

func TestMCPToolsDeclareCataloguedMethodsAndHeadroom(t *testing.T) {
	t.Parallel()
	for _, tool := range Tools {
		if len(tool.RPCMethods) == 0 {
			t.Errorf("tool %s declares no daemon methods", tool.Name)
			continue
		}
		for _, method := range tool.RPCMethods {
			timing, ok := rpc.LookupMethodTiming(method)
			if !ok {
				t.Errorf("tool %s declares uncatalogued method %q", tool.Name, method)
				continue
			}
			if timing.Lifetime != rpc.MethodLifetimeUnary {
				t.Errorf("tool %s declares streaming method %q; streams belong in MCP resources", tool.Name, method)
			}
		}
		budget := mcpToolCallTimeout(tool.Name, nil)
		for _, method := range mcpToolMethodsForCall(tool.Name, nil) {
			timing, ok := rpc.LookupMethodTiming(method)
			if ok && budget <= timing.DaemonTimeout {
				t.Errorf("tool %s budget %s does not outlive %q daemon timeout %s", tool.Name, budget, method, timing.DaemonTimeout)
			}
		}
	}
}

// Tool.RPCMethods is an executable declaration, not commentary. Compare each
// tool's literal declaration with the conn.Call methods in its own handler;
// the two composed helper paths list their calls explicitly below.
func TestMCPToolMethodDeclarationsMatchHandlers(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "tools.go", nil, 0)
	if err != nil {
		t.Fatalf("parse tools.go: %v", err)
	}
	var toolsLiteral *ast.CompositeLit
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			values, ok := spec.(*ast.ValueSpec)
			if !ok || len(values.Names) != 1 || values.Names[0].Name != "Tools" || len(values.Values) != 1 {
				continue
			}
			toolsLiteral, _ = values.Values[0].(*ast.CompositeLit)
		}
	}
	if toolsLiteral == nil {
		t.Fatal("Tools composite literal not found")
	}
	for _, element := range toolsLiteral.Elts {
		tool, ok := element.(*ast.CompositeLit)
		if !ok {
			continue
		}
		name := toolLiteralName(tool)
		declared := toolLiteralMethods(tool, "RPCMethods")
		called := toolLiteralMethods(tool, "Handler")
		for method := range called {
			if !declared[method] {
				t.Errorf("tool %s calls rpc.%s but does not declare it", name, method)
			}
		}
		for method := range declared {
			if !called[method] {
				t.Errorf("tool %s declares unused rpc.%s", name, method)
			}
		}
	}
}

func toolLiteralName(tool *ast.CompositeLit) string {
	for _, element := range tool.Elts {
		field, ok := element.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := field.Key.(*ast.Ident)
		if !ok || key.Name != "Name" {
			continue
		}
		lit, ok := field.Value.(*ast.BasicLit)
		if !ok {
			return ""
		}
		name, _ := strconv.Unquote(lit.Value)
		return name
	}
	return ""
}

func toolLiteralMethods(tool *ast.CompositeLit, fieldName string) map[string]bool {
	out := map[string]bool{}
	for _, element := range tool.Elts {
		field, ok := element.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := field.Key.(*ast.Ident)
		if !ok || key.Name != fieldName {
			continue
		}
		ast.Inspect(field.Value, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) < 2 {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "Call" {
				return true
			}
			method, ok := call.Args[1].(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := method.X.(*ast.Ident)
			if ok && pkg.Name == "rpc" && strings.HasPrefix(method.Sel.Name, "Method") {
				out[method.Sel.Name] = true
			}
			return true
		})
		if methods, ok := field.Value.(*ast.CompositeLit); ok {
			for _, item := range methods.Elts {
				selector, ok := item.(*ast.SelectorExpr)
				if !ok {
					continue
				}
				pkg, ok := selector.X.(*ast.Ident)
				if ok && pkg.Name == "rpc" && strings.HasPrefix(selector.Sel.Name, "Method") {
					out[selector.Sel.Name] = true
				}
			}
		}
	}
	return out
}

// A starting daemon verifies its database before it publishes its socket, and
// that wait grows with the file. Opening the connection must get that budget
// rather than the tool's own: charging startup to a tool deadline can turn a
// healthy boot into a tool timeout.
func TestMCPToolDialGetsStartupBudgetNotToolBudget(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	startup := dial.StartupBudget()
	toolBudget := mcpToolCallTimeout("canary_status", nil)
	if startup == toolBudget {
		t.Fatalf("test fixture does not distinguish startup budget %s from tool budget %s", startup, toolBudget)
	}

	var dialBudget time.Duration
	srv := NewServer(nil, "test")
	srv.SetContextDialer(func(ctx context.Context) (*dial.Conn, error) {
		if deadline, ok := ctx.Deadline(); ok {
			dialBudget = time.Until(deadline)
		}
		return nil, errors.New("no daemon")
	})

	in := &bytes.Buffer{}
	in.WriteString(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"canary_status","arguments":{}}}` + "\n")
	if err := srv.Serve(context.Background(), in, &bytes.Buffer{}); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	const schedulingMargin = 100 * time.Millisecond
	if delta := startup - dialBudget; delta < 0 || delta > schedulingMargin {
		t.Fatalf("dial budget = %s, want within %s of startup budget %s rather than tool budget %s", dialBudget, schedulingMargin, startup, toolBudget)
	}
}

func TestMCPToolCallTimesOutHungDaemon(t *testing.T) {
	dialer, stop := silentDaemonDialer(t)
	defer stop()

	in := &bytes.Buffer{}
	in.WriteString(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"canary_status","arguments":{}}}` + "\n")
	out := &bytes.Buffer{}
	srv := NewServer(nil, "test")
	srv.SetDialer(dialer)

	wantTimeout := mcpToolCallTimeout("canary_status", nil)
	start := time.Now()
	if err := srv.Serve(context.Background(), in, out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > wantTimeout+2*time.Second {
		t.Fatalf("hung daemon response took %s, want bounded below MCP host timeout", elapsed)
	}

	var resp struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp); err != nil {
		t.Fatalf("decode response: %v\n%s", err, out.String())
	}
	if !resp.Result.IsError {
		t.Fatalf("expected isError=true, got: %s", out.String())
	}
	if len(resp.Result.Content) != 1 || !strings.Contains(resp.Result.Content[0].Text, "canary_status timed out after "+wantTimeout.String()) {
		t.Fatalf("timeout message = %+v", resp.Result.Content)
	}
}

func silentDaemonDialer(t *testing.T) (func() (*dial.Conn, error), func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "canary-mcp-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	path := filepath.Join(dir, "m.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		_ = os.RemoveAll(dir)
		t.Fatalf("listen unix: %v", err)
	}

	var mu sync.Mutex
	var conns []net.Conn
	done := make(chan struct{})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
			go func(c net.Conn) {
				<-done
				_ = c.Close()
			}(conn)
		}
	}()

	stopOnce := sync.Once{}
	stop := func() {
		stopOnce.Do(func() {
			close(done)
			_ = ln.Close()
			_ = os.RemoveAll(dir)
			mu.Lock()
			defer mu.Unlock()
			for _, conn := range conns {
				_ = conn.Close()
			}
		})
	}
	dialer := func() (*dial.Conn, error) {
		return dial.Connect(path)
	}
	return dialer, stop
}
