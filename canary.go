// Package canary is the Go client for a running Canary daemon.
//
// It executes the tool catalogue that canary mcp serves, over the daemon's
// Unix socket and without an MCP process in between, for programs that read
// Canary mechanically rather than through a model. Capability to call a tool
// is not authority: the daemon keeps every account, mode, policy and broker
// gate, and this package restates none of them.
//
// A Client opens one connection per call because the daemon answers one
// request at a time per connection. Concurrent calls therefore never queue
// behind each other, and a Client holds nothing between calls.
package canary

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/mcp"
	"github.com/osauer/canary/v2/internal/rpc"
)

// Options configures a Client. The zero value selects the canonical socket
// and never starts a daemon.
type Options struct {
	// SocketPath is the daemon's Unix socket. Empty selects the canonical
	// path, which honours CANARY_SOCKET.
	SocketPath string
	// Executable is the canary binary whose daemon is started when the
	// socket is absent. Empty disables that start; Call, Subscribe and
	// Version then report ErrDaemonUnavailable until a daemon serves the
	// socket.
	Executable string
}

// Client executes catalogue tools and display subscriptions against one
// daemon socket. It is safe for concurrent use and needs no Close.
type Client struct {
	socket     string
	executable string
	spawn      chan struct{}
}

// New returns a Client for o. It performs no I/O.
func New(o Options) *Client {
	socket := strings.TrimSpace(o.SocketPath)
	if socket == "" {
		socket = dial.DefaultSocketPath()
	}
	return &Client{socket: socket, executable: strings.TrimSpace(o.Executable), spawn: make(chan struct{}, 1)}
}

// SocketPath reports the daemon socket the Client dials.
func (c *Client) SocketPath() string { return c.socket }

// Call executes the catalogue tool name with JSON args and returns the tool's
// JSON result, the same bytes canary mcp places in its text content block.
// Empty args mean an empty object. The call ends at ctx's deadline or at the
// tool's catalogue budget, whichever comes first, and such an end reports
// context.DeadlineExceeded. A failure the daemon reported is an *Error.
func (c *Client) Call(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	tool, ok := mcp.Lookup(name)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownTool, name)
	}
	if len(bytes.TrimSpace(args)) == 0 {
		args = json.RawMessage(`{}`)
	}
	if budget := mcp.ToolCallTimeout(name, args); budget > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, budget)
		defer cancel()
	}
	conn, err := c.connect(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	defer conn.Close()
	out, err := tool.Handler(ctx, conn, args)
	if err != nil {
		return nil, failure(ctx, name, err)
	}
	return out, nil
}

// Subscribe streams display snapshots until ctx ends, the daemon closes the
// stream, or onFrame returns an error. Each frame holds one snapshot's JSON,
// byte for byte what canary market --watch prints per line. Subscribe
// returns nil when the daemon ends the stream, ctx.Err() when ctx ends,
// onFrame's error unchanged, and an *Error when the daemon refuses the
// subscription. The daemon serves at most four display readers.
func (c *Client) Subscribe(ctx context.Context, onFrame func(json.RawMessage) error) error {
	if onFrame == nil {
		return errors.New("canary: Subscribe needs an onFrame callback")
	}
	conn, err := c.connect(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return daemonError(conn.Stream(ctx, rpc.MethodDisplaySubscribe, nil, onFrame))
}

// Version returns the running daemon's stamped version.
func (c *Client) Version(ctx context.Context) (string, error) {
	conn, err := c.connect(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	version, err := conn.DaemonVersion(ctx)
	return version, daemonError(err)
}

// connect opens one daemon connection, starting the daemon from the
// configured executable when the socket is absent. Starts are serialised so
// that later callers wait for the first attempt and then find the socket
// instead of racing to spawn a second daemon.
func (c *Client) connect(ctx context.Context) (*dial.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	conn, err := dial.Connect(c.socket)
	if err == nil {
		return conn, nil
	}
	if !errors.Is(err, dial.ErrSocketMissing) {
		return nil, err
	}
	if c.executable == "" {
		return nil, fmt.Errorf("%w: no daemon serves %s", ErrDaemonUnavailable, dial.DisplayPath(c.socket))
	}
	select {
	case c.spawn <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-c.spawn }()
	if conn, err := dial.Connect(c.socket); err == nil {
		return conn, nil
	}
	conn, err = dial.AutospawnAndConnectContextFromExecutable(ctx, c.socket, c.executable)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDaemonUnavailable, err)
	}
	return conn, nil
}

// Error is a failure the daemon reported. Code is one of the Code constants.
type Error struct {
	// Code classifies the failure.
	Code string
	// Message is the daemon's explanation.
	Message string
}

// Error renders the code and message.
func (e *Error) Error() string { return e.Code + ": " + e.Message }

// ErrUnknownTool reports a tool name outside the catalogue.
var ErrUnknownTool = errors.New("canary: unknown tool")

// ErrDaemonUnavailable reports that no daemon serves the socket and none
// could be started.
var ErrDaemonUnavailable = errors.New("canary: daemon unavailable")

// Daemon failure codes carried by Error.Code.
const (
	// CodeUnknownMethod means the daemon does not serve the requested method.
	CodeUnknownMethod = rpc.CodeUnknownMethod
	// CodeBadRequest means the daemon rejected the arguments.
	CodeBadRequest = rpc.CodeBadRequest
	// CodeGatewayUnavailable means the broker gateway is not connected.
	CodeGatewayUnavailable = rpc.CodeGatewayUnavailable
	// CodeSymbolInactive means the requested instrument is not active or the
	// broker has no definition for it.
	CodeSymbolInactive = rpc.CodeSymbolInactive
	// CodeTimeout means the daemon gave up waiting on the broker.
	CodeTimeout = rpc.CodeTimeout
	// CodeTradingDisabled means the daemon was built without trading.
	CodeTradingDisabled = rpc.CodeTradingDisabled
	// CodeInternal means the daemon failed for a reason it does not classify.
	CodeInternal = rpc.CodeInternal
	// CodeRegimeUnavailable means no validated regime snapshot exists.
	CodeRegimeUnavailable = rpc.CodeRegimeUnavailable
)

// daemonError converts a daemon-reported failure into *Error and leaves
// every other error, including nil, unchanged.
func daemonError(err error) error {
	if reported, ok := errors.AsType[*rpc.Error](err); ok {
		return &Error{Code: reported.Code, Message: reported.Message}
	}
	return err
}

// failure names the tool on a Call error. A socket deadline mirrors ctx, so
// when ctx has ended the bound is reported rather than the transport error.
func failure(ctx context.Context, name string, err error) error {
	if reported, ok := errors.AsType[*rpc.Error](err); ok {
		return fmt.Errorf("%s: %w", name, &Error{Code: reported.Code, Message: reported.Message})
	}
	if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
		return fmt.Errorf("%s: %w: %v", name, ctxErr, err)
	}
	return fmt.Errorf("%s: %w", name, err)
}

// Tool describes one catalogue entry as canary mcp advertises it.
type Tool struct {
	// Name is the tool name Call accepts.
	Name string
	// Title is the short human title.
	Title string
	// Description is the model-facing description.
	Description string
	// InputSchema is the JSON Schema for Call's args.
	InputSchema json.RawMessage
	// ReadOnly reports the catalogue's read-only hint. Every catalogue tool
	// is read-only today; the field keeps a future write tool from passing
	// as a read.
	ReadOnly bool
	// Methods lists the daemon methods the tool may invoke.
	Methods []string
}

// Tools returns the full catalogue in catalogue order. The result is a copy.
func Tools() []Tool {
	tools := make([]Tool, 0, len(mcp.Tools))
	for _, t := range mcp.Tools {
		tools = append(tools, describe(t))
	}
	return tools
}

// Lookup returns the catalogue entry for name.
func Lookup(name string) (Tool, bool) {
	t, ok := mcp.Lookup(name)
	if !ok {
		return Tool{}, false
	}
	return describe(t), true
}

func describe(t mcp.Tool) Tool {
	return Tool{
		Name:        t.Name,
		Title:       t.Title,
		Description: t.Description,
		InputSchema: slices.Clone(t.JSONSchema),
		ReadOnly:    t.ReadOnlyHint == nil || *t.ReadOnlyHint,
		Methods:     slices.Clone(t.RPCMethods),
	}
}
