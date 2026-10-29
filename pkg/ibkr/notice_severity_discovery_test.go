package ibkr

import (
	"context"
	"encoding/binary"
	"strconv"
	"strings"
	"testing"
	"time"
)

// syntheticSystemNoticeText is syntheticSystemNotice with the broker text
// chosen by the test, for notices whose severity depends on the message.
func syntheticSystemNoticeText(id, code int, message string) []string {
	payload := binary.AppendUvarint(nil, 1<<3)
	payload = binary.AppendUvarint(payload, uint64(id))
	payload = binary.AppendUvarint(payload, 3<<3)
	payload = binary.AppendUvarint(payload, uint64(code))
	payload = binary.AppendUvarint(payload, 4<<3|2)
	payload = binary.AppendUvarint(payload, uint64(len(message)))
	payload = append(payload, message...)
	return []string{strconv.Itoa(msgSystemNotification), string(payload)}
}

func singleNoticeLine(t *testing.T, buf *safeBuffer, substr string) string {
	t.Helper()
	lines := logLines(buf, substr)
	if len(lines) != 1 {
		t.Fatalf("lines matching %q = %v, want exactly one", substr, lines)
	}
	return lines[0]
}

// 162 "query cancelled" acknowledges the requester's own timeout cancel and
// stays in debug; 162 "no data" is a per-contract verdict at INFO, as is a
// 162 refusal for want of a market data permission, which its requester
// records and flags once; a pacing 162 keeps its warning.
func TestHistoricalNoticeSeveritiesFollowTheText(t *testing.T) {
	buf := captureConnectorLogs(t)
	conn, _ := newReadyWireTestConnection(t)
	epoch := conn.BrokerSessionEpoch()

	conn.processSystemNoticeMessageAtEpoch(syntheticSystemNoticeText(31, 162, "Historical Market Data Service error message:API historical data query cancelled: 31"), epoch)
	if lines := logLines(buf, "query cancelled"); len(lines) != 0 {
		t.Fatalf("cancel acknowledgement logged above debug: %v", lines)
	}

	// (No "SMART" in the text: the parser-misalignment heuristic treats that
	// substring as a frame error and would log the notice at ERROR.)
	conn.processSystemNoticeMessageAtEpoch(syntheticSystemNoticeText(32, 162, "Historical Market Data Service error message:HMDS query returned no data: ESZ6@CME Trades"), epoch)
	if line := singleNoticeLine(t, buf, "returned no data"); !strings.Contains(line, "level=INFO") {
		t.Fatalf("no-data verdict severity: %s", line)
	}

	conn.processSystemNoticeMessageAtEpoch(syntheticSystemNoticeText(34, 162, "Historical Market Data Service error message:No market data permissions for IBIS2 STK"), epoch)
	if line := singleNoticeLine(t, buf, "No market data permissions"); !strings.Contains(line, "level=INFO") || !strings.Contains(line, "requester records this verdict") {
		t.Fatalf("permission refusal severity: %s", line)
	}
	if !HistoricalNotEntitled(&HistoricalRequestError{Code: 162, Message: "Historical Market Data Service error message:No market data permissions for IBIS2 STK"}) ||
		HistoricalNotEntitled(&HistoricalRequestError{Code: 162, Message: "Historical Market Data Service error message:HMDS query returned no data: ESZ6@CME Trades"}) {
		t.Fatal("HistoricalNotEntitled misreads the 162 text")
	}

	conn.processSystemNoticeMessageAtEpoch(syntheticSystemNoticeText(33, 162, "Historical Market Data Service error message:Historical data request pacing violation"), epoch)
	if line := singleNoticeLine(t, buf, "pacing violation"); !strings.Contains(line, "level=WARN") {
		t.Fatalf("pacing violation lost its warning: %s", line)
	}
}

// A "no security definition" for a symbol-only stock lookup is a discovery
// miss the requester classifies (INFO); the same answer for a request that
// carried a conID is a stale identity and keeps its warning.
func TestDefinitionMissSeverityDependsOnConID(t *testing.T) {
	buf := captureConnectorLogs(t)
	conn, _ := newReadyWireTestConnection(t)
	epoch := conn.BrokerSessionEpoch()
	const noDefinition = "No security definition has been found for the request"

	conn.registerReqAlias(context.Background(), 41, Contract{Symbol: "PAIIU", SecType: "STK", Exchange: "SMART", Currency: "USD"})
	conn.processSystemNoticeMessageAtEpoch(syntheticSystemNoticeText(41, 200, noDefinition), epoch)
	if line := singleNoticeLine(t, buf, "(PAIIU STK)"); !strings.Contains(line, "level=INFO") || !strings.Contains(line, "symbol-only lookup") {
		t.Fatalf("symbol-only miss severity: %s", line)
	}

	conn.registerReqAlias(context.Background(), 42, Contract{Symbol: "OKE", SecType: "STK", Exchange: "SMART", Currency: "USD", ConID: 10794})
	conn.processSystemNoticeMessageAtEpoch(syntheticSystemNoticeText(42, 200, noDefinition), epoch)
	if line := singleNoticeLine(t, buf, "(OKE STK)"); !strings.Contains(line, "level=WARN") {
		t.Fatalf("stale conID miss lost its warning: %s", line)
	}

	// The same conID miss under a requester that records the verdict itself
	// (the lending worker's delisted names) is INFO.
	conn.registerReqAlias(WithDefinitionMissClassified(context.Background()), 44, Contract{Symbol: "CMND.OLD", SecType: "STK", Exchange: "SMART", Currency: "USD", ConID: 5551})
	conn.processSystemNoticeMessageAtEpoch(syntheticSystemNoticeText(44, 200, noDefinition), epoch)
	if line := singleNoticeLine(t, buf, "(CMND.OLD STK)"); !strings.Contains(line, "level=INFO") || !strings.Contains(line, "requester records this verdict") {
		t.Fatalf("classified conID miss severity: %s", line)
	}

	// A notice with no alias at all (unknown reqID) keeps the warning: there
	// is no requester known to classify it.
	conn.processSystemNoticeMessageAtEpoch(syntheticSystemNoticeText(43, 200, noDefinition), epoch)
	if line := singleNoticeLine(t, buf, "reqID=43"); !strings.Contains(line, "level=WARN") {
		t.Fatalf("unaliased miss lost its warning: %s", line)
	}
}

// A currency pair's notice names the whole pair: "EUR CASH" alone could be
// any of the account's EUR.* pairs, so a refusal on 2026-10-07 could not be
// tied to the one an order preview needed.
func TestCashNoticeNamesThePair(t *testing.T) {
	buf := captureConnectorLogs(t)
	conn, _ := newReadyWireTestConnection(t)
	epoch := conn.BrokerSessionEpoch()

	conn.registerReqAlias(context.Background(), 51, Contract{Symbol: "EUR", SecType: "CASH", Exchange: "IDEALPRO", Currency: "USD"})
	conn.processSystemNoticeMessageAtEpoch(syntheticSystemNoticeText(51, 354, "Requested market data is not subscribed."), epoch)
	singleNoticeLine(t, buf, "reqID=51 (EUR.USD CASH)")
}

// A code 200 inside a backend-link outage, or within the restore settle, is
// the outage's echo whatever the security type: debug, counted on the
// restore bookend. After the settle an index miss warns again, and a
// cached-conID miss keeps its stale-identity warning.
// Owner instruction (Oliver, in chat, 2026-10-13 09:04 CEST): dependents of
// an owned outage log through the owner.
func TestDefinitionMissDuringBackendOutageStaysInDebug(t *testing.T) {
	buf := captureConnectorLogs(t)
	c := NewConnector(&ConnectorConfig{})
	c.conn.rateLimiter.Stop()
	conn, _ := newReadyWireTestConnection(t)
	c.conn = conn
	conn.setNoticeLogContext(c.noticeLogContext())
	t0 := time.Date(2026, 10, 12, 4, 10, 0, 0, time.UTC)
	now := t0
	c.absenceNow = func() time.Time { return now }
	epoch := conn.BrokerSessionEpoch()
	const noDefinition = "No security definition has been found for the request"
	ctx := context.Background()
	conn.registerReqAlias(ctx, 71, Contract{Symbol: "SYNIDX", SecType: "IND", Exchange: "CBOE", Currency: "USD"})
	conn.registerReqAlias(ctx, 72, Contract{Symbol: "SYNFUT", SecType: "FUT", Exchange: "CME", Currency: "USD"})
	conn.registerReqAlias(ctx, 73, Contract{Symbol: "SYNBILL", SecType: "BILL", Exchange: "SMART", Currency: "EUR"})
	conn.registerReqAlias(ctx, 74, Contract{Symbol: "SYNSTK", SecType: "STK", Exchange: "SMART", Currency: "USD", ConID: 990001})
	miss := func(id int) {
		conn.processSystemNoticeMessageAtEpoch(syntheticSystemNoticeText(id, 200, noDefinition), epoch)
	}

	c.setBackendConnectivityDown(true, t0)
	for _, id := range []int{71, 72, 73, 74} {
		miss(id)
	}
	if lines := logLines(buf, "code=200"); len(lines) != 0 {
		t.Fatalf("definition misses escaped debug during the outage: %v", lines)
	}
	now = t0.Add(20 * time.Second)
	c.recordBackendConnectivity(false, now, 1102)
	restored := logLines(buf, "TWS restored connectivity")
	if len(restored) != 1 || !strings.Contains(restored[0], "4 definition misses (code 200) kept in debug logs") {
		t.Fatalf("restore bookend did not account for the misses: %v", restored)
	}

	// The flush right after the restore is still the outage's echo.
	now = now.Add(BackendRestoreSettle / 2)
	miss(71)
	if lines := logLines(buf, "code=200"); len(lines) != 0 {
		t.Fatalf("definition miss inside the restore settle escaped debug: %v", lines)
	}

	now = now.Add(BackendRestoreSettle)
	miss(71)
	if line := singleNoticeLine(t, buf, "(SYNIDX IND)"); !strings.Contains(line, "level=WARN") {
		t.Fatalf("index miss after the settle lost its warning: %s", line)
	}
	miss(74)
	if line := singleNoticeLine(t, buf, "(SYNSTK STK)"); !strings.Contains(line, "level=WARN") {
		t.Fatalf("stale conID miss outside an outage lost its warning: %s", line)
	}
}
