// Package register is the compiled side of the strategy register
// (internal-docs/design/strategy-register.md; owner decision 2026-10-18
// 09:01 CEST: "one register for all machine strategies, the ladder stage in
// Canary's risk policy, shadow on the production path, and strategies
// declaring the data Canary collects"). The policy side is
// risk.ConstitutionMandate.Register(): one owner-written entry per machine
// strategy. This package lists the rules this Canary carries and resolves
// each entry's effective stage and the part of its risk allocation it
// reserves. It holds no state and performs no I/O; the daemon is its only
// caller.
package register

import (
	"fmt"
	"slices"
	"strings"

	"github.com/osauer/canary/v3/internal/core"
	"github.com/osauer/canary/v3/internal/risk"
)

// Form says which policy form a rule runs in: the core's [mandate.core], or
// a [mandate.strategies.<id>] entry.
type Form string

// The two forms.
const (
	FormCore  Form = "core"
	FormEntry Form = "entry"
)

// SeriesNeed is one daily series a rule reads: the instrument by ISIN and
// currency (the line IBKR lists for it, judged on its listing's calendar),
// and how many sessions the rule's window needs.
type SeriesNeed struct {
	ISIN     string `json:"isin,omitempty"`
	Currency string `json:"currency"`
	Symbol   string `json:"symbol,omitempty"`
	Sessions int    `json:"sessions"`
}

// ChainNeed is one option-chain underlying a rule reads, recorded on the
// recorder's plan of that name (only "v1" exists).
type ChainNeed struct {
	Underlying string `json:"underlying"`
	Plan       string `json:"plan"`
}

// Data is what a rule declares it reads, compiled with its version.
type Data struct {
	Series []SeriesNeed `json:"series,omitempty"`
	Chains []ChainNeed  `json:"chains,omitempty"`
}

// Instrument is the fund a strategy trades: by ISIN and currency, with the
// owner's optional pins (a contract id among the ISIN's lines, a venue IBKR
// lists it on). Symbol is the owner's name for it, when the rule compiles
// one; IBKR's record names the line.
type Instrument struct {
	ISIN     string `json:"isin"`
	Currency string `json:"currency"`
	Symbol   string `json:"symbol,omitempty"`
	ConID    int    `json:"con_id,omitempty"`
	Exchange string `json:"exchange,omitempty"`
}

// Terms are the owner's numbers the evaluator sizes with: the no-trade band
// in percent of the target, the minimum order in base currency and the
// value cap in percent of the budget base × room.
type Terms struct {
	BandPct      float64 `json:"band_pct"`
	MinOrderBase float64 `json:"min_order_base"`
	MaxValuePct  float64 `json:"max_value_pct"`
}

// Rule is one compiled rule: the lab spec it implements, the policy form it
// runs in, whether this build serves its orders, its proposal bucket and its
// declared data. A compiled instance (the core) carries its instrument and
// its filter; a parameterised rule reads them from the entry's keys
// (internal-docs/design/strategy-evaluator.md §1).
type Rule struct {
	ID     string `json:"id"`
	SpecID string `json:"spec_id,omitempty"`
	// SpecHash is the spec a compiled instance implements; empty for a
	// parameterised rule, whose entry cites its own.
	SpecHash string `json:"spec_hash,omitempty"`
	Form     Form   `json:"form"`
	// Sends is whether this build serves the rule's rows. Every compiled
	// rule does; a policy that writes an entry live on a rule that does not
	// is refused (Refusals), never run as anything else.
	Sends bool `json:"sends"`
	// Bucket is the proposal bucket of the rule's rows; never
	// pre-authorisable.
	Bucket string `json:"bucket"`
	Data   Data   `json:"data"`
	// Parameterised says the entry's keys supply the instrument, the signal
	// and the terms; Instrument and Trend are then empty here.
	Parameterised bool       `json:"parameterised,omitempty"`
	Instrument    Instrument `json:"instrument,omitzero"`
	// Trend is the compiled filter's id in core.Trends().
	Trend string `json:"trend,omitempty"`
}

// CoreRebalanceBucket is the core's proposal bucket (core-sleeve.md §2).
const CoreRebalanceBucket = "core_rebalance"

// TimedHoldingRuleID is the parameterised evaluator rule: one fund named by
// the entry, held while the entry's filter is on, sized to the entry's
// sleeve's allocation (strategy-evaluator.md). A live entry on it serves its
// month's row as the core does (strategy-register.md, section 2).
const TimedHoldingRuleID = "timed-holding-v1"

// TimedHoldingBucket is the proposal bucket of its rows; never
// pre-authorisable.
const TimedHoldingBucket = "strategy_rebalance"

// Catalogue is the rules this Canary carries. Both send: each live entry's
// monthly row (core-sleeve.md phase 2) is served as a proposal the owner
// confirms on a device, or Desk's controller sends under the standing
// mandate (phase 3), so a live stage runs live.
func Catalogue() []Rule {
	return []Rule{{
		ID: core.RuleID, SpecID: core.SpecID, SpecHash: core.SpecHash, Form: FormCore, Sends: true, Bucket: CoreRebalanceBucket,
		Data:       Data{Series: []SeriesNeed{{ISIN: core.ISIN, Currency: core.Currency, Symbol: core.Symbol, Sessions: core.WindowCloses}}},
		Instrument: Instrument{ISIN: core.ISIN, Currency: core.Currency, Symbol: core.Symbol}, Trend: core.TrendSMA10.ID,
	}, {
		ID: TimedHoldingRuleID, Form: FormEntry, Sends: true, Bucket: TimedHoldingBucket, Parameterised: true,
	}}
}

// Find returns the rule with the id, or false.
func Find(rules []Rule, id string) (Rule, bool) {
	i := slices.IndexFunc(rules, func(r Rule) bool { return r.ID == id })
	if i < 0 {
		return Rule{}, false
	}
	return rules[i], true
}

// State is an entry's effective stage.
type State string

// The effective stages: on hold, in shadow, live, or suspended.
const (
	StateHold      State = "hold"
	StateShadow    State = "shadow"
	StateLive      State = "live"
	StateSuspended State = "suspended"
)

// Resolved is one entry as this build runs it.
type Resolved struct {
	Entry risk.StrategyEntry `json:"entry"`
	// Rule is the compiled rule the entry names; nil when it is not
	// compiled.
	Rule  *Rule `json:"rule,omitempty"`
	State State `json:"state"`
	// ScalePct is the scale the rule sizes at, in shadow and live.
	ScalePct float64 `json:"scale_pct"`
	// ReservedPct is the part of the allocation reserved, in percent: the
	// scale while live, 0 in every other state.
	ReservedPct float64 `json:"reserved_pct"`
	// AllocationPct is the entry's risk allocation, percent a year of the
	// budget base at full room; 0 while unwritten.
	AllocationPct float64 `json:"allocation_pct"`
	// Reasons name every hold; Notes say why a written stage runs as
	// another (live written, the build sends nothing; a stage unwritten).
	Reasons []string `json:"reasons,omitempty"`
	Notes   []string `json:"notes,omitempty"`
	// What the evaluator runs (strategy-evaluator.md §1): the instrument,
	// the filter's id, the terms, the spec the run is recorded under (the
	// rule's own, or the entry's citation) and the declared data, from the
	// compiled rule or the entry's keys. Empty on a hold.
	Instrument Instrument `json:"instrument,omitzero"`
	Trend      string     `json:"trend,omitempty"`
	Terms      Terms      `json:"terms,omitzero"`
	SpecHash   string     `json:"spec_hash,omitempty"`
	Data       Data       `json:"data,omitzero"`
	// KillDrawdownK is the kill rule's drawdown bound, nil while unwritten.
	KillDrawdownK *float64 `json:"kill_drawdown_k,omitempty"`
	// Signal is the listing the signal is read on while the traded line's
	// own closes do not fill the window, and SignalIn the currency its
	// closes are measured in (strategy-evaluator.md §7); zero for an entry
	// that reads only its own closes, and for the core.
	Signal   Instrument `json:"signal,omitzero"`
	SignalIn string     `json:"signal_in,omitempty"`
}

// Reserves reports whether the entry reserves any of its allocation.
func (r Resolved) Reserves() bool { return r.State == StateLive && r.ReservedPct > 0 }

// Resolve gives each entry its effective state against the compiled rules.
// An entry holds on an unwritten key, a rule this Canary does not carry, a
// written spec hash that is not the rule's, or a rule in the other form. A
// live stage runs live: no entry written live runs as anything else (owner,
// 2026-10-18 21:05 CEST: "we NEVER want shadow"), and a policy writing one
// live on a rule without rows is refused at load (Refusals). Only a
// live entry reserves its allocation, at its scale.
func Resolve(entries []risk.StrategyEntry, rules []Rule) []Resolved {
	out := make([]Resolved, 0, len(entries))
	for _, e := range entries {
		out = append(out, resolveOne(e, rules))
	}
	return out
}

func resolveOne(e risk.StrategyEntry, rules []Rule) Resolved {
	r := Resolved{Entry: e}
	if e.AllocationPct != nil {
		r.AllocationPct = *e.AllocationPct
	}
	if e.ScalePct != nil {
		r.ScalePct = *e.ScalePct
	}
	hold := func(format string, args ...any) { r.Reasons = append(r.Reasons, fmt.Sprintf(format, args...)) }
	unwritten := slices.Clone(e.Unwritten)
	var rule Rule
	if e.Rule != "" {
		var ok bool
		rule, ok = Find(rules, e.Rule)
		if ok {
			r.Rule = &rule
			if rule.Parameterised {
				unwritten = append(unwritten, parameterisedUnwritten(e)...)
			}
		}
	}
	if len(unwritten) > 0 {
		hold("%s unwritten", strings.Join(unwritten, ", "))
	}
	switch {
	case e.Rule == "":
	case r.Rule == nil:
		hold("rule %s is not compiled into this Canary", e.Rule)
	default:
		if rule.SpecHash != "" && e.SpecHash != "" && e.SpecHash != rule.SpecHash {
			hold("spec_hash %s is not the spec rule %s implements (%s)", e.SpecHash, rule.ID, rule.SpecHash)
		}
		if want := formOf(e); rule.Form != want {
			if rule.Form == FormCore {
				hold("rule %s runs only as [mandate.core]", rule.ID)
			} else {
				hold("[mandate.core] runs only the core's rule; %s is a [mandate.strategies] rule", rule.ID)
			}
		}
		if rule.Parameterised && e.Signal != "" {
			if _, ok := core.FindTrend(e.Signal); !ok {
				hold("signal %s is not in this Canary's library", e.Signal)
			}
		}
	}
	if len(r.Reasons) > 0 {
		r.State, r.ScalePct = StateHold, 0
		return r
	}
	r.KillDrawdownK = e.KillDrawdownK
	if rule.Parameterised {
		r.Instrument = Instrument{ISIN: e.ISIN, Currency: e.Currency, Exchange: e.Exchange}
		r.Trend, r.SpecHash = e.Signal, e.SpecHash
		r.Data = Data{Series: []SeriesNeed{{ISIN: e.ISIN, Currency: e.Currency, Sessions: core.WindowCloses}}}
		if e.SignalISIN != "" {
			r.Signal = Instrument{ISIN: e.SignalISIN, Currency: e.SignalCurrency}
			r.SignalIn = e.SignalIn
			if r.SignalIn == "" {
				r.SignalIn = e.SignalCurrency
			}
			r.Data.Series = append(r.Data.Series, SeriesNeed{ISIN: e.SignalISIN, Currency: e.SignalCurrency, Sessions: core.WindowCloses})
		}
	} else {
		r.Instrument, r.Trend, r.SpecHash, r.Data = rule.Instrument, rule.Trend, rule.SpecHash, rule.Data
		r.Instrument.Exchange = e.Exchange
	}
	if e.ConID != nil && *e.ConID > 0 {
		r.Instrument.ConID = *e.ConID
	}
	if e.BandPct != nil && e.MinOrderBase != nil && e.MaxValuePct != nil {
		r.Terms = Terms{BandPct: *e.BandPct, MinOrderBase: *e.MinOrderBase, MaxValuePct: *e.MaxValuePct}
	}
	stage := e.Stage
	if stage == "" {
		// Only the core's first form leaves its stage unwritten without a
		// hold: it runs in shadow, as before the register.
		stage = risk.StageShadow
		r.Notes = append(r.Notes, "stage unwritten: shadow, as before the register")
	}
	if e.Form == risk.StrategyFormCore && e.SpecHash == "" {
		r.Notes = append(r.Notes, "spec_hash unwritten: the compiled rule's own spec stands")
	}
	switch {
	case stage == risk.StageSuspended, r.ScalePct <= 0:
		r.State, r.ScalePct = StateSuspended, 0
	case stage == risk.StageLive && r.Rule.Sends:
		r.State, r.ReservedPct = StateLive, r.ScalePct
	case stage == risk.StageLive:
		// Refusals keeps such a file from loading; a caller that resolves
		// one anyway sees it hold, never run in another stage.
		r.State, r.ScalePct = StateHold, 0
		r.Reasons = append(r.Reasons, fmt.Sprintf("rule %s serves no rows in this build", r.Rule.ID))
	default:
		r.State = StateShadow
	}
	return r
}

// KillBoundMax is the smallest kill_drawdown_k a live entry is refused on: a
// drawdown of one stage capital is the whole invested sleeve.
const KillBoundMax = 1.0

// Refusals names what refuses a policy file against this build's rules:
// every entry written live on a compiled rule that serves no rows (a live
// entry either produces rows or the file does not load), a live entry whose
// written kill bound cannot fire (KillBoundMax), and every two
// entries that name one fund line (the same ISIN and currency, or the same
// con_id; the core's compiled fund when its keys are unwritten), since two
// strategies on one line would each count the other's units as their own
// (placement, mandate-risk-budget.md). The policy manager refuses such a
// file and keeps the last good policy. A rule that is not compiled holds its
// entry instead, named on it.
func Refusals(entries []risk.StrategyEntry, rules []Rule) []string {
	var out []string
	type line struct{ isin, ccy string }
	lines := map[line]string{}
	conIDs := map[int]string{}
	for _, e := range entries {
		table := "[" + nonEmptyKey(e) + "]"
		if rule, ok := Find(rules, e.Rule); ok && e.Stage == risk.StageLive && !rule.Sends {
			out = append(out, fmt.Sprintf("%s is written live, but rule %s serves no rows in this build; write stage = \"suspended\" or install a build that sends it", table, rule.ID))
		}
		if e.Stage == risk.StageLive && e.KillDrawdownK != nil && *e.KillDrawdownK >= KillBoundMax {
			// The bound is in units of the sleeve fully invested at its
			// stage (stageCapital): at 1 K or more the kill rule fires only
			// once the whole invested sleeve is lost, which is no kill rule
			// (review, 2026-10-19). An unwritten bound holds the entry
			// instead, as any unwritten key does.
			out = append(out, fmt.Sprintf("%s is written live with kill_drawdown_k = %g: a bound of %g K or more is the whole sleeve invested at its stage, so the kill rule cannot fire before the sleeve is gone; write a bound below %g", table, *e.KillDrawdownK, KillBoundMax, KillBoundMax))
		}
		isin, ccy := strings.TrimSpace(e.ISIN), strings.ToUpper(strings.TrimSpace(e.Currency))
		if e.Form == risk.StrategyFormCore {
			if rule, ok := Find(rules, e.Rule); ok && !rule.Parameterised {
				isin, ccy = nonEmpty(isin, rule.Instrument.ISIN), nonEmpty(ccy, rule.Instrument.Currency)
			}
		}
		if isin != "" && ccy != "" {
			k := line{isin, ccy}
			if other, taken := lines[k]; taken {
				out = append(out, fmt.Sprintf("%s names the fund %s in %s, as %s does: one strategy per fund line", table, isin, ccy, other))
			} else {
				lines[k] = table
			}
		}
		if e.ConID != nil && *e.ConID > 0 {
			if other, taken := conIDs[*e.ConID]; taken {
				out = append(out, fmt.Sprintf("%s pins con_id %d, as %s does: one strategy per fund line", table, *e.ConID, other))
			} else {
				conIDs[*e.ConID] = table
			}
		}
	}
	return out
}

// Table is the policy table an entry is written in, without brackets:
// mandate.core, or mandate.strategies.<id>.
func Table(e risk.StrategyEntry) string { return nonEmptyKey(e) }

func nonEmptyKey(e risk.StrategyEntry) string {
	if e.KeyPrefix != "" {
		return e.KeyPrefix
	}
	if e.Form == risk.StrategyFormCore {
		return "mandate.core"
	}
	return "mandate.strategies." + e.ID
}

func nonEmpty(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}

func formOf(e risk.StrategyEntry) Form {
	if e.Form == risk.StrategyFormCore {
		return FormCore
	}
	return FormEntry
}

// parameterisedUnwritten names the evaluator keys a parameterised rule
// needs that the entry leaves unwritten (strategy-evaluator.md §2).
func parameterisedUnwritten(e risk.StrategyEntry) []string {
	var out []string
	for _, k := range []struct {
		name    string
		missing bool
	}{
		{"isin", strings.TrimSpace(e.ISIN) == ""},
		{"currency", strings.TrimSpace(e.Currency) == ""},
		{"signal", strings.TrimSpace(e.Signal) == ""},
		{"band_pct", e.BandPct == nil},
		{"min_order_base", e.MinOrderBase == nil},
		{"max_value_pct", e.MaxValuePct == nil},
	} {
		if k.missing {
			out = append(out, e.KeyPrefix+"."+k.name)
		}
	}
	return out
}
