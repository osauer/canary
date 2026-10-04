package ibkr

import (
	"strings"
	"testing"
)

// Only the gateway's echo of a shifted frame is a misalignment: a parse-fault
// code quoting a venue name without its first byte, or a NumberFormatException
// naming a word. Notices that merely mention a venue are not.
func TestNoticeSignalsParserMisalignment(t *testing.T) {
	for _, tc := range []struct {
		name    string
		code    int
		message string
		want    bool
	}{
		{"shifted SMART in a parse fault", 320, `Error reading request. Unable to parse data. java.lang.NumberFormatException: For input string: "MART"`, true},
		{"shifted CBOE quoted", 320, "Error reading request:-'bN' : cause - Unknown exchange 'BOE'", true},
		{"shifted NASDAQ bare", 321, "Error validating request:-'bN' : cause - Invalid exchange ASDAQ", true},
		{"word in a numeric slot", 320, `Error reading request. java.lang.NumberFormatException: For input string: "USD"`, true},
		{"exception without a code", 10000, `Unable to parse data. java.lang.NumberFormatException: For input string: "STK"`, true},
		{"numeric NumberFormatException is not a shift", 320, `java.lang.NumberFormatException: For input string: "1.5"`, false},
		{"SMART mentioned in passing", 162, "Historical Market Data Service error message:HMDS query returned no data: AAPL@SMART Trades", false},
		{"SMART in a parse fault message", 320, "Error reading request: SMART routing is unavailable for this contract", false},
		{"CBOE in a definition miss", 200, "No security definition has been found for the request (CBOE)", false},
		{"cancel echo", 300, "Can't find EId with tickerId:30235", false},
		{"validation without fragment", 321, "Error validating request:-'bN' : cause - Please enter exchange", false},
		{"venue-named fragment is not a fragment", 320, "Error reading request: invalid exchange TSE", false},
		{"empty", 320, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := noticeSignalsParserMisalignment(tc.code, tc.message); got != tc.want {
				t.Fatalf("misalignment=%t want=%t for code %d %q", got, tc.want, tc.code, tc.message)
			}
		})
	}
}

// End to end: a notice that mentions SMART keeps its ordinary severity, and a
// real shifted-frame echo is the one that logs at ERROR with parser context.
func TestSystemNoticeMisalignmentSeverity(t *testing.T) {
	buf := captureConnectorLogs(t)
	conn, _ := newReadyWireTestConnection(t)
	epoch := conn.BrokerSessionEpoch()

	conn.processSystemNoticeMessageAtEpoch(syntheticSystemNoticeText(51, 162, "Historical Market Data Service error message:HMDS query returned no data: AAPL@SMART Trades"), epoch)
	if line := singleNoticeLine(t, buf, "AAPL@SMART"); !strings.Contains(line, "level=INFO") {
		t.Fatalf("passing SMART mention escalated: %s", line)
	}

	conn.processSystemNoticeMessageAtEpoch(syntheticSystemNoticeText(52, 320, `Error reading request. Unable to parse data. java.lang.NumberFormatException: For input string: "MART"`), epoch)
	if line := singleNoticeLine(t, buf, "reqID=52"); !strings.Contains(line, "level=ERROR") || !strings.Contains(line, "MART") {
		t.Fatalf("shifted frame echo not at ERROR: %s", line)
	}
}
