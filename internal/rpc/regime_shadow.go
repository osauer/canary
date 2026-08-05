package rpc

import "math"

// A shadow scoring of the regime, computed beside the shipped stage machine
// and never served.
//
// The shipped machine decides in bands: an indicator is red or it is not, and
// six side-conditions then decide whether that red is allowed to mean
// anything. That shape is why 2026-06-12 happened — HYG closed 0.07% below its
// 50-day average, which is one side of a line, and one side of a line was the
// whole input. The gates added afterwards each filter one way the line lies.
//
// This model asks how bad instead of whether. Each indicator gets a strength
// on [0,1] ramped through three anchors, so a 7bp break and a 3% break are
// different numbers rather than the same band. The claim under test is that
// the ramp makes the false positive unrepresentable on arithmetic alone,
// without any of the gates.
//
// It is recorded to the decisions journal so the claim can be settled on real
// readings instead of argued. Nothing reads it back. Do not wire it to a
// surface, a rule, or an alert until the corpus says which model is right —
// and the corpus cannot say that yet: at the time of writing it holds a
// fortnight of calm tape in which six of the eight indicators never printed a
// red row.
//
// Design notes, including why four of the six gates are absent and two are
// kept, are in the shadow spec carried with the analysis harness.

// RegimeShadowIndicatorInput is one indicator's reading for the shadow model.
type RegimeShadowIndicatorInput struct {
	Indicator string
	// Depth is the indicator's stress axis: higher is worse, for every
	// indicator, in that indicator's own units. It is the same quantity the
	// eligibility gate reads. Nil means the row measured nothing.
	Depth *float64
	// FreshnessClass gates confirmation exactly as it does today. Currency is
	// the one row gate the ramp cannot subsume: how bad a reading is and
	// whether it is current are different questions.
	FreshnessClass string
	// StressSessions counts consecutive sessions since the indicator was last
	// green. Unlike the band-scoped streak it does not reset when a band
	// worsens, which is what keeps the score monotone under deterioration.
	// Zero means unknown and is treated as one.
	StressSessions int
	// Rankable is false when the row cannot be ranked at all (gamma's
	// rankability veto, a breadth engine that is not ready).
	Rankable bool
	// SecondaryDepth is the reading on an indicator's second banding axis,
	// where it has one. Credit bands on an OAS level OR a 20-day widening;
	// only scoring the level makes a widening-driven red read as calm. Nil
	// for every indicator that bands on one axis.
	SecondaryDepth *float64
}

// RegimeShadowIndicator is one indicator's scored contribution.
type RegimeShadowIndicator struct {
	Strength    float64 `json:"s"`
	Persistence float64 `json:"p"`
	Weight      float64 `json:"w"`
	// Zeroed names the gate that zeroed this row, empty when none did.
	Zeroed string `json:"zeroed,omitempty"`
}

// RegimeShadowRead is the whole shadow scoring of one snapshot. It is written
// to the decisions journal and is deliberately compact: the per-indicator
// weights are what make a disagreement diagnosable, and nothing beyond them
// is stored.
type RegimeShadowRead struct {
	Model string `json:"model"`
	// Confirming is the arm that gates the two stress stages. Each cluster
	// contributes only what it carries ABOVE its own red boundary, so no
	// quantity of sub-red evidence can reach the cutoff however much of it
	// there is. The tape then multiplies that sum, so a broken tape over a
	// board carrying nothing above red multiplies zero.
	Confirming float64 `json:"confirming"`
	// Warning is the cluster sum plus the tape, and reaches early_warning.
	// The tape is an addend here and a multiplier on Confirming: the shipped
	// machine warns on a tape break alone and never confirms on one, and this
	// is that same asymmetry.
	Warning float64 `json:"warning"`
	Tape    float64 `json:"tape"`
	Stage   string  `json:"stage"`
	Ranked  int     `json:"ranked"`

	Clusters   map[string]float64               `json:"clusters,omitempty"`
	Indicators map[string]RegimeShadowIndicator `json:"indicators,omitempty"`
}

// RegimeShadowModel identifies the scoring rules a stored read was produced
// under. A change to the anchors, the arms, or the cutoffs must bump it, or
// the corpus blends models and measures nothing. Same discipline as
// RegimeCurrencyPolicyVersion.
const RegimeShadowModel = "regime-shadow-v4"

// Stage cutoffs. Derived, not fitted: under the confirming arm's transform a
// cluster exactly at its red boundary contributes 0 and one at saturation
// contributes 1.0, so 1.0 is "one saturated cluster, or two at the ramp
// midpoint" — the shape of the shipped machine's two-eligible-reds rule.
//
// They are NOT fitted to the decisions journal, and must not be. That journal
// is a fortnight of calm tape in which only two indicators ever printed red;
// a cutoff fitted there is fitted to two indicators' noise. Refit only against
// a corpus that contains stress.
//
// The tape multiplier is derived the same way, by reading the shipped
// machine's own tape arms as a relaxation of its required red count:
//
//	confirmed_stress  2 eligible reds → 1 at SPY −2.5%   ratio 2 = 1 + tape(−2.5%)
//	panic             3 eligible reds → 1 at SPY −4.0%   ratio 3 = 1 + tape(−4.0%)
//
// Both land on 1 + tape, on the ramp's own integer labels, which is why the
// multiplier carries no constant of its own. The shipped machine's third
// relaxation — 3 reds → 0 at SPY −7% — is the one this model declines; see
// RegimeShadowTape.
const (
	regimeShadowConfirmedCutoff = 1.0
	regimeShadowPanicCutoff     = 1.5
	regimeShadowEarlyCutoff     = 0.25
)

// regimeShadowRamp is a strength ramp: green scores 0, red scores 0.5, and
// saturation scores 1.0, linear in between and clamped outside.
type regimeShadowRamp struct{ green, red, saturation float64 }

// regimeShadowRampAnchors carries each indicator's two band boundaries on its
// depth axis, which runs the same direction for all eight: higher is worse.
// The saturation anchor is deliberately absent — it is the gate table's
// FastDepth, and primaryRamp reads it from there so that number keeps exactly
// one home. Repeating it here is the drift that put four wrong thresholds in
// the backtest builder's published text.
//
// The boundaries themselves ARE transcribed, so the analysis harness bisects
// each compiled classifier and checks them.
var regimeShadowRampAnchors = map[string]struct{ green, red float64 }{
	RegimeIndicatorVIXTerm:  {0.92, 1.00},
	RegimeIndicatorVolOfVol: {90, 110},
	// hyg_spy's yellow/red split is carried by a second axis this model does
	// not read (SPY against its 52-week high), so 0.5 anchors on the gate's
	// own noise floor instead of a bisected boundary. Stated as a limit
	// rather than hidden: this indicator's mid-ramp is the weakest anchor
	// in the table.
	RegimeIndicatorHYGSPY:    {0.0, 0.25},
	RegimeIndicatorCredit:    {4.0, 5.5},
	RegimeIndicatorFunding:   {25, 75},
	RegimeIndicatorUSDJPY:    {1.0, 2.0},
	RegimeIndicatorGammaZero: {-2.0, 2.0},
	RegimeIndicatorBreadth:   {-15.0, 0.0},
}

// primaryRamp assembles an indicator's ramp, taking the saturation anchor from
// the gate table rather than a second copy.
func primaryRamp(indicator string) (regimeShadowRamp, bool) {
	a, ok := regimeShadowRampAnchors[indicator]
	if !ok {
		return regimeShadowRamp{}, false
	}
	return regimeShadowRamp{a.green, a.red, regimeGates[indicator].FastDepth}, true
}

// regimeShadowSecondaryRamps carries the second axis for an indicator that
// bands on more than one, scored on its own ramp and combined worst-of.
//
// Credit is the case that forces this. Its band goes red on HY OAS >= 5.5 OR
// a 20-day widening >= 1.00pp, and those are different quantities. Scoring the
// level alone put a red reached through the widening path — HY OAS near 4.2 —
// at a strength of about 0.07, so the model stayed silent on exactly the shape
// a credit event takes when it arrives as fast repricing from a low base.
//
// Saturation is explicit here because the gate table's FastDepth describes the
// primary axis only. 1.3pp is the same red + 0.6 x (yellow band width) rule the
// gate table states, and it is the weakest number in this file: 2008 widened
// high yield by many times it. It says "a 20-day repricing this fast is as bad
// as this axis can express", not "this is as bad as credit gets".
var regimeShadowSecondaryRamps = map[string]regimeShadowRamp{
	RegimeIndicatorCredit: {0.5, 1.0, 1.3},
}

// RegimeShadowRampFor exposes an indicator's primary ramp anchors for the
// analysis harness, which bisects the compiled classifier and checks them. The
// bool reports whether the indicator is known.
func RegimeShadowRampFor(indicator string) (green, red, saturation float64, ok bool) {
	r, ok := primaryRamp(indicator)
	if !ok {
		return 0, 0, 0, false
	}
	return r.green, r.red, r.saturation, true
}

// RegimeShadowSecondaryRampFor exposes an indicator's second axis, if it has
// one. The bool reports whether it does.
func RegimeShadowSecondaryRampFor(indicator string) (green, red, saturation float64, ok bool) {
	r, ok := regimeShadowSecondaryRamps[indicator]
	if !ok {
		return 0, 0, 0, false
	}
	return r.green, r.red, r.saturation, true
}

// RegimeShadowStageFor exposes the arm-to-stage mapping for the analysis
// harness, which scores candidate arm placements against a stored read. It is
// the same function the model uses, so a candidate is scored on the shipped
// cutoffs rather than on a second copy of them.
func RegimeShadowStageFor(warning, confirming float64, ranked int) string {
	return regimeShadowStage(&RegimeShadowRead{Warning: warning, Confirming: confirming}, ranked)
}

// rampStrength maps a reading onto [0,1] through three anchors, clamped
// outside. Linear in between, so the derivative is finite everywhere and no
// reading sits on a cliff.
func (r regimeShadowRamp) strength(depth float64) float64 {
	switch {
	case depth <= r.green:
		return 0
	case r.saturation <= r.red:
		// No saturation anchor above the red boundary: the red boundary is as
		// far as this axis is calibrated, so it saturates there.
		return 1
	case depth >= r.saturation:
		return 1
	case depth <= r.red:
		return 0.5 * (depth - r.green) / (r.red - r.green)
	default:
		return 0.5 + 0.5*(depth-r.red)/(r.saturation-r.red)
	}
}

// regimeShadowStrength scores an indicator across every axis it bands on and
// takes the worst, so a red reached on either axis is scored as a red.
func regimeShadowStrength(indicator string, depth float64, secondary *float64) float64 {
	r, ok := primaryRamp(indicator)
	if !ok {
		return 0
	}
	s := r.strength(depth)
	if secondary != nil {
		if sr, ok := regimeShadowSecondaryRamps[indicator]; ok {
			s = math.Max(s, sr.strength(*secondary))
		}
	}
	return s
}

// regimeShadowPersistence ramps time-in-stress onto [0,1]. Deep evidence needs
// no time — at full strength this is 1 on day one, which is what the shipped
// fast path buys — and shallow evidence needs the indicator's full minimum.
//
// It rides on sessions since the indicator was last GREEN, not on the
// band-scoped streak, because the band-scoped streak resets when a band
// worsens and would put a cliff at exactly the boundary that matters.
func regimeShadowPersistence(indicator string, strength float64, stressSessions int) float64 {
	minSessions := max(regimeGates[indicator].MinSessions, 1)
	sessions := max(stressSessions, 1)
	p := (float64(sessions) + strength*float64(minSessions-1)) / float64(minSessions)
	return math.Min(1, p)
}

// RegimeShadowTape scores the tape on the same anchors the shipped machine's
// crash triggers use: 0 at −1.5%, 1 at −2.5%, 2 at −4%, 3 at −7%. It lives
// here rather than in the daemon so it shares its session and currency
// predicates with the machine it mirrors — a second copy of those is the drift
// this codebase has already paid for once.
//
// The term multiplies the confirming arm and adds into the warning arm. A day's
// price is not independent evidence that a regime exists — it is the event the
// regime is supposed to anticipate — so it may not create a confirmation, only
// weigh one the clusters already carry. Multiplying is what "may not create"
// means arithmetically: a board with nothing above red confirms at no tape
// level, because the tape multiplies zero.
//
// Two earlier placements are recorded as failures. An unconditional arm at the
// ramp's integer labels confirmed stress on a wholly green board; moving the
// arms to −4%/−7% moved that class rather than removing it. A third — adding
// the tape only once some cluster is above red — enumerates identically to the
// multiplier on the discrete alphabet and fails on the continuum: it confirms
// the instant a reading steps over its red line, which is 2026-06-12 with the
// tape as amplifier.
//
// One divergence from the shipped machine is deliberate. Its −7% arm panics
// with no cluster evidence at all; here that reading warns. A −7% print over
// six green clusters is a contradiction the market does not produce, so the
// state says the board is wrong, not that the regime is confirmed.
func RegimeShadowTape(r RegimeSnapshotResult) float64 {
	// Currency is the one gate the ramp cannot subsume, and it binds here for
	// the same reason it binds on a row: the term now carries weight, so a
	// frozen or overdue print must not multiply a confirmation. Same predicate
	// the shipped machine gates its own SPY arms on, not a second copy.
	if !regimeLifecycleSPYTapeCurrent(r) {
		return 0
	}
	drop := -*r.HYGSPYDivergence.SPYChangePct
	switch {
	case drop <= 1.5:
		return 0
	case drop <= 2.5:
		return drop - 1.5
	case drop <= 4.0:
		return 1 + (drop-2.5)/1.5
	case drop <= 7.0:
		return 2 + (drop-4.0)/3.0
	default:
		return 3
	}
}

// EvaluateRegimeShadow scores a snapshot under the shadow model. Pure: no
// clock, no I/O, and no dependence on map iteration order.
func EvaluateRegimeShadow(rows []RegimeShadowIndicatorInput, tape float64, ranked int) *RegimeShadowRead {
	out := &RegimeShadowRead{
		Model: RegimeShadowModel, Tape: tape, Ranked: ranked,
		Clusters: map[string]float64{}, Indicators: map[string]RegimeShadowIndicator{},
	}
	for _, in := range rows {
		if _, known := regimeShadowRampAnchors[in.Indicator]; !known {
			continue
		}
		var scored RegimeShadowIndicator
		switch {
		case !in.Rankable:
			scored.Zeroed = "unrankable"
		case in.Depth == nil:
			scored.Zeroed = "no_reading"
		case !RegimeCurrencyMayConfirm(in.FreshnessClass):
			// Banded and visible, but never confirming. Identical policy to
			// the shipped machine.
			scored.Strength = regimeShadowStrength(in.Indicator, *in.Depth, in.SecondaryDepth)
			scored.Zeroed = "currency"
		default:
			scored.Strength = regimeShadowStrength(in.Indicator, *in.Depth, in.SecondaryDepth)
			scored.Persistence = regimeShadowPersistence(in.Indicator, scored.Strength, in.StressSessions)
			scored.Weight = scored.Strength * scored.Persistence
		}
		out.Indicators[in.Indicator] = scored
		if cluster := RegimeIndicatorCluster(in.Indicator); cluster != "" {
			out.Clusters[cluster] = math.Max(out.Clusters[cluster], scored.Weight)
		}
	}
	for _, c := range out.Clusters {
		out.Warning += c
		// Only what a cluster carries above its own red boundary reaches the
		// confirming arm. Six clusters just under red contribute their full
		// weight to Warning and exactly zero to Confirming, which is what
		// stops mild evidence accumulating into a confirmation.
		out.Confirming += math.Max(0, 2*(c-0.5))
	}
	// The tape weighs confirmation but never creates it, and warns on its own.
	// See RegimeShadowTape for why the arms take different shapes.
	out.Confirming *= 1 + tape
	out.Warning += tape
	out.Stage = regimeShadowStage(out, ranked)
	return out
}

func regimeShadowStage(r *RegimeShadowRead, ranked int) string {
	if ranked < RegimeVerdictFloor {
		return LifecycleDataQuality
	}
	switch {
	case r.Confirming >= regimeShadowPanicCutoff:
		return LifecyclePanic
	case r.Confirming >= regimeShadowConfirmedCutoff:
		return LifecycleConfirmedStress
	case r.Warning >= regimeShadowEarlyCutoff:
		return LifecycleEarlyWarning
	default:
		return LifecycleQuiet
	}
}
