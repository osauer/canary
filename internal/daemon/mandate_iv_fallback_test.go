package daemon

import (
	"math"
	"testing"

	"github.com/osauer/canary/v2/internal/mandate"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// An option row without a volatility is priced at the one its own mark
// implies, tagged so the evaluator can tell (owner, 2026-10-28 08:51 CET).
func TestMandateFallbackIVInvertsTheMark(t *testing.T) {
	const years, spot, strike = 0.25, 100.0, 105.0
	price := mandate.OptionPrice(true, spot, strike, years, 0.42)
	leg := mandate.Leg{Kind: mandate.KindOption, Right: "C", Spot: spot, Strike: strike, TimeToExpiryYears: years}
	row := rpc.PositionView{Mark: price, PriceAt: mandateTestNow}
	mandateFallbackIV(&leg, row)
	if leg.IV == nil || math.Abs(*leg.IV-0.42) > 1e-3 || leg.IVSource != mandate.IVSourceMark || !leg.IVAsOf.Equal(mandateTestNow) {
		t.Fatalf("leg = %+v, want IV 0.42 from the mark", leg)
	}

	live := 0.3
	held := mandate.Leg{Kind: mandate.KindOption, Right: "C", Spot: spot, Strike: strike, TimeToExpiryYears: years, IV: &live}
	mandateFallbackIV(&held, row)
	if *held.IV != 0.3 || held.IVSource != "" {
		t.Fatalf("a row's own volatility is kept: %+v", held)
	}

	intrinsic := mandate.Leg{Kind: mandate.KindOption, Right: "C", Spot: 120, Strike: strike, TimeToExpiryYears: years}
	mandateFallbackIV(&intrinsic, rpc.PositionView{Mark: 15})
	if intrinsic.IV != nil {
		t.Fatalf("a mark at intrinsic implies no volatility: %+v", intrinsic)
	}
}

// The fallback binds only once every key of [mandate.iv_fallback] is
// written; a half-written table keeps it off.
func TestMandatePolicyNumbersTakeOnlyAWholeFallbackTable(t *testing.T) {
	c := testMandateConstitution()
	if mandatePolicyNumbers(c).IVFallback != nil {
		t.Fatal("no table, no fallback")
	}
	c.Mandate.IVFallback = &risk.ConstitutionMandateIVFallback{ShortBandPct: new(65.0), MonthBandPct: new(15.0), LongBandPct: new(8.0), MeasuredPct: new(5.0)}
	if mandatePolicyNumbers(c).IVFallback != nil {
		t.Fatal("a half-written table must keep the fallback off")
	}
	c.Mandate.IVFallback.RangePct = new(20.0)
	got := mandatePolicyNumbers(c).IVFallback
	if got == nil || *got != (mandate.IVFallback{ShortBandPct: 65, MonthBandPct: 15, LongBandPct: 8, MeasuredPct: 5, RangePct: 20}) {
		t.Fatalf("fallback = %+v", got)
	}
}
