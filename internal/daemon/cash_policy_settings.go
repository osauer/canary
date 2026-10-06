package daemon

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/osauer/canary/v2/internal/rpc"
)

// Cash policy settings (owner requirement 2026-10-06 13:35 CEST: the cash
// management settings can be changed from Desk's Settings as well as in the
// file, with Save, Cancel and Reset to Canary defaults). The protection policy
// file stays Canary's: policy.cash.get reads every key in scope with its value,
// where the value comes from, Canary's written default, its bounds and help;
// policy.cash.check validates a draft with the loader's own rules and writes
// nothing; policy.cash.apply is the only write. The methods are in no
// catalogue: Desk's console reaches them after the owner's device confirms each
// save; no MCP tool, CLI command or agent grant does.
//
// The owner answered the design's questions (2026-10-06 15:31 CEST); each
// answer lives in one place:
//  1. a save needs a confirmation reference: the owner's device, or an
//     earlier save the device confirmed in the same Desk console session
//     inside [cash] confirmation_window, which is file-only (review decision
//     2026-10-06 16:56 CEST); a save that lets more reach the broker always
//     needs the device (cashPolicyReliance), and so does writing a number the
//     file had none of (cashPolicyConsequences);
//  2. [cash] pre_authorised stays file-only: it is not in cashPolicySpecs;
//  3. while the file pre-authorises the sweep, a save may still switch the
//     sweep on or move it to active; the first consequence then says that
//     the daemon sends the sweep's orders itself after the veto window, and
//     the cap in force (cashPolicyDaemonSends);
//  4. Reset restores Canary's written numbers and rules only, never a switch,
//     the mode or a per-currency entry (cashPolicySpec.reset);
//  6. bill ISIN lists, the ETF fallback and tax_reviewed_at stay in the file:
//     they are not in cashPolicySpecs.

// cashPolicySpec is one setting in scope.
type cashPolicySpec struct {
	section string
	leaf    string
	// perCurrency keys live in [cash.<section>.currency.<CCY>].
	perCurrency bool
	label, help string
	typ, unit   string
	choices     []string
	choiceNames []string
	min, max    *float64
	exclusive   bool
	// absent is the source of a key the file leaves out; builtin is the
	// value that then applies (canary_default keys only).
	absent  string
	builtin any
	// removable keys a change may remove with null.
	removable bool
	// more reports whether moving from → to lets more reach the broker; nil
	// means no direction does.
	more func(from, to any) bool
}

// cashPolicySpecs is every setting the screen edits, in screen order. Keys
// left out stay file-only: [cash] pre_authorised, currency_priority (the
// runtime cash priority overrides it), reserve_cushion_eur, the advisory
// min_net_gain and tax_reviewed_at, and every per-currency key but the
// sweep's keep_cash and leveling's deliberate_carry (instruments, fallback
// ETF, ISIN lists, maturities, the settlement route).
var cashPolicySpecs = []cashPolicySpec{
	{section: rpc.CashPolicySectionLeveling, leaf: "enabled", label: "Currency leveling", typ: rpc.CashPolicyTypeBool,
		help:   "Repays a borrowed currency from the currencies whose cash earns least.",
		absent: rpc.CashPolicySourceCanaryDefault, builtin: false, more: cashPolicyTurnsOn},
	{section: rpc.CashPolicySectionLeveling, leaf: "trigger_base", label: "Band", typ: rpc.CashPolicyTypeNumber, unit: rpc.CashPolicyUnitBase,
		help: "Loans smaller than this are left alone.", min: new(0.0), exclusive: true,
		absent: rpc.CashPolicySourceNotWritten, more: cashPolicyFalls},
	{section: rpc.CashPolicySectionLeveling, leaf: "cushion_base", label: "Cushion", typ: rpc.CashPolicyTypeNumber, unit: rpc.CashPolicyUnitBase,
		help: "A repaid currency ends between zero and this; a currency that pays keeps this much.", min: new(0.0),
		absent: rpc.CashPolicySourceNotWritten, more: cashPolicyRises},
	{section: rpc.CashPolicySectionLeveling, leaf: "max_slippage_bp", label: "Limit from the mid", typ: rpc.CashPolicyTypeNumber, unit: rpc.CashPolicyUnitBP,
		help: "The conversion's limit sits at most this far from the live mid; a wider market is not traded.", min: new(0.0), exclusive: true, max: new(currencyLevelingMaxSlippageBP),
		absent: rpc.CashPolicySourceNotWritten, more: cashPolicyRises},
	{section: rpc.CashPolicySectionLeveling, leaf: "payback_days", label: "Pays back within", typ: rpc.CashPolicyTypeInteger, unit: rpc.CashPolicyUnitDays,
		help: "A conversion must save more interest than its worst-case cost within this many days.", min: new(1.0), max: new(float64(currencyLevelingMaxPaybackDays)),
		absent: rpc.CashPolicySourceNotWritten, more: cashPolicyRises},
	{section: rpc.CashPolicySectionLeveling, leaf: "deliberate_carry", perCurrency: true, label: "Keep borrowed on purpose", typ: rpc.CashPolicyTypeBool,
		help:   "Leveling never repays this currency and never spends it.",
		absent: rpc.CashPolicySourceCanaryDefault, builtin: false, more: cashPolicyTurnsOff},
	{section: rpc.CashPolicySectionSweep, leaf: "enabled", label: "Cash sweep", typ: rpc.CashPolicyTypeBool,
		help:   "Keeps a reserve as cash and buys bills of the same currency with the rest; never converts.",
		absent: rpc.CashPolicySourceCanaryDefault, builtin: false, more: cashPolicyTurnsOn},
	{section: rpc.CashPolicySectionSweep, leaf: "mode", label: "Orders", typ: rpc.CashPolicyTypeChoice,
		choices: []string{rpc.CashSweepModeShadow, rpc.CashSweepModeActive}, choiceNames: []string{"Observe only", "Propose orders"},
		help:   "Observe only lists and logs the bills it would buy; Propose orders makes each one a proposal for you to approve.",
		absent: rpc.CashPolicySourceCanaryDefault, builtin: rpc.CashSweepModeShadow,
		more: func(from, to any) bool { return from != rpc.CashSweepModeActive && to == rpc.CashSweepModeActive }},
	{section: rpc.CashPolicySectionSweep, leaf: "reserve_floor_base", label: "Reserve, at least", typ: rpc.CashPolicyTypeNumber, unit: rpc.CashPolicyUnitBase,
		help: "Cash kept uninvested, held in the base currency first.", min: new(0.0),
		absent: rpc.CashPolicySourceNotWritten, more: cashPolicyFalls},
	{section: rpc.CashPolicySectionSweep, leaf: "reserve_pct_nlv", label: "Reserve, share of NLV", typ: rpc.CashPolicyTypeNumber, unit: rpc.CashPolicyUnitPctNLV,
		help: "The reserve is the larger of this share and the amount above.", min: new(0.0), max: new(100.0),
		absent: rpc.CashPolicySourceNotWritten, more: cashPolicyFalls},
	{section: rpc.CashPolicySectionSweep, leaf: "min_order_notional", label: "Smallest buy", typ: rpc.CashPolicyTypeNumber, unit: rpc.CashPolicyUnitBase,
		help: "Below this nothing is bought; a sale that restores cash can be smaller.", min: new(0.0),
		absent: rpc.CashPolicySourceNotWritten, more: cashPolicyFalls},
	{section: rpc.CashPolicySectionSweep, leaf: "max_order_notional", label: "Largest order", typ: rpc.CashPolicyTypeNumber, unit: rpc.CashPolicyUnitBase,
		help: "Each order, buy or sale, is at most the larger of this and the share below.", min: new(0.0), exclusive: true,
		absent: rpc.CashPolicySourceNotWritten, more: cashPolicyRises},
	{section: rpc.CashPolicySectionSweep, leaf: "max_order_pct_nlv", label: "Largest order, share of NLV", typ: rpc.CashPolicyTypeNumber, unit: rpc.CashPolicyUnitPctNLV,
		help: "Raises the largest order as NLV grows; 0 keeps the fixed amount.", min: new(0.0), max: new(100.0),
		absent: rpc.CashPolicySourceNotWritten, more: cashPolicyRises},
	{section: rpc.CashPolicySectionSweep, leaf: "no_buy_while_borrowed", label: "No bill buys while a currency is borrowed", typ: rpc.CashPolicyTypeBool,
		help:   "Holds every bill buy while any currency's cash is negative; sales still go ahead.",
		absent: rpc.CashPolicySourceNotWritten, more: cashPolicyTurnsOff},
	{section: rpc.CashPolicySectionSweep, leaf: "bills_exempt_from_trading_max_notional", label: "Bills may pass the order cap", typ: rpc.CashPolicyTypeBool,
		help:   "A same-currency bill order may exceed the order cap in force, up to the sweep's largest order.",
		absent: rpc.CashPolicySourceCanaryDefault, builtin: false, more: cashPolicyTurnsOn},
	{section: rpc.CashPolicySectionSweep, leaf: "order_step_base", label: "Order step", typ: rpc.CashPolicyTypeNumber, unit: rpc.CashPolicyUnitBase,
		help: "Orders are sized in whole steps, so an approval stays valid while NLV moves; 0 means no steps.", min: new(0.0),
		absent: rpc.CashPolicySourceNotWritten},
	{section: rpc.CashPolicySectionSweep, leaf: "keep_cash", label: "Settlement float", typ: rpc.CashPolicyTypeNumber, unit: rpc.CashPolicyUnitOwn,
		help: "Kept as cash in every currency, in that currency's own unit, unless the currency sets its own.", min: new(0.0),
		absent: rpc.CashPolicySourceNotWritten, more: cashPolicyFalls},
	{section: rpc.CashPolicySectionSweep, leaf: "keep_cash", perCurrency: true, label: "Settlement float", typ: rpc.CashPolicyTypeNumber, unit: rpc.CashPolicyUnitOwn,
		help: "This currency's own float; leave it empty to use the common one.", min: new(0.0),
		absent: rpc.CashPolicySourceCanaryDefault, removable: true, more: cashPolicyFalls},
}

func cashPolicyTurnsOn(from, to any) bool  { return from != true && to == true }
func cashPolicyTurnsOff(from, to any) bool { return from != false && to == false }

// cashPolicyFalls and cashPolicyRises compare two numbers; a number written
// where none was is not a direction (the feature was waiting for it).
func cashPolicyFalls(from, to any) bool {
	f, fok := cashPolicyNumber(from)
	t, tok := cashPolicyNumber(to)
	return fok && tok && t < f
}

func cashPolicyRises(from, to any) bool {
	f, fok := cashPolicyNumber(from)
	t, tok := cashPolicyNumber(to)
	return fok && tok && t > f
}

func cashPolicyNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	}
	return 0, false
}

// cashPolicyWritten is the value Canary writes for each key: policy
// ensure's written defaults (currencyLevelingWrittenDefaults,
// cashSweepWrittenDefaults), read once from their TOML literals.
var cashPolicyWritten = func() map[string]any {
	out := map[string]any{}
	for prefix, list := range map[string][]cashSweepWrittenDefault{"cash.leveling.": currencyLevelingWrittenDefaults, "cash.sweep.": cashSweepWrittenDefaults} {
		for _, d := range list {
			var probe struct{ V any }
			if _, err := toml.Decode("V = "+d.value, &probe); err != nil {
				panic("cash policy written default " + d.key + ": " + err.Error())
			}
			if n, ok := probe.V.(int64); ok {
				probe.V = int(n)
			}
			out[prefix+d.key] = probe.V
		}
	}
	return out
}()

// key names the setting, with ccy for a per-currency one.
func (sp cashPolicySpec) key(ccy string) string {
	if sp.perCurrency {
		return "cash." + sp.section + ".currency." + ccy + "." + sp.leaf
	}
	return "cash." + sp.section + "." + sp.leaf
}

// canary is the value Canary writes for the key: its written default, else
// the built-in value that applies when the key is left out.
func (sp cashPolicySpec) canary() any {
	if v, ok := cashPolicyWritten[sp.key("")]; ok && !sp.perCurrency {
		return v
	}
	return sp.builtin
}

// reset reports whether a section's Reset to Canary defaults restores the
// key: the numbers and rules Canary writes, never a switch, the sweep's mode
// or a per-currency entry (owner question 4, 2026-10-06).
func (sp cashPolicySpec) reset() bool {
	_, written := cashPolicyWritten[sp.key("")]
	return written && !sp.perCurrency && sp.leaf != "enabled"
}

// get reads the key from a parsed policy: its value, and whether the policy
// carries one (a table that is present, a pointer that is set).
func (sp cashPolicySpec) get(p protectionPolicy, ccy string) (any, bool) {
	if sp.section == rpc.CashPolicySectionLeveling {
		l := p.Cash.Leveling
		if l == nil {
			return nil, false
		}
		switch sp.leaf {
		case "enabled":
			return l.Enabled, true
		case "trigger_base":
			return cashPolicyPtr(l.TriggerBase)
		case "cushion_base":
			return cashPolicyPtr(l.CushionBase)
		case "max_slippage_bp":
			return cashPolicyPtr(l.MaxSlippageBP)
		case "payback_days":
			if l.PaybackDays == nil {
				return nil, false
			}
			return *l.PaybackDays, true
		case "deliberate_carry":
			c, ok := l.Currency[ccy]
			return c.DeliberateCarry, ok
		}
		return nil, false
	}
	s := p.Cash.Sweep
	if s == nil {
		return nil, false
	}
	switch sp.leaf {
	case "enabled":
		return s.Enabled, true
	case "mode":
		if strings.TrimSpace(s.Mode) == "" {
			return nil, false
		}
		return s.effectiveMode(), true
	case "reserve_floor_base":
		return cashPolicyPtr(s.ReserveFloorBase)
	case "reserve_pct_nlv":
		return cashPolicyPtr(s.ReservePctNLV)
	case "min_order_notional":
		return cashPolicyPtr(s.MinOrderNotional)
	case "max_order_notional":
		if s.MaxOrderNotional == 0 {
			return nil, false
		}
		return s.MaxOrderNotional, true
	case "max_order_pct_nlv":
		return cashPolicyPtr(s.MaxOrderPctNLV)
	case "no_buy_while_borrowed":
		if s.NoBuyWhileBorrowed == nil {
			return nil, false
		}
		return *s.NoBuyWhileBorrowed, true
	case "bills_exempt_from_trading_max_notional":
		if s.BillsExemptFromTradingMaxNotional == nil {
			return nil, false
		}
		return *s.BillsExemptFromTradingMaxNotional, true
	case "order_step_base":
		return cashPolicyPtr(s.OrderStepBase)
	case "keep_cash":
		if sp.perCurrency {
			c, ok := s.Currency[ccy]
			if !ok {
				return nil, false
			}
			return cashPolicyPtr(c.KeepCash)
		}
		return cashPolicyPtr(s.KeepCash)
	}
	return nil, false
}

func cashPolicyPtr(v *float64) (any, bool) {
	if v == nil {
		return nil, false
	}
	return *v, true
}

// effective is what applies for the key: the policy's value when it carries
// one, else the built-in value (nil for a key the feature waits for).
func (sp cashPolicySpec) effective(p protectionPolicy, ccy string) any {
	if v, ok := sp.get(p, ccy); ok {
		return v
	}
	return sp.builtin
}

// valueText is a value in the review's words: On or Off, a choice's label,
// a number with its unit; nil is a per-currency float's common value, or
// the key left out of the file.
func (sp cashPolicySpec) valueText(v any, base, ccy string) string {
	switch x := v.(type) {
	case nil:
		if sp.removable {
			return "the common float"
		}
		return "not in the file"
	case bool:
		if x {
			return "On"
		}
		return "Off"
	case string:
		if i := slices.Index(sp.choices, x); i >= 0 && i < len(sp.choiceNames) {
			return sp.choiceNames[i]
		}
		return x
	}
	n, _ := cashPolicyNumber(v)
	text := policyCheckNumber(n)
	switch sp.unit {
	case rpc.CashPolicyUnitBase:
		return text + " " + nonEmptyString(base, "in base currency")
	case rpc.CashPolicyUnitOwn:
		if ccy != "" {
			return text + " " + ccy
		}
		return text + " in each currency's own unit"
	case rpc.CashPolicyUnitPctNLV:
		return text + "% of NLV"
	case rpc.CashPolicyUnitBP:
		return text + " bp"
	case rpc.CashPolicyUnitDays:
		return text + " days"
	}
	return text
}

// fromText is the value before a change: as written, or what applies while
// the file leaves the key out.
func (sp cashPolicySpec) fromText(v any, source, base, ccy string) string {
	if source == rpc.CashPolicySourceFile || v == nil {
		return sp.valueText(v, base, ccy)
	}
	return sp.valueText(v, base, ccy) + " (not in the file)"
}

// cashPolicySpecFor parses a dotted key in scope: its spec and currency.
func cashPolicySpecFor(key string) (cashPolicySpec, string, bool) {
	for _, section := range []string{rpc.CashPolicySectionLeveling, rpc.CashPolicySectionSweep} {
		rest, ok := strings.CutPrefix(key, "cash."+section+".")
		if !ok {
			continue
		}
		ccy, leaf := "", rest
		if after, ok := strings.CutPrefix(rest, "currency."); ok {
			code, l, ok := strings.Cut(after, ".")
			if !ok || !cashPolicyCurrencyCode(code) {
				return cashPolicySpec{}, "", false
			}
			ccy, leaf = code, l
		}
		for _, sp := range cashPolicySpecs {
			if sp.section == section && sp.leaf == leaf && sp.perCurrency == (ccy != "") {
				return sp, ccy, true
			}
		}
	}
	return cashPolicySpec{}, "", false
}

// cashPolicyCurrencyCode accepts a table name the loader accepts: three
// capital letters.
func cashPolicyCurrencyCode(ccy string) bool {
	return len(ccy) == 3 && strings.IndexFunc(ccy, func(r rune) bool { return r < 'A' || r > 'Z' }) < 0
}

// cashPolicyBook is what the facts are worked out from: the account's base
// currency, net liquidation value and per-currency trade-date cash, the order
// cap in force, the interest rates from the broker's statements and, while
// leveling is on, its planned repayments. Each *Reason says why an input is
// unavailable.
type cashPolicyBook struct {
	at             time.Time
	base           string
	nlv            float64
	cash, fx       map[string]float64
	ledgerReason   string
	orderCap       float64
	orderCapReason string
	rates          map[string]currencyLevelingRate
	ratesThrough   string
	ratesReason    string
	leveling       *rpc.TradeProposalCurrencyLevelingStatus
	check          *PolicyCheckBook
}

// borrowed lists the currencies whose trade-date cash is negative by more
// than one unit, most borrowed (in base) first.
func (b cashPolicyBook) borrowed() []string {
	var out []string
	for ccy, cash := range b.cash {
		if cash < -rpc.CashSweepBorrowedToleranceUnits {
			out = append(out, ccy)
		}
	}
	slices.SortFunc(out, func(a, c string) int {
		return int(math.Copysign(1, b.base0(a)-b.base0(c)))
	})
	return out
}

// base0 is ccy's cash in base, or 0 when its rate is unknown.
func (b cashPolicyBook) base0(ccy string) float64 {
	if r, ok := b.fx[ccy]; ok {
		return b.cash[ccy] * r
	}
	return 0
}

// currencies lists the rows: every ledger currency and every currency with a
// table in the file, base currency first. Without a ledger, Canary's display
// currencies stand in so deliberate_carry stays reachable.
func (b cashPolicyBook) currencies(p protectionPolicy) []string {
	set := map[string]bool{}
	for ccy := range b.cash {
		set[ccy] = true
	}
	if l := p.Cash.Leveling; l != nil {
		for ccy := range l.Currency {
			set[ccy] = true
		}
	}
	if s := p.Cash.Sweep; s != nil {
		for ccy := range s.Currency {
			set[ccy] = true
		}
	}
	if len(b.cash) == 0 {
		for _, ccy := range currencyLevelingDisplayCurrencies {
			set[ccy] = true
		}
	}
	if b.base != "" {
		set[b.base] = true
	}
	out := slices.Sorted(maps.Keys(set))
	if i := slices.Index(out, b.base); i > 0 {
		out = append([]string{b.base}, slices.Delete(out, i, i+1)...)
	}
	return out
}

// cashPolicySettingsFor lists every key in scope for p. defined reports
// whether the file (or, for a read-only view, the policy in force) writes a
// key.
func cashPolicySettingsFor(p protectionPolicy, defined func(key string) bool, currencies []string, facts map[string]string) []rpc.CashPolicySetting {
	var out []rpc.CashPolicySetting
	for _, sp := range cashPolicySpecs {
		ccys := []string{""}
		if sp.perCurrency {
			ccys = currencies
		}
		for _, ccy := range ccys {
			key := sp.key(ccy)
			row := rpc.CashPolicySetting{Key: key, Section: sp.section, Currency: ccy, Label: sp.label, Help: sp.help, Type: sp.typ,
				Choices: sp.choices, ChoiceLabels: sp.choiceNames, Unit: sp.unit, Min: sp.min, MinExclusive: sp.exclusive, Max: sp.max,
				Default: sp.canary(), Reset: sp.reset(), Removable: sp.removable, Fact: facts[key], Source: sp.absent, Value: sp.builtin}
			if defined(key) {
				if v, ok := sp.get(p, ccy); ok {
					row.Value, row.Source = v, rpc.CashPolicySourceFile
				}
			}
			out = append(out, row)
		}
	}
	return out
}

// cashPolicyDefinedIn reports what a parsed policy carries, for a view of
// the policy in force (drift, refused, missing), whose file metadata is not
// the one Canary runs.
func cashPolicyDefinedIn(p protectionPolicy) func(string) bool {
	return func(key string) bool {
		sp, ccy, ok := cashPolicySpecFor(key)
		if !ok {
			return false
		}
		_, set := sp.get(p, ccy)
		return set
	}
}

// cashPolicyFacts works out the line beside each setting and each section's
// header lines from p (the file, or a draft) and the book. A missing input
// gives Canary's sentence saying why.
func cashPolicyFacts(p protectionPolicy, b cashPolicyBook) (map[string]string, rpc.CashPolicySections) {
	facts := map[string]string{}
	var sections rpc.CashPolicySections
	money := func(v float64) string { return cashPolicyMoney(v, b.base) }

	lev := sections.Leveling
	switch {
	case b.ratesThrough != "":
		lev.Fact = "Interest rates from the broker's statements, to " + cashPolicyDay(b.ratesThrough) + "."
	case b.ratesReason != "":
		lev.Fact = sentence(b.ratesReason) + ", so leveling would wait."
	}
	lev.Authority = "You approve each repayment; it is never sent on its own."
	if b.orderCapReason == "" && b.orderCap > 0 {
		lev.Cap = "One repayment at most " + money(b.orderCap) + ": the order cap in force."
	} else {
		lev.Cap = "The order cap in force cannot be read now, so leveling would wait."
	}
	sections.Leveling = lev

	l := p.Cash.Leveling
	if l == nil {
		l = &protectionCurrencyLevelingPolicy{}
	}
	shown := b.levelingShown()
	if l.TriggerBase != nil {
		facts["cash.leveling.trigger_base"] = cashPolicyBandFact(*l.TriggerBase, b, shown, l)
	}
	if l.CushionBase != nil && len(shown) > 0 {
		facts["cash.leveling.cushion_base"] = "About " + cashPolicyApprox(*l.CushionBase/b.fx[shown[0]]) + " " + shown[0] + "."
	}
	if l.Enabled && b.leveling != nil && len(b.leveling.Bundles) > 0 {
		var parts []string
		for _, bundle := range b.leveling.Bundles {
			parts = append(parts, fmt.Sprintf("%s now: saves about %s within %d days, costs at most %s.", bundle.Currency, money(bundle.SavingBase), bundle.PaybackDays, money(math.Ceil(bundle.CostBase))))
		}
		facts["cash.leveling.payback_days"] = strings.Join(parts, " ")
	}

	sweep := sections.Sweep
	if p.preAuthorised(preAuthorisedBucketCashSweep) {
		sweep.PreAuthorised = true
		sweep.Authority = "The daemon sends sweep orders itself " + cashPolicyDuration(p.Authority.vetoWindow()) + " after announcing them, because the file pre-authorises the sweep. Change this in the file: [cash] pre_authorised."
	} else {
		sweep.Authority = "You approve each sweep order."
	}
	sections.Sweep = sweep

	s := p.Cash.Sweep
	if s == nil {
		s = &protectionCashSweepPolicy{}
	}
	if reserve := cashPolicyReserveFact(s, b); reserve != "" {
		facts["cash.sweep.reserve_floor_base"], facts["cash.sweep.reserve_pct_nlv"] = reserve, reserve
	}
	if orders := cashPolicyOrderFact(s, b); orders != "" {
		for _, k := range []string{"min_order_notional", "max_order_notional", "max_order_pct_nlv"} {
			facts["cash.sweep."+k] = orders
		}
	}
	switch borrowed := b.borrowed(); {
	case b.ledgerReason != "":
		facts["cash.sweep.no_buy_while_borrowed"] = "Whether a currency is borrowed is unknown now: " + b.ledgerReason + "."
	case len(borrowed) == 0:
		facts["cash.sweep.no_buy_while_borrowed"] = "No currency is borrowed now."
	case s.NoBuyWhileBorrowed != nil && !*s.NoBuyWhileBorrowed:
		facts["cash.sweep.no_buy_while_borrowed"] = cashPolicyList(borrowed) + cashPolicyIsAre(borrowed) + " borrowed now; bill buys go ahead."
	default:
		facts["cash.sweep.no_buy_while_borrowed"] = cashPolicyList(borrowed) + cashPolicyIsAre(borrowed) + " borrowed now, so bill buys wait."
	}
	if b.orderCapReason == "" && b.orderCap > 0 {
		facts["cash.sweep.bills_exempt_from_trading_max_notional"] = "Order cap in force: " + money(b.orderCap) + "."
	} else {
		facts["cash.sweep.bills_exempt_from_trading_max_notional"] = "The order cap in force cannot be read now."
	}
	return facts, sections
}

// levelingShown is the currencies the band is shown in: those borrowed now
// other than the base currency, else the first other ledger currency (USD
// first), each with a known rate.
func (b cashPolicyBook) levelingShown() []string {
	var out []string
	for _, ccy := range b.borrowed() {
		if ccy != b.base && b.fx[ccy] > 0 {
			out = append(out, ccy)
		}
	}
	if len(out) > 0 {
		return out[:min(len(out), 2)]
	}
	others := slices.Sorted(maps.Keys(b.fx))
	slices.SortStableFunc(others, func(a, c string) int {
		switch {
		case a == "USD":
			return -1
		case c == "USD":
			return 1
		}
		return 0
	})
	for _, ccy := range others {
		if ccy != b.base && b.fx[ccy] > 0 {
			return []string{ccy}
		}
	}
	return nil
}

// cashPolicyBandFact states the band in the currencies shown and which loans
// are beyond it now.
func cashPolicyBandFact(band float64, b cashPolicyBook, shown []string, l *protectionCurrencyLevelingPolicy) string {
	var parts []string
	for _, ccy := range shown {
		parts = append(parts, cashPolicyApprox(band/b.fx[ccy])+" "+ccy)
	}
	text := ""
	if len(parts) > 0 {
		text = "About " + strings.Join(parts, ", ") + "."
	}
	if b.ledgerReason != "" {
		return strings.TrimSpace(text)
	}
	var beyond, inside []string
	for _, ccy := range b.borrowed() {
		if l.deliberateCarry(ccy) {
			continue
		}
		owed := -b.base0(ccy)
		switch {
		case b.fx[ccy] <= 0:
		case owed > band:
			beyond = append(beyond, ccy)
		default:
			inside = append(inside, cashPolicyMoney(-b.cash[ccy], ccy))
		}
	}
	switch {
	case len(beyond) > 0:
		text += " " + cashPolicyList(beyond) + cashPolicyIsAre(beyond) + " borrowed beyond it now."
	case len(inside) > 0:
		text += " Borrowed inside the band now: " + strings.Join(inside, ", ") + "; leveling leaves it."
	default:
		text += " No currency is borrowed beyond it now."
	}
	return strings.TrimSpace(text)
}

// cashPolicyReserveFact states the reserve at today's NLV.
func cashPolicyReserveFact(s *protectionCashSweepPolicy, b cashPolicyBook) string {
	if s.ReserveFloorBase == nil && s.ReservePctNLV == nil {
		return ""
	}
	floor, pct := policyCheckDeref(s.ReserveFloorBase), policyCheckDeref(s.ReservePctNLV)
	if b.nlv <= 0 {
		return "At least " + cashPolicyMoney(floor, b.base) + "; NLV is unknown now."
	}
	share := pct / 100 * b.nlv
	if share > floor {
		return "Now " + cashPolicyMoney(share, b.base) + ": " + policyCheckNumber(pct) + "% of NLV."
	}
	return "Now " + cashPolicyMoney(floor, b.base) + ": the amount above, more than " + policyCheckNumber(pct) + "% of NLV."
}

// cashPolicyOrderFact states the order range at today's NLV.
func cashPolicyOrderFact(s *protectionCashSweepPolicy, b cashPolicyBook) string {
	if s.MaxOrderNotional <= 0 {
		return ""
	}
	top := s.MaxOrderNotional
	if pct := policyCheckDeref(s.MaxOrderPctNLV); pct > 0 {
		if b.nlv <= 0 {
			return "At most " + cashPolicyMoney(top, b.base) + " per order, more as NLV grows; NLV is unknown now."
		}
		top = max(top, pct/100*b.nlv)
	}
	if s.MinOrderNotional == nil {
		return "Now at most " + cashPolicyMoney(top, b.base) + " per order."
	}
	return "Now " + policyCheckNumber(math.Round(*s.MinOrderNotional)) + " to " + cashPolicyMoney(top, b.base) + " per order."
}

// cashPolicyCurrencyRows lists each currency's cash, pair, rates and what
// the sweep buys in it.
func cashPolicyCurrencyRows(p protectionPolicy, b cashPolicyBook, currencies []string) []rpc.CashPolicyCurrency {
	out := make([]rpc.CashPolicyCurrency, 0, len(currencies))
	for _, ccy := range currencies {
		row := rpc.CashPolicyCurrency{Currency: ccy, Buys: cashPolicyBuys(p.Cash.Sweep.currency(ccy))}
		_, row.Pair = orderFXBasePriority[ccy]
		if cash, ok := b.cash[ccy]; ok {
			row.Cash = new(cash)
		}
		if r, ok := b.rates[ccy]; ok {
			if v, through, bound, ok := r.loanRate(); ok {
				row.LoanRate, row.LoanRateThrough, row.LoanRateStandIn = new(v), through, bound
			}
			if v, through, bound, ok := r.cashRate(); ok {
				row.CashRate, row.CashRateThrough, row.CashRateStandIn = new(v), through, bound
			}
		}
		out = append(out, row)
	}
	return out
}

// cashPolicyBuys summarises what the sweep may buy in a currency.
func cashPolicyBuys(c protectionCashSweepCurrency) string {
	if len(c.Instruments) == 0 || slices.Equal(c.Instruments, []string{cashSweepInstrumentNone}) {
		return "Kept as cash"
	}
	names := map[string]string{cashSweepInstrumentUSTBill: "US Treasury bills", cashSweepInstrumentDEBubill: "Bubills", cashSweepInstrumentFRBTF: "BTFs",
		cashSweepInstrumentUKTBill: "UK Treasury bills", cashSweepInstrumentCATBill: "Canadian Treasury bills"}
	var parts []string
	listed := false
	for _, instrument := range c.Instruments {
		if instrument == cashSweepInstrumentETF {
			parts = append(parts, "the ETF "+nonEmptyString(c.ETFSymbol, "(no symbol in the file)"))
			continue
		}
		parts = append(parts, names[instrument])
		listed = listed || instrument != cashSweepInstrumentUSTBill
	}
	text := cashPolicyList(parts)
	if listed {
		switch n := len(c.ISINs); n {
		case 0:
			text += " · No bills listed: add ISINs in the file"
		case 1:
			text += " · 1 bill listed"
		default:
			text += " · " + strconv.Itoa(n) + " bills listed"
		}
	}
	if c.Fallback == cashSweepInstrumentETF {
		text += " · ETF fallback " + nonEmptyString(c.ETFSymbol, "(no symbol in the file)")
	}
	return text
}

// cashPolicyFindingRules are the `canary policy check` rules the screen
// shows, each for the cash keys it names.
var cashPolicyFindingRules = []string{"sweep_minimum_above_cap", "cap_above_trading_max", "cash_reserve_vs_nlv", "order_cap_vs_nlv",
	"sweep_buys_while_borrowed", "leveling_debit_inside_band", "sweep_minimum_uneconomic", "order_entry_off_for_active_bucket", "sweep_cap_exempt"}

// cashPolicyFindings keeps the report's findings of those rules that name a
// cash key, with the keys as dotted names.
func cashPolicyFindings(report rpc.PolicyCheckReport) []rpc.CashPolicyFinding {
	out := []rpc.CashPolicyFinding{}
	for _, f := range report.Findings {
		if !slices.Contains(cashPolicyFindingRules, f.Rule) {
			continue
		}
		var keys []string
		for _, k := range f.Keys {
			table, leaf, ok := strings.Cut(strings.TrimPrefix(k.Key, "["), "].")
			if ok && (table == "cash" || strings.HasPrefix(table, "cash.")) && !slices.Contains(keys, table+"."+leaf) {
				keys = append(keys, table+"."+leaf)
			}
		}
		if len(keys) == 0 {
			continue
		}
		text := strings.TrimSpace(f.Message + " " + f.Suggestion)
		out = append(out, rpc.CashPolicyFinding{Rule: f.Rule, Severity: f.Severity, Keys: keys, Text: text})
	}
	return out
}

// cashPolicyMoney is a whole amount with thousands separators and its
// currency.
func cashPolicyMoney(v float64, ccy string) string {
	return strings.TrimSpace(policyCheckNumber(math.Round(v)) + " " + ccy)
}

// cashPolicyApprox rounds an amount converted at the ledger rate: to tens
// below 1,000, to hundreds above.
func cashPolicyApprox(v float64) string {
	step := 100.0
	if math.Abs(v) < 1000 {
		step = 10
	}
	return policyCheckNumber(math.Round(v/step) * step)
}

// cashPolicyDay renders a statement day, 2026-10-03, as 3 Oct 2026.
func cashPolicyDay(day string) string {
	t, err := time.Parse(time.DateOnly, day)
	if err != nil {
		return day
	}
	return t.Format("2 Jan 2006")
}

// cashPolicyDuration renders a veto window or a confirmation window in words.
func cashPolicyDuration(d time.Duration) string {
	unit := func(n int, one string) string {
		if n == 1 {
			return "1 " + one
		}
		return strconv.Itoa(n) + " " + one + "s"
	}
	switch {
	case d%time.Hour == 0 && d >= time.Hour:
		return unit(int(d/time.Hour), "hour")
	case d < time.Minute:
		return unit(int(math.Round(d.Seconds())), "second")
	case d%time.Minute == 0:
		return unit(int(d/time.Minute), "minute")
	default:
		return shortDuration(d)
	}
}

// cashPolicyList joins names: "USD", "USD and GBP", "USD, GBP and CAD".
func cashPolicyList(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

func cashPolicyIsAre(names []string) string {
	if len(names) == 1 {
		return " is"
	}
	return " are"
}
