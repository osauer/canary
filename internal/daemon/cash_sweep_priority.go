package daemon

import (
	"cmp"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

func (p *protectionCashSweepPolicy) reserveDesignEnabled() bool {
	return p != nil && (p.CurrencyPriority != "" || p.ReserveCushionEUR != nil)
}

func (p *protectionCashSweepPolicy) effectiveCurrencyPriority() string {
	if p == nil || !p.reserveDesignEnabled() {
		return ""
	}
	if p.CurrencyPriority != "" {
		return p.CurrencyPriority
	}
	return rpc.CashSweepPriorityUSDFirst
}

func applyCashSweepReservePolicy(p *protectionCashSweepPolicy, in *cashSweepInput, st *rpc.TradeProposalCashSweepStatus, now time.Time) {
	if !p.reserveDesignEnabled() {
		return
	}
	st.CurrencyPriority = p.effectiveCurrencyPriority()
	cushion := 10000.0
	if p.ReserveCushionEUR != nil {
		cushion = *p.ReserveCushionEUR
	}
	st.ReserveCushionEUR = new(cushion)
	if in.FundingEvidence != nil {
		st.FundingAsOf, st.FundingValidUntil = in.FundingEvidence.AsOf, in.FundingEvidence.ValidUntil
		st.ScenarioFingerprint = in.FundingEvidence.ScenarioFingerprint
	}
	rates, floors := map[string]float64{}, map[string]float64{}
	for _, ccy := range cashSweepCurrencies(p, *in) {
		row := in.Ledger[ccy]
		// Allocation uses EUR per native unit. The live EUR-base observer
		// must prove these rates; unsupported bases are held, not guessed.
		if in.BaseCurrency == "EUR" && in.LedgerReason == "" && row.Observed {
			rates[ccy] = row.ExchangeRate
		}
		floors[ccy] = p.currency(ccy).KeepCash
	}
	in.BufferAllocations, in.EffectiveReserves, st.ReserveReason = risk.CashSweepReserveAllocation(cushion, rates, floors, in.FundingEvidence, now)
	st.ReserveState = "ready"
	if st.ReserveReason != "" {
		st.ReserveState = "unavailable"
	}
}

func orderCashSweepCurrencies(p *protectionCashSweepPolicy, plan *cashSweepPlan) {
	mode := p.effectiveCurrencyPriority()
	if mode == "" {
		return
	}
	preferred := "USD"
	if mode == rpc.CashSweepPriorityEURFirst {
		preferred = "EUR"
	}
	slices.SortStableFunc(plan.currencies, func(a, b cashSweepCurrencyPlan) int {
		// Restoring liquidity comes before either investment preference.
		if a.side == rpc.CashSweepSideRedeem && b.side != rpc.CashSweepSideRedeem {
			return -1
		}
		if b.side == rpc.CashSweepSideRedeem && a.side != rpc.CashSweepSideRedeem {
			return 1
		}
		if mode == rpc.CashSweepPriorityBalanced {
			if d := cmp.Compare(b.free*b.rate, a.free*a.rate); d != 0 {
				return d
			}
		} else {
			if a.status.Currency == preferred && b.status.Currency != preferred {
				return -1
			}
			if b.status.Currency == preferred && a.status.Currency != preferred {
				return 1
			}
		}
		return cmp.Compare(a.status.Currency, b.status.Currency)
	})
	for i := range plan.currencies {
		plan.currencies[i].status.PriorityRank = i + 1
	}
}

// enforceCashSweepFundedReserves protects the whole additional cushion with
// actual native settled money. A not-yet-settled sale or an earmark in another
// currency cannot justify any new investment. Existing redemption gates remain.
func enforceCashSweepFundedReserves(p *protectionCashSweepPolicy, plan *cashSweepPlan) {
	if !p.reserveDesignEnabled() || plan.status.ReserveState != "ready" {
		return
	}
	var gaps []string
	for _, cp := range plan.currencies {
		c := cp.status
		if c.EffectiveReserve == nil || c.Cash == nil || c.Committed == nil || c.SettledCashSource != rpc.CashSweepSettledSourceBroker ||
			!finiteProtectionOptionPolicyValue(*c.EffectiveReserve) || !finiteProtectionOptionPolicyValue(*c.Cash) || !finiteProtectionOptionPolicyValue(*c.Committed) || *c.EffectiveReserve < 0 || *c.Committed < 0 {
			gaps = append(gaps, c.Currency+" settled funding is unavailable")
		} else if *c.Cash-*c.Committed+cashSweepMoneyEpsilon < *c.EffectiveReserve {
			gaps = append(gaps, c.Currency+" cash reserve is not funded")
		}
	}
	if len(gaps) == 0 {
		return
	}
	plan.status.ReserveState = "funding_hold"
	plan.status.ReserveReason = "reserve_native_funding_required: " + strings.Join(gaps, "; ")
	for i := range plan.currencies {
		cp := &plan.currencies[i]
		if cp.side == rpc.CashSweepSideInvest {
			cp.side = ""
			cp.status.State, cp.status.Reason = rpc.CashSweepStateHold, plan.status.ReserveReason
		}
	}
}

// prioritizeCashSweepRecords reuses only existing sweep slots. Other buckets
// keep their positions and execution eligibility.
func prioritizeCashSweepRecords(records []automaticSubmissionRecord, proposals []rpc.TradeProposal) {
	ranks := map[string]int{}
	for _, p := range proposals {
		if p.CashSweep != nil && p.CashSweep.PriorityRank > 0 {
			ranks[automaticRecordKey(p.Key, p.Revision)] = p.CashSweep.PriorityRank
		}
	}
	var slots []int
	var sweeps []automaticSubmissionRecord
	for i, rec := range records {
		if rec.Bucket == rpc.TradeProposalBucketCashSweep && ranks[automaticRecordKey(rec.Key, rec.Revision)] > 0 {
			slots, sweeps = append(slots, i), append(sweeps, rec)
		}
	}
	slices.SortStableFunc(sweeps, func(a, b automaticSubmissionRecord) int {
		return cmp.Compare(ranks[automaticRecordKey(a.Key, a.Revision)], ranks[automaticRecordKey(b.Key, b.Revision)])
	})
	for i, slot := range slots {
		records[slot] = sweeps[i]
	}
}
