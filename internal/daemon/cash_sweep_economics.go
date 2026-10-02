package daemon

import (
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

// Keep actual order safety separate from the advisory benefit estimate.
// The whole-order floor applies to the gross reviewed value on either side;
// fees remain cash debits, rather than permission to resize the order.
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
	if !positiveFinite(gross) || !positiveFinite(s.ExchangeRate) || !finiteProtectionOptionPolicyValue(s.MinTranche) || s.MinTranche < 0 || !finiteProtectionOptionPolicyValue(s.MinOrderNotionalBase) || s.MinOrderNotionalBase < 0 || math.IsNaN(minimum) || math.IsInf(minimum, 0) {
		return block("cash_sweep_notional_unknown", "The exact sweep value or minimum is unavailable.")
	}
	if gross < minimum-cashSweepMoneyEpsilon {
		return block("cash_sweep_below_minimum_tranche", "The reviewed order after lot rounding is below the whole-order minimum.")
	}
	fee, known := cashSweepFeeUpper(preview, s.Currency)
	if s.Side == rpc.CashSweepSideRedeem {
		if !known {
			return block("cash_sweep_fees_unknown", "The exact sale has no valid maximum commission in its currency; net cash is unknown.")
		}
		net := gross - fee
		if !positiveFinite(net) {
			return block("cash_sweep_net_proceeds_invalid", "The exact sale cannot restore positive cash after maximum fees.")
		}
		return nil
	}
	return nil
}

// cashSweepEconomicsAdvisory describes the exact reviewed order without
// changing its quantity or granting authority. Unknown benefit assumptions
// remain unavailable; they do not prevent an otherwise safe whole order.
func cashSweepEconomicsAdvisory(prop rpc.TradeProposal, preview *rpc.OrderPreviewResult) *rpc.CashSweepEconomics {
	s := prop.CashSweep
	if prop.Bucket != rpc.TradeProposalBucketCashSweep || s == nil || preview == nil || preview.Draft.Bond == nil {
		return nil
	}
	out := &rpc.CashSweepEconomics{State: "unavailable", Currency: s.Currency, BaseCurrency: preview.BaseCurrency, AsOf: preview.AsOf}
	if finiteProtectionOptionPolicyValue(s.MinNetGainBase) && s.MinNetGainBase >= 0 {
		out.MinNetGainBase = s.MinNetGainBase
	}
	fee, known := cashSweepFeeUpper(preview, s.Currency)
	if !known {
		out.Message = "The exact order's fee reserve is unavailable; its benefit estimate cannot be calculated."
		return out
	}
	out.FeeReserve = new(fee)
	d := preview.Draft
	if s.Side == rpc.CashSweepSideRedeem {
		net := bondOrderNotional(d.Quantity, d.Bond, d.LimitPrice) - fee
		if !positiveFinite(net) || !finiteProtectionOptionPolicyValue(s.RedemptionTarget) || s.RedemptionTarget < 0 {
			out.Message = "Net sale proceeds or the cash restoration target are unavailable."
			return out
		}
		remaining := max(0, s.RedemptionTarget-net)
		out.State, out.NetProceeds, out.RemainingGap = "estimated", new(net), new(remaining)
		out.Message = "Estimated net proceeds after the broker fee reserve; cash is restored only when settlement is observed."
		if remaining > cashSweepMoneyEpsilon {
			out.State = "partial_restoration"
			out.Message = fmt.Sprintf("Estimated net sale proceeds %.2f %s leave %.2f of the cash target to restore; the reviewed quantity is unchanged and settlement remains pending.", net, s.Currency, remaining)
		}
		return out
	}
	if s.Side != rpc.CashSweepSideInvest {
		out.Message = "The order side is unavailable for a cash-benefit estimate."
		return out
	}
	at := preview.AsOf
	maturity, err := time.Parse(time.DateOnly, s.MaturityDate)
	if s.Bill != nil {
		maturity, err = time.Parse(time.DateOnly, s.Bill.Maturity)
	}
	validThrough, validErr := time.Parse(time.DateOnly, s.CashInterestValidThrough)
	if at.IsZero() || err != nil || !maturity.After(cashSweepDay(at)) || validErr != nil || cashSweepDay(at).After(validThrough) || s.CashInterestRateUpper == nil || !finiteProtectionOptionPolicyValue(*s.CashInterestRateUpper) || *s.CashInterestRateUpper < 0 {
		out.Message = "Incremental benefit is unavailable: a current dated cash-interest assumption and future maturity are needed for the comparison. This is advisory."
		return out
	}
	if preview.Quote.Ask == nil || !positiveFinite(*preview.Quote.Ask) || !positiveFinite(d.LimitPrice) || !finiteProtectionOptionPolicyValue(s.MinNetGainBase) || s.MinNetGainBase < 0 {
		out.Message = "Incremental benefit is unavailable: the exact price or comparison benchmark is unknown. This is advisory."
		return out
	}
	// The conservative ask already includes entry spread. Subtracting that
	// spread a second time would understate the expected benefit.
	price := max(d.LimitPrice, *preview.Quote.Ask)
	face := float64(d.Quantity) * d.Bond.FacePerUnit
	days := maturity.Sub(cashSweepDay(at)).Hours() / 24
	gain, valid := risk.CashSweepIncrementalGain(face, face*price/100, fee, *s.CashInterestRateUpper, days, s.ExchangeRate)
	if !valid {
		out.Message = "Incremental benefit cannot be calculated from the available order terms. This is advisory."
		return out
	}
	out.State, out.IncrementalGainBase = "estimated", new(gain)
	out.Message = "Conservative incremental benefit after entry spread, the broker fee reserve and cash interest forgone. This is advisory."
	if s.MinNetGainBase > 0 && gain < s.MinNetGainBase-cashSweepMoneyEpsilon {
		out.State = "below_benchmark"
		out.Message = fmt.Sprintf("Conservative incremental benefit %.2f in base is below the advisory benchmark %.2f; the reviewed whole order remains unchanged.", gain, s.MinNetGainBase)
	}
	return out
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
	return nil
}
