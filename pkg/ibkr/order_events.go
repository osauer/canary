package ibkr

import (
	"strconv"
	"strings"
)

const (
	// OrderLifecycleEventOpenOrder identifies an openOrder callback.
	OrderLifecycleEventOpenOrder = "openOrder"
	// OrderLifecycleEventStatus identifies an orderStatus callback.
	OrderLifecycleEventStatus = "orderStatus"
	// OrderLifecycleEventExecDetails identifies an execDetails callback.
	OrderLifecycleEventExecDetails = "execDetails"
	// OrderLifecycleEventError identifies a correlated broker error synthesized by Connector.
	OrderLifecycleEventError = "error"
)

// OrderLifecycleEvent is a typed subset of one broker order callback. Type
// identifies which fields are meaningful. Status remains the broker's text and
// no individual callback, socket write, or local mutation proves lifecycle
// finality; consumers must reconcile the callback sequence and broker truth.
//
// Quantities are in the broker's instrument units and prices are in the
// contract's quote currency. Numeric zero can be a real value or an absent or
// unparsable field because the wire callbacks do not preserve that distinction.
// Raw is a copied slice of untrusted wire fields and may be nil for synthesized
// error events.
type OrderLifecycleEvent struct {
	AlgoStrategy     string
	AdaptivePriority string
	AlgoKnown        bool
	Type             string // Type is one of the OrderLifecycleEvent constants.
	OrderID          int    // OrderID is session-scoped; zero means absent.
	PermID           int    // PermID is IBKR's permanent order ID; zero means absent.
	ClientID         int    // ClientID identifies the TWS API client; zero is a valid client ID.
	ClientIDPresent  bool   // ClientIDPresent distinguishes explicit client 0 from an omitted legacy field.
	RequestID        int    // RequestID correlates execDetails requests; zero means absent.
	Status           string // Status is unnormalized broker state and may be empty.
	ErrorCode        int    // ErrorCode is populated for synthesized error events.
	Message          string // Message is untrusted broker warning or error text.
	Symbol           string
	SecType          string
	ConID            int
	Expiry           string
	Strike           float64
	Right            string
	Multiplier       int
	Exchange         string
	Currency         string
	LocalSymbol      string
	TradingClass     string
	Action           string
	TotalQuantity    float64 // TotalQuantity is in shares or contracts.
	OrderType        string
	LimitPrice       float64 // LimitPrice is in quote-currency units.
	AuxPrice         float64 // AuxPrice is a stop price or trailing amount.
	TrailingPercent  float64 // TrailingPercent is the broker percentage value, not a fraction.
	TrailStopPrice   float64 // TrailStopPrice is in quote-currency units.
	LmtPriceOffset   float64 // LmtPriceOffset is in price units.
	TIF              string
	TriggerMethod    int
	OutsideRth       bool
	WhatIf           bool
	Filled           float64 // Filled is the callback's cumulative filled quantity.
	Remaining        float64 // Remaining is the callback's unfilled quantity.
	AvgFillPrice     float64 // AvgFillPrice is in quote-currency units.
	LastFillPrice    float64 // LastFillPrice is in quote-currency units.
	WhyHeld          string
	MktCapPrice      float64
	ExecID           string
	ExecTime         string
	Account          string
	ExecutionSide    string
	Shares           float64 // Shares is this execution's quantity in shares or contracts.
	Price            float64 // Price is this execution's price in quote-currency units.
	CumQty           float64 // CumQty is the execution's cumulative filled quantity.
	OrderRef         string
	Raw              []string
}

// OrderLifecycleReceipt binds one parsed broker lifecycle event to the exact
// Connection object and socket epoch that read its wire frame. Session is
// process-local evidence, not durable order authority.
type OrderLifecycleReceipt struct {
	Session ConnectorSessionBinding
	Event   OrderLifecycleEvent
}

// ParseOrderLifecycleEvent parses openOrder, orderStatus, and execDetails wire
// callbacks. It accepts both legacy and summarized protobuf forms. Unknown or
// malformed messages return ok=false and a zero event; broker error events are
// correlated and synthesized separately by Connector. Parsed events are
// observations, not a finality decision.
func ParseOrderLifecycleEvent(fields []string) (ev OrderLifecycleEvent, ok bool) {
	if len(fields) == 0 {
		return OrderLifecycleEvent{}, false
	}
	msgID, err := strconv.Atoi(strings.TrimSpace(fields[0]))
	if err != nil {
		return OrderLifecycleEvent{}, false
	}
	switch msgID {
	case msgOpenOrder:
		return parseOpenOrderEvent(fields)
	case msgOrderStatus:
		return parseOrderStatusEvent(fields)
	case msgExecDetails:
		return parseExecDetailsEvent(fields)
	default:
		return OrderLifecycleEvent{}, false
	}
}

func parseOpenOrderEvent(fields []string) (OrderLifecycleEvent, bool) {
	if len(fields) > 1 && fields[1] == "protobuf" {
		return parseOpenOrderProtoEvent(fields)
	}
	start := 1
	if len(fields) > 2 && orderEventIntOK(fields[1]) {
		start = 2
	}
	if len(fields) <= start+17 {
		return OrderLifecycleEvent{}, false
	}
	statusIdx := start + 18
	whatIf := false
	if parseIBKRBool(orderEventField(fields, statusIdx)) && orderWhatIfStateStatus(orderEventField(fields, statusIdx+1)) {
		whatIf = true
		statusIdx++
	}
	ev := OrderLifecycleEvent{
		Type:          OrderLifecycleEventOpenOrder,
		OrderID:       orderEventInt(fields[start]),
		ConID:         orderEventInt(orderEventField(fields, start+1)),
		PermID:        orderEventInt(orderEventField(fields, start+19)),
		Symbol:        strings.ToUpper(strings.TrimSpace(orderEventField(fields, start+2))),
		SecType:       strings.ToUpper(strings.TrimSpace(orderEventField(fields, start+3))),
		Expiry:        strings.TrimSpace(orderEventField(fields, start+4)),
		Strike:        orderEventFloat(orderEventField(fields, start+5)),
		Right:         strings.ToUpper(strings.TrimSpace(orderEventField(fields, start+6))),
		Multiplier:    orderEventInt(orderEventField(fields, start+7)),
		Exchange:      strings.TrimSpace(orderEventField(fields, start+8)),
		Currency:      strings.ToUpper(strings.TrimSpace(orderEventField(fields, start+9))),
		LocalSymbol:   strings.TrimSpace(orderEventField(fields, start+10)),
		TradingClass:  strings.TrimSpace(orderEventField(fields, start+11)),
		Action:        strings.ToUpper(strings.TrimSpace(orderEventField(fields, start+12))),
		TotalQuantity: orderEventFloat(orderEventField(fields, start+13)),
		OrderType:     strings.ToUpper(strings.TrimSpace(orderEventField(fields, start+14))),
		LimitPrice:    orderEventFloat(orderEventField(fields, start+15)),
		AuxPrice:      orderEventFloat(orderEventField(fields, start+16)),
		TIF:           strings.ToUpper(strings.TrimSpace(orderEventField(fields, start+17))),
		WhatIf:        whatIf,
		Status:        strings.TrimSpace(orderEventField(fields, statusIdx)),
		Raw:           append([]string{}, fields...),
	}
	return ev, ev.OrderID > 0
}

func parseOrderStatusEvent(fields []string) (OrderLifecycleEvent, bool) {
	if len(fields) > 1 && fields[1] == "protobuf" {
		return parseOrderStatusProtoEvent(fields)
	}
	start := 1
	if len(fields) > 3 && orderEventIntOK(fields[1]) && orderEventIntOK(fields[2]) {
		start = 2
	}
	if len(fields) <= start+3 {
		return OrderLifecycleEvent{}, false
	}
	ev := OrderLifecycleEvent{
		Type:            OrderLifecycleEventStatus,
		OrderID:         orderEventInt(fields[start]),
		Status:          strings.TrimSpace(orderEventField(fields, start+1)),
		Filled:          orderEventFloat(orderEventField(fields, start+2)),
		Remaining:       orderEventFloat(orderEventField(fields, start+3)),
		AvgFillPrice:    orderEventFloat(orderEventField(fields, start+4)),
		PermID:          orderEventInt(orderEventField(fields, start+5)),
		LastFillPrice:   orderEventFloat(orderEventField(fields, start+6)),
		ClientID:        orderEventInt(orderEventField(fields, start+7)),
		ClientIDPresent: len(fields) > start+7 && orderEventIntOK(orderEventField(fields, start+7)),
		WhyHeld:         strings.TrimSpace(orderEventField(fields, start+9)),
		MktCapPrice:     orderEventFloat(orderEventField(fields, start+10)),
		Raw:             append([]string{}, fields...),
	}
	return ev, ev.OrderID > 0 && ev.Status != ""
}

func parseExecDetailsEvent(fields []string) (OrderLifecycleEvent, bool) {
	if len(fields) > 1 && fields[1] == "protobuf" {
		return parseExecDetailsProtoEvent(fields)
	}
	start := 1
	if len(fields) > 2 && orderEventIntOK(fields[1]) {
		start = 2
	}
	if len(fields) <= start+19 {
		return OrderLifecycleEvent{}, false
	}
	ev := OrderLifecycleEvent{
		Type:            OrderLifecycleEventExecDetails,
		RequestID:       orderEventInt(orderEventField(fields, start)),
		OrderID:         orderEventInt(orderEventField(fields, start+1)),
		ConID:           orderEventInt(orderEventField(fields, start+2)),
		Symbol:          strings.ToUpper(strings.TrimSpace(orderEventField(fields, start+3))),
		SecType:         strings.ToUpper(strings.TrimSpace(orderEventField(fields, start+4))),
		Expiry:          strings.TrimSpace(orderEventField(fields, start+5)),
		Strike:          orderEventFloat(orderEventField(fields, start+6)),
		Right:           strings.ToUpper(strings.TrimSpace(orderEventField(fields, start+7))),
		Multiplier:      orderEventInt(orderEventField(fields, start+8)),
		Exchange:        strings.TrimSpace(orderEventField(fields, start+9)),
		Currency:        strings.ToUpper(strings.TrimSpace(orderEventField(fields, start+10))),
		LocalSymbol:     strings.TrimSpace(orderEventField(fields, start+11)),
		ExecID:          strings.TrimSpace(orderEventField(fields, start+12)),
		ExecTime:        strings.TrimSpace(orderEventField(fields, start+13)),
		Account:         strings.TrimSpace(orderEventField(fields, start+14)),
		ExecutionSide:   strings.ToUpper(strings.TrimSpace(orderEventField(fields, start+16))),
		Shares:          orderEventFloat(orderEventField(fields, start+17)),
		Price:           orderEventFloat(orderEventField(fields, start+18)),
		PermID:          orderEventInt(orderEventField(fields, start+19)),
		ClientID:        orderEventInt(orderEventField(fields, start+20)),
		ClientIDPresent: len(fields) > start+20 && orderEventIntOK(orderEventField(fields, start+20)),
		CumQty:          orderEventFloat(orderEventField(fields, start+22)),
		AvgFillPrice:    orderEventFloat(orderEventField(fields, start+23)),
		OrderRef:        strings.TrimSpace(orderEventField(fields, start+24)),
		Raw:             append([]string{}, fields...),
	}
	return ev, ev.ExecID != "" && (ev.OrderID > 0 || ev.PermID > 0 || ev.OrderRef != "")
}

func parseOpenOrderProtoEvent(fields []string) (OrderLifecycleEvent, bool) {
	clientID, clientIDPresent := orderEventSummaryInt(fields, "clientId=")
	ev := OrderLifecycleEvent{
		AlgoKnown:        true,
		AlgoStrategy:     summaryFieldValue(fields, "algoStrategy="),
		AdaptivePriority: summaryFieldValue(fields, "adaptivePriority="),
		Type:             OrderLifecycleEventOpenOrder,
		OrderID:          orderEventInt(summaryFieldValue(fields, "orderId=")),
		ConID:            orderEventInt(summaryFieldValue(fields, "conId=")),
		PermID:           orderEventInt(summaryFieldValue(fields, "permId=")),
		ClientID:         clientID,
		ClientIDPresent:  clientIDPresent,
		Symbol:           strings.ToUpper(strings.TrimSpace(summaryFieldValue(fields, "symbol="))),
		SecType:          strings.ToUpper(strings.TrimSpace(summaryFieldValue(fields, "secType="))),
		Expiry:           strings.TrimSpace(summaryFieldValue(fields, "expiry=")),
		Strike:           orderEventFloat(summaryFieldValue(fields, "strike=")),
		Right:            strings.ToUpper(strings.TrimSpace(summaryFieldValue(fields, "right="))),
		Multiplier:       orderEventInt(summaryFieldValue(fields, "multiplier=")),
		Exchange:         strings.TrimSpace(summaryFieldValue(fields, "exchange=")),
		Currency:         strings.ToUpper(strings.TrimSpace(summaryFieldValue(fields, "currency="))),
		LocalSymbol:      strings.TrimSpace(summaryFieldValue(fields, "localSymbol=")),
		TradingClass:     strings.TrimSpace(summaryFieldValue(fields, "tradingClass=")),
		Action:           strings.ToUpper(strings.TrimSpace(summaryFieldValue(fields, "action="))),
		TotalQuantity:    orderEventFloat(summaryFieldValue(fields, "qty=")),
		OrderType:        strings.ToUpper(strings.TrimSpace(summaryFieldValue(fields, "orderType="))),
		LimitPrice:       orderEventFloat(summaryFieldValue(fields, "lmtPrice=")),
		AuxPrice:         orderEventFloat(summaryFieldValue(fields, "auxPrice=")),
		TrailingPercent:  orderEventFloat(summaryFieldValue(fields, "trailingPercent=")),
		TrailStopPrice:   orderEventFloat(summaryFieldValue(fields, "trailStopPrice=")),
		LmtPriceOffset:   orderEventFloat(summaryFieldValue(fields, "lmtPriceOffset=")),
		TIF:              strings.ToUpper(strings.TrimSpace(summaryFieldValue(fields, "tif="))),
		TriggerMethod:    orderEventInt(summaryFieldValue(fields, "triggerMethod=")),
		OutsideRth:       protoSummaryBool(fields, "outsideRth="),
		WhatIf:           protoSummaryBool(fields, "whatIf="),
		Account:          strings.TrimSpace(summaryFieldValue(fields, "account=")),
		OrderRef:         strings.TrimSpace(summaryFieldValue(fields, "orderRef=")),
		Status:           strings.TrimSpace(summaryFieldValue(fields, "status=")),
		Message:          orderEventWarningMessage(fields),
		Raw:              append([]string{}, fields...),
	}
	return ev, ev.OrderID > 0
}

func orderEventWarningMessage(fields []string) string {
	rejectReason := strings.TrimSpace(summaryFieldValue(fields, "rejectReason="))
	warningText := strings.TrimSpace(summaryFieldValue(fields, "warningText="))
	switch {
	case rejectReason != "" && warningText != "":
		return rejectReason + "; " + warningText
	case rejectReason != "":
		return rejectReason
	default:
		return warningText
	}
}

func parseOrderStatusProtoEvent(fields []string) (OrderLifecycleEvent, bool) {
	clientID, clientIDPresent := orderEventSummaryInt(fields, "clientId=")
	ev := OrderLifecycleEvent{
		Type:            OrderLifecycleEventStatus,
		OrderID:         orderEventInt(summaryFieldValue(fields, "orderId=")),
		ConID:           orderEventInt(summaryFieldValue(fields, "conId=")),
		Status:          strings.TrimSpace(summaryFieldValue(fields, "status=")),
		Filled:          orderEventFloat(summaryFieldValue(fields, "filled=")),
		Remaining:       orderEventFloat(summaryFieldValue(fields, "remaining=")),
		AvgFillPrice:    orderEventFloat(summaryFieldValue(fields, "avgFillPrice=")),
		PermID:          orderEventInt(summaryFieldValue(fields, "permId=")),
		LastFillPrice:   orderEventFloat(summaryFieldValue(fields, "lastFillPrice=")),
		ClientID:        clientID,
		ClientIDPresent: clientIDPresent,
		WhyHeld:         strings.TrimSpace(summaryFieldValue(fields, "whyHeld=")),
		MktCapPrice:     orderEventFloat(summaryFieldValue(fields, "mktCapPrice=")),
		Raw:             append([]string{}, fields...),
	}
	return ev, ev.OrderID > 0 && ev.Status != ""
}

func parseExecDetailsProtoEvent(fields []string) (OrderLifecycleEvent, bool) {
	clientID, clientIDPresent := orderEventSummaryInt(fields, "clientId=")
	ev := OrderLifecycleEvent{
		Type:            OrderLifecycleEventExecDetails,
		RequestID:       orderEventInt(summaryFieldValue(fields, "reqId=")),
		OrderID:         orderEventInt(summaryFieldValue(fields, "orderId=")),
		ConID:           orderEventInt(summaryFieldValue(fields, "conId=")),
		Symbol:          strings.ToUpper(strings.TrimSpace(summaryFieldValue(fields, "symbol="))),
		SecType:         strings.ToUpper(strings.TrimSpace(summaryFieldValue(fields, "secType="))),
		Expiry:          strings.TrimSpace(summaryFieldValue(fields, "expiry=")),
		Strike:          orderEventFloat(summaryFieldValue(fields, "strike=")),
		Right:           strings.ToUpper(strings.TrimSpace(summaryFieldValue(fields, "right="))),
		Multiplier:      orderEventInt(summaryFieldValue(fields, "multiplier=")),
		Exchange:        strings.TrimSpace(summaryFieldValue(fields, "exchange=")),
		Currency:        strings.ToUpper(strings.TrimSpace(summaryFieldValue(fields, "currency="))),
		LocalSymbol:     strings.TrimSpace(summaryFieldValue(fields, "localSymbol=")),
		ExecID:          strings.TrimSpace(summaryFieldValue(fields, "execId=")),
		ExecTime:        strings.TrimSpace(summaryFieldValue(fields, "execTime=")),
		Account:         strings.TrimSpace(summaryFieldValue(fields, "account=")),
		ExecutionSide:   strings.ToUpper(strings.TrimSpace(summaryFieldValue(fields, "executionSide="))),
		Shares:          orderEventFloat(summaryFieldValue(fields, "shares=")),
		Price:           orderEventFloat(summaryFieldValue(fields, "price=")),
		PermID:          orderEventInt(summaryFieldValue(fields, "permId=")),
		ClientID:        clientID,
		ClientIDPresent: clientIDPresent,
		CumQty:          orderEventFloat(summaryFieldValue(fields, "cumQty=")),
		AvgFillPrice:    orderEventFloat(summaryFieldValue(fields, "avgFillPrice=")),
		OrderRef:        strings.TrimSpace(summaryFieldValue(fields, "orderRef=")),
		Raw:             append([]string{}, fields...),
	}
	return ev, ev.ExecID != "" && (ev.OrderID > 0 || ev.PermID > 0 || ev.OrderRef != "")
}

func orderEventField(fields []string, idx int) string {
	if idx < 0 || idx >= len(fields) {
		return ""
	}
	return fields[idx]
}

func orderEventInt(s string) int {
	v, _ := strconv.Atoi(strings.TrimSpace(s))
	return v
}

func orderEventIntOK(s string) bool {
	_, err := strconv.Atoi(strings.TrimSpace(s))
	return err == nil
}

func orderEventSummaryInt(fields []string, prefix string) (int, bool) {
	for _, field := range fields {
		value, ok := strings.CutPrefix(field, prefix)
		if !ok || !orderEventIntOK(value) {
			continue
		}
		return orderEventInt(value), true
	}
	return 0, false
}

func orderEventFloat(s string) float64 {
	v, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return v
}
