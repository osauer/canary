package daemon

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/broker"
	"github.com/osauer/canary/v2/internal/flexstmt"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestReconLegacySampleNeedsValuationBasis(t *testing.T) {
	s := newReconV3TestServer(t)
	now := time.Now().UTC()
	scope := brokerStateScope{Account: "U1234567", Mode: rpc.AccountModeLive}
	s.riskCapital.Observe(98000, now, s.riskPolicies.snapshot().policy, scope, true)
	day := utcDate(now.Year(), now.Month(), now.Day())
	got := s.reconEquityCheck(retainedStatementMerge{equityByDay: map[string]flexstmt.EquityRow{"day": {ReportDate: day, TotalBase: 100000}}}, scope)
	if got.DivergencePct != nil {
		t.Fatalf("date-only sample produced a policy divergence: %v; valuation and FX basis are unproved", *got.DivergencePct)
	}
	if got.RuntimeEquityBase == nil || *got.RuntimeEquityBase != 98000 {
		t.Fatal("the original runtime observation must remain available as evidence")
	}
}

func reconValuationFixture() (flexstmt.EquityRow, flexstmt.Statement, rpc.ReconRuntimeValuation) {
	day := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	row := flexstmt.EquityRow{ReportDate: day, TotalBase: 100000}
	st := flexstmt.Statement{AccountID: "U1234567", FromDate: day, ToDate: day, WhenGenerated: day.Add(30 * time.Hour), Equity: []flexstmt.EquityRow{row},
		FX:        &flexstmt.FXSnapshot{Day: "2026-09-23", SingleDay: true, BaseCurrency: "EUR", NAV: 100000, Book: map[string]float64{"EUR": 50000, "USD": 50000}, Rates: map[string]float64{"EUR": 1, "USD": 1}},
		Positions: []flexstmt.OpenPosition{{ConID: 101, AccountID: "U1234567", Currency: "USD", AssetClass: "STK", ReportDate: day, LevelOfDetail: "SUMMARY", Quantity: new(100.0), Multiplier: new(1.0), MarkPrice: new(100.0)}},
	}
	sample := rpc.ReconRuntimeValuation{AsOf: day.Add(22 * time.Hour), BaseCurrency: "EUR", EquityBase: 98000,
		Currencies: map[string]rpc.ReconNativeValuation{"EUR": {Equity: 50000, FXRate: 1}, "USD": {Equity: 50000, FXRate: .96}},
		Positions:  []rpc.ReconPositionValuation{{ConID: 101, Currency: "USD", AssetClass: "STK", Quantity: 100, Multiplier: 1, MarketValue: 10000, Mark: 100, ObservedAt: day.Add(22 * time.Hour)}},
	}
	return row, st, sample
}

func TestReconCommonValuationBasis(t *testing.T) {
	for _, tc := range []struct {
		name                string
		price, fx, residual float64
	}{
		{"FX alone", 100, .96, 0}, {"marks alone", 90, .9, 0}, {"FX and marks", 90, .96, 0}, {"real residual retained", 90, .96, -2000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row, st, sample := reconValuationFixture()
			row.TotalBase, st.FX.NAV = 95000, 95000
			st.Equity[0] = row
			st.FX.Rates["USD"] = .9
			sample.Positions[0].Mark, sample.Positions[0].MarketValue = tc.price, 100*tc.price
			native := 40000 + 100*tc.price
			sample.Currencies["USD"] = rpc.ReconNativeValuation{Equity: native, FXRate: tc.fx}
			sample.EquityBase = 50000 + native*tc.fx + tc.residual
			got := compareReconValuation(row, st, sample)
			if got.Comparability != "comparable" || got.DivergencePct == nil || math.Abs(*got.DivergencePct-tc.residual/row.TotalBase*100) > 1e-10 {
				t.Fatalf("comparison=%+v", got)
			}
			if got.Evidence == nil || got.Evidence.UnallocatedRuntimeBase != tc.residual || got.RuntimeAsOf != sample.AsOf || *got.RuntimeEquityBase != sample.EquityBase {
				t.Fatalf("lost original evidence: %+v", got)
			}
			pol := newReconV3TestServer(t).riskPolicies.snapshot().policy
			originalLimit := *pol.Recon.MaxEquityDivergencePct
			rep := cleanReconReport(sample.AsOf)
			rep.Equity = got
			if autoExtendEligible(rpc.RiskPolicyStatusActive, pol, rep, sample.AsOf) != (tc.residual == 0) {
				t.Fatal("normalization hid a real breach or rejected a valuation-only difference")
			}
			if *pol.Recon.MaxEquityDivergencePct != originalLimit {
				t.Fatal("changed policy limit")
			}
		})
	}
}

func TestReconMissingOrChangedBasisCannotPass(t *testing.T) {
	cases := map[string]func(*flexstmt.Statement, *rpc.ReconRuntimeValuation){
		"FX missing": func(st *flexstmt.Statement, _ *rpc.ReconRuntimeValuation) { delete(st.FX.Rates, "USD") },
		"runtime FX missing": func(_ *flexstmt.Statement, v *rpc.ReconRuntimeValuation) {
			v.Currencies["USD"] = rpc.ReconNativeValuation{Equity: 50000}
		},
		"native currency absent":  func(_ *flexstmt.Statement, v *rpc.ReconRuntimeValuation) { delete(v.Currencies, "USD") },
		"different base currency": func(_ *flexstmt.Statement, v *rpc.ReconRuntimeValuation) { v.BaseCurrency = "USD" },
		"different date":          func(_ *flexstmt.Statement, v *rpc.ReconRuntimeValuation) { v.AsOf = v.AsOf.Add(-24 * time.Hour) },
		"different quantity":      func(_ *flexstmt.Statement, v *rpc.ReconRuntimeValuation) { v.Positions[0].Quantity = 99 },
		"missing position":        func(_ *flexstmt.Statement, v *rpc.ReconRuntimeValuation) { v.Positions = nil },
		"incomplete closing book": func(st *flexstmt.Statement, _ *rpc.ReconRuntimeValuation) {
			st.FX.Reason = "native_cash_balances_missing"
		},
		"incoherent runtime":     func(_ *flexstmt.Statement, v *rpc.ReconRuntimeValuation) { v.Reason = "runtime_valuation_incoherent" },
		"unsupported instrument": func(st *flexstmt.Statement, _ *rpc.ReconRuntimeValuation) { st.Positions[0].AssetClass = "FUT" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			row, st, v := reconValuationFixture()
			mutate(&st, &v)
			got := compareReconValuation(row, st, v)
			if got.DivergencePct != nil || got.Comparability == "comparable" || got.Evidence == nil {
				t.Fatalf("unproved basis passed or evidence lost: %+v", got)
			}
		})
	}
}

func TestReconValuationEvidenceSurvivesRestartAndIsScoped(t *testing.T) {
	store, pol, now := recoveryStore(t)
	row, statement, sample := reconValuationFixture()
	*now = sample.AsOf
	store.observeWithValuation(sample.EquityBase, mandatePnLReading{}, sample.AsOf, pol, testLiveObserveScope, true, &sample)
	check := compareReconValuation(row, statement, sample)
	rep := &rpc.ReconResult{ReportID: "recon-test-evidence", Equity: check}
	if err := store.retainReconValuation(rep, testLiveObserveScope); err != nil {
		t.Fatal(err)
	}
	// The caller's maps must not mutate retained evidence.
	sample.Currencies["USD"] = rpc.ReconNativeValuation{Equity: 1, FXRate: 1}
	restarted := &riskCapitalStore{now: store.now}
	if err := restarted.bindCore(t.Context(), store.core); err != nil {
		t.Fatal(err)
	}
	ctx, err := restarted.ReplayContextForScope(testLiveObserveScope)
	if err != nil {
		t.Fatal(err)
	}
	retained, ok := ctx.DailyValuation["2026-09-23"]
	if !ok || retained.Currencies["USD"].Equity != 50000 || retained.AsOf != *now {
		t.Fatalf("retained=%+v", retained)
	}
	other := testLiveObserveScope
	other.Account = "U7654321"
	ctx, err = restarted.ReplayContextForScope(other)
	if err != nil {
		t.Fatal(err)
	}
	if len(ctx.DailyValuation) != 0 {
		t.Fatal("valuation leaked across account scopes")
	}
	payloads, err := restarted.GovernanceEventPayloadsForScope(t.Context(), testLiveObserveScope)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range payloads {
		var event struct {
			Kind   string                `json:"kind"`
			Equity *rpc.ReconEquityCheck `json:"equity"`
		}
		if json.Unmarshal(p, &event) == nil && event.Kind == "recon_valuation_evaluated" {
			found = event.Equity != nil && event.Equity.Evidence != nil && event.Equity.Evidence.Runtime.Currencies["USD"].Equity == 50000
		}
	}
	if !found {
		t.Fatal("reproducible comparison absent from durable journal")
	}
}

// A read just before UTC midnight can catch a portfolio frame stamped after
// it. That incomplete last sample must not displace the day's complete one,
// or the day's statement can never be compared (live, 2026-10-22).
func TestReconIncompleteLaterSampleKeepsDayComparable(t *testing.T) {
	store, pol, now := recoveryStore(t)
	row, statement, complete := reconValuationFixture()
	observe := func(sample rpc.ReconRuntimeValuation) {
		*now = sample.AsOf
		store.observeWithValuation(sample.EquityBase, mandatePnLReading{}, sample.AsOf, pol, testLiveObserveScope, true, &sample)
	}
	kept := func() rpc.ReconRuntimeValuation {
		ctx, err := store.ReplayContextForScope(testLiveObserveScope)
		if err != nil {
			t.Fatal(err)
		}
		return ctx.DailyValuation["2026-09-23"]
	}
	observe(complete)

	late := cloneReconValuation(complete)
	late.AsOf = complete.AsOf.Add(time.Hour + 59*time.Minute + 53*time.Second)
	late.Positions[0].ObservedAt = late.AsOf.Add(10 * time.Second)
	late.Reason = "runtime_position_basis_missing"
	observe(late)
	if got := kept(); got.Reason != "" || !got.AsOf.Equal(complete.AsOf) {
		t.Fatalf("incomplete late sample displaced the complete one: %+v", got)
	}
	if got := compareReconValuation(row, statement, kept()); got.Comparability != "comparable" {
		t.Fatalf("comparison=%+v", got)
	}

	// A later complete sample is better evidence and replaces the earlier one.
	later := cloneReconValuation(complete)
	later.AsOf = late.AsOf.Add(time.Second)
	later.Positions[0].ObservedAt = later.AsOf
	observe(later)
	if got := kept(); !got.AsOf.Equal(later.AsOf) {
		t.Fatalf("later complete sample was not kept: %+v", got)
	}
}

func TestReconReportIdentityPinsValuationEvidence(t *testing.T) {
	row, st, sample := reconValuationFixture()
	pol := newReconV3TestServer(t).riskPolicies.snapshot().policy
	id := func(check *rpc.ReconEquityCheck) string {
		return reconReportID(nil, nil, nil, nil, nil, st.FromDate, st.ToDate, st.WhenGenerated, pol, "scope", check)
	}
	first := id(compareReconValuation(row, st, sample))
	sample.Currencies["USD"] = rpc.ReconNativeValuation{Equity: 50000, FXRate: .97}
	if id(compareReconValuation(row, st, sample)) == first {
		t.Fatal("changed FX evidence reused a report identity")
	}
}

func TestReconCapturesBrokerBasisWithoutInventingMissingRates(t *testing.T) {
	_, _, sample := reconValuationFixture()
	raw := &broker.AccountSummary{BaseCurrency: "EUR", BaseCurrencyProvenance: broker.BaseCurrencyExplicitTag, NetLiquidation: new(sample.EquityBase), CurrencyLedger: map[string]broker.CurrencyLedger{
		"EUR": {NetLiquidationByCurrency: 50000, NetLiquidationObserved: true},
		"USD": {NetLiquidationByCurrency: 50000, NetLiquidationObserved: true, StockMarketValue: 10000, StockMarketValueText: "10000", ExchangeRate: .96, ExchangeRateObserved: true},
	}}
	positions := []*ibkrlib.RawPosition{{ValuationAt: sample.AsOf, Contract: ibkrlib.Contract{ConID: 101, SecType: "STK", Currency: "USD"}, Position: 100, MarketPrice: 100, MarketValue: 10000}}
	got := captureReconValuation(raw, positions, sample.AsOf, nil)
	if got == nil || got.Reason != "" || got.Currencies["USD"].FXRate != .96 {
		t.Fatalf("capture=%+v", got)
	}
	row := raw.CurrencyLedger["USD"]
	row.ExchangeRateObserved = false
	raw.CurrencyLedger["USD"] = row
	if got = captureReconValuation(raw, positions, sample.AsOf, nil); got == nil || got.Reason == "" {
		t.Fatal("unobserved FX rate was accepted")
	}
	row.ExchangeRateObserved = true
	raw.CurrencyLedger["USD"] = row
	positions[0].MarketValue = 9999
	if got = captureReconValuation(raw, positions, sample.AsOf, nil); got == nil || got.Reason != "runtime_valuation_incoherent" {
		t.Fatal("mixed book snapshots were accepted")
	}
	positions[0].MarketValue = math.NaN()
	got = captureReconValuation(raw, positions, sample.AsOf, nil)
	if _, err := json.Marshal(got); err != nil {
		t.Fatal("invalid broker value poisoned durable state", err)
	}
}

func TestReconBacktestSeparatesRawFromComparableDifference(t *testing.T) {
	row, st, sample := reconValuationFixture()
	merged := mergeRetainedStatements([]flexstmt.Statement{st})
	ctx := capitalReplayContext{DailyEquity: map[string]float64{"2026-09-23": sample.EquityBase}, DailyValuation: map[string]rpc.ReconRuntimeValuation{"2026-09-23": sample}}
	got := buildCapitalReplay(merged, ctx, 10, 20, 100000, time.Time{})
	if got.SameDayComparisons != 1 || got.MaxSameDayDivergencePct == nil || *got.MaxSameDayDivergencePct != 0 || got.MaxRawSameDayDivergencePct == nil || *got.MaxRawSameDayDivergencePct != -2 {
		t.Fatalf("replay=%+v", got)
	}
	delete(ctx.DailyValuation, row.ReportDate.Format("2006-01-02"))
	got = buildCapitalReplay(merged, ctx, 10, 20, 100000, time.Time{})
	if got.SameDayComparisons != 0 || got.MaxSameDayDivergencePct != nil || got.RawSameDayComparisons != 1 {
		t.Fatal("legacy replay counted an unproved pair")
	}
}

func TestReconLatestStatementCannotBorrowOlderComparableEvidence(t *testing.T) {
	s := newReconV3TestServer(t)
	row, statement, sample := reconValuationFixture()
	now := sample.AsOf.Add(24 * time.Hour)
	s.riskCapital.now = func() time.Time { return now }
	scope := brokerStateScope{Account: "U1234567", Mode: rpc.AccountModeLive}
	s.riskCapital.observeWithValuation(sample.EquityBase, mandatePnLReading{}, sample.AsOf, s.riskPolicies.snapshot().policy, scope, true, &sample)
	latest := row
	latest.ReportDate = latest.ReportDate.Add(24 * time.Hour)
	merged := mergeRetainedStatements([]flexstmt.Statement{statement, {AccountID: statement.AccountID, WhenGenerated: statement.WhenGenerated.Add(24 * time.Hour), Equity: []flexstmt.EquityRow{latest}}})
	got := s.reconEquityCheck(merged, scope)
	if !got.StatementDate.Equal(latest.ReportDate) || got.DivergencePct != nil {
		t.Fatalf("borrowed older pair: %+v", got)
	}
}

func TestReconCaptureRespectsPublishedWholeUnitPrecision(t *testing.T) {
	_, _, sample := reconValuationFixture()
	raw := &broker.AccountSummary{BaseCurrency: "EUR", BaseCurrencyProvenance: broker.BaseCurrencyExplicitTag, NetLiquidation: new(sample.EquityBase), Raw: map[string]string{"$LEDGER:StockMarketValue_USD": "10000"}, CurrencyLedger: map[string]broker.CurrencyLedger{
		"EUR": {NetLiquidationByCurrency: 50000, NetLiquidationObserved: true},
		"USD": {NetLiquidationByCurrency: 50000, NetLiquidationObserved: true, StockMarketValue: 10000, StockMarketValueText: "10000", ExchangeRate: .96, ExchangeRateObserved: true},
	}}
	positions := []*ibkrlib.RawPosition{{ValuationAt: sample.AsOf, Contract: ibkrlib.Contract{ConID: 101, SecType: "STK", Currency: "USD"}, Position: 100, MarketPrice: 100.002, MarketValue: 10000.20}}
	got := captureReconValuation(raw, positions, sample.AsOf, nil)
	if got == nil || got.Reason != "" {
		t.Fatalf("a book agreeing at the published whole-unit precision was rejected: %+v", got)
	}
}

func TestReconPublishedPrecisionIsEvidenceNotTolerance(t *testing.T) {
	for _, tc := range []struct {
		total    float64
		reported string
		matches  bool
	}{
		{10000.20, "10000", true}, {10000.20, "10000.00", false},
		{10001, "10000", false}, {-10000.20, "-10000", true},
		{10000, "", false}, {0, "", true}, {10000, "1e4", false}, {0, "NaN", false},
	} {
		if got := reconPublishedValueMatches(tc.total, tc.reported); got != tc.matches {
			t.Errorf("%v vs %q: %t", tc.total, tc.reported, got)
		}
	}
}

// The broker delivers each day in three queries: year to date, the single
// day, and a 35-day window generated last. Only the single-day statement
// carries the day's closing FX basis; the windows read
// daily_statement_required. The newest generation still owns the equity
// row, but the closing basis comes from the newest statement that has one,
// so the window generated last cannot leave every report incomparable (from
// 39ee0097 on 2026-10-22 it did, and the reconcile clock lapsed 2026-10-27).
func TestReconClosingBasisComesFromTheSingleDayStatement(t *testing.T) {
	s := newReconV3TestServer(t)
	row, daily, sample := reconValuationFixture()
	s.riskCapital.now = func() time.Time { return sample.AsOf.Add(time.Hour) }
	scope := brokerStateScope{Account: "U1234567", Mode: rpc.AccountModeLive}
	s.riskCapital.observeWithValuation(sample.EquityBase, mandatePnLReading{}, sample.AsOf, s.riskPolicies.snapshot().policy, scope, true, &sample)
	window := func(days int, generated time.Duration) flexstmt.Statement {
		st := daily
		st.FromDate, st.WhenGenerated = row.ReportDate.AddDate(0, 0, -days), daily.WhenGenerated.Add(generated)
		st.FX = &flexstmt.FXSnapshot{Day: daily.FX.Day, Reason: "daily_statement_required"}
		return st
	}
	merged := mergeRetainedStatements([]flexstmt.Statement{window(265, -time.Minute), daily, window(34, 90*time.Minute)})
	got := s.reconEquityCheck(merged, scope)
	if got.Comparability != "comparable" || got.DivergencePct == nil {
		t.Fatalf("the window generated last hid the single day's closing basis: %+v", got)
	}
	// Without a single-day statement the day stays unproven.
	merged = mergeRetainedStatements([]flexstmt.Statement{window(265, -time.Minute), window(34, 90*time.Minute)})
	if got := s.reconEquityCheck(merged, scope); got.Comparability != "statement_basis_missing" || got.DivergencePct != nil {
		t.Fatalf("a window without a closing basis passed: %+v", got)
	}
}

// The statement keeps sub-cent FX dust in currencies the live account stream
// never publishes (CHF and HKD under a thousandth on 2026-10-26). A closing
// balance that rounds to zero at the cent, the broker's monetary precision, is
// no balance; anything at a cent or more still needs a runtime slice.
func TestReconSubCentClosingDustNeedsNoRuntimeSlice(t *testing.T) {
	row, st, sample := reconValuationFixture()
	st.FX.Book["CHF"], st.FX.Rates["CHF"] = 0.000004, 1.07
	st.FX.Book["HKD"], st.FX.Rates["HKD"] = 0.0009, 0.11
	if got := compareReconValuation(row, st, sample); got.Comparability != "comparable" {
		t.Fatalf("sub-cent dust blocked the comparison: %+v", got)
	}
	st.FX.Book["CHF"] = 0.01
	if got := compareReconValuation(row, st, sample); got.Comparability != "runtime_currency_basis_missing" || got.DivergencePct != nil {
		t.Fatalf("a one-cent balance without a runtime slice passed: %+v", got)
	}
}
