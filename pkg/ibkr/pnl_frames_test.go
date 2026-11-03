package ibkr

import (
	"errors"
	"strconv"
	"testing"
)

// histTPnLFixture is a ready connector with the inbound handlers installed,
// so P&L frames travel the socket reader's path into the cache.
func histTPnLFixture(t *testing.T) (*Connector, *Connection, *histTGateway) {
	t.Helper()
	c, conn, g := histTFixture(t)
	conn.displayChanged = c.display.notify
	c.registerHandlers(conn)
	return c, conn, g
}

func histTPnLReqIDs(t *testing.T, c *Connector, conID int) (account, position int) {
	t.Helper()
	c.pnl.mu.RLock()
	defer c.pnl.mu.RUnlock()
	position, ok := c.pnl.positionReqIDs[conID]
	if !ok {
		t.Fatalf("no position P&L subscription for conID %d", conID)
	}
	return c.pnl.accountReqID, position
}

func histTFloat(t *testing.T, name string, got *float64, want float64) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("%s = %v, want %v", name, got, want)
	}
}

// The Connection encoders send the IBKR frames as documented and refuse
// what the gateway would reject: an unclaimed or reused request id, a blank
// account, a non-ASCII account, a missing contract id, and no session.
func TestPnLConnectionEncodersAndRefusals(t *testing.T) {
	conn, out := newReadyWireTestConnection(t)
	conn.observeNextValidOrderID(100)
	if err := conn.RequestPnL(100, "DU1234567", ""); err != nil {
		t.Fatalf("RequestPnL = %v", err)
	}
	if err := conn.RequestPnLSingle(101, "DU1234567", "", 4242); err != nil {
		t.Fatalf("RequestPnLSingle = %v", err)
	}
	if err := conn.CancelPnL(100); err != nil {
		t.Fatalf("CancelPnL = %v", err)
	}
	if err := conn.CancelPnLSingle(101); err != nil {
		t.Fatalf("CancelPnLSingle = %v", err)
	}
	frames := decodeOutboundFrames(t, conn, out.Bytes())
	if len(frames) != 4 {
		t.Fatalf("frames = %v, want four", frames)
	}
	assertFields(t, frames[0], []fieldAssertion{{0, "92", "reqPnL"}, {1, "100", "reqId"}, {2, "DU1234567", "account"}, {3, "", "modelCode"}})
	assertFields(t, frames[1], []fieldAssertion{{0, "94", "reqPnLSingle"}, {1, "101", "reqId"}, {2, "DU1234567", "account"}, {3, "", "modelCode"}, {4, "4242", "conId"}})
	assertFields(t, frames[2], []fieldAssertion{{0, "93", "cancelPnL"}, {1, "100", "reqId"}})
	assertFields(t, frames[3], []fieldAssertion{{0, "95", "cancelPnLSingle"}, {1, "101", "reqId"}})

	before := out.Len()
	for name, err := range map[string]error{
		"reused id":         conn.RequestPnL(100, "DU1234567", ""),
		"blank account":     conn.RequestPnL(102, "", ""),
		"non-ascii account": conn.RequestPnL(102, "DÜ1234567", ""),
		"non-ascii model":   conn.RequestPnLSingle(102, "DU1234567", "mödel", 4242),
		"missing conId":     conn.RequestPnLSingle(102, "DU1234567", "", 0),
		"single blank acct": conn.RequestPnLSingle(102, "", "", 4242),
		"cancel id zero":    conn.CancelPnL(0),
		"cancel single big": conn.CancelPnLSingle(maxProtoInt32 + 1),
	} {
		if err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	conn.status = StatusDisconnected
	if err := conn.RequestPnL(102, "DU1234567", ""); err == nil {
		t.Fatal("reqPnL without a session was accepted")
	}
	if err := conn.RequestPnLSingle(102, "DU1234567", "", 4242); err == nil {
		t.Fatal("reqPnLSingle without a session was accepted")
	}
	if err := conn.CancelPnL(100); err != nil {
		t.Fatalf("cancelPnL without a session = %v, want a no-op", err)
	}
	if err := conn.CancelPnLSingle(101); err != nil {
		t.Fatalf("cancelPnLSingle without a session = %v, want a no-op", err)
	}
	if out.Len() != before {
		t.Fatal("a refused request reached the socket")
	}
}

// The subscriptions go out as reqPnL and reqPnLSingle for the account and
// contract; the gateway's pnl and pnlSingle frames land in the caches,
// readable without wire traffic, with IBKR's not-yet-computed sentinel and
// blanks read as missing rather than zero.
func TestPnLFramesPopulateTheAccountAndPositionCaches(t *testing.T) {
	c, conn, g := histTPnLFixture(t)
	if err := c.SubscribeAccountPnL("DU1234567"); err != nil {
		t.Fatal(err)
	}
	if err := c.SubscribePositionDailyPnL("DU1234567", 4242); err != nil {
		t.Fatal(err)
	}
	if err := c.SubscribeAccountPnL("DU1234567"); err != nil {
		t.Fatal(err)
	}
	if err := c.SubscribePositionDailyPnL("DU1234567", 4242); err != nil {
		t.Fatal(err)
	}
	acctReq, posReq := histTPnLReqIDs(t, c, 4242)
	requests := g.framesByID(reqPnL)
	singles := g.framesByID(reqPnLSingle)
	if len(requests) != 1 || len(singles) != 1 {
		t.Fatalf("reqPnL=%v reqPnLSingle=%v, want one each for repeated subscriptions", requests, singles)
	}
	assertFields(t, requests[0], []fieldAssertion{{1, strconv.Itoa(acctReq), "reqId"}, {2, "DU1234567", "account"}, {3, "", "modelCode"}})
	assertFields(t, singles[0], []fieldAssertion{{1, strconv.Itoa(posReq), "reqId"}, {2, "DU1234567", "account"}, {3, "", "modelCode"}, {4, "4242", "conId"}})
	if _, ok := c.AccountDailyPnL(); ok {
		t.Fatal("account P&L reported before any frame")
	}
	if snap, ok := c.PositionDailyPnL(4242); !ok || snap.DailyPnL != nil {
		t.Fatalf("position P&L before any frame = %+v, %t; want an empty tracked snapshot", snap, ok)
	}
	if _, ok := c.PositionDailyPnL(9999); ok {
		t.Fatal("an unsubscribed contract reported P&L")
	}

	changed := c.DisplayChanges()
	conn.processMessage(twsGatewayFrame(msgPnL, strconv.Itoa(acctReq), "250.5", "1200", "-40"))
	conn.processMessage(twsGatewayFrame(msgPnLSingle, strconv.Itoa(posReq), "100", "12.5", "1.7976931348623157E308", "1.5"))
	select {
	case <-changed:
	default:
		t.Fatal("P&L frames did not invalidate the display")
	}
	acct, ok := c.AccountDailyPnL()
	if !ok || acct.DailyPnLStatus != DailyPnLFrameAvailable || acct.AsOf.IsZero() {
		t.Fatalf("account P&L = %+v, %t", acct, ok)
	}
	histTFloat(t, "account daily", acct.DailyPnL, 250.5)
	histTFloat(t, "account unrealized", acct.UnrealizedTotalPnL, 1200)
	histTFloat(t, "account realized", acct.RealizedTotalPnL, -40)
	pos, ok := c.PositionDailyPnL(4242)
	if !ok || pos.AsOf.IsZero() || pos.UnrealizedTotalPnL != nil {
		t.Fatalf("position P&L = %+v, %t; want the sentinel read as missing", pos, ok)
	}
	histTFloat(t, "position daily", pos.DailyPnL, 12.5)
	histTFloat(t, "position realized", pos.RealizedTotalPnL, 1.5)

	// The short account frame of an older gateway carries only the daily
	// figure; a blank daily is unavailable, a non-number malformed.
	conn.processMessage(twsGatewayFrame(msgPnL, strconv.Itoa(acctReq), "42"))
	if acct, _ = c.AccountDailyPnL(); acct.UnrealizedTotalPnL != nil || acct.RealizedTotalPnL != nil || acct.DailyPnL == nil || *acct.DailyPnL != 42 {
		t.Fatalf("short frame = %+v", acct)
	}
	conn.processMessage(twsGatewayFrame(msgPnL, strconv.Itoa(acctReq), "", "1", "2"))
	if acct, _ = c.AccountDailyPnL(); acct.DailyPnL != nil || acct.DailyPnLStatus != DailyPnLFrameUnavailable {
		t.Fatalf("blank daily = %+v", acct)
	}
	conn.processMessage(twsGatewayFrame(msgPnL, strconv.Itoa(acctReq), "n/a", "1", "2"))
	if acct, _ = c.AccountDailyPnL(); acct.DailyPnL != nil || acct.DailyPnLStatus != DailyPnLFrameMalformed {
		t.Fatalf("malformed daily = %+v", acct)
	}

	// Frames for an unknown request id or a retired socket generation
	// change nothing.
	conn.processMessage(twsGatewayFrame(msgPnLSingle, strconv.Itoa(posReq+77), "100", "99", "99", "99"))
	conn.processMessageAtEpoch(twsGatewayFrame(msgPnLSingle, strconv.Itoa(posReq), "100", "77", "77", "77"), conn.BrokerSessionEpoch()-1)
	conn.processMessage(twsGatewayFrame(msgPnLSingle, "seven", "100", "77", "77", "77"))
	if pos, _ = c.PositionDailyPnL(4242); pos.DailyPnL == nil || *pos.DailyPnL != 12.5 {
		t.Fatalf("position P&L after stray frames = %+v", pos)
	}
}

// The display projection carries the P&L caches and the account's streamed
// values with the receive clock of its net liquidation; a later generation
// of the socket makes the capture fail closed.
func TestCaptureDisplayProjectsPnLAndAccountValues(t *testing.T) {
	c, conn, _ := histTPnLFixture(t)
	if err := c.SubscribeAccountPnL("DU7654321"); err != nil {
		t.Fatal(err)
	}
	if err := c.SubscribePositionDailyPnL("DU7654321", 4242); err != nil {
		t.Fatal(err)
	}
	acctReq, posReq := histTPnLReqIDs(t, c, 4242)
	conn.processMessage(twsGatewayFrame(msgPnL, strconv.Itoa(acctReq), "10", "20", "30"))
	conn.processMessage(twsGatewayFrame(msgPnLSingle, strconv.Itoa(posReq), "5", "1", "2", "3"))

	snapshot, ok := c.CaptureDisplay()
	if !ok || snapshot.PnLAccount != "DU7654321" || snapshot.Account != nil || !snapshot.NetLiquidationAt.IsZero() {
		t.Fatalf("display before account values = %+v, %t", snapshot, ok)
	}
	histTFloat(t, "display account daily", snapshot.AccountPnL.DailyPnL, 10)
	histTFloat(t, "display position daily", snapshot.PositionPnL[4242].DailyPnL, 1)

	conn.portfolioHealthMu.Lock()
	conn.portfolioHealth.Account = "DU7654321"
	conn.portfolioHealthMu.Unlock()
	conn.positionsMu.Lock()
	conn.positions["SYNX"] = &RawPosition{Account: "DU7654321", Position: 10, Contract: Contract{Symbol: "SYNX", ConID: 4242, ComboLegs: []ComboLeg{{ConID: 1}}}}
	conn.positionsMu.Unlock()
	conn.handleAccountValue([]string{"6", "2", "NetLiquidation", "100000", "USD", "DU7654321"})
	conn.handleAccountValue([]string{"6", "2", "TotalCashValue", "25000", "USD", "DU7654321"})
	snapshot, ok = c.CaptureDisplay()
	if !ok || snapshot.Account == nil || snapshot.Account.AccountID != "DU7654321" || snapshot.NetLiquidationAt.IsZero() || !snapshot.Account.AsOf.IsZero() {
		t.Fatalf("display with account values = %+v, %t", snapshot, ok)
	}
	histTFloat(t, "display net liquidation", snapshot.Account.NetLiquidation, 100000)
	histTFloat(t, "display cash", snapshot.Account.TotalCashValue, 25000)
	if snapshot.Health.Account != "DU7654321" || snapshot.Quotes == nil || snapshot.DataTypes == nil {
		t.Fatalf("display health/quotes = %+v", snapshot)
	}
	if len(snapshot.Positions) != 1 || snapshot.Positions[0].Contract.ConID != 4242 || snapshot.Positions[0].Position != 10 {
		t.Fatalf("display positions = %+v", snapshot.Positions)
	}
	snapshot.Positions[0].Position = 0
	snapshot.Positions[0].Contract.ComboLegs[0].ConID = 99
	if live, ok := conn.GetPosition("SYNX"); !ok || live.Position != 10 || live.Contract.ComboLegs[0].ConID != 1 {
		t.Fatalf("the display projection shares memory with the book: %+v", live)
	}

	conn.brokerSessionEpoch.Add(1)
	if _, ok := c.PositionDailyPnL(4242); ok {
		t.Fatal("a retired generation still reported position P&L")
	}
	if _, ok := c.AccountDailyPnL(); ok {
		t.Fatal("a retired generation still reported account P&L")
	}
	conn.status = StatusDisconnected
	if _, ok := c.CaptureDisplay(); ok {
		t.Fatal("a capture without a session succeeded")
	}
}

// Changing the account cancels the old stream before the new request; the
// shutdown sweep cancels every stream and empties the caches; the contract
// cap and the input gates hold.
func TestPnLAccountChangeShutdownAndLimits(t *testing.T) {
	c, _, g := histTPnLFixture(t)
	if err := c.SubscribeAccountPnL("DU1234567"); err != nil {
		t.Fatal(err)
	}
	first := g.framesByID(reqPnL)[0][1]
	if err := c.SubscribeAccountPnL("DU7654321"); err != nil {
		t.Fatal(err)
	}
	cancels := g.framesByID(cancelPnL)
	requests := g.framesByID(reqPnL)
	if len(cancels) != 1 || cancels[0][1] != first || len(requests) != 2 || requests[1][2] != "DU7654321" {
		t.Fatalf("account change: cancels=%v requests=%v", cancels, requests)
	}

	for conID := 1; conID <= 50; conID++ {
		if err := c.SubscribePositionDailyPnL("DU7654321", conID); err != nil {
			t.Fatalf("subscription %d = %v", conID, err)
		}
	}
	if err := c.SubscribePositionDailyPnL("DU7654321", 51); err == nil {
		t.Fatal("the 51st contract was accepted")
	}
	if n := c.ActiveDailyPnLSubscriptions(); n != 50 {
		t.Fatalf("active subscriptions = %d, want 50", n)
	}
	for name, err := range map[string]error{
		"blank account": c.SubscribePositionDailyPnL("", 4242),
		"zero conId":    c.SubscribePositionDailyPnL("DU7654321", 0),
		"non-ascii":     c.SubscribePositionDailyPnL("DÜ7654321", 4242),
		"blank acct":    c.SubscribeAccountPnL(""),
		"non-ascii acc": c.SubscribeAccountPnL("DÜ7654321"),
	} {
		if err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}

	acctReq, _ := histTPnLReqIDs(t, c, 7)
	c.cancelAllPnL()
	cancels = g.framesByID(cancelPnL)
	if len(cancels) != 2 || cancels[1][1] != strconv.Itoa(acctReq) {
		t.Fatalf("shutdown account cancels = %v", cancels)
	}
	if n := len(g.framesByID(cancelPnLSingle)); n != 50 {
		t.Fatalf("shutdown position cancels = %d, want 50", n)
	}
	if c.ActiveDailyPnLSubscriptions() != 0 {
		t.Fatal("shutdown left subscriptions tracked")
	}
	if _, ok := c.PositionDailyPnL(7); ok {
		t.Fatal("shutdown left a position snapshot")
	}
	if _, ok := c.AccountDailyPnL(); ok {
		t.Fatal("shutdown left the account snapshot")
	}

	c.mu.Lock()
	c.ready = false
	c.mu.Unlock()
	if err := c.SubscribeAccountPnL("DU7654321"); !errors.Is(err, ErrIBKRUnavailable) {
		t.Fatalf("subscribe without a session = %v, want ErrIBKRUnavailable", err)
	}
	if err := c.SubscribePositionDailyPnL("DU7654321", 4242); !errors.Is(err, ErrIBKRUnavailable) {
		t.Fatalf("position subscribe without a session = %v, want ErrIBKRUnavailable", err)
	}
}

// parsePositionPnLFields and parsePnLFloat: the wire's blank, non-numeric,
// NaN and DBL_MAX values are all missing, never zero.
func TestPnLWireValuesThatAreNotNumbersReadAsMissing(t *testing.T) {
	for _, raw := range []string{"", "n/a", "NaN", "1.7976931348623157E308", "-1.7976931348623157E308"} {
		if got := parsePnLFloat(raw); got != nil {
			t.Fatalf("parsePnLFloat(%q) = %v, want missing", raw, *got)
		}
	}
	if got := parsePnLFloat("-0.25"); got == nil || *got != -0.25 {
		t.Fatalf("parsePnLFloat(-0.25) = %v", got)
	}
	if _, _, ok := parsePositionPnLFields([]string{"95", "7", "100"}); ok {
		t.Fatal("a pnlSingle frame without a daily field parsed")
	}
	if _, _, ok := parsePositionPnLFields([]string{"95", "seven", "100", "1"}); ok {
		t.Fatal("a pnlSingle frame with a non-numeric request id parsed")
	}
	reqID, snap, ok := parsePositionPnLFields([]string{"95", "7", "100", "1"})
	if !ok || reqID != 7 || snap.DailyPnL == nil || snap.UnrealizedTotalPnL != nil || snap.RealizedTotalPnL != nil {
		t.Fatalf("short pnlSingle = %d %+v %t", reqID, snap, ok)
	}
	if _, status := parseDailyPnLFloat("+Inf"); status != DailyPnLFrameMalformed {
		t.Fatalf("infinite daily status = %s, want malformed", status)
	}
	if v, status := parseDailyPnLFloat("1e300"); v != nil || status != DailyPnLFrameUnavailable {
		t.Fatalf("sentinel daily = %v %s, want unavailable", v, status)
	}
}
