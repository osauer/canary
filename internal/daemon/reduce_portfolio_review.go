package daemon

import (
	"math"

	"github.com/osauer/canary/v3/internal/rpc"
)

// projectReducePortfolioReview only annotates the already-sized net preview
// (owner, 2026-10-19: net delta before and after, and every contract left
// unchanged with its reason). It uses the same position snapshot and prepared
// legs, never a second read or a new allocation. Blocked legs contribute
// nothing to the estimated reduction. Locked lines (res.Kept) join the same
// list, so the review names what is kept once.
func projectReducePortfolioReview(res *rpc.TradeProposalReducePortfolioResult, pos *rpc.PositionsResult, cands []reduceSweepCandidate) {
	if pos == nil || res.NetDeltaIncomplete {
		return
	}
	after := res.NetDollarDeltaBefore - math.Copysign(res.AchievedDollarDelta, res.NetDollarDeltaBefore)
	res.NetDollarDeltaAfter = &after
	res.UntouchedPositions = make([]rpc.TradeProposalReduceUntouchedPosition, 0)
	prepared := make(map[rpc.ContractParams]rpc.TradeProposalReduceLeg, len(cands))
	for i, c := range cands {
		if i < len(res.Legs) {
			prepared[reduceReviewContract(c.row)] = res.Legs[i]
		}
	}
	locked := map[int]bool{}
	for _, k := range res.Kept {
		if k.Reason == reduceKeptLocked && k.ConID > 0 {
			locked[k.ConID] = true
		}
	}
	rows := append(append(make([]rpc.PositionView, 0, len(pos.Stocks)+len(pos.Options)), pos.Stocks...), pos.Options...)
	for _, row := range rows {
		if row.Quantity == 0 {
			continue
		}
		contract := reduceReviewContract(row)
		code, reason := "below_whole_unit", "Allocated reduction is below one whole share or contract."
		if locked[row.ConID] {
			code, reason = reduceKeptLocked, "Locked by you; never in a bulk reduction."
		} else if leg, ok := prepared[contract]; ok {
			if leg.SubmitEligible && len(leg.Blockers) == 0 {
				continue
			}
			code, reason = "blocked", "Order preview is not eligible for submission."
			if len(leg.Blockers) > 0 {
				reason = leg.Blockers[0].Message
			}
		} else if !reduceEligible(row) {
			code, reason = "unsupported", "This position is outside the supported reduction scope."
		} else if dd, ok := positionDollarDelta(row, positionWireSecType(row.SecType) == "OPT"); ok && dd*res.NetDollarDeltaBefore < 0 {
			code, reason = "opposite_delta", "Opposite-direction exposure stays unchanged."
		}
		res.UntouchedPositions = append(res.UntouchedPositions, rpc.TradeProposalReduceUntouchedPosition{Contract: contract, PositionQuantity: row.Quantity, Code: code, Reason: reason})
	}
}

func reduceReviewContract(row rpc.PositionView) rpc.ContractParams {
	return rpc.ContractParams{
		ConID: row.ConID, Symbol: row.Symbol, SecType: positionWireSecType(row.SecType),
		Exchange: row.Exchange, PrimaryExch: row.PrimaryExch, Currency: row.Currency,
		LocalSymbol: row.LocalSymbol, TradingClass: row.TradingClass,
		Expiry: row.Expiry, Strike: row.Strike, Right: row.Right, Multiplier: row.Multiplier,
	}
}
