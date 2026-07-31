package rpc

import (
	"strconv"
	"strings"
)

// This file is the single copy of regime confirmation policy: eligibility
// gates, cluster combination with isolated-red downgrades, and headline
// wording. Daemon composite, lifecycle builder, CLI renderer, Stress, and
// the backtest builder all consume these functions or their served outputs.

// Indicator keys, shared with the daemon streak store and the eligibility
// gates table. Stable strings — they key persisted state.
const (
	RegimeIndicatorVIXTerm   = "vix_term"
	RegimeIndicatorVolOfVol  = "vol_of_vol"
	RegimeIndicatorHYGSPY    = "hyg_spy"
	RegimeIndicatorCredit    = "credit_spreads"
	RegimeIndicatorFunding   = "funding_stress"
	RegimeIndicatorUSDJPY    = "usdjpy"
	RegimeIndicatorGammaZero = "gamma_zero"
	RegimeIndicatorBreadth   = "breadth"
)

// Cluster indexes for the six-cluster combination. Order is part of the
// contract (lifecycle evidence and source-health rows iterate it).
const (
	RegimeClusterEquityVol = iota
	RegimeClusterCredit
	RegimeClusterFunding
	RegimeClusterFX
	RegimeClusterGamma
	RegimeClusterBreadth
	regimeClusterCount
)

// RegimeClusterNames are the wire names for the six clusters, indexed by the
// RegimeCluster* constants.
var RegimeClusterNames = []string{"vol", "credit", "funding", "fx", "gamma", "breadth"}

// RegimeVerdictFloor is the minimum ranked-cluster count required to claim a
// verdict above "insufficient signal".
const RegimeVerdictFloor = 3

// RegimeGate is one indicator's confirmation-eligibility policy. Depth units
// are per-indicator (documented at each table entry). A zero MinDepth means
// the red band threshold itself is the depth gate (already-deep bands).
// FastDepth, when non-zero, makes a red eligible on day one regardless of
// streak — the crash-day escape hatch that keeps persistence gates safe.
//
// These values are heuristic noise floors, pending_backtest like the band
// thresholds themselves. They stay code constants (not settings) until the
// decisions journal provides promotion evidence — user-tunable gates would
// fork the journal's comparability.
type RegimeGate struct {
	MinSessions int
	MinDepth    float64
	FastDepth   float64
}

// regimeGates is the per-indicator eligibility policy table from
// internal-docs/design/regime-calibration.md Part 1.
var regimeGates = map[string]RegimeGate{
	// depth = VIX/VIX3M ratio. Inversion is already discrete; fast path on a
	// deep day-one inversion.
	RegimeIndicatorVIXTerm: {MinSessions: 2, MinDepth: 1.00, FastDepth: 1.05},
	// depth = VVIX level. 120 keeps the existing isolated-VVIX rule's level.
	RegimeIndicatorVolOfVol: {MinSessions: 2, MinDepth: 110, FastDepth: 120},
	// depth = percent below the 50DMA ((dma-price)/dma*100). 0.25% is the
	// noise floor; a 1% break is eligible day one.
	RegimeIndicatorHYGSPY: {MinSessions: 2, MinDepth: 0.25, FastDepth: 1.0},
	// Official daily series: red levels are already deep, streak 1.
	RegimeIndicatorCredit:  {MinSessions: 1},
	RegimeIndicatorFunding: {MinSessions: 1},
	// Speed is the depth (>=2%/week yen strength), streak 1 by design —
	// August-2024 carry unwinds play out in three sessions.
	RegimeIndicatorUSDJPY: {MinSessions: 1},
	// depth = percent below gamma-zero (-gap_pct). Within 0.5% of the
	// crossing is transition noise; a wholly-short profile (no crossing)
	// passes as fast-path — callers encode that by passing a depth >=
	// FastDepth.
	RegimeIndicatorGammaZero: {MinSessions: 1, MinDepth: 0.5, FastDepth: 2.0},
	// depth = points below the 40% band floor (40 - pct_above_50dma).
	RegimeIndicatorBreadth: {MinSessions: 2, MinDepth: 2.0, FastDepth: 10.0},
}

// RegimeGateFor exposes the eligibility gate table for renderers (--explain)
// and the spec-doc generator. The bool reports whether the indicator is known.
func RegimeGateFor(indicator string) (RegimeGate, bool) {
	g, ok := regimeGates[indicator]
	return g, ok
}

// RegimeGammaDepth extracts gamma's eligibility depth in percent below
// gamma-zero (−gap). A wholly-short profile with no crossing is an extreme
// state — fast-path depth by construction. Combined scope averages the
// per-index gaps the weighted vote read.
func RegimeGammaDepth(c *GammaZeroComputed) *float64 {
	if c == nil {
		return nil
	}
	if c.Scope == GammaZeroScopeCombined && len(c.PerIndex) > 0 {
		var sum float64
		var count int
		for _, key := range []string{"SPY", "SPX"} {
			sub := c.PerIndex[key]
			if sub == nil || sub.GapPct == nil {
				continue
			}
			sum += *sub.GapPct
			count++
		}
		if count > 0 {
			d := -sum / float64(count)
			return &d
		}
	}
	if c.GapPct != nil {
		d := -*c.GapPct
		return &d
	}
	if c.GammaSign == "negative" {
		d := 100.0
		return &d
	}
	return nil
}

// RegimeIndicatorCluster maps an indicator key to its cluster wire name.
func RegimeIndicatorCluster(indicator string) string {
	switch indicator {
	case RegimeIndicatorVIXTerm, RegimeIndicatorVolOfVol:
		return "vol"
	case RegimeIndicatorHYGSPY, RegimeIndicatorCredit:
		return "credit"
	case RegimeIndicatorFunding:
		return "funding"
	case RegimeIndicatorUSDJPY:
		return "fx"
	case RegimeIndicatorGammaZero:
		return "gamma"
	case RegimeIndicatorBreadth:
		return "breadth"
	default:
		return ""
	}
}

// RegimeEligibilityInput is one red row's gate evidence. Depth is in the
// indicator's gate units; nil means the indicator has no separate depth
// metric (the band threshold is the depth gate). StreakSessions <= 0 is
// treated as 1 (fresh install / deleted store).
type RegimeEligibilityInput struct {
	Indicator      string
	Band           string
	Depth          *float64
	StreakSessions int
	Fresh          bool
	FreshnessClass string
	Latched        bool
}

// EvaluateRegimeEligibility applies the depth/persistence/freshness gates to
// one red row. Returns nil for non-red bands — eligibility is a property of
// red evidence only. The latch holds eligibility for the life of the red
// streak once earned, but never overrides freshness: overdue data drops
// eligibility mid-streak.
func EvaluateRegimeEligibility(in RegimeEligibilityInput) *RegimeEligibility {
	if strings.ToLower(strings.TrimSpace(in.Band)) != "red" {
		return nil
	}
	gate, ok := regimeGates[in.Indicator]
	if !ok {
		gate = RegimeGate{MinSessions: 1}
	}
	sessions := max(in.StreakSessions, 1)
	out := &RegimeEligibility{}
	if in.FreshnessClass == RegimeFreshnessNotDue {
		out.Reasons = append(out.Reasons, "data_not_due")
		return out
	}
	if !in.Fresh || in.FreshnessClass == RegimeFreshnessOverdue {
		out.Reasons = append(out.Reasons, "data_overdue")
		return out
	}
	if in.Latched {
		out.Eligible = true
		out.Latched = true
		return out
	}
	fastOK := gate.FastDepth > 0 && in.Depth != nil && *in.Depth >= gate.FastDepth
	depthOK := gate.MinDepth <= 0 || in.Depth == nil || *in.Depth >= gate.MinDepth
	switch {
	case fastOK:
		out.Eligible = true
	case !depthOK:
		out.Reasons = append(out.Reasons, "depth_below_min")
	case sessions < gate.MinSessions:
		out.Reasons = append(out.Reasons, streakReason(sessions, gate.MinSessions))
	default:
		out.Eligible = true
	}
	return out
}

func streakReason(sessions, want int) string {
	return "streak_" + strconv.Itoa(sessions) + "_of_" + strconv.Itoa(want)
}

// RegimeClusterBands is the shared cluster combination: Raw worst-of row
// bands per cluster, Confirmed after the isolated-red downgrades, and
// Eligible flagging clusters whose red evidence passed the confirmation
// gates. Eligible[i] is only meaningful where Raw[i] == "red".
type RegimeClusterBands struct {
	Raw       []string
	Confirmed []string
	Eligible  []bool
}

// EligibleRedCount counts clusters that survive downgrades as red AND carry
// eligible evidence — the only reds that may confirm stress.
func (b RegimeClusterBands) EligibleRedCount() int {
	n := 0
	for i, band := range b.Confirmed {
		if band == "red" && i < len(b.Eligible) && b.Eligible[i] {
			n++
		}
	}
	return n
}

// ProvisionalRedCount counts raw reds that may NOT confirm: either the row
// evidence failed the eligibility gates or the cluster was downgraded.
func (b RegimeClusterBands) ProvisionalRedCount() int {
	n := 0
	for i, band := range b.Raw {
		if band != "red" {
			continue
		}
		if i < len(b.Confirmed) && b.Confirmed[i] == "red" && i < len(b.Eligible) && b.Eligible[i] {
			continue
		}
		n++
	}
	return n
}

// BuildRegimeClusterBands combines served row bands into the six cluster
// bands. Row banding (classification + hysteresis) happens daemon-side once;
// every consumer of this function reads the served result. Independence
// rescue counts ELIGIBLE reds only — a marginal or stale red can no longer
// rescue another cluster from its isolated-red downgrade.
func BuildRegimeClusterBands(r *RegimeSnapshotResult) RegimeClusterBands {
	if r == nil {
		return RegimeClusterBands{}
	}
	raw := []string{
		strongestLifecycleBand(r.VIXTermStructure.Band, r.VolOfVol.Band),
		strongestLifecycleBand(r.HYGSPYDivergence.Band, r.CreditSpreads.Band),
		strongestLifecycleBand(r.FundingStress.Band),
		strongestLifecycleBand(r.USDJPY.Band),
		strongestLifecycleBand(rankableLifecycleGammaBand(r.GammaZero)),
		strongestLifecycleBand(r.Breadth.Band),
	}
	eligible := []bool{
		redEligible(r.VIXTermStructure.RegimeIndicatorMeta) || redEligible(r.VolOfVol.RegimeIndicatorMeta),
		redEligible(r.HYGSPYDivergence.RegimeIndicatorMeta) || redEligible(r.CreditSpreads.RegimeIndicatorMeta),
		redEligible(r.FundingStress.RegimeIndicatorMeta),
		redEligible(r.USDJPY.RegimeIndicatorMeta),
		gammaRedEligible(r.GammaZero),
		redEligible(r.Breadth.RegimeIndicatorMeta),
	}
	confirmed := append([]string(nil), raw...)
	if r.HYGSPYDivergence.Band == "red" && r.CreditSpreads.Band != "red" && !hasIndependentEligibleRed(raw, eligible, RegimeClusterCredit) {
		confirmed[RegimeClusterCredit] = "yellow"
	}
	if r.USDJPY.Band == "red" && !hasIndependentEligibleRed(raw, eligible, RegimeClusterFX) {
		confirmed[RegimeClusterFX] = "yellow"
	}
	if confirmed[RegimeClusterEquityVol] == "red" && !hasIndependentEligibleRed(confirmed, eligible, RegimeClusterEquityVol) && !isolatedLifecycleEquityVolConfirmed(*r) {
		confirmed[RegimeClusterEquityVol] = "yellow"
	}
	return RegimeClusterBands{Raw: raw, Confirmed: confirmed, Eligible: eligible}
}

func redEligible(meta RegimeIndicatorMeta) bool {
	return meta.Band == "red" && meta.Eligibility != nil && meta.Eligibility.Eligible
}

// gammaRedEligible additionally requires the rankability gate the gamma vote
// has always had — context_only/blocked/unavailable gamma is awareness
// evidence, not confirmation, regardless of its band.
func gammaRedEligible(g RegimeGammaZero) bool {
	return rankableLifecycleGammaBand(g) == "red" && g.Eligibility != nil && g.Eligibility.Eligible
}

func hasIndependentEligibleRed(bands []string, eligible []bool, self int) bool {
	for i, band := range bands {
		if i != self && band == "red" && i < len(eligible) && eligible[i] {
			return true
		}
	}
	return false
}

// ApplyRegimeClusterTallies fills the cluster-level counts on a composite
// from the shared combination — the daemon, the backtest builder, and tests
// all populate composites through this one function. Row-level counts
// (GreenCount etc.) remain the caller's concern; Verdict is set afterwards
// via RegimeHeadline once the lifecycle stage is known.
func ApplyRegimeClusterTallies(c *RegimeComposite, cb RegimeClusterBands) {
	if c == nil {
		return
	}
	c.ClusterGreenCount, c.ClusterYellowCount, c.ClusterRedCount = 0, 0, 0
	c.ClusterRankedCount, c.ClusterUnrankedCount = 0, 0
	for _, band := range cb.Confirmed {
		switch band {
		case "green":
			c.ClusterGreenCount++
			c.ClusterRankedCount++
		case "yellow":
			c.ClusterYellowCount++
			c.ClusterRankedCount++
		case "red":
			c.ClusterRedCount++
			c.ClusterRankedCount++
		default:
			c.ClusterUnrankedCount++
		}
	}
	c.ClusterEligibleRedCount = cb.EligibleRedCount()
	c.ClusterProvisionalRedCount = cb.ProvisionalRedCount()
}

// RegimeHeadline is the single wording table for the regime headline. Both
// composite.verdict and posture.label render this string; the CLI, MCP, SPA,
// and backtest all show the served value. First match wins.
func RegimeHeadline(c RegimeComposite, stage string) string {
	switch {
	case strings.EqualFold(strings.TrimSpace(stage), LifecycleDataQuality):
		return "Market state undefined — data incomplete"
	case c.ClusterRankedCount == 0:
		return "No usable signal yet"
	case c.ClusterRankedCount < RegimeVerdictFloor:
		return "Insufficient signal — too few inputs ready"
	case c.ClusterUnrankedCount == 0 && c.ClusterEligibleRedCount == c.ClusterRankedCount:
		return "Full risk-off conditions"
	case c.ClusterEligibleRedCount >= 3:
		return "Broad stress regime"
	case stageConfirmsStress(stage):
		return "Confirmed stress regime"
	case c.ClusterRedCount >= 1 || c.ClusterEligibleRedCount+c.ClusterProvisionalRedCount >= 1:
		return "Stress signal present"
	case c.ClusterYellowCount >= 3:
		return "Elevated stress watch"
	default:
		return "Normal regime"
	}
}

func stageConfirmsStress(stage string) bool {
	switch strings.ToLower(strings.TrimSpace(stage)) {
	case LifecycleConfirmedStress, LifecyclePanic:
		return true
	default:
		return false
	}
}

// GammaTransitionGapPct is the ± band, in percent of the zero-gamma level,
// inside which dealer positioning reads as transitional rather than long or
// short gamma. Single copy: the daemon gamma rows and every CLI renderer
// classify through GammaRegimeFromGap, and prose that names the band derives
// its number from this constant.
const GammaTransitionGapPct = 2.0

// GammaRegimeFromGap maps the signed spot-vs-zero-gamma gap (percent of the
// zero-gamma level, positive = spot above) to its wire regime label. A nil
// gap — no measurable crossing — is transitional: without a gap the
// classifier must not claim direction.
func GammaRegimeFromGap(gapPct *float64) string {
	if gapPct == nil {
		return "transition_gamma"
	}
	switch {
	case *gapPct > GammaTransitionGapPct:
		return "long_gamma"
	case *gapPct >= -GammaTransitionGapPct:
		return "transition_gamma"
	default:
		return "short_gamma"
	}
}

// GammaBucketRegime classifies one horizon bucket (0DTE / 1-7 / term) from
// its zero-gamma level and profile sign. With a usable crossing the gap
// classifies through GammaRegimeFromGap; without one the swept profile's
// sign decides, and an unknown sign yields "" (bucket unavailable).
func GammaBucketRegime(spot float64, zero *float64, sign string) string {
	if zero != nil && *zero > 0 {
		gap := (spot - *zero) / *zero * 100
		return GammaRegimeFromGap(&gap)
	}
	switch sign {
	case "positive":
		return "long_gamma"
	case "negative":
		return "short_gamma"
	}
	return ""
}

// HeuristicThresholds builds the heuristic/pending-backtest threshold
// metadata shared by the daemon regime rows and the backtest builder. The
// Heuristic and PendingBacktest bits are policy: they mark bands whose
// values have not yet earned promotion through the decisions journal. Trip is
// the compact display form of red and is authored here beside it, so a gauge
// face can print a trigger without any renderer parsing the worded band.
func HeuristicThresholds(label, green, yellow, red, trip string) *RegimeThresholds {
	return &RegimeThresholds{
		Label:           label,
		Green:           green,
		Yellow:          yellow,
		Red:             red,
		Trip:            trip,
		Heuristic:       true,
		PendingBacktest: true,
	}
}
