package daemon

import (
	"math"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/flexstmt"
	"github.com/osauer/canary/v2/internal/rpc"
)

func fxTestSnapshot(day, previous string, eur, usd, rate float64) *flexstmt.FXSnapshot {
	return &flexstmt.FXSnapshot{Day: day, PreviousDay: previous, BaseCurrency: "EUR", SingleDay: true, Foreign: usd != 0, ClosingForeign: usd != 0, Book: map[string]float64{"EUR": eur, "USD": usd}, Rates: map[string]float64{"EUR": 1, "USD": rate}, NAV: eur + usd*rate, Conversion: map[string]float64{}, External: map[string]float64{}}
}

func TestFXAccountingWitnesses(t *testing.T) {
	tests := []struct {
		name                   string
		eur0, usd0, eur1, usd1 float64
		conversion, external   map[string]float64
		externalBase, want     float64
	}{
		{name: "buy foreign cash", eur0: 1000, eur1: 150, usd1: 1000, conversion: map[string]float64{"EUR": -850, "USD": 1000}, want: -50},
		{name: "sell foreign cash", usd0: 1000, eur1: 850, conversion: map[string]float64{"EUR": 850, "USD": -1000}, want: -50},
		{name: "flat intraday conversion", eur0: 1000, eur1: 950, conversion: map[string]float64{"EUR": -50}, want: -50},
		{name: "price interaction", usd0: 1000, usd1: 1100, want: -110},
		{name: "income closing convention", usd0: 1000, usd1: 1100, want: -110},
		{name: "foreign deposit at actual value", usd1: 1000, external: map[string]float64{"USD": 1000}, externalBase: 850, want: -50},
		{name: "base deposit", eur1: 1000, external: map[string]float64{"EUR": 1000}, externalBase: 1000, want: 0},
		{name: "foreign commission counted once", usd0: 1000, usd1: 990, want: -99},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start := fxTestSnapshot("2026-09-30", "2026-09-29", tt.eur0, tt.usd0, .9)
			end := fxTestSnapshot("2026-10-01", start.Day, tt.eur1, tt.usd1, .8)
			end.Conversion = tt.conversion
			end.External = tt.external
			end.ExternalBase = tt.externalBase
			if len(tt.conversion) > 0 {
				end.Foreign = true
			}
			got := attributeFX(end, start)
			if got.Contribution == nil || math.Abs(*got.Contribution-tt.want) > 1e-8 {
				t.Fatalf("FX %v reason %s, want %g", got.Contribution, got.Reason, tt.want)
			}
			if got.ReconciliationResidual == nil || math.Abs(*got.ReconciliationResidual) > 1e-8 {
				t.Fatal("NAV bridge failed")
			}
		})
	}
}

func TestFXBoundaryFailuresNeverZero(t *testing.T) {
	start := fxTestSnapshot("2026-09-30", "2026-09-29", 100, 1000, .9)
	end := fxTestSnapshot("2026-10-01", start.Day, 100, 1000, .8)
	t.Run("missing opening", func(t *testing.T) {
		if attributeFX(end, nil).Contribution != nil {
			t.Fatal("missing opening certified")
		}
	})
	t.Run("cash discontinuity", func(t *testing.T) {
		a, b := *start, *end
		a.CashEnd = map[string]float64{"USD": 2}
		b.CashStart = map[string]float64{"USD": 3}
		if attributeFX(&b, &a).Contribution != nil {
			t.Fatal("cash gap certified")
		}
	})
	t.Run("accrual discontinuity", func(t *testing.T) {
		a, b := *start, *end
		a.InterestEnd = map[string]float64{"USD": 2}
		b.InterestStart = map[string]float64{"USD": 3}
		if attributeFX(&b, &a).Contribution != nil {
			t.Fatal("accrual gap certified")
		}
	})
	t.Run("missing rate", func(t *testing.T) {
		a := *start
		a.Rates = map[string]float64{"EUR": 1}
		if attributeFX(end, &a).Contribution != nil {
			t.Fatal("missing rate certified")
		}
	})
	t.Run("NAV mismatch", func(t *testing.T) {
		a := *start
		a.NAV++
		if attributeFX(end, &a).Contribution != nil {
			t.Fatal("NAV mismatch certified")
		}
	})
}

func TestFXPeriodsAndHistoricalExposure(t *testing.T) {
	now := time.Date(2026, 1, 6, 12, 0, 0, 0, time.UTC)
	dates := []string{"2025-12-31", "2026-01-01", "2026-01-02", "2026-01-05"}
	rows := []flexstmt.Statement{}
	equity := []flexstmt.EquityRow{}
	for i, d := range dates {
		day, _ := time.Parse(performanceDayFormat, d)
		f := fxTestSnapshot(d, "2025-12-30", 1000, 0, .9)
		if i > 0 {
			f.PreviousDay = dates[i-1]
		}
		if i == 1 {
			f.Foreign = true
		} // Intraday foreign exposure, closing book flat.
		rows = append(rows, flexstmt.Statement{FromDate: day, ToDate: day, WhenGenerated: now, FX: f})
		equity = append(equity, flexstmt.EquityRow{ReportDate: day, TotalBase: 1000})
	}
	rows = append(rows, flexstmt.Statement{FromDate: time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC), ToDate: time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC), WhenGenerated: now, Equity: equity})
	r := buildFX(rows, now)
	if err := rpc.ValidateFXResult(r); err != nil {
		t.Fatal(err)
	}
	if r.Periods[0].State != "no_exposure" || r.Periods[3].State != "available" || r.Periods[3].Contribution == nil {
		t.Fatalf("historical exposure lost: %+v", r.Periods)
	}
	rows[2].FX = nil
	r = buildFX(rows, now)
	if r.Periods[3].Contribution != nil || r.Periods[3].State != "partial" || len(r.Periods[3].MissingDays) != 2 {
		t.Fatalf("gap certified: %+v", r.Periods[3])
	}
}
