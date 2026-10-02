package daemon

import (
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// observeCashSweepFunding consumes existing reads only. It does not invent
// deliverables, settled available shares, FX shocks or original source clocks.
func (e *proposalEngine) observeCashSweepFunding(acct *rpc.AccountResult, pos *rpc.PositionsResult, scope brokerStateScope, cash cashSweepInput, now time.Time) (*risk.CashSweepOperationalObservation, []risk.CashSweepCalibrationStudy) {
	pol := e.rulebookPolicy()
	in := risk.CashSweepOperationalInput{AsOf: now, Source: "live_partial", PolicyFingerprint: pol.FingerprintKey(),
		AccountReceiptAt: cash.AccountReceiptAt, PositionsReceiptAt: cash.PositionsReceiptAt, TakeoverGapPct: pol.TakeoverGapPct}
	if acct == nil || pos == nil || !currentPortfolioAuthority(acct.Authority) || !currentPortfolioAuthority(pos.Authority) ||
		(acct.Authority.Source != rpc.AccountDataSourceAccountSummaryRequest && acct.Authority.Source != rpc.AccountDataSourceAccountUpdatesCache) || pos.Authority.Source != rpc.AccountDataSourcePortfolioStream ||
		acct.Authority.Scope != accountDataScope(scope) || pos.Authority.Scope != accountDataScope(scope) || acct.AccountID != scope.Account || pos.AccountID != scope.Account ||
		cash.AccountReceiptAt.IsZero() || cash.AccountReceiptAt.After(now) || cash.PositionsReceiptAt.IsZero() || cash.PositionsReceiptAt.After(now) ||
		!cash.AccountReceiptAt.Equal(acct.Authority.AsOf) || !cash.PositionsReceiptAt.Equal(pos.Authority.AsOf) || now.Sub(cash.AccountReceiptAt) > accountSnapshotFreshFor || now.Sub(cash.PositionsReceiptAt) > portfolioStreamReceiptMaxAge ||
		cash.LedgerReason != "" || e == nil || !sameBrokerScope(e.currentScope(), scope) {
		in.ScopeReason = "current_account_positions_scope_unavailable"
	}
	for ccy := range cash.Ledger {
		in.Currencies = append(in.Currencies, ccy)
	}
	study := risk.CashSweepCalibrationInput{AsOf: now, Source: "live_partial", PolicyFingerprint: pol.FingerprintKey(), EURRates: map[string]float64{},
		ClusterDropPct: pol.ClusterDropPct, TakeoverGapPct: pol.TakeoverGapPct, ExitParticipationPct: pol.ExitParticipationPct}
	if in.ScopeReason == "" {
		budget := e.resolveBudgetInput(acct, now)
		if budget.Constitution != nil && budget.AccountBaseCurrency == "EUR" {
			study.ProtectedFloor = cloneFloat64Ptr(budget.Constitution.Capital.ProtectedFloor)
		}
		if acct.Authority.Fields != nil && acct.Authority.Fields.NetLiquidation && positiveFinite(acct.NetLiquidation) && acct.BaseCurrency == "EUR" {
			study.BaseNAV = new(acct.NetLiquidation)
		}
		for ccy, row := range cash.Ledger {
			if row.Observed && row.ExchangeRate > 0 {
				study.EURRates[ccy] = row.ExchangeRate
			}
		}
		rows := append(append([]rpc.PositionView(nil), pos.Stocks...), pos.Options...)
		for _, p := range rows {
			r := risk.CashSweepFundingPosition{ConID: p.ConID, Symbol: p.Symbol, Currency: normCcy(p.Currency), SecType: p.SecType, Right: p.Right,
				Expiry:   p.Expiry,
				Quantity: p.Quantity, Strike: p.Strike, Multiplier: p.Multiplier, PriceAt: p.QuotePriceAt}
			// Portfolio marks and receipt clocks do not prove a current tradeable
			// quote. Missing exact terms/available shares remain nil deliberately.
			if !p.Stale && p.QuotePrice != nil && (p.FeedType == "live" || p.DataType == "live") {
				r.Price = cloneFloat64Ptr(p.QuotePrice)
			}
			in.Positions = append(in.Positions, r)
			study.Lines = append(study.Lines, risk.CashSweepCalibrationLine{ConID: p.ConID, Currency: r.Currency, SecType: p.SecType, Right: p.Right,
				Quantity: p.Quantity, Multiplier: float64(p.Multiplier), CurrentMark: p.Mark, Spot: derefFloat(p.Underlying), Strike: p.Strike, PriceOriginalAt: p.PriceAt, IV: cloneFloat64Ptr(p.IV)})
		}
	}
	out := risk.ObserveCashSweepOperationalFunding(in)
	// The public receipt contract has no common original connector epoch.
	// A sampled planning epoch cannot silently certify the earlier reads.
	out.Gaps = append(out.Gaps, "common_read_session_provenance_unavailable")
	return &out, risk.StudyCashSweepFiniteHorizons(study)
}
