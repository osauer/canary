// Package mandate evaluates the owner's mandate risk budget
// (internal-docs/design/mandate-risk-budget.md): the loss measure, premium
// at risk on every bought option plus the cash loss to every armed share
// stop, against the loss the mandate still allows (the headline, spec
// TM-04.12); and ex-ante portfolio volatility by historical simulation with
// full revaluation of every leg, named gap and index stresses, which sizes
// the sleeve allocations and reports the daily swing.
//
// Only the daemon evaluates: it gathers positions, equity and its
// decision-grade daily closes, calls Evaluate and serves the result as
// MethodMandateRisk. Readers fetch the daemon's verdict and never compute
// one, so a reader built at an older version cannot disagree with the
// installed daemon. The package holds no runtime state and performs no I/O.
package mandate

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"
)

// Scenarios is the number of daily log-returns the simulation runs, and
// Closes the number of shared daily closes it needs.
const (
	Scenarios = 252
	Closes    = Scenarios + 1
	// ESLevel is the expected-shortfall confidence.
	ESLevel = 0.99
	// IndexShockPct is the index stress, applied through each name's beta.
	IndexShockPct = 10.0
	// DecayDays is the time decay applied to every option in a scenario.
	DecayDays = 1.0
	// IndexSymbol is the index proxy every name's beta is measured to.
	IndexSymbol = "SPY"
)

// Verdict statuses: within (the loss measure fits the loss room and no gap
// cap is breached), reduce (it does not), unknown (something the
// measurement lacks). StatusOver was the volatility headline's middle
// reading (over the budget, under 1.25 times it); since the loss measure
// headlines (spec TM-04.12, 2026-10-27) no verdict reads it, and readers
// keep the name only for verdicts kept by an older daemon.
const (
	StatusWithin  = "within"
	StatusOver    = "over"
	StatusReduce  = "reduce"
	StatusUnknown = "unknown"
)

// Leg kinds.
const (
	KindStock  = "stock"
	KindOption = "option"
)

// Leg is one held line as the simulation prices it.
type Leg struct {
	// ID identifies the exact contract (the broker con_id); two legs of one
	// contract are summed.
	ID string `json:"id"`
	// Symbol is the line as the owner knows it; Underlying keys the daily
	// series (the stock's own symbol, or the option's underlying).
	Symbol     string `json:"symbol"`
	Underlying string `json:"underlying"`
	Kind       string `json:"kind"`
	// Quantity is signed units: shares, or contracts; Multiplier is 1 for a
	// stock and the contract multiplier for an option.
	Quantity   float64 `json:"quantity"`
	Multiplier float64 `json:"multiplier"`
	Currency   string  `json:"currency"`
	// Spot is the underlying's current price in the leg currency.
	Spot float64 `json:"spot"`
	// Option terms. IV is the leg's current implied volatility as a
	// decimal; nil means unknown, and the leg is not priced.
	Right             string   `json:"right,omitempty"`
	Strike            float64  `json:"strike,omitempty"`
	TimeToExpiryYears float64  `json:"time_to_expiry_years,omitempty"`
	IV                *float64 `json:"iv,omitempty"`
	// IVSource is empty for the position row's own volatility and
	// IVSourceMark when the daemon implied it from the leg's mark; IVAsOf is
	// that mark's time. A mark-implied volatility counts only under the
	// owner's [mandate.iv_fallback] (ivfallback.go).
	IVSource string    `json:"iv_source,omitempty"`
	IVAsOf   time.Time `json:"iv_as_of,omitzero"`
	// ISIN names the fund behind the line when the daemon knows it (a
	// position row carries none): Evaluate fills it from Input.LineISINs.
	ISIN string `json:"isin,omitempty"`
	// LossPerUnitBase is the leg's share of the loss measure per unit held,
	// in base currency (spec TM-04.12, owner decision 2026-10-27 19:50 CET):
	// for a bought option the premium at risk per contract, the higher of
	// the price paid and today's value (rule 2's line figure); for a share
	// line with an armed stop the cash loss to the stop per share. Zero for
	// a short option and for shares without a stop, whose loss the measure
	// does not bound. The daemon fills it from the position row and the
	// open-order inventory.
	LossPerUnitBase float64 `json:"loss_per_unit_base,omitempty"`
	// LossUnknown says why a bought option's premium cannot be measured; set,
	// the verdict is unknown.
	LossUnknown string `json:"loss_unknown,omitempty"`
	// StopArmed marks a share line fully covered by working stops or trails.
	// Partial coverage still contributes LossPerUnitBase; the uncovered
	// residual is also listed as unstopped on the verdict.
	StopArmed bool `json:"stop_armed,omitempty"`
}

// LossMeasure is the mandate's headline (spec TM-04.12): what the book can
// lose, as the owner measures it, against the loss the mandate still allows.
type LossMeasure struct {
	// TotalBase is PremiumBase + StopLossBase + IndexHedgeBase: the premium
	// at risk on every bought option, the cash loss to the stop on every
	// share line with an armed stop, and the loss of every held index future
	// in the 10% index move against it, in base currency.
	TotalBase    float64 `json:"total_base"`
	PremiumBase  float64 `json:"premium_base"`
	StopLossBase float64 `json:"stop_loss_base"`
	// IndexHedgeBase is the loss of the held index futures (the Reduce
	// hedge) when the index moves IndexShockPct against them: a short loses
	// in the rise, a long in the fall. A hedge spends loss room like every
	// other line (owner decision 2026-10-28 20:07 CET); a stop that covers
	// the whole line bounds it at the stop's distance instead.
	IndexHedgeBase float64 `json:"index_hedge_base,omitempty"`
	// RoomBase is the loss the mandate still allows: max_loss_pct of the
	// budget base, less the drawdown already taken (room × D × budget base).
	RoomBase float64 `json:"room_base"`
	// RatioToRoom is TotalBase over RoomBase; +Inf when the room is zero and
	// the measure is not (encoded as null).
	RatioToRoom float64 `json:"ratio_to_room"`
	// Names lists each underlying's part of the measure, largest first; the
	// forced reduction cuts in this order.
	Names []LossName `json:"names,omitempty"`
	// Unstopped names the share lines without an armed stop: their loss is
	// not bounded and not counted.
	Unstopped []string `json:"unstopped,omitempty"`
}

// LossName is one underlying's part of the loss measure.
type LossName struct {
	Underlying     string  `json:"underlying"`
	TotalBase      float64 `json:"total_base"`
	PremiumBase    float64 `json:"premium_base"`
	StopLossBase   float64 `json:"stop_loss_base"`
	IndexHedgeBase float64 `json:"index_hedge_base,omitempty"`
}

// MarshalJSON writes an unbounded ratio as null, as Risk does.
func (m LossMeasure) MarshalJSON() ([]byte, error) {
	type plain LossMeasure
	wire := struct {
		plain
		RatioToRoom *float64 `json:"ratio_to_room"`
	}{plain: plain(m)}
	if !math.IsInf(m.RatioToRoom, 0) && !math.IsNaN(m.RatioToRoom) {
		wire.RatioToRoom = &m.RatioToRoom
	}
	return json.Marshal(wire)
}

// UnmarshalJSON reads a null ratio back as unbounded while the measure is
// not zero.
func (m *LossMeasure) UnmarshalJSON(raw []byte) error {
	type plain LossMeasure
	wire := struct {
		*plain
		RatioToRoom *float64 `json:"ratio_to_room"`
	}{plain: (*plain)(m)}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return err
	}
	switch {
	case wire.RatioToRoom != nil:
		m.RatioToRoom = *wire.RatioToRoom
	case m.TotalBase > 0:
		m.RatioToRoom = math.Inf(1)
	default:
		m.RatioToRoom = 0
	}
	return nil
}

// LossRoomBase is the loss the mandate still allows: room × D × budget base.
func LossRoomBase(p Policy, c Capital) float64 {
	if !c.Known {
		return 0
	}
	return c.Room * p.MaxLossPct / 100 * c.BudgetBase
}

// MeasureLoss is the loss measure alone, without the simulation: the
// verdict's headline for a book, with every reason the verdict would give
// for it being unknown (the daemon's own grounds, capital without a loss
// room, a bought option whose premium is unmeasured). Any reason makes the
// figure unknown to a reader.
func MeasureLoss(in Input) (*LossMeasure, []string) {
	reasons := slices.Clone(in.Reasons)
	if !in.Capital.Known {
		reasons = append(reasons, in.Capital.Reasons...)
		if len(in.Capital.Reasons) == 0 {
			reasons = append(reasons, "the mandate's capital is unknown")
		}
	}
	m, lossReasons := measureLoss(in, mergeLegs(in))
	return m, append(reasons, lossReasons...)
}

// measureLoss sums the legs' loss figures (mergeLegs has normalised them)
// against the loss room. The reasons name each bought option whose premium
// cannot be measured; any makes the verdict unknown.
func measureLoss(in Input, legs []Leg) (*LossMeasure, []string) {
	out := &LossMeasure{RoomBase: LossRoomBase(in.Policy, in.Capital)}
	var reasons []string
	byName := map[string]*LossName{}
	var order []string
	for _, l := range legs {
		if l.Quantity == 0 {
			continue
		}
		if l.Kind == KindOption && l.Quantity > 0 && l.LossUnknown != "" {
			reasons = append(reasons, fmt.Sprintf("%s: premium at risk unmeasured: %s", l.Symbol, l.LossUnknown))
			continue
		}
		n, ok := byName[l.Underlying]
		if !ok {
			n = &LossName{Underlying: l.Underlying}
			byName[l.Underlying] = n
			order = append(order, l.Underlying)
		}
		loss := legLoss(l)
		switch {
		case l.Kind == KindOption && l.Quantity > 0:
			n.PremiumBase += loss
			out.PremiumBase += loss
		case IsIndexFuture(l):
			// Its loss is bounded by the index move (or a full stop), so it
			// is never unstopped.
			n.IndexHedgeBase += loss
			out.IndexHedgeBase += loss
		case l.Kind == KindStock:
			n.StopLossBase += loss
			out.StopLossBase += loss
			if !l.StopArmed {
				out.Unstopped = append(out.Unstopped, l.Symbol)
			}
		}
		n.TotalBase = n.PremiumBase + n.StopLossBase + n.IndexHedgeBase
	}
	out.TotalBase = out.PremiumBase + out.StopLossBase + out.IndexHedgeBase
	for _, name := range order {
		if n := byName[name]; n.TotalBase > 0 {
			out.Names = append(out.Names, *n)
		}
	}
	sort.SliceStable(out.Names, func(i, j int) bool { return out.Names[i].TotalBase > out.Names[j].TotalBase })
	slices.Sort(out.Unstopped)
	switch {
	case out.RoomBase > 0:
		out.RatioToRoom = out.TotalBase / out.RoomBase
	case out.TotalBase > 0:
		out.RatioToRoom = math.Inf(1)
	}
	return out, reasons
}

// legLoss is one leg's part of the loss measure, as measureLoss counts it.
func legLoss(l Leg) float64 {
	switch {
	case l.Kind == KindOption && l.Quantity > 0 && l.LossUnknown == "":
		return l.Quantity * l.LossPerUnitBase
	case l.Kind == KindStock:
		return math.Abs(l.Quantity) * l.LossPerUnitBase
	}
	return 0
}

// lossOverRoom reports a measure past the room; equal fits.
func lossOverRoom(m *LossMeasure) bool {
	return m != nil && m.TotalBase > m.RoomBase+lossEpsilon
}

// lossEpsilon absorbs floating-point dust at the room.
const lossEpsilon = 1e-6

// Series is one decision-grade daily close series, ascending by date.
type Series struct {
	Key    string    `json:"key"`
	Dates  []string  `json:"dates"`
	Closes []float64 `json:"closes"`
	// AsOf is the source time of the last read; Stale is the daemon's
	// freshness judgement, which makes the verdict unknown.
	AsOf  time.Time `json:"as_of,omitzero"`
	Stale bool      `json:"stale,omitempty"`
}

// Input is everything Evaluate reads.
type Input struct {
	Now          time.Time
	BaseCurrency string
	Policy       Policy
	Capital      Capital
	Legs         []Leg
	// Series holds the underlyings' closes by symbol, including the index.
	Series map[string]Series
	// FX holds, per non-base currency, the base units one unit of it buys,
	// as a daily series; FXNow is the rate the positions were valued at.
	FX    map[string]Series
	FXNow map[string]float64
	// NotModelled names held lines the daemon left out (bills, bonds,
	// conversions) so the verdict says so.
	NotModelled []string
	// Allocations is the allocation plan; nil measures no allocations.
	Allocations *AllocationPlan
	// OwnerStop is the owner sleeve's drawdown stop as the daemon keeps it;
	// nil while the policy writes none. Evaluate reads the sleeve's use
	// against it (sleevestop.go).
	OwnerStop *SleeveStop
	// Reasons are the daemon's own grounds for an unknown verdict (a stale
	// positions read, an unproven base currency); any makes it unknown.
	Reasons []string
	// LineISINs names, by leg ID, the ISIN IBKR listed a contract under
	// (the core's fund, resolved by ISIN): such a line takes the broad
	// index gap and its fund's returns proxy whatever its broker symbol.
	LineISINs map[string]string
}

// Risk is the simulated portfolio risk in base currency.
type Risk struct {
	SD1DBase     float64 `json:"sd_1d_base"`
	SDAnnualBase float64 `json:"sd_annual_base"`
	// ES99Base is the mean loss of the worst 1% of scenarios, positive.
	ES99Base float64 `json:"es99_base"`
	// RatioToBudget is annualised sd over the budget; +Inf when the budget
	// is zero and risk is not.
	RatioToBudget float64 `json:"ratio_to_budget"`
	// FXContributionPct is the Euler share of the currency part of every
	// leg's base P&L; the local-currency part carries the rest.
	FXContributionPct float64 `json:"fx_contribution_pct"`
}

// MarshalJSON writes a non-finite ratio (a budget of zero, at the maximum
// loss) as null: JSON has no infinity, and an encoding failure would leave
// readers on an older verdict.
func (r Risk) MarshalJSON() ([]byte, error) {
	type plain Risk
	wire := struct {
		plain
		RatioToBudget *float64 `json:"ratio_to_budget"`
	}{plain: plain(r)}
	if !math.IsInf(r.RatioToBudget, 0) && !math.IsNaN(r.RatioToBudget) {
		wire.RatioToBudget = &r.RatioToBudget
	}
	return json.Marshal(wire)
}

// UnmarshalJSON reads a null ratio back as unbounded while risk is
// measured, the value the evaluator set.
func (r *Risk) UnmarshalJSON(raw []byte) error {
	type plain Risk
	wire := struct {
		*plain
		RatioToBudget *float64 `json:"ratio_to_budget"`
	}{plain: (*plain)(r)}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return err
	}
	switch {
	case wire.RatioToBudget != nil:
		r.RatioToBudget = *wire.RatioToBudget
	case r.SDAnnualBase > 0:
		r.RatioToBudget = math.Inf(1)
	default:
		r.RatioToBudget = 0
	}
	return nil
}

// LegReport is one leg's place in the risk.
type LegReport struct {
	ID              string  `json:"id"`
	Symbol          string  `json:"symbol"`
	Underlying      string  `json:"underlying"`
	Kind            string  `json:"kind"`
	Quantity        float64 `json:"quantity"`
	ValueBase       float64 `json:"value_base"`
	ContributionPct float64 `json:"contribution_pct"`
	// Unknown names why the leg could not be priced; empty when it was.
	Unknown string `json:"unknown,omitempty"`
	// LossBase is the leg's part of the loss measure: its premium at risk,
	// or the cash loss to its armed stop.
	LossBase float64 `json:"loss_base,omitempty"`
}

// UnderlyingReport is one underlying's contribution and its stresses.
type UnderlyingReport struct {
	Underlying      string  `json:"underlying"`
	ValueBase       float64 `json:"value_base"`
	ContributionPct float64 `json:"contribution_pct"`
	Beta            float64 `json:"beta"`
	// BroadIndex says every leg of the underlying is on the broad index
	// list (IsBroadIndex); GapShockPct is the gap it was stressed by: the
	// policy's index_gap_shock_pct for a broad index when written, else
	// gap_shock_pct.
	BroadIndex  bool    `json:"broad_index,omitempty"`
	GapShockPct float64 `json:"gap_shock_pct"`
	// GapDownLossBase is the loss at −GapShockPct; GapUpLossBase is the
	// loss at +GapShockPct where a short call or short stock loses on the
	// upside, else nil. GapLossBase is the worse of the two.
	GapDownLossBase float64  `json:"gap_down_loss_base"`
	GapUpLossBase   *float64 `json:"gap_up_loss_base,omitempty"`
	GapLossBase     float64  `json:"gap_loss_base"`
	GapCapBase      float64  `json:"gap_cap_base"`
	GapBreach       bool     `json:"gap_breach,omitempty"`
}

// Stresses is the named stress set.
type Stresses struct {
	// GapShockPct is the one-name gap; IndexGapShockPct the gap a broad
	// index takes instead (equal to GapShockPct while the policy carries no
	// index_gap_shock_pct).
	GapShockPct      float64 `json:"gap_shock_pct"`
	IndexGapShockPct float64 `json:"index_gap_shock_pct,omitempty"`
	GapCapBase       float64 `json:"gap_cap_base"`
	IndexShockPct    float64 `json:"index_shock_pct"`
	// IndexLossBase is the loss when the index falls IndexShockPct and every
	// name follows with its beta; IndexRiseLossBase the loss when it rises
	// as far (owner, 2026-10-28 13:13 CET: a plan is scored on both sides).
	// A negative loss is a gain.
	IndexLossBase     float64 `json:"index_loss_base"`
	IndexRiseLossBase float64 `json:"index_rise_loss_base"`
	// GapBreaches names the underlyings whose gap loss exceeds the cap.
	GapBreaches []string `json:"gap_breaches,omitempty"`
}

// Verdict is the daemon-owned result.
type Verdict struct {
	Status       string    `json:"status"`
	Reasons      []string  `json:"reasons,omitempty"`
	AsOf         time.Time `json:"as_of"`
	BaseCurrency string    `json:"base_currency,omitempty"`
	Capital      Capital   `json:"capital"`
	// Loss is the headline: the loss measure against the loss room (spec
	// TM-04.12). It is measured whenever the legs allow, so an unknown
	// verdict still shows it.
	Loss *LossMeasure `json:"loss,omitempty"`
	// Risk and Stresses are nil while the status is unknown. The risk is
	// the sizing input and the 1-day swing, no longer the headline.
	Risk     *Risk     `json:"risk,omitempty"`
	Stresses *Stresses `json:"stresses,omitempty"`
	// Scenarios is the number of aligned daily returns the simulation ran,
	// over the dates ScenariosFrom..ScenariosTo.
	Scenarios     int                `json:"scenarios,omitempty"`
	ScenariosFrom string             `json:"scenarios_from,omitempty"`
	ScenariosTo   string             `json:"scenarios_to,omitempty"`
	Legs          []LegReport        `json:"legs,omitempty"`
	Underlyings   []UnderlyingReport `json:"underlyings,omitempty"`
	NotModelled   []string           `json:"not_modelled,omitempty"`
	// Allocations is each sleeve's allocation and use, owner first; nil while the risk
	// is not measured or the input carries no allocation plan.
	Allocations []AllocationReport `json:"allocations,omitempty"`
	// OwnerStop is the owner sleeve's drawdown stop with the sleeve's use on
	// this verdict; nil while the policy writes none.
	OwnerStop *SleeveStop `json:"owner_stop,omitempty"`
	// IVFallback names the options priced on a fallback volatility, the
	// S&P-fall range across their bands and its tier; nil when none is.
	IVFallback *IVFallbackReport `json:"iv_fallback,omitempty"`
}

// Evaluate runs the historical simulation and judges the book against the
// budget. Anything the measurement lacks reads unknown, never within.
func Evaluate(in Input) Verdict {
	v := Verdict{Status: StatusUnknown, AsOf: in.Now, BaseCurrency: in.BaseCurrency, Capital: in.Capital, NotModelled: slices.Clone(in.NotModelled)}
	v.Reasons = append(v.Reasons, in.Reasons...)
	if !in.Capital.Known {
		v.Reasons = append(v.Reasons, in.Capital.Reasons...)
	}
	legs := mergeLegs(in)
	// The headline needs no history: the loss measure is read first, and a
	// bought option whose premium is unmeasured is a reason like any other.
	var lossReasons []string
	v.Loss, lossReasons = measureLoss(in, legs)
	v.Reasons = append(v.Reasons, lossReasons...)
	// Which series the book needs: every underlying's returns (its own, or
	// its proxy's: ReturnsSeries), the index and every non-base currency.
	needed := map[string]bool{IndexSymbol: true}
	currencies := map[string]bool{}
	for _, l := range legs {
		if l.Underlying != "" {
			needed[LegReturnsSeries(l)] = true
		}
		if ccy := strings.ToUpper(l.Currency); ccy != "" && ccy != strings.ToUpper(in.BaseCurrency) {
			currencies[ccy] = true
		}
	}
	series := map[string]Series{}
	for key := range needed {
		s, ok := in.Series[key]
		switch {
		case !ok || len(s.Closes) == 0:
			v.Reasons = append(v.Reasons, fmt.Sprintf("no decision-grade daily history for %s", key))
		case s.Stale:
			v.Reasons = append(v.Reasons, fmt.Sprintf("daily history for %s is stale (last read %s)", key, s.AsOf.UTC().Format(time.DateTime)))
		case len(s.Closes) < Closes || len(s.Dates) != len(s.Closes):
			v.Reasons = append(v.Reasons, fmt.Sprintf("daily history for %s holds %d closes; %d are needed", key, len(s.Closes), Closes))
		default:
			series["u:"+key] = s
		}
	}
	for ccy := range currencies {
		s, ok := in.FX[ccy]
		switch {
		case !ok || len(s.Closes) == 0:
			v.Reasons = append(v.Reasons, fmt.Sprintf("no decision-grade daily history for the %s/%s exchange rate", in.BaseCurrency, ccy))
		case s.Stale:
			v.Reasons = append(v.Reasons, fmt.Sprintf("daily history for the %s/%s exchange rate is stale (last read %s)", in.BaseCurrency, ccy, s.AsOf.UTC().Format(time.DateTime)))
		case len(s.Closes) < Closes || len(s.Dates) != len(s.Closes):
			v.Reasons = append(v.Reasons, fmt.Sprintf("daily history for the %s/%s exchange rate holds %d closes; %d are needed", in.BaseCurrency, ccy, len(s.Closes), Closes))
		default:
			series["fx:"+ccy] = s
		}
		if rate, ok := in.FXNow[ccy]; !ok || !(rate > 0) || math.IsInf(rate, 0) {
			v.Reasons = append(v.Reasons, fmt.Sprintf("no current %s/%s exchange rate on the positions", in.BaseCurrency, ccy))
		}
	}
	// Leg-level evidence: an option without IV or any leg without a spot
	// cannot be priced; the verdict says which.
	for _, l := range legs {
		if why := legUnknownUnder(l, in.Policy); why != "" {
			v.Reasons = append(v.Reasons, fmt.Sprintf("%s: %s", l.Symbol, why))
		}
	}
	var returns map[string][]float64
	var from, to string
	if len(v.Reasons) == 0 {
		var err error
		returns, from, to, err = alignedReturns(series)
		if err != nil {
			v.Reasons = append(v.Reasons, err.Error())
		}
	}
	// Reports with contributions unknown still list the legs.
	if len(v.Reasons) > 0 {
		for _, l := range legs {
			v.Legs = append(v.Legs, LegReport{ID: l.ID, Symbol: l.Symbol, Underlying: l.Underlying, Kind: l.Kind, Quantity: l.Quantity, Unknown: firstNonEmpty(legUnknownUnder(l, in.Policy), "not simulated"), LossBase: legLoss(l)})
		}
		ApplySleeveStop(&v, in.OwnerStop)
		return v
	}
	v.Scenarios, v.ScenariosFrom, v.ScenariosTo = Scenarios, from, to
	sim := simulate(in, legs, returns)
	v.Risk, v.Legs, v.Underlyings, v.Stresses, v.Allocations = sim.risk, sim.legs, sim.underlyings, sim.stresses, sim.allocations
	// The simulation orders its reports by contribution, so each leg's loss
	// is matched by ID, never by position.
	lossByID := map[string]float64{}
	for _, l := range legs {
		lossByID[l.ID] += legLoss(l)
	}
	for i := range v.Legs {
		v.Legs[i].LossBase = lossByID[v.Legs[i].ID]
	}
	budget := in.Capital.BudgetAnnualSD
	if sim.risk.SDAnnualBase > 0 && budget <= 0 {
		v.Risk.RatioToBudget = math.Inf(1)
	} else if budget > 0 {
		v.Risk.RatioToBudget = sim.risk.SDAnnualBase / budget
	}
	// The headline (spec TM-04.12, owner decision 2026-10-27 19:50 CET): the
	// loss measure against the loss room, and the gap caps, which are shares
	// of the same room. The volatility ratio is reported, not judged.
	switch {
	case len(sim.stresses.GapBreaches) > 0 || lossOverRoom(v.Loss):
		v.Status = StatusReduce
	default:
		v.Status = StatusWithin
	}
	if r := judgeIVFallback(in, legs, returns, sim, v.Loss, v.Status); r != nil {
		v.IVFallback = r
		if r.Tier == IVTierUnknown {
			// Unknown keeps its contract: no risk, stresses or allocations.
			v.Status, v.Risk, v.Stresses, v.Allocations = StatusUnknown, nil, nil, nil
			v.Reasons = append(v.Reasons, r.Reason)
		}
	}
	ApplySleeveStop(&v, in.OwnerStop)
	return v
}

// WithLeg returns the input with one more leg (an order's addition), summed
// into the same contract when held, for the incremental pre-trade check.
func WithLeg(in Input, add Leg) Input {
	out := in
	out.Legs = append(slices.Clone(in.Legs), add)
	return out
}

// legUnknownUnder is legUnknown under the policy: a mark-implied volatility
// counts only while the owner's [mandate.iv_fallback] is written.
func legUnknownUnder(l Leg, p Policy) string {
	if l.Kind == KindOption && l.IVSource == IVSourceMark && p.IVFallback == nil {
		return "no implied volatility on the position row"
	}
	return legUnknown(l)
}

func legUnknown(l Leg) string {
	switch l.Kind {
	case KindStock:
		if !(l.Spot > 0) {
			return "no price"
		}
	case KindOption:
		if !(l.Spot > 0) {
			return "no underlying price"
		}
		if l.IV == nil || !(*l.IV > 0) || math.IsInf(*l.IV, 0) {
			return "no implied volatility on the position row"
		}
		if !(l.Strike > 0) || (l.Right != "C" && l.Right != "P") {
			return "incomplete option terms"
		}
	default:
		return fmt.Sprintf("security type %q is not modelled", l.Kind)
	}
	if l.Multiplier <= 0 {
		return "no contract multiplier"
	}
	return ""
}

// IsIndexFuture reports a linear leg on the index future key: a held Micro
// S&P future, or the Reduce hedge's planned or ordered contracts.
func IsIndexFuture(l Leg) bool {
	return l.Kind == KindStock && strings.EqualFold(strings.TrimSpace(l.Underlying), MicroSP500Future)
}

// IndexFutureLossPerUnit is one contract's loss when the index moves
// IndexShockPct against it, in base currency at fx: price × multiplier ×
// the shock. The sign of the holding does not matter: a short loses in the
// rise, a long in the fall, by the same amount.
func IndexFutureLossPerUnit(l Leg, fx float64) float64 {
	if !(l.Spot > 0) || !(l.Multiplier > 0) || !(fx > 0) || math.IsInf(fx, 0) {
		return 0
	}
	return IndexShockPct / 100 * l.Spot * l.Multiplier * fx
}

// fxNowFor is the base units one unit of the leg's currency buys now: 1 in
// the base currency, else the positions' rate (0 when none, which the
// verdict names as a missing rate).
func fxNowFor(in Input, l Leg) float64 {
	ccy := strings.ToUpper(strings.TrimSpace(l.Currency))
	if ccy == "" || ccy == strings.ToUpper(strings.TrimSpace(in.BaseCurrency)) {
		return 1
	}
	return in.FXNow[ccy]
}

// mergeLegs normalises the input's legs and sums two lines of one contract.
// A held index future without a full stop counts its index-move loss per
// contract here, so every book the evaluator reads (the verdict, a plan's
// after book, the gate's book with the order) measures it the same way.
func mergeLegs(in Input) []Leg {
	index := map[string]int{}
	var out []Leg
	for _, l := range in.Legs {
		l.Right = strings.ToUpper(strings.TrimSpace(l.Right))
		l.Currency = strings.ToUpper(strings.TrimSpace(l.Currency))
		l.Underlying = strings.ToUpper(strings.TrimSpace(l.Underlying))
		if isin, ok := in.LineISINs[l.ID]; ok && l.ID != "" && l.ISIN == "" {
			l.ISIN = isin
		}
		l.ISIN = strings.ToUpper(strings.TrimSpace(l.ISIN))
		if IsIndexFuture(l) && !l.StopArmed {
			l.LossPerUnitBase = IndexFutureLossPerUnit(l, fxNowFor(in, l))
		}
		if i, ok := index[l.ID]; ok && l.ID != "" {
			// Two lines of one contract on one side: the loss per unit is
			// the quantity-weighted figure, so the sum carries both
			// premiums. A closing line (an order selling what is held) only
			// takes units away: those left keep their own figure and stop,
			// and past flat the new side's units are the closing line's.
			was, add := out[i].Quantity, l.Quantity
			switch {
			case was*add >= 0:
				if math.Abs(was)+math.Abs(add) > 0 {
					out[i].LossPerUnitBase = (out[i].LossPerUnitBase*math.Abs(was) + l.LossPerUnitBase*math.Abs(add)) / (math.Abs(was) + math.Abs(add))
				}
				out[i].StopArmed = out[i].StopArmed && l.StopArmed
				if l.LossUnknown != "" && out[i].LossUnknown == "" {
					out[i].LossUnknown = l.LossUnknown
				}
			case math.Abs(add) > math.Abs(was):
				out[i].LossPerUnitBase, out[i].StopArmed, out[i].LossUnknown = l.LossPerUnitBase, l.StopArmed, l.LossUnknown
			}
			out[i].Quantity += l.Quantity
			continue
		}
		index[l.ID] = len(out)
		out = append(out, l)
	}
	return out
}

// alignedReturns intersects the series' dates, keeps the last Closes shared
// dates and returns each series' log-returns over them.
func alignedReturns(series map[string]Series) (map[string][]float64, string, string, error) {
	if len(series) == 0 {
		return nil, "", "", fmt.Errorf("no series to align")
	}
	var shared map[string]bool
	for _, s := range series {
		dates := map[string]bool{}
		for i, d := range s.Dates {
			if s.Closes[i] > 0 && !math.IsInf(s.Closes[i], 0) {
				dates[d] = true
			}
		}
		if shared == nil {
			shared = dates
			continue
		}
		for d := range shared {
			if !dates[d] {
				delete(shared, d)
			}
		}
	}
	dates := make([]string, 0, len(shared))
	for d := range shared {
		dates = append(dates, d)
	}
	sort.Strings(dates)
	if len(dates) < Closes {
		return nil, "", "", fmt.Errorf("only %d daily closes are shared by every needed series; %d are needed", len(dates), Closes)
	}
	dates = dates[len(dates)-Closes:]
	out := make(map[string][]float64, len(series))
	for key, s := range series {
		byDate := make(map[string]float64, len(s.Dates))
		for i, d := range s.Dates {
			byDate[d] = s.Closes[i]
		}
		r := make([]float64, Scenarios)
		for i := 1; i < len(dates); i++ {
			r[i-1] = math.Log(byDate[dates[i]] / byDate[dates[i-1]])
		}
		out[key] = r
	}
	return out, dates[0], dates[len(dates)-1], nil
}

type simulation struct {
	risk        *Risk
	legs        []LegReport
	underlyings []UnderlyingReport
	stresses    *Stresses
	allocations []AllocationReport
}

// legValue prices one leg in its own currency at spot and the given time to
// expiry (options), per unit times quantity and multiplier.
func legValue(l Leg, spot, t float64) float64 {
	switch l.Kind {
	case KindOption:
		return l.Quantity * l.Multiplier * OptionPrice(l.Right == "C", spot, l.Strike, t, *l.IV)
	default:
		return l.Quantity * l.Multiplier * spot
	}
}

func simulate(in Input, legs []Leg, returns map[string][]float64) simulation {
	base := strings.ToUpper(in.BaseCurrency)
	fxNow := func(l Leg) float64 {
		if l.Currency == "" || l.Currency == base {
			return 1
		}
		return in.FXNow[l.Currency]
	}
	n := Scenarios
	total := make([]float64, n)
	fxPart := make([]float64, n)
	legPnL := make([][]float64, len(legs))
	values := make([]float64, len(legs))
	for i, l := range legs {
		r := returns["u:"+LegReturnsSeries(l)]
		var rfx []float64
		if l.Currency != "" && l.Currency != base {
			rfx = returns["fx:"+l.Currency]
		}
		fx := fxNow(l)
		now := legValue(l, l.Spot, l.TimeToExpiryYears)
		values[i] = now * fx
		pnl := make([]float64, n)
		for k := range n {
			next := legValue(l, l.Spot*math.Exp(r[k]), l.TimeToExpiryYears-DecayDays/365)
			local := (next - now) * fx
			fxk := fx
			if rfx != nil {
				fxk = fx * math.Exp(rfx[k])
			}
			pnl[k] = next*fxk - now*fx
			fxPart[k] += pnl[k] - local
			total[k] += pnl[k]
		}
		legPnL[i] = pnl
	}
	mean, variance := meanVar(total)
	sd := math.Sqrt(variance)
	risk := &Risk{SD1DBase: sd, SDAnnualBase: sd * math.Sqrt(Scenarios), ES99Base: expectedShortfall(total)}
	if variance > 0 {
		risk.FXContributionPct = covariance(fxPart, total, mean) / variance * 100
	}
	// Euler contributions: cov(leg, total) / var(total), summing to 100%.
	legReports := make([]LegReport, len(legs))
	contributions := make([]float64, len(legs))
	byUnderlying := map[string]*UnderlyingReport{}
	returnsOf := map[string]string{}
	var order []string
	for i, l := range legs {
		rep := LegReport{ID: l.ID, Symbol: l.Symbol, Underlying: l.Underlying, Kind: l.Kind, Quantity: l.Quantity, ValueBase: values[i]}
		if variance > 0 {
			rep.ContributionPct = covariance(legPnL[i], total, mean) / variance * 100
		}
		legReports[i], contributions[i] = rep, rep.ContributionPct
		u, ok := byUnderlying[l.Underlying]
		if !ok {
			u = &UnderlyingReport{Underlying: l.Underlying}
			byUnderlying[l.Underlying] = u
			order = append(order, l.Underlying)
			returnsOf[l.Underlying] = LegReturnsSeries(l)
		}
		u.ValueBase += rep.ValueBase
		u.ContributionPct += rep.ContributionPct
	}
	// Betas to the index over the same returns.
	indexReturns := returns["u:"+IndexSymbol]
	indexMean, indexVar := meanVar(indexReturns)
	cap := GapCapBase(in.Policy, in.Capital)
	stresses := &Stresses{GapShockPct: in.Policy.GapShockPct, IndexGapShockPct: in.Policy.GapShockPctFor(true), GapCapBase: cap, IndexShockPct: IndexShockPct}
	betas := map[string]float64{}
	for _, name := range order {
		u := byUnderlying[name]
		if name == IndexSymbol {
			u.Beta = 1
		} else if indexVar > 0 {
			u.Beta = covariance(returns["u:"+returnsOf[name]], indexReturns, indexMean) / indexVar
		}
		betas[name] = u.Beta
		u.BroadIndex = broadIndexUnderlying(legs, name)
		u.GapShockPct = in.Policy.GapShockPctFor(u.BroadIndex)
		gap := u.GapShockPct / 100
		u.GapDownLossBase = shockLoss(legs, fxNow, func(l Leg) float64 {
			if l.Underlying == name {
				return 1 - gap
			}
			return 1
		})
		if upsideLoser(legs, name) {
			up := shockLoss(legs, fxNow, func(l Leg) float64 {
				if l.Underlying == name {
					return 1 + gap
				}
				return 1
			})
			u.GapUpLossBase = &up
		}
		u.GapLossBase = u.GapDownLossBase
		if u.GapUpLossBase != nil && *u.GapUpLossBase > u.GapLossBase {
			u.GapLossBase = *u.GapUpLossBase
		}
		u.GapCapBase = cap
		if u.GapLossBase > cap {
			u.GapBreach = true
			stresses.GapBreaches = append(stresses.GapBreaches, name)
		}
	}
	stresses.IndexLossBase = shockLoss(legs, fxNow, func(l Leg) float64 {
		return 1 - IndexShockPct/100*betas[l.Underlying]
	})
	stresses.IndexRiseLossBase = shockLoss(legs, fxNow, func(l Leg) float64 {
		return 1 + IndexShockPct/100*betas[l.Underlying]
	})
	underlyings := make([]UnderlyingReport, 0, len(order))
	for _, name := range order {
		underlyings = append(underlyings, *byUnderlying[name])
	}
	slices.SortStableFunc(underlyings, func(a, b UnderlyingReport) int {
		if a.ContributionPct != b.ContributionPct {
			if a.ContributionPct > b.ContributionPct {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Underlying, b.Underlying)
	})
	slices.SortStableFunc(legReports, func(a, b LegReport) int {
		if a.ContributionPct != b.ContributionPct {
			if a.ContributionPct > b.ContributionPct {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Symbol, b.Symbol)
	})
	var allocations []AllocationReport
	if in.Allocations != nil {
		allocations = allocationReports(in.Allocations, in.Policy, in.Capital, legs, legPnL, contributions, risk)
	}
	return simulation{risk: risk, legs: legReports, underlyings: underlyings, stresses: stresses, allocations: allocations}
}

// shockLoss revalues every leg at its underlying's spot times factor(leg)
// (an instant gap: no time decay, the currency unchanged) and returns the
// loss in base currency, positive when the book loses.
func shockLoss(legs []Leg, fxNow func(Leg) float64, factor func(Leg) float64) float64 {
	loss := 0.0
	for _, l := range legs {
		f := factor(l)
		if f <= 0 {
			f = 0
		}
		now := legValue(l, l.Spot, l.TimeToExpiryYears)
		next := legValue(l, l.Spot*f, l.TimeToExpiryYears)
		loss -= (next - now) * fxNow(l)
	}
	return loss
}

// broadIndexUnderlying reports whether every leg of an underlying is on
// the broad index list: one leg that is not keeps the one-name gap.
func broadIndexUnderlying(legs []Leg, underlying string) bool {
	found := false
	for _, l := range legs {
		if l.Underlying != underlying {
			continue
		}
		if !IsBroadIndexLine(l.Underlying, l.Currency, l.ISIN) {
			return false
		}
		found = true
	}
	return found
}

// upsideLoser reports whether an underlying carries a leg that loses when
// its price gaps up: short stock or a short call.
func upsideLoser(legs []Leg, underlying string) bool {
	for _, l := range legs {
		if l.Underlying != underlying || l.Quantity >= 0 {
			continue
		}
		if l.Kind == KindStock || l.Kind == KindOption && l.Right == "C" {
			return true
		}
	}
	return false
}

func meanVar(x []float64) (mean, variance float64) {
	if len(x) < 2 {
		return 0, 0
	}
	for _, v := range x {
		mean += v
	}
	mean /= float64(len(x))
	for _, v := range x {
		variance += (v - mean) * (v - mean)
	}
	return mean, variance / float64(len(x)-1)
}

func covariance(a, total []float64, totalMean float64) float64 {
	if len(a) != len(total) || len(a) < 2 {
		return 0
	}
	aMean := 0.0
	for _, v := range a {
		aMean += v
	}
	aMean /= float64(len(a))
	sum := 0.0
	for i := range a {
		sum += (a[i] - aMean) * (total[i] - totalMean)
	}
	return sum / float64(len(a)-1)
}

// expectedShortfall is the mean loss of the worst ⌈(1 − ESLevel) × n⌉
// scenarios, positive.
func expectedShortfall(pnl []float64) float64 {
	if len(pnl) == 0 {
		return 0
	}
	sorted := slices.Clone(pnl)
	sort.Float64s(sorted)
	m := int(math.Ceil((1 - ESLevel) * float64(len(sorted))))
	m = max(1, min(m, len(sorted)))
	sum := 0.0
	for _, v := range sorted[:m] {
		sum += v
	}
	return -sum / float64(m)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
