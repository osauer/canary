package edge

import (
	"math"
	"math/rand"
	"slices"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/flexstmt"
)

func TestAnalyzeAccountAndDecisionPriceImpact(t *testing.T) {
	t.Parallel()
	st := edgeStatement()
	result, err := Analyze(Input{AsOf: day("2026-02-10"), WindowDays: 90, BaseCurrency: "EUR", Statements: []flexstmt.Statement{st}, Bars: barsMap(123, "2026-01-06", 20, 105)})
	if err != nil {
		t.Fatal(err)
	}
	if result.Account == nil {
		t.Fatal("account result is absent")
	}
	if got, want := result.Account.ProfitLossBase, 500.0; got != want {
		t.Fatalf("account P/L = %.2f, want %.2f", got, want)
	}
	if len(result.Changes) != 1 || result.Changes[0].Action != ActionOpen || result.Changes[0].Direction != DirectionLong {
		t.Fatalf("changes = %+v", result.Changes)
	}
	change := result.Changes[0]
	for _, want := range []struct {
		sessions int
		impact   float64
	}{{1, 43.2}, {5, 79.2}, {20, 214.2}} {
		score := scoreFor(change, want.sessions)
		if score == nil || score.DecisionImpactBase == nil || math.Abs(*score.DecisionImpactBase-want.impact) > 1e-9 {
			t.Fatalf("%d-session score = %+v, want %.2f", want.sessions, score, want.impact)
		}
	}
	if len(result.Rollups) != 4 || result.Rollups[0].Action != ActionOpen {
		t.Fatalf("rollups = %+v", result.Rollups)
	}
	if result.Fingerprint == "" || !result.NotExecution || !result.Method.NoCausalClaim {
		t.Fatalf("result contract = %+v", result)
	}
}

func TestEndToEndFlexXMLToHandCalculatedHorizons(t *testing.T) {
	t.Parallel()
	retainedXML := []byte(`<FlexQueryResponse><FlexStatements><FlexStatement accountId="U" fromDate="20251101" toDate="20260201" whenGenerated="20260202;010000">
<Trades><Trade accountId="U" assetCategory="STK" currency="USD" fxRateToBase="0.9" symbol="ACME" conid="123" multiplier="1" tradeID="t1" ibOrderID="o1" ibExecID="e1" tradeDate="20260105" tradeTime="120000" buySell="BUY" quantity="10" tradePrice="100" IBCommission="-2" IBCommissionCurrency="USD" taxes="0" levelOfDetail="EXECUTION" /></Trades>
<FinancialInstrumentInformation><FinancialInstrument assetCategory="STK" currency="USD" symbol="ACME" conid="123" multiplier="1" reportDate="20260201" /></FinancialInstrumentInformation>
<OpenPositions><OpenPosition accountId="U" assetCategory="STK" currency="USD" fxRateToBase="0.9" symbol="ACME" conid="123" reportDate="20260201" position="10" multiplier="1" levelOfDetail="SUMMARY" /></OpenPositions>
<OptionEAE></OptionEAE><CorporateActions></CorporateActions><Transfers></Transfers><CashTransactions></CashTransactions>
<EquitySummaryInBase><EquitySummaryByReportDateInBase reportDate="20251112" total="10000"/><EquitySummaryByReportDateInBase reportDate="20260201" total="10500"/></EquitySummaryInBase>
<ConversionRates><ConversionRate dateTime="20260106;000000" fromCurrency="USD" toCurrency="EUR" rate="0.9"/><ConversionRate dateTime="20260110;000000" fromCurrency="USD" toCurrency="EUR" rate="0.9"/><ConversionRate dateTime="20260125;000000" fromCurrency="USD" toCurrency="EUR" rate="0.9"/></ConversionRates>
</FlexStatement></FlexStatements></FlexQueryResponse>`)
	statements, err := flexstmt.Parse(retainedXML)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Analyze(Input{
		AsOf:         day("2026-02-10"),
		WindowDays:   90,
		BaseCurrency: "EUR",
		Statements:   statements,
		Bars:         barsMap(123, "2026-01-06", 20, 105),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Changes) != 1 {
		t.Fatalf("changes=%+v", result.Changes)
	}
	for _, want := range []struct {
		sessions int
		impact   float64
	}{{1, 43.2}, {5, 79.2}, {20, 214.2}} {
		score := scoreFor(result.Changes[0], want.sessions)
		if score == nil || score.DecisionImpactBase == nil || math.Abs(*score.DecisionImpactBase-want.impact) > 1e-9 {
			t.Fatalf("%d-session score=%+v want %.2f", want.sessions, score, want.impact)
		}
	}
}

func TestAnalyzeInterveningChangeAndFlip(t *testing.T) {
	t.Parallel()
	st := edgeStatement()
	st.Trades = append(st.Trades, flexstmt.Trade{
		RecordID: "trade-two", AccountID: "U", ConID: 123, Symbol: "ACME", AssetClass: "STK", Currency: "USD",
		FXRateToBase: new(.9), OrderID: "o2", ExecutionID: "e2", ExecutedAt: dayTime("2026-01-08", 12), Side: "SELL",
		Quantity: new(float64(15)), Price: new(float64(108)), Multiplier: new(float64(1)), Commission: new(float64(-2)), Taxes: new(float64(0)), LevelOfDetail: "EXECUTION",
	})
	st.Positions[0].Quantity = new(float64(-5))
	result, err := Analyze(Input{AsOf: day("2026-02-10"), WindowDays: 90, BaseCurrency: "EUR", Statements: []flexstmt.Statement{st}, Bars: barsMap(123, "2026-01-06", 25, 105)})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Changes) != 3 {
		t.Fatalf("changes = %d, want open + flip exit + flip open", len(result.Changes))
	}
	first := result.Changes[0]
	if scoreFor(first, 1).DecisionImpactBase == nil || scoreFor(first, 5).Reason != ReasonInterveningChange || scoreFor(first, 20).Reason != ReasonInterveningChange {
		t.Fatalf("first decision overlap = %+v", first.Scores)
	}
	if result.Changes[1].Action != ActionExit || result.Changes[1].DeltaQuantity != -10 || result.Changes[2].Action != ActionOpen || result.Changes[2].Direction != DirectionShort || result.Changes[2].DeltaQuantity != -5 {
		t.Fatalf("flip split = %+v / %+v", result.Changes[1], result.Changes[2])
	}
	allocatedCosts := *result.Changes[1].DirectCostsBase + *result.Changes[2].DirectCostsBase
	if math.Abs(allocatedCosts-1.8) > 1e-9 {
		t.Fatalf("flip costs were not allocated exactly once: %.4f", allocatedCosts)
	}
}

func TestAnalyzeNeverUsesBarsAfterSnapshotAsOf(t *testing.T) {
	t.Parallel()
	st := edgeStatement()
	result, err := Analyze(Input{AsOf: day("2026-01-10"), WindowDays: 90, BaseCurrency: "EUR", Statements: []flexstmt.Statement{st}, Bars: barsMap(123, "2026-01-06", 20, 105)})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Changes) != 1 {
		t.Fatalf("changes=%+v", result.Changes)
	}
	if scoreFor(result.Changes[0], 5).DecisionImpactBase == nil {
		t.Fatalf("available fifth session was suppressed: %+v", result.Changes[0].Scores)
	}
	if score := scoreFor(result.Changes[0], 20); score.Reason != ReasonMissingHorizon || score.HorizonDay != nil {
		t.Fatalf("post-as-of bars entered the result: %+v", score)
	}
}

func TestAnalyzePositionAnchorMismatchSuppressesContract(t *testing.T) {
	t.Parallel()
	st := edgeStatement()
	st.Positions = append(st.Positions, flexstmt.OpenPosition{RecordID: "p-early", AccountID: "U", ConID: 123, Symbol: "ACME", AssetClass: "STK", Currency: "USD", FXRateToBase: new(.9), ReportDate: day("2026-01-10"), Quantity: new(float64(10)), Multiplier: new(float64(1))})
	st.Positions[0].ReportDate = day("2026-02-01")
	st.Positions[0].Quantity = new(float64(11))
	result, err := Analyze(Input{AsOf: day("2026-02-10"), WindowDays: 90, BaseCurrency: "EUR", Statements: []flexstmt.Statement{st}, Bars: barsMap(123, "2026-01-06", 20, 105)})
	if err != nil {
		t.Fatal(err)
	}
	for _, score := range result.Changes[0].Scores {
		if score.Reason != ReasonPositionPathUnbalanced || score.DecisionImpactBase != nil {
			t.Fatalf("unbalanced score = %+v", score)
		}
	}
	if result.Coverage.ReasonCounts[ReasonPositionPathUnbalanced] != 1 {
		t.Fatalf("coverage = %+v", result.Coverage)
	}
}

func TestAnalyzeExactOptionGroupingAndNoCounterfactual(t *testing.T) {
	t.Parallel()
	st := edgeStatement()
	st.Instruments = append(st.Instruments,
		flexstmt.Instrument{RecordID: "call-instrument", ConID: 456, UnderlyingConID: 123, Symbol: "ACME CALL", UnderlyingSymbol: "ACME", AssetClass: "OPT", Currency: "USD", Multiplier: new(float64(100)), Strike: new(float64(100)), Expiry: "20260320", PutCall: "C"},
		flexstmt.Instrument{RecordID: "put-instrument", ConID: 789, UnderlyingConID: 123, Symbol: "ACME PUT", UnderlyingSymbol: "ACME", AssetClass: "OPT", Currency: "USD", Multiplier: new(float64(100)), Strike: new(float64(90)), Expiry: "20260320", PutCall: "P"},
	)
	st.Trades = append(st.Trades,
		flexstmt.Trade{RecordID: "call", AccountID: "U", ConID: 456, UnderlyingConID: 123, Symbol: "ACME CALL", UnderlyingSymbol: "ACME", AssetClass: "OPT", Currency: "USD", FXRateToBase: new(.9), OrderID: "combo", ExecutionID: "c", ExecutedAt: dayTime("2026-01-12", 10), Side: "SELL", OpenClose: "C", Quantity: new(float64(1)), Price: new(float64(3)), Multiplier: new(float64(100)), Commission: new(float64(-1)), Taxes: new(float64(0)), RealizedPNL: new(float64(100)), LevelOfDetail: "EXECUTION"},
		flexstmt.Trade{RecordID: "put", AccountID: "U", ConID: 789, UnderlyingConID: 123, Symbol: "ACME PUT", UnderlyingSymbol: "ACME", AssetClass: "OPT", Currency: "USD", FXRateToBase: new(.9), OrderID: "combo", ExecutionID: "p", ExecutedAt: dayTime("2026-01-12", 10), Side: "SELL", OpenClose: "C", Quantity: new(float64(1)), Price: new(float64(2)), Multiplier: new(float64(100)), Commission: new(float64(-1)), Taxes: new(float64(0)), RealizedPNL: new(float64(50)), LevelOfDetail: "EXECUTION"},
	)
	result, err := Analyze(Input{AsOf: day("2026-02-10"), WindowDays: 90, BaseCurrency: "EUR", Statements: []flexstmt.Statement{st}, Bars: barsMap(123, "2026-01-06", 20, 105)})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Options.Realized.Episodes) != 1 {
		t.Fatalf("option result = %+v", result.Options)
	}
	option := result.Options.Realized.Episodes[0]
	if option.Grouping != OptionGroupingExactOrder || option.Lifecycle != OptionLifecycleClosing || len(option.Legs) != 2 || option.RealizedPNLBase == nil || *option.RealizedPNLBase != 135 || option.PNLStatus != OptionPNLComplete {
		t.Fatalf("option episode = %+v", option)
	}
	if option.Legs[0].Expiry != "2026-03-20" || option.Legs[0].PutCall == "" || option.Legs[1].PutCall == "" {
		t.Fatalf("option leg identity = %+v", option.Legs)
	}
	for _, change := range result.Changes {
		if change.AssetClass == "OPT" {
			for _, score := range change.Scores {
				if score.Reason != ReasonUnsupportedAsset || score.DecisionImpactBase != nil {
					t.Fatalf("option counterfactual was scored: %+v", change)
				}
			}
		}
	}
}

func TestAnalyzeIsStableAcrossShuffleDuplicateAndRestatement(t *testing.T) {
	t.Parallel()
	older := edgeStatement()
	newer := edgeStatement()
	older.WhenGenerated = dayTime("2026-02-01", 1)
	newer.WhenGenerated = dayTime("2026-02-02", 1)
	newer.Trades[0].Price = new(float64(101))
	older.FXRates[5].Rate = new(.8)
	newer.FXRates[5].Rate = new(.9)
	input := Input{AsOf: day("2026-02-10"), WindowDays: 90, BaseCurrency: "EUR", Statements: []flexstmt.Statement{older, newer, newer}, Bars: barsMap(123, "2026-01-06", 20, 105)}
	want, err := Analyze(input)
	if err != nil {
		t.Fatal(err)
	}
	for seed := range int64(20) {
		shuffled := append([]flexstmt.Statement(nil), input.Statements...)
		rand.New(rand.NewSource(seed)).Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		got, err := Analyze(Input{AsOf: input.AsOf, WindowDays: input.WindowDays, BaseCurrency: input.BaseCurrency, Statements: shuffled, Bars: input.Bars})
		if err != nil {
			t.Fatal(err)
		}
		if got.Fingerprint != want.Fingerprint {
			t.Fatalf("seed %d fingerprint %s, want %s", seed, got.Fingerprint, want.Fingerprint)
		}
	}
}

func TestClassifyChangeCoversLongShortTrimExitAndFlip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		before, delta float64
		want          []changePart
	}{
		{"open long", 0, 5, []changePart{{ActionOpen, DirectionLong, 5}}},
		{"open short", 0, -5, []changePart{{ActionOpen, DirectionShort, -5}}},
		{"add long", 5, 2, []changePart{{ActionAdd, DirectionLong, 2}}},
		{"add short", -5, -2, []changePart{{ActionAdd, DirectionShort, -2}}},
		{"trim long", 5, -2, []changePart{{ActionTrim, DirectionLong, -2}}},
		{"trim short", -5, 2, []changePart{{ActionTrim, DirectionShort, 2}}},
		{"exit long", 5, -5, []changePart{{ActionExit, DirectionLong, -5}}},
		{"exit short", -5, 5, []changePart{{ActionExit, DirectionShort, 5}}},
		{"flip short", 5, -8, []changePart{{ActionExit, DirectionLong, -5}, {ActionOpen, DirectionShort, -3}}},
		{"flip long", -5, 8, []changePart{{ActionExit, DirectionShort, 5}, {ActionOpen, DirectionLong, 3}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := classifyChange(test.before, test.delta)
			if len(got) != len(test.want) {
				t.Fatalf("parts = %+v, want %+v", got, test.want)
			}
			for i := range got {
				if got[i] != test.want[i] {
					t.Fatalf("part %d = %+v, want %+v", i, got[i], test.want[i])
				}
			}
		})
	}
}

func TestAnalyzeUsesCommissionCurrencyAndBaseCurrencyIdentity(t *testing.T) {
	t.Parallel()
	st := edgeStatement()
	st.Trades[0].CommissionCurrency = "GBP"
	st.Trades[0].Commission = new(float64(-2))
	st.Trades[0].Taxes = new(float64(1))
	st.FXRates = append(st.FXRates, flexstmt.FXRate{RecordID: "gbp", Date: day("2026-01-05"), FromCurrency: "GBP", ToCurrency: "EUR", Rate: new(.8)})
	result, err := Analyze(Input{AsOf: day("2026-02-10"), WindowDays: 90, BaseCurrency: "EUR", Statements: []flexstmt.Statement{st}, Bars: barsMap(123, "2026-01-06", 20, 101)})
	if err != nil {
		t.Fatal(err)
	}
	if score := scoreFor(result.Changes[0], 1); score.DecisionImpactBase == nil || math.Abs(*score.DecisionImpactBase-6.5) > 1e-9 {
		t.Fatalf("commission-currency score = %+v, want 6.5", score)
	}

	baseTrade := edgeStatement()
	baseTrade.Trades[0].Currency = "EUR"
	baseTrade.Trades[0].CommissionCurrency = "EUR"
	baseTrade.Trades[0].FXRateToBase = nil
	baseTrade.Trades[0].Taxes = new(float64(0))
	baseTrade.Trades[0].Commission = new(float64(-2))
	baseTrade.Positions[0].Currency = "EUR"
	baseTrade.Positions[0].FXRateToBase = nil
	baseResult, err := Analyze(Input{AsOf: day("2026-02-10"), WindowDays: 90, BaseCurrency: "EUR", Statements: []flexstmt.Statement{baseTrade}, Bars: barsMap(123, "2026-01-06", 20, 101)})
	if err != nil {
		t.Fatal(err)
	}
	if score := scoreFor(baseResult.Changes[0], 1); score.DecisionImpactBase == nil || *score.DecisionImpactBase != 8 {
		t.Fatalf("base-currency score = %+v, want 8", score)
	}
}

func TestAnalyzeCorporateActionSuppressesOnlyCrossingHorizons(t *testing.T) {
	t.Parallel()
	st := edgeStatement()
	st.CorporateActions = []flexstmt.CorporateAction{{RecordID: "split", AccountID: "U", ConID: 123, Date: day("2026-01-07"), Quantity: new(float64(10)), Type: "FS"}}
	st.Positions[0].Quantity = new(float64(20))
	result, err := Analyze(Input{AsOf: day("2026-02-10"), WindowDays: 90, BaseCurrency: "EUR", Statements: []flexstmt.Statement{st}, Bars: barsMap(123, "2026-01-06", 20, 105)})
	if err != nil {
		t.Fatal(err)
	}
	if scoreFor(result.Changes[0], 1).DecisionImpactBase == nil {
		t.Fatalf("pre-action horizon was suppressed: %+v", result.Changes[0].Scores)
	}
	for _, sessions := range []int{5, 20} {
		if score := scoreFor(result.Changes[0], sessions); score.Reason != ReasonCorporateAction || score.DecisionImpactBase != nil {
			t.Fatalf("%d-session corporate-action score = %+v", sessions, score)
		}
	}
}

func TestAnalyzeAccountFlowsStartAfterActualOpeningBoundary(t *testing.T) {
	t.Parallel()
	st := edgeStatement()
	st.Cash = []flexstmt.CashLine{
		{ID: "opening-day", Category: flexstmt.CategoryFlow, AmountBase: new(float64(200)), ValueDate: dayTime("2025-11-12", 12)},
		{ID: "later", Category: flexstmt.CategoryFlow, AmountBase: new(float64(300)), ValueDate: day("2025-11-13")},
	}
	result, err := Analyze(Input{AsOf: day("2026-02-10"), WindowDays: 90, BaseCurrency: "EUR", Statements: []flexstmt.Statement{st}, Bars: barsMap(123, "2026-01-06", 20, 105)})
	if err != nil {
		t.Fatal(err)
	}
	if result.Account == nil || result.Account.ExternalFlowsBase != 300 || result.Account.ProfitLossBase != 1200 {
		t.Fatalf("account boundary result = %+v", result.Account)
	}
}

func TestAnalyzeDoesNotDoubleCountTransferMirroredInCashTransactions(t *testing.T) {
	t.Parallel()
	st := edgeStatement()
	st.Cash = []flexstmt.CashLine{{
		ID: "cash-flow", TransactionID: "txn-1", Category: flexstmt.CategoryFlow,
		AmountBase: new(float64(1000)), ValueDate: day("2026-01-15"),
	}}
	st.Transfers = []flexstmt.Transfer{{
		ID: "transfer-flow", TransactionID: "txn-1", AccountID: "U", Direction: "IN",
		Date: day("2026-01-15"), AmountBase: new(float64(1000)),
	}}
	result, err := Analyze(Input{AsOf: day("2026-02-10"), WindowDays: 90, BaseCurrency: "EUR", Statements: []flexstmt.Statement{st}, Bars: barsMap(123, "2026-01-06", 20, 105)})
	if err != nil {
		t.Fatal(err)
	}
	if result.Account == nil || result.Account.ExternalFlowsBase != 1000 || result.Account.ProfitLossBase != 500 {
		t.Fatalf("mirrored flow result = %+v", result.Account)
	}
}

func TestSimultaneousIndependentExecutionsAreNotLaterChanges(t *testing.T) {
	t.Parallel()
	at := dayTime("2026-01-05", 12)
	group := groupedTrade{key: "execution:a", conid: 123, executedAt: at}
	mutations := []mutation{
		{conid: 123, at: at, kind: "trade", key: group.key},
		{conid: 123, at: at, kind: "trade", key: "execution:b"},
	}
	if reason := interveningReason(group, day("2026-01-06"), mutations); reason != "" {
		t.Fatalf("simultaneous execution was classified as later: %s", reason)
	}
}

func TestScoringIndexPartitionsMutationsAndNormalizesBarsOnce(t *testing.T) {
	t.Parallel()
	at := dayTime("2026-01-05", 12)
	mutations := []mutation{
		{conid: 123, at: at, kind: "trade", key: "execution:a"},
		{conid: 456, at: at.Add(time.Hour), kind: "trade", key: "execution:other"},
		{conid: 123, at: at.Add(24 * time.Hour), kind: "corporate_action", key: "split"},
	}
	input := Input{AsOf: day("2026-01-10"), Bars: map[int64][]DailyBar{
		123: {
			{ConID: 123, Day: day("2026-01-06"), Close: 101},
			{ConID: 123, Day: day("2026-01-06"), Close: 102},
			{ConID: 123, Day: day("2026-01-07"), Close: 103},
		},
	}}
	index := buildScoringIndex(input, mutations)
	if len(index.mutationsByConID[123]) != 2 || len(index.mutationsByConID[456]) != 1 {
		t.Fatalf("mutation index=%+v", index.mutationsByConID)
	}
	if len(index.barsByConID[123]) != 2 {
		t.Fatalf("normalized bars=%+v", index.barsByConID[123])
	}
	group := groupedTrade{key: "execution:a", conid: 123, executedAt: at}
	if got := interveningReason(group, day("2026-01-07"), index.mutationsByConID[123]); got != ReasonCorporateAction {
		t.Fatalf("indexed intervening reason=%q want %q", got, ReasonCorporateAction)
	}
}

func TestAnalyzeOptionReviewSeparatesOpenSnapshotAndUnmatchedEvents(t *testing.T) {
	t.Parallel()
	st := edgeStatement()
	st.Instruments = append(st.Instruments,
		flexstmt.Instrument{RecordID: "call-instrument", ConID: 456, Symbol: "ACME CALL", UnderlyingSymbol: "ACME", AssetClass: "OPT", Currency: "USD", Multiplier: new(float64(100)), Strike: new(float64(100)), Expiry: "20260320", PutCall: "C"},
		flexstmt.Instrument{RecordID: "put-instrument", ConID: 789, Symbol: "ACME PUT", UnderlyingSymbol: "ACME", AssetClass: "OPT", Currency: "USD", Multiplier: new(float64(100)), Strike: new(float64(90)), Expiry: "20260116", PutCall: "P"},
	)
	st.Positions = append(st.Positions, flexstmt.OpenPosition{RecordID: "open-opt", AccountID: "U", ConID: 456, Symbol: "ACME CALL", UnderlyingSymbol: "ACME", AssetClass: "OPT", Currency: "USD", FXRateToBase: new(.9), ReportDate: day("2026-02-01"), Quantity: new(float64(1)), UnrealizedPNL: new(float64(20))})
	st.OptionEvents = append(st.OptionEvents, flexstmt.OptionEvent{RecordID: "expired-opt", AccountID: "U", ConID: 789, Symbol: "ACME PUT", UnderlyingSymbol: "ACME", AssetClass: "OPT", Currency: "USD", FXRateToBase: new(.9), Date: day("2026-01-16"), TransactionType: "EXPIRATION", Quantity: new(float64(-1)), RealizedPNL: new(float64(-10))})
	result, err := Analyze(Input{AsOf: day("2026-02-10"), WindowDays: 90, BaseCurrency: "EUR", Statements: []flexstmt.Statement{st}, Bars: barsMap(123, "2026-01-06", 20, 105)})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Options.Open.Positions) != 1 || result.Options.Open.Positions[0].OpenPNLBase == nil || *result.Options.Open.Positions[0].OpenPNLBase != 18 {
		t.Fatalf("open option snapshot = %+v", result.Options.Open)
	}
	if len(result.Options.Realized.Episodes) != 1 || result.Options.Realized.Episodes[0].RealizedPNLBase == nil || *result.Options.Realized.Episodes[0].RealizedPNLBase != -9 || result.Options.Realized.Episodes[0].EventType != "expiration" {
		t.Fatalf("realized option event = %+v", result.Options.Realized)
	}
	if result.Options.Open.KnownPNLBase == nil || *result.Options.Open.KnownPNLBase != 18 || result.Options.Realized.KnownPNLBase == nil || *result.Options.Realized.KnownPNLBase != -9 {
		t.Fatalf("option summaries = %+v", result.Options)
	}
}

func TestAnalyzeOptionOpenPNLUsesOnlyLatestPositionSnapshot(t *testing.T) {
	t.Parallel()
	older := edgeStatement()
	older.ToDate = day("2026-01-31")
	older.WhenGenerated = dayTime("2026-02-01", 1)
	for i := range older.Positions {
		older.Positions[i].ReportDate = older.ToDate
		older.Positions[i].RecordID = "stock-old"
	}
	older.Positions = append(older.Positions, flexstmt.OpenPosition{
		RecordID: "option-old", AccountID: "U", ConID: 456, Symbol: "ACME CALL", UnderlyingSymbol: "ACME",
		AssetClass: "OPT", Currency: "USD", FXRateToBase: new(.9), ReportDate: older.ToDate,
		Quantity: new(float64(1)), UnrealizedPNL: new(float64(10)),
	})

	newer := edgeStatement()
	newer.WhenGenerated = dayTime("2026-02-02", 1)
	newer.Positions = append(newer.Positions, flexstmt.OpenPosition{
		RecordID: "option-new", AccountID: "U", ConID: 456, Symbol: "ACME CALL", UnderlyingSymbol: "ACME",
		AssetClass: "OPT", Currency: "USD", FXRateToBase: new(.9), ReportDate: newer.ToDate,
		Quantity: new(float64(1)), UnrealizedPNL: new(float64(20)),
	})

	result, err := Analyze(Input{AsOf: day("2026-02-10"), WindowDays: 90, BaseCurrency: "EUR", Statements: []flexstmt.Statement{newer, older}, Bars: barsMap(123, "2026-01-06", 20, 105)})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Options.Open.Positions) != 1 || result.Options.Open.Positions[0].OpenPNLBase == nil || *result.Options.Open.Positions[0].OpenPNLBase != 18 {
		t.Fatalf("latest option open P/L = %+v, want EUR 18 once", result.Options)
	}
	if !result.Options.Open.SnapshotDate.Equal(day("2026-02-01")) || len(result.Options.Realized.Episodes) != 0 {
		t.Fatalf("open and realized scopes were blended: %+v", result.Options)
	}
}

func TestAnalyzeOptionOpenSnapshotDoesNotDependOnRealizedEvidence(t *testing.T) {
	t.Parallel()
	st := edgeStatement()
	st.Trades = append(st.Trades, flexstmt.Trade{
		RecordID: "option-trade", AccountID: "U", ConID: 456, Symbol: "ACME CALL", UnderlyingSymbol: "ACME",
		AssetClass: "OPT", Currency: "USD", FXRateToBase: new(.9), ExecutionID: "option-exec",
		ExecutedAt: dayTime("2026-01-12", 10), Side: "BUY", OpenClose: "O", Quantity: new(float64(1)), Price: new(float64(2)),
		Multiplier: new(float64(100)), Commission: new(float64(-1)), Taxes: new(float64(0)), LevelOfDetail: "EXECUTION",
	})
	st.Positions = append(st.Positions, flexstmt.OpenPosition{
		RecordID: "option-open", AccountID: "U", ConID: 456, Symbol: "ACME CALL", UnderlyingSymbol: "ACME",
		AssetClass: "OPT", Currency: "USD", FXRateToBase: new(.9), ReportDate: day("2026-02-01"),
		Quantity: new(float64(1)), UnrealizedPNL: new(float64(20)),
	})
	result, err := Analyze(Input{AsOf: day("2026-02-10"), WindowDays: 90, BaseCurrency: "EUR", Statements: []flexstmt.Statement{st}, Bars: barsMap(123, "2026-01-06", 20, 105)})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Options.Realized.Episodes) != 0 || len(result.Options.Open.Positions) != 1 || result.Options.Open.Positions[0].OpenPNLBase == nil || *result.Options.Open.Positions[0].OpenPNLBase != 18 {
		t.Fatalf("open P/L was blended with missing realized evidence: %+v", result.Options)
	}
}

func TestAnalyzeDistinguishesConfirmedEmptyFromMissingOptionOpenSnapshot(t *testing.T) {
	t.Parallel()
	confirmed := edgeStatement()
	confirmed.Trades = nil
	confirmed.Positions = nil

	result, err := Analyze(Input{AsOf: day("2026-02-10"), WindowDays: 90, BaseCurrency: "EUR", Statements: []flexstmt.Statement{confirmed}})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Options.Open.SnapshotDate.Equal(confirmed.ToDate) || len(result.Options.Open.Positions) != 0 || result.Options.Open.KnownPNLBase != nil {
		t.Fatalf("confirmed-empty option snapshot = %+v", result.Options.Open)
	}

	missing := confirmed
	missing.Coverage = slices.Clone(confirmed.Coverage)
	for i := range missing.Coverage {
		if missing.Coverage[i].Key == "open_positions" {
			missing.Coverage[i].Present = false
		}
	}
	result, err = Analyze(Input{AsOf: day("2026-02-10"), WindowDays: 90, BaseCurrency: "EUR", Statements: []flexstmt.Statement{missing}})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Options.Open.SnapshotDate.IsZero() || len(result.Options.Open.Positions) != 0 {
		t.Fatalf("missing option snapshot acquired false date: %+v", result.Options.Open)
	}
}

func TestAnalyzeOpeningOnlyZeroEpisodeIsCoverageNotRealizedResult(t *testing.T) {
	t.Parallel()
	st := edgeStatement()
	st.Trades = append(st.Trades, flexstmt.Trade{
		RecordID: "option-order-open", AccountID: "U", ConID: 456, Symbol: "ACME CALL", UnderlyingSymbol: "ACME",
		AssetClass: "OPT", Currency: "USD", FXRateToBase: new(.9), OrderID: "option-order", ExecutionID: "option-exec",
		ExecutedAt: dayTime("2026-01-12", 10), Side: "BUY", OpenClose: "O", Quantity: new(float64(1)), Price: new(float64(2)),
		Multiplier: new(float64(100)), Commission: new(float64(-1)), Taxes: new(float64(0)), RealizedPNL: new(float64(0)), LevelOfDetail: "EXECUTION",
	})
	st.Positions = append(st.Positions, flexstmt.OpenPosition{
		RecordID: "option-order-open-position", AccountID: "U", ConID: 456, Symbol: "ACME CALL", UnderlyingSymbol: "ACME",
		AssetClass: "OPT", Currency: "USD", FXRateToBase: new(.9), ReportDate: day("2026-02-01"),
		Quantity: new(float64(1)), UnrealizedPNL: new(float64(20)),
	})
	result, err := Analyze(Input{AsOf: day("2026-02-10"), WindowDays: 90, BaseCurrency: "EUR", Statements: []flexstmt.Statement{st}, Bars: barsMap(123, "2026-01-06", 20, 105)})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Options.Realized.Episodes) != 0 || result.Options.Coverage.OpeningEpisodes != 1 || result.Options.Coverage.OpeningOnlyZeroEpisodes != 1 {
		t.Fatalf("opening-only zero was treated as a realized result: %+v", result.Options)
	}
	if len(result.Options.Open.Positions) != 1 || result.Options.Open.Positions[0].OpenPNLBase == nil || *result.Options.Open.Positions[0].OpenPNLBase != 18 {
		t.Fatalf("opening coverage displaced the open snapshot: %+v", result.Options.Open)
	}
}

func TestAnalyzeOpeningOptionVolumeCannotDisplaceAClosingOutcome(t *testing.T) {
	t.Parallel()
	st := edgeStatement()
	st.Instruments = append(st.Instruments, optionTestInstrument(456, "ACME CALL", 100, "20260320", "C"))
	for i := range 100 {
		st.Trades = append(st.Trades, flexstmt.Trade{
			RecordID: "opening-" + time.Duration(i).String(), AccountID: "U", ConID: 456, Symbol: "ACME CALL", UnderlyingSymbol: "ACME",
			AssetClass: "OPT", Currency: "USD", FXRateToBase: new(.9), OrderID: "opening-order-" + time.Duration(i).String(),
			ExecutedAt: dayTime("2026-01-12", 10).Add(time.Duration(i) * time.Second), Side: "BUY", OpenClose: "O",
			Quantity: new(float64(1)), Price: new(float64(2)), Multiplier: new(float64(100)), Commission: new(float64(-1)), Taxes: new(float64(0)), RealizedPNL: new(float64(0)), LevelOfDetail: "EXECUTION",
		})
	}
	st.Trades = append(st.Trades, flexstmt.Trade{
		RecordID: "closing-loss", AccountID: "U", ConID: 456, Symbol: "ACME CALL", UnderlyingSymbol: "ACME",
		AssetClass: "OPT", Currency: "USD", FXRateToBase: new(.9), OrderID: "closing-order", ExecutedAt: dayTime("2026-01-20", 10),
		Side: "SELL", OpenClose: "C", Quantity: new(float64(1)), Price: new(float64(1)), Multiplier: new(float64(100)),
		Commission: new(float64(-1)), Taxes: new(float64(0)), RealizedPNL: new(float64(-50)), LevelOfDetail: "EXECUTION",
	})

	result, err := Analyze(Input{AsOf: day("2026-02-10"), WindowDays: 90, BaseCurrency: "EUR", Statements: []flexstmt.Statement{st}, Bars: barsMap(123, "2026-01-06", 20, 105)})
	if err != nil {
		t.Fatal(err)
	}
	if result.Options.Coverage.ExecutionEpisodes != 101 || result.Options.Coverage.OpeningEpisodes != 100 || result.Options.Coverage.OpeningOnlyZeroEpisodes != 100 || result.Options.Coverage.ClosingEpisodes != 1 {
		t.Fatalf("option activity coverage=%+v", result.Options.Coverage)
	}
	if len(result.Options.Realized.Episodes) != 1 || result.Options.Realized.Episodes[0].RealizedPNLBase == nil || *result.Options.Realized.Episodes[0].RealizedPNLBase != -45 {
		t.Fatalf("opening activity displaced the closing outcome: %+v", result.Options.Realized)
	}
}

func TestAnalyzePartialOptionPNLStaysPartial(t *testing.T) {
	t.Parallel()
	st := edgeStatement()
	st.Instruments = append(st.Instruments,
		optionTestInstrument(456, "ACME CALL", 100, "20260320", "C"),
		optionTestInstrument(789, "ACME PUT", 90, "20260320", "P"),
	)
	st.Trades = append(st.Trades,
		flexstmt.Trade{RecordID: "known-leg", AccountID: "U", ConID: 456, Symbol: "ACME CALL", UnderlyingSymbol: "ACME", AssetClass: "OPT", Currency: "USD", FXRateToBase: new(.9), OrderID: "partial-order", ExecutedAt: dayTime("2026-01-20", 10), Side: "SELL", OpenClose: "C", Quantity: new(float64(1)), Price: new(float64(3)), RealizedPNL: new(float64(100))},
		flexstmt.Trade{RecordID: "missing-leg", AccountID: "U", ConID: 789, Symbol: "ACME PUT", UnderlyingSymbol: "ACME", AssetClass: "OPT", Currency: "USD", FXRateToBase: new(.9), OrderID: "partial-order", ExecutedAt: dayTime("2026-01-20", 10), Side: "BUY", OpenClose: "C", Quantity: new(float64(1)), Price: new(float64(2))},
	)

	result, err := Analyze(Input{AsOf: day("2026-02-10"), WindowDays: 90, BaseCurrency: "EUR", Statements: []flexstmt.Statement{st}, Bars: barsMap(123, "2026-01-06", 20, 105)})
	if err != nil {
		t.Fatal(err)
	}
	episode := result.Options.Realized.Episodes[0]
	if episode.PNLStatus != OptionPNLPartial || episode.RealizedPNLBase == nil || *episode.RealizedPNLBase != 90 || !hasString(episode.MissingEvidence, OptionMissingRealizedPNL) {
		t.Fatalf("partial option P/L was overstated: %+v", episode)
	}
	if result.Options.Realized.PartialCount != 1 || result.Options.Realized.CompleteCount != 0 || result.Options.Realized.PositiveCount != 1 {
		t.Fatalf("partial option summary=%+v", result.Options.Realized)
	}
}

func TestAnalyzeMixedOptionOrderIsNotCalledARoll(t *testing.T) {
	t.Parallel()
	st := edgeStatement()
	st.Instruments = append(st.Instruments,
		optionTestInstrument(456, "ACME OLD CALL", 100, "20260320", "C"),
		optionTestInstrument(789, "ACME NEW CALL", 105, "20260417", "C"),
	)
	st.Trades = append(st.Trades,
		flexstmt.Trade{RecordID: "close-leg", AccountID: "U", ConID: 456, Symbol: "ACME OLD CALL", UnderlyingSymbol: "ACME", AssetClass: "OPT", Currency: "USD", FXRateToBase: new(.9), OrderID: "mixed-order", ExecutedAt: dayTime("2026-01-20", 10), Side: "SELL", OpenClose: "C", Quantity: new(float64(1)), RealizedPNL: new(float64(40))},
		flexstmt.Trade{RecordID: "open-leg", AccountID: "U", ConID: 789, Symbol: "ACME NEW CALL", UnderlyingSymbol: "ACME", AssetClass: "OPT", Currency: "USD", FXRateToBase: new(.9), OrderID: "mixed-order", ExecutedAt: dayTime("2026-01-20", 10), Side: "BUY", OpenClose: "O", Quantity: new(float64(1)), RealizedPNL: new(float64(0))},
	)

	result, err := Analyze(Input{AsOf: day("2026-02-10"), WindowDays: 90, BaseCurrency: "EUR", Statements: []flexstmt.Statement{st}, Bars: barsMap(123, "2026-01-06", 20, 105)})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Options.Realized.Episodes) != 1 || result.Options.Realized.Episodes[0].Grouping != OptionGroupingExactOrder || result.Options.Realized.Episodes[0].Lifecycle != OptionLifecycleMixed {
		t.Fatalf("mixed exact order was given invented strategy semantics: %+v", result.Options)
	}
}

func TestAnalyzeSameOptionOrderIdentityOnDifferentDaysRemainsSeparate(t *testing.T) {
	t.Parallel()
	st := edgeStatement()
	st.Instruments = append(st.Instruments, optionTestInstrument(456, "ACME CALL", 100, "20260320", "C"))
	for i, date := range []string{"2026-01-20", "2026-01-21"} {
		st.Trades = append(st.Trades, flexstmt.Trade{
			RecordID: "same-order-" + time.Duration(i).String(), AccountID: "U", ConID: 456, Symbol: "ACME CALL", UnderlyingSymbol: "ACME",
			AssetClass: "OPT", Currency: "USD", FXRateToBase: new(.9), OrderID: "reused-order-id", ExecutedAt: dayTime(date, 10),
			Side: "SELL", OpenClose: "C", Quantity: new(float64(1)), RealizedPNL: new(float64(10 + i)),
		})
	}

	result, err := Analyze(Input{AsOf: day("2026-02-10"), WindowDays: 90, BaseCurrency: "EUR", Statements: []flexstmt.Statement{st}, Bars: barsMap(123, "2026-01-06", 20, 105)})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Options.Realized.Episodes) != 2 || result.Options.Realized.Episodes[0].ID == result.Options.Realized.Episodes[1].ID {
		t.Fatalf("same order identity across days was blended: %+v", result.Options.Realized.Episodes)
	}
}

func TestAnalyzeOptionEventLinkedToTradeIsNotDoubleCounted(t *testing.T) {
	t.Parallel()
	st := edgeStatement()
	st.Instruments = append(st.Instruments, optionTestInstrument(456, "ACME CALL", 100, "20260320", "C"))
	st.Trades = append(st.Trades, flexstmt.Trade{
		RecordID: "linked-trade", AccountID: "U", ConID: 456, Symbol: "ACME CALL", UnderlyingSymbol: "ACME", AssetClass: "OPT", Currency: "USD", FXRateToBase: new(.9),
		TradeID: "linked", ExecutedAt: dayTime("2026-01-20", 10), Side: "SELL", OpenClose: "C", Quantity: new(float64(1)), RealizedPNL: new(float64(25)),
	})
	st.OptionEvents = append(st.OptionEvents, flexstmt.OptionEvent{
		RecordID: "linked-event", AccountID: "U", ConID: 456, Symbol: "ACME CALL", UnderlyingSymbol: "ACME", AssetClass: "OPT", Currency: "USD", FXRateToBase: new(.9),
		Date: day("2026-01-20"), TransactionType: "ASSIGNMENT", Quantity: new(float64(-1)), RealizedPNL: new(float64(25)), TradeID: "linked",
	})

	result, err := Analyze(Input{AsOf: day("2026-02-10"), WindowDays: 90, BaseCurrency: "EUR", Statements: []flexstmt.Statement{st}, Bars: barsMap(123, "2026-01-06", 20, 105)})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Options.Realized.Episodes) != 1 || result.Options.Coverage.EventEpisodes != 0 || result.Options.Realized.KnownPNLBase == nil || *result.Options.Realized.KnownPNLBase != 22.5 {
		t.Fatalf("linked trade and OptionEAE were double counted: %+v", result.Options)
	}
}

func TestAnalyzeMissingOptionOpenPNLIsUnavailableNotZero(t *testing.T) {
	t.Parallel()
	st := edgeStatement()
	st.Instruments = append(st.Instruments, optionTestInstrument(456, "ACME CALL", 100, "20260320", "C"))
	st.Positions = append(st.Positions, flexstmt.OpenPosition{
		RecordID: "open-missing-pnl", AccountID: "U", ConID: 456, Symbol: "ACME CALL", UnderlyingSymbol: "ACME", AssetClass: "OPT", Currency: "USD", FXRateToBase: new(.9),
		ReportDate: day("2026-02-01"), Quantity: new(float64(1)), MarkPrice: new(float64(2)), CostBasisMoney: new(float64(200)), Side: "LONG",
	})

	result, err := Analyze(Input{AsOf: day("2026-02-10"), WindowDays: 90, BaseCurrency: "EUR", Statements: []flexstmt.Statement{st}, Bars: barsMap(123, "2026-01-06", 20, 105)})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Options.Open.Positions) != 1 || result.Options.Open.KnownPNLBase != nil || result.Options.Open.CompleteCount != 0 || result.Options.Open.UnavailableCount != 1 {
		t.Fatalf("missing open P/L became a numeric result: %+v", result.Options.Open)
	}
	position := result.Options.Open.Positions[0]
	if position.OpenPNLBase != nil || position.PNLStatus != OptionPNLUnavailable || !hasString(position.MissingEvidence, OptionMissingOpenPNL) {
		t.Fatalf("missing open P/L evidence=%+v", position)
	}
}

func TestAnalyzeTreatsMissingContractInLatestOpenPositionsAsZeroAnchor(t *testing.T) {
	t.Parallel()
	older := edgeStatement()
	older.ToDate = day("2026-01-10")
	older.WhenGenerated = dayTime("2026-01-11", 1)
	older.Positions[0].ReportDate = older.ToDate
	older.Positions[0].RecordID = "position-old"

	newer := edgeStatement()
	newer.WhenGenerated = dayTime("2026-02-02", 1)
	newer.Positions = nil
	result, err := Analyze(Input{AsOf: day("2026-02-10"), WindowDays: 90, BaseCurrency: "EUR", Statements: []flexstmt.Statement{older, newer}, Bars: barsMap(123, "2026-01-06", 20, 105)})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Changes) != 1 {
		t.Fatalf("changes=%+v", result.Changes)
	}
	for _, score := range result.Changes[0].Scores {
		if score.Reason != ReasonPositionPathUnbalanced || score.DecisionImpactBase != nil {
			t.Fatalf("missing current anchor did not suppress contract: %+v", result.Changes[0].Scores)
		}
	}
}

func TestTradesWithoutOrderIdentityRemainIndependent(t *testing.T) {
	t.Parallel()
	st := edgeStatement()
	first := st.Trades[0]
	first.OrderID = ""
	second := first
	second.RecordID = "trade-independent"
	second.ExecutionID = "different"
	second.Price = new(float64(101))
	groups := groupTrades([]flexstmt.Trade{first, second}, "EUR", st.FXRates)
	if len(groups) != 2 {
		t.Fatalf("execution groups = %d, want 2", len(groups))
	}
}

func TestFindingsRequireAccountMaterialityBeforePercentageRanking(t *testing.T) {
	t.Parallel()
	change := func(id string, impact, notional float64) Change {
		impactPct := impact / notional * 100
		return Change{
			ID: id, Symbol: id, Action: ActionAdd, Direction: DirectionLong, ExecutedAt: day("2026-01-05"),
			Scores: []HorizonScore{{Sessions: 20, DecisionNotionalBase: new(notional), DecisionImpactBase: new(impact), DecisionImpactPct: new(impactPct)}},
		}
	}
	findings := buildFindings([]Change{
		change("change_gain_100", 100, 10_000),
		change("change_gain_90", 90, 1_000),
		change("change_gain_80", 80, 2_000),
		change("change_loss_70", -70, 1_000),
		change("change_loss_10", -10, 100),
	}, 100_000)
	if len(findings) != 3 {
		t.Fatalf("findings=%+v", findings)
	}
	want := []string{"change_gain_90", "change_loss_70", "change_gain_80"}
	for i, id := range want {
		if findings[i].ChangeID != id {
			t.Fatalf("finding %d=%q want %q; all=%+v", i, findings[i].ChangeID, id, findings)
		}
	}
}

func TestCoverageKeepsOverlappedStockDecisionsInTheEligibleDenominator(t *testing.T) {
	t.Parallel()
	coverage := Coverage{ScoredByHorizon: map[int]int{}, ReasonCounts: map[string]int{}}
	populateCoverage(&coverage, []Change{
		{ID: "change_stock", AssetClass: "STK", Scores: []HorizonScore{{Sessions: 1, Reason: ReasonInterveningChange}, {Sessions: 5, Reason: ReasonInterveningChange}, {Sessions: 20, Reason: ReasonInterveningChange}}},
		{ID: "change_option", AssetClass: "OPT", Scores: []HorizonScore{{Sessions: 1, Reason: ReasonUnsupportedAsset}}},
	})
	if coverage.TradeChanges != 2 || coverage.EligibleChanges != 1 || coverage.ScoredByHorizon[20] != 0 || coverage.ReasonCounts[ReasonInterveningChange] != 1 {
		t.Fatalf("coverage=%+v", coverage)
	}
}

func TestMarketContextExplainsTheSameIntervalWithoutChangingImpact(t *testing.T) {
	t.Parallel()
	st := edgeStatement()
	input := Input{
		AsOf: day("2026-02-10"), WindowDays: 90, BaseCurrency: "EUR",
		Statements: []flexstmt.Statement{st}, Bars: barsMap(123, "2026-01-06", 25, 105),
	}
	withoutContext, err := Analyze(input)
	if err != nil {
		t.Fatal(err)
	}
	input.ContextBars = map[string][]DailyBar{
		"SPY": {{Day: day("2026-01-02"), Close: 100}, {Day: day("2026-01-06"), Close: 110}, {Day: day("2026-01-10"), Close: 120}, {Day: day("2026-01-25"), Close: 130}},
		"QQQ": {{Day: day("2026-01-02"), Close: 200}, {Day: day("2026-01-06"), Close: 190}, {Day: day("2026-01-10"), Close: 210}, {Day: day("2026-01-25"), Close: 220}},
		"DIA": {{Day: day("2026-01-02"), Close: 400}, {Day: day("2026-01-06"), Close: 404}, {Day: day("2026-01-10"), Close: 408}, {Day: day("2026-01-25"), Close: 412}},
		"VIX": {{Day: day("2026-01-02"), Close: 20}, {Day: day("2026-01-06"), Close: 25}, {Day: day("2026-01-10"), Close: 18}, {Day: day("2026-01-25"), Close: 15}},
	}
	withContext, err := Analyze(input)
	if err != nil {
		t.Fatal(err)
	}
	for index, score := range withContext.Changes[0].Scores {
		baseline := withoutContext.Changes[0].Scores[index]
		if score.DecisionImpactBase == nil || baseline.DecisionImpactBase == nil || *score.DecisionImpactBase != *baseline.DecisionImpactBase {
			t.Fatalf("context changed Decision price impact: with=%+v without=%+v", score, baseline)
		}
		if len(score.MarketContext) != 4 {
			t.Fatalf("%d-session context=%+v", score.Sessions, score.MarketContext)
		}
	}
	first := withContext.Changes[0].Scores[0].MarketContext
	if first[0].Key != "spy" || !almostEqual(first[0].ChangePct, 10) || first[3].Key != "vix" || first[3].ChangePoints == nil || *first[3].ChangePoints != 5 {
		t.Fatalf("unexpected first-horizon context=%+v", first)
	}
	if len(withContext.Rollups[0].Horizons[0].MarketContext) != 4 || len(withContext.Findings) == 0 || len(withContext.Findings[0].MarketContext) != 4 {
		t.Fatalf("context did not travel through rollup/finding: rollup=%+v findings=%+v", withContext.Rollups, withContext.Findings)
	}
}

func edgeStatement() flexstmt.Statement {
	coverage := make([]flexstmt.SectionCoverage, 0, len(flexstmt.CanonicalQueryManifest()))
	for _, section := range flexstmt.CanonicalQueryManifest() {
		coverage = append(coverage, flexstmt.SectionCoverage{Key: section.Key, Present: true})
	}
	fx := []flexstmt.FXRate{}
	for i := range 40 {
		fx = append(fx, flexstmt.FXRate{RecordID: "fx" + time.Duration(i).String(), Date: day("2026-01-01").AddDate(0, 0, i), FromCurrency: "USD", ToCurrency: "EUR", Rate: new(.9)})
	}
	return flexstmt.Statement{
		AccountID: "U", FromDate: day("2025-11-01"), ToDate: day("2026-02-01"), WhenGenerated: dayTime("2026-02-02", 1), ManifestVersion: flexstmt.ManifestVersion, Coverage: coverage,
		Trades:    []flexstmt.Trade{{RecordID: "trade-one", AccountID: "U", ConID: 123, Symbol: "ACME", AssetClass: "STK", Currency: "USD", FXRateToBase: new(.9), OrderID: "o1", ExecutionID: "e1", ExecutedAt: dayTime("2026-01-05", 12), Side: "BUY", Quantity: new(float64(10)), Price: new(float64(100)), Multiplier: new(float64(1)), Commission: new(float64(-2)), Taxes: new(float64(0)), LevelOfDetail: "EXECUTION"}},
		Positions: []flexstmt.OpenPosition{{RecordID: "p-latest", AccountID: "U", ConID: 123, Symbol: "ACME", AssetClass: "STK", Currency: "USD", FXRateToBase: new(.9), ReportDate: day("2026-02-01"), Quantity: new(float64(10)), Multiplier: new(float64(1))}},
		Cash:      []flexstmt.CashLine{{ID: "flow", Category: flexstmt.CategoryFlow, Currency: "EUR", Amount: new(float64(1000)), AmountBase: new(float64(1000)), ValueDate: day("2026-01-15")}},
		Equity:    []flexstmt.EquityRow{{ReportDate: day("2025-11-12"), TotalBase: 10000}, {ReportDate: day("2026-02-01"), TotalBase: 11500}},
		FXRates:   fx,
	}
}

func risingBars(conid int64, start string, count int, firstClose float64) []DailyBar {
	out := make([]DailyBar, 0, count)
	startDay := day(start)
	for i := range count {
		out = append(out, DailyBar{ConID: conid, Day: startDay.AddDate(0, 0, i), Close: firstClose + float64(i)})
	}
	return out
}

func barsMap(conid int64, start string, count int, firstClose float64) map[int64][]DailyBar {
	return map[int64][]DailyBar{conid: risingBars(conid, start, count, firstClose)}
}

func optionTestInstrument(conid int64, symbol string, strike float64, expiry, putCall string) flexstmt.Instrument {
	return flexstmt.Instrument{
		RecordID: "instrument-" + symbol, ConID: conid, Symbol: symbol, UnderlyingSymbol: "ACME",
		AssetClass: "OPT", Currency: "USD", Multiplier: new(float64(100)), Strike: new(strike), Expiry: expiry, PutCall: putCall,
	}
}

func hasString(values []string, want string) bool {
	return slices.Contains(values, want)
}

func day(v string) time.Time {
	parsed, err := time.Parse("2006-01-02", v)
	if err != nil {
		panic(err)
	}
	return parsed
}
func dayTime(v string, hour int) time.Time { return day(v).Add(time.Duration(hour) * time.Hour) }
