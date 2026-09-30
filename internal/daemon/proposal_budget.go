package daemon

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// The premium budget governor (internal-docs/design/budget-governor.md).
//
// With basis declared_risk_capital, while the risk constitution's drawdown
// brake is engaged, long option premium is reduced to the share of declared
// risk capital the owner wrote into [buckets.budget_reduction]. With basis
// rulebook, it is reduced whenever the Rulebook's own limits are breached: a
// line above option_line_act_pct of NLV, or the book's premium at risk at or
// above rule 3's premium budget act level of the regime set in force, which
// is cut back to that set's watch level (rule 1's trim convention). Either
// way: per line first, then the total, in whole contracts, the line that
// relieves the most open Rulebook rules first, then the most time value, then
// the largest unrealised loss (amendment 2026-09-30). Protection legs are
// never selected. Every row is a SELL that reduces or closes; in shadow mode
// the rows are listed and journaled and nothing can preview or submit them,
// and under basis rulebook an unreviewed Rulebook policy file holds the
// governor in shadow whatever the mode says.

// budgetGovernorInput is what the governor reads from the risk constitution
// and its runtime verdict. The engine gathers it; tests build it directly.
type budgetGovernorInput struct {
	Constitution *risk.Constitution
	Capital      rpc.CapitalStateReport
	Unapproved   []string
	// Rulebook is the Rulebook policy in force; the rulebook basis reads its
	// limits against the account's NLV. Available funds are context only.
	Rulebook            risk.RulebookPolicy
	NLVBase             *float64
	AvailableFundsBase  *float64
	AccountBaseCurrency string
	// RegimeStage and RegimeStageCarried are the latched regime bucket and
	// whether it is stale, read exactly as the Rulebook reads them, so the
	// premium budget is the one rule 3 applies. Empty means never observed
	// (the calm set).
	RegimeStage        string
	RegimeStageCarried bool
	// RulebookReview is the review state of the Rulebook policy file in
	// force (rpc.PolicyReviewUnreviewed while it is Canary's default) and
	// RulebookPath where it lives: under basis rulebook an unreviewed file
	// holds the governor in shadow (amendment 2026-09-30).
	RulebookReview string
	RulebookPath   string
	// Rules is the latest Rulebook result the daemon holds for this scope and
	// policy, nil when none is current. The ranking reads from it which open
	// rules each line offends; the governor never re-measures them.
	Rules *rpc.RulesResult
	// Ignored reports a governor row key the owner ignored: the plan passes
	// over that line to the next candidate.
	Ignored func(key string) bool
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
	var server *Server
	if e != nil {
		server = e.server
	}
	// One read, so the review state is the one of the policy the limits
	// come from.
	rulebook, rulebookStatus := server.activeRulebookPolicy()
	in := budgetGovernorInput{Rulebook: rulebook, RulebookReview: rulebookStatus.Review, RulebookPath: rulebookStatus.Path}
	if server != nil {
		// The Rulebook's own current verdict, exactly as the stress read
		// takes it: bound to this broker scope and policy, never re-measured.
		in.Rules, _ = server.cachedRulebookResult(server.currentRulebookBinding(), rulesPreviewTTL, server.orderNow().UTC())
	}
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
	if e != nil && e.server != nil {
		stage, carried := e.server.rulebookRegimeStage(in.Rulebook, now)
		in.RegimeStage, in.RegimeStageCarried = stage.Bucket, carried
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
	// relief is the open Rulebook rules this line or its issuer offends,
	// timeValuePct its extrinsic value as a percent of its value (nil when
	// unknown), rank its place in the ranking (0 when it cannot be sold), and
	// ignored says the owner ignored its row.
	relief       []budgetRelief
	timeValuePct *float64
	rank         int
	ignored      bool
}

// budgetRelief is one open Rulebook rule a sale of the line relieves, with
// the line's or its issuer's observed value on that rule.
type budgetRelief struct {
	id       string
	number   int
	observed float64
}

// budgetPlanOrder is one order of the plan: a line (index into
// budgetPlan.lines), the contracts it sells, and the cycle it can go in.
type budgetPlanOrder struct {
	line      int
	contracts int
	cycle     int
}

// budgetPlan is the pure result of measuring a book against the caps.
type budgetPlan struct {
	status     rpc.TradeProposalBudgetStatus
	lines      []budgetLine
	declared   float64
	perLineCap float64
	totalCap   float64
	total      float64
	// Rulebook basis: NLV and the premium at risk above the premium
	// budget's watch level once the total reached its act level.
	nlv    float64
	excess float64
	// ranked holds the sellable lines' indices in rank order; orders is the
	// plan, every order across cycles, one line's orders together.
	ranked []int
	orders []budgetPlanOrder
}

const budgetMoneyEpsilon = 1e-6

// budgetCandidateLimit is how many ranked lines the status lists.
const budgetCandidateLimit = 3

// budgetReductionPlan measures the book against the caps, then applies the
// review gate (amendment 2026-09-30): under basis rulebook, while the
// Rulebook policy file in force still carries Canary's defaults, the governor
// runs in shadow whatever its mode says, so an unreviewed default never
// generates an order. The mode stays the owner's; the gate asks only that
// the numbers the governor sells against be the owner's numbers.
func budgetReductionPlan(policy protectionPolicy, input budgetGovernorInput, pos *rpc.PositionsResult, now time.Time) budgetPlan {
	plan := budgetMeasurePlan(policy, input, pos, now)
	if policy.Buckets.BudgetReduction.basis() == rpc.BudgetBasisRulebook && input.RulebookReview == rpc.PolicyReviewUnreviewed {
		plan.status.Shadow, plan.status.ShadowReason = true, rpc.BudgetShadowRulebookUnreviewed
	}
	return plan
}

// budgetMeasurePlan measures the book against the caps. It generates no
// proposals; the gates it applies, in order, are the constitution (active and
// approved), its block enforcement class (advisory or stronger), the brake
// (latched or breached), and the base currency (the constitution's must be
// the account's).
func budgetMeasurePlan(policy protectionPolicy, input budgetGovernorInput, pos *rpc.PositionsResult, now time.Time) budgetPlan {
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
	budgetAnnotateLines(plan.lines, input, st)
	plan.ranked = budgetRank(plan.lines, budgetLineUnitValue)

	// Per line first: a line above its cap keeps floor(cap / unit) contracts
	// and sells the rest; a cap that rounds to zero contracts is a full close.
	// An ignored line stays over its cap but sells nothing.
	overLine := false
	for i := range plan.lines {
		line := &plan.lines[i]
		if line.unitBase <= 0 || line.valueBase <= plan.perLineCap+budgetMoneyEpsilon {
			continue
		}
		overLine = true
		if line.ignored {
			continue
		}
		keep := max(int(math.Floor(plan.perLineCap/line.unitBase+1e-9)), 0)
		line.perLineCut = max(line.contracts-keep, 1)
	}
	// Then the total: while the projected sum still exceeds the cap, sell
	// whole contracts from the lines in rank order.
	remaining := plan.total
	for _, line := range plan.lines {
		remaining -= float64(line.perLineCut) * line.unitBase
	}
	budgetTotalPass(plan.lines, plan.ranked, remaining-plan.totalCap, budgetLineUnitValue)
	budgetPlanOrders(&plan, bucket.MaxOrderNotional)
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

// budgetLineUnitValue is what one contract of a line adds to a total
// measured in value (the declared-risk-capital basis).
func budgetLineUnitValue(line budgetLine) float64 { return line.unitBase }

// budgetLineUnitAtRisk is what one contract of a line adds to a total
// measured as premium at risk, the higher of price paid and value (the
// Rulebook basis): a losing line sheds its price paid per contract sold.
func budgetLineUnitAtRisk(line budgetLine) float64 {
	if line.contracts <= 0 {
		return 0
	}
	return line.atRiskBase / float64(line.contracts)
}

// budgetReliefRules are the Rulebook rows whose offenders are lines or
// issuers, so a sale of the line relieves them: rules 1, 2, 4, 5, 13, 16 and
// 18. Rule 3 is the budget itself; the others have no line to sell.
var budgetReliefRules = []string{
	risk.RuleSingleNameExposure, risk.RuleOptionLinePremium, risk.RuleExtrinsicBudget, risk.RuleExpiryRunway,
	risk.RuleExitDiscipline, risk.RuleDeltaSwing, risk.RuleLossBudget,
}

// budgetAnnotateLines reads what the ranking needs for every measured line:
// the open rules it relieves from the Rulebook result the daemon holds (none
// held: every line relieves nothing and the status says
// ranking_without_rulebook), its time-value share, and whether the owner
// ignored its row.
func budgetAnnotateLines(lines []budgetLine, input budgetGovernorInput, st *rpc.TradeProposalBudgetStatus) {
	rules := input.Rules
	if rules != nil && len(rules.Rules) == 0 {
		rules = nil // an unavailable result carries no verdicts
	}
	st.RankingWithoutRulebook = rules == nil && len(lines) > 0
	for i := range lines {
		line := &lines[i]
		line.relief = budgetLineRelief(rules, input.Rulebook, line.row)
		line.timeValuePct = budgetTimeValuePct(line.row)
		line.ignored = input.Ignored != nil && input.Ignored(budgetRowKey(line.row))
	}
}

// budgetLineRelief lists the Rulebook rows at watch or act on which the line
// (its exact leg) or its issuer is a measured offender, in rulebook order.
func budgetLineRelief(rules *rpc.RulesResult, pol risk.RulebookPolicy, row rpc.PositionView) []budgetRelief {
	if rules == nil {
		return nil
	}
	leg, issuer := legDesc(row), pol.IssuerOf(row.Symbol)
	var out []budgetRelief
	for _, r := range rules.Rules {
		if (r.Status != risk.RuleStatusWatch && r.Status != risk.RuleStatusAct) || !slices.Contains(budgetReliefRules, r.ID) {
			continue
		}
		for _, o := range r.Offenders {
			if o.Status == risk.RuleStatusUnknown {
				continue // listed because it could not be measured, not an offender
			}
			if (o.Leg != "" && strings.EqualFold(o.Leg, leg)) || (o.Leg == "" && strings.EqualFold(strings.TrimSpace(o.Symbol), issuer)) {
				out = append(out, budgetRelief{id: r.ID, number: r.Number, observed: o.Observed})
				break
			}
		}
	}
	slices.SortFunc(out, func(a, b budgetRelief) int { return cmp.Compare(a.number, b.number) })
	return out
}

// budgetTimeValuePct is the line's extrinsic value as a percent of its value,
// from the shared decomposition the theta bucket and rule 4 use; nil when the
// underlying price or the mark is missing.
func budgetTimeValuePct(row rpc.PositionView) *float64 {
	mark := math.Abs(row.Mark)
	if mark <= 0 {
		mark = math.Abs(row.ValuationMark)
	}
	extrinsic, ok := risk.OptionExtrinsicPerShare(row.Right, row.Underlying, row.Strike, mark)
	if !ok {
		return nil
	}
	return new(extrinsic / mark * 100)
}

// budgetRowKey is the key a governor row for the line carries, so an ignore
// can be matched before the row exists.
func budgetRowKey(row rpc.PositionView) string {
	return proposalKey(rpc.TradeProposalBucketBudgetReduction, proposalContractFromPosition(row, positionWireSecType(row.SecType)), rpc.OrderActionSell)
}

// budgetRank orders the lines a sale can come from by what the sale fixes
// (amendment 2026-09-30): the most open Rulebook rules relieved first, then
// the largest time-value share (unknown last), then the largest unrealised
// loss, then the largest value, then the lower contract id. An ignored line,
// or one whose contracts count for nothing, is not ranked. It sets each
// ranked line's rank and returns the indices in rank order.
func budgetRank(lines []budgetLine, unit func(budgetLine) float64) []int {
	var order []int
	for i, line := range lines {
		if !line.ignored && unit(line) > 0 {
			order = append(order, i)
		}
	}
	slices.SortStableFunc(order, func(a, b int) int {
		la, lb := lines[a], lines[b]
		if c := cmp.Compare(len(lb.relief), len(la.relief)); c != 0 {
			return c
		}
		switch {
		case la.timeValuePct != nil && lb.timeValuePct == nil:
			return -1
		case la.timeValuePct == nil && lb.timeValuePct != nil:
			return 1
		case la.timeValuePct != nil:
			if c := cmp.Compare(*lb.timeValuePct, *la.timeValuePct); c != 0 {
				return c
			}
		}
		if c := cmp.Compare(lb.lossBase, la.lossBase); c != 0 {
			return c
		}
		if c := cmp.Compare(lb.valueBase, la.valueBase); c != 0 {
			return c
		}
		return cmp.Compare(la.row.ConID, lb.row.ConID)
	})
	for place, i := range order {
		lines[i].rank = place + 1
	}
	return order
}

// budgetTotalPass sells whole contracts from the lines in rank order until
// the excess is covered; unit says what one contract of a line counts toward
// it. Contracts the per-line pass already sold are not sold twice.
func budgetTotalPass(lines []budgetLine, order []int, excess float64, unit func(budgetLine) float64) {
	if excess <= budgetMoneyEpsilon {
		return
	}
	place := 0
	for _, i := range order {
		line := &lines[i]
		available := line.contracts - line.perLineCut
		per := unit(*line)
		if per <= 0 || available <= 0 {
			continue
		}
		take := min(int(math.Ceil(excess/per-1e-9)), available)
		if take <= 0 {
			continue
		}
		place++
		line.totalCut, line.order = take, place
		excess -= float64(take) * per
		if excess <= budgetMoneyEpsilon {
			return
		}
	}
}

// budgetRulebookPlan measures the book against the Rulebook's own limits as
// shares of NLV. A line whose premium at risk (the higher of price paid and
// value) exceeds option_line_act_pct is cut to it. When the book's premium at
// risk reaches rule 3's premium budget act level of the regime set in force
// (at or above, like rule 3), lines are sold in rank order until the
// total is back at that set's watch level, counting each contract at its
// premium at risk (rule 1's trim convention: act triggers, watch is the
// target). It needs the account's NLV, not the risk constitution or available
// funds, and waits for no brake.
func budgetRulebookPlan(policy protectionPolicy, input budgetGovernorInput, pos *rpc.PositionsResult, now time.Time) budgetPlan {
	mode := policy.Buckets.BudgetReduction.effectiveMode()
	rb := input.Rulebook
	watchPct, actPct, set := rb.PremiumBudgetInForce(input.RegimeStage, input.RegimeStageCarried)
	plan := budgetPlan{status: rpc.TradeProposalBudgetStatus{
		Mode: mode, Shadow: mode == rpc.BudgetReductionModeShadow, Basis: rpc.BudgetBasisRulebook,
		PerLinePctOfNLV: rb.OptionLineActPct, PremiumBudgetWatchPct: watchPct, PremiumBudgetActPct: actPct, PremiumBudgetSet: set,
		BaseCurrency: protectionCoverageBaseCurrency(pos),
	}}
	st := &plan.status
	if input.NLVBase == nil || *input.NLVBase <= 0 {
		st.State, st.Reason = rpc.BudgetStateAccountUnavailable, "the account's NLV is unavailable; the Rulebook limits are shares of NLV"
		return plan
	}
	if want, have := normCcy(input.AccountBaseCurrency), normCcy(st.BaseCurrency); want != "" && have != "" && want != have {
		st.State, st.Reason = rpc.BudgetStateCurrencyMismatch, fmt.Sprintf("the account reports NLV in %s and positions in %s; the limits cannot be compared", want, have)
		return plan
	}
	plan.nlv = *input.NLVBase
	st.NLVBase = new(plan.nlv)
	if input.AvailableFundsBase != nil {
		st.AvailableFundsBase = new(*input.AvailableFundsBase)
	}
	plan.perLineCap = rb.OptionLineActPct / 100 * plan.nlv
	plan.totalCap = watchPct / 100 * plan.nlv
	plan.lines, _ = budgetMeasureLines(policy, rb, pos, st, now)
	if st.IncludedLegs == 0 && st.ExcludedLegs > 0 {
		st.State, st.Reason = rpc.BudgetStateUnmeasurable, fmt.Sprintf("%d long option %s without a base market value; nothing can be measured", st.ExcludedLegs, pluralNoun(st.ExcludedLegs, "leg"))
		return plan
	}
	// The Rulebook's measure is premium at risk, not value: a losing line
	// counts at the price paid, so a fall in value frees no budget.
	for _, line := range plan.lines {
		plan.total += line.atRiskBase
	}
	st.MeasuredPremiumBase = new(plan.total)
	st.PremiumPctOfNLV = new(plan.total / plan.nlv * 100)
	budgetAnnotateLines(plan.lines, input, st)
	plan.ranked = budgetRank(plan.lines, budgetLineUnitAtRisk)

	overLine := false
	cut := 0.0
	for i := range plan.lines {
		line := &plan.lines[i]
		if line.contracts <= 0 || line.atRiskBase <= plan.perLineCap+budgetMoneyEpsilon {
			continue
		}
		overLine = true
		if line.ignored {
			continue
		}
		unitRisk := budgetLineUnitAtRisk(*line)
		keep := max(int(math.Floor(plan.perLineCap/unitRisk+1e-9)), 0)
		line.perLineCut = max(line.contracts-keep, 1)
		cut += float64(line.perLineCut) * unitRisk
	}
	if *st.PremiumPctOfNLV >= actPct {
		plan.excess = plan.total - plan.totalCap
		st.PremiumExcessBase = new(plan.excess)
		budgetTotalPass(plan.lines, plan.ranked, plan.excess-cut, budgetLineUnitAtRisk)
	}
	budgetPlanOrders(&plan, policy.Buckets.BudgetReduction.MaxOrderNotional)
	if overLine || plan.excess > budgetMoneyEpsilon {
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
// mode, and shadow mode adds shadow_mode in front of them, behind
// rulebook_unreviewed when the review gate holds the governor in shadow. An
// ignored row's line leaves the plan, which moves on to the next candidate.
func (e *proposalEngine) budgetReductionProposals(policy protectionPolicy, status rpc.ProtectionPolicyStatus, input budgetGovernorInput, acct *rpc.AccountResult, pos *rpc.PositionsResult, sources rpc.TradeProposalSourceFingerprints, marketEvents *rpc.MarketEventsResult, scope brokerStateScope, now time.Time) ([]rpc.TradeProposal, *rpc.TradeProposalBudgetStatus) {
	bucket := policy.Buckets.BudgetReduction
	if !bucket.enabled() {
		return nil, nil
	}
	if e != nil && input.Ignored == nil {
		input.Ignored = func(key string) bool { return e.isIgnored(scope, key) }
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
			// reader must know about the row, and the review gate comes before
			// the mode, because it holds the row whatever the mode says.
			var lead []rpc.TradingBlocker
			if st.ShadowReason == rpc.BudgetShadowRulebookUnreviewed {
				lead = append(lead, budgetRulebookUnreviewedBlocker(input.RulebookPath))
			}
			if st.Mode == rpc.BudgetReductionModeShadow {
				lead = append(lead, budgetShadowBlocker())
			}
			p.Blockers = append(lead, p.Blockers...)
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
	if limit := budgetOrderLimit(row, bucket.MaxOrderNotional); limit > 0 {
		qty = min(qty, limit)
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
		reason = fmt.Sprintf("premium line is %.1f%% of declared risk capital, above the %.1f%% per-line cap (%d %s), and total premium at risk is %.1f%%, above the %.1f%% cap (%s in the reduction order: %s; %d %s); %s",
			linePct, bucket.PerLinePctOfRiskCapital, line.perLineCut, pluralNoun(line.perLineCut, "contract"),
			totalPct, bucket.PremiumAtRiskPctOfRiskCapital, budgetOrdinal(line.order), budgetRankingText(plan), line.totalCut, pluralNoun(line.totalCut, "contract"),
			budgetSellClause(qty, cut, line.contracts, bucket.MaxOrderNotional, ""))
	case line.perLineCut > 0:
		budget.Cap = "per_line"
		reason = fmt.Sprintf("premium line is %.1f%% of declared risk capital, above the %.1f%% per-line cap; %s",
			linePct, bucket.PerLinePctOfRiskCapital, budgetSellClause(qty, cut, line.contracts, bucket.MaxOrderNotional, " to the cap"))
	default:
		budget.Cap = "total"
		reason = fmt.Sprintf("total premium at risk is %.1f%% of declared risk capital, above the %.1f%% cap; %s in the reduction order (%s): %s",
			totalPct, bucket.PremiumAtRiskPctOfRiskCapital, budgetOrdinal(line.order), budgetRankingText(plan), budgetSellClause(qty, cut, line.contracts, bucket.MaxOrderNotional, ""))
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
	details = append(details, budgetPlanDetails(plan, line)...)
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
	totalPct := plan.total / plan.nlv * 100
	watchPct, actPct, set := plan.status.PremiumBudgetWatchPct, plan.status.PremiumBudgetActPct, plan.status.PremiumBudgetSet
	budget := &rpc.TradeProposalBudget{
		Mode: plan.status.Mode, Basis: rpc.BudgetBasisRulebook,
		LineMarketValueBase: line.valueBase, LineAtRiskBase: line.atRiskBase, LinePctOfNLV: linePct,
		PerLinePctOfNLV:       plan.status.PerLinePctOfNLV,
		PremiumBudgetWatchPct: watchPct, PremiumBudgetActPct: actPct, PremiumPctOfNLV: totalPct,
		TotalMeasuredBase: plan.total, Order: line.order,
		ContractsPerLine: line.perLineCut, ContractsTotal: line.totalCut,
		UnrealizedPnLBase: line.pnlBase, BaseCurrency: base,
	}
	if line.perLineCut > 0 {
		budget.LineExcessBase = new(line.atRiskBase - plan.perLineCap)
	}
	if plan.excess > budgetMoneyEpsilon {
		budget.PremiumExcessBase = new(plan.excess)
	}
	budgetText := fmt.Sprintf("option premium at risk is %.1f%% of NLV, at or above the Rulebook's %s%% premium budget act level (%s set), so the total is cut back to its %s%% budget",
		totalPct, trimFloat(actPct), set, trimFloat(watchPct))
	var reason string
	switch {
	case line.perLineCut > 0 && line.totalCut > 0:
		budget.Cap = "per_line+total"
		reason = fmt.Sprintf("the line puts %.1f%% of NLV at risk, above the Rulebook's %.1f%% line limit (%d %s), and %s (%s in the reduction order: %s; %d %s); %s",
			linePct, plan.status.PerLinePctOfNLV, line.perLineCut, pluralNoun(line.perLineCut, "contract"),
			budgetText, budgetOrdinal(line.order), budgetRankingText(plan), line.totalCut, pluralNoun(line.totalCut, "contract"),
			budgetSellClause(qty, cut, line.contracts, bucket.MaxOrderNotional, ""))
	case line.perLineCut > 0:
		budget.Cap = "per_line"
		reason = fmt.Sprintf("the line puts %.1f%% of NLV at risk, above the Rulebook's %.1f%% line limit; %s",
			linePct, plan.status.PerLinePctOfNLV, budgetSellClause(qty, cut, line.contracts, bucket.MaxOrderNotional, " to the limit"))
	default:
		budget.Cap = "total"
		reason = fmt.Sprintf("%s; %s in the reduction order (%s): %s",
			budgetText, budgetOrdinal(line.order), budgetRankingText(plan), budgetSellClause(qty, cut, line.contracts, bucket.MaxOrderNotional, ""))
	}
	details := []string{
		fmt.Sprintf("line %s at risk (%.1f%% of NLV; the higher of price paid and value) · line limit %s (%.1f%% of %s NLV)",
			formatBudgetMoney(line.atRiskBase, base), linePct, formatBudgetMoney(plan.perLineCap, base), plan.status.PerLinePctOfNLV, formatBudgetMoney(plan.nlv, base)),
	}
	if plan.excess > budgetMoneyEpsilon {
		details = append(details, fmt.Sprintf("premium budget (%s set): %s at risk (%.1f%% of NLV) · act at %s%%, back to %s (%s%%) · %s over; each contract sold removes about %s at risk",
			set, formatBudgetMoney(plan.total, base), totalPct, trimFloat(actPct), formatBudgetMoney(plan.totalCap, base), trimFloat(watchPct),
			formatBudgetMoney(plan.excess, base), formatBudgetMoney(budgetLineUnitAtRisk(line), base)))
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
	details = append(details, budgetPlanDetails(plan, line)...)
	details = append(details, "waits the full veto window even under a latched brake: a reduction to budget is a discretionary-scale action, not a stop")

	p := baseProposal(policy, status, sources, now, rpc.TradeProposalBucketBudgetReduction, line.row, rpc.OrderActionSell, qty, effect, reason)
	p.Budget = budget
	p.Details = details
	p.Score = float64(cut) * line.unitBase
	p.Shadow = plan.status.Shadow
	p.NeverSkipVeto = true
	return p
}

// budgetSellClause states the order the row actually carries. The plan's cut
// can exceed what one order may sell: max_order_notional holds qty below it,
// and the reason then names both numbers and the limit, so the sentence never
// disagrees with the row's quantity. tail finishes an unheld clause (" to the
// cap"); a held order does not reach the cap, so it drops the tail.
func budgetSellClause(qty, cut, contracts int, maxOrderNotional float64, tail string) string {
	if qty >= cut {
		return fmt.Sprintf("sell %d of %d %s%s", qty, contracts, pluralNoun(contracts, "contract"), tail)
	}
	return fmt.Sprintf("sell %d of %d %s now (the plan calls for %d; max_order_notional %.0f holds one order to %d, and the next cycle measures the rest)",
		qty, contracts, pluralNoun(contracts, "contract"), cut, maxOrderNotional, qty)
}

// budgetOrderLimit is the most contracts one order may sell under
// max_order_notional at the line's mark, exactly as risk_reduction bounds its
// orders and never below one; zero when nothing bounds it.
func budgetOrderLimit(row rpc.PositionView, maxOrderNotional float64) int {
	mark := math.Abs(row.Mark)
	if mark <= 0 {
		mark = math.Abs(row.ValuationMark)
	}
	if mark <= 0 || maxOrderNotional <= 0 {
		return 0
	}
	return int(math.Max(1, math.Floor(maxOrderNotional/(mark*float64(max(row.Multiplier, 1))))))
}

// budgetPlanOrders lays out the whole fix (amendment 2026-09-30): the ranked
// lines' cuts in rank order, each split into orders of at most what
// max_order_notional allows. A line's first order is this cycle's row; the
// rest follow one cycle at a time, each re-measured from the position as it
// then is. The status carries the plan and the first three candidates.
func budgetPlanOrders(plan *budgetPlan, maxOrderNotional float64) {
	st := &plan.status
	for n, i := range plan.ranked {
		line := plan.lines[i]
		if n < budgetCandidateLimit {
			c := rpc.TradeProposalBudgetCandidate{
				Rank: line.rank, Contract: budgetDisplayContract(line.row), Contracts: line.contracts, UnitValueBase: line.unitBase,
				TimeValuePct: cloneFloat64Ptr(line.timeValuePct), UnrealizedPnLBase: cloneFloat64Ptr(line.pnlBase),
				Why: budgetWhy(line, st.RankingWithoutRulebook),
			}
			for _, r := range line.relief {
				c.Relief = append(c.Relief, r.id)
			}
			st.Candidates = append(st.Candidates, c)
		}
		limit := budgetOrderLimit(line.row, maxOrderNotional)
		for left, cycle := min(line.perLineCut+line.totalCut, line.contracts), 1; left > 0; cycle++ {
			contracts := left
			if limit > 0 {
				contracts = min(contracts, limit)
			}
			plan.orders = append(plan.orders, budgetPlanOrder{line: i, contracts: contracts, cycle: cycle})
			st.Plan = append(st.Plan, rpc.TradeProposalBudgetPlanOrder{
				Rank: line.rank, Contract: budgetDisplayContract(line.row), Contracts: contracts,
				RaisesBase: float64(contracts) * line.unitBase, Cycle: cycle,
			})
			left -= contracts
		}
	}
}

// cloneBudgetCandidates deep-copies the status candidates for a snapshot copy.
func cloneBudgetCandidates(in []rpc.TradeProposalBudgetCandidate) []rpc.TradeProposalBudgetCandidate {
	out := slices.Clone(in)
	for i := range out {
		out[i].Relief = slices.Clone(in[i].Relief)
		out[i].TimeValuePct = cloneFloat64Ptr(in[i].TimeValuePct)
		out[i].UnrealizedPnLBase = cloneFloat64Ptr(in[i].UnrealizedPnLBase)
	}
	return out
}

// budgetDisplayContract is the contract as the status names it.
func budgetDisplayContract(row rpc.PositionView) rpc.ContractParams {
	return rpc.ContractParams{ConID: row.ConID, Symbol: strings.ToUpper(strings.TrimSpace(row.Symbol)), SecType: positionWireSecType(row.SecType),
		Expiry: row.Expiry, Strike: row.Strike, Right: row.Right}
}

// budgetRankingText names the order the total pass used.
func budgetRankingText(plan budgetPlan) string {
	if plan.status.RankingWithoutRulebook {
		return "no current Rulebook result, so most time value first, then largest loss"
	}
	return "most open Rulebook rules relieved first, then most time value, then largest loss"
}

// budgetWhy is a line's ranking in one line: the open rules it offends, its
// time-value share and its unrealised P&L.
func budgetWhy(line budgetLine, withoutRulebook bool) string {
	var parts []string
	switch n := len(line.relief); {
	case withoutRulebook:
		parts = append(parts, "no Rulebook result")
	case n == 0:
		parts = append(parts, "offends no open rule")
	default:
		parts = append(parts, fmt.Sprintf("offends %d open %s", n, pluralNoun(n, "rule")))
	}
	if line.timeValuePct != nil {
		parts = append(parts, fmt.Sprintf("%.0f%% time value", *line.timeValuePct))
	} else {
		parts = append(parts, "time value unknown")
	}
	if line.pnlBase != nil {
		parts = append(parts, "unrealised "+budgetCompactMoney(*line.pnlBase))
	} else {
		parts = append(parts, "unrealised unknown")
	}
	return strings.Join(parts, "; ")
}

// budgetCompactMoney writes a signed amount in thousands or millions:
// −2.1k, +850, 1.2M.
func budgetCompactMoney(v float64) string {
	sign := ""
	switch {
	case v < 0:
		sign = "−"
	case v > 0:
		sign = "+"
	}
	switch a := math.Abs(v); {
	case a >= 1e6:
		return fmt.Sprintf("%s%.1fM", sign, a/1e6)
	case a >= 1e3:
		return fmt.Sprintf("%s%.1fk", sign, a/1e3)
	default:
		return fmt.Sprintf("%s%.0f", sign, a)
	}
}

// budgetPlanDetails are the three lines every governor row carries about the
// whole fix: its place in the plan, the other open rules the sale relieves,
// and the next two candidates, so the owner sees the choice.
func budgetPlanDetails(plan budgetPlan, line budgetLine) []string {
	var out []string
	total := len(plan.orders)
	if at := slices.IndexFunc(plan.orders, func(o budgetPlanOrder) bool { return plan.lines[o.line].rank == line.rank }); at >= 0 {
		place := fmt.Sprintf("order %d of %d in the plan", at+1, total)
		if at+1 < total {
			next := plan.orders[at+1]
			if nextLine := plan.lines[next.line]; nextLine.rank == line.rank {
				place += fmt.Sprintf("; order %d of %d (%d %s of the same line) follows after this fill", at+2, total, next.contracts, pluralNoun(next.contracts, "contract"))
			} else {
				place += fmt.Sprintf("; order %d of %d (%d %s of %s) is the next line", at+2, total, next.contracts, pluralNoun(next.contracts, "contract"), legDesc(nextLine.row))
			}
		} else {
			place += ", the last"
		}
		out = append(out, place)
	}
	if plan.status.RankingWithoutRulebook {
		out = append(out, "relief unknown: Canary holds no current Rulebook result, so the ranking reads time value, then loss")
	} else {
		var named []string
		for _, r := range line.relief {
			if r.id == risk.RuleOptionLinePremium && plan.status.Basis == rpc.BudgetBasisRulebook && line.perLineCut > 0 {
				continue // rule 2's act level is this row's own reason
			}
			named = append(named, budgetReliefText(r))
		}
		if len(named) == 0 {
			out = append(out, "relieves no other open Rulebook rule")
		} else {
			out = append(out, "also relieves "+strings.Join(named, ", "))
		}
	}
	var alternatives []string
	if at := slices.IndexFunc(plan.ranked, func(i int) bool { return plan.lines[i].rank == line.rank }); at >= 0 {
		for _, i := range plan.ranked[at+1 : min(at+3, len(plan.ranked))] {
			alt := plan.lines[i]
			alternatives = append(alternatives, fmt.Sprintf("%s %s (%s)", budgetOrdinal(alt.rank), legDesc(alt.row), budgetWhy(alt, plan.status.RankingWithoutRulebook)))
		}
	}
	if len(alternatives) == 0 {
		out = append(out, "alternatives: none, no other line can be sold")
	} else {
		out = append(out, "alternatives: "+strings.Join(alternatives, "; "))
	}
	return out
}

// budgetReliefText names one relieved rule with the line's or its issuer's
// observed value on it.
func budgetReliefText(r budgetRelief) string {
	switch r.id {
	case risk.RuleSingleNameExposure:
		return fmt.Sprintf("rule 1 (issuer worst case %.1f%% of NLV)", r.observed)
	case risk.RuleOptionLinePremium:
		return fmt.Sprintf("rule 2 (line %.1f%% of NLV)", r.observed)
	case risk.RuleExpiryRunway:
		return fmt.Sprintf("rule 5 (%.0f DTE)", r.observed)
	case risk.RuleExitDiscipline:
		return fmt.Sprintf("rule 13 (%.0f%% of premium lost)", r.observed)
	case risk.RuleDeltaSwing:
		return fmt.Sprintf("rule 16 (delta %.1f%% of NLV)", r.observed)
	case risk.RuleLossBudget:
		if r.observed > 0 {
			return fmt.Sprintf("rule 18 (%.1f%% of risk capital)", r.observed)
		}
	}
	return fmt.Sprintf("rule %d", r.number)
}

func budgetShadowBlocker() rpc.TradingBlocker {
	return rpc.TradingBlocker{
		Code:    "shadow_mode",
		Message: "budget reduction runs in shadow mode; this row is listed for observation and cannot be previewed or submitted",
		Action:  "Set mode = \"active\" under [buckets.budget_reduction] and bump policy_version to make these rows ordinary proposals.",
	}
}

// budgetRulebookUnreviewedBlocker is the review gate's refusal: the first
// blocker on every governor row while the Rulebook policy file it sells
// against is Canary's unreviewed default. path is the file in force.
func budgetRulebookUnreviewedBlocker(path string) rpc.TradingBlocker {
	file := config.DefaultRulebookPolicyFile
	if path = strings.TrimSpace(path); path != "" && path != expandUserPath(file) {
		file = path
	}
	return rpc.TradingBlocker{
		Code:    rpc.BudgetShadowRulebookUnreviewed,
		Message: "the Rulebook policy file still carries Canary's defaults, not yet reviewed",
		Action:  "read " + file + ", set the limits you have decided, then delete its first line",
	}
}

// shadowProposalBlockers is the preview and submit refusal for a shadow row.
// It reads the flag, not the bucket, so any future shadow-mode bucket is
// refused the same way; a row the review gate holds is refused with the
// gate's own blocker, which names what lifts it.
func shadowProposalBlockers(p rpc.TradeProposal) []rpc.TradingBlocker {
	if i := slices.IndexFunc(p.Blockers, func(b rpc.TradingBlocker) bool { return b.Code == rpc.BudgetShadowRulebookUnreviewed }); p.Shadow && i >= 0 {
		return []rpc.TradingBlocker{p.Blockers[i]}
	}
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
