package ibkr

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
)

const (
	// OrderWhatIfStatusUnavailable means no broker decision was obtained.
	OrderWhatIfStatusUnavailable = "unavailable"
	// OrderWhatIfStatusAccepted means the matching callback was not classified as rejected.
	// It is preview evidence, not order acceptance, submit authority, or a working order.
	OrderWhatIfStatusAccepted = "accepted"
	// OrderWhatIfStatusRejected means the broker callback or error rejected the preview.
	OrderWhatIfStatusRejected = "rejected"

	placeOrderFieldWhatIf = 100
)

// ErrOrderWhatIfUnavailable is the package sentinel for unavailable WhatIf
// evaluation. PreviewOrderWhatIf currently reports this condition through an
// [OrderWhatIfResult] with Status set to [OrderWhatIfStatusUnavailable] and a
// nil error; validation failures still return non-nil errors.
var ErrOrderWhatIfUnavailable = errors.New("order whatif unavailable")

// OrderWhatIfMargin is the broker's pre-trade margin and commission estimate
// returned on a WhatIf openOrder callback. Margin values are monetary amounts
// in Currency, while commission values use CommissionCurrency. A nil numeric
// pointer means the broker omitted the field or supplied an unusable sentinel;
// zero is a reported numeric value.
type OrderWhatIfMargin struct {
	Currency                string // Currency is the currency of the margin and equity values.
	InitialMarginBefore     *float64
	InitialMarginAfter      *float64
	MaintenanceMarginBefore *float64
	MaintenanceMarginAfter  *float64
	EquityWithLoanBefore    *float64
	EquityWithLoanAfter     *float64
	Commission              *float64
	MinCommission           *float64
	MaxCommission           *float64
	CommissionCurrency      string // CommissionCurrency is the currency of commission values.
	WarningText             string // WarningText is untrusted broker text and does not authorize submission.
}

// OrderWhatIfResult is a broker WhatIf evaluation. Status is one of the
// OrderWhatIfStatus constants. OrderID correlates this preview only, Message and
// AdvancedRejectJSON are untrusted broker text, and nil margin pointers mean
// the corresponding estimates were absent. The result is neither submit
// authority nor an order receipt.
type OrderWhatIfResult struct {
	OrderID            int    // OrderID is the request's broker order ID; zero means unavailable before allocation.
	Status             string // Status is the package-level WhatIf classification.
	BrokerStatus       string // BrokerStatus is the unnormalized status from the matching callback.
	Message            string // Message explains rejection or unavailability and may contain broker text.
	AdvancedRejectJSON string // AdvancedRejectJSON is opaque, untrusted broker rejection data.
	Margin             OrderWhatIfMargin
}

// PreviewOrderWhatIf sends a broker WhatIf order preview and waits for the
// matching openOrder or error callback. It is available in both build modes:
// the encoded request has WhatIf and Transmit set true for broker evaluation,
// but does not create a working order. Preview evidence never grants submit
// authority; unrestricted PlaceOrder and CancelOrder remain build-tag guarded.
//
// The method mutates order by applying defaults and IDs and by setting WhatIf
// and Transmit. Local validation or encoding failures are returned as errors.
// A disconnected connection, send failure, or ctx completion instead returns a
// result with Status [OrderWhatIfStatusUnavailable] and a nil error. Accepted
// and rejected results reflect callback classification, not order finality.
func (c *Connection) PreviewOrderWhatIf(ctx context.Context, order *IBKROrder) (OrderWhatIfResult, error) {
	return c.previewOrderWhatIfForEpoch(ctx, order, nil)
}

func (c *Connection) previewOrderWhatIfForEpoch(ctx context.Context, order *IBKROrder, expectedEpoch *uint64) (OrderWhatIfResult, error) {
	if order == nil {
		return OrderWhatIfResult{}, fmt.Errorf("order is nil")
	}
	if !c.IsConnected() {
		return orderWhatIfUnavailableResult("not connected to IBKR"), nil
	}
	epoch, err := preparePlaceOrder(order, c, expectedEpoch)
	if err != nil {
		return OrderWhatIfResult{}, err
	}
	order.WhatIf = true
	order.Transmit = true
	c.markWhatIfOrderID(order.OrderID)
	defer c.clearWhatIfOrderID(order.OrderID)

	resultCh := make(chan OrderWhatIfResult, 1)
	register := func(msgID int, handler func([]string)) uint64 {
		if expectedEpoch == nil {
			return c.RegisterHandler(msgID, handler)
		}
		return c.RegisterHandlerAtEpoch(msgID, func(fields []string, receiptEpoch uint64) {
			if receiptEpoch == *expectedEpoch {
				handler(fields)
			}
		})
	}
	errorHandlerID := register(msgErrMsg, func(fields []string) {
		reqID, code, msg, ok := parseOrderWhatIfError(fields)
		if !ok || reqID != order.OrderID || orderWhatIfInformationalError(code) {
			return
		}
		sendOrderWhatIfResult(resultCh, OrderWhatIfResult{
			OrderID:      order.OrderID,
			Status:       OrderWhatIfStatusRejected,
			BrokerStatus: "rejected",
			Message:      orderWhatIfErrorMessage(code, msg, ""),
		})
	})
	systemNoticeHandlerID := register(msgSystemNotification, func(fields []string) {
		reqID, code, msg, advancedRejectJSON, ok := parseOrderWhatIfSystemNotice(fields)
		if !ok || reqID != order.OrderID || orderWhatIfInformationalError(code) {
			return
		}
		sendOrderWhatIfResult(resultCh, OrderWhatIfResult{
			OrderID:            order.OrderID,
			Status:             OrderWhatIfStatusRejected,
			BrokerStatus:       "rejected",
			Message:            orderWhatIfErrorMessage(code, msg, advancedRejectJSON),
			AdvancedRejectJSON: advancedRejectJSON,
		})
	})
	openOrderHandlerID := register(msgOpenOrder, func(fields []string) {
		result, ok := parseOrderWhatIfOpenOrder(fields, order.OrderID, c.serverVersion)
		if ok {
			sendOrderWhatIfResult(resultCh, result)
		}
	})
	defer c.UnregisterHandler(msgErrMsg, errorHandlerID)
	defer c.UnregisterHandler(msgSystemNotification, systemNoticeHandlerID)
	defer c.UnregisterHandler(msgOpenOrder, openOrderHandlerID)
	// Every frame that names the order is recorded, so a WhatIf that times out
	// says what IBKR did send: a notice the handlers count as informational,
	// or an openOrder they could not read (a bond WhatIf timed out live on
	// 2026-10-07 with nothing in the log).
	trace := &whatIfFrameTrace{}
	releaseTap := c.tapInboundFrames(func(fields []string, receiptEpoch uint64) {
		if expectedEpoch != nil && receiptEpoch != *expectedEpoch {
			return
		}
		if line, ok := whatIfTapFrame(fields, order.OrderID); ok {
			trace.add(line)
		}
	})
	defer releaseTap()

	if err := c.sendPlaceOrderFrameGuarded(ctx, order, epoch, nil); err != nil {
		return orderWhatIfUnavailableResult(fmt.Sprintf("send broker WhatIf: %v", err)), nil
	}

	select {
	case <-ctx.Done():
		message := "timeout waiting for broker WhatIf response"
		if seen := trace.snapshot(); len(seen) > 0 {
			message += fmt.Sprintf(" (IBKR sent for order %d: %s)", order.OrderID, strings.Join(seen, "; "))
		} else {
			message += fmt.Sprintf(" (IBKR sent nothing naming order %d)", order.OrderID)
		}
		ibkrLogger.Warnf("WhatIf %s %s: %s", order.SecType, order.Symbol, message)
		return orderWhatIfUnavailableResult(message), nil
	case result := <-resultCh:
		if expectedEpoch != nil && c.BrokerSessionEpoch() != *expectedEpoch {
			return orderWhatIfUnavailableResult("broker session changed while waiting for WhatIf response"), nil
		}
		return result, nil
	}
}

// PreviewOrderWhatIf sends a broker WhatIf preview for a connector-level
// contract/order pair. Nil inputs fail validation. It does not mutate order or
// add to Connector.openOrders because no working order should exist. Status and
// error behavior match [Connection.PreviewOrderWhatIf].
func (c *Connector) PreviewOrderWhatIf(ctx context.Context, contract *Contract, order *RawOrder) (OrderWhatIfResult, error) {
	return c.previewOrderWhatIf(ctx, contract, order, 0, nil)
}

// PreviewOrderWhatIfForSession is PreviewOrderWhatIf constrained to the exact
// connector socket generation captured by binding.
func (c *Connector) PreviewOrderWhatIfForSession(ctx context.Context, binding ConnectorSessionBinding, contract *Contract, order *RawOrder) (OrderWhatIfResult, error) {
	return c.previewOrderWhatIf(ctx, contract, order, 0, &binding)
}

// PreviewOrderWhatIfWithOrderID sends a broker WhatIf preview using a positive,
// caller-supplied broker order ID. This supports evaluating a replacement draft
// for a tracked order, but does not modify that order or grant authority to do
// so. Status and error behavior match [Connection.PreviewOrderWhatIf].
func (c *Connector) PreviewOrderWhatIfWithOrderID(ctx context.Context, contract *Contract, order *RawOrder, orderID int) (OrderWhatIfResult, error) {
	if orderID <= 0 {
		return OrderWhatIfResult{}, fmt.Errorf("order ID must be positive")
	}
	return c.previewOrderWhatIf(ctx, contract, order, orderID, nil)
}

// PreviewOrderWhatIfWithOrderIDForSession is
// PreviewOrderWhatIfWithOrderID constrained to the exact connector socket
// generation captured by binding.
func (c *Connector) PreviewOrderWhatIfWithOrderIDForSession(ctx context.Context, binding ConnectorSessionBinding, contract *Contract, order *RawOrder, orderID int) (OrderWhatIfResult, error) {
	if orderID <= 0 {
		return OrderWhatIfResult{}, fmt.Errorf("order ID must be positive")
	}
	return c.previewOrderWhatIf(ctx, contract, order, orderID, &binding)
}

func (c *Connector) previewOrderWhatIf(ctx context.Context, contract *Contract, order *RawOrder, orderID int, binding *ConnectorSessionBinding) (OrderWhatIfResult, error) {
	if contract == nil {
		return OrderWhatIfResult{}, fmt.Errorf("contract is nil")
	}
	if order == nil {
		return OrderWhatIfResult{}, fmt.Errorf("order is nil")
	}
	if !c.isConnected() {
		return orderWhatIfUnavailableResult("not connected to IBKR"), nil
	}

	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if binding != nil {
		if !c.SessionCurrent(*binding) {
			return orderWhatIfUnavailableResult("broker session changed before WhatIf request"), nil
		}
		conn = binding.connection
	}
	if conn == nil {
		return orderWhatIfUnavailableResult("no active connection"), nil
	}

	ibkrOrder := &IBKROrder{
		ConID:           contract.ConID,
		Symbol:          contract.Symbol,
		SecType:         contract.SecType,
		Expiry:          contract.Expiry,
		Strike:          contract.Strike,
		Right:           contract.Right,
		Multiplier:      multiplierToString(contract.Multiplier),
		Exchange:        contract.Exchange,
		PrimaryExch:     contract.PrimaryExch,
		Currency:        contract.Currency,
		LocalSymbol:     contract.LocalSymbol,
		TradingClass:    contract.TradingClass,
		SecIDType:       contract.SecIDType,
		SecID:           contract.SecID,
		BondRules:       cloneBondOrderRules(contract.BondRules),
		ClientID:        order.ClientID,
		Action:          order.Action,
		TotalQty:        order.TotalQty,
		OrderType:       order.OrderType,
		LmtPrice:        order.LmtPrice,
		AuxPrice:        order.AuxPrice,
		TrailStopPrice:  order.TrailStopPrice,
		TrailingPercent: order.TrailingPercent,
		LmtPriceOffset:  order.LmtPriceOffset,
		TIF:             order.TIF,
		TriggerMethod:   order.TriggerMethod,
		OrderRef:        order.OrderRef,
		OutsideRth:      order.OutsideRth,
		Account:         order.Account,
		Transmit:        false,
		WhatIf:          true,
		OpenClose:       strings.ToUpper(strings.TrimSpace(order.OpenClose)),
		Origin:          0,
	}
	if ibkrOrder.OpenClose == "" {
		ibkrOrder.OpenClose = "O"
	}
	if orderID > 0 {
		ibkrOrder.OrderID = orderID
	}
	if c.whatIfBeforeBrokerIDClaim != nil {
		c.whatIfBeforeBrokerIDClaim()
	}
	var claimEpoch uint64
	c.brokerIDNamespaceMu.Lock()
	if ibkrOrder.OrderID <= 0 {
		var err error
		if binding != nil {
			ibkrOrder.OrderID, claimEpoch, err = c.nextDisjointOrderIDLockedForSession(*binding)
		} else {
			ibkrOrder.OrderID, claimEpoch, err = c.nextDisjointOrderIDLocked(conn)
		}
		if err != nil {
			c.brokerIDNamespaceMu.Unlock()
			return OrderWhatIfResult{}, err
		}
	} else {
		if c.feeRequestOwnsID(ibkrOrder.OrderID) {
			c.brokerIDNamespaceMu.Unlock()
			return OrderWhatIfResult{}, fmt.Errorf("%w: explicit WhatIf order ID is owned by an active read-only request", ErrBrokerIDNamespaceConflict)
		}
		owned := c.isKnownBrokerOrderID(ibkrOrder.OrderID)
		var err error
		if binding != nil {
			claimEpoch, err = conn.claimOrderIDForForwardingAtEpoch(ibkrOrder.OrderID, owned, &binding.epoch)
		} else {
			claimEpoch, err = conn.claimOrderIDForForwarding(ibkrOrder.OrderID, owned)
		}
		if err != nil {
			c.brokerIDNamespaceMu.Unlock()
			return OrderWhatIfResult{}, err
		}
	}
	defer conn.discardOrderIDReservation(ibkrOrder.OrderID)
	c.orderIDHighWater = max(c.orderIDHighWater, ibkrOrder.OrderID)
	// Mark under the shared boundary so a FEE allocation cannot race the
	// interval before Connection.PreviewOrderWhatIf installs its handlers.
	conn.markWhatIfOrderID(ibkrOrder.OrderID)
	defer conn.clearWhatIfOrderID(ibkrOrder.OrderID)
	c.brokerIDNamespaceMu.Unlock()
	result, err := conn.previewOrderWhatIfForEpoch(ctx, ibkrOrder, &claimEpoch)
	if err == nil && binding != nil && !c.SessionCurrent(*binding) {
		return orderWhatIfUnavailableResult("broker session changed during WhatIf request"), nil
	}
	return result, err
}

func preparePlaceOrder(order *IBKROrder, c *Connection, expectedEpoch *uint64) (uint64, error) {
	if err := ValidateOrder(order); err != nil {
		return 0, err
	}

	stringFields := []struct {
		name  string
		value string
	}{
		{"symbol", order.Symbol},
		{"secType", order.SecType},
		{"exchange", order.Exchange},
		{"currency", order.Currency},
		{"primary exchange", order.PrimaryExch},
		{"local symbol", order.LocalSymbol},
		{"trading class", order.TradingClass},
		{"account", order.Account},
		{"orderRef", order.OrderRef},
		{"tif", order.TIF},
		{"action", order.Action},
	}
	for _, field := range stringFields {
		if err := ensureASCII(field.name, field.value); err != nil {
			return 0, err
		}
	}

	claimEpoch := expectedEpoch
	if order.OrderID == 0 {
		id, epoch, err := c.reserveNextOrderID()
		if err != nil {
			return 0, err
		}
		order.OrderID = id
		claimEpoch = &epoch
	}
	var (
		epoch uint64
		err   error
	)
	if claimEpoch != nil {
		epoch, err = c.claimOrderIDForEpoch(order.OrderID, c.orderIDOwned(order.OrderID), *claimEpoch)
	} else {
		epoch, err = c.claimOrderIDCurrentEpoch(order.OrderID, c.orderIDOwned(order.OrderID))
	}
	if err != nil {
		return 0, err
	}
	if order.ClientID == 0 {
		order.ClientID = c.config.ClientID
	}
	if order.Account == "" {
		order.Account = c.account
	}
	if order.Account == "" {
		order.Account = c.config.Account
	}
	if order.OpenClose == "" {
		order.OpenClose = "O"
	}
	return epoch, nil
}

func (c *Connection) sendPlaceOrderFrameGuarded(ctx context.Context, order *IBKROrder, epoch uint64, guard func() error) error {
	if c.serverVersion >= minServerVerProtoBufPlaceOrder {
		msg, err := encodePlaceOrderProtoFrame(order)
		if err != nil {
			return definitelyUnsent(err)
		}
		return c.sendMessageWithTypeContextForEpochGuarded(ctx, msg, RequestTypeOrder, epoch, true, guard)
	}
	if order.LmtPriceOffset != 0 {
		return definitelyUnsent(fmt.Errorf("legacy placeOrder encoder does not support lmtPriceOffset"))
	}

	fields := clonePlaceOrderFields()
	assignPlaceOrderFields(fields, order)

	interfaces := make([]any, len(fields))
	interfaces[0] = placeOrder
	for i := 1; i < len(fields); i++ {
		interfaces[i] = fields[i]
	}

	msg := c.encodeMsg(interfaces...)
	return c.sendMessageWithTypeContextForEpochGuarded(ctx, msg, RequestTypeOrder, epoch, true, guard)
}

func orderWhatIfUnavailableResult(message string) OrderWhatIfResult {
	return OrderWhatIfResult{
		Status:  OrderWhatIfStatusUnavailable,
		Message: strings.TrimSpace(message),
	}
}

func sendOrderWhatIfResult(ch chan OrderWhatIfResult, result OrderWhatIfResult) {
	select {
	case ch <- result:
	default:
	}
}

func parseOrderWhatIfError(fields []string) (reqID, code int, message string, ok bool) {
	if len(fields) < 4 {
		return 0, 0, "", false
	}
	reqID, _ = strconv.Atoi(strings.TrimSpace(orderEventField(fields, 2)))
	code, _ = strconv.Atoi(strings.TrimSpace(orderEventField(fields, 3)))
	message = strings.TrimSpace(orderEventField(fields, 4))
	return reqID, code, message, code != 0
}

func parseOrderWhatIfSystemNotice(fields []string) (reqID, code int, message, advancedRejectJSON string, ok bool) {
	if len(fields) < 2 {
		return 0, 0, "", "", false
	}
	note, err := parseSystemNotificationPayload([]byte(fields[1]))
	if err != nil || note == nil || note.tickerID < 0 || note.code == 0 {
		return 0, 0, "", "", false
	}
	return int(note.tickerID), note.code, strings.TrimSpace(note.message), strings.TrimSpace(note.advancedOrderRejectJSON), true
}

// whatIfFrameTrace keeps the first frames that name one WhatIf order.
type whatIfFrameTrace struct {
	mu    sync.Mutex
	lines []string
}

const whatIfFrameTraceMax = 8

func (t *whatIfFrameTrace) add(line string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.lines) < whatIfFrameTraceMax {
		t.lines = append(t.lines, line)
	}
}

func (t *whatIfFrameTrace) snapshot() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.lines)
}

// whatIfTapFrame describes a frame that names orderID: an error or notice
// with IBKR's code and text, an openOrder with its WhatIf flag and status
// when it is a protobuf summary, or any other message by id and field count.
// ok is false for a frame about another order.
func whatIfTapFrame(fields []string, orderID int) (string, bool) {
	msgID, err := strconv.Atoi(strings.TrimSpace(safeGet(fields, 0)))
	if err != nil || orderID <= 0 {
		return "", false
	}
	switch msgID {
	case msgSystemNotification:
		note, err := parseSystemNotificationPayload([]byte(safeGet(fields, 1)))
		if err != nil || note.tickerID != int64(orderID) {
			return "", false
		}
		return fmt.Sprintf("notice %d: %s", note.code, brokerNoticeLine(note.message)), true
	case msgErrMsg:
		reqID, code, msg, ok := parseOrderWhatIfError(fields)
		if !ok || reqID != orderID {
			return "", false
		}
		return fmt.Sprintf("error %d: %s", code, brokerNoticeLine(msg)), true
	}
	id := strconv.Itoa(orderID)
	if strings.TrimSpace(safeGet(fields, 1)) != id && strings.TrimSpace(safeGet(fields, 2)) != id && !slices.Contains(fields, "orderId="+id) {
		return "", false
	}
	line := fmt.Sprintf("msg %d, %d fields", msgID, len(fields))
	if safeGet(fields, 1) == "protobuf" {
		line += fmt.Sprintf(" (whatIf=%s, status=%s, symbol=%s)", wireValue(summaryFieldValue(fields, "whatIf=")),
			wireValue(summaryFieldValue(fields, "status=")), wireValue(summaryFieldValue(fields, "symbol=")))
	}
	return line, true
}

func orderWhatIfInformationalError(code int) bool {
	switch code {
	case 2104, 2106, 2107, 2119, 2158, 2169:
		return true
	}
	return code >= 2100 && code < 2200
}

func orderWhatIfErrorMessage(code int, message, advancedRejectJSON string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		message = fmt.Sprintf("broker rejected WhatIf request with error code %d", code)
	} else {
		message = fmt.Sprintf("broker rejected WhatIf request with error code %d: %s", code, message)
	}
	if advancedRejectJSON = strings.TrimSpace(advancedRejectJSON); advancedRejectJSON != "" {
		message += " advanced_reject_json=" + advancedRejectJSON
	}
	return message
}

func parseOrderWhatIfOpenOrder(fields []string, orderID, serverVersion int) (OrderWhatIfResult, bool) {
	if len(fields) == 0 || strings.TrimSpace(fields[0]) != strconv.Itoa(msgOpenOrder) {
		return OrderWhatIfResult{}, false
	}
	if len(fields) > 1 && fields[1] == "protobuf" {
		return parseOrderWhatIfOpenOrderProto(fields, orderID)
	}
	start := 1
	if len(fields) > 2 && orderEventIntOK(fields[1]) {
		start = 2
	}
	if orderEventInt(orderEventField(fields, start)) != orderID {
		return OrderWhatIfResult{}, false
	}

	idx := findOrderWhatIfStateIndex(fields)
	if idx < 0 {
		return OrderWhatIfResult{}, false
	}
	brokerStatus := strings.TrimSpace(orderEventField(fields, idx+1))
	margin, rejectReason := parseOrderWhatIfMargin(fields, idx+2, serverVersion)
	status := OrderWhatIfStatusAccepted
	message := ""
	if orderWhatIfRejectedStatus(brokerStatus) || rejectReason != "" {
		status = OrderWhatIfStatusRejected
		message = rejectReason
		if message == "" {
			message = fmt.Sprintf("broker WhatIf status is %s", brokerStatus)
		}
	}
	return OrderWhatIfResult{
		OrderID:      orderID,
		Status:       status,
		BrokerStatus: brokerStatus,
		Message:      message,
		Margin:       margin,
	}, true
}

func parseOrderWhatIfOpenOrderProto(fields []string, orderID int) (OrderWhatIfResult, bool) {
	if orderEventInt(summaryFieldValue(fields, "orderId=")) != orderID {
		return OrderWhatIfResult{}, false
	}
	if !protoSummaryBool(fields, "whatIf=") {
		return OrderWhatIfResult{}, false
	}
	brokerStatus := strings.TrimSpace(summaryFieldValue(fields, "status="))
	if brokerStatus == "" {
		return OrderWhatIfResult{}, false
	}
	rejectReason := strings.TrimSpace(summaryFieldValue(fields, "rejectReason="))
	margin := OrderWhatIfMargin{
		InitialMarginBefore:     parseIBKRFloatPtr(summaryFieldValue(fields, "initMarginBefore=")),
		MaintenanceMarginBefore: parseIBKRFloatPtr(summaryFieldValue(fields, "maintMarginBefore=")),
		EquityWithLoanBefore:    parseIBKRFloatPtr(summaryFieldValue(fields, "equityWithLoanBefore=")),
		InitialMarginAfter:      parseIBKRFloatPtr(summaryFieldValue(fields, "initMarginAfter=")),
		MaintenanceMarginAfter:  parseIBKRFloatPtr(summaryFieldValue(fields, "maintMarginAfter=")),
		EquityWithLoanAfter:     parseIBKRFloatPtr(summaryFieldValue(fields, "equityWithLoanAfter=")),
		Commission:              parseIBKRFloatPtr(summaryFieldValue(fields, "commission=")),
		MinCommission:           parseIBKRFloatPtr(summaryFieldValue(fields, "minCommission=")),
		MaxCommission:           parseIBKRFloatPtr(summaryFieldValue(fields, "maxCommission=")),
		CommissionCurrency:      strings.TrimSpace(summaryFieldValue(fields, "commissionCurrency=")),
		Currency:                strings.TrimSpace(summaryFieldValue(fields, "marginCurrency=")),
		WarningText:             strings.TrimSpace(summaryFieldValue(fields, "warningText=")),
	}
	status := OrderWhatIfStatusAccepted
	message := ""
	if orderWhatIfRejectedStatus(brokerStatus) || rejectReason != "" {
		status = OrderWhatIfStatusRejected
		message = rejectReason
		if message == "" {
			message = fmt.Sprintf("broker WhatIf status is %s", brokerStatus)
		}
	}
	return OrderWhatIfResult{
		OrderID:      orderID,
		Status:       status,
		BrokerStatus: brokerStatus,
		Message:      message,
		Margin:       margin,
	}, true
}

func findOrderWhatIfStateIndex(fields []string) int {
	for i := 0; i+1 < len(fields); i++ {
		if !parseIBKRBool(fields[i]) {
			continue
		}
		if orderWhatIfStateStatus(fields[i+1]) {
			return i
		}
	}
	return -1
}

func orderWhatIfStateStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "pending", "pendingsubmit", "apisent", "apipending", "presubmitted", "submitted", "inactive", "cancelled", "apicancelled", "rejected":
		return true
	default:
		return false
	}
}

func orderWhatIfRejectedStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "inactive", "cancelled", "apicancelled", "rejected":
		return true
	default:
		return false
	}
}

func parseOrderWhatIfMargin(fields []string, idx, serverVersion int) (OrderWhatIfMargin, string) {
	var margin OrderWhatIfMargin
	if serverVersion >= 142 {
		margin.InitialMarginBefore = parseIBKRFloatPtr(orderEventField(fields, idx))
		margin.MaintenanceMarginBefore = parseIBKRFloatPtr(orderEventField(fields, idx+1))
		margin.EquityWithLoanBefore = parseIBKRFloatPtr(orderEventField(fields, idx+2))
		idx += 6 // skip before/change; the public shape exposes before/after.
	}
	margin.InitialMarginAfter = parseIBKRFloatPtr(orderEventField(fields, idx))
	margin.MaintenanceMarginAfter = parseIBKRFloatPtr(orderEventField(fields, idx+1))
	margin.EquityWithLoanAfter = parseIBKRFloatPtr(orderEventField(fields, idx+2))
	margin.Commission = parseIBKRFloatPtr(orderEventField(fields, idx+3))
	margin.MinCommission = parseIBKRFloatPtr(orderEventField(fields, idx+4))
	margin.MaxCommission = parseIBKRFloatPtr(orderEventField(fields, idx+5))
	margin.CommissionCurrency = strings.TrimSpace(orderEventField(fields, idx+6))
	idx += 7

	rejectReason := ""
	if serverVersion >= 195 {
		margin.Currency = strings.TrimSpace(orderEventField(fields, idx))
		idx += 11 // currency + outside-RTH fields + suggestedSize
		rejectReason = strings.TrimSpace(orderEventField(fields, idx))
		idx++
		allocationsCount := orderEventInt(orderEventField(fields, idx))
		idx++
		if allocationsCount > 0 {
			idx += allocationsCount * 7
		}
	}
	margin.WarningText = strings.TrimSpace(orderEventField(fields, idx))
	return margin, rejectReason
}

func parseIBKRFloatPtr(raw string) *float64 {
	s := strings.TrimSpace(strings.ReplaceAll(raw, ",", ""))
	if s == "" || strings.EqualFold(s, "N/A") || strings.EqualFold(s, "NaN") {
		return nil
	}
	value, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsInf(value, 0) || math.IsNaN(value) || value == math.MaxFloat64 {
		return nil
	}
	return &value
}

func parseIBKRBool(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}
