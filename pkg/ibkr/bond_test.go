package ibkr

import (
	"context"
	"encoding/binary"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// syntheticBondFrameAt is a complete bondContractData frame in the layout
// of serverVersion (IBKR API 10.37 EDecoder): a message version before 164,
// the md size multiplier from 110 to 163, trading hours from 188, the size
// rules from 164, a secIdList with the CUSIP and ISIN. The identifiers are
// synthetic.
func syntheticBondFrameAt(serverVersion, reqID int, conID, cusip, isin, currency, maturity, issue string) []string {
	frame := []string{strconv.Itoa(msgBondContractData)}
	if serverVersion < minServerVerSizeRules {
		frame = append(frame, "6")
	}
	frame = append(frame, strconv.Itoa(reqID), "SYNTHB", "BOND", cusip, "0", maturity+" 16:00:00 US/Eastern", issue,
		"", "GOVT", "ZERO", "0", "0", "0", "SYNTH 0 bill", "SMART", currency, "SYNTHB", "SYNTHB", conID, "0.0001")
	if serverVersion >= minServerVerMdSizeMultiplier && serverVersion < minServerVerSizeRules {
		frame = append(frame, "1")
	}
	frame = append(frame, "LMT", "SMART", "", "", "0", "", "Synthetic Treasury Bill")
	if serverVersion >= minServerVerBondTradingHours {
		frame = append(frame, "US/Eastern", "20261001:0800-20261001:1700", "20261001:0800-20261001:1700")
	}
	frame = append(frame, "", "0", "2", "CUSIP", cusip, "ISIN", isin)
	if serverVersion >= minServerVerAggGroup {
		frame = append(frame, "1")
	}
	if serverVersion >= minServerVerMarketRules {
		frame = append(frame, "1")
	}
	if serverVersion >= minServerVerSizeRules {
		frame = append(frame, "1000", "1000", "1000")
	}
	return frame
}

// syntheticBondFrame is syntheticBondFrameAt the version Canary negotiates.
func syntheticBondFrame(reqID int, conID, cusip, isin, currency, maturity, issue string) []string {
	return syntheticBondFrameAt(maxClientVersion, reqID, conID, cusip, isin, currency, maturity, issue)
}

// syntheticBillFrame is syntheticBondFrame for a line IBKR lists as BILL.
func syntheticBillFrame(reqID int, conID, cusip, isin, currency, maturity, issue string) []string {
	frame := syntheticBondFrame(reqID, conID, cusip, isin, currency, maturity, issue)
	frame[3] = "BILL"
	return frame
}

// syntheticBillContractData is a complete ordinary contractData frame
// (message 10) at server version 203 for a synthetic bill line: the
// maturity as the lastTradeDateOrContractMonth, the CUSIP as the local
// symbol and in the secIdList with the ISIN, and the size rules.
func syntheticBillContractData(reqID int, conID, cusip, isin, maturity string) []string {
	return []string{
		strconv.Itoa(msgContractData), strconv.Itoa(reqID), "SYNTHB", "BILL", maturity, "", "", "", "SMART", "USD", cusip, "SYNTHB", "SYNTHB", conID, "0.0001",
		"", "LMT", "SMART", "1", "0", "Synthetic Treasury Bill", "", "", "", "", "", "US/Eastern", "20261001:0800-20261001:1700", "20261001:0800-20261001:1700",
		"", "0", "2", "CUSIP", cusip, "ISIN", isin, "1", "", "", "1", "", "", "1000", "1000", "1000", "0",
	}
}

// decodeBond decodes a bondContractData frame at the negotiated version.
func decodeBond(frame []string, reqID int) (BondContractDetails, []string, error) {
	return decodeBondContractData(frame, reqID, bondFrameLayoutFor(maxClientVersion))
}

func TestIsBillOrBond(t *testing.T) {
	for secType, want := range map[string]bool{"BILL": true, " bond ": true, "Bill": true, "STK": false, "": false, "BAG": false} {
		if got := IsBillOrBond(secType); got != want {
			t.Errorf("IsBillOrBond(%q) = %v", secType, got)
		}
	}
	for secType, want := range map[string]string{"bill": "BILL", "BOND": "BOND", "": "BOND", "STK": "BOND"} {
		if got := BillOrBondSecType(secType); got != want {
			t.Errorf("BillOrBondSecType(%q) = %q", secType, got)
		}
	}
	if d, _, err := decodeBond(syntheticBillFrame(7, "880001", "912797ZZ3", "US912797ZZ37", "USD", "20261126", "20260827"), 7); err != nil || d.SecType != "BILL" {
		t.Fatalf("bill frame = %+v %v", d, err)
	}
}

func TestParseBondContractDetailsCompleteFrame(t *testing.T) {
	frame := syntheticBondFrame(7, "880001", "912797ZZ3", "US912797ZZ37", "USD", "20261126", "20260827")
	d, notes, err := decodeBond(frame, 7)
	if err != nil || !d.Complete || len(notes) != 0 {
		t.Fatalf("complete frame refused: %+v %v %v", d, notes, err)
	}
	if d.ConID != 880001 || d.Currency != "USD" || d.Maturity != "20261126" || d.IssueDate != "20260827" || d.SecType != "BOND" ||
		d.MinTick != 0.0001 || d.MinSize != 1000 || d.SizeIncrement != 1000 || d.LongName != "Synthetic Treasury Bill" {
		t.Fatalf("decoded = %+v", d)
	}
	if d.ISIN() != "US912797ZZ37" || d.CUSIP() != "912797ZZ3" {
		t.Fatalf("identifiers = %q %q", d.ISIN(), d.CUSIP())
	}
	if _, _, err := decodeBond(frame, 8); !errors.Is(err, errBondFrameOtherRequest) {
		t.Fatalf("a frame for another request = %v", err)
	}
	for raw, want := range map[string]string{"20261126": "20261126", "20261126 16:00:00 US/Eastern": "20261126", "20261126-16:00:00": "20261126", "2026-11-26": "20261126", "11/26/2026": "11/26/2026"} {
		if got := bondWireDate(raw); got != want {
			t.Errorf("bondWireDate(%q) = %q, want %q", raw, got, want)
		}
	}
}

// IBKR's bond descriptions may leave the maturity, the currency and the
// identifiers empty (bond data licensing): such a frame is still a line,
// and its notes name what it lacks. A frame whose tail does not decode
// keeps its identity and drops the size rules and identifiers. A frame
// whose identity does not decode (a field of the wrong type up to the
// minimum tick, no contract id, a malformed currency, an early end) is
// refused, and the error names the field.
func TestDecodeBondContractDataIdentityAndGaps(t *testing.T) {
	frame := syntheticBondFrame(7, "880001", "912797ZZ3", "US912797ZZ37", "USD", "20261126", "20260827")
	truncated := slices.Clone(frame[:36])
	d, notes, err := decodeBond(truncated, 7)
	if err != nil || d.Complete || d.MinSize != 0 || d.SecIDs != nil || d.ConID != 880001 || d.CUSIP() != "912797ZZ3" ||
		len(notes) != 1 || notes[0] != "size rules, identifiers and hours not kept: the secIdList count 2 does not fit the 2 fields left" {
		t.Fatalf("truncated tail = %+v %v %v", d, notes, err)
	}
	restricted := slices.Clone(frame)
	restricted[4], restricted[6], restricted[7], restricted[16] = "", "", "", ""
	d, notes, err = decodeBond(restricted, 7)
	if err != nil || d.ConID != 880001 || d.Maturity != "" || d.Currency != "" || !d.Complete ||
		!slices.Equal(notes, []string{"no maturity", "no currency (the request's stands in)"}) {
		t.Fatalf("licence-restricted frame = %+v %v %v", d, notes, err)
	}
	odd := slices.Clone(frame)
	odd[6] = "11/26/2026"
	if d, notes, err := decodeBond(odd, 7); err != nil || d.Maturity != "11/26/2026" || !slices.Equal(notes, []string{`maturity "11/26/2026" unreadable`}) {
		t.Fatalf("unreadable maturity = %+v %v %v", d, notes, err)
	}
	for name, tc := range map[string]struct {
		mutate func([]string) []string
		want   string
	}{
		"contract id not a number": {func(f []string) []string { f[19] = "SMART"; return f }, `the contract id "SMART" is not an integer (field 19)`},
		"no contract id":           {func(f []string) []string { f[19] = ""; return f }, "the frame carries no positive contract id"},
		"currency malformed":       {func(f []string) []string { f[16] = "US"; return f }, `the currency "US" is not a three-letter code`},
		"coupon not a number":      {func(f []string) []string { f[5] = "zero"; return f }, `the coupon "zero" is not a finite number (field 5)`},
		"flag not boolean":         {func(f []string) []string { f[12] = "maybe"; return f }, `the callable flag "maybe" is not 0 or 1 (field 12)`},
		"ends before the tick":     {func(f []string) []string { return f[:20] }, "the frame ends before the minimum tick (20 fields)"},
		"request id not a number":  {func(f []string) []string { f[1] = "x"; return f }, `the request id "x" is not an integer (field 1)`},
	} {
		if d, _, err := decodeBond(tc.mutate(slices.Clone(frame)), 7); err == nil || err.Error() != tc.want {
			t.Fatalf("%s: %+v %v, want %q", name, d, err, tc.want)
		}
	}
}

// The layout follows the negotiated server version (IBKR API 10.37
// EDecoder.processBondContractDataMsg): each version's own frame decodes
// whole, with the fields its layout carries. A frame of another layout
// keeps its identity (a frame that names the request after a message
// version is read with one) but never its tail, so a moved field can never
// become a size rule.
func TestDecodeBondContractDataLayouts(t *testing.T) {
	layouts := []struct {
		name          string
		serverVersion int
		fields        int
		hours, sizes  bool
	}{
		{"server 203: no message version, trading hours, size rules", maxClientVersion, 43, true, true},
		{"server 176: no message version, no trading hours, size rules", 176, 40, false, true},
		{"server 150: message version, md size multiplier, no size rules", 150, 39, false, false},
	}
	for _, tc := range layouts {
		frame := syntheticBondFrameAt(tc.serverVersion, 7, "880001", "912797ZZ3", "US912797ZZ37", "USD", "20261126", "20260827")
		if len(frame) != tc.fields {
			t.Fatalf("%s: synthetic frame has %d fields, want %d", tc.name, len(frame), tc.fields)
		}
		negotiated := bondFrameLayoutFor(tc.serverVersion)
		at, ours := bondFrameLayoutNaming(frame, 7, negotiated)
		if !ours || at != negotiated {
			t.Fatalf("%s: layout = %+v %v", tc.name, at, ours)
		}
		// IBKR closes every frame with a separator: one trailing empty field.
		d, notes, err := decodeBondContractData(append(frame, ""), 7, at)
		if err != nil || !d.Complete || len(notes) != 0 || d.ConID != 880001 || d.Maturity != "20261126" || d.SecIDs["ISIN"] != "US912797ZZ37" ||
			d.LongName != "Synthetic Treasury Bill" || (d.TimeZoneID == "US/Eastern") != tc.hours || (d.MinSize == 1000) != tc.sizes {
			t.Fatalf("%s: decoded = %+v %v %v", tc.name, d, notes, err)
		}
		for _, other := range layouts {
			if other.serverVersion == tc.serverVersion {
				continue
			}
			at, ours := bondFrameLayoutNaming(frame, 7, bondFrameLayoutFor(other.serverVersion))
			d, notes, err := decodeBondContractData(frame, 7, at)
			if !ours || err != nil || d.Complete || d.ConID != 880001 || d.Maturity != "20261126" || d.MinSize != 0 || d.SecIDs != nil ||
				len(notes) != 1 || !strings.Contains(notes[0], "not kept") {
				t.Fatalf("%s frame read as %s (%s): %+v %v %v", tc.name, other.name, at, d, notes, err)
			}
		}
	}
	if got := (bondFrameLayout{serverVersion: maxClientVersion, versionField: true}).String(); got != "server 203, read with a message version" {
		t.Fatalf("layout name = %q", got)
	}
}

// An ordinary contractData frame that answers a bill request is a line:
// identity, exchange, currency, maturity, hours, secIdList and size rules
// from the contractData layout, the bond-only fields empty.
func TestDecodeContractDataLine(t *testing.T) {
	layout := bondFrameLayoutFor(maxClientVersion)
	frame := syntheticBillContractData(9, "880003", "912797ZZ3", "US912797ZZ37", "20261126")
	d, notes, err := decodeContractDataLine(append(slices.Clone(frame), ""), 9, layout)
	if err != nil || !d.Complete || d.ConID != 880003 || d.SecType != "BILL" || d.Exchange != "SMART" || d.Currency != "USD" || d.Maturity != "20261126" ||
		d.MinTick != 0.0001 || d.MinSize != 1000 || d.SizeIncrement != 1000 || d.TimeZoneID != "US/Eastern" || d.LongName != "Synthetic Treasury Bill" ||
		d.CUSIP() != "912797ZZ3" || d.ISIN() != "US912797ZZ37" || d.Coupon != 0 || d.CUSIPField != "" || d.IssueDate != "" {
		t.Fatalf("contractData line = %+v %v", d, err)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "ordinary contractData frame") {
		t.Fatalf("notes = %v", notes)
	}
	summary := bondFrameSummary(frame, layout)
	if summary.Kind != BondFrameContractData || summary.ConID != "880003" || summary.Exchange != "SMART" || summary.Currency != "USD" ||
		summary.LocalSymbol != "912797ZZ3" || summary.Maturity != "20261126" || summary.SecType != "BILL" || summary.Fields != len(frame) {
		t.Fatalf("summary = %+v", summary)
	}
	// The maturity falls back to the lastTradeDate, then to the real
	// expiration date of a whole frame.
	lastTrade := slices.Clone(frame)
	lastTrade[4], lastTrade[5] = "", "20261126"
	realExpiration := slices.Clone(frame)
	realExpiration[4], realExpiration[40] = "", "20261126"
	for name, f := range map[string][]string{"last trade date": lastTrade, "real expiration": realExpiration} {
		if d, _, err := decodeContractDataLine(f, 9, layout); err != nil || d.Maturity != "20261126" {
			t.Fatalf("%s: %+v %v", name, d, err)
		}
	}
	if d, notes, err := decodeContractDataLine(frame[:20], 9, layout); err != nil || d.Complete || d.MinSize != 0 || d.ConID != 880003 || !strings.Contains(strings.Join(notes, "; "), "not kept") {
		t.Fatalf("truncated = %+v %v %v", d, notes, err)
	}
	if _, _, err := decodeContractDataLine(frame, 10, layout); !errors.Is(err, errBondFrameOtherRequest) {
		t.Fatalf("another request = %v", err)
	}
	noConID := slices.Clone(frame)
	noConID[13] = ""
	if _, _, err := decodeContractDataLine(noConID, 9, layout); err == nil || !strings.Contains(err.Error(), "no positive contract id") {
		t.Fatalf("no contract id = %v", err)
	}
}

func TestBondIdentifierChecks(t *testing.T) {
	for _, s := range []string{"US912797ZZ37", "DE000BU0ZZ19", "FR0128ZZZZ13", "GB00ZZZZZZ11"} {
		if !ValidISIN(s) {
			t.Errorf("%s refused", s)
		}
	}
	for _, s := range []string{"US912797ZZ38", "DE000BU0ZZ1", "1S912797ZZ37", "us912797zz37"} {
		if ValidISIN(s) {
			t.Errorf("%s accepted", s)
		}
	}
	if !ValidCUSIP("912797ZZ3") || ValidCUSIP("912797ZZ4") || ValidCUSIP("912797ZZ") {
		t.Fatal("CUSIP check digit")
	}
}

// The request names the bond the way IBKR documents it, the identifier as
// the symbol (BOND, SMART, the currency, no secIdType); the frames and the
// end marker complete it. A malformed identifier never reaches the wire.
func TestBondContractDetailsByISIN(t *testing.T) {
	conn, connector, socket, _, _ := newQueuedInstructionReconnectFixture(t)
	type result struct {
		lines []BondContractDetails
		err   error
	}
	done := make(chan result, 1)
	go func() {
		lines, err := connector.BondContractDetails(context.Background(), BondContractRequest{IDType: BondIdentifierISIN, ID: "DE000BU0ZZ19", Currency: "EUR"}, 2*time.Second)
		done <- result{lines, err}
	}()
	reqID := waitForHandlerReqID(t, conn, msgBondContractData)
	waitForBondRequestFrame(t, conn, socket)
	frames := decodeOutboundFrames(t, conn, socket.Bytes())
	assertBondRequestFrame(t, frames[len(frames)-1], "DE000BU0ZZ19", "EUR", "", "")
	conn.dispatchHandlers(msgBondContractData, syntheticBondFrame(reqID, "880002", "DE000BU0ZZ19", "DE000BU0ZZ19", "EUR", "20270120", "20260722"), conn.BrokerSessionEpoch())
	prewarmTestEnd(conn, reqID)
	got := <-done
	if got.err != nil || len(got.lines) != 1 || got.lines[0].ConID != 880002 || got.lines[0].ISIN() != "DE000BU0ZZ19" || got.lines[0].Currency != "EUR" {
		t.Fatalf("lines = %+v err %v", got.lines, got.err)
	}
	if _, err := connector.BondContractDetails(context.Background(), BondContractRequest{IDType: BondIdentifierISIN, ID: "DE000BU0ZZ18", Currency: "EUR"}, time.Second); err == nil {
		t.Fatal("an ISIN with a bad check digit was sent")
	}
}

// A CUSIP IBKR does not find by symbol is asked again by secIdType/secId,
// then by symbol with no exchange. When no form finds a line, the error
// keeps every answer with IBKR's own code and text and still reads as the
// no-definition verdict; when a later form answers, its line is the result.
func TestBondContractDetailsFallsBackToSecID(t *testing.T) {
	conn, connector, socket, _, _ := newQueuedInstructionReconnectFixture(t)
	type result struct {
		lines []BondContractDetails
		err   error
	}
	done := make(chan result, 1)
	ask := func() {
		go func() {
			lines, err := connector.BondContractDetails(context.Background(), BondContractRequest{IDType: BondIdentifierCUSIP, ID: "912797ZZ3", Currency: "USD"}, 2*time.Second)
			done <- result{lines, err}
		}()
	}
	const noDefinition = "No security definition has been found for the request"

	ask()
	assertBondRequestFrame(t, nthBondRequestFrame(t, conn, socket, 1), "912797ZZ3", "USD", "", "")
	reqID := waitForHandlerReqID(t, conn, msgBondContractData)
	if !connector.failPendingContractDetails(reqID, 200, noDefinition) {
		t.Fatal("the bond request was not armed for the broker's rejection")
	}
	assertBondRequestFrame(t, nthBondRequestFrame(t, conn, socket, 2), "", "USD", "CUSIP", "912797ZZ3")
	reqID = waitForHandlerReqIDAfter(t, conn, msgBondContractData, reqID)
	if !connector.failPendingContractDetails(reqID, 200, noDefinition+"\n") {
		t.Fatal("the secId request was not armed for the broker's rejection")
	}
	assertBondWireFrame(t, nthBondRequestFrame(t, conn, socket, 3), "BOND", "912797ZZ3", "", "USD", "", "")
	reqID = waitForHandlerReqIDAfter(t, conn, msgBondContractData, reqID)
	if !connector.failPendingContractDetails(reqID, 321, "Error validating request") {
		t.Fatal("the no-exchange request was not armed for the broker's rejection")
	}
	got := <-done
	lookupErr, ok := errors.AsType[*BondLookupError](got.err)
	if !ok || !errors.Is(got.err, ErrContractNoDefinition) || len(lookupErr.Attempts) != 3 {
		t.Fatalf("rejection = %#v", got.err)
	}
	for i, want := range []struct {
		form string
		code int
		text string
	}{{"BOND by symbol", 200, noDefinition}, {"BOND by secIdType CUSIP", 200, noDefinition}, {"BOND by symbol, no exchange", 321, "Error validating request"}} {
		if a := lookupErr.Attempts[i]; a.Form != want.form || a.Code != want.code || a.Message != want.text || a.Outcome != BondAttemptRejected || a.ReqID == 0 {
			t.Fatalf("attempt %d = %+v", i, a)
		}
	}
	if want := `BOND by symbol: IBKR 200 "` + noDefinition + `"`; !strings.Contains(got.err.Error(), want) || !strings.Contains(got.err.Error(), "BOND CUSIP 912797ZZ3 on SMART in USD") {
		t.Fatalf("error text = %q", got.err.Error())
	}

	// The symbol form answers a line that names another CUSIP: it is not the
	// bill asked for, and the secId form's line is.
	ask()
	nthBondRequestFrame(t, conn, socket, 4)
	reqID = waitForHandlerReqIDAfter(t, conn, msgBondContractData, reqID)
	conn.dispatchHandlers(msgBondContractData, syntheticBondFrame(reqID, "880009", "912797ZY6", "", "USD", "20261126", "20260827"), conn.BrokerSessionEpoch())
	prewarmTestEnd(conn, reqID)
	nthBondRequestFrame(t, conn, socket, 5)
	reqID = waitForHandlerReqIDAfter(t, conn, msgBondContractData, reqID)
	conn.dispatchHandlers(msgBondContractData, syntheticBondFrame(reqID, "880001", "912797ZZ3", "US912797ZZ37", "USD", "20261126", "20260827"), conn.BrokerSessionEpoch())
	prewarmTestEnd(conn, reqID)
	if got := <-done; got.err != nil || len(got.lines) != 1 || got.lines[0].ConID != 880001 {
		t.Fatalf("fallback lines = %+v err %v", got.lines, got.err)
	}
}

// A request asked as BILL then BOND tries every BILL form before any BOND
// form: IBKR lists US Treasury bills as BILL, and a bill asked as BOND
// finds no line. The line found keeps the type it resolved as, and an
// exhausted lookup names every attempt with IBKR's code and text.
func TestBondContractDetailsAsksBillThenBond(t *testing.T) {
	conn, connector, socket, _, _ := newQueuedInstructionReconnectFixture(t)
	type result struct {
		lines []BondContractDetails
		err   error
	}
	done := make(chan result, 1)
	ask := func(r BondContractRequest) {
		go func() {
			lines, err := connector.BondContractDetails(context.Background(), r, 2*time.Second)
			done <- result{lines, err}
		}()
	}
	const noDefinition = "No security definition has been found for the request"

	// A US bill asked as BILL resolves by symbol on the first request.
	ask(BondContractRequest{IDType: BondIdentifierCUSIP, ID: "912797ZZ3", Currency: "USD", SecTypes: []string{"BILL"}})
	assertBillOrBondRequestFrame(t, nthBondRequestFrame(t, conn, socket, 1), "BILL", "912797ZZ3", "USD", "", "")
	reqID := waitForHandlerReqID(t, conn, msgBondContractData)
	conn.dispatchHandlers(msgBondContractData, syntheticBillFrame(reqID, "880001", "912797ZZ3", "US912797ZZ37", "USD", "20261126", "20260827"), conn.BrokerSessionEpoch())
	prewarmTestEnd(conn, reqID)
	if got := <-done; got.err != nil || len(got.lines) != 1 || got.lines[0].SecType != "BILL" || got.lines[0].ConID != 880001 {
		t.Fatalf("bill lines = %+v err %v", got.lines, got.err)
	}

	// A German ISIN asked as BILL then BOND: no BILL form finds a line, the
	// BOND symbol form does.
	ask(BondContractRequest{IDType: BondIdentifierISIN, ID: "DE000BU0ZZ19", Currency: "EUR", SecTypes: []string{"BILL", "BOND"}})
	assertBillOrBondRequestFrame(t, nthBondRequestFrame(t, conn, socket, 2), "BILL", "DE000BU0ZZ19", "EUR", "", "")
	reqID = waitForHandlerReqIDAfter(t, conn, msgBondContractData, reqID)
	if !connector.failPendingContractDetails(reqID, 200, noDefinition) {
		t.Fatal("the BILL symbol request was not armed for the broker's rejection")
	}
	assertBillOrBondRequestFrame(t, nthBondRequestFrame(t, conn, socket, 3), "BILL", "", "EUR", "ISIN", "DE000BU0ZZ19")
	reqID = waitForHandlerReqIDAfter(t, conn, msgBondContractData, reqID)
	prewarmTestEnd(conn, reqID)
	assertBondWireFrame(t, nthBondRequestFrame(t, conn, socket, 4), "BILL", "DE000BU0ZZ19", "", "EUR", "", "")
	reqID = waitForHandlerReqIDAfter(t, conn, msgBondContractData, reqID)
	prewarmTestEnd(conn, reqID)
	assertBillOrBondRequestFrame(t, nthBondRequestFrame(t, conn, socket, 5), "BOND", "DE000BU0ZZ19", "EUR", "", "")
	reqID = waitForHandlerReqIDAfter(t, conn, msgBondContractData, reqID)
	conn.dispatchHandlers(msgBondContractData, syntheticBondFrame(reqID, "880002", "DE000BU0ZZ19", "DE000BU0ZZ19", "EUR", "20270120", "20260722"), conn.BrokerSessionEpoch())
	prewarmTestEnd(conn, reqID)
	if got := <-done; got.err != nil || len(got.lines) != 1 || got.lines[0].SecType != "BOND" || got.lines[0].ConID != 880002 {
		t.Fatalf("bond lines = %+v err %v", got.lines, got.err)
	}

	// Asked as BILL then BOND and found by none: six attempts, in order.
	ask(BondContractRequest{IDType: BondIdentifierCUSIP, ID: "912797ZZ3", Currency: "USD", SecTypes: []string{"BILL", "BOND"}})
	for n := 6; n <= 11; n++ {
		nthBondRequestFrame(t, conn, socket, n)
		reqID = waitForHandlerReqIDAfter(t, conn, msgBondContractData, reqID)
		if !connector.failPendingContractDetails(reqID, 200, noDefinition) {
			t.Fatalf("request %d was not armed for the broker's rejection", n)
		}
	}
	got := <-done
	lookupErr, ok := errors.AsType[*BondLookupError](got.err)
	if !ok || !errors.Is(got.err, ErrContractNoDefinition) || len(lookupErr.Attempts) != 6 {
		t.Fatalf("rejection = %#v", got.err)
	}
	for i, form := range []string{"BILL by symbol", "BILL by secIdType CUSIP", "BILL by symbol, no exchange", "BOND by symbol", "BOND by secIdType CUSIP", "BOND by symbol, no exchange"} {
		if a := lookupErr.Attempts[i]; a.Form != form || a.Code != 200 || a.Message != noDefinition {
			t.Fatalf("attempt %d = %+v", i, a)
		}
	}
	if !strings.Contains(got.err.Error(), "BILL then BOND CUSIP 912797ZZ3 on SMART in USD") {
		t.Fatalf("error text = %q", got.err.Error())
	}
}

// inboundWireFrame is fields as the gateway writes them from server 201:
// the message id as a 4-byte integer, then every field with its separator.
func inboundWireFrame(fields ...string) []byte {
	msgID, _ := strconv.Atoi(fields[0])
	b := binary.BigEndian.AppendUint32(nil, uint32(msgID))
	for _, f := range fields[1:] {
		b = append(append(b, f...), 0)
	}
	return b
}

// inboundNotice is the protobuf error notice (message 204) the gateway sends
// for a request from server 201.
func inboundNotice(reqID, code int, text string) []byte {
	b := binary.BigEndian.AppendUint32(nil, uint32(msgSystemNotification))
	b = binary.AppendUvarint(binary.AppendUvarint(b, 1<<3), uint64(reqID))
	b = binary.AppendUvarint(binary.AppendUvarint(b, 3<<3), uint64(code))
	b = binary.AppendUvarint(binary.AppendUvarint(b, 4<<3|2), uint64(len(text)))
	return append(b, text...)
}

// Every frame that names a bond request's id reaches the lookup's trace
// through the reader's own path, reduced to identifiers, with the reason a
// contract frame is not a line: a bondContractData frame whose contract id
// is empty, IBKR's code-200 notice, a frame of another message, and an
// ordinary contractData frame, which is the line. A frame for another
// request is not recorded. The third form asks with no exchange.
func TestBondLookupRecordsEveryFrame(t *testing.T) {
	conn, connector, socket, _, _ := newQueuedInstructionReconnectFixture(t)
	type result struct {
		lines []BondContractDetails
		trace *BondLookupTrace
		err   error
	}
	done := make(chan result, 1)
	go func() {
		lines, trace, err := connector.BondContractDetailsTraced(context.Background(), BondContractRequest{IDType: BondIdentifierCUSIP, ID: "912797ZZ3", Currency: "USD", SecTypes: []string{"BILL"}}, 2*time.Second)
		done <- result{lines, trace, err}
	}()
	receive := func(frame []byte) { conn.processMessageAtEpoch(frame, conn.BrokerSessionEpoch()) }
	const noDefinition = "No security definition has been found for the request"

	nthBondRequestFrame(t, conn, socket, 1)
	first := waitForHandlerReqID(t, conn, msgBondContractData)
	stripped := syntheticBillFrame(first, "", "", "", "", "", "")
	stripped[6] = ""
	receive(inboundWireFrame(stripped...))
	receive(inboundWireFrame(syntheticBillFrame(first+1000, "880009", "912797ZY6", "", "USD", "20261126", "20260827")...))
	receive(inboundWireFrame(strconv.Itoa(msgContractDataEnd), "1", strconv.Itoa(first)))

	nthBondRequestFrame(t, conn, socket, 2)
	second := waitForHandlerReqIDAfter(t, conn, msgBondContractData, first)
	receive(inboundNotice(second, 200, noDefinition))
	connector.failPendingContractDetails(second, 200, noDefinition) // the fixture wires no notice hook

	assertBondWireFrame(t, nthBondRequestFrame(t, conn, socket, 3), "BILL", "912797ZZ3", "", "USD", "", "")
	third := waitForHandlerReqIDAfter(t, conn, msgBondContractData, second)
	receive(inboundWireFrame("999", "1", strconv.Itoa(third)))
	receive(inboundWireFrame(syntheticBillContractData(third, "880003", "912797ZZ3", "US912797ZZ37", "20261126")...))
	receive(inboundWireFrame(strconv.Itoa(msgContractDataEnd), "1", strconv.Itoa(third)))

	got := <-done
	if got.err != nil || len(got.lines) != 1 || got.lines[0].ConID != 880003 || got.lines[0].SecType != "BILL" || !got.lines[0].Complete || got.lines[0].CUSIP() != "912797ZZ3" {
		t.Fatalf("lines = %+v err %v", got.lines, got.err)
	}
	trace := got.trace
	if trace == nil || trace.ServerVersion != minServerVerProtoBufPlaceOrder || trace.Request != "BILL CUSIP 912797ZZ3 on SMART in USD" || len(trace.Attempts) != 3 {
		t.Fatalf("trace = %+v", trace)
	}
	a := trace.Attempts
	if a[0].Form != "BILL by symbol" || a[0].ReqID != first || a[0].Symbol != "912797ZZ3" || a[0].Exchange != "SMART" || a[0].Currency != "USD" || a[0].Outcome != BondAttemptNoLine ||
		a[0].Message != "the search ended without a line: IBKR sent 1 contract frame that did not decode (bondContractData: the frame carries no positive contract id)" {
		t.Fatalf("attempt 0 = %+v", a[0])
	}
	if f := a[0].Frames; len(f) != 2 || f[0].Kind != BondFrameBondContractData || f[0].Line || f[0].Fields != len(stripped)+1 || f[0].ConID != "" || f[0].SecType != "BILL" ||
		f[0].Layout != "server 203" || f[1].Kind != BondFrameContractDataEnd {
		t.Fatalf("attempt 0 frames = %+v", f)
	}
	if a[1].Form != "BILL by secIdType CUSIP" || a[1].SecIDType != "CUSIP" || a[1].SecID != "912797ZZ3" || a[1].Outcome != BondAttemptRejected || a[1].Code != 200 ||
		len(a[1].Frames) != 1 || a[1].Frames[0].Kind != BondFrameError || a[1].Frames[0].Code != 200 || a[1].Frames[0].Note != noDefinition {
		t.Fatalf("attempt 1 = %+v", a[1])
	}
	if a[2].Form != "BILL by symbol, no exchange" || a[2].Exchange != "" || a[2].Outcome != BondAttemptLine || a[2].Lines != 1 || len(a[2].Frames) != 3 {
		t.Fatalf("attempt 2 = %+v", a[2])
	}
	if f := a[2].Frames; f[0].Kind != BondFrameOther || f[0].MessageID != 999 || f[1].Kind != BondFrameContractData || !f[1].Line || !f[1].Complete ||
		f[1].ConID != "880003" || f[1].Maturity != "20261126" || f[2].Kind != BondFrameContractDataEnd {
		t.Fatalf("attempt 2 frames = %+v", f)
	}
	if line := a[2].Frames[1].String(); !strings.Contains(line, `msg 10 contractData, 47 fields, layout "server 203", con_id "880003", sec_type "BILL"`) || !strings.Contains(line, ": a line, whole frame decoded") {
		t.Fatalf("frame log line = %q", line)
	}
	if conn.inboundTaps.Load() != 0 {
		t.Fatal("the lookup left its frame tap armed")
	}
}

// A bond description IBKR sends without maturity, currency or identifiers
// is a line: it resolves in the currency asked, and its frame says what it
// lacks. An empty search says no frame named the request.
func TestBondLookupKeepsARestrictedLine(t *testing.T) {
	conn, connector, socket, _, _ := newQueuedInstructionReconnectFixture(t)
	type result struct {
		lines []BondContractDetails
		trace *BondLookupTrace
		err   error
	}
	done := make(chan result, 1)
	go func() {
		lines, trace, err := connector.BondContractDetailsTraced(context.Background(), BondContractRequest{IDType: BondIdentifierCUSIP, ID: "912797ZZ3", Currency: "USD", SecTypes: []string{"BILL"}}, 2*time.Second)
		done <- result{lines, trace, err}
	}()
	nthBondRequestFrame(t, conn, socket, 1)
	reqID := waitForHandlerReqID(t, conn, msgBondContractData)
	prewarmTestEnd(conn, reqID)
	nthBondRequestFrame(t, conn, socket, 2)
	reqID = waitForHandlerReqIDAfter(t, conn, msgBondContractData, reqID)
	restricted := syntheticBillFrame(reqID, "880001", "", "", "", "", "")
	restricted[6] = ""
	conn.processMessageAtEpoch(inboundWireFrame(restricted...), conn.BrokerSessionEpoch())
	prewarmTestEnd(conn, reqID)
	got := <-done
	if got.err != nil || len(got.lines) != 1 || got.lines[0].Currency != "USD" || got.lines[0].SecType != "BILL" || got.lines[0].Maturity != "" || got.lines[0].ConID != 880001 {
		t.Fatalf("lines = %+v err %v", got.lines, got.err)
	}
	a := got.trace.Attempts
	if len(a) != 2 || a[0].Message != "the search ended without a line: no frame named the request before its end marker" || a[1].Outcome != BondAttemptLine {
		t.Fatalf("attempts = %+v", a)
	}
	if f := a[1].Frames[0]; !f.Line || f.Note != "no maturity; no currency (the request's stands in)" {
		t.Fatalf("frame = %+v", f)
	}
}

// A contract id is asked once per type, by id alone; an identifier is asked
// by symbol, then by secIdType/secId, on SMART unless an exchange is named,
// then by symbol with no exchange, every form of one type before the next
// type.
func TestBondContractRequestForms(t *testing.T) {
	forms, err := BondContractRequest{ConID: 880001, Currency: "usd"}.wireForms()
	if err != nil || len(forms) != 1 || forms[0].contract.ConID != 880001 || forms[0].contract.Exchange != "" || forms[0].contract.Symbol != "" || forms[0].contract.SecIDType != "" {
		t.Fatalf("contract id forms = %+v %v", forms, err)
	}
	forms, err = BondContractRequest{IDType: "cusip", ID: "912797zz3", Currency: "USD"}.wireForms()
	if err != nil || len(forms) != 3 {
		t.Fatalf("CUSIP forms = %+v %v", forms, err)
	}
	if c := forms[0].contract; c.Symbol != "912797ZZ3" || c.SecIDType != "" || c.SecID != "" || c.SecType != "BOND" || c.Exchange != "SMART" || c.Currency != "USD" {
		t.Fatalf("symbol form = %+v", c)
	}
	if c := forms[1].contract; c.Symbol != "" || c.SecIDType != "CUSIP" || c.SecID != "912797ZZ3" || c.Exchange != "SMART" {
		t.Fatalf("secId form = %+v", c)
	}
	if c := forms[2].contract; forms[2].label != "BOND by symbol, no exchange" || c.Symbol != "912797ZZ3" || c.SecIDType != "" || c.Exchange != "" || c.Currency != "USD" {
		t.Fatalf("no-exchange form = %+v", forms[2])
	}
	if a := forms[2].attempt(); a.Form != forms[2].label || a.Symbol != "912797ZZ3" || a.Exchange != "" || a.SecType != "BOND" || a.Currency != "USD" {
		t.Fatalf("attempt = %+v", a)
	}
	forms, err = BondContractRequest{IDType: "ISIN", ID: "DE000BU0ZZ19", Currency: "EUR", SecTypes: []string{"bill", "BOND", "BILL"}}.wireForms()
	if err != nil || len(forms) != 6 {
		t.Fatalf("BILL then BOND forms = %+v %v", forms, err)
	}
	for i, want := range []struct{ label, secType string }{{"BILL by symbol", "BILL"}, {"BILL by secIdType ISIN", "BILL"}, {"BILL by symbol, no exchange", "BILL"},
		{"BOND by symbol", "BOND"}, {"BOND by secIdType ISIN", "BOND"}, {"BOND by symbol, no exchange", "BOND"}} {
		if forms[i].label != want.label || forms[i].contract.SecType != want.secType {
			t.Fatalf("form %d = %+v", i, forms[i])
		}
	}
	forms, err = BondContractRequest{ConID: 880001, Currency: "USD", SecTypes: []string{"BILL", "BOND"}}.wireForms()
	if err != nil || len(forms) != 2 || forms[0].contract.SecType != "BILL" || forms[1].contract.SecType != "BOND" || forms[1].contract.ConID != 880001 {
		t.Fatalf("contract id BILL then BOND forms = %+v %v", forms, err)
	}
	if forms, err := (BondContractRequest{ConID: 880001, Currency: "USD"}).wireForms(); err != nil || forms[0].contract.SecType != "BOND" || forms[0].label != "BOND by contract id" {
		t.Fatalf("default type forms = %+v %v", forms, err)
	}
	for _, bad := range []BondContractRequest{{IDType: "CUSIP", ID: "912797ZZ4", Currency: "USD"}, {IDType: "ISIN", ID: "912797ZZ3", Currency: "USD"}, {IDType: "CUSIP", ID: "912797ZZ3", Currency: "US"},
		{IDType: "CUSIP", ID: "912797ZZ3", Currency: "USD", SecTypes: []string{"BILL", "STK"}}} {
		if _, err := bad.wireForms(); err == nil {
			t.Fatalf("%+v accepted", bad)
		}
	}
	if got := brokerNoticeLine("  No security\tdefinition\r\n found  "); got != "No security definition found" {
		t.Fatalf("notice line = %q", got)
	}
}

// nthBondRequestFrame waits until the socket carries n BOND
// reqContractDetails frames and returns the nth: the request is armed for
// the gateway's answer before it is written.
func nthBondRequestFrame(t *testing.T, conn *Connection, socket *safeBuffer, n int) []string {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var bond [][]string
		for _, frame := range decodeOutboundFrames(t, conn, socket.Bytes()) {
			if len(frame) > 5 && frame[0] == strconv.Itoa(reqContractData) && IsBillOrBond(frame[5]) {
				bond = append(bond, frame)
			}
		}
		if len(bond) >= n {
			return bond[n-1]
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("BOND request frame %d was never written", n)
	return nil
}

// assertBondRequestFrame checks a BOND reqContractDetails frame on SMART.
func assertBondRequestFrame(t *testing.T, frame []string, symbol, currency, secIDType, secID string) {
	t.Helper()
	assertBondWireFrame(t, frame, "BOND", symbol, "SMART", currency, secIDType, secID)
}

// assertBillOrBondRequestFrame checks a BILL or BOND reqContractDetails
// frame on SMART.
func assertBillOrBondRequestFrame(t *testing.T, frame []string, secType, symbol, currency, secIDType, secID string) {
	t.Helper()
	assertBondWireFrame(t, frame, secType, symbol, "SMART", currency, secIDType, secID)
}

// assertBondWireFrame checks a bond reqContractDetails frame field by field
// against IBKR's documented bond example and the official version-8
// encoder: the named fields as given and every other field empty
// (includeExpired false, sent as 0; no primary exchange; issuerId empty).
func assertBondWireFrame(t *testing.T, frame []string, secType, symbol, exchange, currency, secIDType, secID string) {
	t.Helper()
	assertFields(t, frame, []fieldAssertion{
		{0, strconv.Itoa(reqContractData), "message"}, {1, "8", "version"}, {3, "0", "conId"}, {4, symbol, "symbol"}, {5, secType, "secType"},
		{6, "", "lastTradeDateOrContractMonth"}, {7, "", "strike"}, {8, "", "right"}, {9, "", "multiplier"}, {10, exchange, "exchange"},
		{11, "", "primaryExchange"}, {12, currency, "currency"}, {13, "", "localSymbol"}, {14, "", "tradingClass"}, {15, "0", "includeExpired"},
		{16, secIDType, "secIdType"}, {17, secID, "secId"}, {18, "", "issuerId"},
	})
	if len(frame) > 20 || (len(frame) == 20 && frame[19] != "") {
		t.Fatalf("bond request carries %d fields: %#v", len(frame), frame)
	}
}

func waitForBondRequestFrame(t *testing.T, conn *Connection, socket *safeBuffer) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if socket.Len() > 0 {
			if frames := decodeOutboundFrames(t, conn, socket.Bytes()); len(frames) > 0 && slices.Contains(frames[len(frames)-1], "BOND") {
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("no request frame was written")
}

// Yield ticks are stored as yields, never as prices, and a bond quote
// subscription asks for no generic ticks.
func TestBondYieldTicksAndGenericTicks(t *testing.T) {
	sub := &Subscription{}
	sub.recordBondYield(tickBidYield, 4.1)
	sub.recordBondYield(tickDelayedAskYield, 4.0)
	sub.recordBondYield(tickLastYield, -1)
	if sub.bidYield == nil || *sub.bidYield != 4.1 || sub.askYield == nil || *sub.askYield != 4.0 || sub.lastYield != nil {
		t.Fatalf("yields = %v %v %v", sub.bidYield, sub.askYield, sub.lastYield)
	}
	if !isBondYieldTick(50) || !isBondYieldTick(105) || isBondYieldTick(1) || isBondYieldTick(37) {
		t.Fatal("yield tick set")
	}
	bond := Contract{ConID: 880001, Symbol: "SYNTHB", SecType: "BOND", Exchange: "SMART", Currency: "USD"}
	if _, ticks, err := marketDataReplayRequest(mdReplaySpec{contract: bond}); err != nil || ticks != "" {
		t.Fatalf("bond ticks = %q %v", ticks, err)
	}
	if _, ticks, _ := marketDataReplayRequest(mdReplaySpec{contract: Contract{Symbol: "SYNTH", SecType: "STK"}}); ticks != sharedGenericTicks {
		t.Fatalf("stock ticks = %q", ticks)
	}
}

// Only explicitly broker-labeled SettledCash is a typed native ledger field.
// Unprefixed aggregate callbacks remain unavailable as native cash.
func TestLedgerSettledCashIsTyped(t *testing.T) {
	if got := accountSummaryRequestRowDisposition("All", "$LEDGER-SettledCash", "USD", "DU1234567", []string{"DU1234567"}); got != accountSummaryRowAcceptLedger {
		t.Fatalf("SettledCash ledger row disposition = %v", got)
	}
	ledger := extractCurrencyLedger(map[string]string{"$LEDGER:CashBalance_USD": "12000", "$LEDGER:SettledCash_USD": "11000", "$LEDGER:CashBalance_EUR": "500"})
	if usd := ledger["USD"]; !usd.SettledCashObserved || usd.SettledCash != 11000 || usd.CashBalance != 12000 {
		t.Fatalf("USD = %+v", usd)
	}
	if eur := ledger["EUR"]; eur.SettledCashObserved || eur.SettledCash != 0 {
		t.Fatalf("EUR settled cash invented: %+v", eur)
	}
	legacy := extractCurrencyLedger(map[string]string{"$LEDGER-SettledCash_GBP": "7", "$LEDGER-CashBalance_GBP": "9", "SettledCash_GBP": "40"})
	if gbp := legacy["GBP"]; !gbp.SettledCashObserved || gbp.SettledCash != 7 {
		t.Fatalf("10.47 dialect GBP = %+v", gbp)
	}
}

// A bare SettledCash is reqAccountUpdates' account-level value: one figure
// in the base currency covering every currency. In the streaming map it
// carries the base currency's suffix and must never read as that
// currency's settled cash, in either wire dialect.
func TestLedgerBareSettledCashIsAccountLevel(t *testing.T) {
	for name, raw := range map[string]map[string]string{
		"bare dialect":     {"CashBalance_EUR": "900", "SettledCash_EUR": "4000", "CashBalance_USD": "3000", "ExchangeRate_USD": "0.9"},
		"prefixed dialect": {"$LEDGER-CashBalance_EUR": "900", "SettledCash_EUR": "4000", "$LEDGER-CashBalance_USD": "3000"},
	} {
		ledger := extractCurrencyLedger(raw)
		if eur := ledger["EUR"]; eur.CashBalance != 900 || eur.SettledCashObserved || eur.SettledCash != 0 {
			t.Fatalf("%s: EUR = %+v", name, eur)
		}
		if usd := ledger["USD"]; usd.CashBalance != 3000 || usd.SettledCashObserved {
			t.Fatalf("%s: USD = %+v", name, usd)
		}
	}
}

// IBKR sends notice 2130 before the contract details of a factor-priced
// line, such as an inflation-linked Bund; the line carries it, and a line
// answered without it does not.
func TestBondLookupMarksAFactorPricedLine(t *testing.T) {
	conn, connector, socket, _, _ := newQueuedInstructionReconnectFixture(t)
	type result struct {
		lines []BondContractDetails
		err   error
	}
	done := make(chan result, 1)
	lookup := func(isin string) {
		lines, err := connector.BondContractDetails(context.Background(), BondContractRequest{IDType: BondIdentifierISIN, ID: isin, Currency: "EUR"}, 2*time.Second)
		done <- result{lines, err}
	}
	go lookup("DE000BU0ZZ19")
	reqID := waitForHandlerReqID(t, conn, msgBondContractData)
	waitForBondRequestFrame(t, conn, socket)
	conn.processMessageAtEpoch(inboundNotice(reqID, 2130, "Warning: SYNTH product is trading on the basis of currency price with factor"), conn.BrokerSessionEpoch())
	conn.dispatchHandlers(msgBondContractData, syntheticBondFrame(reqID, "880002", "DE000BU0ZZ19", "DE000BU0ZZ19", "EUR", "20270120", "20260722"), conn.BrokerSessionEpoch())
	prewarmTestEnd(conn, reqID)
	got := <-done
	if got.err != nil || len(got.lines) != 1 || !got.lines[0].FactorPriced {
		t.Fatalf("a line answered with notice 2130 is not marked factor-priced: %+v err %v", got.lines, got.err)
	}

	go lookup("DE000BU0ZZ19")
	next := waitForHandlerReqIDAfter(t, conn, msgBondContractData, reqID)
	conn.dispatchHandlers(msgBondContractData, syntheticBondFrame(next, "880002", "DE000BU0ZZ19", "DE000BU0ZZ19", "EUR", "20270120", "20260722"), conn.BrokerSessionEpoch())
	prewarmTestEnd(conn, next)
	got = <-done
	if got.err != nil || len(got.lines) != 1 || got.lines[0].FactorPriced {
		t.Fatalf("a line answered without the notice is marked factor-priced: %+v err %v", got.lines, got.err)
	}
}
