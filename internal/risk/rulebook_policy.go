package risk

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"sort"
	"strings"
)

// RegimeBucketCalm and the related constants are normalized regime buckets
// consumed by regime-conditional rules. The evaluator does not accept raw
// lifecycle stage names.
const (
	RegimeBucketCalm         = "calm"
	RegimeBucketEarlyWarning = "early_warning"
	RegimeBucketConfirmed    = "confirmed"
)

// RegimeThresholds is one stage's threshold set for the regime-conditional
// rules: rule 4 extrinsic budget (ex-protection), rule 12 protection band.
// hedge band.
type RegimeThresholds struct {
	CashSellOnlyPct   float64 `toml:"cash_sell_only_pct" json:"cash_sell_only_pct"`
	ExtrinsicWatchPct float64 `toml:"extrinsic_watch_pct" json:"extrinsic_watch_pct"`
	ExtrinsicActPct   float64 `toml:"extrinsic_act_pct" json:"extrinsic_act_pct"`
	HedgeBandMinPct   float64 `toml:"hedge_band_min_pct" json:"hedge_band_min_pct"`
	HedgeBandMaxPct   float64 `toml:"hedge_band_max_pct" json:"hedge_band_max_pct"`
}

// RulebookPolicy carries the compiled thresholds for the daily trading
// rulebook. The TOML tags reserve the planned operator policy loader; no such
// loader is shipped today. It owns rulebook verdict thresholds, not the source
// observations that callers map into RuleInputs.
type RulebookPolicy struct {
	ID      string `toml:"id" json:"id"`
	Version int    `toml:"version" json:"version"`
	// Modes controls whether each hard-coded rule is off, visible for tracking,
	// or allowed to create alert episodes.
	Modes map[string]string `toml:"modes" json:"modes"`

	// Rule 1 — single_name_exposure (% of NLV, delta-dollar exposure).
	SingleNameWatchPct float64 `toml:"single_name_watch_pct" json:"single_name_watch_pct"`
	SingleNameActPct   float64 `toml:"single_name_act_pct" json:"single_name_act_pct"`

	// Rule 3 — minimum broker-reported available funds as a share of NLV.
	CashReserveMinPct float64 `toml:"cash_reserve_min_pct" json:"cash_reserve_min_pct"`

	// Rule 2 — option_line_premium (% of NLV, long option line market value).
	// Hedge-classified legs (rule12HedgeLeg) evaluate against the hedge tier;
	// rule 12 owns hedge sizing, this tier only bounds premium at risk.
	OptionLineWatchPct float64 `toml:"option_line_watch_pct" json:"option_line_watch_pct"`
	OptionLineActPct   float64 `toml:"option_line_act_pct" json:"option_line_act_pct"`
	HedgeLineWatchPct  float64 `toml:"hedge_line_watch_pct" json:"hedge_line_watch_pct"`
	HedgeLineActPct    float64 `toml:"hedge_line_act_pct" json:"hedge_line_act_pct"`

	// Rule 5 — expiry_runway (calendar DTE bounds for long options).
	RunwayWatchDTE      int     `toml:"runway_watch_dte" json:"runway_watch_dte"`
	RunwayActDTE        int     `toml:"runway_act_dte" json:"runway_act_dte"`
	RunwayITMDeltaFloor float64 `toml:"runway_itm_delta_floor" json:"runway_itm_delta_floor"`

	// Rule 7 — overwrite_earnings short-put act tier: a spanning short put
	// escalates from watch to act when its assignment notional reaches the
	// line share of NLV, or a name's spanning short puts together reach the
	// name share.
	ShortPutActLinePctNLV float64 `toml:"short_put_act_line_pct_nlv" json:"short_put_act_line_pct_nlv"`
	ShortPutActNamePctNLV float64 `toml:"short_put_act_name_pct_nlv" json:"short_put_act_name_pct_nlv"`

	// Rule 8 — earnings_size_freeze (US sessions to earnings).
	EarningsFreezeSessions int `toml:"earnings_freeze_sessions" json:"earnings_freeze_sessions"`

	// Rules 9/10 — tape thresholds (day-change %).
	RedOnGreenNameDropPct float64 `toml:"red_on_green_name_drop_pct" json:"red_on_green_name_drop_pct"`
	RedOnGreenSPYUpPct    float64 `toml:"red_on_green_spy_up_pct" json:"red_on_green_spy_up_pct"`
	WinnerTrimDayUpPct    float64 `toml:"winner_trim_day_up_pct" json:"winner_trim_day_up_pct"`
	WinnerTrimMinExpoPct  float64 `toml:"winner_trim_min_exposure_pct" json:"winner_trim_min_exposure_pct"`

	// Rules 4/12 — regime-conditional threshold sets. A fresh regime stage
	// selects its set; a carried or never-seen stage evaluates the carried set
	// AND the calm set
	// and keeps the worse verdict, so stale regime data can hold or tighten
	// a verdict but never relax it.
	RegimeCalm         RegimeThresholds `toml:"regime_calm" json:"regime_calm"`
	RegimeEarlyWarning RegimeThresholds `toml:"regime_early_warning" json:"regime_early_warning"`
	RegimeConfirmed    RegimeThresholds `toml:"regime_confirmed" json:"regime_confirmed"`
	// RegimeStageMaxAgeMinutes bounds trust in the latched regime stage;
	// older stages evaluate as carried (worse-of semantics above).
	RegimeStageMaxAgeMinutes int `toml:"regime_stage_max_age_minutes" json:"regime_stage_max_age_minutes"`

	// Rule 13 — exit_discipline (% of premium paid lost on a long line).
	ExitWatchLossPct float64 `toml:"exit_watch_loss_pct" json:"exit_watch_loss_pct"`
	ExitActLossPct   float64 `toml:"exit_act_loss_pct" json:"exit_act_loss_pct"`

	// Rule 14 — fx_exposure (% of NLV held in non-base currencies). This is a
	// watch-only structural condition.
	FXExposureWatchPct float64 `toml:"fx_exposure_watch_pct" json:"fx_exposure_watch_pct"`

	// HedgeSymbols is the policy-owned index list whose long puts classify
	// as hedges (rules 1, 2, 5, 12, 13). Uppercased on load.
	HedgeSymbols []string `toml:"hedge_symbols" json:"hedge_symbols"`

	// GreeksGapFloorPctNLV is the materiality floor: a name whose legs
	// missing delta exceed this notional share of NLV renders its
	// exposure-dependent rows unknown instead of silently understating.
	GreeksGapFloorPctNLV float64 `toml:"greeks_gap_floor_pct_nlv" json:"greeks_gap_floor_pct_nlv"`

	// EarningsStaleDays bounds trust in a fetched earnings date; older
	// observations flip rules 6-8 to unknown until refreshed or overridden.
	EarningsStaleDays int `toml:"earnings_stale_days" json:"earnings_stale_days"`
}

// DefaultRulebookPolicy returns a complete baseline policy.
func DefaultRulebookPolicy() RulebookPolicy {
	return RulebookPolicy{
		ID:      "rulebook-v2",
		Version: 2,
		Modes: map[string]string{
			RuleSingleNameExposure: RuleModeAlert,
			RuleOptionLinePremium:  RuleModeTrack,
			RuleCashSellOnly:       RuleModeAlert,
			RuleExtrinsicBudget:    RuleModeAlert,
			RuleExpiryRunway:       RuleModeAlert,
			RuleCatalystCoverage:   RuleModeTrack,
			RuleOverwriteEarnings:  RuleModeAlert,
			RuleEarningsSizeFreeze: RuleModeTrack,
			RuleRedOnGreen:         RuleModeOff,
			RuleWinnerTrim:         RuleModeOff,
			RuleGreenDayAction:     RuleModeOff,
			RuleHedgeIntegrity:     RuleModeAlert,
			RuleExitDiscipline:     RuleModeAlert,
			RuleFXExposure:         RuleModeTrack,
		},
		SingleNameWatchPct:     30,
		SingleNameActPct:       40,
		CashReserveMinPct:      75,
		OptionLineWatchPct:     5,
		OptionLineActPct:       10,
		HedgeLineWatchPct:      15,
		HedgeLineActPct:        25,
		RunwayWatchDTE:         14,
		RunwayActDTE:           7,
		RunwayITMDeltaFloor:    0.70,
		ShortPutActLinePctNLV:  10,
		ShortPutActNamePctNLV:  20,
		EarningsFreezeSessions: 3,
		RedOnGreenNameDropPct:  -1.5,
		RedOnGreenSPYUpPct:     0.5,
		WinnerTrimDayUpPct:     4,
		WinnerTrimMinExpoPct:   15,
		RegimeCalm: RegimeThresholds{
			CashSellOnlyPct:   -25,
			ExtrinsicWatchPct: 10,
			ExtrinsicActPct:   15,
			HedgeBandMinPct:   25,
			HedgeBandMaxPct:   35,
		},
		RegimeEarlyWarning: RegimeThresholds{
			CashSellOnlyPct:   0,
			ExtrinsicWatchPct: 7.5,
			ExtrinsicActPct:   12,
			HedgeBandMinPct:   30,
			HedgeBandMaxPct:   50,
		},
		RegimeConfirmed: RegimeThresholds{
			CashSellOnlyPct:   10,
			ExtrinsicWatchPct: 5,
			ExtrinsicActPct:   10,
			HedgeBandMinPct:   40,
			HedgeBandMaxPct:   70,
		},
		RegimeStageMaxAgeMinutes: 240,
		ExitWatchLossPct:         40,
		ExitActLossPct:           60,
		FXExposureWatchPct:       60,
		HedgeSymbols:             []string{"SPY", "SPX", "SPXW", "QQQ", "IWM"},
		GreeksGapFloorPctNLV:     1,
		EarningsStaleDays:        10,
	}
}

// SetForBucket returns the threshold set for a regime bucket; unrecognized
// buckets fall to the early-warning set (middle, disclosed by the caller),
// never silently to calm.
func (p RulebookPolicy) SetForBucket(bucket string) RegimeThresholds {
	switch bucket {
	case RegimeBucketCalm:
		return p.RegimeCalm
	case RegimeBucketConfirmed:
		return p.RegimeConfirmed
	default:
		return p.RegimeEarlyWarning
	}
}

// Normalize uppercases and sorts the hedge list so fingerprints are stable
// regardless of TOML ordering.
func (p *RulebookPolicy) Normalize() {
	for i, s := range p.HedgeSymbols {
		p.HedgeSymbols[i] = strings.ToUpper(strings.TrimSpace(s))
	}
	sort.Strings(p.HedgeSymbols)
	if p.Modes == nil {
		p.Modes = map[string]string{}
	}
}

// ModeFor returns one rule's notification mode. Missing entries retain the
// historical alert behavior for compatibility with older policy fixtures.
func (p RulebookPolicy) ModeFor(id string) string {
	switch p.Modes[id] {
	case RuleModeOff, RuleModeTrack, RuleModeAlert:
		return p.Modes[id]
	default:
		return RuleModeAlert
	}
}

// FingerprintKey hashes the full policy so every result discloses exactly
// which thresholds produced it (mirrors risk.Policy.FingerprintKey). Every
// field must appear here: a threshold outside the fingerprint is a silent
// policy change.
func (p RulebookPolicy) FingerprintKey() string {
	q := p
	q.HedgeSymbols = slices.Clone(p.HedgeSymbols)
	q.Normalize()
	projection := struct {
		ID                       string            `json:"id"`
		Version                  int               `json:"version"`
		Modes                    map[string]string `json:"modes"`
		SingleNameWatchPct       float64           `json:"single_name_watch_pct"`
		SingleNameActPct         float64           `json:"single_name_act_pct"`
		CashReserveMinPct        float64           `json:"cash_reserve_min_pct"`
		OptionLineWatchPct       float64           `json:"option_line_watch_pct"`
		OptionLineActPct         float64           `json:"option_line_act_pct"`
		HedgeLineWatchPct        float64           `json:"hedge_line_watch_pct"`
		HedgeLineActPct          float64           `json:"hedge_line_act_pct"`
		RunwayWatchDTE           int               `json:"runway_watch_dte"`
		RunwayActDTE             int               `json:"runway_act_dte"`
		RunwayITMDeltaFloor      float64           `json:"runway_itm_delta_floor"`
		ShortPutActLinePctNLV    float64           `json:"short_put_act_line_pct_nlv"`
		ShortPutActNamePctNLV    float64           `json:"short_put_act_name_pct_nlv"`
		EarningsFreezeSessions   int               `json:"earnings_freeze_sessions"`
		RedOnGreenNameDropPct    float64           `json:"red_on_green_name_drop_pct"`
		RedOnGreenSPYUpPct       float64           `json:"red_on_green_spy_up_pct"`
		WinnerTrimDayUpPct       float64           `json:"winner_trim_day_up_pct"`
		WinnerTrimMinExpoPct     float64           `json:"winner_trim_min_exposure_pct"`
		RegimeCalm               RegimeThresholds  `json:"regime_calm"`
		RegimeEarlyWarning       RegimeThresholds  `json:"regime_early_warning"`
		RegimeConfirmed          RegimeThresholds  `json:"regime_confirmed"`
		RegimeStageMaxAgeMinutes int               `json:"regime_stage_max_age_minutes"`
		ExitWatchLossPct         float64           `json:"exit_watch_loss_pct"`
		ExitActLossPct           float64           `json:"exit_act_loss_pct"`
		FXExposureWatchPct       float64           `json:"fx_exposure_watch_pct"`
		HedgeSymbols             []string          `json:"hedge_symbols"`
		GreeksGapFloorPctNLV     float64           `json:"greeks_gap_floor_pct_nlv"`
		EarningsStaleDays        int               `json:"earnings_stale_days"`
	}{
		ID:                       q.ID,
		Version:                  q.Version,
		Modes:                    q.Modes,
		SingleNameWatchPct:       q.SingleNameWatchPct,
		SingleNameActPct:         q.SingleNameActPct,
		CashReserveMinPct:        q.CashReserveMinPct,
		OptionLineWatchPct:       q.OptionLineWatchPct,
		OptionLineActPct:         q.OptionLineActPct,
		HedgeLineWatchPct:        q.HedgeLineWatchPct,
		HedgeLineActPct:          q.HedgeLineActPct,
		RunwayWatchDTE:           q.RunwayWatchDTE,
		RunwayActDTE:             q.RunwayActDTE,
		RunwayITMDeltaFloor:      q.RunwayITMDeltaFloor,
		ShortPutActLinePctNLV:    q.ShortPutActLinePctNLV,
		ShortPutActNamePctNLV:    q.ShortPutActNamePctNLV,
		EarningsFreezeSessions:   q.EarningsFreezeSessions,
		RedOnGreenNameDropPct:    q.RedOnGreenNameDropPct,
		RedOnGreenSPYUpPct:       q.RedOnGreenSPYUpPct,
		WinnerTrimDayUpPct:       q.WinnerTrimDayUpPct,
		WinnerTrimMinExpoPct:     q.WinnerTrimMinExpoPct,
		RegimeCalm:               q.RegimeCalm,
		RegimeEarlyWarning:       q.RegimeEarlyWarning,
		RegimeConfirmed:          q.RegimeConfirmed,
		RegimeStageMaxAgeMinutes: q.RegimeStageMaxAgeMinutes,
		ExitWatchLossPct:         q.ExitWatchLossPct,
		ExitActLossPct:           q.ExitActLossPct,
		FXExposureWatchPct:       q.FXExposureWatchPct,
		HedgeSymbols:             q.HedgeSymbols,
		GreeksGapFloorPctNLV:     q.GreeksGapFloorPctNLV,
		EarningsStaleDays:        q.EarningsStaleDays,
	}
	raw, _ := json.Marshal(projection)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// IsHedgeSymbol reports whether sym is on the policy hedge list.
func (p RulebookPolicy) IsHedgeSymbol(sym string) bool {
	sym = strings.ToUpper(strings.TrimSpace(sym))
	return slices.Contains(p.HedgeSymbols, sym)
}
