package daemon

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// The premium budget governor (internal-docs/design/budget-governor.md).
//
// With basis declared_risk_capital, while the risk constitution's drawdown
// brake is engaged, long option premium is reduced to the share of declared
// risk capital the owner wrote into [buckets.budget_reduction]. With basis
// rulebook, it is reduced whenever the Rulebook's own limits are breached: a
// line above option_line_act_pct of NLV, or available funds below
// cash_reserve_min_pct of NLV. Either way: per line first, then the total, in
// whole contracts, largest unrealised loss first. Protection legs are never
// selected. Every row is a SELL that reduces or closes; in shadow mode the
// rows are listed and journaled and nothing can preview or submit them.

// budgetGovernorInput is what the governor reads from the risk constitution
// and its runtime verdict. The engine gathers it; tests build it directly.
type budgetGovernorInput struct {
	Constitution *risk.Constitution
	Capital      rpc.CapitalStateReport
	Unapproved   []string
	// Rulebook is the Rulebook policy in force; the rulebook basis reads its
	// limits against the account's NLV and available funds.
	Rulebook            risk.RulebookPolicy
	NLVBase             *float64
	AvailableFundsBase  *float64
	AccountBaseCurrency string
}

// resolveBudgetInput honours the test seam, else reads the daemon's own
// risk-policy manager and capital state.
func (e *proposalEngine) resolveBudgetInput(acct *rpc.AccountResult, now time.Time) budgetGovernorInput {
	if e != nil && e.budgetInput != nil {
		return e.budgetInput(acct, now)
	}
	return e.budgetGovernorInput(acct, now)
}

// budgetGovernorInput resolves the active constitution and its capital report
// for the connected account. A daemon without the risk-policy manager (tests,
// or a build that never installed it) reads as no constitution.
func (e *proposalEngine) budgetGovernorInput(acct *rpc.AccountResult, now time.Time) budgetGovernorInput {
	in := budgetGovernorInput{Rulebook: e.rulebookPolicy()}
	if acct != nil && currentPortfolioAuthority(acct.Authority) && acct.AccountID == acct.Authority.Scope.AccountID &&
		acct.Authority.Fields != nil && acct.Authority.Fields.BaseCurrency && normCcy(acct.BaseCurrency) != "" {
		in.AccountBaseCurrency = normCcy(acct.BaseCurrency)
		if acct.Authority.Fields.NetLiquidation && positiveFinite(acct.NetLiquidation) {
			in.NLVBase = new(acct.NetLiquidation)
		}
		// Legacy scalar zeros are not evidence that the broker observed zero.
		if acct.Authority.Fields.AvailableFunds && !math.IsNaN(acct.AvailableFunds) && !math.IsInf(acct.AvailableFunds, 0) {
			in.AvailableFundsBase = new(acct.AvailableFunds)
		}
	}
	if e == nil || e.server == nil || e.server.riskPolicies == nil || e.server.riskCapital == nil {
		return in
	}
	authority := e.server.acceptedRiskPolicy(now)
	res := e.server.policyResultForEvaluation(acct, nil, authority, now)
	in.Constitution, in.Capital, in.Unapproved = authority.policy, res.Capital, res.Unapproved
	return in
}

// budgetLine is one measured non-protection long option leg.
type budgetLine struct {
	row       rpc.PositionView
	contracts int
	// unitBase is the base-currency value of one contract; valueBase is the
	// whole line. lossBase is the unrealised loss (zero when unknown or a
	// gain), the first ordering key of the total pass. atRiskBase is the
	// higher of price paid and value, the Rulebook's per-line measure.
	unitBase   float64
	valueBase  float64
	atRiskBase float64
	lossBase   float64
	pnlBase    *float64
	// unit marks a leg of a multi-leg unit or an ambiguous underlying: it is
	// measured, and a row for it routes to the strategy workflow.
	unit bool
	// perLineCut and totalCut are the contracts each pass asks to sell;
	// order is the line's place in the total pass (1 = first).
	perLineCut int
	totalCut   int
	order      int
}

// budgetPlan is the pure result of measuring a book against the caps.
type budgetPlan struct {
	status     rpc.TradeProposalBudgetStatus
	lines      []budgetLine
	declared   float64
	perLineCap float64
	totalCap   float64
	total      float64
	// Rulebook basis: NLV and the cash the reserve is short.
	nlv       float64
	shortfall float64
}

const budgetMoneyEpsilon = 1e-6

// budgetReductionPlan measures the book against the caps. It generates no
// proposals; the gates it applies, in order, are the constitution (active and
// approved), its block enforcement class (advisory or stronger), the brake
// (latched or breached), and the base currency (the constitution's must be
// the account's).
func budgetReductionPlan(policy protectionPolicy, input budgetGovernorInput, pos *rpc.PositionsResult, now time.Time) budgetPlan {
	bucket := policy.Buckets.BudgetReduction
	if input.Rulebook.ID == "" {
		// An input built without a Rulebook policy (a test seam) must not
		// classify with an empty hedge list: protection legs would become
		// sellable lines.
		input.Rulebook = risk.DefaultRulebookPolicy()
	}
	if missing := bucket.missingNumbers(); len(missing) > 0 {
		mode := bucket.effectiveMode()
		return budgetPlan{status: rpc.TradeProposalBudgetStatus{
			Mode: mode, Shadow: mode == rpc.BudgetReductionModeShadow, Basis: bucket.basis(),
			BaseCurrency: protectionCoverageBaseCurrency(pos),
			State:        rpc.BudgetStateNeedsYourNumber,
			Reason:       "needs your number: " + strings.Join(missing, ", ") + " in [buckets.budget_reduction]; the governor stays off until you write them",
		}}
	}
	if bucket.basis() == rpc.BudgetBasisRulebook {
		return budgetRulebookPlan(policy, input, pos, now)
	}
	mode := bucket.effectiveMode()
	plan := budgetPlan{status: rpc.TradeProposalBudgetStatus{
		Mode: mode, Shadow: mode == rpc.BudgetReductionModeShadow, Basis: rpc.BudgetBasisDeclaredRiskCapital,
		PremiumAtRiskPctOfRiskCapital: bucket.PremiumAtRiskPctOfRiskCapital,
		PerLinePctOfRiskCapital:       bucket.PerLinePctOfRiskCapital,
		BaseCurrency:                  protectionCoverageBaseCurrency(pos),
	}}
	st := &plan.status
	c := input.Constitution
	switch {
	case c == nil:
		st.State, st.Reason = rpc.BudgetStateConstitutionUnapproved, "no risk constitution is active; the budget is measured against capital.declared_risk_capital, which nobody has declared"
		return plan
	case len(input.Unapproved) > 0 || len(c.UnapprovedKeys()) > 0 || input.Capital.Tier == risk.CapitalTierUnapproved || c.Capital.DeclaredRiskCapital == nil:
		st.State, st.Reason = rpc.BudgetStateConstitutionUnapproved, "the risk constitution has unapproved material keys; the capital tier is unapproved and the governor measures nothing"
		return plan
	case c.EffectiveBlockEnforcement() == risk.EnforcementShadow:
		st.State, st.Reason = rpc.BudgetStateEnforcementShadow, "drawdown.block_enforcement is shadow; the governor acts only under advisory or stronger enforcement"
		return plan
	case !input.Capital.BlockLatched && input.Capital.Tier != risk.CapitalTierBlock:
		st.State, st.Reason = rpc.BudgetStateNotLatched, fmt.Sprintf("the drawdown block tier is neither latched nor breached (tier %s); the governor waits for the brake", nonEmptyString(input.Capital.Tier, risk.CapitalTierUnknown))
		return plan
	}
	if want, have := normCcy(c.Capital.BaseCurrency), normCcy(st.BaseCurrency); want != "" && have != "" && want != have {
		st.State, st.Reason = rpc.BudgetStateCurrencyMismatch, fmt.Sprintf("the constitution declares risk capital in %s and the account reports base values in %s; the caps cannot be compared", want, have)
		return plan
	}
	plan.declared = *c.Capital.DeclaredRiskCapital
	plan.perLineCap = bucket.PerLinePctOfRiskCapital / 100 * plan.declared
	plan.totalCap = bucket.PremiumAtRiskPctOfRiskCapital / 100 * plan.declared
	st.DeclaredRiskCapitalBase = new(plan.declared)

	plan.lines, plan.total = budgetMeasureLines(policy, input.Rulebook, pos, st, now)
	if st.IncludedLegs == 0 && st.ExcludedLegs > 0 {
		st.State, st.Reason = rpc.BudgetStateUnmeasurable, fmt.Sprintf("%d long option %s without a base market value; nothing can be measured", st.ExcludedLegs, pluralNoun(st.ExcludedLegs, "leg"))
		return plan
	}
	st.MeasuredPremiumBase = new(plan.total)
	pct := plan.total / plan.declared * 100
	st.MeasuredPctOfRiskCapital = &pct

	// Per line first: a line above its cap keeps floor(cap / unit) contracts
	// and sells the rest; a cap that rounds to zero contracts is a full close.
	overLine := false
	for i := range plan.lines {
		line := &plan.lines[i]
		if line.unitBase <= 0 || line.valueBase <= plan.perLineCap+budgetMoneyEpsilon {
			continue
		}
		keep := max(int(math.Floor(plan.perLineCap/line.unitBase+1e-9)), 0)
		line.perLineCut = max(line.contracts-keep, 1)
		overLine = true
	}
	// Then the total: while the projected sum still exceeds the cap, sell
	// whole contracts from the lines in order, largest unrealised loss first,
	// then largest line value.
	remaining := plan.total
	for _, line := range plan.lines {
		remaining -= float64(line.perLineCut) * line.unitBase
	}
	budgetTotalPass(plan.lines, remaining-plan.totalCap)
	if totalExcess := plan.total - plan.totalCap; totalExcess > budgetMoneyEpsilon {
		st.TotalExcessBase = new(totalExcess)
	}
	if st.TotalExcessBase != nil || overLine {
		st.State = rpc.BudgetStateOverBudget
	} else {
		st.State = rpc.BudgetStateWithinBudget
	}
	if st.ExcludedLegs > 0 {
		st.Reason = fmt.Sprintf("%d long option %s excluded for want of a base market value; the measured total is understated by that much", st.ExcludedLegs, pluralNoun(st.ExcludedLegs, "leg"))
	}
	return plan
}

// budgetMeasureLines measures every non-protection long option line and
// counts protection and unmeasurable legs into st. The total is the sum of
// line values.
func budgetMeasureLines(policy protectionPolicy, rulebook risk.RulebookPolicy, pos *rpc.PositionsResult, st *rpc.TradeProposalBudgetStatus, now time.Time) ([]budgetLine, float64) {
	if pos == nil {
		return nil, 0
	}
	var lines []budgetLine
	total := 0.0
	intents := directionalOptionIntents(policy.Buckets.TrailingStop.Options)
	strategyLegs, ambiguous := optionExitStrategyScope(pos, intents, now)
	for _, row := range pos.Options {
		if row.Quantity <= 0 || math.IsNaN(row.Quantity) || math.IsInf(row.Quantity, 0) || positionWireSecType(row.SecType) != "OPT" {
			continue
		}
		if budgetLegIsProtection(policy.Buckets.TrailingStop.Options, row, pos, strategyLegs, ambiguous, rulebook, now) {
			st.ProtectionLegs++
			continue
		}
		contracts := int(math.Floor(row.Quantity + 1e-9))
		if row.MarketValueBase == nil || contracts <= 0 {
			st.ExcludedLegs++
			continue
		}
		line := budgetLine{row: row, contracts: contracts, valueBase: max(*row.MarketValueBase, 0), unit: strategyLegs[row.ConID] || ambiguous[normSym(row.Symbol)]}
		line.unitBase = line.valueBase / float64(contracts)
		line.atRiskBase = line.valueBase
		if rate, ok := positionBaseRate(row, st.BaseCurrency); ok && row.AvgCost > 0 {
			line.atRiskBase = max(line.valueBase, row.AvgCost*float64(contracts)*rate)
		}
		line.pnlBase = budgetLinePnLBase(row, st.BaseCurrency)
		if line.pnlBase != nil && *line.pnlBase < 0 {
			line.lossBase = -*line.pnlBase
		}
		lines = append(lines, line)
		total += line.valueBase
		st.IncludedLegs++
	}
	return lines, total
}

// budgetTotalPass sells whole contracts, largest unrealised loss first and
// then largest line value, until the excess (in value) is covered. Contracts
// the per-line pass already sold are not sold twice.
func budgetTotalPass(lines []budgetLine, excess float64) {
	if excess <= budgetMoneyEpsilon {
		return
	}
	order := make([]int, len(lines))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int {
		la, lb := lines[a], lines[b]
		if la.lossBase != lb.lossBase {
			if la.lossBase > lb.lossBase {
				return -1
			}
			return 1
		}
		if la.valueBase != lb.valueBase {
			if la.valueBase > lb.valueBase {
				return -1
			}
			return 1
		}
		return la.row.ConID - lb.row.ConID
	})
	place := 0
	for _, i := range order {
		line := &lines[i]
		available := line.contracts - line.perLineCut
		if line.unitBase <= 0 || available <= 0 {
			continue
		}
		take := min(int(math.Ceil(excess/line.unitBase-1e-9)), available)
		if take <= 0 {
			continue
		}
		place++
		line.totalCut, line.order = take, place
		excess -= float64(take) * line.unitBase
		if excess <= budgetMoneyEpsilon {
			return
		}
	}
}

// budgetRulebookPlan measures the book against the Rulebook's own limits as
// shares of NLV: a line whose premium at risk (the higher of price paid and
// value) exceeds option_line_act_pct is cut to it, and when available funds
// sit below cash_reserve_min_pct, lines are sold in the loss-first order
// until their value covers the shortfall. It needs the account's NLV and
// available funds, not the risk constitution, and waits for no brake.
func budgetRulebookPlan(policy protectionPolicy, input budgetGovernorInput, pos *rpc.PositionsResult, now time.Time) budgetPlan {
	mode := policy.Buckets.BudgetReduction.effectiveMode()
	rb := input.Rulebook
	plan := budgetPlan{status: rpc.TradeProposalBudgetStatus{
		Mode: mode, Shadow: mode == rpc.BudgetReductionModeShadow, Basis: rpc.BudgetBasisRulebook,
		PerLinePctOfNLV: rb.OptionLineActPct, CashReserveMinPct: rb.CashReserveMinPct,
		BaseCurrency: protectionCoverageBaseCurrency(pos),
	}}
	st := &plan.status
	if input.NLVBase == nil || *input.NLVBase <= 0 || input.AvailableFundsBase == nil {
		st.State, st.Reason = rpc.BudgetStateAccountUnavailable, "the account's NLV or available funds are unavailable; the Rulebook limits are shares of NLV"
		return plan
	}
	if want, have := normCcy(input.AccountBaseCurrency), normCcy(st.BaseCurrency); want != "" && have != "" && want != have {
		st.State, st.Reason = rpc.BudgetStateCurrencyMismatch, fmt.Sprintf("the account reports NLV in %s and positions in %s; the limits cannot be compared", want, have)
		return plan
	}
	plan.nlv = *input.NLVBase
	st.NLVBase, st.AvailableFundsBase = new(plan.nlv), new(*input.AvailableFundsBase)
	plan.perLineCap = rb.OptionLineActPct / 100 * plan.nlv
	plan.lines, plan.total = budgetMeasureLines(policy, rb, pos, st, now)
	if st.IncludedLegs == 0 && st.ExcludedLegs > 0 {
		st.State, st.Reason = rpc.BudgetStateUnmeasurable, fmt.Sprintf("%d long option %s without a base market value; nothing can be measured", st.ExcludedLegs, pluralNoun(st.ExcludedLegs, "leg"))
		return plan
	}
	st.MeasuredPremiumBase = new(plan.total)

	overLine := false
	raised := 0.0
	for i := range plan.lines {
		line := &plan.lines[i]
		if line.contracts <= 0 || line.atRiskBase <= plan.perLineCap+budgetMoneyEpsilon {
			continue
		}
		unitRisk := line.atRiskBase / float64(line.contracts)
		keep := max(int(math.Floor(plan.perLineCap/unitRisk+1e-9)), 0)
		line.perLineCut = max(line.contracts-keep, 1)
		raised += float64(line.perLineCut) * line.unitBase
		overLine = true
	}
	plan.shortfall = rb.CashReserveMinPct/100*plan.nlv - *input.AvailableFundsBase
	if plan.shortfall > budgetMoneyEpsilon {
		st.CashShortfallBase = new(plan.shortfall)
		budgetTotalPass(plan.lines, plan.shortfall-raised)
	}
	if overLine || plan.shortfall > budgetMoneyEpsilon {
		st.State = rpc.BudgetStateOverBudget
	} else {
		st.State = rpc.BudgetStateWithinBudget
	}
	if st.ExcludedLegs > 0 {
		st.Reason = fmt.Sprintf("%d long option %s excluded for want of a base market value", st.ExcludedLegs, pluralNoun(st.ExcludedLegs, "leg"))
	}
	return plan
}

// budgetLegIsProtection applies the standing option-purpose derivation and,
// on top of it, the two rules the governor never bends: a hedge-listed long
// put is protection whatever any declaration says, and an option that covers a
// stock of its own underlying is protection.
func budgetLegIsProtection(cfg protectionTrailOptionPolicy, row rpc.PositionView, pos *rpc.PositionsResult, legs map[int]bool, ambiguous map[string]bool, pol risk.RulebookPolicy, now time.Time) bool {
	if optionExitPurpose(cfg, row, pos, legs, ambiguous, pol, now) == "protection" {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(row.Right), "P") && pol.IsHedgeSymbol(row.Symbol) {
		return true
	}
	return optionExitCallHedge(row, pos, pol) || optionExitPutHedge(row, pos, pol)
}

// budgetLinePnLBase reads the unrealised P&L in base currency, converting the
// local figure when only the rate is known. Nil means unknown.
func budgetLinePnLBase(row rpc.PositionView, base string) *float64 {
	if row.UnrealizedPnLBase != nil {
		return cloneFloat64Ptr(row.UnrealizedPnLBase)
	}
	if row.UnrealizedPnL == 0 {
		return nil
	}
	if rate, ok := positionBaseRate(row, base); ok {
		value := row.UnrealizedPnL * rate
		return &value
	}
	return nil
}

// budgetReductionProposals turns a plan into proposals. Every row is a SELL
// that reduces or closes; blockers name what would stop the order in active
// mode, and shadow mode adds shadow_mode in front of them.
func (e *proposalEngine) budgetReductionProposals(policy protectionPolicy, status rpc.ProtectionPolicyStatus, input budgetGovernorInput, acct *rpc.AccountResult, pos *rpc.PositionsResult, sources rpc.TradeProposalSourceFingerprints, marketEvents *rpc.MarketEventsResult, scope brokerStateScope, now time.Time) ([]rpc.TradeProposal, *rpc.TradeProposalBudgetStatus) {
	bucket := policy.Buckets.BudgetReduction
	if !bucket.enabled() {
		return nil, nil
	}
	plan := budgetReductionPlan(policy, input, pos, now)
	st := plan.status
	var out []rpc.TradeProposal
	for _, line := range plan.lines {
		cut := line.perLineCut + line.totalCut
		if cut <= 0 {
			continue
		}
		p := budgetReductionRow(policy, status, sources, now, plan, line, cut)
		if line.row.Stale {
			budgetBlock(&p, rpc.TradingBlocker{Code: "fresh_option_quote_required", Message: "the position mark is stale, so the measured line value cannot price an order", Action: "Refresh during the options session (09:30-16:00 ET) so the mark is current before acting."})
		}
		if line.unit {
			budgetBlock(&p, rpc.TradingBlocker{Code: "strategy_workflow_required", Message: "this leg belongs to a multi-leg unit; a budget reduction cannot trim one leg of it as a single-leg order", Action: "Reduce the unit through the strategy workflow (canary strategies close ID REVISION) or a combo order at the broker."})
		}
		enrichProposalPositionContext(&p, line.row, acct)
		applyMarketEventFlagsToProposal(&p, marketEvents)
		if st.Shadow {
			// In front of every other blocker: the mode is the first thing a
			// reader must know about the row.
			p.Blockers = append([]rpc.TradingBlocker{budgetShadowBlocker()}, p.Blockers...)
			p.State = rpc.TradeProposalStateBlocked
		}
		if e != nil && e.isIgnored(scope, p.Key) {
			continue
		}
		out = append(out, p)
	}
	st.Rows = len(out)
	return out, &st
}

// budgetReductionRow builds one governor proposal. The quantity is the sum of
// both passes' cuts, bounded by the order notional cap exactly as
// risk_reduction bounds its orders; a bounded row is a reduce even when the
// plan asked for the whole line, and the next cycle measures what is left.
func budgetReductionRow(policy protectionPolicy, status rpc.ProtectionPolicyStatus, sources rpc.TradeProposalSourceFingerprints, now time.Time, plan budgetPlan, line budgetLine, cut int) rpc.TradeProposal {
	bucket := policy.Buckets.BudgetReduction
	row := line.row
	qty := cut
	mark := math.Abs(row.Mark)
	if mark <= 0 {
		mark = math.Abs(row.ValuationMark)
	}
	if mark > 0 && bucket.MaxOrderNotional > 0 {
		mult := float64(max(row.Multiplier, 1))
		qty = min(qty, int(math.Max(1, math.Floor(bucket.MaxOrderNotional/(mark*mult)))))
	}
	qty = max(1, min(qty, line.contracts))
	effect := rpc.OrderPositionEffectReduce
	if qty == line.contracts {
		effect = rpc.OrderPositionEffectClose
	}
	if plan.status.Basis == rpc.BudgetBasisRulebook {
		return budgetRulebookRow(policy, status, sources, now, plan, line, cut, qty, effect)
	}
	base := plan.status.BaseCurrency
	linePct := line.valueBase / plan.declared * 100
	totalPct := plan.total / plan.declared * 100
	budget := &rpc.TradeProposalBudget{
		Basis:                         rpc.BudgetBasisDeclaredRiskCapital,
		Mode:                          plan.status.Mode,
		PremiumAtRiskPctOfRiskCapital: bucket.PremiumAtRiskPctOfRiskCapital,
		PerLinePctOfRiskCapital:       bucket.PerLinePctOfRiskCapital,
		DeclaredRiskCapitalBase:       plan.declared,
		LineMarketValueBase:           line.valueBase,
		LinePctOfRiskCapital:          linePct,
		TotalMeasuredBase:             plan.total,
		TotalPctOfRiskCapital:         totalPct,
		Order:                         line.order,
		ContractsPerLine:              line.perLineCut,
		ContractsTotal:                line.totalCut,
		UnrealizedPnLBase:             line.pnlBase,
		BaseCurrency:                  base,
	}
	if line.perLineCut > 0 {
		budget.LineExcessBase = new(line.valueBase - plan.perLineCap)
	}
	if excess := plan.total - plan.totalCap; excess > budgetMoneyEpsilon {
		budget.TotalExcessBase = new(excess)
	}
	var reason string
	var details []string
	switch {
	case line.perLineCut > 0 && line.totalCut > 0:
		budget.Cap = "per_line+total"
		reason = fmt.Sprintf("premium line is %.1f%% of declared risk capital, above the %.1f%% per-line cap (%d %s), and total premium at risk is %.1f%%, above the %.1f%% cap (%s in order, %d %s)",
			linePct, bucket.PerLinePctOfRiskCapital, line.perLineCut, pluralNoun(line.perLineCut, "contract"),
			totalPct, bucket.PremiumAtRiskPctOfRiskCapital, budgetOrdinal(line.order), line.totalCut, pluralNoun(line.totalCut, "contract"))
	case line.perLineCut > 0:
		budget.Cap = "per_line"
		reason = fmt.Sprintf("premium line is %.1f%% of declared risk capital, above the %.1f%% per-line cap; sell %d of %d %s to the cap",
			linePct, bucket.PerLinePctOfRiskCapital, line.perLineCut, line.contracts, pluralNoun(line.contracts, "contract"))
	default:
		budget.Cap = "total"
		reason = fmt.Sprintf("total premium at risk is %.1f%% of declared risk capital, above the %.1f%% cap; %s in the reduction order (largest loss first): sell %d of %d %s",
			totalPct, bucket.PremiumAtRiskPctOfRiskCapital, budgetOrdinal(line.order), line.totalCut, line.contracts, pluralNoun(line.contracts, "contract"))
	}
	details = append(details, fmt.Sprintf("line %s (%.1f%%) · per-line cap %s (%.1f%% of %s declared)",
		formatBudgetMoney(line.valueBase, base), linePct, formatBudgetMoney(plan.perLineCap, base), bucket.PerLinePctOfRiskCapital, formatBudgetMoney(plan.declared, base)))
	details = append(details, fmt.Sprintf("total %s (%.1f%%) · total cap %s (%.1f%%)",
		formatBudgetMoney(plan.total, base), totalPct, formatBudgetMoney(plan.totalCap, base), bucket.PremiumAtRiskPctOfRiskCapital))
	if line.pnlBase != nil {
		details = append(details, fmt.Sprintf("unrealised %s", formatBudgetMoney(*line.pnlBase, base)))
	}
	if qty < cut {
		details = append(details, fmt.Sprintf("order held to %d %s by max_order_notional %.0f; the next cycle measures the remainder", qty, pluralNoun(qty, "contract"), bucket.MaxOrderNotional))
	}
	if effect == rpc.OrderPositionEffectClose {
		details = append(details, "full close: the cap leaves no whole contract to keep")
	}
	details = append(details, "waits the full veto window even under a latched brake: a reduction to budget is a discretionary-scale action, not a stop")

	p := baseProposal(policy, status, sources, now, rpc.TradeProposalBucketBudgetReduction, row, rpc.OrderActionSell, qty, effect, reason)
	p.Budget = budget
	p.Details = details
	p.Score = float64(cut) * line.unitBase
	p.Shadow = plan.status.Shadow
	p.NeverSkipVeto = true
	return p
}

// budgetRulebookRow builds one governor row measured against the Rulebook's
// limits. The quantity is already bounded by the order notional cap.
func budgetRulebookRow(policy protectionPolicy, status rpc.ProtectionPolicyStatus, sources rpc.TradeProposalSourceFingerprints, now time.Time, plan budgetPlan, line budgetLine, cut, qty int, effect string) rpc.TradeProposal {
	bucket := policy.Buckets.BudgetReduction
	base := plan.status.BaseCurrency
	linePct := line.atRiskBase / plan.nlv * 100
	budget := &rpc.TradeProposalBudget{
		Mode: plan.status.Mode, Basis: rpc.BudgetBasisRulebook,
		LineMarketValueBase: line.valueBase, LineAtRiskBase: line.atRiskBase, LinePctOfNLV: linePct,
		PerLinePctOfNLV: plan.status.PerLinePctOfNLV, CashReserveMinPct: plan.status.CashReserveMinPct,
		TotalMeasuredBase: plan.total, Order: line.order,
		ContractsPerLine: line.perLineCut, ContractsTotal: line.totalCut,
		UnrealizedPnLBase: line.pnlBase, BaseCurrency: base,
	}
	if line.perLineCut > 0 {
		budget.LineExcessBase = new(line.atRiskBase - plan.perLineCap)
	}
	if plan.shortfall > budgetMoneyEpsilon {
		budget.CashShortfallBase = new(plan.shortfall)
	}
	availablePct := 0.0
	if plan.status.AvailableFundsBase != nil {
		availablePct = *plan.status.AvailableFundsBase / plan.nlv * 100
	}
	var reason string
	switch {
	case line.perLineCut > 0 && line.totalCut > 0:
		budget.Cap = "per_line+total"
		reason = fmt.Sprintf("the line puts %.1f%% of NLV at risk, above the Rulebook's %.1f%% line limit (%d %s), and available funds are %.1f%% of NLV, below the %.0f%% cash reserve (%s in order, %d %s)",
			linePct, plan.status.PerLinePctOfNLV, line.perLineCut, pluralNoun(line.perLineCut, "contract"),
			availablePct, plan.status.CashReserveMinPct, budgetOrdinal(line.order), line.totalCut, pluralNoun(line.totalCut, "contract"))
	case line.perLineCut > 0:
		budget.Cap = "per_line"
		reason = fmt.Sprintf("the line puts %.1f%% of NLV at risk, above the Rulebook's %.1f%% line limit; sell %d of %d %s to the limit",
			linePct, plan.status.PerLinePctOfNLV, line.perLineCut, line.contracts, pluralNoun(line.contracts, "contract"))
	default:
		budget.Cap = "total"
		reason = fmt.Sprintf("available funds are %.1f%% of NLV, below the Rulebook's %.0f%% cash reserve; %s in the reduction order (largest loss first): sell %d of %d %s",
			availablePct, plan.status.CashReserveMinPct, budgetOrdinal(line.order), line.totalCut, line.contracts, pluralNoun(line.contracts, "contract"))
	}
	details := []string{
		fmt.Sprintf("line %s at risk (%.1f%% of NLV; the higher of price paid and value) · line limit %s (%.1f%% of %s NLV)",
			formatBudgetMoney(line.atRiskBase, base), linePct, formatBudgetMoney(plan.perLineCap, base), plan.status.PerLinePctOfNLV, formatBudgetMoney(plan.nlv, base)),
	}
	if plan.shortfall > budgetMoneyEpsilon {
		details = append(details, fmt.Sprintf("cash reserve short by %s; a sale at today's value raises about %s per contract", formatBudgetMoney(plan.shortfall, base), formatBudgetMoney(line.unitBase, base)))
	}
	if line.pnlBase != nil {
		details = append(details, fmt.Sprintf("unrealised %s", formatBudgetMoney(*line.pnlBase, base)))
	}
	if qty < cut {
		details = append(details, fmt.Sprintf("order held to %d %s by max_order_notional %.0f; the next cycle measures the remainder", qty, pluralNoun(qty, "contract"), bucket.MaxOrderNotional))
	}
	if effect == rpc.OrderPositionEffectClose {
		details = append(details, "full close: the limit leaves no whole contract to keep")
	}
	details = append(details, "waits the full veto window even under a latched brake: a reduction to budget is a discretionary-scale action, not a stop")

	p := baseProposal(policy, status, sources, now, rpc.TradeProposalBucketBudgetReduction, line.row, rpc.OrderActionSell, qty, effect, reason)
	p.Budget = budget
	p.Details = details
	p.Score = float64(cut) * line.unitBase
	p.Shadow = plan.status.Shadow
	p.NeverSkipVeto = true
	return p
}

func budgetShadowBlocker() rpc.TradingBlocker {
	return rpc.TradingBlocker{
		Code:    "shadow_mode",
		Message: "budget reduction runs in shadow mode; this row is listed for observation and cannot be previewed or submitted",
		Action:  "Set mode = \"active\" under [buckets.budget_reduction] and bump policy_version to make these rows ordinary proposals.",
	}
}

// shadowProposalBlockers is the preview and submit refusal for a shadow row.
// It reads the flag, not the bucket, so any future shadow-mode bucket is
// refused the same way.
func shadowProposalBlockers(p rpc.TradeProposal) []rpc.TradingBlocker {
	if !p.Shadow {
		return nil
	}
	return []rpc.TradingBlocker{budgetShadowBlocker()}
}

func budgetBlock(p *rpc.TradeProposal, blocker rpc.TradingBlocker) {
	if p == nil {
		return
	}
	p.State = rpc.TradeProposalStateBlocked
	p.Blockers = appendTradingBlockerOnce(p.Blockers, blocker)
}

func budgetOrdinal(n int) string {
	switch {
	case n <= 0:
		return "unranked"
	case n%100 >= 11 && n%100 <= 13:
		return fmt.Sprintf("%dth", n)
	case n%10 == 1:
		return fmt.Sprintf("%dst", n)
	case n%10 == 2:
		return fmt.Sprintf("%dnd", n)
	case n%10 == 3:
		return fmt.Sprintf("%drd", n)
	default:
		return fmt.Sprintf("%dth", n)
	}
}

func formatBudgetMoney(value float64, currency string) string {
	if currency = strings.TrimSpace(currency); currency == "" {
		return fmt.Sprintf("%.0f", value)
	}
	return fmt.Sprintf("%.0f %s", value, currency)
}
