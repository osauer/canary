package daemon

import (
	"fmt"
	"maps"
	"math"
	"slices"
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
	{id: "cap_above_trading_max", severity: rpc.PolicyCheckError, category: rpc.PolicyCheckCategoryContradiction,
		summary: "A bucket's per-order cap lets an order exceed [trading].max_notional, which the gate always refuses, unless a documented exemption covers it.", run: checkCapAboveTradingMax},
	{id: "sweep_minimum_above_cap", severity: rpc.PolicyCheckError, category: rpc.PolicyCheckCategoryContradiction,
		summary: "A cash sweep currency's minimum order is above the sweep's or the trading cap, so no order can satisfy both.", run: checkSweepMinimumAboveCap},
	{id: "watch_act_inverted", severity: rpc.PolicyCheckError, category: rpc.PolicyCheckCategoryContradiction,
		summary: "A watch level sits beyond its act level (or a minimum above its maximum), per regime set and in the constitution's drawdown ladder.", run: checkWatchActInverted},
	{id: "regime_loosens_under_stress", severity: rpc.PolicyCheckError, category: rpc.PolicyCheckCategoryContradiction,
		summary: "A regime-conditional limit is looser in a worse regime than in a calmer one.", run: checkRegimeLoosens},
	{id: "order_entry_off_for_active_bucket", severity: rpc.PolicyCheckError, category: rpc.PolicyCheckCategoryContradiction,
		summary: "A bucket is active or pre-authorised while [trading].mode disables order entry, so its orders can never be placed.", run: checkOrderEntryOff},
	{id: "settlement_route_expired", severity: rpc.PolicyCheckError, category: rpc.PolicyCheckCategoryContradiction,
		summary: "A sweep currency's settlement_valid_through has passed, so every bill order in it holds.", run: checkSettlementRouteExpired},
	{id: "sweep_reserve_key_invalid", severity: rpc.PolicyCheckError, category: rpc.PolicyCheckCategoryContradiction,
		summary: "reserve_floor_base, reserve_pct_nlv, max_order_pct_nlv or bills_exempt_from_trading_max_notional is out of range or of the wrong type.", run: checkSweepReserveKeys},
	{id: "base_currency_mismatch", severity: rpc.PolicyCheckError, category: rpc.PolicyCheckCategoryBook, needsBook: true,
		summary: "The constitution's base_currency differs from the account's, so capital math refuses every observation.", run: checkBaseCurrencyMismatch},
	{id: "lot_above_trading_max", severity: rpc.PolicyCheckError, category: rpc.PolicyCheckCategoryBook, needsBook: true,
		summary: "One unit of a held line is worth more than [trading].max_notional, so no reduction or exit order for it can pass the gate.", run: checkLotAboveTradingMax},
	{id: "cap_without_fx_headroom", severity: rpc.PolicyCheckWarn, category: rpc.PolicyCheckCategoryContradiction,
		summary: "A cap sized in another currency sits within 2% of [trading].max_notional, so an FX move makes the gate refuse an order sized at the cap.", run: checkCapFXHeadroom},
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
	{id: "sweep_minimum_uneconomic", severity: rpc.PolicyCheckWarn, category: rpc.PolicyCheckCategoryEconomics,
		summary: "The sweep's minimum bill order earns less interest to the shortest rung than the commission it pays.", run: checkSweepEconomics},
	{id: "version_not_bumped", severity: rpc.PolicyCheckWarn, category: rpc.PolicyCheckCategoryProvenance,
		summary: "A policy file was edited without a higher policy_version, so the daemon keeps the old policy in force.", run: checkVersionNotBumped},
	{id: "dated_assumption_expired", severity: rpc.PolicyCheckWarn, category: rpc.PolicyCheckCategoryProvenance,
		summary: "A dated assumption has expired: cash_interest_valid_through, or a directional intent's expires_at.", run: checkDatedExpired},
	{id: "dated_assumption_expiring", severity: rpc.PolicyCheckInfo, category: rpc.PolicyCheckCategoryProvenance,
		summary: "A dated assumption ends within 14 days.", run: checkDatedExpiring},
	{id: "file_unreviewed", severity: rpc.PolicyCheckInfo, category: rpc.PolicyCheckCategoryProvenance,
		summary: "A policy file still carries the \"Canary defaults, not yet reviewed\" header.", run: checkFileUnreviewed},
	{id: "compiled_default_in_force", severity: rpc.PolicyCheckInfo, category: rpc.PolicyCheckCategoryProvenance,
		summary: "A limit nobody wrote runs on Canary's compiled default: [trading].max_notional, a sweep currency's keep_cash or min_tranche.", run: checkCompiledDefaults},
	{id: "sweep_cap_exempt", severity: rpc.PolicyCheckInfo, category: rpc.PolicyCheckCategoryContradiction,
		summary: "bills_exempt_from_trading_max_notional makes a sweep cap above the trading cap legitimate; reported so the exemption stays visible.", run: checkSweepExempt},
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
	tradingCap := c.trading.MaxNotional
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
		out = append(out, policyCheckHit{keys: keys,
			message: fmt.Sprintf("[buckets.%s] lets one order reach %s%s, above the trading cap of %s: Canary lists such an order as ready and the trading gate refuses it every time.",
				cp.bucket, policyCheckMoney(cp.base, c.base()), where, policyCheckMoney(tradingCap, c.base())),
			suggestion: fmt.Sprintf("Set the cap to %s, the trading cap in the order's currency rounded down, so every order it sizes can pass the gate; or raise [trading].max_notional yourself if larger orders are intended.",
				policyCheckMoney(suggest, cp.nativeCcy))})
	}
	return out
}

func checkSweepMinimumAboveCap(c *policyCheckContext) []policyCheckHit {
	s := c.sweep()
	if s == nil {
		return nil
	}
	capBase, capKeys, capOK := c.sweepCapBase()
	var out []policyCheckHit
	for _, ccy := range c.sweepCurrencies() {
		cfg := s.currency(ccy)
		fx, fxOK := c.fx(ccy)
		if !fxOK {
			c.skip(fmt.Sprintf("the %s sweep minimum against the caps: no %s rate without a live account", ccy, ccy))
			continue
		}
		minNative := cashSweepMinimum(s, cfg, fx)
		minBase := minNative * fx
		keys := []rpc.PolicyCheckKey{c.sweepCurrencyKey(ccy, "min_tranche", policyCheckMoney(cfg.MinTranche, ccy))}
		if s.MinOrderNotional > 0 {
			keys = append(keys, c.protectionKey("buckets.cash_sweep", "min_order_notional", policyCheckMoney(s.MinOrderNotional, c.base())))
		}
		if capOK && minBase > capBase*(1+1e-9) {
			out = append(out, policyCheckHit{keys: append(slices.Clone(keys), capKeys...),
				message: fmt.Sprintf("The smallest %s sweep order is %s (%s) but the sweep cap allows at most %s: no %s order can be both, so the sweep holds every one.",
					ccy, policyCheckMoney(minNative, ccy), policyCheckMoney(minBase, c.base()), policyCheckMoney(capBase, c.base()), ccy),
				suggestion: fmt.Sprintf("Lower min_tranche for %s to at most %s, or raise the sweep cap above %s.", ccy, policyCheckMoney(policyCheckRoundDown(capBase/fx), ccy), policyCheckMoney(minBase, c.base()))})
		}
		if !c.sweepExempt() && minBase > c.trading.MaxNotional*(1+1e-9) {
			out = append(out, policyCheckHit{keys: append(slices.Clone(keys), c.tradingCapKey()),
				message: fmt.Sprintf("The smallest %s sweep order is %s (%s), above the trading cap of %s: the gate refuses every %s sweep order.",
					ccy, policyCheckMoney(minNative, ccy), policyCheckMoney(minBase, c.base()), policyCheckMoney(c.trading.MaxNotional, c.base()), ccy),
				suggestion: fmt.Sprintf("Lower min_tranche for %s to at most %s.", ccy, policyCheckMoney(policyCheckRoundDown(c.trading.MaxNotional/fx), ccy))})
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
		active = append(active, "[buckets.cash_sweep] mode = active")
	}
	if b := c.protection.Buckets.BudgetReduction; b.enabled() && b.effectiveMode() == rpc.BudgetReductionModeActive {
		active = append(active, "[buckets.budget_reduction] mode = active")
	}
	if pre := c.protection.Authority.PreAuthorised; len(pre) > 0 {
		active = append(active, "[authority].pre_authorised = "+strings.Join(pre, ", "))
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

func checkSweepReserveKeys(c *policyCheckContext) []policyCheckHit {
	var out []policyCheckHit
	bad := func(key, value, why string) {
		out = append(out, policyCheckHit{keys: []rpc.PolicyCheckKey{c.protectionKey("buckets.cash_sweep", key, value)}, message: why})
	}
	for _, k := range []struct {
		key      string
		min, max float64
		openMin  bool
	}{{"reserve_floor_base", 0, math.Inf(1), false}, {"reserve_pct_nlv", 0, 100, false}, {"max_order_pct_nlv", 0, 100, true}} {
		v, present, valid := c.sweepRawNumber(k.key)
		if !present {
			continue
		}
		raw, _ := c.sweepRaw(k.key)
		switch {
		case !valid:
			bad(k.key, fmt.Sprint(raw), fmt.Sprintf("%s must be a number; the sweep cannot read it.", k.key))
		case v < k.min || (k.openMin && v == k.min) || v > k.max:
			rng := fmt.Sprintf("between %s and %s", policyCheckNumber(k.min), policyCheckNumber(k.max))
			if math.IsInf(k.max, 1) {
				rng = "zero or more"
			} else if k.openMin {
				rng = fmt.Sprintf("above %s and at most %s", policyCheckNumber(k.min), policyCheckNumber(k.max))
			}
			bad(k.key, policyCheckNumber(v), fmt.Sprintf("%s is %s; it must be %s.", k.key, policyCheckNumber(v), rng))
		}
	}
	if v, ok := c.sweepRaw("bills_exempt_from_trading_max_notional"); ok {
		if _, isBool := v.(bool); !isBool {
			bad("bills_exempt_from_trading_max_notional", fmt.Sprint(v), "bills_exempt_from_trading_max_notional must be true or false.")
		}
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
	if !c.book.PositionsKnown {
		return nil
	}
	var out []policyCheckHit
	for _, p := range c.book.Positions {
		// A protective stop on a long stock line is exempt from the cap.
		if p.Kind == policyCheckKindStock || p.Quantity == 0 {
			continue
		}
		unit := math.Abs(p.MarketValueBase / p.Quantity)
		if unit <= c.trading.MaxNotional {
			continue
		}
		out = append(out, policyCheckHit{keys: []rpc.PolicyCheckKey{c.tradingCapKey(), {File: "live account", Key: "one " + p.Currency + " option contract", Value: policyCheckMoney(unit, c.base())}},
			message:    fmt.Sprintf("One contract of a held %s option line is worth %s, above the trading cap of %s: no loss exit, budget reduction or close for it can pass the gate, even for a single contract.", p.Currency, policyCheckMoney(unit, c.base()), policyCheckMoney(c.trading.MaxNotional, c.base())),
			suggestion: fmt.Sprintf("Raise [trading].max_notional yourself to at least %s if Canary should be able to exit this line, or plan its exit by hand.", policyCheckMoney(policyCheckRoundUp(unit), c.base()))})
	}
	return out
}

func checkCapFXHeadroom(c *policyCheckContext) []policyCheckHit {
	tradingCap := c.trading.MaxNotional
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
			message: fmt.Sprintf("[buckets.%s] caps one order at %s, within %s of the trading cap of %s; the bucket %s while the gate converts at the session quote, so an FX move of that size refuses an order sized at the cap.",
				cp.bucket, policyCheckMoney(cp.base, c.base()), policyCheckPct(policyCheckFXHeadroom), policyCheckMoney(tradingCap, c.base()), how),
			suggestion: fmt.Sprintf("Set the cap to %s, at most 97%% of the trading cap in the order's currency rounded down, which leaves room for a 3%% FX move.", policyCheckMoney(policyCheckRoundDown(tradingCap*0.97/cp.fx), cp.nativeCcy))})
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
	rows := []capRow{{label: "[trading].max_notional", keys: []rpc.PolicyCheckKey{c.tradingCapKey()}, base: c.trading.MaxNotional, fx: 1, ccy: base}}
	for _, cp := range c.bucketCaps() {
		rows = append(rows, capRow{label: "[buckets." + cp.bucket + "]", keys: cp.keys, base: cp.base, fx: cp.fx, ccy: cp.nativeCcy})
	}
	var out []policyCheckHit
	for _, r := range rows {
		share := r.base / nlv
		switch {
		case share < policyCheckTinyShareNLV:
			want := policyCheckRoundUp(0.05 * nlv)
			if r.label != "[trading].max_notional" {
				want = min(want, c.trading.MaxNotional)
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
		want := min(policyCheckRoundUp(size/policyCheckMaxSplitOrders), c.trading.MaxNotional)
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
	if c.protection.Buckets.TrailingStop.Options.Enabled {
		report([]rpc.PolicyCheckKey{c.tradingCapKey()}, "[trading].max_notional", "a whole-line exit of the largest option line (option exits are not exempt from the cap)",
			largest(policyCheckKindOption), c.trading.MaxNotional, 1, base)
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
	reserve := 0.0
	floor, floorPresent, floorOK := c.sweepRawNumber("reserve_floor_base")
	pct, pctPresent, pctOK := c.sweepRawNumber("reserve_pct_nlv")
	how := ""
	switch {
	case (floorPresent && floorOK) || (pctPresent && pctOK):
		if floorPresent && floorOK {
			reserve = floor
			keys = append(keys, c.protectionKey("buckets.cash_sweep", "reserve_floor_base", policyCheckMoney(floor, base)))
		}
		if pctPresent && pctOK {
			reserve = max(reserve, pct/100*nlv)
			keys = append(keys, c.protectionKey("buckets.cash_sweep", "reserve_pct_nlv", policyCheckNumber(pct)))
		}
		how = "the reserve floor and percentage keep back"
	default:
		for _, ccy := range c.sweepCurrencies() {
			fx, ok := c.fx(ccy)
			if !ok {
				continue
			}
			kc := s.currency(ccy).KeepCash
			reserve += kc * fx
			keys = append(keys, c.sweepCurrencyKey(ccy, "keep_cash", policyCheckMoney(kc, ccy)))
		}
		if s.ReserveCushionEUR != nil {
			if fx, ok := c.fx("EUR"); ok {
				reserve += *s.ReserveCushionEUR * fx
				keys = append(keys, c.protectionKey("buckets.cash_sweep", "reserve_cushion_eur", policyCheckMoney(*s.ReserveCushionEUR, "EUR")))
			}
		}
		how = "keep_cash across the swept currencies keeps back"
	}
	if len(keys) == 0 {
		return nil
	}
	share := reserve / nlv
	switch {
	case share < policyCheckTinyShareNLV:
		return []policyCheckHit{{keys: keys,
			message:    fmt.Sprintf("Together %s %s, %s of NLV (%s): a margin call, an assignment or a settling buy can need more cash than that before a bill matures.", how, policyCheckMoney(reserve, base), policyCheckPct(share), policyCheckMoney(nlv, base)),
			suggestion: fmt.Sprintf("Keep at least %s (2%% of NLV, rounded up) as cash across the currencies you trade; a fixed amount that ignores NLV drifts out of proportion as the book changes.", policyCheckMoney(policyCheckRoundUp(policyCheckTinyShareNLV*nlv), base))}}
	case share > policyCheckHugeShareNLV:
		return []policyCheckHit{{keys: keys,
			message:    fmt.Sprintf("Together %s %s, %s of NLV (%s): the sweep can hardly put any idle cash to work.", how, policyCheckMoney(reserve, base), policyCheckPct(share), policyCheckMoney(nlv, base)),
			suggestion: fmt.Sprintf("About %s (10%% of NLV) is a common working reserve.", policyCheckMoney(policyCheckRoundUp(0.10*nlv), base))}}
	}
	return nil
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
		fx, fxOK := c.fx(ccy)
		minNative := cfg.MinTranche
		keys := []rpc.PolicyCheckKey{c.sweepCurrencyKey(ccy, "min_tranche", policyCheckMoney(cfg.MinTranche, ccy)), c.sweepCurrencyKey(ccy, "min_maturity_days", fmt.Sprint(cfg.MinMaturityDays))}
		minKey := "min_tranche"
		if s.MinOrderNotional > 0 {
			if !fxOK {
				c.skip(fmt.Sprintf("min_order_notional in the %s sweep economics: no %s rate without a live account (min_tranche used)", ccy, ccy))
			} else if m := cashSweepMinimum(s, cfg, fx); m > minNative {
				minNative, minKey = m, "min_order_notional"
				keys = append(keys, c.protectionKey("buckets.cash_sweep", "min_order_notional", policyCheckMoney(s.MinOrderNotional, c.base())))
			}
		}
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
		if s.MinNetGain > 0 && fxOK {
			threshold += s.MinNetGain / fx
			keys = append(keys, c.protectionKey("buckets.cash_sweep", "min_net_gain", policyCheckMoney(s.MinNetGain, c.base())))
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
			suggestion = fmt.Sprintf("Set %s to at least %s, twice the breakeven of %s, so the commission takes at most half the interest to a %d-day bill",
				minKey, policyCheckMoney(want, ccy), policyCheckMoney(breakeven, ccy), cfg.MinMaturityDays)
			if minKey == "min_order_notional" && fxOK {
				suggestion = fmt.Sprintf("Set min_order_notional to at least %s (%s), twice the breakeven of %s, so the commission takes at most half the interest to a %d-day bill",
					policyCheckMoney(policyCheckRoundUp(want*fx), c.base()), policyCheckMoney(want, ccy), policyCheckMoney(breakeven, ccy), cfg.MinMaturityDays)
			}
			if longer := 91.0; days < longer {
				suggestion += fmt.Sprintf("; or raise min_maturity_days: at %d days the breakeven falls to %s", int(longer), policyCheckMoney(threshold/(net*longer/365-a.pctCommission), ccy))
			}
			suggestion += "."
		}
		out = append(out, policyCheckHit{keys: keys,
			message: fmt.Sprintf("The smallest %s sweep order, %s in a %d-day bill, earns about %s over cash at an assumed %s a year, less than the assumed %s commission: the smallest bill buy the sweep places loses money.",
				ccy, policyCheckMoney(minNative, ccy), cfg.MinMaturityDays, policyCheckMoney(interest, ccy), policyCheckPct(net), policyCheckMoney(commission, ccy)),
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
	if s := c.protection.Buckets.CashSweep; s != nil {
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
	if c.capSource == PolicyCheckCapFromDefault {
		out = append(out, policyCheckHit{keys: []rpc.PolicyCheckKey{c.tradingCapKey()},
			message: fmt.Sprintf("[trading].max_notional runs on Canary's compiled default of %s, a fixed amount that does not follow the size of the book.", policyCheckNumber(c.trading.MaxNotional))})
	}
	s := c.sweep()
	if s == nil {
		return out
	}
	for _, ccy := range c.sweepCurrencies() {
		var unwritten []rpc.PolicyCheckKey
		cfg := s.currency(ccy)
		for _, k := range []struct {
			key string
			v   float64
		}{{"keep_cash", cfg.KeepCash}, {"min_tranche", cfg.MinTranche}} {
			if c.protectionMD == nil || !c.protectionMD.IsDefined("buckets", "cash_sweep", "currency", ccy, k.key) {
				unwritten = append(unwritten, c.sweepCurrencyKey(ccy, k.key, policyCheckMoney(k.v, ccy)))
			}
		}
		if len(unwritten) == 0 {
			continue
		}
		out = append(out, policyCheckHit{keys: unwritten,
			message: fmt.Sprintf("The %s sweep keeps cash and sizes its smallest order on Canary's compiled defaults, fixed amounts chosen for no particular book.", ccy)})
	}
	return out
}

func checkSweepExempt(c *policyCheckContext) []policyCheckHit {
	if c.sweep() == nil || !c.sweepExempt() {
		return nil
	}
	capBase, keys, ok := c.sweepCapBase()
	exempt := c.protectionKey("buckets.cash_sweep", "bills_exempt_from_trading_max_notional", "true")
	if !ok || capBase <= c.trading.MaxNotional {
		return []policyCheckHit{{keys: []rpc.PolicyCheckKey{exempt},
			message: "Bill orders are declared exempt from [trading].max_notional, but the sweep cap is within the trading cap, so the exemption is not used."}}
	}
	return []policyCheckHit{{keys: append(append(keys, exempt), c.tradingCapKey()),
		message: fmt.Sprintf("The sweep cap of %s is above the trading cap of %s; bills_exempt_from_trading_max_notional declares bill orders exempt, so the gap is intended.", policyCheckMoney(capBase, c.base()), policyCheckMoney(c.trading.MaxNotional, c.base()))}}
}
