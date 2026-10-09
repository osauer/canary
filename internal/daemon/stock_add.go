package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"slices"
	"strings"

	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

type stockAddEvidence struct {
	input               risk.StockAddInput
	contract            rpc.ContractParams
	review              rpc.AddReview
	broker              *orderPreviewBrokerAuthority
	ordersGeneration    uint64
	portfolioGeneration uint64
	accountFingerprint  string
}

// stockAddProblem preserves the reason category across daemon evidence collection.
type stockAddProblem struct{ kind, message string }

func (p *stockAddProblem) Error() string { return p.message }

func (s *Server) addHeld(p rpc.AddParams, code, message string) *rpc.AddPlanResult {
	return &rpc.AddPlanResult{AsOf: s.orderNow(), Contract: p.Contract, LimitPrice: p.LimitPrice, Currency: p.Contract.Currency, Blockers: []risk.StockAddBlocker{{Kind: "evidence", Code: code, Message: message}}}
}

func (s *Server) handleAddPlan(ctx context.Context, req *rpc.Request) (*rpc.AddPlanResult, error) {
	var p rpc.AddParams
	d := json.NewDecoder(bytes.NewReader(req.Params))
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil {
		return nil, errBadRequest(err.Error())
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, errBadRequest("one Add parameter object required")
	}
	var err error
	p, err = rpc.NormalizeAddParams(p)
	if err != nil {
		return nil, errBadRequest(err.Error())
	}
	return s.planStockAdd(ctx, p)
}

func (s *Server) handleAddPreview(ctx context.Context, req *rpc.Request) (*rpc.OrderPreviewResult, error) {
	plan, err := s.handleAddPlan(ctx, req)
	if err != nil {
		return nil, err
	}
	if len(plan.Blockers) > 0 || plan.Quantity <= 0 {
		return nil, errBadRequest("Add held: " + addBlockerText(plan.StockAddPlan))
	}
	return s.previewOrder(ctx, rpc.OrderPreviewParams{Action: rpc.OrderActionBuy, Contract: plan.Contract, Quantity: plan.Quantity, OrderType: "LMT", LimitPrice: new(plan.LimitPrice), Strategy: "explicit-limit", TIF: "DAY", Add: true})
}

func addBlockerText(p risk.StockAddPlan) string {
	if len(p.Blockers) > 0 {
		return p.Blockers[0].Message
	}
	return "no permitted quantity"
}

// stockAddQuoteBudget bounds broker work, not investment size or owner policy.
// A maximum is claimed only when every greater candidate up to the proven
// arithmetic upper bound has been ruled out. No fee monotonicity is assumed.
const stockAddQuoteBudget = 24

func (s *Server) planStockAdd(ctx context.Context, p rpc.AddParams) (*rpc.AddPlanResult, error) {
	ev, err := s.stockAddEvidence(ctx, p)
	if err != nil {
		out := s.addHeld(p, "add_evidence_unavailable", err.Error())
		if problem, ok := errors.AsType[*stockAddProblem](err); ok {
			out.Blockers[0].Kind = problem.kind
		}
		return out, nil
	}
	capacityInput := ev.input
	capacityInput.Requested, capacityInput.Fee, capacityInput.BrokerMargin = 0, 0, nil
	capacity := risk.SizeStockAdd(capacityInput)
	upper := capacity.MaxQuantity
	capacity.OrderUpperBound, capacity.MaxQuantity = upper, 0
	capacity.Quantity, capacity.After, capacity.Cost, capacity.CashAfter = 0, capacity.Before, 0, ev.input.FreeCash
	capacity.StockPctAfter, capacity.UnderlyingPctAfter = capacity.StockPctBefore, capacity.UnderlyingPctBefore
	if capacity.Protection != nil {
		capacity.Protection.UncoveredAfter = capacity.Protection.UncoveredBefore
	}
	capacity.Sizing = "quantity"
	if p.Max {
		capacity.Sizing = "max"
	}
	capacity.RequestedQuantity = p.Quantity
	out := &rpc.AddPlanResult{StockAddPlan: capacity, Contract: ev.contract, LimitPrice: p.LimitPrice, Currency: ev.contract.Currency, BaseCurrency: ev.input.Rules.BaseCurrency, AsOf: s.orderNow()}
	held := func(kind, code, message string) (*rpc.AddPlanResult, error) {
		out.Quantity = 0
		out.Review = nil
		out.Blockers = append(out.Blockers, risk.StockAddBlocker{Kind: kind, Code: code, Message: message})
		return out, nil
	}
	if len(capacity.Blockers) > 0 {
		return out, nil
	}
	if p.Quantity > upper {
		return held("capacity", "add_quantity_above_max", "The requested addition exceeds the current order allowance before broker costs.")
	}
	type candidate struct {
		plan risk.StockAddPlan
		fee  float64
	}
	checked := map[int]candidate{}
	quote := func(q int) (candidate, error) {
		if !s.stockAddEvidenceCurrent(ev) {
			return candidate{}, fmt.Errorf("account, orders or policy changed while sizing; calculate again")
		}
		draft := rpc.OrderDraft{AdaptivePriority: p.AdaptivePriority, Action: rpc.OrderActionBuy, Contract: ev.contract, Quantity: q, OrderType: "LMT", LimitPrice: p.LimitPrice, TIF: "DAY", Strategy: "explicit-limit"}
		w, err := s.fetchPreviewWhatIfBound(ctx, s.currentTradingStatus(), draft, orderPreviewDefaultWait, ev.broker)
		if err != nil {
			return candidate{}, err
		}
		fee, ok := cashSweepFeeUpper(&rpc.OrderPreviewResult{WhatIf: w}, ev.contract.Currency)
		if !ok {
			return candidate{}, fmt.Errorf("the exact candidate needs an accepted broker commission bound in its cash currency")
		}
		input := ev.input
		input.Requested, input.Fee = q, fee
		input.BrokerMargin, err = stockAddWhatIfMargin(input, w, q)
		if err != nil {
			return candidate{}, err
		}
		c := candidate{plan: risk.CheckStockAdd(input), fee: fee}
		checked[q] = c
		return c, nil
	}
	finish := func(c candidate, maximum bool) (*rpc.AddPlanResult, error) {
		if !s.stockAddEvidenceCurrent(ev) {
			return held("evidence", "add_evidence_changed", "Account, orders or policy changed while sizing; calculate again.")
		}
		out.StockAddPlan = c.plan
		out.Sizing = capacity.Sizing
		out.RequestedQuantity = p.Quantity
		out.MaxCommission = new(c.fee)
		if maximum {
			out.MaxQuantity, out.MaximumKnown = c.plan.Quantity, true
		}
		if len(c.plan.Blockers) == 0 && c.plan.Quantity > 0 {
			ev.review.Plan = out.StockAddPlan
			out.Review = &ev.review
		}
		return out, nil
	}
	if !p.Max {
		c, err := quote(p.Quantity)
		if err != nil {
			return held("evidence", "add_broker_evidence_unavailable", err.Error())
		}
		return finish(c, false)
	}
	q, best := upper, 0
	for range stockAddQuoteBudget {
		if err := ctx.Err(); err != nil {
			return held("evidence", "add_search_incomplete", "The planning time budget ended before a maximum was established.")
		}
		c, err := quote(q)
		if err != nil {
			return held("evidence", "add_broker_evidence_unavailable", err.Error())
		}
		if c.plan.Quantity > best && len(c.plan.Blockers) == 0 {
			best = q
		}
		out.SupportedQuantity = best
		// Certification is exhaustive above the best accepted quantity. Even a
		// discontinuous fee schedule cannot make an untested larger lot disappear.
		next := upper
		for next > best {
			if _, seen := checked[next]; !seen {
				break
			}
			next--
		}
		if next == best {
			if best == 0 {
				return held("capacity", "add_no_capacity", "Every quantity within the order allowance failed its exact cash or margin check.")
			}
			return finish(checked[best], true)
		}
		if best == 0 {
			// Find one affordable witness early, but never use its fee to certify
			// a different quantity. The untested upper interval is still checked.
			affordable := max(0, int(math.Floor((ev.input.FreeCash-c.fee)/ev.input.Price)))
			next = min(next, affordable)
			if next >= q {
				next = q / 2
			}
			if next == 0 {
				next = 1
			}
			if _, seen := checked[next]; seen {
				next = upper
				for next > 0 {
					if _, seen := checked[next]; !seen {
						break
					}
					next--
				}
			}
		}
		q = next
	}
	return held("evidence", "add_search_incomplete", fmt.Sprintf("Maximum not established within the bounded broker search. %d additional shares passed an exact check; use an explicit quantity for a fresh review.", best))
}

// stockAddWhatIfMargin applies only this draft's simulated margin change. The
// broker must identify the account-base denomination of margin amounts;
// commission has its own native currency. Missing currency supplies no credit.
func stockAddWhatIfMargin(in risk.StockAddInput, w rpc.OrderWhatIfResult, quantity int) (*risk.StockAddMargin, error) {
	m := w.Margin
	if w.Status != rpc.OrderWhatIfStatusAccepted || m == nil || (m.Currency == "" || m.Currency != in.Rules.BaseCurrency) {
		return nil, fmt.Errorf("the exact order needs accepted broker margin evidence in account base currency")
	}
	for _, v := range []*float64{m.InitialMarginBefore, m.InitialMarginAfter, m.MaintenanceMarginBefore, m.MaintenanceMarginAfter, m.EquityWithLoanBefore, m.EquityWithLoanAfter, in.Rules.ExcessLiquidityBase} {
		if v == nil || !finiteProtectionOptionPolicyValue(*v) {
			return nil, fmt.Errorf("the exact order needs complete before and after broker margin evidence")
		}
	}
	if *m.InitialMarginBefore < 0 || *m.InitialMarginAfter < 0 || *m.MaintenanceMarginBefore < 0 || *m.MaintenanceMarginAfter < 0 {
		return nil, fmt.Errorf("broker margin evidence is invalid")
	}
	before := *m.EquityWithLoanBefore - *m.MaintenanceMarginBefore
	after := *m.EquityWithLoanAfter - *m.MaintenanceMarginAfter
	// Pending purchases are already charged against observed headroom. Keep
	// their full debit; never give a margin-release credit to this new purchase.
	change := min(0, after-before) - in.Fee*in.FX
	out := &risk.StockAddMargin{Quantity: quantity, ExcessLiquidityBase: min(*in.Rules.ExcessLiquidityBase+in.PendingCostBase, before) - in.PendingCostBase + change, InitialMarginBase: *m.InitialMarginAfter, MaintenanceMarginBase: *m.MaintenanceMarginAfter}
	if in.Rules.LookAheadExcessLiquidityBase != nil {
		out.LookAheadExcessBase = new(min(*in.Rules.LookAheadExcessLiquidityBase+in.PendingCostBase, before) - in.PendingCostBase + change)
	}
	return out, nil
}

// stockAddCashFunding leaves the account reserve funded once across measured
// cash currencies, after every currency's float and fee-inclusive commitments.
// Only the selected currency can pay for the stock; no FX or borrowing is created.
func stockAddCashFunding(bucket *protectionCashSweepPolicy, ledger map[string]cashSweepLedgerRow, commitments cashSweepCommitments, currency string, reserve, rate float64) (*risk.StockAddCash, error) {
	out := &risk.StockAddCash{AccountReserveBase: reserve}
	for _, ccy := range slices.Sorted(maps.Keys(ledger)) {
		row := ledger[ccy]
		keep, known := bucket.keepCash(ccy)
		if !known || !row.Observed || row.Settled == nil || !finiteProtectionOptionPolicyValue(*row.Settled) || !finiteProtectionOptionPolicyValue(row.TradeDate) || !positiveFinite(row.ExchangeRate) {
			return nil, fmt.Errorf("settled cash, currency float and conversion are required for account reserve funding in %s", ccy)
		}
		available := min(row.TradeDate, *row.Settled)
		committed := commitments.ByCurrency[ccy]
		free := available - committed - keep
		if ccy == currency {
			out.Available, out.Committed, out.CurrencyFloat = available, committed, keep
		} else {
			out.OtherReserveFundingBase += free * row.ExchangeRate
		}
	}
	if _, ok := ledger[currency]; !ok || !positiveFinite(rate) {
		return nil, fmt.Errorf("selected currency cash is unavailable")
	}
	out.ReserveInCurrency = max(0, reserve-out.OtherReserveFundingBase) / rate
	out.Spendable = out.Available - out.Committed - out.CurrencyFloat - out.ReserveInCurrency
	return out, nil
}

func (s *Server) stockAddEvidence(ctx context.Context, p rpc.AddParams) (stockAddEvidence, error) {
	if s.stockAddEvidenceForTest != nil {
		return s.stockAddEvidenceForTest(ctx, p)
	}
	ev := stockAddEvidence{}
	mgr := s.riskPolicies.snapshot()
	if mgr.policy == nil {
		return ev, &stockAddProblem{kind: "policy", message: "the existing risk policy is unavailable"}
	}
	if mgr.status != rpc.RiskPolicyStatusActive || mgr.review == rpc.PolicyReviewUnreviewed {
		return ev, &stockAddProblem{kind: "policy", message: "the risk policy must be current and owner-reviewed"}
	}
	pol, polStatus := s.activeRulebookPolicy()
	if polStatus.Review == rpc.PolicyReviewUnreviewed || polStatus.Status != "active" {
		return ev, &stockAddProblem{kind: "policy", message: "the Rulebook policy must be current and owner-reviewed"}
	}
	cashPolicy, cashStatus := s.protectionPolicies.Active()
	if cashStatus.Status != "active" || cashStatus.Review == rpc.PolicyReviewUnreviewed {
		return ev, &stockAddProblem{kind: "policy", message: "the cash reserve policy must be current and owner-reviewed"}
	}
	broker, err := s.captureOrderPreviewBrokerAuthority()
	if err != nil {
		return ev, err
	}
	if broker == nil {
		return ev, fmt.Errorf("current broker session unavailable")
	}
	ev.broker = broker
	contract, err := s.resolvePreviewOrderContract(ctx, broker, p.Contract, previewMinTickTimeout)
	if err != nil {
		return ev, err
	}
	if contract.SecType != "STK" || contract.ConID <= 0 || contract.Symbol != p.Contract.Symbol || contract.Currency != p.Contract.Currency || (p.Contract.ConID > 0 && contract.ConID != p.Contract.ConID) {
		return ev, fmt.Errorf("resolved stock identity differs from the selected instrument")
	}
	ev.contract = contract
	ev.accountFingerprint = stockAddAccountFingerprint(broker)
	if ev.accountFingerprint == "" {
		return ev, fmt.Errorf("current account cash and margin evidence unavailable")
	}
	acct, aa, err := s.buildAccountSummaryWithAuthority(ctx, false)
	if err != nil {
		return ev, err
	}
	var health ibkrlib.PortfolioStreamHealth
	scope := s.currentBrokerStateScope()
	pos, err := s.handlePositionsListCapturedForScope(ctx, &rpc.Request{}, &health, scope)
	if err != nil {
		return ev, err
	}
	now := s.orderNow()
	if acct == nil || pos == nil || !currentPortfolioAuthority(acct.Authority) || !currentPortfolioAuthority(pos.Authority) || !pos.Authority.PortfolioComplete || acct.Authority.Scope != pos.Authority.Scope || acct.Authority.BrokerReadSession != pos.Authority.BrokerReadSession || proposalPositionsUnprimed(pos, acct, health) {
		return ev, fmt.Errorf("a complete same-session account and position read is required")
	}
	base, ledger, why := cashSweepLedgerAt(acct, now)
	if why != "" {
		return ev, fmt.Errorf("%s", why)
	}
	row, ok := ledger[contract.Currency]
	if !ok || !row.Observed || row.Settled == nil || !finiteProtectionOptionPolicyValue(*row.Settled) {
		return ev, fmt.Errorf("native settled cash is unavailable for %s", contract.Currency)
	}
	bucket := cashPolicy.Cash.Sweep
	if bucket == nil || bucket.ReserveFloorBase == nil || bucket.ReservePctNLV == nil || bucket.NoBuyWhileBorrowed == nil {
		return ev, &stockAddProblem{kind: "policy", message: "account cash reserve is unapproved"}
	}
	if !aa.NetLiquidationAvailable || !aa.BaseCurrencyAvailable || !aa.ExcessLiquidityAvailable || acct.NetLiquidation <= 0 {
		return ev, fmt.Errorf("net liquidation, base currency and margin headroom must be measured")
	}
	borrowing := cashSweepBorrowingFor(bucket, cashSweepInput{Ledger: ledger}, slices.Sorted(maps.Keys(ledger)))
	if borrowing.HoldsBuys {
		return ev, fmt.Errorf("the approved cash policy holds new purchases while any currency is borrowed or unknown")
	}
	n, err := s.captureOrderNotionalAuthority(ctx, broker, p.LimitPrice, contract.Currency, base, orderPreviewDefaultWait)
	if err != nil {
		return ev, err
	}
	reserve := max(*bucket.ReserveFloorBase, acct.NetLiquidation*(*bucket.ReservePctNLV)/100)
	inventory, orderScope, err := s.brokerOpenOrderInventory(ctx, true)
	if err != nil {
		return ev, err
	}
	if !sameBrokerScope(scope, orderScope) {
		return ev, fmt.Errorf("orders belong to another account session")
	}
	events, err := s.orderJournal.LoadEvents(0)
	if err != nil {
		return ev, err
	}
	var queued []queuedAuthRecord
	if s.tradeProposals != nil {
		queued = s.tradeProposals.queued.list()
	}
	for _, q := range queued {
		if q.liveIntent() && sameBrokerScope(q.scope(), scope) {
			return ev, fmt.Errorf("an armed queued instruction must resolve before an equity Add can be sized")
		}
	}
	commitments := cashSweepCommitmentsFrom(inventory.Orders, queued, scope, cashSweepFeeEvidence{Events: events, Now: now, Endpoint: s.currentTradingStatus().Endpoint})
	if !commitments.Known || len(commitments.Unknown) > 0 {
		return ev, fmt.Errorf("all purchase commitments and their fee bounds must be known across currencies")
	}
	committedBase := 0.
	for ccy, amount := range commitments.ByCurrency {
		rate := ledger[ccy].ExchangeRate
		if !positiveFinite(rate) {
			return ev, fmt.Errorf("a committed currency has no measured conversion")
		}
		committedBase += amount * rate
	}
	cash, err := stockAddCashFunding(bucket, ledger, commitments, contract.Currency, reserve, n.BasePerContract)
	if err != nil {
		return ev, err
	}
	input := risk.StockAddInput{Policy: mgr.policy.PositionAdd, Manual: p.Quantity > 0, Symbol: contract.Symbol, ConID: contract.ConID, Price: p.LimitPrice, FX: n.BasePerContract, Requested: p.Quantity, FreeCash: cash.Spendable, Cash: cash, PendingCostBase: committedBase, Rulebook: pol,
		Rules: risk.RuleInputs{AsOf: now, BaseCurrency: base, Positions: risk.SourceState{Healthy: true}, Account: risk.SourceState{Healthy: true}, NLVBase: new(acct.NetLiquidation), RiskCapital: s.rulebookRiskCapital(acct, nil, base, now), Names: mapRuleNames(pos, pol, base)}}
	input.Rules.NonBaseNLVBase, input.Rules.NonBaseCurrencies = nonBaseExposure(acct, pos)
	input.Rules.ExcessLiquidityBase, input.Rules.LookAheadExcessLiquidityBase, input.Rules.InitialMarginBase, input.Rules.MaintenanceMarginBase = rulebookMarginInputs(acct, aa)
	if input.Rules.ExcessLiquidityBase != nil {
		input.Rules.ExcessLiquidityBase = new(*input.Rules.ExcessLiquidityBase - committedBase)
	}
	if input.Rules.LookAheadExcessLiquidityBase != nil {
		input.Rules.LookAheadExcessLiquidityBase = new(*input.Rules.LookAheadExcessLiquidityBase - committedBase)
	}
	if st, carried := s.rulebookRegimeStage(pol, now); st.Bucket != "" && !carried {
		input.Rules.RegimeStage = st.Bucket
		input.Rules.RegimeStageAsOf = st.AsOf
		input.Rules.RegimeStageCarried = carried
	} else {
		return ev, fmt.Errorf("current regime evidence is unavailable")
	}
	capital := s.riskCapital.Report(mgr.policy, &risk.CapitalObservation{EquityBase: acct.NetLiquidation, AsOf: acct.AsOf}, scope)
	if (!input.Manual && capital.Tier != risk.CapitalTierOK) || capital.EquityStale || capital.ReconcileStale {
		return ev, fmt.Errorf("capital policy is not ready for new risk: %s", capital.Tier)
	}
	limits := s.orderLimitsInForceForPreview(ctx, base)
	if !limits.Complete {
		return ev, fmt.Errorf("per-order limits are incomplete")
	}
	input.OrderCapBase = limits.CapBase
	if err := stockAddHeldStocks(&input, pos, contract, base); err != nil {
		return ev, err
	}
	if err := stockAddPending(&input, inventory.Orders, ledger, contract, scope); err != nil {
		return ev, err
	}
	// Use the same earnings-aware option economics and liquidity evidence as
	// the Rulebook. No provider refresh or model recommendation is triggered.
	if !slices.ContainsFunc(input.Rules.Names, func(n risk.NameInput) bool { return n.Symbol == contract.Symbol }) {
		input.Rules.Names = append(input.Rules.Names, risk.NameInput{Symbol: contract.Symbol, StockConID: contract.ConID, StockSecType: "STK", UnderlyingSecType: "STK", ExposureBaseComplete: true})
	}
	s.attachRulebookLiquidity(ctx, input.Rules.Names, pos, now, false)
	input.Rules.Earnings, _ = s.assembleEarnings(ctx, input.Rules.Names, pol, marketcal.New(), now, false)
	if e := input.Rules.Earnings[contract.Symbol]; e.TerminalNonReporting {
		return ev, fmt.Errorf("this instrument is terminal and cannot receive new investment")
	}
	input.Rules.Names = rulebookEconomicNames(input.Rules.Names, input.Rules.Earnings)
	ev.input = input
	ev.ordersGeneration = inventory.Generation
	ev.portfolioGeneration = health.ProjectionGeneration
	ev.review = rpc.AddReview{RegimeStage: input.Rules.RegimeStage, RegimeAsOf: input.Rules.RegimeStageAsOf, PolicyFingerprint: mgr.policy.FingerprintKey(), RulebookFingerprint: pol.FingerprintKey(), CashPolicyFingerprint: fingerprintProtectionPolicy(cashPolicy).Key, AsOf: now}
	if !s.stockAddEvidenceCurrent(ev) {
		return ev, fmt.Errorf("account, orders or policy changed while collecting Add evidence")
	}
	return ev, nil
}

func stockAddPending(in *risk.StockAddInput, orders []ibkrlib.OrderLifecycleEvent, ledger map[string]cashSweepLedgerRow, contract rpc.ContractParams, scope brokerStateScope) error {
	// Stops must be covered by filled shares, never by another pending buy.
	held := map[int]float64{}
	reservedSells := map[int]float64{}
	for _, n := range in.Rules.Names {
		if n.StockConID > 0 {
			held[n.StockConID] += n.StockQuantity
		}
	}
	for _, o := range orders {
		if !brokerOrderWorking(o) {
			continue
		}
		if o.Account == "" || !strings.EqualFold(o.Account, scope.Account) {
			return fmt.Errorf("order inventory includes another account")
		}
		if o.SecType != "STK" {
			return &stockAddProblem{kind: "unsupported", message: "pending non-stock risk cannot yet be included in equity Add"}
		}
		if o.Action == "SELL" {
			protective := slices.Contains([]string{"STP", "STP LMT", "TRAIL", "TRAIL LIMIT"}, o.OrderType)
			reservedSells[o.ConID] += brokerOrderRemaining(o)
			covered := brokerOrderRemaining(o) > 0 && positiveFinite(reservedSells[o.ConID]) && held[o.ConID] >= reservedSells[o.ConID]
			if !protective || !covered {
				return fmt.Errorf("a pending sale must resolve before adding stock")
			}
			if o.ConID == contract.ConID {
				in.StopQuantity += brokerOrderRemaining(o)
			}
			continue
		}
		if o.Action != "BUY" || o.SecType != "STK" || o.ConID <= 0 {
			return &stockAddProblem{kind: "unsupported", message: "pending non-stock risk cannot yet be included in equity Add"}
		}
		remaining := o.Remaining
		if remaining <= 0 {
			remaining = o.TotalQuantity - o.Filled
		}
		rate := ledger[o.Currency].ExchangeRate
		if !positiveFinite(remaining) || !positiveFinite(o.LimitPrice) || !positiveFinite(rate) || o.OrderType != "LMT" {
			return fmt.Errorf("pending purchase has no bounded stock exposure")
		}
		value := remaining * o.LimitPrice * rate
		in.StockValueBase += value
		if o.Symbol == contract.Symbol {
			if o.ConID != contract.ConID {
				return fmt.Errorf("pending purchase has a conflicting stock identity")
			}
			in.UnderlyingStockBase += value
			in.PendingQuantity += remaining
		}
		found := false
		for i := range in.Rules.Names {
			n := &in.Rules.Names[i]
			if n.Symbol != o.Symbol {
				continue
			}
			if n.HasStockLeg && n.StockConID != o.ConID {
				return fmt.Errorf("pending purchase has a conflicting held identity")
			}
			n.StockQuantity += remaining
			n.StockMark = max(n.StockMark, o.LimitPrice)
			n.StockFXToBase = new(rate)
			n.HasStockLeg = true
			n.StockConID = o.ConID
			n.StockSecType = "STK"
			n.UnderlyingSecType = "STK"
			n.ExposureBase += value
			n.MarketValueBase += value
			found = true
			break
		}
		if !found {
			in.Rules.Names = append(in.Rules.Names, risk.NameInput{Symbol: o.Symbol, StockConID: o.ConID, StockSecType: "STK", UnderlyingSecType: "STK", HasStockLeg: true, StockQuantity: remaining, StockMark: o.LimitPrice, StockFXToBase: new(rate), ExposureBase: value, ExposureBaseComplete: true, MarketValueBase: value})
		}
	}
	return nil
}

func (s *Server) stockAddEvidenceCurrent(ev stockAddEvidence) bool {
	if s.stockAddEvidenceForTest != nil {
		return true
	}
	if s.stockAddQueuedInstruction(s.currentBrokerStateScope()) {
		return false
	}
	if !s.orderPreviewBrokerAuthorityCurrent(ev.broker) || ev.broker.connector.PortfolioProjectionGeneration() != ev.portfolioGeneration || stockAddAccountFingerprint(ev.broker) != ev.accountFingerprint {
		return false
	}
	b := s.currentProtectionOrderSnapshotBinding()
	if b.generation != ev.ordersGeneration {
		return false
	}
	return s.stockAddPolicyCurrent(ev.review)
}

func (s *Server) stockAddPolicyCurrent(review rpc.AddReview) bool {
	mgr := s.riskPolicies.snapshot()
	pol, status := s.activeRulebookPolicy()
	cash, cs := s.protectionPolicies.Active()
	if review.RegimeStage != "" {
		stage, carried := s.rulebookRegimeStage(pol, s.orderNow())
		if carried || stage.Bucket != review.RegimeStage || !stage.AsOf.Equal(review.RegimeAsOf) {
			return false
		}
	}
	return mgr.policy != nil && mgr.status == rpc.RiskPolicyStatusActive && mgr.review != rpc.PolicyReviewUnreviewed && status.Status == "active" && status.Review != rpc.PolicyReviewUnreviewed && cs.Status == "active" && cs.Review != rpc.PolicyReviewUnreviewed && mgr.policy.FingerprintKey() == review.PolicyFingerprint && pol.FingerprintKey() == review.RulebookFingerprint && fingerprintProtectionPolicy(cash).Key == review.CashPolicyFingerprint
}

func (s *Server) stockAddRequired(draft rpc.OrderDraft, position rpc.OrderPositionImpact) bool {
	mgr := s.riskPolicies.snapshot()
	return draft.Add != nil || (mgr.policy != nil && mgr.policy.PositionAdd != nil && mgr.policy.PositionAdd.AdmissionContract == risk.StockAddAdmissionV1 && draft.Contract.SecType == "STK" && draft.Action == rpc.OrderActionBuy && position.After > max(0, position.Before))
}

func (s *Server) validateStockAddDraft(ctx context.Context, draft rpc.OrderDraft, w rpc.OrderWhatIfResult) (stockAddEvidence, error) {
	ev := stockAddEvidence{}
	if draft.Contract.SecType != "STK" || draft.Action != rpc.OrderActionBuy || draft.OrderType != "LMT" || draft.TIF != "DAY" || draft.OutsideRTH || draft.StrategyGroup != nil || draft.Trail != nil {
		return ev, fmt.Errorf("equity Add supports single-stock regular-session DAY limit buys only")
	}
	var err error
	ev, err = s.stockAddEvidence(ctx, rpc.AddParams{Contract: draft.Contract, LimitPrice: draft.LimitPrice, Quantity: draft.Quantity})
	if err != nil {
		return ev, err
	}
	fee, ok := cashSweepFeeUpper(&rpc.OrderPreviewResult{WhatIf: w}, draft.Contract.Currency)
	if !ok {
		return ev, fmt.Errorf("equity Add requires the exact draft's broker maximum commission")
	}
	ev.input.Fee = fee
	ev.input.Requested = draft.Quantity
	ev.input.BrokerMargin, err = stockAddWhatIfMargin(ev.input, w, draft.Quantity)
	if err != nil {
		return ev, err
	}
	plan := risk.CheckStockAdd(ev.input)
	if draft.Add != nil {
		// The owner confirmed these exact warning facts. Changed warnings need
		// another preview; a previous acknowledgement cannot cover new risk.
		if !slices.Equal(draft.Add.Plan.Warnings, plan.Warnings) {
			return ev, fmt.Errorf("add warnings changed; review and confirm again")
		}
	}
	if len(plan.Blockers) > 0 || plan.Quantity != draft.Quantity {
		return ev, fmt.Errorf("%s", addBlockerText(plan))
	}
	ev.review.Plan = plan
	if draft.Add != nil && (draft.Add.PolicyFingerprint != ev.review.PolicyFingerprint || draft.Add.RulebookFingerprint != ev.review.RulebookFingerprint || draft.Add.CashPolicyFingerprint != ev.review.CashPolicyFingerprint || draft.Add.Plan.Before != plan.Before) {
		return ev, fmt.Errorf("position or policy changed since the Add review; preview again")
	}
	if !s.stockAddEvidenceCurrent(ev) {
		return ev, fmt.Errorf("stock addition evidence changed during review")
	}
	return ev, nil
}

// A value fingerprint also catches cash/margin-only updates that do not change
// structural positions. The raw account data never leaves the daemon.
func stockAddAccountFingerprint(broker *orderPreviewBrokerAuthority) string {
	if broker == nil || broker.connector == nil {
		return ""
	}
	raw := broker.connector.AccountSummaryRaw()
	if len(raw) == 0 {
		return ""
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

func (s *Server) stockAddQueuedInstruction(scope brokerStateScope) bool {
	if s.tradeProposals == nil {
		return false
	}
	for _, q := range s.tradeProposals.queued.list() {
		if q.liveIntent() && sameBrokerScope(q.scope(), scope) {
			return true
		}
	}
	return false
}

// stockAddHeldStocks consumes canonical position rows; broker request codes are a separate contract.
func stockAddHeldStocks(input *risk.StockAddInput, pos *rpc.PositionsResult, contract rpc.ContractParams, base string) error {
	for _, stock := range pos.Stocks {
		if stock.Quantity == 0 {
			continue
		}
		if !rpc.PositionQuotesAsStock(stock) {
			// Government debt has a separately evidenced issuer; corporate debt or
			// an unresolved instrument cannot silently disappear from issuer risk.
			government := stock.SecType == "BOND" && slices.ContainsFunc(pos.Bonds, func(b rpc.PositionBond) bool {
				return b.ConID == stock.ConID && b.Quantity == stock.Quantity && b.IssuerClass == rpc.BondIssuerGovernment && b.EvidenceIssuer != "" && b.EvidenceSource != ""
			})
			if government {
				continue
			}
			return &stockAddProblem{kind: "unsupported", message: fmt.Sprintf("cross-asset issuer concentration for held %s is not yet supported by equity Add", stock.SecType)}
		}
		rate, ok := positionBaseRate(stock, base)
		if !ok || stock.Stale || !positiveFinite(stock.Mark) {
			return fmt.Errorf("a held stock has no current valuation or currency conversion")
		}
		value := math.Abs(stock.Quantity) * stock.Mark * rate
		input.StockValueBase += value
		if stock.Symbol == contract.Symbol {
			input.UnderlyingStockBase += value
			if stock.ConID != contract.ConID {
				return fmt.Errorf("another stock identity shares this symbol; exact underlying aggregation is required")
			}
			input.CurrentQuantity += stock.Quantity
		}
	}
	return nil
}
