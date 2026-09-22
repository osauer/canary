package daemon

import (
	"math"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkr "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestDisplayPnLCurrencyConversion(t *testing.T) {
	at := time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		change func(*ibkr.DisplaySnapshot, *fxRateCache)
		want   *float64
	}{
		{name: "base to native", want: new(100.0)},
		{name: "same currency needs no FX", change: func(s *ibkr.DisplaySnapshot, f *fxRateCache) {
			s.Positions[0].Contract.Currency = "EUR"
			clear(f.rates)
		}, want: new(90.0)},
		{name: "zero is present", change: func(s *ibkr.DisplaySnapshot, _ *fxRateCache) {
			p := s.PositionPnL[101]
			p.DailyPnL = new(0.0)
			s.PositionPnL[101] = p
		}, want: new(0.0)},
		{name: "loss keeps sign", change: func(s *ibkr.DisplaySnapshot, _ *fxRateCache) {
			p := s.PositionPnL[101]
			p.DailyPnL = new(-90.0)
			s.PositionPnL[101] = p
		}, want: new(-100.0)},
		{name: "missing PnL", change: func(s *ibkr.DisplaySnapshot, _ *fxRateCache) { clear(s.PositionPnL) }},
		{name: "missing receipt", change: func(s *ibkr.DisplaySnapshot, _ *fxRateCache) {
			p := s.PositionPnL[101]
			p.AsOf = time.Time{}
			s.PositionPnL[101] = p
		}},
		{name: "foreign PnL account", change: func(s *ibkr.DisplaySnapshot, _ *fxRateCache) { s.PnLAccount = "U_FOREIGN" }},
		{name: "foreign account currency", change: func(s *ibkr.DisplaySnapshot, _ *fxRateCache) { s.Account.AccountID = "U_FOREIGN" }},
		{name: "unproven base", change: func(s *ibkr.DisplaySnapshot, _ *fxRateCache) { s.Account.BaseCurrencyProvenance = "" }},
		{name: "missing native currency", change: func(s *ibkr.DisplaySnapshot, _ *fxRateCache) { s.Positions[0].Contract.Currency = "" }},
		{name: "missing FX", change: func(_ *ibkr.DisplaySnapshot, f *fxRateCache) { clear(f.rates) }},
		{name: "wrong currency pair", change: func(_ *ibkr.DisplaySnapshot, f *fxRateCache) { clear(f.rates); f.put("USD", "EUR", .9) }},
		{name: "stale FX", change: func(_ *ibkr.DisplaySnapshot, f *fxRateCache) {
			f.now = func() time.Time { return at.Add(fxCacheFreshWindow + time.Second) }
		}},
		{name: "future FX", change: func(_ *ibkr.DisplaySnapshot, f *fxRateCache) {
			f.now = func() time.Time { return at.Add(-time.Second) }
		}},
		{name: "invalid FX", change: func(_ *ibkr.DisplaySnapshot, f *fxRateCache) { f.put("EUR", "USD", math.NaN()) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			contract := ibkr.Contract{ConID: 101, Symbol: "SYNTH", SecType: "STK", Currency: "USD"}
			snapshot := ibkr.DisplaySnapshot{
				Account:     &ibkr.RawAccountSummary{AccountID: "U_SYNTHETIC", BaseCurrency: "EUR", BaseCurrencyProvenance: ibkr.AccountBaseCurrencyExplicitTag},
				PnLAccount:  "U_SYNTHETIC",
				Positions:   []*ibkr.RawPosition{{Account: "U_SYNTHETIC", Contract: contract, Position: 1, UnrealizedPNL: 12, ValuationAt: at.Add(-time.Minute)}},
				PositionPnL: map[int]ibkr.PositionDailyPnL{101: {DailyPnL: new(90.0), AsOf: at}},
			}
			cache := newFXRateCache()
			cache.now = func() time.Time { return at }
			cache.put("EUR", "USD", .9)
			if tc.change != nil {
				tc.change(&snapshot, cache)
			}
			s := &Server{fxRates: cache}
			scope := rpc.AccountDataScope{AccountID: "U_SYNTHETIC", AccountMode: "paper"}
			out := projectDisplay(snapshot, nil, scope, s.displayPnLRates(snapshot, scope))
			if len(out.Positions) != 1 {
				t.Fatalf("positions = %d", len(out.Positions))
			}
			row := out.Positions[0]
			if tc.want == nil {
				if row.DailyPnL != nil || !row.PnLAt.IsZero() {
					t.Fatal("unavailable P&L gained a value or receipt")
				}
			} else if row.DailyPnL == nil || math.Abs(*row.DailyPnL-*tc.want) > 1e-9 || row.PnLAt != at {
				t.Fatalf("native daily P&L or broker receipt lost: %+v", row)
			}
			if row.UnrealizedPnL == nil || *row.UnrealizedPnL != 12 || row.UnrealizedAt != at.Add(-time.Minute) {
				t.Fatal("conversion changed the independent native valuation")
			}
		})
	}
}

func TestPositionDailyPnLCurrencyConversion(t *testing.T) {
	for _, tc := range []struct {
		name, base, native                 string
		amount, rate, wantNative, wantBase *float64
	}{
		{name: "base to native", base: "EUR", native: "USD", amount: new(90.0), rate: new(.9), wantNative: new(100.0), wantBase: new(90.0)},
		{name: "same currency", base: "EUR", native: "EUR", amount: new(90.0), wantNative: new(90.0), wantBase: new(90.0)},
		{name: "zero", base: "EUR", native: "USD", amount: new(0.0), rate: new(.9), wantNative: new(0.0), wantBase: new(0.0)},
		{name: "missing FX keeps base", base: "EUR", native: "USD", amount: new(90.0), wantBase: new(90.0)},
		{name: "missing native keeps base", base: "EUR", amount: new(90.0), rate: new(.9), wantBase: new(90.0)},
		{name: "unknown base", native: "USD", amount: new(90.0), rate: new(.9)},
		{name: "unset amount", base: "EUR", native: "USD", rate: new(.9)},
		{name: "invalid amount", base: "EUR", native: "USD", amount: new(math.NaN()), rate: new(.9)},
		{name: "broker unset sentinel", base: "EUR", native: "USD", amount: new(math.MaxFloat64), rate: new(.9)},
		{name: "invalid FX", base: "EUR", native: "USD", amount: new(90.0), rate: new(math.Inf(1)), wantBase: new(90.0)},
		{name: "negative FX", base: "EUR", native: "USD", amount: new(90.0), rate: new(-.9), wantBase: new(90.0)},
		{name: "overflow", base: "EUR", native: "USD", amount: new(90.0), rate: new(math.SmallestNonzeroFloat64), wantBase: new(90.0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := []rpc.PositionView{{Currency: tc.native, FXRate: tc.rate}}
			fillPositionDailyPnL(&rows[0], tc.amount, tc.base)
			fillBaseValues(rows, tc.base)
			for name, pair := range map[string][2]*float64{"native": {rows[0].DailyPnL, tc.wantNative}, "base": {rows[0].DailyPnLBase, tc.wantBase}} {
				got, want := pair[0], pair[1]
				if want == nil {
					if got != nil {
						t.Fatalf("%s: unavailable value became %v", name, *got)
					}
				} else if got == nil || math.IsNaN(*got) || math.Abs(*got-*want) > 1e-9 {
					t.Fatalf("%s: got %v, want %v", name, got, *want)
				}
			}
		})
	}
}
