package daemon

import (
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

const proposalStopGapScenarioPct = 5.0

// enrichProposalPositionContext attaches holding-level decision context to a
// single-position proposal: the full position market value, its share of NLV,
// and today's P&L move. This is what a human needs to weigh the action — the
// exposure being acted on and whether the name is up or down today — separate
// from the order size in the proposal head.
func enrichProposalPositionContext(prop *rpc.TradeProposal, row rpc.PositionView, acct *rpc.AccountResult) {
	if prop == nil {
		return
	}
	prop.PositionMarketValue = row.MarketValue
	if prop.MarketValuePctNLV == nil && acct != nil && acct.NetLiquidation > 0 && row.MarketValueBase != nil {
		pct := *row.MarketValueBase / acct.NetLiquidation * 100
		prop.MarketValuePctNLV = &pct
	}
	prop.PositionDayChangeMoney = cloneFloat64Ptr(row.DayChangeMoney)
	prop.PositionDayChangePct = cloneFloat64Ptr(row.DayChangePct)
	prop.PositionDayChangeCurrency = nonEmptyString(row.Currency, prop.Contract.Currency)
}

// enrichRiskReductionContext is the group-level analogue: a risk-reduction
// proposal acts on a whole single-name group (stock + its options), so the
// decision context is the group's total market value and today's group P&L,
// not one leg. MarketValuePctNLV is already set from the group; this fills the
// market value and today's move (reported in base currency, the only currency
// the daemon aggregates group P&L in).
func enrichRiskReductionContext(prop *rpc.TradeProposal, group rpc.PositionGroup, acct *rpc.AccountResult) {
	if prop == nil {
		return
	}
	prop.PositionMarketValue = group.GroupMarketValue
	if group.GroupDailyPnLBase != nil {
		prop.PositionDayChangeMoney = cloneFloat64Ptr(group.GroupDailyPnLBase)
		if acct != nil {
			prop.PositionDayChangeCurrency = strings.ToUpper(strings.TrimSpace(acct.BaseCurrency))
		}
		if group.GroupMarketValueBase != nil && *group.GroupMarketValueBase != 0 {
			pct := *group.GroupDailyPnLBase / *group.GroupMarketValueBase * 100
			prop.PositionDayChangePct = &pct
		}
	}
}

func enrichProtectiveStopProposal(prop *rpc.TradeProposal, row rpc.PositionView, acct *rpc.AccountResult) {
	if prop == nil || prop.Trail == nil || !isTrailOrderType(prop.OrderType) {
		return
	}
	reference, source, refAt := proposalStopReference(row, prop.Action)
	prop.ExecutionSemantics = buildProposalExecutionSemantics(*prop, source, reference, refAt)
	prop.StopRisk = buildProposalStopRisk(*prop, row, acct, reference)
	prop.StopLadder = buildProposalStopLadder(*prop, row, acct, reference)
}

func proposalStopReference(row rpc.PositionView, action string) (float64, string, time.Time) {
	if strings.EqualFold(row.SecType, "OPTION") || strings.EqualFold(row.SecType, "OPT") {
		reference, _, ok := optionTrailReference(row, action)
		if ok {
			if strings.EqualFold(action, rpc.OrderActionBuy) {
				return reference, "ask", row.PriceAt
			}
			return reference, "bid", row.PriceAt
		}
	}
	return trailingStopReference(row, action)
}

func buildProposalExecutionSemantics(prop rpc.TradeProposal, referenceSide string, reference float64, refAt time.Time) *rpc.TradeProposalExecutionSemantics {
	out := &rpc.TradeProposalExecutionSemantics{
		ReferenceSide:      strings.TrimSpace(referenceSide),
		TriggerMethod:      proposalTriggerMethod(prop),
		TriggerMethodLabel: orderTriggerMethodLabel(proposalTriggerMethod(prop)),
		TriggerSource:      orderTriggerMethodSource(proposalTriggerMethod(prop)),
	}
	if reference > 0 {
		out.ReferencePrice = cloneFloat64Ptr(&reference)
	}
	if !refAt.IsZero() {
		out.ReferenceAsOf = refAt
	}
	switch strings.ToUpper(strings.TrimSpace(prop.OrderType)) {
	case rpc.OrderTypeTRAILLIMIT:
		out.TriggerEffect = "limit_order_when_triggered"
		out.PriceGuarantee = "stop_limit_can_leave_position_unfilled"
	case rpc.OrderTypeTRAIL:
		out.TriggerEffect = "market_order_when_triggered"
		out.PriceGuarantee = "stop_price_is_not_execution_price"
	default:
		out.TriggerEffect = "order_when_triggered"
	}
	return out
}

func orderTriggerMethodLabel(method int) string {
	switch method {
	case rpc.OrderTriggerMethodDoubleBidAsk:
		return "double bid/ask"
	case rpc.OrderTriggerMethodLast:
		return "last"
	case rpc.OrderTriggerMethodDoubleLast:
		return "double last"
	case rpc.OrderTriggerMethodBidAsk:
		return "bid/ask"
	case rpc.OrderTriggerMethodLastOrBidAsk:
		return "last or bid/ask"
	case rpc.OrderTriggerMethodMidpoint:
		return "midpoint"
	case rpc.OrderTriggerMethodDefault:
		return "broker default"
	default:
		return "unknown"
	}
}

func orderTriggerMethodSource(method int) string {
	switch method {
	case rpc.OrderTriggerMethodDoubleBidAsk, rpc.OrderTriggerMethodBidAsk:
		return "bid_ask"
	case rpc.OrderTriggerMethodLast, rpc.OrderTriggerMethodDoubleLast:
		return "last"
	case rpc.OrderTriggerMethodLastOrBidAsk:
		return "last_or_bid_ask"
	case rpc.OrderTriggerMethodMidpoint:
		return "midpoint"
	case rpc.OrderTriggerMethodDefault:
		return "broker_default"
	default:
		return "unknown"
	}
}

func buildProposalStopRisk(prop rpc.TradeProposal, row rpc.PositionView, acct *rpc.AccountResult, reference float64) *rpc.TradeProposalStopRisk {
	if reference <= 0 || prop.Trail == nil || prop.Trail.InitialStopPrice <= 0 {
		return &rpc.TradeProposalStopRisk{WarningCodes: []string{"stop_risk_reference_unavailable"}}
	}
	stop := prop.Trail.InitialStopPrice
	distance := protectiveStopDistance(prop.Action, reference, stop)
	if distance < 0 {
		return &rpc.TradeProposalStopRisk{ReferencePrice: cloneFloat64Ptr(&reference), StopPrice: cloneFloat64Ptr(&stop), WarningCodes: []string{"stop_on_wrong_side_of_reference"}}
	}
	distancePct := distance / reference * 100
	multiplier := max(row.Multiplier, 1)
	loss := distance * float64(max(prop.Quantity, 0)) * float64(multiplier)
	currency := riskCurrency(row, prop)
	base, baseCurrency, pctNLV := baseRiskFields(loss, currency, row, acct)
	gapPrice := protectiveStopGapExecutionPrice(prop.Action, stop, proposalStopGapScenarioPct)
	gapDistance := protectiveStopDistance(prop.Action, reference, gapPrice)
	gapLoss := gapDistance * float64(max(prop.Quantity, 0)) * float64(multiplier)
	gapBase, _, gapPctNLV := baseRiskFields(gapLoss, currency, row, acct)
	return &rpc.TradeProposalStopRisk{
		ReferencePrice:      cloneFloat64Ptr(&reference),
		StopPrice:           cloneFloat64Ptr(&stop),
		Distance:            cloneFloat64Ptr(&distance),
		DistancePct:         cloneFloat64Ptr(&distancePct),
		Quantity:            prop.Quantity,
		Multiplier:          multiplier,
		EstimatedLoss:       cloneFloat64Ptr(&loss),
		Currency:            currency,
		EstimatedLossBase:   base,
		BaseCurrency:        baseCurrency,
		EstimatedLossPctNLV: pctNLV,
		GapScenario: &rpc.TradeProposalStopRiskGap{
			Label:                 "5pct_beyond_stop",
			GapPct:                proposalStopGapScenarioPct,
			AssumedExecutionPrice: cloneFloat64Ptr(&gapPrice),
			EstimatedLoss:         cloneFloat64Ptr(&gapLoss),
			EstimatedLossBase:     gapBase,
			EstimatedLossPctNLV:   gapPctNLV,
		},
	}
}

func buildProposalStopLadder(prop rpc.TradeProposal, row rpc.PositionView, acct *rpc.AccountResult, reference float64) []rpc.TradeProposalStopLadderStep {
	if reference <= 0 || prop.Trail == nil {
		return nil
	}
	seen := map[string]bool{}
	steps := []rpc.TradeProposalStopLadderStep{}
	add := func(label, kind string, pct float64) {
		if pct <= 0 || math.IsNaN(pct) || math.IsInf(pct, 0) {
			return
		}
		key := kind + ":" + strings.TrimRight(strings.TrimRight(formatFloatKey(pct), "0"), ".")
		if seen[key] {
			return
		}
		seen[key] = true
		stop := protectiveStopPriceForPct(prop.Action, reference, pct, prop.Contract)
		loss := protectiveStopDistance(prop.Action, reference, stop) * float64(max(prop.Quantity, 0)) * float64(max(row.Multiplier, 1))
		base, _, pctNLV := baseRiskFields(loss, riskCurrency(row, prop), row, acct)
		steps = append(steps, rpc.TradeProposalStopLadderStep{
			Label:               label,
			Kind:                kind,
			Percent:             cloneFloat64Ptr(&pct),
			StopPrice:           cloneFloat64Ptr(&stop),
			EstimatedLoss:       cloneFloat64Ptr(&loss),
			EstimatedLossBase:   base,
			EstimatedLossPctNLV: pctNLV,
			ReferencePrice:      cloneFloat64Ptr(&reference),
		})
	}
	add("5%", "fixed_5pct", 5)
	add("10%", "fixed_10pct", 10)
	if prop.TrailSizing != nil {
		add("policy chosen", "policy_chosen", prop.TrailSizing.ChosenPct)
		if prop.TrailSizing.ATRCandidatePct != nil {
			add("ATR candidate", "atr_candidate", *prop.TrailSizing.ATRCandidatePct)
		}
		add("policy min", "policy_min", prop.TrailSizing.PolicyMinPct)
		add("policy max", "policy_max", prop.TrailSizing.PolicyMaxPct)
	}
	return steps
}

func protectiveStopDistance(action string, reference, stop float64) float64 {
	if strings.EqualFold(action, rpc.OrderActionBuy) {
		return stop - reference
	}
	return reference - stop
}

func protectiveStopPriceForPct(action string, reference, pct float64, contract rpc.ContractParams) float64 {
	amount := ceilPriceToTick(reference*pct/100, trailMinimumTick(contract, reference))
	return trailingStopInitialPriceForContract(action, reference, amount, contract)
}

func protectiveStopGapExecutionPrice(action string, stop, pct float64) float64 {
	if strings.EqualFold(action, rpc.OrderActionBuy) {
		return stop * (1 + pct/100)
	}
	return stop * (1 - pct/100)
}

func baseRiskFields(loss float64, currency string, row rpc.PositionView, acct *rpc.AccountResult) (*float64, string, *float64) {
	var base *float64
	baseCurrency := ""
	if acct != nil {
		baseCurrency = strings.ToUpper(strings.TrimSpace(acct.BaseCurrency))
	}
	fx := 0.0
	if row.FXRate != nil && *row.FXRate > 0 {
		fx = *row.FXRate
	} else if baseCurrency != "" && strings.EqualFold(baseCurrency, currency) {
		fx = 1
	}
	if fx > 0 {
		v := loss * fx
		base = &v
	}
	var pct *float64
	if acct != nil && acct.NetLiquidation > 0 && base != nil {
		v := *base / acct.NetLiquidation * 100
		pct = &v
	}
	return base, baseCurrency, pct
}

func riskCurrency(row rpc.PositionView, prop rpc.TradeProposal) string {
	if c := strings.ToUpper(strings.TrimSpace(row.Currency)); c != "" {
		return c
	}
	return strings.ToUpper(strings.TrimSpace(prop.Contract.Currency))
}

func formatFloatKey(v float64) string {
	return strings.TrimSpace(strings.TrimRight(strings.TrimRight(strconv.FormatFloat(v, 'f', 6, 64), "0"), "."))
}
