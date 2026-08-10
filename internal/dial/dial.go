// Package dial connects local adapters to the daemon's typed, newline-delimited
package dial

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/osauer/canary/v2/internal/productidentity"
	"github.com/osauer/canary/v2/internal/rpc"
)

// ErrSocketMissing indicates the daemon is not reachable: either the socket
var ErrSocketMissing = errors.New("daemon socket missing")

// DefaultSocketPath returns the canonical socket location.
func DefaultSocketPath() string {
	// docgen:env CANARY_SOCKET | Override the daemon IPC socket path. Defaults to `$XDG_RUNTIME_DIR/ibkr/ibkr.sock` or `$HOME/.cache/ibkr/ibkr.sock`.
	if v := os.Getenv("CANARY_SOCKET"); v != "" {
		return v
	}
	if v := os.Getenv("XDG_RUNTIME_DIR"); v != "" {
		return filepath.Join(v, productidentity.PersistentNamespace, productidentity.DaemonSocketName)
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", productidentity.PersistentNamespace, productidentity.DaemonSocketName)
}

// SocketPathOverridden reports whether CANARY_SOCKET points the CLI at a
// non-default daemon scope. Commands that manage system-wide state by
// process name (e.g. `canary restart`'s implicit app management) use this
// to stay hands-off: a process found by name cannot be attributed to the
// overridden scope, so signaling it would cross scopes.
func SocketPathOverridden() bool {
	// docgen:env CANARY_SOCKET | Override the daemon IPC socket path. Defaults to `$XDG_RUNTIME_DIR/ibkr/ibkr.sock` or `$HOME/.cache/ibkr/ibkr.sock`.
	return os.Getenv("CANARY_SOCKET") != ""
}

// DefaultAuthorityPath returns the canonical daemon authority database
func DefaultAuthorityPath() (string, error) {
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return filepath.Join(v, productidentity.PersistentNamespace, "daemon.db"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, ".local", "state", productidentity.PersistentNamespace, "daemon.db"), nil
}

// DefaultLogPath returns the canonical daemon log location. It reads
// XDG_STATE_HOME the way DefaultAuthorityPath does, so a desk that moves its
// state directory keeps the log beside the database it describes.
func DefaultLogPath() string {
	// docgen:env CANARY_LOG | Override the daemon log file path. Defaults to `$XDG_STATE_HOME/ibkr/ibkr-daemon.log` or `$HOME/.local/state/ibkr/ibkr-daemon.log`.
	if v := os.Getenv("CANARY_LOG"); v != "" {
		return v
	}
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return filepath.Join(v, productidentity.PersistentNamespace, "ibkr-daemon.log")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", productidentity.PersistentNamespace, "ibkr-daemon.log")
}

// DisplayPath renders p for a human-facing hint, abbreviating the home
// directory to ~. Hints name the path Canary will actually use; spelling the
// home directory out puts the account name into terminal output and
// screenshots without telling the reader anything.
func DisplayPath(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	rest, ok := strings.CutPrefix(p, home+string(os.PathSeparator))
	if !ok {
		return p
	}
	return "~" + string(os.PathSeparator) + rest
}

// Conn is a single client connection over the Unix socket.
type Conn struct {
	c   net.Conn
	mu  sync.Mutex
	r   *bufio.Reader
	enc *json.Encoder
}

// Connect opens the socket. Returns ErrSocketMissing if path doesn't exist
func Connect(path string) (*Conn, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrSocketMissing
		}
		return nil, err
	}
	c, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) {
			return nil, ErrSocketMissing
		}
		return nil, err
	}
	return &Conn{
		c:   c,
		r:   bufio.NewReaderSize(c, 64<<10),
		enc: json.NewEncoder(c),
	}, nil
}

// DaemonVersion runs a one-shot status.health call against the open Conn and
// returns the daemon's stamped version string. Short timeout so a wedged
// daemon doesn't delay the user's actual command — the caller (typically
// main.go) emits a non-fatal warning on mismatch, not an error.
//
// Defined here rather than in main.go so internal/mcp can run the same
// check at boot if it ever wants to.
func (c *Conn) DaemonVersion(ctx context.Context) (string, error) {
	var h struct {
		DaemonVersion string `json:"daemon_version"`
	}
	if err := c.Call(ctx, rpc.MethodStatusHealth, nil, &h); err != nil {
		return "", err
	}
	return h.DaemonVersion, nil
}

// Close releases the socket.
func (c *Conn) Close() error {
	if c == nil {
		return nil
	}
	return c.c.Close()
}

// Call performs a unary request/response round trip and decodes result into
// out. ctx cancellation forces an immediate read deadline so the in-flight
// read returns and Call surfaces ctx.Err(), matching Stream cancellation.
//
// The socket deadline is cleared on return, success or failure, so a
// subsequent caller, including a long-lived stream, starts with fresh timing
// state rather than inheriting the unary call's deadline.
func (c *Conn) Call(ctx context.Context, method string, params any, out any) error {
	req, err := newRequest(method, params)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	defer func() { _ = c.c.SetDeadline(time.Time{}) }()

	if err := c.applyDeadline(ctx); err != nil {
		return err
	}

	if err := c.enc.Encode(req); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("write request: %w", err)
	}

	defer c.installCancelWatcher(ctx)()

	line, err := c.r.ReadBytes('\n')
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("read response: %w", err)
	}
	var resp rpc.Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if !resp.Ok {
		if resp.Error != nil {
			return resp.Error
		}
		return errors.New("daemon returned !ok with no error payload")
	}
	if out != nil && len(resp.Result) > 0 {
		if err := json.Unmarshal(resp.Result, out); err != nil {
			return fmt.Errorf("decode result: %w", err)
		}
	}
	return nil
}

// Stream sends a subscribe-style request and invokes onFrame for each frame
func (c *Conn) Stream(ctx context.Context, method string, params any, onFrame func(json.RawMessage) error) error {
	req, err := newRequest(method, params)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.enc.Encode(req); err != nil {
		return fmt.Errorf("write request: %w", err)
	}

	// Cancellation forces an immediate read deadline so the read loop returns.
	defer c.installCancelWatcher(ctx)()

	for {
		line, err := c.r.ReadBytes('\n')
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return ctx.Err()
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return ctx.Err()
			}
			return err
		}
		var resp rpc.Response
		if err := json.Unmarshal(line, &resp); err != nil {
			return err
		}
		if !resp.Ok {
			if resp.Error != nil {
				return resp.Error
			}
			return errors.New("daemon returned !ok with no error payload")
		}
		if resp.End {
			return nil
		}
		if len(resp.Frame) > 0 && onFrame != nil {
			if err := onFrame(resp.Frame); err != nil {
				return err
			}
		}
	}
}

func newRequest(method string, params any) (*rpc.Request, error) {
	id, err := newID()
	if err != nil {
		return nil, err
	}
	r := &rpc.Request{ID: id, Method: method}
	if params != nil {
		buf, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		r.Params = buf
	}
	return r, nil
}

func newID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "r-" + hex.EncodeToString(b[:]), nil
}

func (c *Conn) applyDeadline(ctx context.Context) error {
	dl, ok := ctx.Deadline()
	if !ok {
		return c.c.SetDeadline(time.Time{})
	}
	return c.c.SetDeadline(dl)
}

// installCancelWatcher spawns a goroutine that, on ctx cancellation, forces
// cleanup function the caller must defer — defers don't compose with bare
func (c *Conn) installCancelWatcher(ctx context.Context) func() {
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = c.c.SetReadDeadline(time.Now())
		case <-stop:
		}
	}()
	return func() { close(stop) }
}
