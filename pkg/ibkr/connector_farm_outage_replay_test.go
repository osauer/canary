package ibkr

import (
	"testing"
	"time"
)

// farmOutage20260729Sequence replays the exact cid=15 notice order from the
// 2026-07-29 16:22-16:24 TWS outage (ibkr-daemon.log): boot all-OK wave,
// break/heal flapping that leaves every farm OK by 16:22:41, then code=1100
// connectivity-lost immediately followed by the queued code=200 answers for
// pending reqMktData resubscriptions. The per-farm break notices for the real
// outage arrived only at 16:24:20-22, AFTER the 200 burst, so the farm map
// alone reads healthy at burst time — 1100 is the only impairment signal.
var farmOutage20260729Sequence = []struct {
	code int
	msg  string
}{
	// 16:22:05 boot wave.
	{2104, "Market data farm connection is OK:hfarm"},
	{2104, "Market data farm connection is OK:eufarmnj"},
	{2104, "Market data farm connection is OK:cashfarm"},
	{2104, "Market data farm connection is OK:usfuture"},
	{2104, "Market data farm connection is OK:jfarm"},
	{2104, "Market data farm connection is OK:usfarm.nj"},
	{2104, "Market data farm connection is OK:eufarm"},
	{2104, "Market data farm connection is OK:usopt"},
	{2104, "Market data farm connection is OK:usfarm"},
	{2106, "HMDS data farm connection is OK:euhmds"},
	{2106, "HMDS data farm connection is OK:cashhmds"},
	{2106, "HMDS data farm connection is OK:fundfarm"},
	{2106, "HMDS data farm connection is OK:ushmds"},
	{2158, "Sec-def data farm connection is OK:secdefeu"},
	// 16:22:37-41 break/heal flap: every break is healed within seconds.
	{2103, "Market data farm connection is broken:cashfarm"},
	{2103, "Market data farm connection is broken:eufarmnj"},
	{2105, "HMDS data farm connection is broken:cashhmds"},
	{2105, "HMDS data farm connection is broken:euhmds"},
	{2157, "Sec-def data farm connection is broken:secdefeu"},
	{2105, "HMDS data farm connection is broken:ushmds"},
	{2103, "Market data farm connection is broken:usfarm"},
	{2106, "HMDS data farm connection is OK:euhmds"},
	{2106, "HMDS data farm connection is OK:cashhmds"},
	{2158, "Sec-def data farm connection is OK:secdefeu"},
	{2105, "HMDS data farm connection is broken:fundfarm"},
	{2103, "Market data farm connection is broken:eufarm"},
	{2104, "Market data farm connection is OK:cashfarm"},
	{2103, "Market data farm connection is broken:usopt"},
	{2103, "Market data farm connection is broken:usfuture"},
	{2103, "Market data farm connection is broken:jfarm"},
	{2104, "Market data farm connection is OK:eufarmnj"},
	{2103, "Market data farm connection is broken:usfarm.nj"},
	{2106, "HMDS data farm connection is OK:ushmds"},
	{2106, "HMDS data farm connection is OK:fundfarm"},
	{2104, "Market data farm connection is OK:eufarm"},
	{2104, "Market data farm connection is OK:usfarm"},
	{2104, "Market data farm connection is OK:usopt"},
	{2104, "Market data farm connection is OK:usfuture"},
	{2103, "Market data farm connection is broken:hfarm"},
	{2104, "Market data farm connection is OK:usfarm.nj"},
	{2104, "Market data farm connection is OK:jfarm"},
	{2104, "Market data farm connection is OK:hfarm"},
	// 16:24:17.251 — the only signal preceding the queued 200 burst.
	{1100, "Connectivity between IBKR and Trader Workstation has been lost."},
}

const outageReplayIWMKey = "IWM|STK|SMART|ARCA|USD||"

func seedOutageReplaySubscriptions(c *Connector) {
	c.subMu.Lock()
	c.reqIDMap[259] = outageReplayIWMKey
	c.reqIDMap[311] = outageReplayIWMKey
	c.subscriptions[outageReplayIWMKey] = &Subscription{Symbol: "IWM", ReqID: 311}
	c.subMu.Unlock()
}

func TestFarmOutageReplay20260729(t *testing.T) {
	c := readyBrokerEvidenceTestConnector(t)
	now := time.Now()
	seedOutageReplaySubscriptions(c)

	for _, n := range farmOutage20260729Sequence {
		c.processSystemNotice(reqAliasEntry{}, &systemNotification{
			tickerID: -1, code: n.code, message: n.msg, timestamp: now,
		})
	}

	// 16:24:17.252 — queued 200 answers for the resubscriptions.
	for _, reqID := range []int64{259, 311} {
		c.processSystemNotice(reqAliasEntry{symbol: "IWM", secType: "STK"}, &systemNotification{
			tickerID: reqID, code: 200,
			message:   "No security definition has been found for the request",
			timestamp: now,
		})
	}

	if c.IsSymbolInactive(outageReplayIWMKey) {
		reason, _ := c.InactiveReason(outageReplayIWMKey)
		t.Fatalf("IWM marked inactive during connectivity-lost window; reason=%q", reason)
	}
}

// Sanity leg: the same burst with healthy farms and no connectivity loss must
// mark, otherwise the replay above passes vacuously.
func TestFarmOutageReplaySanityMarksWhenHealthy(t *testing.T) {
	c := readyBrokerEvidenceTestConnector(t)
	now := time.Now()
	seedOutageReplaySubscriptions(c)

	for _, reqID := range []int64{259, 311} {
		c.processSystemNotice(reqAliasEntry{symbol: "IWM", secType: "STK"}, &systemNotification{
			tickerID: reqID, code: 200,
			message:   "No security definition has been found for the request",
			timestamp: now,
		})
	}
	if !c.IsSymbolInactive(outageReplayIWMKey) {
		t.Fatal("healthy-farm burst must mark; replay harness is not exercising the marking path")
	}
}

// Wire-level leg: same sequence through real msg-204 frames and the
// connection's epoch/lease machinery, not the connector-level helper.
func TestFarmOutageReplayWireLevel(t *testing.T) {
	conn, c, _, _, _ := newQueuedInstructionReconnectFixture(t)
	c.registerHandlers(conn)
	seedOutageReplaySubscriptions(c)
	c.conn.registerReqAlias(259, Contract{Symbol: "IWM", SecType: "STK"})
	c.conn.registerReqAlias(311, Contract{Symbol: "IWM", SecType: "STK"})

	for _, n := range farmOutage20260729Sequence {
		conn.processMessage(encodeSystemNotificationForTest(-1, n.code, n.msg, ""))
	}
	conn.processMessage(encodeSystemNotificationForTest(259, 200, "No security definition has been found for the request", ""))
	conn.processMessage(encodeSystemNotificationForTest(311, 200, "No security definition has been found for the request", ""))

	if c.IsSymbolInactive(outageReplayIWMKey) {
		t.Fatal("wire-level replay marked IWM inactive during connectivity-lost window")
	}
}
