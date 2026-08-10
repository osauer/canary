package risk

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// RuleStatusPass and the related constants are rule-row outcomes. Missing,
// pending, or partial inputs cannot produce a false pass.
const (
	RuleStatusPass         = "pass"
	RuleStatusInfo         = "info"
	RuleStatusWatch        = "watch"
	RuleStatusAct          = "act"
	RuleStatusUnknown      = "unknown"
	RuleStatusNotEvaluated = "not_evaluated"
)

// These are the closed, policy-owned reasons that can make one canonical
const (
	EarningsReasonTerminalNonReporting = "terminal_non_reporting"
	EarningsReasonBrokerNonIssuer      = "broker_nonissuer"
	EarningsReasonNonIssuerSecurity    = "nonissuer_security"
	EarningsReasonNotApplicable        = "earnings_not_applicable"
	RuleReasonOffSession               = "off_session"
	RuleReasonNoLongBook               = "no_long_book"
	RuleReasonPnLUnavailable           = "pnl_unavailable"
)

// RuleSingleNameExposure and the related constants identify rules in stable
const (
	RuleSingleNameExposure = "single_name_exposure"
	RuleOptionLinePremium  = "option_line_premium"
	RuleCashSellOnly       = "cash_sell_only"
	RuleExtrinsicBudget    = "extrinsic_budget"
	RuleExpiryRunway       = "expiry_runway"
	RuleCatalystCoverage   = "catalyst_coverage"
	RuleOverwriteEarnings  = "overwrite_earnings"
	RuleEarningsSizeFreeze = "earnings_size_freeze"
	RuleRedOnGreen         = "red_on_green"
	RuleWinnerTrim         = "winner_trim"
	RuleGreenDayAction     = "green_day_action"
	RuleHedgeIntegrity     = "hedge_integrity"
	RuleExitDiscipline     = "exit_discipline"
	RuleFXExposure         = "fx_exposure"
)

// UnderlyingSourceGreeksTick and UnderlyingSourceStockLegMark identify how a
// semantics for compatibility.
const (
	UnderlyingSourceGreeksTick   = "greeks_tick"
	UnderlyingSourceStockLegMark = "stock_leg_mark"
)

// MarketValueBaseSourceSubstituted marks a leg whose MarketValueBase is the
// was available. The number is only meaningful when the leg's currency IS the
// must therefore treat such a leg as unmeasured rather than compare it, because
// an understating pair silences the comparison instead of failing it. The empty
const MarketValueBaseSourceSubstituted = "substituted_unconverted"

// RuleOffender is one name or leg contributing to a breach, worst first.
type RuleOffender struct {
	Symbol     string  `json:"symbol"`
	Leg        string  `json:"leg,omitempty"`
	Observed   float64 `json:"observed"`
	ImpactBase float64 `json:"impact_base,omitempty"`
	Note       string  `json:"note,omitempty"`
}

// RuleRow is one rule's verdict.
type RuleRow struct {
	ID         string         `json:"id"`
	Number     int            `json:"number"`
	Title      string         `json:"title"`
	Status     string         `json:"status"`
	Observed   *float64       `json:"observed,omitempty"`
	Threshold  *float64       `json:"threshold,omitempty"`
	Unit       string         `json:"unit,omitempty"`
	Evidence   string         `json:"evidence"`
	Reason     string         `json:"reason,omitempty"`
	Offenders  []RuleOffender `json:"offenders,omitempty"`
	Exempt     []RuleOffender `json:"exempt,omitempty"`
	ImpactBase float64        `json:"impact_base,omitempty"`
	Notes      []string       `json:"notes,omitempty"`
	// ObservedIsLowerBound marks Observed as a provable minimum computed
	// from partial inputs ("≥ X%"), not an exact measurement. Only breaches
	// may carry it — a lower bound can indict, never acquit.
	ObservedIsLowerBound bool `json:"observed_is_lower_bound,omitempty"`
}

// EarningsInput is the per-name earnings context mapped by the daemon.
type EarningsInput struct {
	Known bool
	// NotApplicable marks fresh exact broker identity proving that this held
	// contract is a nonissuer security. It is independent from reviewed
	NotApplicable bool
	// NonIssuerSecurity marks a holding whose security type has no issuer
	// the position's own security type rather than an exact broker proof, so it
	// is a weaker authority than NotApplicable and stays a separate field: a
	// reader must never mistake a typed classification for a proven contract.
	NonIssuerSecurity bool
	// TerminalNonReporting marks reviewed exact-contract evidence that no
	// future issuer earnings event applies. It is neither a known date nor an
	// unknown: rules 6-8 disclose an exemption for the relevant name.
	TerminalNonReporting bool
	Date                 time.Time // ET calendar date (midnight ET)
	TimeOfDay            string    // "amc" | "bmo" | "" (unspecified)
	Estimated            bool
	Stale                bool
	// SessionsUntil is the number of US equity sessions from today (ET) to
	SessionsUntil *int
	// Source is the daemon's closed provenance vocabulary. verified_terminal
	// means exact-contract authority was present; TerminalNonReporting is true
	// only when that authority is current, identity-matched, and conflict-free.
	Source string
	// Reason is a stable typed explanation for unknown/stale evidence. It is
	// disclosure only and never turns absence into a pass.
	Reason string
}

// LegInput is one option leg of a name.
type LegInput struct {
	Desc       string // "NOW 20260717 C 130"
	Right      string // C | P
	Strike     float64
	Expiry     time.Time // ET calendar date
	DTE        int
	Quantity   float64 // signed contracts
	Multiplier float64
	Mark       float64
	Underlying *float64
	Delta      *float64
	// MarketValueBase is the signed base-currency mark value of the leg.
	MarketValueBase float64
	// MarketValueBaseSource discloses whether MarketValueBase was converted or
	// substituted: empty for a converted figure,
	// MarketValueBaseSourceSubstituted when no FX rate existed and the raw
	// contract-currency value stands in. Follows UnderlyingSource's shape.
	MarketValueBaseSource string
	// ExtrinsicBase is the base-currency extrinsic value for long legs;
	// nil when uncomputable (missing underlying or mark) — never zero it.
	ExtrinsicBase *float64
	// CostBasisBase is the base-currency premium paid for the leg (avg cost
	CostBasisBase *float64
	// FXToBase converts the leg's quote currency into base; nil when the FX
	FXToBase *float64
	// UnderlyingSource discloses where Underlying came from: empty or
	// spots support OTM-ness and extrinsic; they never classify hedge legs.
	UnderlyingSource string
	// HedgeListed marks the underlying as being on the policy hedge list.
	HedgeListed bool
}

// NameInput is the per-underlying aggregation the daemon maps from
// PositionGroup/UnderlyingExposure. ExposureBase must be the same value the
// canary's concentration check reads.
type NameInput struct {
	Symbol string
	// StockConID and StockSecType preserve the held underlying stock's broker
	// identity for exact-contract classifications. Zero means no stock leg or
	// no broker identity; symbol alone never activates such a classification.
	StockConID   int
	StockSecType string
	// UnderlyingSecType is the canonical security type of the held non-option
	UnderlyingSecType string
	// ExposureBase = stock + Σ delta×contracts×multiplier×spot, base ccy.
	ExposureBase float64
	// ExposureBaseComplete reports whether ExposureBase covers every leg the
	// build on a partial sum — "proven ≥" must never overstate.
	ExposureBaseComplete bool
	// GreeksGapNotionalBase is |notional| of option legs missing delta —
	GreeksGapNotionalBase float64
	MarketValueBase       float64
	HasStockLeg           bool
	// StockDayChangePct is the stock leg's quote-enriched day change; nil
	// for option-only names or when enrichment failed.
	StockDayChangePct *float64
	Legs              []LegInput
}

// SourceState is the mapped health of one input source.
type SourceState struct {
	Healthy bool
	Reason  string // positions_pending, account_unavailable, …
}

// RuleInputs is the full mapped snapshot for one evaluation.
type RuleInputs struct {
	AsOf         time.Time
	BaseCurrency string

	Positions SourceState
	Account   SourceState

	NLVBase      *float64
	CashBase     *float64
	DailyPnLBase *float64

	Names []NameInput

	// SessionOpen: US equity regular session per marketcal.
	SessionOpen bool
	// SPYDayChangePct is the market tape for rule 9; nil when unavailable.
	SPYDayChangePct *float64

	Earnings map[string]EarningsInput

	// RegimeStage is the bucketed regime lifecycle stage (RegimeBucket*);
	RegimeStage        string
	RegimeStageAsOf    time.Time
	RegimeStageCarried bool

	// NonBaseNLVBase is the base-currency net liquidation held in non-base
	// currencies (rule 14). nil = unavailable (absent currency report,
	// missing FX) — never zero it: an empty currency report on a book with
	// non-base legs is a data gap, not a base-only book.
	NonBaseNLVBase *float64
	// NonBaseCurrencies names the currencies behind NonBaseNLVBase.
	NonBaseCurrencies []string
}

// Evaluation is the pure result: rows in rulebook order plus the
// hardest-first ranking (indexes into Rows).
type Evaluation struct {
	Rows   []RuleRow
	Ranked []int
}

type ruleContext struct {
	in     RuleInputs
	pol    RulebookPolicy
	nlv    float64
	hasNLV bool
	// overHedged suppresses rule 5's hedge exemption (a "hedge" bigger than
	// the band protects nothing extra; it may be a directional bet).
	overHedged bool
}

// EvaluateRulebook computes all 14 rules. It never returns fewer than 14
func EvaluateRulebook(in RuleInputs, pol RulebookPolicy) Evaluation {
	pol.Normalize()
	ctx := &ruleContext{in: in, pol: pol}
	if in.NLVBase != nil && *in.NLVBase > 0 {
		ctx.nlv = *in.NLVBase
		ctx.hasNLV = true
	}

	// Rule 12 runs first: its over-hedged verdict feeds rule 5's exemption
	r12 := ctx.hedgeIntegrity()
	rows := []RuleRow{
		ctx.singleNameExposure(),
		ctx.optionLinePremium(),
		ctx.cashSellOnly(),
		ctx.extrinsicBudget(),
		ctx.expiryRunway(),
		ctx.catalystCoverage(),
		ctx.overwriteEarnings(),
		ctx.earningsSizeFreeze(),
		ctx.redOnGreen(),
		ctx.winnerTrim(),
		{}, // rule 11 placeholder
		r12,
		ctx.exitDiscipline(),
		ctx.fxExposure(),
	}
	rows[10] = ctx.greenDayAction(rows)
	return Evaluation{Rows: rows, Ranked: rankRows(rows)}
}

// regimeEval runs a regime-conditional rule body under the applicable
// or tighten a verdict, never relax it — in either band direction (a stale
func (c *ruleContext) regimeEval(eval func(RegimeThresholds) RuleRow) RuleRow {
	stage := c.in.RegimeStage
	switch {
	case stage == "":
		row := eval(c.pol.RegimeCalm)
		row.Notes = append(row.Notes, "regime stage never observed — calm thresholds applied; a fresh regime read may tighten this verdict")
		return row
	case !c.in.RegimeStageCarried:
		row := eval(c.pol.SetForBucket(stage))
		note := fmt.Sprintf("thresholds: %s regime set (stage as of %s)", stage, c.in.RegimeStageAsOf.Format("Jan 2 15:04 MST"))
		if stage != RegimeBucketCalm && stage != RegimeBucketEarlyWarning && stage != RegimeBucketConfirmed {
			note = fmt.Sprintf("unrecognized regime stage %q — early-warning thresholds applied", stage)
		}
		row.Notes = append(row.Notes, note)
		return row
	default:
		carried := eval(c.pol.SetForBucket(stage))
		calm := eval(c.pol.RegimeCalm)
		row := carried
		if statusWeight(calm.Status) > statusWeight(carried.Status) {
			row = calm
		}
		row.Notes = append(row.Notes, fmt.Sprintf("regime stage %s carried from %s — worse of carried/calm thresholds applied", stage, c.in.RegimeStageAsOf.Format("Jan 2 15:04 MST")))
		return row
	}
}

// portfolioGate degrades a portfolio-dependent rule when positions or
// account inputs are unhealthy. Returns a non-nil row to emit instead.
func (c *ruleContext) portfolioGate(id string, num int, title string) *RuleRow {
	if !c.in.Positions.Healthy {
		return &RuleRow{ID: id, Number: num, Title: title, Status: RuleStatusUnknown,
			Reason:   nonEmpty(c.in.Positions.Reason, "positions_unavailable"),
			Evidence: "Positions source not healthy — not asserting a pass."}
	}
	if !c.in.Account.Healthy || !c.hasNLV {
		return &RuleRow{ID: id, Number: num, Title: title, Status: RuleStatusUnknown,
			Reason:   nonEmpty(c.in.Account.Reason, "account_unavailable"),
			Evidence: "Account NLV not available — not asserting a pass."}
	}
	return nil
}

func (c *ruleContext) singleNameExposure() RuleRow {
	row := RuleRow{ID: RuleSingleNameExposure, Number: 1, Title: "Per-name exposure cap", Unit: "% NLV"}
	if g := c.portfolioGate(row.ID, row.Number, row.Title); g != nil {
		return *g
	}
	watch, act := c.pol.SingleNameWatchPct, c.pol.SingleNameActPct
	row.Threshold = new(act)
	var offenders, gaps, hedges, unmeasured []RuleOffender
	worst, worstBound := 0.0, 0.0
	for _, n := range c.in.Names {
		if c.greeksGapMaterial(n) {
			// Partial data may indict, never acquit: when the provable
			if bound, ok := nameExposureLowerBound(n); ok {
				if bp := pct(bound, c.nlv); bp >= watch {
					worstBound = math.Max(worstBound, bp)
					offenders = append(offenders, RuleOffender{Symbol: n.Symbol, Observed: round1(bp),
						ImpactBase: bound, Note: "lower bound — delta missing on some legs; true exposure is at least this"})
					continue
				}
			}
			gaps = append(gaps, RuleOffender{Symbol: n.Symbol, Observed: pct(n.GreeksGapNotionalBase, c.nlv),
				Note: "delta unavailable on material legs; exposure understated"})
			continue
		}
		// A name whose base exposure could not be fully measured — missing FX
		// over a book nobody measured. Disclosed unknown, never exposure 0.
		if !n.ExposureBaseComplete {
			unmeasured = append(unmeasured, RuleOffender{Symbol: n.Symbol,
				Note: "exposure not fully measured (FX or price missing) — not compared against the cap"})
			continue
		}
		// A policy-hedge index name carrying net-short delta is the hedge:
		// the real concentration offenders. Exempt only what rule 12 can
		// Disclosed via Exempt, never silently dropped.
		if c.pol.IsHedgeSymbol(n.Symbol) && n.ExposureBase < 0 {
			sized := 0.0
			for _, l := range n.Legs {
				if rule12HedgeLeg(l) {
					sized += math.Abs(*l.Delta * l.Quantity * l.Multiplier * *l.Underlying)
				}
			}
			exempt := math.Min(sized, math.Abs(n.ExposureBase))
			if exempt > 0 {
				hedges = append(hedges, RuleOffender{Symbol: n.Symbol,
					Observed: round1(pct(exempt, c.nlv)),
					Note:     "hedge-classified short exposure — sized by rule 12, not concentration"})
			}
			resid := math.Abs(n.ExposureBase) - exempt
			if resid <= 0 {
				continue
			}
			p := pct(resid, c.nlv)
			worst = math.Max(worst, p)
			if p >= watch {
				offenders = append(offenders, RuleOffender{Symbol: n.Symbol, Observed: round1(p),
					ImpactBase: resid, Note: "short exposure beyond rule-12-sized hedge legs"})
			}
			continue
		}
		p := pct(math.Abs(n.ExposureBase), c.nlv)
		worst = math.Max(worst, p)
		if p >= watch {
			offenders = append(offenders, RuleOffender{Symbol: n.Symbol, Observed: round1(p),
				ImpactBase: math.Abs(n.ExposureBase)})
		}
	}
	sortOffenders(offenders)
	effective := math.Max(worst, worstBound)
	row.Observed = new(round1(effective))
	row.ObservedIsLowerBound = worstBound > worst
	row.Offenders = offenders
	row.Exempt = hedges
	bound := ""
	if row.ObservedIsLowerBound {
		bound = " (lower bound)"
	}
	switch {
	case effective > act:
		row.Status = RuleStatusAct
		row.Evidence = fmt.Sprintf("%s at %.1f%%%s of NLV exceeds the %.0f%% cap.", offenders[0].Symbol, offenders[0].Observed, bound, act)
	case effective >= watch:
		row.Status = RuleStatusWatch
		row.Evidence = fmt.Sprintf("%s at %.1f%%%s of NLV approaches the %.0f%% cap.", offenders[0].Symbol, offenders[0].Observed, bound, act)
	case len(gaps) > 0 || len(unmeasured) > 0:
		// No pass over an unmeasured book: a name that was never converted or
		// never fully priced was never compared against the cap.
		row.Status = RuleStatusUnknown
		row.ObservedIsLowerBound = false
		row.Offenders = append(append(offenders, gaps...), unmeasured...)
		switch {
		case len(gaps) == 0:
			row.Reason = "exposure_incomplete"
			row.Evidence = fmt.Sprintf("%d name(s) with exposure never fully measured (FX or price missing) — not asserting a pass.", len(unmeasured))
		case len(unmeasured) == 0:
			row.Reason = "greeks_gap"
			row.Evidence = fmt.Sprintf("%d name(s) missing delta on material legs — exposure not trustworthy.", len(gaps))
		default:
			row.Reason = "greeks_gap"
			row.Evidence = fmt.Sprintf("%d name(s) missing delta and %d with unmeasured exposure — exposure not trustworthy.", len(gaps), len(unmeasured))
		}
	default:
		row.Status = RuleStatusPass
		row.Evidence = fmt.Sprintf("Largest name %.1f%% of NLV, under the %.0f%% cap.", round1(worst), act)
	}
	// Disclosure is unconditional: a measured breach stands, and the names
	if row.Status != RuleStatusUnknown && len(gaps)+len(unmeasured) > 0 {
		row.Offenders = append(append(row.Offenders, gaps...), unmeasured...)
		row.Notes = append(row.Notes, fmt.Sprintf("%d name(s) additionally not fully assessable (delta or FX missing) — the breach above is proven regardless.", len(gaps)+len(unmeasured)))
	}
	for _, o := range row.Offenders {
		row.ImpactBase += o.ImpactBase
	}
	return row
}

// nameExposureLowerBound computes a provable minimum |net delta-dollar|
// exposure for a name whose material legs miss delta. Known legs are already
// summed into ExposureBase; each delta-less leg contributes a signed
// interval: a long call at least its intrinsic (delta·S ≥ C ≥ intrinsic) and
// at most its notional; a long put between −notional and 0; shorts mirrored.
// Put intrinsic is NOT a bound on |delta·S| (deep-ITM K−S can exceed S) and
// is never used. Any delta-less leg missing underlying or FX makes the
// interval unbounded — nothing is provable.
func nameExposureLowerBound(n NameInput) (bound float64, ok bool) {
	if !n.ExposureBaseComplete {
		return 0, false // partial known sum — nothing is provable from it
	}
	low, high := n.ExposureBase, n.ExposureBase
	for _, l := range n.Legs {
		if l.Delta != nil || l.Quantity == 0 {
			continue
		}
		if l.Underlying == nil || l.FXToBase == nil {
			return 0, false
		}
		qty := math.Abs(l.Quantity)
		notional := qty * l.Multiplier * *l.Underlying * *l.FXToBase
		callIntrinsic := 0.0
		if isCall(l.Right) {
			callIntrinsic = OptionIntrinsicPerShare(l.Right, *l.Underlying, l.Strike) * qty * l.Multiplier * *l.FXToBase
		}
		switch {
		case l.Quantity > 0 && isCall(l.Right):
			low += callIntrinsic
			high += notional
		case l.Quantity > 0 && isPut(l.Right):
			low -= notional
		case l.Quantity < 0 && isCall(l.Right):
			low -= notional
			high -= callIntrinsic
		case l.Quantity < 0 && isPut(l.Right):
			high += notional
		default:
			return 0, false // unrecognized right — nothing provable
		}
	}
	switch {
	case low > 0:
		return low, true
	case high < 0:
		return -high, true
	default:
		return 0, false // interval straddles zero
	}
}

func (c *ruleContext) optionLinePremium() RuleRow {
	row := RuleRow{ID: RuleOptionLinePremium, Number: 2, Title: "Single option line premium", Unit: "% NLV"}
	if g := c.portfolioGate(row.ID, row.Number, row.Title); g != nil {
		return *g
	}
	watch, act := c.pol.OptionLineWatchPct, c.pol.OptionLineActPct
	hWatch, hAct := c.pol.HedgeLineWatchPct, c.pol.HedgeLineActPct
	row.Threshold = new(watch)
	var normalOff, hedgeOff, unmeasured []RuleOffender
	worst, hedgeWorst := 0.0, 0.0
	for _, n := range c.in.Names {
		for _, l := range n.Legs {
			if l.Quantity <= 0 {
				continue
			}
			// A substituted base value is wrong by the exchange rate, and an
			// understating pair would simply keep the leg under the tier and
			// report a quiet pass on a line that breaches. This rule has no
			// unknown tier of its own — tierStatus returns pass, watch or act —
			// so an unmeasurable leg is collected and forces the row to unknown
			// below rather than being compared or silently skipped.
			if l.MarketValueBaseSource == MarketValueBaseSourceSubstituted {
				unmeasured = append(unmeasured, RuleOffender{Symbol: n.Symbol, Leg: l.Desc,
					Note: "premium not convertible to base — no FX rate for the leg's currency"})
				continue
			}
			p := pct(math.Abs(l.MarketValueBase), c.nlv)
			// Hedge-classified legs measure against their own premium tier:
			// rule 12 owns the hedge's sizing; this tier only bounds how much
			if rule12HedgeLeg(l) {
				hedgeWorst = math.Max(hedgeWorst, p)
				if p >= hWatch {
					hedgeOff = append(hedgeOff, RuleOffender{Symbol: n.Symbol, Leg: l.Desc,
						Observed: round1(p), ImpactBase: math.Abs(l.MarketValueBase),
						Note: fmt.Sprintf("hedge-premium tier (watch %.0f%%/act %.0f%%) — sized by rule 12", hWatch, hAct)})
				}
				continue
			}
			worst = math.Max(worst, p)
			if p >= watch {
				normalOff = append(normalOff, RuleOffender{Symbol: n.Symbol, Leg: l.Desc,
					Observed: round1(p), ImpactBase: math.Abs(l.MarketValueBase)})
			}
		}
	}
	sortOffenders(normalOff)
	sortOffenders(hedgeOff)
	offenders := append(append([]RuleOffender{}, normalOff...), hedgeOff...)
	sortOffenders(offenders)
	row.Observed = new(round1(worst))
	row.Offenders = offenders
	normal := tierStatus(worst, watch, act)
	hedge := tierStatus(hedgeWorst, hWatch, hAct)
	status := normal
	hedgeWins := statusWeight(hedge) > statusWeight(normal)
	if hedgeWins {
		status = hedge
		// Observed and Threshold are the generic renderer contract. When the
		// hedge tier drives the verdict, they must describe that tier rather
		row.Observed = new(round1(hedgeWorst))
		row.Threshold = new(hWatch)
	}
	// No input condition may produce a pass by absence of data. A leg whose
	// premium could not be converted was never compared against either tier, so
	// must not be downgraded to unknown by an unrelated leg's missing rate.
	// Disclosure is unconditional; only the status is conditional. Appending
	row.Offenders = append(offenders, unmeasured...)
	if len(unmeasured) > 0 && status == RuleStatusPass {
		status = RuleStatusUnknown
		row.Reason = "premium_unconvertible"
	}
	row.Status = status
	// The headline names the offender of the tier that produced the status,
	// cut the hedge (the exact confusion the tier split exists to end).
	switch {
	case status == RuleStatusUnknown:
		// Must precede the tier cases: with every leg unconvertible both
		row.Evidence = fmt.Sprintf("%d option leg(s) have no FX rate to base — their premium was not measured, so no pass is asserted.", len(unmeasured))
	case status == RuleStatusPass:
		row.Evidence = fmt.Sprintf("Largest option line %.1f%% of NLV, under the %.0f%% cap.", round1(worst), watch)
	case hedgeWins:
		row.Evidence = fmt.Sprintf("%s holds %.1f%% of NLV in one hedge line (hedge tier %.0f%%/%.0f%%).", hedgeOff[0].Leg, hedgeOff[0].Observed, hWatch, hAct)
	default:
		row.Evidence = fmt.Sprintf("%s holds %.1f%% of NLV in one option line (cap %.0f%%).", normalOff[0].Leg, normalOff[0].Observed, watch)
	}
	if hedgeWorst > 0 {
		row.Notes = append(row.Notes, fmt.Sprintf("largest hedge line %.1f%% of NLV against the %.0f%%/%.0f%% hedge tier", round1(hedgeWorst), hWatch, hAct))
	}
	for _, o := range row.Offenders {
		row.ImpactBase += o.ImpactBase
	}
	return row
}

// tierStatus maps an observed percentage onto pass/watch/act for one
func tierStatus(observed, watch, act float64) string {
	switch {
	case observed > act:
		return RuleStatusAct
	case observed >= watch:
		return RuleStatusWatch
	default:
		return RuleStatusPass
	}
}

func (c *ruleContext) cashSellOnly() RuleRow {
	row := RuleRow{ID: RuleCashSellOnly, Number: 3, Title: "Negative cash sell-only posture", Unit: "% NLV"}
	if !c.in.Account.Healthy || !c.hasNLV || c.in.CashBase == nil {
		row.Status = RuleStatusUnknown
		row.Reason = nonEmpty(c.in.Account.Reason, "cash_unavailable")
		row.Evidence = "Cash balance not available — not asserting a pass."
		return row
	}
	ratio := pct(*c.in.CashBase, c.nlv)
	return c.regimeEval(func(rt RegimeThresholds) RuleRow {
		r := row
		limit := rt.CashSellOnlyPct
		r.Observed = new(round1(ratio))
		r.Threshold = new(limit)
		if ratio < limit {
			r.Status = RuleStatusAct
			r.Evidence = fmt.Sprintf("Cash at %.1f%% of NLV is below %.0f%% — take an advisory sell-only posture until the debit shrinks (margin interest is negative carry too).", round1(ratio), limit)
		} else {
			r.Status = RuleStatusPass
			r.Evidence = fmt.Sprintf("Cash at %.1f%% of NLV, above the %.0f%% floor.", round1(ratio), limit)
		}
		return r
	})
}

func (c *ruleContext) extrinsicBudget() RuleRow {
	row := RuleRow{ID: RuleExtrinsicBudget, Number: 4, Title: "Portfolio extrinsic budget", Unit: "% NLV"}
	if g := c.portfolioGate(row.ID, row.Number, row.Title); g != nil {
		return *g
	}
	total, hedgeTotal := 0.0, 0.0
	var offenders, unknowns []RuleOffender
	for _, n := range c.in.Names {
		for _, l := range n.Legs {
			if l.Quantity <= 0 {
				continue
			}
			if l.ExtrinsicBase == nil {
				// The materiality floor decides whether an uncomputable leg is
				// disclosed at all. A substituted base value cannot be measured
				// against it: an understating currency pair drops the leg below
				// the floor and suppresses the very disclosure this branch
				// exists to make. Such a leg is therefore always listed.
				if l.MarketValueBaseSource == MarketValueBaseSourceSubstituted ||
					pct(math.Abs(l.MarketValueBase), c.nlv) >= c.pol.GreeksGapFloorPctNLV {
					unknowns = append(unknowns, RuleOffender{Symbol: n.Symbol, Leg: l.Desc,
						Note: "extrinsic uncomputable (missing underlying or mark)"})
				}
				continue
			}
			// The budget bounds speculative decay; the hedge's premium is
			// governed by rule 2's hedge tier and rule 12's band. An
			// unclassifiable leg (no delta yet) counts against the budget —
			// no relief without classification.
			if rule12HedgeLeg(l) {
				hedgeTotal += *l.ExtrinsicBase
				continue
			}
			total += *l.ExtrinsicBase
			offenders = append(offenders, RuleOffender{Symbol: n.Symbol, Leg: l.Desc,
				Observed: round1(pct(*l.ExtrinsicBase, c.nlv)), ImpactBase: *l.ExtrinsicBase})
		}
	}
	p := pct(total, c.nlv)
	row.Observed = new(round1(p))
	row.ImpactBase = total
	if len(unknowns) > 0 {
		row.Status = RuleStatusUnknown
		row.Reason = "extrinsic_uncomputable"
		row.Offenders = unknowns
		row.Evidence = fmt.Sprintf("%d material leg(s) with uncomputable extrinsic — budget not trustworthy.", len(unknowns))
		return row
	}
	sortOffenders(offenders)
	if len(offenders) > 3 {
		offenders = offenders[:3]
	}
	row.Offenders = offenders
	if hedgeTotal > 0 {
		row.Notes = append(row.Notes, fmt.Sprintf("hedge extrinsic %.1f%% of NLV excluded from this budget (rule 2 hedge tier / rule 12 govern the hedge)", round1(pct(hedgeTotal, c.nlv))))
	}
	return c.regimeEval(func(rt RegimeThresholds) RuleRow {
		r := row
		watch, act := rt.ExtrinsicWatchPct, rt.ExtrinsicActPct
		r.Threshold = new(watch)
		switch {
		case p > act:
			r.Status = RuleStatusAct
			r.Evidence = fmt.Sprintf("Paying decay on %.1f%% of NLV in speculative extrinsic (budget %.0f%%).", round1(p), watch)
		case p >= watch:
			r.Status = RuleStatusWatch
			r.Evidence = fmt.Sprintf("Speculative extrinsic at %.1f%% of NLV against a %.0f%% budget.", round1(p), watch)
		default:
			r.Status = RuleStatusPass
			r.Evidence = fmt.Sprintf("Speculative extrinsic at %.1f%% of NLV, inside the %.0f%% budget.", round1(p), watch)
		}
		return r
	})
}

func (c *ruleContext) expiryRunway() RuleRow {
	row := RuleRow{ID: RuleExpiryRunway, Number: 5, Title: "Expiry runway", Unit: "DTE"}
	if g := c.portfolioGate(row.ID, row.Number, row.Title); g != nil {
		return *g
	}
	watchDTE, actDTE := c.pol.RunwayWatchDTE, c.pol.RunwayActDTE
	row.Threshold = new(float64(watchDTE))
	var offenders, exempt []RuleOffender
	worstStatus := RuleStatusPass
	minDTE := math.Inf(1)
	for _, n := range c.in.Names {
		for _, l := range n.Legs {
			if l.Quantity <= 0 || l.DTE >= watchDTE {
				continue
			}
			minDTE = math.Min(minDTE, float64(l.DTE))
			if l.Delta != nil && math.Abs(*l.Delta) >= c.pol.RunwayITMDeltaFloor {
				exempt = append(exempt, RuleOffender{Symbol: n.Symbol, Leg: l.Desc, Observed: float64(l.DTE),
					Note: fmt.Sprintf("ITM exemption (|delta| %.2f)", math.Abs(*l.Delta))})
				continue
			}
			if l.HedgeListed && isPut(l.Right) {
				if c.overHedged {
					offenders = append(offenders, RuleOffender{Symbol: n.Symbol, Leg: l.Desc, Observed: float64(l.DTE),
						ImpactBase: math.Abs(l.MarketValueBase), Note: "hedge exemption suppressed: hedge band breached high"})
					worstStatus = worseRunway(worstStatus, l.DTE, actDTE)
					continue
				}
				exempt = append(exempt, RuleOffender{Symbol: n.Symbol, Leg: l.Desc, Observed: float64(l.DTE), Note: "hedge leg"})
				continue
			}
			note := ""
			if l.Delta == nil {
				note = "delta unavailable — ITM exemption not assessable, flagged conservatively"
			}
			offenders = append(offenders, RuleOffender{Symbol: n.Symbol, Leg: l.Desc, Observed: float64(l.DTE),
				ImpactBase: math.Abs(l.MarketValueBase), Note: note})
			worstStatus = worseRunway(worstStatus, l.DTE, actDTE)
		}
	}
	sort.Slice(offenders, func(i, j int) bool { return offenders[i].Observed < offenders[j].Observed })
	row.Offenders = offenders
	row.Exempt = exempt
	for _, o := range offenders {
		row.ImpactBase += o.ImpactBase
	}
	if len(offenders) == 0 {
		row.Status = RuleStatusPass
		row.Evidence = fmt.Sprintf("No long option inside %d DTE without an exemption.", watchDTE)
		return row
	}
	row.Observed = new(minDTE)
	row.Status = worstStatus
	row.Evidence = fmt.Sprintf("%d long option line(s) inside %d DTE — roll or close before the final-week decay cliff.", len(offenders), watchDTE)
	return row
}

// earningsFor resolves usable earnings context for a name; ok=false means
// unknown/stale and the caller must degrade, not pass.
func (c *ruleContext) earningsFor(sym string) (EarningsInput, bool) {
	e, found := c.in.Earnings[sym]
	if !found || !e.Known || e.Stale {
		return e, false
	}
	return e, true
}

func (c *ruleContext) terminalEarningsFor(sym string) (EarningsInput, bool) {
	e, found := c.in.Earnings[sym]
	if !found || !e.TerminalNonReporting || e.Stale || e.Source != "verified_terminal" || e.Reason == "" {
		return e, false
	}
	return e, true
}

// nonIssuerSecurityEarningsFor recognizes a holding whose security type has no
// issuer earnings concept. The daemon classifies the type; this only accepts
// its closed vocabulary. Unlike the broker-identity path it needs no exact
func (c *ruleContext) nonIssuerSecurityEarningsFor(sym string) (EarningsInput, bool) {
	e, found := c.in.Earnings[sym]
	if !found || !e.NonIssuerSecurity || e.Stale || e.Source != "security_type" {
		return e, false
	}
	return e, true
}

func (c *ruleContext) brokerNonIssuerEarningsFor(name NameInput) (EarningsInput, bool) {
	e, found := c.in.Earnings[name.Symbol]
	// The broker proof identifies the held stock contract only. Without each
	// option leg's exact underlying ConID, a mixed symbol group must use the
	// ordinary issuer-event evidence path.
	if len(name.Legs) != 0 || !found || !e.NotApplicable || e.Stale || e.Source != "broker_identity" || e.Reason != EarningsReasonBrokerNonIssuer {
		return e, false
	}
	return e, true
}

// unresolvedTerminalEarningsFor identifies exact-contract terminal authority
// typed boundary; these states must fail closed before option-side or size
func (c *ruleContext) unresolvedTerminalEarningsFor(sym string) (EarningsInput, bool) {
	e, found := c.in.Earnings[sym]
	if !found || e.Source != "verified_terminal" {
		return e, false
	}
	if _, accepted := c.terminalEarningsFor(sym); accepted {
		return e, false
	}
	return e, true
}

func (c *ruleContext) unresolvedBrokerEarningsFor(name NameInput) (EarningsInput, bool) {
	e, found := c.in.Earnings[name.Symbol]
	if !found || e.Source != "broker_identity" {
		return e, false
	}
	if _, accepted := c.brokerNonIssuerEarningsFor(name); accepted {
		return e, false
	}
	return e, true
}

func unresolvedTerminalEarningsOffender(sym string) RuleOffender {
	return RuleOffender{Symbol: sym, Note: "exact-contract terminal authority is unresolved; no date or exemption is usable"}
}

func unresolvedBrokerEarningsOffender(sym string) RuleOffender {
	return RuleOffender{Symbol: sym, Note: "exact-contract broker identity authority is unresolved; no date or exemption is usable"}
}

func unresolvedIssuerEarningsOffender(sym string, e EarningsInput) RuleOffender {
	return RuleOffender{Symbol: sym, Note: "ordinary issuer earnings date " + earningsGapWord(e)}
}

func terminalEarningsExemption(sym string) RuleOffender {
	return RuleOffender{Symbol: sym, Note: "exact contract is verified terminal/non-reporting; no future issuer earnings event applies"}
}

func brokerNonIssuerEarningsExemption(sym string) RuleOffender {
	return RuleOffender{Symbol: sym, Note: "exact broker identity proves a nonissuer security; issuer earnings do not apply"}
}

func nonIssuerSecurityEarningsExemption(sym string) RuleOffender {
	return RuleOffender{Symbol: sym, Note: "security type has no issuer earnings; issuer earnings do not apply"}
}

// earningsExemptionReason names the exemption class when exactly one kind of
// authority produced every exempt row. A mix stays the generic not-applicable
// reason so a reader cannot infer a single authority from a mixed set.
func earningsExemptionReason(terminal, broker, secType int) string {
	switch {
	case broker > 0 && terminal == 0 && secType == 0:
		return EarningsReasonBrokerNonIssuer
	case terminal > 0 && broker == 0 && secType == 0:
		return EarningsReasonTerminalNonReporting
	case secType > 0 && terminal == 0 && broker == 0:
		return EarningsReasonNonIssuerSecurity
	default:
		return EarningsReasonNotApplicable
	}
}

func (c *ruleContext) catalystCoverage() RuleRow {
	row := RuleRow{ID: RuleCatalystCoverage, Number: 6, Title: "Option outlives its catalyst"}
	if g := c.portfolioGate(row.ID, row.Number, row.Title); g != nil {
		return *g
	}
	var offenders, unknowns, exempt []RuleOffender
	earningsDrove := false
	assessed := 0
	terminalExempt, brokerExempt, secTypeExempt := 0, 0, 0
	for _, n := range c.in.Names {
		if _, terminal := c.terminalEarningsFor(n.Symbol); terminal {
			exempt = append(exempt, terminalEarningsExemption(n.Symbol))
			terminalExempt++
			continue
		}
		if _, nonissuer := c.brokerNonIssuerEarningsFor(n); nonissuer {
			exempt = append(exempt, brokerNonIssuerEarningsExemption(n.Symbol))
			brokerExempt++
			continue
		}
		if _, nonissuer := c.nonIssuerSecurityEarningsFor(n.Symbol); nonissuer {
			exempt = append(exempt, nonIssuerSecurityEarningsExemption(n.Symbol))
			secTypeExempt++
			continue
		}
		if _, unresolved := c.unresolvedTerminalEarningsFor(n.Symbol); unresolved {
			earningsDrove = true
			unknowns = append(unknowns, unresolvedTerminalEarningsOffender(n.Symbol))
			continue
		}
		if _, unresolved := c.unresolvedBrokerEarningsFor(n); unresolved {
			earningsDrove = true
			unknowns = append(unknowns, unresolvedBrokerEarningsOffender(n.Symbol))
			continue
		}
		if e, ok := c.earningsFor(n.Symbol); !ok {
			// Unknown ordinary issuers are unresolved before option relevance.
			earningsDrove = true
			unknowns = append(unknowns, unresolvedIssuerEarningsOffender(n.Symbol, e))
			continue
		}
		hasLong := false
		for _, l := range n.Legs {
			if l.Quantity > 0 {
				hasLong = true
				break
			}
		}
		if !hasLong {
			continue
		}
		assessed++
		var otmLegs []LegInput
		unassessable := 0
		for _, l := range n.Legs {
			if l.Quantity <= 0 {
				continue
			}
			if l.Underlying == nil {
				// OTM-ness unassessable — a named unknown, never a silent
				unassessable++
				continue
			}
			if OptionIntrinsicPerShare(l.Right, *l.Underlying, l.Strike) > 0 {
				continue
			}
			otmLegs = append(otmLegs, l)
		}
		if unassessable > 0 {
			unknowns = append(unknowns, RuleOffender{Symbol: n.Symbol,
				Note: fmt.Sprintf("%d long leg(s) with underlying unavailable — OTM-ness unassessable", unassessable)})
		}
		if len(otmLegs) == 0 {
			continue
		}
		e, ok := c.earningsFor(n.Symbol)
		if !ok {
			earningsDrove = true
			unknowns = append(unknowns, RuleOffender{Symbol: n.Symbol,
				Note: fmt.Sprintf("%d OTM long leg(s), earnings date %s", len(otmLegs), earningsGapWord(e))})
			continue
		}
		for _, l := range otmLegs {
			if expiresBeforeCatalyst(l.Expiry, e) {
				offenders = append(offenders, RuleOffender{Symbol: n.Symbol, Leg: l.Desc,
					Observed: float64(l.DTE), ImpactBase: math.Abs(l.MarketValueBase),
					Note: fmt.Sprintf("expires %s, earnings %s%s", l.Expiry.Format("Jan 2"), e.Date.Format("Jan 2"), estNote(e))})
			}
		}
	}
	sortOffenders(offenders)
	row.Offenders = offenders
	row.Exempt = exempt
	for _, o := range offenders {
		row.ImpactBase += o.ImpactBase
	}
	switch {
	case len(offenders) > 0:
		row.Status = RuleStatusWatch
		row.Evidence = fmt.Sprintf("%d OTM long option(s) die before their name's earnings — decay with no catalyst inside the option's life.", len(offenders))
		row.Offenders = append(offenders, unknowns...)
	case len(unknowns) > 0:
		row.Status = RuleStatusUnknown
		row.Reason = "underlying_unavailable"
		if earningsDrove {
			row.Reason = "earnings_unknown"
		}
		row.Offenders = unknowns
		row.Evidence = fmt.Sprintf("%d name(s) not assessable (missing earnings date or option underlying).", len(unknowns))
	case assessed == 0 && len(exempt) > 0:
		row.Status = RuleStatusNotEvaluated
		row.Reason = earningsExemptionReason(terminalExempt, brokerExempt, secTypeExempt)
		row.Evidence = fmt.Sprintf("%d exact contract(s) are exempt from issuer earnings catalyst checks.", len(exempt))
	default:
		row.Status = RuleStatusPass
		row.Evidence = "Every OTM long option outlives its name's next earnings (or no OTM longs held)."
	}
	return row
}

func (c *ruleContext) overwriteEarnings() RuleRow {
	row := RuleRow{ID: RuleOverwriteEarnings, Number: 7, Title: "Overwrite spans earnings"}
	if g := c.portfolioGate(row.ID, row.Number, row.Title); g != nil {
		return *g
	}
	var actOffenders, watchOffenders, unknowns, exempt []RuleOffender
	assessed := 0
	terminalExempt, brokerExempt, secTypeExempt := 0, 0, 0
	for _, n := range c.in.Names {
		if _, terminal := c.terminalEarningsFor(n.Symbol); terminal {
			exempt = append(exempt, terminalEarningsExemption(n.Symbol))
			terminalExempt++
			continue
		}
		if _, nonissuer := c.brokerNonIssuerEarningsFor(n); nonissuer {
			exempt = append(exempt, brokerNonIssuerEarningsExemption(n.Symbol))
			brokerExempt++
			continue
		}
		if _, nonissuer := c.nonIssuerSecurityEarningsFor(n.Symbol); nonissuer {
			exempt = append(exempt, nonIssuerSecurityEarningsExemption(n.Symbol))
			secTypeExempt++
			continue
		}
		if _, unresolved := c.unresolvedTerminalEarningsFor(n.Symbol); unresolved {
			unknowns = append(unknowns, unresolvedTerminalEarningsOffender(n.Symbol))
			continue
		}
		if _, unresolved := c.unresolvedBrokerEarningsFor(n); unresolved {
			unknowns = append(unknowns, unresolvedBrokerEarningsOffender(n.Symbol))
			continue
		}
		if e, ok := c.earningsFor(n.Symbol); !ok {
			// Resolve ordinary issuer applicability before short-option
			unknowns = append(unknowns, unresolvedIssuerEarningsOffender(n.Symbol, e))
			continue
		}
		hasShort := false
		for _, l := range n.Legs {
			if l.Quantity < 0 {
				hasShort = true
				break
			}
		}
		if !hasShort {
			continue
		}
		assessed++
		var shortCalls, shortPuts []LegInput
		for _, l := range n.Legs {
			switch {
			case l.Quantity < 0 && isCall(l.Right):
				shortCalls = append(shortCalls, l)
			case l.Quantity < 0 && isPut(l.Right):
				shortPuts = append(shortPuts, l)
			}
		}
		if len(shortCalls) == 0 && len(shortPuts) == 0 {
			continue
		}
		e, ok := c.earningsFor(n.Symbol)
		if !ok {
			unknowns = append(unknowns, RuleOffender{Symbol: n.Symbol,
				Note: fmt.Sprintf("%d short option leg(s), earnings date %s", len(shortCalls)+len(shortPuts), earningsGapWord(e))})
			continue
		}
		for _, l := range shortCalls {
			spans, ambiguous := spansEarningsGap(c.in.AsOf, l.Expiry, e)
			if !spans {
				continue
			}
			note := fmt.Sprintf("short through earnings %s%s", e.Date.Format("Jan 2"), estNote(e))
			if ambiguous {
				note += "; time-of-day unknown, flagged conservatively"
			}
			actOffenders = append(actOffenders, RuleOffender{Symbol: n.Symbol, Leg: l.Desc,
				ImpactBase: math.Abs(l.MarketValueBase), Note: note})
		}
		// Short puts spanning the print: watch by default, act when the
		// tier is unassessable — stay at watch, disclosed, never quietly
		var namePutOffenders []RuleOffender
		namePutPct, nameTierKnown := 0.0, true
		for _, l := range shortPuts {
			spans, ambiguous := spansEarningsGap(c.in.AsOf, l.Expiry, e)
			if !spans {
				continue
			}
			note := fmt.Sprintf("short put through earnings %s%s", e.Date.Format("Jan 2"), estNote(e))
			if ambiguous {
				note += "; time-of-day unknown, flagged conservatively"
			}
			o := RuleOffender{Symbol: n.Symbol, Leg: l.Desc, ImpactBase: math.Abs(l.MarketValueBase)}
			if l.FXToBase == nil {
				nameTierKnown = false
				o.Note = note + "; assignment notional unassessable (FX unknown)"
				namePutOffenders = append(namePutOffenders, o)
				continue
			}
			assignBase := l.Strike * l.Multiplier * math.Abs(l.Quantity) * *l.FXToBase
			linePct := pct(assignBase, c.nlv)
			namePutPct += linePct
			o.Observed = round1(linePct)
			o.Note = note + fmt.Sprintf("; assignment %.1f%% of NLV", round1(linePct))
			if linePct >= c.pol.ShortPutActLinePctNLV {
				actOffenders = append(actOffenders, o)
			} else {
				namePutOffenders = append(namePutOffenders, o)
			}
		}
		if nameTierKnown && namePutPct >= c.pol.ShortPutActNamePctNLV {
			actOffenders = append(actOffenders, namePutOffenders...)
		} else {
			watchOffenders = append(watchOffenders, namePutOffenders...)
		}
	}
	sortOffenders(actOffenders)
	sortOffenders(watchOffenders)
	offenders := append(actOffenders, watchOffenders...)
	row.Offenders = offenders
	row.Exempt = exempt
	for _, o := range offenders {
		row.ImpactBase += o.ImpactBase
	}
	switch {
	case len(actOffenders) > 0:
		row.Status = RuleStatusAct
		row.Evidence = fmt.Sprintf("%d short option line(s) span an earnings print at act severity — capped upside or forced assignment through the exact event that pays.", len(actOffenders))
		row.Offenders = append(offenders, unknowns...)
	case len(watchOffenders) > 0:
		row.Status = RuleStatusWatch
		row.Evidence = fmt.Sprintf("%d short put line(s) span an earnings print — assignment risk through the gap.", len(watchOffenders))
		row.Offenders = append(offenders, unknowns...)
	case len(unknowns) > 0:
		row.Status = RuleStatusUnknown
		row.Reason = "earnings_unknown"
		row.Offenders = unknowns
		row.Evidence = fmt.Sprintf("%d name(s) have no usable earnings date for the overwrite check.", len(unknowns))
	case assessed == 0 && len(exempt) > 0:
		row.Status = RuleStatusNotEvaluated
		row.Reason = earningsExemptionReason(terminalExempt, brokerExempt, secTypeExempt)
		row.Evidence = fmt.Sprintf("%d exact contract(s) are exempt from issuer earnings print checks.", len(exempt))
	default:
		row.Status = RuleStatusPass
		row.Evidence = "No short option spans a known earnings print."
	}
	return row
}

func (c *ruleContext) earningsSizeFreeze() RuleRow {
	row := RuleRow{ID: RuleEarningsSizeFreeze, Number: 8, Title: "At size before earnings", Unit: "sessions"}
	if g := c.portfolioGate(row.ID, row.Number, row.Title); g != nil {
		return *g
	}
	freeze := c.pol.EarningsFreezeSessions
	row.Threshold = new(float64(freeze))
	var offenders, unknowns, exempt []RuleOffender
	assessed := 0
	terminalExempt, brokerExempt, secTypeExempt := 0, 0, 0
	for _, n := range c.in.Names {
		if _, terminal := c.terminalEarningsFor(n.Symbol); terminal {
			exempt = append(exempt, terminalEarningsExemption(n.Symbol))
			terminalExempt++
			continue
		}
		if _, nonissuer := c.brokerNonIssuerEarningsFor(n); nonissuer {
			exempt = append(exempt, brokerNonIssuerEarningsExemption(n.Symbol))
			brokerExempt++
			continue
		}
		if _, nonissuer := c.nonIssuerSecurityEarningsFor(n.Symbol); nonissuer {
			exempt = append(exempt, nonIssuerSecurityEarningsExemption(n.Symbol))
			secTypeExempt++
			continue
		}
		if _, unresolved := c.unresolvedTerminalEarningsFor(n.Symbol); unresolved {
			unknowns = append(unknowns, unresolvedTerminalEarningsOffender(n.Symbol))
			continue
		}
		if _, unresolved := c.unresolvedBrokerEarningsFor(n); unresolved {
			unknowns = append(unknowns, unresolvedBrokerEarningsOffender(n.Symbol))
			continue
		}
		if e, ok := c.earningsFor(n.Symbol); !ok {
			// Resolve ordinary issuer applicability before the size threshold.
			unknowns = append(unknowns, unresolvedIssuerEarningsOffender(n.Symbol, e))
			continue
		}
		assessed++
		if c.greeksGapMaterial(n) {
			// Exposure not assessable — the freeze cannot be ruled out
			if e, ok := c.earningsFor(n.Symbol); ok && e.SessionsUntil != nil &&
				(*e.SessionsUntil < 0 || *e.SessionsUntil > freeze) {
				continue // earnings provably outside the freeze window
			}
			unknowns = append(unknowns, RuleOffender{Symbol: n.Symbol,
				Note: "size not assessable (delta missing) with earnings unknown or near — freeze window can't be ruled out"})
			continue
		}
		// Same escape and same duty for a name whose exposure was never fully
		// floor by construction, so comparing it silently skips exactly the
		// oversized-into-earnings name this rule exists to freeze. Earnings
		if !n.ExposureBaseComplete {
			if e, ok := c.earningsFor(n.Symbol); ok && e.SessionsUntil != nil &&
				(*e.SessionsUntil < 0 || *e.SessionsUntil > freeze) {
				continue
			}
			unknowns = append(unknowns, RuleOffender{Symbol: n.Symbol,
				Note: "size not assessable (exposure not fully measured) with earnings unknown or near — freeze window can't be ruled out"})
			continue
		}
		p := pct(math.Abs(n.ExposureBase), c.nlv)
		if p < c.pol.SingleNameWatchPct {
			continue
		}
		e, ok := c.earningsFor(n.Symbol)
		if !ok {
			unknowns = append(unknowns, RuleOffender{Symbol: n.Symbol, Observed: round1(p),
				Note: "oversized with earnings date " + earningsGapWord(e)})
			continue
		}
		if e.SessionsUntil == nil {
			unknowns = append(unknowns, RuleOffender{Symbol: n.Symbol, Observed: round1(p), Note: "session distance uncomputable"})
			continue
		}
		if *e.SessionsUntil >= 0 && *e.SessionsUntil <= freeze {
			offenders = append(offenders, RuleOffender{Symbol: n.Symbol, Observed: float64(*e.SessionsUntil),
				ImpactBase: math.Abs(n.ExposureBase),
				Note:       fmt.Sprintf("%.1f%% of NLV, earnings %s%s in %d session(s)", round1(p), e.Date.Format("Jan 2"), estNote(e), *e.SessionsUntil)})
		}
	}
	sortOffenders(offenders)
	row.Offenders = offenders
	row.Exempt = exempt
	for _, o := range offenders {
		row.ImpactBase += o.ImpactBase
	}
	switch {
	case len(offenders) > 0:
		row.Status = RuleStatusAct
		row.Evidence = fmt.Sprintf("%d oversized name(s) inside %d sessions of earnings — hold only what you'd buy fresh today.", len(offenders), freeze)
		row.Offenders = append(offenders, unknowns...)
	case len(unknowns) > 0:
		row.Status = RuleStatusUnknown
		row.Reason = "earnings_unknown"
		row.Offenders = unknowns
		// Each offender note names what failed — the earnings date, the delta,
		// or the exposure measurement — so the sentence stays neutral.
		row.Evidence = fmt.Sprintf("%d name(s) could not complete the pre-earnings size check.", len(unknowns))
	case assessed == 0 && len(exempt) > 0:
		row.Status = RuleStatusNotEvaluated
		row.Reason = earningsExemptionReason(terminalExempt, brokerExempt, secTypeExempt)
		row.Evidence = fmt.Sprintf("%d exact contract(s) are exempt from issuer pre-earnings freeze checks.", len(exempt))
	default:
		row.Status = RuleStatusPass
		row.Evidence = "No oversized name inside the pre-earnings freeze window."
	}
	return row
}

func (c *ruleContext) redOnGreen() RuleRow {
	row := RuleRow{ID: RuleRedOnGreen, Number: 9, Title: "Relative weakness on a green tape", Unit: "% day"}
	if !c.in.SessionOpen {
		row.Status = RuleStatusNotEvaluated
		row.Reason = RuleReasonOffSession
		row.Evidence = "Tape rules evaluate during the US regular session only."
		return row
	}
	if g := c.portfolioGate(row.ID, row.Number, row.Title); g != nil {
		return *g
	}
	if c.in.SPYDayChangePct == nil {
		row.Status = RuleStatusUnknown
		row.Reason = "no_spy_tape"
		row.Evidence = "SPY day change unavailable — market tape not assessable."
		return row
	}
	spy := *c.in.SPYDayChangePct
	row.Threshold = new(c.pol.RedOnGreenNameDropPct)
	if spy < c.pol.RedOnGreenSPYUpPct {
		row.Status = RuleStatusPass
		row.Evidence = fmt.Sprintf("Tape not green (SPY %+.1f%%) — relative-weakness screen idle.", spy)
		return row
	}
	var offenders []RuleOffender
	skipped := 0
	for _, n := range c.in.Names {
		if n.StockDayChangePct == nil {
			if !n.HasStockLeg {
				skipped++
			}
			continue
		}
		if *n.StockDayChangePct <= c.pol.RedOnGreenNameDropPct {
			offenders = append(offenders, RuleOffender{Symbol: n.Symbol, Observed: round1(*n.StockDayChangePct),
				ImpactBase: math.Abs(n.ExposureBase),
				Note:       fmt.Sprintf("%+.1f%% against SPY %+.1f%%", round1(*n.StockDayChangePct), round1(spy))})
		}
	}
	sortOffenders(offenders)
	row.Offenders = offenders
	if skipped > 0 {
		row.Notes = append(row.Notes, fmt.Sprintf("%d option-only name(s) have no stock-leg tape and were not screened.", skipped))
	}
	if len(offenders) > 0 {
		row.Status = RuleStatusWatch
		row.Evidence = fmt.Sprintf("%d name(s) red on a green tape — the market is naming your exits.", len(offenders))
		for _, o := range offenders {
			row.ImpactBase += o.ImpactBase
		}
	} else {
		row.Status = RuleStatusPass
		row.Evidence = fmt.Sprintf("No held name red beyond %.1f%% on a green tape.", c.pol.RedOnGreenNameDropPct)
	}
	return row
}

func (c *ruleContext) winnerTrim() RuleRow {
	row := RuleRow{ID: RuleWinnerTrim, Number: 10, Title: "Trim winners into strength", Unit: "% day"}
	if !c.in.SessionOpen {
		row.Status = RuleStatusNotEvaluated
		row.Reason = RuleReasonOffSession
		row.Evidence = "Tape rules evaluate during the US regular session only."
		return row
	}
	if g := c.portfolioGate(row.ID, row.Number, row.Title); g != nil {
		return *g
	}
	row.Threshold = new(c.pol.WinnerTrimDayUpPct)
	var offenders, unmeasured []RuleOffender
	for _, n := range c.in.Names {
		if n.StockDayChangePct == nil {
			continue
		}
		if *n.StockDayChangePct < c.pol.WinnerTrimDayUpPct {
			// Not up past the trigger — a true negative on the measured tape,
			// whatever the name's size.
			continue
		}
		// The name is up past the trigger, so the size floor decides. A name
		// whose exposure was never fully measured (missing FX or price, no
		// exist to trim. Disclosed unknown, never exposure 0.
		if !n.ExposureBaseComplete && !c.greeksGapMaterial(n) {
			unmeasured = append(unmeasured, RuleOffender{Symbol: n.Symbol, Observed: round1(*n.StockDayChangePct),
				Note: fmt.Sprintf("+%.1f%% today with exposure not fully measured — the %.0f%% NLV trim floor can't be ruled out", round1(*n.StockDayChangePct), c.pol.WinnerTrimMinExpoPct)})
			continue
		}
		expo := pct(math.Abs(n.ExposureBase), c.nlv)
		if expo >= c.pol.WinnerTrimMinExpoPct {
			offenders = append(offenders, RuleOffender{Symbol: n.Symbol, Observed: round1(*n.StockDayChangePct),
				ImpactBase: math.Abs(n.ExposureBase),
				Note:       fmt.Sprintf("+%.1f%% today at %.1f%% of NLV — someone is paying up", round1(*n.StockDayChangePct), round1(expo))})
		}
	}
	sortOffenders(offenders)
	switch {
	case len(offenders) > 0:
		row.Status = RuleStatusWatch
		// A measured breach stands; the unmeasured winners stay disclosed on
		// the same row with no ImpactBase — no weight they cannot prove.
		row.Offenders = append(offenders, unmeasured...)
		row.Evidence = fmt.Sprintf("%d oversized name(s) up hard today — sell strength while the bid is there.", len(offenders))
		for _, o := range offenders {
			row.ImpactBase += o.ImpactBase
		}
	case len(unmeasured) > 0:
		row.Status = RuleStatusUnknown
		row.Reason = "exposure_incomplete"
		row.Offenders = unmeasured
		row.Evidence = fmt.Sprintf("%d name(s) up past the trim trigger with unmeasured exposure — the %.0f%% NLV floor can't be ruled out.", len(unmeasured), c.pol.WinnerTrimMinExpoPct)
	default:
		row.Status = RuleStatusPass
		row.Offenders = offenders
		row.Evidence = "No oversized name up past the trim trigger today."
	}
	return row
}

func (c *ruleContext) greenDayAction(rows []RuleRow) RuleRow {
	row := RuleRow{ID: RuleGreenDayAction, Number: 11, Title: "Green day is an execution day"}
	if !c.in.Account.Healthy || c.in.DailyPnLBase == nil ||
		math.IsNaN(*c.in.DailyPnLBase) || math.IsInf(*c.in.DailyPnLBase, 0) {
		row.Status = RuleStatusNotEvaluated
		row.Reason = RuleReasonPnLUnavailable
		row.Evidence = "Daily P&L unavailable — nudge idle."
		return row
	}
	actOpen := 0
	for _, r := range rows {
		if r.Status == RuleStatusAct {
			actOpen++
		}
	}
	if *c.in.DailyPnLBase > 0 && actOpen > 0 {
		row.Status = RuleStatusInfo
		row.Evidence = fmt.Sprintf("Portfolio green today with %d act-severity rule(s) open — run the de-risk list now, not on a hypothetical better day.", actOpen)
	} else {
		row.Status = RuleStatusPass
		row.Evidence = "No green-day nudge: either the tape is red or nothing is at act severity."
	}
	return row
}

// rule12HedgeLeg reports whether rule 12 sizes this leg as a hedge: a long
func rule12HedgeLeg(l LegInput) bool {
	return RulebookHedgeLeg(l)
}

// RulebookHedgeLeg exposes the policy-owned hedge classification to daemon
// typed position row into LegInput and receive the exact classification used
func RulebookHedgeLeg(l LegInput) bool {
	return l.HedgeListed && isPut(l.Right) && l.Quantity > 0 && l.Delta != nil && l.Underlying != nil &&
		l.UnderlyingSource != UnderlyingSourceStockLegMark
}

func (c *ruleContext) hedgeIntegrity() RuleRow {
	row := RuleRow{ID: RuleHedgeIntegrity, Number: 12, Title: "Hedge sized to the book", Unit: "% gross long"}
	if g := c.portfolioGate(row.ID, row.Number, row.Title); g != nil {
		return *g
	}
	grossLong, hedgeShort := 0.0, 0.0
	var gaps, unmeasured []RuleOffender
	var hedgeLegs []RuleOffender
	for _, n := range c.in.Names {
		if c.greeksGapMaterial(n) {
			gaps = append(gaps, RuleOffender{Symbol: n.Symbol, Note: "delta unavailable on material legs"})
			continue
		}
		// A name whose base exposure was never fully measured (missing FX or
		// price, no greeks gap to catch it) contributes a degraded or zero
		// sum. Dropping it from grossLong shrinks the ratio's denominator, so
		// an under-hedged book reads adequately hedged — the quiet direction.
		// Like a delta gap, it poisons the ratio itself: whole row unknown.
		if !n.ExposureBaseComplete {
			unmeasured = append(unmeasured, RuleOffender{Symbol: n.Symbol,
				Note: "exposure not fully measured (FX or price missing) — gross long exposure cannot be trusted"})
			continue
		}
		if n.ExposureBase > 0 {
			grossLong += n.ExposureBase
		}
		for _, l := range n.Legs {
			if rule12HedgeLeg(l) {
				short := math.Abs(*l.Delta * l.Quantity * l.Multiplier * *l.Underlying)
				hedgeShort += short
				hedgeLegs = append(hedgeLegs, RuleOffender{Symbol: n.Symbol, Leg: l.Desc, Observed: round1(short), Note: "classified hedge"})
			}
		}
	}
	if len(gaps) > 0 || len(unmeasured) > 0 {
		row.Status = RuleStatusUnknown
		row.Offenders = append(gaps, unmeasured...)
		switch {
		case len(gaps) == 0:
			row.Reason = "exposure_incomplete"
			row.Evidence = "Unmeasured exposure makes the hedge ratio untrustworthy."
		case len(unmeasured) == 0:
			row.Reason = "greeks_gap"
			row.Evidence = "Delta gaps make the hedge ratio untrustworthy."
		default:
			row.Reason = "greeks_gap"
			row.Evidence = "Delta gaps and unmeasured exposure make the hedge ratio untrustworthy."
		}
		return row
	}
	if grossLong <= 0 {
		row.Status = RuleStatusNotEvaluated
		row.Reason = RuleReasonNoLongBook
		row.Evidence = "No net-long exposure to hedge."
		return row
	}
	ratio := pct(hedgeShort, grossLong)
	row.Observed = new(round1(ratio))
	row.Exempt = hedgeLegs
	return c.regimeEval(func(rt RegimeThresholds) RuleRow {
		r := row
		minB, maxB := rt.HedgeBandMinPct, rt.HedgeBandMaxPct
		r.Threshold = new(minB)
		if ratio > maxB {
			// Set for every applicable threshold set: rule 5's hedge
			c.overHedged = true
		}
		switch {
		case ratio > 2*maxB:
			r.Status = RuleStatusAct
			r.Evidence = fmt.Sprintf("Hedge short-delta at %.1f%% of gross long exposure — more than twice the %.0f–%.0f%% band top. This is a directional short wearing a hedge's clothing; the flag is sizing honesty, not a directive to get long.", round1(ratio), minB, maxB)
		case ratio > maxB:
			r.Status = RuleStatusWatch
			r.Evidence = fmt.Sprintf("Hedge short-delta at %.1f%% of gross long exposure — over the %.0f–%.0f%% band; oversized hedges are directional bets in disguise.", round1(ratio), minB, maxB)
		case ratio < minB:
			r.Status = RuleStatusWatch
			r.Evidence = fmt.Sprintf("Hedge short-delta covers %.1f%% of gross long exposure — under the %.0f–%.0f%% band; the book is barer than it feels (a decayed hedge shrinks here first).", round1(ratio), minB, maxB)
		default:
			r.Status = RuleStatusPass
			r.Evidence = fmt.Sprintf("Hedge at %.1f%% of gross long exposure, inside the %.0f–%.0f%% band.", round1(ratio), minB, maxB)
		}
		return r
	})
}

func (c *ruleContext) exitDiscipline() RuleRow {
	row := RuleRow{ID: RuleExitDiscipline, Number: 13, Title: "Exit the dead thesis", Unit: "% premium lost"}
	if g := c.portfolioGate(row.ID, row.Number, row.Title); g != nil {
		return *g
	}
	watch, act := c.pol.ExitWatchLossPct, c.pol.ExitActLossPct
	row.Threshold = new(watch)
	var actOff, watchOff, unknowns, exempt []RuleOffender
	worst := 0.0
	for _, n := range c.in.Names {
		for _, l := range n.Legs {
			if l.Quantity <= 0 {
				continue
			}
			if rule12HedgeLeg(l) {
				exempt = append(exempt, RuleOffender{Symbol: n.Symbol, Leg: l.Desc,
					Note: "hedge leg — decay is the cost of protection; rule 12 sizes it"})
				continue
			}
			if l.CostBasisBase == nil || *l.CostBasisBase <= 0 {
				// Same materiality floor, same reason it cannot be trusted on a
				// substituted base value: an understating pair would drop the leg
				// below the floor and hide an unassessable position.
				if l.MarketValueBaseSource == MarketValueBaseSourceSubstituted ||
					pct(math.Abs(l.MarketValueBase), c.nlv) >= c.pol.GreeksGapFloorPctNLV {
					unknowns = append(unknowns, RuleOffender{Symbol: n.Symbol, Leg: l.Desc,
						Note: "cost basis unavailable — loss not assessable"})
				}
				continue
			}
			loss := pct(*l.CostBasisBase-l.MarketValueBase, *l.CostBasisBase)
			if loss < watch {
				continue
			}
			worst = math.Max(worst, loss)
			o := RuleOffender{Symbol: n.Symbol, Leg: l.Desc, Observed: round1(loss),
				ImpactBase: math.Abs(l.MarketValueBase),
				Note:       fmt.Sprintf("-%.0f%% of premium paid; %.1f%% of NLV still salvageable", round1(loss), round1(pct(math.Abs(l.MarketValueBase), c.nlv)))}
			if loss >= act {
				actOff = append(actOff, o)
			} else {
				watchOff = append(watchOff, o)
			}
		}
	}
	sortOffenders(actOff)
	sortOffenders(watchOff)
	offenders := append(actOff, watchOff...)
	row.Offenders = offenders
	row.Exempt = exempt
	for _, o := range offenders {
		row.ImpactBase += o.ImpactBase
	}
	switch {
	case len(actOff) > 0:
		row.Status = RuleStatusAct
		row.Observed = new(round1(worst))
		row.Evidence = fmt.Sprintf("%d long line(s) past the -%.0f%% loss fence — decide the exit; theta is deciding it for you.", len(actOff), act)
		row.Offenders = append(offenders, unknowns...)
	case len(watchOff) > 0:
		row.Status = RuleStatusWatch
		row.Observed = new(round1(worst))
		row.Evidence = fmt.Sprintf("%d long line(s) past the -%.0f%% loss fence — restate the thesis or exit while premium remains.", len(watchOff), watch)
		row.Offenders = append(offenders, unknowns...)
	case len(unknowns) > 0:
		row.Status = RuleStatusUnknown
		row.Reason = "cost_basis_unavailable"
		row.Offenders = unknowns
		row.Evidence = fmt.Sprintf("%d material long line(s) missing cost basis — losses not assessable.", len(unknowns))
	default:
		row.Status = RuleStatusPass
		row.Evidence = fmt.Sprintf("No long option line past the -%.0f%% loss fence (note: averaging down resets the basis; the fence does not follow it down).", watch)
	}
	return row
}

func (c *ruleContext) fxExposure() RuleRow {
	row := RuleRow{ID: RuleFXExposure, Number: 14, Title: "Non-base currency exposure", Unit: "% NLV"}
	if !c.in.Account.Healthy || !c.hasNLV {
		row.Status = RuleStatusUnknown
		row.Reason = nonEmpty(c.in.Account.Reason, "account_unavailable")
		row.Evidence = "Account NLV not available — not asserting a pass."
		return row
	}
	if c.in.NonBaseNLVBase == nil {
		row.Status = RuleStatusUnknown
		row.Reason = "fx_unavailable"
		row.Evidence = "Currency exposure report unavailable — not asserting a pass (an empty report on a book with non-base legs is a data gap, not a base-only book)."
		return row
	}
	watch := c.pol.FXExposureWatchPct
	p := pct(math.Abs(*c.in.NonBaseNLVBase), c.nlv)
	row.Observed = new(round1(p))
	row.Threshold = new(watch)
	ccys := strings.Join(c.in.NonBaseCurrencies, ", ")
	if ccys == "" {
		ccys = "non-base currencies"
	}
	if p >= watch {
		// Watch-only by design: at structurally high non-base exposure a
		// permanent act would be pure alarm fatigue. The rule exists to make
		// the exposure explicit — hedge it or accept it, on purpose.
		row.Status = RuleStatusWatch
		row.ImpactBase = math.Abs(*c.in.NonBaseNLVBase)
		row.Evidence = fmt.Sprintf("%.1f%% of NLV is held in %s (threshold %.0f%%) — hedge or accept this FX exposure explicitly; a 1%% move is ~%.1f%% of NLV.", round1(p), ccys, watch, round1(p/100))
	} else {
		row.Status = RuleStatusPass
		row.Evidence = fmt.Sprintf("%.1f%% of NLV in non-base currencies, under the %.0f%% threshold.", round1(p), watch)
	}
	return row
}

func (c *ruleContext) greeksGapMaterial(n NameInput) bool {
	return c.hasNLV && pct(n.GreeksGapNotionalBase, c.nlv) >= c.pol.GreeksGapFloorPctNLV
}

// expiresBeforeCatalyst: rule 6 — an OTM long that dies before its name's
func expiresBeforeCatalyst(expiry time.Time, e EarningsInput) bool {
	ed, xd := dateOnly(e.Date), dateOnly(expiry)
	if xd.Before(ed) {
		return true
	}
	if xd.Equal(ed) {
		switch e.TimeOfDay {
		case "bmo":
			return false
		default: // amc or unknown → gap is after the close
			return true
		}
	}
	return false
}

// spansEarningsGap: rule 7 — a short call alive through the earnings gap.
func spansEarningsGap(now, expiry time.Time, e EarningsInput) (spans, ambiguous bool) {
	ed, xd, today := dateOnly(e.Date), dateOnly(expiry), dateOnly(now)
	if ed.Before(today) || xd.Before(ed) {
		return false, false
	}
	if xd.Equal(ed) {
		switch e.TimeOfDay {
		case "amc":
			return false, false // dies at the close, gap is after
		case "bmo":
			return true, false
		default:
			return true, true // conservative
		}
	}
	return true, false
}

// statusWeight orders rule statuses by severity for ranking and worse-of
func statusWeight(s string) int {
	switch s {
	case RuleStatusAct:
		return 5
	case RuleStatusWatch:
		return 4
	case RuleStatusUnknown:
		return 3
	case RuleStatusInfo:
		return 2
	case RuleStatusNotEvaluated:
		return 1
	default:
		return 0
	}
}

func rankRows(rows []RuleRow) []int {
	idx := make([]int, 0, len(rows))
	for i := range rows {
		idx = append(idx, i)
	}
	sort.SliceStable(idx, func(a, b int) bool {
		ra, rb := rows[idx[a]], rows[idx[b]]
		if statusWeight(ra.Status) != statusWeight(rb.Status) {
			return statusWeight(ra.Status) > statusWeight(rb.Status)
		}
		if ra.ImpactBase != rb.ImpactBase {
			return ra.ImpactBase > rb.ImpactBase
		}
		return ra.Number < rb.Number
	})
	return idx
}

func worseRunway(current string, dte, actDTE int) string {
	if dte < actDTE {
		return RuleStatusAct
	}
	if current != RuleStatusAct {
		return RuleStatusWatch
	}
	return current
}

func sortOffenders(o []RuleOffender) {
	sort.SliceStable(o, func(i, j int) bool {
		if o[i].ImpactBase != o[j].ImpactBase {
			return o[i].ImpactBase > o[j].ImpactBase
		}
		return math.Abs(o[i].Observed) > math.Abs(o[j].Observed)
	})
}

func earningsGapWord(e EarningsInput) string {
	if e.Known && e.Stale {
		return "stale (" + earningsReasonWords(e.Reason) + ")"
	}
	if e.Reason != "" {
		return earningsReasonWords(e.Reason)
	}
	return "unknown"
}

func earningsReasonWords(reason string) string {
	switch reason {
	case "no_date_published":
		return "not published by the provider"
	case "unsupported_security":
		return "unsupported by the provider"
	case "format_change":
		return "unreadable after a provider format change"
	case "transport_failure":
		return "unavailable after a provider transport failure"
	case "conflicting_sources":
		return "conflicting across providers"
	case "not_observed":
		return "not checked yet"
	case "retained_last_good":
		return "retained from the last good provider result"
	case "single_source":
		return "confirmed by only one provider"
	case "date_elapsed":
		return "elapsed without a newly published date"
	default:
		return "unknown"
	}
}

func estNote(e EarningsInput) string {
	if e.Estimated {
		return " (estimated)"
	}
	return ""
}

func isPut(right string) bool  { return right == "P" || right == "PUT" || right == "p" }
func isCall(right string) bool { return right == "C" || right == "CALL" || right == "c" }

func dateOnly(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

func pct(v, base float64) float64 {
	if base == 0 {
		return 0
	}
	return v / base * 100
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

func nonEmpty(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}
