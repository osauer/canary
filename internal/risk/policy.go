package risk

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// StressPolicyFingerprintVersion labels fingerprints of the stress threshold
// policy and keeps that identity domain separate from the constitution.
const StressPolicyFingerprintVersion = "stress-policy-fp-v1"

// Policy holds the shared stress thresholds used by live monitors and
// protection proposal policy.
type Policy struct {
	Name    string `json:"name"`
	Profile string `json:"profile"`
	Version string `json:"version"`

	MarginUrgentPct float64 `json:"margin_urgent_pct"`
	MarginActPct    float64 `json:"margin_act_pct"`
	MarginWatchPct  float64 `json:"margin_watch_pct"`
	MarginTargetPct float64 `json:"margin_target_pct"`

	GrossExposureWatchPct float64 `json:"gross_exposure_watch_pct"`
	NetDeltaWatchPct      float64 `json:"net_delta_watch_pct"`
	GrossDeltaWatchPct    float64 `json:"gross_delta_watch_pct"`

	GrossExposureStressActPct float64 `json:"gross_exposure_stress_act_pct"`
	NetDeltaStressActPct      float64 `json:"net_delta_stress_act_pct"`
	GrossDeltaStressActPct    float64 `json:"gross_delta_stress_act_pct"`

	GrossExposureStressUrgentPct float64 `json:"gross_exposure_stress_urgent_pct"`
	NetDeltaStressUrgentPct      float64 `json:"net_delta_stress_urgent_pct"`
	GrossDeltaStressUrgentPct    float64 `json:"gross_delta_stress_urgent_pct"`

	SingleNameExposureWatchPct float64 `json:"single_name_exposure_watch_pct"`
	SingleNameDeltaWatchPct    float64 `json:"single_name_delta_watch_pct"`
	SingleNameTargetPct        float64 `json:"single_name_target_pct"`

	OptionGreeksMinCoveragePct float64 `json:"option_greeks_min_coverage_pct"`

	SPYDropPct      float64 `json:"spy_drop_pct"`
	SPYHardDropPct  float64 `json:"spy_hard_drop_pct"`
	SPYCrashPct     float64 `json:"spy_crash_pct"`
	SPYRallyPct     float64 `json:"spy_rally_pct"`
	SPYHardRallyPct float64 `json:"spy_hard_rally_pct"`

	VIXSpikePct     float64 `json:"vix_spike_pct"`
	VIXHardSpikePct float64 `json:"vix_hard_spike_pct"`
	VIXCrushPct     float64 `json:"vix_crush_pct"`
	VIXHardCrushPct float64 `json:"vix_hard_crush_pct"`

	DailyPnLWatchPct float64 `json:"daily_pnl_watch_pct"`
	DailyPnLActPct   float64 `json:"daily_pnl_act_pct"`

	HeldStressMaterialPct             float64 `json:"held_stress_material_pct"`
	HeldUnderlyingPnLWatchPct         float64 `json:"held_underlying_pnl_watch_pct"`
	HeldUnderlyingPnLActPct           float64 `json:"held_underlying_pnl_act_pct"`
	HeldOptionNearDTE                 int     `json:"held_option_near_dte"`
	HeldOptionDeltaWatchPct           float64 `json:"held_option_delta_watch_pct"`
	HeldOptionDeltaActPct             float64 `json:"held_option_delta_act_pct"`
	HeldLiquidityStockSpreadPct       float64 `json:"held_liquidity_stock_spread_pct"`
	HeldLiquidityOptionSpreadPctOfMid float64 `json:"held_liquidity_option_spread_pct_of_mid"`

	Reduce ReducePolicy `json:"reduce"`
}

// ReducePolicy contains the option-selection and order-shape constraints used
// when constructing risk-reduction proposals. It does not authorize an order.
type ReducePolicy struct {
	FrontDTE                int     `json:"front_dte"`
	MidDTE                  int     `json:"mid_dte"`
	HedgeOffsetMinPct       float64 `json:"hedge_offset_min_pct"`
	HedgeOffsetMaxPct       float64 `json:"hedge_offset_max_pct"`
	MaxOptionSpreadAbs      float64 `json:"max_option_spread_abs"`
	MaxOptionSpreadPctOfMid float64 `json:"max_option_spread_pct_of_mid"`
	OrderType               string  `json:"order_type"`
	TIF                     string  `json:"tif"`
	AllowMarketOrders       bool    `json:"allow_market_orders"`
}

// DefaultPolicy returns the complete compiled stress policy.
func DefaultPolicy() Policy {
	return Policy{
		Name:    "active-v1",
		Profile: "active-v1",
		Version: "risk-policy-v1",

		MarginUrgentPct: 10,
		MarginActPct:    20,
		MarginWatchPct:  35,
		MarginTargetPct: 25,

		GrossExposureWatchPct: 150,
		NetDeltaWatchPct:      125,
		GrossDeltaWatchPct:    150,

		GrossExposureStressActPct: 100,
		NetDeltaStressActPct:      80,
		GrossDeltaStressActPct:    100,

		GrossExposureStressUrgentPct: 150,
		NetDeltaStressUrgentPct:      125,
		GrossDeltaStressUrgentPct:    150,

		SingleNameExposureWatchPct: 35,
		SingleNameDeltaWatchPct:    35,
		SingleNameTargetPct:        25,

		OptionGreeksMinCoveragePct: 80,

		SPYDropPct:      -1.5,
		SPYHardDropPct:  -2.5,
		SPYCrashPct:     -4,
		SPYRallyPct:     1.5,
		SPYHardRallyPct: 2.5,

		VIXSpikePct:     10,
		VIXHardSpikePct: 20,
		VIXCrushPct:     -10,
		VIXHardCrushPct: -20,

		DailyPnLWatchPct: 5,
		DailyPnLActPct:   10,

		HeldStressMaterialPct:             25,
		HeldUnderlyingPnLWatchPct:         2,
		HeldUnderlyingPnLActPct:           5,
		HeldOptionNearDTE:                 7,
		HeldOptionDeltaWatchPct:           25,
		HeldOptionDeltaActPct:             50,
		HeldLiquidityStockSpreadPct:       1,
		HeldLiquidityOptionSpreadPctOfMid: 25,

		Reduce: ReducePolicy{
			FrontDTE:                21,
			MidDTE:                  60,
			HedgeOffsetMinPct:       20,
			HedgeOffsetMaxPct:       120,
			MaxOptionSpreadAbs:      0.30,
			MaxOptionSpreadPctOfMid: 25,
			OrderType:               "LMT",
			TIF:                     "DAY",
			AllowMarketOrders:       false,
		},
	}
}

// PolicyProfile returns Profile, falling back to Name for older policy values.
func (p Policy) PolicyProfile() string {
	if p.Profile != "" {
		return p.Profile
	}
	return p.Name
}

// PolicyVersion returns the policy's version label.
func (p Policy) PolicyVersion() string {
	return p.Version
}

// FingerprintKey returns a deterministic digest of the policy profile,
// version, and every threshold field. Name is represented through the
// PolicyProfile fallback and is otherwise not part of the digest.
func (p Policy) FingerprintKey() string {
	projection := struct {
		Profile string       `json:"profile"`
		Version string       `json:"version"`
		Policy  policyFields `json:"policy"`
	}{
		Profile: p.PolicyProfile(),
		Version: p.PolicyVersion(),
		Policy: policyFields{
			MarginUrgentPct:                   p.MarginUrgentPct,
			MarginActPct:                      p.MarginActPct,
			MarginWatchPct:                    p.MarginWatchPct,
			MarginTargetPct:                   p.MarginTargetPct,
			GrossExposureWatchPct:             p.GrossExposureWatchPct,
			NetDeltaWatchPct:                  p.NetDeltaWatchPct,
			GrossDeltaWatchPct:                p.GrossDeltaWatchPct,
			GrossExposureStressActPct:         p.GrossExposureStressActPct,
			NetDeltaStressActPct:              p.NetDeltaStressActPct,
			GrossDeltaStressActPct:            p.GrossDeltaStressActPct,
			GrossExposureStressUrgentPct:      p.GrossExposureStressUrgentPct,
			NetDeltaStressUrgentPct:           p.NetDeltaStressUrgentPct,
			GrossDeltaStressUrgentPct:         p.GrossDeltaStressUrgentPct,
			SingleNameExposureWatchPct:        p.SingleNameExposureWatchPct,
			SingleNameDeltaWatchPct:           p.SingleNameDeltaWatchPct,
			SingleNameTargetPct:               p.SingleNameTargetPct,
			OptionGreeksMinCoveragePct:        p.OptionGreeksMinCoveragePct,
			SPYDropPct:                        p.SPYDropPct,
			SPYHardDropPct:                    p.SPYHardDropPct,
			SPYCrashPct:                       p.SPYCrashPct,
			SPYRallyPct:                       p.SPYRallyPct,
			SPYHardRallyPct:                   p.SPYHardRallyPct,
			VIXSpikePct:                       p.VIXSpikePct,
			VIXHardSpikePct:                   p.VIXHardSpikePct,
			VIXCrushPct:                       p.VIXCrushPct,
			VIXHardCrushPct:                   p.VIXHardCrushPct,
			DailyPnLWatchPct:                  p.DailyPnLWatchPct,
			DailyPnLActPct:                    p.DailyPnLActPct,
			HeldStressMaterialPct:             p.HeldStressMaterialPct,
			HeldUnderlyingPnLWatchPct:         p.HeldUnderlyingPnLWatchPct,
			HeldUnderlyingPnLActPct:           p.HeldUnderlyingPnLActPct,
			HeldOptionNearDTE:                 p.HeldOptionNearDTE,
			HeldOptionDeltaWatchPct:           p.HeldOptionDeltaWatchPct,
			HeldOptionDeltaActPct:             p.HeldOptionDeltaActPct,
			HeldLiquidityStockSpreadPct:       p.HeldLiquidityStockSpreadPct,
			HeldLiquidityOptionSpreadPctOfMid: p.HeldLiquidityOptionSpreadPctOfMid,
			Reduce:                            p.Reduce,
		},
	}
	raw, _ := json.Marshal(projection)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

type policyFields struct {
	MarginUrgentPct                   float64      `json:"margin_urgent_pct"`
	MarginActPct                      float64      `json:"margin_act_pct"`
	MarginWatchPct                    float64      `json:"margin_watch_pct"`
	MarginTargetPct                   float64      `json:"margin_target_pct"`
	GrossExposureWatchPct             float64      `json:"gross_exposure_watch_pct"`
	NetDeltaWatchPct                  float64      `json:"net_delta_watch_pct"`
	GrossDeltaWatchPct                float64      `json:"gross_delta_watch_pct"`
	GrossExposureStressActPct         float64      `json:"gross_exposure_stress_act_pct"`
	NetDeltaStressActPct              float64      `json:"net_delta_stress_act_pct"`
	GrossDeltaStressActPct            float64      `json:"gross_delta_stress_act_pct"`
	GrossExposureStressUrgentPct      float64      `json:"gross_exposure_stress_urgent_pct"`
	NetDeltaStressUrgentPct           float64      `json:"net_delta_stress_urgent_pct"`
	GrossDeltaStressUrgentPct         float64      `json:"gross_delta_stress_urgent_pct"`
	SingleNameExposureWatchPct        float64      `json:"single_name_exposure_watch_pct"`
	SingleNameDeltaWatchPct           float64      `json:"single_name_delta_watch_pct"`
	SingleNameTargetPct               float64      `json:"single_name_target_pct"`
	OptionGreeksMinCoveragePct        float64      `json:"option_greeks_min_coverage_pct"`
	SPYDropPct                        float64      `json:"spy_drop_pct"`
	SPYHardDropPct                    float64      `json:"spy_hard_drop_pct"`
	SPYCrashPct                       float64      `json:"spy_crash_pct"`
	SPYRallyPct                       float64      `json:"spy_rally_pct"`
	SPYHardRallyPct                   float64      `json:"spy_hard_rally_pct"`
	VIXSpikePct                       float64      `json:"vix_spike_pct"`
	VIXHardSpikePct                   float64      `json:"vix_hard_spike_pct"`
	VIXCrushPct                       float64      `json:"vix_crush_pct"`
	VIXHardCrushPct                   float64      `json:"vix_hard_crush_pct"`
	DailyPnLWatchPct                  float64      `json:"daily_pnl_watch_pct"`
	DailyPnLActPct                    float64      `json:"daily_pnl_act_pct"`
	HeldStressMaterialPct             float64      `json:"held_stress_material_pct"`
	HeldUnderlyingPnLWatchPct         float64      `json:"held_underlying_pnl_watch_pct"`
	HeldUnderlyingPnLActPct           float64      `json:"held_underlying_pnl_act_pct"`
	HeldOptionNearDTE                 int          `json:"held_option_near_dte"`
	HeldOptionDeltaWatchPct           float64      `json:"held_option_delta_watch_pct"`
	HeldOptionDeltaActPct             float64      `json:"held_option_delta_act_pct"`
	HeldLiquidityStockSpreadPct       float64      `json:"held_liquidity_stock_spread_pct"`
	HeldLiquidityOptionSpreadPctOfMid float64      `json:"held_liquidity_option_spread_pct_of_mid"`
	Reduce                            ReducePolicy `json:"reduce"`
}
