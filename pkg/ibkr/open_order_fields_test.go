package ibkr

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// wireTOutboundFrame waits for the first outbound frame with msgID and
// returns its decoded fields (the last entry is the terminator of the final
// field, see TestEncodeDecodeRoundTripKeepsFieldPositions).
func wireTOutboundFrame(t *testing.T, conn *Connection, out *safeBuffer, msgID int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, frame := range decodeOutboundFrames(t, conn, out.Bytes()) {
			if len(frame) > 0 && frame[0] == strconv.Itoa(msgID) {
				return frame
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("no outbound frame with msgID %d", msgID)
	return nil
}

func wireTAssertFrame(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("frame\n got  %q\n want %q", got, want)
	}
}

// --- 6. reqMktData encoders ------------------------------------------------

// RequestMarketDataWithPrimary puts the EClient reqMktData v11 layout on the
// wire: msgId, version, reqId, conId, symbol, secType, lastTradeDate,
// strike, right, multiplier, exchange, primaryExch, currency, localSymbol,
// tradingClass, deltaNeutral flag, genericTicks, snapshot, regulatory
// snapshot, mktDataOptions. The symbol classifier fills the contract: a
// stock routes SMART/USD, a dual-class ticker goes out in IBKR's space form,
// an FX pair is CASH on IDEALPRO with the base as symbol. The layout has no
// server-version branch.
func TestRequestMarketDataWithPrimaryFrameLayout(t *testing.T) {
	const ticks = "100,101,104,106,165,221,233,236"
	for _, tc := range []struct {
		name, symbol, primary string
		contract              []string // conId … tradingClass
	}{
		{"stock", "SYNX", "", []string{"0", "SYNX", "STK", "", "", "", "", "SMART", "", "USD", "", ""}},
		{"stock with primary", "SYNX", "NASDAQ", []string{"0", "SYNX", "STK", "", "", "", "", "SMART", "NASDAQ", "USD", "", ""}},
		{"dual class", "ZZ.A", "", []string{"0", "ZZ A", "STK", "", "", "", "", "SMART", "", "USD", "", ""}},
		{"fx pair", "EUR.USD", "", []string{"0", "EUR", "CASH", "", "", "", "", "IDEALPRO", "IDEALPRO", "USD", "", ""}},
		{"listed index", "VIX", "", []string{"0", "VIX", "IND", "", "", "", "", "CBOE", "CBOE", "USD", "VIX", "VIX"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, out := newReadyWireTestConnection(t)
			reqID, err := conn.RequestMarketDataWithPrimary(t.Context(), tc.symbol, tc.primary)
			if err != nil {
				t.Fatalf("RequestMarketDataWithPrimary: %v", err)
			}
			want := append([]string{strconv.Itoa(reqMktData), "11", strconv.Itoa(reqID)}, tc.contract...)
			want = append(want, "0", ticks, "0", "0", "", "")
			wireTAssertFrame(t, wireTOutboundFrame(t, conn, out, reqMktData), want)
			conn.marketDataSlotsMu.Lock()
			_, held := conn.marketDataSlots[reqID]
			conn.marketDataSlotsMu.Unlock()
			if !held {
				t.Fatal("the request did not take a market-data slot")
			}
		})
	}
}

// Nothing goes on the wire when the session cannot carry the request.
func TestRequestMarketDataWithPrimaryRefusals(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(conn *Connection)
		symbol  string
		wantErr string
	}{
		{"not connected", func(conn *Connection) { conn.status = StatusDisconnected }, "SYNX", "not connected"},
		{"version not negotiated", func(conn *Connection) { conn.serverVersion = 0 }, "SYNX", "server version not negotiated"},
		{"version below minimum", func(conn *Connection) { conn.serverVersion = 123 }, "SYNX", "too old"},
		{"non-ASCII symbol", func(*Connection) {}, "SYNÖ", "non-ASCII"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, out := newReadyWireTestConnection(t)
			tc.prepare(conn)
			if _, err := conn.RequestMarketDataWithPrimary(t.Context(), tc.symbol, ""); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			time.Sleep(10 * time.Millisecond)
			if out.Len() != 0 {
				t.Fatalf("refused request wrote %d bytes", out.Len())
			}
		})
	}
}

// RequestOptionsMarketData resolves the option through the contract cache,
// then puts the resolved identity on the wire: conId, OPT, the expiry
// re-formatted to YYYYMMDD, the strike, the right upper-cased, multiplier
// 100, the resolved local symbol and trading class, and no primary exchange
// once the conId is known. Its generic tick list omits 165 and 233.
func TestRequestOptionsMarketDataFrameLayout(t *testing.T) {
	conn, out := newReadyWireTestConnection(t)
	key := optionContractKey("SYNX", "SYNX", "20261218", 100, "C")
	conn.optionContractMu.Lock()
	conn.optionContractCache[key] = ContractDetailsLite{Symbol: "SYNX", SecType: "OPT", Expiry: "20261218", Strike: 100, Right: "C",
		Exchange: "SMART", PrimaryExch: "CBOE", ConID: 4242, LocalSymbol: "SYNX  261218C00100000", TradingClass: "SYNX", Multiplier: 100}
	conn.optionContractMu.Unlock()

	reqID, err := conn.RequestOptionsMarketData(t.Context(), "SYNX", "2026-12-18", 100, "c")
	if err != nil {
		t.Fatalf("RequestOptionsMarketData: %v", err)
	}
	want := []string{strconv.Itoa(reqMktData), "11", strconv.Itoa(reqID),
		"4242", "SYNX", "OPT", "20261218", "100", "C", "100", "SMART", "", "USD", "SYNX  261218C00100000", "SYNX",
		"0", "100,101,104,106,221,236", "0", "0", "", ""}
	wireTAssertFrame(t, wireTOutboundFrame(t, conn, out, reqMktData), want)
	if frames := decodeOutboundFrames(t, conn, out.Bytes()); len(frames) != 1 {
		t.Fatalf("a cached option still sent %d frames (a contract lookup?)", len(frames))
	}
}

// --- 7. Legacy placeOrder field encoder ------------------------------------

// assignPlaceOrderFields fills the pre-protobuf placeOrder layout (servers
// below 203) from an IBKROrder; the EClient.placeOrder field order is the
// arbiter of the indices. Text fields are upper-cased where TWS requires
// it, prices are written only when set, and an unset field keeps the
// layout's default so the frame never shifts.
func TestAssignPlaceOrderFieldsLayout(t *testing.T) {
	order := &IBKROrder{
		OrderID: 77, ConID: 4242, Symbol: "SYNX", SecType: "STK", Exchange: "ISLAND", PrimaryExch: "NASDAQ", Currency: "EUR",
		LocalSymbol: "SYNX", TradingClass: "NMS", SecIDType: "ISIN", SecID: "US0000000000",
		Action: "sell", TotalQty: 10, OrderType: "lmt", LmtPrice: 50.25, AuxPrice: 49.5, TIF: "gtc", OcaGroup: "oca-synthetic-1",
		Account: "DU1234567", OpenClose: "C", Origin: 1, OrderRef: "ref-synthetic", Transmit: false, ParentID: 70,
		BlockOrder: true, SweepToFill: true, DisplaySize: 5, TriggerMethod: 2, OutsideRth: true, Hidden: true,
		DiscretionaryAmt: 0.5, GoodAfterTime: "20261104 09:30:00 US/Eastern", GoodTillDate: "20261231 23:59:59 US/Eastern",
		ShortSaleSlot: 1, DesignatedLocation: "loc", ExemptCode: 2, OcaType: 3, Rule80A: "I", SettlingFirm: "firm", AllOrNone: true, MinQty: 4,
		PercentOffset: 0.25, TrailStopPrice: 48, TrailingPercent: 2.5, WhatIf: true,
	}
	fields := clonePlaceOrderFields()
	assignPlaceOrderFields(fields, order)
	if len(fields) != len(placeOrderBaseFields) {
		t.Fatalf("field count changed: %d", len(fields))
	}
	assertFields(t, fields, []fieldAssertion{
		{0, "3", "msgId placeOrder"}, {1, "77", "orderId"}, {2, "4242", "conId"}, {3, "SYNX", "symbol"}, {4, "STK", "secType"},
		{5, "", "lastTradeDate"}, {6, "0.0", "strike default"}, {7, "", "right"}, {8, "", "multiplier"}, {9, "ISLAND", "exchange"},
		{10, "NASDAQ", "primaryExch"}, {11, "EUR", "currency"}, {12, "SYNX", "localSymbol"}, {13, "NMS", "tradingClass"},
		{14, "ISIN", "secIdType"}, {15, "US0000000000", "secId"},
		{16, "SELL", "action upper-cased"}, {17, "10", "totalQty"}, {18, "LMT", "orderType upper-cased"}, {19, "50.25", "lmtPrice"}, {20, "49.5", "auxPrice"},
		{21, "GTC", "tif upper-cased"}, {22, "oca-synthetic-1", "ocaGroup"}, {23, "DU1234567", "account"}, {24, "C", "openClose"}, {25, "1", "origin"},
		{26, "ref-synthetic", "orderRef"}, {27, "0", "transmit false"}, {28, "70", "parentId"}, {29, "1", "blockOrder"}, {30, "1", "sweepToFill"},
		{31, "5", "displaySize"}, {32, "2", "triggerMethod"}, {33, "1", "outsideRth"}, {34, "1", "hidden"}, {35, "", "sharesAllocation (deprecated)"},
		{36, "0.5", "discretionaryAmt"}, {37, "20261104 09:30:00 US/Eastern", "goodAfterTime"}, {38, "20261231 23:59:59 US/Eastern", "goodTillDate"},
		{44, "1", "shortSaleSlot"}, {45, "loc", "designatedLocation"}, {46, "2", "exemptCode"}, {47, "3", "ocaType"}, {48, "I", "rule80A"},
		{49, "firm", "settlingFirm"}, {50, "1", "allOrNone"}, {51, "4", "minQty"}, {52, "0.25", "percentOffset"},
		{53, "0", "eTradeOnly"}, {54, "0", "firmQuoteOnly"}, {77, "48", "trailStopPrice"}, {78, "2.5", "trailingPercent"},
		{placeOrderFieldWhatIf, "1", "whatIf"},
	})
}

// Defaults the encoder leaves in place: an unset price stays "0" (a MKT
// order never carries its limit), a zero order id or quantity keeps the
// layout's zero, an empty exchange and currency keep SMART and USD, and
// exemptCode keeps EClient's -1. Fields the encoder always writes (origin,
// parentId, booleans) are "0" for the zero value.
func TestAssignPlaceOrderFieldsDefaults(t *testing.T) {
	fields := clonePlaceOrderFields()
	assignPlaceOrderFields(fields, &IBKROrder{Symbol: "SYNX", SecType: "STK", Action: "buy", TotalQty: 1, OrderType: "MKT", LmtPrice: 50, Transmit: true})
	assertFields(t, fields, []fieldAssertion{
		{1, "0", "orderId zero"}, {2, "0", "conId zero"}, {9, "SMART", "exchange default"}, {11, "USD", "currency default"},
		{16, "BUY", "action"}, {17, "1", "totalQty"}, {18, "MKT", "orderType"}, {19, "0", "lmtPrice ignored for MKT"}, {20, "", "auxPrice unset"},
		{21, "DAY", "tif default"}, {25, "0", "origin"}, {27, "1", "transmit"}, {28, "0", "parentId"}, {33, "0", "outsideRth"}, {34, "0", "hidden"},
		{36, "0", "discretionaryAmt default"}, {44, "0", "shortSaleSlot"}, {46, "-1", "exemptCode default"}, {47, "0", "ocaType"},
		{placeOrderFieldWhatIf, "0", "whatIf"},
	})
	// A limit order with an unset price keeps the default rather than "0.0".
	fields = clonePlaceOrderFields()
	assignPlaceOrderFields(fields, &IBKROrder{Symbol: "SYNX", OrderType: "LMT"})
	assertFields(t, fields, []fieldAssertion{{18, "LMT", "orderType"}, {19, "0", "lmtPrice unset"}, {16, "BUY", "action default"}})
}
