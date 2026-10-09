package ibkr

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

func adaptiveTestOrder() IBKROrder {
	return IBKROrder{OrderID: 17, ClientID: 9, Symbol: "SYN", SecType: "STK", ConID: 42, Exchange: "SMART", Currency: "USD", Action: "BUY", TotalQty: 2, OrderType: "LMT", LmtPrice: 20.25, LmtPriceSet: true, TIF: "DAY", AdaptivePriority: "Normal"}
}

func TestAdaptiveProtoTermsAreIdenticalForWhatIfAndTransmit(t *testing.T) {
	for _, priority := range []string{"Patient", "Normal", "Urgent"} {
		var terms [][]byte
		for _, whatIf := range []bool{true, false} {
			order := adaptiveTestOrder()
			order.AdaptivePriority = priority
			order.WhatIf = whatIf
			order.Transmit = true
			raw, err := encodePlaceOrderProtoBody(&order)
			if err != nil {
				t.Fatal(err)
			}
			var body []byte
			if err := forEachProtoField(raw, func(field, wire int, value []byte) error {
				if field == 3 {
					body = value
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			var got []byte
			strategy, key, value := "", "", ""
			params := 0
			if err := forEachProtoField(body, func(field, wire int, b []byte) error {
				if field == 61 {
					strategy = string(b)
				}
				if field == 62 {
					params++
					if err := forEachProtoField(b, func(f, w int, v []byte) error {
						if f == 1 {
							key = string(v)
						}
						if f == 2 {
							value = string(v)
						}
						return nil
					}); err != nil {
						return err
					}
				}
				if field != 65 && field != 66 {
					got = append(got, byte(field), byte(wire))
					got = append(got, b...)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if strategy != "Adaptive" || params != 1 || key != "adaptivePriority" || value != priority {
				t.Fatalf("lost Adaptive terms: %q %d %q %q", strategy, params, key, value)
			}
			terms = append(terms, got)
		}
		if !bytes.Equal(terms[0], terms[1]) {
			t.Fatalf("WhatIf and transmit differ for %s", priority)
		}
	}
}

func TestAdaptiveUnsupportedShapesDoNotBecomePlainOrders(t *testing.T) {
	for _, change := range []func(*IBKROrder){
		func(o *IBKROrder) { o.AdaptivePriority = "normal" },
		func(o *IBKROrder) { o.AdaptivePriority = "Urgent\x00" },
		func(o *IBKROrder) { o.SecType = "OPT" },
		func(o *IBKROrder) { o.OrderType = "MKT" },
		func(o *IBKROrder) { o.TIF = "GTC" },
		func(o *IBKROrder) { o.Exchange = "NYSE" },
		func(o *IBKROrder) { o.OutsideRth = true },
	} {
		order := adaptiveTestOrder()
		change(&order)
		if raw, err := encodePlaceOrderProtoBody(&order); err == nil || len(raw) != 0 {
			t.Fatalf("unsupported Adaptive encoded: %+v", order)
		}
	}
	// The legacy encoder has no Adaptive support. It must refuse before I/O.
	order := adaptiveTestOrder()
	c := &Connection{}
	if err := c.sendPlaceOrderFrameGuarded(context.Background(), &order, 0, nil); err == nil || !strings.Contains(err.Error(), "does not support Adaptive") {
		t.Fatalf("legacy fallback: %v", err)
	}
}

func TestAdaptiveBrokerCallbackRetainsAlgorithm(t *testing.T) {
	order := adaptiveTestOrder()
	raw, err := encodePlaceOrderProtoBody(&order)
	if err != nil {
		t.Fatal(err)
	}
	// OpenOrder uses the same contract/order field numbers as PlaceOrder.
	fields := summarizeOpenOrderProtoCallback(raw)
	event, ok := parseOpenOrderProtoEvent(fields)
	if !ok || !event.AlgoKnown || event.AlgoStrategy != "Adaptive" || event.AdaptivePriority != "Normal" {
		t.Fatalf("broker algorithm lost: %+v", event)
	}
	order.AdaptivePriority = ""
	raw, err = encodePlaceOrderProtoBody(&order)
	if err != nil {
		t.Fatal(err)
	}
	event, ok = parseOpenOrderProtoEvent(summarizeOpenOrderProtoCallback(raw))
	if !ok || !event.AlgoKnown || event.AlgoStrategy != "" || event.AdaptivePriority != "" {
		t.Fatalf("plain order misread: %+v", event)
	}
}

func TestAdaptiveWhatIfRequiresMatchingBrokerAlgorithm(t *testing.T) {
	for _, observed := range []string{"Normal", "", "Urgent"} {
		t.Run("broker-"+observed, func(t *testing.T) {
			connector := readyBrokerEvidenceTestConnector(t)
			conn := connector.conn
			setServerVersionReady(conn, minServerVerProtoBufPlaceOrder)
			conn.observeNextValidOrderID(77)
			var buf safeBuffer
			conn.writer = bufio.NewWriter(&buf)
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			done := make(chan OrderWhatIfResult, 1)
			go func() {
				got, err := connector.PreviewOrderWhatIf(ctx, &Contract{ConID: 42, Symbol: "SYN", SecType: "STK", Exchange: "SMART", Currency: "USD"}, &RawOrder{Action: "BUY", TotalQty: 2, OrderType: "LMT", LmtPrice: 20.25, TIF: "DAY", AdaptivePriority: "Normal"})
				if err != nil {
					t.Error(err)
				}
				done <- got
			}()
			waitForWhatIfFrame(t, &buf)
			payload := extractFramePayload(t, &buf)
			summary, err := parseOpenOrderProtoCallback(payload[4:])
			if err != nil || summary.algoStrategy != "Adaptive" || summary.adaptivePriority != "Normal" {
				t.Fatalf("connector lost algorithm: %+v %v", summary, err)
			}
			ack := adaptiveTestOrder()
			ack.OrderID = summary.orderID
			ack.WhatIf = true
			ack.Transmit = true
			ack.AdaptivePriority = observed
			body, err := encodePlaceOrderProtoBody(&ack)
			if err != nil {
				t.Fatal(err)
			}
			body = protoAppendMessage(body, 4, protoAppendString(nil, 1, "Submitted"))
			frame := binary.BigEndian.AppendUint32(nil, uint32(msgOpenOrder+protoBufMsgID))
			conn.processMessage(append(frame, body...))
			result := <-done
			if observed == "Normal" {
				if result.Status != OrderWhatIfStatusAccepted {
					t.Fatalf("matching callback: %+v", result)
				}
			} else if result.Status != OrderWhatIfStatusUnavailable {
				t.Fatalf("changed/missing algorithm accepted: %+v", result)
			}
		})
	}
}
