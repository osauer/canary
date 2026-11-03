package ibkr

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// wireTWriteFrame sends body to the client as one length-prefixed frame, the
// shape IB Gateway puts on the socket: 4-byte big-endian length, then body.
func wireTWriteFrame(t *testing.T, gateway net.Conn, body []byte) {
	t.Helper()
	if _, err := gateway.Write(append(binary.BigEndian.AppendUint32(nil, uint32(len(body))), body...)); err != nil {
		t.Fatalf("gateway write: %v", err)
	}
}

// wireTWaitStatus polls until conn reports want or fails the test.
func wireTWaitStatus(t *testing.T, conn *Connection, want ConnectionStatus) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for conn.Status() != want {
		if time.Now().After(deadline) {
			t.Fatalf("status = %s, want %s", conn.Status(), want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// wireTWaitCount polls until counter reaches want or fails the test.
func wireTWaitCount(t *testing.T, counter *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for counter.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("handler count = %d, want %d", counter.Load(), want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func wireTLastError(conn *Connection) error {
	conn.statusMu.RLock()
	defer conn.statusMu.RUnlock()
	return conn.lastError
}

// wireTReader is a connected reader on a loopback socket with a negotiated
// protocol version; the test writes Gateway frames to the returned socket.
func wireTReader(t *testing.T) (*Connection, net.Conn) {
	t.Helper()
	conn, gateway := heartbeatPair(t, time.Hour)
	conn.serverVersion = maxClientVersion
	conn.wg.Add(1)
	go conn.readMessages()
	return conn, gateway
}

// wireTTickString is a tickString frame; the reader always dispatches it to
// registered handlers, so it is the probe for "still reading".
func wireTTickString(reqID int) []byte {
	return twsGatewayFrame(msgTickString, "1", strconv.Itoa(reqID), "45", "1762160400")
}

// --- 1. Frame reader -------------------------------------------------------

// A length prefix above the 16 MB cap is a protocol fault: the reader ends
// the session with that reason instead of allocating or waiting for a body.
func TestReaderRefusesOversizedLengthPrefix(t *testing.T) {
	conn, gateway := wireTReader(t)
	if _, err := gateway.Write([]byte{0x01, 0x00, 0x00, 0x01}); err != nil { // 16 MiB + 1
		t.Fatal(err)
	}
	wireTWaitStatus(t, conn, StatusDisconnected)
	last, _ := conn.GetConnectionInfo()["last_error"].(string)
	if !strings.Contains(last, "message too large") || !strings.Contains(last, strconv.Itoa(16*1024*1024+1)) {
		t.Fatalf("last_error = %q, want the oversized length named", last)
	}
}

// A body shorter than its prefix followed by the peer hanging up is a
// truncated stream, reported as such, never parsed as a frame.
func TestReaderTruncatedBodyThenPeerCloseEndsSession(t *testing.T) {
	conn, gateway := wireTReader(t)
	var seen atomic.Int32
	conn.RegisterHandler(msgTickString, func([]string) { seen.Add(1) })
	if _, err := gateway.Write(append(binary.BigEndian.AppendUint32(nil, 100), []byte("0123456789")...)); err != nil {
		t.Fatal(err)
	}
	_ = gateway.Close()
	wireTWaitStatus(t, conn, StatusDisconnected)
	if err := wireTLastError(conn); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("lastError = %v, want io.ErrUnexpectedEOF", err)
	}
	if seen.Load() != 0 {
		t.Fatalf("truncated body reached a handler %d times", seen.Load())
	}
}

// A zero-length frame and an unknown message id are both skipped: the reader
// stays aligned and the next real frame still reaches its handler.
func TestReaderSkipsEmptyAndUnknownFramesWithoutDisconnecting(t *testing.T) {
	conn, gateway := wireTReader(t)
	var seen atomic.Int32
	conn.RegisterHandler(msgTickString, func(fields []string) {
		if len(fields) > 2 && fields[2] == "7" {
			seen.Add(1)
		}
	})
	if _, err := gateway.Write([]byte{0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	wireTWriteFrame(t, gateway, wireTTickString(7))
	wireTWaitCount(t, &seen, 1)
	wireTWriteFrame(t, gateway, twsGatewayFrame(9999, "1", "synthetic"))
	wireTWriteFrame(t, gateway, wireTTickString(7))
	wireTWaitCount(t, &seen, 2)
	if conn.Status() != StatusConnected {
		t.Fatalf("status = %s after empty and unknown frames, want connected", conn.Status())
	}
}

// A contractData answer for a request this session never sent, or whose
// handler has since been unregistered, is dropped: no handler runs, no
// disconnect, and the stream keeps flowing.
func TestReaderDropsLateContractDataAnswerWithoutSideEffects(t *testing.T) {
	conn, gateway := wireTReader(t)
	var contract, ticks atomic.Int32
	id := conn.RegisterHandler(msgContractData, func([]string) { contract.Add(1) })
	conn.UnregisterHandler(msgContractData, id)
	conn.RegisterHandler(msgTickString, func([]string) { ticks.Add(1) })

	frame := make([]string, 28)
	frame[0] = "4242" // reqID never issued by this session
	frame[1], frame[2], frame[3] = "SYNX", "STK", ""
	frame[8], frame[12] = "USD", "4242"
	wireTWriteFrame(t, gateway, twsGatewayFrame(msgContractData, frame...))
	wireTWriteFrame(t, gateway, wireTTickString(1))
	wireTWaitCount(t, &ticks, 1)
	if contract.Load() != 0 {
		t.Fatalf("unregistered contractData handler ran %d times", contract.Load())
	}
	if conn.Status() != StatusConnected {
		t.Fatalf("status = %s, want connected", conn.Status())
	}
	if _, held := conn.lookupReqAlias(4242); held {
		t.Fatal("a dropped answer registered a request alias")
	}
}

// encodeMsg writes the v100 frame body (4-byte big-endian message id, then
// NUL-terminated fields) and decodeMessage reads it back field for field.
// Every field keeps its position, an empty field stays an empty string, and
// the terminator of the last field decodes as one extra empty field; the
// handlers index by position and rely on all three.
func TestEncodeDecodeRoundTripKeepsFieldPositions(t *testing.T) {
	conn, out := newReadyWireTestConnection(t)
	body := conn.encodeMsg(msgPosition, "3", "", 4242, 50.25, true, false, int64(7))
	wantHead := []byte{0, 0, 0, byte(msgPosition)}
	if string(body[:4]) != string(wantHead) {
		t.Fatalf("message id bytes = % x, want % x", body[:4], wantHead)
	}
	if got, want := string(body[4:]), "3\x00\x004242\x0050.25\x001\x000\x007\x00"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
	got := conn.decodeMessage(body)
	want := []string{"61", "3", "", "4242", "50.25", "1", "0", "7", ""}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("decoded = %#v, want %#v", got, want)
	}
	if conn.encodeMsg(int64(1)<<40, "x") != nil || conn.encodeMsg("notAnID", "x") != nil {
		t.Fatal("an unencodable message id must yield no frame")
	}

	// On the socket the body is preceded by its 4-byte big-endian length.
	if err := conn.sendMessage(conn.encodeMsg(reqPositions, "1")); err != nil {
		t.Fatalf("sendMessage: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for out.Len() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	raw := out.Bytes()
	if len(raw) != 4+4+2 || binary.BigEndian.Uint32(raw[:4]) != 6 {
		t.Fatalf("socket bytes = % x, want length prefix 6 then id and one field", raw)
	}
	if frames := decodeOutboundFrames(t, conn, raw); len(frames) != 1 || frames[0][0] != strconv.Itoa(reqPositions) || frames[0][1] != "1" {
		t.Fatalf("decoded outbound = %#v", frames)
	}
}

// --- 3. Error frames -------------------------------------------------------

// wireTErrorConn is a ready connection that never redials on a disconnect
// (the default config would dial the live Gateway port).
func wireTErrorConn(t *testing.T) (*Connection, *safeBuffer) {
	t.Helper()
	conn, out := newReadyWireTestConnection(t)
	conn.config.AutoReconnect = false
	return conn, out
}

func wireTErrorFrame(reqID, code int, message string) []byte {
	return twsGatewayFrame(msgErrMsg, "2", strconv.Itoa(reqID), strconv.Itoa(code), message)
}

// Code 326 marks the session as a client-id collision so Connect stops
// walking retries; some Gateway builds send the same verdict as prose under
// a system code, which must land on the same sentinel.
func TestErrorFrameClientIDInUseSetsSentinel(t *testing.T) {
	for _, tc := range []struct {
		name  string
		frame []byte
	}{
		{"code 326", wireTErrorFrame(-1, 326, "Unable to connect as the client id is already in use. Retry with a unique client id.")},
		{"system prose", wireTErrorFrame(-1, -1, "Unable to connect as client id 15 is already in use")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, _ := wireTErrorConn(t)
			conn.processMessage(tc.frame)
			if err := wireTLastError(conn); !errors.Is(err, errClientIDInUse) {
				t.Fatalf("lastError = %v, want errClientIDInUse", err)
			}
			if conn.Status() != StatusConnected {
				t.Fatalf("a 326 verdict must not end the session itself; status = %s", conn.Status())
			}
		})
	}
}

// 502, 503 and 504 say TWS itself is gone or must be upgraded: the session
// ends with the code as the reason.
func TestErrorFrameConnectionCodesEndSession(t *testing.T) {
	for _, code := range []int{502, 503, 504} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			conn, _ := wireTErrorConn(t)
			var disconnected error
			conn.SetOnDisconnect(func(err error) { disconnected = err })
			conn.processMessage(wireTErrorFrame(-1, code, strconv.Itoa(code)))
			if conn.Status() != StatusDisconnected {
				t.Fatalf("status = %s, want disconnected", conn.Status())
			}
			if disconnected == nil || !strings.Contains(disconnected.Error(), fmt.Sprintf("connection error %d", code)) {
				t.Fatalf("onDisconnect error = %v, want connection error %d", disconnected, code)
			}
			// The numeric message is replaced by the catalogue text.
			if !strings.Contains(disconnected.Error(), getErrorDescription(code)) {
				t.Fatalf("onDisconnect error = %v, want description %q", disconnected, getErrorDescription(code))
			}
		})
	}
}

// 10197 (competing live session) flips the connection to delayed market
// data exactly once: one reqMarketDataType(3) frame goes out, and a repeat
// of the code adds nothing.
func TestErrorFrame10197ForcesDelayedDataOnce(t *testing.T) {
	conn, out := wireTErrorConn(t)
	conn.processMessage(wireTErrorFrame(9, 10197, "No market data during competing live session"))
	if !conn.HasCompetingLiveSession() {
		t.Fatal("competing live session not recorded")
	}
	deadline := time.Now().Add(2 * time.Second)
	for out.Len() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	frames := decodeOutboundFrames(t, conn, out.Bytes())
	if len(frames) != 1 {
		t.Fatalf("outbound frames = %#v, want exactly one", frames)
	}
	assertFields(t, frames[0], []fieldAssertion{{0, strconv.Itoa(reqMarketDataType), "msgId"}, {1, "1", "version"}, {2, "3", "delayed data type"}})

	conn.processMessage(wireTErrorFrame(10, 10197, "No market data during competing live session"))
	time.Sleep(20 * time.Millisecond)
	if len(decodeOutboundFrames(t, conn, out.Bytes())) != 1 {
		t.Fatal("a repeated 10197 sent another market-data-type frame")
	}
}

// Connectivity and farm notices are information: the session stays up and
// every registered error handler sees the frame once, field for field.
func TestErrorFrameNoticesReachHandlersWithoutDisconnect(t *testing.T) {
	for _, tc := range []struct {
		code    int
		message string
	}{
		{1100, "Connectivity between IB and Trader Workstation has been lost."},
		{1101, "Connectivity between IB and Trader Workstation has been restored - data lost."},
		{1102, "Connectivity between IB and Trader Workstation has been restored - data maintained."},
		{2104, "Market data farm connection is OK:usfarm"},
		{2106, "HMDS data farm connection is OK:ushmds"},
		{2158, "Sec-def data farm connection is OK:secdefil"},
		{-1, "Market data farm connected"},
		{-1, "2104"},
	} {
		t.Run(fmt.Sprintf("%d %s", tc.code, tc.message), func(t *testing.T) {
			conn, out := wireTErrorConn(t)
			var seen [][]string
			conn.RegisterHandler(msgErrMsg, func(fields []string) { seen = append(seen, fields) })
			conn.processMessage(wireTErrorFrame(-1, tc.code, tc.message))
			if conn.Status() != StatusConnected {
				t.Fatalf("status = %s, want connected", conn.Status())
			}
			if len(seen) != 1 {
				t.Fatalf("handler ran %d times, want once", len(seen))
			}
			assertFields(t, seen[0], []fieldAssertion{{0, "4", "msgId"}, {2, "-1", "reqId"}, {3, strconv.Itoa(tc.code), "code"}, {4, tc.message, "message"}})
			if out.Len() != 0 {
				t.Fatalf("a notice sent %d bytes to the broker", out.Len())
			}
		})
	}
}

// A code 200 or 354 refusal for a market-data request frees that request's
// subscription slot; any other request's slot is untouched.
func TestErrorFrameMarketDataRefusalReleasesSlot(t *testing.T) {
	for _, code := range []int{200, 354} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			conn, _ := wireTErrorConn(t)
			for _, id := range []int{7, 8} {
				if err := conn.acquireMarketDataSlot(t.Context(), id, "test"); err != nil {
					t.Fatalf("acquire slot %d: %v", id, err)
				}
			}
			conn.processMessage(wireTErrorFrame(7, code, "No security definition has been found for the request"))
			conn.marketDataSlotsMu.Lock()
			_, held7 := conn.marketDataSlots[7]
			_, held8 := conn.marketDataSlots[8]
			conn.marketDataSlotsMu.Unlock()
			if held7 || !held8 {
				t.Fatalf("slots after refusal: 7 held=%t 8 held=%t, want 7 released and 8 kept", held7, held8)
			}
		})
	}
}

// A frame with fewer than three fields carries no code: it is ignored.
func TestErrorFrameTooShortIsIgnored(t *testing.T) {
	conn, _ := wireTErrorConn(t)
	conn.processMessage(twsGatewayFrame(msgErrMsg, "2"))
	if conn.Status() != StatusConnected || wireTLastError(conn) != nil {
		t.Fatalf("short error frame changed state: status=%s err=%v", conn.Status(), wireTLastError(conn))
	}
}

// --- 8. Error catalogue ----------------------------------------------------

func TestErrorDescriptionCatalogue(t *testing.T) {
	for code, want := range map[int]string{
		1: "Requested market data is not available", 2: "Requested market data is not subscribed",
		3: "Requested market data cannot be retrieved", 4: "Market data request error",
		110: "Price does not conform to minimum price variation", 161: "Cancel attempted when order is not in a cancellable state",
		162: "Historical market data service error", 320: "Reading request error - Invalid ticker or exchange",
		321: "Error validating request", 322: "Error processing request - Duplicate ticker ID",
		326: "Unable to connect as client id is already in use. Retry with unique client id",
		354: "Requested market data is not subscribed", 430: "The account code is required for this operation",
		431: "Invalid account code", 502: "Couldn't connect to TWS", 503: "The TWS is out of date and must be upgraded",
		504: "Not connected to TWS", 2104: "Market data farm connected", 2106: "Historical data farm connected",
		2107: "Historical data farm connected (inactive)", 2108: "Market data farm disconnected",
		2110: "Connectivity between TWS and server is broken", 2119: "Market data farm connection is OK",
		2158: "Security definition data farm connected", 9999: "",
	} {
		if got := getErrorDescription(code); got != want {
			t.Errorf("getErrorDescription(%d) = %q, want %q", code, got, want)
		}
	}
}

// --- 5. Reconnect backoff --------------------------------------------------

// The schedule: attempt 0 waits nothing, then InitialDelay doubles per
// attempt and is capped at MaxDelay; jitter keeps every sample inside ±10%.
func TestCalculateBackoffScheduleAndJitterEnvelope(t *testing.T) {
	conn := &Connection{config: &ConnectionConfig{InitialDelay: 5 * time.Second, MaxDelay: 60 * time.Second, BackoffMultiplier: 2}}
	want := []time.Duration{0, 5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 60 * time.Second, 60 * time.Second}
	for attempt, d := range want {
		if got := conn.calculateBackoff(attempt); got != d {
			t.Errorf("attempt %d: %s, want %s", attempt, got, d)
		}
	}
	conn.config.Jitter = true
	if got := conn.calculateBackoff(0); got != 0 {
		t.Fatalf("jittered attempt 0 = %s, want 0", got)
	}
	for range 200 {
		for attempt, base := range want[1:] {
			got := conn.calculateBackoff(attempt + 1)
			lo, hi := time.Duration(float64(base)*0.9), time.Duration(float64(base)*1.1)
			if got < lo || got > hi {
				t.Fatalf("attempt %d jittered to %s, outside [%s, %s]", attempt+1, got, lo, hi)
			}
		}
	}
}

// wireTRefusingConfig points a connection at a port nothing listens on and
// keeps every delay in milliseconds.
func wireTRefusingConfig(t *testing.T, maxRetries int, initialDelay time.Duration) *ConnectionConfig {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return &ConnectionConfig{Host: "127.0.0.1", Port: port, ClientID: 15, MaxClientIDRetries: 1, MaxRetries: maxRetries,
		InitialDelay: initialDelay, MaxDelay: 4 * initialDelay, BackoffMultiplier: 2, ConnectTimeout: 500 * time.Millisecond, ManagedConnectionLogging: true}
}

// With nothing listening, the loop makes MaxRetries attempts, counts each
// failure and ends in StatusFailed.
func TestReconnectWithBackoffExhaustsScheduleIntoFailed(t *testing.T) {
	conn := NewConnection(wireTRefusingConfig(t, 2, time.Millisecond))
	t.Cleanup(func() { _ = conn.Disconnect() })
	conn.wg.Add(1)
	go conn.reconnectWithBackoff(t.Context())
	wireTWaitStatus(t, conn, StatusFailed)
	conn.wg.Wait()
	info := conn.GetConnectionInfo()
	if info["error_count"] != 2 {
		t.Fatalf("error_count = %v, want 2", info["error_count"])
	}
	if last, _ := info["last_error"].(string); !strings.Contains(last, "connection refused") {
		t.Fatalf("last_error = %q, want the dial refusal", last)
	}
}

// Disconnect during a long wait stops the loop at once: the stop channel is
// the loop's cancellation, so Disconnect returns well inside its own 2 s
// bound and the attempt counter does not move.
func TestReconnectWithBackoffStopsOnDisconnect(t *testing.T) {
	conn := NewConnection(wireTRefusingConfig(t, 10, time.Hour))
	conn.wg.Add(1)
	go conn.reconnectWithBackoff(t.Context())
	// The first attempt runs without delay and fails; the loop then waits an hour.
	deadline := time.Now().Add(5 * time.Second)
	for conn.GetConnectionInfo()["error_count"] != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("first attempt not recorded: %v", conn.GetConnectionInfo())
		}
		time.Sleep(2 * time.Millisecond)
	}
	started := time.Now()
	if err := conn.Disconnect(); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(started); took > time.Second {
		t.Fatalf("Disconnect took %s with a reconnect loop parked in backoff", took)
	}
	if n := conn.GetConnectionInfo()["error_count"]; n != 1 {
		t.Fatalf("error_count after Disconnect = %v, want 1", n)
	}
}

// The parent context bounds each dial but does not end the loop: a cancelled
// context exhausts the schedule without touching the socket.
func TestReconnectWithBackoffCancelledContextNeverDials(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var accepted atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			_ = c.Close()
		}
	}()
	conn := NewConnection(&ConnectionConfig{Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, ClientID: 15, MaxClientIDRetries: 1,
		MaxRetries: 2, InitialDelay: time.Millisecond, MaxDelay: time.Millisecond, BackoffMultiplier: 2, ConnectTimeout: time.Second})
	t.Cleanup(func() { _ = conn.Disconnect() })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	conn.wg.Add(1)
	go conn.reconnectWithBackoff(ctx)
	wireTWaitStatus(t, conn, StatusFailed)
	conn.wg.Wait()
	if accepted.Load() != 0 {
		t.Fatalf("a cancelled context still dialled %d times", accepted.Load())
	}
	if err := wireTLastError(conn); !errors.Is(err, context.Canceled) {
		t.Fatalf("lastError = %v, want context.Canceled", err)
	}
}
