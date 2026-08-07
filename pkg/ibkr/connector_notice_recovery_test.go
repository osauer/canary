package ibkr

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"
)

// These tests pin the system-notice recovery path. TWS API server ≥203
// delivers request-scoped errors as msg-204 system notifications, not
// msgErrMsg frames, so processSystemNotice — not handleIBKRError — is the
// only error path that actually runs against current gateways (observed
// 2026-06-11: 779 system notices, zero msgErrMsg errors, while the
// ZVZZT/354 churn loop, the cancel-after-354 error-300 loop, and the
// historical timeout-then-cancel error-366 loop all repeated every poll
// cycle).

// TestSystemNoticeFailsPendingHistorical pins the error-366 fix: a
// terminal notice targeting a pending historical request must fail it
// immediately — the caller previously burned its full timeout and then
// wire-cancelled a query the server had already killed. Error 200 must
// propagate (a caller that waits on data that will never come burns a
// full timeout).
func TestSystemNoticeFailsPendingHistorical(t *testing.T) {
	c := readyBrokerEvidenceTestConnector(t)

	req := c.createHistoricalRequest(202, "ZVZZT")
	c.processSystemNotice(reqAliasEntry{}, &systemNotification{
		tickerID: 202,
		code:     200,
		message:  "No security definition has been found for the request",
	})

	select {
	case res := <-req.result:
		var hErr *HistoricalRequestError
		if !errors.As(res.err, &hErr) {
			t.Fatalf("expected HistoricalRequestError, got %v", res.err)
		}
		if hErr.Code != 200 {
			t.Fatalf("expected code 200, got %d", hErr.Code)
		}
	default:
		t.Fatal("historical request not failed by system notice")
	}

	// 162 keeps the pacing-backoff semantics of the legacy path.
	req162 := c.createHistoricalRequest(203, "QQQ")
	c.processSystemNotice(reqAliasEntry{}, &systemNotification{
		tickerID: 203,
		code:     162,
		message:  "Historical Market Data Service error message",
	})
	res := <-req162.result
	var hErr *HistoricalRequestError
	if !errors.As(res.err, &hErr) {
		t.Fatalf("expected HistoricalRequestError, got %v", res.err)
	}
	if hErr.Code != 162 || hErr.RetryAfter != 30*time.Second {
		t.Fatalf("expected code 162 with 30s base backoff, got code=%d retryAfter=%v", hErr.Code, hErr.RetryAfter)
	}

	// Informational notices must leave a pending request running.
	reqInfo := c.createHistoricalRequest(204, "SPY")
	c.processSystemNotice(reqAliasEntry{}, &systemNotification{
		tickerID: 204,
		code:     2106,
		message:  "HMDS data farm connection is OK",
	})
	select {
	case res := <-reqInfo.result:
		t.Fatalf("informational notice must not fail the request, got %v", res.err)
	default:
	}
}

// TestSystemNotice354RemembersAbsence pins the 354+2129 churn fix: a
// terminal entitlement rejection feeds a 30-minute absence memory keyed
// by the connector's own subscription key, the exact reqID is marked
// server-dead (so teardown skips the wire cancel that drew error 300),
// and the next subscribe attempt inside the window fails fast with a
// typed error instead of re-burning a request. The window expiry re-arms
// the probe.
func TestSystemNotice354RemembersAbsence(t *testing.T) {
	c := readyBrokerEvidenceTestConnector(t)
	now := time.Date(2026, 6, 11, 19, 14, 0, 0, time.UTC)
	c.absenceNow = func() time.Time { return now }

	sub := &Subscription{Symbol: "ZVZZT", ReqID: 7}
	c.subMu.Lock()
	c.reqIDMap[7] = "ZVZZT"
	c.subscriptions["ZVZZT"] = sub
	c.subMu.Unlock()

	c.processSystemNotice(reqAliasEntry{symbol: "ZVZZT", secType: "STK"}, &systemNotification{
		tickerID: 7,
		code:     354,
		message:  "Requested market data is not subscribed.",
	})

	if sub.rejectedReqID != 7 {
		t.Fatalf("rejectedReqID = %d, want 7", sub.rejectedReqID)
	}
	if wireCancelNeeded(sub) {
		t.Fatal("wire cancel must be skipped for the server-rejected reqID")
	}

	// Simulate the holder releasing the dead line, then the next poll
	// cycle trying again: the absence memory must fail it fast.
	c.subMu.Lock()
	delete(c.subscriptions, "ZVZZT")
	c.subMu.Unlock()

	err := c.SubscribeMarketData(context.Background(), "ZVZZT", nil)
	var absent *MarketDataAbsenceError
	if !errors.As(err, &absent) {
		t.Fatalf("expected MarketDataAbsenceError, got %v", err)
	}
	if absent.Code != 354 || absent.Key != "ZVZZT" {
		t.Fatalf("absence = %+v, want code 354 key ZVZZT", absent)
	}
	if got := len(c.MarketDataSnapshot()); got != 0 {
		t.Fatalf("suppressed subscribe must not create a subscription, found %d", got)
	}

	// Past the retry window the probe re-arms.
	now = now.Add(marketDataAbsenceRetry + time.Second)
	if err := c.SubscribeMarketData(context.Background(), "ZVZZT", nil); err != nil {
		t.Fatalf("expired absence must re-arm the subscribe, got %v", err)
	}
}

// TestSystemNotice354AbsenceGuards pins the cases that must NOT feed the
// absence memory: option reqIDs (their subscription key is the
// underlying — remembering would blind the stock), derivative aliases,
// reqIDs the connector does not own (snapshot path), and notices that
// arrive while a market-data farm is impaired (a bounce-window 354 says
// nothing about entitlement, and a farm bounce does not rebuild the
// connector, so the reconnect-clears-memory path would not save an
// entitled name from a 30-minute blackout).
func TestSystemNotice354AbsenceGuards(t *testing.T) {
	note := func(reqID int) *systemNotification {
		return &systemNotification{tickerID: int64(reqID), code: 354, message: "not subscribed"}
	}

	t.Run("option reqID", func(t *testing.T) {
		c := NewConnector(&ConnectorConfig{})
		c.subMu.Lock()
		c.reqIDMap[9] = "SPY"
		c.subMu.Unlock()
		c.optMu.Lock()
		c.optReqIDs[9] = "SPY"
		c.optMu.Unlock()
		c.processSystemNotice(reqAliasEntry{symbol: "SPY", secType: "OPT"}, note(9))
		if abs := c.marketDataAbsenceFor("SPY"); abs != nil {
			t.Fatalf("option-leg 354 must not blind the underlying, got %v", abs)
		}
	})

	t.Run("derivative alias", func(t *testing.T) {
		c := NewConnector(&ConnectorConfig{})
		c.subMu.Lock()
		c.reqIDMap[9] = "SPY"
		c.subMu.Unlock()
		c.processSystemNotice(reqAliasEntry{symbol: "SPY", secType: "FOP"}, note(9))
		if abs := c.marketDataAbsenceFor("SPY"); abs != nil {
			t.Fatalf("derivative 354 must not blind the underlying, got %v", abs)
		}
	})

	t.Run("unowned reqID", func(t *testing.T) {
		c := NewConnector(&ConnectorConfig{})
		c.processSystemNotice(reqAliasEntry{symbol: "AAPL", secType: "STK"}, note(11))
		if abs := c.marketDataAbsenceFor("AAPL"); abs != nil {
			t.Fatalf("snapshot-path 354 must not record absence, got %v", abs)
		}
	})

	t.Run("impaired farm", func(t *testing.T) {
		c := NewConnector(&ConnectorConfig{})
		c.subMu.Lock()
		c.reqIDMap[12] = "AAPL"
		c.subMu.Unlock()
		c.dataFarmMu.Lock()
		c.dataFarms = map[string]DataFarmStatus{
			dataFarmKey("market", "usfarm"): {Name: "usfarm", Type: "market", Status: "disconnected"},
		}
		c.dataFarmMu.Unlock()
		c.processSystemNotice(reqAliasEntry{symbol: "AAPL", secType: "STK"}, note(12))
		if abs := c.marketDataAbsenceFor("AAPL"); abs != nil {
			t.Fatalf("bounce-window 354 must not record absence, got %v", abs)
		}
	})
}

// TestMarkSubscriptionRejectedGuardsStaleReqID pins the reviewer-required
// shape of the rejection mark: it is the rejected reqID, not a sticky
// flag, so a notice for a stale reqID cannot poison a live replacement
// subscription, and a refresh that reassigns ReqID naturally re-arms the
// wire cancel.
func TestMarkSubscriptionRejectedGuardsStaleReqID(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	sub := &Subscription{Symbol: "AMD", ReqID: 12}
	c.subMu.Lock()
	c.subscriptions["AMD"] = sub
	c.reqIDMap[7] = "AMD" // stale mapping from a previous request
	c.reqIDMap[12] = "AMD"
	c.subMu.Unlock()

	c.markSubscriptionRejected(7)
	if !wireCancelNeeded(sub) {
		t.Fatal("stale-reqID rejection must not mark the live subscription")
	}

	c.markSubscriptionRejected(12)
	if wireCancelNeeded(sub) {
		t.Fatal("matching-reqID rejection must mark the subscription")
	}

	sub.ReqID = 20 // refresh reassigned the line
	if !wireCancelNeeded(sub) {
		t.Fatal("a reassigned reqID must re-arm the wire cancel")
	}
}

// TestSystemNotice10197ForcesDelayed pins the force-delayed parity: the
// competing-live-session side effect used to live only on the dead
// msgErrMsg path.
func TestSystemNotice10197ForcesDelayed(t *testing.T) {
	c := readyBrokerEvidenceTestConnector(t)
	c.processSystemNotice(reqAliasEntry{symbol: "SPY", secType: "STK"}, &systemNotification{
		tickerID: 31,
		code:     10197,
		message:  "No market data during competing live session",
	})
	if !c.conn.HasCompetingLiveSession() {
		t.Fatal("10197 notice must flag the competing live session")
	}
}

// TestSystemNotice10197ReplaysRejectedSharedSubscription pins the user-facing
// first-symbol failure: reqMarketDataType(3) applies only to later requests,
// while 10197 has already killed the request that triggered the switch. The
// connector must therefore rebind that same shared subscription to one new
// reqID without aborting its in-flight poller.
func TestSystemNotice10197ReplaysRejectedSharedSubscription(t *testing.T) {
	c, conn, out := newBackendConnectivityConnector(t, nil)
	if !c.SeedContractDetails("SPY", ContractDetailsLite{
		Symbol: "SPY", SecType: "STK", Exchange: "SMART", PrimaryExch: "ARCA",
		Currency: "USD", ConID: 756733, TradingClass: "SPY",
	}) {
		t.Fatal("seed SPY contract details")
	}
	if err := c.SubscribeMarketData(context.Background(), "SPY", []string{"LAST"}); err != nil {
		t.Fatalf("subscribe SPY: %v", err)
	}
	oldReqID := c.subReqIDForTest("SPY")
	if oldReqID == 0 {
		t.Fatal("SPY subscription has no initial reqID")
	}
	rejectCh := c.SubscriptionRejectCh("SPY")
	if rejectCh == nil {
		t.Fatal("SPY subscription has no rejection channel")
	}
	wireBefore := out.Len()

	c.processSystemNotice(reqAliasEntry{symbol: "SPY", secType: "STK"}, &systemNotification{
		tickerID: int64(oldReqID), code: 10197, message: "untrusted competing-session detail",
	})

	newReqID := c.subReqIDForTest("SPY")
	if newReqID == 0 || newReqID == oldReqID {
		t.Fatalf("SPY reqID=%d after 10197, want replacement for %d", newReqID, oldReqID)
	}
	select {
	case rejection := <-rejectCh:
		t.Fatalf("transparent 10197 replay aborted the original poller: %+v", rejection)
	default:
	}

	payload := out.Bytes()
	frames := decodeOutboundFrames(t, conn, payload[wireBefore:])
	if len(frames) != 2 || len(frames[0]) < 3 || len(frames[1]) < 3 {
		t.Fatalf("10197 recovery frames=%#v, want delayed-mode switch then one market-data replay", frames)
	}
	if frames[0][0] != strconv.Itoa(reqMarketDataType) || frames[0][2] != "3" {
		t.Fatalf("first 10197 recovery frame=%#v, want reqMarketDataType(3)", frames[0])
	}
	if frames[1][0] != strconv.Itoa(reqMktData) || frames[1][2] != strconv.Itoa(newReqID) {
		t.Fatalf("second 10197 recovery frame=%#v, want reqMktData for %d", frames[1], newReqID)
	}

	c.subMu.RLock()
	_, oldMapped := c.reqIDMap[oldReqID]
	newKey := c.reqIDMap[newReqID]
	sub := c.subscriptions["SPY"]
	c.subMu.RUnlock()
	if oldMapped || newKey != "SPY" || sub == nil || sub.rejectedReqID == newReqID {
		t.Fatalf("SPY replay mapping oldMapped=%v newKey=%q sub=%+v", oldMapped, newKey, sub)
	}
	conn.marketDataSlotsMu.Lock()
	_, oldSlot := conn.marketDataSlots[oldReqID]
	_, newSlot := conn.marketDataSlots[newReqID]
	slotCount := len(conn.marketDataSlots)
	conn.marketDataSlotsMu.Unlock()
	if oldSlot || !newSlot || slotCount != 1 {
		t.Fatalf("SPY replay slots old=%v new=%v count=%d, want only replacement", oldSlot, newSlot, slotCount)
	}

	epoch := conn.BrokerSessionEpoch()
	if !conn.processMarketDataTypeAtEpoch([]string{"58", "1", strconv.Itoa(newReqID), "3"}, epoch) {
		t.Fatal("delayed market-data type notice was not accepted for replacement")
	}
	c.handleTickPrice([]string{"1", "2", strconv.Itoa(newReqID), "68", "773.10"})
	quote := c.MarketDataSnapshot()["SPY"]
	if quote == nil || quote.Last != 773.10 || c.MarketDataTypeForSymbol("SPY") != 3 {
		t.Fatalf("SPY delayed replay quote=%+v dataType=%d", quote, c.MarketDataTypeForSymbol("SPY"))
	}

	duplicateBefore := out.Len()
	c.processSystemNotice(reqAliasEntry{symbol: "SPY", secType: "STK"}, &systemNotification{
		tickerID: int64(oldReqID), code: 10197, message: "duplicate old request",
	})
	if got := out.Len(); got != duplicateBefore {
		t.Fatalf("duplicate old 10197 wrote %d bytes after successful replay", got-duplicateBefore)
	}

	c.processSystemNotice(reqAliasEntry{symbol: "SPY", secType: "STK"}, &systemNotification{
		tickerID: int64(newReqID), code: 10197, message: "replacement also rejected",
	})
	if got := out.Len(); got != duplicateBefore {
		t.Fatalf("second current 10197 wrote %d bytes, want bounded single replay", got-duplicateBefore)
	}
	select {
	case rejection := <-rejectCh:
		if rejection.Code != 10197 {
			t.Fatalf("second 10197 rejection=%+v, want typed 10197", rejection)
		}
	default:
		t.Fatal("second current 10197 did not terminate the bounded poller")
	}
}

// TestSystemNoticeOrderIDCollisionSkipsRequestRecovery pins the msg-204
// id-space guard: order errors and request errors arrive through the same
// id field, and TWS order IDs (nextValidId) are independent from the
// connection's reqIDSeq. A rejection for an order this connector owns must
// not fail an innocent colliding historical request, mark a live
// subscription server-dead (releasing a slot it still holds), or strike
// the symbol's inactive-candidate counter.
func TestSystemNoticeOrderIDCollisionSkipsRequestRecovery(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	defer c.conn.rateLimiter.Stop()

	c.orderMu.Lock()
	c.brokerOrderIndex["202"] = "ibkr-20260611-202"
	c.orderMu.Unlock()

	req := c.createHistoricalRequest(202, "AAPL")
	sub := &Subscription{Symbol: "AAPL", ReqID: 202}
	c.subMu.Lock()
	c.reqIDMap[202] = "AAPL"
	c.subscriptions["AAPL"] = sub
	c.subMu.Unlock()

	c.processSystemNotice(reqAliasEntry{symbol: "AAPL", secType: "STK"}, &systemNotification{
		tickerID: 202,
		code:     200,
		message:  "No security definition has been found for the request",
	})

	select {
	case res := <-req.result:
		t.Fatalf("order-scoped notice must not fail the colliding historical request, got %v", res.err)
	default:
	}
	if sub.rejectedReqID != 0 {
		t.Fatalf("order-scoped notice must not mark the colliding subscription rejected (rejectedReqID=%d)", sub.rejectedReqID)
	}
	c.inactiveMu.RLock()
	candidates := len(c.inactiveCandidates)
	c.inactiveMu.RUnlock()
	if candidates != 0 {
		t.Fatalf("order-scoped notice must not strike inactive candidates, got %d", candidates)
	}
}

func TestConnectionLossRetainsSystemNoticeHandlerForLateOrderErrorReceipt(t *testing.T) {
	c := readyBrokerEvidenceTestConnector(t)
	conn := c.conn
	conn.serverVersion = 203
	conn.config.AutoReconnect = false
	c.attachConnectionHooks(conn)
	epoch := conn.BrokerSessionEpoch()
	c.orderMu.Lock()
	c.brokerOrderIndex["91"] = "late-retired-order"
	c.orderMu.Unlock()
	receipts := make(chan OrderLifecycleReceipt, 1)
	c.RegisterOrderLifecycleReceiptHandler(func(receipt OrderLifecycleReceipt) {
		receipts <- receipt
	})
	// onConnectionLost is the Connector retirement boundary. A decoded frame
	// may still arrive after it because Connection.Stop uses a bounded drain.
	conn.handleDisconnection(errors.New("test connection retirement"))
	if conn.Status() != StatusDisconnected {
		t.Fatalf("connection status = %s, want disconnected retirement", conn.Status())
	}
	conn.processMessageAtEpoch(encodeSystemNotificationForTest(91, 201, "order rejected", ""), epoch)

	select {
	case receipt := <-receipts:
		if receipt.Session.connector != c || receipt.Session.connection != conn || receipt.Session.epoch != epoch {
			t.Fatalf("late receipt session = %+v, want retired Connector/Connection epoch %d", receipt.Session, epoch)
		}
		if receipt.Event.Type != OrderLifecycleEventError || receipt.Event.OrderID != 91 || receipt.Event.ErrorCode != 201 {
			t.Fatalf("late msg-204 receipt = %+v", receipt.Event)
		}
		if c.SessionReceiptCurrent(receipt.Session) {
			t.Fatal("retired Connection receipt was accepted as current")
		}
	case <-time.After(time.Second):
		t.Fatal("late msg-204 order error was buffered instead of reaching the retained receipt handler")
	}
	conn.systemNoticeMu.RLock()
	handlerRetained := conn.systemNoticeHandler != nil
	conn.systemNoticeMu.RUnlock()
	if !handlerRetained {
		t.Fatal("retired system notice handler was cleared")
	}
}

// TestStaleRefreshAfterServerRejectionKeepsSlotAccounting pins the
// EnsureMarketDataSubscription stale-refresh seam: once the notice path
// has released a rejected reqID's rate-limiter slot, a later stale
// refresh across a disconnect must go through the idempotent per-reqID
// release. The raw rateLimiter release this path used to make would
// double-release and panic the semaphore ("Release called without
// matching Acquire").
func TestStaleRefreshAfterServerRejectionKeepsSlotAccounting(t *testing.T) {
	c := readyBrokerEvidenceTestConnector(t)
	ctx := context.Background()

	if err := c.conn.rateLimiter.AcquireMarketDataSlot(ctx); err != nil {
		t.Fatalf("acquire slot: %v", err)
	}
	c.conn.marketDataSlotsMu.Lock()
	c.conn.marketDataSlots[9] = c.conn.BrokerSessionEpoch()
	c.conn.marketDataSlotsMu.Unlock()

	sub := &Subscription{Symbol: "ZVZZT", ReqID: 9, LastTime: time.Now().Add(-time.Hour)}
	c.subMu.Lock()
	c.reqIDMap[9] = "ZVZZT"
	c.subscriptions["ZVZZT"] = sub
	c.subMu.Unlock()

	// Terminal definition rejection: the notice path releases the slot and
	// marks the exact reqID server-dead.
	c.processSystemNotice(reqAliasEntry{symbol: "ZVZZT", secType: "STK"}, &systemNotification{
		tickerID: 9,
		code:     200,
		message:  "No security definition has been found for the request",
	})
	if got := c.conn.rateLimiter.marketDataSubs.Count(); got != 0 {
		t.Fatalf("notice path must release the slot, count=%d", got)
	}

	// The refresh fails (no live session) — but it must not over-release.
	if _, err := c.EnsureMarketDataSubscription(ctx, "ZVZZT", nil, time.Millisecond); err == nil {
		t.Fatal("expected refresh to fail without a live session")
	}
	if got := c.conn.rateLimiter.marketDataSubs.Count(); got != 0 {
		t.Fatalf("stale refresh must keep slot accounting at zero, count=%d", got)
	}
}
