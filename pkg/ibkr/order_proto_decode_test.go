package ibkr

import (
	"encoding/binary"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Inbound protobuf order callbacks (openOrder, orderStatus, execDetails and
// their end markers) are decoded by hand from the field numbers IBKR's
// .proto files assign. These tests build bodies the same way and pin what the
// summary carries, that unknown fields are skipped, and that malformed bodies
// become a decode_error field rather than a panic.

// ordTExecution is one execDetails callback: reqId (1), the contract (2)
// and the Execution message (3).
type ordTExecution struct {
	reqID, orderID, permID, clientID             int
	execID, execTime, account, exchange, side    string
	shares, cumQty, orderRef                     string
	price, avgPrice                              float64
	symbol, secType, exchange2, currency, lclSym string
}

func ordTExecDetailsBody(e ordTExecution) []byte {
	contract := encodeContractProtoForTest(e.symbol, e.secType, e.exchange2, "", e.currency, e.lclSym, "")
	var exec []byte
	exec = protoAppendInt32(exec, 1, int32(e.orderID))
	exec = protoAppendString(exec, 2, e.execID)
	exec = protoAppendString(exec, 3, e.execTime)
	exec = protoAppendString(exec, 4, e.account)
	exec = protoAppendString(exec, 5, e.exchange)
	exec = protoAppendString(exec, 6, e.side)
	exec = protoAppendString(exec, 7, e.shares)
	if e.price != 0 {
		exec = protoAppendDouble(exec, 8, e.price)
	}
	exec = protoAppendInt32(exec, 9, int32(e.permID))
	exec = protoAppendInt32(exec, 10, int32(e.clientID))
	exec = protoAppendString(exec, 12, e.cumQty)
	if e.avgPrice != 0 {
		exec = protoAppendDouble(exec, 13, e.avgPrice)
	}
	exec = protoAppendString(exec, 14, e.orderRef)
	var body []byte
	body = protoAppendInt32(body, 1, int32(e.reqID))
	body = protoAppendMessage(body, 2, contract)
	body = protoAppendMessage(body, 3, exec)
	return body
}

// ordTOrderStatusBody is an OrderStatus callback in IBKR's field order.
func ordTOrderStatusBody(orderID int, status, filled, remaining string, avg float64, permID int, parentID *int, lastFill float64, clientID int, whyHeld string, mktCap float64) []byte {
	var body []byte
	body = protoAppendInt32(body, 1, int32(orderID))
	body = protoAppendString(body, 2, status)
	body = protoAppendString(body, 3, filled)
	body = protoAppendString(body, 4, remaining)
	body = protoAppendDouble(body, 5, avg)
	body = protoAppendInt32(body, 6, int32(permID))
	if parentID != nil {
		body = protoAppendInt32(body, 7, int32(*parentID))
	}
	body = protoAppendDouble(body, 8, lastFill)
	body = protoAppendInt32(body, 9, int32(clientID))
	body = protoAppendString(body, 10, whyHeld)
	body = protoAppendDouble(body, 11, mktCap)
	return body
}

func ordTRequireSummary(t *testing.T, fields []string, want map[string]string) {
	t.Helper()
	for key, value := range want {
		if got := summaryFieldValue(fields, key+"="); got != value {
			t.Errorf("%s = %q, want %q (fields %#v)", key, got, value, fields)
		}
	}
}

func TestOrderStatusProtoCallbackSummaryCarriesEveryField(t *testing.T) {
	parent := 40
	fields, ok := summarizeInboundOrderProtoCallback(protoOrderStatusMsgID, ordTOrderStatusBody(41, "Submitted", "3", "7", 100.5, 9001, &parent, 100.6, 15, "locate", 0.25))
	if !ok || len(fields) < 3 || fields[0] != strconv.Itoa(msgOrderStatus) || fields[1] != "protobuf" || fields[2] != "wire_msg_id="+strconv.Itoa(protoOrderStatusMsgID) {
		t.Fatalf("summary head: %#v ok=%t", fields, ok)
	}
	ordTRequireSummary(t, fields, map[string]string{
		"orderId": "41", "status": "Submitted", "filled": "3", "remaining": "7", "avgFillPrice": "100.5",
		"permId": "9001", "parentId": "40", "lastFillPrice": "100.6", "clientId": "15", "whyHeld": "locate", "mktCapPrice": "0.25",
	})
	// A status without a parent carries no parentId at all, so a reader
	// cannot mistake a top-level order for a child of order 0.
	fields, _ = summarizeInboundOrderProtoCallback(protoOrderStatusMsgID, ordTOrderStatusBody(41, "Filled", "10", "0", 100.5, 9001, nil, 100.5, 15, "", 0))
	for _, f := range fields {
		if strings.HasPrefix(f, "parentId=") {
			t.Fatalf("parentId reported without the field: %#v", fields)
		}
	}
	// The lifecycle reader consumes the same summary.
	ev, ok := ParseOrderLifecycleEvent(fields)
	if !ok || ev.OrderID != 41 || ev.Status != "Filled" || ev.Filled != 10 || ev.ClientID != 15 || !ev.ClientIDPresent {
		t.Fatalf("lifecycle event from proto summary: %+v ok=%t", ev, ok)
	}
}

func TestExecDetailsProtoCallbackSummaryAndEndMarker(t *testing.T) {
	body := ordTExecDetailsBody(ordTExecution{reqID: 7, orderID: 41, permID: 9001, clientID: 15, execID: "0000e1a7.6b2c3d4e.01.01", execTime: "20261103 15:30:00 US/Eastern",
		account: "DU1234567", exchange: "ISLAND", side: "BOT", shares: "3", cumQty: "3", orderRef: "ref-1", price: 100.5, avgPrice: 100.5,
		symbol: "SYNX", secType: "STK", exchange2: "SMART", currency: "USD", lclSym: "SYNX"})
	fields, ok := summarizeInboundOrderProtoCallback(protoExecDetailsMsgID, body)
	if !ok || fields[0] != strconv.Itoa(msgExecDetails) || fields[1] != "protobuf" {
		t.Fatalf("exec summary head: %#v ok=%t", fields, ok)
	}
	ordTRequireSummary(t, fields, map[string]string{
		"reqId": "7", "orderId": "41", "execId": "0000e1a7.6b2c3d4e.01.01", "execTime": "20261103 15:30:00 US/Eastern", "account": "DU1234567",
		"executionExchange": "ISLAND", "executionSide": "BOT", "shares": "3", "price": "100.5", "permId": "9001", "clientId": "15",
		"cumQty": "3", "avgFillPrice": "100.5", "orderRef": "ref-1", "symbol": "SYNX", "secType": "STK", "exchange": "SMART", "currency": "USD", "localSymbol": "SYNX",
	})

	end, ok := summarizeInboundOrderProtoCallback(protoExecDetailsEndMsgID, protoAppendInt32(nil, 1, 7))
	if !ok || end[0] != strconv.Itoa(msgExecDetailsEnd) || summaryFieldValue(end, "reqId=") != "7" {
		t.Fatalf("exec end: %#v ok=%t", end, ok)
	}
	end, ok = summarizeInboundOrderProtoCallback(protoExecDetailsEndMsgID, protoAppendString(nil, 1, "x"))
	if !ok || summaryFieldValue(end, "decode_error=") == "" {
		t.Fatalf("exec end with a string reqId: %#v", end)
	}
	openEnd, ok := summarizeInboundOrderProtoCallback(protoOpenOrderEndMsgID, nil)
	if !ok || openEnd[0] != strconv.Itoa(msgOpenOrderEnd) {
		t.Fatalf("open order end: %#v ok=%t", openEnd, ok)
	}
	if _, ok := summarizeInboundOrderProtoCallback(protoBufMsgID+msgErrMsg, nil); ok {
		t.Fatal("a non-order protobuf message was summarized as an order callback")
	}
}

func TestOpenOrderProtoCallbackContractAndStateFields(t *testing.T) {
	// Contract as the inbound decoder reads it: conId (1, int32), strike (5,
	// double), multiplier (7, read as a double and formatted; the outbound
	// encoder writes field 7 as a string, an asymmetry this test records,
	// not settles), expiry in 4 and the lastTradeDate in 21.
	var contract []byte
	contract = protoAppendInt32(contract, 1, 4242)
	contract = protoAppendString(contract, 2, "SYNX")
	contract = protoAppendString(contract, 3, "OPT")
	contract = protoAppendString(contract, 4, "20261218")
	contract = protoAppendDouble(contract, 5, 100.5)
	contract = protoAppendString(contract, 6, "C")
	contract = protoAppendDouble(contract, 7, 100)
	contract = protoAppendString(contract, 8, "SMART")
	contract = protoAppendString(contract, 9, "CBOE")
	contract = protoAppendString(contract, 10, "USD")
	contract = protoAppendString(contract, 11, "SYNX  261218C00100500")
	contract = protoAppendString(contract, 12, "SYNX")
	contract = protoAppendString(contract, 99, "ignored unknown field")

	var order []byte
	order = protoAppendInt32(order, 1, 15)
	order = protoAppendInt32(order, 2, 77)
	order = protoAppendInt64(order, 3, 9001)
	order = protoAppendInt32(order, 4, 70)
	order = protoAppendString(order, 5, "SELL")
	order = protoAppendString(order, 6, "2")
	order = protoAppendString(order, 8, "LMT")
	order = protoAppendDouble(order, 9, 3.5)
	order = protoAppendString(order, 11, "DAY")
	order = protoAppendString(order, 12, "DU1234567")
	order = protoAppendBool(order, 19, true)
	order = protoAppendString(order, 27, "oca-1")
	order = protoAppendString(order, 28, "ref-1")
	order = protoAppendInt32(order, 30, 2)
	order = protoAppendInt32(order, 31, 2)
	order = protoAppendBool(order, 66, true)
	order = protoAppendString(order, 68, "C")
	order = protoAppendBool(order, 138, true)
	// Sentinels IBKR sends for unset fields never count as populated.
	order = protoAppendInt32(order, 72, -1)
	order = protoAppendInt32(order, 20, math.MaxInt32)
	order = protoAppendDouble(order, 76, math.MaxFloat64)

	var state []byte
	state = protoAppendString(state, 1, "PreSubmitted")
	state = protoAppendDouble(state, 2, 1000)
	state = protoAppendDouble(state, 8, 1100)
	state = protoAppendDouble(state, 11, 0)
	state = protoAppendString(state, 14, "USD")
	state = protoAppendString(state, 15, "EUR")
	state = protoAppendString(state, 26, "")
	state = protoAppendString(state, 28, "warn")

	var body []byte
	body = protoAppendInt32(body, 1, 77)
	body = protoAppendMessage(body, 2, contract)
	body = protoAppendMessage(body, 3, order)
	body = protoAppendMessage(body, 4, state)
	body = protoAppendInt32(body, 5, 1) // unknown top-level field: skipped

	fields, ok := summarizeInboundOrderProtoCallback(protoOpenOrderMsgID, body)
	if !ok {
		t.Fatal("open order not summarized")
	}
	ordTRequireSummary(t, fields, map[string]string{
		"orderId": "77", "permId": "9001", "clientId": "15", "parentId": "70", "conId": "4242", "symbol": "SYNX", "secType": "OPT",
		"expiry": "20261218", "strike": "100.5", "right": "C", "multiplier": "100", "exchange": "SMART", "primaryExch": "CBOE",
		"currency": "USD", "localSymbol": "SYNX  261218C00100500", "tradingClass": "SYNX", "action": "SELL", "qty": "2",
		"orderType": "LMT", "lmtPrice": "3.5", "tif": "DAY", "account": "DU1234567", "outsideRth": "true", "ocaGroup": "oca-1",
		"ocaType": "2", "triggerMethod": "2", "transmit": "true", "whatIf": "false", "openClose": "C", "deactivated": "true",
		"status": "PreSubmitted", "initMarginBefore": "1000", "initMarginAfter": "1100", "commission": "0",
		"commissionCurrency": "USD", "marginCurrency": "EUR", "warningText": "warn", "modifyTermsKnown": "true",
		// An attached child (parentId, field 4) is not a DAY LMT replacement target.
		"modifyUnsupported": "unsupported broker order field 4",
	})
	// An OrderState double IBKR did not send reads empty, never 0.
	for _, key := range []string{"maintMarginBefore", "equityWithLoanBefore", "minCommission", "maxCommission"} {
		if got := summaryFieldValue(fields, key+"="); got != "" {
			t.Errorf("%s = %q for an absent field, want empty", key, got)
		}
	}
	// The lastTradeDate (21) replaces the expiry when it follows.
	withLastTrade := append(slices.Clip(contract), protoAppendString(nil, 21, "20261219")...)
	var summary inboundOrderProtoSummary
	if err := parseInboundContractProto(withLastTrade, &summary); err != nil || summary.expiry != "20261219" {
		t.Fatalf("lastTradeDate: expiry %q err %v", summary.expiry, err)
	}
	// conId is an int32 on the wire: a value with the high bit set reads negative.
	summary = inboundOrderProtoSummary{}
	if err := parseInboundContractProto(protoAppendInt64(nil, 1, int64(math.MaxUint32)), &summary); err != nil || summary.conID != -1 {
		t.Fatalf("conId int32 truncation: %d err %v", summary.conID, err)
	}
}

// Fields the DAY LMT replacement cannot round-trip mark the order as not
// modifiable, naming the first such field; IBKR's unset sentinels do not.
func TestInboundOrderProtoModifyUnsupportedDetection(t *testing.T) {
	parse := func(order []byte) inboundOrderProtoSummary {
		var s inboundOrderProtoSummary
		if err := parseInboundOrderProto(order, &s); err != nil {
			t.Fatalf("parse: %v", err)
		}
		return s
	}
	base := protoAppendBool(protoAppendString(protoAppendInt32(nil, 1, 15), 5, "BUY"), 66, true)
	if s := parse(base); !s.modifyTermsKnown || s.modifyUnsupported != "" {
		t.Fatalf("plain order: %+v", s)
	}
	if s := parse(protoAppendInt32(append([]byte(nil), base...), 20, 5)); s.modifyUnsupported != "unsupported broker order field 20" {
		t.Fatalf("populated unsupported varint: %q", s.modifyUnsupported)
	}
	if s := parse(protoAppendDouble(append([]byte(nil), base...), 76, 0.5)); s.modifyUnsupported != "unsupported broker order field 76" {
		t.Fatalf("populated unsupported double: %q", s.modifyUnsupported)
	}
	if s := parse(protoAppendString(append([]byte(nil), base...), 40, "x")); s.modifyUnsupported != "unsupported broker order field 40" {
		t.Fatalf("populated unsupported string: %q", s.modifyUnsupported)
	}
	for name, body := range map[string][]byte{
		"exempt code -1":   protoAppendInt32(append([]byte(nil), base...), 72, -1),
		"max int32":        protoAppendInt32(append([]byte(nil), base...), 20, math.MaxInt32),
		"max float":        protoAppendDouble(append([]byte(nil), base...), 76, math.MaxFloat64),
		"zero double":      protoAppendDouble(append([]byte(nil), base...), 76, 0),
		"supported fields": protoAppendInt32(protoAppendString(append([]byte(nil), base...), 27, "oca"), 31, 2),
	} {
		if s := parse(body); s.modifyUnsupported != "" {
			t.Errorf("%s flagged as unsupported: %q", name, s.modifyUnsupported)
		}
	}
	// Adaptive algo params: adaptivePriority is read once; anything else
	// or a duplicate refuses modification.
	algo := func(entries ...[]byte) []byte {
		body := protoAppendString(append([]byte(nil), base...), 61, "Adaptive")
		for _, e := range entries {
			body = protoAppendMessage(body, 62, e)
		}
		return body
	}
	entry := func(k, v string) []byte { return protoAppendString(protoAppendString(nil, 1, k), 2, v) }
	if s := parse(algo(entry("adaptivePriority", "Normal"))); s.algoStrategy != "Adaptive" || s.adaptivePriority != "Normal" || s.modifyUnsupported != "" {
		t.Fatalf("adaptive: %+v", s)
	}
	if s := parse(algo(entry("adaptivePriority", "Normal"), entry("adaptivePriority", "Urgent"))); s.adaptivePriority != "Normal" || !strings.Contains(s.modifyUnsupported, "duplicate") {
		t.Fatalf("duplicate adaptive param: %+v", s)
	}
	if s := parse(algo(entry("other", "1"))); !strings.Contains(s.modifyUnsupported, "unsupported or duplicate algorithm parameter") {
		t.Fatalf("foreign algo param: %q", s.modifyUnsupported)
	}
	// A broker order that is not explicitly transmitted is not modifiable.
	summary, err := parseOpenOrderProtoCallback(protoAppendMessage(protoAppendInt32(nil, 1, 77), 3, protoAppendInt32(nil, 1, 15)))
	if err != nil || summary.modifyUnsupported != "order is not explicitly transmitted" {
		t.Fatalf("untransmitted: %+v err %v", summary, err)
	}
	// The top-level orderId wins over the order message's.
	summary, err = parseOpenOrderProtoCallback(protoAppendMessage(protoAppendInt32(nil, 1, 77), 3, protoAppendInt32(nil, 2, 78)))
	if err != nil || summary.orderID != 77 {
		t.Fatalf("order id precedence: %d err %v", summary.orderID, err)
	}
	summary, err = parseOpenOrderProtoCallback(protoAppendMessage(nil, 3, protoAppendInt32(nil, 2, 78)))
	if err != nil || summary.orderID != 78 {
		t.Fatalf("order id from the order message: %d err %v", summary.orderID, err)
	}
}

// Malformed bodies become a decode_error field; nothing panics and no
// partial figure is reported as a value.
func TestInboundOrderProtoMalformedBodiesReportDecodeErrors(t *testing.T) {
	cases := map[string]struct {
		msgID int
		body  []byte
		want  string
	}{
		"truncated varint tag":        {protoOrderStatusMsgID, []byte{0x80}, "invalid_tag"},
		"truncated varint value":      {protoOrderStatusMsgID, []byte{0x08, 0x80}, "invalid_varint_field_1"},
		"short fixed64":               {protoOrderStatusMsgID, append([]byte{0x29}, 1, 2, 3), "truncated_fixed64_field_5"},
		"bytes length overrun":        {protoOrderStatusMsgID, []byte{0x12, 0x05, 'a'}, "truncated_bytes_field_2"},
		"bytes length missing":        {protoOrderStatusMsgID, []byte{0x12}, "invalid_length_field_2"},
		"start group wire type":       {protoOrderStatusMsgID, []byte{0x0b}, "unsupported_wire_3_field_1"},
		"status orderId as string":    {protoOrderStatusMsgID, protoAppendString(nil, 1, "41"), "field_1_not_varint"},
		"status avg price as varint":  {protoOrderStatusMsgID, protoAppendInt32(nil, 5, 1), "field_5_not_fixed64"},
		"status parentId as string":   {protoOrderStatusMsgID, protoAppendString(nil, 7, "x"), "field_7_not_varint"},
		"status lastFill as string":   {protoOrderStatusMsgID, protoAppendString(nil, 8, "x"), "field_8_not_fixed64"},
		"status clientId as string":   {protoOrderStatusMsgID, protoAppendString(nil, 9, "x"), "field_9_not_varint"},
		"status mktCap as varint":     {protoOrderStatusMsgID, protoAppendInt32(nil, 11, 1), "field_11_not_fixed64"},
		"open order id as string":     {protoOpenOrderMsgID, protoAppendString(nil, 1, "77"), "field_1_not_varint"},
		"open contract conId string":  {protoOpenOrderMsgID, protoAppendMessage(nil, 2, protoAppendString(nil, 1, "x")), "field_1_not_varint"},
		"open contract strike varint": {protoOpenOrderMsgID, protoAppendMessage(nil, 2, protoAppendInt32(nil, 5, 1)), "field_5_not_fixed64"},
		"open contract mult varint":   {protoOpenOrderMsgID, protoAppendMessage(nil, 2, protoAppendInt32(nil, 7, 1)), "field_7_not_fixed64"},
		"open order nested truncated": {protoOpenOrderMsgID, protoAppendMessage(nil, 3, []byte{0x08, 0x80}), "invalid_varint_field_1"},
		"open order deactivate 2":     {protoOpenOrderMsgID, protoAppendMessage(nil, 3, protoAppendInt32(nil, 138, 2)), "invalid order deactivate field"},
		"open order algo as varint":   {protoOpenOrderMsgID, protoAppendMessage(nil, 3, protoAppendInt32(nil, 61, 1)), "invalid algoStrategy wire type"},
		"open order params as varint": {protoOpenOrderMsgID, protoAppendMessage(nil, 3, protoAppendInt32(nil, 62, 1)), "invalid algoParams wire type"},
		"open order param entry int":  {protoOpenOrderMsgID, protoAppendMessage(nil, 3, protoAppendMessage(nil, 62, protoAppendInt32(nil, 1, 1))), "invalid algo parameter wire type"},
		"open state margin as varint": {protoOpenOrderMsgID, protoAppendMessage(nil, 4, protoAppendInt32(nil, 2, 1)), "field_2_not_fixed64"},
		"exec reqId as string":        {protoExecDetailsMsgID, protoAppendString(nil, 1, "7"), "field_1_not_varint"},
		"exec price as varint":        {protoExecDetailsMsgID, protoAppendMessage(nil, 3, protoAppendInt32(nil, 8, 1)), "field_8_not_fixed64"},
		"exec permId as string":       {protoExecDetailsMsgID, protoAppendMessage(nil, 3, protoAppendString(nil, 9, "x")), "field_9_not_varint"},
		"exec clientId as string":     {protoExecDetailsMsgID, protoAppendMessage(nil, 3, protoAppendString(nil, 10, "x")), "field_10_not_varint"},
		"exec avg price as varint":    {protoExecDetailsMsgID, protoAppendMessage(nil, 3, protoAppendInt32(nil, 13, 1)), "field_13_not_fixed64"},
		"exec orderId as string":      {protoExecDetailsMsgID, protoAppendMessage(nil, 3, protoAppendString(nil, 1, "x")), "field_1_not_varint"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fields, ok := summarizeInboundOrderProtoCallback(tc.msgID, tc.body)
			if !ok || len(fields) != 4 || fields[1] != "protobuf" {
				t.Fatalf("summary shape: %#v ok=%t", fields, ok)
			}
			if got := summaryFieldValue(fields, "decode_error="); got != tc.want {
				t.Fatalf("decode_error = %q, want %q", got, tc.want)
			}
		})
	}
	// Order fields of the open order are checked by type too (each of the
	// numeric fields in parseInboundOrderProto refuses the other wire type).
	for _, field := range []int{1, 2, 3, 4, 19, 30, 31, 65, 66} {
		var s inboundOrderProtoSummary
		if err := parseInboundOrderProto(protoAppendString(nil, field, "x"), &s); err == nil || !strings.Contains(err.Error(), "field_"+strconv.Itoa(field)+"_not_varint") {
			t.Errorf("order field %d as string: %v", field, err)
		}
	}
	for _, field := range []int{9, 10, 22, 23, 99} {
		var s inboundOrderProtoSummary
		if err := parseInboundOrderProto(protoAppendInt32(nil, field, 1), &s); err == nil || !strings.Contains(err.Error(), "field_"+strconv.Itoa(field)+"_not_fixed64") {
			t.Errorf("order field %d as varint: %v", field, err)
		}
	}
	// A fixed64 helper refuses a value slice of the wrong length.
	if _, err := protoFixed64Value(5, protoWireFixed64, []byte{1, 2}); err == nil {
		t.Fatal("short fixed64 value accepted")
	}
	if _, err := protoVarintValue(1, protoWireVarint, []byte{0x80}); err == nil {
		t.Fatal("unterminated varint accepted")
	}
}

// The inbound proto decoder reaches these summaries through the connection's
// own decode path on a server that speaks protobuf, and the raw field
// splitter below that version.
func TestConnectionDecodeMessageRoutesProtoOrderCallbacksByServerVersion(t *testing.T) {
	conn := NewConnection(DefaultConfig())
	t.Cleanup(conn.rateLimiter.Stop)
	setServerVersionReady(conn, minServerVerProtoBufPlaceOrder)
	parent := 40
	frame := encodeProtoCallbackFrameForTest(protoOrderStatusMsgID, ordTOrderStatusBody(41, "Submitted", "0", "10", 0, 9001, &parent, 0, 15, "", 0))
	fields := conn.decodeMessage(frame)
	if fields[0] != strconv.Itoa(msgOrderStatus) || fields[1] != "protobuf" || summaryFieldValue(fields, "orderId=") != "41" {
		t.Fatalf("proto server: %#v", fields)
	}
	conn.serverVersion = minServerVerProtoBufPlaceOrder - 1
	fields = conn.decodeMessage(frame)
	if fields[0] != strconv.Itoa(protoOrderStatusMsgID) || (len(fields) > 1 && fields[1] == "protobuf") {
		t.Fatalf("legacy server must not decode protobuf: %#v", fields)
	}
	// The wire interceptor's summary reader finds orderId= wherever it sits.
	if summaryFieldValue([]string{"x", "orderId=5"}, "orderId=") != "5" || summaryFieldValue(nil, "orderId=") != "" {
		t.Fatal("summaryFieldValue")
	}
	if !protoSummaryBool([]string{"whatIf=TRUE"}, "whatIf=") || protoSummaryBool([]string{"whatIf=0"}, "whatIf=") {
		t.Fatal("protoSummaryBool")
	}
}

// The cancel-order protobuf summary names the order and the OrderCancel
// sub-message; a sub-message with a field IBKR does not define is refused.
func TestCancelOrderProtoSummaryDecodes(t *testing.T) {
	frame := encodeCancelOrderProtoFrame(42)
	if got := binary.BigEndian.Uint32(frame[:4]); got != uint32(protoCancelOrderMsgID) {
		t.Fatalf("cancel wire id %d", got)
	}
	fields := summarizeCancelOrderProtoFrame(frame)
	if fields[0] != strconv.Itoa(protoCancelOrderMsgID) || fields[1] != "protobuf" || summaryFieldValue(fields, "orderId=") != "42" || !strings.Contains(strings.Join(fields, " "), "orderCancel=true") {
		t.Fatalf("cancel summary: %#v", fields)
	}
	if fields := summarizeCancelOrderProtoFrame([]byte{0, 0}); summaryFieldValue(fields, "decode_error=") != "truncated" {
		t.Fatalf("short frame: %#v", fields)
	}
	withCancel := func(sub []byte) []byte {
		return append(binary.BigEndian.AppendUint32(nil, uint32(protoCancelOrderMsgID)), protoAppendMessage(protoAppendInt32(nil, 1, 42), 2, sub)...)
	}
	for name, tc := range map[string]struct {
		frame []byte
		want  string
	}{
		"manual time and ext operator": {withCancel(protoAppendInt32(protoAppendString(protoAppendString(nil, 1, "20261103 15:30:00"), 2, "op"), 3, 1)), ""},
		"unknown sub field":            {withCancel(protoAppendInt32(nil, 4, 1)), "unsupported_order_cancel_field_4"},
		"manual time as varint":        {withCancel(protoAppendInt32(nil, 1, 1)), "field_1_not_string"},
		"override as string":           {withCancel(protoAppendString(nil, 3, "1")), "field_3_not_varint"},
		"order cancel as varint":       {append(binary.BigEndian.AppendUint32(nil, uint32(protoCancelOrderMsgID)), protoAppendInt32(protoAppendInt32(nil, 1, 42), 2, 1)...), "field_2_not_message"},
		"order id as string":           {append(binary.BigEndian.AppendUint32(nil, uint32(protoCancelOrderMsgID)), protoAppendString(nil, 1, "42")...), "field_1_not_varint"},
	} {
		t.Run(name, func(t *testing.T) {
			fields := summarizeCancelOrderProtoFrame(tc.frame)
			if got := summaryFieldValue(fields, "decode_error="); got != tc.want {
				t.Fatalf("decode_error = %q, want %q (%#v)", got, tc.want, fields)
			}
			if tc.want == "" && summaryFieldValue(fields, "orderId=") != "42" {
				t.Fatalf("orderId missing: %#v", fields)
			}
		})
	}
}
