package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// Rule 1 values every held share of a symbol: the flat stock rows are summed
// at a share-weighted mark with their common FX rate, and rows whose rates
// disagree leave the line unmeasured rather than guessed. Option gamma rides
// along for rule 16.
func TestMapRuleNamesSumsStockRowsForTheIssuerMeasure(t *testing.T) {
	gamma := 0.02
	a1 := rpc.PositionView{Symbol: "AAA", SecType: "STK", ConID: 1, Currency: "USD", Quantity: 100, Mark: 50, Multiplier: 1, FXRate: new(0.9)}
	a2 := rpc.PositionView{Symbol: "AAA", SecType: "STK", ConID: 2, Currency: "USD", Quantity: 50, Mark: 56, Multiplier: 1, FXRate: new(0.9)}
	b1 := rpc.PositionView{Symbol: "BBB", SecType: "STK", ConID: 3, Currency: "USD", Quantity: 10, Mark: 20, Multiplier: 1, FXRate: new(0.9)}
	b2 := rpc.PositionView{Symbol: "BBB", SecType: "STK", ConID: 4, Currency: "CAD", Quantity: 10, Mark: 20, Multiplier: 1, FXRate: new(0.7)}
	opt := rpc.PositionView{Symbol: "AAA", SecType: "OPT", ConID: 5, Currency: "USD", Quantity: 1, Multiplier: 100, Mark: 2, Right: "C", Strike: 60, Expiry: "20261218", Gamma: &gamma}
	pos := &rpc.PositionsResult{
		Stocks:  []rpc.PositionView{a1, a2, b1, b2},
		Options: []rpc.PositionView{opt},
		ByUnderlying: []rpc.PositionGroup{
			{Underlying: "AAA", Stock: &a2, Options: []rpc.PositionView{opt}},
			{Underlying: "BBB", Stock: &b2},
		},
	}
	names := mapRuleNames(pos, risk.DefaultRulebookPolicy(), "EUR")
	if len(names) != 2 {
		t.Fatalf("names = %+v", names)
	}
	aaa, bbb := names[0], names[1]
	if aaa.StockQuantity != 150 || aaa.StockMark != 52 || aaa.StockFXToBase == nil || *aaa.StockFXToBase != 0.9 {
		t.Fatalf("AAA line = qty %v mark %v fx %v, want 150 at 52 × 0.9", aaa.StockQuantity, aaa.StockMark, aaa.StockFXToBase)
	}
	if len(aaa.Legs) != 1 || aaa.Legs[0].Gamma == nil || *aaa.Legs[0].Gamma != gamma {
		t.Fatalf("option gamma not carried: %+v", aaa.Legs)
	}
	if bbb.StockQuantity != 20 || bbb.StockFXToBase != nil {
		t.Fatalf("BBB rows in two currencies must leave the line unmeasured: qty %v fx %v", bbb.StockQuantity, bbb.StockFXToBase)
	}
}

func liquidityTestServer() *Server {
	return &Server{quoteLiquidity: newQuoteLiquidityCache(), quoteHistory: newQuoteHistoryCache()}
}

// The 20-day average volume comes from the daemon's caches only: the quote
// path's liquidity entry, else its daily bars without a session still in
// progress. A miss is unavailable, never zero, and never a broker read.
func TestRulebookADV20ReadsCachesNeverTheBroker(t *testing.T) {
	now := time.Date(2026, 9, 25, 15, 0, 0, 0, time.UTC) // Friday, 11:00 ET, session open
	s := liquidityTestServer()
	if _, ok := s.rulebookADV20("AAA", "", "USD", now); ok {
		t.Fatal("an empty cache produced a volume")
	}
	key, _, _ := rulebookLiquidityKey("AAA", "", "USD")
	s.quoteLiquidity.put(key, quoteLiquidityEntry{status: "ok", sampleDays: 20, avgVolume: 123456}, now)
	if adv, ok := s.rulebookADV20("AAA", "", "USD", now); !ok || adv != 123456 {
		t.Fatalf("liquidity entry = %v %v", adv, ok)
	}
	partial, _, _ := rulebookLiquidityKey("PPP", "", "USD")
	s.quoteLiquidity.put(partial, quoteLiquidityEntry{status: "partial", sampleDays: 12, avgVolume: 5}, now)
	if _, ok := s.rulebookADV20("PPP", "", "USD", now); ok {
		t.Fatal("a partial sample served as a 20-day average")
	}
	loc, _ := time.LoadLocation("America/New_York")
	var bars []ibkrlib.HistoricalBar
	day := time.Date(2026, 8, 17, 0, 0, 0, 0, loc)
	for len(bars) < 25 {
		if day.Weekday() != time.Saturday && day.Weekday() != time.Sunday {
			bars = append(bars, ibkrlib.HistoricalBar{Time: day, Close: 10, Volume: 1000})
		}
		day = day.AddDate(0, 0, 1)
	}
	bars = append(bars, ibkrlib.HistoricalBar{Time: time.Date(2026, 9, 25, 0, 0, 0, 0, loc), Close: 10, Volume: 10}) // today, in progress
	bbb, _, _ := rulebookLiquidityKey("BBB", "", "USD")
	s.quoteHistory.put(bbb, quoteHistoryEntry{bars: bars, fetched: now, until: now.Add(time.Hour)}, now)
	if adv, ok := s.rulebookADV20("BBB", "", "USD", now); !ok || adv != 1000 {
		t.Fatalf("history bars = %v %v, want 1000 with today's partial bar dropped", adv, ok)
	}
}

// Broad indices trade no shares and are skipped; a name without volume stays
// unavailable so rule 1 keeps its normal bands and says so.
func TestAttachRulebookLiquidityLeavesGapsUnknown(t *testing.T) {
	now := time.Date(2026, 9, 25, 15, 0, 0, 0, time.UTC)
	s := liquidityTestServer()
	key, _, _ := rulebookLiquidityKey("AAA", "", "USD")
	s.quoteLiquidity.put(key, quoteLiquidityEntry{status: "ok", sampleDays: 20, avgVolume: 900}, now)
	pos := &rpc.PositionsResult{Stocks: []rpc.PositionView{{Symbol: "AAA", SecType: "STK", Currency: "USD", Quantity: 1}}}
	names := []risk.NameInput{{Symbol: "AAA"}, {Symbol: "BBB"}, {Symbol: "SPX"}}
	s.attachRulebookLiquidity(context.Background(), names, pos, now, false)
	if names[0].AvgDailyVolume == nil || *names[0].AvgDailyVolume != 900 || names[1].AvgDailyVolume != nil || names[2].AvgDailyVolume != nil {
		t.Fatalf("volumes = %v %v %v", names[0].AvgDailyVolume, names[1].AvgDailyVolume, names[2].AvgDailyVolume)
	}
}

// Rule 18 names what it needs: no constitution, a missing number, a currency
// mismatch or no equity observation each say so; only a complete input
// carries effective risk capital.
func TestRulebookRiskCapitalNamesTheMissingNumber(t *testing.T) {
	if got := (&Server{}).rulebookRiskCapital(nil, nil, "EUR", time.Now()); got.EffectiveBase != nil || !strings.Contains(got.Missing, "risk-policy.toml") {
		t.Fatalf("no constitution manager = %+v", got)
	}
	effective := 20000.0
	report := func() *rpc.CapitalStateReport { return &rpc.CapitalStateReport{EffectiveRiskCapitalBase: &effective} }
	if got := rulebookRiskCapitalFrom(nil, "EUR", report); !strings.Contains(got.Missing, "no constitution is loaded") {
		t.Fatalf("absent constitution = %+v", got)
	}
	c := approvedTestConstitution()
	c.Capital.DeclaredRiskCapital = nil
	if got := rulebookRiskCapitalFrom(c, "EUR", report); got.EffectiveBase != nil || got.Missing != "capital.declared_risk_capital in risk-policy.toml" {
		t.Fatalf("missing declared capital = %+v", got)
	}
	c = approvedTestConstitution()
	if got := rulebookRiskCapitalFrom(c, "USD", report); !strings.Contains(got.Missing, "declares EUR") {
		t.Fatalf("currency mismatch = %+v", got)
	}
	if got := rulebookRiskCapitalFrom(c, "EUR", func() *rpc.CapitalStateReport { return &rpc.CapitalStateReport{} }); !strings.Contains(got.Missing, "equity observation") {
		t.Fatalf("no equity observation = %+v", got)
	}
	if got := rulebookRiskCapitalFrom(c, "EUR", report); got.EffectiveBase == nil || *got.EffectiveBase != 20000 {
		t.Fatalf("complete input = %+v", got)
	}
}
