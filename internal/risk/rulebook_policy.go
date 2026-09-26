package risk

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
)

// RulebookPolicyKind identifies an owner Rulebook policy file.
const RulebookPolicyKind = "ibkr.rulebook_policy"

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
	// ExtrinsicWatchPct is rule 4's watch level: option time value outside protection as a percent of NLV.
	ExtrinsicWatchPct float64 `toml:"extrinsic_watch_pct" json:"extrinsic_watch_pct"`
	// ExtrinsicActPct is rule 4's act level: option time value outside protection as a percent of NLV.
	ExtrinsicActPct float64 `toml:"extrinsic_act_pct" json:"extrinsic_act_pct"`
	// HedgeBandMinPct is rule 12's lower bound: index protection as a percent of gross long exposure.
	HedgeBandMinPct float64 `toml:"hedge_band_min_pct" json:"hedge_band_min_pct"`
	// HedgeBandMaxPct is rule 12's upper bound: index protection as a percent of gross long exposure.
	HedgeBandMaxPct float64 `toml:"hedge_band_max_pct" json:"hedge_band_max_pct"`
}

// RulebookPolicy carries the Rulebook's verdict thresholds and rule modes.
// DefaultRulebookPolicy is the compiled baseline; the owner's
// rulebook-policy.toml overrides any subset of it, and absent keys keep the
// baseline. It owns rulebook verdict thresholds, not the source observations
// that callers map into RuleInputs.
type RulebookPolicy struct {
	// Kind identifies the file (ibkr.rulebook_policy); optional, never part of the fingerprint.
	Kind string `toml:"kind" json:"-"`
	// SchemaVersion is the file schema (1); optional, never part of the fingerprint.
	SchemaVersion int `toml:"schema_version" json:"-"`
	// ID names the policy (baseline rulebook-v3; canary rules policy set writes rulebook-owner).
	ID string `toml:"policy_id" json:"id"`
	// Version must rise for an edit to take effect; the first file is adopted at any version.
	Version int `toml:"policy_version" json:"version"`
	// Modes sets each rule to off (not evaluated), track (shown, never alerts) or alert, keyed by rule id.
	Modes map[string]string `toml:"modes" json:"modes"`

	// SingleNameWatchPct is rule 1's watch level: the worst-case loss on one issuer, every leg netted, as a percent of NLV; the risk-reduction trim goes back to it.
	SingleNameWatchPct float64 `toml:"single_name_watch_pct" json:"single_name_watch_pct"`
	// SingleNameActPct is rule 1's act level for one issuer's worst-case loss; the risk-reduction bucket proposes a trim from here.
	SingleNameActPct float64 `toml:"single_name_act_pct" json:"single_name_act_pct"`
	// TakeoverGapPct sizes legs that lose without limit as the price rises (short stock, uncovered short calls): they are measured at a rise of this percent.
	TakeoverGapPct float64 `toml:"takeover_gap_pct" json:"takeover_gap_pct"`
	// HedgeMinDays is the fewest days to expiry at which a long option counts as protection; it must also expire after the issuer's next earnings.
	HedgeMinDays int `toml:"hedge_min_days" json:"hedge_min_days"`
	// ExitParticipationPct is the share of 20-day average daily volume one exit may take when rule 1 measures days to exit.
	ExitParticipationPct float64 `toml:"exit_participation_pct" json:"exit_participation_pct"`
	// IlliquidDaysToExit: an issuer that needs more days than this to exit is measured against the illiquid bands.
	IlliquidDaysToExit float64 `toml:"illiquid_days_to_exit" json:"illiquid_days_to_exit"`
	// IlliquidWatchPct is rule 1's watch level for an illiquid issuer.
	IlliquidWatchPct float64 `toml:"illiquid_watch_pct" json:"illiquid_watch_pct"`
	// IlliquidActPct is rule 1's act level for an illiquid issuer.
	IlliquidActPct float64 `toml:"illiquid_act_pct" json:"illiquid_act_pct"`
	// DeltaSwingWatchPct is rule 16's watch level: one issuer's dollar delta as a percent of NLV. It never acts.
	DeltaSwingWatchPct float64 `toml:"delta_swing_watch_pct" json:"delta_swing_watch_pct"`
	// ClusterDropPct is rule 17's scenario: every issuer in a declared cluster falls this percent together.
	ClusterDropPct float64 `toml:"cluster_drop_pct" json:"cluster_drop_pct"`
	// ClusterWatchPct is rule 17's watch level: the cluster's loss in that fall as a percent of NLV. It never acts.
	ClusterWatchPct float64 `toml:"cluster_watch_pct" json:"cluster_watch_pct"`
	// BudgetWatchPct is rule 18's watch level: one issuer's worst-case loss as a percent of the constitution's effective risk capital. It never acts.
	BudgetWatchPct float64 `toml:"budget_watch_pct" json:"budget_watch_pct"`
	// IssuerGroups joins share classes and ADR/ordinary lines into one issuer, keyed by a name you choose. Canary has no issuer data: an ungrouped symbol is its own issuer.
	IssuerGroups map[string][]string `toml:"issuer_groups" json:"issuer_groups"`
	// Clusters names related issuers that rule 17 tests falling together, keyed by a name you choose; members are symbols or issuer group names.
	Clusters map[string][]string `toml:"clusters" json:"clusters"`

	// CashReserveMinPct is rule 3's cash reserve: broker-reported available funds as a percent of NLV.
	CashReserveMinPct float64 `toml:"cash_reserve_min_pct" json:"cash_reserve_min_pct"`

	// OptionLineWatchPct is rule 2's watch level: one long option position at risk (the higher of price paid and value) as a percent of NLV.
	OptionLineWatchPct float64 `toml:"option_line_watch_pct" json:"option_line_watch_pct"`
	// OptionLineActPct is rule 2's act level for one long option position; the budget governor's per-line limit under basis = rulebook.
	OptionLineActPct float64 `toml:"option_line_act_pct" json:"option_line_act_pct"`
	// HedgeLineWatchPct is rule 2's watch level for a protection position, which rule 12 sizes.
	HedgeLineWatchPct float64 `toml:"hedge_line_watch_pct" json:"hedge_line_watch_pct"`
	// HedgeLineActPct is rule 2's act level for a protection position.
	HedgeLineActPct float64 `toml:"hedge_line_act_pct" json:"hedge_line_act_pct"`

	// RunwayWatchDTE is rule 5's watch horizon: a long option at this many calendar days to expiry or fewer watches.
	RunwayWatchDTE int `toml:"runway_watch_dte" json:"runway_watch_dte"`
	// RunwayActDTE is rule 5's act horizon: at this many calendar days to expiry or fewer the option acts.
	RunwayActDTE int `toml:"runway_act_dte" json:"runway_act_dte"`
	// RunwayITMDeltaFloor is the delta from which rule 5 treats an option as in the money.
	RunwayITMDeltaFloor float64 `toml:"runway_itm_delta_floor" json:"runway_itm_delta_floor"`

	// ShortPutActLinePctNLV is rule 7's act level: one short put's assignment notional through earnings as a percent of NLV.
	ShortPutActLinePctNLV float64 `toml:"short_put_act_line_pct_nlv" json:"short_put_act_line_pct_nlv"`
	// ShortPutActNamePctNLV is rule 7's act level for one name's short puts together.
	ShortPutActNamePctNLV float64 `toml:"short_put_act_name_pct_nlv" json:"short_put_act_name_pct_nlv"`

	// EarningsFreezeSessions is rule 8's window: US sessions before earnings.
	EarningsFreezeSessions int `toml:"earnings_freeze_sessions" json:"earnings_freeze_sessions"`

	// RedOnGreenNameDropPct is rule 9's holding day change (negative percent).
	RedOnGreenNameDropPct float64 `toml:"red_on_green_name_drop_pct" json:"red_on_green_name_drop_pct"`
	// RedOnGreenSPYUpPct is rule 9's SPY day change (percent).
	RedOnGreenSPYUpPct float64 `toml:"red_on_green_spy_up_pct" json:"red_on_green_spy_up_pct"`
	// WinnerTrimDayUpPct is rule 10's holding day gain (percent).
	WinnerTrimDayUpPct float64 `toml:"winner_trim_day_up_pct" json:"winner_trim_day_up_pct"`
	// WinnerTrimMinExpoPct is rule 10's minimum position size as a percent of NLV.
	WinnerTrimMinExpoPct float64 `toml:"winner_trim_min_exposure_pct" json:"winner_trim_min_exposure_pct"`

	// RegimeCalm holds rules 4 and 12 levels in a calm regime. A carried or
	// never-seen regime stage evaluates the carried set and the calm set and
	// keeps the worse verdict, so stale regime data can hold or tighten a
	// verdict but never relax it.
	RegimeCalm RegimeThresholds `toml:"regime_calm" json:"regime_calm"`
	// RegimeEarlyWarning holds rules 4 and 12 levels in an early-warning regime.
	RegimeEarlyWarning RegimeThresholds `toml:"regime_early_warning" json:"regime_early_warning"`
	// RegimeConfirmed holds rules 4 and 12 levels in a confirmed-stress regime.
	RegimeConfirmed RegimeThresholds `toml:"regime_confirmed" json:"regime_confirmed"`
	// RegimeStageMaxAgeMinutes bounds trust in the latched regime stage; older stages evaluate as carried.
	RegimeStageMaxAgeMinutes int `toml:"regime_stage_max_age_minutes" json:"regime_stage_max_age_minutes"`
	// OverhedgeMultiple is the over-hedge boundary as a multiple of rule 12's band top: rule 12 acts above
	// this multiple of the current regime's top, and index puts above this multiple of the widest regime's
	// top count as directional exposure rather than protection.
	OverhedgeMultiple float64 `toml:"overhedge_multiple" json:"overhedge_multiple"`

	// ExitWatchLossPct is rule 13's watch level: percent of premium paid lost on a long option.
	ExitWatchLossPct float64 `toml:"exit_watch_loss_pct" json:"exit_watch_loss_pct"`
	// ExitActLossPct is rule 13's act level; the option loss exit proposes a sale here.
	ExitActLossPct float64 `toml:"exit_act_loss_pct" json:"exit_act_loss_pct"`

	// FXExposureWatchPct is rule 14's watch level: NLV held in other currencies as a percent of NLV.
	FXExposureWatchPct float64 `toml:"fx_exposure_watch_pct" json:"fx_exposure_watch_pct"`

	// NetExposureWatchPct is rule 15's watch level: the whole book's signed stock-equivalent exposure, hedges included, as a percent of NLV.
	NetExposureWatchPct float64 `toml:"net_exposure_watch_pct" json:"net_exposure_watch_pct"`
	// NetExposureActPct is rule 15's act level for the whole book's net exposure.
	NetExposureActPct float64 `toml:"net_exposure_act_pct" json:"net_exposure_act_pct"`

	// HedgeSymbols lists the index underlyings whose long puts can classify as protection (rules 1, 2, 5, 12, 13).
	HedgeSymbols []string `toml:"hedge_symbols" json:"hedge_symbols"`

	// GreeksGapFloorPctNLV is the materiality floor: a name whose legs missing delta exceed this share of NLV makes exposure rules unknown rather than understated.
	GreeksGapFloorPctNLV float64 `toml:"greeks_gap_floor_pct_nlv" json:"greeks_gap_floor_pct_nlv"`

	// EarningsStaleDays bounds trust in a fetched earnings date; older dates make rules 6-8 unknown.
	EarningsStaleDays int `toml:"earnings_stale_days" json:"earnings_stale_days"`
}

// DefaultRulebookPolicy returns the compiled baseline policy.
func DefaultRulebookPolicy() RulebookPolicy {
	return RulebookPolicy{
		ID:      "rulebook-v4",
		Version: 4,
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
			RuleNetExposure:        RuleModeTrack,
			RuleDeltaSwing:         RuleModeTrack,
			RuleClusterStress:      RuleModeTrack,
			RuleLossBudget:         RuleModeAlert,
		},
		SingleNameWatchPct:     30,
		SingleNameActPct:       40,
		TakeoverGapPct:         100,
		HedgeMinDays:           14,
		ExitParticipationPct:   20,
		IlliquidDaysToExit:     3,
		IlliquidWatchPct:       20,
		IlliquidActPct:         30,
		DeltaSwingWatchPct:     30,
		ClusterDropPct:         30,
		ClusterWatchPct:        15,
		BudgetWatchPct:         100,
		IssuerGroups:           map[string][]string{},
		Clusters:               map[string][]string{},
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
			ExtrinsicWatchPct: 10,
			ExtrinsicActPct:   15,
			HedgeBandMinPct:   25,
			HedgeBandMaxPct:   35,
		},
		RegimeEarlyWarning: RegimeThresholds{
			ExtrinsicWatchPct: 7.5,
			ExtrinsicActPct:   12,
			HedgeBandMinPct:   30,
			HedgeBandMaxPct:   50,
		},
		RegimeConfirmed: RegimeThresholds{
			ExtrinsicWatchPct: 5,
			ExtrinsicActPct:   10,
			HedgeBandMinPct:   40,
			HedgeBandMaxPct:   70,
		},
		RegimeStageMaxAgeMinutes: 240,
		OverhedgeMultiple:        2,
		ExitWatchLossPct:         40,
		ExitActLossPct:           60,
		FXExposureWatchPct:       60,
		NetExposureWatchPct:      100,
		NetExposureActPct:        150,
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
	p.IssuerGroups = normalizeSymbolGroups(p.IssuerGroups)
	p.Clusters = normalizeSymbolGroups(p.Clusters)
}

// normalizeSymbolGroups trims group names, upper-cases and sorts members, and
// drops blank members, so a group's identity never depends on TOML spelling.
// It returns a new map; a nil input becomes an empty map.
func normalizeSymbolGroups(in map[string][]string) map[string][]string {
	out := make(map[string][]string, len(in))
	for name, members := range in {
		name = strings.TrimSpace(name)
		var clean []string
		for _, m := range members {
			if m = strings.ToUpper(strings.TrimSpace(m)); m != "" && !slices.Contains(clean, m) {
				clean = append(clean, m)
			}
		}
		sort.Strings(clean)
		out[name] = append(out[name], clean...)
	}
	return out
}

// IssuerOf returns the issuer a held symbol belongs to: its issuer group's
// name, or the symbol itself when no group lists it.
func (p RulebookPolicy) IssuerOf(symbol string) string {
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	for name, members := range p.IssuerGroups {
		if slices.Contains(members, symbol) {
			return name
		}
	}
	return symbol
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
		ID                       string              `json:"id"`
		Version                  int                 `json:"version"`
		Modes                    map[string]string   `json:"modes"`
		SingleNameWatchPct       float64             `json:"single_name_watch_pct"`
		SingleNameActPct         float64             `json:"single_name_act_pct"`
		CashReserveMinPct        float64             `json:"cash_reserve_min_pct"`
		OptionLineWatchPct       float64             `json:"option_line_watch_pct"`
		OptionLineActPct         float64             `json:"option_line_act_pct"`
		HedgeLineWatchPct        float64             `json:"hedge_line_watch_pct"`
		HedgeLineActPct          float64             `json:"hedge_line_act_pct"`
		RunwayWatchDTE           int                 `json:"runway_watch_dte"`
		RunwayActDTE             int                 `json:"runway_act_dte"`
		RunwayITMDeltaFloor      float64             `json:"runway_itm_delta_floor"`
		ShortPutActLinePctNLV    float64             `json:"short_put_act_line_pct_nlv"`
		ShortPutActNamePctNLV    float64             `json:"short_put_act_name_pct_nlv"`
		EarningsFreezeSessions   int                 `json:"earnings_freeze_sessions"`
		RedOnGreenNameDropPct    float64             `json:"red_on_green_name_drop_pct"`
		RedOnGreenSPYUpPct       float64             `json:"red_on_green_spy_up_pct"`
		WinnerTrimDayUpPct       float64             `json:"winner_trim_day_up_pct"`
		WinnerTrimMinExpoPct     float64             `json:"winner_trim_min_exposure_pct"`
		RegimeCalm               RegimeThresholds    `json:"regime_calm"`
		RegimeEarlyWarning       RegimeThresholds    `json:"regime_early_warning"`
		RegimeConfirmed          RegimeThresholds    `json:"regime_confirmed"`
		RegimeStageMaxAgeMinutes int                 `json:"regime_stage_max_age_minutes"`
		OverhedgeMultiple        float64             `json:"overhedge_multiple"`
		ExitWatchLossPct         float64             `json:"exit_watch_loss_pct"`
		ExitActLossPct           float64             `json:"exit_act_loss_pct"`
		FXExposureWatchPct       float64             `json:"fx_exposure_watch_pct"`
		NetExposureWatchPct      float64             `json:"net_exposure_watch_pct"`
		NetExposureActPct        float64             `json:"net_exposure_act_pct"`
		HedgeSymbols             []string            `json:"hedge_symbols"`
		GreeksGapFloorPctNLV     float64             `json:"greeks_gap_floor_pct_nlv"`
		EarningsStaleDays        int                 `json:"earnings_stale_days"`
		TakeoverGapPct           float64             `json:"takeover_gap_pct"`
		HedgeMinDays             int                 `json:"hedge_min_days"`
		ExitParticipationPct     float64             `json:"exit_participation_pct"`
		IlliquidDaysToExit       float64             `json:"illiquid_days_to_exit"`
		IlliquidWatchPct         float64             `json:"illiquid_watch_pct"`
		IlliquidActPct           float64             `json:"illiquid_act_pct"`
		DeltaSwingWatchPct       float64             `json:"delta_swing_watch_pct"`
		ClusterDropPct           float64             `json:"cluster_drop_pct"`
		ClusterWatchPct          float64             `json:"cluster_watch_pct"`
		BudgetWatchPct           float64             `json:"budget_watch_pct"`
		IssuerGroups             map[string][]string `json:"issuer_groups"`
		Clusters                 map[string][]string `json:"clusters"`
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
		OverhedgeMultiple:        q.OverhedgeMultiple,
		ExitWatchLossPct:         q.ExitWatchLossPct,
		ExitActLossPct:           q.ExitActLossPct,
		FXExposureWatchPct:       q.FXExposureWatchPct,
		NetExposureWatchPct:      q.NetExposureWatchPct,
		NetExposureActPct:        q.NetExposureActPct,
		HedgeSymbols:             q.HedgeSymbols,
		GreeksGapFloorPctNLV:     q.GreeksGapFloorPctNLV,
		EarningsStaleDays:        q.EarningsStaleDays,
		TakeoverGapPct:           q.TakeoverGapPct,
		HedgeMinDays:             q.HedgeMinDays,
		ExitParticipationPct:     q.ExitParticipationPct,
		IlliquidDaysToExit:       q.IlliquidDaysToExit,
		IlliquidWatchPct:         q.IlliquidWatchPct,
		IlliquidActPct:           q.IlliquidActPct,
		DeltaSwingWatchPct:       q.DeltaSwingWatchPct,
		ClusterDropPct:           q.ClusterDropPct,
		ClusterWatchPct:          q.ClusterWatchPct,
		BudgetWatchPct:           q.BudgetWatchPct,
		IssuerGroups:             q.IssuerGroups,
		Clusters:                 q.Clusters,
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

// Validate rejects a policy the Rulebook cannot evaluate faithfully: a
// missing identity, a non-finite or out-of-range threshold, a watch level
// above its act level, or a mode outside the closed set. It never repairs a
// value; the caller keeps the policy it already had.
func (p RulebookPolicy) Validate() error {
	if p.Kind != "" && p.Kind != RulebookPolicyKind {
		return fmt.Errorf("kind must be %q", RulebookPolicyKind)
	}
	if p.SchemaVersion != 0 && p.SchemaVersion != 1 {
		return fmt.Errorf("schema_version must be 1")
	}
	if strings.TrimSpace(p.ID) == "" {
		return fmt.Errorf("policy_id must not be empty")
	}
	if p.Version < 1 {
		return fmt.Errorf("policy_version must be at least 1")
	}
	for id, mode := range p.Modes {
		if !slices.Contains(RuleIDs(), id) {
			return fmt.Errorf("modes.%s is not a Rulebook rule (known: %s)", id, strings.Join(RuleIDs(), ", "))
		}
		switch mode {
		case RuleModeOff, RuleModeTrack, RuleModeAlert:
		default:
			return fmt.Errorf("modes.%s must be off, track or alert", id)
		}
	}
	type bounded struct {
		key      string
		value    float64
		min, max float64
	}
	checks := []bounded{
		{"single_name_watch_pct", p.SingleNameWatchPct, 0, 1000},
		{"single_name_act_pct", p.SingleNameActPct, 0, 1000},
		{"cash_reserve_min_pct", p.CashReserveMinPct, 0, 100},
		{"option_line_watch_pct", p.OptionLineWatchPct, 0, 100},
		{"option_line_act_pct", p.OptionLineActPct, 0, 100},
		{"hedge_line_watch_pct", p.HedgeLineWatchPct, 0, 100},
		{"hedge_line_act_pct", p.HedgeLineActPct, 0, 100},
		{"runway_itm_delta_floor", p.RunwayITMDeltaFloor, 0, 1},
		{"short_put_act_line_pct_nlv", p.ShortPutActLinePctNLV, 0, 1000},
		{"short_put_act_name_pct_nlv", p.ShortPutActNamePctNLV, 0, 1000},
		{"red_on_green_name_drop_pct", p.RedOnGreenNameDropPct, -100, 0},
		{"red_on_green_spy_up_pct", p.RedOnGreenSPYUpPct, 0, 100},
		{"winner_trim_day_up_pct", p.WinnerTrimDayUpPct, 0, 100},
		{"winner_trim_min_exposure_pct", p.WinnerTrimMinExpoPct, 0, 1000},
		{"exit_watch_loss_pct", p.ExitWatchLossPct, 0, 100},
		{"exit_act_loss_pct", p.ExitActLossPct, 0, 100},
		{"fx_exposure_watch_pct", p.FXExposureWatchPct, 0, 1000},
		{"net_exposure_watch_pct", p.NetExposureWatchPct, 0, 10000},
		{"net_exposure_act_pct", p.NetExposureActPct, 0, 10000},
		{"overhedge_multiple", p.OverhedgeMultiple, 1, 10},
		{"greeks_gap_floor_pct_nlv", p.GreeksGapFloorPctNLV, 0, 100},
		{"illiquid_watch_pct", p.IlliquidWatchPct, 0, 1000},
		{"illiquid_act_pct", p.IlliquidActPct, 0, 1000},
		{"delta_swing_watch_pct", p.DeltaSwingWatchPct, 0, 1000},
		{"cluster_watch_pct", p.ClusterWatchPct, 0, 1000},
		{"budget_watch_pct", p.BudgetWatchPct, 0, 10000},
	}
	for _, set := range []struct {
		name string
		t    RegimeThresholds
	}{{"regime_calm", p.RegimeCalm}, {"regime_early_warning", p.RegimeEarlyWarning}, {"regime_confirmed", p.RegimeConfirmed}} {
		checks = append(checks,
			bounded{set.name + ".extrinsic_watch_pct", set.t.ExtrinsicWatchPct, 0, 100},
			bounded{set.name + ".extrinsic_act_pct", set.t.ExtrinsicActPct, 0, 100},
			bounded{set.name + ".hedge_band_min_pct", set.t.HedgeBandMinPct, 0, 1000},
			bounded{set.name + ".hedge_band_max_pct", set.t.HedgeBandMaxPct, 0, 1000},
		)
	}
	for _, c := range checks {
		if math.IsNaN(c.value) || math.IsInf(c.value, 0) || c.value < c.min || c.value > c.max {
			return fmt.Errorf("%s must be between %g and %g", c.key, c.min, c.max)
		}
	}
	for _, pair := range []struct {
		watch, act string
		w, a       float64
	}{
		{"single_name_watch_pct", "single_name_act_pct", p.SingleNameWatchPct, p.SingleNameActPct},
		{"illiquid_watch_pct", "illiquid_act_pct", p.IlliquidWatchPct, p.IlliquidActPct},
		{"option_line_watch_pct", "option_line_act_pct", p.OptionLineWatchPct, p.OptionLineActPct},
		{"hedge_line_watch_pct", "hedge_line_act_pct", p.HedgeLineWatchPct, p.HedgeLineActPct},
		{"exit_watch_loss_pct", "exit_act_loss_pct", p.ExitWatchLossPct, p.ExitActLossPct},
		{"net_exposure_watch_pct", "net_exposure_act_pct", p.NetExposureWatchPct, p.NetExposureActPct},
		{"regime_calm.extrinsic_watch_pct", "regime_calm.extrinsic_act_pct", p.RegimeCalm.ExtrinsicWatchPct, p.RegimeCalm.ExtrinsicActPct},
		{"regime_early_warning.extrinsic_watch_pct", "regime_early_warning.extrinsic_act_pct", p.RegimeEarlyWarning.ExtrinsicWatchPct, p.RegimeEarlyWarning.ExtrinsicActPct},
		{"regime_confirmed.extrinsic_watch_pct", "regime_confirmed.extrinsic_act_pct", p.RegimeConfirmed.ExtrinsicWatchPct, p.RegimeConfirmed.ExtrinsicActPct},
		{"regime_calm.hedge_band_min_pct", "regime_calm.hedge_band_max_pct", p.RegimeCalm.HedgeBandMinPct, p.RegimeCalm.HedgeBandMaxPct},
		{"regime_early_warning.hedge_band_min_pct", "regime_early_warning.hedge_band_max_pct", p.RegimeEarlyWarning.HedgeBandMinPct, p.RegimeEarlyWarning.HedgeBandMaxPct},
		{"regime_confirmed.hedge_band_min_pct", "regime_confirmed.hedge_band_max_pct", p.RegimeConfirmed.HedgeBandMinPct, p.RegimeConfirmed.HedgeBandMaxPct},
	} {
		if pair.w > pair.a {
			return fmt.Errorf("%s (%g) must not exceed %s (%g)", pair.watch, pair.w, pair.act, pair.a)
		}
	}
	switch {
	case p.RunwayActDTE < 0 || p.RunwayWatchDTE < p.RunwayActDTE:
		return fmt.Errorf("runway_act_dte must be at least 0 and at most runway_watch_dte")
	case p.EarningsFreezeSessions < 0:
		return fmt.Errorf("earnings_freeze_sessions must be at least 0")
	case p.RegimeStageMaxAgeMinutes < 1:
		return fmt.Errorf("regime_stage_max_age_minutes must be at least 1")
	case p.EarningsStaleDays < 1:
		return fmt.Errorf("earnings_stale_days must be at least 1")
	case p.HedgeMinDays < 0 || p.HedgeMinDays > 3650:
		return fmt.Errorf("hedge_min_days must be between 0 and 3650")
	}
	// Open-interval limits: zero would size an unbounded leg at no move, let an
	// exit take no volume, or test a cluster that does not fall.
	for _, c := range []bounded{
		{"takeover_gap_pct", p.TakeoverGapPct, 0, 1000},
		{"exit_participation_pct", p.ExitParticipationPct, 0, 100},
		{"illiquid_days_to_exit", p.IlliquidDaysToExit, 0, 365},
		{"cluster_drop_pct", p.ClusterDropPct, 0, 100},
	} {
		if math.IsNaN(c.value) || math.IsInf(c.value, 0) || c.value <= c.min || c.value > c.max {
			return fmt.Errorf("%s must be above %g and at most %g", c.key, c.min, c.max)
		}
	}
	for _, sym := range p.HedgeSymbols {
		if strings.TrimSpace(sym) == "" {
			return fmt.Errorf("hedge_symbols must not contain an empty symbol")
		}
	}
	grouped := map[string]string{}
	for name, members := range p.IssuerGroups {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("issuer_groups needs a name for every group")
		}
		if len(members) == 0 {
			return fmt.Errorf("issuer_groups.%s lists no symbol", name)
		}
		for _, m := range members {
			m = strings.ToUpper(strings.TrimSpace(m))
			if m == "" {
				return fmt.Errorf("issuer_groups.%s must not contain an empty symbol", name)
			}
			if other, dup := grouped[m]; dup && other != name {
				return fmt.Errorf("%s is listed in issuer_groups.%s and issuer_groups.%s; a symbol belongs to one issuer", m, other, name)
			}
			grouped[m] = name
		}
	}
	for name, members := range p.Clusters {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("clusters needs a name for every cluster")
		}
		if len(members) == 0 {
			return fmt.Errorf("clusters.%s lists no member", name)
		}
		for _, m := range members {
			if strings.TrimSpace(m) == "" {
				return fmt.Errorf("clusters.%s must not contain an empty member", name)
			}
		}
	}
	return nil
}
