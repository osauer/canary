package ibkr

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"

	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestOptionDetailMatchesRequestRejectsTradingClassMismatch(t *testing.T) {
	t.Parallel()
	requested := Contract{Symbol: "SPX", TradingClass: "SPX", Expiry: "20260619", Strike: 5400, Right: "C"}

	if optionDetailMatchesRequest(ContractDetailsLite{ConID: 123, TradingClass: "SPXW"}, requested) {
		t.Fatalf("SPX request must not accept SPXW contract details")
	}
	if !optionDetailMatchesRequest(ContractDetailsLite{ConID: 123, TradingClass: "SPX"}, requested) {
		t.Fatalf("matching SPX contract details should be accepted")
	}
	if optionDetailMatchesRequest(ContractDetailsLite{TradingClass: "SPX"}, requested) {
		t.Fatalf("zero ConID contract details should be rejected")
	}
}

func TestSubscribeOptionResolvesSPYThenBlanksPrimaryForMarketDataAndOpenInterestTicks(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	conn, out := newReadyWireTestConnection(t)
	c.conn = conn
	c.running = true
	c.ready = true

	responderDone := make(chan struct{})
	go func() {
		defer close(responderDone)
		contractReqID := waitForHandlerReqID(t, conn, msgContractData)
		if contractReqID == 0 {
			return
		}
		frame := make([]string, 29)
		frame[0] = strconv.Itoa(msgContractData)
		frame[1] = strconv.Itoa(contractReqID)
		frame[2] = "SPY"
		frame[3] = "OPT"
		frame[4] = "20260619"
		frame[6] = "500"
		frame[7] = "C"
		frame[8] = "ARCA"
		frame[9] = "USD"
		frame[10] = "SPY   260619C00500000"
		frame[12] = "SPY"
		frame[13] = "99999"
		frame[15] = "100"
		frame[21] = "ARCA"
		for _, h := range conn.snapshotHandlers(msgContractData) {
			h(frame)
		}
		time.Sleep(20 * time.Millisecond)
		endFrame := []string{strconv.Itoa(msgContractDataEnd), "1", strconv.Itoa(contractReqID)}
		for _, h := range conn.snapshotHandlers(msgContractDataEnd) {
			h(endFrame)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	key, reqID, err := c.SubscribeOption(ctx, "SPY", "SPY", "20260619", 500, "C")
	if err != nil {
		t.Fatalf("SubscribeOption: %v", err)
	}
	<-responderDone
	if reqID == 0 || key == "" {
		t.Fatalf("expected subscription key and reqID, got key=%q reqID=%d", key, reqID)
	}

	frames := decodeOutboundFrames(t, conn, out.Bytes())
	contractDetails := findOutboundFrame(t, frames, reqContractData)
	marketData := findOutboundFrame(t, frames, reqMktData)

	assertField(t, contractDetails, 4, "SPY", "contractDetails symbol")
	assertField(t, contractDetails, 5, "OPT", "contractDetails secType")
	assertField(t, contractDetails, 10, "SMART", "contractDetails exchange")
	assertField(t, contractDetails, 11, "ARCA", "contractDetails primaryExchange")

	assertField(t, marketData, 3, "99999", "marketData conID")
	assertField(t, marketData, 4, "SPY", "marketData symbol")
	assertField(t, marketData, 5, "OPT", "marketData secType")
	assertField(t, marketData, 9, "100", "marketData multiplier")
	assertField(t, marketData, 10, "ARCA", "marketData exchange")
	assertField(t, marketData, 11, "", "marketData primaryExchange")
	assertField(t, marketData, 13, "SPY   260619C00500000", "marketData localSymbol")
	assertField(t, marketData, 14, "SPY", "marketData tradingClass")
	if len(marketData) <= 16 || !strings.Contains(marketData[16], "101") {
		t.Fatalf("marketData generic ticks missing 101: fields=%#v", marketData)
	}
}

func newReadyWireTestConnection(t *testing.T) (*Connection, *safeBuffer) {
	t.Helper()
	conn := NewConnection(nil)
	t.Cleanup(func() { conn.rateLimiter.Stop() })
	conn.status = StatusConnected
	setServerVersionReady(conn, maxClientVersion)
	out := &safeBuffer{}
	conn.writer = bufio.NewWriter(out)
	return conn, out
}

func decodeOutboundFrames(t *testing.T, conn *Connection, payload []byte) [][]string {
	t.Helper()
	var frames [][]string
	offset := 0
	for offset+4 <= len(payload) {
		length := int(binary.BigEndian.Uint32(payload[offset : offset+4]))
		start := offset + 4
		end := start + length
		if length < 0 || end > len(payload) {
			t.Fatalf("invalid outbound length=%d offset=%d payloadLen=%d", length, offset, len(payload))
		}
		frames = append(frames, conn.decodeMessage(payload[start:end]))
		offset = end
	}
	if offset != len(payload) {
		t.Fatalf("trailing partial outbound frame: offset=%d payloadLen=%d", offset, len(payload))
	}
	return frames
}

func findOutboundFrame(t *testing.T, frames [][]string, msgID int) []string {
	t.Helper()
	want := strconv.Itoa(msgID)
	for _, frame := range frames {
		if len(frame) > 0 && frame[0] == want {
			return frame
		}
	}
	t.Fatalf("outbound frame msgID=%d not found: %#v", msgID, frames)
	return nil
}

func assertField(t *testing.T, fields []string, idx int, want string, name string) {
	t.Helper()
	if len(fields) <= idx {
		t.Fatalf("%s field[%d] missing: %#v", name, idx, fields)
	}
	if fields[idx] != want {
		t.Fatalf("%s field[%d] = %q, want %q; fields=%#v", name, idx, fields[idx], want, fields)
	}
}

func waitForHandlerReqID(t *testing.T, conn *Connection, msgID int) int {
	t.Helper()
	return waitForHandlerReqIDAfter(t, conn, msgID, 0)
}

func waitForHandlerReqIDAfter(t *testing.T, conn *Connection, msgID int, after int) int {
	t.Helper()
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		conn.handlersMu.RLock()
		registered := len(conn.msgHandlers[msgID]) > 0
		conn.handlersMu.RUnlock()
		if registered {
			conn.reqIDMu.Lock()
			reqID := conn.reqIDSeq - 1
			conn.reqIDMu.Unlock()
			if reqID > after {
				return reqID
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Errorf("handler for msgID=%d never registered", msgID)
	return 0
}

func TestHandshakeParsesLengthPrefixedServerResponse(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() {
		client.Close()
		server.Close()
	})

	conn := &Connection{
		config: &ConnectionConfig{ClientID: 42},
		conn:   client,
		reader: bufio.NewReader(client),
		writer: bufio.NewWriter(client),
	}

	errCh := make(chan error, 1)
	go func() {
		expected := buildHandshakeFrame("v100..203")
		buf := make([]byte, len(expected))
		if _, err := io.ReadFull(server, buf); err != nil {
			errCh <- err
			return
		}
		if !bytes.Equal(buf, expected) {
			errCh <- fmt.Errorf("unexpected handshake payload: got %x", buf)
			return
		}

		ack := buildHandshakeAck("131", "20250922 12:34:56")
		if _, err := server.Write(ack); err != nil {
			errCh <- err
			return
		}

		errCh <- nil
	}()

	if err := conn.handshake(); err != nil {
		t.Fatalf("handshake failed: %v", err)
	}

	if err := <-errCh; err != nil {
		t.Fatalf("handshake goroutine error: %v", err)
	}

	if conn.serverVersion != 131 {
		t.Fatalf("expected serverVersion 131, got %d", conn.serverVersion)
	}

	if conn.connTime != "20250922 12:34:56" {
		t.Fatalf("expected connTime '20250922 12:34:56', got %q", conn.connTime)
	}
}

func TestHandshakeRejectsOldServerVersion(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() {
		client.Close()
		server.Close()
	})

	conn := &Connection{
		config: &ConnectionConfig{ClientID: 3},
		conn:   client,
		reader: bufio.NewReader(client),
		writer: bufio.NewWriter(client),
	}

	errCh := make(chan error, 1)
	go func() {
		expected := buildHandshakeFrame("v100..203")
		buf := make([]byte, len(expected))
		if _, err := io.ReadFull(server, buf); err != nil {
			errCh <- err
			return
		}
		if !bytes.Equal(buf, expected) {
			errCh <- fmt.Errorf("unexpected handshake payload: got %x", buf)
			return
		}

		if _, err := server.Write([]byte("80\x0020250922 09:00:00\x00")); err != nil {
			errCh <- err
			return
		}
		server.Close()
		errCh <- nil
	}()

	err := conn.handshake()
	if err == nil {
		t.Fatalf("expected handshake failure for old server version")
	}
	if !strings.Contains(err.Error(), "too old") {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := <-errCh; err != nil {
		t.Fatalf("handshake goroutine error: %v", err)
	}
}

func buildHandshakeFrame(version string) []byte {
	descriptorBytes := append([]byte(version), '\x00')
	frame := make([]byte, 0, 4+4+len(descriptorBytes))
	frame = append(frame, 'A', 'P', 'I', '\x00')
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(descriptorBytes)))
	frame = append(frame, lenBuf[:]...)
	frame = append(frame, descriptorBytes...)
	return frame
}

func buildHandshakeAck(fields ...string) []byte {
	var payload bytes.Buffer
	for _, field := range fields {
		payload.WriteString(field)
		payload.WriteByte('\x00')
	}

	body := payload.Bytes()
	frame := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(body)))
	copy(frame[4:], body)
	return frame
}

type testOpenOrderProtoCallback struct {
	OrderID            int
	PermID             int
	ClientID           int
	Symbol             string
	SecType            string
	Exchange           string
	PrimaryExch        string
	Currency           string
	LocalSymbol        string
	TradingClass       string
	Action             string
	Quantity           string
	OrderType          string
	LimitPrice         float64
	AuxPrice           float64
	TrailingPercent    float64
	TrailStopPrice     float64
	LmtPriceOffset     float64
	TIF                string
	TriggerMethod      int
	Account            string
	OrderRef           string
	OutsideRth         bool
	WhatIf             bool
	Transmit           bool
	Status             string
	InitMarginBefore   float64
	MaintMarginBefore  float64
	EquityBefore       float64
	InitMarginAfter    float64
	MaintMarginAfter   float64
	EquityAfter        float64
	Commission         float64
	MinCommission      float64
	MaxCommission      float64
	CommissionCurrency string
	MarginCurrency     string
	RejectReason       string
	WarningText        string
}

func TestDecodeMessageV203OpenOrderProtoCallbackPreservesTrailFields(t *testing.T) {
	t.Parallel()
	conn := NewConnection(DefaultConfig())
	defer conn.rateLimiter.Stop()
	setServerVersionReady(conn, minServerVerProtoBufPlaceOrder)

	fields := conn.decodeMessage(encodeOpenOrderProtoCallbackForTest(testOpenOrderProtoCallback{
		OrderID:         78,
		PermID:          987655,
		ClientID:        31,
		Symbol:          "SPY",
		SecType:         "STK",
		Exchange:        "SMART",
		Currency:        "USD",
		LocalSymbol:     "SPY",
		TradingClass:    "SPY",
		Action:          "SELL",
		Quantity:        "10",
		OrderType:       "TRAIL LIMIT",
		TrailingPercent: 2,
		TrailStopPrice:  98,
		LmtPriceOffset:  0.05,
		TriggerMethod:   2,
		TIF:             "DAY",
		Account:         "DU123456",
		OrderRef:        "trail-test",
		Transmit:        true,
		Status:          "Submitted",
	}))
	if summaryFieldValue(fields, "trailingPercent=") != "2" || summaryFieldValue(fields, "trailStopPrice=") != "98" || summaryFieldValue(fields, "lmtPriceOffset=") != "0.05" || summaryFieldValue(fields, "triggerMethod=") != "2" {
		t.Fatalf("decoded trail fields = %#v", fields)
	}
	ev, ok := ParseOrderLifecycleEvent(fields)
	if !ok {
		t.Fatalf("ParseOrderLifecycleEvent ok=false for fields %#v", fields)
	}
	if ev.OrderType != "TRAIL LIMIT" || ev.TrailingPercent != 2 || ev.TrailStopPrice != 98 || ev.LmtPriceOffset != 0.05 || ev.TriggerMethod != 2 {
		t.Fatalf("event trail fields = %+v", ev)
	}
}

func TestParseSystemNotificationPayloadPreservesAdvancedRejectJSON(t *testing.T) {
	t.Parallel()
	var body []byte
	body = protoAppendInt64(body, 1, 1001)
	body = protoAppendInt32(body, 3, 201)
	body = protoAppendString(body, 4, "Order rejected by precautionary settings")
	body = protoAppendString(body, 5, `{"reason":"size"}`)

	note, err := parseSystemNotificationPayload(body)
	if err != nil {
		t.Fatalf("parseSystemNotificationPayload: %v", err)
	}
	if note.tickerID != 1001 || note.code != 201 || note.message != "Order rejected by precautionary settings" || note.advancedOrderRejectJSON != `{"reason":"size"}` {
		t.Fatalf("note = %+v, want advanced reject details", note)
	}
}

func encodeOpenOrderProtoCallbackForTest(f testOpenOrderProtoCallback) []byte {
	contract := encodeContractProtoForTest(f.Symbol, f.SecType, f.Exchange, f.PrimaryExch, f.Currency, f.LocalSymbol, f.TradingClass)

	var order []byte
	order = protoAppendInt32(order, 1, int32(f.ClientID))
	order = protoAppendInt32(order, 2, int32(f.OrderID))
	order = protoAppendInt64(order, 3, int64(f.PermID))
	order = protoAppendString(order, 5, f.Action)
	order = protoAppendString(order, 6, f.Quantity)
	order = protoAppendString(order, 8, f.OrderType)
	if f.LimitPrice != 0 {
		order = protoAppendDouble(order, 9, f.LimitPrice)
	}
	if f.AuxPrice != 0 {
		order = protoAppendDouble(order, 10, f.AuxPrice)
	}
	if f.TrailingPercent != 0 {
		order = protoAppendDouble(order, 22, f.TrailingPercent)
	}
	if f.TrailStopPrice != 0 {
		order = protoAppendDouble(order, 23, f.TrailStopPrice)
	}
	order = protoAppendString(order, 11, f.TIF)
	order = protoAppendString(order, 12, f.Account)
	if f.OutsideRth {
		order = protoAppendBool(order, 19, true)
	}
	order = protoAppendString(order, 28, f.OrderRef)
	if f.TriggerMethod != 0 {
		order = protoAppendInt32(order, 31, int32(f.TriggerMethod))
	}
	if f.WhatIf {
		order = protoAppendBool(order, 65, true)
	}
	if f.Transmit {
		order = protoAppendBool(order, 66, true)
	}
	if f.LmtPriceOffset != 0 {
		order = protoAppendDouble(order, 99, f.LmtPriceOffset)
	}

	var state []byte
	state = protoAppendString(state, 1, f.Status)
	state = protoAppendDouble(state, 2, f.InitMarginBefore)
	state = protoAppendDouble(state, 3, f.MaintMarginBefore)
	state = protoAppendDouble(state, 4, f.EquityBefore)
	state = protoAppendDouble(state, 8, f.InitMarginAfter)
	state = protoAppendDouble(state, 9, f.MaintMarginAfter)
	state = protoAppendDouble(state, 10, f.EquityAfter)
	state = protoAppendDouble(state, 11, f.Commission)
	state = protoAppendDouble(state, 12, f.MinCommission)
	state = protoAppendDouble(state, 13, f.MaxCommission)
	state = protoAppendString(state, 14, f.CommissionCurrency)
	state = protoAppendString(state, 15, f.MarginCurrency)
	state = protoAppendString(state, 26, f.RejectReason)
	state = protoAppendString(state, 28, f.WarningText)

	var body []byte
	body = protoAppendInt32(body, 1, int32(f.OrderID))
	body = protoAppendMessage(body, 2, contract)
	body = protoAppendMessage(body, 3, order)
	body = protoAppendMessage(body, 4, state)
	return encodeProtoCallbackFrameForTest(protoOpenOrderMsgID, body)
}

func encodeContractProtoForTest(symbol, secType, exchange, primaryExch, currency, localSymbol, tradingClass string) []byte {
	var contract []byte
	contract = protoAppendString(contract, 2, symbol)
	contract = protoAppendString(contract, 3, secType)
	contract = protoAppendString(contract, 8, exchange)
	contract = protoAppendString(contract, 9, primaryExch)
	contract = protoAppendString(contract, 10, currency)
	contract = protoAppendString(contract, 11, localSymbol)
	contract = protoAppendString(contract, 12, tradingClass)
	return contract
}

func encodeProtoCallbackFrameForTest(msgID int, body []byte) []byte {
	var msg []byte
	msg = binary.BigEndian.AppendUint32(msg, uint32(msgID))
	return append(msg, body...)
}
