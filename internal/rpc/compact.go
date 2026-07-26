package rpc

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
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
	Name    string             `json:"name"`
	Status  string             `json:"status"`
	Band    string             `json:"band,omitempty"`
	AsOf    *RegimeAsOfSummary `json:"as_of,omitempty"`
	Reading string             `json:"reading,omitempty"`
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
			{Name: "VIX/VIX3M", Status: r.VIXTermStructure.Status, Band: r.VIXTermStructure.Band, AsOf: r.VIXTermStructure.AsOf, Reading: readingJoin(formatPtr("ratio", r.VIXTermStructure.Ratio), formatPtr("VIX", r.VIXTermStructure.VIX), formatPtr("VIX3M", r.VIXTermStructure.VIX3M)), Eligibility: r.VIXTermStructure.Eligibility, FreshnessClass: freshnessClass(r.VIXTermStructure.Freshness)},
			{Name: "VVIX", Status: r.VolOfVol.Status, Band: r.VolOfVol.Band, AsOf: regimeAsOf(r.VolOfVol.AsOf, r.VolOfVol.AsOfDate), Reading: readingJoin(formatPtr("last", r.VolOfVol.Last), formatPtr("20d", r.VolOfVol.Change20D)), Eligibility: r.VolOfVol.Eligibility, FreshnessClass: freshnessClass(r.VolOfVol.Freshness)},
			{Name: "HYG/SPY", Status: r.HYGSPYDivergence.Status, Band: r.HYGSPYDivergence.Band, AsOf: r.HYGSPYDivergence.AsOf, Reading: readingJoin(formatPtr("HYG", r.HYGSPYDivergence.HYGPrice), formatPtr("SPY", r.HYGSPYDivergence.SPYPrice), formatPtr("SPY chg%", r.HYGSPYDivergence.SPYChangePct)), Eligibility: r.HYGSPYDivergence.Eligibility, FreshnessClass: freshnessClass(r.HYGSPYDivergence.Freshness)},
			{Name: "HY/IG OAS", Status: r.CreditSpreads.Status, Band: r.CreditSpreads.Band, AsOf: regimeAsOf(r.CreditSpreads.AsOf, r.CreditSpreads.AsOfDate), Reading: readingJoin(formatPtr("HY", r.CreditSpreads.HYOAS), formatPtr("IG", r.CreditSpreads.IGOAS), formatPtr("HY-IG", r.CreditSpreads.HYIGSpread)), Eligibility: r.CreditSpreads.Eligibility, FreshnessClass: freshnessClass(r.CreditSpreads.Freshness)},
			{Name: "Funding", Status: r.FundingStress.Status, Band: r.FundingStress.Band, AsOf: regimeAsOf(r.FundingStress.AsOf, r.FundingStress.AsOfDate), Reading: formatPtr("spread bp", r.FundingStress.SpreadBps), Eligibility: r.FundingStress.Eligibility, FreshnessClass: freshnessClass(r.FundingStress.Freshness)},
			{Name: "USD/JPY", Status: r.USDJPY.Status, Band: r.USDJPY.Band, AsOf: r.USDJPY.AsOf, Reading: readingJoin(formatPtr("last", r.USDJPY.Last), formatPtr("week%", r.USDJPY.WeeklyChange)), Eligibility: r.USDJPY.Eligibility, FreshnessClass: freshnessClass(r.USDJPY.Freshness)},
			{Name: "Gamma", Status: r.GammaZero.Status, Band: r.GammaZero.Band, AsOf: r.GammaZero.AsOf, Reading: gammaMonitorReading(r.GammaZero), Eligibility: r.GammaZero.Eligibility, FreshnessClass: freshnessClass(r.GammaZero.Freshness)},
			{Name: "Breadth", Status: r.Breadth.Status, Band: r.Breadth.Band, AsOf: r.Breadth.AsOf, Reading: readingJoin(formatFloat("50dma%", r.Breadth.PctAbove50DMA), formatFloat("200dma%", r.Breadth.PctAbove200DMA), formatFloat("net highs%", r.Breadth.NetNewHighsPct)), Eligibility: r.Breadth.Eligibility, FreshnessClass: freshnessClass(r.Breadth.Freshness)},
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
