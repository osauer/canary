package rpc

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/osauer/canary/v2/internal/risk"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	// ViewFull requests the complete result shape.
	ViewFull = "full"
	// ViewAlert requests the compact Stress alert projection.
	ViewAlert = "alert"
	// ViewDetail requests expanded detail from a supporting surface.
	ViewDetail = "detail"
	// ViewMonitor requests the compact regime-monitor projection.
	ViewMonitor = "monitor"
	// ViewRisk requests the compact positions-risk projection.
	ViewRisk = "risk"

	optionLowDTEThresholdDays        = 7
	largeStaleOptionLossThresholdPct = -0.5
	defaultRiskExposureLimit         = 5
)

// StressAlertResult is a bounded, alert-safe projection of Stress state. Empty
// flags are not reassuring unless the carried source health is conclusive.
type StressAlertResult struct {
	AsOf               time.Time                `json:"as_of"`
	Fingerprint        Fingerprint              `json:"fingerprint"`
	SourceFingerprints StressSourceFingerprints `json:"source_fingerprints,omitzero"`
	SourceHealth       []CompactSourceHealth    `json:"source_health,omitempty"`
	Action             string                   `json:"action,omitempty"`
	MarketConfirmation string                   `json:"market_confirmation,omitempty"`
	PortfolioFit       string                   `json:"portfolio_fit,omitempty"`
	// PortfolioAlertRelevant carries the producer-stamped relevance verdict
	// through the alert view; see StressResult.PortfolioAlertRelevant.
	PortfolioAlertRelevant *bool                      `json:"portfolio_alert_relevant,omitempty"`
	InputHealth            string                     `json:"input_health,omitempty"`
	Direction              risk.SignalDirection       `json:"direction,omitempty"`
	Severity               risk.SignalSeverity        `json:"severity"`
	PlannerModeHint        risk.PlannerMode           `json:"planner_mode_hint,omitempty"`
	PlannerReadiness       risk.PlannerReadiness      `json:"planner_readiness,omitempty"`
	Summary                string                     `json:"summary"`
	PrimaryDrivers         []risk.SignalID            `json:"primary_drivers,omitempty"`
	Portfolio              StressPortfolioSummary     `json:"portfolio"`
	Market                 StressMarketSummary        `json:"market"`
	OptionHealth           OptionHealthSummary        `json:"option_health"`
	ProtectionCoverage     *ProtectionCoverageSummary `json:"protection_coverage,omitempty"`
	SPYHedgeOffsetPct      *float64                   `json:"spy_hedge_offset_pct,omitempty"`
	Flags                  []StressAlertFlag          `json:"flags,omitempty"`
	Warnings               []string                   `json:"warnings,omitempty"`
	NotExecution           string                     `json:"not_execution"`
}

// StressAlertFlag is one compact advisory finding.
type StressAlertFlag struct {
	Title     string               `json:"title"`
	Direction risk.SignalDirection `json:"direction,omitempty"`
	Severity  risk.SignalSeverity  `json:"severity"`
}

// RegimeMonitorResult is the compact regime lifecycle and indicator projection.
type RegimeMonitorResult struct {
	AsOf            time.Time                `json:"as_of"`
	AuthorityHealth *RegimeAuthorityHealth   `json:"authority_health,omitempty"`
	Fingerprint     Fingerprint              `json:"fingerprint"`
	Lifecycle       LifecycleState           `json:"lifecycle,omitzero"`
	Summary         RegimeSummary            `json:"summary"`
	Posture         RegimePosture            `json:"posture,omitzero"`
	Composite       RegimeComposite          `json:"composite"`
	WarningDetails  []RegimeWarning          `json:"warning_details,omitempty"`
	DataQuality     []DataQualityHealth      `json:"data_quality,omitempty"`
	SourceHealth    []CompactSourceHealth    `json:"source_health,omitempty"`
	Indicators      []RegimeMonitorIndicator `json:"indicators"`
}

// CompactSourceHealth retains the status, reason, and freshness needed to
// interpret a compact result without raw upstream detail.
type CompactSourceHealth struct {
	Source       string         `json:"source"`
	Status       string         `json:"status"`
	AsOf         time.Time      `json:"as_of,omitzero"`
	Confidence   string         `json:"confidence,omitempty"`
	RefreshState string         `json:"refresh_state,omitempty"`
	NextAttempt  *time.Time     `json:"next_attempt,omitempty"`
	LastFailure  *SourceFailure `json:"last_failure,omitempty"`
	Notes        []string       `json:"notes,omitempty"`
}

// RegimeMonitorIndicator is a compact indicator reading and its data quality.
type RegimeMonitorIndicator struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	// Cluster is the indicator's wire cluster name (RegimeClusterNames), so a
	// monitor consumer can group readings without re-deriving the mapping.
	Cluster string             `json:"cluster,omitempty"`
	Band    string             `json:"band,omitempty"`
	AsOf    *RegimeAsOfSummary `json:"as_of,omitempty"`
	Reading string             `json:"reading,omitempty"`
	// Thresholds carries the served worded bands and their compact trip so a
	// monitor face can print the trigger beside the reading. Passed through
	// verbatim from the row's own metadata; this projection authors none of
	// it.
	Thresholds *RegimeThresholds `json:"thresholds,omitempty"`
	// Eligibility and FreshnessClass mirror the detail view's
	// confirmation-eligibility verdict so monitor consumers can tell an
	// eligible red from a provisional one without fetching the full
	// snapshot. Semantic values only — no ticking ages (SSE-hash
	// stability).
	Eligibility    *RegimeEligibility `json:"eligibility,omitempty"`
	FreshnessClass string             `json:"freshness_class,omitempty"`
}

// PositionsRiskResult is a bounded portfolio-risk projection. TopRisks is
// sorted and capped by the requested topN.
type PositionsRiskResult struct {
	DataType           string                     `json:"data_type,omitempty"`
	AsOf               time.Time                  `json:"as_of"`
	AccountID          string                     `json:"account_id,omitempty"`
	Portfolio          *PositionsPortfolio        `json:"portfolio,omitempty"`
	TopExposure        []UnderlyingExposure       `json:"top_exposure,omitempty"`
	OptionHealth       OptionHealthSummary        `json:"option_health"`
	ProtectionCoverage *ProtectionCoverageSummary `json:"protection_coverage,omitempty"`
	SPYHedgeOffsetPct  *float64                   `json:"spy_hedge_offset_pct,omitempty"`
	FlaggedOptionLegs  []OptionRiskLegSummary     `json:"flagged_option_legs,omitempty"`
	Authority          *AccountDataAuthority      `json:"authority,omitempty"`
}

// OptionHealthSummary summarizes availability and concentration of option risk.
type OptionHealthSummary struct {
	GreeksCoverage                  int     `json:"greeks_coverage"`
	GreeksTotal                     int     `json:"greeks_total"`
	MissingGreeksCount              int     `json:"missing_greeks_count"`
	LowDTECount                     int     `json:"low_dte_count"`
	LowDTEThresholdDays             int     `json:"low_dte_threshold_days"`
	OptionsClosedCount              int     `json:"options_closed_count"`
	MarkOutsideBidAskCount          int     `json:"mark_outside_bid_ask_count"`
	LargeStaleDailyLossCount        int     `json:"large_stale_daily_loss_count"`
	LargeStaleDailyLossThresholdPct float64 `json:"large_stale_daily_loss_threshold_pct_nlv"`
	FlaggedLegCount                 int     `json:"flagged_leg_count"`
	FlaggedLegsReturned             int     `json:"flagged_legs_returned"`
}

// OptionRiskLegSummary is one material option-risk leg in a compact result.
type OptionRiskLegSummary struct {
	Symbol       string    `json:"symbol"`
	Expiry       string    `json:"expiry,omitempty"`
	DTE          *int      `json:"dte,omitempty"`
	Right        string    `json:"right,omitempty"`
	Strike       float64   `json:"strike,omitempty"`
	Quantity     float64   `json:"quantity"`
	MarketValue  float64   `json:"market_value_ccy"`
	DailyPnLBase *float64  `json:"daily_pnl_base,omitempty"`
	Delta        *float64  `json:"delta,omitempty"`
	Gamma        *float64  `json:"gamma,omitempty"`
	Theta        *float64  `json:"theta,omitempty"`
	Vega         *float64  `json:"vega,omitempty"`
	DataType     string    `json:"data_type,omitempty"`
	QuoteQuality string    `json:"quote_quality,omitempty"`
	Warnings     []string  `json:"warnings,omitempty"`
	Reasons      []string  `json:"reasons"`
	AsOf         time.Time `json:"as_of,omitzero"`
}

// CompactStressAlert builds an alert-safe projection without changing the
// authority or freshness of its source snapshots.
func CompactStressAlert(c *StressResult, positions *PositionsResult) StressAlertResult {
	if c == nil {
		return StressAlertResult{}
	}
	out := StressAlertResult{
		AsOf:                   c.AsOf,
		Fingerprint:            c.Fingerprint,
		SourceFingerprints:     c.SourceFingerprints,
		SourceHealth:           compactSourceHealth(c.SourceHealth),
		Action:                 c.Action,
		MarketConfirmation:     c.MarketConfirmation,
		PortfolioFit:           c.PortfolioFit,
		PortfolioAlertRelevant: c.PortfolioAlertRelevant,
		InputHealth:            c.InputHealth,
		Direction:              c.Direction,
		Severity:               c.Severity,
		PlannerModeHint:        c.PlannerModeHint,
		PlannerReadiness:       c.PlannerReadiness,
		Summary:                c.Summary,
		PrimaryDrivers:         c.PrimaryDrivers,
		Portfolio:              c.Portfolio,
		Market:                 c.Market,
		Warnings:               c.Warnings,
		NotExecution:           c.NotExecution,
	}
	for _, row := range c.Rows {
		if row.Severity != "" && row.Severity != risk.SeverityObserve {
			out.Flags = append(out.Flags, StressAlertFlag{
				Title:     row.Title,
				Direction: row.Direction,
				Severity:  row.Severity,
			})
		}
	}
	if positions != nil {
		health, legs := optionHealthAndFlaggedLegs(*positions, defaultRiskExposureLimit)
		out.OptionHealth = health
		out.OptionHealth.FlaggedLegsReturned = len(legs)
		out.ProtectionCoverage = positions.ProtectionCoverage
		out.Portfolio.ProtectionCoverage = nil
		out.SPYHedgeOffsetPct = spyHedgeOffsetPct(*positions)
	}
	return out
}

// CompactRegimeMonitor builds the bounded monitor projection from a regime
// snapshot. A nil input yields an unavailable zero-value result.
func CompactRegimeMonitor(r *RegimeSnapshotResult) RegimeMonitorResult {
	if r == nil {
		return RegimeMonitorResult{}
	}
	CompactRegimeSnapshot(r)
	posture := r.Posture
	if posture.Label == "" && posture.Tone == "" {
		posture = BuildRegimePosture(r)
	}
	return RegimeMonitorResult{
		AsOf:            r.AsOf,
		AuthorityHealth: r.AuthorityHealth,
		Fingerprint:     r.Fingerprint,
		Lifecycle:       r.Lifecycle,
		Summary:         r.Summary,
		Posture:         posture,
		Composite:       r.Composite,
		WarningDetails:  r.WarningDetails,
		DataQuality:     r.DataQuality,
		SourceHealth:    compactSourceHealth(r.SourceHealth),
		Indicators: []RegimeMonitorIndicator{
			{Name: "VIX/VIX3M", Status: r.VIXTermStructure.Status, Cluster: RegimeIndicatorCluster(RegimeIndicatorVIXTerm), Band: r.VIXTermStructure.Band, AsOf: r.VIXTermStructure.AsOf, Reading: readingJoin(formatPtr("ratio", r.VIXTermStructure.Ratio), formatPtr("VIX", r.VIXTermStructure.VIX), formatPtr("VIX3M", r.VIXTermStructure.VIX3M)), Thresholds: r.VIXTermStructure.Thresholds, Eligibility: r.VIXTermStructure.Eligibility, FreshnessClass: freshnessClass(r.VIXTermStructure.Freshness)},
			{Name: "VVIX", Status: r.VolOfVol.Status, Cluster: RegimeIndicatorCluster(RegimeIndicatorVolOfVol), Band: r.VolOfVol.Band, AsOf: regimeAsOf(r.VolOfVol.AsOf, r.VolOfVol.AsOfDate), Reading: readingJoin(formatPtr("last", r.VolOfVol.Last), formatPtr("5d%", r.VolOfVol.Change5D), formatPtr("20d", r.VolOfVol.Change20D), range52WReading(r.VolOfVol.Range52W)), Thresholds: r.VolOfVol.Thresholds, Eligibility: r.VolOfVol.Eligibility, FreshnessClass: freshnessClass(r.VolOfVol.Freshness)},
			{Name: "HYG/SPY", Status: r.HYGSPYDivergence.Status, Cluster: RegimeIndicatorCluster(RegimeIndicatorHYGSPY), Band: r.HYGSPYDivergence.Band, AsOf: r.HYGSPYDivergence.AsOf, Reading: readingJoin(formatPtr("HYG", r.HYGSPYDivergence.HYGPrice), formatPtr("SPY", r.HYGSPYDivergence.SPYPrice), formatPtr("SPY chg%", r.HYGSPYDivergence.SPYChangePct), range52WReading(r.HYGSPYDivergence.HYGRange52W)), Thresholds: r.HYGSPYDivergence.Thresholds, Eligibility: r.HYGSPYDivergence.Eligibility, FreshnessClass: freshnessClass(r.HYGSPYDivergence.Freshness)},
			{Name: "Credit spreads", Status: r.CreditSpreads.Status, Cluster: RegimeIndicatorCluster(RegimeIndicatorCredit), Band: r.CreditSpreads.Band, AsOf: regimeAsOf(r.CreditSpreads.AsOf, r.CreditSpreads.AsOfDate), Reading: readingJoin(formatPtr("HY", r.CreditSpreads.HYOAS), formatPtr("IG", r.CreditSpreads.IGOAS), formatPtr("HY-IG", r.CreditSpreads.HYIGSpread)), Thresholds: r.CreditSpreads.Thresholds, Eligibility: r.CreditSpreads.Eligibility, FreshnessClass: freshnessClass(r.CreditSpreads.Freshness)},
			{Name: "Funding", Status: r.FundingStress.Status, Cluster: RegimeIndicatorCluster(RegimeIndicatorFunding), Band: r.FundingStress.Band, AsOf: regimeAsOf(r.FundingStress.AsOf, r.FundingStress.AsOfDate), Reading: formatPtr("spread bp", r.FundingStress.SpreadBps), Thresholds: r.FundingStress.Thresholds, Eligibility: r.FundingStress.Eligibility, FreshnessClass: freshnessClass(r.FundingStress.Freshness)},
			{Name: "USD/JPY", Status: r.USDJPY.Status, Cluster: RegimeIndicatorCluster(RegimeIndicatorUSDJPY), Band: r.USDJPY.Band, AsOf: r.USDJPY.AsOf, Reading: readingJoin(formatPtr("last", r.USDJPY.Last), formatPtr("week%", r.USDJPY.WeeklyChange), range52WReading(r.USDJPY.Range52W)), Thresholds: r.USDJPY.Thresholds, Eligibility: r.USDJPY.Eligibility, FreshnessClass: freshnessClass(r.USDJPY.Freshness)},
			{Name: "Gamma", Status: r.GammaZero.Status, Cluster: RegimeIndicatorCluster(RegimeIndicatorGammaZero), Band: r.GammaZero.Band, AsOf: r.GammaZero.AsOf, Reading: gammaMonitorReading(r.GammaZero), Thresholds: r.GammaZero.Thresholds, Eligibility: r.GammaZero.Eligibility, FreshnessClass: freshnessClass(r.GammaZero.Freshness)},
			{Name: "Breadth", Status: r.Breadth.Status, Cluster: RegimeIndicatorCluster(RegimeIndicatorBreadth), Band: r.Breadth.Band, AsOf: r.Breadth.AsOf, Reading: readingJoin(formatFloat("50dma%", r.Breadth.PctAbove50DMA), formatFloat("200dma%", r.Breadth.PctAbove200DMA), formatFloat("net highs%", r.Breadth.NetNewHighsPct)), Thresholds: r.Breadth.Thresholds, Eligibility: r.Breadth.Eligibility, FreshnessClass: freshnessClass(r.Breadth.Freshness)},
		},
	}
}

// CompactPositionsRisk builds the bounded portfolio-risk projection. Nonpositive
// topN uses the package default.
func CompactPositionsRisk(p *PositionsResult, topN int) PositionsRiskResult {
	if p == nil {
		return PositionsRiskResult{}
	}
	if topN <= 0 {
		topN = defaultRiskExposureLimit
	}
	health, legs := optionHealthAndFlaggedLegs(*p, topN)
	health.FlaggedLegsReturned = len(legs)
	out := PositionsRiskResult{
		DataType:           p.DataType,
		AsOf:               p.AsOf,
		AccountID:          p.AccountID,
		Portfolio:          p.Portfolio,
		OptionHealth:       health,
		ProtectionCoverage: p.ProtectionCoverage,
		SPYHedgeOffsetPct:  spyHedgeOffsetPct(*p),
		FlaggedOptionLegs:  legs,
		Authority:          p.Authority,
	}
	if p.Portfolio != nil {
		out.TopExposure = append([]UnderlyingExposure(nil), p.Portfolio.ExposureBase...)
		if len(out.TopExposure) > topN {
			out.TopExposure = out.TopExposure[:topN]
		}
	}
	return out
}

func freshnessClass(f *RegimeFreshness) string {
	if f == nil {
		return ""
	}
	return f.Class
}

func compactSourceHealth(in []SourceHealth) []CompactSourceHealth {
	out := make([]CompactSourceHealth, 0, len(in))
	for _, src := range in {
		out = append(out, CompactSourceHealth{
			Source:       src.Source,
			Status:       src.Status,
			AsOf:         src.AsOf,
			Confidence:   src.Confidence,
			RefreshState: src.RefreshState,
			NextAttempt:  src.NextAttempt,
			LastFailure:  cloneSourceFailure(src.LastFailure),
			Notes:        src.Notes,
		})
	}
	return out
}

func cloneSourceFailure(in *SourceFailure) *SourceFailure {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func optionHealthAndFlaggedLegs(p PositionsResult, maxLegs int) (OptionHealthSummary, []OptionRiskLegSummary) {
	health := OptionHealthSummary{
		LowDTEThresholdDays:             optionLowDTEThresholdDays,
		LargeStaleDailyLossThresholdPct: largeStaleOptionLossThresholdPct,
	}
	if p.Portfolio != nil {
		health.GreeksCoverage = p.Portfolio.GreeksCoverage
		health.GreeksTotal = p.Portfolio.GreeksTotal
	}
	nlv := positionsNLV(p)
	flagged := []OptionRiskLegSummary{}
	for _, opt := range p.Options {
		leg := optionRiskLeg(p, opt, nlv)
		if slices.Contains(leg.Reasons, "low_dte") {
			health.LowDTECount++
		}
		if slices.Contains(leg.Reasons, "missing_greeks") {
			health.MissingGreeksCount++
		}
		if slices.Contains(leg.Reasons, "options_closed") {
			health.OptionsClosedCount++
		}
		if slices.Contains(leg.Reasons, "mark_outside_bid_ask") {
			health.MarkOutsideBidAskCount++
		}
		if slices.Contains(leg.Reasons, "large_stale_daily_loss") {
			health.LargeStaleDailyLossCount++
		}
		if len(leg.Reasons) == 0 {
			continue
		}
		health.FlaggedLegCount++
		if maxLegs <= 0 || len(flagged) < maxLegs {
			flagged = append(flagged, leg)
		}
	}
	if len(p.Options) == 0 && health.GreeksTotal > health.GreeksCoverage {
		health.MissingGreeksCount = health.GreeksTotal - health.GreeksCoverage
	}
	return health, flagged
}

func optionRiskLeg(p PositionsResult, opt PositionView, nlv float64) OptionRiskLegSummary {
	leg := OptionRiskLegSummary{
		Symbol:       strings.ToUpper(opt.Symbol),
		Expiry:       opt.Expiry,
		Right:        opt.Right,
		Strike:       opt.Strike,
		Quantity:     opt.Quantity,
		MarketValue:  opt.MarketValue,
		DailyPnLBase: opt.DailyPnLBase,
		Delta:        opt.Delta,
		Gamma:        opt.Gamma,
		Theta:        opt.Theta,
		Vega:         opt.Vega,
		DataType:     opt.DataType,
		QuoteQuality: opt.QuoteQuality,
		Warnings:     warningCodes(opt.WarningDetails),
		AsOf:         opt.PriceAt,
	}
	if dte, ok := optionDTE(opt.Expiry, p.AsOf); ok {
		leg.DTE = new(dte)
		if dte <= optionLowDTEThresholdDays {
			leg.Reasons = append(leg.Reasons, "low_dte")
		}
	}
	if opt.Delta == nil || opt.Gamma == nil || opt.Theta == nil || opt.Vega == nil {
		leg.Reasons = append(leg.Reasons, "missing_greeks")
	}
	if positionWarningHas(opt.WarningDetails, "options_closed") {
		leg.Reasons = append(leg.Reasons, "options_closed")
	}
	if opt.MarkOutsideBidAsk {
		leg.Reasons = append(leg.Reasons, "mark_outside_bid_ask")
	}
	if largeStaleDailyOptionLoss(p, opt, nlv) {
		leg.Reasons = append(leg.Reasons, "large_stale_daily_loss")
	}
	return leg
}

func largeStaleDailyOptionLoss(p PositionsResult, opt PositionView, nlv float64) bool {
	if nlv <= 0 || opt.DailyPnLBase == nil {
		return false
	}
	lossPct := *opt.DailyPnLBase / nlv * 100
	return lossPct <= largeStaleOptionLossThresholdPct && staleUnderlyingForOption(p, opt)
}

func staleUnderlyingForOption(p PositionsResult, opt PositionView) bool {
	sym := strings.ToUpper(opt.Symbol)
	for _, stock := range p.Stocks {
		if strings.ToUpper(stock.Symbol) == sym {
			return stalePositionQuote(stock)
		}
	}
	for _, group := range p.ByUnderlying {
		if strings.ToUpper(group.Underlying) == sym && group.Stock != nil {
			return stalePositionQuote(*group.Stock)
		}
	}
	return stalePositionQuote(opt)
}

func stalePositionQuote(p PositionView) bool {
	if p.Stale {
		return true
	}
	switch strings.ToLower(p.DataType) {
	case MarketDataFrozen, MarketDataDelayedFrozen, MarketDataPrevClose, MarketDataClosed:
		return true
	}
	switch strings.ToLower(p.QuoteQuality) {
	case "stale", "missing", "prev_close":
		return true
	}
	return false
}

func optionDTE(raw string, asOf time.Time) (int, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	var expiry time.Time
	var err error
	for _, layout := range []string{"20060102", "2006-01-02"} {
		expiry, err = time.ParseInLocation(layout, raw, asOfLocation(asOf))
		if err == nil {
			break
		}
	}
	if err != nil {
		return 0, false
	}
	base := asOf
	if base.IsZero() {
		base = time.Now()
	}
	loc := asOfLocation(base)
	today := time.Date(base.In(loc).Year(), base.In(loc).Month(), base.In(loc).Day(), 0, 0, 0, 0, time.UTC)
	expiryDay := time.Date(expiry.Year(), expiry.Month(), expiry.Day(), 0, 0, 0, 0, time.UTC)
	return int(expiryDay.Sub(today) / (24 * time.Hour)), true
}

func asOfLocation(t time.Time) *time.Location {
	if t.IsZero() || t.Location() == nil {
		return time.Local
	}
	return t.Location()
}

func positionsNLV(p PositionsResult) float64 {
	if p.Portfolio != nil && p.Portfolio.NetLiquidationBase != nil {
		return *p.Portfolio.NetLiquidationBase
	}
	return 0
}

func spyHedgeOffsetPct(p PositionsResult) *float64 {
	var spyNegative, nonSPYPositive float64
	if p.Portfolio == nil {
		return nil
	}
	for _, exposure := range p.Portfolio.ExposureBase {
		if exposure.DollarDeltaBase == nil {
			continue
		}
		delta := *exposure.DollarDeltaBase
		if strings.EqualFold(exposure.Underlying, "SPY") && delta < 0 {
			spyNegative += math.Abs(delta)
		}
		if !strings.EqualFold(exposure.Underlying, "SPY") && delta > 0 {
			nonSPYPositive += delta
		}
	}
	if spyNegative <= 0 || nonSPYPositive <= 0 {
		return nil
	}
	return new(spyNegative / nonSPYPositive * 100)
}

func warningCodes(warnings []DataWarning) []string {
	out := []string{}
	for _, warning := range warnings {
		if strings.TrimSpace(warning.Code) != "" {
			out = append(out, warning.Code)
		}
	}
	return out
}

func positionWarningHas(warnings []DataWarning, code string) bool {
	for _, warning := range warnings {
		if warning.Code == code {
			return true
		}
	}
	return false
}

func regimeAsOf(asOf *RegimeAsOfSummary, date string) *RegimeAsOfSummary {
	if asOf != nil {
		return asOf
	}
	if strings.TrimSpace(date) == "" {
		return nil
	}
	return &RegimeAsOfSummary{Label: "date " + date, Date: date}
}

func readingJoin(parts ...string) string {
	kept := []string{}
	for _, part := range parts {
		if strings.TrimSpace(part) != "" {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, "; ")
}

func formatPtr(label string, v *float64) string {
	if v == nil {
		return ""
	}
	return formatFloat(label, *v)
}

func formatFloat(label string, v float64) string {
	if v == 0 {
		return ""
	}
	return fmt.Sprintf("%s %.2f", label, v)
}

// range52WReading renders the served 52-week range and the reading's position
// in it for a monitor reading line, e.g. "52w 81.72-147.14 (pos 15%)".
func range52WReading(r *RegimeRange52W) string {
	if r == nil {
		return ""
	}
	return fmt.Sprintf("52w %.2f-%.2f (pos %.0f%%)", r.Low, r.High, r.Pos)
}

func gammaMonitorReading(g RegimeGammaZero) string {
	if g.Envelope.Result != nil && g.Envelope.Result.Summary != nil {
		return g.Envelope.Result.Summary.PrimaryStatement
	}
	if strings.TrimSpace(g.Envelope.ColdReason) != "" {
		return g.Envelope.ColdReason
	}
	if strings.TrimSpace(g.Envelope.Error) != "" {
		return g.Envelope.Error
	}
	return g.Status
}

// Fingerprint versions identify the semantic projection used for each source.
// They are not data-freshness or authority versions.
const (
	regimeFingerprintVersionV1 = "regime-fp-v1"
	// RegimeFingerprintVersion identifies a semantic fingerprint projection.
	RegimeFingerprintVersion = "regime-fp-v2"
	// AccountFingerprintVersion identifies a semantic fingerprint projection.
	AccountFingerprintVersion = "account-fp-v1"
	// PositionsFingerprintVersion identifies a semantic fingerprint projection.
	PositionsFingerprintVersion = "positions-fp-v1"
	// StressFingerprintVersion identifies a semantic fingerprint projection.
	StressFingerprintVersion = "stress-fp-v2"
)

// BuildMarketEventsFingerprint returns the semantic identity of current
// market-event flags. It deliberately ignores timestamps, source prose, and
// exact numeric values except their classified flag status.
func BuildMarketEventsFingerprint(r *MarketEventsResult) Fingerprint {
	if r == nil {
		return semanticFingerprint(MarketEventsFingerprintVersion, nil)
	}
	flags := make([]marketEventFlagFingerprint, 0, len(r.Flags))
	for _, flag := range r.Flags {
		flags = append(flags, marketEventFlagFingerprint{
			ID:       cleanString(flag.ID),
			Symbol:   cleanString(flag.Symbol),
			Status:   cleanString(flag.Status),
			Severity: cleanString(flag.Severity),
			Role:     cleanString(flag.Role),
			Source:   cleanString(flag.Source),
		})
	}
	slices.SortFunc(flags, func(a, b marketEventFlagFingerprint) int {
		if c := cmp.Compare(a.Symbol, b.Symbol); c != 0 {
			return c
		}
		if c := cmp.Compare(a.ID, b.ID); c != 0 {
			return c
		}
		return cmp.Compare(a.Status, b.Status)
	})
	projection := struct {
		Symbols           []string                                  `json:"symbols,omitempty"`
		Flags             []marketEventFlagFingerprint              `json:"flags,omitempty"`
		Sources           []sourceHealthFingerprint                 `json:"sources,omitempty"`
		BorrowFeeCoverage []marketEventBorrowFeeCoverageFingerprint `json:"borrow_fee_coverage,omitempty"`
	}{
		Symbols:           cleanSorted(r.Symbols),
		Flags:             flags,
		Sources:           sourceHealthFingerprints(r.SourceHealth),
		BorrowFeeCoverage: marketEventBorrowFeeCoverageFingerprints(r.BorrowFeeCoverage),
	}
	return semanticFingerprint(MarketEventsFingerprintVersion, projection)
}

type marketEventBorrowFeeCoverageFingerprint struct {
	Symbol              string `json:"symbol,omitempty"`
	ContractConID       int    `json:"contract_con_id,omitempty"`
	ContractFingerprint string `json:"contract_fingerprint,omitempty"`
	CoverageScope       string `json:"coverage_scope,omitempty"`
	Status              string `json:"status,omitempty"`
	Reason              string `json:"reason,omitempty"`
	Source              string `json:"source,omitempty"`
	DataType            string `json:"data_type,omitempty"`
	Entitlement         string `json:"entitlement,omitempty"`
	ScaleStatus         string `json:"scale_status,omitempty"`
	PolicyEligible      bool   `json:"policy_eligible"`
	FailureCode         string `json:"failure_code,omitempty"`
	FailureStage        string `json:"failure_stage,omitempty"`
}

func marketEventBorrowFeeCoverageFingerprints(rows []MarketEventBorrowFeeCoverage) []marketEventBorrowFeeCoverageFingerprint {
	out := make([]marketEventBorrowFeeCoverageFingerprint, 0, len(rows))
	for _, row := range rows {
		fingerprint := marketEventBorrowFeeCoverageFingerprint{
			Symbol:              cleanString(row.Symbol),
			ContractConID:       row.ContractConID,
			ContractFingerprint: cleanString(row.ContractFingerprint),
			CoverageScope:       cleanString(row.CoverageScope),
			Status:              cleanString(row.Status),
			Reason:              cleanString(row.Reason),
			Source:              cleanString(row.Source),
			DataType:            cleanString(row.DataType),
			Entitlement:         cleanString(row.Entitlement),
			ScaleStatus:         cleanString(row.ScaleStatus),
			PolicyEligible:      row.PolicyEligible,
		}
		if row.LastFailure != nil {
			fingerprint.FailureCode = cleanString(row.LastFailure.Code)
			fingerprint.FailureStage = cleanString(row.LastFailure.Stage)
		}
		out = append(out, fingerprint)
	}
	slices.SortFunc(out, func(a, b marketEventBorrowFeeCoverageFingerprint) int {
		if c := cmp.Compare(a.Symbol, b.Symbol); c != 0 {
			return c
		}
		if c := cmp.Compare(a.ContractConID, b.ContractConID); c != 0 {
			return c
		}
		if c := cmp.Compare(a.ContractFingerprint, b.ContractFingerprint); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Source, b.Source); c != 0 {
			return c
		}
		return cmp.Compare(a.Status, b.Status)
	})
	return out
}

// BuildRegimeFingerprint returns the semantic identity of a regime snapshot.
// It hashes classified state only: bands, statuses, composite counts, warning
// codes/scopes/severities, and high-level data quality. It deliberately ignores
// timestamps, raw measurements, and prose.
func BuildRegimeFingerprint(r *RegimeSnapshotResult) Fingerprint {
	if r == nil {
		return buildRegimeFingerprint(nil, RegimeFingerprintVersion, nil)
	}
	return buildRegimeFingerprint(r, RegimeFingerprintVersion, sourceHealthFingerprints(r.SourceHealth))
}

// RegimeFingerprintMatchesSnapshot recomputes the semantic identity embedded
// in r. It accepts the current projection and the sole persisted predecessor,
// whose source-health projection predates typed failure code/stage fields.
// Unknown versions and mismatched keys fail closed.
func RegimeFingerprintMatchesSnapshot(r *RegimeSnapshotResult) bool {
	if r == nil {
		return false
	}
	var want Fingerprint
	switch r.Fingerprint.Version {
	case RegimeFingerprintVersion:
		want = buildRegimeFingerprint(r, RegimeFingerprintVersion, sourceHealthFingerprints(r.SourceHealth))
	case regimeFingerprintVersionV1:
		for _, source := range r.SourceHealth {
			if source.LastFailure != nil {
				return false
			}
		}
		want = buildRegimeFingerprint(r, regimeFingerprintVersionV1, sourceHealthFingerprintsV1(r.SourceHealth))
	default:
		return false
	}
	return r.Fingerprint == want
}

func buildRegimeFingerprint(r *RegimeSnapshotResult, version string, sources []sourceHealthFingerprint) Fingerprint {
	if r == nil {
		return semanticFingerprint(version, nil)
	}
	projection := regimeFingerprintProjection{
		Composite: regimeCompositeFingerprint{
			Verdict:              cleanString(r.Composite.Verdict),
			GreenCount:           r.Composite.GreenCount,
			YellowCount:          r.Composite.YellowCount,
			RedCount:             r.Composite.RedCount,
			RankedCount:          r.Composite.RankedCount,
			UnrankedCount:        r.Composite.UnrankedCount,
			ClusterGreenCount:    r.Composite.ClusterGreenCount,
			ClusterYellowCount:   r.Composite.ClusterYellowCount,
			ClusterRedCount:      r.Composite.ClusterRedCount,
			ClusterRankedCount:   r.Composite.ClusterRankedCount,
			ClusterUnrankedCount: r.Composite.ClusterUnrankedCount,
		},
		Indicators: []regimeIndicatorFingerprint{
			{Name: "vix_term_structure", Band: cleanString(r.VIXTermStructure.Band), Status: cleanString(r.VIXTermStructure.Status), FieldsMissing: cleanSorted(r.VIXTermStructure.FieldsMissing)},
			{Name: "vol_of_vol", Band: cleanString(r.VolOfVol.Band), Status: cleanString(r.VolOfVol.Status)},
			{Name: "hyg_spy_divergence", Band: cleanString(r.HYGSPYDivergence.Band), Status: cleanString(r.HYGSPYDivergence.Status), FieldsMissing: cleanSorted(r.HYGSPYDivergence.FieldsMissing)},
			{Name: "credit_spreads", Band: cleanString(r.CreditSpreads.Band), Status: cleanString(r.CreditSpreads.Status), FieldsMissing: cleanSorted(r.CreditSpreads.FieldsMissing)},
			{Name: "funding_stress", Band: cleanString(r.FundingStress.Band), Status: cleanString(r.FundingStress.Status), FieldsMissing: cleanSorted(r.FundingStress.FieldsMissing)},
			{Name: "usd_jpy", Band: cleanString(r.USDJPY.Band), Status: cleanString(r.USDJPY.Status), FieldsMissing: cleanSorted(r.USDJPY.FieldsMissing)},
			{Name: "gamma_zero", Band: cleanString(r.GammaZero.Band), Status: cleanString(r.GammaZero.Status), FieldsMissing: cleanSorted(r.GammaZero.FieldsMissing)},
			{Name: "breadth", Band: cleanString(r.Breadth.Band), Status: cleanString(r.Breadth.Status), FieldsMissing: cleanSorted(r.Breadth.FieldsMissing)},
		},
		Gamma:       buildRegimeGammaFingerprint(r.GammaZero),
		Breadth:     buildRegimeBreadthFingerprint(r.Breadth),
		Lifecycle:   lifecycleFingerprintProjectionFromState(r.Lifecycle),
		Sources:     sources,
		Warnings:    regimeWarningFingerprints(r.WarningDetails),
		DataQuality: dataQualityFingerprints(r.DataQuality),
	}
	return semanticFingerprint(version, projection)
}

// BuildStressFingerprint returns the semantic alert identity of a Stress
// result. It hashes the classified alert state and source fingerprints, not
// timestamps, exact observed values, evidence strings, or render text.
func BuildStressFingerprint(r *StressResult) Fingerprint {
	if r == nil {
		return semanticFingerprint(StressFingerprintVersion, nil)
	}
	projection := stressFingerprintProjection{
		Policy:             cleanString(r.Policy),
		PolicyProfile:      cleanString(r.PolicyProfile),
		PolicyVersion:      cleanString(r.PolicyVersion),
		PolicyFingerprint:  r.PolicyFingerprint,
		Action:             cleanString(r.Action),
		MarketConfirmation: cleanString(r.MarketConfirmation),
		PortfolioFit:       cleanString(r.PortfolioFit),
		InputHealth:        cleanString(r.InputHealth),
		Direction:          r.Direction,
		Severity:           r.Severity,
		PlannerModeHint:    r.PlannerModeHint,
		PlannerReadiness:   r.PlannerReadiness,
		PrimaryDrivers:     signalIDs(r.PrimaryDrivers),
		Signals:            stressSignalFingerprints(r.Signals),
		Rows:               stressRowFingerprints(r.Rows),
		Market: stressMarketFingerprint{
			RegimeVerdict:      cleanString(r.Market.RegimeVerdict),
			RedClusters:        r.Market.RedClusters,
			YellowClusters:     r.Market.YellowClusters,
			RankedClusters:     r.Market.RankedClusters,
			UnrankedClusters:   r.Market.UnrankedClusters,
			RedClusterNames:    cleanSorted(r.Market.RedClusterNames),
			YellowClusterNames: cleanSorted(r.Market.YellowClusterNames),
			AmbiguousClusters:  cleanSorted(r.Market.AmbiguousClusters),
			PartialClusters:    cleanSorted(r.Market.PartialClusters),
			ComputingClusters:  cleanSorted(r.Market.ComputingClusters),
			DegradedClusters:   cleanSorted(r.Market.DegradedClusters),
			StaleClusters:      cleanSorted(r.Market.StaleClusters),
		},
		Sources: sourceHealthFingerprints(r.SourceHealth),
	}
	if r.SourceFingerprints.Account != nil {
		projection.Source.Account = *r.SourceFingerprints.Account
	}
	if r.SourceFingerprints.Positions != nil {
		projection.Source.Positions = *r.SourceFingerprints.Positions
	}
	if r.SourceFingerprints.Regime != nil {
		projection.Source.Regime = *r.SourceFingerprints.Regime
	}
	if r.SourceFingerprints.MarketEvents != nil {
		projection.Source.MarketEvents = *r.SourceFingerprints.MarketEvents
	}
	return semanticFingerprint(StressFingerprintVersion, projection)
}

// BuildAccountFingerprint hashes only Stress-relevant account buckets. It is
// stable across tiny NLV, cushion, or P&L movement until a risk bucket changes.
func BuildAccountFingerprint(a *AccountResult) Fingerprint {
	if a == nil {
		return semanticFingerprint(AccountFingerprintVersion, nil)
	}
	policy := risk.DefaultPolicy()
	projection := struct {
		BaseCurrency      string `json:"base_currency,omitempty"`
		AccountType       string `json:"account_type,omitempty"`
		MarginCushion     string `json:"margin_cushion,omitempty"`
		LookAheadCushion  string `json:"lookahead_cushion,omitempty"`
		GrossExposure     string `json:"gross_exposure,omitempty"`
		DailyPnL          string `json:"daily_pnl,omitempty"`
		HasMarginContext  bool   `json:"has_margin_context,omitempty"`
		HasNetLiquidation bool   `json:"has_net_liquidation,omitempty"`
	}{
		BaseCurrency:      cleanString(a.BaseCurrency),
		AccountType:       cleanString(a.AccountType),
		HasMarginContext:  accountHasMarginContext(*a),
		HasNetLiquidation: a.NetLiquidation > 0,
	}
	if cushion := accountCushionPct(*a); cushion != nil {
		projection.MarginCushion = riskBucket(*cushion, policy.MarginUrgentPct, policy.MarginActPct, policy.MarginWatchPct, true)
	}
	if cushion := accountLookAheadCushionPct(*a); cushion != nil {
		projection.LookAheadCushion = riskBucket(*cushion, policy.MarginUrgentPct, policy.MarginActPct, policy.MarginWatchPct, true)
	}
	if a.NetLiquidation > 0 && a.GrossPositionValue > 0 {
		grossPct := a.GrossPositionValue / a.NetLiquidation * 100
		projection.GrossExposure = riskBucket(grossPct, policy.GrossExposureStressUrgentPct, policy.GrossExposureStressActPct, policy.GrossExposureWatchPct, false)
	}
	if a.NetLiquidation > 0 && a.DailyPnL != nil {
		pnlPct := *a.DailyPnL / a.NetLiquidation * 100
		projection.DailyPnL = pnlBucket(pnlPct, policy.DailyPnLActPct, policy.DailyPnLWatchPct)
	}
	return semanticFingerprint(AccountFingerprintVersion, projection)
}

// BuildPositionsFingerprint hashes portfolio exposure buckets, not raw marks.
func BuildPositionsFingerprint(p *PositionsResult, netLiquidation float64) Fingerprint {
	if p == nil {
		return semanticFingerprint(PositionsFingerprintVersion, nil)
	}
	policy := risk.DefaultPolicy()
	projection := struct {
		HasPortfolio      bool   `json:"has_portfolio,omitempty"`
		NetDelta          string `json:"net_delta,omitempty"`
		GrossDelta        string `json:"gross_delta,omitempty"`
		LargestExposure   string `json:"largest_exposure,omitempty"`
		LargestExposureID string `json:"largest_exposure_id,omitempty"`
		LargestDelta      string `json:"largest_delta,omitempty"`
		LargestDeltaID    string `json:"largest_delta_id,omitempty"`
		Gamma             string `json:"gamma,omitempty"`
		GreeksCoverage    string `json:"greeks_coverage,omitempty"`
		Stocks            string `json:"stocks,omitempty"`
		Options           string `json:"options,omitempty"`
	}{
		HasPortfolio: p.Portfolio != nil,
		Stocks:       countBucket(len(p.Stocks)),
		Options:      countBucket(len(p.Options)),
	}
	if p.Portfolio == nil {
		return semanticFingerprint(PositionsFingerprintVersion, projection)
	}
	if p.Portfolio.DollarDeltaBase != nil && netLiquidation > 0 {
		pct := absFloat(*p.Portfolio.DollarDeltaBase) / netLiquidation * 100
		projection.NetDelta = riskBucket(pct, policy.NetDeltaStressUrgentPct, policy.NetDeltaStressActPct, policy.NetDeltaWatchPct, false)
	}
	var grossDelta float64
	var largestExposure, largestDelta float64
	for _, e := range p.Portfolio.ExposureBase {
		if e.MarketValuePctNLV != nil && absFloat(*e.MarketValuePctNLV) > largestExposure {
			largestExposure = absFloat(*e.MarketValuePctNLV)
			projection.LargestExposureID = cleanString(e.Underlying)
		}
		if e.DollarDeltaBase != nil && netLiquidation > 0 {
			pct := absFloat(*e.DollarDeltaBase) / netLiquidation * 100
			grossDelta += pct
			if pct > largestDelta {
				largestDelta = pct
				projection.LargestDeltaID = cleanString(e.Underlying)
			}
		}
	}
	if grossDelta > 0 {
		projection.GrossDelta = riskBucket(grossDelta, policy.GrossDeltaStressUrgentPct, policy.GrossDeltaStressActPct, policy.GrossDeltaWatchPct, false)
	}
	if largestExposure > 0 {
		projection.LargestExposure = riskBucket(largestExposure, policy.SingleNameExposureWatchPct*2, policy.SingleNameExposureWatchPct, policy.SingleNameExposureWatchPct, false)
	}
	if largestDelta > 0 {
		projection.LargestDelta = riskBucket(largestDelta, policy.SingleNameDeltaWatchPct*2, policy.SingleNameDeltaWatchPct, policy.SingleNameDeltaWatchPct, false)
	}
	if p.Portfolio.Gamma != nil {
		switch {
		case *p.Portfolio.Gamma < 0:
			projection.Gamma = "negative"
		case *p.Portfolio.Gamma > 0:
			projection.Gamma = "positive"
		default:
			projection.Gamma = "flat"
		}
	}
	if p.Portfolio.GreeksTotal > 0 {
		coverage := float64(p.Portfolio.GreeksCoverage) / float64(p.Portfolio.GreeksTotal) * 100
		if coverage < policy.OptionGreeksMinCoveragePct {
			projection.GreeksCoverage = "degraded"
		} else {
			projection.GreeksCoverage = "ok"
		}
	}
	return semanticFingerprint(PositionsFingerprintVersion, projection)
}

type regimeFingerprintProjection struct {
	Composite   regimeCompositeFingerprint     `json:"composite"`
	Indicators  []regimeIndicatorFingerprint   `json:"indicators"`
	Gamma       regimeGammaFingerprint         `json:"gamma,omitzero"`
	Breadth     regimeBreadthFingerprint       `json:"breadth,omitzero"`
	Lifecycle   lifecycleFingerprintProjection `json:"lifecycle,omitzero"`
	Sources     []sourceHealthFingerprint      `json:"sources,omitempty"`
	Warnings    []regimeWarningFingerprint     `json:"warnings,omitempty"`
	DataQuality []dataQualityFingerprint       `json:"data_quality,omitempty"`
}

type regimeCompositeFingerprint struct {
	Verdict              string `json:"verdict,omitempty"`
	GreenCount           int    `json:"green_count"`
	YellowCount          int    `json:"yellow_count"`
	RedCount             int    `json:"red_count"`
	RankedCount          int    `json:"ranked_count"`
	UnrankedCount        int    `json:"unranked_count"`
	ClusterGreenCount    int    `json:"cluster_green_count"`
	ClusterYellowCount   int    `json:"cluster_yellow_count"`
	ClusterRedCount      int    `json:"cluster_red_count"`
	ClusterRankedCount   int    `json:"cluster_ranked_count"`
	ClusterUnrankedCount int    `json:"cluster_unranked_count"`
}

type regimeIndicatorFingerprint struct {
	Name          string   `json:"name"`
	Band          string   `json:"band,omitempty"`
	Status        string   `json:"status,omitempty"`
	FieldsMissing []string `json:"fields_missing,omitempty"`
}

type regimeGammaFingerprint struct {
	EnvelopeStatus   string `json:"envelope_status,omitempty"`
	Rankability      string `json:"rankability,omitempty"`
	ZeroGammaStatus  string `json:"zero_gamma_status,omitempty"`
	Regime           string `json:"regime,omitempty"`
	Confidence       string `json:"confidence,omitempty"`
	RegimeAgreement  string `json:"regime_agreement,omitempty"`
	HorizonAgreement string `json:"horizon_agreement,omitempty"`
}

type regimeBreadthFingerprint struct {
	State string `json:"state,omitempty"`
}

type regimeWarningFingerprint struct {
	Code     string `json:"code,omitempty"`
	Scope    string `json:"scope,omitempty"`
	Severity string `json:"severity,omitempty"`
}

type dataQualityFingerprint struct {
	Surface          string   `json:"surface,omitempty"`
	Status           string   `json:"status,omitempty"`
	StaleClusters    []string `json:"stale_clusters,omitempty"`
	PartialClusters  []string `json:"partial_clusters,omitempty"`
	DegradedClusters []string `json:"degraded_clusters,omitempty"`
}

type stressFingerprintProjection struct {
	Policy             string                    `json:"policy,omitempty"`
	PolicyProfile      string                    `json:"policy_profile,omitempty"`
	PolicyVersion      string                    `json:"policy_version,omitempty"`
	PolicyFingerprint  Fingerprint               `json:"policy_fingerprint,omitzero"`
	Action             string                    `json:"action,omitempty"`
	MarketConfirmation string                    `json:"market_confirmation,omitempty"`
	PortfolioFit       string                    `json:"portfolio_fit,omitempty"`
	InputHealth        string                    `json:"input_health,omitempty"`
	Direction          risk.SignalDirection      `json:"direction,omitempty"`
	Severity           risk.SignalSeverity       `json:"severity,omitempty"`
	PlannerModeHint    risk.PlannerMode          `json:"planner_mode_hint,omitempty"`
	PlannerReadiness   risk.PlannerReadiness     `json:"planner_readiness,omitempty"`
	PrimaryDrivers     []string                  `json:"primary_drivers,omitempty"`
	Signals            []stressSignalFingerprint `json:"signals,omitempty"`
	Rows               []stressRowFingerprint    `json:"rows,omitempty"`
	Market             stressMarketFingerprint   `json:"market"`
	Sources            []sourceHealthFingerprint `json:"sources,omitempty"`
	Source             stressSourceFingerprint   `json:"source,omitzero"`
}

type stressSourceFingerprint struct {
	Account      Fingerprint `json:"account,omitzero"`
	Positions    Fingerprint `json:"positions,omitzero"`
	Regime       Fingerprint `json:"regime,omitzero"`
	MarketEvents Fingerprint `json:"market_events,omitzero"`
}

type lifecycleFingerprintProjection struct {
	Scope       string              `json:"scope,omitempty"`
	Stage       string              `json:"stage,omitempty"`
	Severity    string              `json:"severity,omitempty"`
	Readiness   string              `json:"readiness,omitempty"`
	Timing      string              `json:"timing,omitempty"`
	Confidence  string              `json:"confidence,omitempty"`
	Evidence    []LifecycleEvidence `json:"evidence,omitempty"`
	ConfirmedBy []string            `json:"confirmed_by,omitempty"`
	Unconfirmed []string            `json:"unconfirmed,omitempty"`
	Suppressed  []string            `json:"suppressed,omitempty"`
	RejectedBy  []string            `json:"rejected_by,omitempty"`
}

type sourceHealthFingerprint struct {
	Source               string `json:"source"`
	Status               string `json:"status"`
	Confidence           string `json:"confidence,omitempty"`
	FingerprintStability string `json:"fingerprint_stability,omitempty"`
	FailureCode          string `json:"failure_code,omitempty"`
	FailureStage         string `json:"failure_stage,omitempty"`
}

type marketEventFlagFingerprint struct {
	ID       string `json:"id"`
	Symbol   string `json:"symbol"`
	Status   string `json:"status"`
	Severity string `json:"severity"`
	Role     string `json:"role"`
	Source   string `json:"source,omitempty"`
}

type stressMarketFingerprint struct {
	RegimeVerdict      string   `json:"regime_verdict,omitempty"`
	RedClusters        int      `json:"red_clusters"`
	YellowClusters     int      `json:"yellow_clusters"`
	RankedClusters     int      `json:"ranked_clusters"`
	UnrankedClusters   int      `json:"unranked_clusters"`
	RedClusterNames    []string `json:"red_cluster_names,omitempty"`
	YellowClusterNames []string `json:"yellow_cluster_names,omitempty"`
	AmbiguousClusters  []string `json:"ambiguous_clusters,omitempty"`
	PartialClusters    []string `json:"partial_clusters,omitempty"`
	ComputingClusters  []string `json:"computing_clusters,omitempty"`
	DegradedClusters   []string `json:"degraded_clusters,omitempty"`
	StaleClusters      []string `json:"stale_clusters,omitempty"`
}

type stressSignalFingerprint struct {
	ID               string   `json:"id"`
	Direction        string   `json:"direction,omitempty"`
	Posture          string   `json:"posture,omitempty"`
	Severity         string   `json:"severity,omitempty"`
	Subject          string   `json:"subject,omitempty"`
	Metric           string   `json:"metric,omitempty"`
	Threshold        string   `json:"threshold,omitempty"`
	Target           string   `json:"target,omitempty"`
	Unit             string   `json:"unit,omitempty"`
	Confidence       string   `json:"confidence,omitempty"`
	BlockedBy        []string `json:"blocked_by,omitempty"`
	ConfidenceImpact string   `json:"confidence_impact,omitempty"`
}

type stressRowFingerprint struct {
	Title     string `json:"title,omitempty"`
	Direction string `json:"direction,omitempty"`
	Severity  string `json:"severity,omitempty"`
}

func semanticFingerprint(version string, projection any) Fingerprint {
	b, _ := json.Marshal(projection)
	sum := sha256.Sum256(b)
	return Fingerprint{
		Version: version,
		Key:     "sha256:" + hex.EncodeToString(sum[:]),
	}
}

func buildRegimeGammaFingerprint(g RegimeGammaZero) regimeGammaFingerprint {
	fp := regimeGammaFingerprint{
		EnvelopeStatus:   cleanString(g.Envelope.Status),
		HorizonAgreement: cleanString(g.HorizonAgreement),
	}
	if g.Envelope.Result == nil {
		return fp
	}
	if g.Envelope.Result.Quality != nil {
		fp.Rankability = cleanString(g.Envelope.Result.Quality.Rankability)
	}
	fp.RegimeAgreement = cleanString(g.Envelope.Result.RegimeAgreement)
	if g.Envelope.Result.Summary != nil {
		fp.ZeroGammaStatus = cleanString(g.Envelope.Result.Summary.ZeroGammaStatus)
		fp.Regime = cleanString(g.Envelope.Result.Summary.Regime)
		fp.Confidence = cleanString(g.Envelope.Result.Summary.Confidence)
	}
	return fp
}

func buildRegimeBreadthFingerprint(b RegimeBreadth) regimeBreadthFingerprint {
	return regimeBreadthFingerprint{State: cleanString(string(b.Envelope.State))}
}

func regimeWarningFingerprints(warnings []RegimeWarning) []regimeWarningFingerprint {
	out := make([]regimeWarningFingerprint, 0, len(warnings))
	for _, w := range warnings {
		fp := regimeWarningFingerprint{
			Code:     cleanString(w.Code),
			Scope:    cleanString(w.Scope),
			Severity: cleanString(w.Severity),
		}
		if fp.Code == "" && fp.Scope == "" && fp.Severity == "" {
			continue
		}
		out = append(out, fp)
	}
	slices.SortFunc(out, func(a, b regimeWarningFingerprint) int {
		return cmp.Or(
			cmp.Compare(a.Code, b.Code),
			cmp.Compare(a.Scope, b.Scope),
			cmp.Compare(a.Severity, b.Severity),
		)
	})
	return out
}

func dataQualityFingerprints(values []DataQualityHealth) []dataQualityFingerprint {
	out := make([]dataQualityFingerprint, 0, len(values))
	for _, q := range values {
		fp := dataQualityFingerprint{
			Surface:          cleanString(q.Surface),
			Status:           cleanString(q.Status),
			StaleClusters:    cleanSorted(q.StaleClusters),
			PartialClusters:  cleanSorted(q.PartialClusters),
			DegradedClusters: cleanSorted(q.DegradedClusters),
		}
		if fp.Surface == "" && fp.Status == "" && len(fp.StaleClusters) == 0 && len(fp.PartialClusters) == 0 && len(fp.DegradedClusters) == 0 {
			continue
		}
		out = append(out, fp)
	}
	slices.SortFunc(out, func(a, b dataQualityFingerprint) int {
		return cmp.Or(
			cmp.Compare(a.Surface, b.Surface),
			cmp.Compare(a.Status, b.Status),
			cmp.Compare(strings.Join(a.StaleClusters, ","), strings.Join(b.StaleClusters, ",")),
			cmp.Compare(strings.Join(a.PartialClusters, ","), strings.Join(b.PartialClusters, ",")),
			cmp.Compare(strings.Join(a.DegradedClusters, ","), strings.Join(b.DegradedClusters, ",")),
		)
	})
	return out
}

func lifecycleFingerprintProjectionFromState(state LifecycleState) lifecycleFingerprintProjection {
	return lifecycleFingerprintProjection{
		Scope:       cleanString(state.Scope),
		Stage:       cleanString(state.Stage),
		Severity:    cleanString(state.Severity),
		Readiness:   cleanString(state.Readiness),
		Timing:      cleanString(state.Timing),
		Confidence:  cleanString(state.Confidence),
		Evidence:    lifecycleEvidenceFingerprints(state.Evidence),
		ConfirmedBy: cleanSorted(state.ConfirmedBy),
		Unconfirmed: cleanSorted(state.Unconfirmed),
		Suppressed:  cleanSorted(state.Suppressed),
		RejectedBy:  cleanSorted(state.RejectedBy),
	}
}

func lifecycleEvidenceFingerprints(values []LifecycleEvidence) []LifecycleEvidence {
	out := make([]LifecycleEvidence, 0, len(values))
	for _, v := range values {
		fp := LifecycleEvidence{
			Source:    cleanString(v.Source),
			Signal:    cleanString(v.Signal),
			Bucket:    cleanString(v.Bucket),
			Timing:    cleanString(v.Timing),
			Severity:  cleanString(v.Severity),
			Confirmed: v.Confirmed,
		}
		if fp.Source == "" && fp.Signal == "" && fp.Bucket == "" && fp.Timing == "" && fp.Severity == "" && !fp.Confirmed {
			continue
		}
		out = append(out, fp)
	}
	slices.SortFunc(out, func(a, b LifecycleEvidence) int {
		return cmp.Or(
			cmp.Compare(a.Source, b.Source),
			cmp.Compare(a.Signal, b.Signal),
			cmp.Compare(a.Bucket, b.Bucket),
			cmp.Compare(a.Timing, b.Timing),
			cmp.Compare(a.Severity, b.Severity),
			cmp.Compare(boolFingerprint(a.Confirmed), boolFingerprint(b.Confirmed)),
		)
	})
	return out
}

func sourceHealthFingerprints(values []SourceHealth) []sourceHealthFingerprint {
	return buildSourceHealthFingerprints(values, true)
}

func sourceHealthFingerprintsV1(values []SourceHealth) []sourceHealthFingerprint {
	return buildSourceHealthFingerprints(values, false)
}

func buildSourceHealthFingerprints(values []SourceHealth, includeFailures bool) []sourceHealthFingerprint {
	out := make([]sourceHealthFingerprint, 0, len(values))
	for _, v := range values {
		fp := sourceHealthFingerprint{
			Source:               cleanString(v.Source),
			Status:               cleanString(v.Status),
			Confidence:           cleanString(v.Confidence),
			FingerprintStability: cleanString(v.FingerprintStability),
		}
		if includeFailures && v.LastFailure != nil {
			fp.FailureCode = cleanString(v.LastFailure.Code)
			fp.FailureStage = cleanString(v.LastFailure.Stage)
		}
		if fp.Source == "" && fp.Status == "" && fp.Confidence == "" {
			continue
		}
		out = append(out, fp)
	}
	slices.SortFunc(out, func(a, b sourceHealthFingerprint) int {
		return cmp.Or(
			cmp.Compare(a.Source, b.Source),
			cmp.Compare(a.Status, b.Status),
			cmp.Compare(a.Confidence, b.Confidence),
			cmp.Compare(a.FingerprintStability, b.FingerprintStability),
			cmp.Compare(a.FailureCode, b.FailureCode),
			cmp.Compare(a.FailureStage, b.FailureStage),
		)
	})
	return out
}

func stressSignalFingerprints(signals []risk.Signal) []stressSignalFingerprint {
	out := make([]stressSignalFingerprint, 0, len(signals))
	for _, s := range signals {
		fp := stressSignalFingerprint{
			ID:               cleanString(string(s.ID)),
			Direction:        cleanString(string(s.Direction)),
			Posture:          cleanString(string(s.Posture)),
			Severity:         cleanString(string(s.Severity)),
			Subject:          cleanString(s.Subject),
			Metric:           cleanString(s.Metric),
			Threshold:        fingerprintFloat(s.Threshold),
			Target:           fingerprintFloat(s.Target),
			Unit:             cleanString(s.Unit),
			Confidence:       cleanString(s.Confidence),
			BlockedBy:        cleanSorted(s.BlockedBy),
			ConfidenceImpact: cleanString(s.ConfidenceImpact),
		}
		if fp.ID == "" {
			continue
		}
		out = append(out, fp)
	}
	slices.SortFunc(out, func(a, b stressSignalFingerprint) int {
		return cmp.Or(
			cmp.Compare(a.ID, b.ID),
			cmp.Compare(a.Direction, b.Direction),
			cmp.Compare(a.Posture, b.Posture),
			cmp.Compare(a.Severity, b.Severity),
			cmp.Compare(a.Subject, b.Subject),
			cmp.Compare(a.Metric, b.Metric),
			cmp.Compare(a.Threshold, b.Threshold),
			cmp.Compare(a.Target, b.Target),
			cmp.Compare(a.Unit, b.Unit),
			cmp.Compare(a.Confidence, b.Confidence),
			cmp.Compare(strings.Join(a.BlockedBy, ","), strings.Join(b.BlockedBy, ",")),
			cmp.Compare(a.ConfidenceImpact, b.ConfidenceImpact),
		)
	})
	return out
}

func stressRowFingerprints(rows []StressRow) []stressRowFingerprint {
	out := make([]stressRowFingerprint, 0, len(rows))
	for _, row := range rows {
		fp := stressRowFingerprint{
			Title:     cleanString(row.Title),
			Direction: cleanString(string(row.Direction)),
			Severity:  cleanString(string(row.Severity)),
		}
		if fp.Title == "" && fp.Direction == "" && fp.Severity == "" {
			continue
		}
		out = append(out, fp)
	}
	slices.SortFunc(out, func(a, b stressRowFingerprint) int {
		return cmp.Or(
			cmp.Compare(a.Title, b.Title),
			cmp.Compare(a.Direction, b.Direction),
			cmp.Compare(a.Severity, b.Severity),
		)
	})
	return out
}

func signalIDs(ids []risk.SignalID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if s := cleanString(string(id)); s != "" {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func cleanSorted(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if s := cleanString(v); s != "" {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func cleanString(v string) string {
	return strings.ToLower(strings.TrimSpace(v))
}

func fingerprintFloat(v *float64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatFloat(*v, 'g', -1, 64)
}

func accountCushionPct(a AccountResult) *float64 {
	if a.NetLiquidation <= 0 {
		return nil
	}
	switch {
	case a.Cushion != 0:
		v := a.Cushion * 100
		return &v
	case a.ExcessLiquidity != 0:
		v := a.ExcessLiquidity / a.NetLiquidation * 100
		return &v
	case accountHasMarginContext(a):
		v := 0.0
		return &v
	default:
		return nil
	}
}

func accountLookAheadCushionPct(a AccountResult) *float64 {
	if a.NetLiquidation <= 0 {
		return nil
	}
	switch {
	case a.LookAheadExcess != 0:
		v := a.LookAheadExcess / a.NetLiquidation * 100
		return &v
	case a.LookAheadMaintMargin > 0 || a.LookAheadInitMargin > 0 || a.LookAheadAvailable < 0:
		v := 0.0
		return &v
	default:
		return nil
	}
}

func accountHasMarginContext(a AccountResult) bool {
	return a.ExcessLiquidity < 0 ||
		a.AvailableFunds < 0 ||
		a.MaintenanceMargin > 0 ||
		a.InitialMargin > 0
}

func riskBucket(v, urgent, act, watch float64, lowerIsWorse bool) string {
	if lowerIsWorse {
		switch {
		case v < urgent:
			return "urgent"
		case v < act:
			return "act"
		case v < watch:
			return "watch"
		default:
			return "ok"
		}
	}
	switch {
	case v >= urgent:
		return "urgent"
	case v >= act:
		return "act"
	case v >= watch:
		return "watch"
	default:
		return "ok"
	}
}

func pnlBucket(v, act, watch float64) string {
	switch {
	case v <= -act:
		return "loss_act"
	case v <= -watch:
		return "loss_watch"
	case v >= act:
		return "gain_act"
	case v >= watch:
		return "gain_watch"
	default:
		return "ok"
	}
}

func countBucket(n int) string {
	switch {
	case n == 0:
		return "zero"
	case n <= 5:
		return "small"
	case n <= 25:
		return "medium"
	default:
		return "large"
	}
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func boolFingerprint(v bool) string {
	if v {
		return "true"
	}
	return "false"
}
