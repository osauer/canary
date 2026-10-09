package ibkr

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

const (
	protoOrderStatusMsgID  = msgOrderStatus + protoBufMsgID
	protoOpenOrderMsgID    = msgOpenOrder + protoBufMsgID
	protoExecDetailsMsgID  = msgExecDetails + protoBufMsgID
	protoOpenOrderEndMsgID = msgOpenOrderEnd + protoBufMsgID
)

type inboundOrderProtoSummary struct {
	algoStrategy         string
	adaptivePriority     string
	orderID              int
	requestID            int
	permID               int
	parentID             int
	clientID             int
	status               string
	symbol               string
	secType              string
	expiry               string
	strike               float64
	right                string
	multiplier           string
	exchange             string
	primaryExch          string
	currency             string
	localSymbol          string
	tradingClass         string
	action               string
	quantity             string
	orderType            string
	lmtPrice             float64
	auxPrice             float64
	trailingPercent      float64
	trailStopPrice       float64
	lmtPriceOffset       float64
	tif                  string
	triggerMethod        int
	account              string
	orderRef             string
	outsideRth           bool
	whatIf               bool
	transmit             bool
	filled               string
	remaining            string
	avgFillPrice         float64
	lastFillPrice        float64
	whyHeld              string
	mktCapPrice          float64
	initMarginBefore     float64
	maintMarginBefore    float64
	equityWithLoanBefore float64
	initMarginChange     float64
	maintMarginChange    float64
	equityWithLoanChange float64
	initMarginAfter      float64
	maintMarginAfter     float64
	equityWithLoanAfter  float64
	commission           float64
	minCommission        float64
	maxCommission        float64
	commissionCurrency   string
	marginCurrency       string
	rejectReason         string
	warningText          string
	execID               string
	execTime             string
	execAccount          string
	executionExchange    string
	executionSide        string
	shares               string
	price                float64
	cumQty               string
}

func summarizeInboundOrderProtoCallback(wireMsgID int, body []byte) ([]string, bool) {
	switch wireMsgID {
	case protoOpenOrderMsgID:
		return summarizeOpenOrderProtoCallback(body), true
	case protoOrderStatusMsgID:
		return summarizeOrderStatusProtoCallback(body), true
	case protoExecDetailsMsgID:
		return summarizeExecDetailsProtoCallback(body), true
	case protoOpenOrderEndMsgID:
		return []string{strconv.Itoa(msgOpenOrderEnd), "protobuf", "wire_msg_id=" + strconv.Itoa(wireMsgID)}, true
	default:
		return nil, false
	}
}

func summarizeOpenOrderProtoCallback(body []byte) []string {
	fields := inboundProtoBaseFields(msgOpenOrder, protoOpenOrderMsgID)
	summary, err := parseOpenOrderProtoCallback(body)
	if err != nil {
		return append(fields, "decode_error="+err.Error())
	}
	fields = append(fields,
		"orderId="+strconv.Itoa(summary.orderID),
		"permId="+strconv.Itoa(summary.permID),
		"clientId="+strconv.Itoa(summary.clientID),
	)
	fields = appendContractSummaryFields(fields, summary)
	fields = appendOrderSummaryFields(fields, summary)
	fields = append(fields,
		"status="+summary.status,
		"initMarginBefore="+formatProtoSummaryFloat(summary.initMarginBefore),
		"maintMarginBefore="+formatProtoSummaryFloat(summary.maintMarginBefore),
		"equityWithLoanBefore="+formatProtoSummaryFloat(summary.equityWithLoanBefore),
		"initMarginChange="+formatProtoSummaryFloat(summary.initMarginChange),
		"maintMarginChange="+formatProtoSummaryFloat(summary.maintMarginChange),
		"equityWithLoanChange="+formatProtoSummaryFloat(summary.equityWithLoanChange),
		"initMarginAfter="+formatProtoSummaryFloat(summary.initMarginAfter),
		"maintMarginAfter="+formatProtoSummaryFloat(summary.maintMarginAfter),
		"equityWithLoanAfter="+formatProtoSummaryFloat(summary.equityWithLoanAfter),
		"commission="+formatProtoSummaryFloat(summary.commission),
		"minCommission="+formatProtoSummaryFloat(summary.minCommission),
		"maxCommission="+formatProtoSummaryFloat(summary.maxCommission),
		"commissionCurrency="+summary.commissionCurrency,
		"marginCurrency="+summary.marginCurrency,
		"rejectReason="+summary.rejectReason,
		"warningText="+summary.warningText,
	)
	return fields
}

func summarizeOrderStatusProtoCallback(body []byte) []string {
	fields := inboundProtoBaseFields(msgOrderStatus, protoOrderStatusMsgID)
	summary, err := parseOrderStatusProtoCallback(body)
	if err != nil {
		return append(fields, "decode_error="+err.Error())
	}
	return append(fields,
		"orderId="+strconv.Itoa(summary.orderID),
		"status="+summary.status,
		"filled="+summary.filled,
		"remaining="+summary.remaining,
		"avgFillPrice="+formatProtoSummaryFloat(summary.avgFillPrice),
		"permId="+strconv.Itoa(summary.permID),
		"parentId="+strconv.Itoa(summary.parentID),
		"lastFillPrice="+formatProtoSummaryFloat(summary.lastFillPrice),
		"clientId="+strconv.Itoa(summary.clientID),
		"whyHeld="+summary.whyHeld,
		"mktCapPrice="+formatProtoSummaryFloat(summary.mktCapPrice),
	)
}

func summarizeExecDetailsProtoCallback(body []byte) []string {
	fields := inboundProtoBaseFields(msgExecDetails, protoExecDetailsMsgID)
	summary, err := parseExecDetailsProtoCallback(body)
	if err != nil {
		return append(fields, "decode_error="+err.Error())
	}
	fields = append(fields, "reqId="+strconv.Itoa(summary.requestID))
	fields = appendContractSummaryFields(fields, summary)
	return append(fields,
		"orderId="+strconv.Itoa(summary.orderID),
		"execId="+summary.execID,
		"execTime="+summary.execTime,
		"account="+summary.execAccount,
		"executionExchange="+summary.executionExchange,
		"executionSide="+summary.executionSide,
		"shares="+summary.shares,
		"price="+formatProtoSummaryFloat(summary.price),
		"permId="+strconv.Itoa(summary.permID),
		"clientId="+strconv.Itoa(summary.clientID),
		"cumQty="+summary.cumQty,
		"avgFillPrice="+formatProtoSummaryFloat(summary.avgFillPrice),
		"orderRef="+summary.orderRef,
	)
}

func inboundProtoBaseFields(baseMsgID, wireMsgID int) []string {
	return []string{strconv.Itoa(baseMsgID), "protobuf", "wire_msg_id=" + strconv.Itoa(wireMsgID)}
}

func appendContractSummaryFields(fields []string, summary inboundOrderProtoSummary) []string {
	return append(fields,
		"symbol="+summary.symbol,
		"secType="+summary.secType,
		"expiry="+summary.expiry,
		"strike="+formatProtoSummaryFloat(summary.strike),
		"right="+summary.right,
		"multiplier="+summary.multiplier,
		"exchange="+summary.exchange,
		"primaryExch="+summary.primaryExch,
		"currency="+summary.currency,
		"localSymbol="+summary.localSymbol,
		"tradingClass="+summary.tradingClass,
	)
}

func appendOrderSummaryFields(fields []string, summary inboundOrderProtoSummary) []string {
	return append(fields,
		"algoStrategy="+summary.algoStrategy,
		"adaptivePriority="+summary.adaptivePriority,
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
	)
}

func parseOpenOrderProtoCallback(body []byte) (inboundOrderProtoSummary, error) {
	var summary inboundOrderProtoSummary
	err := forEachProtoField(body, func(fieldNumber, wireType int, value []byte) error {
		switch fieldNumber {
		case 1:
			v, err := protoVarintValue(fieldNumber, wireType, value)
			if err != nil {
				return err
			}
			summary.orderID = int(v)
		case 2:
			return parseInboundContractProto(value, &summary)
		case 3:
			return parseInboundOrderProto(value, &summary)
		case 4:
			return parseInboundOrderStateProto(value, &summary)
		}
		return nil
	})
	return summary, err
}

func parseOrderStatusProtoCallback(body []byte) (inboundOrderProtoSummary, error) {
	var summary inboundOrderProtoSummary
	err := forEachProtoField(body, func(fieldNumber, wireType int, value []byte) error {
		switch fieldNumber {
		case 1:
			v, err := protoVarintValue(fieldNumber, wireType, value)
			if err != nil {
				return err
			}
			summary.orderID = int(v)
		case 2:
			summary.status = string(value)
		case 3:
			summary.filled = string(value)
		case 4:
			summary.remaining = string(value)
		case 5:
			v, err := protoFixed64Value(fieldNumber, wireType, value)
			if err != nil {
				return err
			}
			summary.avgFillPrice = math.Float64frombits(v)
		case 6:
			v, err := protoVarintValue(fieldNumber, wireType, value)
			if err != nil {
				return err
			}
			summary.permID = int(v)
		case 7:
			v, err := protoVarintValue(fieldNumber, wireType, value)
			if err != nil {
				return err
			}
			summary.parentID = int(v)
		case 8:
			v, err := protoFixed64Value(fieldNumber, wireType, value)
			if err != nil {
				return err
			}
			summary.lastFillPrice = math.Float64frombits(v)
		case 9:
			v, err := protoVarintValue(fieldNumber, wireType, value)
			if err != nil {
				return err
			}
			summary.clientID = int(v)
		case 10:
			summary.whyHeld = string(value)
		case 11:
			v, err := protoFixed64Value(fieldNumber, wireType, value)
			if err != nil {
				return err
			}
			summary.mktCapPrice = math.Float64frombits(v)
		}
		return nil
	})
	return summary, err
}

func parseExecDetailsProtoCallback(body []byte) (inboundOrderProtoSummary, error) {
	var summary inboundOrderProtoSummary
	err := forEachProtoField(body, func(fieldNumber, wireType int, value []byte) error {
		switch fieldNumber {
		case 1:
			v, err := protoVarintValue(fieldNumber, wireType, value)
			if err != nil {
				return err
			}
			summary.requestID = int(v)
		case 2:
			return parseInboundContractProto(value, &summary)
		case 3:
			return parseInboundExecutionProto(value, &summary)
		}
		return nil
	})
	return summary, err
}

func parseInboundContractProto(body []byte, summary *inboundOrderProtoSummary) error {
	return forEachProtoField(body, func(fieldNumber, wireType int, value []byte) error {
		switch fieldNumber {
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
			v, err := protoFixed64Value(fieldNumber, wireType, value)
			if err != nil {
				return err
			}
			summary.multiplier = formatProtoSummaryFloat(math.Float64frombits(v))
		case 8:
			summary.exchange = string(value)
		case 9:
			summary.primaryExch = string(value)
		case 10:
			summary.currency = string(value)
		case 11:
			summary.localSymbol = string(value)
		case 12:
			summary.tradingClass = string(value)
		case 21:
			summary.expiry = string(value)
		}
		return nil
	})
}

func parseInboundOrderProto(body []byte, summary *inboundOrderProtoSummary) error {
	return forEachProtoField(body, func(fieldNumber, wireType int, value []byte) error {
		switch fieldNumber {
		case 61:
			if wireType != protoWireBytes {
				return fmt.Errorf("invalid algoStrategy wire type")
			}
			summary.algoStrategy = string(value)
		case 62:
			if wireType != protoWireBytes {
				return fmt.Errorf("invalid algoParams wire type")
			}
			var key, param string
			if err := forEachProtoField(value, func(f, w int, v []byte) error {
				if w != protoWireBytes {
					return fmt.Errorf("invalid algo parameter wire type")
				}
				if f == 1 {
					key = string(v)
				}
				if f == 2 {
					param = string(v)
				}
				return nil
			}); err != nil {
				return err
			}
			if key == "adaptivePriority" {
				summary.adaptivePriority = param
			}

		case 1:
			v, err := protoVarintValue(fieldNumber, wireType, value)
			if err != nil {
				return err
			}
			summary.clientID = int(v)
		case 2:
			v, err := protoVarintValue(fieldNumber, wireType, value)
			if err != nil {
				return err
			}
			if summary.orderID == 0 {
				summary.orderID = int(v)
			}
		case 3:
			v, err := protoVarintValue(fieldNumber, wireType, value)
			if err != nil {
				return err
			}
			summary.permID = int(v)
		case 4:
			v, err := protoVarintValue(fieldNumber, wireType, value)
			if err != nil {
				return err
			}
			summary.parentID = int(v)
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
		case 28:
			summary.orderRef = string(value)
		case 31:
			v, err := protoVarintValue(fieldNumber, wireType, value)
			if err != nil {
				return err
			}
			summary.triggerMethod = int(v)
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

func parseInboundOrderStateProto(body []byte, summary *inboundOrderProtoSummary) error {
	return forEachProtoField(body, func(fieldNumber, wireType int, value []byte) error {
		switch fieldNumber {
		case 1:
			summary.status = string(value)
		case 2:
			return setProtoDouble(fieldNumber, wireType, value, &summary.initMarginBefore)
		case 3:
			return setProtoDouble(fieldNumber, wireType, value, &summary.maintMarginBefore)
		case 4:
			return setProtoDouble(fieldNumber, wireType, value, &summary.equityWithLoanBefore)
		case 5:
			return setProtoDouble(fieldNumber, wireType, value, &summary.initMarginChange)
		case 6:
			return setProtoDouble(fieldNumber, wireType, value, &summary.maintMarginChange)
		case 7:
			return setProtoDouble(fieldNumber, wireType, value, &summary.equityWithLoanChange)
		case 8:
			return setProtoDouble(fieldNumber, wireType, value, &summary.initMarginAfter)
		case 9:
			return setProtoDouble(fieldNumber, wireType, value, &summary.maintMarginAfter)
		case 10:
			return setProtoDouble(fieldNumber, wireType, value, &summary.equityWithLoanAfter)
		case 11:
			return setProtoDouble(fieldNumber, wireType, value, &summary.commission)
		case 12:
			return setProtoDouble(fieldNumber, wireType, value, &summary.minCommission)
		case 13:
			return setProtoDouble(fieldNumber, wireType, value, &summary.maxCommission)
		case 14:
			summary.commissionCurrency = string(value)
		case 15:
			summary.marginCurrency = string(value)
		case 26:
			summary.rejectReason = string(value)
		case 28:
			summary.warningText = string(value)
		}
		return nil
	})
}

func parseInboundExecutionProto(body []byte, summary *inboundOrderProtoSummary) error {
	return forEachProtoField(body, func(fieldNumber, wireType int, value []byte) error {
		switch fieldNumber {
		case 1:
			v, err := protoVarintValue(fieldNumber, wireType, value)
			if err != nil {
				return err
			}
			summary.orderID = int(v)
		case 2:
			summary.execID = string(value)
		case 3:
			summary.execTime = string(value)
		case 4:
			summary.execAccount = string(value)
		case 5:
			summary.executionExchange = string(value)
		case 6:
			summary.executionSide = string(value)
		case 7:
			summary.shares = string(value)
		case 8:
			return setProtoDouble(fieldNumber, wireType, value, &summary.price)
		case 9:
			v, err := protoVarintValue(fieldNumber, wireType, value)
			if err != nil {
				return err
			}
			summary.permID = int(v)
		case 10:
			v, err := protoVarintValue(fieldNumber, wireType, value)
			if err != nil {
				return err
			}
			summary.clientID = int(v)
		case 12:
			summary.cumQty = string(value)
		case 13:
			return setProtoDouble(fieldNumber, wireType, value, &summary.avgFillPrice)
		case 14:
			summary.orderRef = string(value)
		}
		return nil
	})
}

func setProtoDouble(fieldNumber, wireType int, value []byte, dst *float64) error {
	v, err := protoFixed64Value(fieldNumber, wireType, value)
	if err != nil {
		return err
	}
	*dst = math.Float64frombits(v)
	return nil
}

func protoSummaryBool(fields []string, prefix string) bool {
	return strings.EqualFold(summaryFieldValue(fields, prefix), "true")
}
