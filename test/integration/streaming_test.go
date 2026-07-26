package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/mcp"
	"github.com/osauer/canary/v2/internal/rpc"
)

// streamingTestSymbol is the symbol every streaming integration test
// subscribes against. AAPL is liquid enough to receive ticks within seconds
// during market hours; after hours the gateway returns frozen mode (one
// snapshot then silent), which is also a valid signal for these tests —
// they assert frame delivery, not specific tick churn.
const streamingTestSymbol = "AAPL"

// streamingTestTimeout bounds how long we wait for the first frame. Eight
// seconds is the same budget snapshot tests use; it's enough for IBKR to
// deliver the first tick on a fresh subscribe even on a busy gateway.
const streamingTestTimeout = 8 * time.Second

// TestStreamingDirectSubscribe is the end-to-end smoke for the daemon's
// quote.subscribe RPC: open a dial.Conn against the shared daemon, call
// Stream, assert at least one Frame arrives within the timeout. This is
// the same code path `canary quote --watch` exercises, validated against a
// live IB Gateway.
func TestStreamingDirectSubscribe(t *testing.T) {
	skipIfNoGateway(t)

	conn, err := dial.Connect(sharedSocket)
	if err != nil {
		t.Fatalf("dial daemon: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), streamingTestTimeout)
	defer cancel()

	gotFrame := make(chan rpc.Frame, 1)
	go func() {
		params := rpc.QuoteSubscribeParams{
			Contract: rpc.ContractParams{Symbol: streamingTestSymbol, SecType: "STK", Currency: "USD"},
		}
		_ = conn.Stream(ctx, rpc.MethodQuoteSubscribe, params, func(raw json.RawMessage) error {
			var f rpc.Frame
			if err := json.Unmarshal(raw, &f); err != nil {
				return err
			}
			select {
			case gotFrame <- f:
			default:
			}
			cancel() // first frame is enough — tear the stream down
			return nil
		})
	}()

	select {
	case f := <-gotFrame:
		// Frame is the contract being met. We tolerate both real-tick frames
		// and structured-error frames: an after-hours subscription with no
		// IBKR data entitlement may legitimately receive only a frozen-state
		// or error frame, and we don't want the integration suite to fail
		// for legitimate gateway state.
		if f.Error != nil {
			t.Logf("subscribe ended with structured error frame: %s — %s", f.Error.Code, f.Error.Message)
		}
	case <-time.After(streamingTestTimeout + time.Second):
		t.Fatalf("no frame received within %s — gateway may be wedged", streamingTestTimeout+time.Second)
	}
}

// TestStreamingConcurrentSubscribers covers F4 + P2 from the spec: two
// concurrent subscribers to the same symbol must both receive frames, and
// the daemon must hold exactly one IBKR market-data line for that symbol
// (the fan-out refactor's defining property).
//
// We don't have a daemon-introspection RPC for "how many IBKR lines does
// it currently hold," so the assertion is indirect: both subscribers
// receive at least one frame within the timeout (which they would not if
// the second sub were rejected with "already subscribed", as the
// pre-refactor code did). The unit-test layer (TestFanoutSharesIBKRLine)
// covers the strict refcount invariant.
func TestStreamingConcurrentSubscribers(t *testing.T) {
	skipIfNoGateway(t)

	const subscribers = 2
	var wg sync.WaitGroup
	results := make(chan int, subscribers) // 1 per sub that received a frame

	ctx, cancel := context.WithTimeout(context.Background(), streamingTestTimeout)
	defer cancel()

	for i := range subscribers {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			conn, err := dial.Connect(sharedSocket)
			if err != nil {
				t.Errorf("subscriber %d: dial: %v", id, err)
				return
			}
			defer conn.Close()
			subCtx, subCancel := context.WithCancel(ctx)
			defer subCancel()

			params := rpc.QuoteSubscribeParams{
				Contract: rpc.ContractParams{Symbol: streamingTestSymbol, SecType: "STK", Currency: "USD"},
			}
			frameCount := 0
			_ = conn.Stream(subCtx, rpc.MethodQuoteSubscribe, params, func(raw json.RawMessage) error {
				frameCount++
				if frameCount >= 1 {
					results <- 1
					subCancel() // got our frame; release this sub
				}
				return nil
			})
		}(i)
	}

	wg.Wait()
	close(results)

	got := 0
	for r := range results {
		got += r
	}
	if got != subscribers {
		t.Fatalf("only %d of %d subscribers received a frame (want both — fan-out broken?)", got, subscribers)
	}
}

// TestStreamingMCPResourceSubscribe is the MCP-side end-to-end smoke:
// construct an mcp.Server pointed at the live daemon, drive it via stdin
// (initialize + resources/subscribe), read the resulting notifications
// from stdout, and assert at least one resources/updated notification
// arrives within the timeout.
//
// In-process rather than spawning `canary mcp` because we want to observe
// the wire output deterministically; the dialer points at the same
// sharedSocket so the daemon-side fan-out machinery is exercised.
func TestStreamingMCPResourceSubscribe(t *testing.T) {
	skipIfNoGateway(t)

	conn, err := dial.Connect(sharedSocket)
	if err != nil {
		t.Fatalf("dial daemon for MCP server: %v", err)
	}
	defer conn.Close()

	srv := mcp.NewServer(conn, "test")
	srv.SetDialer(func() (*dial.Conn, error) { return dial.Connect(sharedSocket) })

	buf := &bytes.Buffer{}
	buf.WriteString(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n")
	buf.WriteString(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")
	buf.WriteString(`{"jsonrpc":"2.0","id":2,"method":"resources/templates/list"}` + "\n")
	stockQuoteURI := strings.Replace(mcp.StockQuoteURITemplate, "{symbol}", streamingTestSymbol, 1)
	buf.WriteString(`{"jsonrpc":"2.0","id":3,"method":"resources/subscribe","params":{"uri":"` + stockQuoteURI + `"}}` + "\n")

	out := &safeBuffer{}
	ctx, cancel := context.WithTimeout(context.Background(), streamingTestTimeout)
	defer cancel()

	// Wrap the buffer so Read blocks on EOF until ctx is cancelled. A bare
	// bytes.Buffer EOFs the instant the four request lines drain, which
	// makes srv.Serve return; its deferred shutdownSubscriptions cancels
	// the just-spawned subscription goroutine before the daemon's 150ms
	// tick loop has fanned out the first frame. In production an MCP
	// client (Claude Desktop, etc.) keeps stdin open for its lifetime.
	in := &stayOpenReader{buf: buf, done: ctx.Done()}

	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(ctx, in, out) }()

	deadline := time.Now().Add(streamingTestTimeout)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), `"method":"notifications/resources/updated"`) {
			cancel()
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	<-serveDone // wait for graceful shutdown

	body := out.String()
	if !strings.Contains(body, `"protocolVersion":"`+mcp.ProtocolVersion+`"`) {
		t.Errorf("initialize response missing protocolVersion: %s", body)
	}
	if !strings.Contains(body, `"uriTemplate":"`+mcp.StockQuoteURITemplate+`"`) {
		t.Errorf("resources/templates/list response missing stock template: %s", body)
	}
	if !strings.Contains(body, `"method":"notifications/resources/updated"`) {
		t.Fatalf("no resources/updated notification within %s — fan-out or notification path broken\n%s", streamingTestTimeout, body)
	}
}

// stayOpenReader feeds a bytes.Buffer to a consumer that should block on
// EOF — mimicking a real MCP client whose stdin stays open for the client's
// lifetime. Once the buffer drains, Read blocks until done fires.
type stayOpenReader struct {
	buf  *bytes.Buffer
	done <-chan struct{}
}

func (r *stayOpenReader) Read(p []byte) (int, error) {
	n, err := r.buf.Read(p)
	if err == nil || n > 0 {
		return n, err
	}
	<-r.done
	return 0, io.EOF
}

// safeBuffer is a goroutine-safe bytes.Buffer wrapper. The mcp.Server
// writes to its `out` from goroutines (notification emitter, response
// writer) — without serialization the test's Read could race the writes.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
