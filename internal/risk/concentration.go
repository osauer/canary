package risk

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
)

// Issuer concentration (amendment 15, owner decisions 2026-09-26).
//
// Rule 1 measures the worst-case loss on one issuer across all prices, as a
// share of NLV, netting every leg on the issuer from current marks. An issuer
// is an underlying joined with the owner's issuer groups (share classes,
// ADR/ordinary lines); an ungrouped symbol is its own issuer, and index or
// other-underlying options sit on their own issuer, so they give no credit.
//
// Legs are valued on intrinsic payoffs at a price grid: zero, every strike,
// spot, and spot × (1 + takeover gap). An early assignment realizes exactly a
// short leg's intrinsic value, so assuming any short leg can be assigned at
// any price never breaks the netting. Between grid points every payoff is
// linear, so the grid finds the true worst price. A long option pays off in
// the grid only when it counts as protection: it expires after the issuer's
// next earnings and at least hedge_min_days out. Otherwise it contributes its
// full premium as loss and no protective payoff. A book that keeps losing as
// the price rises (short stock, uncovered short calls) is unbounded; the
// takeover gap sizes it and the legs responsible say so.

// Issuer leg kinds.
const (
	IssuerLegStock = "stock"
	IssuerLegCall  = "call"
	IssuerLegPut   = "put"
)

// Hedge credit states of a long option that protects another leg of its
// issuer.
const (
	IssuerHedgeCredited   = "credited"
	IssuerHedgeUncredited = "uncredited"
)

// IssuerLeg is one position on an issuer as rule 1 netted it.
type IssuerLeg struct {
	Symbol string `json:"symbol"`
	Leg    string `json:"leg"`
	Kind   string `json:"kind"`
	// Quantity is signed shares for stock and signed contracts for options.
	Quantity float64 `json:"quantity"`
	// LossBase is this leg's loss at the issuer's worst price, measured from
	// current marks in base currency; a gain is negative.
	LossBase float64 `json:"loss_base"`
	// Hedge is set on a long option that would protect another leg of the
	// issuer: credited when it counts as protection, uncredited when it does
	// not (its premium is then all it contributes).
	Hedge string `json:"hedge,omitempty"`
	// Unbounded marks a leg that keeps losing as the price rises; the
	// takeover gap sizes it.
	Unbounded bool   `json:"unbounded,omitempty"`
	Note      string `json:"note,omitempty"`
}

// IssuerExposure is the worst-case loss on one issuer, every leg netted.
type IssuerExposure struct {
	Issuer string `json:"issuer"`
	// Lines are the held symbols that make up the issuer.
	Lines             []string `json:"lines"`
	WorstCaseLossBase float64  `json:"worst_case_loss_base"`
	WorstCaseLossPct  float64  `json:"worst_case_loss_pct_nlv"`
	// WorstMovePct is the issuer's price move at the worst price, in percent:
	// −100 is a fall to zero and the takeover gap is the largest rise. It is
	// absent when the price of an option-only line is unknown.
	WorstMovePct *float64 `json:"worst_move_pct,omitempty"`
	// LowerBound marks a loss proven from partial inputs: the true worst case
	// is at least this. It may indict, never acquit.
	LowerBound bool `json:"lower_bound,omitempty"`
	// Unbounded says the book keeps losing as the price rises; the loss above
	// is sized at the takeover gap.
	Unbounded bool `json:"unbounded,omitempty"`
	// WatchPct and ActPct are the bands this issuer is measured against: the
	// normal bands, or the illiquid ones when it takes too long to exit.
	WatchPct float64 `json:"watch_pct"`
	ActPct   float64 `json:"act_pct"`
	Illiquid bool    `json:"illiquid,omitempty"`
	// DaysToExit is the longest line's shares (share-equivalents for an
	// option-only line) over the allowed share of its 20-day average volume.
	DaysToExit *float64    `json:"days_to_exit,omitempty"`
	Legs       []IssuerLeg `json:"legs"`
	Notes      []string    `json:"notes,omitempty"`
}

// issuerLine is one held symbol of an issuer and the price its scenarios
// move from.
type issuerLine struct {
	name NameInput
	// spot is the line's price in its quote currency; for an option-only
	// line with no price (strike space) it is 1 and the grid is in prices.
	spot float64
}

// issuerLegModel is one leg in the scenario arithmetic.
type issuerLegModel struct {
	out    IssuerLeg
	line   int
	stock  bool
	right  string
	strike float64
	// units is signed shares: the stock quantity, or contracts × multiplier.
	units float64
	fx    float64
	// nowBase is the leg's signed value at current marks in base currency.
	nowBase float64
	// counted says the leg's payoff enters the scenarios; an uncredited long
	// option keeps only its premium at risk.
	counted bool
	delta   *float64
}

// issuerEval is one issuer's netting shared by rules 1, 8, 16, 17 and 18.
type issuerEval struct {
	exposure IssuerExposure
	lines    []issuerLine
	legs     []issuerLegModel
	// measured says every leg was valued and every line priced; strike space
	// marks one option-only line valued at prices because its spot is unknown.
	measured    bool
	strikeSpace bool
	gaps        []string
	status      string // rule 1 verdict for this issuer: pass | watch | act | unknown
}

// scenarioReady reports whether the issuer can be moved by a percentage, as
// the cluster fall does: every leg valued and a known price on every line.
func (e issuerEval) scenarioReady() bool { return e.measured && !e.strikeSpace }

// pnlAt is the issuer's profit at price factor m (every line's price times m)
// against current marks, in base currency.
func (e issuerEval) pnlAt(m float64) float64 {
	total := 0.0
	for _, l := range e.legs {
		total += e.legPnL(l, m)
	}
	return total
}

func (e issuerEval) legPnL(l issuerLegModel, m float64) float64 {
	price := e.lines[l.line].spot * m
	value := 0.0
	switch {
	case l.stock:
		value = l.units * price * l.fx
	case l.counted:
		value = l.units * OptionIntrinsicPerShare(l.right, price, l.strike) * l.fx
	}
	return value - l.nowBase
}

// upwardSlope is the rate the issuer's value changes with m beyond every
// strike: negative means it keeps losing as the price rises.
func (e issuerEval) upwardSlope() float64 {
	slope := 0.0
	for _, l := range e.legs {
		slope += e.legUpwardSlope(l)
	}
	return slope
}

func (e issuerEval) legUpwardSlope(l issuerLegModel) float64 {
	spot := e.lines[l.line].spot
	switch {
	case l.stock:
		return l.units * spot * l.fx
	case l.counted && isCall(l.right):
		return l.units * spot * l.fx
	default:
		return 0
	}
}

// grid is the price-factor grid: zero, every strike, spot and the takeover
// gap. In strike space only zero and the strikes are known prices.
func (e issuerEval) grid(takeoverGapPct float64) []float64 {
	points := []float64{0}
	if !e.strikeSpace {
		points = append(points, 1, 1+takeoverGapPct/100)
	}
	for _, l := range e.legs {
		if !l.stock && l.strike > 0 {
			points = append(points, l.strike/e.lines[l.line].spot)
		}
	}
	sort.Float64s(points)
	return slices.Compact(points)
}

// worstCase finds the grid point with the largest loss. The loss is never
// below zero: a book that gains at every price has nothing at risk.
func (e issuerEval) worstCase(takeoverGapPct float64) (loss, at float64) {
	loss, at = math.Inf(-1), 0
	for _, m := range e.grid(takeoverGapPct) {
		if l := -e.pnlAt(m); l > loss {
			loss, at = l, m
		}
	}
	return math.Max(loss, 0), at
}

// issuers returns the memoized issuer netting for this evaluation.
func (c *ruleContext) issuers() []issuerEval {
	if c.issuersDone {
		return c.issuerBooks
	}
	c.issuersDone = true
	byIssuer := map[string][]NameInput{}
	var order []string
	for _, n := range c.in.Names {
		if !n.HasStockLeg && len(n.Legs) == 0 {
			continue
		}
		issuer := c.pol.IssuerOf(n.Symbol)
		if _, seen := byIssuer[issuer]; !seen {
			order = append(order, issuer)
		}
		byIssuer[issuer] = append(byIssuer[issuer], n)
	}
	sort.Strings(order)
	for _, issuer := range order {
		c.issuerBooks = append(c.issuerBooks, c.evaluateIssuer(issuer, byIssuer[issuer]))
	}
	return c.issuerBooks
}

// issuerFor returns the evaluation of the issuer a held symbol belongs to.
func (c *ruleContext) issuerFor(symbol string) (issuerEval, bool) {
	issuer := c.pol.IssuerOf(symbol)
	for _, e := range c.issuers() {
		if e.exposure.Issuer == issuer {
			return e, true
		}
	}
	return issuerEval{}, false
}

func (c *ruleContext) evaluateIssuer(issuer string, names []NameInput) issuerEval {
	slices.SortFunc(names, func(a, b NameInput) int { return strings.Compare(a.Symbol, b.Symbol) })
	e := issuerEval{measured: true, exposure: IssuerExposure{Issuer: issuer}}
	earn := c.issuerEarnings(names)
	downside, upside := false, false
	for _, n := range names {
		if n.StockQuantity > 0 {
			downside = true
		}
		if n.StockQuantity < 0 {
			upside = true
		}
		for _, l := range n.Legs {
			switch {
			case l.Quantity < 0 && isPut(l.Right):
				downside = true
			case l.Quantity < 0 && isCall(l.Right):
				upside = true
			}
		}
	}
	for _, n := range names {
		e.exposure.Lines = append(e.exposure.Lines, n.Symbol)
		line := issuerLine{name: n, spot: n.StockMark}
		if line.spot <= 0 {
			line.spot = 0
			for _, l := range n.Legs {
				if l.Underlying != nil && *l.Underlying > 0 {
					line.spot = *l.Underlying
					break
				}
			}
		}
		li := len(e.lines)
		e.lines = append(e.lines, line)
		if n.StockQuantity != 0 {
			leg := IssuerLeg{Symbol: n.Symbol, Leg: n.Symbol + " stock", Kind: IssuerLegStock, Quantity: n.StockQuantity}
			switch {
			case n.StockMark <= 0:
				e.measured = false
				e.gaps = append(e.gaps, n.Symbol+" stock price unavailable")
				leg.Note = "price unavailable — not measured"
				e.exposure.Legs = append(e.exposure.Legs, leg)
			case n.StockFXToBase == nil:
				e.measured = false
				e.gaps = append(e.gaps, n.Symbol+" stock has no FX rate to base")
				leg.Note = "no FX rate to base — not measured"
				e.exposure.Legs = append(e.exposure.Legs, leg)
			default:
				fx := *n.StockFXToBase
				e.legs = append(e.legs, issuerLegModel{out: leg, line: li, stock: true, units: n.StockQuantity,
					fx: fx, nowBase: n.StockQuantity * n.StockMark * fx, counted: true})
			}
		} else if n.HasStockLeg {
			// The group reports a stock leg without a share count: it cannot be
			// valued, and valuing it at nothing would acquit it.
			e.measured = false
			e.gaps = append(e.gaps, n.Symbol+" stock quantity unavailable")
		}
		for _, l := range n.Legs {
			if l.Quantity == 0 {
				continue
			}
			kind := IssuerLegCall
			if isPut(l.Right) {
				kind = IssuerLegPut
			}
			leg := IssuerLeg{Symbol: n.Symbol, Leg: l.Desc, Kind: kind, Quantity: l.Quantity}
			if !isPut(l.Right) && !isCall(l.Right) {
				e.measured = false
				e.gaps = append(e.gaps, l.Desc+" has an unrecognized right")
				leg.Note = "unrecognized right — not measured"
				e.exposure.Legs = append(e.exposure.Legs, leg)
				continue
			}
			if l.FXToBase == nil || l.MarketValueBaseSource == MarketValueBaseSourceSubstituted {
				e.measured = false
				e.gaps = append(e.gaps, l.Desc+" has no FX rate to base")
				leg.Note = "no FX rate to base — not measured"
				e.exposure.Legs = append(e.exposure.Legs, leg)
				continue
			}
			m := issuerLegModel{out: leg, line: li, right: strings.ToUpper(l.Right[:1]), strike: l.Strike,
				units: l.Quantity * l.Multiplier, fx: *l.FXToBase, nowBase: l.MarketValueBase, counted: true, delta: l.Delta}
			if l.Quantity > 0 {
				credited, note := c.hedgeCredit(l, earn)
				protects := (isPut(l.Right) && downside) || (isCall(l.Right) && upside)
				switch {
				case protects && credited:
					m.out.Hedge = IssuerHedgeCredited
					m.out.Note = "protection counted: " + note
				case protects:
					m.out.Hedge = IssuerHedgeUncredited
					m.out.Note = "no protection credit: " + note + "; counts its premium only"
				case !credited:
					m.out.Note = "loses at most its premium"
				}
				m.counted = credited
			}
			e.legs = append(e.legs, m)
		}
	}
	// An option-only line with no price can still be valued at absolute
	// prices when it is the issuer's only line: zero and its strikes are the
	// kinks of its payoff. A grouped line without a price cannot be moved with
	// the others, so it stays unmeasured.
	for i := range e.lines {
		if e.lines[i].spot > 0 {
			continue
		}
		if e.lines[i].name.StockQuantity == 0 && len(e.lines) == 1 {
			e.lines[i].spot = 1
			e.strikeSpace = true
			continue
		}
		e.measured = false
		e.gaps = append(e.gaps, e.lines[i].name.Symbol+" price unavailable")
	}
	if earn.note != "" {
		e.exposure.Notes = append(e.exposure.Notes, earn.note)
	}
	c.measureIssuer(&e)
	return e
}

// measureIssuer fills the worst case, bands and rule 1 status of a built book.
func (c *ruleContext) measureIssuer(e *issuerEval) {
	watch, act := c.issuerBands(e)
	e.exposure.WatchPct, e.exposure.ActPct = watch, act
	if !e.measured || !c.hasNLV {
		e.status = RuleStatusUnknown
		for _, l := range e.legs {
			e.exposure.Legs = append(e.exposure.Legs, l.out)
		}
		return
	}
	gap := c.pol.TakeoverGapPct
	loss, at := e.worstCase(gap)
	slope := e.upwardSlope()
	unbounded := slope < -1e-9
	e.exposure.Unbounded = unbounded
	negSlope := 0.0
	for _, l := range e.legs {
		if s := e.legUpwardSlope(l); s < 0 {
			negSlope += s
		}
	}
	for _, l := range e.legs {
		out := l.out
		out.LossBase = -e.legPnL(l, at)
		if unbounded && e.legUpwardSlope(l) < 0 {
			out.Unbounded = true
			share := slope / negSlope
			label := "unbounded as the price rises"
			if share < 0.995 {
				label = fmt.Sprintf("about %.0f%% uncovered, unbounded as the price rises", share*100)
			}
			if e.strikeSpace {
				out.Note = joinNote(out.Note, label+" — the price is unknown, so the takeover gap cannot be sized")
			} else {
				out.Note = joinNote(out.Note, fmt.Sprintf("%s — sized at the %s%% takeover gap", label, limitText(gap)))
			}
		}
		e.exposure.Legs = append(e.exposure.Legs, out)
	}
	e.exposure.WorstCaseLossBase = loss
	e.exposure.WorstCaseLossPct = round1(pct(loss, c.nlv))
	if !e.strikeSpace {
		e.exposure.WorstMovePct = new(round1((at - 1) * 100))
	}
	status := bandStatus(pct(loss, c.nlv), watch, act)
	if e.strikeSpace && unbounded {
		// The takeover gap needs the price. The loss at the highest strike is
		// proven, so it may indict; it can never acquit.
		e.exposure.LowerBound = true
		if status == RuleStatusPass {
			status = RuleStatusUnknown
			e.gaps = append(e.gaps, e.exposure.Lines[0]+" price unavailable to size an unbounded leg")
		}
	}
	e.status = status
}

// issuerBands returns the bands an issuer is measured against. Days to exit
// uses each line's shares, or for an option-only line its share-equivalents
// (|delta| × contracts × multiplier, the full contract when delta is
// missing), over the allowed share of its 20-day average volume. A line
// without volume keeps the normal bands and says so; partial data may indict
// but never acquit, so any measured line past the limit makes the issuer
// illiquid.
func (c *ruleContext) issuerBands(e *issuerEval) (watch, act float64) {
	worst := -1.0
	var noVolume []string
	for _, line := range e.lines {
		n := line.name
		shares := math.Abs(n.StockQuantity)
		if shares == 0 {
			for _, l := range n.Legs {
				per := math.Abs(l.Quantity * l.Multiplier)
				if l.Delta != nil {
					per *= math.Min(math.Abs(*l.Delta), 1)
				}
				shares += per
			}
		}
		if shares == 0 {
			continue
		}
		if n.AvgDailyVolume == nil || *n.AvgDailyVolume <= 0 || math.IsNaN(*n.AvgDailyVolume) || math.IsInf(*n.AvgDailyVolume, 0) {
			noVolume = append(noVolume, n.Symbol)
			continue
		}
		days := shares / (c.pol.ExitParticipationPct / 100 * *n.AvgDailyVolume)
		worst = math.Max(worst, days)
	}
	if worst >= 0 {
		e.exposure.DaysToExit = new(round1(worst))
	}
	if worst > c.pol.IlliquidDaysToExit {
		e.exposure.Illiquid = true
		e.exposure.Notes = append(e.exposure.Notes, fmt.Sprintf("illiquid: %.1f days to exit at %s%% of 20-day average volume, over %s days — bands %s/%s%%",
			round1(worst), limitText(c.pol.ExitParticipationPct), limitText(c.pol.IlliquidDaysToExit), limitText(c.pol.IlliquidWatchPct), limitText(c.pol.IlliquidActPct)))
		return c.pol.IlliquidWatchPct, c.pol.IlliquidActPct
	}
	if len(noVolume) > 0 {
		e.exposure.Notes = append(e.exposure.Notes, fmt.Sprintf("20-day average volume unavailable for %s — days to exit not assessed, normal bands kept", strings.Join(noVolume, ", ")))
	}
	return c.pol.SingleNameWatchPct, c.pol.SingleNameActPct
}

// issuerEarningsContext is the next earnings event an issuer's hedges must
// outlive, or why there is none.
type issuerEarningsContext struct {
	known bool
	e     EarningsInput
	note  string
}

func (c *ruleContext) issuerEarnings(names []NameInput) issuerEarningsContext {
	var out issuerEarningsContext
	unknown := false
	for _, n := range names {
		if _, ok := c.terminalEarningsFor(n.Symbol); ok {
			continue
		}
		if _, ok := c.brokerNonIssuerEarningsFor(n); ok {
			continue
		}
		if _, ok := c.nonIssuerSecurityEarningsFor(n.Symbol); ok {
			continue
		}
		e, ok := c.earningsFor(n.Symbol)
		if !ok {
			unknown = true
			continue
		}
		if !out.known || dateOnly(e.Date).Before(dateOnly(out.e.Date)) {
			out.known, out.e = true, e
		}
	}
	if !out.known && unknown {
		out.note = fmt.Sprintf("next earnings date unknown — hedge credit uses the %d-day minimum only", c.pol.HedgeMinDays)
	}
	return out
}

// hedgeCredit decides whether a long option counts as protection: it must
// expire at least hedge_min_days out and after the issuer's next earnings.
func (c *ruleContext) hedgeCredit(l LegInput, earn issuerEarningsContext) (bool, string) {
	if l.DTE < c.pol.HedgeMinDays {
		return false, fmt.Sprintf("expires in %d days, inside the %d-day minimum", l.DTE, c.pol.HedgeMinDays)
	}
	if earn.known && expiresBeforeCatalyst(l.Expiry, earn.e) {
		return false, fmt.Sprintf("expires %s, before earnings %s%s", l.Expiry.Format("Jan 2"), earn.e.Date.Format("Jan 2"), estNote(earn.e))
	}
	if earn.known {
		return true, fmt.Sprintf("expires %s, after earnings %s, %d days out", l.Expiry.Format("Jan 2"), earn.e.Date.Format("Jan 2"), l.DTE)
	}
	return true, fmt.Sprintf("expires %s, %d days out", l.Expiry.Format("Jan 2"), l.DTE)
}

func joinNote(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + "; " + b
	}
}

// issuerOffender renders one issuer as a rule offender with its full detail.
func issuerOffender(e issuerEval, observed float64, note string) RuleOffender {
	x := e.exposure
	x.Lines = slices.Clone(x.Lines)
	x.Legs = slices.Clone(x.Legs)
	x.Notes = slices.Clone(x.Notes)
	return RuleOffender{Symbol: x.Issuer, Observed: round1(observed), ImpactBase: x.WorstCaseLossBase, Note: note, Issuer: &x}
}

// worstMoveText describes where the loss is worst.
func (c *ruleContext) worstMoveText(x IssuerExposure) string {
	switch {
	case x.WorstMovePct == nil:
		return ""
	case *x.WorstMovePct <= -100:
		return "at a fall to zero"
	case x.Unbounded && *x.WorstMovePct >= c.pol.TakeoverGapPct-0.05:
		return fmt.Sprintf("at a %s%% rise, the takeover gap", limitText(c.pol.TakeoverGapPct))
	case *x.WorstMovePct < 0:
		return fmt.Sprintf("at a %.1f%% fall", -*x.WorstMovePct)
	case *x.WorstMovePct > 0:
		return fmt.Sprintf("at a %.1f%% rise", *x.WorstMovePct)
	default:
		return "at today's price"
	}
}

func (c *ruleContext) issuerSummary(x IssuerExposure) string {
	var parts []string
	if w := c.worstMoveText(x); w != "" {
		parts = append(parts, "worst "+w)
	}
	if x.Unbounded {
		parts = append(parts, "unbounded upside loss")
	}
	credited, uncredited := 0, 0
	for _, l := range x.Legs {
		switch l.Hedge {
		case IssuerHedgeCredited:
			credited++
		case IssuerHedgeUncredited:
			uncredited++
		}
	}
	if credited > 0 {
		parts = append(parts, fmt.Sprintf("%d hedge(s) credited", credited))
	}
	if uncredited > 0 {
		parts = append(parts, fmt.Sprintf("%d hedge(s) not credited", uncredited))
	}
	if x.Illiquid {
		parts = append(parts, fmt.Sprintf("illiquid bands %s/%s%%", limitText(x.WatchPct), limitText(x.ActPct)))
	}
	return strings.Join(parts, "; ")
}

func (c *ruleContext) singleNameExposure() RuleRow {
	row := RuleRow{ID: RuleSingleNameExposure, Number: 1, Title: "Worst-case loss on one issuer", Unit: "% NLV"}
	if g := c.portfolioGate(row.ID, row.Number, row.Title); g != nil {
		return *g
	}
	var offenders, unknowns []RuleOffender
	var driver *issuerEval
	driverRank := func(e issuerEval) (int, float64) {
		return statusWeight(e.status), e.exposure.WorstCaseLossPct / math.Max(e.exposure.WatchPct, 1e-9)
	}
	books := c.issuers()
	for i := range books {
		e := books[i]
		switch e.status {
		case RuleStatusAct, RuleStatusWatch:
			note := c.issuerSummary(e.exposure)
			if e.exposure.LowerBound {
				note = joinNote("lower bound — at least this", note)
			}
			o := issuerOffender(e, e.exposure.WorstCaseLossPct, note)
			o.Status = e.status
			offenders = append(offenders, o)
		case RuleStatusUnknown:
			o := issuerOffender(e, 0, "not measured: "+strings.Join(e.gaps, "; "))
			o.ImpactBase, o.Status = 0, RuleStatusUnknown
			unknowns = append(unknowns, o)
			continue
		}
		if driver == nil {
			driver = &books[i]
			continue
		}
		w, r := driverRank(e)
		dw, dr := driverRank(*driver)
		if w > dw || (w == dw && r > dr) {
			driver = &books[i]
		}
	}
	sort.SliceStable(offenders, func(i, j int) bool {
		wi, wj := statusWeight(offenders[i].Status), statusWeight(offenders[j].Status)
		if wi != wj {
			return wi > wj
		}
		return offenders[i].Observed > offenders[j].Observed
	})
	row.Offenders = offenders
	watch, act := c.pol.SingleNameWatchPct, c.pol.SingleNameActPct
	if driver != nil {
		watch, act = driver.exposure.WatchPct, driver.exposure.ActPct
	}
	row.setBands(watch, act)
	band := RuleStatusPass
	if driver != nil {
		band = driver.status
	}
	bandsText := func(x IssuerExposure) string {
		if x.Illiquid {
			return " for an illiquid issuer"
		}
		return ""
	}
	switch {
	case band == RuleStatusAct || band == RuleStatusWatch:
		x := driver.exposure
		row.Status = band
		row.Observed = new(x.WorstCaseLossPct)
		row.ObservedIsLowerBound = x.LowerBound
		bound, where := "", c.worstMoveText(x)
		if x.LowerBound {
			bound = " at least"
		}
		if where != "" {
			where = " " + where
		}
		if band == RuleStatusAct {
			row.Evidence = fmt.Sprintf("%s can lose%s %.1f%% of NLV%s, at or above the %s%% cap%s.", x.Issuer, bound, x.WorstCaseLossPct, where, limitText(act), bandsText(x))
		} else {
			row.Evidence = fmt.Sprintf("%s can lose%s %.1f%% of NLV%s, at or above the %s%% watch level%s; the cap is %s%%.", x.Issuer, bound, x.WorstCaseLossPct, where, limitText(watch), bandsText(x), limitText(act))
		}
		if x.Unbounded {
			row.Notes = append(row.Notes, fmt.Sprintf("%s keeps losing as the price rises; that leg is sized at the %s%% takeover gap.", x.Issuer, limitText(c.pol.TakeoverGapPct)))
		}
	case len(unknowns) > 0:
		row.Status = RuleStatusUnknown
		row.Reason = "exposure_incomplete"
		row.Evidence = fmt.Sprintf("Canary could not measure the worst-case loss on %d issuer(s) because a price, share count or FX rate is missing.", len(unknowns))
	default:
		row.Status = RuleStatusPass
		largest, name := 0.0, "no issuer"
		if driver != nil {
			largest, name = driver.exposure.WorstCaseLossPct, driver.exposure.Issuer
		}
		row.Observed = new(largest)
		row.Evidence = fmt.Sprintf("The largest worst-case loss on one issuer is %.1f%% of NLV (%s), under the %s%% watch level%s.", largest, name, limitText(watch), func() string {
			if driver != nil {
				return bandsText(driver.exposure)
			}
			return ""
		}())
	}
	// Disclosure is unconditional: a measured breach stands, and the issuers
	// that could not be measured are named beside it.
	row.Offenders = append(row.Offenders, unknowns...)
	if row.Status != RuleStatusUnknown && len(unknowns) > 0 {
		row.Notes = append(row.Notes, fmt.Sprintf("%d issuer(s) additionally not measured (price, share count or FX missing) — the verdict above stands regardless.", len(unknowns)))
	}
	for _, o := range row.Offenders {
		row.ImpactBase += o.ImpactBase
	}
	return row
}

// IssuerConcentration is the per-issuer netting behind rule 1, exposed for
// consumers that act on it (the risk-reduction bucket) so they read the same
// measurement as the Rulebook rather than a second one.
type IssuerConcentration struct {
	Exposure IssuerExposure
	// Status is rule 1's verdict for this issuer: pass, watch, act or unknown.
	Status string
	Gaps   []string
}

// EvaluateIssuerConcentration nets every issuer in the inputs exactly as rule
// 1 does and returns them in issuer order.
func EvaluateIssuerConcentration(in RuleInputs, pol RulebookPolicy) []IssuerConcentration {
	c := newRuleContext(in, pol)
	var out []IssuerConcentration
	for _, e := range c.issuers() {
		out = append(out, IssuerConcentration{Exposure: e.exposure, Status: e.status, Gaps: slices.Clone(e.gaps)})
	}
	return out
}

// newRuleContext prepares inputs and policy the way EvaluateRulebook does.
func newRuleContext(in RuleInputs, pol RulebookPolicy) *ruleContext {
	pol.Normalize()
	in = classifyIndexPutRoles(in, pol)
	c := &ruleContext{in: in, pol: pol}
	if in.NLVBase != nil && *in.NLVBase > 0 {
		c.nlv = *in.NLVBase
		c.hasNLV = true
	}
	return c
}

// IssuerTrimPlan is the reduction of one leg that brings an issuer's
// worst-case loss back to its watch band.
type IssuerTrimPlan struct {
	Issuer string
	Symbol string
	// Leg is the option description, or empty for the stock line.
	Leg   string
	Stock bool
	// Quantity is how many shares or contracts to sell (a long leg) or buy
	// back (a short leg); always positive.
	Quantity float64
	// Held is the leg's current signed quantity in the same unit.
	Held           float64
	LossBeforeBase float64
	LossAfterBase  float64
	TargetBase     float64
	WatchPct       float64
	ActPct         float64
	// Reachable is false when reducing this leg alone cannot bring the loss
	// to the target; Quantity is then the reduction that lowers it most.
	Reachable bool

	// per and others are the leg's and the rest of the issuer's P&L at each
	// price of the grid the plan was solved on, and unit the leg's size, so
	// LossAfter can price any other reduction of the same leg.
	per, others []float64
	unit        float64
}

// LossAfter is the issuer's worst-case loss in base currency after reducing
// the plan's leg by q shares or contracts (clamped to the held size), on the
// same price grid the plan was solved on. An order capped below the planned
// quantity reports its own loss after, never the plan's.
func (p IssuerTrimPlan) LossAfter(q float64) float64 {
	if p.unit <= 0 || len(p.per) == 0 {
		return p.LossAfterBase
	}
	k := 1 - math.Min(math.Max(q, 0), p.unit)/p.unit
	worst := 0.0
	for j := range p.per {
		worst = math.Max(worst, -(p.others[j] + k*p.per[j]))
	}
	return worst
}

// PlanIssuerTrim returns the smallest reduction of one leg of the issuer
// holding symbol that brings its worst-case loss to the issuer's watch band,
// when rule 1 is at act for that issuer. The leg is the one losing most at the
// worst price (stock first on a tie). Every price scenario is linear in the
// leg's quantity, so the quantities that meet the target form one interval,
// computed exactly. Cheaper alternatives (rolls, collars, trims across legs)
// are not ranked here.
func PlanIssuerTrim(in RuleInputs, pol RulebookPolicy, symbol string) (IssuerTrimPlan, bool) {
	c := newRuleContext(in, pol)
	e, ok := c.issuerFor(symbol)
	if !ok || e.status != RuleStatusAct || !e.measured || e.strikeSpace || !c.hasNLV {
		return IssuerTrimPlan{}, false
	}
	gap := c.pol.TakeoverGapPct
	loss, at := e.worstCase(gap)
	pick, best := -1, 0.0
	for i, l := range e.legs {
		contribution := -e.legPnL(l, at)
		switch {
		case contribution <= 0:
		case contribution > best+1e-9:
			pick, best = i, contribution
		case contribution > best-1e-9 && l.stock && pick >= 0 && !e.legs[pick].stock:
			pick = i
		}
	}
	if pick < 0 {
		return IssuerTrimPlan{}, false
	}
	leg := e.legs[pick]
	target := e.exposure.WatchPct / 100 * c.nlv
	grid := e.grid(gap)
	// Per scenario: loss_j(k) = −(others_j + k·per_j), where k scales the
	// leg's quantity (1 = held, 0 = closed).
	per := make([]float64, len(grid))
	others := make([]float64, len(grid))
	for j, m := range grid {
		per[j] = e.legPnL(leg, m)
		others[j] = e.pnlAt(m) - per[j]
	}
	lossAt := func(k float64) float64 {
		worst := 0.0
		for j := range grid {
			worst = math.Max(worst, -(others[j] + k*per[j]))
		}
		return worst
	}
	lo, hi := 0.0, 1.0
	for j := range grid {
		// need −(others + k·per) ≤ target  ⇔  k·per ≥ −target − others
		rhs := -target - others[j]
		switch {
		case per[j] > 0:
			lo = math.Max(lo, rhs/per[j])
		case per[j] < 0:
			hi = math.Min(hi, rhs/per[j])
		case rhs > 0:
			lo, hi = 1, 0 // unreachable at this price whatever the size
		}
	}
	unit := math.Abs(leg.units)
	if !leg.stock {
		unit = math.Abs(leg.out.Quantity)
	}
	plan := IssuerTrimPlan{Issuer: e.exposure.Issuer, Symbol: leg.out.Symbol, Stock: leg.stock, Held: leg.out.Quantity,
		LossBeforeBase: loss, TargetBase: target, WatchPct: e.exposure.WatchPct, ActPct: e.exposure.ActPct,
		per: per, others: others, unit: unit}
	if !leg.stock {
		plan.Leg = leg.out.Leg
	}
	var keep float64 // fraction of the held quantity kept
	if lo <= hi && hi >= 0 {
		plan.Reachable = true
		keep = math.Floor(hi*unit+1e-9) / unit
		if keep*unit < lo*unit-1e-9 {
			// Rounding down to whole units overshot the interval's floor.
			keep = math.Ceil(lo*unit-1e-9) / unit
		}
	} else {
		// No size of this leg alone meets the target: the loss is convex in
		// the size, so the best size is an end or a crossing of two scenarios.
		candidates := []float64{0, 1}
		for a := range grid {
			for b := a + 1; b < len(grid); b++ {
				if d := per[a] - per[b]; d != 0 {
					if k := (others[b] - others[a]) / d; k > 0 && k < 1 {
						candidates = append(candidates, k)
					}
				}
			}
		}
		best := math.Inf(1)
		for _, k := range candidates {
			for _, r := range []float64{math.Floor(k*unit) / unit, math.Ceil(k*unit) / unit} {
				if l := lossAt(r); l < best-1e-9 || (math.Abs(l-best) <= 1e-9 && r > keep) {
					best, keep = l, r
				}
			}
		}
	}
	keep = math.Min(math.Max(keep, 0), 1)
	plan.Quantity = math.Round((1-keep)*unit*1e6) / 1e6
	plan.LossAfterBase = lossAt(keep)
	if plan.Quantity <= 0 {
		return IssuerTrimPlan{}, false
	}
	return plan, true
}
