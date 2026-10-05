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
}

// PolicyCheckPosition is one held line, measured in base currency.
type PolicyCheckPosition struct {
	// Kind is stock (stocks and ETFs) or option; other lines are not read.
	Kind            string
	Currency        string
	Quantity        float64
	MarketValueBase float64
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
			if kind == "" || row.Quantity == 0 {
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
			b.Positions = append(b.Positions, PolicyCheckPosition{Kind: kind, Currency: ccy, Quantity: row.Quantity, MarketValueBase: mv})
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
	// ConfigPath is the config.toml the trading limits came from.
	ConfigPath string
	// Trading is the trading configuration in force: config.toml with its
	// defaults, and any runtime-settings override applied.
	Trading config.Trading
	// TradingCapSource names where [trading].max_notional came from:
	// config.toml, runtime settings or Canary's compiled default.
	TradingCapSource string
	// Book is the live account; nil skips every book check, for BookSkipped.
	Book        *PolicyCheckBook
	BookSkipped string
	// FileStatus is the daemon's status per policy file (active, drift,
	// error); nil when no daemon answered.
	FileStatus map[string]string
}

// TradingCapSource labels.
const (
	PolicyCheckCapFromConfig  = "config.toml"
	PolicyCheckCapFromRuntime = "runtime settings"
	PolicyCheckCapFromDefault = "compiled default"
)

// PolicyCheckInputFromConfigFile reads config.toml the way the daemon does
// and resolves the policy paths and the trading limits from it, for a check
// that runs without a daemon.
func PolicyCheckInputFromConfigFile(path string) (PolicyCheckInput, error) {
	if strings.TrimSpace(path) == "" {
		path = config.DefaultPath()
	}
	in := PolicyCheckInput{ConfigPath: path, Trading: config.Trading{}.WithDefaults(), TradingCapSource: PolicyCheckCapFromDefault, Files: PolicyFileSetFor(nil)}
	cfg, _, err := config.LoadForDaemon(path)
	if err != nil {
		return in, err
	}
	if cfg.Trading.MaxNotional != 0 {
		in.TradingCapSource = PolicyCheckCapFromConfig
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
	now       time.Time
	trading   config.Trading
	capSource string
	config    string

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
	c := &policyCheckContext{now: in.Now, trading: in.Trading.WithDefaults(), capSource: cmp.Or(in.TradingCapSource, PolicyCheckCapFromDefault),
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
	c.protection = defaultProtectionPolicy()
	if c.protectionSrc.state == policyCheckFileRead {
		if _, _, err := parseProtectionPolicy(c.protectionSrc.data); err != nil {
			c.protectionSrc.refused = err.Error()
		}
		var p protectionPolicy
		md, err := toml.Decode(string(c.protectionSrc.data), &p)
		if err == nil {
			applyProtectionPolicyDefaults(&p, &md)
			applyCashSweepDefaults(p.Buckets.CashSweep, &md)
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
	return c
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

// tradingCapKey is the trading cap as a finding key.
func (c *policyCheckContext) tradingCapKey() rpc.PolicyCheckKey {
	file := c.config
	switch c.capSource {
	case PolicyCheckCapFromRuntime:
		file = "runtime settings"
	case PolicyCheckCapFromDefault:
		file = c.config + " (compiled default)"
	}
	return rpc.PolicyCheckKey{File: file, Key: "[trading].max_notional", Value: policyCheckMoney(c.trading.MaxNotional, c.base())}
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
	if s := c.protection.Buckets.CashSweep; s.enabled() {
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
	if c.protectionMD == nil || !c.protectionMD.IsDefined("buckets", "cash_sweep", "currency", ccy, key) {
		file += " (compiled default)"
	}
	return rpc.PolicyCheckKey{File: file, Key: "[buckets.cash_sweep.currency." + ccy + "]." + key, Value: value}
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
		keys = append(keys, c.protectionKey("buckets.cash_sweep", "max_order_notional", policyCheckMoney(s.MaxOrderNotional, c.base())))
	}
	if pct := s.MaxOrderPctNLV; pct != nil && *pct > 0 {
		keys = append(keys, c.protectionKey("buckets.cash_sweep", "max_order_pct_nlv", policyCheckNumber(*pct)))
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
// [trading].max_notional, up to the sweep's cap in force.
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
		m.keys = append(m.keys, c.protectionKey("buckets.cash_sweep", "min_order_notional", policyCheckMoney(minOrder, c.base())))
	}
	if cfg.MinTranche != nil {
		m.keys = append(m.keys, rpc.PolicyCheckKey{File: c.protectionSrc.label + " (retired key)", Key: "[buckets.cash_sweep.currency." + ccy + "].min_tranche", Value: policyCheckMoney(tranche, ccy)})
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
				Keys: hit.keys, Message: hit.message, Suggestion: hit.suggestion})
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
