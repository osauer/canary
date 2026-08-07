package ibkr

import (
	"math"
	"testing"
)

// TestHandleOptionComputationPersistsGreeks verifies that a single model-
// computation tick (msg 21, tickType 13) populates the optGreeks /
// optUnderlyingPx maps, and that OptionGreeks returns what was stored.
//
// Field order matches the comment in connector.go::handleOptionComputation,
// which targets IBKR server version ≥ MIN_SERVER_VER_PRICE_BASED_VOLATILITY
// (= 165). minServerVersionRequired is 124 and modern gateways report 200+,
// so this is the wire layout we always see in practice:
//
//	[msgID, reqID, tickType, tickAttrib, impliedVol, delta, optPrice,
//	 pvDividend, gamma, vega, theta, underlyingPrice]
func TestHandleOptionComputationPersistsGreeks(t *testing.T) {
	conn := NewConnector(nil)
	key := "AAPL_260117C200"
	conn.optReqIDs[7] = key

	// tickType 13 = model computation.
	fields := []string{
		"21",     // msgID
		"7",      // reqID
		"13",     // tickType: model
		"0",      // tickAttrib
		"0.275",  // impliedVol
		"0.523",  // delta
		"12.50",  // optPrice
		"0.0",    // pvDividend
		"0.0142", // gamma
		"34.21",  // vega
		"-0.083", // theta
		"199.40", // underlyingPrice
	}
	conn.handleOptionComputation(fields)

	g, ok := conn.OptionGreeks(key)
	if !ok {
		t.Fatalf("OptionGreeks(%q): not found", key)
	}
	if math.Abs(g.Delta-0.523) > 1e-9 {
		t.Errorf("delta = %v, want 0.523", g.Delta)
	}
	if math.Abs(g.Gamma-0.0142) > 1e-9 {
		t.Errorf("gamma = %v, want 0.0142", g.Gamma)
	}
	if math.Abs(g.Vega-34.21) > 1e-9 {
		t.Errorf("vega = %v, want 34.21", g.Vega)
	}
	if math.Abs(g.Theta-(-0.083)) > 1e-9 {
		t.Errorf("theta = %v, want -0.083", g.Theta)
	}
	if iv, ok := conn.OptionIV(key); !ok || math.Abs(iv-0.275) > 1e-9 {
		t.Errorf("optIV = %v ok=%v, want 0.275", iv, ok)
	}
	if iv, dataType, ok := conn.OptionIVWithDataType(key); !ok || math.Abs(iv-0.275) > 1e-9 || dataType != OptionModelDataTypeLive {
		t.Errorf("typed optIV = %v dataType=%d ok=%v, want 0.275 type=%d", iv, dataType, ok, OptionModelDataTypeLive)
	}
	if u, ok := conn.OptionUnderlyingPrice(key); !ok || math.Abs(u-199.40) > 1e-9 {
		t.Errorf("underlying = %v ok=%v, want 199.40", u, ok)
	}
}

func TestHandleDelayedOptionComputationPersistsAlignedSource(t *testing.T) {
	conn := NewConnector(nil)
	key := "SPY_260117C600"
	conn.optReqIDs[17] = key

	row := func(tickType, optionPrice string) []string {
		return []string{"21", "17", tickType, "0", "0.31", "0.48", optionPrice, "0", "0.012", "21", "-0.08", "598.4"}
	}
	conn.handleOptionComputation(row("80", "10.20"))
	conn.handleOptionComputation(row("81", "10.60"))
	conn.handleOptionComputation(row("83", "10.40"))

	iv, dataType, ok := conn.OptionIVWithDataType(key)
	if !ok || math.Abs(iv-0.31) > 1e-9 || dataType != OptionModelDataTypeDelayed {
		t.Fatalf("typed delayed IV = %v dataType=%d ok=%v, want 0.31 type=%d", iv, dataType, ok, OptionModelDataTypeDelayed)
	}
	bid, ask, ok := conn.OptionQuoteBidAsk(key)
	if !ok || math.Abs(bid-10.20) > 1e-9 || math.Abs(ask-10.60) > 1e-9 {
		t.Fatalf("delayed option quote = bid %v ask %v ok=%v, want 10.20/10.60", bid, ask, ok)
	}
	if g, ok := conn.OptionGreeks(key); !ok || math.Abs(g.Gamma-0.012) > 1e-9 {
		t.Fatalf("delayed model Greeks = %+v ok=%v, want gamma 0.012", g, ok)
	}
}

func TestGenericIVCannotInheritDelayedModelSource(t *testing.T) {
	conn := NewConnector(nil)
	key := "SPY_260117C600"
	conn.optReqIDs[17] = key
	conn.reqIDMap[17] = key
	conn.subscriptions[key] = &Subscription{Symbol: key}
	conn.handleOptionComputation([]string{"21", "17", "83", "0", "0.31", "0.48", "10.4", "0", "0.012", "21", "-0.08", "598.4"})
	conn.handleTickGeneric([]string{"45", "1", "17", "24", "0.44"})

	iv, dataType, ok := conn.OptionIVWithDataType(key)
	if !ok || math.Abs(iv-0.44) > 1e-9 || dataType != 0 {
		t.Fatalf("generic IV = %v dataType=%d ok=%v, want 0.44 with no model source", iv, dataType, ok)
	}
}

// TestHandleOptionComputationWireOffsetIsAtFieldOne is a regression test
// for the v0.12.1 fix: an earlier release read fields[2] as reqID and
// fields[3] as tickType, which matched the pre-server-version-165 wire
// layout. Modern gateways send the new layout (reqID at fields[1]); this
// test asserts the handler routes the row to the right OPRA key by
// reqID, not by what happens to sit at the old offset.
func TestHandleOptionComputationWireOffsetIsAtFieldOne(t *testing.T) {
	conn := NewConnector(nil)
	conn.optReqIDs[42] = "WIRE_TEST_C100"
	// fields[1] = 42 — the real reqID under the new layout.
	// If a regression reverts to fields[2], it would read "13" as reqID
	// (the tickType), find no entry for 13 in optReqIDs, and silently
	// drop the row — reproducing the original v0.10–v0.12.0 bug.
	fields := []string{
		"21",   // msgID
		"42",   // reqID  ← the wire layout under test
		"13",   // tickType
		"0",    // tickAttrib
		"0.30", // IV
		"0.50", // delta
		"5.00", // optPrice
		"0.0",  // pvDividend
		"0.01", // gamma
		"10.0", // vega
		"-0.1", // theta
		"100",  // underlying
	}
	conn.handleOptionComputation(fields)
	if _, ok := conn.OptionGreeks("WIRE_TEST_C100"); !ok {
		t.Fatalf("Greeks not routed to OPRA key — wire offset regressed")
	}
}

// TestHandleOptionComputationRejectsSentinelGreeks proves that IBKR's
// "model hasn't priced this row yet" sentinel values (MaxFloat or NaN
// across the Greeks fields) do not pollute the cache. This is the
// behavior the renderer relies on to decide whether to show "—" vs a
// number — we must never fabricate a zero when the model abstained.
func TestHandleOptionComputationRejectsSentinelGreeks(t *testing.T) {
	conn := NewConnector(nil)
	key := "AAPL_260117P150"
	conn.optReqIDs[3] = key

	// MaxFloat is the standard IBKR "no data" sentinel for greeks.
	const max = "1.7976931348623157E308"
	fields := []string{
		"21", "3", "13", "0",
		"0.0",   // impliedVol absent
		max,     // delta sentinel
		"0.0",   // optPrice
		"0.0",   // pvDividend
		max,     // gamma sentinel
		max,     // vega sentinel
		max,     // theta sentinel
		"199.4", // underlyingPrice still useful
	}
	conn.handleOptionComputation(fields)

	if g, ok := conn.OptionGreeks(key); ok {
		t.Errorf("expected no greeks cached on sentinel row, got %+v", g)
	}
	// Underlying price IS sane and should land — saneGreek doesn't gate
	// underlying price (it has its own bound check in handleOptionComputation).
	if u, ok := conn.OptionUnderlyingPrice(key); !ok || u <= 0 {
		t.Errorf("underlying = %v ok=%v, want a positive value", u, ok)
	}
}

// TestHandleOptionComputationIgnoresUnknownReqID verifies the early-exit
// when a model tick arrives for a reqID we never recorded (e.g. after a
// reconnect drops the map mid-stream).
func TestHandleOptionComputationIgnoresUnknownReqID(t *testing.T) {
	conn := NewConnector(nil)
	// No optReqIDs entry — the function must not panic or record anything.
	fields := []string{"21", "9999", "13", "0", "0.3", "0.5", "10", "0", "0.01", "20", "-0.05", "150"}
	conn.handleOptionComputation(fields)
	if len(conn.optGreeks) != 0 {
		t.Fatalf("expected empty optGreeks, got %d entries", len(conn.optGreeks))
	}
}

// TestHandleOptionComputationMalformedFields checks the length guard:
// short rows must be a no-op, not a panic.
func TestHandleOptionComputationMalformedFields(t *testing.T) {
	conn := NewConnector(nil)
	conn.optReqIDs[1] = "X_260117C100"
	// 11 fields when 12 are required.
	short := []string{"21", "1", "13", "0", "0.3", "0.5", "10", "0", "0.01", "20", "-0.05"}
	conn.handleOptionComputation(short)
	if _, ok := conn.OptionGreeks("X_260117C100"); ok {
		t.Fatalf("expected no entry on short row")
	}
}
