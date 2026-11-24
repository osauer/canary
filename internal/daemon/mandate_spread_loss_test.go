package daemon

import (
	"math"
	"testing"

	"github.com/osauer/canary/v3/internal/mandate"
	"github.com/osauer/canary/v3/internal/rpc"
)

func mandateSpreadRow(conID int, qty, mark, avgCost, strike float64, right string) rpc.PositionView {
	iv, fx := 0.3, 1.0
	mv := qty * 100 * mark
	return rpc.PositionView{Symbol: "SYNC", SecType: "OPT", ConID: conID, Currency: "USD", Quantity: qty, Multiplier: 100, Mark: mark, AvgCost: avgCost,
		MarketValue: mv, MarketValueBase: &mv, FXRate: &fx, Expiry: "20270115", Strike: strike, Right: right, TradingClass: "SYNC", IV: &iv, Underlying: new(100.0)}
}

func mandateLegLoss(legs []mandate.Leg, conID int) float64 {
	for _, l := range legs {
		if l.ID == mandateConLegID(conID) {
			return l.LossPerUnitBase * math.Abs(l.Quantity)
		}
	}
	return math.NaN()
}

// A held debit vertical counts in the loss measure at what the pair can
// still lose, its net value, so the measure after an open's fill is what
// the open was admitted at, not the bought leg's whole value. A bought leg
// the sold one covers only in part counts its uncovered contracts in full,
// and a sold leg nothing covers offsets nothing.
func TestMandateLossCountsAHeldSpreadAtItsNetValue(t *testing.T) {
	cases := []struct {
		name string
		rows []rpc.PositionView
		want float64 // the bought leg's part of the measure
	}{
		{"bull call spread", []rpc.PositionView{mandateSpreadRow(1, 1, 5, 350, 100, "C"), mandateSpreadRow(2, -1, 2, 150, 110, "C")}, 300},
		{"two units, one covered", []rpc.PositionView{mandateSpreadRow(1, 2, 5, 350, 100, "C"), mandateSpreadRow(2, -1, 2, 150, 110, "C")}, 800},
		{"sold leg nearer the money", []rpc.PositionView{mandateSpreadRow(1, 1, 2, 150, 110, "C"), mandateSpreadRow(2, -1, 5, 350, 100, "C")}, 200},
		{"bear put spread", []rpc.PositionView{mandateSpreadRow(1, 1, 9, 900, 600, "P"), mandateSpreadRow(2, -1, 4, 400, 570, "P")}, 500},
		{"bought leg alone", []rpc.PositionView{mandateSpreadRow(1, 1, 5, 350, 100, "C")}, 500},
	}
	for _, tc := range cases {
		legs, _, _, _ := mandateLegsFromPositions(&rpc.PositionsResult{Options: tc.rows}, "USD", mandateTestNow)
		if got := mandateLegLoss(legs, 1); math.Abs(got-tc.want) > 1e-6 {
			t.Errorf("%s: bought leg counts %v, want %v", tc.name, got, tc.want)
		}
		if got := mandateLegLoss(legs, 2); len(tc.rows) > 1 && got != 0 {
			t.Errorf("%s: sold leg counts %v, want 0", tc.name, got)
		}
	}
}
