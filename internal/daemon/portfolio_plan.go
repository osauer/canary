package daemon

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// Deliberately stronger than the bucketed display fingerprints: a planning
// generation cannot outlive a change in the actual financial observations.
func portfolioPlanBookFingerprint(account *rpc.AccountResult, positions *rpc.PositionsResult) string {
	if account == nil || positions == nil {
		return ""
	}
	a, p := *account, *positions
	a.AsOf, p.AsOf = time.Time{}, time.Time{}
	a.Authority, p.Authority = nil, nil
	if a.StreamObservation != nil {
		observation := *a.StreamObservation
		observation.ReadAt = time.Time{}
		a.StreamObservation = &observation
	}
	if a.DailyPnLObservation != nil && a.DailyPnLObservation.Status == rpc.DailyPnLObservationNotDue {
		observation := *a.DailyPnLObservation
		observation.AsOf = time.Time{}
		a.DailyPnLObservation = &observation
	}
	// These decorations are read-time UI context, not financial evidence.
	// Coverage is independently fenced by the working-order generation.
	p.ProtectionCoverage, p.ByUnderlying = nil, nil
	normalize := func(rows []rpc.PositionView) []rpc.PositionView {
		rows = slices.Clone(rows)
		for i := range rows {
			rows[i].SessionContext = nil
			rows[i].PriceAsOf, rows[i].QuotePriceAsOf = "", ""
		}
		slices.SortFunc(rows, func(a, b rpc.PositionView) int { return cmp.Compare(a.ConID, b.ConID) })
		return rows
	}
	p.Stocks, p.Options = normalize(p.Stocks), normalize(p.Options)
	raw, err := json.Marshal(struct {
		Account   rpc.AccountResult
		Positions rpc.PositionsResult
	}{a, p})
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

func portfolioPlanHold(out *rpc.PortfolioPlanResult, code, reason string) {
	out.State, out.Reason, out.Next = "held", reason, nil
	out.Blockers = append(out.Blockers, rpc.TradingBlocker{Code: code, Message: reason})
}

// Bind the producer's decision policies and latched regime, not their reload
// timestamps. Opting into portfolio planning creates no new active policy.
func (s *Server) portfolioPlanningPolicyFingerprint(now time.Time) string {
	mgr := s.riskPolicies.snapshot()
	if mgr.policy == nil || mgr.policy.PortfolioPlan == nil {
		return ""
	}
	rules, rs := s.activeRulebookPolicy()
	_, ps := s.protectionPolicies.Active()
	stage, carried := s.rulebookRegimeStage(rules, now)
	raw, err := json.Marshal(struct {
		Risk, Rulebook, Protection string
		Status                     [6]string
		Stage                      string
		AsOf                       time.Time
		Carried                    bool
	}{mgr.policy.FingerprintKey(), rules.FingerprintKey(), ps.Fingerprint.Key, [6]string{string(mgr.status), mgr.review, rs.Status, rs.Review, ps.Status, ps.Review}, stage.Bucket, stage.AsOf, carried})
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

// Served automatic/queue state can change without changing the proposal's
// immutable row revision. Include it in the plan's final concurrency fence.
func portfolioPlanProposalFingerprint(snap rpc.TradeProposalSnapshot) string {
	snap.Proposals = slices.Clone(snap.Proposals)
	for i := range snap.Proposals {
		snap.Proposals[i].Readiness = nil
	}
	raw, err := json.Marshal(struct {
		Rows     []rpc.TradeProposal
		Blockers []rpc.TradingBlocker
		Loaded   bool
	}{snap.Proposals, snap.Blockers, snap.LoadedFromState})
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

func (s *Server) handlePortfolioPlan(ctx context.Context, req *rpc.Request) (*rpc.PortfolioPlanResult, error) {
	if err := rpc.ValidatePortfolioPlanParams(req.Params); err != nil {
		return nil, errBadRequest(err.Error())
	}
	now := s.orderNow()
	out := &rpc.PortfolioPlanResult{Kind: "ibkr.portfolio_plan", AsOf: now, State: "hold", Targets: []risk.PortfolioIntent{}, Reductions: []rpc.TradeProposal{}, Unassigned: []rpc.ContractParams{}, Blockers: []rpc.TradingBlocker{}}
	mgr := s.riskPolicies.snapshot()
	if mgr.policy == nil || mgr.policy.PortfolioPlan == nil {
		portfolioPlanHold(out, "portfolio_policy_missing", "Portfolio target bands, priorities, entry regimes and price ceilings have not been specified.")
		return out, nil
	}
	mandate := mgr.policy.PortfolioPlan
	out.ValidUntil, out.PolicyFingerprint = mandate.ValidUntil, mgr.policy.FingerprintKey()
	if mgr.status != rpc.RiskPolicyStatusActive || mgr.review == rpc.PolicyReviewUnreviewed || !now.Before(mandate.ValidUntil) {
		portfolioPlanHold(out, "portfolio_policy_unavailable", "A current reviewed portfolio planning mandate is required.")
		return out, nil
	}
	broker, err := s.captureOrderPreviewBrokerAuthority()
	if err != nil || broker == nil {
		portfolioPlanHold(out, "portfolio_book_unavailable", "The current broker session is unavailable.")
		return out, nil
	}
	accountFingerprint := stockAddAccountFingerprint(broker)
	ordersGeneration := s.currentProtectionOrderSnapshotBinding().generation
	scope := s.currentBrokerStateScope()
	acct, _, err := s.buildAccountSummaryWithAuthority(ctx, false)
	if err != nil {
		portfolioPlanHold(out, "portfolio_book_unavailable", "Current account evidence is unavailable.")
		return out, nil
	}
	var health ibkrlib.PortfolioStreamHealth
	pos, err := s.handlePositionsListCapturedForScope(ctx, &rpc.Request{}, &health, scope)
	if err != nil || acct == nil || pos == nil || !currentPortfolioAuthority(acct.Authority) || !currentPortfolioAuthority(pos.Authority) || !pos.Authority.PortfolioComplete || acct.Authority.Scope != pos.Authority.Scope || acct.Authority.BrokerReadSession != pos.Authority.BrokerReadSession || proposalPositionsUnprimed(pos, acct, health) {
		portfolioPlanHold(out, "portfolio_book_unavailable", "A complete current account and portfolio from the same broker session are required.")
		return out, nil
	}
	pos = s.analysisPositions(pos, now)
	out.Authority = pos.Authority
	out.BaseCurrency = acct.BaseCurrency
	watch, err := s.handleWatchlistList(ctx)
	if err != nil {
		portfolioPlanHold(out, "portfolio_watchlist_unavailable", "The accepted watchlist cannot be read.")
		return out, nil
	}
	out.WatchlistRevision = watch.Revision
	rulebook, status := s.activeRulebookPolicy()
	stage, carried := s.rulebookRegimeStage(rulebook, now)
	if status.Status != "active" || status.Review == rpc.PolicyReviewUnreviewed || carried || stage.AsOf.After(now) {
		stage.Bucket = ""
	}
	out.Regime, out.RegimeAsOf = stage.Bucket, stage.AsOf
	var nlv *float64
	if acct.Authority.Fields != nil && acct.Authority.Fields.NetLiquidation {
		nlv = new(acct.NetLiquidation)
	}
	evidence, unassigned := portfolioTargetEvidence(mandate, acct, pos, watch, now)
	out.Unassigned = unassigned
	out.Targets, err = risk.EvaluatePortfolioTargets(mandate, now, nlv, stage.Bucket, evidence)
	if err != nil {
		portfolioPlanHold(out, "portfolio_policy_invalid", err.Error())
		return out, nil
	}
	snap := s.tradeProposals.snapshotForRead(false, false)
	planningPolicy := s.portfolioPlanningPolicyFingerprint(now)
	proposalFingerprint := portfolioPlanProposalFingerprint(snap)
	_, protectionStatus := s.protectionPolicies.Active()
	out.ProposalRevision, out.ProposalAsOf = snap.Revision, snap.AsOf
	if !sameProposalPolicy(snap, protectionStatus) || !portfolioProposalEvidenceCurrent(snap, acct, pos, now, s.cfg.AutoTrade.WithDefaults().ProposalCadenceDuration(), planningPolicy) {
		portfolioPlanHold(out, "portfolio_reductions_unavailable", "The reduction review must match this complete portfolio and broker session before a next action can be selected.")
		return out, nil
	}
	selectPortfolioAction(ctx, out, snap, s.planStockAdd)
	// Fence every source again after bounded broker WhatIf reads. This result is
	// observation only; preview and submission still re-read all their own gates.
	watchAfter, watchErr := s.handleWatchlistList(ctx)
	after := s.riskPolicies.snapshot()
	rulebookAfter, rulebookStatusAfter := s.activeRulebookPolicy()
	_, protectionStatusAfter := s.protectionPolicies.Active()
	stageAfter, carriedAfter := s.rulebookRegimeStage(rulebookAfter, s.orderNow())
	snapAfter := s.tradeProposals.snapshotForRead(false, false)
	if proposalFingerprint == "" || portfolioPlanProposalFingerprint(snapAfter) != proposalFingerprint || s.portfolioPlanningPolicyFingerprint(s.orderNow()) != planningPolicy || !portfolioProposalEvidenceCurrent(snapAfter, acct, pos, s.orderNow(), s.cfg.AutoTrade.WithDefaults().ProposalCadenceDuration(), planningPolicy) {
		portfolioPlanHold(out, "portfolio_proposals_changed", "The reduction or existing-instruction evidence changed or expired during planning; calculate again.")
		return out, nil
	}
	if ctx.Err() != nil || watchErr != nil || watchAfter.Revision != watch.Revision || after.policy == nil || after.status != rpc.RiskPolicyStatusActive || after.review == rpc.PolicyReviewUnreviewed || after.policy.FingerprintKey() != out.PolicyFingerprint || !s.orderNow().Before(mandate.ValidUntil) || !s.orderPreviewBrokerAuthorityCurrent(broker) || broker.connector.PortfolioProjectionGeneration() != health.ProjectionGeneration || accountFingerprint == "" || stockAddAccountFingerprint(broker) != accountFingerprint || s.currentProtectionOrderSnapshotBinding().generation != ordersGeneration || snapAfter.Revision != snap.Revision || snapAfter.PlanningEvidence != snap.PlanningEvidence || stageAfter.Bucket != out.Regime || !stageAfter.AsOf.Equal(out.RegimeAsOf) || carriedAfter || rulebookAfter.FingerprintKey() != rulebook.FingerprintKey() || rulebookStatusAfter.Status != status.Status || rulebookStatusAfter.Review != status.Review || protectionStatusAfter.Fingerprint != protectionStatus.Fingerprint || protectionStatusAfter.Status != protectionStatus.Status || protectionStatusAfter.Review != protectionStatus.Review {
		portfolioPlanHold(out, "portfolio_evidence_changed", "Portfolio, orders, policy, watchlist or regime changed during planning; calculate again.")
	}
	return out, nil
}

func portfolioProposalEvidenceCurrent(snap rpc.TradeProposalSnapshot, acct *rpc.AccountResult, pos *rpc.PositionsResult, now time.Time, cadence time.Duration, policy string) bool {
	a := snap.PlanningAuthority
	if policy == "" || snap.PlanningPolicyFingerprint != policy {
		return false
	}
	return acct != nil && pos != nil && currentPortfolioAuthority(pos.Authority) && currentPortfolioAuthority(a) && a.PortfolioComplete && !a.BrokerReadSession.DaemonStartedAt.IsZero() && a.BrokerReadSession.ConnectorGeneration > 0 && a.BrokerReadSession.SocketEpoch > 0 && a.Scope == pos.Authority.Scope && a.BrokerReadSession == pos.Authority.BrokerReadSession && snap.AccountID == a.Scope.AccountID && snap.AccountMode == a.Scope.AccountMode && !snap.LoadedFromState && len(snap.Blockers) == 0 && cadence > 0 && !snap.AsOf.IsZero() && !snap.AsOf.After(now) && now.Sub(snap.AsOf) <= cadence && snap.PlanningEvidence != "" && snap.PlanningEvidence == portfolioPlanBookFingerprint(acct, pos)
}

func portfolioTargetEvidence(policy *risk.PortfolioPlanPolicy, acct *rpc.AccountResult, pos *rpc.PositionsResult, watch *rpc.Watchlist, now time.Time) ([]risk.PortfolioTargetEvidence, []rpc.ContractParams) {
	evidence := []risk.PortfolioTargetEvidence{}
	unassigned := []rpc.ContractParams{}
	_, ledger, ledgerWhy := cashSweepLedgerAt(acct, now)
	if ledgerWhy != "" {
		ledger = nil
	}
	configured := func(c rpc.ContractParams) bool {
		return slices.ContainsFunc(policy.Targets, func(t risk.PortfolioPlanTarget) bool {
			return c.SecType == "STK" && t.ConID == c.ConID && t.Symbol == c.Symbol && t.Currency == c.Currency
		})
	}
	for _, p := range pos.Stocks {
		secType := p.SecType
		if rpc.PositionQuotesAsStock(p) {
			secType = "STK"
		}
		c := proposalContractFromPosition(p, secType)
		if !configured(c) {
			unassigned = append(unassigned, c)
		}
		if !rpc.PositionQuotesAsStock(p) {
			continue
		}
		fx, ok := positionBaseRate(p, acct.BaseCurrency)
		evidence = append(evidence, risk.PortfolioTargetEvidence{ConID: p.ConID, Symbol: p.Symbol, Currency: p.Currency, Quantity: p.Quantity, Mark: p.Mark, FX: fx, Complete: ok && !p.Stale, InUniverse: true})
	}
	for _, w := range watch.Symbols {
		c := rpc.ContractParams{ConID: w.ConID, Symbol: w.Symbol, SecType: w.SecType, Currency: w.Currency, Exchange: w.Exchange}
		if slices.ContainsFunc(pos.Stocks, func(p rpc.PositionView) bool {
			return p.ConID == w.ConID && p.Symbol == w.Symbol && p.Currency == w.Currency
		}) {
			continue
		}
		if !configured(c) {
			unassigned = append(unassigned, c)
		}
		fx := ledger[w.Currency].ExchangeRate
		if w.Currency == acct.BaseCurrency {
			fx = 1
		}
		evidence = append(evidence, risk.PortfolioTargetEvidence{ConID: w.ConID, Symbol: w.Symbol, Currency: w.Currency, FX: fx, Complete: w.ConID > 0 && w.SecType == "STK", InUniverse: true})
	}
	return evidence, unassigned
}

// Only one candidate is sized. Pending purchases of the chosen stock must
// resolve first: their quantity must never be bought again to fill a target gap.
func selectPortfolioAction(ctx context.Context, out *rpc.PortfolioPlanResult, snap rpc.TradeProposalSnapshot, size func(context.Context, rpc.AddParams) (*rpc.AddPlanResult, error)) {
	for _, p := range snap.Proposals {
		if p.PositionEffect != rpc.OrderPositionEffectReduce && p.PositionEffect != rpc.OrderPositionEffectClose {
			continue
		}
		out.Reductions = append(out.Reductions, p)
	}
	for _, p := range snap.Proposals {
		if (p.Automatic != nil && (p.Automatic.PreAuthorised || slices.Contains([]string{rpc.TradeProposalAutomaticPending, rpc.TradeProposalAutomaticDeferred, rpc.TradeProposalAutomaticSubmitting, rpc.TradeProposalAutomaticSubmitted}, p.Automatic.State))) || p.Queued != nil {
			portfolioPlanHold(out, "portfolio_existing_instruction", "An existing automatic or queued instruction must resolve before a new portfolio action.")
			return
		}
	}
	for _, p := range out.Reductions {
		if p.CoveredBy != "" {
			continue
		}
		if !proposalUnblocked(snap, p) || p.Quantity <= 0 || p.Key == "" || p.Revision == "" {
			portfolioPlanHold(out, "portfolio_reduction_review", "A reduction or protection review needs resolution before adding exposure.")
			return
		}
		kind := "reduction"
		if p.Trail != nil || slices.Contains([]string{"STP", "STP LMT", "TRAIL", "TRAIL LIMIT"}, p.OrderType) {
			kind = "protection"
		}
		out.State, out.Reason = "review", "Review the existing exact proposal first, then calculate again after its outcome. No sale proceeds are credited."
		out.Next = &rpc.PortfolioPlanAction{Kind: kind, ProposalKey: p.Key, ProposalRevision: p.Revision}
		return
	}
	for _, r := range out.Targets {
		if r.Decision == "cannot_evaluate" || r.Decision == "review_reduction" {
			portfolioPlanHold(out, "portfolio_target_review", r.Symbol+": "+r.Reason)
			return
		}
	}
	for i, r := range out.Targets {
		if r.Decision != "add" {
			continue
		}
		p := rpc.AddParams{Contract: rpc.ContractParams{ConID: r.ConID, Symbol: r.Symbol, Currency: r.Currency, SecType: "STK", Exchange: "SMART"}, LimitPrice: *r.LimitPrice, Max: true}
		add, err := size(ctx, p)
		if err != nil || add == nil {
			portfolioPlanHold(out, "portfolio_add_unavailable", "The selected target's current addition could not be checked.")
			return
		}
		if len(add.Blockers) > 0 || !add.MaximumKnown || add.Quantity <= 0 {
			portfolioPlanHold(out, "portfolio_add_held", addBlockerText(add.StockAddPlan))
			return
		}
		if add.Protection == nil || add.Protection.PendingQuantity != 0 {
			portfolioPlanHold(out, "portfolio_pending_add", "A purchase commitment for the selected stock must resolve before filling its target gap.")
			return
		}
		if add.Quantity > r.DesiredQuantity {
			p.Max, p.Quantity = false, r.DesiredQuantity
			add, err = size(ctx, p) // Smaller quantities need their own fee/margin quote.
		}
		if err != nil || add == nil || len(add.Blockers) > 0 || add.Quantity <= 0 || add.Quantity > r.DesiredQuantity || add.Review == nil || add.Review.PolicyFingerprint != out.PolicyFingerprint || add.Review.RegimeStage != out.Regime || !add.Review.RegimeAsOf.Equal(out.RegimeAsOf) || add.Contract.ConID != r.ConID || add.Contract.Symbol != r.Symbol || add.Contract.Currency != r.Currency || add.Contract.SecType != "STK" || add.LimitPrice != *r.LimitPrice || r.QuantityBefore == nil || add.Before != *r.QuantityBefore || add.Protection == nil || add.Protection.PendingQuantity != 0 {
			portfolioPlanHold(out, "portfolio_add_changed", "The exact target quantity, position, policy or regime could not be confirmed; calculate again.")
			return
		}
		if add.BaseCurrency != out.BaseCurrency || r.TargetPctNLV == nil || !positiveFinite(add.UnderlyingPctAfter) || add.UnderlyingPctAfter > *r.TargetPctNLV || (!p.Max && add.Quantity != p.Quantity) {
			portfolioPlanHold(out, "portfolio_target_changed", "The checked addition does not fit the target under its current currency and account valuation; calculate again.")
			return
		}
		out.State, out.Reason = "review", "One addition is checked for review. Calculate again after its outcome before sizing another candidate."
		out.Next = &rpc.PortfolioPlanAction{Kind: "add", Add: add}
		for j := i + 1; j < len(out.Targets); j++ {
			if out.Targets[j].Decision == "add" {
				out.Targets[j].Decision, out.Targets[j].Reason = "wait_for_replan", "This desired addition has no reserved allowance; calculate again after the first action's outcome."
			}
		}
		return
	}
	out.State, out.Reason = "hold", "No configured target currently calls for an addition or reduction."
}
