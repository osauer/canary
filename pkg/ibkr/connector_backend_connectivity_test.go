package ibkr

import (
	"bufio"
	"context"
	"strings"
	"testing"
	"time"
)

// newBackendConnectivityConnector builds a Connector over a Connection whose
// writer lands in a capture buffer, mirroring the subscribe-path fixtures.
func newBackendConnectivityConnector(t *testing.T, cfg *ConnectionConfig) (*Connector, *Connection, *safeBuffer) {
	t.Helper()
	c := NewConnector(&ConnectorConfig{})
	out := &safeBuffer{}
	conn := NewConnection(cfg)
	t.Cleanup(conn.rateLimiter.Stop)
	conn.status = StatusConnected
	setServerVersionReady(conn, minServerVersionRequired)
	conn.writer = bufio.NewWriter(out)
	c.conn = conn
	c.running = true
	c.ready = true
	return c, conn, out
}

func globalNotice(code int, message string) *systemNotification {
	return &systemNotification{tickerID: -1, code: code, message: message}
}

func waitForBackendReplay(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met before deadline")
}

func (c *Connector) subReqIDForTest(key string) int {
	c.subMu.RLock()
	defer c.subMu.RUnlock()
	if sub := c.subscriptions[key]; sub != nil {
		return sub.ReqID
	}
	return 0
}

// A 1101 restore means the gateway lost every server-side subscription:
// each live shared subscription must be re-issued exactly once with its
// original wire shape, while exact-session subscriptions stay untouched.
func TestBackendConnectivity1101ReplaysSharedSubscriptions(t *testing.T) {
	c, _, out := newBackendConnectivityConnector(t, nil)

	keyMBG, err := c.SubscribeMarketDataWithContract(context.Background(), Contract{
		Symbol: "MBG", SecType: "STK", Exchange: "IBIS", Currency: "EUR",
	}, []string{"LAST"})
	if err != nil {
		t.Fatalf("subscribe MBG: %v", err)
	}
	keyIWM, err := c.SubscribeMarketDataWithContract(context.Background(), Contract{
		Symbol: "IWM", SecType: "STK", Exchange: "SMART", Currency: "USD",
	}, []string{"LAST"})
	if err != nil {
		t.Fatalf("subscribe IWM: %v", err)
	}
	oldMBG, oldIWM := c.subReqIDForTest(keyMBG), c.subReqIDForTest(keyIWM)
	if oldMBG == 0 || oldIWM == 0 {
		t.Fatalf("expected live reqIDs, got MBG=%d IWM=%d", oldMBG, oldIWM)
	}

	// An exact-session subscription (broker-write quote evidence) must not
	// be replayed; its owner fails and retries itself.
	c.subMu.Lock()
	c.subscriptions["SPY|EXACT:9"] = &Subscription{
		Symbol: "SPY|EXACT:9", ReqID: 99, SessionEpoch: 7,
		replaySpec: &mdReplaySpec{contract: Contract{Symbol: "SPY"}, genericTicks: sharedGenericTicks},
	}
	c.reqIDMap[99] = "SPY|EXACT:9"
	c.subMu.Unlock()

	c.processSystemNotice(reqAliasEntry{}, globalNotice(1100, "Connectivity between IBKR and Trader Workstation has been lost."))
	if down, _ := c.backendConnectivityDown(); !down {
		t.Fatal("1100 did not mark the backend link down")
	}
	c.processSystemNotice(reqAliasEntry{}, globalNotice(1101, "Connectivity between IBKR and Trader Workstation has been restored - data lost."))
	if down, _ := c.backendConnectivityDown(); down {
		t.Fatal("1101 did not clear the backend-down state")
	}

	waitForBackendReplay(t, 5*time.Second, func() bool {
		mbg, iwm := c.subReqIDForTest(keyMBG), c.subReqIDForTest(keyIWM)
		return mbg != 0 && mbg != oldMBG && iwm != 0 && iwm != oldIWM
	})

	c.subMu.RLock()
	newMBG := c.subscriptions[keyMBG].ReqID
	newIWM := c.subscriptions[keyIWM].ReqID
	exact := c.subscriptions["SPY|EXACT:9"].ReqID
	_, oldMBGMapped := c.reqIDMap[oldMBG]
	_, oldIWMMapped := c.reqIDMap[oldIWM]
	mappedMBG := c.reqIDMap[newMBG]
	mappedIWM := c.reqIDMap[newIWM]
	c.subMu.RUnlock()

	if exact != 99 {
		t.Fatalf("exact-session subscription was replayed; reqID=%d, want 99", exact)
	}
	if oldMBGMapped || oldIWMMapped {
		t.Fatal("old reqIDs still mapped after replay")
	}
	if mappedMBG != keyMBG || mappedIWM != keyIWM {
		t.Fatalf("reqIDMap not rebound: %q->%q, %q->%q", mappedMBG, keyMBG, mappedIWM, keyIWM)
	}
	if got := strings.Count(string(out.Bytes()), "MBG\x00"); got < 2 {
		t.Fatalf("expected a second MBG wire request after replay, saw %d frames mentioning MBG", got)
	}
}

// A 1102 restore means the gateway kept every subscription: nothing may be
// re-issued and no reqID may change.
func TestBackendConnectivity1102PreservesSubscriptions(t *testing.T) {
	c, _, out := newBackendConnectivityConnector(t, nil)

	key, err := c.SubscribeMarketDataWithContract(context.Background(), Contract{
		Symbol: "MBG", SecType: "STK", Exchange: "IBIS", Currency: "EUR",
	}, []string{"LAST"})
	if err != nil {
		t.Fatalf("subscribe MBG: %v", err)
	}
	oldReqID := c.subReqIDForTest(key)
	wireBefore := out.Len()

	c.processSystemNotice(reqAliasEntry{}, globalNotice(1100, "Connectivity between IBKR and Trader Workstation has been lost."))
	c.processSystemNotice(reqAliasEntry{}, globalNotice(1102, "Connectivity between IBKR and Trader Workstation has been restored - data maintained."))
	if down, _ := c.backendConnectivityDown(); down {
		t.Fatal("1102 did not clear the backend-down state")
	}

	time.Sleep(50 * time.Millisecond)
	if got := c.subReqIDForTest(key); got != oldReqID {
		t.Fatalf("1102 replayed the subscription: reqID %d -> %d", oldReqID, got)
	}
	if out.Len() != wireBefore {
		t.Fatalf("1102 produced %d bytes of unexpected wire traffic", out.Len()-wireBefore)
	}
}

// During a 1100 outage every order transmission must be refused as
// definitely-unsent; a 1101 restore lifts the gate.
func TestBackendConnectivity1100RefusesOrderTransmission(t *testing.T) {
	cfg := &ConnectionConfig{Host: "127.0.0.1", Port: 7497, ClientID: 41, Account: "DU7654321"}
	c, conn, _ := newBackendConnectivityConnector(t, cfg)
	setServerVersionReady(conn, minServerVerProtoBufPlaceOrder)
	conn.observeNextValidOrderID(100)
	gate := PaperOrderGate{Mode: "paper", Account: cfg.Account, Host: cfg.Host, Port: cfg.Port, ClientID: cfg.ClientID}
	binding := ConnectorSessionBinding{connector: c, connection: conn, epoch: conn.BrokerSessionEpoch()}
	contract := &Contract{Symbol: "SPY", SecType: "STK", Exchange: "SMART", Currency: "USD"}
	order := &RawOrder{Action: "BUY", TotalQty: 1, OrderType: "LMT", LmtPrice: 1, TIF: "DAY", Account: cfg.Account}

	c.processSystemNotice(reqAliasEntry{}, globalNotice(1100, "Connectivity between IBKR and Trader Workstation has been lost."))
	err := c.SubmitPaperOrderForSession(binding, gate, contract, order)
	if err == nil {
		t.Fatal("order transmission succeeded during a 1100 backend outage")
	}
	if !strings.Contains(err.Error(), "1100") {
		t.Fatalf("refusal does not name the 1100 outage: %v", err)
	}
	if brokerSendMayHaveBeenWritten(err) {
		t.Fatalf("1100 refusal must be definitely-unsent, got uncertain disposition: %v", err)
	}

	c.processSystemNotice(reqAliasEntry{}, globalNotice(1101, "Connectivity between IBKR and Trader Workstation has been restored - data lost."))
	if err := c.SubmitPaperOrderForSession(binding, gate, contract, order); err != nil {
		t.Fatalf("order transmission still refused after 1101 restore: %v", err)
	}
}

// TWS answers pending requests with per-reqID code-1100 notices during an
// outage; those must set the backend-down state exactly like the global form.
func TestBackendConnectivityRequestScoped1100SetsGate(t *testing.T) {
	c, _, _ := newBackendConnectivityConnector(t, nil)
	c.processSystemNotice(reqAliasEntry{}, &systemNotification{
		tickerID: 55, code: 1100,
		message: "Connectivity between IBKR and Trader Workstation has been lost.",
	})
	if down, _ := c.backendConnectivityDown(); !down {
		t.Fatal("request-scoped 1100 did not mark the backend link down")
	}
}
