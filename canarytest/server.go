// Package canarytest runs a fake Canary daemon for tests of programs built on
// the canary client. The fake speaks the daemon's real wire types on a fresh
// Unix socket, answers one request at a time per connection like the daemon,
// and records the methods it served.
package canarytest

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

// Handler answers one unary daemon method with a JSON result. An error made
// by Fail reports its code; any other error reports the internal code.
type Handler func(ctx context.Context, params json.RawMessage) (json.RawMessage, error)

// StreamHandler answers a streaming method. Each emit sends one frame. When
// the handler returns, the stream ends and the connection closes, as on the
// daemon; a returned error is reported instead of the end frame. ctx ends
// when the client disconnects or the server closes.
type StreamHandler func(ctx context.Context, params json.RawMessage, emit func(json.RawMessage) error) error

// Server is a fake daemon. Handlers may be registered or replaced at any
// time, including while connections are open.
type Server struct {
	path     string
	listener net.Listener
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup

	mu       sync.Mutex
	handlers map[string]Handler
	streams  map[string]StreamHandler
	calls    []string
}

// Serve starts a fake daemon on a fresh socket and stops it when the test
// ends. The socket lives under /tmp because macOS bounds socket paths to 104
// bytes and a test's temporary directory is often longer.
func Serve(t testing.TB) *Server {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "canarytest-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "d.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{path: path, listener: listener, ctx: ctx, cancel: cancel, handlers: map[string]Handler{}, streams: map[string]StreamHandler{}}
	s.wg.Go(s.accept)
	t.Cleanup(s.Close)
	return s
}

// SocketPath is the socket a client should dial.
func (s *Server) SocketPath() string { return s.path }

// Handle answers method with h. A nil h removes the handler.
func (s *Server) Handle(method string, h Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h == nil {
		delete(s.handlers, method)
		return
	}
	s.handlers[method] = h
}

// HandleStream answers the streaming method with h. A nil h removes it.
func (s *Server) HandleStream(method string, h StreamHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h == nil {
		delete(s.streams, method)
		return
	}
	s.streams[method] = h
}

// Calls returns every method served so far, in arrival order.
func (s *Server) Calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// Close stops serving, ends every handler's context and waits for them.
func (s *Server) Close() {
	s.cancel()
	_ = s.listener.Close()
	s.wg.Wait()
}

// Result returns a Handler that answers every call with v marshalled as JSON.
func Result(v any) Handler {
	return func(context.Context, json.RawMessage) (json.RawMessage, error) { return json.Marshal(v) }
}

// Fail returns an error a Handler or StreamHandler can return to report code
// and message the way the daemon does.
func Fail(code, message string) error { return &rpc.Error{Code: code, Message: message} }

func (s *Server) accept() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.wg.Go(func() { s.serve(conn) })
	}
}

func (s *Server) serve(conn net.Conn) {
	defer conn.Close()
	stop := context.AfterFunc(s.ctx, func() { _ = conn.Close() })
	defer stop()
	reader := bufio.NewReaderSize(conn, 64<<10)
	encoder := json.NewEncoder(conn)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		var req rpc.Request
		if err := json.Unmarshal(line, &req); err != nil {
			_ = encoder.Encode(rpc.Response{ID: req.ID, Error: &rpc.Error{Code: rpc.CodeBadRequest, Message: err.Error()}})
			return
		}
		s.record(req.Method)
		if stream := s.stream(req.Method); stream != nil {
			s.serveStream(conn, reader, encoder, req, stream)
			return
		}
		handler := s.handler(req.Method)
		if handler == nil {
			_ = encoder.Encode(rpc.Response{ID: req.ID, Error: &rpc.Error{Code: rpc.CodeUnknownMethod, Message: "unknown method: " + req.Method}})
			continue
		}
		result, err := handler(s.ctx, req.Params)
		if err != nil {
			_ = encoder.Encode(rpc.Response{ID: req.ID, Error: reported(err)})
			continue
		}
		_ = encoder.Encode(rpc.Response{ID: req.ID, Ok: true, Result: result})
	}
}

// serveStream runs one connection-terminal stream. A client that hangs up
// ends the handler's context, as the daemon's EOF watcher does.
func (s *Server) serveStream(conn net.Conn, reader *bufio.Reader, encoder *json.Encoder, req rpc.Request, stream StreamHandler) {
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	go func() {
		_, _ = reader.ReadByte()
		cancel()
	}()
	emit := func(frame json.RawMessage) error {
		return encoder.Encode(rpc.Response{ID: req.ID, Ok: true, Stream: true, Frame: frame})
	}
	if err := stream(ctx, req.Params, emit); err != nil {
		_ = encoder.Encode(rpc.Response{ID: req.ID, Error: reported(err)})
		return
	}
	_ = encoder.Encode(rpc.Response{ID: req.ID, Ok: true, End: true})
	_ = conn.Close()
}

func (s *Server) record(method string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, method)
}

func (s *Server) handler(method string) Handler {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.handlers[method]
}

func (s *Server) stream(method string) StreamHandler {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streams[method]
}

func reported(err error) *rpc.Error {
	if e, ok := errors.AsType[*rpc.Error](err); ok {
		return e
	}
	return &rpc.Error{Code: rpc.CodeInternal, Message: err.Error()}
}
