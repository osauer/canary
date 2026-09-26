package daemon

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// The risk-reduction bucket (owner decision 2026-09-26) trims an issuer at
// rule 1's act level back to its watch level on the Rulebook's own
// worst-case-loss measure; the levels live in rulebook-policy.toml.

// issuerConcentrationInputs maps the book into the Rulebook's issuer inputs
// exactly as the canonical evaluation does, so the risk-reduction bucket trims
// on rule 1's own measurement rather than a second one. It issues no broker
// read and starts no background refresh.
func (e *proposalEngine) issuerConcentrationInputs(ctx context.Context, acct *rpc.AccountResult, pos *rpc.PositionsResult, now time.Time) (risk.RuleInputs, risk.RulebookPolicy, bool) {
	pol := e.rulebookPolicy()
	if acct == nil || pos == nil || !positiveFinite(acct.NetLiquidation) {
		return risk.RuleInputs{}, pol, false
	}
	baseCcy := normCcy(acct.BaseCurrency)
	names := mapRuleNames(pos, pol, baseCcy)
	var earnings map[string]risk.EarningsInput
	if e != nil && e.server != nil {
		e.server.attachRulebookLiquidity(ctx, names, pos, now, false)
		earnings, _ = e.server.assembleEarnings(ctx, names, pol, marketcal.New(), now, false)
		names = rulebookEconomicNames(names, earnings)
	}
	return risk.RuleInputs{
		AsOf: now, BaseCurrency: baseCcy,
		Positions: risk.SourceState{Healthy: true}, Account: risk.SourceState{Healthy: true},
		NLVBase: new(acct.NetLiquidation), Names: names, Earnings: earnings,
	}, pol, true
}

// riskReductionProposals trims every issuer at rule 1's act level back to its
// watch level on the same worst-case-loss measure (owner decision 2026-09-26).
// Cost-ranked alternatives (rolls, collars, trims spread across legs) are a
// later step; each issuer gets one reduce-only order on the leg that loses
// most at its worst price.
func (e *proposalEngine) riskReductionProposals(ctx context.Context, policy protectionPolicy, status rpc.ProtectionPolicyStatus, acct *rpc.AccountResult, pos *rpc.PositionsResult, sources rpc.TradeProposalSourceFingerprints, now time.Time) []rpc.TradeProposal {
	in, pol, ok := e.issuerConcentrationInputs(ctx, acct, pos, now)
	if !ok {
		return nil
	}
	var out []rpc.TradeProposal
	for _, issuer := range risk.EvaluateIssuerConcentration(in, pol) {
		if issuer.Status != risk.RuleStatusAct || len(issuer.Exposure.Lines) == 0 {
			continue
		}
		plan, ok := risk.PlanIssuerTrim(in, pol, issuer.Exposure.Lines[0])
		if !ok {
			continue
		}
		row, found := riskReductionRow(pos, plan)
		if !found {
			continue
		}
		p, ok := riskReductionProposal(policy, status, plan, row, *in.NLVBase, in.BaseCurrency, sources, now)
		if !ok {
			continue
		}
		for _, group := range pos.ByUnderlying {
			if strings.EqualFold(group.Underlying, plan.Symbol) {
				p.MarketValuePctNLV = cloneFloat64Ptr(group.GroupMarketValuePctNLV)
				enrichRiskReductionContext(&p, group, acct)
				break
			}
		}
		out = append(out, p)
	}
	return out
}

// riskReductionRow finds the position row a trim plan reduces: the largest
// stock row of the line, or the option row with the plan's leg description.
func riskReductionRow(pos *rpc.PositionsResult, plan risk.IssuerTrimPlan) (rpc.PositionView, bool) {
	var best rpc.PositionView
	found := false
	if plan.Stock {
		for _, row := range pos.Stocks {
			if strings.EqualFold(strings.TrimSpace(row.Symbol), plan.Symbol) && isRulebookStockSecurityType(row.SecType) &&
				row.Quantity != 0 && (!found || math.Abs(row.Quantity) > math.Abs(best.Quantity)) {
				best, found = row, true
			}
		}
		return best, found
	}
	for _, row := range pos.Options {
		if row.Quantity != 0 && legDesc(row) == plan.Leg {
			return row, true
		}
	}
	return rpc.PositionView{}, false
}

// riskReductionProposal turns one issuer trim plan into a reduce-only order
// on the leg the plan reduces, capped by max_order_notional; the remainder
// waits for the next cycle.
func riskReductionProposal(policy protectionPolicy, status rpc.ProtectionPolicyStatus, plan risk.IssuerTrimPlan, row rpc.PositionView, nlv float64, baseCcy string, sources rpc.TradeProposalSourceFingerprints, now time.Time) (rpc.TradeProposal, bool) {
	if row.Symbol == "" || row.Quantity == 0 || !proposalSupportedSecType(row.SecType) || !positiveFinite(nlv) {
		return rpc.TradeProposal{}, false
	}
	action := rpc.OrderActionSell
	if row.Quantity < 0 {
		action = rpc.OrderActionBuy
	}
	maxQty := int(math.Ceil(math.Abs(row.Quantity)))
	qty := int(math.Ceil(plan.Quantity - 1e-9))
	capped := false
	mark := math.Abs(row.Mark)
	if mark <= 0 {
		mark = math.Abs(row.ValuationMark)
	}
	if mark > 0 {
		mult := float64(max(row.Multiplier, 1))
		if byNotional := int(math.Max(1, math.Floor(policy.Buckets.RiskReduction.MaxOrderNotional/(mark*mult)))); qty > byNotional {
			qty, capped = byNotional, true
		}
	}
	qty = max(1, min(qty, maxQty))
	effect := rpc.OrderPositionEffectReduce
	if qty == maxQty {
		effect = rpc.OrderPositionEffectClose
	}
	// Percentages are kept to 1e-6 so float noise in the scenario sums never
	// reads as a different level (29.999999… is 30).
	lossPct := math.Round(plan.LossBeforeBase/nlv*100*1e6) / 1e6
	// The loss after is priced for the quantity this order proposes, which
	// max_order_notional may cap below the plan's.
	afterBase := plan.LossAfter(float64(qty))
	afterPct := math.Round(afterBase/nlv*100*1e6) / 1e6
	var reason string
	switch {
	case !plan.Reachable:
		reason = fmt.Sprintf("%s can lose %.1f%% of NLV at worst, at or above the %s%% cap; trimming this leg alone lowers it to %.1f%%, still above the %s%% watch level",
			plan.Issuer, lossPct, rulebookLimitText(plan.ActPct), afterPct, rulebookLimitText(plan.WatchPct))
	case afterBase > plan.TargetBase+1e-6:
		reason = fmt.Sprintf("%s can lose %.1f%% of NLV at worst, at or above the %s%% cap; this order lowers it to %.1f%%, and the rest of the trim to the %s%% watch level waits for the next cycle",
			plan.Issuer, lossPct, rulebookLimitText(plan.ActPct), afterPct, rulebookLimitText(plan.WatchPct))
	default:
		reason = fmt.Sprintf("%s can lose %.1f%% of NLV at worst, at or above the %s%% cap; this trim returns it to the %s%% watch level",
			plan.Issuer, lossPct, rulebookLimitText(plan.ActPct), rulebookLimitText(plan.WatchPct))
	}
	p := baseProposal(policy, status, sources, now, rpc.TradeProposalBucketRiskReduction, row, action, qty, effect, reason)
	p.Details = append(p.Details, "worst-case loss nets every leg on the issuer from current marks (Rulebook rule 1)")
	if !plan.Reachable {
		p.Details = append(p.Details, "other legs keep the issuer above its watch level; rolls and collars are not ranked yet")
	}
	if capped {
		p.Details = append(p.Details, "capped at max_order_notional; the rest waits for the next cycle")
	}
	p.Issuer = plan.Issuer
	p.IssuerLossPctNLV = new(lossPct)
	p.IssuerLossAfterPctNLV = new(afterPct)
	p.IssuerTargetPctNLV = new(plan.WatchPct)
	excess := math.Max(plan.LossBeforeBase-plan.TargetBase, 0)
	p.RiskExcessNotional = excess
	p.RiskExcessCurrency = baseCcy
	p.RiskExcessNotionalBase = new(excess)
	p.Score = lossPct
	return p, true
}

// rulebookLimitText renders a policy limit exactly as configured, the way
// the Rulebook's evidence does (7.5 stays 7.5, 40 stays 40).
func rulebookLimitText(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}
