package ibkr

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
)

// Pure parsing and classification: symbol routing hints, bond identifier
// checks and lookup records, and the protobuf parsers of the read-only API
// configuration diagnostic. The identifiers are synthetic (they pass the
// check digit, they name nothing).

func TestClassifySymbolRoutingTable(t *testing.T) {
	cases := map[string][4]string{
		"VIX":     {"IND", "CBOE", "USD", "CBOE"},
		"VVIX":    {"IND", "CBOE", "USD", "CBOE"},
		"VIX3M":   {"IND", "CBOE", "USD", "CBOE"},
		"SPX":     {"IND", "CBOE", "USD", "CBOE"},
		"NDX":     {"IND", "NASDAQ", "USD", "NASDAQ"},
		"RUT":     {"IND", "RUSSELL", "USD", "RUSSELL"},
		"DJI":     {"IND", "CBOE", "USD", "CBOE"},
		"DJX":     {"IND", "CBOE", "USD", "CBOE"},
		"DXY":     {"IND", "ICEUS", "USD", "ICEUS"},
		"SPY":     {"STK", "SMART", "USD", "ARCA"},
		"IBIT":    {"STK", "SMART", "USD", "ARCA"},
		"XLRE":    {"STK", "SMART", "USD", "ARCA"},
		"SYNX":    {"STK", "SMART", "USD", ""},
		"EUR.USD": {"CASH", "IDEALPRO", "USD", "IDEALPRO"},
		"usd/jpy": {"CASH", "IDEALPRO", "JPY", "IDEALPRO"},
		"ES":      {"STK", "SMART", "USD", ""}, // no implicit future
	}
	for symbol, want := range cases {
		secType, exchange, currency, primary := classifySymbol(symbol)
		if got := [4]string{secType, exchange, currency, primary}; got != want {
			t.Errorf("classifySymbol(%q) = %v, want %v", symbol, got, want)
		}
	}
	if DefaultHistoricalCurrency(" eur.gbp ") != "GBP" || DefaultHistoricalCurrency("spy") != "USD" {
		t.Fatal("DefaultHistoricalCurrency")
	}
	// Option underlyings take the stock's primary hint; an index has none.
	if optionUnderlyingPrimaryExchangeHint(" spy ") != "ARCA" || optionUnderlyingPrimaryExchangeHint("SYNX") != "" || optionUnderlyingPrimaryExchangeHint("SPX") != "" {
		t.Fatal("optionUnderlyingPrimaryExchangeHint")
	}
	for name, tc := range map[string]struct {
		symbol, secType, local, class string
	}{
		"option":          {"synx", "OPT", "", "SYNX"},
		"named index":     {"vix", "IND", "VIX", "VIX"},
		"other index":     {"RUT", "IND", "", ""},
		"commodity":       {"xauusd", "CMDTY", "XAUUSD", "XAUUSD"},
		"stock":           {"SYNX", "STK", "", ""},
		"dollar index":    {"DXY", "IND", "DXY", "DXY"},
		"three month vix": {"VIX3M", "IND", "VIX3M", "VIX3M"},
	} {
		local, class := contractDisplayHints(tc.symbol, tc.secType)
		if local != tc.local || class != tc.class {
			t.Errorf("%s: hints %q %q, want %q %q", name, local, class, tc.local, tc.class)
		}
	}
}

func TestFxPairRecognisesTheCurrenciesCanaryReads(t *testing.T) {
	for symbol, want := range map[string][2]string{
		"EUR.USD": {"EUR", "USD"}, "usd/jpy": {"USD", "JPY"}, " gbp.chf ": {"GBP", "CHF"}, "AUD/NZD": {"AUD", "NZD"}, "CAD.USD": {"CAD", "USD"},
		"USD.HKD": {"USD", "HKD"}, "EUR.SGD": {"EUR", "SGD"}, "SGD.HKD": {"SGD", "HKD"}, "HKD.JPY": {"HKD", "JPY"},
	} {
		base, quote, ok := FxPair(symbol)
		if !ok || base != want[0] || quote != want[1] {
			t.Errorf("FxPair(%q) = %q %q %t", symbol, base, quote, ok)
		}
	}
	for _, symbol := range []string{"EURUSD", "EUR.USD.JPY", "EUR/USD/JPY", "EUR.US", "EURO.USD", "SEK.USD", "USD.MXN", "BRK.B", "", "./"} {
		if _, _, ok := FxPair(symbol); ok {
			t.Errorf("FxPair(%q) accepted", symbol)
		}
	}
	if dualClassWireSymbol(" brk.b ") != "BRK B" || dualClassWireSymbol("BF.B") != "BF B" || dualClassWireSymbol("EUR.USD") != "EUR.USD" || dualClassWireSymbol("SYNX") != "SYNX" {
		t.Fatal("dualClassWireSymbol")
	}
}

func TestBondIdentifiersAndDates(t *testing.T) {
	// Index 11 must be a digit; indexes 0 and 1 must be letters.
	if ValidISIN("US912797ZZ3A") || ValidISIN("U5912797ZZ37") || ValidISIN("US912797ZZ3-") {
		t.Fatal("ISIN shape")
	}
	if ValidCUSIP("912797ZZ-") || ValidCUSIP("91279-ZZ3") || !ValidCUSIP("912797ZZ3") {
		t.Fatal("CUSIP shape")
	}
	// The special CUSIP characters carry their published values.
	for _, s := range []string{"912797*Z", "912797@Z", "912797#Z"} {
		found := false
		for d := range 10 {
			if ValidCUSIP(s + strconv.Itoa(d)) {
				found = true
			}
		}
		if !found {
			t.Errorf("no check digit satisfies %s", s)
		}
	}
	if luhnValid("") || !luhnValid("79927398713") || luhnValid("79927398710") {
		t.Fatal("luhn")
	}
	for ccy, want := range map[string]bool{"USD": true, "usd": false, "US": false, "USDX": false, "U$D": false} {
		if bondCurrencyCode(ccy) != want {
			t.Errorf("bondCurrencyCode(%q) = %t", ccy, !want)
		}
	}
	for raw, want := range map[string]string{
		"20261001 16:00:00 US/Eastern": "20261001", "2026-10-01": "20261001", "20261001-16:00:00": "20261001", "20261001": "20261001", "": "", "soon": "soon",
	} {
		if got := bondWireDate(raw); got != want {
			t.Errorf("bondWireDate(%q) = %q, want %q", raw, got, want)
		}
	}

	d := BondContractDetails{SecIDs: map[string]string{BondIdentifierISIN: " us912797zz37 ", BondIdentifierCUSIP: "912797ZZ3"}, IssueDate: "20260101", Maturity: "2026-13-01"}
	if d.ISIN() != "US912797ZZ37" || d.CUSIP() != "912797ZZ3" {
		t.Fatalf("identifiers from the secIdList: %q %q", d.ISIN(), d.CUSIP())
	}
	if issue, ok := d.IssueDateValue(); !ok || issue.Year() != 2026 || issue.Month() != 1 {
		t.Fatalf("issue date %v %t", issue, ok)
	}
	if _, ok := d.MaturityDate(); ok {
		t.Fatal("an impossible maturity parsed")
	}
	// The cusip field may carry an ISIN; a CUSIP derives from a US or CA ISIN.
	fromField := BondContractDetails{CUSIPField: "US912797ZZ37"}
	if fromField.ISIN() != "US912797ZZ37" || fromField.CUSIP() != "912797ZZ37"[:9] {
		t.Fatalf("identifiers from the cusip field: %q %q", fromField.ISIN(), fromField.CUSIP())
	}
	if (BondContractDetails{CUSIPField: "912797ZZ3"}).CUSIP() != "912797ZZ3" {
		t.Fatal("cusip field as CUSIP")
	}
	if (BondContractDetails{SecIDs: map[string]string{BondIdentifierISIN: "DE000BU0ZZ19"}}).CUSIP() != "" {
		t.Fatal("a CUSIP derived from a German ISIN")
	}
	if (BondContractDetails{CUSIPField: "bad"}).ISIN() != "" || (BondContractDetails{}).CUSIP() != "" {
		t.Fatal("identifiers from nothing")
	}
}

func TestBondContractRequestDescriptionAndLineFilter(t *testing.T) {
	byID := BondContractRequest{IDType: "isin", ID: " us912797zz37 ", Currency: "usd"}
	if got := byID.description(); got != "BOND ISIN US912797ZZ37 on SMART in USD" {
		t.Fatalf("description %q", got)
	}
	both := BondContractRequest{IDType: "CUSIP", ID: "912797ZZ3", Currency: "USD", Exchange: "smart", SecTypes: []string{"bill", "BOND", "BILL"}}
	if got := both.description(); got != "BILL then BOND CUSIP 912797ZZ3 on SMART in USD" {
		t.Fatalf("description %q", got)
	}
	if got := (BondContractRequest{ConID: 4242, Currency: "EUR", SecTypes: []string{"bond"}}).description(); got != "BOND contract id 4242 in EUR" {
		t.Fatalf("description %q", got)
	}
	if got := (BondContractRequest{ConID: 4242, Currency: "EUR", SecTypes: []string{"STK"}}).description(); !strings.HasPrefix(got, "BOND contract id") {
		t.Fatalf("description with a refused type %q", got)
	}
	if _, err := (BondContractRequest{SecTypes: []string{"STK"}}).AskedSecTypes(); err == nil {
		t.Fatal("STK accepted as a bond type")
	}

	isinLine := BondContractDetails{ConID: 1, SecIDs: map[string]string{BondIdentifierISIN: "US912797ZZ37", BondIdentifierCUSIP: "912797ZZ3"}}
	otherLine := BondContractDetails{ConID: 2, SecIDs: map[string]string{BondIdentifierISIN: "DE000BU0ZZ19"}}
	bareLine := BondContractDetails{ConID: 3}
	lines := []BondContractDetails{isinLine, otherLine, bareLine}
	kept := byID.linesNaming(append([]BondContractDetails(nil), lines...))
	if len(kept) != 2 || kept[0].ConID != 1 || kept[1].ConID != 3 {
		t.Fatalf("ISIN filter kept %+v", kept)
	}
	kept = both.linesNaming(append([]BondContractDetails(nil), lines...))
	if len(kept) != 3 {
		t.Fatalf("CUSIP filter dropped a line that names no CUSIP: %+v", kept)
	}
	foreign := BondContractDetails{ConID: 4, SecIDs: map[string]string{BondIdentifierCUSIP: "912796ZZ6"}}
	if kept = both.linesNaming([]BondContractDetails{foreign}); len(kept) != 0 && ValidCUSIP("912796ZZ6") {
		t.Fatalf("CUSIP filter kept another CUSIP: %+v", kept)
	}
	if kept = (BondContractRequest{ConID: 4242}).linesNaming(append([]BondContractDetails(nil), lines...)); len(kept) != 3 {
		t.Fatal("a contract-id request filtered lines")
	}
}

func TestBondLookupErrorsAndMisses(t *testing.T) {
	rejection := &ContractDetailsRejection{Code: 200, Message: "No security definition has been found for the request"}
	if rejection.Error() != `IBKR 200 "No security definition has been found for the request"` {
		t.Fatalf("rejection text %q", rejection.Error())
	}
	if !errors.Is(rejection, ErrContractNoDefinition) || errors.Is(&ContractDetailsRejection{Code: 200, Message: "other"}, ErrContractNoDefinition) || errors.Is(&ContractDetailsRejection{Code: 321, Message: "no security definition"}, ErrContractNoDefinition) {
		t.Fatal("code-200 classification")
	}
	attempt := BondLookupAttempt{Form: "BOND by symbol"}
	got, ok := bondLookupMiss(attempt, fmt.Errorf("wrapped: %w", rejection))
	if !ok || got.Outcome != BondAttemptRejected || got.Code != 200 || got.Message != rejection.Message {
		t.Fatalf("rejection miss: %+v %t", got, ok)
	}
	if got.String() != `BOND by symbol: IBKR 200 "No security definition has been found for the request"` {
		t.Fatalf("attempt line %q", got.String())
	}
	got, ok = bondLookupMiss(attempt, ErrBondContractNotFound)
	if !ok || got.Outcome != BondAttemptNoLine || got.Message != "the search ended without a line" {
		t.Fatalf("bare not-found miss: %+v %t", got, ok)
	}
	got, ok = bondLookupMiss(attempt, fmt.Errorf("%w: IBKR sent nothing", ErrBondContractNotFound))
	if !ok || got.Outcome != BondAttemptNoLine || got.Message != "IBKR sent nothing" {
		t.Fatalf("noted not-found miss: %+v %t", got, ok)
	}
	if got.String() != "BOND by symbol: IBKR sent nothing" {
		t.Fatalf("no-line attempt line %q", got.String())
	}
	got, ok = bondLookupMiss(attempt, errors.New("timed\tout"))
	if ok || got.Outcome != BondAttemptFailed || got.Message != "timed out" {
		t.Fatalf("failure: %+v %t", got, ok)
	}

	lookup := &BondLookupError{Request: "BOND ISIN US912797ZZ37 on SMART in USD", Attempts: []BondLookupAttempt{
		{Form: "BOND by symbol", Outcome: BondAttemptNoLine, Message: "the search ended without a line"},
		{Form: "BOND by secIdType ISIN", Outcome: BondAttemptRejected, Code: 200, Message: "No security definition has been found for the request"},
	}}
	if !errors.Is(lookup, ErrBondContractNotFound) || !errors.Is(lookup, ErrContractNoDefinition) {
		t.Fatal("lookup error does not unwrap its answers")
	}
	if !strings.Contains(lookup.Error(), lookup.Request) || !strings.Contains(lookup.Answers(), "; BOND by secIdType ISIN: IBKR 200") {
		t.Fatalf("lookup text %q", lookup.Error())
	}
	if len(lookup.Unwrap()) != 2 {
		t.Fatal("unwrap count")
	}
	if errors.Is(&BondLookupError{Attempts: []BondLookupAttempt{{Code: 321, Message: "x"}}}, ErrContractNoDefinition) {
		t.Fatal("a 321 rejection classified as no definition")
	}

	long := strings.Repeat("é", 250)
	line := brokerNoticeLine("a\x00b\u0007c " + long)
	if !strings.HasPrefix(line, "a b c ") || !strings.HasSuffix(line, "…") || len([]rune(line)) != 201 {
		t.Fatalf("notice line %d runes, prefix %q", len([]rune(line)), line[:10])
	}
	if brokerNoticeLine("") != "" {
		t.Fatal("empty notice")
	}
}

func TestBondYieldTicksIgnoreSentinels(t *testing.T) {
	sub := &Subscription{}
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1, -2} {
		sub.recordBondYield(tickBidYield, v)
	}
	if sub.bidYield != nil {
		t.Fatalf("sentinel yield kept: %v", *sub.bidYield)
	}
	sub.recordBondYield(tickDelayedBidYield, 4.1)
	sub.recordBondYield(tickAskYield, 4.2)
	sub.recordBondYield(tickDelayedLastYld, -0.5) // a negative yield is a value
	sub.recordBondYield(99, 7)                    // not a yield tick
	if sub.bidYield == nil || *sub.bidYield != 4.1 || sub.askYield == nil || *sub.askYield != 4.2 || sub.lastYield == nil || *sub.lastYield != -0.5 {
		t.Fatalf("yields %v %v %v", sub.bidYield, sub.askYield, sub.lastYield)
	}
	copied := cloneYield(sub.bidYield)
	*copied = 9
	if *sub.bidYield != 4.1 || cloneYield(nil) != nil {
		t.Fatal("cloneYield aliases or mishandles nil")
	}
	if !isBondMarketDataContract(Contract{SecType: "bill"}) || isBondMarketDataContract(Contract{SecType: "STK"}) {
		t.Fatal("isBondMarketDataContract")
	}
}

func TestBondLookupFrameRecords(t *testing.T) {
	line := BondLookupFrame{MessageID: msgBondContractData, Kind: BondFrameBondContractData, Fields: 40, Layout: "server 203", ConID: "4242", SecType: "BOND", Currency: "USD", Maturity: "20261001", Line: true, Complete: true}
	if got := line.String(); got != `msg 18 bondContractData, 40 fields, layout "server 203", con_id "4242", sec_type "BOND", currency "USD", maturity "20261001": a line, whole frame decoded` {
		t.Fatalf("complete line %q", got)
	}
	partial := line
	partial.Complete, partial.Note = false, "no maturity"
	if got := partial.String(); !strings.HasSuffix(got, ": a line: no maturity") {
		t.Fatalf("partial line %q", got)
	}
	refused := BondLookupFrame{MessageID: msgContractData, Kind: BondFrameContractData, Fields: 3, Note: "the frame carries no positive contract id"}
	if got := refused.String(); got != "msg 10 contractData, 3 fields: not a line: the frame carries no positive contract id" {
		t.Fatalf("refused line %q", got)
	}
	errFrame := BondLookupFrame{MessageID: msgErrMsg, Kind: BondFrameError, Fields: 5, Code: 200, Note: "no definition"}
	if got := errFrame.String(); got != "msg 4 error, 5 fields, code 200: no definition" {
		t.Fatalf("error line %q", got)
	}
	if (BondLookupFrame{Kind: BondFrameOther}).contract() || !refused.contract() {
		t.Fatal("contract kind")
	}

	for _, tc := range []struct {
		layout bondFrameLayout
		want   string
		reqAt  int
	}{
		{bondFrameLayoutFor(203), "server 203", 1},
		{bondFrameLayoutFor(150), "server 150", 2},
		{bondFrameLayout{serverVersion: 203, versionField: true}, "server 203, read with a message version", 2},
		{bondFrameLayout{serverVersion: 150, versionField: false}, "server 150, read without a message version", 1},
	} {
		if got := tc.layout.String(); got != tc.want {
			t.Errorf("layout %+v named %q, want %q", tc.layout, got, tc.want)
		}
		if tc.layout.reqIDIndex() != tc.reqAt {
			t.Errorf("layout %+v request id at %d", tc.layout, tc.layout.reqIDIndex())
		}
	}

	// bondTapFrame keeps error, notice and foreign frames that name the
	// request; contract frames belong to the request's own handlers.
	if f, ok := bondTapFrame([]string{"4", "9001", "200", "No security definition"}, 9001); !ok || f.Kind != BondFrameError || f.Code != 200 || f.Note != "No security definition" {
		t.Fatalf("current error frame: %+v %t", f, ok)
	}
	if f, ok := bondTapFrame([]string{"4", "2", "9001", "321", "bad\x00request"}, 9001); !ok || f.Code != 321 || f.Note != "bad request" {
		t.Fatalf("legacy error frame: %+v %t", f, ok)
	}
	if _, ok := bondTapFrame([]string{"4", "2", "9002", "321", "x"}, 9001); ok {
		t.Fatal("another request's error kept")
	}
	notice := inboundNotice(9001, 2130, "trading on the basis of currency price with factor")
	if f, ok := bondTapFrame([]string{"204", string(notice[4:])}, 9001); !ok || f.Kind != BondFrameError || f.Code != 2130 {
		t.Fatalf("notice: %+v %t", f, ok)
	}
	if _, ok := bondTapFrame([]string{"204", string(inboundNotice(9002, 2130, "x")[4:])}, 9001); ok {
		t.Fatal("another request's notice kept")
	}
	if _, ok := bondTapFrame([]string{"204", "\xff"}, 9001); ok {
		t.Fatal("malformed notice kept")
	}
	for _, id := range []string{"10", "18", "52"} {
		if _, ok := bondTapFrame([]string{id, "9001"}, 9001); ok {
			t.Fatalf("contract frame %s tapped", id)
		}
	}
	if f, ok := bondTapFrame([]string{"3", "9001", "x"}, 9001); !ok || f.Kind != BondFrameOther || f.Fields != 3 {
		t.Fatalf("other frame by position 1: %+v %t", f, ok)
	}
	if f, ok := bondTapFrame([]string{"5", "1", "9001"}, 9001); !ok || f.Kind != BondFrameOther {
		t.Fatalf("other frame by position 2: %+v %t", f, ok)
	}
	if _, ok := bondTapFrame([]string{"5", "1", "2"}, 9001); ok {
		t.Fatal("unrelated frame kept")
	}
	if _, ok := bondTapFrame([]string{"x"}, 9001); ok {
		t.Fatal("non-numeric message kept")
	}

	if wireValue(" a\x01bé ") != "a?b?" || wireValue(strings.Repeat("x", 41)) != strings.Repeat("x", 40)+"…" {
		t.Fatal("wireValue")
	}

	// The per-form log caps what it keeps and counts the rest; emptyNote
	// names why a form ended without a line.
	log := &bondFrameLog{}
	for i := range bondFrameLogMax + 2 {
		log.add(BondLookupFrame{MessageID: msgErrMsg, Kind: BondFrameOther, Fields: i})
	}
	frames, omitted := log.snapshot()
	if len(frames) != bondFrameLogMax || omitted != 2 {
		t.Fatalf("log kept %d, omitted %d", len(frames), omitted)
	}
	if got := log.emptyNote(); got != fmt.Sprintf("the search ended without a line: %d frames of other messages named the request before its end marker", bondFrameLogMax+2) {
		t.Fatalf("other-frames note %q", got)
	}
	if got := (&bondFrameLog{}).emptyNote(); got != "the search ended without a line: no frame named the request before its end marker" {
		t.Fatalf("empty note %q", got)
	}
	one := &bondFrameLog{}
	one.add(BondLookupFrame{Kind: BondFrameContractDataEnd})
	one.add(BondLookupFrame{Kind: BondFrameBondContractData, Note: "the frame carries no positive contract id"})
	if got := one.emptyNote(); got != "the search ended without a line: IBKR sent 1 contract frame that did not decode (bondContractData: the frame carries no positive contract id)" {
		t.Fatalf("one refused note %q", got)
	}
	one.add(BondLookupFrame{Kind: BondFrameContractData, Note: "later"})
	if got := one.emptyNote(); got != "the search ended without a line: IBKR sent 2 contract frames that did not decode (the first bondContractData: the frame carries no positive contract id)" {
		t.Fatalf("two refused note %q", got)
	}

	// The wire reader: a finished frame may leave only the closing empty field.
	r := &wireReader{fields: []string{"1", ""}, idx: 1}
	if err := r.finish(bondFrameLayoutFor(203)); err != nil {
		t.Fatal(err)
	}
	r = &wireReader{fields: []string{"1", "2", "3"}, idx: 1}
	if err := r.finish(bondFrameLayoutFor(203)); err == nil || !strings.Contains(err.Error(), "2 fields past the 1") {
		t.Fatalf("unread fields: %v", err)
	}
	r = &wireReader{fields: []string{"x"}}
	if r.integer("count"); r.err == nil {
		t.Fatal("non-integer accepted")
	}
	if err := r.finish(bondFrameLayoutFor(203)); err != r.err {
		t.Fatal("finish hides the reader's error")
	}
	r = &wireReader{fields: []string{"", "1.5", "nan", "2"}}
	if r.number("a") != 0 || r.number("b") != 1.5 || r.err != nil {
		t.Fatalf("numbers: %v", r.err)
	}
	if r.number("c"); r.err == nil {
		t.Fatal("NaN accepted")
	}
	r = &wireReader{fields: []string{"1", "2"}}
	r.flag("a")
	if r.err != nil {
		t.Fatal(r.err)
	}
	r.flag("b")
	if r.err == nil {
		t.Fatal("flag 2 accepted")
	}
	r = &wireReader{fields: []string{"2", "CUSIP", "912797ZZ3", "", "x"}}
	if tags := r.tagValues("secIdList"); len(tags) != 1 || tags["CUSIP"] != "912797ZZ3" || r.err != nil {
		t.Fatalf("tag values %v err %v", tags, r.err)
	}
	r = &wireReader{fields: []string{"3", "a", "b"}}
	if r.tagValues("secIdList"); r.err == nil {
		t.Fatal("tag count beyond the frame accepted")
	}
	if err := bondLineIdentity(BondContractDetails{ConID: 1, Currency: "usd"}, nil); err == nil {
		t.Fatal("lower-case currency accepted as a code")
	}
	if err := bondLineIdentity(BondContractDetails{}, nil); err == nil || !strings.Contains(err.Error(), "contract id") {
		t.Fatalf("missing contract id: %v", err)
	}
	if err := bondLineIdentity(BondContractDetails{ConID: 1}, errors.New("short")); err == nil || err.Error() != "short" {
		t.Fatalf("decode error precedence: %v", err)
	}
	gaps := bondLineGaps(BondContractDetails{Maturity: "soon"}, "soon")
	if len(gaps) != 2 || !strings.Contains(gaps[0], "unreadable") || !strings.Contains(gaps[1], "no currency") {
		t.Fatalf("gaps %v", gaps)
	}
	if gaps := bondLineGaps(BondContractDetails{Maturity: "20261001", Currency: "USD"}, "20261001"); len(gaps) != 0 {
		t.Fatalf("gaps on a whole line %v", gaps)
	}
	if gaps := bondLineGaps(BondContractDetails{Currency: "USD"}, ""); len(gaps) != 1 || gaps[0] != "no maturity" {
		t.Fatalf("gaps without maturity %v", gaps)
	}
}

func TestAPIConfigParsersRefuseMalformedSections(t *testing.T) {
	// An unconnected connector reports the read as unavailable.
	c := NewConnector(&ConnectorConfig{})
	t.Cleanup(c.conn.rateLimiter.Stop)
	if got := c.ReadAPIConfig(t.Context()); got.State != "unavailable" || got.Reason != "not_connected" || got.AsOf.IsZero() {
		t.Fatalf("%+v", got)
	}

	id, code, err := diagnosticError(protoAppendInt32(protoAppendInt32(protoAppendString(nil, 4, "text"), 1, 7), 3, 321))
	if err != nil || id != 7 || code != 321 {
		t.Fatalf("diagnostic error %d %d %v", id, code, err)
	}
	if _, _, err := diagnosticError(protoAppendString(nil, 3, "x")); err == nil {
		t.Fatal("code as string accepted")
	}
	if id, _, err := diagnosticError(nil); err != nil || id != -1 {
		t.Fatalf("empty error body %d %v", id, err)
	}

	lock, err := parseLockAndExit(protoAppendString(protoAppendString(protoAppendString(protoAppendInt32(nil, 9, 1), 1, " 01:45 "), 2, "pm"), 3, "LOGOFF"))
	if err != nil || *lock != (LockAndExit{Time: "01:45", Period: "pm", Type: "logoff"}) {
		t.Fatalf("lock and exit %+v %v", lock, err)
	}
	if _, err := parseLockAndExit(protoAppendInt32(nil, 2, 1)); err == nil {
		t.Fatal("period as varint accepted")
	}
	if _, err := parseLockAndExit(protoAppendString(protoAppendString(nil, 1, "a"), 1, "b")); err == nil {
		t.Fatal("duplicate time accepted")
	}

	settings := protoAppendBool(nil, 1, true)
	if out, err := parseAPIConfig(protoAppendMessage(protoAppendInt32(nil, 1, 1), 4, protoAppendMessage(nil, 2, settings)), 1); err != nil || out.settings == nil || out.settings.ReadOnly == nil || !*out.settings.ReadOnly {
		t.Fatalf("minimal config %+v %v", out, err)
	}
	for name, body := range map[string][]byte{
		"request id as string":  protoAppendMessage(protoAppendString(nil, 1, "1"), 4, protoAppendMessage(nil, 2, settings)),
		"lock as varint":        protoAppendMessage(protoAppendInt32(protoAppendInt32(nil, 1, 1), 2, 1), 4, protoAppendMessage(nil, 2, settings)),
		"api as varint":         protoAppendInt32(protoAppendInt32(nil, 1, 1), 4, 1),
		"settings as varint":    protoAppendMessage(protoAppendInt32(nil, 1, 1), 4, protoAppendInt32(nil, 2, 1)),
		"precautions as varint": protoAppendMessage(protoAppendInt32(nil, 1, 1), 4, protoAppendMessage(protoAppendInt32(nil, 1, 1), 2, settings)),
		"duplicate request id":  protoAppendMessage(protoAppendInt32(protoAppendInt32(nil, 1, 1), 1, 1), 4, protoAppendMessage(nil, 2, settings)),
		"duplicate settings":    protoAppendMessage(protoAppendInt32(nil, 1, 1), 4, protoAppendMessage(protoAppendMessage(nil, 2, settings), 2, settings)),
		"other request":         protoAppendMessage(protoAppendInt32(nil, 1, 2), 4, protoAppendMessage(nil, 2, settings)),
		"no settings":           protoAppendMessage(protoAppendInt32(nil, 1, 1), 4, protoAppendMessage(nil, 1, nil)),
	} {
		if _, err := parseAPIConfig(body, 1); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// Sections the diagnostic never reads (messages 3, orders 5) and unknown
	// api fields are skipped unread.
	skipped := protoAppendMessage(protoAppendMessage(protoAppendInt32(nil, 1, 1), 3, protoAppendString(nil, 1, "private-message-sentinel")), 5, protoAppendInt32(nil, 1, 1))
	skipped = protoAppendMessage(skipped, 4, protoAppendInt32(protoAppendMessage(nil, 2, settings), 7, 1))
	if out, err := parseAPIConfig(skipped, 1); err != nil || out.settings == nil || out.precautions != nil || out.lockAndExit != nil {
		t.Fatalf("skipped sections %+v %v", out, err)
	}

	pre, err := parseAPIPrecautions(protoAppendBool(protoAppendBool(protoAppendInt32(nil, 42, 1), 10, true), 3, false))
	if err != nil || fmt.Sprint(pre.Bypassed) != "[route marketable to BBO]" {
		t.Fatalf("precautions %+v %v", pre, err)
	}
	for name, body := range map[string][]byte{
		"duplicate": protoAppendBool(protoAppendBool(nil, 1, true), 1, true),
		"value 2":   protoAppendInt32(nil, 1, 2),
		"as string": protoAppendString(nil, 1, "true"),
	} {
		if _, err := parseAPIPrecautions(body); err == nil {
			t.Errorf("precautions %s accepted", name)
		}
	}

	s, err := parseAPISettings(protoAppendString(protoAppendInt32(protoAppendInt32(protoAppendBool(nil, 24, true), 8, 4002), 18, 0), 36, "[::1]"))
	if err != nil || s.VolumeInLots == nil || !*s.VolumeInLots || s.SocketPort == nil || *s.SocketPort != 4002 || s.MasterClientID == nil || *s.MasterClientID != 0 || s.LoopbackTrusted == nil || !*s.LoopbackTrusted {
		t.Fatalf("settings %+v %v", s, err)
	}
	if s, err := parseAPISettings(protoAppendString(protoAppendInt32(nil, 99, 1), 36, "10.0.0.7;10.0.0.8")); err != nil || s.LoopbackTrusted == nil || *s.LoopbackTrusted {
		t.Fatalf("untrusted loopback %+v %v", s, err)
	}
	for name, body := range map[string][]byte{
		"port zero":           protoAppendInt32(nil, 8, 0),
		"port too large":      protoAppendInt32(nil, 8, 65536),
		"master beyond int32": protoAppendInt64(nil, 18, math.MaxInt32+1),
		"trusted as varint":   protoAppendInt32(nil, 36, 1),
		"level too long":      protoAppendString(nil, 17, strings.Repeat("x", maxDiagnosticText+1)),
		"level as varint":     protoAppendInt32(nil, 17, 1),
		"duplicate text":      protoAppendString(protoAppendString(nil, 17, "a"), 17, "b"),
		"boolean 2":           protoAppendInt32(nil, 26, 2),
		"boolean as string":   protoAppendString(nil, 26, "yes"),
	} {
		if _, err := parseAPISettings(body); err == nil {
			t.Errorf("settings %s accepted", name)
		}
	}
	for list, want := range map[string]bool{"127.0.0.1": true, "::1": true, "[::1]": true, " localhost ": true, "10.0.0.7,\t[::1]": true, "10.0.0.7; 10.0.0.8": false, "": false, "127.0.0.10": false} {
		if loopbackTrusted(list) != want {
			t.Errorf("loopbackTrusted(%q) = %t", list, !want)
		}
	}
}
