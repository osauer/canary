package daemon

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

func cashSweepMinimum(p *protectionCashSweepPolicy, c protectionCashSweepCurrency, rate float64) float64 {
	if p != nil && p.MinOrderNotional > 0 && positiveFinite(rate) {
		return max(c.MinTranche, p.MinOrderNotional/rate)
	}
	return c.MinTranche
}

// Evaluate the exact draft, not the proposal's face-value estimate. Sales
// restore net cash. Purchases compare conservative proceeds at maturity with
// all-in cost and a dated upper bound on cash interest forgone. The ask already
// includes the entry spread; subtracting the spread again would double-count it.
func cashSweepEconomicsBlockers(prop rpc.TradeProposal, preview *rpc.OrderPreviewResult) []rpc.TradingBlocker {
	s := prop.CashSweep
	if prop.Bucket != rpc.TradeProposalBucketCashSweep || s == nil || preview == nil || preview.Draft.Bond == nil {
		return nil
	}
	block := func(code, msg string) []rpc.TradingBlocker {
		return []rpc.TradingBlocker{{Code: code, Message: msg, Action: "Refresh and preview again with complete evidence and a whole tranche."}}
	}
	d := preview.Draft
	if b := cashSweepCalendarBlockers(s, d.Contract.Exchange, preview.AsOf); len(b) > 0 {
		return b
	}
	gross := bondOrderNotional(d.Quantity, d.Bond, d.LimitPrice)
	minimum := max(s.MinTranche, s.MinOrderNotionalBase/s.ExchangeRate)
	if !finiteProtectionOptionPolicyValue(s.MinNetGainBase) || s.MinNetGainBase < 0 || !finiteProtectionOptionPolicyValue(s.RedemptionTarget) || s.RedemptionTarget < 0 || !positiveFinite(gross) || !positiveFinite(s.ExchangeRate) || math.IsNaN(minimum) || math.IsInf(minimum, 0) {
		return block("cash_sweep_notional_unknown", "The exact sweep value or minimum is unavailable.")
	}
	if s.Side == rpc.CashSweepSideInvest && gross < minimum-cashSweepMoneyEpsilon {
		return block("cash_sweep_below_minimum_tranche", "The purchase after lot rounding is below the whole-order minimum.")
	}
	fee, known := cashSweepFeeUpper(preview, s.Currency)
	if s.Side == rpc.CashSweepSideRedeem {
		if !known {
			return block("cash_sweep_fees_unknown", "The exact sale has no valid maximum commission in its currency; net cash is unknown.")
		}
		target := max(minimum, s.RedemptionTarget)
		if s.HeldToCap {
			target = minimum
		}
		net := gross - fee
		if !positiveFinite(net) || net < target-cashSweepMoneyEpsilon {
			return block("cash_sweep_net_proceeds_below_target", fmt.Sprintf("Net sale proceeds %.2f %s after maximum fees cannot restore the required tranche %.2f.", net, s.Currency, target))
		}
		return nil
	}
	if s.Side != rpc.CashSweepSideInvest || s.MinNetGainBase <= 0 {
		return nil
	}
	if !known {
		return block("cash_sweep_fees_unknown", "The exact purchase has no valid maximum commission in its currency.")
	}
	at := preview.AsOf
	maturity, err := time.Parse(time.DateOnly, s.MaturityDate)
	if s.Bill != nil {
		maturity, err = time.Parse(time.DateOnly, s.Bill.Maturity)
	}
	validThrough, validErr := time.Parse(time.DateOnly, s.CashInterestValidThrough)
	if at.IsZero() || err != nil || !maturity.After(cashSweepDay(at)) || validErr != nil || cashSweepDay(at).After(validThrough) || s.CashInterestRateUpper == nil || !finiteProtectionOptionPolicyValue(*s.CashInterestRateUpper) || *s.CashInterestRateUpper < 0 {
		return block("cash_sweep_net_value_unknown", "A dated cash-interest upper bound and a future maturity are required to evaluate incremental gain.")
	}
	price := d.LimitPrice
	if preview.Quote.Ask == nil || !positiveFinite(*preview.Quote.Ask) {
		return block("cash_sweep_net_value_unknown", "A fresh ask is required to include entry spread in the gain estimate.")
	}
	price = max(price, *preview.Quote.Ask)
	face := float64(d.Quantity) * d.Bond.FacePerUnit
	days := maturity.Sub(cashSweepDay(at)).Hours() / 24
	gainBase, valid := risk.CashSweepIncrementalGain(face, face*price/100, fee, *s.CashInterestRateUpper, days, s.ExchangeRate)
	if !valid || gainBase < s.MinNetGainBase-cashSweepMoneyEpsilon {
		return block("cash_sweep_net_gain_below_minimum", fmt.Sprintf("Conservative incremental gain %.2f in base is below min_net_gain %.2f after spread, maximum fees and cash interest forgone.", gainBase, s.MinNetGainBase))
	}
	return nil
}

// Before a new review only, size using the observed exact fee bound, then
// obtain a new WhatIf for the changed quantity. The final draft must meet every
// guard. A signed/prepared order is never resized after its review.
func (e *proposalEngine) previewSweepSized(ctx context.Context, prop rpc.TradeProposal, params rpc.OrderPreviewParams) (*rpc.OrderPreviewResult, error) {
	preview, err := e.server.previewOrder(ctx, params)
	if err != nil || prop.Bucket != rpc.TradeProposalBucketCashSweep || prop.CashSweep == nil || params.Bounded != nil {
		return preview, err
	}
	s := prop.CashSweep
	for range 2 {
		fee, known := cashSweepFeeUpper(preview, s.Currency)
		if !known || preview.Draft.Bond == nil || !positiveFinite(preview.Draft.LimitPrice) {
			break
		}
		d := preview.Draft
		rules := previewBondRules(d)
		if rules == nil || rules.Validate() != nil {
			break
		}
		quantity := d.Quantity
		if s.Side == rpc.CashSweepSideRedeem {
			unit := d.Bond.FacePerUnit * d.LimitPrice / 100
			target := max(s.MinTranche, s.MinOrderNotionalBase/s.ExchangeRate, s.RedemptionTarget)
			capUnits := int(math.Floor(min(float64(prop.MaxQuantity), s.MaxOrderNotionalBase/s.ExchangeRate/unit) + 1e-9))
			want := int(math.Ceil(min(float64(prop.MaxQuantity), (target+fee)/unit) - 1e-9))
			quantity, _ = cashSweepRedeemUnits(want, min(prop.MaxQuantity, capUnits), *rules)
		} else if s.Side == rpc.CashSweepSideInvest && bondOrderNotional(d.Quantity, d.Bond, d.LimitPrice)+fee > s.Free+cashSweepMoneyEpsilon {
			budget := min(s.Free-fee, s.MaxOrderNotionalBase/s.ExchangeRate)
			quantity, _ = cashSweepInvestUnits(budget, cashSweepInstrumentConventions[s.Instrument], *rules, d.LimitPrice, s.Currency)
			quantity = min(quantity, prop.MaxQuantity, params.Quantity)
		}
		if quantity < 1 || quantity == d.Quantity {
			break
		}
		params.Quantity = quantity
		preview, err = e.server.previewOrder(ctx, params)
		if err != nil {
			return preview, err
		}
	}
	return preview, nil
}

// Recheck dated evidence at consumption, including retained prepared previews.
// Preview.AsOf is the original evidence time and must not renew an expiry.
func cashSweepCurrentEvidenceBlockers(prop rpc.TradeProposal, at time.Time) []rpc.TradingBlocker {
	s := prop.CashSweep
	if prop.Bucket != rpc.TradeProposalBucketCashSweep || s == nil {
		return nil
	}
	if blockers := cashSweepCalendarBlockers(s, prop.Contract.Exchange, at); len(blockers) > 0 {
		return blockers
	}
	if s.Side == rpc.CashSweepSideInvest && s.MinNetGainBase > 0 {
		valid, err := time.Parse(time.DateOnly, s.CashInterestValidThrough)
		if at.IsZero() || err != nil || cashSweepDay(at).After(valid) {
			return []rpc.TradingBlocker{{Code: "cash_sweep_net_value_unknown", Message: "The reviewed cash-interest assumption has expired; the original preview cannot renew it."}}
		}
	}
	return nil
}
