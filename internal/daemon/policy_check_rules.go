package daemon

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// policyCheckHit is one finding a catalogue entry returns; the entry supplies
// its id, severity and category.
type policyCheckHit struct {
	keys       []rpc.PolicyCheckKey
	message    string
	suggestion string
}

// policyCheckRule is one catalogue entry.
type policyCheckRule struct {
	id       string
	severity string
	category string
	// needsBook skips the entry when no live account is available.
	needsBook bool
	// summary is the one-line description the docs list.
	summary string
	run     func(*policyCheckContext) []policyCheckHit
}

// Plausibility bounds. They are this check's judgement, not limits: nothing
// enforces them.
const (
	// A per-order cap or a cash reserve below this share of NLV is tiny.
	policyCheckTinyShareNLV = 0.02
	// A per-order cap above this share of NLV is huge.
	policyCheckHugeShareNLV = 0.50
	// A planned reduction split into more orders than this is implausible.
	policyCheckMaxSplitOrders = 5
	// A cap within this share of the trading cap, sized in another currency,
	// leaves no room for the gate's own FX conversion.
	policyCheckFXHeadroom = 0.02
	// A dated assumption that ends within this window is reported.
	policyCheckExpiringWithin = 14 * 24 * time.Hour
)

// policyCheckBillAssumption is the economics of one bill currency the check
// assumes, because no policy or config key carries it.
type policyCheckBillAssumption struct {
	yield         float64 // annual, decimal
	minCommission float64 // per order, in the bill's currency
	pctCommission float64 // of face, decimal
	note          string
}

// policyCheckBillAssumptions are deliberately conservative: yields below the
// recent bill rates and the broker's published minimum ticket. Outside USD
// the percentage fee is not modelled, so the cost is understated and a
// warning stands even then.
var policyCheckBillAssumptions = map[string]policyCheckBillAssumption{
	"USD": {yield: 0.035, minCommission: 5, pctCommission: 0.00002, note: "US Treasury bill: assumed yield 3.5% a year; assumed commission 0.002% of face, at least 5 USD per order"},
	"EUR": {yield: 0.0175, minCommission: 5, note: "EUR bill: assumed yield 1.75% a year; assumed commission at least 5 EUR per order (percentage fee not modelled)"},
	"GBP": {yield: 0.035, minCommission: 5, note: "UK Treasury bill: assumed yield 3.5% a year; assumed commission at least 5 GBP per order (percentage fee not modelled)"},
	"CAD": {yield: 0.0225, minCommission: 5, note: "Canadian Treasury bill: assumed yield 2.25% a year; assumed commission at least 5 CAD per order (percentage fee not modelled)"},
}

// policyCheckCatalogue is every plausibility rule, in report order within a
// severity. A new rule is one entry.
var policyCheckCatalogue = []policyCheckRule{
	{id: "file_refused", severity: rpc.PolicyCheckError, category: rpc.PolicyCheckCategoryProvenance,
		summary: "Canary's loader refuses a policy file, so the previous policy or Canary's defaults stay in force.", run: checkFileRefused},
	{id: "order_limits_missing", severity: rpc.PolicyCheckError, category: rpc.PolicyCheckCategoryProvenance,
		summary: "The risk constitution does not write every [order_limits] key, so the trading gate refuses every order preview.", run: checkOrderLimitsMissing},
	{id: "cap_above_trading_max", severity: rpc.PolicyCheckError, category: rpc.PolicyCheckCategoryContradiction,
		summary: "A bucket's per-order cap lets an order exceed the order cap in force ([order_limits]), which the gate always refuses, unless a documented exemption covers it.", run: checkCapAboveTradingMax},
	{id: "sweep_minimum_above_cap", severity: rpc.PolicyCheckError, category: rpc.PolicyCheckCategoryContradiction,
		summary: "A cash sweep currency's smallest buy (min_order_notional, or a retired min_tranche) is above the sweep's cap in force or the trading cap, so no order can satisfy both.", run: checkSweepMinimumAboveCap},
	{id: "watch_act_inverted", severity: rpc.PolicyCheckError, category: rpc.PolicyCheckCategoryContradiction,
		summary: "A watch level sits beyond its act level (or a minimum above its maximum), per regime set and in the constitution's drawdown ladder.", run: checkWatchActInverted},
	{id: "regime_loosens_under_stress", severity: rpc.PolicyCheckError, category: rpc.PolicyCheckCategoryContradiction,
		summary: "A regime-conditional limit is looser in a worse regime than in a calmer one.", run: checkRegimeLoosens},
	{id: "order_entry_off_for_active_bucket", severity: rpc.PolicyCheckError, category: rpc.PolicyCheckCategoryContradiction,
		summary: "A bucket is active or pre-authorised while [trading].mode disables order entry, so its orders can never be placed.", run: checkOrderEntryOff},
	{id: "settlement_route_expired", severity: rpc.PolicyCheckError, category: rpc.PolicyCheckCategoryContradiction,
		summary: "A sweep currency's settlement_valid_through has passed, so every bill order in it holds.", run: checkSettlementRouteExpired},
	{id: "base_currency_mismatch", severity: rpc.PolicyCheckError, category: rpc.PolicyCheckCategoryBook, needsBook: true,
		summary: "The constitution's base_currency differs from the account's, so capital math refuses every observation.", run: checkBaseCurrencyMismatch},
	{id: "lot_above_trading_max", severity: rpc.PolicyCheckError, category: rpc.PolicyCheckCategoryBook, needsBook: true,
		summary: "One unit of a held line is worth more than the order cap in force, so no reduction or exit order for it can pass the gate.", run: checkLotAboveTradingMax},
	{id: "cap_without_fx_headroom", severity: rpc.PolicyCheckWarn, category: rpc.PolicyCheckCategoryContradiction,
		summary: "A cap sized in another currency sits within 2% of the order cap in force, so an FX move makes the gate refuse an order sized at the cap.", run: checkCapFXHeadroom},
	{id: "order_cap_vs_nlv", severity: rpc.PolicyCheckWarn, category: rpc.PolicyCheckCategoryBook, needsBook: true,
		summary: "A per-order cap is under 2% or over 50% of NLV.", run: checkOrderCapVsNLV},
	{id: "order_cap_splits_reduction", severity: rpc.PolicyCheckWarn, category: rpc.PolicyCheckCategoryBook, needsBook: true,
		summary: "A per-order cap splits a planned reduction or a whole-line exit into more than 5 orders.", run: checkOrderCapSplits},
	{id: "cash_reserve_vs_nlv", severity: rpc.PolicyCheckWarn, category: rpc.PolicyCheckCategoryBook, needsBook: true,
		summary: "The cash the sweep keeps back is under 2% or over 50% of NLV.", run: checkCashReserveVsNLV},
	{id: "protected_floor_vs_equity", severity: rpc.PolicyCheckWarn, category: rpc.PolicyCheckCategoryBook, needsBook: true,
		summary: "The protected floor leaves less than the declared risk capital above it, or sits at or above equity.", run: checkProtectedFloor},
	{id: "declared_risk_vs_nlv", severity: rpc.PolicyCheckWarn, category: rpc.PolicyCheckCategoryBook, needsBook: true,
		summary: "Declared risk capital is above NLV or under 2% of it.", run: checkDeclaredRisk},
	{id: "sweep_buys_while_borrowed", severity: rpc.PolicyCheckWarn, category: rpc.PolicyCheckCategoryBook, needsBook: true,
		summary: "no_buy_while_borrowed is false while a currency's cash is negative, so the sweep may buy bills while the account pays margin interest.", run: checkSweepBuysWhileBorrowed},
	{id: "leveling_debit_inside_band", severity: rpc.PolicyCheckWarn, category: rpc.PolicyCheckCategoryBook, needsBook: true,
		summary: "A currency is borrowed within currency leveling's trigger_base, so leveling leaves the margin loan in place while no_buy_while_borrowed may hold the sweep's bill buys.", run: checkLevelingDebitInsideBand},
	{id: "sweep_minimum_uneconomic", severity: rpc.PolicyCheckWarn, category: rpc.PolicyCheckCategoryEconomics,
		summary: "The sweep's minimum bill order earns less interest to the shortest rung than the commission it pays.", run: checkSweepEconomics},
	{id: "retired_trading_gate", severity: rpc.PolicyCheckWarn, category: rpc.PolicyCheckCategoryProvenance,
		summary: "config.toml [trading] still carries a retired order gate whose value differs from [order_limits], which decides.", run: checkRetiredTradingGates},
	{id: "version_not_bumped", severity: rpc.PolicyCheckWarn, category: rpc.PolicyCheckCategoryProvenance,
		summary: "A policy file was edited without a higher policy_version, so the daemon keeps the old policy in force.", run: checkVersionNotBumped},
	{id: "dated_assumption_expired", severity: rpc.PolicyCheckWarn, category: rpc.PolicyCheckCategoryProvenance,
		summary: "A dated assumption has expired: cash_interest_valid_through, or a directional intent's expires_at.", run: checkDatedExpired},
	{id: "dated_assumption_expiring", severity: rpc.PolicyCheckInfo, category: rpc.PolicyCheckCategoryProvenance,
		summary: "A dated assumption ends within 14 days.", run: checkDatedExpiring},
	{id: "file_unreviewed", severity: rpc.PolicyCheckInfo, category: rpc.PolicyCheckCategoryProvenance,
		summary: "A policy file still carries the \"Canary defaults, not yet reviewed\" header.", run: checkFileUnreviewed},
	{id: "compiled_default_in_force", severity: rpc.PolicyCheckInfo, category: rpc.PolicyCheckCategoryProvenance,
		summary: "A sweep sizing number is not written, so the sweep holds.", run: checkCompiledDefaults},
	{id: "sweep_cap_exempt", severity: rpc.PolicyCheckInfo, category: rpc.PolicyCheckCategoryContradiction,
		summary: "bills_exempt_from_trading_max_notional makes a sweep cap above the order cap in force legitimate; reported so the exemption stays visible.", run: checkSweepExempt},
}

// PolicyCheckRuleIDs lists the catalogue's rule ids in order.
func PolicyCheckRuleIDs() []string {
	out := make([]string, 0, len(policyCheckCatalogue))
	for _, r := range policyCheckCatalogue {
		out = append(out, r.id)
	}
	return out
}

func checkFileRefused(c *policyCheckContext) []policyCheckHit {
	var out []policyCheckHit
	for _, src := range c.sources() {
		if src.refused == "" {
			continue
		}
		out = append(out, policyCheckHit{
			keys:    []rpc.PolicyCheckKey{{File: src.label, Key: "(whole file)", Value: src.path}},
			message: fmt.Sprintf("Canary refuses %s (%s), so the policy that was in force before, or Canary's defaults, keeps running and none of your edits apply.", src.label, src.refused),
		})
	}
	return out
}

// policyCheckCap is one bucket's per-order cap, in base currency.
type policyCheckCap struct {
	bucket string
	keys   []rpc.PolicyCheckKey
	// native is the written cap and nativeCcy its currency ("" for the
	// contract currency, unknown without a book).
	native    float64
	nativeCcy string
	base      float64
	// fx is base per unit of the currency the order is sized in; ccy names it.
	fx  float64
	ccy string
	// assumedBase is set when the order currency is unknown and the cap was
	// read as base currency.
	assumedBase bool
	// kind is the line kind the bucket sells (stock, option) or bill.
	kind string
}

// worstFX returns the highest base-per-unit rate among held lines of kind,
// the currency it belongs to, and whether a book supplied it.
func (c *policyCheckContext) worstFX(kind string) (float64, string, bool) {
	if c.book == nil || !c.book.PositionsKnown {
		return 1, c.base(), false
	}
	best, ccy, found := 0.0, "", false
	for _, p := range c.book.Positions {
		if p.Kind != kind {
			continue
		}
		if r, ok := c.fx(p.Currency); ok && r > best {
			best, ccy, found = r, p.Currency, true
		}
	}
	if !found {
		return 1, c.base(), false
	}
	return best, ccy, true
}

// bucketCaps lists the per-order caps of every enabled selling or buying
// bucket, converted to base currency.
func (c *policyCheckContext) bucketCaps() []policyCheckCap {
	var out []policyCheckCap
	contract := func(bucket string, v float64, kind string) {
		fx, ccy, known := c.worstFX(kind)
		out = append(out, policyCheckCap{bucket: bucket, native: v, nativeCcy: ccy, base: v * fx, fx: fx, ccy: ccy, assumedBase: !known, kind: kind,
			keys: []rpc.PolicyCheckKey{c.protectionKey("buckets."+bucket, "max_order_notional", policyCheckNumber(v)+" (contract currency)")}})
	}
	if rr := c.protection.Buckets.RiskReduction; rr.Enabled && rr.MaxOrderNotional > 0 {
		contract("risk_reduction", rr.MaxOrderNotional, policyCheckKindStock)
	}
	if b := c.protection.Buckets.BudgetReduction; b.enabled() && b.MaxOrderNotional > 0 {
		contract("budget_reduction", b.MaxOrderNotional, policyCheckKindOption)
	}
	if capBase, keys, ok := c.sweepCapBase(); ok {
		out = append(out, policyCheckCap{bucket: "cash_sweep", keys: keys, native: capBase, nativeCcy: c.base(), base: capBase, fx: 1, ccy: c.base(), kind: "bill"})
	}
	return out
}

func checkCapAboveTradingMax(c *policyCheckContext) []policyCheckHit {
	tradingCap, ok := c.orderCap()
	if !ok {
		return nil
	}
	var out []policyCheckHit
	for _, cp := range c.bucketCaps() {
		if cp.bucket == "cash_sweep" && c.sweepExempt() {
			continue
		}
		if cp.base <= tradingCap*(1+1e-9) {
			continue
		}
		keys := append(slices.Clone(cp.keys), c.tradingCapKey())
		where := ""
		if cp.assumedBase {
			where = " (read as base currency: no held line shows the order currency)"
		} else if cp.ccy != c.base() {
			where = fmt.Sprintf(" (%s at %s per %s)", policyCheckMoney(cp.native, cp.ccy), policyCheckNumber(cp.fx), cp.ccy)
		}
		suggest := policyCheckRoundDown(tradingCap / cp.fx)
		suggestion := fmt.Sprintf("Set the cap to %s, the order cap in force in the order's currency rounded down, so every order it sizes can pass the gate; or raise [order_limits] max_order_floor_base or max_order_pct_nlv yourself if larger orders are intended.",
			policyCheckMoney(suggest, cp.nativeCcy))
		if cp.bucket == "cash_sweep" {
			bound := c.sweepCapBound(cp.base)
			suggestion = fmt.Sprintf("Either declare bills_exempt_from_trading_max_notional = true, which lets bill orders pass the order cap in force up to the sweep's own cap, or bring the cap in force (set by %s) down to %s.", bound, policyCheckMoney(suggest, c.base()))
			if bound == "max_order_pct_nlv" && c.book != nil {
				suggestion = fmt.Sprintf("Either declare bills_exempt_from_trading_max_notional = true, which lets bill orders pass the order cap in force up to the sweep's own cap, or lower max_order_pct_nlv to %s and max_order_notional to at most %s.",
					policyCheckNumber(math.Floor(tradingCap/c.book.NetLiquidation*1000)/10), policyCheckMoney(suggest, c.base()))
			}
		}
		out = append(out, policyCheckHit{keys: keys,
			message: fmt.Sprintf("[buckets.%s] lets one order reach %s%s, above the order cap in force of %s: Canary lists such an order as ready and the trading gate refuses it every time.",
				cp.bucket, policyCheckMoney(cp.base, c.base()), where, policyCheckMoney(tradingCap, c.base())),
			suggestion: suggestion})
	}
	return out
}

func checkSweepMinimumAboveCap(c *policyCheckContext) []policyCheckHit {
	if c.sweep() == nil {
		return nil
	}
	capBase, capKeys, capOK := c.sweepCapBase()
	var out []policyCheckHit
	for _, ccy := range c.sweepCurrencies() {
		m, ok := c.sweepMinimum(ccy)
		if !ok {
			continue
		}
		fix := func(limitBase float64) string {
			if m.binding == "min_tranche" {
				return fmt.Sprintf("Delete the retired min_tranche for %s (min_order_notional sets the smallest buy now), or lower it to at most %s.", ccy, policyCheckMoney(policyCheckRoundDown(limitBase/m.fx), ccy))
			}
			return fmt.Sprintf("Lower min_order_notional to at most %s.", policyCheckMoney(policyCheckRoundDown(limitBase), c.base()))
		}
		if capOK && m.base > capBase*(1+1e-9) {
			out = append(out, policyCheckHit{keys: append(slices.Clone(m.keys), capKeys...),
				message: fmt.Sprintf("The smallest %s sweep buy is %s (%s, from %s) but the sweep's cap in force is %s: no %s order can be both, so the sweep holds every buy.",
					ccy, policyCheckMoney(m.native, ccy), policyCheckMoney(m.base, c.base()), m.binding, policyCheckMoney(capBase, c.base()), ccy),
				suggestion: fix(capBase) + fmt.Sprintf(" Or raise the cap above %s.", policyCheckMoney(m.base, c.base()))})
		}
		if tradingCap, ok := c.orderCap(); ok && !c.sweepExempt() && m.base > tradingCap*(1+1e-9) {
			out = append(out, policyCheckHit{keys: append(slices.Clone(m.keys), c.tradingCapKey()),
				message: fmt.Sprintf("The smallest %s sweep buy is %s (%s, from %s), above the order cap in force of %s, and bill orders are not exempt: the gate refuses every %s sweep buy.",
					ccy, policyCheckMoney(m.native, ccy), policyCheckMoney(m.base, c.base()), m.binding, policyCheckMoney(tradingCap, c.base()), ccy),
				suggestion: fix(tradingCap) + " Or declare bills_exempt_from_trading_max_notional = true."})
		}
	}
	return out
}

// policyCheckPair is a watch/act (or min/max) pair. below marks a measure
// that alarms when it falls: its act level must not exceed its watch level.
type policyCheckPair struct {
	low, high   string
	lowV, highV float64
	below       bool
}

func checkWatchActInverted(c *policyCheckContext) []policyCheckHit {
	p := c.rulebook
	pairs := []policyCheckPair{
		{"single_name_watch_pct", "single_name_act_pct", p.SingleNameWatchPct, p.SingleNameActPct, false},
		{"illiquid_watch_pct", "illiquid_act_pct", p.IlliquidWatchPct, p.IlliquidActPct, false},
		{"option_line_watch_pct", "option_line_act_pct", p.OptionLineWatchPct, p.OptionLineActPct, false},
		{"hedge_line_watch_pct", "hedge_line_act_pct", p.HedgeLineWatchPct, p.HedgeLineActPct, false},
		{"exit_watch_loss_pct", "exit_act_loss_pct", p.ExitWatchLossPct, p.ExitActLossPct, false},
		{"margin_headroom_act_pct", "margin_headroom_watch_pct", p.MarginHeadroomActPct, p.MarginHeadroomWatchPct, true},
		{"runway_act_dte", "runway_watch_dte", float64(p.RunwayActDTE), float64(p.RunwayWatchDTE), true},
	}
	for _, set := range rulebookRegimeSets(p) {
		pairs = append(pairs,
			policyCheckPair{set.name + ".premium_budget_watch_pct", set.name + ".premium_budget_act_pct", set.t.PremiumBudgetWatchPct, set.t.PremiumBudgetActPct, false},
			policyCheckPair{set.name + ".extrinsic_watch_pct", set.name + ".extrinsic_act_pct", set.t.ExtrinsicWatchPct, set.t.ExtrinsicActPct, false},
			policyCheckPair{set.name + ".net_exposure_watch_pct", set.name + ".net_exposure_act_pct", set.t.NetExposureWatchPct, set.t.NetExposureActPct, false},
			policyCheckPair{set.name + ".hedge_band_min_pct", set.name + ".hedge_band_max_pct", set.t.HedgeBandMinPct, set.t.HedgeBandMaxPct, false},
		)
	}
	var out []policyCheckHit
	for _, pr := range pairs {
		if pr.lowV <= pr.highV {
			continue
		}
		keys := []rpc.PolicyCheckKey{c.rulebookKey(pr.low, pr.lowV), c.rulebookKey(pr.high, pr.highV)}
		msg := fmt.Sprintf("%s (%s) is above %s (%s): the act level fires before the watch level ever warns, so the warning step is lost.", pr.low, policyCheckNumber(pr.lowV), pr.high, policyCheckNumber(pr.highV))
		if pr.below {
			msg = fmt.Sprintf("%s (%s) is above %s (%s): this measure alarms as it falls, so the act level fires first and the watch level never warns.", pr.low, policyCheckNumber(pr.lowV), pr.high, policyCheckNumber(pr.highV))
		}
		if strings.HasSuffix(pr.low, "_min_pct") {
			msg = fmt.Sprintf("%s (%s) is above %s (%s): no hedge size lies inside the band.", pr.low, policyCheckNumber(pr.lowV), pr.high, policyCheckNumber(pr.highV))
		}
		out = append(out, policyCheckHit{keys: keys, message: msg,
			suggestion: fmt.Sprintf("Swap them: %s = %s and %s = %s; Canary refuses the file until the order is right.", pr.low, policyCheckNumber(pr.highV), pr.high, policyCheckNumber(pr.lowV))})
	}
	if k := c.constitution; k != nil && k.Drawdown.WarnConsumedPct != nil && k.Drawdown.BlockConsumedPct != nil && *k.Drawdown.WarnConsumedPct > *k.Drawdown.BlockConsumedPct {
		w, b := *k.Drawdown.WarnConsumedPct, *k.Drawdown.BlockConsumedPct
		out = append(out, policyCheckHit{
			keys:       []rpc.PolicyCheckKey{c.constitutionKey("drawdown", "warn_consumed_pct", policyCheckNumber(w)), c.constitutionKey("drawdown", "block_consumed_pct", policyCheckNumber(b))},
			message:    fmt.Sprintf("The drawdown warn level (%s%% consumed) is above the block level (%s%%): the brake latches before you are ever warned.", policyCheckNumber(w), policyCheckNumber(b)),
			suggestion: fmt.Sprintf("Swap them: warn_consumed_pct = %s and block_consumed_pct = %s.", policyCheckNumber(b), policyCheckNumber(w))})
	}
	return out
}

// policyCheckRegimeMeasures are the regime-conditional limits. tightens is
// the direction a worse regime must move them: down for budgets, up for the
// protection band.
var policyCheckRegimeMeasures = []struct {
	key   string
	value func(risk.RegimeThresholds) float64
	up    bool
}{
	{"premium_budget_watch_pct", func(t risk.RegimeThresholds) float64 { return t.PremiumBudgetWatchPct }, false},
	{"premium_budget_act_pct", func(t risk.RegimeThresholds) float64 { return t.PremiumBudgetActPct }, false},
	{"extrinsic_watch_pct", func(t risk.RegimeThresholds) float64 { return t.ExtrinsicWatchPct }, false},
	{"extrinsic_act_pct", func(t risk.RegimeThresholds) float64 { return t.ExtrinsicActPct }, false},
	{"net_exposure_watch_pct", func(t risk.RegimeThresholds) float64 { return t.NetExposureWatchPct }, false},
	{"net_exposure_act_pct", func(t risk.RegimeThresholds) float64 { return t.NetExposureActPct }, false},
	{"hedge_band_min_pct", func(t risk.RegimeThresholds) float64 { return t.HedgeBandMinPct }, true},
	{"hedge_band_max_pct", func(t risk.RegimeThresholds) float64 { return t.HedgeBandMaxPct }, true},
}

func checkRegimeLoosens(c *policyCheckContext) []policyCheckHit {
	sets := rulebookRegimeSets(c.rulebook)
	var out []policyCheckHit
	for _, m := range policyCheckRegimeMeasures {
		for i := 1; i < len(sets); i++ {
			calmer, worse := sets[i-1], sets[i]
			a, b := m.value(calmer.t), m.value(worse.t)
			loosens := b > a
			if m.up {
				loosens = b < a
			}
			if !loosens {
				continue
			}
			dir, want := "higher", "at most"
			if m.up {
				dir, want = "lower", "at least"
			}
			out = append(out, policyCheckHit{
				keys: []rpc.PolicyCheckKey{c.rulebookKey(calmer.name+"."+m.key, a), c.rulebookKey(worse.name+"."+m.key, b)},
				message: fmt.Sprintf("%s is %s in [%s] (%s) than in [%s] (%s): the limit loosens as the regime worsens, the opposite of what the regime sets are for.",
					m.key, dir, worse.name, policyCheckNumber(b), calmer.name, policyCheckNumber(a)),
				suggestion: fmt.Sprintf("Set [%s].%s to %s %s, the calmer set's value.", worse.name, m.key, want, policyCheckNumber(a))})
		}
	}
	return out
}

func checkOrderEntryOff(c *policyCheckContext) []policyCheckHit {
	if c.trading.OrderEntryEnabled() {
		return nil
	}
	var active []string
	if s := c.sweep(); s != nil && s.effectiveMode() == rpc.CashSweepModeActive {
		active = append(active, "[cash.sweep] mode = active")
	}
	if b := c.protection.Buckets.BudgetReduction; b.enabled() && b.effectiveMode() == rpc.BudgetReductionModeActive {
		active = append(active, "[buckets.budget_reduction] mode = active")
	}
	if l := c.protection.Cash.Leveling; l.enabled() {
		active = append(active, "[cash.leveling] enabled = true")
	}
	if pre := c.protection.Authority.PreAuthorised; len(pre) > 0 {
		active = append(active, "[authority].pre_authorised = "+strings.Join(pre, ", "))
	}
	if pre := c.protection.Cash.PreAuthorised; len(pre) > 0 {
		active = append(active, "[cash].pre_authorised = "+strings.Join(pre, ", "))
	}
	if len(active) == 0 {
		return nil
	}
	keys := []rpc.PolicyCheckKey{{File: c.config, Key: "[trading].mode", Value: c.trading.Mode}}
	for _, a := range active {
		k, v, _ := strings.Cut(a, " = ")
		keys = append(keys, rpc.PolicyCheckKey{File: c.protectionSrc.label, Key: k, Value: v})
	}
	return []policyCheckHit{{keys: keys,
		message:    fmt.Sprintf("Order entry is %s, yet %s: those rows are listed as ready and can never be placed.", c.trading.Mode, strings.Join(active, " and ")),
		suggestion: "Set the bucket's mode to shadow (and empty pre_authorised) while order entry is off, or turn order entry on yourself."}}
}

func checkSettlementRouteExpired(c *policyCheckContext) []policyCheckHit {
	s := c.sweep()
	if s == nil {
		return nil
	}
	today := c.now.Format(time.DateOnly)
	var out []policyCheckHit
	for _, ccy := range slices.Sorted(maps.Keys(s.Currency)) {
		d := string(s.Currency[ccy].SettlementValidThrough)
		if d == "" || !policyDate(d).valid() || d >= today {
			continue
		}
		out = append(out, policyCheckHit{keys: []rpc.PolicyCheckKey{c.sweepCurrencyKey(ccy, "settlement_valid_through", d)},
			message:    fmt.Sprintf("The %s settlement route ended on %s, so every %s bill order holds until the date is renewed.", ccy, d, ccy),
			suggestion: "Delete settlement_valid_through so Canary's maintained route applies, or write a later date you have checked."})
	}
	return out
}

func checkBaseCurrencyMismatch(c *policyCheckContext) []policyCheckHit {
	k := c.constitution
	if k == nil || strings.TrimSpace(k.Capital.BaseCurrency) == "" || strings.EqualFold(k.Capital.BaseCurrency, c.book.BaseCurrency) {
		return nil
	}
	return []policyCheckHit{{keys: []rpc.PolicyCheckKey{c.constitutionKey("capital", "base_currency", k.Capital.BaseCurrency), {File: "live account", Key: "base currency", Value: c.book.BaseCurrency}},
		message:    fmt.Sprintf("The constitution counts capital in %s but the account reports %s: capital math refuses every equity observation, so the drawdown ladder cannot run.", k.Capital.BaseCurrency, c.book.BaseCurrency),
		suggestion: fmt.Sprintf("Set base_currency = %q and restate protected_floor and declared_risk_capital in %s.", c.book.BaseCurrency, c.book.BaseCurrency)}}
}

func checkLotAboveTradingMax(c *policyCheckContext) []policyCheckHit {
	tradingCap, ok := c.orderCap()
	if !c.book.PositionsKnown || !ok {
		return nil
	}
	var out []policyCheckHit
	for _, p := range c.book.Positions {
		// A protective stop on a long stock line is exempt from the cap.
		if p.Kind == policyCheckKindStock || p.Quantity == 0 {
			continue
		}
		unit := math.Abs(p.MarketValueBase / p.Quantity)
		if unit <= tradingCap {
			continue
		}
		out = append(out, policyCheckHit{keys: []rpc.PolicyCheckKey{c.tradingCapKey(), {File: "live account", Key: "one " + p.Currency + " option contract", Value: policyCheckMoney(unit, c.base())}},
			message:    fmt.Sprintf("One contract of a held %s option line is worth %s, above the order cap in force of %s: no loss exit, budget reduction or close for it can pass the gate, even for a single contract.", p.Currency, policyCheckMoney(unit, c.base()), policyCheckMoney(tradingCap, c.base())),
			suggestion: fmt.Sprintf("Raise [order_limits] max_order_floor_base (or max_order_pct_nlv) yourself so the cap in force reaches at least %s if Canary should be able to exit this line, or plan its exit by hand.", policyCheckMoney(policyCheckRoundUp(unit), c.base()))})
	}
	return out
}

func checkCapFXHeadroom(c *policyCheckContext) []policyCheckHit {
	tradingCap, ok := c.orderCap()
	if !ok {
		return nil
	}
	var out []policyCheckHit
	for _, cp := range c.bucketCaps() {
		if cp.bucket == "cash_sweep" && c.sweepExempt() {
			continue
		}
		if cp.base > tradingCap*(1+1e-9) || cp.base < tradingCap*(1-policyCheckFXHeadroom) {
			continue
		}
		// The sweep caps in base and converts to each bill currency at the
		// ledger rate; the other buckets size in the contract currency.
		var foreign []string
		if cp.bucket == "cash_sweep" {
			for _, ccy := range c.sweepCurrencies() {
				if ccy != c.base() {
					foreign = append(foreign, ccy)
				}
			}
		} else if !cp.assumedBase && cp.ccy != c.base() {
			foreign = []string{cp.ccy}
		}
		if len(foreign) == 0 {
			continue
		}
		how := "sizes each order in the contract currency"
		if cp.bucket == "cash_sweep" {
			how = "sizes each " + strings.Join(foreign, " and ") + " order at the ledger rate"
		}
		out = append(out, policyCheckHit{keys: append(slices.Clone(cp.keys), c.tradingCapKey()),
			message: fmt.Sprintf("[buckets.%s] caps one order at %s, within %s of the order cap in force of %s; the bucket %s while the gate converts at the session quote, so an FX move of that size refuses an order sized at the cap.",
				cp.bucket, policyCheckMoney(cp.base, c.base()), policyCheckPct(policyCheckFXHeadroom), policyCheckMoney(tradingCap, c.base()), how),
			suggestion: fmt.Sprintf("Set the cap to %s, at most 97%% of the order cap in force in the order's currency rounded down, which leaves room for a 3%% FX move.", policyCheckMoney(policyCheckRoundDown(tradingCap*0.97/cp.fx), cp.nativeCcy))})
	}
	return out
}

func checkOrderCapVsNLV(c *policyCheckContext) []policyCheckHit {
	nlv, base := c.book.NetLiquidation, c.book.BaseCurrency
	type capRow struct {
		label string
		keys  []rpc.PolicyCheckKey
		base  float64
		fx    float64
		ccy   string
	}
	const orderCapLabel = "The order cap in force ([order_limits])"
	tradingCap, capOK := c.orderCap()
	var rows []capRow
	if capOK {
		rows = append(rows, capRow{label: orderCapLabel, keys: []rpc.PolicyCheckKey{c.tradingCapKey()}, base: tradingCap, fx: 1, ccy: base})
	}
	for _, cp := range c.bucketCaps() {
		rows = append(rows, capRow{label: "[buckets." + cp.bucket + "]", keys: cp.keys, base: cp.base, fx: cp.fx, ccy: cp.nativeCcy})
	}
	var out []policyCheckHit
	for _, r := range rows {
		share := r.base / nlv
		switch {
		case share < policyCheckTinyShareNLV:
			want := policyCheckRoundUp(0.05 * nlv)
			if r.label != orderCapLabel && capOK {
				want = min(want, tradingCap)
			}
			out = append(out, policyCheckHit{keys: r.keys,
				message: fmt.Sprintf("%s caps one order at %s, %s of NLV (%s): a trim of 10%% of the book takes %d orders, each waiting its own cycle and approval.",
					r.label, policyCheckMoney(r.base, base), policyCheckPct(share), policyCheckMoney(nlv, base), int(math.Ceil(0.10*nlv/r.base))),
				suggestion: fmt.Sprintf("About %s (5%% of NLV, no more than the trading cap), so a typical trim fits in two orders.", policyCheckMoney(policyCheckRoundDown(want/r.fx), r.ccy))})
		case share > policyCheckHugeShareNLV:
			out = append(out, policyCheckHit{keys: r.keys,
				message: fmt.Sprintf("%s caps one order at %s, %s of NLV (%s): a single order may move more than half the book.",
					r.label, policyCheckMoney(r.base, base), policyCheckPct(share), policyCheckMoney(nlv, base)),
				suggestion: fmt.Sprintf("About %s (a quarter of NLV), which keeps one order material without letting it move half the book.", policyCheckMoney(policyCheckRoundDown(0.25*nlv/r.fx), r.ccy))})
		}
	}
	return out
}

func checkOrderCapSplits(c *policyCheckContext) []policyCheckHit {
	nlv, base := c.book.NetLiquidation, c.book.BaseCurrency
	largest := func(kind string) float64 {
		best := 0.0
		for _, p := range c.book.Positions {
			if p.Kind == kind {
				best = max(best, math.Abs(p.MarketValueBase))
			}
		}
		return best
	}
	var out []policyCheckHit
	report := func(keys []rpc.PolicyCheckKey, label, what string, size, capBase, fx float64, ccy string) {
		if capBase <= 0 || size <= 0 {
			return
		}
		n := int(math.Ceil(size/capBase - 1e-9))
		if n <= policyCheckMaxSplitOrders {
			return
		}
		want := policyCheckRoundUp(size / policyCheckMaxSplitOrders)
		if tradingCap, ok := c.orderCap(); ok {
			want = min(want, tradingCap)
		}
		out = append(out, policyCheckHit{keys: keys,
			message: fmt.Sprintf("%s splits %s of %s into %d orders of at most %s; each waits its own proposal cycle and approval while the rest of the risk stays on.",
				label, what, policyCheckMoney(size, base), n, policyCheckMoney(capBase, base)),
			suggestion: fmt.Sprintf("About %s, so it takes at most %d orders (no more than the trading cap).", policyCheckMoney(policyCheckRoundUp(want/fx), ccy), max(policyCheckMaxSplitOrders, int(math.Ceil(size/want-1e-9))))})
	}
	for _, cp := range c.bucketCaps() {
		switch cp.bucket {
		case "risk_reduction":
			p := c.rulebook
			trim := (p.SingleNameActPct - p.SingleNameWatchPct) / 100 * nlv
			if line := largest(policyCheckKindStock); line > p.SingleNameWatchPct/100*nlv {
				trim = max(trim, line-p.SingleNameWatchPct/100*nlv)
			}
			report(append(slices.Clone(cp.keys), c.rulebookKey("single_name_act_pct", p.SingleNameActPct), c.rulebookKey("single_name_watch_pct", p.SingleNameWatchPct)),
				"[buckets.risk_reduction]", "a trim from the issuer act level back to watch", trim, cp.base, cp.fx, cp.nativeCcy)
		case "budget_reduction":
			b := c.protection.Buckets.BudgetReduction
			if b.basis() != rpc.BudgetBasisRulebook {
				continue
			}
			t := c.rulebook.RegimeCalm
			cut := (t.PremiumBudgetActPct - t.PremiumBudgetWatchPct) / 100 * nlv
			report(append(slices.Clone(cp.keys), c.rulebookKey("regime_calm.premium_budget_act_pct", t.PremiumBudgetActPct), c.rulebookKey("regime_calm.premium_budget_watch_pct", t.PremiumBudgetWatchPct)),
				"[buckets.budget_reduction]", "the cut from the calm premium budget's act level back to watch", cut, cp.base, cp.fx, cp.nativeCcy)
		}
	}
	if tradingCap, ok := c.orderCap(); ok && c.protection.Buckets.TrailingStop.Options.Enabled {
		report([]rpc.PolicyCheckKey{c.tradingCapKey()}, "The order cap in force ([order_limits])", "a whole-line exit of the largest option line (option exits are not exempt from the cap)",
			largest(policyCheckKindOption), tradingCap, 1, base)
	}
	return out
}

func checkCashReserveVsNLV(c *policyCheckContext) []policyCheckHit {
	s := c.sweep()
	if s == nil {
		return nil
	}
	nlv, base := c.book.NetLiquidation, c.book.BaseCurrency
	var keys []rpc.PolicyCheckKey
	// The reserve design: the largest of reserve_floor_base and
	// reserve_pct_nlv of NLV (planned needs are 0 today), held in the base
	// currency, which keeps the larger of its keep_cash and the reserve;
	// every other currency keeps its own keep_cash.
	design := s.ReserveFloorBase != nil || s.ReservePctNLV != nil
	reserve := 0.0
	if s.ReserveFloorBase != nil {
		reserve = *s.ReserveFloorBase
		keys = append(keys, c.protectionKey("cash.sweep", "reserve_floor_base", policyCheckMoney(*s.ReserveFloorBase, base)))
	}
	if s.ReservePctNLV != nil {
		reserve = max(reserve, *s.ReservePctNLV/100*nlv)
		keys = append(keys, c.protectionKey("cash.sweep", "reserve_pct_nlv", policyCheckNumber(*s.ReservePctNLV)))
	}
	kept, baseSeen := 0.0, false
	for _, ccy := range c.sweepCurrencies() {
		fx, ok := c.fx(ccy)
		kc, written := s.keepCash(ccy)
		if !ok {
			continue
		}
		if written {
			keys = append(keys, c.sweepKeepCashKey(ccy, kc))
		}
		if ccy == base && design {
			kept += max(kc, reserve)
			baseSeen = true
			continue
		}
		kept += kc * fx
	}
	if design && !baseSeen {
		kept += reserve
	}
	if len(keys) == 0 {
		return nil
	}
	how := "keep_cash across the swept currencies keeps back"
	if design {
		how = fmt.Sprintf("the reserve (%s, held in %s) and keep_cash in the other currencies keep back", policyCheckMoney(reserve, base), base)
	}
	share := kept / nlv
	switch {
	case share < policyCheckTinyShareNLV:
		suggestion := fmt.Sprintf("Keep at least %s (2%% of NLV, rounded up) as cash across the currencies you trade.", policyCheckMoney(policyCheckRoundUp(policyCheckTinyShareNLV*nlv), base))
		if design {
			suggestion = fmt.Sprintf("Set reserve_pct_nlv to at least 2 (%s at today's NLV), so the reserve follows the book instead of a fixed amount.", policyCheckMoney(policyCheckRoundUp(policyCheckTinyShareNLV*nlv), base))
		}
		return []policyCheckHit{{keys: keys,
			message:    fmt.Sprintf("Together %s %s, %s of NLV (%s): a margin call, an assignment or a settling buy can need more cash than that before a bill matures.", how, policyCheckMoney(kept, base), policyCheckPct(share), policyCheckMoney(nlv, base)),
			suggestion: suggestion}}
	case share > policyCheckHugeShareNLV:
		return []policyCheckHit{{keys: keys,
			message:    fmt.Sprintf("Together %s %s, %s of NLV (%s): the sweep can hardly put any idle cash to work.", how, policyCheckMoney(kept, base), policyCheckPct(share), policyCheckMoney(nlv, base)),
			suggestion: fmt.Sprintf("About %s (10%% of NLV) is a common working reserve; reserve_pct_nlv = 10 keeps it in step with the book.", policyCheckMoney(policyCheckRoundUp(0.10*nlv), base))}}
	}
	return nil
}

// sweepKeepCashKey names the keep_cash a currency uses: its own table's, or
// the bucket's.
func (c *policyCheckContext) sweepKeepCashKey(ccy string, v float64) rpc.PolicyCheckKey {
	if s := c.sweep(); s != nil {
		if t, ok := s.Currency[ccy]; ok && t.KeepCash != nil {
			return c.sweepCurrencyKey(ccy, "keep_cash", policyCheckMoney(v, ccy))
		}
	}
	return c.protectionKey("cash.sweep", "keep_cash", policyCheckMoney(v, ccy)+" (in "+ccy+")")
}

// constitutionNumbers returns the floor and declared risk capital when the
// constitution writes them in the account's base currency.
func (c *policyCheckContext) constitutionNumbers() (floor, declared *float64, ok bool) {
	k := c.constitution
	if k == nil || (k.Capital.BaseCurrency != "" && !strings.EqualFold(k.Capital.BaseCurrency, c.book.BaseCurrency)) {
		return nil, nil, false
	}
	return k.Capital.ProtectedFloor, k.Capital.DeclaredRiskCapital, true
}

func checkProtectedFloor(c *policyCheckContext) []policyCheckHit {
	floor, declared, ok := c.constitutionNumbers()
	if !ok || floor == nil {
		return nil
	}
	equity, base := c.book.NetLiquidation, c.book.BaseCurrency
	keys := []rpc.PolicyCheckKey{c.constitutionKey("capital", "protected_floor", policyCheckMoney(*floor, base)), {File: "live account", Key: "equity (NLV)", Value: policyCheckMoney(equity, base)}}
	above := equity - *floor
	if above <= 0 {
		suggest := "a floor below current equity"
		if declared != nil && *declared > 0 {
			suggest = policyCheckMoney(policyCheckRoundDown(equity-*declared), base) + " (equity less the declared risk capital)"
		}
		return []policyCheckHit{{keys: keys,
			message:    fmt.Sprintf("The protected floor %s is at or above current equity %s: effective risk capital is zero, so the drawdown ladder reads the whole budget as consumed.", policyCheckMoney(*floor, base), policyCheckMoney(equity, base)),
			suggestion: "Set protected_floor to " + suggest + "."}}
	}
	if declared == nil || *declared <= above {
		return nil
	}
	keys = append(keys, c.constitutionKey("capital", "declared_risk_capital", policyCheckMoney(*declared, base)))
	return []policyCheckHit{{keys: keys,
		message: fmt.Sprintf("Only %s of the declared risk capital %s sits above the protected floor, so every loss limit measures against the smaller figure: the drawdown ladder trips after a loss %s smaller than the declaration suggests.",
			policyCheckMoney(above, base), policyCheckMoney(*declared, base), policyCheckPct(1 - above / *declared)),
		suggestion: fmt.Sprintf("Set declared_risk_capital to %s (equity less the floor), or lower protected_floor to %s.", policyCheckMoney(policyCheckRoundDown(above), base), policyCheckMoney(policyCheckRoundDown(equity-*declared), base))}}
}

func checkDeclaredRisk(c *policyCheckContext) []policyCheckHit {
	_, declared, ok := c.constitutionNumbers()
	if !ok || declared == nil {
		return nil
	}
	nlv, base := c.book.NetLiquidation, c.book.BaseCurrency
	keys := []rpc.PolicyCheckKey{c.constitutionKey("capital", "declared_risk_capital", policyCheckMoney(*declared, base)), {File: "live account", Key: "NLV", Value: policyCheckMoney(nlv, base)}}
	switch share := *declared / nlv; {
	case share > 1:
		return []policyCheckHit{{keys: keys,
			message:    fmt.Sprintf("Declared risk capital %s is above NLV %s: you would be authorising the loss of more than the account holds, so the loss ladder can never reach its levels.", policyCheckMoney(*declared, base), policyCheckMoney(nlv, base)),
			suggestion: fmt.Sprintf("At most %s (a quarter of NLV) unless you mean to risk more.", policyCheckMoney(policyCheckRoundDown(0.25*nlv), base))}}
	case share < policyCheckTinyShareNLV:
		return []policyCheckHit{{keys: keys,
			message:    fmt.Sprintf("Declared risk capital %s is %s of NLV: ordinary daily moves consume the budget, so the drawdown warning and brake trip on noise.", policyCheckMoney(*declared, base), policyCheckPct(share)),
			suggestion: fmt.Sprintf("At least %s (5%% of NLV).", policyCheckMoney(policyCheckRoundUp(0.05*nlv), base))}}
	}
	return nil
}

func checkSweepEconomics(c *policyCheckContext) []policyCheckHit {
	s := c.sweep()
	if s == nil {
		return nil
	}
	var out []policyCheckHit
	for _, ccy := range c.sweepCurrencies() {
		cfg := s.currency(ccy)
		if !slices.ContainsFunc(cfg.Instruments, func(i string) bool { return cashSweepBillCurrency[i] == ccy }) {
			continue
		}
		a, ok := policyCheckBillAssumptions[ccy]
		if !ok {
			c.skip(fmt.Sprintf("the %s sweep economics: Canary has no bill assumption for %s", ccy, ccy))
			continue
		}
		c.assume(a.note)
		m, ok := c.sweepMinimum(ccy)
		if !ok {
			continue
		}
		fx, minNative := m.fx, m.native
		keys := append(slices.Clone(m.keys), c.sweepCurrencyKey(ccy, "min_maturity_days", fmt.Sprint(cfg.MinMaturityDays)))
		cashRate := 0.0
		if cfg.CashInterestRateUpper != nil {
			cashRate = *cfg.CashInterestRateUpper
			keys = append(keys, c.sweepCurrencyKey(ccy, "cash_interest_rate_upper", policyCheckNumber(cashRate)))
		} else {
			c.assume(ccy + " cash: assumed to earn no interest (cash_interest_rate_upper is not written)")
		}
		net := a.yield - cashRate
		days := float64(cfg.MinMaturityDays)
		threshold := a.minCommission
		if s.MinNetGain > 0 {
			threshold += s.MinNetGain / fx
			keys = append(keys, c.protectionKey("cash.sweep", "min_net_gain", policyCheckMoney(s.MinNetGain, c.base())))
		}
		commission := max(a.minCommission, a.pctCommission*minNative)
		interest := minNative * net * days / 365
		if interest >= commission+(threshold-a.minCommission) {
			continue
		}
		perUnit := net*days/365 - a.pctCommission
		suggestion := fmt.Sprintf("Bills do not out-earn %s cash at %d days on these assumptions; lengthen min_maturity_days or leave %s as cash.", ccy, cfg.MinMaturityDays, ccy)
		if perUnit > 0 {
			breakeven := threshold / perUnit
			want := policyCheckRoundUp(2 * breakeven)
			suggestion = fmt.Sprintf("Set min_order_notional to at least %s (%s), twice the breakeven of %s, so the commission takes at most half the interest to a %d-day bill",
				policyCheckMoney(policyCheckRoundUp(want*fx), c.base()), policyCheckMoney(want, ccy), policyCheckMoney(breakeven, ccy), cfg.MinMaturityDays)
			if m.binding == "min_tranche" {
				suggestion = fmt.Sprintf("The retired min_tranche sets the %s minimum; delete it and set min_order_notional to at least %s (%s), twice the breakeven of %s, so the commission takes at most half the interest to a %d-day bill",
					ccy, policyCheckMoney(policyCheckRoundUp(want*fx), c.base()), policyCheckMoney(want, ccy), policyCheckMoney(breakeven, ccy), cfg.MinMaturityDays)
			}
			if longer := 91.0; days < longer {
				suggestion += fmt.Sprintf("; or raise min_maturity_days: at %d days the breakeven falls to %s", int(longer), policyCheckMoney(threshold/(net*longer/365-a.pctCommission), ccy))
			}
			suggestion += "."
		}
		out = append(out, policyCheckHit{keys: keys,
			message: fmt.Sprintf("The smallest %s sweep buy, %s (%s, from %s) in a %d-day bill, earns about %s over cash at an assumed %s a year, less than the assumed %s commission: the smallest bill buy the sweep places loses money.",
				ccy, policyCheckMoney(minNative, ccy), policyCheckMoney(m.base, c.base()), m.binding, cfg.MinMaturityDays, policyCheckMoney(interest, ccy), policyCheckPct(net), policyCheckMoney(commission, ccy)),
			suggestion: suggestion})
	}
	return out
}

func checkVersionNotBumped(c *policyCheckContext) []policyCheckHit {
	var out []policyCheckHit
	for _, src := range c.sources() {
		if c.fileStatus[src.policy] != "drift" {
			continue
		}
		out = append(out, policyCheckHit{keys: []rpc.PolicyCheckKey{{File: src.label, Key: "policy_version", Value: "unchanged since the last edit"}},
			message:    fmt.Sprintf("%s changed on disk but its policy_version did not rise, so the daemon keeps the previous policy in force and your edit does nothing yet.", src.label),
			suggestion: "Raise policy_version by one."})
	}
	return out
}

// policyCheckDated is one dated assumption.
type policyCheckDated struct {
	key  rpc.PolicyCheckKey
	at   time.Time
	what string
}

func (c *policyCheckContext) datedAssumptions() []policyCheckDated {
	var out []policyCheckDated
	if s := c.protection.Cash.Sweep; s != nil {
		for _, ccy := range slices.Sorted(maps.Keys(s.Currency)) {
			d := string(s.Currency[ccy].CashInterestValidThrough)
			t, err := time.ParseInLocation(time.DateOnly, d, c.now.Location())
			if d == "" || err != nil {
				continue
			}
			out = append(out, policyCheckDated{key: c.sweepCurrencyKey(ccy, "cash_interest_valid_through", d), at: t.AddDate(0, 0, 1),
				what: "the " + ccy + " cash-interest assumption, so the sweep's benefit comparison reads unavailable"})
		}
	}
	for i, intent := range c.protection.Buckets.TrailingStop.Options.DirectionalIntents {
		if intent.ExpiresAt.IsZero() {
			continue
		}
		out = append(out, policyCheckDated{key: c.protectionKey("buckets.trailing_stop.options", fmt.Sprintf("directional_intents[%d].expires_at", i), intent.ExpiresAt.UTC().Format(time.RFC3339)), at: intent.ExpiresAt,
			what: fmt.Sprintf("directional intent %d (contract %d); an expired declaration still overrides the standing defaults for that contract, so its exits stop", i, intent.ConID)})
	}
	return out
}

func checkDatedExpired(c *policyCheckContext) []policyCheckHit {
	var out []policyCheckHit
	for _, d := range c.datedAssumptions() {
		if d.at.After(c.now) {
			continue
		}
		out = append(out, policyCheckHit{keys: []rpc.PolicyCheckKey{d.key},
			message:    fmt.Sprintf("This date has passed and with it %s.", d.what),
			suggestion: "Review the assumption and write a new date, or delete the entry."})
	}
	return out
}

func checkDatedExpiring(c *policyCheckContext) []policyCheckHit {
	var out []policyCheckHit
	for _, d := range c.datedAssumptions() {
		if !d.at.After(c.now) || d.at.Sub(c.now) > policyCheckExpiringWithin {
			continue
		}
		out = append(out, policyCheckHit{keys: []rpc.PolicyCheckKey{d.key},
			message: fmt.Sprintf("This date ends in %d days, and with it %s.", int(math.Ceil(d.at.Sub(c.now).Hours()/24)), d.what)})
	}
	return out
}

func checkFileUnreviewed(c *policyCheckContext) []policyCheckHit {
	var out []policyCheckHit
	for _, src := range c.sources() {
		if src.review != rpc.PolicyReviewUnreviewed {
			continue
		}
		msg := fmt.Sprintf("%s still says \"Canary defaults, not yet reviewed\": every surface reports its limits as Canary's, not yours.", src.label)
		if src.policy == PolicyFileRulebook {
			changed := rulebookValuesChanged(c.rulebook)
			if len(changed) > 0 {
				verb := "differ"
				if len(changed) == 1 {
					verb = "differs"
				}
				msg = fmt.Sprintf("%s still says \"Canary defaults, not yet reviewed\", yet %d of its limits %s from Canary's defaults (%s): it reads as unreviewed although you have edited it.",
					src.label, len(changed), verb, strings.Join(changed, ", "))
			} else {
				msg = fmt.Sprintf("%s still says \"Canary defaults, not yet reviewed\" and every limit equals Canary's compiled default: none of these numbers has been chosen for this book.", src.label)
			}
		}
		out = append(out, policyCheckHit{keys: []rpc.PolicyCheckKey{{File: src.label, Key: "header", Value: "Canary defaults, not yet reviewed"}}, message: msg,
			suggestion: "Review the limits against your book (this check's warnings are a start), then delete the header line."})
	}
	return out
}

// rulebookValuesChanged lists the Rulebook keys whose value differs from
// Canary's compiled default.
func rulebookValuesChanged(p risk.RulebookPolicy) []string {
	got, def := rulebookPolicyValues(p), rulebookPolicyValues(risk.DefaultRulebookPolicy())
	var out []string
	for _, k := range slices.Sorted(maps.Keys(got)) {
		switch k {
		case "kind", "schema_version", "policy_id", "policy_version", "earnings_stale_days", "issuer_groups", "clusters", "modes", "hedge_symbols":
			continue
		}
		if fmt.Sprint(got[k]) != fmt.Sprint(def[k]) {
			out = append(out, k)
		}
	}
	return out
}

func checkCompiledDefaults(c *policyCheckContext) []policyCheckHit {
	var out []policyCheckHit
	s := c.sweep()
	if s == nil {
		return out
	}
	// Since the reserve design (2026-10-05) a sweep number missing from the
	// file holds the sweep; the ensure step at daemon start writes it.
	if missing := s.missingNumbers(); len(missing) > 0 {
		var keys []rpc.PolicyCheckKey
		for _, key := range missing {
			keys = append(keys, c.protectionKey("cash.sweep", key, "not written"))
		}
		out = append(out, policyCheckHit{keys: keys,
			message:    "The cash sweep holds until these numbers are written in the policy file; Canary reads them from the file only.",
			suggestion: "Run canary restart: at start the daemon writes each absent number at Canary's default after a backup. A number written as 0 counts as missing; write your own value with a higher policy_version."})
	}
	return out
}

func checkSweepExempt(c *policyCheckContext) []policyCheckHit {
	if c.sweep() == nil || !c.sweepExempt() {
		return nil
	}
	capBase, keys, ok := c.sweepCapBase()
	exempt := c.protectionKey("cash.sweep", "bills_exempt_from_trading_max_notional", "true")
	tradingCap, capOK := c.orderCap()
	if !capOK {
		return nil
	}
	if !ok || capBase <= tradingCap {
		return []policyCheckHit{{keys: []rpc.PolicyCheckKey{exempt},
			message: "Bill orders are declared exempt from the order cap in force, but the sweep's cap in force is within it, so the exemption is not used."}}
	}
	return []policyCheckHit{{keys: append(append(keys, exempt), c.tradingCapKey()),
		message: fmt.Sprintf("The sweep's cap in force of %s is above the order cap in force of %s; bills_exempt_from_trading_max_notional lets bill orders pass the order cap up to the sweep's cap, so the gap is intended (stocks, ETFs, the fallback ETF and conversions keep the order cap).", policyCheckMoney(capBase, c.base()), policyCheckMoney(tradingCap, c.base()))}}
}

// checkSweepBuysWhileBorrowed reports a sweep allowed to buy bills while a
// currency is borrowed (owner decision 2026-10-05 21:24 CEST): margin
// interest on the debit usually costs more than the bill earns.
func checkSweepBuysWhileBorrowed(c *policyCheckContext) []policyCheckHit {
	s := c.sweep()
	if s == nil || s.NoBuyWhileBorrowed == nil || *s.NoBuyWhileBorrowed || c.book == nil {
		return nil
	}
	var borrowed []string
	for _, ccy := range slices.Sorted(maps.Keys(c.book.Cash)) {
		if cash := c.book.Cash[ccy]; finiteProtectionOptionPolicyValue(cash) && cash < -rpc.CashSweepBorrowedToleranceUnits {
			borrowed = append(borrowed, cashSweepSignedMoney(cash, ccy))
		}
	}
	if len(borrowed) == 0 {
		return nil
	}
	return []policyCheckHit{{keys: []rpc.PolicyCheckKey{c.protectionKey("cash.sweep", "no_buy_while_borrowed", "false")},
		message:    fmt.Sprintf("The account is borrowing (%s), and no_buy_while_borrowed = false lets the sweep buy bills meanwhile; margin interest on the debit usually costs more than a bill earns.", strings.Join(borrowed, ", ")),
		suggestion: "Set no_buy_while_borrowed = true in [cash.sweep] and raise policy_version; repay the debit by converting or depositing (the sweep never converts)."}}
}

// policyCheckDeref reads an optional policy number; an unwritten key reads 0.
func policyCheckDeref(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}

// checkOrderLimitsMissing reports an [order_limits] table the trading gate
// cannot use: a key not written, or no constitution at all.
func checkOrderLimitsMissing(c *policyCheckContext) []policyCheckHit {
	l := c.orderLimits
	if l.Complete && l.BondMaturityUnset && l.Unavailable == "" {
		return []policyCheckHit{{keys: []rpc.PolicyCheckKey{{File: c.constitutionSrc.label, Key: "[order_limits]." + risk.OrderLimitMaxBondMaturityYears, Value: "not written"}},
			message:    "risk-policy.toml [order_limits] does not write max_bond_maturity_years, so the trading gate refuses every bond buy; other orders are judged as usual.",
			suggestion: "Run canary restart: at start the daemon writes the owner's 30-year limit after a backup. Or write the key with a higher policy_version."}}
	}
	if l.Complete {
		return nil
	}
	var keys []rpc.PolicyCheckKey
	for _, k := range l.Missing {
		keys = append(keys, rpc.PolicyCheckKey{File: c.constitutionSrc.label, Key: "[order_limits]." + strings.TrimPrefix(k, risk.OrderLimitsTable+"."), Value: "not written"})
	}
	why := l.Unavailable
	if why == "" {
		why = "risk-policy.toml [order_limits] does not write " + strings.Join(l.Missing, ", ")
	}
	return []policyCheckHit{{keys: keys,
		message:    strings.ToUpper(why[:1]) + why[1:] + ", so the trading gate refuses every order preview.",
		suggestion: "Run canary restart: at start the daemon writes today's effective gates from config.toml and the scaled cap after a backup. Or write the keys with a higher policy_version."}}
}

// checkRetiredTradingGates warns while config.toml [trading] still carries a
// retired order gate whose value differs from [order_limits], which decides.
func checkRetiredTradingGates(c *policyCheckContext) []policyCheckHit {
	var o risk.ConstitutionOrderLimits
	if c.constitution != nil && c.constitution.OrderLimits != nil {
		o = *c.constitution.OrderLimits
	}
	t := c.trading
	type gate struct {
		config, policy   string
		configV, policyV string
		differs          bool
	}
	var gates []gate
	if t.MaxNotional != nil && o.MaxOrderFloorBase != nil {
		gates = append(gates, gate{"max_notional", "max_order_floor_base", policyCheckMoney(*t.MaxNotional, c.base()), policyCheckMoney(*o.MaxOrderFloorBase, c.base()), *t.MaxNotional != *o.MaxOrderFloorBase})
	}
	if t.MaxOptionContracts != nil && o.MaxOptionContracts != nil {
		gates = append(gates, gate{"max_option_contracts", "max_option_contracts", strconv.Itoa(*t.MaxOptionContracts), strconv.Itoa(*o.MaxOptionContracts), *t.MaxOptionContracts != *o.MaxOptionContracts})
	}
	if t.AllowStockShort != nil && o.AllowStockShort != nil {
		gates = append(gates, gate{"allow_stock_short", "allow_stock_short", strconv.FormatBool(*t.AllowStockShort), strconv.FormatBool(*o.AllowStockShort), *t.AllowStockShort != *o.AllowStockShort})
	}
	if t.AllowOptionSellToOpen != nil && o.AllowOptionSellToOpen != nil {
		gates = append(gates, gate{"allow_option_sell_to_open", "allow_option_sell_to_open", strconv.FormatBool(*t.AllowOptionSellToOpen), strconv.FormatBool(*o.AllowOptionSellToOpen), *t.AllowOptionSellToOpen != *o.AllowOptionSellToOpen})
	}
	var out []policyCheckHit
	for _, g := range gates {
		if !g.differs {
			continue
		}
		out = append(out, policyCheckHit{keys: []rpc.PolicyCheckKey{{File: c.config, Key: "[trading]." + g.config, Value: g.configV}, {File: c.constitutionSrc.label, Key: "[order_limits]." + g.policy, Value: g.policyV}},
			message:    fmt.Sprintf("[trading].%s = %s is retired and no longer read; [order_limits].%s = %s decides. A reader of config.toml sees a limit that is not in force.", g.config, g.configV, g.policy, g.policyV),
			suggestion: fmt.Sprintf("Delete %s from [trading] in %s; the order limits live in the risk constitution only.", g.config, c.config)})
	}
	return out
}

// checkLevelingDebitInsideBand reports a margin loan leveling leaves alone
// because it sits inside the band: it keeps paying debit interest, and while
// no_buy_while_borrowed holds the sweep's bill buys for any debit beyond one
// unit, nothing is proposed to clear it.
func checkLevelingDebitInsideBand(c *policyCheckContext) []policyCheckHit {
	l := c.protection.Cash.Leveling
	if !l.enabled() || l.TriggerBase == nil || c.book == nil {
		return nil
	}
	sweepHolds := ""
	if s := c.sweep(); s.enabled() && s.noBuyWhileBorrowed() {
		sweepHolds = " Meanwhile no_buy_while_borrowed holds the sweep's bill buys."
	}
	var out []policyCheckHit
	for _, ccy := range slices.Sorted(maps.Keys(c.book.Cash)) {
		cash := c.book.Cash[ccy]
		rate, ok := c.fx(ccy)
		if !ok || l.deliberateCarry(ccy) || !finiteProtectionOptionPolicyValue(cash) || cash >= -rpc.CashSweepBorrowedToleranceUnits {
			continue
		}
		debit := -cash * rate
		if debit > *l.TriggerBase {
			continue
		}
		out = append(out, policyCheckHit{keys: []rpc.PolicyCheckKey{c.protectionKey("cash.leveling", "trigger_base", policyCheckNumber(*l.TriggerBase))},
			message: fmt.Sprintf("%s cash is %s (%s), inside the %s band: leveling does not convert it, so it stays a margin loan and pays debit interest.%s",
				ccy, policyCheckMoney(cash, ccy), policyCheckMoney(-debit, c.base()), policyCheckMoney(*l.TriggerBase, c.base()), sweepHolds),
			suggestion: fmt.Sprintf("Convert it yourself, lower trigger_base below %s, or set deliberate_carry = true for %s if you keep it borrowed on purpose.", policyCheckMoney(policyCheckRoundDown(debit), c.base()), ccy)})
	}
	return out
}
