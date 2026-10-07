package ibkr

import (
	"context"
	"encoding/binary"
	"strconv"
	"strings"
	"testing"
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
// stays in debug; 162 "no data" is a per-contract verdict at INFO; a pacing
// 162 keeps its warning.
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
