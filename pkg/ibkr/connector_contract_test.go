package ibkr

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseContractDetailsLiteVersion1(t *testing.T) {
	fields := []string{
		"10", // message id
		"1",  // reqID (version omitted because serverVersion >= size rules)
		"GLD",
		"STK",
		"",  // last trade date / contract month
		"",  // strike
		"0", // right
		"",  // empty exchange placeholder (older payloads)
		"SMART",
		"USD",
		"GLD",
		"GLD",
		"GLD",
		"51529211",
		"0.01",
		"",                           // md size multiplier (deprecated)
		"ACTIVETIM,AD,ADDONT,ADJUST", // order types (partial)
	}

	lite, ok := parseContractDetailsLite(fields, 1, 203)
	if !ok {
		t.Fatalf("expected parseContractDetailsLite to succeed")
	}
	if lite.ConID != 51529211 {
		t.Fatalf("expected conID 51529211, got %d", lite.ConID)
	}
	if lite.LocalSymbol != "GLD" {
		t.Fatalf("expected local symbol GLD, got %q", lite.LocalSymbol)
	}
}

func TestParseContractDetailsLiteDecodesTypedClassificationTail(t *testing.T) {
	fields := syntheticStockContractDetailsFields(maxClientVersion, "TYPE_CODE")
	lite, ok := parseContractDetailsLite(fields, 41, maxClientVersion)
	if !ok {
		t.Fatal("typed contract-details frame was rejected")
	}
	if lite.Industry != "INDUSTRY_CODE" || lite.Category != "CATEGORY_CODE" || lite.Subcategory != "SUBCATEGORY_CODE" {
		t.Fatalf("classification tuple = (%q,%q,%q)", lite.Industry, lite.Category, lite.Subcategory)
	}
	if lite.StockType != "TYPE_CODE" {
		t.Fatalf("stock type = %q, want typed tail value", lite.StockType)
	}
}

func TestParseContractDetailsLiteStockTypeServerBoundary(t *testing.T) {
	below := syntheticStockContractDetailsFields(minServerVerStockType-1, "")
	got, ok := parseContractDetailsLite(below, 41, minServerVerStockType-1)
	if !ok {
		t.Fatal("pre-stock-type frame was rejected")
	}
	if got.StockType != "" {
		t.Fatalf("pre-boundary stock type = %q, want empty", got.StockType)
	}

	at := syntheticStockContractDetailsFields(minServerVerStockType, "TYPE_CODE")
	got, ok = parseContractDetailsLite(at, 41, minServerVerStockType)
	if !ok || got.StockType != "TYPE_CODE" {
		t.Fatalf("boundary stock type = %q ok=%v", got.StockType, ok)
	}

	for _, serverVersion := range []int{
		minServerVerStockType,
		minServerVerFractionalSize,
		minServerVerSizeRules,
		minServerVerFundDataFields,
		minServerVerIneligibility,
		maxClientVersion,
	} {
		t.Run(strconv.Itoa(serverVersion), func(t *testing.T) {
			fields := syntheticStockContractDetailsFields(serverVersion, "TYPE_CODE")
			fields = append(fields, "") // terminal wire delimiter is accepted
			got, ok := parseContractDetailsLite(fields, 41, serverVersion)
			if !ok || got.StockType != "TYPE_CODE" {
				t.Fatalf("server %d stock type=%q ok=%v", serverVersion, got.StockType, ok)
			}
		})
	}

	for version := 1; version <= 8; version++ {
		t.Run("message_version_"+strconv.Itoa(version), func(t *testing.T) {
			fields := syntheticContractDetailsFieldsVersion(minServerVerStockType, version, "STK", "TYPE_CODE")
			got, ok := parseContractDetailsLite(fields, 41, minServerVerStockType)
			if !ok || got.StockType != "TYPE_CODE" {
				t.Fatalf("message version %d stock type=%q ok=%v", version, got.StockType, ok)
			}
		})
	}
}

func TestParseContractDetailsLiteOptionExpiryDropsSettlementTimeSuffix(t *testing.T) {
	// The gateway reports lastTradeDateOrContractMonth for options with a
	// settlement-time suffix ("20260729 15:00:00 US/Central"). The parsed
	// Expiry must be the bare date or every cached row contradicts its
	// optionContractKey and the contract-cache save is refused.
	for _, tc := range []struct {
		name, wire, want string
	}{
		{"space_time_zone", "20260729 15:00:00 US/Central", "20260729"},
		{"hyphen_time", "20260729-15:00:00", "20260729"},
		{"bare_date", "20260729", "20260729"},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fields := syntheticContractDetailsFields(maxClientVersion, "OPT", "")
			fields[4] = tc.wire // lastTradeDateOrContractMonth
			fields[6] = "555"   // strike
			fields[7] = "P"     // right
			got, ok := parseContractDetailsLite(fields, 41, maxClientVersion)
			if !ok {
				t.Fatal("option contract-details frame was rejected")
			}
			if got.Expiry != tc.want {
				t.Fatalf("expiry = %q, want %q", got.Expiry, tc.want)
			}
			if got.Strike != 555 || got.Right != "P" {
				t.Fatalf("strike/right = %v/%q, want 555/P", got.Strike, got.Right)
			}
		})
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

func TestParseContractDetailsLiteETFOnlyComesFromExactTypedStockTypeField(t *testing.T) {
	for _, secType := range []string{"STK", "FUND"} {
		t.Run(secType, func(t *testing.T) {
			fields := syntheticContractDetailsFields(maxClientVersion, secType, "STOCK_TYPE_MARKER")
			stockTypeIndex := syntheticFieldIndex(t, fields, "STOCK_TYPE_MARKER")

			for index := range fields {
				if index == stockTypeIndex {
					continue
				}
				t.Run("replace_"+strconv.Itoa(index), func(t *testing.T) {
					mutated := append([]string(nil), fields...)
					mutated[stockTypeIndex] = ""
					mutated[index] = "ETF"
					got, ok := parseContractDetailsLite(mutated, 41, maxClientVersion)
					if ok && got.StockType == "ETF" {
						t.Fatalf("untrusted field %d became StockType", index)
					}
				})
			}
		})
	}
}

func TestParseContractDetailsLiteShiftedETFNeverBecomesStockType(t *testing.T) {
	fields := syntheticStockContractDetailsFields(maxClientVersion, "ETF")
	stockTypeIndex := syntheticFieldIndex(t, fields, "ETF")

	for index := range fields {
		if index == stockTypeIndex {
			continue
		}
		t.Run("delete_"+strconv.Itoa(index), func(t *testing.T) {
			mutated := append([]string(nil), fields[:index]...)
			mutated = append(mutated, fields[index+1:]...)
			got, ok := parseContractDetailsLite(mutated, 41, maxClientVersion)
			if ok && got.StockType == "ETF" {
				t.Fatalf("missing field %d retained shifted StockType", index)
			}
		})
	}
	for index := 0; index <= len(fields); index++ {
		t.Run("insert_"+strconv.Itoa(index), func(t *testing.T) {
			mutated := make([]string, 0, len(fields)+1)
			mutated = append(mutated, fields[:index]...)
			mutated = append(mutated, "ETF")
			mutated = append(mutated, fields[index:]...)
			got, ok := parseContractDetailsLite(mutated, 41, maxClientVersion)
			if ok && got.StockType == "ETF" {
				t.Fatalf("extra field at %d retained shifted StockType", index)
			}
		})
	}
}

func TestParseContractDetailsLiteTypedFundAndIneligibilitySuffixes(t *testing.T) {
	fund := syntheticContractDetailsFields(maxClientVersion, "FUND", "FUND_TYPE_CODE")
	got, ok := parseContractDetailsLite(fund, 41, maxClientVersion)
	if !ok || got.StockType != "FUND_TYPE_CODE" {
		t.Fatalf("valid FUND frame stock type=%q ok=%v", got.StockType, ok)
	}

	stock := syntheticStockContractDetailsFields(maxClientVersion, "TYPE_CODE")
	stock[len(stock)-1] = "1" // ineligibility reason count
	stock = append(stock, "ETF", "ETF")
	got, ok = parseContractDetailsLite(stock, 41, maxClientVersion)
	if !ok || got.StockType != "TYPE_CODE" {
		t.Fatalf("typed ineligibility strings changed stock type=%q ok=%v", got.StockType, ok)
	}
}

func TestParseContractDetailsLiteFundBooleanWireVocabulary(t *testing.T) {
	base := syntheticContractDetailsFields(maxClientVersion, "FUND", "TYPE_CODE")
	firstFundField := syntheticFieldIndex(t, base, "FUND_NAME_CODE")
	firstBoolean := firstFundField + 7
	for offset := range 3 {
		for _, value := range []string{"", "0", "1"} {
			t.Run("valid_"+strconv.Itoa(offset)+"_"+value, func(t *testing.T) {
				fields := append([]string(nil), base...)
				fields[firstBoolean+offset] = value
				got, ok := parseContractDetailsLite(fields, 41, maxClientVersion)
				if !ok || got.StockType != "TYPE_CODE" {
					t.Fatalf("valid boolean %q cleared stock type=%q ok=%v", value, got.StockType, ok)
				}
			})
		}
		for _, value := range []string{"-1", "2", "true", "ETF"} {
			t.Run("invalid_"+strconv.Itoa(offset)+"_"+value, func(t *testing.T) {
				fields := append([]string(nil), base...)
				fields[firstBoolean+offset] = value
				got, ok := parseContractDetailsLite(fields, 41, maxClientVersion)
				if ok && got.StockType != "" {
					t.Fatalf("non-canonical boolean %q retained stock type %q", value, got.StockType)
				}
			})
		}
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
		"SYNTH1", secType, "", // symbol, secType, contract month
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

func syntheticFieldIndex(t *testing.T, fields []string, marker string) int {
	t.Helper()
	for index, field := range fields {
		if field == marker {
			return index
		}
	}
	t.Fatalf("marker %q missing from synthetic frame", marker)
	return -1
}

func TestNormalizeEquityRoutingUsesSmartExchange(t *testing.T) {
	contract := Contract{
		SecType:  "STK",
		Exchange: "NASDAQ",
	}

	normalizeEquityRouting(&contract, "")

	if contract.Exchange != "SMART" {
		t.Fatalf("expected exchange SMART, got %q", contract.Exchange)
	}
	if contract.PrimaryExch != "NASDAQ" {
		t.Fatalf("expected primary NASDAQ, got %q", contract.PrimaryExch)
	}
}

func TestNormalizeEquityRoutingRespectsFallback(t *testing.T) {
	contract := Contract{
		SecType:  "STK",
		Exchange: "SMART",
	}

	normalizeEquityRouting(&contract, "ARCA")

	if contract.Exchange != "SMART" {
		t.Fatalf("expected exchange SMART, got %q", contract.Exchange)
	}
	if contract.PrimaryExch != "ARCA" {
		t.Fatalf("expected primary ARCA, got %q", contract.PrimaryExch)
	}
}

func TestExactOrderContractFiltersStockListingAndPreservesRoute(t *testing.T) {
	request := Contract{Symbol: "abc", SecType: "STK", Exchange: "smart", PrimaryExch: "nasdaq", Currency: "usd"}
	resolved, err := exactOrderContract(request, []ContractDetailsLite{
		{ConID: 22, Symbol: "ABC", SecType: "STK", Exchange: "SMART", PrimaryExch: "NYSE", Currency: "USD"},
		{ConID: 11, Symbol: "ABC", SecType: "STK", Exchange: "SMART", PrimaryExch: "NASDAQ", Currency: "USD", LocalSymbol: "ABC", TradingClass: "NMS"},
	})
	if err != nil {
		t.Fatalf("exactOrderContract: %v", err)
	}
	if resolved.Contract.ConID != 11 || resolved.Contract.Exchange != "SMART" || resolved.Contract.PrimaryExch != "NASDAQ" {
		t.Fatalf("resolved contract = %+v", resolved.Contract)
	}
}

func TestExactOrderContractRequiresSuppliedStockPrimaryExchange(t *testing.T) {
	request := Contract{Symbol: "ABC", SecType: "STK", Exchange: "SMART", PrimaryExch: "NASDAQ", Currency: "USD"}
	for name, detail := range map[string]ContractDetailsLite{
		"missing":  {ConID: 11, Symbol: "ABC", SecType: "STK", Exchange: "SMART", Currency: "USD"},
		"conflict": {ConID: 11, Symbol: "ABC", SecType: "STK", Exchange: "SMART", PrimaryExch: "NYSE", Currency: "USD"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := exactOrderContract(request, []ContractDetailsLite{detail}); err == nil {
				t.Fatal("expected supplied primary exchange mismatch to fail closed")
			}
		})
	}
}

func TestExactOrderContractFiltersDirectExchange(t *testing.T) {
	request := Contract{Symbol: "ABC", SecType: "STK", Exchange: "IBIS", Currency: "EUR"}
	resolved, err := exactOrderContract(request, []ContractDetailsLite{
		{ConID: 1, Symbol: "ABC", SecType: "STK", Exchange: "SMART", PrimaryExch: "IBIS", Currency: "EUR"},
		{ConID: 2, Symbol: "ABC", SecType: "STK", Exchange: "IBIS", PrimaryExch: "IBIS", Currency: "EUR"},
	})
	if err != nil {
		t.Fatalf("exactOrderContract: %v", err)
	}
	if resolved.Contract.ConID != 2 || resolved.Contract.Exchange != "IBIS" {
		t.Fatalf("resolved contract = %+v", resolved.Contract)
	}
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

func TestExactOrderContractOptionMultiplierAndPrimaryHint(t *testing.T) {
	request := Contract{Symbol: "ABC", SecType: "OPT", Expiry: "20261218", Strike: 100, Right: "C", Multiplier: 100, Exchange: "SMART", PrimaryExch: "NASDAQ", Currency: "USD"}
	if _, err := exactOrderContract(request, []ContractDetailsLite{{
		ConID: 8, Symbol: "ABC", SecType: "OPT", Expiry: "20261218", Strike: 100, Right: "C", Multiplier: 10, Exchange: "SMART", Currency: "USD",
	}}); err == nil {
		t.Fatal("expected nonstandard multiplier mismatch to fail closed")
	}
	resolved, err := exactOrderContract(request, []ContractDetailsLite{{
		ConID: 9, Symbol: "ABC", SecType: "OPT", Expiry: "20261218", Strike: 100, Right: "C", Multiplier: 100, Exchange: "SMART", Currency: "USD",
	}})
	if err != nil {
		t.Fatalf("exactOrderContract: %v", err)
	}
	if resolved.Contract.ConID != 9 || resolved.Contract.Multiplier != 100 || resolved.Contract.PrimaryExch != "NASDAQ" {
		t.Fatalf("resolved option = %+v", resolved.Contract)
	}
}

func TestExactOrderContractUsesBrokerStockTypeForETF(t *testing.T) {
	resolved, err := exactOrderContract(Contract{Symbol: "HYG", SecType: "ETF", Exchange: "SMART", PrimaryExch: "ARCA", Currency: "USD"}, []ContractDetailsLite{{
		ConID: 43652089, Symbol: "HYG", SecType: "STK", Exchange: "SMART", PrimaryExch: "ARCA", Currency: "USD",
	}})
	if err != nil {
		t.Fatalf("exactOrderContract: %v", err)
	}
	if resolved.Contract.SecType != "STK" {
		t.Fatalf("resolved SecType = %q, want broker STK", resolved.Contract.SecType)
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

func TestResolveOrderContractForSessionBrokerizesETFAndReturnsExactIdentity(t *testing.T) {
	conn, connector, socket, _, _ := newQueuedInstructionReconnectFixture(t)
	binding, ok := connector.CaptureSession()
	if !ok {
		t.Fatal("capture session")
	}
	type outcome struct {
		resolved ResolvedOrderContract
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		resolved, err := connector.ResolveOrderContractForSession(context.Background(), binding, Contract{
			Symbol: "HYG", SecType: "ETF", Exchange: "SMART", PrimaryExch: "ARCA", Currency: "USD",
		}, time.Second)
		done <- outcome{resolved: resolved, err: err}
	}()
	reqID := waitForHandlerReqID(t, conn, msgContractData)
	requestFields := onlyOutboundFrame(t, conn, socket.Bytes())
	assertField(t, requestFields, 5, "STK", "contractDetails broker secType")

	frame := orderContractDetailsFrame(reqID, 43652089, "HYG", "STK", "SMART", "ARCA", "USD", "HYG", "HYG", 0)
	conn.dispatchHandlers(msgContractData, frame, binding.epoch)
	conn.dispatchHandlers(msgContractDataEnd, []string{strconv.Itoa(msgContractDataEnd), "1", strconv.Itoa(reqID)}, binding.epoch)
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("ResolveOrderContractForSession: %v", got.err)
		}
		if got.resolved.Contract.ConID != 43652089 || got.resolved.Contract.SecType != "STK" || got.resolved.Contract.PrimaryExch != "ARCA" {
			t.Fatalf("resolved = %+v", got.resolved)
		}
	case <-time.After(time.Second):
		t.Fatal("resolver did not complete")
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
