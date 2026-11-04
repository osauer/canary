package ibkr

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"strconv"
	"sync"
	"testing"
	"time"
)

// captureWriter keeps every frame the connection writes and signals each one.
type captureWriter struct {
	mu     sync.Mutex
	frames [][]byte
	wrote  chan struct{}
}

func (w *captureWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.frames = append(w.frames, append([]byte(nil), p...))
	w.mu.Unlock()
	select {
	case w.wrote <- struct{}{}:
	default:
	}
	return len(p), nil
}

func (w *captureWriter) last() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.frames) == 0 {
		return nil
	}
	return w.frames[len(w.frames)-1]
}

// synthetic legacy execDetails frame in the layout parseExecDetailsEvent reads
// (version first): reqId, orderId, conId, symbol, secType, expiry, strike,
// right, multiplier, exchange, currency, localSymbol, execId, time, account,
// exchange, side, shares, price, permId, clientId, liquidation, cumQty,
// avgPrice, orderRef.
func syntheticExecDetailsFrame(c *Connection, reqID int, execID string, clientID int) []byte {
	return c.encodeMsg(msgExecDetails, "10", strconv.Itoa(reqID), "0", "900001", "SYNX", "STK", "", "0", "", "", "ISLAND", "USD", "SYNX",
		execID, "20260918 10:15:30 US/Eastern", "DU1234567", "ISLAND", "BOT", "100", "10.5", "7001", strconv.Itoa(clientID), "0", "100", "10.5", "")
}

func TestFetchExecutionsCollectsItsAnswerAndKeepsLiveFillsOnTheLifecyclePath(t *testing.T) {
	c := readyBrokerEvidenceTestConnector(t)
	conn := c.conn
	setServerVersionReady(conn, maxClientVersion)
	c.ensureHandlersRegistered(conn)
	writer := &captureWriter{wrote: make(chan struct{}, 1)}
	conn.writer = bufio.NewWriterSize(writer, 64*1024)

	var lifecycleMu sync.Mutex
	var lifecycle []OrderLifecycleEvent
	c.RegisterOrderLifecycleReceiptHandler(func(receipt OrderLifecycleReceipt) {
		lifecycleMu.Lock()
		lifecycle = append(lifecycle, receipt.Event)
		lifecycleMu.Unlock()
	})

	type outcome struct {
		snap ExecutionsSnapshot
		err  error
	}
	results := make(chan outcome, 1)
	go func() {
		snap, err := c.FetchExecutions(context.Background(), "DU1234567", 5*time.Second)
		results <- outcome{snap, err}
	}()

	select {
	case <-writer.wrote:
	case <-time.After(5 * time.Second):
		t.Fatal("reqExecutions frame was not written")
	}
	frame := writer.last()
	if len(frame) < 8 {
		t.Fatalf("short frame %x", frame)
	}
	// Length prefix, binary message ID, then null-separated fields.
	if got := int(binary.BigEndian.Uint32(frame[4:8])); got != reqExecutions {
		t.Fatalf("outbound message id = %d, want %d", got, reqExecutions)
	}
	fields := bytes.Split(frame[8:], []byte{0})
	if len(fields) < 4 || string(fields[0]) != "3" || string(fields[3]) != "DU1234567" {
		t.Fatalf("reqExecutions fields = %q, want version 3 and the account filter", fields)
	}
	reqID, err := strconv.Atoi(string(fields[1]))
	if err != nil || reqID <= 0 {
		t.Fatalf("reqExecutions request id = %q", fields[1])
	}
	if string(fields[2]) != "0" {
		t.Fatalf("client id filter = %q, want 0 (unfiltered)", fields[2])
	}

	// The requested inventory answers with the request id; a live fill answers
	// with -1. Only the live fill may reach the order lifecycle handlers.
	conn.processMessage(syntheticExecDetailsFrame(conn, reqID, "0000e0d5.1", 0))
	conn.processMessage(syntheticExecDetailsFrame(conn, reqID, "0000e0d5.2", 15))
	conn.processMessage(syntheticExecDetailsFrame(conn, -1, "0000e0d5.live", 15))
	conn.processMessage(conn.encodeMsg(msgExecDetailsEnd, "1", strconv.Itoa(reqID)))

	var got outcome
	select {
	case got = <-results:
	case <-time.After(5 * time.Second):
		t.Fatal("FetchExecutions did not return after execDetailsEnd")
	}
	if got.err != nil {
		t.Fatalf("FetchExecutions: %v", got.err)
	}
	if !got.snap.Complete || got.snap.RequestID != reqID {
		t.Fatalf("snapshot = %+v, want complete for request %d", got.snap, reqID)
	}
	if len(got.snap.Executions) != 2 || got.snap.Executions[0].ExecID != "0000e0d5.1" || got.snap.Executions[1].ExecID != "0000e0d5.2" {
		t.Fatalf("executions = %+v, want the two requested rows in order", got.snap.Executions)
	}
	if row := got.snap.Executions[1]; row.ClientID != 15 || !row.ClientIDPresent || row.Account != "DU1234567" || row.Shares != 100 || row.Price != 10.5 {
		t.Fatalf("requested row lost its fields: %+v", row)
	}
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	if len(lifecycle) != 1 || lifecycle[0].ExecID != "0000e0d5.live" || lifecycle[0].RequestID != -1 {
		t.Fatalf("lifecycle received %+v, want only the live fill", lifecycle)
	}

	// A late answer after the flight ended is dropped, never dispatched.
	conn.processMessage(syntheticExecDetailsFrame(conn, reqID, "0000e0d5.late", 0))
	if len(lifecycle) != 1 {
		t.Fatalf("late requested execution reached the lifecycle path: %+v", lifecycle)
	}
}

func TestFetchExecutionsReportsAnIncompleteAnswerAtTheDeadline(t *testing.T) {
	c := readyBrokerEvidenceTestConnector(t)
	conn := c.conn
	setServerVersionReady(conn, maxClientVersion)
	c.ensureHandlersRegistered(conn)
	writer := &captureWriter{wrote: make(chan struct{}, 1)}
	conn.writer = bufio.NewWriterSize(writer, 64*1024)

	snap, err := c.FetchExecutions(context.Background(), "DU1234567", 50*time.Millisecond)
	if err != nil {
		t.Fatalf("deadline without an end marker must not error: %v", err)
	}
	if snap.Complete || len(snap.Executions) != 0 || snap.RequestID <= 0 {
		t.Fatalf("snapshot = %+v, want incomplete and empty", snap)
	}
	c.executionsMu.Lock()
	remaining := len(c.executions)
	c.executionsMu.Unlock()
	if remaining != 0 {
		t.Fatalf("flight registry kept %d entries after the deadline", remaining)
	}
}

func TestFetchExecutionsRefusesWithoutAReadyConnector(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	t.Cleanup(c.conn.rateLimiter.Stop)
	if _, err := c.FetchExecutions(context.Background(), "DU1234567", time.Second); err != ErrIBKRUnavailable {
		t.Fatalf("err = %v, want ErrIBKRUnavailable", err)
	}
}

func TestExecDetailsEndRequestIDReadsBothEncodings(t *testing.T) {
	if got := execDetailsEndRequestID([]string{"55", "1", "42"}); got != 42 {
		t.Fatalf("legacy request id = %d, want 42", got)
	}
	// ExecutionDetailsEnd protobuf: field 1 varint reqId.
	body := binary.AppendUvarint([]byte{1 << 3}, 42)
	fields, ok := summarizeInboundOrderProtoCallback(protoExecDetailsEndMsgID, body)
	if !ok || fields[0] != strconv.Itoa(msgExecDetailsEnd) {
		t.Fatalf("protobuf end summary = %q ok=%t", fields, ok)
	}
	if got := execDetailsEndRequestID(fields); got != 42 {
		t.Fatalf("protobuf request id = %d, want 42; fields %q", got, fields)
	}
	if got := execDetailsEndRequestID([]string{"55"}); got != 0 {
		t.Fatalf("short frame request id = %d, want 0", got)
	}
}

// The filter's tail follows the server version: from 200 (TWS API 10.30) the
// gateway reads lastNDays and the specificDates count after side, and a frame
// that stopped at side made it read the next frame's bytes as those fields
// (2026-10-18: "Error reading request. Unable to parse field: 'Client Req Id'
// for input string: 'MART'" once a minute on the executions poll, the request
// after it lost). Below 200 the frame still ends at side.
func TestFetchExecutionsFrameEndsWhereTheServerVersionSays(t *testing.T) {
	for _, tc := range []struct {
		version   int
		lastNDays int
		fields    int
		tail      []string
	}{{199, 0, 10, nil}, {200, 0, 12, []string{"", "0"}}, {maxClientVersion, 0, 12, []string{"", "0"}}, {200, 7, 12, []string{"7", "0"}}, {199, 7, 10, nil}} {
		t.Run(strconv.Itoa(tc.version)+"/"+strconv.Itoa(tc.lastNDays), func(t *testing.T) {
			c := readyBrokerEvidenceTestConnector(t)
			conn := c.conn
			setServerVersionReady(conn, tc.version)
			c.ensureHandlersRegistered(conn)
			writer := &captureWriter{wrote: make(chan struct{}, 1)}
			conn.writer = bufio.NewWriterSize(writer, 64*1024)
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, _ = c.FetchExecutionsFiltered(context.Background(), "DU1234567", ExecutionsFilter{LastNDays: tc.lastNDays}, 5*time.Second)
			}()
			select {
			case <-writer.wrote:
			case <-time.After(5 * time.Second):
				t.Fatal("reqExecutions frame was not written")
			}
			frame := writer.last()
			if len(frame) < 8 || int(binary.BigEndian.Uint32(frame[4:8])) != reqExecutions {
				t.Fatalf("frame %x", frame)
			}
			// version, request id, clientId, acctCode, time, symbol, secType,
			// exchange, side, then the tail; the last split is the empty
			// string after the final terminator.
			fields := bytes.Split(frame[8:], []byte{0})
			if len(fields) != tc.fields || string(fields[0]) != "3" || string(fields[3]) != "DU1234567" || string(fields[8]) != "" {
				t.Fatalf("server %d: fields %q, want %d with the filter through side", tc.version, fields, tc.fields)
			}
			if tc.tail != nil && (string(fields[9]) != tc.tail[0] || string(fields[10]) != tc.tail[1]) {
				t.Fatalf("server %d: tail %q %q, want lastNDays %q and no specific dates", tc.version, fields[9], fields[10], tc.tail[0])
			}
			reqID, err := strconv.Atoi(string(fields[1]))
			if err != nil {
				t.Fatalf("request id %q", fields[1])
			}
			conn.processMessage(conn.encodeMsg(msgExecDetailsEnd, "1", strconv.Itoa(reqID)))
			<-done
		})
	}
}
