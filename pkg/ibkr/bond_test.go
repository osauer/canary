package ibkr

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// syntheticBondFrame is a complete bondContractData frame at server version
// 188 or later: no version field, trading hours present, a secIdList with
// the CUSIP and ISIN, and the size rules. The identifiers are synthetic.
func syntheticBondFrame(reqID int, conID, cusip, isin, currency, maturity, issue string) []string {
	return []string{
		strconv.Itoa(msgBondContractData), strconv.Itoa(reqID), "SYNTHB", "BOND", cusip, "0", maturity + " 16:00:00 US/Eastern", issue,
		"", "GOVT", "ZERO", "0", "0", "0", "SYNTH 0 bill", "SMART", currency, "SYNTHB", "SYNTHB", conID, "0.0001",
		"LMT", "SMART", "", "", "0", "", "Synthetic Treasury Bill", "US/Eastern", "", "", "", "0",
		"2", "CUSIP", cusip, "ISIN", isin, "1", "1", "1000", "1000", "1000",
	}
}

// syntheticBillFrame is syntheticBondFrame for a line IBKR lists as BILL.
func syntheticBillFrame(reqID int, conID, cusip, isin, currency, maturity, issue string) []string {
	frame := syntheticBondFrame(reqID, conID, cusip, isin, currency, maturity, issue)
	frame[3] = "BILL"
	return frame
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
	if d, ok := parseBondContractDetails(syntheticBillFrame(7, "880001", "912797ZZ3", "US912797ZZ37", "USD", "20261126", "20260827"), 7, maxClientVersion); !ok || d.SecType != "BILL" {
		t.Fatalf("bill frame = %+v %v", d, ok)
	}
}

func TestParseBondContractDetailsCompleteFrame(t *testing.T) {
	frame := syntheticBondFrame(7, "880001", "912797ZZ3", "US912797ZZ37", "USD", "20261126", "20260827")
	d, ok := parseBondContractDetails(frame, 7, maxClientVersion)
	if !ok || !d.Complete {
		t.Fatalf("complete frame refused: %+v %v", d, ok)
	}
	if d.ConID != 880001 || d.Currency != "USD" || d.Maturity != "20261126" || d.IssueDate != "20260827" || d.SecType != "BOND" ||
		d.MinTick != 0.0001 || d.MinSize != 1000 || d.SizeIncrement != 1000 || d.LongName != "Synthetic Treasury Bill" {
		t.Fatalf("decoded = %+v", d)
	}
	if d.ISIN() != "US912797ZZ37" || d.CUSIP() != "912797ZZ3" {
		t.Fatalf("identifiers = %q %q", d.ISIN(), d.CUSIP())
	}
	if _, ok := parseBondContractDetails(frame, 8, maxClientVersion); ok {
		t.Fatal("a frame for another request was accepted")
	}
}

// A frame whose tail does not decode keeps its identity prefix and drops the
// size rules and identifiers; a frame whose prefix does not decode (a shifted
// contract id, no maturity, a malformed currency) is refused.
func TestParseBondContractDetailsFailsClosed(t *testing.T) {
	frame := syntheticBondFrame(7, "880001", "912797ZZ3", "US912797ZZ37", "USD", "20261126", "20260827")
	truncated := slices.Clone(frame[:36])
	d, ok := parseBondContractDetails(truncated, 7, maxClientVersion)
	if !ok || d.Complete || d.MinSize != 0 || d.SecIDs != nil || d.ConID != 880001 || d.CUSIP() != "912797ZZ3" {
		t.Fatalf("truncated tail = %+v %v", d, ok)
	}
	for name, mutate := range map[string]func([]string){
		"contract id not a number": func(f []string) { f[19] = "SMART" },
		"no maturity":              func(f []string) { f[6] = "" },
		"currency malformed":       func(f []string) { f[16] = "US" },
		"coupon not a number":      func(f []string) { f[5] = "zero" },
		"flag not boolean":         func(f []string) { f[12] = "maybe" },
	} {
		bad := slices.Clone(frame)
		mutate(bad)
		if d, ok := parseBondContractDetails(bad, 7, maxClientVersion); ok {
			t.Fatalf("%s: accepted %+v", name, d)
		}
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

// A CUSIP IBKR does not find by symbol is asked again by secIdType/secId.
// When neither form finds a line, the error keeps both answers with IBKR's
// own code and text and still reads as the no-definition verdict; when the
// second form answers, its line is the result.
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
	got := <-done
	lookupErr, ok := errors.AsType[*BondLookupError](got.err)
	if !ok || !errors.Is(got.err, ErrContractNoDefinition) || len(lookupErr.Attempts) != 2 {
		t.Fatalf("rejection = %#v", got.err)
	}
	for i, form := range []string{"BOND by symbol", "BOND by secIdType CUSIP"} {
		if a := lookupErr.Attempts[i]; a.Form != form || a.Code != 200 || a.Message != noDefinition {
			t.Fatalf("attempt %d = %+v", i, a)
		}
	}
	if want := `BOND by symbol: IBKR 200 "` + noDefinition + `"`; !strings.Contains(got.err.Error(), want) || !strings.Contains(got.err.Error(), "BOND CUSIP 912797ZZ3 on SMART in USD") {
		t.Fatalf("error text = %q", got.err.Error())
	}

	// The symbol form answers a line that names another CUSIP: it is not the
	// bill asked for, and the secId form's line is.
	ask()
	nthBondRequestFrame(t, conn, socket, 3)
	reqID = waitForHandlerReqIDAfter(t, conn, msgBondContractData, reqID)
	conn.dispatchHandlers(msgBondContractData, syntheticBondFrame(reqID, "880009", "912797ZY6", "", "USD", "20261126", "20260827"), conn.BrokerSessionEpoch())
	prewarmTestEnd(conn, reqID)
	nthBondRequestFrame(t, conn, socket, 4)
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

	// A German ISIN asked as BILL then BOND: neither BILL form finds a line,
	// the BOND symbol form does.
	ask(BondContractRequest{IDType: BondIdentifierISIN, ID: "DE000BU0ZZ19", Currency: "EUR", SecTypes: []string{"BILL", "BOND"}})
	assertBillOrBondRequestFrame(t, nthBondRequestFrame(t, conn, socket, 2), "BILL", "DE000BU0ZZ19", "EUR", "", "")
	reqID = waitForHandlerReqIDAfter(t, conn, msgBondContractData, reqID)
	if !connector.failPendingContractDetails(reqID, 200, noDefinition) {
		t.Fatal("the BILL symbol request was not armed for the broker's rejection")
	}
	assertBillOrBondRequestFrame(t, nthBondRequestFrame(t, conn, socket, 3), "BILL", "", "EUR", "ISIN", "DE000BU0ZZ19")
	reqID = waitForHandlerReqIDAfter(t, conn, msgBondContractData, reqID)
	prewarmTestEnd(conn, reqID)
	assertBillOrBondRequestFrame(t, nthBondRequestFrame(t, conn, socket, 4), "BOND", "DE000BU0ZZ19", "EUR", "", "")
	reqID = waitForHandlerReqIDAfter(t, conn, msgBondContractData, reqID)
	conn.dispatchHandlers(msgBondContractData, syntheticBondFrame(reqID, "880002", "DE000BU0ZZ19", "DE000BU0ZZ19", "EUR", "20270120", "20260722"), conn.BrokerSessionEpoch())
	prewarmTestEnd(conn, reqID)
	if got := <-done; got.err != nil || len(got.lines) != 1 || got.lines[0].SecType != "BOND" || got.lines[0].ConID != 880002 {
		t.Fatalf("bond lines = %+v err %v", got.lines, got.err)
	}

	// Asked as BILL then BOND and found by neither: four attempts, in order.
	ask(BondContractRequest{IDType: BondIdentifierCUSIP, ID: "912797ZZ3", Currency: "USD", SecTypes: []string{"BILL", "BOND"}})
	for n := 5; n <= 8; n++ {
		nthBondRequestFrame(t, conn, socket, n)
		reqID = waitForHandlerReqIDAfter(t, conn, msgBondContractData, reqID)
		if !connector.failPendingContractDetails(reqID, 200, noDefinition) {
			t.Fatalf("request %d was not armed for the broker's rejection", n)
		}
	}
	got := <-done
	lookupErr, ok := errors.AsType[*BondLookupError](got.err)
	if !ok || !errors.Is(got.err, ErrContractNoDefinition) || len(lookupErr.Attempts) != 4 {
		t.Fatalf("rejection = %#v", got.err)
	}
	for i, form := range []string{"BILL by symbol", "BILL by secIdType CUSIP", "BOND by symbol", "BOND by secIdType CUSIP"} {
		if a := lookupErr.Attempts[i]; a.Form != form || a.Code != 200 || a.Message != noDefinition {
			t.Fatalf("attempt %d = %+v", i, a)
		}
	}
	if !strings.Contains(got.err.Error(), "BILL then BOND CUSIP 912797ZZ3 on SMART in USD") {
		t.Fatalf("error text = %q", got.err.Error())
	}
}

// A contract id is asked once per type, by id alone; an identifier is asked
// by symbol, then by secIdType/secId, on SMART unless an exchange is named,
// every form of one type before the next type.
func TestBondContractRequestForms(t *testing.T) {
	forms, err := BondContractRequest{ConID: 880001, Currency: "usd"}.wireForms()
	if err != nil || len(forms) != 1 || forms[0].contract.ConID != 880001 || forms[0].contract.Exchange != "" || forms[0].contract.Symbol != "" || forms[0].contract.SecIDType != "" {
		t.Fatalf("contract id forms = %+v %v", forms, err)
	}
	forms, err = BondContractRequest{IDType: "cusip", ID: "912797zz3", Currency: "USD"}.wireForms()
	if err != nil || len(forms) != 2 {
		t.Fatalf("CUSIP forms = %+v %v", forms, err)
	}
	if c := forms[0].contract; c.Symbol != "912797ZZ3" || c.SecIDType != "" || c.SecID != "" || c.SecType != "BOND" || c.Exchange != "SMART" || c.Currency != "USD" {
		t.Fatalf("symbol form = %+v", c)
	}
	if c := forms[1].contract; c.Symbol != "" || c.SecIDType != "CUSIP" || c.SecID != "912797ZZ3" || c.Exchange != "SMART" {
		t.Fatalf("secId form = %+v", c)
	}
	forms, err = BondContractRequest{IDType: "ISIN", ID: "DE000BU0ZZ19", Currency: "EUR", SecTypes: []string{"bill", "BOND", "BILL"}}.wireForms()
	if err != nil || len(forms) != 4 {
		t.Fatalf("BILL then BOND forms = %+v %v", forms, err)
	}
	for i, want := range []struct{ label, secType string }{{"BILL by symbol", "BILL"}, {"BILL by secIdType ISIN", "BILL"}, {"BOND by symbol", "BOND"}, {"BOND by secIdType ISIN", "BOND"}} {
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

// assertBondRequestFrame checks a BOND reqContractDetails frame's symbol,
// exchange, currency and secIdType/secId.
func assertBondRequestFrame(t *testing.T, frame []string, symbol, currency, secIDType, secID string) {
	t.Helper()
	assertBillOrBondRequestFrame(t, frame, "BOND", symbol, currency, secIDType, secID)
}

// assertBillOrBondRequestFrame checks a reqContractDetails frame's secType,
// symbol, exchange, currency and secIdType/secId.
func assertBillOrBondRequestFrame(t *testing.T, frame []string, secType, symbol, currency, secIDType, secID string) {
	t.Helper()
	assertFields(t, frame, []fieldAssertion{
		{0, strconv.Itoa(reqContractData), "message"}, {3, "0", "conId"}, {4, symbol, "symbol"}, {5, secType, "secType"},
		{10, "SMART", "exchange"}, {12, currency, "currency"}, {16, secIDType, "secIdType"}, {17, secID, "secId"},
	})
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

// SettledCash is a typed $LEDGER field: admitted from an Account=All row on
// a single-account login, projected per currency, and unobserved (never
// zero) when the gateway sends none.
func TestLedgerSettledCashIsTyped(t *testing.T) {
	if got := accountSummaryRequestRowDisposition("All", "SettledCash", "USD", "DU1234567", []string{"DU1234567"}); got != accountSummaryRowAcceptLedger {
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
