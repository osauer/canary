package ibkr

import (
	"context"
	"errors"
	"slices"
	"strconv"
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

// The request names the bond by ISIN (secIdType/secId) with its currency;
// the frames and the end marker complete it; a definition rejection ends the
// wait with the broker's verdict.
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
	sent := frames[len(frames)-1]
	if !slices.Contains(sent, "BOND") || !slices.Contains(sent, "ISIN") || !slices.Contains(sent, "DE000BU0ZZ19") || !slices.Contains(sent, "EUR") {
		t.Fatalf("request frame = %v", sent)
	}
	conn.dispatchHandlers(msgBondContractData, syntheticBondFrame(reqID, "880002", "DE000BU0ZZ19", "DE000BU0ZZ19", "EUR", "20270120", "20260722"), conn.BrokerSessionEpoch())
	prewarmTestEnd(conn, reqID)
	got := <-done
	if got.err != nil || len(got.lines) != 1 || got.lines[0].ConID != 880002 || got.lines[0].ISIN() != "DE000BU0ZZ19" || got.lines[0].Currency != "EUR" {
		t.Fatalf("lines = %+v err %v", got.lines, got.err)
	}

	go func() {
		_, err := connector.BondContractDetails(context.Background(), BondContractRequest{IDType: BondIdentifierCUSIP, ID: "912797ZZ3", Currency: "USD"}, 5*time.Second)
		done <- result{err: err}
	}()
	reqID = waitForHandlerReqIDAfter(t, conn, msgBondContractData, reqID)
	if !connector.failPendingContractDetails(reqID, 200, "No security definition has been found for the request") {
		t.Fatal("the bond request was not armed for the broker's rejection")
	}
	if got := <-done; !errors.Is(got.err, ErrContractNoDefinition) {
		t.Fatalf("rejection = %v", got.err)
	}
	// A malformed identifier never reaches the wire.
	if _, err := connector.BondContractDetails(context.Background(), BondContractRequest{IDType: BondIdentifierISIN, ID: "DE000BU0ZZ18", Currency: "EUR"}, time.Second); err == nil {
		t.Fatal("an ISIN with a bad check digit was sent")
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
	legacy := extractCurrencyLedger(map[string]string{"$LEDGER-SettledCash_GBP": "7", "$LEDGER-CashBalance_GBP": "9"})
	if gbp := legacy["GBP"]; !gbp.SettledCashObserved || gbp.SettledCash != 7 {
		t.Fatalf("10.47 dialect GBP = %+v", gbp)
	}
}
