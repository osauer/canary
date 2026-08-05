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

// RegimeCurrencyPolicyVersion identifies the input-currency policy a decision
// event was produced under (internal-docs/design/regime-input-currency.md).
// Behaviour changes in how inputs report currency alter the daily fingerprint
// sequence, so the calibration corpus has to be partitionable: a backtest must
// never blend days either side of a cutover. Bump on every change to how a
// class is assigned or consumed.
const RegimeCurrencyPolicyVersion = "regime-currency-v1"

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
//
// Re-calibrating FastDepth. It is the saturation point — where an indicator
// stops getting meaningfully worse — and it is derived, not chosen:
//
//	FastDepth = red boundary + 0.6 × (yellow band width)
//
// The rule reproduces the three anchors that predate it: vix_term
// 1.00+0.6(0.08)=1.048 vs 1.05, vol_of_vol 110+0.6(20)=122 vs 120, breadth
// 40−0.6(15)=31 vs 30. Derive any new indicator the same way and record
// deviations at the row, as usdjpy does.
//
// None of these are fitted values, and the decisions journal cannot make them
// so: it samples a calm tape in which credit, funding, gamma and breadth have
// never left green. Move them only against a corpus that actually contains
// stress. A change to the band boundaries alone is not a reason to move them,
// because the rule already tracks the bands.
//
// MinDepth and MinSessions are separate questions the rule does not speak to.
var regimeGates = map[string]RegimeGate{
	// depth = VIX/VIX3M ratio. Inversion is already discrete; fast path on a
	// deep day-one inversion.
	RegimeIndicatorVIXTerm: {MinSessions: 2, MinDepth: 1.00, FastDepth: 1.05},
	// depth = VVIX level. 120 keeps the existing isolated-VVIX rule's level.
	RegimeIndicatorVolOfVol: {MinSessions: 2, MinDepth: 110, FastDepth: 120},
	// depth = percent below the 50DMA ((dma-price)/dma*100). 0.25% is the
	// noise floor; a 1% break is eligible day one.
	RegimeIndicatorHYGSPY: {MinSessions: 2, MinDepth: 0.25, FastDepth: 1.0},
	// depth = HY OAS percent. The red band itself stays the gate: the band's
	// second trigger (20-day widening ≥1.0pp) can go red near 4%, so a level
	// floor would silently suppress it. 6.5 is the derived saturation — past
	// the 2022 peak, well short of 2020's ~11%.
	RegimeIndicatorCredit: {MinSessions: 1, FastDepth: 6.5},
	// depth = CP−bill spread in bp. Red levels are already deep, streak 1.
	// 105 is the derived saturation; March 2020 reached roughly twice it.
	RegimeIndicatorFunding: {MinSessions: 1, FastDepth: 105},
	// depth = weekly yen strengthening in percent (−WeeklyChange). Speed is
	// the depth (≥2%/week), streak 1 by design — August-2024 carry unwinds
	// play out in three sessions. The one deviation from the rule: it yields
	// 2.6, but a calm fortnight of journal already printed 4.58%/week, so the
	// scale would saturate on ordinary weeks. 7.0 is the August-2024 unwind
	// this indicator exists to catch.
	RegimeIndicatorUSDJPY: {MinSessions: 1, FastDepth: 7.0},
	// depth = percent below gamma-zero (−gap_pct); a wholly-short profile
	// with no crossing reports 100. On a single-index red neither gate can
	// fire — red already requires depth > 2.0, above MinDepth, and
	// MinSessions is 1, so a fresh gamma red confirms same-day on the
	// default branch. That is intended: a dealer gamma flip is a fast
	// amplifier and holding it a session defeats the signal. MinDepth does
	// decide on the combined row, whose gamma-weighted gap can land
	// anywhere between the two indexes.
	RegimeIndicatorGammaZero: {MinSessions: 1, MinDepth: 0.5, FastDepth: 4.5},
	// depth = points below the 40% band floor (40 - pct_above_50dma).
	RegimeIndicatorBreadth: {MinSessions: 2, MinDepth: 2.0, FastDepth: 10.0},
}

// RegimeGateFor exposes the eligibility gate table for renderers (--explain)
// and the spec-doc generator. The bool reports whether the indicator is known.
func RegimeGateFor(indicator string) (RegimeGate, bool) {
	g, ok := regimeGates[indicator]
	return g, ok
}

// GammaIndexWeight is the weight one index carries in the combined SPY+SPX
// row: its gross gamma exposure, or a scale-ratio stand-in when a leg
// reports none. Single copy, because the band vote and the eligibility depth
// have to weigh the two indexes the same way — weighing them differently
// lets a red banded on the dominant index be refused for depth the other
// index diluted (internal-docs/design/regime-calibration.md, gamma path (c)).
func GammaIndexWeight(key string, c *GammaZeroComputed) float64 {
	if c != nil && c.GammaTotalAbs > 0 {
		return c.GammaTotalAbs
	}
	if key == "SPX" {
		return 100
	}
	return 1
}

// GammaCombinedGapPct is the gamma-weighted mean of the per-index gaps on a
// combined-scope result. nil when the result is not combined scope or no
// index reports a crossing.
func GammaCombinedGapPct(c *GammaZeroComputed) *float64 {
	if c == nil || c.Scope != GammaZeroScopeCombined || len(c.PerIndex) == 0 {
		return nil
	}
	var sum, weight float64
	for _, key := range []string{"SPY", "SPX"} {
		sub := c.PerIndex[key]
		if sub == nil || sub.GapPct == nil {
			continue
		}
		w := GammaIndexWeight(key, sub)
		sum += *sub.GapPct * w
		weight += w
	}
	if weight <= 0 {
		return nil
	}
	gap := sum / weight
	return &gap
}

// RegimeGammaDepth extracts gamma's eligibility depth in percent below
// gamma-zero (−gap). A wholly-short profile with no crossing is an extreme
// state — fast-path depth by construction. Combined scope reads the same
// gamma-weighted gap the band vote read.
func RegimeGammaDepth(c *GammaZeroComputed) *float64 {
	if c == nil {
		return nil
	}
	if gap := GammaCombinedGapPct(c); gap != nil {
		d := -*gap
		return &d
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
	// Currency is an allowlist on fresh, never a denylist of known-bad classes:
	// a class added later must fail closed until its authority is decided
	// (internal-docs/design/regime-input-currency.md).
	if !in.Fresh || !RegimeCurrencyMayConfirm(in.FreshnessClass) {
		out.Reasons = append(out.Reasons, regimeEligibilityCurrencyReason(in.FreshnessClass))
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

// regimeEligibilityCurrencyReason names the currency that blocked confirmation.
// The reason tokens are the stable vocabulary renderers and the decisions
// journal already carry; a class with no token of its own reports data_overdue,
// which is what it costs the row.
func regimeEligibilityCurrencyReason(class string) string {
	switch strings.ToLower(strings.TrimSpace(class)) {
	case RegimeFreshnessNotDue:
		return "data_not_due"
	case RegimeFreshnessPending:
		return "data_refresh_pending"
	case RegimeFreshnessStale:
		return "data_stale"
	default:
		return "data_overdue"
	}
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

// regimeThresholdText is one indicator's published band prose. Label is the
// threshold-set version identity, persisted as
// regime_indicators.thresholds_label so the calibration corpus can be
// partitioned by the values a decision was produced under — not a display
// name, which every renderer supplies for itself.
type regimeThresholdText struct{ label, green, yellow, red, trip string }

// regimeThresholdTexts is the single source for published band prose.
//
// It was two hand-maintained copies, and they drifted: the backtest builder
// published four boundaries as strict (>1.00, >110, >75 bp, >2%) where the
// classifiers are inclusive, so a reading exactly on the line was documented
// as the band below the one it actually lands in. It also passed a display
// string as the label, which left replayed rows unpartitionable by
// threshold-set version.
//
// Prose is prose and cannot be proven against the code by the compiler. When a
// classifier boundary moves, this table moves with it — regimelab's boundary
// probe bisects each classifier and compares the parsed claims here against
// what the code does, which is what caught the drift.
var regimeThresholdTexts = map[string]regimeThresholdText{
	RegimeIndicatorVIXTerm:   {"vix_term_structure_v1", "VIX/VIX3M < 0.92", "0.92 <= VIX/VIX3M < 1.00", "VIX/VIX3M >= 1.00", "trips >=1.00"},
	RegimeIndicatorVolOfVol:  {"vvix_daily_v1", "VVIX < 90", "90 <= VVIX < 110", "VVIX >= 110", "trips >=110"},
	RegimeIndicatorHYGSPY:    {"hyg_spy_credit_proxy_v1", "HYG >= 50-day SMA", "HYG < 50-day SMA", "HYG < 50-day SMA and SPY >= 97% of 52-week high", "trips HYG <50dma with SPY >=97% of 52w high"},
	RegimeIndicatorCredit:    {"hy_ig_oas_v1", "HY OAS < 4.0 and 20d widening < 0.50 pp", "HY OAS 4.0-5.5 or 20d widening >= 0.50 pp", "HY OAS >= 5.5 or 20d widening >= 1.00 pp", "trips HY OAS >=5.5"},
	RegimeIndicatorFunding:   {"funding_cp_tbill_v1", "CP/T-bill spread < 25 bp", "25 <= spread < 75 bp", "spread >= 75 bp", "trips >=75 bp"},
	RegimeIndicatorUSDJPY:    {"usd_jpy_carry_proxy_v1", "yen strengthening < 1% over the week", "yen strengthening 1-2% over the week", "yen strengthening >= 2% over the week", "trips yen +2%/week"},
	RegimeIndicatorGammaZero: {"dealer_gamma_v3", "spot > 2% above gamma-zero or profile wholly long-gamma", "spot within +/-2% of gamma-zero or mixed gamma profile", "spot > 2% below gamma-zero, profile wholly short-gamma, or dominant/equal exposure is amplifying", "trips spot >2% below gamma-zero"},
	RegimeIndicatorBreadth:   {"spx_breadth_50dma_v1", "SPX members above 50-DMA > 55%", "40% <= members above 50-DMA <= 55%", "members above 50-DMA < 40%", "trips <40% (50d)"},
}

// RegimeThresholdsFor returns the published band prose for an indicator, or
// nil when the indicator is unknown. Each call builds a fresh value: the
// result is embedded per-snapshot and must not be shared across them.
func RegimeThresholdsFor(indicator string) *RegimeThresholds {
	t, ok := regimeThresholdTexts[indicator]
	if !ok {
		return nil
	}
	return HeuristicThresholds(t.label, t.green, t.yellow, t.red, t.trip)
}
