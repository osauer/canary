package daemon

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// Policy plausibility check (docs/docs/understand/policy.md, "Check a
// policy for plausibility"). Canary validates each key against its own range
// when it loads a file; this check reads the values against each other,
// across config.toml and the policy files, and against the live book, and
// says what is implausible and what to write instead. It is advisory: it
// reads files and the account and never changes a limit, a gate or an order.
//
// The rules live in policyCheckCatalogue (policy_check_rules.go), one entry
// each. A new rule is one entry: an id, a severity, a category, whether it
// needs the live book, and a function that returns its hits.

// PolicyCheckBook is the live account the book checks measure against.
type PolicyCheckBook struct {
	BaseCurrency   string
	NetLiquidation float64
	AsOf           time.Time
	// Cash is the observed cash per currency in that currency.
	Cash map[string]float64
	// FXToBase is base-currency units per unit of each currency; the base
	// currency itself is 1.
	FXToBase map[string]float64
	// Positions is nil when the positions read failed; PositionsKnown says
	// whether an empty list means "no positions".
	Positions      []PolicyCheckPosition
	PositionsKnown bool
	// UnmeasuredLines names, in plain words, the held lines that are neither
	// stock, ETF nor option and carry delta Canary does not measure
	// (futures, indexes, CFDs, funds, warrants); bills, bonds and conversions
	// are not among them. While one exists the delta-reducing exit rule
	// cannot judge any exit, as the gate fails closed on it.
	UnmeasuredLines []string
}

// PolicyCheckPosition is one held line, measured in base currency.
type PolicyCheckPosition struct {
	// Kind is stock (stocks and ETFs) or option; other lines are not read.
	Kind            string
	Currency        string
	Quantity        float64
	MarketValueBase float64
	// ConID is the broker contract id, which the delta checks key a line by.
	ConID int
	// Underlying is the symbol whose delta the line belongs to: the stock's
	// own, or the option's underlying. It groups the lines the delta-reducing
	// exit exemption (delta_reduction.go) sums.
	Underlying string
	// Right, Expiry and Multiplier are an option line's, for the short-leg
	// coverage rule; empty and zero for a stock.
	Right      string
	Expiry     string
	Multiplier int
	// DollarDeltaBase is the line's signed dollar delta in base currency as
	// the trading gate measures it (deltaLegBase: the daemon's
	// positionDollarDelta at the row's own FX rate); nil when the row is
	// stale or lacks a delta, spot or FX rate, with no fallback the gate
	// lacks.
	DollarDeltaBase *float64
}

// exitLowersAbsoluteDelta reports whether closing units of line p would pass
// the delta-reducing exit rule as the trading gate judges it (lower the
// underlying's and the book's absolute delta, leave no short option
// uncovered; deltaReductionEvidence.judge), whether that can be known at
// all (every line of the book needs a measured delta, else the gate fails
// closed and so does this), and the reason when it does not pass or cannot
// be known, in the gate's own words. The working-order inventory, a
// point-in-time matter, is not part of the check.
func (b *PolicyCheckBook) exitLowersAbsoluteDelta(p PolicyCheckPosition, units float64) (lowers, known bool, why string) {
	const unmeasured = "its delta, or that of another line in the book, cannot be measured"
	if b == nil || p.Underlying == "" || p.ConID <= 0 || p.Quantity == 0 || units <= 0 {
		return false, false, unmeasured
	}
	if len(b.UnmeasuredLines) > 0 {
		return false, false, b.UnmeasuredLines[0]
	}
	ev := deltaReductionEvidence{Underlying: p.Underlying, BaseCurrency: b.BaseCurrency, Legs: map[int]deltaReductionLeg{}, Cover: deltaCoverage{Shares: map[int]float64{}}}
	for _, q := range b.Positions {
		if q.DollarDeltaBase == nil || q.Quantity == 0 {
			return false, false, unmeasured
		}
		ev.BookBefore += *q.DollarDeltaBase
		if q.Underlying != p.Underlying {
			continue
		}
		ev.NetBefore += *q.DollarDeltaBase
		if q.Kind == policyCheckKindOption {
			ev.Cover.Options = append(ev.Cover.Options, deltaCoverLeg{ConID: q.ConID, Right: strings.ToUpper(strings.TrimSpace(q.Right)), Expiry: q.Expiry, Quantity: q.Quantity, Multiplier: float64(max(q.Multiplier, 1))})
		} else {
			ev.Cover.Shares[q.ConID] += q.Quantity
		}
		if q.ConID == p.ConID {
			if _, dup := ev.Legs[q.ConID]; dup {
				return false, false, unmeasured
			}
			ev.Legs[q.ConID] = deltaReductionLeg{Quantity: q.Quantity, UnitBase: *q.DollarDeltaBase / q.Quantity}
		}
	}
	ev.Current = true
	// Closing moves the quantity toward zero by units.
	changes := []deltaChange{{ConID: p.ConID, Before: p.Quantity, After: p.Quantity - math.Copysign(units, p.Quantity)}}
	if _, failed := ev.netAfter(changes); failed != nil {
		return false, false, unmeasured
	}
	ok, clause := ev.judge(p.Underlying, changes)
	if ok {
		return true, true, ""
	}
	// The gate's clause as a reason: no leading separator and no closing
	// "so the cap applies", which the finding states itself.
	why = strings.TrimPrefix(clause, "; ")
	why = strings.TrimSuffix(strings.TrimSuffix(why, ", so the cap applies"), ", so the order cap applies")
	return false, true, why
}

// Position kinds the book checks distinguish.
const (
	policyCheckKindStock  = "stock"
	policyCheckKindOption = "option"
)

// PolicyCheckBookFrom builds the book from the account and positions reads.
// A nil or zero-NLV account yields nil: the book checks are skipped.
func PolicyCheckBookFrom(acct *rpc.AccountResult, pos *rpc.PositionsResult) *PolicyCheckBook {
	if acct == nil || !positiveFinite(acct.NetLiquidation) || strings.TrimSpace(acct.BaseCurrency) == "" {
		return nil
	}
	base := strings.ToUpper(strings.TrimSpace(acct.BaseCurrency))
	b := &PolicyCheckBook{BaseCurrency: base, NetLiquidation: acct.NetLiquidation, AsOf: acct.AsOf,
		Cash: map[string]float64{}, FXToBase: map[string]float64{base: 1}}
	if l := acct.BaseCurrencyLedger; l != nil && l.CashObserved {
		b.Cash[base] = l.CashCcy
	}
	for _, row := range acct.CurrencyExposure {
		ccy := strings.ToUpper(strings.TrimSpace(row.Currency))
		if ccy == "" || ccy == "BASE" {
			continue
		}
		if positiveFinite(row.ExchangeRate) {
			b.FXToBase[ccy] = row.ExchangeRate
		}
		if row.CashObserved {
			b.Cash[ccy] = row.CashCcy
		}
	}
	if pos == nil {
		return b
	}
	b.PositionsKnown = true
	for _, rows := range [][]rpc.PositionView{pos.Stocks, pos.Options} {
		for _, row := range rows {
			kind := policyCheckPositionKind(row.SecType)
			if kind == "" {
				// A future, index, CFD, fund or warrant carries delta the
				// gate does not measure; bills, bonds and conversions none.
				if row.Quantity != 0 && !ibkrlib.IsBillOrBond(row.SecType) && !strings.EqualFold(strings.TrimSpace(row.SecType), "CASH") {
					b.UnmeasuredLines = append(b.UnmeasuredLines, fmt.Sprintf("the %s %s line is not a stock, ETF or option, so Canary has no delta for it",
						strings.ToUpper(strings.TrimSpace(row.Symbol)), strings.ToUpper(strings.TrimSpace(row.SecType))))
				}
				continue
			}
			if row.Quantity == 0 {
				continue
			}
			ccy := strings.ToUpper(strings.TrimSpace(row.Currency))
			mv := 0.0
			switch {
			case row.MarketValueBase != nil:
				mv = *row.MarketValueBase
			case ccy == base:
				mv = row.MarketValue
			case positiveFinite(b.FXToBase[ccy]):
				mv = row.MarketValue * b.FXToBase[ccy]
			default:
				continue
			}
			p := PolicyCheckPosition{Kind: kind, Currency: ccy, Quantity: row.Quantity, MarketValueBase: mv, ConID: row.ConID, Underlying: strings.ToUpper(strings.TrimSpace(row.Symbol))}
			if kind == policyCheckKindOption {
				p.Right, p.Expiry, p.Multiplier = strings.ToUpper(strings.TrimSpace(row.Right)), strings.TrimSpace(row.Expiry), optionMultiplier(row)
			}
			if dd, why := deltaLegBase(row, kind == policyCheckKindOption, base); why == "" {
				p.DollarDeltaBase = new(dd)
			}
			b.Positions = append(b.Positions, p)
		}
	}
	return b
}

func policyCheckPositionKind(secType string) string {
	switch strings.ToUpper(strings.TrimSpace(secType)) {
	case "STK", "STOCK", "ETF":
		return policyCheckKindStock
	case "OPT", "OPTION":
		return policyCheckKindOption
	}
	return ""
}

// PolicyCheckInput is everything one check reads besides the policy files.
type PolicyCheckInput struct {
	Now   time.Time
	Files PolicyFileSet
	// ConfigPath is the config.toml the trading mode came from.
	ConfigPath string
	// Trading is config.toml's [trading] as written: the mode, and the
	// retired order gates, which the check compares with [order_limits].
	Trading config.Trading
	// OrderCapOverride is an active one-shot override of the order floor,
	// when the daemon reports one; the cap in force is then the ceiling.
	OrderCapOverride *risk.OrderLimitsOverride
	// Book is the live account; nil skips every book check, for BookSkipped.
	Book        *PolicyCheckBook
	BookSkipped string
	// FileStatus is the daemon's status per policy file (active, drift,
	// error); nil when no daemon answered.
	FileStatus map[string]string
	// ProtectionData, when set, is read as the protection policy file in
	// place of the file at Files.Protection: policy.cash.check measures a
	// draft before anything is written. ConstitutionData does the same for
	// the risk constitution, so a draft's order caps reach every rule.
	ProtectionData   []byte
	ConstitutionData []byte
}

// PolicyCheckInputFromConfigFile reads config.toml the way the daemon does
// and resolves the policy paths and the trading mode from it, for a check
// that runs without a daemon.
func PolicyCheckInputFromConfigFile(path string) (PolicyCheckInput, error) {
	if strings.TrimSpace(path) == "" {
		path = config.DefaultPath()
	}
	in := PolicyCheckInput{ConfigPath: path, Trading: config.Trading{}.WithDefaults(), Files: PolicyFileSetFor(nil)}
	cfg, _, err := config.LoadForDaemon(path)
	if err != nil {
		return in, err
	}
	in.Trading = cfg.Trading.WithDefaults()
	resolved, err := cfg.Resolve()
	if err != nil {
		return in, err
	}
	in.Files = PolicyFileSetFor(resolved)
	return in, nil
}

// policyCheckSource is one file as the check read it.
type policyCheckSource struct {
	policy string
	path   string
	label  string
	state  string // read | absent | unreadable
	review string
	// refused is the error the daemon's strict loader gives this file; the
	// check still reads its values leniently.
	refused string
	data    []byte
}

const (
	policyCheckFileRead       = "read"
	policyCheckFileAbsent     = "absent"
	policyCheckFileUnreadable = "unreadable"
)

func readPolicyCheckSource(policy, path string) policyCheckSource {
	src := policyCheckSource{policy: policy, path: path, label: filepath.Base(path)}
	if strings.TrimSpace(path) == "" {
		src.state, src.label = policyCheckFileAbsent, policy
		return src
	}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		src.state = policyCheckFileAbsent
	case err != nil:
		src.state, src.refused = policyCheckFileUnreadable, err.Error()
	default:
		src.state, src.data, src.review = policyCheckFileRead, data, policyFileReview(data)
	}
	return src
}

// policyCheckContext is what every catalogue entry reads.
type policyCheckContext struct {
	now     time.Time
	trading config.Trading
	config  string
	// orderLimits is the constitution's [order_limits] in force against the
	// book (the floor without one); orderCapOverride lifts the floor.
	orderLimits      risk.OrderLimitsInForce
	orderCapOverride *risk.OrderLimitsOverride

	rulebookSrc     policyCheckSource
	rulebook        risk.RulebookPolicy
	rulebookDefined map[string]bool

	protectionSrc policyCheckSource
	protection    protectionPolicy
	protectionMD  *toml.MetaData

	opportunitySrc policyCheckSource

	constitutionSrc policyCheckSource
	constitution    *risk.Constitution

	book        *PolicyCheckBook
	fileStatus  map[string]string
	skipped     []string
	assumptions []string
}

func newPolicyCheckContext(in PolicyCheckInput) *policyCheckContext {
	c := &policyCheckContext{now: in.Now, trading: in.Trading.WithDefaults(), orderCapOverride: in.OrderCapOverride,
		config: cmp.Or(filepath.Base(in.ConfigPath), "config.toml"), book: in.Book, fileStatus: in.FileStatus}
	if c.now.IsZero() {
		c.now = time.Now()
	}
	if c.config == "." {
		c.config = "config.toml"
	}

	c.rulebookSrc = readPolicyCheckSource(PolicyFileRulebook, in.Files.Rulebook)
	c.rulebook, c.rulebookDefined = risk.DefaultRulebookPolicy(), map[string]bool{}
	if c.rulebookSrc.state == policyCheckFileRead {
		if _, err := parseRulebookPolicy(c.rulebookSrc.data); err != nil {
			c.rulebookSrc.refused = err.Error()
		}
		p := risk.DefaultRulebookPolicy()
		if md, err := toml.Decode(string(c.rulebookSrc.data), &p); err == nil {
			p.Normalize()
			c.rulebook = p
			for _, k := range md.Keys() {
				c.rulebookDefined[k.String()] = true
			}
		} else {
			c.rulebookSrc.state = policyCheckFileUnreadable
		}
	}

	c.protectionSrc = readPolicyCheckSource(PolicyFileProtection, in.Files.Protection)
	if in.ProtectionData != nil {
		c.protectionSrc = policyCheckSource{policy: PolicyFileProtection, path: in.Files.Protection, label: filepath.Base(in.Files.Protection),
			state: policyCheckFileRead, data: in.ProtectionData, review: policyFileReview(in.ProtectionData)}
	}
	c.protection = defaultProtectionPolicy()
	if c.protectionSrc.state == policyCheckFileRead {
		if _, _, err := parseProtectionPolicy(c.protectionSrc.data); err != nil {
			c.protectionSrc.refused = err.Error()
		}
		var p protectionPolicy
		md, err := toml.Decode(string(c.protectionSrc.data), &p)
		if err == nil {
			applyProtectionPolicyDefaults(&p, &md)
			applyCashSweepDefaults(p.Cash.Sweep, &md)
			c.protection, c.protectionMD = p, &md
		} else {
			c.protectionSrc.state = policyCheckFileUnreadable
		}
	}

	c.opportunitySrc = readPolicyCheckSource(PolicyFileOpportunity, in.Files.Opportunity)
	if c.opportunitySrc.state == policyCheckFileRead {
		if _, err := parseOpportunityPolicy(c.opportunitySrc.data); err != nil {
			c.opportunitySrc.refused = err.Error()
		}
	}

	c.constitutionSrc = readPolicyCheckSource(PolicyFileConstitution, in.Files.Constitution)
	if in.ConstitutionData != nil {
		c.constitutionSrc = policyCheckSource{policy: PolicyFileConstitution, path: in.Files.Constitution, label: filepath.Base(in.Files.Constitution),
			state: policyCheckFileRead, data: in.ConstitutionData, review: policyFileReview(in.ConstitutionData)}
	}
	if c.constitutionSrc.state == policyCheckFileRead {
		if err := parseConstitutionPolicy(c.constitutionSrc.data); err != nil {
			c.constitutionSrc.refused = err.Error()
		}
		var k risk.Constitution
		if _, err := toml.Decode(string(c.constitutionSrc.data), &k); err == nil {
			c.constitution = &k
		} else {
			c.constitutionSrc.state = policyCheckFileUnreadable
		}
	}
	if c.book != nil && !c.book.PositionsKnown {
		c.skip("the checks that size orders against held positions: the positions read failed")
	}
	c.orderLimits = c.evaluateOrderLimits()
	return c
}

// evaluateOrderLimits reads [order_limits] the way the trading gate does:
// against the book's NLV, or at the floor when there is no live account.
func (c *policyCheckContext) evaluateOrderLimits() risk.OrderLimitsInForce {
	if c.constitution == nil {
		why := "no risk constitution could be read"
		if c.constitutionSrc.state == policyCheckFileAbsent {
			why = "there is no risk constitution file"
		}
		return risk.EvaluateOrderLimits(nil, c.base(), risk.OrderLimitsNLV{}, nil, why)
	}
	nlv := risk.OrderLimitsNLV{Unavailable: "no live account to size it"}
	if c.book != nil && positiveFinite(c.book.NetLiquidation) {
		nlv = risk.OrderLimitsNLV{Base: c.book.NetLiquidation, AsOf: c.now}
	}
	limits := risk.EvaluateOrderLimits(c.constitution.OrderLimits, c.base(), nlv, c.orderCapOverride, "")
	if limits.Complete && limits.NLVBase == nil && c.orderCapOverride == nil {
		c.assume("the order cap in force at its floor, max_order_floor_base: no live account to apply max_order_pct_nlv to, which is how the trading gate reads it without a current NLV")
	}
	return limits
}

// orderCap is the order cap in force in base currency; false while
// [order_limits] is incomplete, when every order preview is refused anyway.
func (c *policyCheckContext) orderCap() (float64, bool) {
	return c.orderLimits.CapBase, c.orderLimits.Complete
}

func (c *policyCheckContext) sources() []policyCheckSource {
	return []policyCheckSource{c.rulebookSrc, c.protectionSrc, c.opportunitySrc, c.constitutionSrc}
}

func (c *policyCheckContext) skip(why string) {
	if !slices.Contains(c.skipped, why) {
		c.skipped = append(c.skipped, why)
	}
}

func (c *policyCheckContext) assume(what string) {
	if !slices.Contains(c.assumptions, what) {
		c.assumptions = append(c.assumptions, what)
	}
}

// base is the account base currency: the book's, else the constitution's.
func (c *policyCheckContext) base() string {
	if c.book != nil {
		return c.book.BaseCurrency
	}
	if c.constitution != nil {
		return strings.ToUpper(strings.TrimSpace(c.constitution.Capital.BaseCurrency))
	}
	return ""
}

// fx returns base units per unit of ccy, when known.
func (c *policyCheckContext) fx(ccy string) (float64, bool) {
	if ccy != "" && ccy == c.base() {
		return 1, true
	}
	if c.book == nil {
		return 0, false
	}
	r, ok := c.book.FXToBase[ccy]
	return r, ok && positiveFinite(r)
}

// tradingCapKey is the order cap in force as a finding key.
func (c *policyCheckContext) tradingCapKey() rpc.PolicyCheckKey {
	return rpc.PolicyCheckKey{File: c.constitutionSrc.label, Key: "[order_limits] cap in force",
		Value: policyCheckMoney(c.orderLimits.CapBase, c.base()) + " (" + orderCapBoundPhrase(c.orderLimits) + ")"}
}

// orderCapBoundPhrase names the term of the formula that sets the cap.
func orderCapBoundPhrase(l risk.OrderLimitsInForce) string {
	switch l.CapBound {
	case risk.OrderCapBoundPctNLV:
		return policyCheckNumber(l.PctNLV) + "% of NLV"
	case risk.OrderCapBoundCeiling:
		return "the ceiling, max_order_ceiling_base"
	case risk.OrderCapBoundOverride:
		return "override " + l.OverrideID + ": the ceiling"
	default:
		return "the floor, max_order_floor_base"
	}
}

// protectionKey names a protection-policy key.
func (c *policyCheckContext) protectionKey(table, key string, value string) rpc.PolicyCheckKey {
	return rpc.PolicyCheckKey{File: c.protectionSrc.label, Key: "[" + table + "]." + key, Value: value}
}

// rulebookKey names a Rulebook key; a key the file does not set is labelled
// as Canary's compiled default.
func (c *policyCheckContext) rulebookKey(key string, value float64) rpc.PolicyCheckKey {
	file := c.rulebookSrc.label
	if !c.rulebookDefined[key] {
		file += " (compiled default)"
	}
	name := key
	if set, leaf, ok := strings.Cut(key, "."); ok {
		name = "[" + set + "]." + leaf
	}
	return rpc.PolicyCheckKey{File: file, Key: name, Value: policyCheckNumber(value)}
}

// constitutionKey names a constitution key.
func (c *policyCheckContext) constitutionKey(table, key, value string) rpc.PolicyCheckKey {
	return rpc.PolicyCheckKey{File: c.constitutionSrc.label, Key: "[" + table + "]." + key, Value: value}
}

// sweep returns the cash sweep table when it is present and enabled.
func (c *policyCheckContext) sweep() *protectionCashSweepPolicy {
	if s := c.protection.Cash.Sweep; s.enabled() {
		return s
	}
	return nil
}

// sweepCurrencies lists the currencies the sweep invests in: every written
// currency table, the base currency, and every currency the account holds
// cash in. A currency declared none is left out.
func (c *policyCheckContext) sweepCurrencies() []string {
	s := c.sweep()
	if s == nil {
		return nil
	}
	set := map[string]bool{}
	for ccy := range s.Currency {
		set[ccy] = true
	}
	if b := c.base(); b != "" {
		set[b] = true
	}
	if c.book != nil {
		for ccy := range c.book.Cash {
			set[ccy] = true
		}
	} else {
		c.skip("cash sweep currencies without a table that the account may hold: no live account")
	}
	var out []string
	for _, ccy := range slices.Sorted(maps.Keys(set)) {
		cfg := s.currency(ccy)
		if len(cfg.Instruments) == 1 && cfg.Instruments[0] == cashSweepInstrumentNone {
			continue
		}
		out = append(out, ccy)
	}
	return out
}

// sweepCurrencyKey names a key of a sweep currency, labelled as the compiled
// default when the file does not write it.
func (c *policyCheckContext) sweepCurrencyKey(ccy, key, value string) rpc.PolicyCheckKey {
	file := c.protectionSrc.label
	if !cashSweepCurrencyDefined(c.protectionMD, ccy, key) {
		file += " (compiled default)"
	}
	return rpc.PolicyCheckKey{File: file, Key: "[cash.sweep.currency." + ccy + "]." + key, Value: value}
}

// sweepCapBase is the sweep's per-order cap in force, in base currency, as
// the sweep sizes it: the larger of max_order_notional and
// max_order_pct_nlv percent of NLV. Without a live account only
// max_order_notional is known, and the check says the rest was skipped.
func (c *policyCheckContext) sweepCapBase() (capBase float64, keys []rpc.PolicyCheckKey, ok bool) {
	s := c.sweep()
	if s == nil {
		return 0, nil, false
	}
	capBase = s.MaxOrderNotional
	if s.MaxOrderNotional > 0 {
		keys = append(keys, c.protectionKey("cash.sweep", "max_order_notional", policyCheckMoney(s.MaxOrderNotional, c.base())))
	}
	if pct := s.MaxOrderPctNLV; pct != nil && *pct > 0 {
		keys = append(keys, c.protectionKey("cash.sweep", "max_order_pct_nlv", policyCheckNumber(*pct)))
		if c.book != nil {
			capBase = max(capBase, *pct/100*c.book.NetLiquidation)
		} else {
			c.skip("the part of the sweep cap that max_order_pct_nlv sets: no live account to size it (max_order_notional alone is compared)")
		}
	}
	return capBase, keys, capBase > 0
}

// sweepCapBound names the key that sets the cap in force.
func (c *policyCheckContext) sweepCapBound(capBase float64) string {
	if s := c.sweep(); s != nil && capBase > s.MaxOrderNotional+1e-9 {
		return "max_order_pct_nlv"
	}
	return "max_order_notional"
}

// sweepExempt reports whether the owner declared bill orders exempt from
// the order cap in force, up to the sweep's own cap in force.
func (c *policyCheckContext) sweepExempt() bool {
	return c.sweep().billsExempt()
}

// policyCheckSweepMinimum is a currency's smallest sweep buy as the sweep
// sizes it.
type policyCheckSweepMinimum struct {
	native, base float64
	keys         []rpc.PolicyCheckKey
	// binding is the key that sets it: min_order_notional, or the retired
	// min_tranche when a legacy file still carries a larger one.
	binding string
	fx      float64
}

// sweepMinimum is the smallest buy in ccy: min_order_notional (base) at the
// currency's rate, raised by a legacy min_tranche (its own unit) when the
// file still carries one. ok is false when nothing is written (the sweep
// holds) or the rate is unknown.
func (c *policyCheckContext) sweepMinimum(ccy string) (policyCheckSweepMinimum, bool) {
	s := c.sweep()
	if s == nil {
		return policyCheckSweepMinimum{}, false
	}
	fx, fxOK := c.fx(ccy)
	if !fxOK {
		c.skip(fmt.Sprintf("the %s sweep minimum: no %s rate without a live account", ccy, ccy))
		return policyCheckSweepMinimum{}, false
	}
	cfg := s.currency(ccy)
	minOrder, tranche := policyCheckDeref(s.MinOrderNotional), policyCheckDeref(cfg.MinTranche)
	m := policyCheckSweepMinimum{fx: fx, binding: "min_order_notional", native: cashSweepMinimum(tranche, minOrder, fx)}
	if m.native <= 0 {
		return policyCheckSweepMinimum{}, false
	}
	m.base = m.native * fx
	if s.MinOrderNotional != nil {
		m.keys = append(m.keys, c.protectionKey("cash.sweep", "min_order_notional", policyCheckMoney(minOrder, c.base())))
	}
	if cfg.MinTranche != nil {
		m.keys = append(m.keys, rpc.PolicyCheckKey{File: c.protectionSrc.label + " (retired key)", Key: "[cash.sweep.currency." + ccy + "].min_tranche", Value: policyCheckMoney(tranche, ccy)})
		if minOrder <= 0 || tranche > minOrder/fx+1e-9 {
			m.binding = "min_tranche"
		}
	}
	return m, true
}

// CheckPolicy runs the whole catalogue and returns the report.
func CheckPolicy(in PolicyCheckInput) rpc.PolicyCheckReport {
	c := newPolicyCheckContext(in)
	if c.book == nil {
		c.skip("every check against the live book: " + cmp.Or(in.BookSkipped, "no live account"))
	}
	if c.fileStatus == nil {
		c.skip("version-bump drift: no daemon answered, so the policies in force are unknown")
	}
	report := rpc.PolicyCheckReport{AsOf: c.now, Findings: []rpc.PolicyCheckFinding{}}
	for _, src := range c.sources() {
		f := rpc.PolicyCheckFile{Policy: src.policy, Path: src.path, State: src.state, Review: src.review}
		if src.state == policyCheckFileUnreadable {
			f.Error = src.refused
		}
		report.Files = append(report.Files, f)
	}
	if c.book != nil {
		report.Book = &rpc.PolicyCheckBookSummary{BaseCurrency: c.book.BaseCurrency, NetLiquidation: c.book.NetLiquidation, AsOf: c.book.AsOf, Positions: len(c.book.Positions)}
	}
	for _, rule := range policyCheckCatalogue {
		if rule.needsBook && c.book == nil {
			continue
		}
		for _, hit := range rule.run(c) {
			report.Findings = append(report.Findings, rpc.PolicyCheckFinding{Rule: rule.id, Severity: rule.severity, Category: rule.category,
				Keys: hit.keys, Message: hit.message, Suggestion: hit.suggestion, Screen: hit.screen})
		}
	}
	rank := map[string]int{rpc.PolicyCheckError: 0, rpc.PolicyCheckWarn: 1, rpc.PolicyCheckInfo: 2}
	slices.SortStableFunc(report.Findings, func(a, b rpc.PolicyCheckFinding) int { return rank[a.Severity] - rank[b.Severity] })
	for _, f := range report.Findings {
		switch f.Severity {
		case rpc.PolicyCheckError:
			report.Errors++
		case rpc.PolicyCheckWarn:
			report.Warnings++
		default:
			report.Infos++
		}
	}
	switch {
	case report.Errors > 0:
		report.Status = "errors"
	case report.Warnings > 0:
		report.Status = "warnings"
	default:
		report.Status = "ok"
	}
	report.Skipped, report.Assumptions = c.skipped, c.assumptions
	return report
}

// policyCheckNumber renders a number with thousands separators and no
// trailing zeros.
func policyCheckNumber(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	digits, frac, _ := strings.Cut(strconv.FormatFloat(math.Abs(v), 'f', 2, 64), ".")
	frac = strings.TrimRight(frac, "0")
	var b strings.Builder
	if v < 0 && (digits != "0" || frac != "") {
		b.WriteByte('-')
	}
	for i, r := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if frac != "" {
		b.WriteString("." + frac)
	}
	return b.String()
}

// policyCheckMoney renders an amount with its currency, when known.
func policyCheckMoney(v float64, ccy string) string {
	if ccy == "" {
		return policyCheckNumber(v)
	}
	return policyCheckNumber(v) + " " + ccy
}

// policyCheckStep is the rounding step a suggested amount uses: round
// numbers an owner would write.
func policyCheckStep(v float64) float64 {
	switch v = math.Abs(v); {
	case v < 1000:
		return 100
	case v < 10000:
		return 500
	case v < 100000:
		return 1000
	default:
		return 5000
	}
}

func policyCheckRoundUp(v float64) float64 {
	step := policyCheckStep(v)
	return math.Ceil(v/step) * step
}

func policyCheckRoundDown(v float64) float64 {
	step := policyCheckStep(v)
	return math.Floor(v/step) * step
}

// policyCheckPct renders a share as a percent with up to two decimals.
func policyCheckPct(share float64) string {
	return policyCheckNumber(share*100) + "%"
}
