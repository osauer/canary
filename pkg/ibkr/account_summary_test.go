package ibkr

import (
	"bufio"
	"context"

	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type accountSummaryWriteSignal struct {
	once  sync.Once
	wrote chan struct{}
}

func (w *accountSummaryWriteSignal) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.wrote) })
	return len(p), nil
}

func TestAccountBaseCurrencyEvidenceRejectsConflictingValueSuffixes(t *testing.T) {
	currency, provenance := accountBaseCurrencyEvidence(map[string]string{
		"NetLiquidation_USD": "100000",
		"AvailableFunds_EUR": "100",
	})
	if currency != "" || provenance != AccountBaseCurrencyUnknown {
		t.Fatalf("evidence = (%q, %q), want unknown", currency, provenance)
	}
}

func TestRequestAccountSummaryUsesPinnedAccountWithinManagedList(t *testing.T) {
	for _, tc := range []struct {
		name     string
		managed  string
		pinnedID string
	}{
		{name: "single account control", managed: "DU2222222", pinnedID: "DU2222222"},
		{name: "pinned member of managed list", managed: "DU1111111,DU2222222", pinnedID: "DU2222222"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &ConnectionConfig{
				Host:     "127.0.0.1",
				Port:     7497,
				ClientID: 41,
				Account:  tc.pinnedID,
			}
			c := NewConnector(&ConnectorConfig{BaseConfig: cfg})
			conn := c.conn
			t.Cleanup(conn.rateLimiter.Stop)
			conn.status = StatusConnected
			setServerVersionReady(conn, minServerVersionRequired)
			wire := &accountSummaryWriteSignal{wrote: make(chan struct{})}
			conn.writer = bufio.NewWriter(wire)
			c.running = true
			c.ready = true
			conn.processMessage(conn.encodeMsg(msgManagedAccts, "1", tc.managed))

			type result struct {
				summary    *RawAccountSummary
				provenance AccountSummaryProvenance
				err        error
			}
			resultCh := make(chan result, 1)
			go func() {
				summary, provenance, err := c.RequestAccountSummaryWithProvenance(context.Background(), time.Second)
				resultCh <- result{summary: summary, provenance: provenance, err: err}
			}()

			select {
			case got := <-resultCh:
				t.Fatalf("summary returned before sending the pinned-account request: provenance=%q err=%v", got.provenance, got.err)
			case <-wire.wrote:
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for account-summary request")
			}

			conn.handleAccountSummary([]string{"63", "2", "1", cfg.Account, "NetLiquidation", "100000", "USD"})
			conn.processMessage(conn.encodeMsg(msgAccountSummaryEnd, "1", 1))

			select {
			case got := <-resultCh:
				if got.err != nil {
					t.Fatalf("pinned-account summary failed: %v", got.err)
				}
				if got.provenance != AccountSummaryProvenanceRequest {
					t.Fatalf("provenance=%q, want %q", got.provenance, AccountSummaryProvenanceRequest)
				}
				if got.summary == nil {
					t.Fatal("pinned-account summary is nil")
				}
				if got.summary.AccountID != cfg.Account {
					t.Fatalf("summary account=%q, want pinned account %q", got.summary.AccountID, cfg.Account)
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for pinned-account summary")
			}
		})
	}
}

func TestRequestAccountSummaryIgnoresSiblingRowsOnMultiAccountLogin(t *testing.T) {
	cfg := &ConnectionConfig{Host: "127.0.0.1", Port: 7497, ClientID: 41, Account: "DU2222222"}
	c := NewConnector(&ConnectorConfig{BaseConfig: cfg})
	conn := c.conn
	t.Cleanup(conn.rateLimiter.Stop)
	conn.status = StatusConnected
	setServerVersionReady(conn, minServerVersionRequired)
	wire := &accountSummaryWriteSignal{wrote: make(chan struct{})}
	conn.writer = bufio.NewWriter(wire)
	c.running = true
	c.ready = true
	conn.processMessage(conn.encodeMsg(msgManagedAccts, "1", "DU1111111,DU2222222"))

	type result struct {
		summary    *RawAccountSummary
		provenance AccountSummaryProvenance
		err        error
	}
	resultCh := make(chan result, 1)
	go func() {
		summary, provenance, err := c.RequestAccountSummaryWithProvenance(context.Background(), time.Second)
		resultCh <- result{summary: summary, provenance: provenance, err: err}
	}()

	select {
	case <-wire.wrote:
	case got := <-resultCh:
		t.Fatalf("summary returned before request write: %+v", got)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for account-summary request")
	}

	conn.handleAccountSummary([]string{"63", "2", "1", "DU1111111", "NetLiquidation", "7654321", "USD"})
	conn.handleAccountSummary([]string{"63", "2", "1", "DU1111111", "AccountType", "JOINT", ""})
	conn.handleAccountSummary([]string{"63", "2", "1", cfg.Account, "NetLiquidation", "100000", "USD"})
	conn.handleAccountSummary([]string{"63", "2", "1", cfg.Account, "TotalCashValue", "25000", "USD"})
	conn.handleAccountSummary([]string{"63", "2", "1", cfg.Account, "AccountType", "INDIVIDUAL", ""})
	conn.processMessage(conn.encodeMsg(msgAccountSummaryEnd, "1", 1))

	select {
	case got := <-resultCh:
		if got.err != nil {
			t.Fatalf("multi-account one-shot failed: %v", got.err)
		}
		if got.provenance != AccountSummaryProvenanceRequest {
			t.Fatalf("provenance=%q, want %q", got.provenance, AccountSummaryProvenanceRequest)
		}
		if got.summary == nil {
			t.Fatal("multi-account one-shot summary is nil")
		}
		if got.summary.AccountID != cfg.Account {
			t.Fatalf("summary account=%q, want pinned account %q", got.summary.AccountID, cfg.Account)
		}
		if got.summary.NetLiquidation == nil || *got.summary.NetLiquidation != 100000 {
			t.Fatalf("NetLiquidation=%v, want the pinned account's 100000", got.summary.NetLiquidation)
		}
		if got.summary.AccountType != "INDIVIDUAL" {
			t.Fatalf("AccountType=%q, want the pinned account's INDIVIDUAL", got.summary.AccountType)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for multi-account summary")
	}
}

func TestRequestAccountSummaryDoesNotUseSiblingCacheFallback(t *testing.T) {
	cfg := &ConnectionConfig{
		Host:     "127.0.0.1",
		Port:     7497,
		ClientID: 41,
		Account:  "DU2222222",
	}
	c := NewConnector(&ConnectorConfig{BaseConfig: cfg})
	conn := c.conn
	t.Cleanup(conn.rateLimiter.Stop)
	conn.status = StatusConnected
	setServerVersionReady(conn, minServerVersionRequired)
	wire := &accountSummaryWriteSignal{wrote: make(chan struct{})}
	conn.writer = bufio.NewWriter(wire)
	c.running = true
	c.ready = true
	conn.processMessage(conn.encodeMsg(msgManagedAccts, "1", "DU1111111,DU2222222"))

	conn.handleAccountSummary([]string{"63", "2", "99", "DU1111111", "NetLiquidation", "7654321", "USD"})

	type result struct {
		summary    *RawAccountSummary
		provenance AccountSummaryProvenance
		err        error
	}
	resultCh := make(chan result, 1)
	go func() {
		summary, provenance, err := c.RequestAccountSummaryWithProvenance(context.Background(), time.Second)
		resultCh <- result{summary: summary, provenance: provenance, err: err}
	}()

	select {
	case <-wire.wrote:
	case got := <-resultCh:
		t.Fatalf("summary returned before request write: %+v", got)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for account-summary request")
	}
	conn.processMessage(conn.encodeMsg(msgAccountSummaryEnd, "1", 1))

	select {
	case got := <-resultCh:
		if got.err != nil {
			t.Fatalf("account summary failed: %v", got.err)
		}
		if got.provenance != AccountSummaryProvenanceCachedFallback {
			t.Fatalf("provenance=%q, want %q", got.provenance, AccountSummaryProvenanceCachedFallback)
		}
		if got.summary == nil {
			t.Fatal("summary is nil")
		}
		if got.summary.NetLiquidation != nil {
			t.Fatalf("sibling NetLiquidation crossed scope: %v", *got.summary.NetLiquidation)
		}
		if got.summary.AccountID != cfg.Account {
			t.Fatalf("summary account=%q, want pinned account %q", got.summary.AccountID, cfg.Account)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for account-summary result")
	}
}

func TestParseContractDetailsLiteMalformedOrTruncatedTailCannotFabricateStockType(t *testing.T) {
	valid := syntheticStockContractDetailsFields(maxClientVersion, "TYPE_CODE")
	secIDCountIndex := 31
	stockTypeIndex := 39
	if len(valid) <= stockTypeIndex {
		t.Fatalf("synthetic fixture too short: %d", len(valid))
	}

	malformed := append([]string(nil), valid...)
	malformed[secIDCountIndex] = "NOT_A_COUNT"
	got, ok := parseContractDetailsLite(malformed, 41, maxClientVersion)
	if !ok {
		t.Fatal("core contract identity should survive malformed optional tail")
	}
	if got.StockType != "" {
		t.Fatalf("malformed tail fabricated stock type %q", got.StockType)
	}
	if got.Industry != "" || got.Category != "" || got.Subcategory != "" {
		t.Fatal("malformed frame retained non-authoritative classifications")
	}
	shifted := append([]string(nil), valid...)
	shifted[secIDCountIndex] = "2"
	got, ok = parseContractDetailsLite(shifted, 41, maxClientVersion)
	if !ok || got.StockType != "" {
		t.Fatalf("misaligned sec-id tail stock type=%q ok=%v, want empty", got.StockType, ok)
	}

	truncated := append([]string(nil), valid[:stockTypeIndex]...)
	got, ok = parseContractDetailsLite(truncated, 41, maxClientVersion)
	if !ok {
		t.Fatal("core contract identity should survive truncated optional tail")
	}
	if got.StockType != "" {
		t.Fatalf("truncated tail fabricated stock type %q", got.StockType)
	}
}

func syntheticStockContractDetailsFields(serverVersion int, stockType string) []string {
	return syntheticContractDetailsFields(serverVersion, "STK", stockType)
}

func syntheticContractDetailsFields(serverVersion int, secType, stockType string) []string {
	return syntheticContractDetailsFieldsVersion(serverVersion, 8, secType, stockType)
}

func syntheticContractDetailsFieldsVersion(serverVersion, version int, secType, stockType string) []string {
	fields := []string{strconv.Itoa(msgContractData)}
	if serverVersion < minServerVerSizeRules {
		fields = append(fields, strconv.Itoa(version))
	}
	if version >= 3 {
		fields = append(fields, "41")
	}
	fields = append(fields,
		"SYNTH1", secType, "",
	)
	if serverVersion >= minServerVerLastTradeDate {
		fields = append(fields, "")
	}
	fields = append(fields,
		"", "", "SMART", "USD", "SYNTH1", "MARKET_CODE", "CLASS_CODE", "424242", "0.01",
	)
	if serverVersion >= minServerVerMdSizeMultiplier && serverVersion < minServerVerSizeRules {
		fields = append(fields, "")
	}
	fields = append(fields,
		"1", "ORDER_CODE", "EXCHANGE_CODE",
	)
	if version >= 2 {
		fields = append(fields, "1")
	}
	if version >= 4 {
		fields = append(fields, "0")
	}
	if version >= 5 {
		fields = append(fields, "SYNTHETIC NAME", "PRIMARY_CODE")
	}
	if version >= 6 {
		fields = append(fields, "", "INDUSTRY_CODE", "CATEGORY_CODE", "SUBCATEGORY_CODE", "UTC", "", "")
	}
	if version >= 8 {
		fields = append(fields, "", "0.5")
	}
	if version >= 7 {
		fields = append(fields, "1", "ID_TAG", "ID_VALUE")
	}
	if serverVersion >= minServerVerAggGroup {
		fields = append(fields, "0")
	}
	if serverVersion >= minServerVerUnderlyingInfo {
		fields = append(fields, "", "")
	}
	if serverVersion >= minServerVerMarketRules {
		fields = append(fields, "")
	}
	if serverVersion >= minServerVerRealExpiration {
		fields = append(fields, "")
	}
	if serverVersion >= minServerVerStockType {
		fields = append(fields, stockType)
	}
	if serverVersion >= minServerVerFractionalSize && serverVersion < minServerVerSizeRules {
		fields = append(fields, "0")
	}
	if serverVersion >= minServerVerSizeRules {
		fields = append(fields, "0", "0", "0")
	}
	if serverVersion >= minServerVerFundDataFields && secType == "FUND" {
		fields = append(fields,
			"FUND_NAME_CODE", "FUND_FAMILY_CODE", "FUND_TYPE_CODE", "0", "0", "0", "0",
			"0", "0", "0",
			"0", "0", "0", "STATE_CODE", "TERRITORY_CODE", "POLICY_CODE", "ASSET_CODE",
		)
	}
	if serverVersion >= minServerVerIneligibility {
		fields = append(fields, "0")
	}
	return fields
}

func TestExactOrderContractRequiresSuppliedQualifiers(t *testing.T) {
	base := Contract{Symbol: "ABC", SecType: "STK", Exchange: "SMART", Currency: "USD", LocalSymbol: "ABC.N", TradingClass: "NMS"}
	for name, detail := range map[string]ContractDetailsLite{
		"local missing":  {ConID: 1, Symbol: "ABC", SecType: "STK", Exchange: "SMART", Currency: "USD", TradingClass: "NMS"},
		"local conflict": {ConID: 1, Symbol: "ABC", SecType: "STK", Exchange: "SMART", Currency: "USD", LocalSymbol: "OTHER", TradingClass: "NMS"},
		"class missing":  {ConID: 1, Symbol: "ABC", SecType: "STK", Exchange: "SMART", Currency: "USD", LocalSymbol: "ABC.N"},
		"class conflict": {ConID: 1, Symbol: "ABC", SecType: "STK", Exchange: "SMART", Currency: "USD", LocalSymbol: "ABC.N", TradingClass: "OTHER"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := exactOrderContract(base, []ContractDetailsLite{detail}); err == nil {
				t.Fatal("expected supplied qualifier mismatch to fail closed")
			}
		})
	}
}

func TestExactOrderContractRejectsAmbiguityAndNonPositiveIdentity(t *testing.T) {
	request := Contract{Symbol: "ABC", SecType: "STK", Exchange: "SMART", Currency: "USD"}
	if _, err := exactOrderContract(request, []ContractDetailsLite{
		{ConID: 1, Symbol: "ABC", SecType: "STK", Exchange: "SMART", PrimaryExch: "NASDAQ", Currency: "USD"},
		{ConID: 2, Symbol: "ABC", SecType: "STK", Exchange: "SMART", PrimaryExch: "NYSE", Currency: "USD"},
	}); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguity error = %v", err)
	}
	if _, err := exactOrderContract(request, []ContractDetailsLite{{Symbol: "ABC", SecType: "STK", Exchange: "SMART", Currency: "USD"}}); err == nil {
		t.Fatal("expected non-positive identity to fail closed")
	}
}

func TestResolveOrderContractForSessionRejectsRetiredCallbacksAndDoesNotCompleteSuccessor(t *testing.T) {
	conn, connector, _, _, _ := newQueuedInstructionReconnectFixture(t)
	bindingA, ok := connector.CaptureSession()
	if !ok {
		t.Fatal("capture session A")
	}
	type outcome struct {
		resolved ResolvedOrderContract
		err      error
	}
	doneA := make(chan outcome, 1)
	go func() {
		resolved, err := connector.ResolveOrderContractForSession(context.Background(), bindingA, Contract{Symbol: "ABC", SecType: "STK", Exchange: "SMART", Currency: "USD"}, time.Second)
		doneA <- outcome{resolved: resolved, err: err}
	}()
	reqA := waitForHandlerReqID(t, conn, msgContractData)
	conn.resetOrderIDReadiness()
	epochB := conn.BrokerSessionEpoch()
	conn.observeNextValidOrderIDAtEpoch(500, epochB)
	bindingB, ok := connector.CaptureSession()
	if !ok {
		t.Fatal("capture session B")
	}
	doneB := make(chan outcome, 1)
	go func() {
		resolved, err := connector.ResolveOrderContractForSession(context.Background(), bindingB, Contract{Symbol: "ABC", SecType: "STK", Exchange: "SMART", Currency: "USD"}, time.Second)
		doneB <- outcome{resolved: resolved, err: err}
	}()
	reqB := waitForHandlerReqIDAfter(t, conn, msgContractData, reqA)

	staleFrame := orderContractDetailsFrame(reqA, 1, "ABC", "STK", "SMART", "NASDAQ", "USD", "ABC", "NMS", 0)
	conn.dispatchHandlers(msgContractData, staleFrame, bindingA.epoch)
	conn.dispatchHandlers(msgContractDataEnd, []string{strconv.Itoa(msgContractDataEnd), "1", strconv.Itoa(reqA)}, bindingA.epoch)
	select {
	case got := <-doneB:
		t.Fatalf("stale A callback completed B: %+v", got)
	case <-time.After(25 * time.Millisecond):
	}

	currentFrame := orderContractDetailsFrame(reqB, 2, "ABC", "STK", "SMART", "NASDAQ", "USD", "ABC", "NMS", 0)
	conn.dispatchHandlers(msgContractData, currentFrame, bindingB.epoch)
	conn.dispatchHandlers(msgContractDataEnd, []string{strconv.Itoa(msgContractDataEnd), "1", strconv.Itoa(reqB)}, bindingB.epoch)
	select {
	case got := <-doneB:
		if got.err != nil || got.resolved.Contract.ConID != 2 {
			t.Fatalf("B result = %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("B resolver did not complete")
	}
	select {
	case got := <-doneA:
		if got.err == nil {
			t.Fatalf("retired A resolver succeeded: %+v", got.resolved)
		}
	case <-time.After(time.Second):
		t.Fatal("retired A resolver did not fail")
	}
}

func orderContractDetailsFrame(reqID, conID int, symbol, secType, exchange, primary, currency, localSymbol, tradingClass string, multiplier int) []string {
	frame := make([]string, 29)
	frame[0] = strconv.Itoa(msgContractData)
	frame[1] = strconv.Itoa(reqID)
	frame[2] = symbol
	frame[3] = secType
	frame[8] = exchange
	frame[9] = currency
	frame[10] = localSymbol
	frame[12] = tradingClass
	frame[13] = strconv.Itoa(conID)
	frame[14] = "0.01"
	if multiplier > 0 {
		frame[15] = strconv.Itoa(multiplier)
	}
	frame[21] = primary
	return frame
}

func exactQuoteTestContract(conID int) Contract {
	return Contract{
		ConID: conID, Symbol: "SPY", SecType: "OPT", Expiry: "20261218", Strike: 500,
		Right: "C", Multiplier: 100, Exchange: "SMART", PrimaryExch: "ARCA", Currency: "USD",
		LocalSymbol: "SPY   261218C00500000", TradingClass: "SPY",
	}
}

func exactQuoteSubscriptionReqID(t *testing.T, connector *Connector, key string) int {
	t.Helper()
	connector.subMu.RLock()
	defer connector.subMu.RUnlock()
	sub := connector.subscriptions[key]
	if sub == nil || sub.ReqID <= 0 {
		t.Fatalf("exact subscription %q missing request ID: %+v", key, sub)
	}
	return sub.ReqID
}

func marketDataSlotCount(conn *Connection) int {
	conn.marketDataSlotsMu.Lock()
	defer conn.marketDataSlotsMu.Unlock()
	return len(conn.marketDataSlots)
}

func TestExactSessionOptionQuoteCarriesCanonicalIdentityAndClearsUnderlyingPrimary(t *testing.T) {
	conn, connector, oldSocket, _, _ := newQueuedInstructionReconnectFixture(t)
	binding, ok := connector.CaptureSession()
	if !ok {
		t.Fatal("capture exact quote session")
	}
	contract := exactQuoteTestContract(900001)
	key, err := connector.SubscribeMarketDataWithContractForSession(context.Background(), binding, contract, []string{"BID", "ASK"})
	if err != nil {
		t.Fatalf("subscribe exact option: %v", err)
	}
	if key == "" {
		t.Fatal("exact option subscription returned empty key")
	}
	frames := decodeOutboundFrames(t, conn, oldSocket.Bytes())
	marketData := findOutboundFrame(t, frames, reqMktData)
	assertField(t, marketData, 3, "900001", "marketData conID")
	assertField(t, marketData, 4, "SPY", "marketData symbol")
	assertField(t, marketData, 5, "OPT", "marketData secType")
	assertField(t, marketData, 9, "100", "marketData multiplier")
	assertField(t, marketData, 10, "SMART", "marketData exchange")
	assertField(t, marketData, 11, "", "marketData primaryExchange")
	assertField(t, marketData, 13, contract.LocalSymbol, "marketData localSymbol")
	assertField(t, marketData, 14, contract.TradingClass, "marketData tradingClass")
}

func TestConcurrentExactSessionQuotesDoNotShareOrCrossCancel(t *testing.T) {
	conn, connector, oldSocket, _, _ := newQueuedInstructionReconnectFixture(t)
	binding, ok := connector.CaptureSession()
	if !ok {
		t.Fatal("capture exact quote session")
	}
	contract := Contract{ConID: 700001, Symbol: "SAME", SecType: "STK", Exchange: "SMART", Currency: "USD"}
	keyA, err := connector.SubscribeMarketDataWithContractForSession(context.Background(), binding, contract, nil)
	if err != nil {
		t.Fatalf("subscribe exact A: %v", err)
	}
	keyB, err := connector.SubscribeMarketDataWithContractForSession(context.Background(), binding, contract, nil)
	if err != nil {
		t.Fatalf("subscribe exact B: %v", err)
	}
	if keyA == keyB {
		t.Fatalf("concurrent exact subscriptions shared key %q", keyA)
	}
	reqB := exactQuoteSubscriptionReqID(t, connector, keyB)
	if err := connector.UnsubscribeMarketDataForSession(context.Background(), binding, keyA); err != nil {
		t.Fatalf("unsubscribe exact A: %v", err)
	}
	connector.subMu.RLock()
	remaining := connector.subscriptions[keyB]
	mapped := connector.reqIDMap[reqB]
	connector.subMu.RUnlock()
	if remaining == nil || mapped != keyB {
		t.Fatalf("A cleanup crossed into B: remaining=%+v mapped=%q", remaining, mapped)
	}
	frames := decodeOutboundFrames(t, conn, oldSocket.Bytes())
	marketRequests := 0
	cancels := 0
	for _, frame := range frames {
		if len(frame) == 0 {
			continue
		}
		switch frame[0] {
		case "1":
			marketRequests++
		case "2":
			cancels++
		}
	}
	if marketRequests != 2 || cancels != 1 {
		t.Fatalf("exact frames requests=%d cancels=%d, want 2/1: %#v", marketRequests, cancels, frames)
	}
}

func TestStaleExactQuoteTickCannotPopulateSuccessorSubscription(t *testing.T) {
	conn, connector, _, newSocket, _ := newQueuedInstructionReconnectFixture(t)
	bindingA, ok := connector.CaptureSession()
	if !ok {
		t.Fatal("capture session A")
	}
	contractA := Contract{ConID: 700001, Symbol: "SAME", SecType: "STK", Exchange: "SMART", Currency: "USD"}
	keyA, err := connector.SubscribeMarketDataWithContractForSession(context.Background(), bindingA, contractA, nil)
	if err != nil {
		t.Fatalf("subscribe A: %v", err)
	}
	reqA := exactQuoteSubscriptionReqID(t, connector, keyA)

	conn.resetOrderIDReadiness()
	conn.writer = bufio.NewWriter(newSocket)
	conn.observeNextValidOrderIDAtEpoch(500, conn.BrokerSessionEpoch())
	bindingB, ok := connector.CaptureSession()
	if !ok {
		t.Fatal("capture session B")
	}
	contractB := Contract{ConID: 700002, Symbol: "SAME", SecType: "STK", Exchange: "SMART", Currency: "USD"}
	keyB, err := connector.SubscribeMarketDataWithContractForSession(context.Background(), bindingB, contractB, nil)
	if err != nil {
		t.Fatalf("subscribe B: %v", err)
	}
	conn.processMessageAtEpoch(conn.encodeMsg(msgTickPrice, "3", reqA, 1, "9999", "0"), bindingA.epoch)
	time.Sleep(time.Millisecond)
	if got := connector.MarketDataSnapshot()[keyB]; got != nil && (got.Bid != 0 || got.Ask != 0 || got.Last != 0 || got.MarkPrice != 0) {
		t.Fatalf("stale A tick populated B exact quote: %+v", got)
	}
	if keyA == keyB || !containsConID(keyA, contractA.ConID) || !containsConID(keyB, contractB.ConID) {
		t.Fatalf("exact keys do not preserve ConID identity: A=%q B=%q", keyA, keyB)
	}
}

func containsConID(key string, conID int) bool {
	return strings.Contains(key, "CONID:"+strconv.Itoa(conID))
}

func TestReusedConnectionInvalidatesUnstampedObservationAuthority(t *testing.T) {
	connector := readyBrokerEvidenceTestConnector(t)
	conn := connector.conn
	epochA := conn.BrokerSessionEpoch()
	bindingA, ok := connector.CaptureSession()
	if !ok {
		t.Fatal("capture complete session A")
	}

	conn.portfolioProjectionMu.Lock()
	conn.positionsMu.Lock()
	conn.positions["A"] = &RawPosition{Contract: Contract{ConID: 1, Symbol: "A", SecType: "STK"}, Position: 10, Account: "DU-A"}
	conn.positionsMu.Unlock()
	conn.portfolioHealthMu.Lock()
	conn.portfolioHealth = PortfolioStreamHealth{
		Account: "DU-A", RequestedAt: time.Now().Add(-time.Minute), InitialCompletedAt: time.Now(),
		LastUpdateAt: time.Now(), ProjectionGeneration: 7,
	}
	conn.portfolioHealthMu.Unlock()
	conn.portfolioProjectionMu.Unlock()
	conn.accountMu.Lock()
	conn.account = "DU-A"
	conn.accountSummary["NetLiquidation"] = "100000"
	conn.accountMu.Unlock()
	conn.competingMu.Lock()
	conn.competingLiveSession = true
	conn.competingMu.Unlock()
	if err := conn.acquireMarketDataSlot(context.Background(), 91); err != nil {
		t.Fatalf("seed market-data slot: %v", err)
	}

	connector.subMu.Lock()
	connector.subscriptions["A"] = &Subscription{Symbol: "A", ReqID: 91, Bid: 99, Ask: 101, Observed: true}
	connector.reqIDMap[91] = "A"
	connector.subMu.Unlock()
	connector.contractMu.Lock()
	connector.contractCache["A"] = ContractDetailsLite{ConID: 1, Symbol: "A", SecType: "STK"}
	connector.contractMu.Unlock()
	connector.dataFarmMu.Lock()
	connector.dataFarms = make(map[string]DataFarmStatus)
	connector.dataFarms["market\x00a"] = DataFarmStatus{Name: "a", Type: "market", Status: "ok"}
	connector.dataFarmMu.Unlock()
	connector.absenceMu.Lock()
	connector.mktDataAbsent = map[string]marketDataAbsence{"A": {code: 354, at: time.Now()}}
	connector.absenceMu.Unlock()
	connector.inactiveMu.Lock()
	connector.inactiveSymbols = map[string]inactiveSymbolState{"A": {reason: "test", markedAt: time.Now()}}
	connector.inactiveMu.Unlock()

	conn.resetOrderIDReadiness()
	if conn.BrokerSessionEpoch() == epochA {
		t.Fatal("reconnect did not advance socket epoch")
	}
	connector.onConnectionEstablished(conn)
	conn.observeNextValidOrderIDAtEpoch(500, conn.BrokerSessionEpoch())
	bindingB, ok := connector.CaptureSession()
	if !ok {
		t.Fatal("capture successor session B")
	}
	if connector.SessionCurrent(bindingA) || bindingB.epoch == bindingA.epoch {
		t.Fatalf("session A remained current after rollover: A=%d B=%d", bindingA.epoch, bindingB.epoch)
	}

	projection, ok := connector.CapturePortfolioProjectionForSession(bindingB)
	if !ok {
		t.Fatal("capture successor portfolio projection")
	}
	if len(projection.Positions) != 0 || !projection.Health.InitialCompletedAt.IsZero() || projection.Health.Account != "" {
		t.Fatalf("successor inherited completed A portfolio: %+v", projection)
	}
	if projection.Generation <= 7 {
		t.Fatalf("successor projection generation=%d, want invalidation after 7", projection.Generation)
	}
	if conn.GetAccountCode() != "" || len(conn.GetAccountSummary()) != 0 || conn.HasCompetingLiveSession() {
		t.Fatalf("successor inherited account/session observations: account=%q summary=%+v competing=%t",
			conn.GetAccountCode(), conn.GetAccountSummary(), conn.HasCompetingLiveSession())
	}
	if got := connector.MarketDataSnapshot(); len(got) != 0 {
		t.Fatalf("successor inherited quote subscriptions: %+v", got)
	}
	if connector.cachedContractDetail("A") != nil || len(connector.DataFarmStatuses()) != 0 || connector.marketDataAbsenceFor("A") != nil || connector.IsSymbolInactive("A") {
		t.Fatal("successor inherited unstamped contract/farm/absence/inactive cache")
	}
	if got := marketDataSlotCount(conn); got != 0 {
		t.Fatalf("successor inherited market-data slot count=%d", got)
	}
}
