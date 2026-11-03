package ibkr

import (
	"strconv"
	"testing"
)

// legacyOrderStatusFrame is the orderStatus frame of a server below the
// protobuf callbacks, in EDecoder's order: orderId, status, filled,
// remaining, avgFillPrice, permId, parentId, lastFillPrice, clientId,
// whyHeld, mktCapPrice. Servers below 146 (MIN_SERVER_VER_MARKET_CAP_PRICE)
// put a version field first.
func legacyOrderStatusFrame(versioned bool, orderID int, status string, filled, remaining, avg float64, permID, parentID int, lastFill float64, clientID int, whyHeld string) []string {
	f := func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
	fields := []string{strconv.Itoa(msgOrderStatus)}
	if versioned {
		fields = append(fields, "6")
	}
	return append(fields, strconv.Itoa(orderID), status, f(filled), f(remaining), f(avg), strconv.Itoa(permID), strconv.Itoa(parentID), f(lastFill), strconv.Itoa(clientID), whyHeld, "0")
}

// A bracket child's status carries its parent's order id between permId and
// lastFillPrice. Both legacy decoders must read the fill price and the
// client id from their own fields; reading the parent id as a price, or the
// price as a client id, attributes a fill to the parent's id and a working
// child to client 0 (a manual TWS order).
func TestLegacyOrderStatusFrameReadsLastFillPriceAndClientIDAfterParentID(t *testing.T) {
	for _, versioned := range []bool{false, true} {
		frame := legacyOrderStatusFrame(versioned, 41, "Submitted", 3, 7, 100.5, 9001, 40, 100.6, 15, "")
		ev, ok := ParseOrderLifecycleEvent(frame)
		if !ok || ev.OrderID != 41 || ev.Status != "Submitted" || ev.Filled != 3 || ev.Remaining != 7 || ev.AvgFillPrice != 100.5 || ev.PermID != 9001 {
			t.Fatalf("versioned=%t head: %+v ok=%t", versioned, ev, ok)
		}
		if ev.LastFillPrice != 100.6 {
			t.Fatalf("versioned=%t LastFillPrice = %v, want 100.6 (the field after parentId)", versioned, ev.LastFillPrice)
		}
		if ev.ClientID != 15 || !ev.ClientIDPresent {
			t.Fatalf("versioned=%t ClientID = %d present=%t, want 15", versioned, ev.ClientID, ev.ClientIDPresent)
		}
		held := legacyOrderStatusFrame(versioned, 41, "Submitted", 0, 10, 0, 9001, 40, 0, 15, "locate")
		if ev, _ := ParseOrderLifecycleEvent(held); ev.WhyHeld != "locate" || ev.LastFillPrice != 0 || ev.ClientID != 15 {
			t.Fatalf("versioned=%t held: %+v", versioned, ev)
		}
	}

	c := NewConnector(&ConnectorConfig{})
	t.Cleanup(c.conn.rateLimiter.Stop)
	child := &trackedOrder{ID: "child", Status: OrderStatusSubmitted, Quantity: 10}
	c.openOrders["child"] = child
	c.brokerOrderIndex["41"] = "child"
	// No fill yet: the average is 0 and so is the last fill; the parent id
	// 40 must not become the fill price.
	c.handleOrderStatus(legacyOrderStatusFrame(false, 41, "Submitted", 0, 10, 0, 9001, 40, 0, 15, ""))
	if child.FilledPrice != 0 || child.Status != OrderStatusSubmitted {
		t.Fatalf("working child recorded a fill price: %+v", child)
	}
	// A fill reported with only the last fill price uses it.
	c.handleOrderStatus(legacyOrderStatusFrame(false, 41, "Submitted", 5, 5, 0, 9001, 40, 100.6, 15, ""))
	if child.FilledPrice != 100.6 || child.Status != OrderStatusPartial {
		t.Fatalf("partial with last fill price only: %+v", child)
	}
}
