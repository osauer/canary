package ibkr

import (
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
