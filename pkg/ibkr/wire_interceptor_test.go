package ibkr

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The wire interceptor is a diagnostics recorder of decoded frames. It is
// off unless IBKR_WIRE_INTERCEPTOR asks for it, keeps a bounded ring in
// memory, appends one JSON object per frame to IBKR_WIRE_LOG_PATH under a
// private mode, and names each frame's request and symbol so a reader can
// correlate without decoding again. It performs no redaction: a recorded
// frame carries the account-sensitive fields as decoded (see the type's
// comment and SECURITY.md).

func ordTWireEnv(t *testing.T, enable, ring, path string) {
	t.Helper()
	t.Setenv(envWireEnable, enable)
	t.Setenv(envWireRingSize, ring)
	t.Setenv(envWireLogPath, path)
	if enable == "" {
		os.Unsetenv(envWireEnable)
	}
	if ring == "" {
		os.Unsetenv(envWireRingSize)
	}
	if path == "" {
		os.Unsetenv(envWireLogPath)
	}
}

func TestWireInterceptorIsOffUnlessAsked(t *testing.T) {
	for _, value := range []string{"", "0", "false", "off", "no", "garbage"} {
		ordTWireEnv(t, value, "", "")
		w, err := NewWireInterceptorFromEnv(15)
		if err != nil || w != nil {
			t.Fatalf("%q: interceptor %v err %v, want nil", value, w, err)
		}
		if w.Enabled() {
			t.Fatal("nil interceptor enabled")
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		// Recording on the nil interceptor is a no-op, never a panic.
		w.RecordOutbound(placeOrder, []byte{1}, []string{"3"})
		w.RecordInbound(msgOpenOrder, []byte{1}, []string{"5"})
	}
	for _, value := range []string{"1", "true", "YES", " on "} {
		ordTWireEnv(t, value, "", "")
		w, err := NewWireInterceptorFromEnv(15)
		if err != nil || !w.Enabled() || w.ringCap != 256 || w.clientID != 15 || w.jsonEnc != nil {
			t.Fatalf("%q: %+v err %v", value, w, err)
		}
	}
	for ring, want := range map[string]int{"3": 3, "abc": 256, "-1": 256, "0": 256, " 12 ": 12} {
		ordTWireEnv(t, "1", ring, "")
		w, err := NewWireInterceptorFromEnv(15)
		if err != nil || w.ringCap != want {
			t.Fatalf("ring %q: cap %d err %v, want %d", ring, w.ringCap, err, want)
		}
	}
}

func TestWireInterceptorRecordsFramesToRingAndPrivateLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "private", "wire.jsonl")
	ordTWireEnv(t, "1", "2", path)
	w, err := NewWireInterceptorFromEnv(15)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("log mode %o", info.Mode().Perm())
	}
	if dirInfo, err := os.Stat(filepath.Dir(path)); err != nil || dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("log dir mode %v err %v", dirInfo.Mode(), err)
	}

	conn := NewConnection(DefaultConfig())
	t.Cleanup(conn.rateLimiter.Stop)
	setServerVersionReady(conn, minServerVerProtoBufPlaceOrder)
	order := &IBKROrder{OrderID: 77, ClientID: 15, ConID: 4242, Symbol: "SYNX", SecType: "STK", Exchange: "SMART", Currency: "USD",
		Action: "BUY", TotalQty: 1, OrderType: "LMT", LmtPrice: 100.5, TIF: "DAY", Account: "DU1234567", Transmit: true}
	placeFrame, err := encodePlaceOrderProtoFrame(order)
	if err != nil {
		t.Fatal(err)
	}
	w.RecordOutbound(protoPlaceOrderMsgID, placeFrame, conn.decodeOutboundMessage(placeFrame))
	openFrame := encodeOpenOrderProtoCallbackForTest(testOpenOrderProtoCallback{OrderID: 77, ClientID: 15, Symbol: "SYNX", SecType: "STK", Action: "BUY", Quantity: "1", OrderType: "LMT", LimitPrice: 100.5, Status: "Submitted"})
	w.RecordInbound(protoOpenOrderMsgID, openFrame, conn.decodeMessage(openFrame))
	mktData := conn.encodeMsg(reqMktData, 11, 9001, 0, "ZETA", "STK", "", 0, "", "", "SMART", "", "USD", "", "", 0, "", 0, 0, "")
	w.RecordOutbound(reqMktData, mktData, conn.decodeMessage(mktData))

	// The ring holds the last two frames; the sequence keeps counting.
	w.ringMu.Lock()
	ring := append([]WireFrame(nil), w.ring...)
	w.ringMu.Unlock()
	if len(ring) != 2 || ring[0].Seq != 2 || ring[1].Seq != 3 {
		t.Fatalf("ring after eviction: %+v", ring)
	}
	// The protobuf callback carries the wire id (base + 200); its name is
	// not resolved, the correlation comes from the decoded summary.
	if ring[0].Direction != WireInbound || ring[0].MsgID != protoOpenOrderMsgID || ring[0].MsgName == "" || ring[0].ReqID != "77" || ring[0].Symbol != "SYNX" {
		t.Fatalf("inbound frame: %+v", ring[0])
	}
	if ring[1].Direction != WireOutbound || ring[1].MsgName != "reqMktData" || ring[1].ReqID != "9001" || ring[1].Symbol != "ZETA" || ring[1].LengthBytes != fmt.Sprintf("%08x", len(mktData)) {
		t.Fatalf("outbound market data frame: %+v", ring[1])
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	// After Close the ring still records but nothing more is persisted.
	w.RecordInbound(msgErrMsg, []byte("x"), []string{"4", "2", "77", "201", "rejected"})

	// The log holds every frame, one JSON object per line, decodable into
	// WireFrame with the same correlation fields.
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var frames []WireFrame
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var frame WireFrame
		if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
			t.Fatalf("line %q: %v", scanner.Text(), err)
		}
		frames = append(frames, frame)
	}
	if len(frames) != 3 {
		t.Fatalf("%d frames persisted, want 3", len(frames))
	}
	if frames[0].Seq != 1 || frames[0].Direction != WireOutbound || frames[0].MsgName != "placeOrderProtoBuf" || frames[0].MsgID != protoPlaceOrderMsgID || frames[0].ReqID != "77" || frames[0].Symbol != "SYNX" {
		t.Fatalf("persisted placeOrder: %+v", frames[0])
	}
	if frames[0].RawHex == "" || !strings.Contains(strings.Join(frames[0].Fields, " "), "orderId=77") {
		t.Fatalf("persisted placeOrder lacks the raw frame or decoded fields: %+v", frames[0])
	}
	if frames[2].Seq != 3 || frames[2].MsgName != "reqMktData" {
		t.Fatalf("persisted market data: %+v", frames[2])
	}
	// Decoded fields are copied, not aliased: mutating the caller's slice
	// after recording leaves the frame intact.
	fields := []string{"4", "2", "77", "201", "rejected"}
	ordTWireEnv(t, "1", "4", "")
	w2, err := NewWireInterceptorFromEnv(15)
	if err != nil {
		t.Fatal(err)
	}
	w2.RecordInbound(msgErrMsg, nil, fields)
	fields[4] = "changed"
	w2.ringMu.Lock()
	got := w2.ring[0]
	w2.ringMu.Unlock()
	if got.Fields[4] != "rejected" || got.ReqID != "77" || got.MsgName != "error" || got.LengthBytes != "00000000" {
		t.Fatalf("error frame: %+v", got)
	}
	w2.RecordInbound(msgOpenOrderEnd, nil, nil)
	w2.ringMu.Lock()
	empty := w2.ring[1]
	w2.ringMu.Unlock()
	if empty.Fields != nil || empty.ReqID != "" || empty.MsgName != "msg("+strconv.Itoa(msgOpenOrderEnd)+")" {
		t.Fatalf("frame without fields: %+v", empty)
	}
}

func TestWireInterceptorRefusesAnUnwritableLogPath(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ordTWireEnv(t, "1", "", filepath.Join(blocker, "wire.jsonl"))
	w, err := NewWireInterceptorFromEnv(15)
	if err == nil || w != nil {
		t.Fatalf("log under a regular file: %v %v", w, err)
	}
	if !strings.Contains(err.Error(), "wire interceptor") {
		t.Fatalf("error does not name the recorder: %v", err)
	}
	// A path that is itself a directory cannot be opened for appending.
	ordTWireEnv(t, "1", "", dir)
	if w, err := NewWireInterceptorFromEnv(15); err == nil || w != nil {
		t.Fatalf("directory as log: %v %v", w, err)
	}
}

func TestWireInterceptorCorrelationAndMessageNames(t *testing.T) {
	for name, tc := range map[string]struct {
		msgID      int
		fields     []string
		req, symbl string
	}{
		"proto placeOrder":   {protoPlaceOrderMsgID, []string{"203", "protobuf", "orderId=77", "symbol=SYNX"}, "77", "SYNX"},
		"proto callback":     {protoOpenOrderMsgID, []string{"5", "protobuf", "wire_msg_id=205", "orderId=78", "symbol=ZETA"}, "78", "ZETA"},
		"legacy placeOrder":  {placeOrder, []string{"3", "77", "4242", "SYNX"}, "77", "SYNX"},
		"reqMktData":         {reqMktData, []string{"1", "11", "9001", "4242", "ZETA"}, "9001", "ZETA"},
		"reqContractData":    {reqContractData, []string{"9", "8", "9002", "4242", "SYNX"}, "9002", "SYNX"},
		"reqHistoricalData":  {reqHistoricalData, []string{"20", "9003", "4242", "ZETA"}, "9003", "ZETA"},
		"error":              {msgErrMsg, []string{"4", "2", "77", "201", "text"}, "77", ""},
		"unknown message":    {msgOpenOrderEnd, []string{"53", "1"}, "", ""},
		"no fields":          {placeOrder, nil, "", ""},
		"short legacy frame": {placeOrder, []string{"3"}, "", ""},
	} {
		req, sym := extractReqInfo(tc.msgID, tc.fields)
		if req != tc.req || sym != tc.symbl {
			t.Errorf("%s: %q %q, want %q %q", name, req, sym, tc.req, tc.symbl)
		}
	}
	for msgID, want := range map[int]string{
		protoPlaceOrderMsgID: "placeOrderProtoBuf", placeOrder: "placeOrder", reqMktData: "reqMktData", reqContractData: "reqContractData",
		reqHistoricalData: "reqHistoricalData", reqCalcImpliedVolatility: "reqCalcImpliedVol", cancelMktData: "cancelMktData",
		msgOpenOrder: "openOrder", msgExecDetails: "execDetails", msgErrMsg: "error", msgSystemNotification: "systemNotification",
		msgOpenOrderEnd: "msg(" + strconv.Itoa(msgOpenOrderEnd) + ")",
	} {
		if got := resolveMessageName(msgID); got != want {
			t.Errorf("message %d named %q, want %q", msgID, got, want)
		}
	}
	// The protobuf cancel shares its id space with inbound callbacks: the
	// name depends on the direction.
	if got := resolveMessageNameForDirection(WireOutbound, protoCancelOrderMsgID); got != "cancelOrderProtoBuf" {
		t.Fatalf("outbound cancel named %q", got)
	}
	if got := resolveMessageNameForDirection(WireInbound, protoCancelOrderMsgID); got == "cancelOrderProtoBuf" {
		t.Fatalf("inbound frame %d named as the cancel", protoCancelOrderMsgID)
	}
	if got := resolveMessageNameForDirection(WireInbound, msgOpenOrder); got != "openOrder" {
		t.Fatalf("inbound openOrder named %q", got)
	}
	if cloneStringSlice(nil) != nil || cloneStringSlice([]string{}) != nil {
		t.Fatal("empty clone is not nil")
	}
}
