package ibkr

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// missClock is a settable clock for the definition-miss cache.
type missClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *missClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *missClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newMissTestConnector(t *testing.T) (*Connector, *Connection, *safeBuffer, *missClock) {
	t.Helper()
	conn, out := newReadyWireTestConnection(t)
	c := NewConnector(&ConnectorConfig{})
	c.conn.rateLimiter.Stop()
	c.conn = conn
	clock := &missClock{now: time.Now()}
	c.contractMissNow = clock.Now
	return c, conn, out, clock
}

// waitForPendingContractDetails polls until exactly one reqContractDetails is
// armed for notice-driven failure and returns its reqID.
func waitForPendingContractDetails(t *testing.T, c *Connector) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.contractDetailsMu.Lock()
		for reqID := range c.contractDetailsReqs {
			c.contractDetailsMu.Unlock()
			return reqID
		}
		c.contractDetailsMu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no pending contract-details request appeared")
	return 0
}

// A broker code-200 answer ("No security definition has been found") must
// suppress further wire resolution for the symbol until the backoff expires —
// the exact loop observed 2026-08-13, where one delisted held symbol produced
// thousands of full-rate retries because farm impairment vetoed the inactive
// mark.
func TestFetchContractDetailsDefinitionMissGatesWireRequests(t *testing.T) {
	c, conn, out, clock := newMissTestConnector(t)

	fetch := func() error {
		errCh := make(chan error, 1)
		go func() {
			_, err := c.FetchContractDetails("HGENQ", 5*time.Second)
			errCh <- err
		}()
		reqID := waitForPendingContractDetails(t, c)
		if !c.failPendingContractDetails(reqID, 200, "No security definition has been found for the request") {
			t.Fatalf("failPendingContractDetails did not own reqID %d", reqID)
		}
		select {
		case err := <-errCh:
			return err
		case <-time.After(2 * time.Second):
			t.Fatal("FetchContractDetails did not return after injected code-200")
			return nil
		}
	}

	if err := fetch(); !errors.Is(err, ErrContractNoDefinition) {
		t.Fatalf("first fetch error = %v, want ErrContractNoDefinition", err)
	}
	framesAfterFirst := len(decodeOutboundFrames(t, conn, out.Bytes()))
	if framesAfterFirst == 0 {
		t.Fatal("first fetch reached no wire request")
	}

	// Inside the backoff: no wire traffic, typed miss error that still
	// classifies as ErrContractNoDefinition.
	_, err := c.FetchContractDetails("HGENQ", time.Second)
	var miss *ContractResolutionMissError
	if !errors.As(err, &miss) || !errors.Is(err, ErrContractNoDefinition) {
		t.Fatalf("suppressed fetch error = %v, want ContractResolutionMissError", err)
	}
	if got := len(decodeOutboundFrames(t, conn, out.Bytes())); got != framesAfterFirst {
		t.Fatalf("suppressed fetch wrote wire frames: %d -> %d", framesAfterFirst, got)
	}

	// The subscribe path must not fall through to a bare reqMktData either.
	if err := c.SubscribeMarketData(context.Background(), "HGENQ", nil); !errors.Is(err, ErrContractNoDefinition) {
		t.Fatalf("SubscribeMarketData error = %v, want definition-miss", err)
	}
	if got := len(decodeOutboundFrames(t, conn, out.Bytes())); got != framesAfterFirst {
		t.Fatalf("suppressed subscribe wrote wire frames: %d -> %d", framesAfterFirst, got)
	}
	c.subMu.RLock()
	_, subExists := c.subscriptions["HGENQ"]
	c.subMu.RUnlock()
	if subExists {
		t.Fatal("suppressed subscribe still registered a local subscription")
	}

	// After expiry the symbol earns exactly one fresh probe, and a repeated
	// miss escalates the backoff instead of restarting at the floor.
	clock.Advance(contractMissBackoffFloor + time.Second)
	if err := fetch(); !errors.Is(err, ErrContractNoDefinition) {
		t.Fatalf("post-expiry fetch error = %v, want ErrContractNoDefinition", err)
	}
	if got := len(decodeOutboundFrames(t, conn, out.Bytes())); got <= framesAfterFirst {
		t.Fatal("post-expiry probe was suppressed, want one wire request")
	}
	c.contractMissMu.Lock()
	backoff := c.contractMisses["HGENQ"].backoff
	c.contractMissMu.Unlock()
	if backoff != 2*contractMissBackoffFloor {
		t.Fatalf("escalated backoff = %s, want %s", backoff, 2*contractMissBackoffFloor)
	}
}

func TestContractResolutionMissEscalatesToCapAndClearsOnSuccess(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	c.conn.rateLimiter.Stop()
	clock := &missClock{now: time.Now()}
	c.contractMissNow = clock.Now

	c.recordContractResolutionMiss("HGENQ")
	// A second failure inside the same window is the same probe: no escalation.
	c.recordContractResolutionMiss("HGENQ")
	c.contractMissMu.Lock()
	backoff := c.contractMisses["HGENQ"].backoff
	c.contractMissMu.Unlock()
	if backoff != contractMissBackoffFloor {
		t.Fatalf("same-window re-record escalated to %s, want floor %s", backoff, contractMissBackoffFloor)
	}

	for range 10 {
		clock.Advance(contractMissBackoffCap + time.Second)
		c.recordContractResolutionMiss("HGENQ")
	}
	c.contractMissMu.Lock()
	backoff = c.contractMisses["HGENQ"].backoff
	c.contractMissMu.Unlock()
	if backoff != contractMissBackoffCap {
		t.Fatalf("backoff = %s, want cap %s", backoff, contractMissBackoffCap)
	}
	if c.contractResolutionMissFor("HGENQ") == nil {
		t.Fatal("miss inside capped window not reported")
	}

	c.clearContractResolutionMiss("HGENQ")
	if c.contractResolutionMissFor("HGENQ") != nil {
		t.Fatal("cleared miss still reported")
	}
}

// A routed subscribe carrying a position-derived ConID the broker no longer
// resolves draws code 200 on the market-data request itself — no
// contract-details fetch involved — and the app's poll cycle re-subscribes
// every few seconds. The rejection must feed the definition-miss backoff even
// while a farm is impaired (the state that vetoes the inactive mark), and the
// next routed subscribe must fail fast without wire traffic.
func TestRoutedSubscribeGatedAfterDefinitionRejectionOnMarketDataReqID(t *testing.T) {
	conn, out := newReadyWireTestConnection(t)
	c := NewConnector(&ConnectorConfig{})
	c.conn.rateLimiter.Stop()
	c.conn = conn
	conn.evidenceBarrier = &c.evidenceBarrier
	conn.publicationBarrier = &c.publicationBarrier
	c.running = true
	c.ready = true
	clock := &missClock{now: time.Now()}
	c.contractMissNow = clock.Now

	binding, ok := c.CaptureSession()
	if !ok {
		t.Fatal("capture session")
	}

	contract := Contract{Symbol: "HGENQ", SecType: "STK", ConID: 555, Exchange: "SMART", Currency: "USD"}
	key := MarketDataKeyForContract(contract)
	if key == "" {
		t.Fatal("empty route key")
	}
	c.subMu.Lock()
	c.subscriptions[key] = &Subscription{Symbol: key, ReqID: 77, RejectCh: make(chan SubscriptionRejection, 1)}
	c.reqIDMap[77] = key
	c.subMu.Unlock()

	// The observed failure state: an impaired farm row, which blocks the
	// 12-hour inactive mark — but must not block the probe backoff.
	c.dataFarmMu.Lock()
	c.dataFarms = map[string]DataFarmStatus{
		dataFarmKey("historical", "ushmds"): {Name: "ushmds", Type: "historical", Status: "disconnected"},
	}
	c.dataFarmMu.Unlock()

	post := c.recoverFromSystemNotice(binding, reqAliasEntry{symbol: "HGENQ", secType: "STK"}, &systemNotification{
		tickerID: 77,
		code:     200,
		message:  "No security definition has been found for the request",
	})
	if post != nil {
		post()
	}
	if c.contractResolutionMissFor(key) == nil {
		t.Fatal("code-200 on a subscription reqID did not record a definition miss")
	}
	if c.IsSymbolInactive(key) {
		t.Fatal("impaired farm must still veto the inactive mark")
	}

	// The poll cycle released the dead subscription; the next subscribe must
	// fail fast on the miss instead of re-requesting.
	c.subMu.Lock()
	delete(c.subscriptions, key)
	delete(c.reqIDMap, 77)
	c.subMu.Unlock()
	wireBefore := out.Len()
	_, err := c.SubscribeMarketDataWithContract(context.Background(), contract, nil)
	if !errors.Is(err, ErrContractNoDefinition) {
		t.Fatalf("routed subscribe error = %v, want definition-miss", err)
	}
	if out.Len() != wireBefore {
		t.Fatal("suppressed routed subscribe still wrote wire frames")
	}
}

// Backend-link losses and restores must accumulate into one counted
// observation: 85 scattered 1100 warnings a night is not a status surface.
func TestBackendLinkCountersPairLossAndRestore(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	c.conn.rateLimiter.Stop()

	t0 := time.Now()
	c.setBackendConnectivityDown(true, t0)
	c.setBackendConnectivityDown(false, t0.Add(40*time.Second))
	c.setBackendConnectivityDown(true, t0.Add(7*time.Minute))
	// Duplicate 1100 while already down must not double-count the loss.
	c.setBackendConnectivityDown(true, t0.Add(8*time.Minute))
	c.setBackendConnectivityDown(false, t0.Add(7*time.Minute+226*time.Second))

	link := c.BackendLink()
	if link.Down {
		t.Fatal("link still down after restore")
	}
	if link.Losses != 2 {
		t.Fatalf("losses = %d, want 2", link.Losses)
	}
	if link.LastOutage != 226*time.Second {
		t.Fatalf("last outage = %s, want 3m46s", link.LastOutage)
	}
	if link.LongestOutage != 226*time.Second {
		t.Fatalf("longest outage = %s, want 3m46s", link.LongestOutage)
	}
}

// After a 1101 the gateway has already dropped every server-side ticker; the
// replay must release the old reqID slots locally instead of wire-cancelling
// them, because each futile cancel draws an error 300 ("Can't find EId").
func TestReplayAfter1101ReleasesSlotsWithoutWireCancel(t *testing.T) {
	conn, out := newReadyWireTestConnection(t)
	c := NewConnector(&ConnectorConfig{})
	c.conn.rateLimiter.Stop()
	c.conn = conn
	conn.evidenceBarrier = &c.evidenceBarrier
	conn.publicationBarrier = &c.publicationBarrier
	c.running = true
	c.ready = true

	binding, ok := c.CaptureSession()
	if !ok {
		t.Fatal("capture session")
	}

	if err := conn.acquireMarketDataSlot(context.Background(), 91); err != nil {
		t.Fatalf("seed market-data slot: %v", err)
	}
	c.subMu.Lock()
	c.subscriptions["HYG"] = &Subscription{
		Symbol:     "HYG",
		ReqID:      91,
		RejectCh:   make(chan SubscriptionRejection, 1),
		replaySpec: &mdReplaySpec{symbol: "HYG", primaryExch: "ARCA"},
	}
	c.reqIDMap[91] = "HYG"
	c.subMu.Unlock()

	replayed, dropped := c.replayMarketDataSubscriptions(binding)
	if replayed != 1 || dropped != 0 {
		t.Fatalf("replayed=%d dropped=%d, want 1/0", replayed, dropped)
	}

	frames := decodeOutboundFrames(t, conn, out.Bytes())
	for _, frame := range frames {
		if len(frame) > 0 && frame[0] == "2" {
			t.Fatalf("replay wire-cancelled a dropped reqID: %#v", frame)
		}
	}
	findOutboundFrame(t, frames, reqMktData)

	c.subMu.RLock()
	newReqID := c.subscriptions["HYG"].ReqID
	c.subMu.RUnlock()
	if newReqID == 91 || newReqID == 0 {
		t.Fatalf("replay did not adopt a fresh reqID: %d", newReqID)
	}
	if got := marketDataSlotCount(conn); got != 1 {
		t.Fatalf("market-data slots = %d, want 1 (old slot released, new held)", got)
	}
}
