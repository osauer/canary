package ibkr

import (
	"bufio"
	"context"
	"errors"
	"strconv"
	"testing"
	"time"
)

// contractDetailsWakeupFrame is a minimal contractData frame for reqID.
func contractDetailsWakeupFrame(reqID int, symbol, secType, conID string) []string {
	f := make([]string, 29)
	f[0] = strconv.Itoa(msgContractData)
	f[1] = strconv.Itoa(reqID)
	f[2] = symbol
	f[3] = secType
	f[8] = "SMART"
	f[9] = "USD"
	f[10] = symbol
	f[12] = symbol
	f[13] = conID
	f[21] = "ARCA"
	return f
}

// newContractDetailsWakeupConnector answers every contract-details request
// before the send returns, the way a fast gateway lands data and end marker
// ahead of the waiter's first select.
func newContractDetailsWakeupConnector(t *testing.T, symbol, secType string, conIDs ...string) *Connector {
	t.Helper()
	c := NewConnector(&ConnectorConfig{})
	conn, _ := newReadyWireTestConnection(t)
	c.conn, c.running, c.ready = conn, true, true
	conn.writer = bufio.NewWriter(prewarmReplyWriter{func() {
		conn.reqIDMu.Lock()
		id := conn.reqIDSeq - 1
		conn.reqIDMu.Unlock()
		for _, conID := range conIDs {
			conn.dispatchHandlers(msgContractData, contractDetailsWakeupFrame(id, symbol, secType, conID), conn.BrokerSessionEpoch())
		}
		prewarmTestEnd(conn, id)
	}})
	return c
}

func TestContractDetailsSymbolFetchKeepsEarlyEndMarkerAndQueuedDetails(t *testing.T) {
	c := newContractDetailsWakeupConnector(t, "HYG", "STK", "101", "102", "103")
	start := time.Now()
	details, err := c.fetchContractDetailsSymbolWire("HYG", 2*time.Second, nil)
	if err != nil {
		t.Fatalf("fetch returned %v after %s with the answer already delivered", err, time.Since(start))
	}
	if len(details) != 3 {
		t.Fatalf("details = %d, want all 3 delivered before the end marker", len(details))
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("fetch waited %s for an answer delivered before it listened", elapsed)
	}
}

func TestContractDetailsRoutedFetchKeepsEarlyEndMarkerAndQueuedDetails(t *testing.T) {
	c := newContractDetailsWakeupConnector(t, "ES", "FUT", "201", "202", "203", "204")
	contract := Contract{Symbol: "ES", SecType: "FUT", Exchange: "CME", Currency: "USD"}
	start := time.Now()
	details, err := c.fetchContractDetailsForContractWire(contract, MarketDataKeyForContract(contract), 2*time.Second, nil)
	if err != nil {
		t.Fatalf("fetch returned %v after %s with the answer already delivered", err, time.Since(start))
	}
	if len(details) != 4 {
		t.Fatalf("details = %d, want all 4 delivered before the end marker", len(details))
	}
}

func TestOptionContractDetailKeepsEarlyEndMarkerAndQueuedDetails(t *testing.T) {
	conn, _ := newReadyWireTestConnection(t)
	conn.writer = bufio.NewWriter(prewarmReplyWriter{func() {
		id, frame := prewarmTestReply(conn)
		conn.dispatchHandlers(msgContractData, frame, conn.BrokerSessionEpoch())
		prewarmTestEnd(conn, id)
	}})
	detail, err := conn.fetchOptionContractDetail(context.Background(), prewarmTestContract(), 2*time.Second)
	if err != nil {
		t.Fatalf("option detail: %v", err)
	}
	if detail == nil || detail.ConID != 654321 {
		t.Fatalf("option detail = %+v, want conID 654321", detail)
	}
}

// Once the waiter has given up, a late burst larger than the details buffer
// must not block the connection reader that dispatches it.
func TestContractDetailsRoutedFetchNeverBlocksReaderAfterTimeout(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	conn, _ := newReadyWireTestConnection(t)
	c.conn, c.running, c.ready = conn, true, true
	contract := Contract{Symbol: "ES", SecType: "FUT", Exchange: "CME", Currency: "USD"}

	handlers := make(chan []func([]string), 1)
	var reqID int
	conn.writer = bufio.NewWriter(prewarmReplyWriter{func() {
		conn.reqIDMu.Lock()
		reqID = conn.reqIDSeq - 1
		conn.reqIDMu.Unlock()
		handlers <- conn.snapshotHandlers(msgContractData)
	}})
	_, err := c.fetchContractDetailsForContractWire(contract, MarketDataKeyForContract(contract), 50*time.Millisecond, nil)
	if !errors.Is(err, ErrContractDetailsTimeout) {
		t.Fatalf("fetch error = %v, want timeout", err)
	}

	late := <-handlers
	delivered := make(chan struct{})
	go func() {
		defer close(delivered)
		for i := range 25 {
			frame := contractDetailsWakeupFrame(reqID, "ES", "FUT", strconv.Itoa(300+i))
			for _, h := range late {
				h(frame)
			}
		}
	}()
	select {
	case <-delivered:
	case <-time.After(2 * time.Second):
		t.Fatal("late contract details blocked the connection reader")
	}
}
