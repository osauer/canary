package ibkr

import (
	"testing"
)

// marketDataType (58) is both connection state and a frame callers register
// for: FetchMarketSnapshot learns live versus delayed from it. The frame must
// reach registered handlers like every other inbound message, after the
// connection has recorded it. A stale socket generation's frame reaches none.
func TestMarketDataTypeNoticeReachesRegisteredHandlers(t *testing.T) {
	conn, _ := newReadyWireTestConnection(t)
	got := make(chan []string, 2)
	conn.RegisterHandler(msgMarketDataType, func(fields []string) { got <- fields })
	conn.processMessage(twsGatewayFrame(msgMarketDataType, "1", "7", "3"))
	select {
	case fields := <-got:
		if len(fields) < 4 || fields[2] != "7" || fields[3] != "3" {
			t.Fatalf("handler saw %q", fields)
		}
	default:
		t.Fatal("registered handler did not receive the marketDataType frame")
	}
	if conn.MarketDataType(7) != 3 {
		t.Fatalf("data type for request 7 = %d, want 3", conn.MarketDataType(7))
	}
	conn.processMessageAtEpoch(twsGatewayFrame(msgMarketDataType, "1", "8", "1"), conn.BrokerSessionEpoch()+1)
	select {
	case fields := <-got:
		t.Fatalf("a stale generation's frame reached the handler: %q", fields)
	default:
	}
	if conn.MarketDataType(8) != 0 {
		t.Fatal("a stale generation's frame recorded a data type")
	}
}
