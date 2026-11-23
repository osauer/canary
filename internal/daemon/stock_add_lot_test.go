package daemon

import (
	"context"
	"testing"

	"github.com/osauer/canary/v3/internal/rpc"
)

// stockAddLotServer is the stock add rig on a line the broker trades in lots.
func stockAddLotServer(t *testing.T, lot stockLot, freeCash float64) *Server {
	t.Helper()
	s, _ := stockAddTestServer(t)
	evidence := s.stockAddEvidenceForTest
	s.stockAddEvidenceForTest = func(ctx context.Context, p rpc.AddParams) (stockAddEvidence, error) {
		ev, err := evidence(ctx, p)
		ev.lot = lot
		if freeCash > 0 {
			ev.input.FreeCash, ev.input.OrderCapBase = freeCash, freeCash
		}
		return ev, err
	}
	return s
}

func lotBlocker(res *rpc.AddPlanResult) string {
	for _, b := range res.Blockers {
		if b.Code == "add_lot_size" {
			return b.Message
		}
	}
	return ""
}

func TestStockAddHoldsAnOrderOffTheBrokersLot(t *testing.T) {
	s := stockAddLotServer(t, stockLot{min: 100, increment: 100}, 50000)
	p := stockAddTestParams()
	p.Max, p.Quantity = false, 1
	res, err := s.planStockAdd(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	if got := lotBlocker(res); got != "SYNA trades in lots of 100 shares; choose a multiple of 100." || res.Quantity != 0 || res.Review != nil {
		t.Fatalf("one share of a lot-100 line: blocker %q plan %+v", got, res.StockAddPlan)
	}
	p.Quantity = 100
	if res, err := s.planStockAdd(t.Context(), p); err != nil || lotBlocker(res) != "" || res.Quantity != 100 || res.Review == nil {
		t.Fatalf("a whole lot: %+v %v", res, err)
	}
	if got := (stockLot{min: 500, increment: 100}).sentence("SYNA"); got != "SYNA trades from 500 shares in steps of 100; choose 500 shares plus a multiple of 100." {
		t.Fatalf("minimum and step sentence = %q", got)
	}
}

// The maximum is a whole number of lots; a line in single shares sizes as
// before.
func TestStockAddMaximumIsAWholeLot(t *testing.T) {
	res, err := stockAddLotServer(t, stockLot{min: 10, increment: 10}, 0).planStockAdd(t.Context(), stockAddTestParams())
	if err != nil || lotBlocker(res) != "" || res.Quantity != 90 || !res.MaximumKnown {
		t.Fatalf("maximum in lots of 10 = %d (%+v), %v", res.Quantity, res.Blockers, err)
	}
	res, err = stockAddLotServer(t, stockLot{}, 0).planStockAdd(t.Context(), stockAddTestParams())
	if err != nil || res.Quantity != 99 {
		t.Fatalf("maximum in single shares = %d (%+v), %v", res.Quantity, res.Blockers, err)
	}
	if res, err := stockAddLotServer(t, stockLot{min: 1000, increment: 1000}, 0).planStockAdd(t.Context(), stockAddTestParams()); err != nil || lotBlocker(res) == "" {
		t.Fatalf("no whole lot fits: %+v, %v", res, err)
	}
}
