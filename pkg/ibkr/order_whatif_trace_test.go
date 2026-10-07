package ibkr

import (
	"bufio"
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A WhatIf that times out says what IBKR sent for the order: an openOrder
// the reader could not match, or nothing at all (a bond WhatIf timed out live
// on 2026-10-07 with nothing in the log).
func TestWhatIfTimeoutNamesTheFramesIBKRSent(t *testing.T) {
	run := func(inject func(*Connection)) OrderWhatIfResult {
		conn := NewConnection(DefaultConfig())
		defer conn.rateLimiter.Stop()
		conn.status = StatusConnected
		setServerVersionReady(conn, minServerVerProtoBufPlaceOrder)
		conn.observeNextValidOrderID(77)
		var buf safeBuffer
		conn.writer = bufio.NewWriter(&buf)
		ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
		defer cancel()
		rules := BondOrderRules{MinTick: 0.00001, MinSize: 1, SizeIncrement: 1}
		done := make(chan OrderWhatIfResult, 1)
		go func() {
			result, err := conn.PreviewOrderWhatIf(ctx, &IBKROrder{ConID: 900001, SecType: SecTypeBond, Exchange: "SMART", Currency: "USD", BondRules: &rules,
				Action: "BUY", TotalQty: 1, OrderType: "LMT", LmtPrice: 98.8, LmtPriceSet: true, TIF: "DAY", Account: "DU123456"})
			if err != nil {
				t.Errorf("preview: %v", err)
			}
			done <- result
		}()
		waitForWhatIfFrame(t, &buf)
		inject(conn)
		return <-done
	}
	got := run(func(c *Connection) {
		c.processMessageAtEpoch(inboundWireFrame(strconv.Itoa(msgOpenOrder), "77", "an openOrder the reader cannot match"), c.BrokerSessionEpoch())
		c.processMessageAtEpoch(inboundWireFrame(strconv.Itoa(msgOpenOrder), "78", "another order"), c.BrokerSessionEpoch())
	})
	if got.Status != OrderWhatIfStatusUnavailable || !strings.Contains(got.Message, "IBKR sent for order 77: msg 5, ") || strings.Count(got.Message, "msg 5") != 1 {
		t.Fatalf("result = %+v, want the unmatched openOrder for order 77 named", got)
	}
	got = run(func(*Connection) {})
	if !strings.Contains(got.Message, "IBKR sent nothing naming order 77") {
		t.Fatalf("result = %+v, want the silence named", got)
	}
}
