package ibkr

import (
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"
)

const (
	protoWireVarint  = 0
	protoWireFixed64 = 1
	protoWireBytes   = 2

	protoPlaceOrderMsgID = placeOrder + protoBufMsgID

	minProtoInt32 = -1 << 31
	maxProtoInt32 = 1<<31 - 1
)

func encodePlaceOrderProtoFrame(order *IBKROrder) ([]byte, error) {
	body, err := encodePlaceOrderProtoBody(order)
	if err != nil {
		return nil, err
	}
	msg := make([]byte, 0, 4+len(body))
	msg = binary.BigEndian.AppendUint32(msg, uint32(protoPlaceOrderMsgID))
	msg = append(msg, body...)
	return msg, nil
}

func encodePlaceOrderProtoBody(order *IBKROrder) ([]byte, error) {
	if err := validatePlaceOrderProtoSupported(order); err != nil {
		return nil, err
	}

	contract, err := encodePlaceOrderContractProto(order)
	if err != nil {
		return nil, err
	}
	orderMsg, err := encodePlaceOrderOrderProto(order)
	if err != nil {
		return nil, err
	}

	var body []byte
	body = protoAppendInt32(body, 1, int32(order.OrderID))
	body = protoAppendMessage(body, 2, contract)
	body = protoAppendMessage(body, 3, orderMsg)
	body = protoAppendMessage(body, 4, nil)
	return body, nil
}

func encodePlaceOrderContractProto(order *IBKROrder) ([]byte, error) {
	var msg []byte
	msg = protoAppendInt32(msg, 1, int32(order.ConID))
	msg = protoAppendString(msg, 2, order.Symbol)
	msg = protoAppendString(msg, 3, strings.ToUpper(order.SecType))
	msg = protoAppendString(msg, 4, order.Expiry)
	if order.Strike != 0 {
		msg = protoAppendDouble(msg, 5, order.Strike)
	}
	msg = protoAppendString(msg, 6, strings.ToUpper(order.Right))
	msg = protoAppendString(msg, 7, order.Multiplier)
	msg = protoAppendString(msg, 8, order.Exchange)
	msg = protoAppendString(msg, 9, order.PrimaryExch)
	msg = protoAppendString(msg, 10, order.Currency)
	msg = protoAppendString(msg, 11, order.LocalSymbol)
	msg = protoAppendString(msg, 12, order.TradingClass)
	msg = protoAppendString(msg, 13, order.SecIDType)
	msg = protoAppendString(msg, 14, order.SecID)
	for _, leg := range order.ComboLegs {
		msg = protoAppendMessage(msg, 20, encodeComboLegProto(leg))
	}
	return msg, nil
}

func encodeComboLegProto(leg ComboLeg) []byte {
	var msg []byte
	msg = protoAppendInt32(msg, 1, int32(leg.ConID))
	msg = protoAppendInt32(msg, 2, int32(leg.Ratio))
	msg = protoAppendString(msg, 3, strings.ToUpper(strings.TrimSpace(leg.Action)))
	msg = protoAppendString(msg, 4, strings.ToUpper(strings.TrimSpace(leg.Exchange)))
	if leg.OpenClose != 0 {
		msg = protoAppendInt32(msg, 5, int32(leg.OpenClose))
	}
	return msg
}

func encodePlaceOrderOrderProto(order *IBKROrder) ([]byte, error) {
	var msg []byte
	msg = protoAppendInt32(msg, 1, int32(order.ClientID))
	msg = protoAppendInt64(msg, 3, int64(order.PermID))
	msg = protoAppendInt32(msg, 4, int32(order.ParentID))
	msg = protoAppendString(msg, 5, strings.ToUpper(order.Action))
	msg = protoAppendString(msg, 6, strconv.Itoa(order.TotalQty))
	msg = protoAppendInt32(msg, 7, int32(order.DisplaySize))
	msg = protoAppendString(msg, 8, strings.ToUpper(order.OrderType))
	if order.LmtPrice != 0 || order.LmtPriceSet {
		msg = protoAppendDouble(msg, 9, order.LmtPrice)
	}
	if order.AuxPrice != 0 {
		msg = protoAppendDouble(msg, 10, order.AuxPrice)
	}
	msg = protoAppendString(msg, 11, strings.ToUpper(order.TIF))
	msg = protoAppendString(msg, 12, order.Account)
	if order.OutsideRth {
		msg = protoAppendBool(msg, 19, true)
	}
	msg = protoAppendString(msg, 28, order.OrderRef)
	msg = protoAppendInt32(msg, 30, int32(order.OcaType))
	msg = protoAppendInt32(msg, 31, int32(order.TriggerMethod))
	if order.TrailingPercent != 0 {
		msg = protoAppendDouble(msg, 22, order.TrailingPercent)
	}
	if order.TrailStopPrice != 0 {
		msg = protoAppendDouble(msg, 23, order.TrailStopPrice)
	}
	msg = protoAppendInt32(msg, 43, int32(order.DeltaNeutralConID))
	msg = protoAppendInt32(msg, 46, int32(order.DeltaNeutralShortSaleSlot))
	if order.WhatIf {
		msg = protoAppendBool(msg, 65, true)
	}
	if order.Transmit {
		msg = protoAppendBool(msg, 66, true)
	}
	msg = protoAppendString(msg, 68, order.OpenClose)
	msg = protoAppendInt32(msg, 69, int32(order.Origin))
	msg = protoAppendInt32(msg, 70, int32(order.ShortSaleSlot))
	msg = protoAppendInt32(msg, 72, -1)
	msg = protoAppendDouble(msg, 76, order.DiscretionaryAmt)
	msg = protoAppendInt32(msg, 88, 0)
	msg = protoAppendDouble(msg, 89, 0)
	msg = protoAppendDouble(msg, 91, 0)
	if order.LmtPriceOffset != 0 {
		msg = protoAppendDouble(msg, 99, order.LmtPriceOffset)
	}
	msg = protoAppendInt32(msg, 98, 0)
	msg = protoAppendMessage(msg, 105, nil)
	return msg, nil
}

func validatePlaceOrderProtoSupported(order *IBKROrder) error {
	if order == nil {
		return fmt.Errorf("order is nil")
	}
	for _, field := range []struct {
		name  string
		value int
	}{
		{"orderID", order.OrderID},
		{"clientID", order.ClientID},
		{"conID", order.ConID},
		{"totalQty", order.TotalQty},
	} {
		if err := validateProtoInt32(field.name, field.value); err != nil {
			return err
		}
	}

	secType := strings.ToUpper(order.SecType)
	if secType != "STK" && secType != "ETF" && secType != "OPT" && secType != "BAG" {
		return unsupportedPlaceOrderProtoValue("secType", order.SecType, "STK/ETF/OPT/BAG only")
	}
	orderType := strings.ToUpper(strings.TrimSpace(order.OrderType))
	if orderType != "LMT" && orderType != "TRAIL" && orderType != "TRAIL LIMIT" {
		return unsupportedPlaceOrderProtoValue("orderType", order.OrderType, "LMT/TRAIL/TRAIL LIMIT only")
	}
	if err := validatePlaceOrderTriggerMethod(orderType, order.TriggerMethod); err != nil {
		return err
	}
	tif := strings.ToUpper(order.TIF)
	gtcTrail := tif == "GTC" && (orderType == "TRAIL" || orderType == "TRAIL LIMIT")
	if tif != "DAY" && !gtcTrail {
		return unsupportedPlaceOrderProtoValue("tif", order.TIF, "DAY, or GTC for TRAIL/TRAIL LIMIT")
	}
	if err := validateProtoOrderTypePrices(orderType, order); err != nil {
		return err
	}
	if secType == "BAG" {
		if err := validateComboLegs(order.ComboLegs); err != nil {
			return err
		}
		for _, field := range []struct {
			name  string
			value string
		}{
			{"expiry", order.Expiry},
			{"right", order.Right},
			{"multiplier", order.Multiplier},
		} {
			if field.value != "" {
				return unsupportedPlaceOrderProtoField(field.name)
			}
		}
		if order.Strike != 0 {
			return unsupportedPlaceOrderProtoField("strike")
		}
	} else if secType == "OPT" {
		if strings.TrimSpace(order.Expiry) == "" {
			return fmt.Errorf("protobuf placeOrder OPT requires expiry")
		}
		right := strings.ToUpper(strings.TrimSpace(order.Right))
		if right != "C" && right != "P" {
			return unsupportedPlaceOrderProtoValue("right", order.Right, "C/P only")
		}
		if order.Strike <= 0 {
			return fmt.Errorf("protobuf placeOrder OPT requires positive strike")
		}
		if strings.TrimSpace(order.Multiplier) == "" {
			return fmt.Errorf("protobuf placeOrder OPT requires multiplier")
		}
	} else {
		if len(order.ComboLegs) != 0 {
			return unsupportedPlaceOrderProtoField("comboLegs")
		}
		for _, field := range []struct {
			name  string
			value string
		}{
			{"expiry", order.Expiry},
			{"right", order.Right},
			{"multiplier", order.Multiplier},
		} {
			if field.value != "" {
				return unsupportedPlaceOrderProtoField(field.name)
			}
		}
		if order.Strike != 0 {
			return unsupportedPlaceOrderProtoField("strike")
		}
	}

	for _, field := range []struct {
		name  string
		value string
	}{
		{"ocaGroup", order.OcaGroup},
		{"goodAfterTime", order.GoodAfterTime},
		{"goodTillDate", order.GoodTillDate},
		{"faGroup", order.FaGroup},
		{"faMethod", order.FaMethod},
		{"faPercentage", order.FaPercentage},
		{"faProfile", order.FaProfile},
		{"modelCode", order.ModelCode},
		{"designatedLocation", order.DesignatedLocation},
		{"rule80A", order.Rule80A},
		{"settlingFirm", order.SettlingFirm},
		{"deltaNeutralOrderType", order.DeltaNeutralOrderType},
		{"deltaNeutralSettlingFirm", order.DeltaNeutralSettlingFirm},
		{"deltaNeutralClearingAccount", order.DeltaNeutralClearingAccount},
		{"deltaNeutralClearingIntent", order.DeltaNeutralClearingIntent},
		{"deltaNeutralOpenClose", order.DeltaNeutralOpenClose},
		{"deltaNeutralDesignatedLocation", order.DeltaNeutralDesignatedLocation},
		{"hedgeType", order.HedgeType},
		{"hedgeParam", order.HedgeParam},
		{"clearingAccount", order.ClearingAccount},
		{"clearingIntent", order.ClearingIntent},
	} {
		if field.value != "" {
			return unsupportedPlaceOrderProtoField(field.name)
		}
	}

	for _, field := range []struct {
		name  string
		value int
	}{
		{"permID", order.PermID},
		{"parentID", order.ParentID},
		{"displaySize", order.DisplaySize},
		{"ocaType", order.OcaType},
		{"shortSaleSlot", order.ShortSaleSlot},
		{"minQty", order.MinQty},
		{"auctionStrategy", order.AuctionStrategy},
		{"volatilityType", order.VolatilityType},
		{"deltaNeutralConID", order.DeltaNeutralConID},
		{"deltaNeutralShortSaleSlot", order.DeltaNeutralShortSaleSlot},
		{"continuousUpdate", order.ContinuousUpdate},
		{"referencePriceType", order.ReferencePriceType},
		{"basisPointsType", order.BasisPointsType},
		{"scaleInitLevelSize", order.ScaleInitLevelSize},
		{"scaleSubsLevelSize", order.ScaleSubsLevelSize},
		{"scalePriceAdjustInterval", order.ScalePriceAdjustInterval},
		{"scaleInitPosition", order.ScaleInitPosition},
		{"scaleInitFillQty", order.ScaleInitFillQty},
	} {
		if field.value != 0 {
			return unsupportedPlaceOrderProtoField(field.name)
		}
	}

	if order.ExemptCode != 0 && order.ExemptCode != -1 {
		return unsupportedPlaceOrderProtoField("exemptCode")
	}
	openClose := strings.ToUpper(strings.TrimSpace(order.OpenClose))
	if openClose != "" && openClose != "O" && openClose != "C" {
		return unsupportedPlaceOrderProtoValue("openClose", order.OpenClose, "O/C only")
	}

	for _, field := range []struct {
		name  string
		value float64
	}{
		{"discretionaryAmt", order.DiscretionaryAmt},
		{"percentOffset", order.PercentOffset},
		{"nbboPriceCap", order.NbboPriceCap},
		{"startingPrice", order.StartingPrice},
		{"stockRefPrice", order.StockRefPrice},
		{"delta", order.Delta},
		{"stockRangeLower", order.StockRangeLower},
		{"stockRangeUpper", order.StockRangeUpper},
		{"volatility", order.Volatility},
		{"deltaNeutralAuxPrice", order.DeltaNeutralAuxPrice},
		{"basisPoints", order.BasisPoints},
		{"scalePriceIncrement", order.ScalePriceIncrement},
		{"scalePriceAdjustValue", order.ScalePriceAdjustValue},
		{"scaleProfitOffset", order.ScaleProfitOffset},
	} {
		if field.value != 0 {
			return unsupportedPlaceOrderProtoField(field.name)
		}
	}

	for _, field := range []struct {
		name  string
		value bool
	}{
		{"blockOrder", order.BlockOrder},
		{"sweepToFill", order.SweepToFill},
		{"hidden", order.Hidden},
		{"allOrNone", order.AllOrNone},
		{"eTradeOnly", order.ETradeOnly},
		{"firmQuoteOnly", order.FirmQuoteOnly},
		{"overridePercentageConstraints", order.OverridePercentageConstraints},
		{"deltaNeutralShortSale", order.DeltaNeutralShortSale},
		{"scaleAutoReset", order.ScaleAutoReset},
		{"scaleRandomPercent", order.ScaleRandomPercent},
		{"optOutSmartRouting", order.OptOutSmartRouting},
		{"notHeld", order.NotHeld},
	} {
		if field.value {
			return unsupportedPlaceOrderProtoField(field.name)
		}
	}

	return nil
}

func validateComboLegs(legs []ComboLeg) error {
	if len(legs) < 2 {
		return fmt.Errorf("protobuf placeOrder BAG requires at least two combo legs")
	}
	seen := make(map[int]struct{}, len(legs))
	for i, leg := range legs {
		if err := validateProtoInt32(fmt.Sprintf("comboLegs[%d].conID", i), leg.ConID); err != nil {
			return err
		}
		if err := validateProtoInt32(fmt.Sprintf("comboLegs[%d].ratio", i), leg.Ratio); err != nil {
			return err
		}
		if leg.ConID <= 0 {
			return fmt.Errorf("protobuf placeOrder BAG combo leg %d requires a positive ConID", i)
		}
		if leg.Ratio <= 0 {
			return fmt.Errorf("protobuf placeOrder BAG combo leg %d requires a positive ratio", i)
		}
		action := strings.ToUpper(strings.TrimSpace(leg.Action))
		if action != "BUY" && action != "SELL" {
			return unsupportedPlaceOrderProtoValue(fmt.Sprintf("comboLegs[%d].action", i), leg.Action, "BUY/SELL only")
		}
		if strings.TrimSpace(leg.Exchange) == "" {
			return fmt.Errorf("protobuf placeOrder BAG combo leg %d requires an exchange", i)
		}
		if leg.OpenClose < 0 || leg.OpenClose > 3 {
			return unsupportedPlaceOrderProtoValue(fmt.Sprintf("comboLegs[%d].openClose", i), strconv.Itoa(leg.OpenClose), "IBKR values 0-3")
		}
		if _, exists := seen[leg.ConID]; exists {
			return fmt.Errorf("protobuf placeOrder BAG repeats combo leg ConID %d", leg.ConID)
		}
		seen[leg.ConID] = struct{}{}
	}
	return nil
}

func validatePlaceOrderTriggerMethod(orderType string, method int) error {
	if method == 0 {
		return nil
	}
	if orderType != "TRAIL" && orderType != "TRAIL LIMIT" {
		return unsupportedPlaceOrderProtoValue("triggerMethod", strconv.Itoa(method), "only for TRAIL/TRAIL LIMIT")
	}
	switch method {
	case 1, 2, 3, 4, 7, 8:
		return nil
	default:
		return unsupportedPlaceOrderProtoValue("triggerMethod", strconv.Itoa(method), "IBKR values 1,2,3,4,7,8")
	}
}

func validateProtoOrderTypePrices(orderType string, order *IBKROrder) error {
	hasAmount := order.AuxPrice > 0
	hasPercent := order.TrailingPercent > 0
	switch orderType {
	case "LMT":
		if strings.EqualFold(order.SecType, "BAG") {
			if !order.LmtPriceSet || math.IsNaN(order.LmtPrice) || math.IsInf(order.LmtPrice, 0) {
				return fmt.Errorf("protobuf placeOrder BAG requires a finite explicit lmtPrice")
			}
		} else if order.LmtPrice <= 0 {
			return fmt.Errorf("protobuf placeOrder requires positive lmtPrice")
		}
		if order.AuxPrice != 0 || order.TrailStopPrice != 0 || order.TrailingPercent != 0 || order.LmtPriceOffset != 0 {
			return fmt.Errorf("protobuf placeOrder LMT does not support populated trail/auxiliary price fields")
		}
	case "TRAIL":
		if order.LmtPrice != 0 {
			return unsupportedPlaceOrderProtoField("lmtPrice")
		}
		if order.TrailStopPrice <= 0 {
			return fmt.Errorf("protobuf placeOrder TRAIL requires positive trailStopPrice")
		}
		if hasAmount == hasPercent {
			return fmt.Errorf("protobuf placeOrder TRAIL requires exactly one of auxPrice or trailingPercent")
		}
		if order.LmtPriceOffset != 0 {
			return unsupportedPlaceOrderProtoField("lmtPriceOffset")
		}
	case "TRAIL LIMIT":
		if order.LmtPrice != 0 {
			return fmt.Errorf("protobuf placeOrder TRAIL LIMIT must use lmtPriceOffset, not lmtPrice")
		}
		if order.TrailStopPrice <= 0 {
			return fmt.Errorf("protobuf placeOrder TRAIL LIMIT requires positive trailStopPrice")
		}
		if hasAmount == hasPercent {
			return fmt.Errorf("protobuf placeOrder TRAIL LIMIT requires exactly one of auxPrice or trailingPercent")
		}
		if order.LmtPriceOffset <= 0 {
			return fmt.Errorf("protobuf placeOrder TRAIL LIMIT requires positive lmtPriceOffset")
		}
	}
	return nil
}

func validateProtoInt32(name string, value int) error {
	if value < minProtoInt32 || value > maxProtoInt32 {
		return fmt.Errorf("protobuf placeOrder %s=%d is outside int32 range", name, value)
	}
	return nil
}

func unsupportedPlaceOrderProtoField(name string) error {
	return fmt.Errorf("protobuf placeOrder does not support populated field %s in this order slice", name)
}

func unsupportedPlaceOrderProtoValue(name, value, supported string) error {
	if value == "" {
		value = "<empty>"
	}
	return fmt.Errorf("protobuf placeOrder does not support %s=%q in this order slice (%s)", name, value, supported)
}

func protoAppendTag(buf []byte, fieldNumber, wireType int) []byte {
	return binary.AppendUvarint(buf, uint64(fieldNumber<<3|wireType))
}

func protoAppendInt32(buf []byte, fieldNumber int, value int32) []byte {
	buf = protoAppendTag(buf, fieldNumber, protoWireVarint)
	return binary.AppendUvarint(buf, uint64(int64(value)))
}

func protoAppendInt64(buf []byte, fieldNumber int, value int64) []byte {
	buf = protoAppendTag(buf, fieldNumber, protoWireVarint)
	return binary.AppendUvarint(buf, uint64(value))
}

func protoAppendBool(buf []byte, fieldNumber int, value bool) []byte {
	buf = protoAppendTag(buf, fieldNumber, protoWireVarint)
	if value {
		return append(buf, 1)
	}
	return append(buf, 0)
}

func protoAppendDouble(buf []byte, fieldNumber int, value float64) []byte {
	buf = protoAppendTag(buf, fieldNumber, protoWireFixed64)
	return binary.LittleEndian.AppendUint64(buf, math.Float64bits(value))
}

func protoAppendString(buf []byte, fieldNumber int, value string) []byte {
	if value == "" {
		return buf
	}
	buf = protoAppendTag(buf, fieldNumber, protoWireBytes)
	buf = binary.AppendUvarint(buf, uint64(len(value)))
	return append(buf, value...)
}

func protoAppendMessage(buf []byte, fieldNumber int, value []byte) []byte {
	buf = protoAppendTag(buf, fieldNumber, protoWireBytes)
	buf = binary.AppendUvarint(buf, uint64(len(value)))
	return append(buf, value...)
}

func (c *Connection) decodeOutboundMessage(msgBytes []byte) []string {
	if c.serverVersion >= minServerVerProtoBufPlaceOrder {
		switch determineMessageID(c.serverVersion, msgBytes) {
		case protoPlaceOrderMsgID:
			return summarizePlaceOrderProtoFrame(msgBytes)
		case protoCancelOrderMsgID:
			return summarizeCancelOrderProtoFrame(msgBytes)
		}
	}
	return c.decodeMessage(msgBytes)
}

func summarizePlaceOrderProtoFrame(msgBytes []byte) []string {
	fields := []string{strconv.Itoa(protoPlaceOrderMsgID), "protobuf", "base_msg_id=" + strconv.Itoa(placeOrder)}
	if len(msgBytes) < 4 {
		return append(fields, "decode_error=truncated")
	}
	summary, err := parsePlaceOrderProtoSummary(msgBytes[4:])
	if err != nil {
		return append(fields, "decode_error="+err.Error())
	}
	fields = append(fields,
		"orderId="+strconv.Itoa(summary.orderID),
		"conId="+strconv.Itoa(summary.conID),
		"symbol="+summary.symbol,
		"secType="+summary.secType,
		"expiry="+summary.expiry,
		"strike="+formatProtoSummaryFloat(summary.strike),
		"right="+summary.right,
		"multiplier="+summary.multiplier,
		"clientID="+strconv.Itoa(summary.clientID),
		"action="+summary.action,
		"qty="+summary.quantity,
		"orderType="+summary.orderType,
		"lmtPrice="+formatProtoSummaryFloat(summary.lmtPrice),
		"auxPrice="+formatProtoSummaryFloat(summary.auxPrice),
		"trailingPercent="+formatProtoSummaryFloat(summary.trailingPercent),
		"trailStopPrice="+formatProtoSummaryFloat(summary.trailStopPrice),
		"lmtPriceOffset="+formatProtoSummaryFloat(summary.lmtPriceOffset),
		"tif="+summary.tif,
		"triggerMethod="+strconv.Itoa(summary.triggerMethod),
		"account="+summary.account,
		"orderRef="+summary.orderRef,
		"outsideRth="+strconv.FormatBool(summary.outsideRth),
		"whatIf="+strconv.FormatBool(summary.whatIf),
		"transmit="+strconv.FormatBool(summary.transmit),
		"openClose="+summary.openClose,
	)
	return fields
}

type placeOrderProtoSummary struct {
	orderID         int
	conID           int
	symbol          string
	secType         string
	expiry          string
	strike          float64
	right           string
	multiplier      string
	clientID        int
	action          string
	quantity        string
	orderType       string
	lmtPrice        float64
	auxPrice        float64
	trailingPercent float64
	trailStopPrice  float64
	lmtPriceOffset  float64
	tif             string
	triggerMethod   int
	account         string
	orderRef        string
	outsideRth      bool
	whatIf          bool
	transmit        bool
	openClose       string
}

func parsePlaceOrderProtoSummary(body []byte) (placeOrderProtoSummary, error) {
	var summary placeOrderProtoSummary
	err := forEachProtoField(body, func(fieldNumber, wireType int, value []byte) error {
		switch fieldNumber {
		case 1:
			v, err := protoVarintValue(fieldNumber, wireType, value)
			if err != nil {
				return err
			}
			summary.orderID = int(v)
		case 2:
			return parseContractProtoSummary(value, &summary)
		case 3:
			return parseOrderProtoSummary(value, &summary)
		}
		return nil
	})
	return summary, err
}

func parseContractProtoSummary(body []byte, summary *placeOrderProtoSummary) error {
	return forEachProtoField(body, func(fieldNumber, wireType int, value []byte) error {
		switch fieldNumber {
		case 1:
			v, err := protoVarintValue(fieldNumber, wireType, value)
			if err != nil {
				return err
			}
			summary.conID = int(v)
		case 2:
			summary.symbol = string(value)
		case 3:
			summary.secType = string(value)
		case 4:
			summary.expiry = string(value)
		case 5:
			v, err := protoFixed64Value(fieldNumber, wireType, value)
			if err != nil {
				return err
			}
			summary.strike = math.Float64frombits(v)
		case 6:
			summary.right = string(value)
		case 7:
			summary.multiplier = string(value)
		}
		return nil
	})
}

func parseOrderProtoSummary(body []byte, summary *placeOrderProtoSummary) error {
	return forEachProtoField(body, func(fieldNumber, wireType int, value []byte) error {
		switch fieldNumber {
		case 1:
			v, err := protoVarintValue(fieldNumber, wireType, value)
			if err != nil {
				return err
			}
			summary.clientID = int(v)
		case 5:
			summary.action = string(value)
		case 6:
			summary.quantity = string(value)
		case 8:
			summary.orderType = string(value)
		case 9:
			v, err := protoFixed64Value(fieldNumber, wireType, value)
			if err != nil {
				return err
			}
			summary.lmtPrice = math.Float64frombits(v)
		case 10:
			v, err := protoFixed64Value(fieldNumber, wireType, value)
			if err != nil {
				return err
			}
			summary.auxPrice = math.Float64frombits(v)
		case 11:
			summary.tif = string(value)
		case 12:
			summary.account = string(value)
		case 19:
			v, err := protoVarintValue(fieldNumber, wireType, value)
			if err != nil {
				return err
			}
			summary.outsideRth = v != 0
		case 28:
			summary.orderRef = string(value)
		case 31:
			v, err := protoVarintValue(fieldNumber, wireType, value)
			if err != nil {
				return err
			}
			summary.triggerMethod = int(v)
		case 22:
			v, err := protoFixed64Value(fieldNumber, wireType, value)
			if err != nil {
				return err
			}
			summary.trailingPercent = math.Float64frombits(v)
		case 23:
			v, err := protoFixed64Value(fieldNumber, wireType, value)
			if err != nil {
				return err
			}
			summary.trailStopPrice = math.Float64frombits(v)
		case 65:
			v, err := protoVarintValue(fieldNumber, wireType, value)
			if err != nil {
				return err
			}
			summary.whatIf = v != 0
		case 66:
			v, err := protoVarintValue(fieldNumber, wireType, value)
			if err != nil {
				return err
			}
			summary.transmit = v != 0
		case 68:
			summary.openClose = string(value)
		case 99:
			v, err := protoFixed64Value(fieldNumber, wireType, value)
			if err != nil {
				return err
			}
			summary.lmtPriceOffset = math.Float64frombits(v)
		}
		return nil
	})
}

func forEachProtoField(buf []byte, fn func(fieldNumber, wireType int, value []byte) error) error {
	for len(buf) > 0 {
		tag, n := binary.Uvarint(buf)
		if n <= 0 {
			return fmt.Errorf("invalid_tag")
		}
		buf = buf[n:]
		fieldNumber := int(tag >> 3)
		wireType := int(tag & 0x7)
		var value []byte
		switch wireType {
		case protoWireVarint:
			_, n := binary.Uvarint(buf)
			if n <= 0 {
				return fmt.Errorf("invalid_varint_field_%d", fieldNumber)
			}
			value = buf[:n]
			buf = buf[n:]
		case protoWireFixed64:
			if len(buf) < 8 {
				return fmt.Errorf("truncated_fixed64_field_%d", fieldNumber)
			}
			value = buf[:8]
			buf = buf[8:]
		case protoWireBytes:
			length, n := binary.Uvarint(buf)
			if n <= 0 {
				return fmt.Errorf("invalid_length_field_%d", fieldNumber)
			}
			buf = buf[n:]
			if uint64(len(buf)) < length {
				return fmt.Errorf("truncated_bytes_field_%d", fieldNumber)
			}
			value = buf[:length]
			buf = buf[length:]
		default:
			return fmt.Errorf("unsupported_wire_%d_field_%d", wireType, fieldNumber)
		}
		if err := fn(fieldNumber, wireType, value); err != nil {
			return err
		}
	}
	return nil
}

func protoVarintValue(fieldNumber, wireType int, value []byte) (uint64, error) {
	if wireType != protoWireVarint {
		return 0, fmt.Errorf("field_%d_not_varint", fieldNumber)
	}
	v, n := binary.Uvarint(value)
	if n <= 0 {
		return 0, fmt.Errorf("field_%d_invalid_varint", fieldNumber)
	}
	return v, nil
}

func protoFixed64Value(fieldNumber, wireType int, value []byte) (uint64, error) {
	if wireType != protoWireFixed64 {
		return 0, fmt.Errorf("field_%d_not_fixed64", fieldNumber)
	}
	if len(value) != 8 {
		return 0, fmt.Errorf("field_%d_invalid_fixed64", fieldNumber)
	}
	return binary.LittleEndian.Uint64(value), nil
}

func formatProtoSummaryFloat(value float64) string {
	return strconv.FormatFloat(value, 'g', -1, 64)
}
