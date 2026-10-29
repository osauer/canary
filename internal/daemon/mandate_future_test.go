package daemon

import (
	"math"
	"slices"
	"testing"

	"github.com/osauer/canary/v2/internal/mandate"
	"github.com/osauer/canary/v2/internal/rpc"
)

// mandateReductionFuture is a held Micro S&P future in USD, two contracts
// short by default, as the positions spell it (security type FUTURE, the
// multiplier on the row, the month on the row, a base rate of 0.9, and the
// exchange only as the primary one: the portfolio stream carries no other).
func mandateReductionFuture(conID int, contracts, mark float64) rpc.PositionView {
	return rpc.PositionView{Symbol: "MES", SecType: rpc.SecTypeFuture, ConID: conID, PrimaryExch: "CME", Currency: "USD", LocalSymbol: "MESZ6", TradingClass: "MES", Expiry: "20261218",
		Quantity: contracts, Multiplier: 5, Mark: mark, FXRate: new(0.9), DataType: rpc.MarketDataLive}
}

// A held Micro S&P future is the mandate's own MES FUT line (owner decisions
// 2026-10-28 13:17 and 20:07 CET), no longer a security type the mandate does
// not model: 3,000 index shares lose 15,000 in a 10% fall, two short
// contracts at 6,000 × 5 × 0.9 gain 5,400, so the book loses 9,600 and
// gains 9,600 in the rise. The hedge's loss in that rise spends loss room
// (5,400 under index_hedge_base), and the line is never listed as unstopped.
func TestMandateHeldIndexFutureCountsInTheBook(t *testing.T) {
	pos := mandateReductionBook(mandateReductionStock("SPY", 9101, 3000, 50), mandateReductionFuture(750001, -2, 6000))
	legs, notModelled, fxNow, reasons := mandateLegsFromPositions(pos, "EUR", mandateTestNow)
	if len(reasons) > 0 || len(notModelled) > 0 || len(legs) != 2 {
		t.Fatalf("legs %d, not modelled %v, reasons %v; want two modelled legs", len(legs), notModelled, reasons)
	}
	fut := legs[1]
	if fut.ID != "con:750001" || fut.Symbol != "MESZ6" || fut.Underlying != mandate.MicroSP500Future || fut.Kind != mandate.KindStock ||
		fut.Quantity != -2 || fut.Multiplier != 5 || fut.Currency != "USD" || fut.Spot != 6000 || fxNow["USD"] != 0.9 || !mandate.IsIndexFuture(fut) {
		t.Fatalf("future leg = %+v (fx %v), want MESZ6 as the MES FUT line at ×5", fut, fxNow)
	}
	v := mandateReductionFixture(t, pos, 0.5, 1000000, 0).Result.Verdict
	if v.Status == mandate.StatusUnknown || v.Stresses == nil || v.Loss == nil {
		t.Fatalf("verdict %s: %v", v.Status, v.Reasons)
	}
	near := func(got, want float64) bool { return math.Abs(got-want) < 1e-6 }
	if !near(v.Stresses.IndexLossBase, 9600) || !near(v.Stresses.IndexRiseLossBase, -9600) {
		t.Fatalf("fall %.0f rise %.0f, want 9,600 and -9,600 with the hedge counted", v.Stresses.IndexLossBase, v.Stresses.IndexRiseLossBase)
	}
	if !near(v.Loss.IndexHedgeBase, 5400) || !near(v.Loss.TotalBase, v.Loss.PremiumBase+v.Loss.StopLossBase+5400) || slices.Contains(v.Loss.Unstopped, "MESZ6") {
		t.Fatalf("loss = %+v, want the hedge's 5,400 rise loss in the measure and the line not unstopped", v.Loss)
	}
	if i := slices.IndexFunc(v.Loss.Names, func(n mandate.LossName) bool { return n.Underlying == mandate.MicroSP500Future }); i < 0 || !near(v.Loss.Names[i].IndexHedgeBase, 5400) {
		t.Fatalf("names = %+v, want MES FUT at 5,400", v.Loss.Names)
	}
	if dd, ok := positionDollarDelta(pos.Stocks[1], false); !ok || !near(dd, -60000) {
		t.Fatalf("dollar delta %v %v, want -60,000 (quantity × price × multiplier)", dd, ok)
	}
}

// A stop that covers the whole future bounds its loss at the stop's
// distance, at contract value per point: a buy stop 100 points above two
// short contracts loses 100 × 5 × 2 × 0.9 = 900, which replaces the 5,400
// index-move figure.
func TestMandateHeldIndexFutureStopCountsPerPoint(t *testing.T) {
	pos := mandateReductionBook(mandateReductionFuture(750001, -2, 6000))
	legs, _, fxNow, reasons := mandateLegsFromPositions(pos, "EUR", mandateTestNow)
	if len(reasons) > 0 || len(legs) != 1 {
		t.Fatalf("legs %v reasons %v", legs, reasons)
	}
	armed, note := mandateApplyStops(legs, pos, "EUR", map[string][]mandateArmedStop{"con:750001": {{StopPrice: 6100, Quantity: 2}}}, "")
	if note != "" || len(armed) != 1 || !armed[0].StopArmed || math.Abs(armed[0].LossPerUnitBase-450) > 1e-9 {
		t.Fatalf("armed = %+v (%q), want 450 a contract and armed", armed, note)
	}
	m, why := mandate.MeasureLoss(mandate.Input{BaseCurrency: "EUR", Capital: mandate.Capital{Known: true, Room: 1, BudgetBase: 1000000}, Policy: mandate.Policy{MaxLossPct: 30}, Legs: armed, FXNow: fxNow})
	if len(why) > 0 || m == nil || math.Abs(m.IndexHedgeBase-900) > 1e-9 || math.Abs(m.TotalBase-900) > 1e-9 {
		t.Fatalf("measure = %+v (%v), want 900 to the stop", m, why)
	}
	unstopped, _ := mandate.MeasureLoss(mandate.Input{BaseCurrency: "EUR", Capital: mandate.Capital{Known: true, Room: 1, BudgetBase: 1000000}, Policy: mandate.Policy{MaxLossPct: 30}, Legs: legs, FXNow: fxNow})
	if unstopped == nil || math.Abs(unstopped.IndexHedgeBase-5400) > 1e-9 {
		t.Fatalf("without a stop = %+v, want the 5,400 index-move loss", unstopped)
	}
}
