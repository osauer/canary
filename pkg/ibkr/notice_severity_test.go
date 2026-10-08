package ibkr

import (
	"encoding/binary"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Cancel echoes (300) are debug-grade whenever they arrive; those drawn while
// the backend link is broken are counted for the restore bookend, those
// outside a break are not.
func TestCancelEchoDuringBackendBreakStaysInDebug(t *testing.T) {
	buf := captureConnectorLogs(t)
	conn, _ := newReadyWireTestConnection(t)
	down := true
	conn.setNoticeLogContext(noticeLogContext{backendLinkDown: func() bool { return down }})
	epoch := conn.BrokerSessionEpoch()
	for range 3 {
		conn.processSystemNoticeMessageAtEpoch(syntheticSystemNotice(7, 300), epoch)
	}
	if lines := logLines(buf, "code=300"); len(lines) != 0 {
		t.Fatalf("cancel echoes logged above debug while the link was broken: %v", lines)
	}
	if got := conn.takeSuppressedCancelEchoes(); got != 3 {
		t.Fatalf("suppressed echoes=%d want 3", got)
	}
	down = false
	conn.processSystemNoticeMessageAtEpoch(syntheticSystemNotice(7, 300), epoch)
	if lines := logLines(buf, "code=300"); len(lines) != 0 {
		t.Fatalf("standalone cancel echo logged above debug: %v", lines)
	}
	if conn.takeSuppressedCancelEchoes() != 0 {
		t.Fatal("echo outside a break counted toward the restore bookend")
	}
}

// A 354 that repeats an entitlement gap already warned about is an INFO echo.
func TestRepeatEntitlementGapProbeLogsInfo(t *testing.T) {
	buf := captureConnectorLogs(t)
	conn, _ := newReadyWireTestConnection(t)
	known := false
	conn.setNoticeLogContext(noticeLogContext{knownEntitlementGap: func(int, reqAliasEntry) bool { return known }})
	epoch := conn.BrokerSessionEpoch()
	conn.processSystemNoticeMessageAtEpoch(syntheticSystemNotice(9, 354), epoch)
	known = true
	conn.processSystemNoticeMessageAtEpoch(syntheticSystemNotice(9, 354), epoch)
	lines := logLines(buf, "code=354")
	if len(lines) != 2 || !strings.Contains(lines[0], "level=WARN") || !strings.Contains(lines[1], "level=INFO") || !strings.Contains(lines[1], "repeat probe") {
		t.Fatalf("entitlement gap severities: %v", lines)
	}
}

// The connector remembers a warned 354 per subscription key for a day and
// hands that memory, with the live absence window, to its successor.
func TestConnectorEntitlementGapMemoryAndHandover(t *testing.T) {
	now := time.Date(2026, 10, 2, 9, 22, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	newConn := func() *Connector {
		c := NewConnector(&ConnectorConfig{})
		c.conn.rateLimiter.Stop()
		c.absenceNow = clock
		c.reqIDMap[11] = "NDX"
		return c
	}
	alias := reqAliasEntry{symbol: "NDX", secType: "IND"}
	c := newConn()
	if c.knownEntitlementGap(11, alias) {
		t.Fatal("gap known before any rejection")
	}
	c.rememberMarketDataAbsence("NDX", 354, "Requested market data is not subscribed")
	if !c.knownEntitlementGap(11, alias) {
		t.Fatal("warned gap not remembered")
	}
	if c.knownEntitlementGap(12, alias) {
		t.Fatal("unowned reqID matched")
	}
	successor := newConn()
	successor.InheritMarketDataMemory(c.ExportMarketDataMemory())
	if !successor.knownEntitlementGap(11, alias) {
		t.Fatal("successor forgot the warned gap")
	}
	if successor.marketDataAbsenceFor("NDX") == nil {
		t.Fatal("successor lost the absence window")
	}
	// A new process gets the warned gaps alone, as the daemon records them:
	// the gap stays INFO, the subscription is asked again.
	restarted := newConn()
	restarted.InheritMarketDataMemory(MarketDataMemory{}.WithEntitlementGaps(c.ExportMarketDataMemory().EntitlementGaps()))
	if !restarted.knownEntitlementGap(11, alias) || restarted.marketDataAbsenceFor("NDX") != nil {
		t.Fatal("restart seed: want the gap known and the subscription re-probed")
	}
	now = now.Add(marketDataAbsenceRetry)
	if successor.marketDataAbsenceFor("NDX") != nil {
		t.Fatal("absence window outlived its retry")
	}
	if !successor.knownEntitlementGap(11, alias) {
		t.Fatal("gap memory ended with the retry window")
	}
	now = now.Add(entitlementGapMemory)
	if successor.knownEntitlementGap(11, alias) {
		t.Fatal("gap memory did not expire")
	}
	var nilConnector *Connector
	nilConnector.InheritMarketDataMemory(MarketDataMemory{})
	if m := nilConnector.ExportMarketDataMemory(); len(m.absences) != 0 || len(m.gapWarned) != 0 {
		t.Fatal("nil connector exported memory")
	}
}

// 2110 marks the link broken for notice severity until a connectivity
// restore; an ordinary farm OK does not clear it, the 1100 latch also counts.
func TestBackendLinkBrokenFollowsConnectivityNotices(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	c.conn.rateLimiter.Stop()
	at := time.Date(2026, 10, 3, 9, 56, 0, 0, time.UTC)
	if c.backendLinkBroken() {
		t.Fatal("fresh connector reports a broken link")
	}
	c.recordDataFarmNotice(2110, "Connectivity between Trader Workstation and server is broken. It will be restored automatically.", at)
	if !c.backendLinkBroken() {
		t.Fatal("2110 not observed")
	}
	c.recordDataFarmNotice(2104, "Market data farm connection is OK:usfarm", at.Add(time.Second))
	if !c.backendLinkBroken() {
		t.Fatal("a farm OK cleared the link break")
	}
	c.recordDataFarmNotice(1102, "Connectivity between IB and Trader Workstation has been restored - data maintained.", at.Add(time.Minute))
	if c.backendLinkBroken() {
		t.Fatal("1102 did not clear the link break")
	}
	c.setBackendConnectivityDown(true, at.Add(2*time.Minute))
	if !c.backendLinkBroken() {
		t.Fatal("1100 latch not observed")
	}
}

// The restore bookend accounts for the echoes kept at debug during the break.
func TestBackendRestoreReportsSuppressedCancelEchoes(t *testing.T) {
	buf := captureConnectorLogs(t)
	c := NewConnector(&ConnectorConfig{})
	c.conn.rateLimiter.Stop()
	conn, _ := newReadyWireTestConnection(t)
	c.conn = conn
	conn.setNoticeLogContext(c.noticeLogContext())
	t0 := time.Date(2026, 10, 2, 4, 27, 0, 0, time.UTC)
	c.setBackendConnectivityDown(true, t0)
	epoch := conn.BrokerSessionEpoch()
	for range 5 {
		conn.processSystemNoticeMessageAtEpoch(syntheticSystemNotice(7, 300), epoch)
	}
	if lines := logLines(buf, "code=300"); len(lines) != 0 {
		t.Fatalf("echoes escaped debug: %v", lines)
	}
	c.recordBackendConnectivity(false, t0.Add(79*time.Minute), 1102)
	restored := logLines(buf, "TWS restored connectivity")
	if len(restored) != 1 || !strings.Contains(restored[0], "5 cancel echoes (code 300) kept in debug logs") {
		t.Fatalf("restore bookend did not account for echoes: %v", restored)
	}
}

const notSubscribed354 = "Requested market data is not subscribed. Check API status by selecting the Account menu then under Management choose Market Data Subscription Manager and/or Market Data Subscriptions."

// wiredNoticeConnector wires a connector's notice handling the way
// registerConnectionHandlers does, so a wire notice runs severity and memory.
func wiredNoticeConnector(t *testing.T) (*Connector, *Connection, uint64) {
	t.Helper()
	conn, c, _, _, _ := newQueuedInstructionReconnectFixture(t)
	conn.brokerSessionEpoch.Store(1)
	if err := c.SetMarketDataType(2); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.invalidateUnstampedConnectorObservations(conn) })
	conn.SetSystemNoticeHandlerAtEpochWithPostAction(func(note *systemNotification, alias reqAliasEntry, epoch uint64) func() {
		return c.processSystemNoticeFrom(ConnectorSessionBinding{connector: c, connection: conn, epoch: epoch}, alias, note)
	})
	conn.setNoticeLogContext(c.noticeLogContext())
	return c, conn, conn.BrokerSessionEpoch()
}

func subscriptionReqID(c *Connector, key string) int {
	c.subMu.RLock()
	defer c.subMu.RUnlock()
	return c.subscriptions[key].ReqID
}

// timestampedSystemNotice encodes a msg-204 notice as current gateways send
// it: ticker id (-1 for farm notices), an epoch-ms timestamp, code and text.
func timestampedSystemNotice(id int64, at time.Time, code int, message string) []string {
	tid := uint64(id)
	if id < 0 {
		tid = math.MaxUint64
	}
	payload := binary.AppendUvarint(nil, 1<<3)
	payload = binary.AppendUvarint(payload, tid)
	payload = binary.AppendUvarint(payload, 2<<3)
	payload = binary.AppendUvarint(payload, uint64(at.UnixMilli()))
	payload = binary.AppendUvarint(payload, 3<<3)
	payload = binary.AppendUvarint(payload, uint64(code))
	payload = binary.AppendUvarint(payload, 4<<3|2)
	payload = binary.AppendUvarint(payload, uint64(len(message)))
	payload = append(payload, message...)
	return []string{strconv.Itoa(msgSystemNotification), string(payload)}
}

// The 2026-10-07 shape: a held-line quote draws a 354 while an unrelated
// farm is broken, or within the settle window after one recovered. The line
// warns, and the gap is remembered and exported for the daemon, so the next
// probe is INFO; the absence verdict stays vetoed, since a bounce-window
// rejection is no verdict on the contract.
func TestEntitlementGapRememberedWhileAFarmIsImpaired(t *testing.T) {
	at := time.Date(2026, 10, 7, 17, 43, 37, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		farms []string
	}{
		{"unrelated farm broken", []string{"broken:fundfarm"}},
		{"farm recovery settling", []string{"broken:euhmds", "OK:euhmds"}},
	} {
		for _, contract := range []Contract{
			{ConID: 900101, Symbol: "SYNTHEU", SecType: "STK", Exchange: "SMART", Currency: "EUR"},
			{ConID: 900102, Symbol: "EUR", SecType: "CASH", Exchange: "IDEALPRO", Currency: "USD"},
			{ConID: 900103, Symbol: "SYNTHBILL", SecType: "BILL", Exchange: "SMART", Currency: "EUR"},
		} {
			t.Run(tc.name+"/"+contract.SecType, func(t *testing.T) {
				buf := captureConnectorLogs(t)
				c, conn, epoch := wiredNoticeConnector(t)
				for _, farm := range tc.farms {
					code := 2105
					if strings.HasPrefix(farm, "OK") {
						code = 2106
					}
					conn.processSystemNoticeMessageAtEpoch(timestampedSystemNotice(-1, at.Add(-20*time.Second), code, "HMDS data farm connection is "+farm), epoch)
				}
				if !c.marketDataFarmImpaired() {
					t.Fatal("fixture: no farm impairment")
				}
				key, err := c.SubscribeMarketDataWithContract(t.Context(), contract, nil)
				if err != nil {
					t.Fatal(err)
				}
				conn.processSystemNoticeMessageAtEpoch(timestampedSystemNotice(int64(subscriptionReqID(c, key)), at, 354, notSubscribed354), epoch)
				if _, ok := c.ExportMarketDataMemory().EntitlementGaps()[key]; !ok {
					t.Fatal("the warned gap was not remembered for the daemon")
				}
				if c.marketDataAbsenceFor(key) != nil {
					t.Fatal("a rejection during a farm impairment formed an absence verdict")
				}
				if err := c.UnsubscribeMarketData(key); err != nil {
					t.Fatal(err)
				}
				if _, err := c.SubscribeMarketDataWithContract(t.Context(), contract, nil); err != nil {
					t.Fatal(err)
				}
				conn.processSystemNoticeMessageAtEpoch(timestampedSystemNotice(int64(subscriptionReqID(c, key)), at.Add(time.Minute), 354, notSubscribed354), epoch)
				lines := logLines(buf, "code=354")
				if len(lines) != 2 || !strings.Contains(lines[0], "level=WARN") || !strings.Contains(lines[1], "level=INFO") || !strings.Contains(lines[1], "repeat probe") {
					t.Fatalf("354 severities, want WARN then INFO repeat probe: %v", lines)
				}
			})
		}
	}
}

// Exact-session quotes (order and bond previews) key each request with its
// own sequence. The gap memory keys the instrument: the second preview's 354
// is INFO and one key per instrument is exported. The absence verdict stays
// per request, so the next preview is still asked.
func TestExactSessionEntitlementGapIsOnePerInstrument(t *testing.T) {
	for _, contract := range []Contract{
		{ConID: 777001, Symbol: "SYNTHBILL", SecType: "BILL", Exchange: "SMART", Currency: "EUR"},
		{ConID: 4242, Symbol: "SYNTH", SecType: "STK", Exchange: "SMART", Currency: "USD"},
		{Symbol: "EUR", SecType: "CASH", Exchange: "IDEALPRO", Currency: "USD"},
	} {
		t.Run(contract.SecType, func(t *testing.T) {
			buf := captureConnectorLogs(t)
			c, conn, epoch := wiredNoticeConnector(t)
			binding, ok := c.CaptureSession()
			if !ok {
				t.Fatal("no session")
			}
			instrument := MarketDataKeyForContract(normalizeMarketDataContract(contract))
			for range 2 {
				key, err := c.SubscribeMarketDataWithContractForSession(t.Context(), binding, contract, nil)
				if err != nil {
					t.Fatal(err)
				}
				conn.processSystemNoticeMessageAtEpoch(syntheticSystemNoticeText(subscriptionReqID(c, key), 354, notSubscribed354), epoch)
				if !strings.Contains(key, "|EXACT:") {
					t.Fatalf("fixture: %q is not an exact-session key", key)
				}
				if c.marketDataAbsenceFor(key) == nil {
					t.Fatalf("the rejection of %s formed no per-request absence", key)
				}
				_ = c.UnsubscribeMarketDataForSession(t.Context(), binding, key)
			}
			lines := logLines(buf, "code=354")
			if len(lines) != 2 || !strings.Contains(lines[0], "level=WARN") || !strings.Contains(lines[1], "level=INFO") {
				t.Fatalf("354 severities, want WARN then INFO: %v", lines)
			}
			gaps := c.ExportMarketDataMemory().EntitlementGaps()
			if _, ok := gaps[instrument]; !ok || len(gaps) != 1 {
				t.Fatalf("exported gaps = %v, want the instrument key %q alone", gaps, instrument)
			}
			if c.marketDataAbsenceFor(instrument) != nil {
				t.Fatal("an exact-session rejection formed an instrument-wide absence")
			}
			if _, err := c.SubscribeMarketDataWithContractForSession(t.Context(), binding, contract, nil); err != nil {
				t.Fatalf("next exact-session quote refused: %v", err)
			}
		})
	}
}

// A 354 drawn by the delayed fallback's own request is no absence verdict on
// the contract: the live refusal already decided the line. It still warned,
// so the gap memory records it under the line's key and exports it to the
// daemon, and the next probe of that gap is INFO.
func TestDelayedFallback354RemembersTheGapWithoutAnAbsence(t *testing.T) {
	buf := captureConnectorLogs(t)
	c, conn, epoch := wiredNoticeConnector(t)
	contract := Contract{ConID: 900104, Symbol: "SYNTHDLY", SecType: "STK", Exchange: "SMART", Currency: "USD"}
	key, err := c.SubscribeMarketDataWithContract(t.Context(), contract, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.subMu.Lock()
	c.subscriptions[key].delayedFallback = true // the current request is the delayed fallback's
	c.subMu.Unlock()
	if c.marketDataFarmImpaired() {
		t.Fatal("fixture: a farm is impaired; the farm branch would decide instead")
	}
	conn.processSystemNoticeMessageAtEpoch(syntheticSystemNoticeText(subscriptionReqID(c, key), 354, notSubscribed354), epoch)
	if _, ok := c.ExportMarketDataMemory().EntitlementGaps()[key]; !ok {
		t.Fatal("the warned gap was not remembered for the daemon")
	}
	if c.marketDataAbsenceFor(key) != nil {
		t.Fatal("a delayed-fallback rejection formed an absence verdict")
	}
	if lines := logLines(buf, "code=354"); len(lines) != 1 || !strings.Contains(lines[0], "level=WARN") {
		t.Fatalf("354 severities, want one WARN: %v", lines)
	}
}
