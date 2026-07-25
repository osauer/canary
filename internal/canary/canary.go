package canary

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/osauer/ibkr/v2/internal/marketcal"
	"github.com/osauer/ibkr/v2/internal/regimerows"
	"github.com/osauer/ibkr/v2/internal/risk"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

var canaryPolicy = risk.DefaultPolicy()

const canaryHeldStressLimit = 5

const (
	canaryEstablishedRegimeFingerprintVersion       = "regime-fp-v1"
	canaryEstablishedMarketEventsFingerprintVersion = "market-events-fp-v1"
)

// StressInput is the shared typed input contract defined by package rpc.
type StressInput = rpc.StressInput

// StressResult is the shared typed result contract defined by package rpc.
type StressResult = rpc.StressResult

// StressSourceAsOf carries the source timestamps used by an assessment.
type StressSourceAsOf = rpc.StressSourceAsOf

// StressSourceFingerprints carries semantic identities for source snapshots.
type StressSourceFingerprints = rpc.StressSourceFingerprints

// StressRow is one classified row in the assessment.
type StressRow = rpc.StressRow

// StressMarketIndicator is one market indicator exposed with its provenance.
type StressMarketIndicator = rpc.StressMarketIndicator

// StressPortfolioSummary is the redacted portfolio context used by Canary.
type StressPortfolioSummary = rpc.StressPortfolioSummary

// StressMarketSummary is the classified market context used by Canary.
type StressMarketSummary = rpc.StressMarketSummary

// ComputeStress evaluates one typed snapshot. A zero input clock uses the
// current time. Missing, stale, or incomplete sources remain explicit and do
// not become healthy zero values. The result is advisory and performs no broker
// writes.
func ComputeStress(in StressInput) StressResult {
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	res := computeCanary(in, now, canarySourceIssues(in, now), false)
	established := computeCanary(in, now, canaryEstablishedSourceIssues(in, now), true)
	projection := canaryEstablishedAlertProjection(established)
	res.EstablishedAlertProjection = &projection
	return res
}

// computeCanary owns the one Canary decision implementation. The established
// mode freezes only the source-interpretation and source-health behavior that
// existed at ad5b77b; account, positions, market clusters, Regime data quality,
// and every underlying risk calculation continue through the same producer.
func computeCanary(in StressInput, now time.Time, sourceIssues []canarySourceIssue, established bool) StressResult {
	accountFingerprint := rpc.BuildAccountFingerprint(&in.Account)
	positionsFingerprint := rpc.BuildPositionsFingerprint(&in.Positions, in.Account.NetLiquidation)
	regimeFingerprint := in.Regime.Fingerprint
	if regimeFingerprint.Key == "" {
		regimeFingerprint = rpc.BuildRegimeFingerprint(&in.Regime)
	}
	if established {
		regimeFingerprint = canaryEstablishedRegimeFingerprint(in.Regime)
	}
	marketEventsFingerprint := canaryRelevantMarketEventsFingerprint(in.Positions, in.MarketEvents)
	if established {
		marketEventsFingerprint = canaryEstablishedMarketEventsFingerprint(in.MarketEvents)
	}
	sourceAsOf := StressSourceAsOf{Account: in.Account.AsOf, Positions: in.Positions.AsOf, Regime: in.Regime.AsOf, MarketEvents: in.MarketEvents.AsOf}
	sourceFingerprints := StressSourceFingerprints{Account: &accountFingerprint, Positions: &positionsFingerprint, Regime: &regimeFingerprint}
	if marketEventsFingerprint.Key != "" {
		sourceFingerprints.MarketEvents = &marketEventsFingerprint
	}
	res := StressResult{
		AsOf:               now,
		SourceAsOf:         sourceAsOf,
		SourceFingerprints: sourceFingerprints,
		Policy:             canaryPolicy.PolicyProfile(),
		PolicyProfile:      canaryPolicy.PolicyProfile(),
		PolicyVersion:      canaryPolicy.PolicyVersion(),
		PolicyFingerprint:  rpc.Fingerprint{Version: risk.CanaryPolicyFingerprintVersion, Key: canaryPolicy.FingerprintKey()},
		Portfolio:          summarizeCanaryPortfolio(in.Account, in.Positions, in.MarketEvents, now),
		Market:             summarizeCanaryMarket(in.Regime, now),
		MarketIndicators:   canaryMarketIndicators(in.Regime, now),
		NotExecution:       "Read-only canary snapshot; no orders are placed by ibkr.",
	}
	rows := []StressRow{
		canaryMarginRow(res.Portfolio),
		canaryPnLShockRow(res.Portfolio),
		canaryTapeShockRow(res.Portfolio, res.Market),
		canaryMarketRow(res.Market),
		canaryExposureRow(res.Portfolio, res.Market),
		canaryConcentrationRow(res.Portfolio, res.Market),
		canaryProtectionCoverageRow(res.Portfolio),
		canaryHeldStressRow(res.Portfolio, res.Market),
		canaryOptionsRow(res.Portfolio, in.Positions, res.Market),
		canaryDataQualityRow(res.Market, in.Regime),
	}
	res.Signals = canarySignals(res.Portfolio, in.Positions, res.Market, in.Regime)
	res.Signals = canaryApplySourceBlocks(res.Signals, sourceIssues)
	if established {
		res.Signals = append(res.Signals, canaryEstablishedSourceDataQualitySignals(sourceIssues)...)
	} else {
		res.Signals = append(res.Signals, canarySourceDataQualitySignals(sourceIssues)...)
	}
	res.MarketConfirmation = canaryMarketConfirmation(res.Market)
	res.PortfolioFit = canaryPortfolioFit(res.Portfolio, res.Signals)
	res.PortfolioAlertRelevant = new(canaryPortfolioAlertRelevant(&res))
	res.InputHealth = canaryInputHealth(in, res.Market, sourceIssues, now)
	res.Direction, res.Severity = canaryDecisionState(res.MarketConfirmation, res.PortfolioFit, res.InputHealth, res.Market, res.Signals)
	res.Action = canaryAction(res.Direction, res.Severity, res.MarketConfirmation, res.PortfolioFit, res.InputHealth)
	res.PlannerModeHint = canaryPlannerModeFromAction(res.Action)
	res.PlannerReadiness = canaryPlannerReadinessFromAction(res.Action, res.Severity, res.InputHealth)
	res.PrimaryDrivers = canaryPrimaryDrivers(res.Signals)
	res.Summary = canaryDecisionSummary(res)
	overall := canaryOverallRow(res.Direction, res.Severity, res.Summary, res.Market, res.Portfolio)
	res.Rows = append([]StressRow{overall}, rows...)
	res.Warnings = canaryWarnings(res.Market, in.Regime, now)
	if established {
		res.SourceHealth = canaryEstablishedSourceHealth(in, now, accountFingerprint, positionsFingerprint, regimeFingerprint, marketEventsFingerprint, res.InputHealth, res.Market)
	} else {
		res.Warnings = append(res.Warnings, canaryMarketEventWarnings(sourceIssues)...)
		res.SourceHealth = canarySourceHealth(in, now, accountFingerprint, positionsFingerprint, regimeFingerprint, marketEventsFingerprint, res.InputHealth, res.Market)
	}
	res.Fingerprint = rpc.BuildStressFingerprint(&res)
	return res
}

// canaryRelevantMarketEventsFingerprint applies the same exposure boundary as
// canaryMarketEventSourceIssues before market-event health enters alert
// identity. The diagnostic snapshot remains portfolio-neutral, while a borrow
// outage on an all-long book cannot churn Canary or established delivery
// fingerprints. Active flags are retained: only irrelevant borrow source
// health is removed.
func canaryRelevantMarketEventsFingerprint(pos rpc.PositionsResult, events rpc.MarketEventsResult) rpc.Fingerprint {
	if !canaryHasMarketEventsInput(events) {
		return rpc.Fingerprint{}
	}
	if canaryHasShortStockExposure(pos) {
		if events.Fingerprint.Key != "" {
			return events.Fingerprint
		}
		return rpc.BuildMarketEventsFingerprint(&events)
	}
	filtered := canaryRelevantMarketEvents(pos, events)
	filtered.Fingerprint = rpc.Fingerprint{}
	return rpc.BuildMarketEventsFingerprint(&filtered)
}

// canaryEstablishedMarketEventsFingerprint keeps the exact pre-v2
// MarketEvents source projection used by established delivery. New typed
// failure details are stripped, but every v1 source-health bucket remains;
// changing that projection under the same v1 label would break dedupe and
// recovery continuity.
func canaryEstablishedMarketEventsFingerprint(events rpc.MarketEventsResult) rpc.Fingerprint {
	if !canaryHasMarketEventsInput(events) {
		return rpc.Fingerprint{}
	}
	if events.Fingerprint.Key != "" && len(events.BorrowFeeCoverage) == 0 && !canarySourceHealthHasTypedFailure(events.SourceHealth) {
		fingerprint := events.Fingerprint
		fingerprint.Version = canaryEstablishedMarketEventsFingerprintVersion
		return fingerprint
	}
	filtered := events
	filtered.Fingerprint = rpc.Fingerprint{}
	filtered.BorrowFeeCoverage = nil
	filtered.SourceHealth = slices.Clone(filtered.SourceHealth)
	for i := range filtered.SourceHealth {
		filtered.SourceHealth[i].LastFailure = nil
	}
	fingerprint := rpc.BuildMarketEventsFingerprint(&filtered)
	fingerprint.Version = canaryEstablishedMarketEventsFingerprintVersion
	return fingerprint
}

func canaryEstablishedRegimeFingerprint(regime rpc.RegimeSnapshotResult) rpc.Fingerprint {
	if regime.Fingerprint.Key != "" && !canarySourceHealthHasTypedFailure(regime.SourceHealth) {
		fingerprint := regime.Fingerprint
		fingerprint.Version = canaryEstablishedRegimeFingerprintVersion
		return fingerprint
	}
	regime.Fingerprint = rpc.Fingerprint{}
	regime.SourceHealth = slices.Clone(regime.SourceHealth)
	for i := range regime.SourceHealth {
		regime.SourceHealth[i].LastFailure = nil
	}
	fingerprint := rpc.BuildRegimeFingerprint(&regime)
	fingerprint.Version = canaryEstablishedRegimeFingerprintVersion
	return fingerprint
}

func canarySourceHealthHasTypedFailure(health []rpc.SourceHealth) bool {
	for _, source := range health {
		if source.LastFailure != nil {
			return true
		}
	}
	return false
}

func canaryRelevantMarketEvents(pos rpc.PositionsResult, events rpc.MarketEventsResult) rpc.MarketEventsResult {
	if canaryHasShortStockExposure(pos) {
		return events
	}
	filtered := events
	filtered.BorrowFeeCoverage = nil
	filtered.SourceHealth = slices.DeleteFunc(slices.Clone(events.SourceHealth), func(health rpc.SourceHealth) bool {
		return canaryMarketEventBorrowSource(canaryMarketEventSourceName(health.Source))
	})
	filtered.WarningDetails = slices.DeleteFunc(slices.Clone(events.WarningDetails), func(warning rpc.DataWarning) bool {
		return canaryMarketEventBorrowSource(canaryMarketEventSourceName(warning.Scope + " " + warning.Code))
	})
	return filtered
}

func canaryEstablishedAlertProjection(result StressResult) rpc.EstablishedAlertProjection {
	portfolioRelevant := result.PortfolioAlertRelevant != nil && *result.PortfolioAlertRelevant
	actionEligible := severityRankAtLeast(result.Severity, risk.SeverityAct) ||
		result.Action == canaryActionDefend ||
		result.Action == canaryActionRebalance ||
		result.Action == canaryActionConfirmInputs
	occurrenceEligible := portfolioRelevant &&
		(severityRankAtLeast(result.Severity, risk.SeverityWatch) || actionEligible)
	return rpc.EstablishedAlertProjection{
		SchemaVersion:        rpc.EstablishedAlertProjectionSchemaVersion,
		CanonicalFingerprint: rpc.Fingerprint{Version: rpc.EstablishedStressFingerprintVersion, Key: result.Fingerprint.Key},
		OccurrenceEligible:   occurrenceEligible,
		ActOnlyEligible:      occurrenceEligible && actionEligible,
		Action:               result.Action,
		MarketConfirmation:   result.MarketConfirmation,
		Severity:             result.Severity,
		PortfolioRelevant:    portfolioRelevant,
	}
}

func summarizeCanaryPortfolio(acct rpc.AccountResult, pos rpc.PositionsResult, marketEvents rpc.MarketEventsResult, now time.Time) StressPortfolioSummary {
	out := StressPortfolioSummary{
		BaseCurrency:   acct.BaseCurrency,
		NetLiquidation: acct.NetLiquidation,
	}
	if acct.NetLiquidation > 0 {
		out.CushionPct = canaryCurrentCushionPct(acct)
		out.LookAheadCushionPct = canaryLookAheadCushionPct(acct)
		if acct.GrossPositionValue > 0 {
			pct := acct.GrossPositionValue / acct.NetLiquidation * 100
			out.GrossExposurePctNLV = &pct
		}
		if acct.DailyPnL != nil {
			pct := *acct.DailyPnL / acct.NetLiquidation * 100
			out.DailyPnLPct = &pct
		}
	}
	if pos.Portfolio != nil {
		if pos.Portfolio.DollarDeltaBase != nil && acct.NetLiquidation > 0 {
			pct := math.Abs(*pos.Portfolio.DollarDeltaBase) / acct.NetLiquidation * 100
			out.NetDeltaPctNLV = &pct
		}
		if pos.Portfolio.GreeksTotal > 0 {
			out.OptionGreeks = fmt.Sprintf("%d/%d legs", pos.Portfolio.GreeksCoverage, pos.Portfolio.GreeksTotal)
		}
		for _, e := range pos.Portfolio.ExposureBase {
			if e.MarketValuePctNLV != nil {
				if out.LargestExposurePct == nil || math.Abs(*e.MarketValuePctNLV) > math.Abs(*out.LargestExposurePct) {
					pct := *e.MarketValuePctNLV
					out.LargestExposurePct = &pct
					out.LargestExposure = strings.ToUpper(e.Underlying)
				}
			}
			if e.DollarDeltaBase == nil || acct.NetLiquidation <= 0 {
				continue
			}
			pct := math.Abs(*e.DollarDeltaBase) / acct.NetLiquidation * 100
			gross := pct
			if out.GrossDeltaPctNLV != nil {
				gross += *out.GrossDeltaPctNLV
			}
			out.GrossDeltaPctNLV = &gross
			if out.LargestDeltaPctNLV == nil || pct > *out.LargestDeltaPctNLV {
				out.LargestDeltaPctNLV = &pct
				out.LargestDeltaExposure = strings.ToUpper(e.Underlying)
			}
		}
	}
	out.ProtectionCoverage = pos.ProtectionCoverage
	out.HeldStress = canaryHeldStressSummaries(acct, pos, marketEvents, now)
	return out
}

func canaryHeldStressSummaries(acct rpc.AccountResult, pos rpc.PositionsResult, marketEvents rpc.MarketEventsResult, now time.Time) []rpc.HeldStress {
	if acct.NetLiquidation <= 0 {
		return nil
	}
	builder := newCanaryHeldStressBuilder(acct.NetLiquidation)
	builder.addPortfolioExposures(pos.Portfolio)
	builder.addUnderlyingGroups(pos.ByUnderlying)
	builder.addStockRows(pos.Stocks)
	builder.addOptionRows(canaryOptionsByUnderlying(pos), now)
	return builder.rows(marketEvents)
}

type canaryHeldStressBuilder struct {
	netLiquidation float64
	rowsBySymbol   map[string]*rpc.HeldStress
	order          []string
}

func newCanaryHeldStressBuilder(netLiquidation float64) *canaryHeldStressBuilder {
	return &canaryHeldStressBuilder{
		netLiquidation: netLiquidation,
		rowsBySymbol:   map[string]*rpc.HeldStress{},
	}
}

func (b *canaryHeldStressBuilder) ensure(underlying string) *rpc.HeldStress {
	underlying = strings.ToUpper(strings.TrimSpace(underlying))
	if underlying == "" {
		return nil
	}
	if s := b.rowsBySymbol[underlying]; s != nil {
		return s
	}
	b.rowsBySymbol[underlying] = &rpc.HeldStress{Underlying: underlying}
	b.order = append(b.order, underlying)
	return b.rowsBySymbol[underlying]
}

func (b *canaryHeldStressBuilder) addPortfolioExposures(portfolio *rpc.PositionsPortfolio) {
	if portfolio == nil {
		return
	}
	for _, e := range portfolio.ExposureBase {
		s := b.ensure(e.Underlying)
		if s == nil {
			continue
		}
		canarySetFloatPtrIfNil(&s.MarketValuePctNLV, e.MarketValuePctNLV)
		if s.DeltaPctNLV == nil && e.DollarDeltaBase != nil {
			v := math.Abs(*e.DollarDeltaBase) / b.netLiquidation * 100
			s.DeltaPctNLV = &v
		}
		if s.DailyPnLPctNLV == nil && e.DailyPnLBase != nil {
			v := *e.DailyPnLBase / b.netLiquidation * 100
			s.DailyPnLPctNLV = &v
		}
	}
}

func (b *canaryHeldStressBuilder) addUnderlyingGroups(groups []rpc.PositionGroup) {
	for _, group := range groups {
		s := b.ensure(group.Underlying)
		if s == nil {
			continue
		}
		canarySetFloatPtrIfNil(&s.MarketValuePctNLV, group.GroupMarketValuePctNLV)
		if s.MarketValuePctNLV == nil && group.GroupMarketValueBase != nil {
			v := *group.GroupMarketValueBase / b.netLiquidation * 100
			s.MarketValuePctNLV = &v
		}
		if s.DeltaPctNLV == nil && group.GroupDollarDeltaBase != nil {
			v := math.Abs(*group.GroupDollarDeltaBase) / b.netLiquidation * 100
			s.DeltaPctNLV = &v
		}
		if s.DailyPnLPctNLV == nil && group.GroupDailyPnLBase != nil {
			v := *group.GroupDailyPnLBase / b.netLiquidation * 100
			s.DailyPnLPctNLV = &v
		}
		s.LiquidityFlags = canaryUniqueFlags(s.LiquidityFlags, canaryHeldStockLiquidityFlags(group.Stock)...)
	}
}

func (b *canaryHeldStressBuilder) addStockRows(stocks []rpc.PositionView) {
	for i := range stocks {
		stock := &stocks[i]
		s := b.ensure(stock.Symbol)
		if s == nil {
			continue
		}
		if s.MarketValuePctNLV == nil && stock.MarketValueBase != nil {
			v := *stock.MarketValueBase / b.netLiquidation * 100
			s.MarketValuePctNLV = &v
		}
		if s.DailyPnLPctNLV == nil && stock.DailyPnLBase != nil {
			v := *stock.DailyPnLBase / b.netLiquidation * 100
			s.DailyPnLPctNLV = &v
		}
		s.LiquidityFlags = canaryUniqueFlags(s.LiquidityFlags, canaryHeldStockLiquidityFlags(stock)...)
	}
}

func (b *canaryHeldStressBuilder) addOptionRows(optionsByUnderlying map[string][]rpc.PositionView, now time.Time) {
	optionUnderlyings := make([]string, 0, len(optionsByUnderlying))
	for underlying := range optionsByUnderlying {
		optionUnderlyings = append(optionUnderlyings, underlying)
	}
	slices.Sort(optionUnderlyings)
	for _, underlying := range optionUnderlyings {
		options := optionsByUnderlying[underlying]
		s := b.ensure(underlying)
		if s == nil {
			continue
		}
		canaryApplyHeldOptionStress(s, options, now, b.netLiquidation)
		s.LiquidityFlags = canaryUniqueFlags(s.LiquidityFlags, canaryHeldOptionLiquidityFlags(options)...)
	}
}

func (b *canaryHeldStressBuilder) rows(marketEvents rpc.MarketEventsResult) []rpc.HeldStress {
	out := []rpc.HeldStress{}
	for _, underlying := range b.order {
		s := b.rowsBySymbol[underlying]
		s.MarketFlags = canaryHeldMarketFlags(underlying, marketEvents)
		s.MaterialReasons = canaryHeldStressMaterialReasons(*s)
		s.SignalIDs = canaryHeldStressSignalIDs(*s)
		if len(s.MaterialReasons) == 0 || len(s.SignalIDs) == 0 {
			continue
		}
		out = append(out, *s)
	}
	slices.SortStableFunc(out, func(a, b rpc.HeldStress) int {
		return cmp.Compare(canaryHeldStressSortScore(b), canaryHeldStressSortScore(a))
	})
	if len(out) > canaryHeldStressLimit {
		out = out[:canaryHeldStressLimit]
	}
	return out
}

func canaryHeldMarketFlags(underlying string, events rpc.MarketEventsResult) []rpc.MarketEventFlag {
	underlying = strings.ToUpper(strings.TrimSpace(underlying))
	if underlying == "" || events.BySymbol == nil {
		return nil
	}
	out := []rpc.MarketEventFlag{}
	for _, flag := range events.BySymbol[underlying] {
		switch flag.Status {
		case rpc.MarketEventStatusActive, rpc.MarketEventStatusRecent:
			out = append(out, flag)
		}
	}
	slices.SortFunc(out, func(a, b rpc.MarketEventFlag) int {
		if c := strings.Compare(a.Symbol, b.Symbol); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

func canarySetFloatPtrIfNil(dst **float64, src *float64) {
	if *dst != nil || src == nil {
		return
	}
	v := *src
	*dst = &v
}

func canaryOptionsByUnderlying(pos rpc.PositionsResult) map[string][]rpc.PositionView {
	out := map[string][]rpc.PositionView{}
	if len(pos.ByUnderlying) > 0 {
		for _, group := range pos.ByUnderlying {
			underlying := strings.ToUpper(strings.TrimSpace(group.Underlying))
			if underlying == "" || len(group.Options) == 0 {
				continue
			}
			out[underlying] = append(out[underlying], group.Options...)
		}
		return out
	}
	for _, opt := range pos.Options {
		underlying := strings.ToUpper(strings.TrimSpace(opt.Symbol))
		if underlying == "" {
			continue
		}
		out[underlying] = append(out[underlying], opt)
	}
	return out
}

func canaryApplyHeldOptionStress(s *rpc.HeldStress, options []rpc.PositionView, now time.Time, nlv float64) {
	if s == nil || nlv <= 0 {
		return
	}
	var deltaAbsBase, gamma float64
	var hasDelta, hasGamma bool
	var minDTE *int
	for _, opt := range options {
		dte, ok := canaryOptionDTE(opt.Expiry, now)
		if !ok || dte < 0 || dte > canaryPolicy.HeldOptionNearDTE {
			continue
		}
		if minDTE == nil || dte < *minDTE {
			v := dte
			minDTE = &v
		}
		if opt.Delta != nil && opt.Underlying != nil && *opt.Underlying > 0 {
			fx := 1.0
			if opt.FXRate != nil {
				fx = *opt.FXRate
			}
			v := *opt.Delta * opt.Quantity * float64(max(opt.Multiplier, 1)) * *opt.Underlying * fx
			deltaAbsBase += math.Abs(v)
			hasDelta = true
		}
		if opt.Gamma != nil {
			gamma += *opt.Gamma * opt.Quantity * float64(max(opt.Multiplier, 1))
			hasGamma = true
		}
	}
	s.NearExpiryMinDTE = minDTE
	if hasDelta {
		pct := deltaAbsBase / nlv * 100
		s.NearExpiryDeltaPctNLV = &pct
	}
	if hasGamma {
		s.NearExpiryGamma = &gamma
	}
}

func canaryHeldStockLiquidityFlags(stock *rpc.PositionView) []string {
	if stock == nil {
		return nil
	}
	flags := []string{}
	liveOrUnknown := canaryPositionMarketOpenOrUnknown(*stock)
	quality := strings.ToLower(strings.TrimSpace(stock.QuoteQuality))
	switch quality {
	case "stale", "missing", "prev_close":
		flags = append(flags, "stock_quote_"+quality)
	case "wide":
		if liveOrUnknown {
			flags = append(flags, "stock_wide_quote")
		}
	}
	if stock.Stale {
		flags = append(flags, "stock_quote_stale")
	}
	if liveOrUnknown && stock.SpreadPct != nil && *stock.SpreadPct >= canaryPolicy.HeldLiquidityStockSpreadPct {
		flags = append(flags, "stock_wide_spread")
	}
	return canaryUniqueFlags(nil, flags...)
}

func canaryPositionMarketOpenOrUnknown(p rpc.PositionView) bool {
	if p.SessionContext == nil {
		return true
	}
	return p.SessionContext.IsOpen
}

func canaryHeldOptionLiquidityFlags(options []rpc.PositionView) []string {
	flags := []string{}
	for _, opt := range options {
		if canaryPositionWarningHas(opt.WarningDetails, "options_closed") {
			continue
		}
		if opt.MarkOutsideBidAsk {
			flags = append(flags, "option_mark_outside_bid_ask")
		}
		if opt.OptionBid == nil || opt.OptionAsk == nil {
			flags = append(flags, "option_bid_ask_missing")
			continue
		}
		if *opt.OptionBid <= 0 || *opt.OptionAsk <= 0 || *opt.OptionAsk < *opt.OptionBid {
			flags = append(flags, "option_bid_ask_missing")
			continue
		}
		mid := (*opt.OptionBid + *opt.OptionAsk) / 2
		if mid <= 0 {
			flags = append(flags, "option_bid_ask_missing")
			continue
		}
		spreadPct := (*opt.OptionAsk - *opt.OptionBid) / mid * 100
		if spreadPct >= canaryPolicy.HeldLiquidityOptionSpreadPctOfMid {
			flags = append(flags, "option_wide_spread")
		}
	}
	return canaryUniqueFlags(nil, flags...)
}

func canaryPositionWarningHas(details []rpc.DataWarning, code string) bool {
	code = strings.ToLower(strings.TrimSpace(code))
	for _, detail := range details {
		if strings.ToLower(strings.TrimSpace(detail.Code)) == code {
			return true
		}
	}
	return false
}

func canaryOptionDTE(expiry string, now time.Time) (int, bool) {
	expiry = strings.TrimSpace(expiry)
	if expiry == "" {
		return 0, false
	}
	if now.IsZero() {
		now = time.Now()
	}
	loc := now.Location()
	var t time.Time
	var err error
	for _, layout := range []string{"20060102", "2006-01-02"} {
		t, err = time.ParseInLocation(layout, expiry, loc)
		if err == nil {
			break
		}
	}
	if err != nil {
		return 0, false
	}
	y, m, d := now.In(loc).Date()
	start := time.Date(y, m, d, 0, 0, 0, 0, loc)
	return int(t.Sub(start).Hours() / 24), true
}

func canaryHeldStressMaterialReasons(s rpc.HeldStress) []string {
	reasons := []string{}
	if s.MarketValuePctNLV != nil && math.Abs(*s.MarketValuePctNLV) >= canaryPolicy.HeldStressMaterialPct {
		reasons = appendUniqueString(reasons, "market_value")
	}
	if s.DeltaPctNLV != nil && *s.DeltaPctNLV >= canaryPolicy.HeldStressMaterialPct {
		reasons = appendUniqueString(reasons, "delta")
	}
	if s.DailyPnLPctNLV != nil && *s.DailyPnLPctNLV <= -canaryPolicy.HeldUnderlyingPnLWatchPct {
		reasons = appendUniqueString(reasons, "daily_pnl")
	}
	if s.NearExpiryDeltaPctNLV != nil && *s.NearExpiryDeltaPctNLV >= canaryPolicy.HeldOptionDeltaWatchPct {
		reasons = appendUniqueString(reasons, "near_expiry_option_delta")
	}
	return reasons
}

func canaryHeldStressSignalIDs(s rpc.HeldStress) []risk.SignalID {
	ids := []risk.SignalID{}
	if s.DailyPnLPctNLV != nil && *s.DailyPnLPctNLV <= -canaryPolicy.HeldUnderlyingPnLWatchPct {
		ids = append(ids, risk.SignalHeldUnderlyingPnLShock)
	}
	if s.NearExpiryDeltaPctNLV != nil && *s.NearExpiryDeltaPctNLV >= canaryPolicy.HeldOptionDeltaWatchPct {
		ids = append(ids, risk.SignalHeldOptionExpiryConcentration)
	}
	if len(s.LiquidityFlags) > 0 {
		ids = append(ids, risk.SignalHeldLiquidityDegraded)
	}
	return ids
}

func canaryHeldStressSortScore(s rpc.HeldStress) float64 {
	score := 0.0
	if s.MarketValuePctNLV != nil {
		score = max(score, math.Abs(*s.MarketValuePctNLV))
	}
	if s.DeltaPctNLV != nil {
		score = max(score, *s.DeltaPctNLV)
	}
	if s.NearExpiryDeltaPctNLV != nil {
		score = max(score, *s.NearExpiryDeltaPctNLV)
	}
	if s.DailyPnLPctNLV != nil && *s.DailyPnLPctNLV < 0 {
		score = max(score, math.Abs(*s.DailyPnLPctNLV)*10)
	}
	score += float64(len(s.LiquidityFlags)) * 5
	return score
}

func canaryUniqueFlags(flags []string, values ...string) []string {
	for _, value := range values {
		flags = appendUniqueString(flags, value)
	}
	return flags
}

func canaryCurrentCushionPct(acct rpc.AccountResult) *float64 {
	if acct.NetLiquidation <= 0 {
		return nil
	}
	switch {
	case acct.Cushion != 0:
		return new(acct.Cushion * 100)
	case acct.ExcessLiquidity != 0:
		return new(acct.ExcessLiquidity / acct.NetLiquidation * 100)
	case canaryHasActiveMarginContext(acct):
		return new(0.0)
	default:
		return nil
	}
}

func canaryLookAheadCushionPct(acct rpc.AccountResult) *float64 {
	if acct.NetLiquidation <= 0 {
		return nil
	}
	switch {
	case acct.LookAheadExcess != 0:
		return new(acct.LookAheadExcess / acct.NetLiquidation * 100)
	case acct.LookAheadMaintMargin > 0 || acct.LookAheadInitMargin > 0 || acct.LookAheadAvailable < 0:
		return new(0.0)
	default:
		return nil
	}
}

func canaryHasActiveMarginContext(acct rpc.AccountResult) bool {
	return acct.ExcessLiquidity < 0 ||
		acct.AvailableFunds < 0 ||
		acct.MaintenanceMargin > 0 ||
		acct.InitialMargin > 0
}

func summarizeCanaryMarket(r rpc.RegimeSnapshotResult, now time.Time) StressMarketSummary {
	posture := r.Posture
	if posture.Label == "" && posture.Tone == "" {
		posture = rpc.BuildRegimePosture(&r)
	}
	out := StressMarketSummary{
		RegimeVerdict: r.Composite.Verdict,
		RegimePosture: posture,
		SPYPrice:      r.HYGSPYDivergence.SPYPrice,
		SPYChangePct:  r.HYGSPYDivergence.SPYChangePct,
		VIX:           r.VIXTermStructure.VIX,
		VIXChangePct:  r.VIXTermStructure.VIXChangePct,
	}
	out.TapeSessionState, out.TapeSessionReason, out.TapeNextOpen = canaryTapeSession(now)
	contextClusters := canaryMarketContextClusters(r, now)
	// Shared rpc combination: raw worst-of bands, eligibility-keyed
	// isolated-red downgrades, and the eligible/provisional split. Canary
	// previously recomputed this from raw bands (a third policy copy) and
	// confirmed "market stress" on two marginal reds — the 2026-06-12
	// false-positive surface the user actually watches.
	cb := rpc.BuildRegimeClusterBands(&r)
	clusterBands := map[string]string{}
	for i, name := range rpc.RegimeClusterNames {
		clusterBands[name] = cb.Confirmed[i]
		eligible := cb.Confirmed[i] == "red" && cb.Eligible[i]
		if eligible {
			out.EligibleRedClusterNames = append(out.EligibleRedClusterNames, name)
		}
		if cb.Raw[i] == "red" && !eligible {
			out.UnconfirmedRedClusterNames = append(out.UnconfirmedRedClusterNames, name)
		}
	}
	statuses := map[string][]string{
		"vol":     {r.VIXTermStructure.Status, r.VolOfVol.Status},
		"credit":  {r.HYGSPYDivergence.Status, r.CreditSpreads.Status},
		"funding": {r.FundingStress.Status},
		"fx":      {r.USDJPY.Status},
		"gamma":   {r.GammaZero.Status},
		"breadth": {r.Breadth.Status},
	}
	for name, clusterBand := range clusterBands {
		switch clusterBand {
		case "red":
			out.RedClusterNames = append(out.RedClusterNames, name)
		case "yellow":
			out.YellowClusterNames = append(out.YellowClusterNames, name)
		}
		if clusterBand == "" {
			out.UnrankedClusters++
		} else {
			out.RankedClusters++
		}
		status := weakestStatus(statuses[name])
		if status == rpc.RegimeStatusComputing {
			out.ComputingClusters = append(out.ComputingClusters, name)
		}
		if status == rpc.RegimeStatusStale && !contextClusters[name] {
			out.StaleClusters = append(out.StaleClusters, name)
		}
		if clusterBand == "" {
			if !contextClusters[name] {
				out.AmbiguousClusters = append(out.AmbiguousClusters, name)
			}
		} else if !contextClusters[name] && (status == rpc.RegimeStatusError || status == rpc.RegimeStatusUnavailable || status == rpc.RegimeStatusComputing) {
			out.PartialClusters = append(out.PartialClusters, name)
		}
	}
	if canaryGammaDegraded(r.GammaZero) {
		out.DegradedClusters = append(out.DegradedClusters, "gamma")
	}
	slices.Sort(out.RedClusterNames)
	slices.Sort(out.YellowClusterNames)
	slices.Sort(out.EligibleRedClusterNames)
	slices.Sort(out.UnconfirmedRedClusterNames)
	slices.Sort(out.AmbiguousClusters)
	slices.Sort(out.PartialClusters)
	slices.Sort(out.ComputingClusters)
	slices.Sort(out.DegradedClusters)
	slices.Sort(out.StaleClusters)
	out.RedClusters = len(out.RedClusterNames)
	out.EligibleRedClusters = len(out.EligibleRedClusterNames)
	out.YellowClusters = len(out.YellowClusterNames)
	return out
}

func canaryMarketContextClusters(r rpc.RegimeSnapshotResult, now time.Time) map[string]bool {
	out := map[string]bool{}
	if canaryGammaContextOnly(r.GammaZero) {
		out["gamma"] = true
	}
	if canaryVolClosedSessionContext(r, now) {
		out["vol"] = true
	}
	return out
}

func canaryGammaContextOnly(g rpc.RegimeGammaZero) bool {
	return g.Envelope.Result != nil &&
		g.Envelope.Result.Quality != nil &&
		g.Envelope.Result.Quality.Rankability == rpc.GammaRankabilityContextOnly &&
		g.Freshness != nil && g.Freshness.Class == rpc.RegimeFreshnessNotDue
}

func canaryVolClosedSessionContext(r rpc.RegimeSnapshotResult, _ time.Time) bool {
	return r.VIXTermStructure.Freshness != nil &&
		r.VIXTermStructure.Freshness.Class == rpc.RegimeFreshnessNotDue
}

func canaryMarketIndicators(r rpc.RegimeSnapshotResult, now time.Time) []StressMarketIndicator {
	if now.IsZero() {
		now = time.Now()
	}
	contextClusters := canaryMarketContextClusters(r, now)
	rows := []struct {
		cluster string
		row     regimerows.Row
		asOf    *rpc.RegimeAsOfSummary
		date    string
		status  string
	}{
		{cluster: "vol", row: regimerows.VIXTerm(now, r.VIXTermStructure), asOf: r.VIXTermStructure.AsOf, status: r.VIXTermStructure.Status},
		{cluster: "vol", row: regimerows.VolOfVol(now, r.VolOfVol), asOf: r.VolOfVol.AsOf, date: r.VolOfVol.AsOfDate, status: r.VolOfVol.Status},
		{cluster: "credit", row: regimerows.HYGSPY(now, r.HYGSPYDivergence), asOf: r.HYGSPYDivergence.AsOf, status: r.HYGSPYDivergence.Status},
		{cluster: "credit", row: regimerows.CreditSpreads(now, r.CreditSpreads), asOf: r.CreditSpreads.AsOf, date: r.CreditSpreads.AsOfDate, status: r.CreditSpreads.Status},
		{cluster: "funding", row: regimerows.FundingStress(now, r.FundingStress), asOf: r.FundingStress.AsOf, date: r.FundingStress.AsOfDate, status: r.FundingStress.Status},
		{cluster: "fx", row: regimerows.USDJPY(now, r.USDJPY), asOf: r.USDJPY.AsOf, status: r.USDJPY.Status},
		{cluster: "gamma", row: regimerows.Gamma(now, r.GammaZero), asOf: r.GammaZero.AsOf, status: r.GammaZero.Status},
		{cluster: "breadth", row: regimerows.Breadth(now, r.Breadth), asOf: r.Breadth.AsOf, status: r.Breadth.Status},
	}
	out := make([]StressMarketIndicator, 0, len(rows))
	for _, item := range rows {
		reading := item.row.Value
		if item.row.StateNote != "" && item.row.Band == regimerows.BandUnranked {
			reading = item.row.StateNote
		}
		contextOnly := contextClusters[item.cluster]
		out = append(out, StressMarketIndicator{
			Name:    item.row.Name,
			Status:  canaryIndicatorStatus(item.row.Band, item.status, contextOnly),
			AsOf:    canaryIndicatorAsOf(item.asOf, item.date, item.row.AsOf),
			Reading: reading,
			Comment: canaryIndicatorComment(item.row, reading, contextOnly),
		})
	}
	return out
}

func canaryIndicatorStatus(b regimerows.Band, status string, contextOnly bool) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case rpc.RegimeStatusComputing, rpc.RegimeStatusError, rpc.RegimeStatusUnavailable:
		if b == regimerows.BandUnranked {
			return "n/a"
		}
	}
	switch b {
	case regimerows.BandGreen:
		return "green"
	case regimerows.BandYellow:
		return "amber"
	case regimerows.BandRed:
		return "red"
	default:
		if contextOnly {
			return "context"
		}
		return "n/a"
	}
}

func canaryIndicatorAsOf(meta *rpc.RegimeAsOfSummary, date, fallback string) string {
	if meta != nil {
		if meta.Date != "" {
			return meta.Date
		}
		if !meta.Time.IsZero() {
			return meta.Time.Local().Format("2006-01-02")
		}
		if meta.Label != "" {
			return meta.Label
		}
	}
	if date != "" {
		return date
	}
	return regimerows.IfNonEmpty(fallback, "—")
}

func canaryIndicatorComment(row regimerows.Row, reading string, contextOnly bool) string {
	parts := []string{}
	add := func(part string) {
		part = strings.TrimSpace(part)
		if part == "" || strings.EqualFold(part, strings.TrimSpace(reading)) || slices.Contains(parts, part) {
			return
		}
		parts = append(parts, part)
	}
	add(row.Reason)
	if row.Status == rpc.RegimeStatusStale && !strings.Contains(strings.ToLower(row.Reason), "context") {
		if contextOnly {
			add("closed-session cached context")
		} else {
			add("stale input")
		}
	}
	if row.Quality != "" {
		add(strings.TrimSpace(strings.TrimPrefix(row.Quality, "·")))
	}
	return strings.Join(parts, "; ")
}

func canaryGammaDegraded(g rpc.RegimeGammaZero) bool {
	if g.Envelope.Result == nil {
		return false
	}
	if g.Envelope.Result.Quality == nil {
		return true
	}
	switch g.Envelope.Result.Quality.Rankability {
	case rpc.GammaRankabilityRankable:
		return false
	case rpc.GammaRankabilityContextOnly:
		return !canaryGammaContextOnly(g)
	default:
		return true
	}
}

func weakestStatus(statuses []string) string {
	var sawComputing, sawUnavailable, sawError bool
	var sawStale bool
	for _, status := range statuses {
		switch strings.ToLower(strings.TrimSpace(status)) {
		case rpc.RegimeStatusError:
			sawError = true
		case rpc.RegimeStatusUnavailable:
			sawUnavailable = true
		case rpc.RegimeStatusComputing:
			sawComputing = true
		case rpc.RegimeStatusStale:
			sawStale = true
		}
	}
	switch {
	case sawError:
		return rpc.RegimeStatusError
	case sawUnavailable:
		return rpc.RegimeStatusUnavailable
	case sawComputing:
		return rpc.RegimeStatusComputing
	case sawStale:
		return rpc.RegimeStatusStale
	default:
		return rpc.RegimeStatusOK
	}
}

func canaryRow(title string, direction risk.SignalDirection, severity risk.SignalSeverity, guidance, evidence string) StressRow {
	return StressRow{
		Title:     title,
		Direction: direction,
		Severity:  severity,
		Guidance:  guidance,
		Evidence:  evidence,
	}
}

func canaryMarginRow(p StressPortfolioSummary) StressRow {
	cushion := canaryWorstCushionPct(p)
	if cushion != nil {
		switch {
		case *cushion < canaryPolicy.MarginUrgentPct:
			return canaryRow("Immediate margin safety", risk.DirectionDefensive, risk.SeverityUrgent, fmt.Sprintf("Move to cash-heavy / near-flat now; margin cushion is below %.0f%%.", canaryPolicy.MarginUrgentPct), canaryCushionEvidence(p))
		case *cushion < canaryPolicy.MarginActPct:
			return canaryRow("Immediate margin safety", risk.DirectionDefensive, risk.SeverityAct, fmt.Sprintf("Cut gross and net exposure until cushion is back above %.0f%%.", canaryPolicy.MarginTargetPct), canaryCushionEvidence(p))
		case *cushion < canaryPolicy.MarginWatchPct:
			return canaryRow("Immediate margin safety", risk.DirectionDefensive, risk.SeverityWatch, fmt.Sprintf("Do not add risk; prepare a reduction plan if cushion falls below %.0f%%.", canaryPolicy.MarginTargetPct), canaryCushionEvidence(p))
		}
		return canaryRow("Immediate margin safety", "", risk.SeverityObserve, "No forced margin action.", canaryCushionEvidence(p))
	}
	return canaryRow("Immediate margin safety", risk.DirectionDataQuality, risk.SeverityWatch, "No forced margin action, but confirm account cushion before sizing new risk.", "cushion unavailable")
}

func canaryPnLShockRow(p StressPortfolioSummary) StressRow {
	if p.DailyPnLPct == nil {
		return canaryRow("Portfolio P&L shock", risk.DirectionDataQuality, risk.SeverityWatch, "Daily P&L is unavailable; this indicator cannot confirm or reject a P&L shock.", "daily P&L unavailable")
	}
	pct := *p.DailyPnLPct
	absPct := math.Abs(pct)
	evidence := fmt.Sprintf("daily P&L %+.1f%% NLV (watch at ±%.0f%%)", pct, canaryPolicy.DailyPnLWatchPct)
	if absPct >= canaryPolicy.DailyPnLActPct {
		if pct < 0 {
			return canaryRow("Portfolio P&L shock", risk.DirectionDefensive, risk.SeverityAct, "Large daily loss; review defensive actions and protect liquidity.", evidence)
		}
		return canaryRow("Portfolio P&L shock", risk.DirectionDefensive, risk.SeverityWatch, "Large daily gain; protect gains and avoid accidental chase-risk.", evidence)
	}
	if absPct >= canaryPolicy.DailyPnLWatchPct {
		if pct < 0 {
			return canaryRow("Portfolio P&L shock", risk.DirectionDefensive, risk.SeverityWatch, "Daily loss is large enough to review risk before adding exposure.", evidence)
		}
		return canaryRow("Portfolio P&L shock", risk.DirectionDefensive, risk.SeverityWatch, "Daily gain is large enough to review sizing and opportunity deliberately.", evidence)
	}
	return canaryRow("Portfolio P&L shock", "", risk.SeverityObserve, "No daily P&L shock signal.", evidence)
}

func canaryMarketRow(m StressMarketSummary) StressRow {
	evidence := canaryMarketEvidence(m)
	switch {
	case m.EligibleRedClusters >= 3 && m.RankedClusters >= 4:
		return canaryRow("Confirmed market stress", risk.DirectionDefensive, risk.SeverityAct, "Reduce equity beta materially; reserve urgent action for margin or exposure rows.", evidence)
	case m.EligibleRedClusters >= 2 && m.RankedClusters >= 3:
		return canaryRow("Confirmed market stress", risk.DirectionDefensive, risk.SeverityAct, "Cut marginal longs and short-convexity exposure; keep only intentional hedged risk.", evidence)
	case canaryFastCarryUnwind(m):
		return canaryRow("Fast carry unwind", risk.DirectionDefensive, risk.SeverityAct, "Reduce fragile beta and short-vol exposure; FX stress is confirmed by tape or breadth.", evidence)
	case m.RedClusters >= 2:
		return canaryRow("Stress pending confirmation", risk.DirectionDefensive, risk.SeverityWatch, "Stress clusters are visible but not confirmed yet (need more depth, persistence, or fresh data); hold de-risking at watch.", evidence)
	case m.RedClusters == 1 && m.YellowClusters >= 1:
		return canaryRow("Early stress filtered", risk.DirectionDefensive, risk.SeverityWatch, "Wait for a second independent red cluster before major de-risking.", evidence)
	case m.YellowClusters >= 3:
		return canaryRow("Deteriorating tape", risk.DirectionDefensive, risk.SeverityWatch, "Freeze new risk and review hedges; no urgent action without red confirmation.", evidence)
	default:
		if len(m.UnconfirmedRedClusterNames) > 0 {
			// The overall summary calls this warning out; the check still
			// passes, and saying why here keeps the two from contradicting.
			return canaryRow("Market stress", "", risk.SeverityObserve, "An early warning is flashing ("+canaryClusterList(m.UnconfirmedRedClusterNames)+" red, not confirmed); below the de-risking trigger.", evidence)
		}
		return canaryRow("Market stress", "", risk.SeverityObserve, "No market-regime de-risking trigger.", evidence)
	}
}

func canaryTapeShockRow(p StressPortfolioSummary, m StressMarketSummary) StressRow {
	evidence := canaryTapeEvidence(m)
	if m.SPYChangePct == nil && m.VIXChangePct == nil {
		return canaryRow("Index tape shock", risk.DirectionDataQuality, risk.SeverityWatch, "Direct SPY/VIX tape is unavailable; do not treat quiet regime clusters as complete overnight coverage.", evidence)
	}
	spyDrop := pctAtMost(m.SPYChangePct, canaryPolicy.SPYDropPct)
	spyHardDrop := pctAtMost(m.SPYChangePct, canaryPolicy.SPYHardDropPct)
	spyCrash := pctAtMost(m.SPYChangePct, canaryPolicy.SPYCrashPct)
	vixSpike := pctAtLeast(m.VIXChangePct, canaryPolicy.VIXSpikePct)
	vixHardSpike := pctAtLeast(m.VIXChangePct, canaryPolicy.VIXHardSpikePct)
	if !canaryTapeConfirmable(m) {
		if spyDrop || vixSpike {
			return canaryRow("Index tape shock", "", risk.SeverityObserve, canaryTapeDemotedGuidance(m), evidence)
		}
		return canaryRow("Index tape shock", "", risk.SeverityObserve, "No direct SPY/VIX overnight tape shock.", evidence)
	}
	confirmed := (spyDrop && vixSpike) || m.EligibleRedClusters >= 1
	switch {
	case spyCrash && confirmed:
		return canaryRow("Index tape shock", risk.DirectionDefensive, risk.SeverityAct, "Cut broad equity beta now; SPY is in a severe direct tape drawdown with confirmation.", evidence)
	case spyHardDrop && confirmed:
		return canaryRow("Index tape shock", risk.DirectionDefensive, risk.SeverityAct, "Cut marginal longs and pre-hedge remaining beta; direct SPY stress is confirmed.", evidence)
	case vixHardSpike && (spyDrop || m.EligibleRedClusters >= 1):
		return canaryRow("Index tape shock", risk.DirectionDefensive, risk.SeverityAct, "Reduce short-vol and high-beta exposure; direct VIX stress is confirmed.", evidence)
	case spyHardDrop || vixHardSpike || (spyDrop && vixSpike):
		return canaryRow("Index tape shock", risk.DirectionDefensive, risk.SeverityWatch, "Freeze new risk and run a second pass; direct overnight tape stress needs confirmation before urgent action.", evidence)
	case spyDrop || vixSpike:
		return canaryRow("Index tape shock", risk.DirectionDefensive, risk.SeverityWatch, "Freeze new risk; direct SPY/VIX tape is flashing early stress but not enough for defensive action alone.", evidence)
	default:
		return canaryRow("Index tape shock", "", risk.SeverityObserve, "No direct SPY/VIX overnight tape shock.", evidence)
	}
}

func canaryExposureRow(p StressPortfolioSummary, m StressMarketSummary) StressRow {
	gross := derefPct(p.GrossExposurePctNLV)
	delta := derefPct(p.NetDeltaPctNLV)
	grossDelta := derefPct(p.GrossDeltaPctNLV)
	evidence := fmt.Sprintf("gross %.0f%% NLV (watch %.0f%%); net delta %.0f%% NLV (watch %.0f%%); gross delta %.0f%% NLV (watch %.0f%%)",
		gross, canaryPolicy.GrossExposureWatchPct, delta, canaryPolicy.NetDeltaWatchPct, grossDelta, canaryPolicy.GrossDeltaWatchPct)
	stressed := m.EligibleRedClusters >= 2 || canaryConfirmedTapeStress(m)
	switch {
	case (gross >= canaryPolicy.GrossExposureStressUrgentPct || delta >= canaryPolicy.NetDeltaStressUrgentPct || grossDelta >= canaryPolicy.GrossDeltaStressUrgentPct) && stressed:
		return canaryRow("US equity/options exposure", risk.DirectionDefensive, risk.SeverityUrgent, "Go near-flat on broad equity beta; close or hedge option delta first.", evidence)
	case (gross >= canaryPolicy.GrossExposureStressActPct || delta >= canaryPolicy.NetDeltaStressActPct || grossDelta >= canaryPolicy.GrossDeltaStressActPct) && stressed:
		return canaryRow("US equity/options exposure", risk.DirectionDefensive, risk.SeverityAct, "Cut 30-50% of net equity delta and avoid adding long gamma-dollar exposure.", evidence)
	case gross >= canaryPolicy.GrossExposureWatchPct || delta >= canaryPolicy.NetDeltaWatchPct || grossDelta >= canaryPolicy.GrossDeltaWatchPct:
		return canaryRow("US equity/options exposure", risk.DirectionRebalance, risk.SeverityWatch, "Exposure is high; rebalance toward risk limits without treating this as confirmed market stress.", evidence)
	default:
		return canaryRow("US equity/options exposure", "", risk.SeverityObserve, "No exposure-based de-risking trigger.", evidence)
	}
}

func canaryConcentrationRow(p StressPortfolioSummary, m StressMarketSummary) StressRow {
	if (p.LargestExposurePct == nil || p.LargestExposure == "") && (p.LargestDeltaPctNLV == nil || p.LargestDeltaExposure == "") {
		return canaryRow("Largest concentration", "", risk.SeverityObserve, "No concentration action from available base-currency exposure map.", "no dominant exposure")
	}
	pct := math.Abs(derefPct(p.LargestExposurePct))
	deltaPct := derefPct(p.LargestDeltaPctNLV)
	evidence := canaryConcentrationEvidence(p)
	if (pct >= canaryPolicy.SingleNameExposureWatchPct || deltaPct >= canaryPolicy.SingleNameDeltaWatchPct) && (m.EligibleRedClusters >= 2 || canaryConfirmedTapeStress(m)) {
		return canaryRow("Largest concentration", risk.DirectionDefensive, risk.SeverityAct, fmt.Sprintf("Trim this concentration before smaller positions; cap it below %.0f%% NLV in stress.", canaryPolicy.SingleNameTargetPct), evidence)
	}
	if pct >= canaryPolicy.SingleNameExposureWatchPct || deltaPct >= canaryPolicy.SingleNameDeltaWatchPct {
		return canaryRow("Largest concentration", risk.DirectionRebalance, risk.SeverityWatch, "Concentration is above risk limits; rebalance this position without treating it as confirmed market stress.", evidence)
	}
	return canaryRow("Largest concentration", "", risk.SeverityObserve, "No concentration trim required by the canary.", evidence)
}

func canaryProtectionCoverageRow(p StressPortfolioSummary) StressRow {
	coverage := p.ProtectionCoverage
	if coverage == nil {
		return canaryRow("Protection coverage", risk.DirectionDataQuality, risk.SeverityWatch, "Protection coverage is unavailable; use positions risk and open orders before relying on stop coverage.", "coverage unavailable")
	}
	evidence := formatProtectionCoverageEvidence(coverage)
	if coverage.Counts.OrphanedOrder > 0 || coverage.Counts.ReconcileRequired > 0 {
		return canaryRow("Protection coverage", risk.DirectionRebalance, risk.SeverityWatch, "Reconcile stale protective orders before counting them as coverage.", evidence)
	}
	if coverage.Counts.Unprotected > 0 || coverage.Counts.Partial > 0 {
		guidance := "Review largest unprotected stock/ETF exposures before adding risk."
		// Naming the position turns "go look somewhere" into a decision the
		// row itself supports; the phrase shape mirrors the Monitor protection
		// panel ("largest unprotected SYM amount").
		if largest := largestUnprotectedPhrase(coverage); largest != "" {
			guidance = "Review largest unprotected stock/ETF exposures before adding risk; largest unprotected " + largest + "."
		}
		return canaryRow("Protection coverage", risk.DirectionRebalance, risk.SeverityWatch, guidance, evidence)
	}
	if coverage.Counts.Unknown > 0 || coverage.Status == rpc.ProtectionCoverageStateUnknown {
		return canaryRow("Protection coverage", risk.DirectionDataQuality, risk.SeverityWatch, "Open-order coverage is unknown; confirm open orders before relying on stop coverage.", evidence)
	}
	return canaryRow("Protection coverage", "", risk.SeverityObserve, "No stock/ETF protection coverage issue in the current open-order ledger.", evidence)
}

func canaryHeldStressRow(p StressPortfolioSummary, m StressMarketSummary) StressRow {
	if len(p.HeldStress) == 0 {
		return canaryRow("Held-name stress", "", risk.SeverityObserve, "No material held-name stress from existing positions data.", "no material held-name stress")
	}
	signals := canaryHeldStressSignals(p.HeldStress, m)
	direction, severity := canaryHeldStressRowState(signals)
	evidence := canaryHeldStressEvidence(p.HeldStress)
	switch direction {
	case risk.DirectionDefensive:
		return canaryRow("Held-name stress", direction, severity, "Held-name stress aligns with confirmed market pressure; review material underlyings before smaller positions.", evidence)
	case risk.DirectionRebalance:
		return canaryRow("Held-name stress", direction, severity, "Review material held names before adding risk; rebalance stressed names without treating this as market-confirmed defense.", evidence)
	case risk.DirectionDataQuality:
		return canaryRow("Held-name stress", direction, severity, "Confirm held-name quotes and option bid/ask context before acting on those names.", evidence)
	default:
		return canaryRow("Held-name stress", "", risk.SeverityObserve, "No material held-name stress from existing positions data.", evidence)
	}
}

func canaryHeldStressRowState(signals []risk.Signal) (risk.SignalDirection, risk.SignalSeverity) {
	var best *risk.Signal
	for i := range signals {
		if signals[i].Direction == risk.DirectionDataQuality {
			continue
		}
		if best == nil || signalSeverityRank(signals[i].Severity) > signalSeverityRank(best.Severity) {
			best = &signals[i]
		}
	}
	if best == nil {
		for i := range signals {
			if best == nil || signalSeverityRank(signals[i].Severity) > signalSeverityRank(best.Severity) {
				best = &signals[i]
			}
		}
	}
	if best == nil {
		return "", risk.SeverityObserve
	}
	return best.Direction, best.Severity
}

func canaryOptionsRow(p StressPortfolioSummary, pos rpc.PositionsResult, m StressMarketSummary) StressRow {
	if pos.Portfolio == nil || pos.Portfolio.GreeksTotal == 0 {
		if len(pos.Options) > 0 {
			return canaryRow("Options convexity", risk.DirectionDataQuality, risk.SeverityWatch, "Option positions are present but greeks coverage is unavailable; do not escalate options-specific actions from this snapshot.", "option greeks unavailable")
		}
		return canaryRow("Options convexity", "", risk.SeverityObserve, "No option-greeks action from the current portfolio snapshot.", "no option greeks required")
	}
	coverage := float64(pos.Portfolio.GreeksCoverage) / float64(pos.Portfolio.GreeksTotal) * 100
	evidence := fmt.Sprintf("greeks %.0f%% covered (%s)", coverage, p.OptionGreeks)
	if coverage < canaryPolicy.OptionGreeksMinCoveragePct {
		return canaryRow("Options convexity", risk.DirectionDataQuality, risk.SeverityWatch, fmt.Sprintf("Do not escalate options-specific actions until greeks coverage is at least %.0f%%.", canaryPolicy.OptionGreeksMinCoveragePct), evidence)
	}
	if pos.Portfolio.Gamma != nil && *pos.Portfolio.Gamma < 0 && m.EligibleRedClusters >= 2 {
		return canaryRow("Options convexity", risk.DirectionDefensive, risk.SeverityAct, "Reduce negative-gamma structures first; prefer defined-risk or hedged residuals.", evidence)
	}
	return canaryRow("Options convexity", "", risk.SeverityObserve, "No option-convexity de-risking trigger.", evidence)
}

func canaryDataQualityRow(m StressMarketSummary, r rpc.RegimeSnapshotResult) StressRow {
	if canaryHasMarketDataIssue(m) && (m.RedClusters > 0 || m.YellowClusters > 0) {
		return canaryRow("Ambiguity filter", risk.DirectionDataQuality, risk.SeverityWatch, "Some market inputs cannot be confirmed right now; treat the stress readings as tentative until those inputs report.", canaryAmbiguityEvidence(m))
	}
	if canaryHasMarketDataIssue(m) {
		return canaryRow("Ambiguity filter", risk.DirectionDataQuality, risk.SeverityWatch, "Some market inputs are incomplete; treat this snapshot as partial until coverage and freshness recover.", canaryAmbiguityEvidence(m))
	}
	if m.RankedClusters < 4 {
		return canaryRow("Data quality gate", risk.DirectionDataQuality, risk.SeverityWatch, "Verify market coverage before action; fewer than four of six market clusters are reporting.", canaryMarketEvidence(m))
	}
	if r.GammaZero.Status == rpc.RegimeStatusComputing || r.Breadth.Status == rpc.RegimeStatusComputing {
		return canaryRow("Data quality gate", risk.DirectionDataQuality, risk.SeverityWatch, "Do not escalate on gamma/breadth while their data is still computing.", canaryMarketEvidence(m))
	}
	return canaryRow("Data quality gate", "", risk.SeverityObserve, "Market data coverage is sufficient for the canary policy.", canaryMarketEvidence(m))
}

func canaryOverallRow(direction risk.SignalDirection, severity risk.SignalSeverity, summary string, m StressMarketSummary, p StressPortfolioSummary) StressRow {
	return canaryRow("Portfolio canary", direction, severity, summary, fmt.Sprintf("%s; %s", canaryMarketEvidence(m), canaryPortfolioEvidence(p)))
}

const (
	canaryMarketNone      = "none"
	canaryMarketPartial   = "partial"
	canaryMarketConfirmed = "confirmed"
	canaryMarketBlocked   = "blocked"

	canaryPortfolioFitUnknown = "unknown"
	canaryPortfolioFitLow     = "low"
	canaryPortfolioFitMedium  = "medium"
	canaryPortfolioFitHigh    = "high"

	canaryInputOK       = "ok"
	canaryInputWarming  = "warming"
	canaryInputDegraded = "degraded"
	canaryInputFailed   = "failed"

	canaryActionStandDown     = "stand_down"
	canaryActionWatch         = "watch"
	canaryActionDefend        = "defend"
	canaryActionRebalance     = "rebalance"
	canaryActionDeploy        = "deploy"
	canaryActionConfirmInputs = "confirm_inputs"
)

func canaryMarketConfirmation(m StressMarketSummary) string {
	if m.RankedClusters < 4 {
		return canaryMarketBlocked
	}
	if canaryConfirmedMarketStress(m) || canaryConfirmedConstructiveTape(m) {
		return canaryMarketConfirmed
	}
	if canaryPartialMarketPressure(m) || canaryPartialConstructiveTape(m) {
		return canaryMarketPartial
	}
	return canaryMarketNone
}

func canaryConfirmedMarketStress(m StressMarketSummary) bool {
	return (m.EligibleRedClusters >= 2 && len(unhealthyConfirmingClusters(m)) == 0) || canaryConfirmedTapeStress(m)
}

func canaryPartialMarketPressure(m StressMarketSummary) bool {
	return m.RedClusters >= 1 ||
		m.YellowClusters >= 3 ||
		len(m.UnconfirmedRedClusterNames) > 0 ||
		(canaryTapeConfirmable(m) &&
			(pctAtMost(m.SPYChangePct, canaryPolicy.SPYDropPct) ||
				pctAtLeast(m.VIXChangePct, canaryPolicy.VIXSpikePct)))
}

func canaryConfirmedConstructiveTape(m StressMarketSummary) bool {
	return canaryTapeConfirmable(m) &&
		(pctAtLeast(m.SPYChangePct, canaryPolicy.SPYHardRallyPct) ||
			pctAtMost(m.VIXChangePct, canaryPolicy.VIXHardCrushPct))
}

func canaryPartialConstructiveTape(m StressMarketSummary) bool {
	return canaryTapeConfirmable(m) &&
		(pctAtLeast(m.SPYChangePct, canaryPolicy.SPYRallyPct) ||
			pctAtMost(m.VIXChangePct, canaryPolicy.VIXCrushPct))
}

func canaryPortfolioFit(p StressPortfolioSummary, signals []risk.Signal) string {
	if p.NetLiquidation <= 0 {
		return canaryPortfolioFitUnknown
	}
	hasMedium := false
	blindExposure := false
	for _, sig := range signals {
		if len(sig.BlockedBy) > 0 || sig.Direction == risk.DirectionDataQuality {
			// A skipped exposure-family signal means the classifier is blind
			// on that axis. "Low" must remain a measurement; when the
			// measuring signals themselves are blocked or data-quality, the
			// honest default is unknown, never low.
			switch sig.ID {
			case risk.SignalGrossExposureHigh,
				risk.SignalNetDeltaHigh,
				risk.SignalGrossDeltaHigh,
				risk.SignalSingleNameExposureHigh,
				risk.SignalSingleNameDeltaHigh,
				risk.SignalShortConvexityHigh,
				risk.SignalOptionGreeksDegraded:
				blindExposure = true
			}
			continue
		}
		switch sig.ID {
		case risk.SignalGrossExposureHigh,
			risk.SignalNetDeltaHigh,
			risk.SignalGrossDeltaHigh,
			risk.SignalSingleNameExposureHigh,
			risk.SignalSingleNameDeltaHigh,
			risk.SignalHeldUnderlyingPnLShock,
			risk.SignalHeldOptionExpiryConcentration,
			risk.SignalShortConvexityHigh:
			return canaryPortfolioFitHigh
		case risk.SignalMarginCushionLow,
			risk.SignalLookAheadCushionLow,
			risk.SignalPortfolioPnLShock,
			risk.SignalOptionGreeksDegraded:
			hasMedium = true
		}
	}
	if hasMedium {
		return canaryPortfolioFitMedium
	}
	if blindExposure {
		return canaryPortfolioFitUnknown
	}
	return canaryPortfolioFitLow
}

// canaryPortfolioAlertRelevant is the single policy copy for "does this
// snapshot concern the live portfolio enough to alert on": only a low-fit,
// flat book (no held stress, every exposure print under 0.5% NLV) is market
// weather rather than a portfolio alert. Unknown fit stays relevant — an
// unmeasurable portfolio must never be silenced. The app alert gate and the
// SPA preview gate read the stamped PortfolioAlertRelevant field instead of
// re-deriving these edge cases.
func canaryPortfolioAlertRelevant(r *StressResult) bool {
	if r.PortfolioFit != canaryPortfolioFitLow {
		return true
	}
	p := r.Portfolio
	if len(p.HeldStress) > 0 {
		return true
	}
	for _, value := range []*float64{
		p.GrossExposurePctNLV,
		p.NetDeltaPctNLV,
		p.GrossDeltaPctNLV,
		p.LargestExposurePct,
		p.LargestDeltaPctNLV,
	} {
		if value != nil && math.Abs(*value) >= 0.5 {
			return true
		}
	}
	return false
}

func canaryInputHealth(in StressInput, m StressMarketSummary, sourceIssues []canarySourceIssue, now time.Time) string {
	switch {
	case in.Account.NetLiquidation <= 0:
		return canaryInputFailed
	case canaryAccountDailyPnLFailed(in.Account, now):
		if in.Account.DailyPnLObservation != nil {
			return canaryInputDegraded
		}
		return canaryInputWarming
	case len(sourceIssues) > 0 || canaryHasMarketDataIssue(m):
		return canaryInputDegraded
	default:
		return canaryInputOK
	}
}

func canaryDecisionState(marketConfirmation, portfolioFit, inputHealth string, m StressMarketSummary, signals []risk.Signal) (risk.SignalDirection, risk.SignalSeverity) {
	if inputHealth == canaryInputFailed || marketConfirmation == canaryMarketBlocked {
		return risk.DirectionDataQuality, risk.SeverityWatch
	}
	if canaryHasConfirmedConstructiveSignal(signals) && inputHealth == canaryInputOK && portfolioFit == canaryPortfolioFitLow {
		return risk.DirectionConstructive, risk.SeverityWatch
	}
	if marketConfirmation == canaryMarketConfirmed && portfolioFit == canaryPortfolioFitHigh {
		if inputHealth == canaryInputOK {
			if canaryHasUrgentPortfolioShape(signals) {
				return risk.DirectionDefensive, risk.SeverityUrgent
			}
			if canaryPanicMarket(m) {
				return risk.DirectionDefensive, risk.SeverityUrgent
			}
			return risk.DirectionDefensive, risk.SeverityAct
		}
		return risk.DirectionDefensive, risk.SeverityWatch
	}
	if marketConfirmation == canaryMarketConfirmed && portfolioFit == canaryPortfolioFitMedium {
		return risk.DirectionDefensive, risk.SeverityWatch
	}
	if marketConfirmation == canaryMarketConfirmed && portfolioFit == canaryPortfolioFitLow {
		return risk.DirectionDefensive, risk.SeverityWatch
	}
	if marketConfirmation == canaryMarketPartial && (portfolioFit == canaryPortfolioFitHigh || portfolioFit == canaryPortfolioFitMedium) {
		return risk.DirectionDefensive, risk.SeverityWatch
	}
	if marketConfirmation == canaryMarketPartial && portfolioFit == canaryPortfolioFitLow {
		return risk.DirectionDefensive, risk.SeverityWatch
	}
	// Unmeasured exposure against live market pressure keeps the defensive
	// watch frame: the market signal is real and must not be demoted to a
	// data-quality footnote just because the portfolio side is blind.
	if (marketConfirmation == canaryMarketConfirmed || marketConfirmation == canaryMarketPartial) && portfolioFit == canaryPortfolioFitUnknown {
		return risk.DirectionDefensive, risk.SeverityWatch
	}
	if marketConfirmation == canaryMarketNone && portfolioFit == canaryPortfolioFitHigh {
		return risk.DirectionRebalance, risk.SeverityWatch
	}
	if inputHealth == canaryInputWarming || inputHealth == canaryInputDegraded {
		return risk.DirectionDataQuality, risk.SeverityWatch
	}
	return "", risk.SeverityObserve
}

func canaryHasUrgentPortfolioShape(signals []risk.Signal) bool {
	for _, sig := range signals {
		if len(sig.BlockedBy) > 0 || !severityRankAtLeast(sig.Severity, risk.SeverityUrgent) {
			continue
		}
		switch sig.ID {
		case risk.SignalGrossExposureHigh,
			risk.SignalNetDeltaHigh,
			risk.SignalGrossDeltaHigh,
			risk.SignalSingleNameExposureHigh,
			risk.SignalSingleNameDeltaHigh,
			risk.SignalShortConvexityHigh:
			return true
		}
	}
	return false
}

func canaryHasConfirmedConstructiveSignal(signals []risk.Signal) bool {
	for _, sig := range signals {
		if sig.Direction == risk.DirectionConstructive && severityRankAtLeast(sig.Severity, risk.SeverityWatch) && len(sig.BlockedBy) == 0 {
			return true
		}
	}
	return false
}

func canaryPanicMarket(m StressMarketSummary) bool {
	return m.EligibleRedClusters >= 3 ||
		(canaryTapeConfirmable(m) &&
			(pctAtMost(m.SPYChangePct, canaryPolicy.SPYCrashPct) ||
				(pctAtLeast(m.VIXChangePct, canaryPolicy.VIXHardSpikePct) && m.EligibleRedClusters >= 1)))
}

func canaryAction(direction risk.SignalDirection, severity risk.SignalSeverity, marketConfirmation, portfolioFit, inputHealth string) string {
	if inputHealth == canaryInputFailed || marketConfirmation == canaryMarketBlocked {
		return canaryActionConfirmInputs
	}
	if direction == risk.DirectionDataQuality {
		if portfolioFit == canaryPortfolioFitHigh && marketConfirmation == canaryMarketPartial {
			return canaryActionWatch
		}
		return canaryActionConfirmInputs
	}
	if direction == risk.DirectionDefensive {
		if severityRankAtLeast(severity, risk.SeverityAct) && marketConfirmation == canaryMarketConfirmed && portfolioFit == canaryPortfolioFitHigh && inputHealth == canaryInputOK {
			return canaryActionDefend
		}
		return canaryActionWatch
	}
	if direction == risk.DirectionRebalance {
		return canaryActionRebalance
	}
	if direction == risk.DirectionConstructive {
		if marketConfirmation == canaryMarketConfirmed && inputHealth == canaryInputOK {
			return canaryActionDeploy
		}
		return canaryActionWatch
	}
	if severity == risk.SeverityWatch {
		return canaryActionWatch
	}
	return canaryActionStandDown
}

func canaryPlannerModeFromAction(action string) risk.PlannerMode {
	switch action {
	case canaryActionConfirmInputs:
		return risk.PlannerModeConfirmData
	case canaryActionDefend:
		return risk.PlannerModeDefend
	case canaryActionRebalance:
		return risk.PlannerModeRebalance
	case canaryActionDeploy:
		return risk.PlannerModeDeploy
	case canaryActionWatch:
		return risk.PlannerModeStage
	default:
		return risk.PlannerModeNone
	}
}

func canaryPlannerReadinessFromAction(action string, severity risk.SignalSeverity, inputHealth string) risk.PlannerReadiness {
	switch action {
	case canaryActionConfirmInputs:
		return risk.PlannerReadinessBlocked
	case canaryActionDefend, canaryActionDeploy:
		if inputHealth == canaryInputOK {
			return risk.PlannerReadinessReady
		}
		return risk.PlannerReadinessPrestage
	case canaryActionRebalance:
		return risk.PlannerReadinessReady
	case canaryActionWatch:
		return risk.PlannerReadinessPrestage
	default:
		if severity == risk.SeverityWatch {
			return risk.PlannerReadinessWatch
		}
		return risk.PlannerReadinessNone
	}
}

func canaryDecisionSummary(r StressResult) string {
	switch r.Action {
	case canaryActionDefend:
		return "Market stress is confirmed against a vulnerable portfolio; review defensive actions."
	case canaryActionWatch:
		if r.PortfolioFit == canaryPortfolioFitLow {
			if r.MarketConfirmation == canaryMarketConfirmed {
				return "Market stress is confirmed, but your exposure is low; keep watching — no reductions needed."
			}
			return canaryPartialMarketSummary(r.Market) + ", but your exposure is low; keep watching — no reductions needed."
		}
		if r.PortfolioFit == canaryPortfolioFitUnknown {
			head := "Market stress is confirmed"
			if r.MarketConfirmation != canaryMarketConfirmed {
				head = canaryPartialMarketSummary(r.Market)
			}
			return head + ", and your portfolio exposure could not be measured from this snapshot; verify exposure before relying on this reading."
		}
		if r.MarketConfirmation == canaryMarketPartial {
			return canaryPartialMarketSummary(r.Market) + " and the portfolio is exposed; freeze new risk and stage reductions."
		}
		return "Watch this portfolio against market weather; do not run a major action from this snapshot alone."
	case canaryActionRebalance:
		return "Portfolio shape is outside risk limits, but market stress is not confirmed; rebalance through the portfolio-risk workflow."
	case canaryActionDeploy:
		return "Constructive pressure is present and input health is clean; deploy only inside risk budget."
	case canaryActionConfirmInputs:
		return "Confirm input health before treating the canary as a market-context signal."
	default:
		return "No market-context canary action."
	}
}

func canaryPartialMarketSummary(m StressMarketSummary) string {
	if m.EligibleRedClusters == 0 && len(m.UnconfirmedRedClusterNames) > 0 {
		return "An early market warning is flashing (" + canaryClusterList(m.UnconfirmedRedClusterNames) + " red, not confirmed yet)"
	}
	return "Market pressure is building"
}

func canarySignals(p StressPortfolioSummary, pos rpc.PositionsResult, m StressMarketSummary, r rpc.RegimeSnapshotResult) []risk.Signal {
	signals := []risk.Signal{}
	signals = append(signals, canaryMarginSignals(p)...)
	signals = append(signals, canaryPnLSignals(p)...)
	signals = append(signals, canaryTapeSignals(p, m)...)
	signals = append(signals, canaryRegimeSignals(m)...)
	signals = append(signals, canaryExposureSignals(p, m)...)
	signals = append(signals, canaryConcentrationSignals(p, m)...)
	signals = append(signals, canaryHeldStressSignals(p.HeldStress, m)...)
	signals = append(signals, canaryOptionSignals(pos, m)...)
	signals = append(signals, canaryDataQualitySignals(m, r)...)
	for i := range signals {
		if signals[i].Posture == "" {
			signals[i].Posture = canarySignalPosture(signals[i].Direction)
		}
	}
	return signals
}

func canaryMarginSignals(p StressPortfolioSummary) []risk.Signal {
	out := []risk.Signal{}
	addCushion := func(id risk.SignalID, metric string, observed *float64) {
		if observed == nil {
			return
		}
		severity, threshold, ok := canaryCushionSeverity(*observed)
		if !ok {
			return
		}
		out = append(out, risk.Signal{
			ID:         id,
			Direction:  risk.DirectionDefensive,
			Severity:   severity,
			Metric:     metric,
			Observed:   observed,
			Threshold:  new(threshold),
			Unit:       "pct_nlv",
			Evidence:   pctEvidence(metric, *observed),
			Confidence: "high",
		})
		if severity == risk.SeverityAct || severity == risk.SeverityUrgent {
			out[len(out)-1].Target = new(canaryPolicy.MarginTargetPct)
		}
	}
	addCushion(risk.SignalMarginCushionLow, "cushion", p.CushionPct)
	addCushion(risk.SignalLookAheadCushionLow, "lookahead_cushion", p.LookAheadCushionPct)
	return out
}

func canaryCushionSeverity(v float64) (risk.SignalSeverity, float64, bool) {
	switch {
	case v < canaryPolicy.MarginUrgentPct:
		return risk.SeverityUrgent, canaryPolicy.MarginUrgentPct, true
	case v < canaryPolicy.MarginActPct:
		return risk.SeverityAct, canaryPolicy.MarginActPct, true
	case v < canaryPolicy.MarginWatchPct:
		return risk.SeverityWatch, canaryPolicy.MarginWatchPct, true
	default:
		return "", 0, false
	}
}

func canaryPnLSignals(p StressPortfolioSummary) []risk.Signal {
	if p.DailyPnLPct == nil {
		return []risk.Signal{{
			ID:               risk.SignalRiskDataDegraded,
			Direction:        risk.DirectionDataQuality,
			Severity:         risk.SeverityWatch,
			Subject:          "account.daily_pnl",
			Metric:           "daily_pnl_pct_nlv",
			Evidence:         "daily P&L unavailable",
			Confidence:       "medium-low",
			ConfidenceImpact: "P&L shock indicator unavailable",
			BlockedBy:        []string{"account.daily_pnl"},
		}}
	}
	pct := *p.DailyPnLPct
	absPct := math.Abs(pct)
	if absPct < canaryPolicy.DailyPnLWatchPct {
		return nil
	}
	direction := risk.DirectionDefensive
	severity := risk.SeverityWatch
	threshold := canaryPolicy.DailyPnLWatchPct
	confidenceImpact := ""
	if pct < 0 && absPct >= canaryPolicy.DailyPnLActPct {
		severity = risk.SeverityAct
		threshold = canaryPolicy.DailyPnLActPct
	} else if pct > 0 && absPct >= canaryPolicy.DailyPnLActPct {
		threshold = canaryPolicy.DailyPnLActPct
		confidenceImpact = "protect gains; not deployable without clean risk budget"
	}
	return []risk.Signal{{
		ID:               risk.SignalPortfolioPnLShock,
		Direction:        direction,
		Severity:         severity,
		Metric:           "daily_pnl_pct_nlv",
		Observed:         new(pct),
		Threshold:        new(threshold),
		Unit:             "pct_nlv",
		Evidence:         fmt.Sprintf("daily P&L %+.1f%% NLV", pct),
		Confidence:       "high",
		ConfidenceImpact: confidenceImpact,
	}}
}

func canaryTapeSignals(p StressPortfolioSummary, m StressMarketSummary) []risk.Signal {
	out := []risk.Signal{}
	if !canaryTapeConfirmable(m) {
		// Closed market date: the frozen day-change prints stay visible as
		// evidence on the tape row, but emit no defensive or constructive
		// tape signals until live prints return at the next open.
		return out
	}
	spyDrop := pctAtMost(m.SPYChangePct, canaryPolicy.SPYDropPct)
	vixSpike := pctAtLeast(m.VIXChangePct, canaryPolicy.VIXSpikePct)
	confirmedDrop := (spyDrop && vixSpike) || m.EligibleRedClusters >= 1
	confirmedVIXSpike := spyDrop || m.EligibleRedClusters >= 1
	if m.SPYChangePct != nil {
		switch {
		case *m.SPYChangePct <= canaryPolicy.SPYCrashPct:
			severity, blockedBy := confirmedSignalSeverity(confirmedDrop)
			out = append(out, tapeSignal(risk.SignalMarketSelloffViolent, risk.DirectionDefensive, severity, "spy_change_pct", *m.SPYChangePct, canaryPolicy.SPYCrashPct, blockedBy...))
		case *m.SPYChangePct <= canaryPolicy.SPYHardDropPct:
			severity, blockedBy := confirmedSignalSeverity(confirmedDrop)
			out = append(out, tapeSignal(risk.SignalMarketSelloffViolent, risk.DirectionDefensive, severity, "spy_change_pct", *m.SPYChangePct, canaryPolicy.SPYHardDropPct, blockedBy...))
		case *m.SPYChangePct <= canaryPolicy.SPYDropPct:
			out = append(out, tapeSignal(risk.SignalMarketSelloffViolent, risk.DirectionDefensive, risk.SeverityWatch, "spy_change_pct", *m.SPYChangePct, canaryPolicy.SPYDropPct))
		case *m.SPYChangePct >= canaryPolicy.SPYHardRallyPct:
			out = append(out, tapeSignal(risk.SignalMarketRallyViolent, risk.DirectionConstructive, risk.SeverityAct, "spy_change_pct", *m.SPYChangePct, canaryPolicy.SPYHardRallyPct))
		case *m.SPYChangePct >= canaryPolicy.SPYRallyPct:
			out = append(out, tapeSignal(risk.SignalMarketRallyViolent, risk.DirectionConstructive, risk.SeverityWatch, "spy_change_pct", *m.SPYChangePct, canaryPolicy.SPYRallyPct))
		}
	}
	if m.VIXChangePct != nil {
		switch {
		case *m.VIXChangePct >= canaryPolicy.VIXHardSpikePct:
			severity, blockedBy := confirmedSignalSeverity(confirmedVIXSpike)
			out = append(out, tapeSignal(risk.SignalVolSpikeConfirmed, risk.DirectionDefensive, severity, "vix_change_pct", *m.VIXChangePct, canaryPolicy.VIXHardSpikePct, blockedBy...))
		case *m.VIXChangePct >= canaryPolicy.VIXSpikePct:
			out = append(out, tapeSignal(risk.SignalVolSpikeConfirmed, risk.DirectionDefensive, risk.SeverityWatch, "vix_change_pct", *m.VIXChangePct, canaryPolicy.VIXSpikePct))
		case *m.VIXChangePct <= canaryPolicy.VIXHardCrushPct:
			out = append(out, tapeSignal(risk.SignalVolCrushConfirmed, risk.DirectionConstructive, risk.SeverityAct, "vix_change_pct", *m.VIXChangePct, canaryPolicy.VIXHardCrushPct))
		case *m.VIXChangePct <= canaryPolicy.VIXCrushPct:
			out = append(out, tapeSignal(risk.SignalVolCrushConfirmed, risk.DirectionConstructive, risk.SeverityWatch, "vix_change_pct", *m.VIXChangePct, canaryPolicy.VIXCrushPct))
		}
	}
	return out
}

func confirmedSignalSeverity(confirmed bool) (risk.SignalSeverity, []string) {
	if confirmed {
		return risk.SeverityAct, nil
	}
	return risk.SeverityWatch, []string{"confirmation"}
}

func tapeSignal(id risk.SignalID, direction risk.SignalDirection, severity risk.SignalSeverity, metric string, observed, threshold float64, blockedBy ...string) risk.Signal {
	sig := risk.Signal{
		ID:         id,
		Direction:  direction,
		Severity:   severity,
		Metric:     metric,
		Observed:   new(observed),
		Threshold:  new(threshold),
		Unit:       "pct",
		Evidence:   fmt.Sprintf("%s %+.2f%%", metric, observed),
		Confidence: "medium",
	}
	if len(blockedBy) > 0 {
		sig.BlockedBy = append([]string(nil), blockedBy...)
		sig.ConfidenceImpact = "requires independent confirmation before action"
	}
	return sig
}

func canaryRegimeSignals(m StressMarketSummary) []risk.Signal {
	out := []risk.Signal{}
	switch {
	case m.EligibleRedClusters >= 2 && m.RankedClusters >= 3:
		// Confirmation-grade: only ELIGIBLE reds (depth + persistence +
		// freshness) may put the act-severity stress signal on the wire.
		observed := float64(m.EligibleRedClusters)
		threshold := 2.0
		sig := risk.Signal{ID: risk.SignalRegimeStressConfirmed, Direction: risk.DirectionDefensive, Severity: risk.SeverityAct, Metric: "eligible_red_clusters", Observed: &observed, Threshold: &threshold, Evidence: canaryMarketEvidence(m), Confidence: "medium"}
		if unhealthy := unhealthyConfirmingClusters(m); len(unhealthy) > 0 {
			sig.BlockedBy = unhealthy
			sig.ConfidenceImpact = "confirmed stress includes unhealthy cluster input; verify before severe market-only action"
		}
		out = append(out, sig)
	case canaryFastCarryUnwind(m):
		observed := 1.0
		threshold := 1.0
		out = append(out, risk.Signal{ID: risk.SignalFXCarryUnwind, Direction: risk.DirectionDefensive, Severity: risk.SeverityAct, Subject: "fx", Metric: "red_fx_cluster_with_tape_confirmation", Observed: &observed, Threshold: &threshold, Evidence: canaryMarketEvidence(m), Confidence: "medium"})
	case m.RedClusters >= 2 || (m.RedClusters == 1 && m.YellowClusters >= 1):
		// Visible reds without confirmation eligibility warn, never act.
		observed := float64(m.RedClusters)
		threshold := 1.0
		out = append(out, risk.Signal{ID: risk.SignalRegimeStressEarly, Direction: risk.DirectionDefensive, Severity: risk.SeverityWatch, Metric: "red_clusters", Observed: &observed, Threshold: &threshold, Evidence: canaryMarketEvidence(m), Confidence: "medium"})
	}
	if slices.Contains(m.RedClusterNames, "gamma") {
		observed := 1.0
		out = append(out, risk.Signal{ID: risk.SignalGammaRed, Direction: risk.DirectionDefensive, Severity: risk.SeverityWatch, Subject: "gamma", Metric: "red_cluster", Observed: &observed, Evidence: "gamma cluster red", Confidence: "medium", ConfidenceImpact: "lower when gamma is degraded"})
	}
	return out
}

func unhealthyConfirmingClusters(m StressMarketSummary) []string {
	out := []string{}
	for _, cluster := range canaryUniqueClusters(m.DegradedClusters, m.StaleClusters, m.PartialClusters, m.ComputingClusters) {
		if slices.Contains(m.RedClusterNames, cluster) {
			out = append(out, cluster)
		}
	}
	slices.Sort(out)
	return out
}

func canaryExposureSignals(p StressPortfolioSummary, m StressMarketSummary) []risk.Signal {
	stressed := m.EligibleRedClusters >= 2 || canaryConfirmedTapeStress(m)
	out := []risk.Signal{}
	out = appendExposureSignal(out, risk.SignalGrossExposureHigh, "gross_exposure_pct_nlv", p.GrossExposurePctNLV, canaryPolicy.GrossExposureWatchPct, canaryPolicy.GrossExposureStressActPct, canaryPolicy.GrossExposureStressUrgentPct, stressed)
	out = appendExposureSignal(out, risk.SignalNetDeltaHigh, "net_delta_pct_nlv", p.NetDeltaPctNLV, canaryPolicy.NetDeltaWatchPct, canaryPolicy.NetDeltaStressActPct, canaryPolicy.NetDeltaStressUrgentPct, stressed)
	out = appendExposureSignal(out, risk.SignalGrossDeltaHigh, "gross_delta_pct_nlv", p.GrossDeltaPctNLV, canaryPolicy.GrossDeltaWatchPct, canaryPolicy.GrossDeltaStressActPct, canaryPolicy.GrossDeltaStressUrgentPct, stressed)
	return out
}

func appendExposureSignal(out []risk.Signal, id risk.SignalID, metric string, observed *float64, watchThreshold, stressActThreshold, stressUrgentThreshold float64, stressed bool) []risk.Signal {
	if observed == nil {
		return out
	}
	threshold := watchThreshold
	severity := risk.SeverityWatch
	direction := risk.DirectionRebalance
	if stressed {
		direction = risk.DirectionDefensive
		switch {
		case *observed >= stressUrgentThreshold:
			severity = risk.SeverityUrgent
			threshold = stressUrgentThreshold
		case *observed >= stressActThreshold:
			severity = risk.SeverityAct
			threshold = stressActThreshold
		case *observed >= watchThreshold:
			severity = risk.SeverityWatch
		default:
			return out
		}
	} else if *observed < watchThreshold {
		return out
	}
	return append(out, risk.Signal{
		ID:         id,
		Direction:  direction,
		Severity:   severity,
		Metric:     metric,
		Observed:   observed,
		Threshold:  new(threshold),
		Unit:       "pct_nlv",
		Evidence:   fmt.Sprintf("%s %.0f%% NLV", metric, *observed),
		Confidence: "high",
	})
}

func canaryConcentrationSignals(p StressPortfolioSummary, m StressMarketSummary) []risk.Signal {
	stressed := m.EligibleRedClusters >= 2 || canaryConfirmedTapeStress(m)
	severity := risk.SeverityWatch
	direction := risk.DirectionRebalance
	if stressed {
		severity = risk.SeverityAct
		direction = risk.DirectionDefensive
	}
	out := []risk.Signal{}
	if p.LargestExposurePct != nil && math.Abs(*p.LargestExposurePct) >= canaryPolicy.SingleNameExposureWatchPct {
		observed := math.Abs(*p.LargestExposurePct)
		out = append(out, risk.Signal{ID: risk.SignalSingleNameExposureHigh, Direction: direction, Severity: severity, Subject: p.LargestExposure, Metric: "market_value_pct_nlv", Observed: &observed, Threshold: new(canaryPolicy.SingleNameExposureWatchPct), Target: new(canaryPolicy.SingleNameTargetPct), Unit: "pct_nlv", Evidence: fmt.Sprintf("%s market %.0f%% NLV", p.LargestExposure, observed), Confidence: "high"})
	}
	if p.LargestDeltaPctNLV != nil && *p.LargestDeltaPctNLV >= canaryPolicy.SingleNameDeltaWatchPct {
		out = append(out, risk.Signal{ID: risk.SignalSingleNameDeltaHigh, Direction: direction, Severity: severity, Subject: p.LargestDeltaExposure, Metric: "delta_pct_nlv", Observed: p.LargestDeltaPctNLV, Threshold: new(canaryPolicy.SingleNameDeltaWatchPct), Target: new(canaryPolicy.SingleNameTargetPct), Unit: "pct_nlv", Evidence: fmt.Sprintf("%s delta %.0f%% NLV", p.LargestDeltaExposure, *p.LargestDeltaPctNLV), Confidence: "high"})
	}
	return out
}

func canaryHeldStressSignals(stresses []rpc.HeldStress, m StressMarketSummary) []risk.Signal {
	out := []risk.Signal{}
	stressed := canaryConfirmedMarketStress(m)
	for _, stress := range stresses {
		subject := strings.ToUpper(strings.TrimSpace(stress.Underlying))
		if subject == "" {
			subject = "held_underlying"
		}
		direction := risk.DirectionRebalance
		if stressed {
			direction = risk.DirectionDefensive
		}
		if stress.DailyPnLPctNLV != nil && *stress.DailyPnLPctNLV <= -canaryPolicy.HeldUnderlyingPnLWatchPct {
			observed := *stress.DailyPnLPctNLV
			severity := risk.SeverityWatch
			threshold := -canaryPolicy.HeldUnderlyingPnLWatchPct
			if observed <= -canaryPolicy.HeldUnderlyingPnLActPct {
				severity = risk.SeverityAct
				threshold = -canaryPolicy.HeldUnderlyingPnLActPct
			}
			out = append(out, risk.Signal{
				ID:         risk.SignalHeldUnderlyingPnLShock,
				Direction:  direction,
				Severity:   severity,
				Subject:    subject,
				Metric:     "held_daily_pnl_pct_nlv",
				Observed:   &observed,
				Threshold:  &threshold,
				Unit:       "pct_nlv",
				Evidence:   fmt.Sprintf("%s daily P&L %+.1f%% NLV", subject, observed),
				Confidence: "medium",
			})
		}
		if stress.NearExpiryDeltaPctNLV != nil && *stress.NearExpiryDeltaPctNLV >= canaryPolicy.HeldOptionDeltaWatchPct {
			observed := *stress.NearExpiryDeltaPctNLV
			severity := risk.SeverityWatch
			threshold := canaryPolicy.HeldOptionDeltaWatchPct
			confidenceImpact := ""
			if observed >= canaryPolicy.HeldOptionDeltaActPct {
				severity = risk.SeverityAct
				threshold = canaryPolicy.HeldOptionDeltaActPct
			}
			if stressed && stress.NearExpiryGamma != nil && *stress.NearExpiryGamma < 0 {
				severity = risk.SeverityAct
				confidenceImpact = "near-expiry negative gamma can accelerate hedging needs under confirmed stress"
			}
			evidence := fmt.Sprintf("%s near-expiry option delta %.0f%% NLV", subject, observed)
			if stress.NearExpiryMinDTE != nil {
				evidence += fmt.Sprintf(" (%d DTE)", *stress.NearExpiryMinDTE)
			}
			out = append(out, risk.Signal{
				ID:               risk.SignalHeldOptionExpiryConcentration,
				Direction:        direction,
				Severity:         severity,
				Subject:          subject,
				Metric:           "near_expiry_option_delta_pct_nlv",
				Observed:         &observed,
				Threshold:        &threshold,
				Unit:             "pct_nlv",
				Evidence:         evidence,
				Confidence:       "medium",
				ConfidenceImpact: confidenceImpact,
			})
		}
		if len(stress.LiquidityFlags) > 0 {
			observed := float64(len(stress.LiquidityFlags))
			threshold := 1.0
			out = append(out, risk.Signal{
				ID:               risk.SignalHeldLiquidityDegraded,
				Direction:        risk.DirectionDataQuality,
				Severity:         risk.SeverityWatch,
				Subject:          subject,
				Metric:           "held_liquidity_flags",
				Observed:         &observed,
				Threshold:        &threshold,
				Evidence:         fmt.Sprintf("%s liquidity %s", subject, strings.Join(stress.LiquidityFlags, ",")),
				Confidence:       "medium-low",
				ConfidenceImpact: "verify held-name quote and option bid/ask context before acting on the affected name",
			})
		}
	}
	return out
}

func canaryOptionSignals(pos rpc.PositionsResult, m StressMarketSummary) []risk.Signal {
	if pos.Portfolio == nil || pos.Portfolio.GreeksTotal == 0 {
		if len(pos.Options) > 0 {
			return []risk.Signal{{
				ID:               risk.SignalOptionGreeksDegraded,
				Direction:        risk.DirectionDataQuality,
				Severity:         risk.SeverityWatch,
				Metric:           "option_greeks_coverage_pct",
				Evidence:         "option greeks unavailable",
				Confidence:       "medium-low",
				ConfidenceImpact: "blocks option-specific planning",
				BlockedBy:        []string{"option_greeks"},
			}}
		}
		return nil
	}
	out := []risk.Signal{}
	coverage := float64(pos.Portfolio.GreeksCoverage) / float64(pos.Portfolio.GreeksTotal) * 100
	if coverage < canaryPolicy.OptionGreeksMinCoveragePct {
		out = append(out, risk.Signal{ID: risk.SignalOptionGreeksDegraded, Direction: risk.DirectionDataQuality, Severity: risk.SeverityWatch, Metric: "option_greeks_coverage_pct", Observed: new(coverage), Threshold: new(canaryPolicy.OptionGreeksMinCoveragePct), Unit: "pct", Evidence: fmt.Sprintf("greeks %.0f%% covered", coverage), Confidence: "medium-low", ConfidenceImpact: "blocks option-specific planning"})
	}
	if pos.Portfolio.Gamma != nil && *pos.Portfolio.Gamma < 0 && m.EligibleRedClusters >= 2 {
		out = append(out, risk.Signal{ID: risk.SignalShortConvexityHigh, Direction: risk.DirectionDefensive, Severity: risk.SeverityAct, Metric: "portfolio_gamma", Observed: pos.Portfolio.Gamma, Evidence: "negative portfolio gamma in confirmed market stress", Confidence: "medium"})
	}
	return out
}

func canaryDataQualitySignals(m StressMarketSummary, r rpc.RegimeSnapshotResult) []risk.Signal {
	out := []risk.Signal{}
	blockedBy := canaryUniqueClusters(m.AmbiguousClusters, m.PartialClusters, m.DegradedClusters, m.ComputingClusters)
	if len(blockedBy) > 0 {
		observed := float64(len(blockedBy))
		out = append(out, risk.Signal{ID: risk.SignalRiskDataDegraded, Direction: risk.DirectionDataQuality, Severity: risk.SeverityWatch, Metric: "degraded_inputs", Observed: &observed, Evidence: canaryAmbiguityEvidence(m), Confidence: "medium-low", ConfidenceImpact: "requires verification before severe action", BlockedBy: blockedBy})
	}
	if len(m.StaleClusters) > 0 {
		observed := float64(len(m.StaleClusters))
		out = append(out, risk.Signal{ID: risk.SignalMarketDataStale, Direction: risk.DirectionDataQuality, Severity: risk.SeverityWatch, Metric: "stale_clusters", Observed: &observed, Evidence: "stale " + strings.Join(m.StaleClusters, ","), Confidence: "medium-low", ConfidenceImpact: "requires fresh data", BlockedBy: m.StaleClusters})
	}
	for _, w := range r.WarningDetails {
		if strings.TrimSpace(w.Scope) != "" && strings.Contains(strings.ToLower(w.Severity), "data") {
			out = append(out, risk.Signal{ID: risk.SignalRiskDataDegraded, Direction: risk.DirectionDataQuality, Severity: risk.SeverityWatch, Subject: w.Scope, Evidence: canaryWarningLine(w), Confidence: "medium-low", ConfidenceImpact: "source warning"})
		}
	}
	return out
}

func canaryUniqueClusters(groups ...[]string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, group := range groups {
		for _, cluster := range group {
			cluster = strings.TrimSpace(cluster)
			if cluster == "" || seen[cluster] {
				continue
			}
			seen[cluster] = true
			out = append(out, cluster)
		}
	}
	slices.Sort(out)
	return out
}

func canarySignalPosture(direction risk.SignalDirection) risk.PortfolioPosture {
	switch direction {
	case risk.DirectionDefensive:
		return risk.PortfolioPostureThreat
	case risk.DirectionConstructive:
		return risk.PortfolioPostureOpportunity
	case risk.DirectionRebalance:
		return risk.PortfolioPostureRebalance
	case risk.DirectionMixed:
		return risk.PortfolioPostureThreatOpportunity
	case risk.DirectionDataQuality:
		return risk.PortfolioPostureConfirmData
	default:
		return risk.PortfolioPostureNeutral
	}
}

type canarySourceIssue struct {
	Source string
	Status string
	Reason string
}

func canarySourceIssues(in StressInput, now time.Time) []canarySourceIssue {
	issues := []canarySourceIssue{}
	switch {
	case in.Account.AsOf.IsZero() && in.Account.NetLiquidation > 0:
		issues = append(issues, canarySourceIssue{Source: "account", Status: rpc.RegimeStatusUnavailable, Reason: "account snapshot timestamp missing"})
	case canarySourceStale(in.Account.AsOf, now):
		issues = append(issues, canarySourceIssue{Source: "account", Status: rpc.RegimeStatusStale, Reason: "account snapshot stale"})
	}
	switch {
	case in.Positions.AsOf.IsZero() && in.Account.NetLiquidation > 0:
		// A never-fetched positions snapshot against a real account is a
		// positions source problem, not a clean empty book: exposure built
		// from it is blind, so dependent signals must block and portfolio
		// fit must derive unknown instead of defaulting to low.
		issues = append(issues, canarySourceIssue{Source: "positions", Status: rpc.RegimeStatusUnavailable, Reason: "positions snapshot never fetched"})
	case canarySourceStale(in.Positions.AsOf, now):
		issues = append(issues, canarySourceIssue{Source: "positions", Status: rpc.RegimeStatusStale, Reason: "positions snapshot stale"})
	}
	if issue, ok := canaryRegimeAuthorityIssue(in.Regime); ok {
		issues = append(issues, issue)
	}
	issues = append(issues, canaryMarketEventSourceIssues(in.Positions, in.MarketEvents, now)...)
	return issues
}

// canaryEstablishedSourceIssues is the exact ad5b77b source-decision
// boundary. In particular, a missing account timestamp, Regime authority
// health, and MarketEvents required-source health were not delivery inputs.
// Keep this frozen unless the operator explicitly approves a new established
// Canary paging policy.
func canaryEstablishedSourceIssues(in StressInput, now time.Time) []canarySourceIssue {
	issues := []canarySourceIssue{}
	if canarySourceStale(in.Account.AsOf, now) {
		issues = append(issues, canarySourceIssue{Source: "account", Status: rpc.RegimeStatusStale, Reason: "account snapshot stale"})
	}
	switch {
	case in.Positions.AsOf.IsZero() && in.Account.NetLiquidation > 0:
		issues = append(issues, canarySourceIssue{Source: "positions", Status: rpc.RegimeStatusUnavailable, Reason: "positions snapshot never fetched"})
	case canarySourceStale(in.Positions.AsOf, now):
		issues = append(issues, canarySourceIssue{Source: "positions", Status: rpc.RegimeStatusStale, Reason: "positions snapshot stale"})
	}
	return issues
}

func canaryRegimeAuthorityIssue(regime rpc.RegimeSnapshotResult) (canarySourceIssue, bool) {
	if regime.AuthorityHealth == nil {
		return canarySourceIssue{}, false
	}
	health := regime.AuthorityHealth
	reason := "regime last-good authority " + string(health.Status)
	if health.FailureCode != rpc.RegimeAuthorityFailureNone {
		reason += " (" + string(health.FailureCode) + ")"
	}
	switch health.Status {
	case rpc.RegimeAuthorityFresh:
		return canarySourceIssue{}, false
	case rpc.RegimeAuthorityStale:
		return canarySourceIssue{Source: "regime", Status: rpc.RegimeStatusStale, Reason: reason}, true
	case rpc.RegimeAuthorityUnavailable:
		return canarySourceIssue{Source: "regime", Status: rpc.RegimeStatusUnavailable, Reason: reason}, true
	default:
		return canarySourceIssue{Source: "regime", Status: rpc.RegimeStatusUnavailable, Reason: "regime authority status invalid"}, true
	}
}

func canaryMarketEventSourceIssues(pos rpc.PositionsResult, events rpc.MarketEventsResult, now time.Time) []canarySourceIssue {
	// The daemon requests market-event context only for held underlyings. A
	// clean empty book therefore needs no market-event source, but a held book
	// must never turn a missing snapshot into an implicit "no flags" answer.
	if len(canaryMarketEventSymbols(pos)) == 0 {
		return nil
	}
	if !canaryHasMarketEventsInput(events) {
		return []canarySourceIssue{{
			Source: "market_events",
			Status: rpc.SourceStatusUnknown,
			Reason: "market-event snapshot missing for held underlyings",
		}}
	}

	shortStock := canaryHasShortStockExposure(pos)
	issues := []canarySourceIssue{}
	issueBySource := map[string]int{}
	seen := map[string]bool{}
	umbrellaSeen := false
	addIssue := func(source, status, reason string) {
		source = canaryMarketEventSourceName(source)
		if source == "" {
			source = "market_events"
		}
		if canaryMarketEventBorrowSource(source) && !shortStock {
			return
		}
		status = canaryMarketEventHealthStatus(status)
		if existing, ok := issueBySource[source]; ok {
			if canaryMarketEventHealthRank(status) > canaryMarketEventHealthRank(issues[existing].Status) {
				issues[existing].Status = status
				issues[existing].Reason = reason
			}
			return
		}
		issueBySource[source] = len(issues)
		issues = append(issues, canarySourceIssue{Source: source, Status: status, Reason: reason})
	}
	// The result timestamp is part of the decision contract. Child rows that
	// say OK cannot make a never-dated or stale aggregate current.
	switch {
	case events.AsOf.IsZero():
		addIssue("market_events", rpc.SourceStatusUnknown, "market-event snapshot timestamp missing")
	case canarySourceStale(events.AsOf, now):
		addIssue("market_events", rpc.SourceStatusStale, "market-event snapshot stale")
	}

	for _, health := range events.SourceHealth {
		source := canaryMarketEventSourceName(health.Source)
		if source == "" {
			source = strings.ToLower(strings.TrimSpace(health.Source))
		}
		if source == "" {
			source = "market_events"
		}
		if canaryMarketEventBorrowSource(source) && !shortStock {
			continue
		}
		seen[source] = true
		if source == "market_events" {
			umbrellaSeen = true
		}
		status := canaryMarketEventHealthStatus(health.Status)
		if status == rpc.SourceStatusOK {
			switch {
			case health.AsOf.IsZero():
				status = rpc.SourceStatusUnknown
			case canaryMarketEventHealthStale(health, now):
				status = rpc.SourceStatusStale
			}
		}
		if status != rpc.SourceStatusOK {
			addIssue(source, status, source+" source "+status)
		}
	}

	// Structured warnings are part of the source contract too. Do not trust an
	// apparently OK health row when the same result says that source failed.
	for _, warning := range events.WarningDetails {
		source := canaryMarketEventSourceName(warning.Scope + " " + warning.Code)
		if source == "" {
			source = "market_events"
		}
		if canaryMarketEventBorrowSource(source) && !shortStock {
			continue
		}
		status := rpc.SourceStatusDegraded
		if strings.Contains(strings.ToLower(warning.Code), "unavailable") {
			status = rpc.SourceStatusUnknown
		}
		addIssue(source, status, source+" source "+status)
	}

	// A detailed market-event result is expected to cover both official
	// sources. Borrow data becomes required only when short stock makes cover
	// friction relevant. An umbrella failure already represents all of them.
	if !umbrellaSeen {
		required := []string{"reg_sho_threshold", "trading_halts"}
		if shortStock {
			required = append(required, "borrow_inventory", "borrow_fee")
		}
		for _, source := range required {
			if !seen[source] {
				addIssue(source, rpc.SourceStatusUnknown, source+" source missing")
			}
		}
	}

	slices.SortStableFunc(issues, func(a, b canarySourceIssue) int {
		return strings.Compare(a.Source, b.Source)
	})
	return issues
}

func canaryHasShortStockExposure(pos rpc.PositionsResult) bool {
	for _, stock := range pos.Stocks {
		if canaryPositionIsStock(stock) && stock.Quantity < 0 {
			return true
		}
	}
	for _, group := range pos.ByUnderlying {
		if group.Stock != nil && canaryPositionIsStock(*group.Stock) && group.Stock.Quantity < 0 {
			return true
		}
	}
	return false
}

func canaryPositionIsStock(position rpc.PositionView) bool {
	secType := strings.ToUpper(strings.TrimSpace(position.SecType))
	// Empty is retained as a compatibility-safe legacy stock projection. Live
	// rows carry STOCK; explicit FUT, IND, or OPTION rows never make stock-borrow
	// evidence decision-relevant.
	return secType == "" || secType == rpc.SecTypeStock || secType == "STK" || secType == "ETF"
}

func canaryMarketEventBorrowSource(source string) bool {
	source = strings.ToLower(strings.TrimSpace(source))
	return strings.Contains(source, "borrow_inventory") || strings.Contains(source, "borrow_fee")
}

func canaryMarketEventSourceName(source string) string {
	source = strings.ToLower(strings.TrimSpace(source))
	switch {
	case strings.Contains(source, "borrow_inventory"):
		return "borrow_inventory"
	case strings.Contains(source, "borrow_fee"):
		return "borrow_fee"
	case strings.Contains(source, "reg_sho"):
		return "reg_sho_threshold"
	case strings.Contains(source, "halt"), strings.Contains(source, "luld"):
		return "trading_halts"
	case strings.Contains(source, "market_events"):
		return "market_events"
	default:
		return ""
	}
}

func canaryMarketEventHealthStatus(status string) string {
	status = strings.ToLower(strings.TrimSpace(status))
	switch status {
	case rpc.SourceStatusOK,
		rpc.SourceStatusPartial,
		rpc.SourceStatusStale,
		rpc.SourceStatusUnknown,
		rpc.SourceStatusDegraded,
		rpc.RegimeStatusError,
		rpc.RegimeStatusUnavailable:
		return status
	case "":
		return rpc.SourceStatusUnknown
	default:
		return rpc.SourceStatusDegraded
	}
}

func canaryMarketEventHealthRank(status string) int {
	switch canaryMarketEventHealthStatus(status) {
	case rpc.RegimeStatusError:
		return 7
	case rpc.RegimeStatusUnavailable:
		return 6
	case rpc.SourceStatusUnknown:
		return 5
	case rpc.SourceStatusDegraded:
		return 4
	case rpc.SourceStatusPartial:
		return 3
	case rpc.SourceStatusStale:
		return 2
	case rpc.SourceStatusOK:
		return 0
	default:
		return 1
	}
}

func canarySourceStale(asOf, now time.Time) bool {
	return !asOf.IsZero() && canarySourceAgeSeconds(now, asOf) > canarySourceMaxAgeSeconds(now)
}

// canaryMarketEventHealthStale honors the producer-authored per-source age
// contract. Daily official files must not be re-staled by Canary's generic
// 10/90-minute polling budget, while a due source at or beyond its own limit
// fails closed. AgeSeconds is authoritative because some fallbacks age the
// fetch attempt rather than the observation date. A typed not_due cadence
// takes precedence over wall-clock age, but never over an explicit non-OK
// status or a future timestamp.
func canaryMarketEventHealthStale(health rpc.SourceHealth, now time.Time) bool {
	if health.AsOf.IsZero() || now.IsZero() {
		return false
	}
	if health.AsOf.After(now.Add(time.Minute)) || health.AgeSeconds < 0 {
		return true
	}
	if health.RefreshState == rpc.SourceRefreshNotDue {
		return false
	}
	if health.MaxAgeSeconds <= 0 {
		return canarySourceStale(health.AsOf, now)
	}
	return health.AgeSeconds >= health.MaxAgeSeconds
}

func canaryApplySourceBlocks(signals []risk.Signal, issues []canarySourceIssue) []risk.Signal {
	if len(issues) == 0 {
		return signals
	}
	accountBlocked := canarySourceIssuePresent(issues, "account")
	positionsBlocked := canarySourceIssuePresent(issues, "positions")
	for i := range signals {
		if accountBlocked && canarySignalDependsOnAccount(signals[i].ID) {
			canaryBlockSignal(&signals[i], "account", "requires fresh account snapshot")
		}
		if positionsBlocked && canarySignalDependsOnPositions(signals[i].ID) {
			canaryBlockSignal(&signals[i], "positions", "requires fresh positions snapshot")
		}
	}
	return signals
}

func canarySourceIssuePresent(issues []canarySourceIssue, source string) bool {
	for _, issue := range issues {
		if issue.Source == source {
			return true
		}
	}
	return false
}

func canarySignalDependsOnAccount(id risk.SignalID) bool {
	switch id {
	case risk.SignalMarginCushionLow,
		risk.SignalLookAheadCushionLow,
		risk.SignalPortfolioPnLShock,
		risk.SignalGrossExposureHigh:
		return true
	default:
		return false
	}
}

func canarySignalDependsOnPositions(id risk.SignalID) bool {
	switch id {
	case risk.SignalNetDeltaHigh,
		risk.SignalGrossDeltaHigh,
		risk.SignalSingleNameExposureHigh,
		risk.SignalSingleNameDeltaHigh,
		risk.SignalHeldUnderlyingPnLShock,
		risk.SignalHeldOptionExpiryConcentration,
		risk.SignalHeldLiquidityDegraded,
		risk.SignalOptionGreeksDegraded,
		risk.SignalShortConvexityHigh:
		return true
	default:
		return false
	}
}

func canaryBlockSignal(sig *risk.Signal, source, impact string) {
	if sig == nil {
		return
	}
	sig.BlockedBy = appendUniqueString(sig.BlockedBy, source)
	sig.Confidence = "medium-low"
	sig.ConfidenceImpact = impact
}

func appendUniqueString(values []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" || slices.Contains(values, value) {
		return values
	}
	values = append(values, value)
	slices.Sort(values)
	return values
}

func canarySourceDataQualitySignals(issues []canarySourceIssue) []risk.Signal {
	if len(issues) == 0 {
		return nil
	}
	blockedBy := []string{}
	for _, issue := range issues {
		blockedBy = appendUniqueString(blockedBy, issue.Source)
	}
	observed := float64(len(blockedBy))
	return []risk.Signal{{
		ID:               risk.SignalRiskDataDegraded,
		Direction:        risk.DirectionDataQuality,
		Severity:         risk.SeverityWatch,
		Metric:           "degraded_sources",
		Observed:         &observed,
		Evidence:         "degraded sources: " + strings.Join(blockedBy, ","),
		Confidence:       "medium-low",
		ConfidenceImpact: "requires healthy decision sources before acting on dependent signals",
		BlockedBy:        blockedBy,
	}}
}

// canaryEstablishedSourceDataQualitySignals preserves the exact classified
// signal fields hashed by the established Canary compatibility projection.
func canaryEstablishedSourceDataQualitySignals(issues []canarySourceIssue) []risk.Signal {
	if len(issues) == 0 {
		return nil
	}
	blockedBy := []string{}
	for _, issue := range issues {
		blockedBy = appendUniqueString(blockedBy, issue.Source)
	}
	observed := float64(len(blockedBy))
	return []risk.Signal{{
		ID:               risk.SignalRiskDataDegraded,
		Direction:        risk.DirectionDataQuality,
		Severity:         risk.SeverityWatch,
		Metric:           "stale_sources",
		Observed:         &observed,
		Evidence:         "stale sources: " + strings.Join(blockedBy, ","),
		Confidence:       "medium-low",
		ConfidenceImpact: "requires fresh account/position source before acting on dependent signals",
		BlockedBy:        blockedBy,
	}}
}

func signalSeverityRank(s risk.SignalSeverity) int {
	switch s {
	case risk.SeverityUrgent:
		return 3
	case risk.SeverityAct:
		return 2
	case risk.SeverityWatch:
		return 1
	default:
		return 0
	}
}

func severityRankAtLeast(got, want risk.SignalSeverity) bool {
	return signalSeverityRank(got) >= signalSeverityRank(want)
}

func canaryPrimaryDrivers(signals []risk.Signal) []risk.SignalID {
	type rankedSignal struct {
		id   risk.SignalID
		rank int
	}
	ranked := []rankedSignal{}
	for _, s := range signals {
		if s.Direction == risk.DirectionDataQuality {
			continue
		}
		ranked = append(ranked, rankedSignal{id: s.ID, rank: signalSeverityRank(s.Severity)})
	}
	if len(ranked) == 0 {
		for _, s := range signals {
			ranked = append(ranked, rankedSignal{id: s.ID, rank: signalSeverityRank(s.Severity)})
		}
	}
	slices.SortStableFunc(ranked, func(a, b rankedSignal) int {
		return cmp.Compare(b.rank, a.rank)
	})
	out := []risk.SignalID{}
	seen := map[risk.SignalID]bool{}
	for _, s := range ranked {
		if seen[s.id] {
			continue
		}
		seen[s.id] = true
		out = append(out, s.id)
		if len(out) == 5 {
			break
		}
	}
	return out
}

func canaryWarnings(m StressMarketSummary, r rpc.RegimeSnapshotResult, now time.Time) []string {
	var warnings []string
	detailWarnings, detailedClusters := canaryRegimeWarningDetails(r.WarningDetails, canaryMarketContextClusters(r, now))
	ambiguousClusters := canaryClustersWithoutDetailedWarning(m.AmbiguousClusters, detailedClusters)
	partialClusters := canaryClustersWithoutDetailedWarning(m.PartialClusters, detailedClusters)
	degradedClusters := canaryClustersWithoutDetailedWarning(m.DegradedClusters, detailedClusters)
	staleClusters := canaryClustersWithoutDetailedWarning(m.StaleClusters, detailedClusters)

	if len(ambiguousClusters) > 0 {
		warnings = append(warnings, "ambiguous clusters: "+canaryClusterList(ambiguousClusters))
	}
	if len(partialClusters) > 0 {
		warnings = append(warnings, "partial clusters: "+canaryClusterList(partialClusters))
	}
	if len(degradedClusters) > 0 {
		warnings = append(warnings, "degraded clusters: "+canaryClusterList(degradedClusters))
	}
	if len(staleClusters) > 0 {
		warnings = append(warnings, "stale clusters: "+canaryClusterList(staleClusters))
	}
	warnings = append(warnings, detailWarnings...)
	return warnings
}

func canaryMarketEventWarnings(issues []canarySourceIssue) []string {
	warnings := []string{}
	for _, issue := range issues {
		if issue.Source == "account" || issue.Source == "positions" {
			continue
		}
		warnings = append(warnings, fmt.Sprintf("market-event source %s: %s", issue.Source, issue.Status))
	}
	return warnings
}

func canaryRegimeWarningDetails(details []rpc.RegimeWarning, contextClusters map[string]bool) ([]string, map[string]bool) {
	lines := []string{}
	clusters := map[string]bool{}
	for _, w := range details {
		if canaryRegimeWarningIsContext(w, contextClusters) {
			continue
		}
		line := canaryWarningLine(w)
		if line == "" {
			continue
		}
		lines = append(lines, line)
		if cluster := canaryRegimeWarningCluster(w); cluster != "" {
			clusters[cluster] = true
		}
	}
	return lines, clusters
}

func canaryRegimeWarningIsContext(w rpc.RegimeWarning, contextClusters map[string]bool) bool {
	lower := strings.ToLower(strings.Join([]string{w.Code, w.Scope, w.Severity, w.Message, w.Impact, w.Action}, " "))
	if contextClusters["gamma"] && strings.Contains(lower, "gamma") &&
		(strings.Contains(lower, "context_only") || strings.Contains(lower, "context only") || strings.Contains(lower, "displayed as context")) {
		return true
	}
	if contextClusters["vol"] && strings.Contains(lower, "vix_term_structure") &&
		(strings.Contains(lower, "stale") || strings.Contains(lower, "no spot tick") || strings.Contains(lower, "calculation hours")) {
		return true
	}
	return false
}

func canaryClustersWithoutDetailedWarning(clusters []string, detailed map[string]bool) []string {
	if len(clusters) == 0 || len(detailed) == 0 {
		return clusters
	}
	out := []string{}
	for _, cluster := range clusters {
		if detailed[cluster] {
			continue
		}
		out = append(out, cluster)
	}
	return out
}

func canaryRegimeWarningCluster(w rpc.RegimeWarning) string {
	text := strings.ToLower(strings.TrimSpace(w.Scope + " " + w.Code))
	switch {
	case strings.Contains(text, "gamma"):
		return "gamma"
	case strings.Contains(text, "funding"):
		return "funding"
	case strings.Contains(text, "vix") || strings.Contains(text, "vvix") || strings.Contains(text, "vol_of_vol"):
		return "vol"
	case strings.Contains(text, "hyg") || strings.Contains(text, "credit") || strings.Contains(text, "oas"):
		return "credit"
	case strings.Contains(text, "usd") || strings.Contains(text, "jpy") || strings.Contains(text, "fx"):
		return "fx"
	case strings.Contains(text, "breadth"):
		return "breadth"
	default:
		return ""
	}
}

// canaryEstablishedSourceHealth preserves the exact ad5b77b source-health
// projection that participates in the established Canary fingerprint. New authority and
// required-source interpretations stay on the main Canary result only.
func canaryEstablishedSourceHealth(in StressInput, now time.Time, accountFP, positionsFP, regimeFP, marketEventsFP rpc.Fingerprint, inputHealth string, m StressMarketSummary) []rpc.SourceHealth {
	out := []rpc.SourceHealth{
		canaryAccountSourceHealth(in.Account, now, accountFP),
		canaryTimedSourceHealth("positions", in.Positions.AsOf, now, positionsFP, canaryPositionsSourceStatus(in.Positions, now), canaryPositionsSourceConfidence(in.Positions)),
		canaryEstablishedRegimeSourceHealth(in.Regime.AsOf, now, regimeFP, canaryInputHealthConfidence(inputHealth), m),
	}
	if canaryHasMarketEventsInput(in.MarketEvents) {
		out = append(out, canaryEstablishedMarketEventsSourceHealth(in.Positions, in.MarketEvents, now, marketEventsFP))
	}
	return out
}

func canaryEstablishedRegimeSourceHealth(asOf, now time.Time, fp rpc.Fingerprint, dataConfidence string, m StressMarketSummary) rpc.SourceHealth {
	status := rpc.RegimeStatusOK
	notes := []string{}
	if len(m.StaleClusters) > 0 {
		notes = append(notes, "stale clusters: "+strings.Join(m.StaleClusters, ","))
	}
	if len(m.DegradedClusters) > 0 {
		notes = append(notes, "degraded clusters: "+strings.Join(m.DegradedClusters, ","))
	}
	if len(m.PartialClusters) > 0 || len(m.AmbiguousClusters) > 0 {
		notes = append(notes, canaryAmbiguityEvidence(m))
	}
	switch {
	case len(m.PartialClusters) > 0 || len(m.AmbiguousClusters) > 0:
		status = "partial"
	case len(m.DegradedClusters) > 0:
		status = "degraded"
	case len(m.StaleClusters) > 0:
		status = rpc.RegimeStatusStale
	}
	health := canaryTimedSourceHealth("regime", asOf, now, fp, status, dataConfidence)
	health.Notes = notes
	return health
}

func canaryEstablishedMarketEventsSourceHealth(pos rpc.PositionsResult, events rpc.MarketEventsResult, now time.Time, fp rpc.Fingerprint) rpc.SourceHealth {
	events = canaryRelevantMarketEvents(pos, events)
	status := rpc.RegimeStatusOK
	confidence := "medium"
	notes := []string{}
	if len(events.Flags) > 0 {
		notes = append(notes, fmt.Sprintf("%d active/recent market-event flags", len(events.Flags)))
	}
	if len(events.WarningDetails) > 0 {
		status = "degraded"
		confidence = "medium-low"
		notes = append(notes, "one or more market-event sources are unavailable")
	}
	for _, health := range events.SourceHealth {
		switch health.Status {
		case rpc.MarketEventStatusUnknown, rpc.MarketEventStatusStale, rpc.MarketEventStatusDegraded, rpc.RegimeStatusError, rpc.RegimeStatusUnavailable:
			if status == rpc.RegimeStatusOK {
				status = "degraded"
				confidence = "medium-low"
			}
		}
	}
	health := canaryTimedSourceHealth("market_events", events.AsOf, now, fp, status, confidence)
	health.Notes = notes
	return health
}

func canarySourceHealth(in StressInput, now time.Time, accountFP, positionsFP, regimeFP, marketEventsFP rpc.Fingerprint, inputHealth string, m StressMarketSummary) []rpc.SourceHealth {
	out := []rpc.SourceHealth{
		canaryAccountSourceHealth(in.Account, now, accountFP),
		canaryTimedSourceHealth("positions", in.Positions.AsOf, now, positionsFP, canaryPositionsSourceStatus(in.Positions, now), canaryPositionsSourceConfidence(in.Positions)),
		canaryRegimeSourceHealth(in.Regime, now, regimeFP, canaryInputHealthConfidence(inputHealth), m),
	}
	if canaryHasMarketEventsInput(in.MarketEvents) || len(canaryMarketEventSymbols(in.Positions)) > 0 {
		out = append(out, canaryMarketEventsSourceHealth(in.Positions, in.MarketEvents, now, marketEventsFP))
	}
	return out
}

func canaryHasMarketEventsInput(events rpc.MarketEventsResult) bool {
	return events.Kind != "" ||
		!events.AsOf.IsZero() ||
		len(events.Symbols) > 0 ||
		len(events.Flags) > 0 ||
		len(events.BorrowFeeCoverage) > 0 ||
		len(events.SourceHealth) > 0 ||
		len(events.WarningDetails) > 0
}

func canaryMarketEventsSourceHealth(pos rpc.PositionsResult, events rpc.MarketEventsResult, now time.Time, fp rpc.Fingerprint) rpc.SourceHealth {
	status := rpc.SourceStatusOK
	confidence := "medium"
	notes := []string{}
	if len(events.Flags) > 0 {
		notes = append(notes, fmt.Sprintf("%d active/recent market-event flags", len(events.Flags)))
	}
	issues := canaryMarketEventSourceIssues(pos, events, now)
	for _, issue := range issues {
		if canaryMarketEventHealthRank(issue.Status) > canaryMarketEventHealthRank(status) {
			status = issue.Status
		}
		notes = append(notes, issue.Source+" "+issue.Status)
	}
	if len(issues) > 0 {
		confidence = "medium-low"
	}
	health := canaryTimedSourceHealth("market_events", events.AsOf, now, fp, status, confidence)
	health.Notes = notes
	return health
}

func canaryInputHealthConfidence(inputHealth string) string {
	switch inputHealth {
	case canaryInputOK:
		return "high"
	case canaryInputFailed:
		return "low"
	default:
		return "medium-low"
	}
}

func canaryTimedSourceHealth(source string, asOf, now time.Time, fp rpc.Fingerprint, status, confidence string) rpc.SourceHealth {
	maxAge := canarySourceMaxAgeSeconds(now)
	age := canarySourceAgeSeconds(now, asOf)
	if !asOf.IsZero() && age > maxAge && status == rpc.RegimeStatusOK {
		status = rpc.RegimeStatusStale
	}
	if status == rpc.RegimeStatusStale && confidence == "high" {
		confidence = "medium"
	}
	return rpc.SourceHealth{
		Source:               source,
		Status:               status,
		AsOf:                 asOf,
		AgeSeconds:           age,
		MaxAgeSeconds:        maxAge,
		Confidence:           confidence,
		Fingerprint:          &fp,
		FingerprintStability: rpc.FingerprintStabilitySemanticBuckets,
	}
}

func canaryRegimeSourceHealth(regime rpc.RegimeSnapshotResult, now time.Time, fp rpc.Fingerprint, dataConfidence string, m StressMarketSummary) rpc.SourceHealth {
	status := rpc.RegimeStatusOK
	notes := []string{}
	if len(m.StaleClusters) > 0 {
		notes = append(notes, "stale clusters: "+strings.Join(m.StaleClusters, ","))
	}
	if len(m.DegradedClusters) > 0 {
		notes = append(notes, "degraded clusters: "+strings.Join(m.DegradedClusters, ","))
	}
	if len(m.PartialClusters) > 0 || len(m.AmbiguousClusters) > 0 {
		notes = append(notes, canaryAmbiguityEvidence(m))
	}
	switch {
	case len(m.PartialClusters) > 0 || len(m.AmbiguousClusters) > 0:
		status = "partial"
	case len(m.DegradedClusters) > 0:
		status = "degraded"
	case len(m.StaleClusters) > 0:
		status = rpc.RegimeStatusStale
	}
	if issue, ok := canaryRegimeAuthorityIssue(regime); ok {
		if canaryMarketEventHealthRank(issue.Status) > canaryMarketEventHealthRank(status) {
			status = issue.Status
		}
		notes = append(notes, issue.Reason)
	}
	health := canaryTimedSourceHealth("regime", regime.AsOf, now, fp, status, dataConfidence)
	health.Notes = notes
	return health
}

func canaryAccountSourceHealth(acct rpc.AccountResult, now time.Time, fp rpc.Fingerprint) rpc.SourceHealth {
	health := canaryTimedSourceHealth("account", acct.AsOf, now, fp, canaryAccountSourceStatus(acct, now), canaryAccountSourceConfidence(acct, now))
	failed, notDue := canaryAccountDailyPnLState(acct, now)
	if notDue && health.Status == rpc.SourceStatusOK {
		health.RefreshState = rpc.SourceRefreshNotDue
		health.Notes = []string{"daily P&L is not due outside the US equity regular session"}
	} else if failed {
		note := "daily P&L feed has not recovered"
		if acct.DailyPnLObservation != nil && acct.DailyPnLObservation.SessionKey != "" {
			note += " for session " + acct.DailyPnLObservation.SessionKey
		}
		health.Notes = append(health.Notes, note)
	}
	return health
}

func canaryAccountSourceStatus(acct rpc.AccountResult, now time.Time) string {
	if acct.NetLiquidation <= 0 {
		return "partial"
	}
	if acct.AsOf.IsZero() {
		return "partial"
	}
	if failed, _ := canaryAccountDailyPnLState(acct, now); failed {
		if acct.DailyPnLObservation == nil {
			return "partial"
		}
		return rpc.SourceStatusDegraded
	}
	if canarySourceAgeSeconds(now, acct.AsOf) > canarySourceMaxAgeSeconds(now) {
		return rpc.RegimeStatusStale
	}
	return rpc.RegimeStatusOK
}

func canaryAccountSourceConfidence(acct rpc.AccountResult, now time.Time) string {
	failed, _ := canaryAccountDailyPnLState(acct, now)
	if acct.NetLiquidation <= 0 || acct.AsOf.IsZero() || failed {
		return "medium-low"
	}
	return "high"
}

func canaryAccountDailyPnLFailed(acct rpc.AccountResult, now time.Time) bool {
	failed, _ := canaryAccountDailyPnLState(acct, now)
	return failed
}

func canaryAccountDailyPnLState(acct rpc.AccountResult, now time.Time) (failed, notDue bool) {
	if observation := acct.DailyPnLObservation; observation != nil {
		switch observation.Status {
		case rpc.DailyPnLObservationNotDue:
			return false, true
		case rpc.DailyPnLObservationMissing, rpc.DailyPnLObservationInvalid, rpc.DailyPnLObservationStale:
			return true, false
		case rpc.DailyPnLObservationOK:
			if acct.DailyPnL == nil || math.IsNaN(*acct.DailyPnL) || math.IsInf(*acct.DailyPnL, 0) {
				return true, false
			}
			return false, false
		default:
			return true, false
		}
	}
	if acct.DailyPnL == nil || math.IsNaN(*acct.DailyPnL) || math.IsInf(*acct.DailyPnL, 0) {
		if canaryDailyPnLDue(now) {
			return true, false
		}
		return false, true
	}
	return false, false
}

// canaryDailyPnLDue follows the same official US-equity session authority as
// the daemon's subscription repair. Calendar failures and dates outside the
// embedded coverage fail closed: a missing P&L remains required.
func canaryDailyPnLDue(now time.Time) bool {
	session, err := marketcal.New().SessionAt(marketcal.MarketUSEquity, now)
	if err != nil || session.State == marketcal.StateUnknown {
		return true
	}
	return session.IsOpen
}

func canaryPositionsSourceStatus(pos rpc.PositionsResult, now time.Time) string {
	if pos.AsOf.IsZero() {
		return "partial"
	}
	if canarySourceAgeSeconds(now, pos.AsOf) > canarySourceMaxAgeSeconds(now) {
		return rpc.RegimeStatusStale
	}
	return rpc.RegimeStatusOK
}

func canaryPositionsSourceConfidence(pos rpc.PositionsResult) string {
	if pos.AsOf.IsZero() {
		return "medium-low"
	}
	return "high"
}

func canarySourceAgeSeconds(now, asOf time.Time) int64 {
	if now.IsZero() || asOf.IsZero() {
		return 0
	}
	age := now.Sub(asOf)
	if age < 0 {
		return 0
	}
	return int64(age.Seconds())
}

func canarySourceMaxAgeSeconds(now time.Time) int64 {
	switch rpc.ClassifySession(now) {
	case rpc.SessionPre, rpc.SessionRTH:
		return int64((10 * time.Minute).Seconds())
	default:
		return int64((90 * time.Minute).Seconds())
	}
}

func canaryWarningLine(w rpc.RegimeWarning) string {
	scope := strings.TrimSpace(w.Scope)
	if scope == "" {
		scope = strings.TrimSpace(w.Code)
	}
	if scope == "" {
		return ""
	}
	msg := strings.TrimSpace(w.Message)
	if msg == "" {
		msg = strings.TrimSpace(w.Impact)
	}
	noisy := strings.Contains(msg, "http://") || strings.Contains(msg, "https://") ||
		strings.Contains(msg, "context deadline exceeded") || strings.Contains(msg, "HTTP ")
	if noisy {
		switch {
		case strings.TrimSpace(w.Impact) != "":
			msg = strings.TrimSpace(w.Impact)
		case strings.TrimSpace(w.Action) != "":
			msg = strings.TrimSpace(w.Action)
		case strings.TrimSpace(w.Code) != "":
			msg = strings.TrimSpace(w.Code)
		default:
			msg = "source unavailable"
		}
	}
	if msg == "" {
		return ""
	}
	if !strings.Contains(msg, " row is unranked;") {
		msg = strings.Replace(msg, " is unranked;", " row is unranked;", 1)
	}
	return fmt.Sprintf("%s: %s", scope, msg)
}

// canaryMarketEvidence renders only the cluster facts that carry information:
// zero-count buckets never render, and the reporting fraction appears only
// while coverage is incomplete.
func canaryMarketEvidence(m StressMarketSummary) string {
	parts := []string{}
	if m.RedClusters > 0 {
		parts = append(parts, fmt.Sprintf("%d red (%s)", m.RedClusters, canaryClusterList(m.RedClusterNames)))
	}
	if m.YellowClusters > 0 {
		parts = append(parts, fmt.Sprintf("%d yellow (%s)", m.YellowClusters, canaryClusterList(m.YellowClusterNames)))
	}
	if len(parts) == 0 {
		parts = append(parts, "no stressed clusters")
	}
	if len(m.UnconfirmedRedClusterNames) > 0 {
		parts = append(parts, "red but unconfirmed: "+canaryClusterList(m.UnconfirmedRedClusterNames))
	}
	total := m.RankedClusters + m.UnrankedClusters
	if m.RankedClusters < total {
		parts = append(parts, fmt.Sprintf("%d of %d clusters reporting", m.RankedClusters, total))
	}
	return strings.Join(parts, "; ")
}

// Trigger levels render next to the observed numbers so a reading can be
// judged as near-miss or comfortable without opening the policy.
func canaryTapeEvidence(m StressMarketSummary) string {
	parts := []string{}
	if m.SPYChangePct != nil {
		parts = append(parts, fmt.Sprintf("SPY %+.2f%% (drop trigger %.1f%%)", *m.SPYChangePct, canaryPolicy.SPYDropPct))
	} else {
		parts = append(parts, "SPY change unavailable")
	}
	if m.VIXChangePct != nil {
		parts = append(parts, fmt.Sprintf("VIX %+.2f%% (spike trigger %+.0f%%)", *m.VIXChangePct, canaryPolicy.VIXSpikePct))
	} else {
		parts = append(parts, "VIX change unavailable")
	}
	if m.TapeSessionState == rpc.TapeSessionClosedDate {
		closed := "market closed"
		if m.TapeSessionReason != "" {
			closed += " (" + m.TapeSessionReason + ")"
		}
		parts = append(parts, closed+" — frozen last-session prints")
	}
	return strings.Join(parts, "; ")
}

// canaryTapeSession delegates to the shared rpc.TapeSessionFor policy copy —
// the regime lifecycle keys its closed-date tape gating on the same
// classification, and two hand-maintained calendars would drift.
func canaryTapeSession(now time.Time) (state, reason string, nextOpen *time.Time) {
	return rpc.TapeSessionFor(now)
}

// canaryTapeConfirmable reports whether direct SPY/VIX day-change prints may
// carry severity or confirm stress right now.
func canaryTapeConfirmable(m StressMarketSummary) bool {
	return m.TapeSessionState != rpc.TapeSessionClosedDate
}

func canaryTapeDemotedGuidance(m StressMarketSummary) string {
	msg := "Frozen last-session tape shock on a closed market date"
	if m.TapeSessionReason != "" {
		msg += " (" + m.TapeSessionReason + ")"
	}
	msg += "; confirm at next open"
	if m.TapeNextOpen != nil {
		msg += " " + m.TapeNextOpen.Format("Mon 15:04 MST")
	}
	return msg + "."
}

func canaryConfirmedTapeStress(m StressMarketSummary) bool {
	if !canaryTapeConfirmable(m) {
		// Frozen prints cannot confirm; only the cluster-side carry-unwind
		// arm (fx red + breadth) may still fire.
		return canaryFastCarryUnwind(m)
	}
	spyDrop := pctAtMost(m.SPYChangePct, canaryPolicy.SPYDropPct)
	spyHardDrop := pctAtMost(m.SPYChangePct, canaryPolicy.SPYHardDropPct)
	vixSpike := pctAtLeast(m.VIXChangePct, canaryPolicy.VIXSpikePct)
	vixHardSpike := pctAtLeast(m.VIXChangePct, canaryPolicy.VIXHardSpikePct)
	return (spyHardDrop && (vixSpike || m.EligibleRedClusters >= 1)) ||
		(vixHardSpike && (spyDrop || m.EligibleRedClusters >= 1)) ||
		(spyDrop && vixSpike && m.EligibleRedClusters >= 1) ||
		canaryFastCarryUnwind(m)
}

func canaryFastCarryUnwind(m StressMarketSummary) bool {
	fxRed := slices.Contains(m.RedClusterNames, "fx") || slices.Contains(m.UnconfirmedRedClusterNames, "fx")
	if !fxRed {
		return false
	}
	tapeConfirms := canaryTapeConfirmable(m) &&
		(pctAtMost(m.SPYChangePct, canaryPolicy.SPYDropPct) ||
			pctAtLeast(m.VIXChangePct, canaryPolicy.VIXSpikePct))
	return tapeConfirms ||
		slices.Contains(m.YellowClusterNames, "breadth") ||
		slices.Contains(m.RedClusterNames, "breadth")
}

func pctAtMost(v *float64, threshold float64) bool {
	return v != nil && *v <= threshold
}

func pctAtLeast(v *float64, threshold float64) bool {
	return v != nil && *v >= threshold
}

func canaryAmbiguityEvidence(m StressMarketSummary) string {
	parts := []string{canaryMarketEvidence(m)}
	if len(m.StaleClusters) > 0 {
		parts = append(parts, "stale "+canaryClusterList(m.StaleClusters))
	}
	if len(m.AmbiguousClusters) > 0 {
		parts = append(parts, "ambiguous "+canaryClusterList(m.AmbiguousClusters))
	}
	if len(m.PartialClusters) > 0 {
		parts = append(parts, "partial "+canaryClusterList(m.PartialClusters))
	}
	if len(m.DegradedClusters) > 0 {
		parts = append(parts, "degraded "+canaryClusterList(m.DegradedClusters))
	}
	if len(m.ComputingClusters) > 0 {
		parts = append(parts, "computing "+canaryClusterList(m.ComputingClusters))
	}
	return strings.Join(parts, "; ")
}

func canaryPortfolioEvidence(p StressPortfolioSummary) string {
	out := fmt.Sprintf("%s, gross %.0f%% NLV, net delta %.0f%% NLV, gross delta %.0f%% NLV",
		canaryCushionEvidence(p), derefPct(p.GrossExposurePctNLV), derefPct(p.NetDeltaPctNLV), derefPct(p.GrossDeltaPctNLV))
	if p.ProtectionCoverage != nil {
		out += ", protection " + formatProtectionCoverageEvidence(p.ProtectionCoverage)
	}
	if len(p.HeldStress) > 0 {
		out += ", held stress " + canaryHeldStressNames(p.HeldStress, 2)
	}
	return out
}

func formatProtectionCoverageEvidence(c *rpc.ProtectionCoverageSummary) string {
	if c == nil {
		return "coverage unavailable"
	}
	parts := []string{nonEmpty(c.Status, "unknown")}
	if c.UnprotectedNotionalBase != nil && *c.UnprotectedNotionalBase != 0 {
		parts = append(parts, "unprotected "+formatMoneyCcy(*c.UnprotectedNotionalBase, c.UnprotectedNotionalBaseCurrency))
	}
	if c.Counts.Unprotected > 0 {
		parts = append(parts, fmt.Sprintf("%d unprotected", c.Counts.Unprotected))
	}
	if c.Counts.Partial > 0 {
		parts = append(parts, fmt.Sprintf("%d partial", c.Counts.Partial))
	}
	if c.Counts.OrphanedOrder > 0 {
		parts = append(parts, fmt.Sprintf("%d orphaned", c.Counts.OrphanedOrder))
	}
	if c.Counts.ReconcileRequired > 0 {
		parts = append(parts, fmt.Sprintf("%d reconcile-required", c.Counts.ReconcileRequired))
	}
	if len(c.LargestUnprotected) > 0 {
		names := make([]string, 0, min(len(c.LargestUnprotected), 3))
		for _, row := range c.LargestUnprotected {
			if row.Underlying != "" {
				names = append(names, row.Underlying)
			}
			if len(names) == 3 {
				break
			}
		}
		if len(names) > 0 {
			parts = append(parts, "largest "+strings.Join(names, ","))
		}
	}
	return strings.Join(parts, "; ")
}

// largestUnprotectedPhrase names the single largest unprotected position and
// its uncovered amount ("MSFT € 12,345.67") for row guidance. The daemon
// orders LargestUnprotected by uncovered notional; a row without a valued
// notional still contributes its name. Empty when the daemon filled nothing.
func largestUnprotectedPhrase(c *rpc.ProtectionCoverageSummary) string {
	if c == nil {
		return ""
	}
	for _, row := range c.LargestUnprotected {
		if row.Underlying == "" {
			continue
		}
		if row.UnprotectedNotionalBase != nil && *row.UnprotectedNotionalBase != 0 {
			ccy := row.UnprotectedNotionalBaseCurrency
			if ccy == "" {
				ccy = c.UnprotectedNotionalBaseCurrency
			}
			return row.Underlying + " " + formatMoneyCcy(*row.UnprotectedNotionalBase, ccy)
		}
		return row.Underlying
	}
	return ""
}

func canaryHasMarketDataIssue(m StressMarketSummary) bool {
	return len(m.AmbiguousClusters) > 0 ||
		len(m.PartialClusters) > 0 ||
		len(m.DegradedClusters) > 0 ||
		len(m.ComputingClusters) > 0 ||
		len(m.StaleClusters) > 0
}

func canaryWorstCushionPct(p StressPortfolioSummary) *float64 {
	switch {
	case p.CushionPct != nil && p.LookAheadCushionPct != nil:
		v := min(*p.CushionPct, *p.LookAheadCushionPct)
		return &v
	case p.CushionPct != nil:
		return p.CushionPct
	default:
		return p.LookAheadCushionPct
	}
}

// The trailing trigger mirrors the tape row's disclosure style: the reader
// sees how far the printed number sits from the policy line, not just the
// number. Both cushion variants share the same watch floor.
func canaryCushionEvidence(p StressPortfolioSummary) string {
	parts := []string{}
	if p.CushionPct != nil {
		parts = append(parts, pctEvidence("cushion", *p.CushionPct))
	}
	if p.LookAheadCushionPct != nil {
		parts = append(parts, pctEvidence("look-ahead cushion", *p.LookAheadCushionPct))
	}
	if len(parts) == 0 {
		return "cushion unavailable"
	}
	return strings.Join(parts, "; ") + fmt.Sprintf(" (watch below %.0f%%)", canaryPolicy.MarginWatchPct)
}

func canaryConcentrationEvidence(p StressPortfolioSummary) string {
	parts := []string{}
	if p.LargestExposurePct != nil && p.LargestExposure != "" {
		parts = append(parts, fmt.Sprintf("%s market %.0f%% NLV (watch %.0f%%)", p.LargestExposure, math.Abs(*p.LargestExposurePct), canaryPolicy.SingleNameExposureWatchPct))
	}
	if p.LargestDeltaPctNLV != nil && p.LargestDeltaExposure != "" {
		parts = append(parts, fmt.Sprintf("%s delta %.0f%% NLV (watch %.0f%%)", p.LargestDeltaExposure, *p.LargestDeltaPctNLV, canaryPolicy.SingleNameDeltaWatchPct))
	}
	return strings.Join(parts, "; ")
}

func canaryHeldStressEvidence(stresses []rpc.HeldStress) string {
	if len(stresses) == 0 {
		return "no material held-name stress"
	}
	parts := []string{}
	for _, stress := range stresses {
		items := []string{}
		if stress.DailyPnLPctNLV != nil && *stress.DailyPnLPctNLV <= -canaryPolicy.HeldUnderlyingPnLWatchPct {
			items = append(items, fmt.Sprintf("daily P&L %+.1f%% NLV", *stress.DailyPnLPctNLV))
		}
		if stress.NearExpiryDeltaPctNLV != nil && *stress.NearExpiryDeltaPctNLV >= canaryPolicy.HeldOptionDeltaWatchPct {
			text := fmt.Sprintf("near-expiry delta %.0f%% NLV", *stress.NearExpiryDeltaPctNLV)
			if stress.NearExpiryMinDTE != nil {
				text += fmt.Sprintf(" at %d DTE", *stress.NearExpiryMinDTE)
			}
			items = append(items, text)
		}
		if len(stress.LiquidityFlags) > 0 {
			items = append(items, "liquidity "+strings.Join(stress.LiquidityFlags, ","))
		}
		if len(items) == 0 {
			items = append(items, strings.Join(stress.MaterialReasons, ","))
		}
		parts = append(parts, stress.Underlying+" "+strings.Join(items, "; "))
		if len(parts) == 3 {
			break
		}
	}
	if len(stresses) > len(parts) {
		parts = append(parts, fmt.Sprintf("+%d more", len(stresses)-len(parts)))
	}
	return strings.Join(parts, "; ")
}

func canaryHeldStressNames(stresses []rpc.HeldStress, limit int) string {
	if limit <= 0 {
		limit = len(stresses)
	}
	names := []string{}
	for _, stress := range stresses {
		if stress.Underlying == "" {
			continue
		}
		names = append(names, stress.Underlying)
		if len(names) == limit {
			break
		}
	}
	if len(names) == 0 {
		return "none"
	}
	out := strings.Join(names, ",")
	if len(stresses) > len(names) {
		out += fmt.Sprintf("+%d", len(stresses)-len(names))
	}
	return out
}

func pctEvidence(label string, pct float64) string {
	return fmt.Sprintf("%s %.0f%%", label, pct)
}

func derefPct(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}

func humanList(value string) string {
	clean := []string{}
	for part := range strings.SplitSeq(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		clean = append(clean, canaryClusterDisplayName(part))
	}
	if len(clean) == 0 {
		return strings.TrimSpace(value)
	}
	if len(clean) == 1 {
		return clean[0]
	}
	if len(clean) == 2 {
		return clean[0] + " and " + clean[1]
	}
	return strings.Join(clean[:len(clean)-1], ", ") + ", and " + clean[len(clean)-1]
}

func canaryClusterList(clusters []string) string {
	return humanList(strings.Join(clusters, ","))
}

func canaryClusterDisplayName(cluster string) string {
	switch strings.ToLower(strings.TrimSpace(cluster)) {
	case "fx":
		return "FX"
	default:
		return strings.TrimSpace(cluster)
	}
}

func nonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

func formatMoneyCcy(v float64, ccy string) string {
	prefix := moneyPrefix(ccy)
	if v == 0 {
		return prefix + "        —"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	s := fmt.Sprintf("%.2f", v)
	dot := strings.IndexByte(s, '.')
	intPart, frac := s[:dot], s[dot:]
	out := prefix + groupThousands(intPart) + frac
	if neg {
		return "-" + out
	}
	return out
}

func moneyPrefix(ccy string) string {
	switch strings.ToUpper(strings.TrimSpace(ccy)) {
	case "", "USD":
		return "$ "
	case "EUR":
		return "€ "
	case "GBP":
		return "£ "
	case "JPY":
		return "¥ "
	default:
		return strings.ToUpper(strings.TrimSpace(ccy)) + " "
	}
}

func groupThousands(s string) string {
	n := len(s)
	if n <= 3 {
		return s
	}
	var out strings.Builder
	for i, r := range s {
		if i > 0 && (n-i)%3 == 0 {
			out.WriteString(",")
		}
		out.WriteRune(r)
	}
	return out.String()
}
