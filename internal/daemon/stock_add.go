package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
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

func (s *Server) addHeld(p rpc.AddParams, code, message string) *rpc.AddPlanResult {
	return &rpc.AddPlanResult{AsOf: s.orderNow(), Contract: p.Contract, LimitPrice: p.LimitPrice, Currency: p.Contract.Currency, Blockers: []risk.StockAddBlocker{{Code: code, Message: message}}}
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

func (s *Server) planStockAdd(ctx context.Context, p rpc.AddParams) (*rpc.AddPlanResult, error) {
	ev, err := s.stockAddEvidence(ctx, p)
	if err != nil {
		out := s.addHeld(p, "add_evidence_unavailable", err.Error())
		out.AsOf = s.orderNow()
		return out, nil
	}
	// First size without fees to select a candidate, then use only that exact
	// quantity's accepted broker fee bound. At most three adjustments; inability
	// to obtain a stable exact quote holds instead of estimating a commission.
	plan := risk.SizeStockAdd(ev.input)
	out := &rpc.AddPlanResult{StockAddPlan: plan, Contract: ev.contract, LimitPrice: p.LimitPrice, Currency: ev.contract.Currency, BaseCurrency: ev.input.Rules.BaseCurrency, AsOf: s.orderNow()}
	for range 3 {
		if plan.Quantity <= 0 || len(plan.Blockers) > 0 {
			out.StockAddPlan = plan
			return out, nil
		}
		draft := rpc.OrderDraft{Action: rpc.OrderActionBuy, Contract: ev.contract, Quantity: plan.Quantity, OrderType: "LMT", LimitPrice: p.LimitPrice, TIF: "DAY", Strategy: "explicit-limit"}
		w, err := s.fetchPreviewWhatIfBound(ctx, s.currentTradingStatus(), draft, orderPreviewDefaultWait, ev.broker)
		if err != nil {
			return s.addHeld(p, "add_fee_unavailable", err.Error()), nil
		}
		fee, ok := cashSweepFeeUpper(&rpc.OrderPreviewResult{WhatIf: w}, ev.contract.Currency)
		if !ok {
			return s.addHeld(p, "add_fee_unavailable", "The exact candidate needs an accepted broker maximum commission in its cash currency."), nil
		}
		ev.input.Fee = fee
		next := risk.SizeStockAdd(ev.input)
		if next.Quantity != plan.Quantity {
			plan = next
			continue
		}
		if !s.stockAddEvidenceCurrent(ev) {
			return s.addHeld(p, "add_evidence_changed", "Account, orders or policy changed while sizing; calculate again."), nil
		}
		out.StockAddPlan = next
		out.MaxCommission = new(fee)
		ev.review.Plan = next
		out.Review = &ev.review
		return out, nil
	}
	return s.addHeld(p, "add_fee_unavailable", "Broker commission did not stabilise for an exact affordable quantity; calculate again."), nil
}

func (s *Server) stockAddEvidence(ctx context.Context, p rpc.AddParams) (stockAddEvidence, error) {
	if s.stockAddEvidenceForTest != nil {
		return s.stockAddEvidenceForTest(ctx, p)
	}
	ev := stockAddEvidence{}
	mgr := s.riskPolicies.snapshot()
	if mgr.policy == nil || mgr.policy.PositionAdd == nil || mgr.policy.PositionAdd.MaxStockPctNLV == nil || mgr.policy.PositionAdd.MaxUnderlyingStockPctNLV == nil {
		return ev, fmt.Errorf("position_add.max_stock_pct_nlv and position_add.max_underlying_stock_pct_nlv are unapproved")
	}
	if mgr.status != rpc.RiskPolicyStatusActive || mgr.review == rpc.PolicyReviewUnreviewed {
		return ev, fmt.Errorf("the risk policy must be current and owner-reviewed")
	}
	pol, polStatus := s.activeRulebookPolicy()
	if polStatus.Review == rpc.PolicyReviewUnreviewed || polStatus.Status != "active" {
		return ev, fmt.Errorf("the Rulebook policy must be current and owner-reviewed")
	}
	cashPolicy, cashStatus := s.protectionPolicies.Active()
	if cashStatus.Status != "active" || cashStatus.Review == rpc.PolicyReviewUnreviewed {
		return ev, fmt.Errorf("the cash reserve policy must be current and owner-reviewed")
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
	keep, known := cashPolicy.Cash.Sweep.keepCash(contract.Currency)
	if !known {
		return ev, fmt.Errorf("cash reserve for %s is unapproved", contract.Currency)
	}
	// Reuse the common account reserve as well as each currency's settlement
	// float. Reserve the full base cushion when spending outside the base too;
	// this is conservative and avoids making cash fungible across currencies.
	bucket := cashPolicy.Cash.Sweep
	if bucket == nil || bucket.ReserveFloorBase == nil || bucket.ReservePctNLV == nil || bucket.NoBuyWhileBorrowed == nil {
		return ev, fmt.Errorf("account cash reserve is unapproved")
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
	keep += reserve / n.BasePerContract
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
	input := risk.StockAddInput{Policy: mgr.policy.PositionAdd, Symbol: contract.Symbol, ConID: contract.ConID, Price: p.LimitPrice, FX: n.BasePerContract, Requested: p.Quantity, FreeCash: min(row.TradeDate, *row.Settled) - keep - commitments.ByCurrency[contract.Currency], Rulebook: pol,
		Rules: risk.RuleInputs{AsOf: now, BaseCurrency: base, Positions: risk.SourceState{Healthy: true}, Account: risk.SourceState{Healthy: true}, NLVBase: new(acct.NetLiquidation), RiskCapital: s.rulebookRiskCapital(acct, nil, base, now), Names: mapRuleNames(pos, pol, base)}}
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
	if capital.Tier != risk.CapitalTierOK || capital.EquityStale || capital.ReconcileStale {
		return ev, fmt.Errorf("capital policy is not ready for new risk: %s", capital.Tier)
	}
	limits := s.orderLimitsInForceForPreview(ctx, base)
	if !limits.Complete {
		return ev, fmt.Errorf("per-order limits are incomplete")
	}
	input.OrderCapBase = limits.CapBase
	for _, stock := range pos.Stocks {
		if stock.Quantity == 0 {
			continue
		}
		if stock.SecType != "STK" {
			// Government debt has a separately evidenced issuer; corporate debt or
			// an unresolved instrument cannot silently disappear from issuer risk.
			government := stock.SecType == "BOND" && slices.ContainsFunc(pos.Bonds, func(b rpc.PositionBond) bool {
				return b.ConID == stock.ConID && b.Quantity == stock.Quantity && b.IssuerClass == rpc.BondIssuerGovernment && b.EvidenceIssuer != "" && b.EvidenceSource != ""
			})
			if government {
				continue
			}
			return ev, fmt.Errorf("cross-asset issuer concentration for held %s is not yet supported by equity Add", stock.SecType)
		}
		rate, ok := positionBaseRate(stock, base)
		if !ok || stock.Stale || !positiveFinite(stock.Mark) {
			return ev, fmt.Errorf("a held stock has no current valuation or currency conversion")
		}
		value := math.Abs(stock.Quantity) * stock.Mark * rate
		input.StockValueBase += value
		if stock.Symbol == contract.Symbol {
			input.UnderlyingStockBase += value
			if stock.ConID != contract.ConID {
				return ev, fmt.Errorf("another stock identity shares this symbol; exact underlying aggregation is required")
			}
			input.CurrentQuantity += stock.Quantity
		}
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
			return fmt.Errorf("pending non-stock risk cannot yet be included in equity Add")
		}
		if o.Action == "SELL" {
			protective := slices.Contains([]string{"STP", "STP LMT", "TRAIL", "TRAIL LIMIT"}, o.OrderType)
			reservedSells[o.ConID] += brokerOrderRemaining(o)
			covered := brokerOrderRemaining(o) > 0 && positiveFinite(reservedSells[o.ConID]) && held[o.ConID] >= reservedSells[o.ConID]
			if !protective || !covered {
				return fmt.Errorf("a pending sale must resolve before adding stock")
			}
			continue
		}
		if o.Action != "BUY" || o.SecType != "STK" || o.ConID <= 0 {
			return fmt.Errorf("pending non-stock risk cannot yet be included in equity Add")
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
	return draft.Add != nil || (mgr.policy != nil && mgr.policy.PositionAdd != nil && draft.Contract.SecType == "STK" && draft.Action == rpc.OrderActionBuy && position.After > max(0, position.Before))
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
	plan := risk.SizeStockAdd(ev.input)
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
