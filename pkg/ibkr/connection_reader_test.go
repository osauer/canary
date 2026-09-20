package ibkr

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"
)

// A socket this side closes on purpose (Disconnect, the connect retry)
// unblocks the reader with net.ErrClosed. That is the reader being told to
// stop, and it must not be logged as a read error on every reconnect.
func TestReaderStopsQuietlyWhenItsSocketIsClosed(t *testing.T) {
	buf := captureConnectorLogs(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			accepted <- c
		}
	}()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server := <-accepted
	defer server.Close()

	conn := NewConnection(nil)
	t.Cleanup(func() { conn.rateLimiter.Stop() })
	conn.config.AutoReconnect = false
	conn.conn = client
	conn.reader = bufio.NewReader(client)
	conn.status = StatusConnected
	conn.wg.Add(1)
	go conn.readMessages()

	// Disconnect's order: the status changes first, then the socket closes.
	conn.setStatus(StatusDisconnected)
	_ = client.Close()
	done := make(chan struct{})
	go func() {
		conn.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("reader did not exit after its socket closed")
	}
	if lines := logLines(buf, "level=ERROR"); len(lines) != 0 {
		t.Fatalf("a deliberate close was logged as an error: %q", lines)
	}
	if lines := logLines(buf, "Failed to read length"); len(lines) != 0 {
		t.Fatalf("a deliberate close was logged as a failed read: %q", lines)
	}
}

// A market-data request that names symbol, security type, exchange and
// currency is resolved by the broker; its conID=0 is the normal state before
// contract details arrive, not a misaligned frame. A frame the broker cannot
// resolve still warns, and the summary reads the tick fields at their real
// positions.
func TestReqMktDataByContractDescriptionIsNotAMisalignment(t *testing.T) {
	conn, _ := newReadyWireTestConnection(t)
	frame := func(contract Contract, reqID int, ticks string) []string {
		return conn.decodeOutboundMessage(conn.encodeMsg(conn.buildReqMktDataFields(contract, reqID, ticks, false, false)...))
	}
	pending := frame(Contract{Symbol: "SPY", SecType: "STK", Exchange: "SMART", PrimaryExch: "ARCA", Currency: "USD"}, 7, "100,101")
	w, ok := summarizeReqMktDataFields(pending)
	if !ok || !w.Expected || !strings.Contains(w.Summary, "ticks=100,101 snap=0 regSnap=0") || !strings.Contains(w.Summary, "(contract details pending)") {
		t.Fatalf("described contract summary = %+v ok=%t", w, ok)
	}
	if _, ok := summarizeReqMktDataFields(frame(Contract{ConID: 756733, Symbol: "SPY", SecType: "STK", Exchange: "SMART", Currency: "USD"}, 8, "")); ok {
		t.Fatal("a resolved contract is not suspicious")
	}
	bare := frame(Contract{Symbol: "QQQ", SecType: "STK", Currency: "USD"}, 9, "")
	if w, ok := summarizeReqMktDataFields(bare); !ok || w.Expected {
		t.Fatalf("a request without an exchange cannot be resolved by the broker: %+v ok=%t", w, ok)
	}

	buf := captureConnectorLogs(t)
	conn.logSuspiciousOutbound(reqMktData, pending)
	if lines := logLines(buf, "Protocol misalignment"); len(lines) != 0 {
		t.Fatalf("the expected pre-resolution frame was warned about: %q", lines)
	}
	conn.logSuspiciousOutbound(reqMktData, bare)
	if lines := logLines(buf, "Protocol misalignment for QQQ"); len(lines) != 1 {
		t.Fatalf("an unresolvable frame must still warn once: %q", lines)
	}
}
