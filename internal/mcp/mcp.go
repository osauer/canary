// Package mcp adapts the daemon's read, research, and preview capabilities to
// the Model Context Protocol over stdio. Its curated tools and resources use
// short-lived typed daemon calls where required; they do not expose broker
// place, modify, cancel, exercise, purge, or restore operations.
//
// Wire: newline-delimited JSON-RPC 2.0 over stdin/stdout, no framing headers.
// The MCP lifecycle is initialize → initialized (notification) → repeated
// tools/list + tools/call → EOF on stdin shuts the server down.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/rpc"
)

// ProtocolVersion is the MCP spec revision we advertise. 2025-03-26 is the
// stable revision Claude Desktop and the official Go/TypeScript SDKs target.
const ProtocolVersion = "2025-03-26"

// Server hosts the MCP loop. One per process. Tool calls and streaming
// resource subscriptions open additional daemon connections via dialer when
// available, so per-call timeouts cannot leave late daemon replies queued on
// the shared control connection. Without a dialer, tools fall back to conn.
type Server struct {
	conn    *dial.Conn
	version string
	profile Profile

	mu  sync.Mutex // serializes writes to out
	out *bufio.Writer

	// dialer is the function used to open daemon connections for tools and
	// resources. Set via SetDialer or SetContextDialer; nil means operations
	// requiring the daemon fall back to conn when present, otherwise return an
	// internal-error response.
	dialer func(context.Context) (*dial.Conn, error)

	// subs tracks active resource subscriptions, keyed by URI string. The
	// CancelFunc tears down the per-subscription goroutine and the
	// underlying daemon conn. Guarded by subMu.
	subMu sync.Mutex
	subs  map[string]context.CancelFunc

	// serveCtx is the parent context for streaming resource subscriptions.
	// Set at the top of Serve() so subscriptions are children of the
	// server's lifecycle, not context.Background() — when Serve returns
	// (client EOF, ctx cancel) all in-flight subscription goroutines see
	// the cancel and unwind. shutdownSubscriptions still nudges them on
	// the way out for prompt teardown of the daemon-side refcount.
	serveCtx context.Context
}

// NewServer wires the MCP server to an optional daemon connection and the
// version string the binary was built with (stamped via -ldflags). Production
// stdio uses SetContextDialer instead of a long-lived conn so an idle MCP
// process does not keep the daemon alive.
func NewServer(conn *dial.Conn, version string) *Server {
	return &Server{
		conn:    conn,
		version: version,
		profile: ProfileFull,
		subs:    map[string]context.CancelFunc{},
	}
}

// SetProfile selects the tool and resource profile exposed by the server.
// An empty profile selects [ProfileFull].
func (s *Server) SetProfile(profile Profile) {
	if profile == "" {
		profile = ProfileFull
	}
	s.profile = profile
}

// SetDialer wires the function used to open daemon connections for tools and
// resources. It is kept for tests and integrations that do not need
// context-aware dialing; production stdio should use SetContextDialer.
func (s *Server) SetDialer(d func() (*dial.Conn, error)) {
	if d == nil {
		s.dialer = nil
		return
	}
	s.dialer = func(context.Context) (*dial.Conn, error) {
		return d()
	}
}

// SetContextDialer wires the function used to open daemon connections with the
// current request/server context. Prefer this in production paths so shutdown
// can abort autospawn waits promptly.
func (s *Server) SetContextDialer(d func(context.Context) (*dial.Conn, error)) {
	s.dialer = d
}

// ServeOptions controls optional lifecycle guards for the stdio server.
type ServeOptions struct {
	// IdleTimeout exits the server after this much time without an MCP
	// request. Active resource subscriptions suppress the timeout because
	// the server is still doing useful work even if stdin is quiet.
	IdleTimeout time.Duration
}

// Serve runs the MCP loop until in returns io.EOF (client disconnect), ctx is
// cancelled, or the client sends the MCP shutdown/exit lifecycle. Returns nil
// on clean shutdown. Active resource subscriptions are cancelled before return
// so per-sub goroutines unwind and the daemon-side refcount decrements promptly.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	return s.ServeWithOptions(ctx, in, out, ServeOptions{})
}

// ServeWithOptions is Serve with explicit lifecycle controls for production
// stdio hosts. Tests and in-process integrations usually call Serve directly.
//
// The scan loop runs in a goroutine because bufScan.Scan() is a blocking
// read on stdin and on darwin os.Stdin uses a blocking syscall.read (not
// the runtime poller), so closing the fd from another goroutine doesn't
// unblock the read. We instead drive the main loop off the scanned-line
// channel and ctx.Done(), and on cancel we return immediately — the
// reader goroutine stays parked on the kernel read and is reaped when the
// process exits. This keeps context and signal cancellation authoritative even
// when stdin itself cannot be interrupted.
func (s *Server) ServeWithOptions(ctx context.Context, in io.Reader, out io.Writer, opts ServeOptions) error {
	s.out = bufio.NewWriter(out)
	s.serveCtx = ctx
	defer s.out.Flush()
	defer s.shutdownSubscriptions()

	reader := bufio.NewReader(in)
	// Generous line buffer — MCP messages can include large tool results.
	// 4 MB matches the daemon's per-request payload ceiling.
	bufScan := bufio.NewScanner(reader)
	bufScan.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	lines := make(chan []byte)
	scanErr := make(chan error, 1)
	go func() {
		defer close(lines)
		for bufScan.Scan() {
			// Copy because bufScan.Bytes() is reused on the next Scan.
			b := bufScan.Bytes()
			cp := make([]byte, len(b))
			copy(cp, b)
			select {
			case lines <- cp:
			case <-ctx.Done():
				return
			}
		}
		if err := bufScan.Err(); err != nil {
			scanErr <- err
		}
	}()

	var idleTimer *time.Timer
	var idle <-chan time.Time
	stopIdleTimer := func() {
		if idleTimer == nil {
			return
		}
		if !idleTimer.Stop() {
			select {
			case <-idleTimer.C:
			default:
			}
		}
		idle = nil
	}
	resetIdleTimer := func() {
		if opts.IdleTimeout <= 0 {
			return
		}
		stopIdleTimer()
		idleTimer = time.NewTimer(opts.IdleTimeout)
		idle = idleTimer.C
	}
	defer stopIdleTimer()
	resetIdleTimer()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-idle:
			if s.hasActiveSubscriptions() {
				resetIdleTimer()
				continue
			}
			return nil
		case line, ok := <-lines:
			if !ok {
				select {
				case err := <-scanErr:
					if !errors.Is(err, io.EOF) {
						return err
					}
				default:
				}
				return nil
			}
			if len(line) == 0 {
				continue
			}
			// Each request is handled inline. Tools call the daemon, which may
			// take seconds; that's fine — MCP clients send one request at a
			// time over stdio and wait for the response.
			if s.handle(ctx, line) {
				return nil
			}
			resetIdleTimer()
		}
	}
}

// rpcRequest is the JSON-RPC 2.0 envelope MCP layers on top of.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // null/missing for notifications
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// JSON-RPC error codes used by the MCP server. The MCP spec inherits the
// JSON-RPC 2.0 error model verbatim.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// handle dispatches one MCP JSON-RPC message. It returns true when the
// protocol lifecycle has ended and Serve should exit after this message.
func (s *Server) handle(ctx context.Context, line []byte) bool {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		s.writeError(nil, codeParseError, err.Error())
		return false
	}
	if req.JSONRPC != "2.0" {
		s.writeError(req.ID, codeInvalidRequest, "jsonrpc must be \"2.0\"")
		return false
	}

	// Notifications carry no id and expect no response.
	isNotification := len(req.ID) == 0 || string(req.ID) == "null"

	switch req.Method {
	case "initialize":
		s.handleInitialize(req.ID, req.Params)
	case "initialized", "notifications/initialized":
		// Client confirms readiness; no response required.
		return false
	case "tools/list":
		s.handleToolsList(req.ID)
	case "tools/call":
		s.handleToolsCall(ctx, req.ID, req.Params)
	case "resources/list":
		s.handleResourcesList(req.ID)
	case "resources/templates/list":
		s.handleResourcesTemplatesList(req.ID)
	case "resources/read":
		s.handleResourcesRead(ctx, req.ID, req.Params)
	case "resources/subscribe":
		s.handleResourcesSubscribe(ctx, req.ID, req.Params)
	case "resources/unsubscribe":
		s.handleResourcesUnsubscribe(req.ID, req.Params)
	case "ping":
		s.writeResult(req.ID, json.RawMessage(`{}`))
	case "shutdown":
		if !isNotification {
			s.writeResult(req.ID, json.RawMessage(`{}`))
		}
		return true
	case "exit":
		return true
	default:
		if !isNotification {
			s.writeError(req.ID, codeMethodNotFound, "method not found: "+req.Method)
		}
	}
	return false
}

// initializeResult is the MCP server-info payload. Capabilities advertise the
// tools surface; we don't currently expose resources or prompts.
type initializeResult struct {
	ProtocolVersion string            `json:"protocolVersion"`
	Capabilities    map[string]any    `json:"capabilities"`
	ServerInfo      initializeSrvInfo `json:"serverInfo"`
	Instructions    string            `json:"instructions,omitempty"`
}

type initializeSrvInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

func (s *Server) handleInitialize(id, _ json.RawMessage) {
	caps := map[string]any{
		"tools":     map[string]any{"listChanged": false},
		"resources": map[string]any{"subscribe": s.dialer != nil, "listChanged": false},
	}
	res := initializeResult{
		ProtocolVersion: ProtocolVersion,
		Capabilities:    caps,
		ServerInfo: initializeSrvInfo{
			Name:    "canary",
			Version: s.version,
		},
		Instructions: s.instructions(),
	}
	b, _ := json.Marshal(res)
	s.writeResult(id, b)
}

func (s *Server) instructions() string {
	if s.profile == ProfileMonitor {
		return "Read-only Canary monitor profile for Interactive Brokers. Use `canary_stress` first for portfolio risk posture; call `canary_status` only for connectivity or degraded-input troubleshooting."
	}
	return "Canary tools and resources for Interactive Brokers. Most tools are read-only: account, positions, snapshot quotes, option chains, daily history, technical/relative-strength screens, market scans, fixed-fractional position sizing, S&P 500 breadth (50-/200-DMA, new highs/new lows), SPX-canonical dealer zero-gamma with SPY context, a broad-market stress-lifecycle regime dashboard, a stateless portfolio stress read, and daemon-owned protection proposal snapshots. Order-status/open-order tools are read-only journal views. The order-preview tool can mint a local preview token and reports submit_eligible separately, but cannot place, modify, cancel, or transmit a broker order. Resources expose live streaming quotes via subscribe (URI template: canary://quote/{symbol})."
}

// toolDescriptor is the wire shape MCP expects in tools/list.
type toolDescriptor struct {
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Annotations toolAnnotations `json:"annotations"`
}

type toolAnnotations struct {
	Title        string `json:"title,omitempty"`
	ReadOnlyHint bool   `json:"readOnlyHint"`
}

func (s *Server) handleToolsList(id json.RawMessage) {
	tools := s.visibleTools()
	descs := make([]toolDescriptor, 0, len(tools))
	for _, t := range tools {
		desc := t.Description
		if s.profile == ProfileMonitor && strings.TrimSpace(t.MonitorDescription) != "" {
			desc = t.MonitorDescription
		}
		readOnly := true
		if t.ReadOnlyHint != nil {
			readOnly = *t.ReadOnlyHint
		}
		descs = append(descs, toolDescriptor{
			Name:        t.Name,
			Title:       t.Title,
			Description: desc,
			InputSchema: t.JSONSchema,
			Annotations: toolAnnotations{
				Title:        t.Title,
				ReadOnlyHint: readOnly,
			},
		})
	}
	b, _ := json.Marshal(map[string]any{"tools": descs})
	s.writeResult(id, b)
}

// callParams is the input to tools/call. We accept missing arguments as an
// empty object; some clients omit the field for zero-arg tools.
type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// toolResultPayload mirrors the MCP tools/call response. Content is always a
// single text block carrying the daemon's JSON, stringified. IsError flags
// daemon/RPC errors so the LLM can distinguish them from on-the-wire success
// with empty payloads.
type toolResultPayload struct {
	Content []contentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (s *Server) handleToolsCall(ctx context.Context, id, params json.RawMessage) {
	var p callParams
	if err := json.Unmarshal(params, &p); err != nil {
		s.writeError(id, codeInvalidParams, err.Error())
		return
	}
	tool, ok := s.lookupVisibleTool(p.Name)
	if !ok {
		if _, exists := lookupTool(p.Name); exists {
			s.writeError(id, codeMethodNotFound, "tool unavailable in mcp profile "+string(s.profile)+": "+p.Name)
			return
		}
		s.writeError(id, codeMethodNotFound, "unknown tool: "+p.Name)
		return
	}
	timeout := mcpToolCallTimeout(p.Name, p.Arguments)
	conn, closeConn, err := s.dialForRequest(ctx)
	if err != nil {
		s.writeToolError(id, err)
		return
	}
	defer closeConn()
	callCtx := ctx
	var cancel context.CancelFunc
	if timeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	out, err := tool.Handler(callCtx, conn, p.Arguments)
	if err != nil {
		if toolCallTimedOut(callCtx, err) && timeout > 0 {
			err = fmt.Errorf("%s timed out after %s", p.Name, timeout)
		}
		s.writeToolError(id, err)
		return
	}
	payload := toolResultPayload{
		Content: []contentBlock{{Type: "text", Text: string(out)}},
	}
	b, _ := json.Marshal(payload)
	s.writeResult(id, b)
}

// dialForRequest opens the daemon connection a tool call or resource read
// needs, under the daemon's own readiness budget instead of the caller's
// per-call budget. A daemon that is starting verifies its database before it
// publishes the socket, and that verification scales with the file, so
// charging the wait to a 2-second tool budget reports a healthy boot as a
// tool timeout. dial.StartupBudget tracks the same file, and autospawn
// enforces it internally, so this deadline is a backstop for the paths that
// never reach autospawn; ctx still aborts the wait when the MCP host exits.
func (s *Server) dialForRequest(ctx context.Context) (*dial.Conn, func(), error) {
	budget := dial.StartupBudget()
	dialCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	conn, closeConn, err := s.toolConn(dialCtx)
	if err != nil && errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return nil, closeConn, fmt.Errorf("the daemon did not start serving within %s; it verifies its database before it accepts connections, so a large one takes longer — check the daemon log", budget)
	}
	return conn, closeConn, err
}

func (s *Server) toolConn(ctx context.Context) (*dial.Conn, func(), error) {
	if s.dialer != nil {
		conn, err := s.dial(ctx)
		if err != nil {
			return nil, func() {}, err
		}
		return conn, func() { _ = conn.Close() }, nil
	}
	if s.conn == nil {
		return nil, func() {}, errors.New("daemon connection required")
	}
	return s.conn, func() {}, nil
}

func (s *Server) dial(ctx context.Context) (*dial.Conn, error) {
	if s.dialer == nil {
		return nil, errors.New("daemon connection required")
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	conn, err := s.dialer(ctx)
	if err != nil {
		return nil, err
	}
	if conn == nil {
		return nil, errors.New("daemon dialer returned nil connection")
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}
	return conn, nil
}

func toolCallTimedOut(ctx context.Context, err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func (s *Server) writeToolError(id json.RawMessage, err error) {
	// Tool-level errors land inside a non-error JSON-RPC response with
	// isError=true, per the MCP spec — clients distinguish protocol
	// errors (codeMethodNotFound, codeInvalidParams) from tool failures
	// (gateway down, symbol inactive, bounded timeout). Surfacing these
	// as JSON-RPC errors would mislead the LLM into thinking the protocol
	// broke.
	payload := toolResultPayload{
		IsError: true,
		Content: []contentBlock{{Type: "text", Text: err.Error()}},
	}
	b, _ := json.Marshal(payload)
	s.writeResult(id, b)
}

const (
	mcpFastToolHeadroom = 1 * time.Second
	mcpDefaultHeadroom  = 5 * time.Second
	mcpDefaultToolFloor = 35 * time.Second
	mcpLongToolFloor    = 60 * time.Second
	mcpScannerToolFloor = 90 * time.Second
	mcpWatchQuoteFloor  = 45 * time.Second
	mcpScanParamsFloor  = 20 * time.Second
)

func mcpToolCallTimeout(name string, args json.RawMessage) time.Duration {
	methods := mcpToolMethodsForCall(name, args)
	headroom := mcpDefaultHeadroom
	floor := mcpDefaultToolFloor
	switch name {
	case "canary_status", "canary_calendar", "canary_breadth":
		headroom = mcpFastToolHeadroom
		floor = 0
	case "canary_scan":
		if scanListModeArgs(args) {
			headroom = mcpFastToolHeadroom
			floor = 0
		} else {
			floor = mcpScannerToolFloor
		}
	case "canary_scan_params":
		floor = mcpScanParamsFloor
	case "canary_watch":
		if watchListOnlyArgs(args) {
			headroom = mcpFastToolHeadroom
			floor = 0
		} else {
			floor = mcpWatchQuoteFloor
		}
	case "canary_chain", "canary_gamma":
		floor = mcpLongToolFloor
	case "canary_technical":
		floor = mcpScannerToolFloor
	case "canary_stress":
		floor = mcpScannerToolFloor
	}
	return mcpMethodBudget(methods, headroom, floor)
}

func mcpMethodBudget(methods []string, headroom, floor time.Duration) time.Duration {
	budget := floor
	for _, method := range methods {
		timing, ok := rpc.LookupMethodTiming(method)
		if !ok {
			continue
		}
		if candidate := timing.ClientTimeout(headroom); candidate > budget {
			budget = candidate
		}
	}
	return budget
}

// mcpToolMethodsForCall narrows handlers with more than one possible daemon
// route to the route selected by this call. All possible methods remain
// declared on Tool.RPCMethods for parity checks.
func mcpToolMethodsForCall(name string, args json.RawMessage) []string {
	tool, ok := lookupTool(name)
	if !ok {
		return nil
	}
	switch name {
	case "canary_scan":
		if scanListModeArgs(args) {
			return []string{rpc.MethodScanList}
		}
		return []string{rpc.MethodScanRun}
	case "canary_chain":
		var in struct {
			Expiry string `json:"expiry"`
		}
		if len(args) > 0 {
			_ = json.Unmarshal(args, &in)
		}
		if in.Expiry == "" {
			return []string{rpc.MethodChainExpiries}
		}
		return []string{rpc.MethodChainFetch}
	case "canary_watch":
		if watchListOnlyArgs(args) {
			return []string{rpc.MethodStatusHealth}
		}
	case "canary_proposals":
		if refreshRequested(args) {
			return []string{rpc.MethodTradeProposalsRefresh}
		}
		return []string{rpc.MethodTradeProposalsSnapshot}
	case "canary_opportunities":
		if refreshRequested(args) {
			return []string{rpc.MethodOpportunitiesRefresh}
		}
		return []string{rpc.MethodOpportunitiesSnapshot}
	}
	return tool.RPCMethods
}

func refreshRequested(args json.RawMessage) bool {
	var in struct {
		Refresh bool `json:"refresh"`
	}
	if len(args) > 0 {
		_ = json.Unmarshal(args, &in)
	}
	return in.Refresh
}

func scanListModeArgs(args json.RawMessage) bool {
	var in struct {
		Preset   string `json:"preset"`
		Type     string `json:"type"`
		Exchange string `json:"exchange"`
	}
	if len(args) > 0 {
		_ = json.Unmarshal(args, &in)
	}
	return in.Preset == "" && in.Type == "" && in.Exchange == ""
}

func watchListOnlyArgs(args json.RawMessage) bool {
	var in struct {
		IncludeQuotes *bool `json:"include_quotes"`
	}
	if len(args) > 0 {
		_ = json.Unmarshal(args, &in)
	}
	return in.IncludeQuotes != nil && !*in.IncludeQuotes
}

func lookupTool(name string) (Tool, bool) {
	for _, t := range Tools {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

func (s *Server) visibleTools() []Tool {
	if s.profile != ProfileMonitor {
		return Tools
	}
	names := []string{"canary_stress", "canary_status"}
	out := make([]Tool, 0, len(names))
	for _, name := range names {
		if tool, ok := lookupTool(name); ok {
			out = append(out, tool)
		}
	}
	return out
}

func (s *Server) lookupVisibleTool(name string) (Tool, bool) {
	for _, t := range s.visibleTools() {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

func (s *Server) writeResult(id, result json.RawMessage) {
	if len(id) == 0 {
		id = json.RawMessage(`null`)
	}
	resp := rpcResponse{JSONRPC: "2.0", ID: id, Result: result}
	s.write(resp)
}

func (s *Server) writeError(id json.RawMessage, code int, msg string) {
	if len(id) == 0 {
		id = json.RawMessage(`null`)
	}
	resp := rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
	s.write(resp)
}

func (s *Server) write(resp rpcResponse) {
	b, err := json.Marshal(resp)
	if err != nil {
		// json.Marshal of a fixed struct only fails on cycles — none here.
		// Fall back to a minimal in-band error so the client doesn't hang.
		b = fmt.Appendf(nil, `{"jsonrpc":"2.0","id":null,"error":{"code":%d,"message":%q}}`, codeInternalError, err.Error())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.out.Write(b)
	_ = s.out.WriteByte('\n')
	_ = s.out.Flush()
}
