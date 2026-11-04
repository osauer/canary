package futu

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/osauer/canary/v2/pkg/futu/internal/pb/notify"
	trdcommon "github.com/osauer/canary/v2/pkg/futu/internal/pb/trd_common"
)

// fixtureClock is the instant the default simulator book starts at, the time
// the keep-alive fixture was recorded with.
var fixtureClock = time.Date(2026, 11, 4, 1, 30, 0, 0, time.UTC)

type replayStep struct {
	out bool // the client wrote this frame
	raw []byte
}

func readFixture(t *testing.T, name string) []replayStep {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "frames", name+".hex"))
	if err != nil {
		t.Fatal(err)
	}
	var steps []replayStep
	for line := range strings.SplitSeq(string(raw), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		direction, encoded, ok := strings.Cut(line, " ")
		if !ok {
			t.Fatalf("malformed fixture line %q", line)
		}
		frame, err := hex.DecodeString(encoded)
		if err != nil {
			t.Fatal(err)
		}
		steps = append(steps, replayStep{out: direction == ">", raw: frame})
	}
	return steps
}

// replaySocket plays a fixture against the client over an in-memory socket: it
// asserts every frame the client writes is the fixture's byte for byte and
// answers with the fixture's replies.
func replaySocket(t *testing.T, steps []replayStep) (net.Conn, chan error) {
	t.Helper()
	client, server := net.Pipe()
	done := make(chan error, 1)
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	go func() {
		for _, step := range steps {
			if step.out {
				got := make([]byte, len(step.raw))
				if _, err := io.ReadFull(server, got); err != nil {
					done <- err
					return
				}
				if !bytes.Equal(got, step.raw) {
					done <- errors.New("the client wrote different bytes: " + hex.EncodeToString(got))
					return
				}
				continue
			}
			if _, err := server.Write(step.raw); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	return client, done
}

func replayConnection(t *testing.T, name string, onPush func(*connection, Frame)) (*connection, chan error) {
	t.Helper()
	nc, done := replaySocket(t, readFixture(t, name))
	conn := newConnection(nc, NewPacer(nil), nil)
	if onPush != nil {
		conn.onPush = func(f Frame) { onPush(conn, f) }
	}
	go conn.serve()
	return conn, done
}

func fixtureCfg() Config { return Config{ClientID: "canary-fixture", ClientVersion: 333} }

func TestReplayInitConnectOK(t *testing.T) {
	conn, done := replayConnection(t, "init_connect_ok", nil)
	info, err := handshake(context.Background(), conn, fixtureCfg())
	if err != nil {
		t.Fatal(err)
	}
	if info.ServerVersion != MinServerVersion || info.ConnID == 0 || info.KeepAliveInterval != 10*time.Second || info.LoginUserID == 0 {
		t.Fatalf("session info %+v", info)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestReplayInitConnectRSARequired(t *testing.T) {
	conn, done := replayConnection(t, "init_connect_rsa_required", nil)
	_, err := handshake(context.Background(), conn, fixtureCfg())
	if !errors.Is(err, ErrHandshakeRefused) {
		t.Fatalf("handshake: %v", err)
	}
	var reply *Error
	if !errors.As(err, &reply) || reply.RetType != RetTypeFailed {
		t.Fatalf("reply %+v", reply)
	}
	if !strings.Contains(err.Error(), "refused the connection") {
		t.Fatalf("text %q", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestReplayInitConnectOldServer(t *testing.T) {
	conn, done := replayConnection(t, "init_connect_old_server", nil)
	_, err := handshake(context.Background(), conn, fixtureCfg())
	if !errors.Is(err, ErrServerVersion) || !strings.Contains(err.Error(), "minimum") {
		t.Fatalf("handshake: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestReplayKeepAliveWritesTheFixtureBytes(t *testing.T) {
	conn, done := replayConnection(t, "keep_alive", nil)
	if _, err := handshake(context.Background(), conn, fixtureCfg()); err != nil {
		t.Fatal(err)
	}
	before := conn.received.Load()
	if err := conn.send(ProtoKeepAlive, keepAliveRequest(fixtureClock)); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for conn.received.Load() == before && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if conn.received.Load() == before {
		t.Fatal("a keep-alive reply did not keep the heartbeat")
	}
}

func TestReplayGlobalState(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status string
		ready  bool
	}{
		{"global_state_ready", "Ready", true},
		{"global_state_disclaimer", "UnAgreeDisclaimer", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, done := replayConnection(t, tc.name, nil)
			if _, err := handshake(context.Background(), conn, fixtureCfg()); err != nil {
				t.Fatal(err)
			}
			state, err := fetchGlobalState(context.Background(), conn)
			if err != nil {
				t.Fatal(err)
			}
			if state.ProgramStatus != tc.status || state.ProgramReady != tc.ready || !state.QuoteLoggedIn || !state.TradeLoggedIn ||
				state.ServerVersion != MinServerVersion || state.BuildNo == 0 || !state.ServerTime.Equal(fixtureClock) {
				t.Fatalf("state %+v", state)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReplayNotifyKickedOutClosesTheConnectionTyped(t *testing.T) {
	c := NewConnector(Config{Account: 1, ClientVersion: 333})
	conn, done := replayConnection(t, "notify_kicked_out", c.handlePush)
	if _, err := handshake(context.Background(), conn, fixtureCfg()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case <-conn.done:
	case <-time.After(time.Second):
		t.Fatal("the kicked-out push left the connection open")
	}
	var end *SessionEndError
	if !errors.As(conn.failure(), &end) || end.Reason != EndKickedOut || end.Detail == "" {
		t.Fatalf("failure %v", conn.failure())
	}
}

// A malformed reply body and a session-level reply code both condemn the
// connection; a request-level code does not.
func TestRepliesClassifyTheConnection(t *testing.T) {
	reply := func(t *testing.T, body []byte) (*connection, error) {
		t.Helper()
		client, server := net.Pipe()
		t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
		conn := newConnection(client, nil, nil)
		go conn.serve()
		go func() {
			frame, err := ReadFrame(server)
			if err != nil {
				return
			}
			_ = WriteFrame(server, Frame{ProtoID: frame.ProtoID, Serial: frame.Serial, Body: body})
		}()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		var resp notify.Response
		return conn, conn.request(ctx, ProtoGetGlobalState, globalStateRequest(), &resp)
	}
	encode := func(retType int32) []byte {
		raw, err := proto.Marshal(&notify.Response{RetType: new(retType), RetMsg: new("synthetic")})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}

	conn, err := reply(t, []byte{0xff, 0xff})
	if !errors.Is(err, ErrFrame) || conn.failure() == nil {
		t.Fatalf("malformed body: %v, connection %v", err, conn.failure())
	}
	if !errors.Is(classifyEnd(conn.failure()), ErrSessionEnded) {
		t.Fatalf("class %v", classifyEnd(conn.failure()))
	}

	conn, err = reply(t, encode(RetTypeTimeOut))
	var replyErr *Error
	if !errors.As(err, &replyErr) || !replyErr.Session() || conn.failure() == nil {
		t.Fatalf("timeout reply: %v, connection %v", err, conn.failure())
	}

	conn, err = reply(t, encode(RetTypeFailed))
	if !errors.As(err, &replyErr) || replyErr.Session() || conn.failure() != nil {
		t.Fatalf("request refusal: %v, connection %v", err, conn.failure())
	}

	// A request after the connection ended fails with its reason, and a
	// cancelled request forgets its call.
	closed, _ := reply(t, encode(RetTypeFailed))
	closed.close(&SessionEndError{Reason: EndSocket})
	var resp notify.Response
	if err := closed.request(context.Background(), ProtoGetGlobalState, globalStateRequest(), &resp); !errors.Is(err, ErrSessionEnded) {
		t.Fatalf("after close: %v", err)
	}

	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	go func() { _, _ = io.Copy(io.Discard, server) }()
	waiting := newConnection(client, nil, nil)
	go waiting.serve()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waiting.request(ctx, ProtoGetGlobalState, globalStateRequest(), &resp); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
	waiting.mu.Lock()
	pending := len(waiting.pending)
	waiting.mu.Unlock()
	if pending != 0 {
		t.Fatalf("%d calls still pending", pending)
	}
}

func TestClassifyEndNamesTheWayAConnectionFailed(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want SessionEnd
	}{
		{nil, EndSocket},
		{io.EOF, EndSocket},
		{errors.New("broken pipe"), EndSocket},
		{ErrFrame, EndFrame},
		{&SessionEndError{Reason: EndKickedOut}, EndKickedOut},
	} {
		var end *SessionEndError
		if !errors.As(classifyEnd(tc.err), &end) || end.Reason != tc.want {
			t.Errorf("%v: %v", tc.err, classifyEnd(tc.err))
		}
	}
	if got := (&SessionEndError{Reason: EndHeartbeat}).Error(); !strings.Contains(got, "heartbeat_lost") {
		t.Errorf("text %q", got)
	}
}

// The trade header carries the pin and the environment read for it, always;
// before the list reported the pin there is no header and the request fails
// typed.
func TestTradeHeaderCarriesThePinAndTheEnvironmentReadForIt(t *testing.T) {
	c := NewConnector(Config{Account: 77, ClientVersion: 333})
	if _, _, err := c.tradeHeaders(); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("no session: %v", err)
	}
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	conn := newConnection(client, nil, nil)
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	if _, _, err := c.tradeHeaders(); !errors.Is(err, ErrNoScope) {
		t.Fatalf("no scope: %v", err)
	}
	c.mu.Lock()
	c.scope = scope{known: true, env: trdcommon.TrdEnv_TrdEnv_Real, markets: []int32{1, 2}}
	c.mu.Unlock()
	_, headers, err := c.tradeHeaders()
	if err != nil || len(headers) != 2 {
		t.Fatalf("headers %v %v", headers, err)
	}
	for _, h := range headers {
		if h.GetAccID() != 77 || h.GetTrdEnv() != int32(trdcommon.TrdEnv_TrdEnv_Real) {
			t.Fatalf("header %+v", h)
		}
	}
	if headers[0].GetTrdMarket() != 1 || headers[1].GetTrdMarket() != 2 {
		t.Fatalf("markets %+v", headers)
	}
}

// No encoder for the trade unlock can exist: no non-test source in this
// package names its protocol id or its message, and its definition is not
// vendored.
func TestNoEncoderCanWriteTheTradeUnlock(t *testing.T) {
	fset := token.NewFileSet()
	matches, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range matches {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.BasicLit:
				if v.Kind == token.INT && v.Value == "2005" {
					t.Errorf("%s: names the unlock protocol id", fset.Position(v.Pos()))
				}
			case *ast.Ident:
				if name := strings.ToLower(v.Name); strings.Contains(name, "unlocktrade") || strings.Contains(name, "tradeunlock") || strings.Contains(name, "trdunlock") {
					t.Errorf("%s: names %s", fset.Position(v.Pos()), v.Name)
				}
			case *ast.ImportSpec:
				if strings.Contains(strings.ToLower(v.Path.Value), "unlock") {
					t.Errorf("%s: imports %s", fset.Position(v.Pos()), v.Path.Value)
				}
			}
			return true
		})
	}
	if _, err := os.Stat(filepath.Join("internal", "pb", "proto", "Trd_UnlockTrade.proto")); !os.IsNotExist(err) {
		t.Errorf("the unlock definition is vendored: %v", err)
	}
	if _, err := os.Stat(filepath.Join("internal", "pb", "trd_unlocktrade")); !os.IsNotExist(err) {
		t.Errorf("the unlock package is generated: %v", err)
	}
}

// The client's builders ask for no encryption and for protobuf pushes.
func TestInitConnectAsksForNoEncryption(t *testing.T) {
	req := initConnectRequest(Config{ClientID: "x", ClientVersion: 333})
	c2s := req.GetC2S()
	if c2s.GetPacketEncAlgo() != -1 || c2s.GetPushProtoFmt() != 0 || !c2s.GetRecvNotify() || c2s.GetClientVer() != 333 || c2s.GetProgrammingLanguage() != "Go" {
		t.Fatalf("request %+v", c2s)
	}
}

// Start is idempotent and Stop on an unstarted connector does nothing.
func TestStartIsIdempotentAndStopIsSafe(t *testing.T) {
	c := NewConnector(Config{Addr: "127.0.0.1:1", Account: 1, ClientVersion: 333, RedialDelay: 5 * time.Millisecond})
	c.Stop()
	c.Start(context.Background())
	c.Start(context.Background())
	c.Stop()
	if c.Health().Phase != PhaseStopped {
		t.Fatalf("phase %s", c.Health().Phase)
	}
	if _, err := c.Accounts(context.Background()); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("accounts: %v", err)
	}
	if _, ok := c.CaptureSession(); ok || c.IsReady() {
		t.Fatal("a session on a stopped connector")
	}
}
