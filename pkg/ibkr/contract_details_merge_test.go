package ibkr

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// mdTFullDetail is a ContractDetailsLite with every merge-relevant field set
// to a recognisable value derived from tag.
func mdTFullDetail(tag string, conID int, minTick float64) ContractDetailsLite {
	return ContractDetailsLite{
		Symbol: tag, ConID: conID, Exchange: tag + "-EXCH", PrimaryExch: tag + "-PRIM", Currency: tag + "-CCY",
		LocalSymbol: tag + "-LOCAL", TradingClass: tag + "-CLASS", Industry: tag + "-IND", Category: tag + "-CAT",
		Subcategory: tag + "-SUB", StockType: tag + "-TYPE", TimeZoneID: tag + "-TZ", TradingHours: tag + "-TH",
		LiquidHours: tag + "-LH", MinTick: minTick,
	}
}

// A merge never overwrites what the base already knows; it only fills gaps
// from the incoming detail, and a non-positive incoming MinTick is no fill.
func TestMergeContractDetailsLiteKeepsBaseAndFillsGaps(t *testing.T) {
	base := mdTFullDetail("BASE", 4242, 0.01)
	incoming := mdTFullDetail("NEW", 9999, 0.05)
	if got := mergeContractDetailsLite(base, incoming); got != base {
		t.Fatalf("populated base changed:\n got %+v\nwant %+v", got, base)
	}
	if got := mergeContractDetailsLite(ContractDetailsLite{}, incoming); got != incoming {
		t.Fatalf("empty base did not take the incoming detail:\n got %+v\nwant %+v", got, incoming)
	}
	partial := ContractDetailsLite{Symbol: "SYNX", ConID: 4242, Exchange: "SMART"}
	got := mergeContractDetailsLite(partial, incoming)
	if got.Symbol != "SYNX" || got.ConID != 4242 || got.Exchange != "SMART" {
		t.Fatalf("known fields overwritten: %+v", got)
	}
	if got.PrimaryExch != "NEW-PRIM" || got.Currency != "NEW-CCY" || got.LocalSymbol != "NEW-LOCAL" || got.TradingClass != "NEW-CLASS" ||
		got.Industry != "NEW-IND" || got.Category != "NEW-CAT" || got.Subcategory != "NEW-SUB" || got.StockType != "NEW-TYPE" ||
		got.TimeZoneID != "NEW-TZ" || got.TradingHours != "NEW-TH" || got.LiquidHours != "NEW-LH" || got.MinTick != 0.05 {
		t.Fatalf("gaps not filled: %+v", got)
	}
	if got := mergeContractDetailsLite(ContractDetailsLite{}, ContractDetailsLite{MinTick: -1}); got.MinTick != 0 {
		t.Fatalf("non-positive MinTick filled: %v", got.MinTick)
	}
	// Fields the merge does not own (security type, expiry, strike, right,
	// multiplier, under ConID) stay as the base had them, even when empty.
	if got := mergeContractDetailsLite(ContractDetailsLite{}, ContractDetailsLite{SecType: "OPT", Expiry: "20991231", Strike: 100, Right: "C", Multiplier: 100, UnderConID: 1}); got.SecType != "" || got.Expiry != "" || got.Strike != 0 || got.Right != "" || got.Multiplier != 0 || got.UnderConID != 0 {
		t.Fatalf("merge invented derivative identity: %+v", got)
	}
}

// mdTClassificationFrame is a current-protocol contractData frame with every
// classification field set, as syntheticContractDetailsFieldsVersion builds it.
func mdTClassificationFrame(serverVersion, version int, secType string) []string {
	return syntheticContractDetailsFieldsVersion(serverVersion, version, secType, "COMMON")
}

func mdTIndexOf(t *testing.T, fields []string, sentinel string) int {
	t.Helper()
	idx := slices.Index(fields, sentinel)
	if idx < 0 {
		t.Fatalf("sentinel %q missing from frame %v", sentinel, fields)
	}
	return idx
}

// The classification tuple is read only from a frame whose every typed field
// parses and whose length matches the negotiated layout; a shifted or
// truncated frame yields nothing rather than a misaligned industry.
func TestParseContractDetailsClassificationAcceptsOnlyCompleteAlignedFrames(t *testing.T) {
	const current = maxClientVersion
	frame := mdTClassificationFrame(current, 8, "STK")
	got, ok := parseContractDetailsClassification(frame, current)
	if !ok || got.industry != "INDUSTRY_CODE" || got.category != "CATEGORY_CODE" || got.subcategory != "SUBCATEGORY_CODE" || got.stockType != "COMMON" || got.validExchanges != "EXCHANGE_CODE" || got.marketRuleIDs != "" {
		t.Fatalf("current frame -> %+v ok=%t", got, ok)
	}
	// One trailing empty token (the delimiter after the last field) is fine;
	// any further field means the frame is longer than the layout.
	if _, ok := parseContractDetailsClassification(append(slices.Clone(frame), ""), current); !ok {
		t.Fatal("trailing delimiter token rejected")
	}
	if _, ok := parseContractDetailsClassification(append(slices.Clone(frame), "extra"), current); ok {
		t.Fatal("over-long frame accepted")
	}
	if _, ok := parseContractDetailsClassification(frame[:len(frame)-1], current); ok {
		t.Fatal("truncated frame accepted")
	}
	wrongID := slices.Clone(frame)
	wrongID[0] = strconv.Itoa(msgContractDataEnd)
	if _, ok := parseContractDetailsClassification(wrongID, current); ok {
		t.Fatal("foreign message id accepted")
	}

	// Each typed field refuses a value of the wrong wire type.
	conID := mdTIndexOf(t, frame, "424242")
	validExchanges := mdTIndexOf(t, frame, "EXCHANGE_CODE")
	evMultiplier := mdTIndexOf(t, frame, "0.5")
	aggGroup := mdTIndexOf(t, frame, "ID_VALUE") + 1
	typed := map[string]struct {
		idx   int
		value string
	}{
		"strike":          {6, "abc"},
		"conID":           {conID, "4242x"},
		"minTick":         {conID + 1, "NaN"},
		"priceMagnifier":  {validExchanges + 1, "1.5"},
		"underConID":      {validExchanges + 2, "x"},
		"evMultiplier":    {evMultiplier, "+Inf"},
		"secIdList count": {evMultiplier + 1, "99"},
		"aggGroup":        {aggGroup, "x"},
		"minSize":         {len(frame) - 4, "x"},
		"ineligibility":   {len(frame) - 1, "-1"},
	}
	for name, tc := range typed {
		mutated := slices.Clone(frame)
		mutated[tc.idx] = tc.value
		if _, ok := parseContractDetailsClassification(mutated, current); ok {
			t.Fatalf("%s=%q accepted", name, tc.value)
		}
	}
	// Empty numeric fields are the protocol's "unset" and stay acceptable.
	empties := slices.Clone(frame)
	empties[6], empties[conID+1], empties[validExchanges+1] = "", "", ""
	if _, ok := parseContractDetailsClassification(empties, current); !ok {
		t.Fatal("empty numeric fields rejected")
	}

	// A fund frame consumes the fund tail, whose closed flags are booleans.
	fund := mdTClassificationFrame(current, 8, "FUND")
	if got, ok := parseContractDetailsClassification(fund, current); !ok || got.industry != "INDUSTRY_CODE" {
		t.Fatalf("fund frame -> %+v ok=%t", got, ok)
	}
	badFlag := slices.Clone(fund)
	badFlag[mdTIndexOf(t, fund, "FUND_TYPE_CODE")+5] = "2"
	if _, ok := parseContractDetailsClassification(badFlag, current); ok {
		t.Fatal("non-boolean fund flag accepted")
	}
	// The fund tail is read only for FUND: a stock frame carrying it is over-long.
	stockWithFundTail := slices.Clone(fund)
	stockWithFundTail[3] = "STK"
	if _, ok := parseContractDetailsClassification(stockWithFundTail, current); ok {
		t.Fatal("stock frame with a fund tail accepted")
	}

	// Legacy servers carry an explicit version; the layout follows it.
	for _, tc := range []struct {
		serverVersion, version int
		wantIndustry           string
	}{
		{150, 8, "INDUSTRY_CODE"},
		{163, 8, "INDUSTRY_CODE"},
		{125, 6, "INDUSTRY_CODE"},
		{150, 5, ""},
		{150, 2, ""},
		{150, 1, ""},
	} {
		legacy := mdTClassificationFrame(tc.serverVersion, tc.version, "STK")
		got, ok := parseContractDetailsClassification(legacy, tc.serverVersion)
		if !ok || got.industry != tc.wantIndustry {
			t.Fatalf("server %d version %d -> %+v ok=%t", tc.serverVersion, tc.version, got, ok)
		}
		if tc.version >= 7 {
			if _, ok := parseContractDetailsClassification(legacy[:len(legacy)-1], tc.serverVersion); ok {
				t.Fatalf("server %d version %d accepted a truncated frame", tc.serverVersion, tc.version)
			}
		}
	}
	for _, version := range []string{"0", "9", "x"} {
		legacy := mdTClassificationFrame(150, 8, "STK")
		legacy[1] = version
		if _, ok := parseContractDetailsClassification(legacy, 150); ok {
			t.Fatalf("legacy version %q accepted", version)
		}
	}
	legacyMultiplier := mdTClassificationFrame(150, 8, "STK")
	legacyMultiplier[mdTIndexOf(t, legacyMultiplier, "424242")+2] = "x" // deprecated mdSizeMultiplier is an integer
	if _, ok := parseContractDetailsClassification(legacyMultiplier, 150); ok {
		t.Fatal("non-integer mdSizeMultiplier accepted")
	}
}

// mdTContractDataFrame is a current-protocol contractData answer for reqID
// naming symbol with conID, in the layout the gateway sends at server 203.
func mdTContractDataFrame(reqID int, symbol string, conID int) []string {
	fields := syntheticContractDetailsFieldsVersion(maxClientVersion, 8, "STK", "COMMON")
	fields[1] = strconv.Itoa(reqID)
	fields[2] = symbol
	fields[10] = symbol
	fields[13] = strconv.Itoa(conID)
	return fields
}

func mdTHandlerCount(conn *Connection, msgID int) int {
	conn.handlersMu.RLock()
	defer conn.handlersMu.RUnlock()
	return len(conn.msgHandlers[msgID])
}

func mdTWaitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A contract-details answer that lands after the caller gave up is still
// cached for the next caller, the request's handlers are released once the
// end marker arrives, and a cached identity is never replaced by the late one.
func TestFetchContractDetailsLateAnswerIsCachedAfterTimeout(t *testing.T) {
	conn, c, out, _, _ := newQueuedInstructionReconnectFixture(t)
	conn.brokerSessionEpoch.Store(1)
	type result struct {
		details []ContractDetailsLite
		err     error
	}
	// The wire fetch is driven directly: FetchContractDetails shares one
	// flight per symbol and lets the wire wait outlive a short caller
	// budget, so only the wire fetch itself sees its own timeout.
	fetch := func(symbol string) (int, result) {
		done := make(chan result, 1)
		go func() {
			d, err := c.fetchContractDetailsSymbolWire(strings.ToUpper(symbol), 50*time.Millisecond, nil)
			done <- result{d, err}
		}()
		reqID := waitForHandlerReqID(t, conn, msgContractData)
		select {
		case r := <-done:
			return reqID, r
		case <-time.After(2 * time.Second):
			t.Fatalf("fetch of %s did not time out", symbol)
			return 0, result{}
		}
	}

	reqID, r := fetch("zeta")
	if !errors.Is(r.err, ErrContractDetailsTimeout) || len(r.details) != 0 {
		t.Fatalf("fetch = %+v %v, want timeout", r.details, r.err)
	}
	request := outboundFramesByID(t, conn, out, reqContractData)
	if len(request) != 1 || !slices.Contains(request[0], "ZETA") {
		t.Fatalf("contract-details request frames = %v", request)
	}
	if mdTHandlerCount(conn, msgContractData) != 1 || mdTHandlerCount(conn, msgContractDataEnd) != 1 {
		t.Fatal("timed-out fetch did not keep draining its handlers")
	}
	conn.processMessage(twsGatewayFrame(msgContractData, mdTContractDataFrame(reqID, "ZETA", 4242)[1:]...))
	mdTWaitFor(t, "late detail in cache", func() bool { return c.hasActiveContract("ZETA") })
	if detail := c.cachedContractDetail("ZETA"); detail.ConID != 4242 || detail.Symbol != "ZETA" || detail.Industry != "INDUSTRY_CODE" {
		t.Fatalf("late detail cached as %+v", detail)
	}
	conn.processMessage(twsGatewayFrame(msgContractDataEnd, "1", strconv.Itoa(reqID)))
	mdTWaitFor(t, "handler release", func() bool {
		return mdTHandlerCount(conn, msgContractData) == 0 && mdTHandlerCount(conn, msgContractDataEnd) == 0
	})
	// The next caller is served from the cache without a wire request.
	details, err := c.FetchContractDetails("ZETA", time.Second)
	if err != nil || len(details) != 1 || details[0].ConID != 4242 {
		t.Fatalf("cached fetch = %+v %v", details, err)
	}
	if n := len(outboundFramesByID(t, conn, out, reqContractData)); n != 1 {
		t.Fatalf("cached fetch sent %d wire requests", n-1)
	}

	// An identity resolved meanwhile wins over the late answer.
	reqID, r = fetch("synx")
	if !errors.Is(r.err, ErrContractDetailsTimeout) {
		t.Fatalf("second fetch = %v", r.err)
	}
	c.contractMu.Lock()
	c.contractCache["SYNX"] = ContractDetailsLite{Symbol: "SYNX", ConID: 1}
	c.contractMu.Unlock()
	conn.processMessage(twsGatewayFrame(msgContractData, mdTContractDataFrame(reqID, "SYNX", 4343)[1:]...))
	conn.processMessage(twsGatewayFrame(msgContractDataEnd, "1", strconv.Itoa(reqID)))
	mdTWaitFor(t, "second handler release", func() bool { return mdTHandlerCount(conn, msgContractData) == 0 })
	if detail := c.cachedContractDetail("SYNX"); detail.ConID != 1 {
		t.Fatalf("late answer replaced a resolved identity: %+v", detail)
	}
}

// Position snapshots seed the bare-symbol contract cache only from stock
// positions that carry a ConID and a valuation; entries are keyed by the
// upper-cased symbol, several positions of one symbol merge, and a cached
// identity is kept while its gaps are filled.
func TestSeedContractCacheFromPositionsKeysBySymbolAndKeepsCachedIdentity(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	t.Cleanup(c.conn.rateLimiter.Stop)
	c.contractCache["ZETA"] = ContractDetailsLite{Symbol: "ZETA", ConID: 1, Exchange: "SMART"}

	c.seedContractCacheFromPositions(nil)
	c.seedContractCacheFromPositions(map[string]*RawPosition{
		"nil":        nil,
		"no conid":   {Account: "DU1234567", Position: 10, MarketPrice: 5, MarketValue: 50, Contract: Contract{Symbol: "NOID", SecType: "STK"}},
		"option":     {Account: "DU1234567", Position: 1, MarketPrice: 2, MarketValue: 200, Contract: Contract{ConID: 7001, Symbol: "OPTX", SecType: "OPT"}},
		"zero value": {Account: "DU1234567", Position: 10, Contract: Contract{ConID: 7002, Symbol: "DEAD", SecType: "STK"}},
		"blank":      {Account: "DU1234567", Position: 10, MarketPrice: 5, MarketValue: 50, Contract: Contract{ConID: 7003, Symbol: "  ", SecType: "STK"}},
		"synx a":     {Account: "DU1234567", Position: 10, MarketPrice: 5, MarketValue: 50, Contract: Contract{ConID: 4242, Symbol: "synx", SecType: "stk", Exchange: " SMART "}},
		"synx b":     {Account: "DU1234567", Position: 5, MarketPrice: 5, MarketValue: 25, Contract: Contract{ConID: 4242, Symbol: "SYNX", SecType: "STK", PrimaryExch: "ARCA", LocalSymbol: "SYNX", TradingClass: "NMS"}},
		"flat":       {Account: "DU1234567", Position: 0, Contract: Contract{ConID: 4343, Symbol: "FLAT", SecType: "STK"}},
		"zeta":       {Account: "DU1234567", Position: 3, MarketPrice: 1, MarketValue: 3, Contract: Contract{ConID: 4444, Symbol: "ZETA", SecType: "STK", PrimaryExch: "NASDAQ"}},
	})

	for _, absent := range []string{"NOID", "OPTX", "DEAD", "", "  ", "synx"} {
		if _, ok := c.contractCache[absent]; ok {
			t.Fatalf("%q seeded", absent)
		}
	}
	// Two rows of one symbol arrive in map order; each fills the gaps the
	// other left, so the result does not depend on which came first.
	synx := c.contractCache["SYNX"]
	if synx.ConID != 4242 || synx.Exchange != "SMART" || synx.PrimaryExch != "ARCA" || synx.LocalSymbol != "SYNX" || synx.TradingClass != "NMS" || synx.Symbol != "SYNX" {
		t.Fatalf("SYNX seeded as %+v", synx)
	}
	flat := c.contractCache["FLAT"]
	if flat.ConID != 4343 {
		t.Fatalf("a flat position with a ConID still names the contract: %+v", flat)
	}
	zeta := c.contractCache["ZETA"]
	if zeta.ConID != 1 || zeta.Exchange != "SMART" || zeta.PrimaryExch != "NASDAQ" {
		t.Fatalf("cached identity not kept while gaps filled: %+v", zeta)
	}
	if len(c.contractCache) != 3 {
		t.Fatalf("cache = %+v", c.contractCache)
	}
}

// Identical contract warnings inside the window are counted, not repeated;
// the next emission after the window names how many were held back, and
// each key keeps its own window.
func TestCoalesceContractWarningSuppressesRepeatsWithinWindow(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	t.Cleanup(c.conn.rateLimiter.Stop)
	now := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	c.contractWarningNow = func() time.Time { return now }

	msg, emit := c.coalesceContractWarning("k1", time.Minute, "unresolved SYNX")
	if !emit || msg != "unresolved SYNX" {
		t.Fatalf("first warning = %q %t", msg, emit)
	}
	for range 2 {
		now = now.Add(20 * time.Second)
		if msg, emit := c.coalesceContractWarning("k1", time.Minute, "unresolved SYNX"); emit || msg != "" {
			t.Fatalf("repeat inside the window emitted %q", msg)
		}
	}
	if msg, emit := c.coalesceContractWarning("k2", time.Minute, "unresolved ZETA"); !emit || msg != "unresolved ZETA" {
		t.Fatalf("other key shared the window: %q %t", msg, emit)
	}
	now = now.Add(21 * time.Second)
	msg, emit = c.coalesceContractWarning("k1", time.Minute, "unresolved SYNX")
	if !emit || !strings.HasSuffix(msg, "(2 identical warnings suppressed)") || !strings.HasPrefix(msg, "unresolved SYNX") {
		t.Fatalf("post-window warning = %q %t", msg, emit)
	}
	now = now.Add(2 * time.Minute)
	if msg, emit = c.coalesceContractWarning("k1", time.Minute, "unresolved SYNX"); !emit || msg != "unresolved SYNX" {
		t.Fatalf("suppressed count not reset: %q %t", msg, emit)
	}
	// A non-positive window means the default five minutes.
	if _, emit := c.coalesceContractWarning("k3", 0, "x"); !emit {
		t.Fatal("first warning on default window held back")
	}
	now = now.Add(4 * time.Minute)
	if _, emit := c.coalesceContractWarning("k3", 0, "x"); emit {
		t.Fatal("default window shorter than five minutes")
	}
	now = now.Add(time.Minute + time.Second)
	if msg, emit := c.coalesceContractWarning("k3", -1, "x"); !emit || msg != "x (1 identical warnings suppressed)" {
		t.Fatalf("default window warning = %q %t", msg, emit)
	}
}
