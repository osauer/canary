package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/v2/internal/cli"
	"github.com/osauer/ibkr/v2/internal/dial"
	"github.com/osauer/ibkr/v2/internal/risk"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

// TestParity is the binding drift gate: every CLI subcommand from
// internal/cli must either have a matching MCP tool (`ibkr_<name>`) or be
// listed in ExcludedCLI with a justification. Adding a new CLI command
// without an MCP tool — or vice versa — fails `make check`.
func TestParity(t *testing.T) {
	t.Parallel()

	cliNames := map[string]bool{}
	for _, c := range cli.Commands() {
		cliNames[c.Name] = true
	}

	mcpNames := map[string]bool{}
	for _, tool := range Tools {
		name := strings.TrimPrefix(tool.Name, "ibkr_")
		if name == tool.Name {
			t.Errorf("MCP tool %q must use the ibkr_ prefix", tool.Name)
			continue
		}
		mcpNames[strings.ReplaceAll(name, "_", "-")] = true
	}

	for name := range cliNames {
		if _, excluded := ExcludedCLI[name]; excluded {
			if mcpNames[name] {
				t.Errorf("CLI command %q is in ExcludedCLI but also has an MCP tool — pick one", name)
			}
			continue
		}
		if !mcpNames[name] && !hasMCPSubtool(mcpNames, name) {
			t.Errorf("CLI command %q has no MCP tool (ibkr_%s); add a Tool entry to internal/mcp/tools.go or list it in ExcludedCLI with a reason", name, name)
		}
	}
	for name := range mcpNames {
		if cliNames[name] {
			continue
		}
		// MCP tools can expose CLI subverbs as their own tool (e.g.
		// `ibkr scan params` → `ibkr_scan_params`). Accept the tool if
		// the prefix up to the first underscore is itself a CLI command,
		// since MCP clients use a flat tool surface and a focused tool
		// per subverb is easier for agents than one mega-tool with a
		// mode discriminator.
		if i := strings.IndexAny(name, "_-"); i > 0 && cliNames[name[:i]] {
			continue
		}
		t.Errorf("MCP tool ibkr_%s has no CLI counterpart (cli.Commands has no command named %q nor a parent command for a subverb) — drop the tool or add the CLI command", name, name)
	}
	for name := range ExcludedCLI {
		if !cliNames[name] {
			t.Errorf("ExcludedCLI entry %q is not in cli.Commands; remove the stale exclusion", name)
		}
	}
}

func hasMCPSubtool(mcpNames map[string]bool, cliName string) bool {
	underscorePrefix := cliName + "_"
	hyphenPrefix := cliName + "-"
	for name := range mcpNames {
		if strings.HasPrefix(name, underscorePrefix) || strings.HasPrefix(name, hyphenPrefix) {
			return true
		}
	}
	return false
}

// TestSchemasAreValidJSON guards against typos in the hand-built JSON
// Schemas. Every tool's InputSchema must be a well-formed object schema.
func TestSchemasAreValidJSON(t *testing.T) {
	t.Parallel()
	for _, tool := range Tools {
		var got map[string]any
		if err := json.Unmarshal(tool.JSONSchema, &got); err != nil {
			t.Errorf("tool %s: invalid JSONSchema: %v", tool.Name, err)
			continue
		}
		if got["type"] != "object" {
			t.Errorf("tool %s: schema must declare type=object (got %v)", tool.Name, got["type"])
		}
	}
}

// TestTradingToolAllowlist is the safety counterpart to the build-tag stub
// and the PreToolUse hook. Preview/read-model order tools are explicit;
// any broker-write tool name still fails the MCP surface.
func TestTradingToolAllowlist(t *testing.T) {
	t.Parallel()
	for _, tool := range Tools {
		switch tool.Name {
		case "ibkr_trading_status":
			desc := strings.ToLower(tool.Description)
			if !strings.Contains(desc, "does not place") || !strings.Contains(desc, "only reports readiness") {
				t.Errorf("tool %s must explicitly say it only reports readiness and does not place orders", tool.Name)
			}
			continue
		case "ibkr_orders_open", "ibkr_orders_history", "ibkr_order_status":
			desc := strings.ToLower(tool.Description)
			if !strings.Contains(desc, "read-only") || !strings.Contains(desc, "does not place") {
				t.Errorf("tool %s must explicitly say it is read-only and does not place orders", tool.Name)
			}
			if tool.Name == "ibkr_orders_history" && !strings.Contains(desc, "not an ibkr activity statement") {
				t.Errorf("tool %s must warn that local history is not an IBKR Activity Statement", tool.Name)
			}
			continue
		case "ibkr_order_preview":
			desc := strings.ToLower(tool.Description)
			for _, want := range []string{"does not submit an order", "submit_eligible", "executable=false"} {
				if !strings.Contains(desc, want) {
					t.Errorf("tool %s description must include %q", tool.Name, want)
				}
			}
			continue
		}
		for _, banned := range []string{"order", "trade", "cancel", "submit", "place"} {
			if strings.Contains(strings.ToLower(tool.Name), banned) {
				t.Errorf("tool %s name contains unallowlisted trading verb %q", tool.Name, banned)
			}
		}
	}
}

func TestOpportunitiesToolIsReadOnlyDiscovery(t *testing.T) {
	t.Parallel()
	idx := slices.IndexFunc(Tools, func(tool Tool) bool { return tool.Name == "ibkr_opportunities" })
	if idx < 0 {
		t.Fatal("missing ibkr_opportunities tool")
	}
	tool := Tools[idx]
	if tool.ReadOnlyHint == nil || !*tool.ReadOnlyHint {
		t.Fatalf("ibkr_opportunities ReadOnlyHint=%v, want true", tool.ReadOnlyHint)
	}
	desc := strings.ToLower(tool.Description)
	for _, want := range []string{"read-only", "option exercise", "does not preview", "submit exercise"} {
		if !strings.Contains(desc, want) {
			t.Fatalf("ibkr_opportunities description missing %q: %s", want, tool.Description)
		}
	}
	if strings.Contains(string(tool.JSONSchema), "exercise") {
		t.Fatalf("ibkr_opportunities schema must not expose exercise controls: %s", tool.JSONSchema)
	}
}

func TestRulesToolDescribesPolicyAndTerminalProvenance(t *testing.T) {
	t.Parallel()
	tool, ok := lookupTool("ibkr_rules")
	if !ok {
		t.Fatal("missing ibkr_rules tool")
	}
	desc := strings.ToLower(tool.Description)
	for _, want := range []string{
		"compiled model is not itself proof",
		"verified_terminal",
		"terminal_non_reporting",
		"never pass",
		"symbol-wide exemption",
		"conflicting or expired terminal evidence stays unknown",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("ibkr_rules description missing %q", want)
		}
	}
}

func TestNormalizeMCPPreviewOrderTypeRejectsLMTTrailContradiction(t *testing.T) {
	t.Parallel()
	if _, err := normalizeMCPPreviewOrderType("LMT", true, false); err == nil || !strings.Contains(err.Error(), "cannot include trail") {
		t.Fatalf("normalizeMCPPreviewOrderType LMT+trail err = %v, want contradiction", err)
	}
	got, err := normalizeMCPPreviewOrderType("", true, true)
	if err != nil {
		t.Fatalf("normalizeMCPPreviewOrderType default trail-limit: %v", err)
	}
	if got != rpc.OrderTypeTRAILLIMIT {
		t.Fatalf("normalizeMCPPreviewOrderType default = %q, want TRAIL LIMIT", got)
	}
}

func TestOrderPreviewSchemaIncludesRouteAndReplaceFields(t *testing.T) {
	t.Parallel()
	tool, ok := lookupTool("ibkr_order_preview")
	if !ok {
		t.Fatal("missing ibkr_order_preview tool")
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(tool.JSONSchema, &schema); err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	for _, prop := range []string{"exchange", "primary_exchange", "currency", "replace_id", "trigger_method"} {
		if _, ok := schema.Properties[prop]; !ok {
			t.Fatalf("ibkr_order_preview schema missing %s", prop)
		}
	}
}

func TestOrderPreviewHandlerForwardsRouteAndReplaceFields(t *testing.T) {
	t.Parallel()
	tool, ok := lookupTool("ibkr_order_preview")
	if !ok {
		t.Fatal("missing ibkr_order_preview tool")
	}
	conn, calls := startMCPOrderPreviewFakeConn(t, rpc.OrderPreviewResult{PreviewTokenID: "tok", TokenMinted: true})
	defer conn.Close()
	args := json.RawMessage(`{"action":"sell","symbol":"AAPL","quantity":1,"order_type":"TRAIL","trailing_percent":2,"trigger_method":4,"exchange":"SMART","primary_exchange":"NASDAQ","currency":"USD","replace_id":"ibkr-123"}`)
	if _, err := tool.Handler(context.Background(), conn, args); err != nil {
		t.Fatalf("handler: %v", err)
	}
	call := <-calls
	if call.method != rpc.MethodOrderPreview {
		t.Fatalf("method = %s, want %s", call.method, rpc.MethodOrderPreview)
	}
	p := call.params
	if p.TriggerMethod != 4 || p.ReplaceID != "ibkr-123" {
		t.Fatalf("params trigger/replace = %d/%q, want 4/ibkr-123", p.TriggerMethod, p.ReplaceID)
	}
	if p.Contract.Exchange != "SMART" || p.Contract.PrimaryExch != "NASDAQ" || p.Contract.Currency != "USD" {
		t.Fatalf("contract route = %+v, want SMART/NASDAQ/USD", p.Contract)
	}
}

func TestToolsHaveDirectoryAnnotations(t *testing.T) {
	t.Parallel()
	for _, tool := range Tools {
		if strings.TrimSpace(tool.Title) == "" {
			t.Errorf("tool %s missing Title; Anthropic directory submissions require human-readable tool titles", tool.Name)
		}
	}
}

func TestRulesToolDescribesWSHEntitlementWithoutProviderInternals(t *testing.T) {
	idx := slices.IndexFunc(Tools, func(tool Tool) bool { return tool.Name == "ibkr_rules" })
	if idx < 0 {
		t.Fatal("ibkr_rules tool missing")
	}
	desc := Tools[idx].Description
	for _, want := range []string{"Wall Street Horizon", "WSH research subscription", "Nasdaq remains active", "unknown, never pass"} {
		if !strings.Contains(desc, want) {
			t.Fatalf("ibkr_rules description missing %q: %s", want, desc)
		}
	}
	for _, forbidden := range []string{"ibkr_wsh", "not_entitled", "wsh_metadata", "wsh_event"} {
		if strings.Contains(desc, forbidden) {
			t.Fatalf("ibkr_rules description exposed provider internals %q: %s", forbidden, desc)
		}
	}
}

type mcpOrderPreviewCall struct {
	method string
	params rpc.OrderPreviewParams
}

func startMCPOrderPreviewFakeConn(t *testing.T, result rpc.OrderPreviewResult) (*dial.Conn, <-chan mcpOrderPreviewCall) {
	t.Helper()
	socketPath := filepath.Join("/tmp", fmt.Sprintf("ibkr-mcp-order-preview-%d.sock", time.Now().UnixNano()))
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen fake daemon: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	calls := make(chan mcpOrderPreviewCall, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		var req rpc.Request
		if err := json.NewDecoder(c).Decode(&req); err != nil {
			return
		}
		var params rpc.OrderPreviewParams
		_ = json.Unmarshal(req.Params, &params)
		calls <- mcpOrderPreviewCall{method: req.Method, params: params}
		raw, _ := json.Marshal(result)
		_ = json.NewEncoder(c).Encode(rpc.Response{ID: req.ID, Ok: true, Result: raw})
	}()

	conn, err := dial.Connect(socketPath)
	if err != nil {
		t.Fatalf("connect fake daemon: %v", err)
	}
	return conn, calls
}

// TestInitializeAndToolsList walks the MCP handshake without touching the
// daemon: send initialize, expect serverInfo + tools capability; send
// tools/list, expect every Tool we registered.
func TestInitializeAndToolsList(t *testing.T) {
	t.Parallel()

	in := &bytes.Buffer{}
	in.WriteString(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n")
	in.WriteString(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n")

	out := &bytes.Buffer{}
	srv := NewServer(nil, "test")
	if err := srv.Serve(context.Background(), in, out); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	lines := bytes.Split(bytes.TrimRight(out.Bytes(), "\n"), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("expected 2 response lines, got %d:\n%s", len(lines), out.String())
	}

	var initResp struct {
		Result struct {
			ProtocolVersion string         `json:"protocolVersion"`
			Capabilities    map[string]any `json:"capabilities"`
			ServerInfo      struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal(lines[0], &initResp); err != nil {
		t.Fatalf("initialize response: %v", err)
	}
	if initResp.Result.ProtocolVersion != ProtocolVersion {
		t.Errorf("protocolVersion: got %q want %q", initResp.Result.ProtocolVersion, ProtocolVersion)
	}
	if initResp.Result.ServerInfo.Name != "ibkr" {
		t.Errorf("serverInfo.name: got %q want %q", initResp.Result.ServerInfo.Name, "ibkr")
	}
	if _, ok := initResp.Result.Capabilities["tools"]; !ok {
		t.Errorf("capabilities missing tools key: %v", initResp.Result.Capabilities)
	}

	var listResp struct {
		Result struct {
			Tools []struct {
				Name        string          `json:"name"`
				Title       string          `json:"title"`
				Description string          `json:"description"`
				InputSchema json.RawMessage `json:"inputSchema"`
				Annotations struct {
					Title        string `json:"title"`
					ReadOnlyHint bool   `json:"readOnlyHint"`
				} `json:"annotations"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(lines[1], &listResp); err != nil {
		t.Fatalf("tools/list response: %v", err)
	}
	if len(listResp.Result.Tools) != len(Tools) {
		t.Errorf("tools/list returned %d tools, expected %d", len(listResp.Result.Tools), len(Tools))
	}
	for i, want := range Tools {
		if listResp.Result.Tools[i].Name != want.Name {
			t.Errorf("tool[%d].name: got %q want %q", i, listResp.Result.Tools[i].Name, want.Name)
		}
		if listResp.Result.Tools[i].Title != want.Title {
			t.Errorf("tool[%d].title: got %q want %q", i, listResp.Result.Tools[i].Title, want.Title)
		}
		if listResp.Result.Tools[i].Annotations.Title != want.Title {
			t.Errorf("tool[%d].annotations.title: got %q want %q", i, listResp.Result.Tools[i].Annotations.Title, want.Title)
		}
		wantReadOnly := true
		if want.ReadOnlyHint != nil {
			wantReadOnly = *want.ReadOnlyHint
		}
		if listResp.Result.Tools[i].Annotations.ReadOnlyHint != wantReadOnly {
			t.Errorf("tool[%d].annotations.readOnlyHint: got %v want %v", i, listResp.Result.Tools[i].Annotations.ReadOnlyHint, wantReadOnly)
		}
	}
}

func TestParseProfile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		raw     string
		want    Profile
		wantErr bool
	}{
		{want: ProfileFull},
		{raw: "full", want: ProfileFull},
		{raw: " monitor ", want: ProfileMonitor},
		{raw: "debug", wantErr: true},
	}
	for _, tc := range tests {
		got, err := ParseProfile(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("ParseProfile(%q) err = nil, want error", tc.raw)
			}
			continue
		}
		if err != nil {
			t.Fatalf("ParseProfile(%q) err = %v", tc.raw, err)
		}
		if got != tc.want {
			t.Fatalf("ParseProfile(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestMonitorProfileInitializeAndToolsList(t *testing.T) {
	t.Parallel()

	lines := serveMCP(t, ProfileMonitor,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	)
	if len(lines) != 2 {
		t.Fatalf("expected 2 response lines, got %d", len(lines))
	}

	var initResp struct {
		Result struct {
			Instructions string `json:"instructions"`
		} `json:"result"`
	}
	if err := json.Unmarshal(lines[0], &initResp); err != nil {
		t.Fatalf("initialize response: %v", err)
	}
	if !strings.Contains(initResp.Result.Instructions, "Use `ibkr_canary` first") {
		t.Fatalf("monitor instructions should steer first call to ibkr_canary: %q", initResp.Result.Instructions)
	}
	if !strings.Contains(initResp.Result.Instructions, "call `ibkr_status` only") {
		t.Fatalf("monitor instructions should keep ibkr_status diagnostic-only: %q", initResp.Result.Instructions)
	}

	var listResp struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(lines[1], &listResp); err != nil {
		t.Fatalf("tools/list response: %v", err)
	}
	got := []string{}
	for _, tool := range listResp.Result.Tools {
		got = append(got, tool.Name)
	}
	want := []string{"ibkr_canary", "ibkr_status"}
	if !slices.Equal(got, want) {
		t.Fatalf("monitor tools = %v, want %v", got, want)
	}
	for _, tool := range listResp.Result.Tools {
		if !strings.Contains(strings.ToLower(tool.Description), "read-only") {
			t.Fatalf("monitor description for %s should say read-only: %q", tool.Name, tool.Description)
		}
	}
}

func TestMonitorToolsListPayloadAtLeastHalfSmallerThanFull(t *testing.T) {
	t.Parallel()

	full := serveMCP(t, ProfileFull, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	monitor := serveMCP(t, ProfileMonitor, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if len(full) != 1 || len(monitor) != 1 {
		t.Fatalf("unexpected response line counts: full=%d monitor=%d", len(full), len(monitor))
	}
	if len(monitor[0])*2 > len(full[0]) {
		t.Fatalf("monitor tools/list payload = %d bytes, full = %d bytes; want monitor at least 50%% smaller", len(monitor[0]), len(full[0]))
	}
}

func TestMonitorProfileRejectsHiddenTools(t *testing.T) {
	t.Parallel()

	lines := serveMCP(t, ProfileMonitor, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"ibkr_positions","arguments":{}}}`)
	if len(lines) != 1 {
		t.Fatalf("expected 1 response line, got %d", len(lines))
	}
	var resp struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(lines[0], &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error == nil {
		t.Fatalf("expected error response, got %s", string(lines[0]))
	}
	if resp.Error.Code != codeMethodNotFound {
		t.Fatalf("error code = %d, want %d", resp.Error.Code, codeMethodNotFound)
	}
	if !strings.Contains(resp.Error.Message, "profile monitor") || !strings.Contains(resp.Error.Message, "ibkr_positions") {
		t.Fatalf("hidden-tool message should name profile and tool, got %q", resp.Error.Message)
	}
}

func TestCompactViewSchemas(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		tool string
		want map[string]bool
	}{
		{tool: "ibkr_canary", want: map[string]bool{rpc.ViewFull: true, rpc.ViewAlert: true}},
		{tool: "ibkr_regime", want: map[string]bool{rpc.ViewDetail: true, rpc.ViewMonitor: true}},
		{tool: "ibkr_positions", want: map[string]bool{rpc.ViewFull: true, rpc.ViewRisk: true}},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			tool, ok := lookupTool(tc.tool)
			if !ok {
				t.Fatalf("%s not registered", tc.tool)
			}
			enum := schemaEnumValues(t, tool.JSONSchema, "view")
			missing := map[string]bool{}
			for want := range tc.want {
				missing[want] = true
			}
			for _, got := range enum {
				delete(missing, got)
			}
			if len(missing) > 0 {
				t.Fatalf("%s view enum missing %v (got %v)", tc.tool, missing, enum)
			}
		})
	}
}

func serveMCP(t *testing.T, profile Profile, messages ...string) []json.RawMessage {
	t.Helper()
	in := &bytes.Buffer{}
	for _, msg := range messages {
		in.WriteString(msg)
		in.WriteByte('\n')
	}
	out := &bytes.Buffer{}
	srv := NewServer(nil, "test")
	srv.SetProfile(profile)
	if err := srv.Serve(context.Background(), in, out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	raw := bytes.Split(bytes.TrimRight(out.Bytes(), "\n"), []byte("\n"))
	lines := make([]json.RawMessage, 0, len(raw))
	for _, line := range raw {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		lines = append(lines, append(json.RawMessage(nil), line...))
	}
	return lines
}

func schemaEnumValues(t *testing.T, schema json.RawMessage, property string) []string {
	t.Helper()
	var decoded struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schema, &decoded); err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	prop, ok := decoded.Properties[property]
	if !ok {
		t.Fatalf("schema missing property %q", property)
	}
	return prop.Enum
}

// TestUnknownToolReturnsMethodNotFound verifies the protocol-error branch
// for tools/call with a bogus name.
func TestUnknownToolReturnsMethodNotFound(t *testing.T) {
	t.Parallel()
	in := &bytes.Buffer{}
	in.WriteString(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope","arguments":{}}}` + "\n")
	out := &bytes.Buffer{}
	srv := NewServer(nil, "test")
	if err := srv.Serve(context.Background(), in, out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	var resp struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error == nil {
		t.Fatalf("expected error response, got: %s", out.String())
	}
	if resp.Error.Code != codeMethodNotFound {
		t.Errorf("code: got %d want %d", resp.Error.Code, codeMethodNotFound)
	}
}

// TestIbkrGammaSchemaHasScopeParam pins the P1 wire-parity gate: the
// ibkr_gamma tool's input schema must declare a `scope` property whose
// enum carries the three CLI-equivalent values ("spy", "spx",
// "spy+spx"). Drops the param without updating the schema and this
// test fails — clients reading tools/list never see the new arg.
func TestIbkrGammaSchemaHasScopeParam(t *testing.T) {
	t.Parallel()
	tool, ok := lookupTool("ibkr_gamma")
	if !ok {
		t.Fatalf("ibkr_gamma tool not registered")
	}
	var schema struct {
		Properties map[string]struct {
			Type string   `json:"type"`
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tool.JSONSchema, &schema); err != nil {
		t.Fatalf("decode ibkr_gamma schema: %v", err)
	}
	scope, ok := schema.Properties["scope"]
	if !ok {
		t.Fatalf("ibkr_gamma schema missing 'scope' property")
	}
	if scope.Type != "string" {
		t.Errorf("scope.type: got %q want %q", scope.Type, "string")
	}
	want := map[string]bool{"spy": true, "spx": true, "spy+spx": true}
	for _, v := range scope.Enum {
		delete(want, v)
	}
	if len(want) > 0 {
		t.Errorf("scope.enum missing values: %v (got %v)", want, scope.Enum)
	}
}

// TestIbkrGammaRejectsUnknownScope pins the validation edge: a bogus
// scope value surfaces as a tool error (isError=true) rather than
// hitting the daemon with garbage and getting a generic wire error.
// MCP-side normalisation keeps the contract close to the CLI's
// behaviour for --only.
func TestIbkrGammaRejectsUnknownScope(t *testing.T) {
	t.Parallel()
	tool, ok := lookupTool("ibkr_gamma")
	if !ok {
		t.Fatalf("ibkr_gamma tool not registered")
	}
	_, err := tool.Handler(context.Background(), nil, json.RawMessage(`{"scope":"nope"}`))
	if err == nil {
		t.Fatalf("expected error on unknown scope, got nil")
	}
	if !strings.Contains(err.Error(), "scope") {
		t.Errorf("error should mention 'scope', got: %v", err)
	}
}

func TestIbkrWatchSchemaHasQuoteParams(t *testing.T) {
	t.Parallel()
	tool, ok := lookupTool("ibkr_watch")
	if !ok {
		t.Fatalf("ibkr_watch tool not registered")
	}
	var schema struct {
		Properties map[string]struct {
			Type        string `json:"type"`
			Description string `json:"description"`
			Minimum     int    `json:"minimum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tool.JSONSchema, &schema); err != nil {
		t.Fatalf("decode ibkr_watch schema: %v", err)
	}
	for _, name := range []string{"include_quotes", "include_positions", "timeout_ms"} {
		if _, ok := schema.Properties[name]; !ok {
			t.Fatalf("ibkr_watch schema missing %q", name)
		}
	}
	if schema.Properties["include_quotes"].Type != "boolean" {
		t.Fatalf("include_quotes.type = %q, want boolean", schema.Properties["include_quotes"].Type)
	}
	if schema.Properties["include_positions"].Type != "boolean" {
		t.Fatalf("include_positions.type = %q, want boolean", schema.Properties["include_positions"].Type)
	}
	if schema.Properties["timeout_ms"].Type != "integer" || schema.Properties["timeout_ms"].Minimum != 100 {
		t.Fatalf("timeout_ms schema = %+v, want integer minimum 100", schema.Properties["timeout_ms"])
	}
	if !strings.Contains(tool.Description, "decision-making monitor") {
		t.Fatalf("ibkr_watch description should explain the enriched quote use case:\n%s", tool.Description)
	}
	if !strings.Contains(schema.Properties["include_quotes"].Description, "default true") {
		t.Fatalf("include_quotes description should document the quote-default behavior: %q", schema.Properties["include_quotes"].Description)
	}
}

func TestWatchlistQuoteContractUsesHoldingRoute(t *testing.T) {
	t.Parallel()
	got := watchlistQuoteContract("MBG", &rpc.WatchlistHolding{Currency: "EUR", Exchange: "IBIS"})
	if got.Currency != "EUR" || got.Market != "de" || got.Exchange != "" {
		t.Fatalf("watchlistQuoteContract route = %+v, want market=de EUR", got)
	}
}

func TestIbkrWatchListOnlyRequiresDaemon(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	tool, ok := lookupTool("ibkr_watch")
	if !ok {
		t.Fatalf("ibkr_watch tool not registered")
	}
	_, err := tool.Handler(context.Background(), nil, json.RawMessage(`{"include_quotes":false}`))
	if err == nil {
		t.Fatal("expected list-only watch to require a daemon connection")
	}
	if !strings.Contains(err.Error(), "daemon connection required") {
		t.Fatalf("error = %v, want daemon connection required", err)
	}
}

func TestIbkrCalendarSchemaHasSupportedMarkets(t *testing.T) {
	t.Parallel()
	tool, ok := lookupTool("ibkr_calendar")
	if !ok {
		t.Fatalf("ibkr_calendar tool not registered")
	}
	var schema struct {
		Properties map[string]struct {
			Type string   `json:"type"`
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tool.JSONSchema, &schema); err != nil {
		t.Fatalf("decode ibkr_calendar schema: %v", err)
	}
	market, ok := schema.Properties["market"]
	if !ok {
		t.Fatalf("ibkr_calendar schema missing market property")
	}
	if market.Type != "string" {
		t.Fatalf("market.type = %q, want string", market.Type)
	}
	want := map[string]bool{"us": true, "us-equity": true, "us-options": true, "de": true, "de-xetra": true}
	for _, v := range market.Enum {
		delete(want, v)
	}
	if len(want) > 0 {
		t.Fatalf("market enum missing values: %v (got %v)", want, market.Enum)
	}
}

// TestIbkrRegimeResponseHasCompositeStreaksQuality is the P2 wire-
// parity gate: every RegimeSnapshotResult field the CLI consumes must
// be reachable via the MCP envelope so an MCP-only client sees the
// same surface. Marshals the rpc shape with all the new optional
// fields populated, then re-decodes and checks each load-bearing
// JSON key landed.
func TestIbkrRegimeResponseHasCompositeStreaksQuality(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	ratio := 0.91
	vix := 14.5
	res := rpc.RegimeSnapshotResult{
		AsOf:        now,
		Fingerprint: rpc.Fingerprint{Version: rpc.RegimeFingerprintVersion, Key: "sha256:regime"},
		Lifecycle: rpc.LifecycleState{
			Stage:       rpc.LifecycleEarlyWarning,
			Severity:    string(risk.SeverityWatch),
			Readiness:   string(risk.PlannerReadinessWatch),
			Timing:      rpc.LifecycleTimingForwardWarning,
			Confidence:  "medium",
			Evidence:    []rpc.LifecycleEvidence{{Source: "vol", Signal: "cluster", Bucket: "yellow", Timing: rpc.LifecycleTimingForwardWarning, Severity: string(risk.SeverityWatch)}},
			Fingerprint: rpc.Fingerprint{Version: "lifecycle-fp-v1", Key: "sha256:lifecycle"},
		},
		SourceHealth: []rpc.SourceHealth{{
			Source: "vol", Status: "ok", AsOf: now, Confidence: "high",
			FingerprintStability: rpc.FingerprintStabilitySemanticBuckets,
		}},
		VIXTermStructure: rpc.RegimeVIXTerm{
			RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{
				Band:       "green",
				BandReason: "<0.92 contango",
				Thresholds: &rpc.RegimeThresholds{
					Label: "vix_term_structure_v1", Green: "VIX/VIX3M < 0.92", Yellow: "0.92-1.00", Red: ">=1.00",
					Heuristic: true, PendingBacktest: true,
				},
				AsOf: &rpc.RegimeAsOfSummary{Label: "live", Time: now, Freshness: rpc.FreshnessLive, Source: "VIX tick"},
			},
			Status: rpc.RegimeStatusOK,
			VIX:    &vix,
			Ratio:  &ratio,
			VIXQuality: &rpc.Quality{
				AsOf: now, FreshnessClass: rpc.FreshnessLive,
				Confidence: rpc.ConfidenceFirm, Source: "VIX tick",
			},
			Streak: &rpc.StreakInfo{Band: "green", Sessions: 3, Since: "2026-05-20"},
		},
		Composite: rpc.RegimeComposite{
			Verdict:              "Normal regime",
			GreenCount:           3,
			YellowCount:          0,
			RedCount:             0,
			RankedCount:          3,
			ClusterGreenCount:    3,
			ClusterRankedCount:   3,
			ClusterUnrankedCount: 4,
		},
		Summary: rpc.RegimeSummary{
			Label: "Normal regime", Evidence: "3 green clusters / 4 unranked clusters", IndicatorEvidence: "3 green",
			PunchLine:  "volatility term structure is constructive.",
			Confidence: "high", NotAdvice: "Regime read only; not investment advice or a trade recommendation.",
		},
		WarningDetails: []rpc.RegimeWarning{{
			Code: "gamma_zero_computing", Scope: "gamma_zero", Severity: "warning",
			Message: "dealer gamma is still computing", Impact: "gamma is unranked", Action: "retry later",
		}},
		SpecDoc: "https://osauer.dev/ibkr/docs/internals/regime-dashboard.html",
	}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	fp, ok := wire["fingerprint"].(map[string]any)
	if !ok || fp["version"] != rpc.RegimeFingerprintVersion || fp["key"] == "" {
		t.Fatalf("fingerprint missing or malformed: %#v", wire["fingerprint"])
	}
	if _, ok := wire["lifecycle"].(map[string]any); !ok {
		t.Fatalf("lifecycle missing from regime envelope: %s", b)
	}
	if sources, ok := wire["source_health"].([]any); !ok || len(sources) == 0 {
		t.Fatalf("source_health missing from regime envelope: %#v", wire["source_health"])
	}
	// Top-level composite key + nested verdict — agents read this
	// path to display the headline.
	comp, ok := wire["composite"].(map[string]any)
	if !ok {
		t.Fatalf("composite missing from envelope: %s", b)
	}
	if comp["verdict"] != "Normal regime" {
		t.Errorf("composite.verdict: got %v want %q", comp["verdict"], "Normal regime")
	}
	if _, ok := comp["cluster_green_count"]; !ok {
		t.Errorf("composite should expose cluster counts for agent scoring: %#v", comp)
	}
	summary, ok := wire["summary"].(map[string]any)
	if !ok {
		t.Fatalf("summary missing from envelope: %s", b)
	}
	if summary["punch_line"] == "" || summary["not_advice"] == "" {
		t.Errorf("summary should carry punch_line and not_advice: %#v", summary)
	}
	if summary["indicator_evidence"] == "" {
		t.Errorf("summary should carry indicator_evidence alongside cluster evidence: %#v", summary)
	}
	for _, key := range []string{"vol_of_vol", "credit_spreads", "funding_stress"} {
		if _, ok := wire[key]; !ok {
			t.Errorf("%s missing from regime MCP/JSON shape", key)
		}
	}
	warnings, ok := wire["warning_details"].([]any)
	if !ok || len(warnings) != 1 {
		t.Fatalf("warning_details missing or wrong shape: %#v", wire["warning_details"])
	}
	// Streak + Quality nested under each indicator row.
	vixRow, ok := wire["vix_term_structure"].(map[string]any)
	if !ok {
		t.Fatalf("vix_term_structure missing")
	}
	if _, ok := vixRow["streak"]; !ok {
		t.Errorf("vix_term_structure.streak missing (CLI --explain shows it)")
	}
	if _, ok := vixRow["vix_quality"]; !ok {
		t.Errorf("vix_term_structure.vix_quality missing (CLI --explain shows it)")
	}
	for _, key := range []string{"band", "band_reason", "thresholds", "as_of"} {
		if _, ok := vixRow[key]; !ok {
			t.Errorf("vix_term_structure.%s missing (agent compact metadata)", key)
		}
	}
}

func TestIbkrCanaryResponseHasSignalsAndFingerprints(t *testing.T) {
	t.Parallel()
	res := rpc.CanaryResult{
		AsOf:        time.Date(2026, 5, 31, 8, 45, 0, 0, time.UTC),
		Fingerprint: rpc.Fingerprint{Version: rpc.CanaryFingerprintVersion, Key: "sha256:canary"},
		SourceFingerprints: rpc.CanarySourceFingerprints{
			Account:   &rpc.Fingerprint{Version: rpc.AccountFingerprintVersion, Key: "sha256:account"},
			Positions: &rpc.Fingerprint{Version: rpc.PositionsFingerprintVersion, Key: "sha256:positions"},
			Regime:    &rpc.Fingerprint{Version: rpc.RegimeFingerprintVersion, Key: "sha256:regime"},
		},
		SourceHealth: []rpc.SourceHealth{
			{Source: "account", Status: "ok", Confidence: "high", FingerprintStability: rpc.FingerprintStabilitySemanticBuckets},
			{Source: "positions", Status: "ok", Confidence: "high", FingerprintStability: rpc.FingerprintStabilitySemanticBuckets},
			{Source: "regime", Status: "stale", Confidence: "medium-low", FingerprintStability: rpc.FingerprintStabilitySemanticBuckets},
		},
		Policy:             "canary-default",
		Action:             "watch",
		MarketConfirmation: "partial",
		PortfolioFit:       "high",
		InputHealth:        "ok",
		Direction:          risk.DirectionDefensive,
		Severity:           risk.SeverityWatch,
		PlannerModeHint:    risk.PlannerModeStage,
		PlannerReadiness:   risk.PlannerReadinessPrestage,
		Summary:            "Freeze new risk.",
		PrimaryDrivers:     []risk.SignalID{risk.SignalMarginCushionLow},
		Signals:            []risk.Signal{{ID: risk.SignalMarginCushionLow, Direction: risk.DirectionDefensive, Posture: risk.PortfolioPostureThreat, Severity: risk.SeverityWatch}},
		Rows:               []rpc.CanaryRow{{Title: "Portfolio canary", Direction: risk.DirectionDefensive, Severity: risk.SeverityWatch, Guidance: "Freeze new risk."}},
		Portfolio:          rpc.CanaryPortfolioSummary{BaseCurrency: "USD", NetLiquidation: 100_000},
		Market:             rpc.CanaryMarketSummary{RegimeVerdict: "Normal regime", RankedClusters: 6},
		Warnings:           []string{"stale clusters: vol"},
		NotExecution:       "Read-only recommendation; no orders are placed by ibkr.",
	}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	for _, key := range []string{
		"fingerprint",
		"source_fingerprints",
		"source_health",
		"policy",
		"action",
		"market_confirmation",
		"portfolio_fit",
		"input_health",
		"direction",
		"severity",
		"planner_mode_hint",
		"planner_readiness",
		"primary_drivers",
		"signals",
		"portfolio",
		"market",
		"warnings",
		"not_execution",
	} {
		if _, ok := wire[key]; !ok {
			t.Errorf("canary JSON missing %s: %s", key, b)
		}
	}
	fp, ok := wire["fingerprint"].(map[string]any)
	if !ok || fp["version"] != rpc.CanaryFingerprintVersion || fp["key"] == "" {
		t.Fatalf("fingerprint missing or malformed: %#v", wire["fingerprint"])
	}
	if wire["action"] != "watch" {
		t.Fatalf("action = %#v, want watch", wire["action"])
	}
	sources, ok := wire["source_fingerprints"].(map[string]any)
	if !ok {
		t.Fatalf("source_fingerprints missing: %s", b)
	}
	regime, ok := sources["regime"].(map[string]any)
	if !ok || regime["version"] != rpc.RegimeFingerprintVersion || regime["key"] == "" {
		t.Fatalf("source_fingerprints.regime missing or malformed: %#v", sources["regime"])
	}
	for _, key := range []string{"account", "positions"} {
		source, ok := sources[key].(map[string]any)
		if !ok || source["key"] == "" {
			t.Fatalf("source_fingerprints.%s missing or malformed: %#v", key, sources[key])
		}
	}
}
