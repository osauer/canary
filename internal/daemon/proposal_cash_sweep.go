package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// The cash sweep (internal-docs/design/cash-sweep.md; owner decisions S1–S6
// and O1–O6, 2026-09-30).
//
// Per currency, in that currency: cash is the lower of trade-date and settled
// cash, committed is working buy orders plus armed queued buys, and free is
// cash − committed − keep_cash. When free exceeds min_tranche the sweep buys
// one tranche of a vocabulary bill in the same currency, in whole units and
// held to max_order_notional; when cash less commitments falls below
// keep_cash it sells the nearest maturity (or the declared ETF) to cover the
// gap. Otherwise nothing. The sweep never converts. Unknown inputs generate
// nothing and say which: cash_unavailable, settlement_unknown,
// equivalents_unclassified, needs_your_number.
//
// Phase B resolves and quotes the bill an invest row names
// (cash_sweep_bills.go), classifies held bills (bond_directory.go) and orders
// them (cash_sweep_orders.go): an active row is an ordinary proposal whose
// BOND LMT DAY order is previewed under every existing gate and submitted on
// the owner's approval, or by the daemon after the full veto window when the
// owner lists cash_sweep under [authority].pre_authorised. Settled cash comes
// from the broker's per-currency $LEDGER SettledCash field. The order journal
// carries an unverified estimate only; without the broker observation the
// sweep holds. The tax review is advisory (owner decision
// 2026-09-30 12:35 CEST): an unreviewed sweep adds a detail line and blocks
// nothing.

// cashSweepLedgerRow is one currency's cash observation from the account
// ledger. Observed false means the row carried no cash balance; Settled is
// the ledger's SettledCash, nil when the gateway sent none.
type cashSweepLedgerRow struct {
	WebCashOriginalAsOf time.Time
	SettledSourceKind   string
	Observed            bool
	TradeDate           float64
	// ExchangeRate is base units per unit of this currency; 1 for the base.
	ExchangeRate  float64
	Settled       *float64
	SettledReason string
}

// cashSweepSettlement carries a legacy estimate from journal fills. The live
// adapter never certifies Known: actual settlement dates and account-wide
// fill coverage are unverified. Unknown names currencies a fill could not be
// attributed to (key "" applies to every currency).
type cashSweepSettlement struct {
	Known  bool
	Reason string
	// Since is the start of the settlement window.
	Since           time.Time
	Unknown         map[string]string
	SaleProceeds    map[string]float64
	PurchaseCosts   map[string]float64
	EquivalentSales map[string]float64
}

// cashSweepCommitments is the cash working buy orders and armed queued buys
// hold per currency. An unauthorised proposal or a prepared, unarmed queue
// entry is never a commitment.
type cashSweepCommitments struct {
	Known      bool
	Reason     string
	Unknown    map[string]string
	ByCurrency map[string]float64
}

// cashSweepHolding is one classified cash equivalent: a vocabulary bill with
// its maturity and face value, or the declared ETF (zero maturity).
type cashSweepHolding struct {
	Bond        rpc.PositionBond
	Row         rpc.PositionView
	Instrument  string
	Maturity    time.Time
	FaceValue   float64
	MarketValue float64
}

// cashSweepBillSearch is a completed bill contract search for one currency
// (Phase B). Only a completed search with no line lets the fallback act.
type cashSweepBillSearch struct {
	Completed bool
	Lines     int
}

// cashSweepInput is everything the pure planner reads. The engine gathers
// it; tests build it directly.
type cashSweepInput struct {
	OperationalFunding                   *risk.CashSweepOperationalObservation
	CalibrationStudies                   []risk.CashSweepCalibrationStudy
	AccountReceiptAt, PositionsReceiptAt time.Time
	PlanningSessionEpoch                 uint64
	PlanningDaemonStartedAt              time.Time
	FundingEvidence                      *risk.CashSweepFundingEvidence
	EffectiveReserves                    map[string]float64
	BufferAllocations                    map[string]float64
	BaseCurrency                         string
	// Ledger holds every currency the account ledger reports; LedgerReason
	// is set when the ledger itself is unavailable.
	Ledger       map[string]cashSweepLedgerRow
	LedgerReason string
	Settlement   cashSweepSettlement
	// Flex projections remain diagnostic until complete activity evidence exists.
	FlexProjections map[string]*rpc.CashSweepSettlementProjection
	Commitments     cashSweepCommitments
	// Holdings are the classified cash equivalents per currency;
	// Unclassified names why a currency's equivalents cannot be measured
	// (key "" applies to every currency).
	Holdings     map[string][]cashSweepHolding
	Unclassified map[string]string
	BillSearch   map[string]cashSweepBillSearch
}

// cashSweepCurrencyPlan is one currency's verdict and, for invest or
// redeem, the order it asks for.
type cashSweepCurrencyPlan struct {
	status rpc.TradeProposalCashSweepCurrency
	side   string
	// Band figures, valid once the state is past the unknown posture.
	cash, committed, free, pending, rate float64
	// Invest.
	instrument  string
	rung        int
	targetDays  int
	orderAmount float64
	quantity    int
	heldToCap   bool
	// bill is the resolved bill (cash_sweep_bills.go); nil until resolution
	// names one, and on a row built straight from the plan.
	bill *rpc.TradeProposalCashSweepBill
	// Redeem.
	holding *cashSweepHolding
	gap     float64
	// capUnits is the most units of the held equivalent one sale may sell
	// under max_order_notional.
	capUnits int
	// Order sizing once resolution ran: units in the bill's order unit on
	// the line's grid (rules), the session its order fills in, and what
	// blocks a redemption the grid cannot size.
	units    int
	rules    *ibkrlib.BondOrderRules
	session  *rpc.BondSession
	blockers []rpc.TradingBlocker
}

// cashSweepPlan is the pure result of one generation.
type cashSweepPlan struct {
	status     rpc.TradeProposalCashSweepStatus
	currencies []cashSweepCurrencyPlan
}

const cashSweepMoneyEpsilon = 1e-6

// cashSweepPlanFor measures every listed currency against the band. It
// generates no proposals.
func cashSweepPlanFor(policy protectionPolicy, in cashSweepInput, now time.Time) cashSweepPlan {
	bucket := policy.Buckets.CashSweep
	mode := bucket.effectiveMode()
	plan := cashSweepPlan{status: rpc.TradeProposalCashSweepStatus{
		OperationalFunding: risk.CloneCashSweepOperationalObservation(in.OperationalFunding),
		CalibrationStudies: risk.CloneCashSweepCalibrationStudies(in.CalibrationStudies),
		AccountReceiptAt:   in.AccountReceiptAt, PositionsReceiptAt: in.PositionsReceiptAt,
		PlanningSessionEpoch: in.PlanningSessionEpoch, PlanningDaemonStartedAt: in.PlanningDaemonStartedAt,
		Mode: mode, Shadow: mode == rpc.CashSweepModeShadow, Reason: in.LedgerReason,
		BaseCurrency: normCcy(in.BaseCurrency), TaxReviewedAt: string(bucket.TaxReviewedAt), TaxReviewed: bucket != nil && bucket.TaxReviewedAt != "",
		NeedsYourNumber: bucket.missingNumbers(), Currencies: []rpc.TradeProposalCashSweepCurrency{},
	}}
	if bucket != nil && bucket.MaxOrderNotional > 0 {
		plan.status.MaxOrderNotionalBase = new(bucket.MaxOrderNotional)
		plan.status.MinOrderNotionalBase = bucket.MinOrderNotional
		plan.status.MinNetGainBase = bucket.MinNetGain
	}
	applyCashSweepReservePolicy(bucket, &in, &plan.status, now)
	for _, ccy := range cashSweepCurrencies(bucket, in) {
		cp := cashSweepPlanCurrency(bucket, in, ccy, now)
		if bucket.reserveDesignEnabled() && plan.status.ReserveState != "ready" {
			cp.side = ""
			if cp.status.State == rpc.CashSweepStateInvest || cp.status.State == rpc.CashSweepStateRedeem || cp.status.State == rpc.CashSweepStateHold {
				cp.status.State, cp.status.Reason = rpc.CashSweepStateHold, plan.status.ReserveReason
			}
			cp.status.Free = nil
		}
		plan.currencies = append(plan.currencies, cp)
	}
	enforceCashSweepFundedReserves(bucket, &plan)
	orderCashSweepCurrencies(bucket, &plan)
	for _, cp := range plan.currencies {
		plan.status.Currencies = append(plan.status.Currencies, cp.status)
	}
	return plan
}

// cashSweepCurrencies lists the currencies the status reports: every ledger
// currency and every currency holding an equivalent. While the ledger is
// unavailable the owner's declared tables are listed too, so the status says
// cash_unavailable instead of going quiet. An opted-in reserve policy always
// includes every declared currency: missing cash must not erase its reserve.
func cashSweepCurrencies(bucket *protectionCashSweepPolicy, in cashSweepInput) []string {
	set := map[string]bool{}
	for ccy := range in.Ledger {
		set[normCcy(ccy)] = true
	}
	for ccy := range in.Holdings {
		set[normCcy(ccy)] = true
	}
	for ccy := range in.Unclassified {
		set[normCcy(ccy)] = true
	}
	if bucket != nil && (in.LedgerReason != "" || bucket.reserveDesignEnabled()) {
		for ccy := range bucket.Currency {
			set[ccy] = true
		}
	}
	delete(set, "")
	return slices.Sorted(maps.Keys(set))
}

func cashSweepPlanCurrency(bucket *protectionCashSweepPolicy, in cashSweepInput, ccy string, now time.Time) cashSweepCurrencyPlan {
	cfg := bucket.currency(ccy)
	if reserve, ok := in.EffectiveReserves[ccy]; ok {
		cfg.KeepCash = reserve
	}
	targets := cashSweepRungTargets(cfg.MinMaturityDays, cfg.MaxMaturityDays, cfg.LadderRungs)
	cp := cashSweepCurrencyPlan{status: rpc.TradeProposalCashSweepCurrency{
		Currency: ccy, Instruments: slices.Clone(cfg.Instruments), Fallback: cfg.Fallback,
		NeedsYourNumber: cfg.missingNumbers(), KeepCash: cfg.KeepCash, MinTranche: cfg.MinTranche,
		MinMaturityDays: cfg.MinMaturityDays, MaxMaturityDays: cfg.MaxMaturityDays, LadderRungs: cfg.LadderRungs,
	}}
	st := &cp.status
	st.WebCashOriginalAsOf, st.SettledSourceKind = in.Ledger[ccy].WebCashOriginalAsOf, in.Ledger[ccy].SettledSourceKind
	st.KeepCash = bucket.currency(ccy).KeepCash
	if reserve, ok := in.EffectiveReserves[ccy]; ok {
		st.EffectiveReserve, st.BufferAllocation = new(reserve), new(in.BufferAllocations[ccy])
		st.FundingNeed = new(in.FundingEvidence.FundingNative[ccy])
	}
	st.SettlementProjection = rpc.CloneCashSweepSettlementProjection(in.FlexProjections[ccy])
	st.Evidence = append(st.Evidence, flexCashProjectionEvidence(st.SettlementProjection)...)
	today := cashSweepDay(now)

	// Figures first, so every state shows what was observed.
	row, inLedger := in.Ledger[ccy]
	cashKnown := in.LedgerReason == "" && inLedger && row.Observed && finiteProtectionOptionPolicyValue(row.TradeDate) &&
		row.ExchangeRate > 0 && finiteProtectionOptionPolicyValue(row.ExchangeRate)
	settlementReason := cashSweepUnknownReason(in.Settlement.Known, in.Settlement.Reason, in.Settlement.Unknown, ccy)
	commitReason := cashSweepUnknownReason(in.Commitments.Known, in.Commitments.Reason, in.Commitments.Unknown, ccy)
	brokerSettled := row.Settled != nil && finiteProtectionOptionPolicyValue(*row.Settled)
	if cashKnown {
		cp.rate = row.ExchangeRate
		st.ExchangeRate, st.TradeDateCash = new(row.ExchangeRate), new(row.TradeDate)
		switch {
		case brokerSettled:
			// The broker's settled cash wins. Pending redemptions still come
			// from the journal; without it every unsettled net sale proceed
			// counts, which can only hold a redemption back.
			settled := *row.Settled
			cp.pending = max(0, row.TradeDate-settled)
			if settlementReason == "" {
				cp.pending = in.Settlement.EquivalentSales[ccy]
			}
			cp.cash = min(row.TradeDate, settled)
			st.SettledCash, st.Cash, st.PendingRedemptions = new(settled), new(cp.cash), new(cp.pending)
			st.SettledCashSource = rpc.CashSweepSettledSourceBroker
		case settlementReason == "":
			settled := row.TradeDate - in.Settlement.SaleProceeds[ccy] + in.Settlement.PurchaseCosts[ccy]
			cp.cash, cp.pending = min(row.TradeDate, settled), in.Settlement.EquivalentSales[ccy]
			st.SettledCash, st.Cash, st.PendingRedemptions = new(settled), new(cp.cash), new(cp.pending)
			st.SettledCashSource = rpc.CashSweepSettledSourceJournal
		}
	}
	if commitReason == "" {
		cp.committed = in.Commitments.ByCurrency[ccy]
		st.Committed = new(cp.committed)
	}
	if st.Cash != nil && st.Committed != nil {
		cp.free = cp.cash - cp.committed - cfg.KeepCash
		st.Free = new(cp.free)
	}
	unclassified := nonEmptyString(in.Unclassified[ccy], in.Unclassified[""])
	holdings := in.Holdings[ccy]
	faces := make([]float64, len(targets))
	if unclassified == "" {
		equivalents := 0.0
		for _, h := range holdings {
			if h.Instrument != cashSweepInstrumentETF {
				days := cashSweepDaysLeft(today, h.Maturity)
				if days > cashSweepMaturityCeilingDays {
					continue
				}
				faces[cashSweepNearestRung(targets, days)] += h.FaceValue
			}
			equivalents += h.MarketValue
		}
		st.CashEquivalents = new(equivalents)
	}
	if st.Cash != nil && st.CashEquivalents != nil {
		st.CashLike = new(*st.Cash + *st.CashEquivalents)
	}
	for k, target := range targets {
		st.Rungs = append(st.Rungs, rpc.TradeProposalCashSweepRung{Rung: k + 1, TargetDays: target, FaceValue: faces[k]})
	}

	plannable := cashSweepPlannable(cfg)
	switch {
	case slices.Equal(cfg.Instruments, []string{cashSweepInstrumentNone}):
		st.State, st.Reason = rpc.CashSweepStateNoInstrument, fmt.Sprintf("no instrument is declared for %s; its cash stays cash", ccy)
		st.Rungs = nil
		return cp
	case !cashKnown:
		st.State, st.Reason = rpc.CashSweepStateCashUnavailable, cashSweepCashReason(in, ccy, inLedger, row)
		return cp
	case settlementReason != "" && !brokerSettled:
		st.State, st.Reason = rpc.CashSweepStateSettlementUnknown, nonEmptyString(row.SettledReason, "the ledger carries no SettledCash for "+ccy+" and "+settlementReason)
		if st.SettlementProjection != nil {
			st.Reason = st.SettlementProjection.Reason
		}
		return cp
	case commitReason != "":
		st.State, st.Reason = rpc.CashSweepStateSettlementUnknown, commitReason
		return cp
	case unclassified != "":
		st.State, st.Reason = rpc.CashSweepStateEquivalentsUnclassified, unclassified
		return cp
	case len(bucket.missingNumbers()) > 0:
		st.State, st.Reason = rpc.CashSweepStateNeedsYourNumber, "needs your number: "+strings.Join(bucket.missingNumbers(), ", ")+" in [buckets.cash_sweep]; the sweep holds until you write it"
		return cp
	case len(plannable) == 0:
		st.State, st.Reason = rpc.CashSweepStateNeedsYourNumber, fmt.Sprintf("needs your number: %s in [buckets.cash_sweep.currency.%s]; the ETF is the only instrument declared", strings.Join(cfg.missingNumbers(), ", "), ccy)
		return cp
	}

	available := cp.cash - cp.committed
	switch {
	case in.Settlement.Known && cp.pending > cashSweepMoneyEpsilon:
		st.State, st.Reason = rpc.CashSweepStateHold, "pending bill-sale proceeds are restoring liquidity; do not reinvest before settlement"
	case cp.free > cashSweepMinimum(bucket, cfg, cp.rate)+cashSweepMoneyEpsilon:
		cashSweepPlanInvest(&cp, bucket, cfg, in, targets, faces, plannable)
	case available+cp.pending < cfg.KeepCash-cashSweepMoneyEpsilon:
		cashSweepPlanRedeem(&cp, bucket, cfg, holdings, now)
	default:
		st.State = rpc.CashSweepStateHold
		st.Reason = fmt.Sprintf("within the band: free cash %s is not above min_tranche %s, and cash less commitments %s is not below keep_cash %s",
			formatBudgetMoney(cp.free, ccy), formatBudgetMoney(cfg.MinTranche, ccy), formatBudgetMoney(available+cp.pending, ccy), formatBudgetMoney(cfg.KeepCash, ccy))
	}
	return cp
}

// cashSweepPlanInvest sizes one buy: the free cash, held to max_order_notional
// at the ledger rate, into the rung that holds least face value. A held order
// below min_tranche is no order.
func cashSweepPlanInvest(cp *cashSweepCurrencyPlan, bucket *protectionCashSweepPolicy, cfg protectionCashSweepCurrency, in cashSweepInput, targets []int, faces []float64, plannable []string) {
	st := &cp.status
	ccy := st.Currency
	instrument := plannable[0]
	if search := in.BillSearch[ccy]; search.Completed && search.Lines == 0 && cashSweepIsBill(instrument) {
		// Only a completed search with no line reaches past the bills: first
		// to a declared primary ETF, else to the fallback.
		instrument = ""
		for _, candidate := range plannable {
			if !cashSweepIsBill(candidate) {
				instrument = candidate
				break
			}
		}
		if instrument == "" && cfg.Fallback == cashSweepInstrumentETF && len(cfg.missingNumbers()) == 0 {
			instrument = cashSweepInstrumentETF
		}
		if instrument == "" {
			st.State, st.Reason = rpc.CashSweepStateHold, fmt.Sprintf("the contract search found no %s bill line and no usable fallback is declared; the cash stays cash", ccy)
			return
		}
	}
	capCcy := bucket.MaxOrderNotional / cp.rate
	order := min(cp.free, capCcy)
	cp.heldToCap = capCcy < cp.free-cashSweepMoneyEpsilon
	if order < cashSweepMinimum(bucket, cfg, cp.rate)-cashSweepMoneyEpsilon {
		st.State = rpc.CashSweepStateHold
		st.Reason = fmt.Sprintf("max_order_notional %s holds one order to %s, below min_tranche %s; nothing is swept",
			formatBudgetMoney(bucket.MaxOrderNotional, nonEmptyString(in.BaseCurrency, "base")), formatBudgetMoney(order, ccy), formatBudgetMoney(cfg.MinTranche, ccy))
		return
	}
	cp.side, cp.instrument, cp.orderAmount = rpc.CashSweepSideInvest, instrument, order
	cp.quantity = int(math.Floor(order + 1e-9))
	rung := 0
	for k := range faces {
		if faces[k] < faces[rung]-cashSweepMoneyEpsilon {
			rung = k
		}
	}
	cp.rung, cp.targetDays = rung+1, targets[rung]
	st.State = rpc.CashSweepStateInvest
	measure := "face"
	if instrument == cashSweepInstrumentETF {
		measure = "worth"
	}
	st.Reason = fmt.Sprintf("free cash %s is above min_tranche %s: buy %s %s of %s, rung %d (%d days)",
		formatBudgetMoney(cp.free, ccy), formatBudgetMoney(cfg.MinTranche, ccy), formatBudgetMoney(float64(cp.quantity), ccy), measure, instrument, cp.rung, cp.targetDays)
	if cp.heldToCap {
		st.Reason += "; max_order_notional holds this order, and the next cycle sweeps the rest"
	}
}

// cashSweepPlanRedeem sells the nearest maturity, or the ETF when no bill is
// held, to cover the gap below keep_cash, held to max_order_notional at the
// ledger rate like a buy (the next cycle sells the rest). A held bill that
// pays out before a sale today would settle makes the sale pointless.
func cashSweepPlanRedeem(cp *cashSweepCurrencyPlan, bucket *protectionCashSweepPolicy, cfg protectionCashSweepCurrency, holdings []cashSweepHolding, now time.Time) {
	today := cashSweepDay(now)
	st := &cp.status
	ccy := st.Currency
	cp.gap = cfg.KeepCash - (cp.cash - cp.committed + cp.pending)
	candidates := slices.Clone(holdings)
	slices.SortStableFunc(candidates, func(a, b cashSweepHolding) int {
		// Bills first, nearest maturity first; the ETF (zero maturity) last.
		if a.Maturity.IsZero() != b.Maturity.IsZero() {
			if a.Maturity.IsZero() {
				return 1
			}
			return -1
		}
		if c := a.Maturity.Compare(b.Maturity); c != 0 {
			return c
		}
		return a.Row.ConID - b.Row.ConID
	})
	var pick *cashSweepHolding
	for i := range candidates {
		if !candidates[i].Maturity.IsZero() && cashSweepDaysLeft(today, candidates[i].Maturity) > cashSweepMaturityCeilingDays {
			continue // not a cash equivalent
		}
		if candidates[i].Row.Quantity >= 1 && candidates[i].MarketValue > 0 {
			pick = &candidates[i]
			break
		}
	}
	if pick == nil {
		st.State = rpc.CashSweepStateHold
		st.Reason = fmt.Sprintf("cash less commitments is below keep_cash %s by %s, and no cash equivalent in %s is held to redeem",
			formatBudgetMoney(cfg.KeepCash, ccy), formatBudgetMoney(cp.gap, ccy), ccy)
		return
	}
	if !pick.Maturity.IsZero() {
		settles, reason := cashSweepSettlementDate(ccy, cfg.SettlementExchange, cfg.SettlementDays, string(cfg.SettlementValidThrough), now)
		if reason != "" {
			st.State, st.Reason = rpc.CashSweepStateHold, "sale held: "+reason
			return
		}
		if !pick.Maturity.After(settles) {
			st.State = rpc.CashSweepStateHold
			st.Reason = fmt.Sprintf("cash less commitments is below keep_cash %s by %s, but a held bill pays out on %s, before a sale today would settle",
				formatBudgetMoney(cfg.KeepCash, ccy), formatBudgetMoney(cp.gap, ccy), pick.Maturity.Format(time.DateOnly))
			return
		}
	}
	held := int(math.Floor(pick.Row.Quantity + 1e-9))
	unit := pick.MarketValue / pick.Row.Quantity
	target := max(cp.gap, cashSweepMinimum(bucket, cfg, cp.rate))
	needed := min(int(math.Ceil(target/unit-1e-9)), held)
	cp.capUnits = int(math.Floor(bucket.MaxOrderNotional/cp.rate/unit + 1e-9))
	if cp.capUnits < 1 {
		st.State = rpc.CashSweepStateHold
		st.Reason = fmt.Sprintf("cash less commitments is below keep_cash %s by %s, but max_order_notional %s holds one sale below one unit worth %s; nothing is sold",
			formatBudgetMoney(cfg.KeepCash, ccy), formatBudgetMoney(cp.gap, ccy), formatBudgetMoney(bucket.MaxOrderNotional, "base"), formatBudgetMoney(unit, ccy))
		return
	}
	cp.heldToCap = cp.capUnits < needed
	cp.quantity = max(1, min(needed, cp.capUnits))
	cp.side, cp.instrument, cp.holding, cp.orderAmount = rpc.CashSweepSideRedeem, pick.Instrument, pick, min(target, bucket.MaxOrderNotional/cp.rate)
	st.State = rpc.CashSweepStateRedeem
	what := "the declared ETF"
	if !pick.Maturity.IsZero() {
		what = "the bill maturing " + pick.Maturity.Format(time.DateOnly)
	}
	st.Reason = fmt.Sprintf("cash less commitments is below keep_cash %s by %s: sell %d of %d of %s",
		formatBudgetMoney(cfg.KeepCash, ccy), formatBudgetMoney(cp.gap, ccy), cp.quantity, held, what)
	if cp.heldToCap {
		st.Reason += "; max_order_notional holds this sale, and the next cycle sells the rest"
	}
}

// cashSweepPlannable lists the declared instruments the sweep can plan with:
// none never, the ETF only once its symbol and exchange are written.
func cashSweepPlannable(cfg protectionCashSweepCurrency) []string {
	var out []string
	for _, instrument := range cfg.Instruments {
		switch {
		case instrument == cashSweepInstrumentNone:
		case instrument == cashSweepInstrumentETF && len(cfg.missingNumbers()) > 0:
		default:
			out = append(out, instrument)
		}
	}
	return out
}

func cashSweepIsBill(instrument string) bool {
	_, ok := cashSweepBillCurrency[instrument]
	return ok
}

// cashSweepUnknownReason names why an input cannot vouch for ccy, or "".
func cashSweepUnknownReason(known bool, reason string, unknown map[string]string, ccy string) string {
	if !known {
		return nonEmptyString(reason, "unavailable")
	}
	return nonEmptyString(unknown[ccy], unknown[""])
}

func cashSweepCashReason(in cashSweepInput, ccy string, inLedger bool, row cashSweepLedgerRow) string {
	switch {
	case in.LedgerReason != "":
		return in.LedgerReason
	case !inLedger:
		return fmt.Sprintf("the account ledger has no %s row; its cash is unavailable, not zero", ccy)
	case !row.Observed:
		return fmt.Sprintf("the account ledger's %s row carries no cash balance", ccy)
	default:
		return fmt.Sprintf("the account ledger has no exchange rate for %s, so max_order_notional cannot be compared", ccy)
	}
}

// cashSweepRungTargets spreads the ladder's target maturities evenly from
// min to max days (O2). A single rung targets max.
func cashSweepRungTargets(minDays, maxDays, rungs int) []int {
	if rungs <= 1 {
		return []int{maxDays}
	}
	out := make([]int, rungs)
	for k := range rungs {
		out[k] = minDays + int(math.Round(float64(k*(maxDays-minDays))/float64(rungs-1)))
	}
	return out
}

// cashSweepNearestRung is the rung whose target is nearest days; a tie goes
// to the shorter rung.
func cashSweepNearestRung(targets []int, days int) int {
	best := 0
	for k, target := range targets {
		if cashSweepAbs(target-days) < cashSweepAbs(targets[best]-days) {
			best = k
		}
	}
	return best
}

func cashSweepAbs(v int) int {
	return max(v, -v)
}

// cashSweepDay is now's calendar day in UTC.
func cashSweepDay(now time.Time) time.Time {
	now = now.UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}

// cashSweepDaysLeft counts calendar days from today to a maturity date.
func cashSweepDaysLeft(today, maturity time.Time) int {
	return int(math.Ceil(cashSweepDay(maturity).Sub(today).Hours() / 24))
}

// cashSweepSettlementWindowStart bounds the legacy T+1 estimate. It has no
// settlement-calendar or T+2 authority and never certifies settled cash.
func cashSweepSettlementWindowStart(now time.Time) time.Time {
	day := cashSweepDay(now).AddDate(0, 0, -1)
	for day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
		day = day.AddDate(0, 0, -1)
	}
	return day
}

// cashSweepProposals is the bucket's generation: the typed status and its
// rows. Nil, nil while the bucket is not enabled.
func (e *proposalEngine) cashSweepProposals(ctx context.Context, policy protectionPolicy, status rpc.ProtectionPolicyStatus, acct *rpc.AccountResult, pos *rpc.PositionsResult, sources rpc.TradeProposalSourceFingerprints, scope brokerStateScope, now time.Time) ([]rpc.TradeProposal, *rpc.TradeProposalCashSweepStatus) {
	if !policy.Buckets.CashSweep.enabled() {
		return nil, nil
	}
	plan := cashSweepPlanFor(policy, e.cashSweepInput(ctx, policy, acct, pos, scope, now), now)
	// An invest row exists only for a bill confirmed by contract details and
	// a quote and sized on its grid; a redemption reads its held bill's grid.
	cashSweepResolveBills(ctx, e.cashSweepBillSourceFor(), policy.Buckets.CashSweep, &plan, now)
	var out []rpc.TradeProposal
	for _, cp := range plan.currencies {
		if cp.side == "" || (cp.side == rpc.CashSweepSideInvest && cp.bill == nil) {
			continue
		}
		p := cashSweepRow(policy, status, sources, now, plan, cp)
		if e.isIgnored(scope, p.Key) {
			continue
		}
		e.applyBillUnitLatch(&p)
		out = append(out, p)
	}
	st := plan.status
	st.Rows = len(out)
	if err := e.persistCashSweepTrace(ctx, policy, sources, scope, now, &st); err != nil {
		st.TraceState = "unavailable"
		if policy.Buckets.CashSweep.reserveDesignEnabled() {
			out, st.Rows = nil, 0
			st.Reason = "decision_trace_unavailable: sweep held until SQLite audit is available"
			for i := range st.Currencies {
				st.Currencies[i].State, st.Currencies[i].Reason = rpc.CashSweepStateHold, st.Reason
			}
		}
	}
	return out, &st
}

// cashSweepRow builds one sweep proposal. Every row waits the full veto
// window. An invest row buys its resolved bill in the bill's order unit; a
// redemption sells the held equivalent in the position's unit. A row built
// without resolution (no bill) is blocked: there is nothing to order.
func cashSweepRow(policy protectionPolicy, status rpc.ProtectionPolicyStatus, sources rpc.TradeProposalSourceFingerprints, now time.Time, plan cashSweepPlan, cp cashSweepCurrencyPlan) rpc.TradeProposal {
	bucket := policy.Buckets.CashSweep
	cfg := bucket.currency(cp.status.Currency)
	if cp.status.EffectiveReserve != nil {
		cfg.KeepCash = *cp.status.EffectiveReserve
	}
	ccy := cp.status.Currency
	block := &rpc.TradeProposalCashSweep{
		PriorityRank: cp.status.PriorityRank,
		Mode:         plan.status.Mode, Side: cp.side, Currency: ccy, Instrument: cp.instrument, OrderAmount: cp.orderAmount,
		Cash: cp.cash, Committed: cp.committed, KeepCash: cfg.KeepCash, Free: cp.free, MinTranche: cfg.MinTranche,
		MinOrderNotionalBase: bucket.MinOrderNotional, MinNetGainBase: bucket.MinNetGain,
		CashInterestRateUpper: cloneFloat64Ptr(cfg.CashInterestRateUpper), CashInterestValidThrough: string(cfg.CashInterestValidThrough),
		SettlementDays: cfg.SettlementDays, SettlementExchange: cfg.SettlementExchange, SettlementValidThrough: string(cfg.SettlementValidThrough),
		Session: rpc.CloneBondSession(cp.session),
	}
	var p rpc.TradeProposal
	details := []string{fmt.Sprintf("cash %s (the lower of trade-date %s and settled %s) · committed %s · keep_cash %s · free %s",
		formatBudgetMoney(cp.cash, ccy), formatBudgetMoney(derefFloat(cp.status.TradeDateCash), ccy), formatBudgetMoney(derefFloat(cp.status.SettledCash), ccy),
		formatBudgetMoney(cp.committed, ccy), formatBudgetMoney(cfg.KeepCash, ccy), formatBudgetMoney(cp.free, ccy))}
	var blockers []rpc.TradingBlocker
	switch cp.side {
	case rpc.CashSweepSideInvest:
		block.Rung, block.TargetDays = cp.rung, cp.targetDays
		block.MinMaturityDays, block.MaxMaturityDays = cfg.MinMaturityDays, cfg.MaxMaturityDays
		block.MaxOrderNotionalBase, block.ExchangeRate, block.HeldToCap = bucket.MaxOrderNotional, cp.rate, cp.heldToCap
		b := cp.bill
		if b == nil {
			p = cashSweepProposal(policy, status, sources, now, cashSweepInvestContract(cfg, ccy, cp.instrument), cashSweepKey(ccy, rpc.CashSweepSideInvest, cp.instrument, 0),
				rpc.OrderActionBuy, cp.quantity, cp.quantity, 0, rpc.OrderPositionEffectOpen, cp.status.Reason)
			blockers = append(blockers, rpc.TradingBlocker{Code: rpc.CashSweepStateInstrumentUnresolved,
				Message: "no bill was confirmed by contract details and a quote this cycle, so there is nothing to order",
				Action:  "Refresh proposals; the sweep names a bill once one resolves and quotes."})
			break
		}
		conv := cashSweepInstrumentConventions[b.Instrument]
		block.Bill = rpc.CloneCashSweepBill(b)
		block.Instrument = b.Instrument
		block.QuantityUnit = conv.QuantityUnit
		block.FaceValue = float64(cp.units) * conv.FacePerUnit
		if b.Price != nil {
			block.EstimatedCost = block.FaceValue * *b.Price / 100
		}
		if block.Session == nil {
			block.Session = rpc.CloneBondSession(b.Session)
		}
		contract := rpc.ContractParams{ConID: b.ConID, Symbol: nonEmptyString(b.Symbol, nonEmptyString(b.CUSIP, b.ISIN)), SecType: ibkrlib.BillOrBondSecType(b.SecType), Exchange: "SMART", Currency: ccy}
		// The key binds the bill: a preview or submit of this key buys the
		// bill the owner saw, never another the next cycle names.
		p = cashSweepProposal(policy, status, sources, now, contract, cashSweepKey(ccy, rpc.CashSweepSideInvest, cp.instrument, b.ConID),
			rpc.OrderActionBuy, cp.units, cp.units, 0, rpc.OrderPositionEffectOpen, cp.status.Reason)
		p.Notional = block.EstimatedCost
		details = append(details, fmt.Sprintf("rung %d of %d targets %d days; the bill must mature in %d–%d days", cp.rung, cfg.LadderRungs, cp.targetDays, cfg.MinMaturityDays, cfg.MaxMaturityDays))
		if cp.heldToCap {
			details = append(details, fmt.Sprintf("order held to %s by max_order_notional %s at %.4f; the next cycle sweeps the rest",
				formatBudgetMoney(cp.orderAmount, ccy), formatBudgetMoney(bucket.MaxOrderNotional, nonEmptyString(plan.status.BaseCurrency, "base")), cp.rate))
		}
		quote := "live"
		if !b.QuoteFresh {
			reason := ""
			if b.Quote != nil {
				reason = b.Quote.StaleReason
			}
			quote = "stale: " + nonEmptyString(reason, "not a live bid or ask")
		}
		details = append(details, fmt.Sprintf("bill %s (%s) matures %s (%d days); quoted %s per 100 of face (%s, %s)",
			cashSweepBillName(*b), b.Instrument, b.Maturity, b.DaysToMaturity, formatBillPrice(b.Price), nonEmptyString(b.PriceSource, "no price"), quote))
		details = append(details, cashSweepBillMaturityDetail(*b))
		details = append(details, fmt.Sprintf("buy %d × %s = %s of face, about %s at that price; the preview prices a limit on the bill's %s tick from a live bid and ask",
			cp.units, conv.QuantityUnit, formatBudgetMoney(block.FaceValue, ccy), formatBudgetMoney(block.EstimatedCost, ccy), formatBillTick(b.MinTick)))
	case rpc.CashSweepSideRedeem:
		h := cp.holding
		held := int(math.Floor(h.Row.Quantity + 1e-9))
		qty := cp.quantity
		if cp.units > 0 {
			qty = cp.units
		}
		effect := rpc.OrderPositionEffectReduce
		if qty >= held {
			effect = rpc.OrderPositionEffectClose
		}
		block.QuantityUnit = rpc.CashSweepQuantityPosition
		block.RedemptionTarget = cp.gap
		block.MaxOrderNotionalBase, block.ExchangeRate, block.HeldToCap = bucket.MaxOrderNotional, cp.rate, cp.heldToCap
		if !h.Maturity.IsZero() {
			block.MaturityDate = h.Maturity.Format(time.DateOnly)
			block.MaturitySource, block.MaturitySourceAsOf = h.Bond.MaturitySource, h.Bond.MaturitySourceAsOf
			block.CUSIP, block.ISIN = h.Bond.CUSIP, h.Bond.ISIN
			block.ResolutionSource = h.Bond.ResolutionSource
		}
		contract := cashSweepHoldingContract(h.Row)
		p = cashSweepProposal(policy, status, sources, now, contract, cashSweepKey(ccy, rpc.CashSweepSideRedeem, h.Instrument, h.Row.ConID),
			rpc.OrderActionSell, qty, cashSweepSaleLimit(held, cp.capUnits, cp.rules), h.Row.Quantity, effect, cp.status.Reason)
		if mark := h.MarketValue / h.Row.Quantity; mark > 0 {
			p.Notional = mark * float64(qty)
		}
		if cp.rules != nil && qty != cp.quantity {
			details = append(details, fmt.Sprintf("the sale of %d is rounded to %d on the bill's size grid (minimum %d, step %d)", cp.quantity, qty, cp.rules.Minimum(), cp.rules.Step()))
		}
		details = append(details, fmt.Sprintf("pending redemptions %s count toward keep_cash until they settle", formatBudgetMoney(cp.pending, ccy)))
		blockers = append(blockers, cp.blockers...)
	}
	if s := block.Session; s != nil && len(s.Windows) > 0 {
		details = append(details, fmt.Sprintf("the order fills in the %s session (%s)", s.Label, cashSweepSessionSource(s.Source)))
	}
	if bucket.TaxReviewedAt == "" {
		details = append(details, rpc.CashSweepTaxUnreviewedDetail)
	}
	details = append(details, "waits the full veto window: a sweep is never a stop")
	p.CashSweep = block
	blockers = append(blockers, cashSweepCalendarBlockers(block, p.Contract.Exchange, now)...)
	p.Details = details
	p.Shadow = plan.status.Shadow
	p.NeverSkipVeto = true
	if cp.side == rpc.CashSweepSideRedeem && cp.holding.Row.Stale {
		cashSweepBlock(&p, rpc.TradingBlocker{Code: rpc.CashSweepBlockerFreshQuote, Message: "the held position's mark is stale, so the sale cannot be sized or priced",
			Action: "Refresh during the instrument's session so the mark is current."})
	}
	if cp.side == rpc.CashSweepSideInvest && cp.bill != nil && !cp.bill.QuoteFresh {
		cashSweepBlock(&p, rpc.TradingBlocker{Code: rpc.CashSweepBlockerFreshQuote, Message: "the bill's quote is not a live bid or ask, so the buy cannot be priced",
			Action: "Refresh during the bill's trading session so the quote is live."})
	}
	for _, b := range blockers {
		cashSweepBlock(&p, b)
	}
	if p.Shadow {
		// In front of every other blocker: the mode is the first thing a
		// reader must know about the row.
		p.Blockers = append([]rpc.TradingBlocker{cashSweepShadowBlocker()}, p.Blockers...)
	}
	return p
}

// cashSweepSessionSource words where a bill session's hours came from.
func cashSweepSessionSource(source string) string {
	switch source {
	case rpc.BondSessionSourceLiquidHours:
		return "the contract's liquid hours"
	case rpc.BondSessionSourceTradingHours:
		return "the contract's trading hours"
	}
	return "assumed hours: the contract details carried none"
}

func formatBillTick(v *float64) string {
	if v == nil {
		return "minimum"
	}
	return strconv.FormatFloat(*v, 'f', -1, 64)
}

// cashSweepProposal is the contract-based counterpart of baseProposal: an
// invest row names an instrument Canary has not resolved to a held position.
func cashSweepProposal(policy protectionPolicy, status rpc.ProtectionPolicyStatus, sources rpc.TradeProposalSourceFingerprints, now time.Time, contract rpc.ContractParams, key, action string, qty, maxQty int, positionQty float64, effect, reason string) rpc.TradeProposal {
	return rpc.TradeProposal{
		Key: key, State: rpc.TradeProposalStateGenerated, Bucket: rpc.TradeProposalBucketCashSweep,
		Symbol: contract.Symbol, SecType: contract.SecType, Action: action, Quantity: qty, MaxQuantity: maxQty,
		PositionQuantity: positionQty, PositionEffect: effect, OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay,
		Contract: contract, Reason: reason, PolicyID: policy.PolicyID, PolicyVersion: policy.PolicyVersion,
		PolicyFingerprint: status.Fingerprint, SourceFingerprints: sources, CreatedAt: now,
	}
}

// cashSweepInvestContract names what an invest row would buy before a bill
// resolves (a row built straight from the plan, blocked): the vocabulary
// instrument, or the owner's ETF declaration.
func cashSweepInvestContract(cfg protectionCashSweepCurrency, ccy, instrument string) rpc.ContractParams {
	if instrument == cashSweepInstrumentETF {
		return rpc.ContractParams{Symbol: cfg.ETFSymbol, SecType: "STK", Exchange: "SMART", PrimaryExch: cfg.ETFExchange, Currency: ccy}
	}
	return rpc.ContractParams{Symbol: strings.ToUpper(instrument), SecType: cashSweepInstrumentSecTypes(instrument)[0], Exchange: "SMART", Currency: ccy}
}

// cashSweepHoldingContract is the held equivalent a redemption sells, by
// contract id. A bill keeps its own BILL or BOND type.
func cashSweepHoldingContract(row rpc.PositionView) rpc.ContractParams {
	if ibkrlib.IsBillOrBond(row.SecType) {
		c := proposalContractFromPosition(row, ibkrlib.BillOrBondSecType(row.SecType))
		c.Exchange = nonEmptyString(row.Exchange, "SMART")
		return c
	}
	return proposalContractFromPosition(row, positionWireSecType(row.SecType))
}

// cashSweepKey is a sweep row's stable key: currency, side, the planned
// instrument and the contract it trades (the resolved bill of an invest row,
// the held line of a redemption). An invest row keeps its key while the rung
// it targets moves and changes it when the bill does.
func cashSweepKey(ccy, side, instrument string, conID int) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{rpc.TradeProposalBucketCashSweep, ccy, side, instrument, strconv.Itoa(conID)}, "|")))
	return rpc.TradeProposalBucketCashSweep + ":" + hex.EncodeToString(sum[:8])
}

func cashSweepBlock(p *rpc.TradeProposal, blocker rpc.TradingBlocker) {
	p.State = rpc.TradeProposalStateBlocked
	p.Blockers = appendTradingBlockerOnce(p.Blockers, blocker)
}

func cashSweepShadowBlocker() rpc.TradingBlocker {
	return rpc.TradingBlocker{
		Code:    "shadow_mode",
		Message: "the cash sweep runs in shadow mode; this row is listed for observation and cannot be previewed or submitted",
		Action:  "Set mode = \"active\" under [buckets.cash_sweep] and bump policy_version to make these rows ordinary proposals.",
	}
}

func derefFloat(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}

// cashSweepCounts counts the sweep's rows and the shadow subset.
func cashSweepCounts(proposals []rpc.TradeProposal) (rows, shadow int) {
	for _, p := range proposals {
		if p.Bucket != rpc.TradeProposalBucketCashSweep {
			continue
		}
		rows++
		if p.Shadow {
			shadow++
		}
	}
	return rows, shadow
}

// cashSweepInput gathers the planner's inputs for the connected scope.
func (e *proposalEngine) cashSweepInput(ctx context.Context, policy protectionPolicy, acct *rpc.AccountResult, pos *rpc.PositionsResult, scope brokerStateScope, now time.Time) cashSweepInput {
	in := cashSweepInput{}
	if acct != nil && acct.Authority != nil {
		in.AccountReceiptAt = acct.Authority.AsOf
	}
	if pos != nil && pos.Authority != nil {
		in.PositionsReceiptAt = pos.Authority.AsOf
	}
	in.PlanningDaemonStartedAt = e.startedAt
	if e.server != nil {
		e.server.mu.Lock()
		in.PlanningSessionEpoch = e.server.connectorEpoch
		e.server.mu.Unlock()
	}
	in.Holdings, in.Unclassified = cashSweepClassify(policy.Buckets.CashSweep, pos)
	in.Settlement = e.cashSweepSettlement(scope, now)
	in.Commitments = e.cashSweepCommitments(ctx, scope)
	// Broker reads can finish after the refresh's planning timestamp. Check
	// cash at consumption time, retaining now for the planning day/calendar.
	cashNow := now
	if e.server != nil {
		cashNow = e.server.nowUTC()
	}
	in.BaseCurrency, in.Ledger, in.LedgerReason = cashSweepLedgerAt(acct, cashNow)
	e.server.cashLedgerValidatePlanning(acct, scope, &in, cashNow)
	e.attachFlexCashProjections(ctx, policy.Buckets.CashSweep, acct, scope, &in)
	if policy.Buckets.CashSweep.reserveDesignEnabled() {
		in.OperationalFunding, in.CalibrationStudies = e.observeCashSweepFunding(acct, pos, scope, in, cashNow)
	}
	return in
}

// cashSweepLedger reads cash per currency from a current one-shot account
// ledger: the non-base rows and the base row. Anything less is unavailable.
func cashSweepLedger(acct *rpc.AccountResult) (string, map[string]cashSweepLedgerRow, string) {
	return cashSweepLedgerAt(acct, time.Now().UTC())
}

func cashSweepLedgerAt(acct *rpc.AccountResult, now time.Time) (string, map[string]cashSweepLedgerRow, string) {
	if acct == nil || !currentPortfolioAuthority(acct.Authority) || acct.AccountID != acct.Authority.Scope.AccountID {
		return "", nil, "the account summary is not current for the connected account; cash is unavailable, not zero"
	}
	fields := acct.Authority.Fields
	base := normCcy(acct.BaseCurrency)
	if fields == nil || !fields.BaseCurrency || !fields.CurrencyExposure || base == "" {
		return base, nil, "the account's per-currency ledger is not proven current; cash is unavailable, not zero"
	}
	ledger := map[string]cashSweepLedgerRow{}
	for _, row := range acct.CurrencyExposure {
		if ccy := normCcy(row.Currency); ccy != "" && ccy != base {
			ledger[ccy] = cashSweepCashObservation(row, acct, now)
		}
	}
	if row := acct.BaseCurrencyLedger; row != nil {
		ledger[base] = cashSweepCashObservation(*row, acct, now)
	}
	return base, ledger, ""
}

// cashSweepClassify sorts held positions into cash equivalents. A BOND row
// is classified by the positions' bonds section (bond_directory.go): a
// vocabulary bill of the row's currency is an equivalent with its maturity
// and face value; any other bond is not; a row that section could not
// resolve makes its currency unclassified. The declared ETF still has no
// contract id, so a holding carrying its symbol makes its currency
// unclassified: the symbol only ever blocks; it never admits.
func cashSweepClassify(bucket *protectionCashSweepPolicy, pos *rpc.PositionsResult) (map[string][]cashSweepHolding, map[string]string) {
	unclassified := map[string]string{}
	if pos == nil {
		return nil, unclassified
	}
	classified := map[int]rpc.PositionBond{}
	for _, b := range pos.Bonds {
		if b.ConID > 0 {
			classified[b.ConID] = b
		}
	}
	var holdings map[string][]cashSweepHolding
	bills := map[string]int{}
	for _, row := range slices.Concat(pos.Stocks, pos.Options) {
		if row.Quantity == 0 {
			continue
		}
		ccy := normCcy(row.Currency)
		switch strings.ToUpper(strings.TrimSpace(row.SecType)) {
		case "BOND", "BILL":
			b, ok := classified[row.ConID]
			if !ok || row.ConID <= 0 || ccy == "" || b.Class == rpc.BondClassUnresolved {
				bills[ccy]++
				continue
			}
			instrument := cashSweepHeldBillInstrument(b, ccy)
			if instrument == "" {
				continue // a bond, or another issuer's bill: not a cash equivalent
			}
			maturity, err := time.Parse(time.DateOnly, b.Maturity)
			if err != nil {
				// A recognized cash equivalent cannot disappear from the ladder
				// or read as zero merely because its broker date is unavailable.
				bills[ccy]++
				continue
			}
			if holdings == nil {
				holdings = map[string][]cashSweepHolding{}
			}
			holdings[ccy] = append(holdings[ccy], cashSweepHolding{Row: row, Bond: b, Instrument: instrument, Maturity: maturity,
				FaceValue: row.Quantity * cashSweepInstrumentConventions[instrument].FacePerUnit, MarketValue: row.MarketValue})
		case "STK", "STOCK", "ETF":
			cfg := bucket.currency(ccy)
			if cfg.declaresETF() && cfg.ETFSymbol != "" && strings.EqualFold(strings.TrimSpace(row.Symbol), cfg.ETFSymbol) {
				unclassified[ccy] = fmt.Sprintf("a %s holding carries the declared ETF's symbol, and the ETF is matched by contract id, which Canary does not resolve yet", ccy)
			}
		}
	}
	for ccy, n := range bills {
		label := nonEmptyString(ccy, "an unknown currency")
		unclassified[ccy] = fmt.Sprintf("%d bond or bill %s in %s could not be classified: its contract identity or maturity is unavailable (see the positions' bonds section)", n, pluralNoun(n, "holding"), label)
	}
	return holdings, unclassified
}

// cashSweepSettlement retains the journal's settlement estimate, but it
// cannot certify settled cash: the journal lacks actual settlement dates,
// settlement-calendar authority and complete account-wide fill coverage.
// Only a broker SettledCash observation can currently admit a sweep.
func (e *proposalEngine) cashSweepSettlement(scope brokerStateScope, now time.Time) cashSweepSettlement {
	since := cashSweepSettlementWindowStart(now)
	if e == nil || e.server == nil {
		return cashSweepSettlement{Since: since, Reason: "no order journal is attached; settled cash is unknown"}
	}
	if started := e.server.startedAt; started.IsZero() || started.After(since) {
		return cashSweepSettlement{Since: since, Reason: fmt.Sprintf("Canary's order journal covers fills only since the daemon started (%s), inside the settlement estimate window from %s; it cannot prove settled cash, which requires a broker SettledCash observation",
			started.UTC().Format(time.RFC3339), since.Format(time.DateOnly))}
	}
	views, eventsByKey, err := e.server.loadOrderViews()
	if err != nil {
		reason := "the order journal is unreadable, so fills in the settlement window are unknown"
		if errors.Is(err, ErrTradingDisabled) {
			reason = "this build keeps no order journal, so fills in the settlement window are unknown"
		}
		return cashSweepSettlement{Since: since, Reason: reason}
	}
	estimate := cashSweepSettlementFrom(views, eventsByKey, scope, since)
	estimate.Known = false
	estimate.Reason = "the order journal has no verified settlement dates or complete account-wide fill coverage; settled cash requires a broker SettledCash observation"
	return estimate
}

// cashSweepSettlementFrom sums the scope's fills since the window start per
// currency, from each order's cumulative fill events. A bond fill is valued
// at its currency's bill convention (price per 100 × face per unit, A5); a
// fill it cannot attribute to one currency (no currency, a conversion) or
// cannot value (a bond in a currency without a bill convention) makes that
// currency unknown.
func cashSweepSettlementFrom(views []rpc.OrderView, eventsByKey map[string][]rpc.OrderEvent, scope brokerStateScope, since time.Time) cashSweepSettlement {
	out := cashSweepSettlement{Known: true, Since: since, Unknown: map[string]string{},
		SaleProceeds: map[string]float64{}, PurchaseCosts: map[string]float64{}, EquivalentSales: map[string]float64{}}
	for _, view := range views {
		if !orderViewMatchesBrokerScope(view, scope) {
			continue
		}
		ccy := normCcy(view.Currency)
		secType := strings.ToUpper(strings.TrimSpace(view.SecType))
		filled, cost := 0.0, 0.0
		for _, ev := range eventsByKey[orderViewKey(view)] {
			if ev.Filled <= filled+1e-9 {
				continue
			}
			price := ev.AvgFillPrice
			cumulative := ev.Filled * price
			delta := cumulative - cost
			if price <= 0 {
				delta = 0
			}
			quantity := ev.Filled - filled
			filled, cost = ev.Filled, cumulative
			if ev.At.Before(since) {
				continue
			}
			switch {
			case secType == "CASH":
				out.Unknown[""] = "a currency conversion filled inside the settlement window; its settlement is not modelled"
				continue
			case ccy == "":
				out.Unknown[""] = "a fill inside the settlement window carries no currency"
				continue
			case price <= 0 || quantity <= 0:
				out.Unknown[ccy] = fmt.Sprintf("a fill in %s inside the settlement window has no fill price", ccy)
				continue
			}
			multiplier, ok := cashSweepMultiplier(secType, view.Multiplier, ccy)
			if !ok && cashSweepBondSecType(secType) {
				out.Unknown[ccy] = fmt.Sprintf("a bond fill in %s inside the settlement window has no bill convention to value it", ccy)
				continue
			}
			if !ok {
				out.Unknown[ccy] = fmt.Sprintf("a %s fill in %s inside the settlement window has no multiplier", secType, ccy)
				continue
			}
			amount := delta * multiplier
			switch strings.ToUpper(strings.TrimSpace(view.Action)) {
			case rpc.OrderActionBuy:
				out.PurchaseCosts[ccy] += amount
			case rpc.OrderActionSell:
				out.SaleProceeds[ccy] += amount
				if cashSweepBondSecType(secType) {
					// A bill sold inside the window is a pending redemption: it
					// counts toward keep_cash so the sale is not repeated
					// while its proceeds settle.
					out.EquivalentSales[ccy] += amount
				}
			default:
				out.Unknown[ccy] = fmt.Sprintf("a fill in %s inside the settlement window has no side", ccy)
			}
		}
	}
	return out
}

// cashSweepMultiplier is the cash multiplier of one unit at its price: 1 for
// a stock or ETF share; for a bond, its currency's face per unit over 100
// (prices are per 100 of face, A5); the contract's own for anything else.
func cashSweepMultiplier(secType string, multiplier int, ccy string) (float64, bool) {
	switch secType {
	case "STK", "STOCK", "ETF":
		return 1, true
	case "BOND", "BILL":
		conv, ok := cashSweepBondConvention(ccy)
		if !ok || conv.FacePerUnit <= 0 {
			return 0, false
		}
		return conv.FacePerUnit / 100, true
	}
	if multiplier > 0 {
		return float64(multiplier), true
	}
	return 0, false
}

func cashSweepBondSecType(secType string) bool {
	return ibkrlib.IsBillOrBond(secType)
}

// cashSweepCommitments sums what working buy orders (every client, from the
// broker's complete open-order inventory) and armed queued buys hold per
// currency.
func (e *proposalEngine) cashSweepCommitments(ctx context.Context, scope brokerStateScope) cashSweepCommitments {
	if e == nil || e.server == nil || ctx == nil {
		return cashSweepCommitments{Reason: "no broker open-order inventory is attached; commitments are unknown"}
	}
	snapshot, snapScope, err := e.server.brokerOpenOrderInventory(ctx, false)
	if err != nil || !sameBrokerScope(snapScope, scope) {
		return cashSweepCommitments{Reason: "complete, current open-order inventory from every client is unavailable, so committed cash is unknown"}
	}
	events, err := e.server.orderJournal.LoadEvents(0)
	if err != nil {
		return cashSweepCommitments{Reason: "durable commission evidence is unavailable, so fee-inclusive commitments are unknown"}
	}
	e.server.mu.Lock()
	ep := e.server.endpoint
	e.server.mu.Unlock()
	endpoint := e.server.tradingStatus(ep).Endpoint
	return cashSweepCommitmentsFrom(snapshot.Orders, e.queued.list(), scope, cashSweepFeeEvidence{Events: events, Now: e.clock(), Endpoint: endpoint})
}

// cashSweepCommitmentsFrom is the pure half of cashSweepCommitments. A working
// buy is valued at its fixed limit price, a bond's at its currency's bill
// convention; one with no price bound makes its currency unknown. A queued
// buy counts only while armed, held or sending, at its worst price. These
// observations need an exact, current durable send-attempt fee bound. Legacy
// records and external buys keep fee-inclusive commitments unknown.
func cashSweepCommitmentsFrom(orders []ibkrlib.OrderLifecycleEvent, queued []queuedAuthRecord, scope brokerStateScope, evidence ...cashSweepFeeEvidence) cashSweepCommitments {
	out := cashSweepCommitments{Known: true, Unknown: map[string]string{}, ByCurrency: map[string]float64{}}
	if len(evidence) == 1 {
		cashSweepUnacknowledgedBuyGuard(&out, orders, scope, evidence[0])
	}
	for _, o := range orders {
		if !brokerOrderWorking(o) || !strings.EqualFold(strings.TrimSpace(o.Action), rpc.OrderActionBuy) {
			continue
		}
		if account := strings.TrimSpace(o.Account); account != "" && !strings.EqualFold(account, strings.TrimSpace(scope.Account)) {
			continue
		}
		ccy := normCcy(o.Currency)
		secType := strings.ToUpper(strings.TrimSpace(o.SecType))
		fee, feeKnown := cashSweepWorkingFee(o, scope, evidence)
		if !feeKnown {
			cashSweepOutstandingFeesUnknown(&out, ccy, "a working")
		}
		remaining := o.Remaining
		if remaining <= 0 {
			remaining = o.TotalQuantity - o.Filled
		}
		price := o.LimitPrice
		// A stop or trailing trigger is not an execution-price cap. Even a
		// trailing limit can ratchet its limit above the current snapshot, so
		// only a fixed limit bounds the cash this working buy can consume.
		orderType := strings.ToUpper(strings.TrimSpace(o.OrderType))
		bounded := orderType == "LMT" || orderType == "STP LMT"
		multiplier, multiplierOK := cashSweepMultiplier(secType, o.Multiplier, ccy)
		switch {
		case ccy == "" || secType == "CASH":
			out.Unknown[""] = "a working buy order carries no single currency (or converts one), so committed cash is unknown"
		case cashSweepBondSecType(secType) && !multiplierOK:
			out.Unknown[ccy] = fmt.Sprintf("a working bond buy in %s has no bill convention to value it, so committed cash is unknown", ccy)
		case !bounded || !positiveFinite(price) || !positiveFinite(remaining) || !multiplierOK:
			out.Unknown[ccy] = fmt.Sprintf("a working buy order in %s has no price bound, so the cash it commits is unknown", ccy)
		default:
			amount := remaining * price * multiplier
			if !positiveFinite(amount) {
				out.Unknown[ccy] = fmt.Sprintf("a working buy order in %s has no finite cash commitment", ccy)
				continue
			}
			total := out.ByCurrency[ccy] + amount + fee
			if !positiveFinite(total) {
				out.Unknown[ccy] = fmt.Sprintf("working buy commitments in %s have no finite total", ccy)
				continue
			}
			out.ByCurrency[ccy] = total
		}
	}
	for _, rec := range queued {
		if !rec.liveIntent() || !sameBrokerScope(rec.scope(), scope) || !strings.EqualFold(rec.Terms.Action, rpc.OrderActionBuy) {
			continue
		}
		ccy := normCcy(nonEmptyString(rec.Terms.Currency, rec.Terms.Contract.Currency))
		cashSweepOutstandingFeesUnknown(&out, ccy, "an armed queued")
		multiplier, ok := cashSweepMultiplier(strings.ToUpper(strings.TrimSpace(rec.Terms.Contract.SecType)), rec.Terms.Contract.Multiplier, ccy)
		if ccy == "" || !ok || !positiveFinite(rec.Terms.WorstPrice) || rec.Terms.MaxQuantity <= 0 {
			out.Unknown[ccy] = "an armed queued buy has no currency, multiplier or worst price, so the cash it commits is unknown"
			continue
		}
		amount := float64(rec.Terms.MaxQuantity) * rec.Terms.WorstPrice * multiplier
		if !positiveFinite(amount) {
			out.Unknown[ccy] = "an armed queued buy has no finite cash commitment"
			continue
		}
		total := out.ByCurrency[ccy] + amount
		if !positiveFinite(total) {
			out.Unknown[ccy] = "working and armed queued buy commitments have no finite total"
			continue
		}
		out.ByCurrency[ccy] = total
	}
	return out
}
