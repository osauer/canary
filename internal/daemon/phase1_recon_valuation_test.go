package daemon

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// A foreign unit rate is the suspicious provider value for which the normal
// account projection already requires independent FX resolution. Capturing it
// as an observed field must not promote it into historical valuation proof.
func TestOperationalReconCaptureForeignUnitFXNeedsIndependentEvidence(t *testing.T) {
	_, _, sample := reconValuationFixture()
	raw := &ibkrlib.RawAccountSummary{
		BaseCurrency: "EUR", BaseCurrencyProvenance: ibkrlib.AccountBaseCurrencyExplicitTag,
		NetLiquidation: new(sample.EquityBase),
		CurrencyLedger: map[string]ibkrlib.CurrencyLedger{
			"EUR": {NetLiquidationByCurrency: 50000, NetLiquidationObserved: true},
			"USD": {NetLiquidationByCurrency: 50000, NetLiquidationObserved: true,
				StockMarketValue: 10000, StockMarketValueText: "10000",
				ExchangeRate: 1, ExchangeRateObserved: true},
		},
	}
	positions := []*ibkrlib.RawPosition{{ValuationAt: sample.AsOf,
		Contract: ibkrlib.Contract{ConID: 101, SecType: "STK", Currency: "USD"},
		Position: 100, MarketPrice: 100, MarketValue: 10000}}
	got := captureReconValuation(raw, positions, sample.AsOf, nil)
	if got == nil {
		t.Fatal("missing basis must retain the original account reading")
	}
	if got.EquityBase != sample.EquityBase || got.Currencies["USD"].FXRate != 1 || got.Currencies["USD"].Equity != 50000 {
		t.Fatalf("raw broker evidence was rewritten: %+v", got)
	}
	if got.Reason == "" {
		t.Error("foreign unit FX with no independent proof was admitted as complete historical basis")
	}
}

// Old durable samples predate the capture guard. Their empty Reason must not
// grandfather suspect FX into the automatic policy-extension decision.
func TestOperationalReconLegacyForeignUnitFXCannotEraseEquityDifference(t *testing.T) {
	for _, equity := range []float64{100000, 98000} {
		t.Run(fmt.Sprintf("equity_%g", equity), func(t *testing.T) {
			row, statement, legacy := reconValuationFixture()
			row.TotalBase, statement.FX.NAV = 95000, 95000
			statement.Equity[0] = row
			statement.FX.Rates["USD"] = .9
			legacy.EquityBase = equity
			legacy.Currencies["USD"] = rpc.ReconNativeValuation{Equity: 50000, FXRate: 1}
			legacy.Reason = ""

			check := compareReconValuation(row, statement, legacy)
			wantRaw := (legacy.EquityBase - row.TotalBase) / row.TotalBase * 100
			if check.RuntimeEquityBase == nil || *check.RuntimeEquityBase != legacy.EquityBase ||
				check.RawDivergencePct == nil || math.Abs(*check.RawDivergencePct-wantRaw) > 1e-10 ||
				check.Evidence == nil || check.Evidence.Runtime.Currencies["USD"].FXRate != 1 ||
				check.Evidence.UnallocatedRuntimeBase != equity-100000 {
				t.Fatalf("unverified comparison lost the raw difference, residual or broker evidence: %+v", check)
			}
			if check.Comparability == "comparable" || check.DivergencePct != nil || check.ComparedRuntimeBase != nil {
				t.Errorf("unverified foreign unit FX manufactured a policy comparison: %+v", check)
			}
			pol := newReconV3TestServer(t).riskPolicies.snapshot().policy
			report := cleanReconReport(legacy.AsOf)
			report.Equity = check
			hold, held := autoExtendHoldFor(rpc.RiskPolicyStatusActive, pol, report, legacy.AsOf)
			if !held || hold.State != rpc.ReconEvaluationStateWaiting || hold.Reason != rpc.ReconEvaluationReasonAccountValuePending {
				t.Errorf("unverified legacy basis must wait for evidence, not extend or assert a proved mismatch: held=%t hold=%+v", held, hold)
			}
		})
	}
}

// The broker's account rows read 1 for a foreign currency on every request, so
// the sample takes its rate from a live quote of the same moment. The quote
// must carry the reading through the whole comparison, keep the account value
// as the starting point and leave a real residual visible.
func TestOperationalReconForeignUnitFXUsesLiveQuoteOfTheSample(t *testing.T) {
	row, st, sample := reconValuationFixture()
	st.FX.Rates["USD"] = .9
	row.TotalBase, st.FX.NAV = 95000, 95000
	st.Equity[0] = row
	raw := &ibkrlib.RawAccountSummary{
		BaseCurrency: "EUR", BaseCurrencyProvenance: ibkrlib.AccountBaseCurrencyExplicitTag,
		NetLiquidation: new(sample.EquityBase),
		CurrencyLedger: map[string]ibkrlib.CurrencyLedger{
			"EUR": {NetLiquidationByCurrency: 50000, NetLiquidationObserved: true, ExchangeRate: 1, ExchangeRateObserved: true},
			"USD": {NetLiquidationByCurrency: 50000, NetLiquidationObserved: true,
				StockMarketValue: 10000, StockMarketValueText: "10000",
				ExchangeRate: 1, ExchangeRateObserved: true},
		},
	}
	positions := []*ibkrlib.RawPosition{{ValuationAt: sample.AsOf,
		Contract: ibkrlib.Contract{ConID: 101, SecType: "STK", Currency: "USD"},
		Position: 100, MarketPrice: 100, MarketValue: 10000}}
	quoteAt := sample.AsOf.Add(time.Second)
	live := map[string]reconFXQuote{"USD": {Rate: .96, At: quoteAt, Source: orderFXSourceExactSessionQuote}}

	got := captureReconValuation(raw, positions, sample.AsOf, live)
	usd := got.Currencies["USD"]
	if got.Reason != "" || usd.FXRate != .96 || usd.FXSource != orderFXSourceExactSessionQuote || !usd.FXObservedAt.Equal(quoteAt) {
		t.Fatalf("a live quote at the sample did not establish the rate: %+v", got)
	}
	if eur := got.Currencies["EUR"]; eur.FXRate != 1 || eur.FXSource != "" {
		t.Fatalf("the base currency needs no quote: %+v", eur)
	}
	check := compareReconValuation(row, st, *got)
	if check.Comparability != "comparable" || check.DivergencePct == nil || math.Abs(*check.DivergencePct) > 1e-9 {
		t.Fatalf("the sample did not reach the comparison: %+v", check)
	}

	// The account value stays the starting point: a real difference still reads.
	short := *got
	short.EquityBase -= 2000
	if check = compareReconValuation(row, st, short); check.DivergencePct == nil || math.Abs(*check.DivergencePct+2000.0/95000*100) > 1e-9 || check.Evidence.UnallocatedRuntimeBase != -2000 {
		t.Fatalf("a quote hid a real residual: %+v", check)
	}

	for name, quote := range map[string]reconFXQuote{
		"older than the window": {Rate: .96, At: sample.AsOf.Add(-reconFXQuoteMaxAge - time.Second), Source: orderFXSourceExactSessionQuote},
		"later than the window": {Rate: .96, At: sample.AsOf.Add(reconFXQuoteMaxAge + time.Second), Source: orderFXSourceExactSessionQuote},
		"a unit quote":          {Rate: 1, At: quoteAt, Source: orderFXSourceExactSessionQuote},
		"no observation time":   {Rate: .96, Source: orderFXSourceExactSessionQuote},
		"no source":             {Rate: .96, At: quoteAt},
		"not a positive number": {Rate: math.NaN(), At: quoteAt, Source: orderFXSourceExactSessionQuote},
		"zero":                  {At: quoteAt, Source: orderFXSourceExactSessionQuote},
	} {
		got := captureReconValuation(raw, positions, sample.AsOf, map[string]reconFXQuote{"USD": quote})
		if usd := got.Currencies["USD"]; got.Reason != "runtime_currency_basis_missing" || usd.FXRate != 1 || usd.FXSource != "" {
			t.Errorf("%s was admitted or rewrote the broker reading: %+v", name, got)
		}
	}
}

// A real broker rate is the evidence, and a quote never displaces it.
func TestOperationalReconBrokerRateOutranksLiveQuote(t *testing.T) {
	_, _, sample := reconValuationFixture()
	raw := &ibkrlib.RawAccountSummary{
		BaseCurrency: "EUR", BaseCurrencyProvenance: ibkrlib.AccountBaseCurrencyExplicitTag,
		NetLiquidation: new(sample.EquityBase),
		CurrencyLedger: map[string]ibkrlib.CurrencyLedger{
			"EUR": {NetLiquidationByCurrency: 50000, NetLiquidationObserved: true},
			"USD": {NetLiquidationByCurrency: 50000, NetLiquidationObserved: true,
				StockMarketValue: 10000, StockMarketValueText: "10000",
				ExchangeRate: .96, ExchangeRateObserved: true},
		},
	}
	positions := []*ibkrlib.RawPosition{{ValuationAt: sample.AsOf,
		Contract: ibkrlib.Contract{ConID: 101, SecType: "STK", Currency: "USD"},
		Position: 100, MarketPrice: 100, MarketValue: 10000}}
	got := captureReconValuation(raw, positions, sample.AsOf, map[string]reconFXQuote{"USD": {Rate: .9, At: sample.AsOf, Source: orderFXSourceExactSessionQuote}})
	if usd := got.Currencies["USD"]; got.Reason != "" || usd.FXRate != .96 || usd.FXSource != "" || !usd.FXObservedAt.IsZero() {
		t.Fatalf("capture=%+v", got)
	}
}

// The account is read every few seconds: only currencies without a usable rate
// take a request, at most one per pair per retry interval, and a failed attempt
// keeps the last quote rather than discarding it.
func TestOperationalReconFXQuotesAreBoundedAndOnlyForMissingRates(t *testing.T) {
	now := time.Date(2026, 11, 6, 23, 59, 0, 0, time.UTC)
	raw := &ibkrlib.RawAccountSummary{
		BaseCurrency: "EUR", BaseCurrencyProvenance: ibkrlib.AccountBaseCurrencyExplicitTag,
		CurrencyLedger: map[string]ibkrlib.CurrencyLedger{
			"EUR": {ExchangeRate: 1, ExchangeRateObserved: true},
			"USD": {ExchangeRate: 1, ExchangeRateObserved: true},
			"CHF": {ExchangeRate: 1.08, ExchangeRateObserved: true},
			"GBP": {},
		},
	}
	var asked []string
	failing := false
	fetch := func(_ context.Context, currency, base string) (reconFXQuote, error) {
		asked = append(asked, base+"."+currency)
		if failing {
			return reconFXQuote{}, errors.New("no quote")
		}
		return reconFXQuote{Rate: .9, At: now, Source: orderFXSourceExactSessionQuote}, nil
	}
	var cache reconFXQuoteCache
	got := cache.quotes(t.Context(), raw, now, fetch)
	slices.Sort(asked)
	if !slices.Equal(asked, []string{"EUR.GBP", "EUR.USD"}) || len(got) != 2 {
		t.Fatalf("asked=%v got=%v", asked, got)
	}
	asked = nil
	if got = cache.quotes(t.Context(), raw, now.Add(reconFXQuoteRetry-time.Second), fetch); len(asked) != 0 || len(got) != 2 {
		t.Fatalf("a read inside the interval took a request: asked=%v got=%v", asked, got)
	}
	failing = true
	if got = cache.quotes(t.Context(), raw, now.Add(reconFXQuoteRetry), fetch); len(asked) != 2 || got["USD"].Rate != .9 {
		t.Fatalf("a failed attempt lost the held quote: asked=%v got=%v", asked, got)
	}

	unproven := *raw
	unproven.BaseCurrencyProvenance = ibkrlib.AccountBaseCurrencyUnknown
	asked = nil
	if got = cache.quotes(t.Context(), &unproven, now.Add(time.Hour), fetch); got != nil || len(asked) != 0 {
		t.Fatalf("an unproven base currency cannot name a pair: asked=%v got=%v", asked, got)
	}
}
