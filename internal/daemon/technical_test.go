package daemon

import (
	"math"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestBuildTechnicalRowComputesTrendRSATRAndLiquidity(t *testing.T) {
	t.Parallel()
	bars := makeTechnicalBars(220, 100, 1, 1_000_000)
	bench63 := 0.05
	bench126 := 0.10

	row := buildTechnicalRow("TEST", bars, &bench63, &bench126, time.Now())

	if row.Price == nil || *row.Price != 319 {
		t.Fatalf("price = %v, want 319", row.Price)
	}
	if row.SMA50 == nil || math.Abs(*row.SMA50-294.5) > 1e-9 {
		t.Fatalf("SMA50 = %v, want 294.5", row.SMA50)
	}
	if row.SMA200 == nil || math.Abs(*row.SMA200-219.5) > 1e-9 {
		t.Fatalf("SMA200 = %v, want 219.5", row.SMA200)
	}
	if row.Return63D == nil || math.Abs(*row.Return63D-(319.0/256.0-1)) > 1e-9 {
		t.Fatalf("return63 = %v", row.Return63D)
	}
	if row.RS63D == nil || math.Abs(*row.RS63D-((319.0/256.0-1)-bench63)) > 1e-9 {
		t.Fatalf("RS63 = %v", row.RS63D)
	}
	if row.ATR14 == nil || *row.ATR14 != 2 {
		t.Fatalf("ATR14 = %v, want 2", row.ATR14)
	}
	if row.AvgVolume20D == nil || *row.AvgVolume20D != 1_000_000 {
		t.Fatalf("avg volume = %v, want 1,000,000", row.AvgVolume20D)
	}
	if row.AvgDollarVolume20D == nil || math.Abs(*row.AvgDollarVolume20D-309_500_000) > 1e-6 {
		t.Fatalf("avg dollar volume = %v, want 309,500,000", row.AvgDollarVolume20D)
	}
	if row.TrendState != "extended" || row.DataQuality != "ok" {
		t.Fatalf("trend/quality = %s/%s, want extended/ok; missing=%v", row.TrendState, row.DataQuality, row.MissingReasons)
	}
}

func TestBuildTechnicalRowMarksInsufficientHistory(t *testing.T) {
	t.Parallel()
	row := buildTechnicalRow("NEW", makeTechnicalBars(30, 10, 0.5, 50_000), nil, nil, time.Now())
	if row.DataQuality != "insufficient_data" {
		t.Fatalf("quality = %q, want insufficient_data", row.DataQuality)
	}
	if row.TrendState != "insufficient_data" {
		t.Fatalf("trend = %q, want insufficient_data", row.TrendState)
	}
	if len(row.MissingReasons) == 0 {
		t.Fatalf("expected missing reasons")
	}
}

func TestComputeHistoricalLiquidity20DPartialSample(t *testing.T) {
	t.Parallel()
	bars := makeTechnicalBars(10, 20, 1, 100)
	liq := computeHistoricalLiquidity20D(bars)
	if liq.sampleDays != 10 {
		t.Fatalf("sample days = %d, want 10", liq.sampleDays)
	}
	if liq.avgVolume == nil || *liq.avgVolume != 100 {
		t.Fatalf("avg volume = %v, want 100", liq.avgVolume)
	}
	if liq.avgDollarVolume == nil || math.Abs(*liq.avgDollarVolume-2450) > 1e-9 {
		t.Fatalf("avg dollar = %v, want 2450", liq.avgDollarVolume)
	}
}

func TestTechnicalRouteSupportsXetraMarket(t *testing.T) {
	t.Parallel()
	route, routed, err := technicalRoute(rpc.TechnicalParams{Market: "de"})
	if err != nil {
		t.Fatalf("technicalRoute returned error: %v", err)
	}
	if !routed {
		t.Fatal("technicalRoute did not mark market=de as routed")
	}
	if route.Market != "de" {
		t.Fatalf("Market = %q, want de", route.Market)
	}
	if route.Exchange != "SMART" || route.PrimaryExch != "IBIS" || route.Currency != "EUR" {
		t.Fatalf("route = %+v, want SMART/IBIS/EUR", route)
	}
}

func makeTechnicalBars(n int, start, step float64, volume int64) []ibkrlib.HistoricalBar {
	out := make([]ibkrlib.HistoricalBar, n)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range n {
		closePx := start + float64(i)*step
		out[i] = ibkrlib.HistoricalBar{
			Date:   base.AddDate(0, 0, i).Format("20060102"),
			Time:   base.AddDate(0, 0, i),
			Open:   closePx - 0.5,
			High:   closePx + 1,
			Low:    closePx - 1,
			Close:  closePx,
			Volume: volume,
		}
	}
	return out
}
