//go:build trading

package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
	"github.com/osauer/canary/v3/pkg/ibkr/twssim"
)

// A stock trailing stop entered in TWS ratchets at the broker as the stock
// rises. The open-orders row and the loss measure read the stop the broker
// reports now, with when it was read, not where it started; when the stock
// falls to it, it sells there. SYNA trades at 50.00; every figure is
// invented.
func TestSimulatedTWSTrailingStopRatchets(t *testing.T) {
	d, _ := planDesk(t, 50, 20)
	id, err := d.sim.AddOutsideTrail(planSYNA, 100, 2, 48)
	if err != nil {
		t.Fatal(err)
	}
	stopNow := func() (rpc.OrderTrailSpec, float64) {
		t.Helper()
		if _, _, err := d.srv.brokerOpenOrderInventory(t.Context(), true); err != nil {
			t.Fatalf("open orders: %v", err)
		}
		res, rerr := dispatchJSON[rpc.OrdersOpenResult](t, d.srv, rpc.MethodOrdersOpen, rpc.OrdersOpenParams{})
		if rerr != nil {
			t.Fatalf("orders open: %+v", rerr)
		}
		var trail rpc.OrderTrailSpec
		for _, v := range res.Untracked {
			if v.ConID == planSYNA && v.OrderType == rpc.OrderTypeTRAIL && v.Trail != nil {
				trail = *v.Trail
			}
		}
		pos, perr := d.srv.mandatePositions(t.Context())
		if perr != nil {
			t.Fatalf("positions: %v", perr)
		}
		stops, why := d.srv.mandateArmedStops(t.Context(), pos, d.srv.currentBrokerStateScope())
		loss := 0.0
		for _, st := range stops["con:900001"] {
			loss = st.StopPrice
		}
		if why != "" {
			t.Fatalf("stops: %s", why)
		}
		return trail, loss
	}
	if trail, stop := stopNow(); trail.StopPrice != 48 || trail.StopPriceSource != rpc.OrderTrailStopSourceBroker || trail.StopPriceAt.IsZero() || stop != 48 {
		t.Fatalf("placed: %+v, loss measure stop %g", trail, stop)
	}
	d.sim.MovePrice(planSYNA, 53)
	d.sim.MovePrice(planSYNA, 52)
	trail, stop := stopNow()
	if trail.StopPrice != 51 || time.Since(trail.StopPriceAt) > time.Minute || stop != 51 {
		t.Fatalf("after the rise: %+v, loss measure stop %g", trail, stop)
	}
	d.sim.MovePrice(planSYNA, 50.9)
	var o twssim.Order
	for _, x := range d.sim.Book().Orders() {
		if x.OrderID == id {
			o = x
		}
	}
	if o.Status != "Filled" || o.Filled != 100 || o.AvgPrice > 51 || o.AvgPrice < 50.8 {
		t.Fatalf("at the ratcheted stop: %+v", o)
	}
}
