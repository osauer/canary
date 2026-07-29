package daemon

import (
	"math"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

// TestBuildPortfolioAggregatesEmpty covers the degenerate case: no
// positions → always non-nil result, all aggregates nil.
func TestBuildPortfolioAggregatesEmpty(t *testing.T) {
	got := buildPortfolioAggregates(nil, nil)
	if got == nil {
		t.Fatalf("buildPortfolioAggregates: got nil result; renderer relies on non-nil")
	}
	if got.EffectiveDelta != nil || got.DailyTheta != nil || got.Gamma != nil || got.Vega != nil {
		t.Errorf("expected all nil aggregates on empty input, got %+v", got)
	}
}

// TestBuildPortfolioAggregatesStocksOnly: a pure stock book should have
// EffectiveDelta = sum of shares and DollarDelta = sum(qty × mark).
func TestBuildPortfolioAggregatesStocksOnly(t *testing.T) {
	stocks := []rpc.PositionView{
		{Symbol: "AAPL", SecType: "STK", Quantity: 100, Mark: 200, Currency: "USD"},
		{Symbol: "MSFT", SecType: "STK", Quantity: 50, Mark: 400, Currency: "USD"},
		{Symbol: "GME", SecType: "STK", Quantity: -10, Mark: 30, Currency: "USD"}, // short
	}
	got := buildPortfolioAggregates(stocks, nil)
	if got.EffectiveDelta == nil || math.Abs(*got.EffectiveDelta-140) > 1e-9 {
		t.Errorf("EffectiveDelta = %v, want 140 (100+50-10)", got.EffectiveDelta)
	}
	// 100*200 + 50*400 - 10*30 = 20000 + 20000 - 300 = 39700
	if got.DollarDelta == nil || math.Abs(*got.DollarDelta-39700) > 1e-9 {
		t.Errorf("DollarDelta = %v, want 39700", got.DollarDelta)
	}
	if got.DollarDeltaCurrency != "USD" {
		t.Errorf("DollarDeltaCurrency = %q, want USD", got.DollarDeltaCurrency)
	}
	if got.DailyTheta != nil {
		t.Errorf("DailyTheta should be nil for stocks-only book, got %v", got.DailyTheta)
	}
}

// TestBuildPortfolioAggregatesOptionsSumGreeks: option legs roll up
// delta/theta/gamma/vega by qty × multiplier (100 default).
func TestBuildPortfolioAggregatesOptionsSumGreeks(t *testing.T) {
	options := []rpc.PositionView{
		// Long 5 calls, delta=0.5, theta=-0.10. Underlying 200 (PrevClose anchor).
		{Symbol: "AAPL", SecType: "OPT", Quantity: 5, Currency: "USD",
			Delta: new(0.5), Theta: new(-0.10), Gamma: new(0.02), Vega: new(float64(15)),
			PrevClose: new(float64(200))},
		// Short 2 puts, delta=-0.3 → qty=-2 → contributes -2 * -0.3 * 100 = +60
		{Symbol: "AAPL", SecType: "OPT", Quantity: -2, Currency: "USD",
			Delta: new(-0.3), Theta: new(-0.05), Gamma: new(0.015), Vega: new(float64(10)),
			PrevClose: new(float64(200))},
	}
	got := buildPortfolioAggregates(nil, options)

	// 5 * 0.5 * 100 + (-2) * (-0.3) * 100 = 250 + 60 = 310
	if got.EffectiveDelta == nil || math.Abs(*got.EffectiveDelta-310) > 1e-9 {
		t.Errorf("EffectiveDelta = %v, want 310", got.EffectiveDelta)
	}
	// Dollar delta = effective_delta * underlying_spot (uniform per leg): 310 * 200 = 62000
	if got.DollarDelta == nil || math.Abs(*got.DollarDelta-62000) > 1e-9 {
		t.Errorf("DollarDelta = %v, want 62000", got.DollarDelta)
	}
	// Daily theta: 5 * -0.10 * 100 + (-2) * -0.05 * 100 = -50 + 10 = -40
	if got.DailyTheta == nil || math.Abs(*got.DailyTheta-(-40)) > 1e-9 {
		t.Errorf("DailyTheta = %v, want -40", got.DailyTheta)
	}
	// Gamma: 5 * 0.02 * 100 + (-2) * 0.015 * 100 = 10 + -3 = 7
	if got.Gamma == nil || math.Abs(*got.Gamma-7) > 1e-9 {
		t.Errorf("Gamma = %v, want 7", got.Gamma)
	}
	// Vega: 5 * 15 * 100 + (-2) * 10 * 100 = 7500 + -2000 = 5500
	if got.Vega == nil || math.Abs(*got.Vega-5500) > 1e-9 {
		t.Errorf("Vega = %v, want 5500", got.Vega)
	}
	if got.GreeksCoverage != 2 || got.GreeksTotal != 2 {
		t.Errorf("coverage = %d/%d, want 2/2", got.GreeksCoverage, got.GreeksTotal)
	}
}

// TestBuildPortfolioAggregatesHonorsMultiplierFromWire pins the v0.13.x
// fix: optionMultiplier reads PositionView.Multiplier instead of
// hard-coding 100. Without this the aggregator silently lies by an
// integer factor on index options (NDX 100, NDXP 100, mini-options 10,
// some indexes 1000). The wire already carries the value via
// pos.Asset.Multiplier; ignoring it was a self-deceptive helper that
// took the answer and threw it away.
func TestBuildPortfolioAggregatesHonorsMultiplierFromWire(t *testing.T) {
	// Index-style option: Multiplier=1000. Without honouring the wire
	// value the aggregator would compute 1*0.5*100 = 50 (10× too low).
	options := []rpc.PositionView{
		{Symbol: "NDX", SecType: rpc.SecTypeOption, Quantity: 1, Currency: "USD",
			Multiplier: 1000,
			Delta:      new(0.5), Theta: new(-0.20),
			Underlying: new(float64(20000))},
	}
	got := buildPortfolioAggregates(nil, options)
	// 1 * 0.5 * 1000 = 500
	if got.EffectiveDelta == nil || math.Abs(*got.EffectiveDelta-500) > 1e-9 {
		t.Errorf("EffectiveDelta = %v, want 500 (mult=1000 honoured)", got.EffectiveDelta)
	}
	// 1 * 0.5 * 1000 * 20000 = 10_000_000
	if got.DollarDelta == nil || math.Abs(*got.DollarDelta-10_000_000) > 1e-9 {
		t.Errorf("DollarDelta = %v, want 10_000_000 (mult=1000 honoured)", got.DollarDelta)
	}
	// 1 * -0.20 * 1000 = -200
	if got.DailyTheta == nil || math.Abs(*got.DailyTheta-(-200)) > 1e-9 {
		t.Errorf("DailyTheta = %v, want -200 (mult=1000 honoured)", got.DailyTheta)
	}
}

// TestBuildPortfolioAggregatesPrefersUnderlyingOverPrevClose pins the
// v0.13.x fix: dollar delta now uses the model-computation underlying
// captured alongside the Greeks (lockstep with the delta), and only
// falls back to PrevClose when the leg's Greeks tick didn't carry a
// spot. Pairing today's delta with the underlying's prior close gives
// an apples-to-oranges answer that lies by the size of any overnight
// gap — a 3% gap was a 3% lie in dollar_delta.
func TestBuildPortfolioAggregatesPrefersUnderlyingOverPrevClose(t *testing.T) {
	t.Run("Underlying takes precedence over PrevClose", func(t *testing.T) {
		// Yesterday's close 200, today's spot 206 (3% gap up).
		options := []rpc.PositionView{
			{Symbol: "AAPL", SecType: rpc.SecTypeOption, Quantity: 5, Currency: "USD",
				Delta:      new(0.5),
				Underlying: new(float64(206)),
				PrevClose:  new(float64(200))},
		}
		got := buildPortfolioAggregates(nil, options)
		// 5 * 0.5 * 100 * 206 = 51_500. With the bug it would have been 50_000.
		if got.DollarDelta == nil || math.Abs(*got.DollarDelta-51_500) > 1e-9 {
			t.Errorf("DollarDelta = %v, want 51500 (Underlying preferred)", got.DollarDelta)
		}
	})

	t.Run("Underlying alone (no PrevClose) populates dollar delta", func(t *testing.T) {
		options := []rpc.PositionView{
			{Symbol: "AAPL", SecType: rpc.SecTypeOption, Quantity: 1, Currency: "USD",
				Delta:      new(0.5),
				Underlying: new(float64(206))},
		}
		got := buildPortfolioAggregates(nil, options)
		// 1 * 0.5 * 100 * 206 = 10300
		if got.DollarDelta == nil || math.Abs(*got.DollarDelta-10_300) > 1e-9 {
			t.Errorf("DollarDelta = %v, want 10300 (Underlying-only path)", got.DollarDelta)
		}
	})

	t.Run("PrevClose still serves as fallback when Underlying missing", func(t *testing.T) {
		// Mirrors the cold-start case: per-leg Greeks tick captured
		// delta but no model-spot — still better to anchor on prev close
		// than to drop the dollar contribution.
		options := []rpc.PositionView{
			{Symbol: "AAPL", SecType: rpc.SecTypeOption, Quantity: 1, Currency: "USD",
				Delta:     new(0.5),
				PrevClose: new(float64(200))},
		}
		got := buildPortfolioAggregates(nil, options)
		// 1 * 0.5 * 100 * 200 = 10000
		if got.DollarDelta == nil || math.Abs(*got.DollarDelta-10_000) > 1e-9 {
			t.Errorf("DollarDelta = %v, want 10000 (PrevClose fallback)", got.DollarDelta)
		}
	})
}

// TestBuildPortfolioAggregatesPartialCoverage: some legs have Greeks,
// some don't. Coverage counts must reflect that; sums aggregate only
// what's present.
func TestBuildPortfolioAggregatesPartialCoverage(t *testing.T) {
	options := []rpc.PositionView{
		{Symbol: "AAPL", SecType: "OPT", Quantity: 1, Currency: "USD",
			Delta: new(0.5), Theta: new(-0.10)}, // priced
		{Symbol: "TSLA", SecType: "OPT", Quantity: 2, Currency: "USD"}, // no Greeks
		{Symbol: "MSFT", SecType: "OPT", Quantity: 1, Currency: "USD",
			Delta: new(0.4)}, // partial: only delta
	}
	got := buildPortfolioAggregates(nil, options)

	// EffectiveDelta: 1*0.5*100 + 1*0.4*100 = 90
	if got.EffectiveDelta == nil || math.Abs(*got.EffectiveDelta-90) > 1e-9 {
		t.Errorf("EffectiveDelta = %v, want 90", got.EffectiveDelta)
	}
	// DailyTheta: only first leg contributes: 1 * -0.10 * 100 = -10
	if got.DailyTheta == nil || math.Abs(*got.DailyTheta-(-10)) > 1e-9 {
		t.Errorf("DailyTheta = %v, want -10", got.DailyTheta)
	}
	if got.GreeksCoverage != 2 || got.GreeksTotal != 3 {
		t.Errorf("coverage = %d/%d, want 2/3", got.GreeksCoverage, got.GreeksTotal)
	}
}

// TestBuildPortfolioAggregatesExcludesZombieStocks verifies the
// delisted-zombie filter (v0.12.1): a held delisted ticker that IBKR
// still streams via msgPortfolioValue with mark=0 must NOT contribute
// to effective_delta. Without this filter, the first positions call
// after daemon start inflates effective_delta by the zombie's full share
// count, then drops on the second call when the inactive flag kicks in —
// the aggregate would jump 5× between back-to-back invocations.
func TestBuildPortfolioAggregatesExcludesZombieStocks(t *testing.T) {
	stocks := []rpc.PositionView{
		{Symbol: "AAPL", SecType: "STK", Quantity: 100, Mark: 200, Currency: "USD"},
		// Zombie: held but no live quote. Mark=0 is the gateway's signal.
		{Symbol: "ZVZZT", SecType: "STK", Quantity: 20000, Mark: 0, Currency: "USD"},
	}
	got := buildPortfolioAggregates(stocks, nil)
	if got.EffectiveDelta == nil || math.Abs(*got.EffectiveDelta-100) > 1e-9 {
		t.Errorf("EffectiveDelta = %v, want 100 (zombie excluded; not 20100)", got.EffectiveDelta)
	}
}

func TestZeroValueStockPositionSkipsQuotePrewarmButStaysVisible(t *testing.T) {
	stocks := []rpc.PositionView{
		{Symbol: "ZVZZT", SecType: "STK", Quantity: 20000, Mark: 0, ValuationMark: 0, MarketValue: 0, Currency: "USD"},
	}
	if shouldPrewarmStockQuote(stocks[0]) {
		t.Fatal("zero-value stock position should not trigger quote prewarm")
	}
	flagZeroValueStockPositions(stocks)
	if len(stocks) != 1 || stocks[0].Symbol != "ZVZZT" {
		t.Fatalf("zero-value stock position should remain visible, got %+v", stocks)
	}
	if !stocks[0].Stale || stocks[0].StaleReason == "" {
		t.Fatalf("zero-value stock should be marked stale with reason: %+v", stocks[0])
	}
	if !positionWarningHasCode(stocks[0].WarningDetails, "zero_value_stock_position") {
		t.Fatalf("zero-value warning missing: %+v", stocks[0].WarningDetails)
	}
}

// TestBuildPortfolioAggregatesMixedCurrencyDollarDelta: a position book
// with both EUR and USD underlyings must flag DollarDeltaCurrency as
// "MIX" so callers don't apply one FX rate.
func TestBuildPortfolioAggregatesMixedCurrencyDollarDelta(t *testing.T) {
	stocks := []rpc.PositionView{
		{Symbol: "AAPL", SecType: "STK", Quantity: 100, Mark: 200, Currency: "USD"},
		{Symbol: "SAP", SecType: "STK", Quantity: 50, Mark: 150, Currency: "EUR"},
	}
	got := buildPortfolioAggregates(stocks, nil)
	if got.DollarDeltaCurrency != "MIX" {
		t.Errorf("DollarDeltaCurrency = %q, want MIX", got.DollarDeltaCurrency)
	}
}

// TestBuildPortfolioAggregatesDailyThetaCurrency: theta is summed in the
// option leg's contract currency. Single-currency book → DailyThetaCurrency
// echoes that currency; mixed-currency book → "MIX" so a renderer knows
// not to stamp a single symbol on a sum that's apples-and-oranges.
func TestBuildPortfolioAggregatesDailyThetaCurrency(t *testing.T) {
	t.Run("single-currency book carries the leg currency", func(t *testing.T) {
		options := []rpc.PositionView{
			{Symbol: "AAPL", SecType: rpc.SecTypeOption, Quantity: 1, Currency: "USD", Theta: new(-0.10)},
			{Symbol: "TSLA", SecType: rpc.SecTypeOption, Quantity: 2, Currency: "USD", Theta: new(-0.05)},
		}
		got := buildPortfolioAggregates(nil, options)
		if got.DailyThetaCurrency != "USD" {
			t.Errorf("DailyThetaCurrency = %q, want USD", got.DailyThetaCurrency)
		}
	})
	t.Run("EUR-only book carries EUR", func(t *testing.T) {
		options := []rpc.PositionView{
			{Symbol: "SAP", SecType: rpc.SecTypeOption, Quantity: 3, Currency: "EUR", Theta: new(-0.08)},
		}
		got := buildPortfolioAggregates(nil, options)
		if got.DailyThetaCurrency != "EUR" {
			t.Errorf("DailyThetaCurrency = %q, want EUR", got.DailyThetaCurrency)
		}
	})
	t.Run("mixed-currency theta-bearing legs → MIX", func(t *testing.T) {
		options := []rpc.PositionView{
			{Symbol: "AAPL", SecType: rpc.SecTypeOption, Quantity: 1, Currency: "USD", Theta: new(-0.10)},
			{Symbol: "SAP", SecType: rpc.SecTypeOption, Quantity: 2, Currency: "EUR", Theta: new(-0.05)},
		}
		got := buildPortfolioAggregates(nil, options)
		if got.DailyThetaCurrency != "MIX" {
			t.Errorf("DailyThetaCurrency = %q, want MIX", got.DailyThetaCurrency)
		}
	})
	t.Run("theta-currency tracking is independent of dollar-delta-currency tracking", func(t *testing.T) {
		// First leg contributes only to delta (no theta tick), second
		// only to theta (no delta tick). The two aggregates' currency
		// fields must be independent.
		options := []rpc.PositionView{
			{Symbol: "AAPL", SecType: rpc.SecTypeOption, Quantity: 1, Currency: "USD",
				Delta: new(0.5), PrevClose: new(float64(200))}, // delta-only, USD
			{Symbol: "SAP", SecType: rpc.SecTypeOption, Quantity: 2, Currency: "EUR",
				Theta: new(-0.05)}, // theta-only, EUR
		}
		got := buildPortfolioAggregates(nil, options)
		if got.DollarDeltaCurrency != "USD" {
			t.Errorf("DollarDeltaCurrency = %q, want USD (only USD leg has spot/delta)", got.DollarDeltaCurrency)
		}
		if got.DailyThetaCurrency != "EUR" {
			t.Errorf("DailyThetaCurrency = %q, want EUR (only EUR leg has theta)", got.DailyThetaCurrency)
		}
	})
	t.Run("no theta-bearing legs → empty currency", func(t *testing.T) {
		options := []rpc.PositionView{
			{Symbol: "AAPL", SecType: rpc.SecTypeOption, Quantity: 1, Currency: "USD"},
		}
		got := buildPortfolioAggregates(nil, options)
		if got.DailyTheta != nil {
			t.Errorf("DailyTheta should be nil with no theta-bearing legs, got %v", got.DailyTheta)
		}
		if got.DailyThetaCurrency != "" {
			t.Errorf("DailyThetaCurrency = %q, want empty (no theta data)", got.DailyThetaCurrency)
		}
	})
}

// TestOptionGreeksKey verifies the cache key matches the format
// produced by Connector.SubscribeOption — drift between the two means
// every cached entry is a miss.
func TestOptionGreeksKey(t *testing.T) {
	cases := []struct {
		name string
		in   rpc.PositionView
		want string
	}{
		{
			name: "standard option (wire SecType OPT)",
			in:   rpc.PositionView{SecType: "OPT", Symbol: "AAPL", Expiry: "20260117", Strike: 200, Right: "C"},
			want: "AAPL_260117C200",
		},
		{
			// pkg/ibkr.convertIBKRPositions stamps PositionView.SecType
			// from the AssetType enum, whose option value is the long
			// form "OPTION". The v0.10.0 release accidentally rejected
			// this and never subscribed to any option's Greeks.
			name: "domain SecType OPTION accepted",
			in:   rpc.PositionView{SecType: "OPTION", Symbol: "AAPL", Expiry: "20260117", Strike: 200, Right: "C"},
			want: "AAPL_260117C200",
		},
		{
			name: "lowercase right normalized",
			// %.0f rounds half-to-even (250.5 → 250). This matches the
			// format Connector.SubscribeOption produces — drift between
			// the two would make every lookup a cache miss.
			in:   rpc.PositionView{SecType: "OPT", Symbol: "tsla", Expiry: "20250620", Strike: 250.5, Right: "p"},
			want: "TSLA_250620P250",
		},
		{
			name: "non-option returns empty",
			in:   rpc.PositionView{SecType: "STK", Symbol: "AAPL"},
			want: "",
		},
		{
			name: "missing expiry returns empty",
			in:   rpc.PositionView{SecType: "OPT", Symbol: "AAPL", Strike: 200, Right: "C"},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := optionGreeksKey(tc.in)
			if got != tc.want {
				t.Errorf("optionGreeksKey = %q, want %q", got, tc.want)
			}
		})
	}
}
