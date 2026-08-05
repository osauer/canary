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
	// there is.
	Confirming float64 `json:"confirming"`
	// Warning is the plain sum and reaches early_warning.
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
const RegimeShadowModel = "regime-shadow-v3"

// Stage cutoffs. Derived, not fitted: under the confirming arm's transform a
// cluster exactly at its red boundary contributes 0 and one at saturation
// contributes 1.0, so 1.0 is "one saturated cluster, or two at the ramp
// midpoint" — the shape of the shipped machine's two-eligible-reds rule.
//
// They are NOT fitted to the decisions journal, and must not be. That journal
// is a fortnight of calm tape in which only two indicators ever printed red;
// a cutoff fitted there is fitted to two indicators' noise. Refit only against
// a corpus that contains stress.
const (
	regimeShadowConfirmedCutoff = 1.0
	regimeShadowPanicCutoff     = 1.5
	regimeShadowEarlyCutoff     = 0.25
)

// regimeShadowRamp is an indicator's strength ramp below the saturation point.
// Green is the green/yellow boundary and scores 0; Red is the yellow/red
// boundary and scores 0.5; the 1.0 anchor is the gate table's FastDepth, read
// from regimeGates rather than repeated here.
//
// All three are on the indicator's depth axis, which runs the same direction
// for every indicator: higher is worse. Anchors are checked against the
// compiled classifiers by bisection in the analysis harness, because a
// transcribed boundary is exactly the drift that put four wrong thresholds in
// the backtest builder's published text.
type regimeShadowRamp struct{ green, red float64 }

var regimeShadowRamps = map[string]regimeShadowRamp{
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

// RegimeShadowRampFor exposes an indicator's ramp anchors for the analysis
// harness, which bisects the compiled classifier and checks them. The bool
// reports whether the indicator is known.
func RegimeShadowRampFor(indicator string) (green, red, saturation float64, ok bool) {
	r, ok := regimeShadowRamps[indicator]
	if !ok {
		return 0, 0, 0, false
	}
	return r.green, r.red, regimeGates[indicator].FastDepth, true
}

// regimeShadowStrength ramps a depth reading onto [0,1] through the three
// anchors, clamped outside. Linear in between, so the derivative is finite
// everywhere and no reading sits on a cliff.
func regimeShadowStrength(indicator string, depth float64) float64 {
	r, ok := regimeShadowRamps[indicator]
	if !ok {
		return 0
	}
	sat := regimeGates[indicator].FastDepth
	switch {
	case depth <= r.green:
		return 0
	case depth >= sat && sat > r.red:
		return 1
	case depth <= r.red:
		return 0.5 * (depth - r.green) / (r.red - r.green)
	case sat > r.red:
		return 0.5 + 0.5*(depth-r.red)/(sat-r.red)
	default:
		// No saturation anchor above the red boundary: the red boundary is
		// as far as this axis is calibrated, so it saturates there.
		return 1
	}
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
// here rather than in the daemon so it shares regimeTapeConfirmable with the
// machine it mirrors — a second copy of that predicate is the drift this
// codebase has already paid for once.
//
// The term is recorded but does not yet reach a stage. Where its arm belongs
// is the one part of the model the enumeration could not settle: the shipped
// −4% trigger also wants two corroborating clusters, so an unconditional arm
// at the same level confirms stress on an empty board. Carrying the number
// now lets the corpus answer it.
func RegimeShadowTape(r RegimeSnapshotResult) float64 {
	if !regimeTapeConfirmable(r) || r.HYGSPYDivergence.SPYChangePct == nil {
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
		if _, known := regimeShadowRamps[in.Indicator]; !known {
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
			scored.Strength = regimeShadowStrength(in.Indicator, *in.Depth)
			scored.Zeroed = "currency"
		default:
			scored.Strength = regimeShadowStrength(in.Indicator, *in.Depth)
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
