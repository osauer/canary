package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

// A tracked trailing-stop row carries the stop the broker last reported and
// when; a row the read does not show, or a report without a stop, carries
// none, and a request echoing one back sends only its own terms.
func TestTrailRowsCarryTheBrokersCurrentStop(t *testing.T) {
	scope := brokerStateScope{Account: "DU1234567", Mode: "paper"}
	asOf := time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC)
	s := &Server{openOrderInventoryForTest: func(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
		return ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: asOf, Orders: []ibkrlib.OrderLifecycleEvent{
			{PermID: 7, ConID: 9001, OrderType: "TRAIL", AuxPrice: 18.5, TrailStopPrice: 288},
			{PermID: 8, ConID: 9002, OrderType: "TRAIL", AuxPrice: 1},
		}}, scope, nil
	}}
	trail := func() *rpc.OrderTrailSpec {
		return &rpc.OrderTrailSpec{OffsetType: rpc.OrderTrailOffsetAmount, TrailingAmount: new(18.5), InitialStopPrice: 270}
	}
	views := []rpc.OrderView{{PermID: 7, ConID: 9001, Open: true, Trail: trail()}, {PermID: 8, ConID: 9002, Open: true, Trail: trail()}, {PermID: 9, ConID: 9003, Open: true, Trail: trail()}}
	s.attachTrailStops(t.Context(), views, scope)
	if got := views[0].Trail; got.StopPrice != 288 || !got.StopPriceAt.Equal(asOf) || got.StopPriceSource != rpc.OrderTrailStopSourceBroker || got.InitialStopPrice != 270 {
		t.Fatalf("reported stop: %+v", got)
	}
	for _, v := range views[1:] {
		if v.Trail.StopPrice != 0 || v.Trail.StopPriceSource != "" {
			t.Fatalf("a stop without a report: %+v", v.Trail)
		}
	}
	echo := *views[0].Trail
	echo.Basis, echo.OffsetType = rpc.OrderTrailBasisInstrumentPrice, rpc.OrderTrailOffsetAmount
	out, err := previewTrailSpec(rpc.OrderActionSell, rpc.OrderTypeTRAIL, &echo, rpc.ContractParams{SecType: "STK"}, rpc.OrderQuoteSnapshot{})
	if err == nil && (out.StopPrice != 0 || out.StopPriceSource != "") {
		t.Fatalf("a request carried the read-only stop: %+v", out)
	}
}

// The loss measure counts the cash loss to where a trail triggers now: the
// broker's ratcheted stop, never the one it started at.
func TestMandateLossReadsTheRatchetedStop(t *testing.T) {
	order := ibkrlib.OrderLifecycleEvent{ConID: 9001, Action: "SELL", OrderType: "TRAIL", Status: "Submitted", TotalQuantity: 40, Remaining: 40, AuxPrice: 18.5, TrailStopPrice: 288}
	stop, ok := mandateStopForPosition(brokerWorkingOrder{Order: order}, rpc.PositionView{ConID: 9001, Quantity: 40})
	if !ok || stop.StopPrice != 288 || stop.Quantity != 40 {
		t.Fatalf("armed stop: %+v %v", stop, ok)
	}
}
