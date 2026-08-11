package stress

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/regimerows"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

var stressPolicy = risk.DefaultPolicy()

const heldStressLimit = 5

const (
	stressEstablishedRegimeFingerprintVersion       = "regime-fp-v1"
	stressEstablishedMarketEventsFingerprintVersion = "market-events-fp-v1"
	stressEstablishedPolicyFingerprintVersion       = "canary-policy-fp-v1"
	stressEstablishedOverallRowTitle                = "Portfolio canary"
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

// StressPortfolioSummary is the redacted portfolio context used by Stress.
type StressPortfolioSummary = rpc.StressPortfolioSummary

// StressMarketSummary is the classified market context used by Stress.
type StressMarketSummary = rpc.StressMarketSummary

// ComputeStress evaluates one typed snapshot. A zero input clock uses the
// not become healthy zero values. The result is advisory and performs no broker
func ComputeStress(in StressInput) StressResult {
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	res := computeStress(in, now, stressSourceIssues(in, now), false)
	established := computeStress(in, now, stressEstablishedSourceIssues(in, now), true)
	projection := stressEstablishedAlertProjection(established)
	res.EstablishedAlertProjection = &projection
	return res
}

// computeStress owns the one Stress decision implementation. The established
// mode freezes only the source-interpretation and source-health behavior that
// existed at ad5b77b; account, positions, market clusters, Regime data quality,
// and every underlying risk calculation continue through the same producer.
func computeStress(in StressInput, now time.Time, sourceIssues []stressSourceIssue, established bool) StressResult {
	accountFingerprint := rpc.BuildAccountFingerprint(&in.Account)
	positionsFingerprint := rpc.BuildPositionsFingerprint(&in.Positions, in.Account.NetLiquidation)
	regimeFingerprint := in.Regime.Fingerprint
	if regimeFingerprint.Key == "" {
		regimeFingerprint = rpc.BuildRegimeFingerprint(&in.Regime)
	}
	if established {
		regimeFingerprint = stressEstablishedRegimeFingerprint(in.Regime)
	}
	marketEventsFingerprint := stressRelevantMarketEventsFingerprint(in.Positions, in.MarketEvents)
	if established {
		marketEventsFingerprint = stressEstablishedMarketEventsFingerprint(in.MarketEvents)
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
		Policy:             stressPolicy.PolicyProfile(),
		PolicyProfile:      stressPolicy.PolicyProfile(),
		PolicyVersion:      stressPolicy.PolicyVersion(),
		PolicyFingerprint:  rpc.Fingerprint{Version: risk.StressPolicyFingerprintVersion, Key: stressPolicy.FingerprintKey()},
		Portfolio:          summarizeStressPortfolio(in.Account, in.Positions, in.MarketEvents, now),
		Market:             summarizeStressMarket(in.Regime, now),
		MarketIndicators:   stressMarketIndicators(in.Regime, now),
		NotExecution:       "Read-only stress snapshot; no orders are placed by Canary.",
	}
	rows := []StressRow{
		stressMarginRow(res.Portfolio),
		stressPnLShockRow(res.Portfolio),
		stressTapeShockRow(res.Portfolio, res.Market),
		stressMarketRow(res.Market),
		stressExposureRow(res.Portfolio, res.Market),
		stressConcentrationRow(res.Portfolio, res.Market),
		stressProtectionCoverageRow(res.Portfolio),
		heldStressRow(res.Portfolio, res.Market),
		stressOptionsRow(res.Portfolio, in.Positions, res.Market),
		stressDataQualityRow(res.Market, in.Regime),
	}
	res.Signals = stressSignals(res.Portfolio, in.Positions, res.Market, in.Regime)
	res.Signals = stressApplySourceBlocks(res.Signals, sourceIssues)
	if established {
		res.Signals = append(res.Signals, stressEstablishedSourceDataQualitySignals(sourceIssues)...)
	} else {
		res.Signals = append(res.Signals, stressSourceDataQualitySignals(sourceIssues)...)
	}
	res.MarketConfirmation = stressMarketConfirmation(res.Market)
	res.PortfolioFit = stressPortfolioFit(res.Portfolio, res.Signals)
	res.PortfolioAlertRelevant = new(stressPortfolioAlertRelevant(&res))
	res.InputHealth = stressInputHealth(in, res.Market, sourceIssues, now)
	res.Direction, res.Severity = stressDecisionState(res.MarketConfirmation, res.PortfolioFit, res.InputHealth, res.Market, res.Signals)
	res.Action = stressAction(res.Direction, res.Severity, res.MarketConfirmation, res.PortfolioFit, res.InputHealth)
	res.PlannerModeHint = stressPlannerModeFromAction(res.Action)
	res.PlannerReadiness = stressPlannerReadinessFromAction(res.Action, res.Severity, res.InputHealth)
	res.PrimaryDrivers = stressPrimaryDrivers(res.Signals)
	res.Summary = stressDecisionSummary(res)
	overall := stressOverallRow(res.Direction, res.Severity, res.Summary, res.Market, res.Portfolio)
	res.Rows = append([]StressRow{overall}, rows...)
	res.Warnings = stressWarnings(res.Market, in.Regime, now)
	if established {
		res.SourceHealth = stressEstablishedSourceHealth(in, now, accountFingerprint, positionsFingerprint, regimeFingerprint, marketEventsFingerprint, res.InputHealth, res.Market)
	} else {
		res.Warnings = append(res.Warnings, stressMarketEventWarnings(sourceIssues)...)
		res.SourceHealth = stressSourceHealth(in, now, accountFingerprint, positionsFingerprint, regimeFingerprint, marketEventsFingerprint, res.InputHealth, res.Market)
	}
	res.Fingerprint = rpc.BuildStressFingerprint(&res)
	return res
}

// stressRelevantMarketEventsFingerprint applies the same exposure boundary as
// fingerprints. Active flags are retained: only irrelevant borrow source
func stressRelevantMarketEventsFingerprint(pos rpc.PositionsResult, events rpc.MarketEventsResult) rpc.Fingerprint {
	if !stressHasMarketEventsInput(events) {
		return rpc.Fingerprint{}
	}
	if stressHasShortStockExposure(pos) {
		if events.Fingerprint.Key != "" {
			return events.Fingerprint
		}
		return rpc.BuildMarketEventsFingerprint(&events)
	}
	filtered := stressRelevantMarketEvents(pos, events)
	filtered.Fingerprint = rpc.Fingerprint{}
	return rpc.BuildMarketEventsFingerprint(&filtered)
}

// stressEstablishedMarketEventsFingerprint keeps the exact pre-v2
// failure details are stripped, but every v1 source-health bucket remains;
func stressEstablishedMarketEventsFingerprint(events rpc.MarketEventsResult) rpc.Fingerprint {
	if !stressHasMarketEventsInput(events) {
		return rpc.Fingerprint{}
	}
	if events.Fingerprint.Key != "" && len(events.BorrowFeeCoverage) == 0 && !stressSourceHealthHasTypedFailure(events.SourceHealth) {
		fingerprint := events.Fingerprint
		fingerprint.Version = stressEstablishedMarketEventsFingerprintVersion
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
	fingerprint.Version = stressEstablishedMarketEventsFingerprintVersion
	return fingerprint
}

func stressEstablishedRegimeFingerprint(regime rpc.RegimeSnapshotResult) rpc.Fingerprint {
	if regime.Fingerprint.Key != "" && !stressSourceHealthHasTypedFailure(regime.SourceHealth) {
		fingerprint := regime.Fingerprint
		fingerprint.Version = stressEstablishedRegimeFingerprintVersion
		return fingerprint
	}
	regime.Fingerprint = rpc.Fingerprint{}
	regime.SourceHealth = slices.Clone(regime.SourceHealth)
	for i := range regime.SourceHealth {
		regime.SourceHealth[i].LastFailure = nil
	}
	fingerprint := rpc.BuildRegimeFingerprint(&regime)
	fingerprint.Version = stressEstablishedRegimeFingerprintVersion
	return fingerprint
}

func stressSourceHealthHasTypedFailure(health []rpc.SourceHealth) bool {
	for _, source := range health {
		if source.LastFailure != nil {
			return true
		}
	}
	return false
}

func stressRelevantMarketEvents(pos rpc.PositionsResult, events rpc.MarketEventsResult) rpc.MarketEventsResult {
	if stressHasShortStockExposure(pos) {
		return events
	}
	filtered := events
	filtered.BorrowFeeCoverage = nil
	filtered.SourceHealth = slices.DeleteFunc(slices.Clone(events.SourceHealth), func(health rpc.SourceHealth) bool {
		return stressMarketEventBorrowSource(stressMarketEventSourceName(health.Source))
	})
	filtered.WarningDetails = slices.DeleteFunc(slices.Clone(events.WarningDetails), func(warning rpc.DataWarning) bool {
		return stressMarketEventBorrowSource(stressMarketEventSourceName(warning.Scope + " " + warning.Code))
	})
	return filtered
}

func stressEstablishedAlertProjection(result StressResult) rpc.EstablishedAlertProjection {
	portfolioRelevant := result.PortfolioAlertRelevant != nil && *result.PortfolioAlertRelevant
	actionEligible := severityRankAtLeast(result.Severity, risk.SeverityAct) ||
		result.Action == stressActionDefend ||
		result.Action == stressActionRebalance ||
		result.Action == stressActionConfirmInputs
	occurrenceEligible := portfolioRelevant &&
		(severityRankAtLeast(result.Severity, risk.SeverityWatch) || actionEligible)
	return rpc.EstablishedAlertProjection{
		SchemaVersion:        rpc.EstablishedAlertProjectionSchemaVersion,
		CanonicalFingerprint: stressEstablishedAlertFingerprint(result),
		OccurrenceEligible:   occurrenceEligible,
		ActOnlyEligible:      occurrenceEligible && actionEligible,
		Action:               result.Action,
		MarketConfirmation:   result.MarketConfirmation,
		Severity:             result.Severity,
		PortfolioRelevant:    portfolioRelevant,
	}
}

// stressEstablishedAlertFingerprint retains the exact semantic projection
// Stress-labelled; only this compatibility copy restores the two renamed
func stressEstablishedAlertFingerprint(result StressResult) rpc.Fingerprint {
	compatibility := result
	compatibility.PolicyFingerprint.Version = stressEstablishedPolicyFingerprintVersion
	compatibility.Rows = slices.Clone(result.Rows)
	if len(compatibility.Rows) > 0 {
		// computeStress always prepends the producer-owned overall row.
		compatibility.Rows[0].Title = stressEstablishedOverallRowTitle
	}
	fingerprint := rpc.BuildStressFingerprint(&compatibility)
	return rpc.Fingerprint{
		Version: rpc.EstablishedStressFingerprintVersion,
		Key:     fingerprint.Key,
	}
}

func summarizeStressPortfolio(acct rpc.AccountResult, pos rpc.PositionsResult, marketEvents rpc.MarketEventsResult, now time.Time) StressPortfolioSummary {
	out := StressPortfolioSummary{
		BaseCurrency:   acct.BaseCurrency,
		NetLiquidation: acct.NetLiquidation,
	}
	if acct.NetLiquidation > 0 {
		out.CushionPct = stressCurrentCushionPct(acct)
		out.LookAheadCushionPct = stressLookAheadCushionPct(acct)
		// The cushion trip is the policy's watch floor — the same number
		if out.CushionPct != nil || out.LookAheadCushionPct != nil {
			out.CushionTripPct = new(stressPolicy.MarginWatchPct)
		}
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
		// Names the aggregator could not value in base at all carry no row here;
		unmeasured := slices.Clone(pos.Portfolio.ExposureUnmeasured)
		for _, e := range pos.Portfolio.ExposureBase {
			if e.MarketValuePctNLV != nil {
				if out.LargestExposurePct == nil || math.Abs(*e.MarketValuePctNLV) > math.Abs(*out.LargestExposurePct) {
					pct := *e.MarketValuePctNLV
					out.LargestExposurePct = &pct
					out.LargestExposure = strings.ToUpper(e.Underlying)
				}
			}
			if e.DollarDeltaBase == nil || acct.NetLiquidation <= 0 {
				if e.DollarDeltaBase == nil {
					unmeasured = append(unmeasured, e.Underlying)
				}
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
		out.ExposureUnmeasured = stressUpperSorted(unmeasured)
	}
	out.ProtectionCoverage = pos.ProtectionCoverage
	out.HeldStress = heldStressSummaries(acct, pos, marketEvents, now)
	return out
}

// stressUpperSorted normalizes a disclosure name list: upper-cased, sorted, and
func stressUpperSorted(names []string) []string {
	var out []string
	for _, n := range names {
		if n = strings.ToUpper(strings.TrimSpace(n)); n != "" {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// stressUnmeasuredNames renders the exposure gap for one row's evidence,
// bounded so a wide book does not turn a row into a list.
func stressUnmeasuredNames(names []string) string {
	if len(names) == 0 {
		return ""
	}
	noun := "names"
	if len(names) == 1 {
		noun = "name"
	}
	shown, extra := names, ""
	if len(shown) > 3 {
		shown, extra = shown[:3], fmt.Sprintf(" +%d more", len(names)-3)
	}
	return fmt.Sprintf("%d %s unmeasured (%s%s)", len(names), noun, strings.Join(shown, ", "), extra)
}

func heldStressSummaries(acct rpc.AccountResult, pos rpc.PositionsResult, marketEvents rpc.MarketEventsResult, now time.Time) []rpc.HeldStress {
	if acct.NetLiquidation <= 0 {
		return nil
	}
	builder := newHeldStressBuilder(acct.NetLiquidation)
	builder.addPortfolioExposures(pos.Portfolio)
	builder.addUnderlyingGroups(pos.ByUnderlying)
	builder.addStockRows(pos.Stocks)
	builder.addOptionRows(stressOptionsByUnderlying(pos), now)
	return builder.rows(marketEvents)
}

type heldStressBuilder struct {
	netLiquidation float64
	rowsBySymbol   map[string]*rpc.HeldStress
	order          []string
}

func newHeldStressBuilder(netLiquidation float64) *heldStressBuilder {
	return &heldStressBuilder{
		netLiquidation: netLiquidation,
		rowsBySymbol:   map[string]*rpc.HeldStress{},
	}
}

func (b *heldStressBuilder) ensure(underlying string) *rpc.HeldStress {
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

func (b *heldStressBuilder) addPortfolioExposures(portfolio *rpc.PositionsPortfolio) {
	if portfolio == nil {
		return
	}
	for _, e := range portfolio.ExposureBase {
		s := b.ensure(e.Underlying)
		if s == nil {
			continue
		}
		stressSetFloatPtrIfNil(&s.MarketValuePctNLV, e.MarketValuePctNLV)
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

func (b *heldStressBuilder) addUnderlyingGroups(groups []rpc.PositionGroup) {
	for _, group := range groups {
		s := b.ensure(group.Underlying)
		if s == nil {
			continue
		}
		stressSetFloatPtrIfNil(&s.MarketValuePctNLV, group.GroupMarketValuePctNLV)
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
		s.LiquidityFlags = stressUniqueFlags(s.LiquidityFlags, stressHeldStockLiquidityFlags(group.Stock)...)
	}
}

func (b *heldStressBuilder) addStockRows(stocks []rpc.PositionView) {
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
		s.LiquidityFlags = stressUniqueFlags(s.LiquidityFlags, stressHeldStockLiquidityFlags(stock)...)
	}
}

func (b *heldStressBuilder) addOptionRows(optionsByUnderlying map[string][]rpc.PositionView, now time.Time) {
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
		applyHeldOptionStress(s, options, now, b.netLiquidation)
		s.LiquidityFlags = stressUniqueFlags(s.LiquidityFlags, stressHeldOptionLiquidityFlags(options)...)
	}
}

func (b *heldStressBuilder) rows(marketEvents rpc.MarketEventsResult) []rpc.HeldStress {
	out := []rpc.HeldStress{}
	for _, underlying := range b.order {
		s := b.rowsBySymbol[underlying]
		s.MarketFlags = stressHeldMarketFlags(underlying, marketEvents)
		s.MaterialReasons = heldStressMaterialReasons(*s)
		s.SignalIDs = heldStressSignalIDs(*s)
		if len(s.MaterialReasons) == 0 || len(s.SignalIDs) == 0 {
			continue
		}
		out = append(out, *s)
	}
	slices.SortStableFunc(out, func(a, b rpc.HeldStress) int {
		return cmp.Compare(heldStressSortScore(b), heldStressSortScore(a))
	})
	if len(out) > heldStressLimit {
		out = out[:heldStressLimit]
	}
	return out
}

func stressHeldMarketFlags(underlying string, events rpc.MarketEventsResult) []rpc.MarketEventFlag {
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

func stressSetFloatPtrIfNil(dst **float64, src *float64) {
	if *dst != nil || src == nil {
		return
	}
	v := *src
	*dst = &v
}

func stressOptionsByUnderlying(pos rpc.PositionsResult) map[string][]rpc.PositionView {
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

func applyHeldOptionStress(s *rpc.HeldStress, options []rpc.PositionView, now time.Time, nlv float64) {
	if s == nil || nlv <= 0 {
		return
	}
	var deltaAbsBase, gamma float64
	var hasDelta, hasGamma bool
	var minDTE *int
	for _, opt := range options {
		dte, ok := stressOptionDTE(opt.Expiry, now)
		if !ok || dte < 0 || dte > stressPolicy.HeldOptionNearDTE {
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

func stressHeldStockLiquidityFlags(stock *rpc.PositionView) []string {
	if stock == nil {
		return nil
	}
	flags := []string{}
	liveOrUnknown := stressPositionMarketOpenOrUnknown(*stock)
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
	if liveOrUnknown && stock.SpreadPct != nil && *stock.SpreadPct >= stressPolicy.HeldLiquidityStockSpreadPct {
		flags = append(flags, "stock_wide_spread")
	}
	return stressUniqueFlags(nil, flags...)
}

func stressPositionMarketOpenOrUnknown(p rpc.PositionView) bool {
	if p.SessionContext == nil {
		return true
	}
	return p.SessionContext.IsOpen
}

func stressHeldOptionLiquidityFlags(options []rpc.PositionView) []string {
	flags := []string{}
	for _, opt := range options {
		if stressPositionWarningHas(opt.WarningDetails, "options_closed") {
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
		if spreadPct >= stressPolicy.HeldLiquidityOptionSpreadPctOfMid {
			flags = append(flags, "option_wide_spread")
		}
	}
	return stressUniqueFlags(nil, flags...)
}

func stressPositionWarningHas(details []rpc.DataWarning, code string) bool {
	code = strings.ToLower(strings.TrimSpace(code))
	for _, detail := range details {
		if strings.ToLower(strings.TrimSpace(detail.Code)) == code {
			return true
		}
	}
	return false
}

func stressOptionDTE(expiry string, now time.Time) (int, bool) {
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

func heldStressMaterialReasons(s rpc.HeldStress) []string {
	reasons := []string{}
	if s.MarketValuePctNLV != nil && math.Abs(*s.MarketValuePctNLV) >= stressPolicy.HeldStressMaterialPct {
		reasons = appendUniqueString(reasons, "market_value")
	}
	if s.DeltaPctNLV != nil && *s.DeltaPctNLV >= stressPolicy.HeldStressMaterialPct {
		reasons = appendUniqueString(reasons, "delta")
	}
	if s.DailyPnLPctNLV != nil && *s.DailyPnLPctNLV <= -stressPolicy.HeldUnderlyingPnLWatchPct {
		reasons = appendUniqueString(reasons, "daily_pnl")
	}
	if s.NearExpiryDeltaPctNLV != nil && *s.NearExpiryDeltaPctNLV >= stressPolicy.HeldOptionDeltaWatchPct {
		reasons = appendUniqueString(reasons, "near_expiry_option_delta")
	}
	return reasons
}

func heldStressSignalIDs(s rpc.HeldStress) []risk.SignalID {
	ids := []risk.SignalID{}
	if s.DailyPnLPctNLV != nil && *s.DailyPnLPctNLV <= -stressPolicy.HeldUnderlyingPnLWatchPct {
		ids = append(ids, risk.SignalHeldUnderlyingPnLShock)
	}
	if s.NearExpiryDeltaPctNLV != nil && *s.NearExpiryDeltaPctNLV >= stressPolicy.HeldOptionDeltaWatchPct {
		ids = append(ids, risk.SignalHeldOptionExpiryConcentration)
	}
	if len(s.LiquidityFlags) > 0 {
		ids = append(ids, risk.SignalHeldLiquidityDegraded)
	}
	return ids
}

func heldStressSortScore(s rpc.HeldStress) float64 {
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

func stressUniqueFlags(flags []string, values ...string) []string {
	for _, value := range values {
		flags = appendUniqueString(flags, value)
	}
	return flags
}

func stressCurrentCushionPct(acct rpc.AccountResult) *float64 {
	if acct.NetLiquidation <= 0 {
		return nil
	}
	switch {
	case acct.Cushion != 0:
		return new(acct.Cushion * 100)
	case acct.ExcessLiquidity != 0:
		return new(acct.ExcessLiquidity / acct.NetLiquidation * 100)
	case stressHasActiveMarginContext(acct):
		return new(0.0)
	default:
		return nil
	}
}

func stressLookAheadCushionPct(acct rpc.AccountResult) *float64 {
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

func stressHasActiveMarginContext(acct rpc.AccountResult) bool {
	return acct.ExcessLiquidity < 0 ||
		acct.AvailableFunds < 0 ||
		acct.MaintenanceMargin > 0 ||
		acct.InitialMargin > 0
}

func summarizeStressMarket(r rpc.RegimeSnapshotResult, now time.Time) StressMarketSummary {
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
	out.TapeSessionState, out.TapeSessionReason, out.TapeNextOpen = stressTapeSession(now)
	contextClusters := stressMarketContextClusters(r, now)
	// Shared rpc combination: raw worst-of bands, eligibility-keyed
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
	rowMeta := map[string][]rpc.RegimeIndicatorMeta{
		"vol":     {r.VIXTermStructure.RegimeIndicatorMeta, r.VolOfVol.RegimeIndicatorMeta},
		"credit":  {r.HYGSPYDivergence.RegimeIndicatorMeta, r.CreditSpreads.RegimeIndicatorMeta},
		"funding": {r.FundingStress.RegimeIndicatorMeta},
		"fx":      {r.USDJPY.RegimeIndicatorMeta},
		"gamma":   {r.GammaZero.RegimeIndicatorMeta},
		"breadth": {r.Breadth.RegimeIndicatorMeta},
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
		if status == rpc.RegimeStatusStale && !contextClusters[name] &&
			!stressClusterStaleNotDue(statuses[name], rowMeta[name]) {
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
	if stressGammaDegraded(r.GammaZero) {
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

func stressMarketContextClusters(r rpc.RegimeSnapshotResult, now time.Time) map[string]bool {
	out := map[string]bool{}
	if stressGammaContextOnly(r.GammaZero) {
		out["gamma"] = true
	}
	if stressVolClosedSessionContext(r, now) {
		out["vol"] = true
	}
	return out
}

func stressGammaContextOnly(g rpc.RegimeGammaZero) bool {
	return g.Envelope.Result != nil &&
		g.Envelope.Result.Quality != nil &&
		g.Envelope.Result.Quality.Rankability == rpc.GammaRankabilityContextOnly &&
		g.Freshness != nil && rpc.RegimeCurrencyScheduled(g.Freshness.Class)
}

func stressVolClosedSessionContext(r rpc.RegimeSnapshotResult, _ time.Time) bool {
	return r.VIXTermStructure.Freshness != nil &&
		r.VIXTermStructure.Freshness.Class == rpc.RegimeFreshnessNotDue
}

func stressMarketIndicators(r rpc.RegimeSnapshotResult, now time.Time) []StressMarketIndicator {
	if now.IsZero() {
		now = time.Now()
	}
	contextClusters := stressMarketContextClusters(r, now)
	rows := []struct {
		indicator   string
		cluster     string
		row         regimerows.Row
		asOf        *rpc.RegimeAsOfSummary
		date        string
		status      string
		trip        string
		thresholds  *rpc.RegimeThresholds
		eligibility *rpc.RegimeEligibility
	}{
		{indicator: rpc.RegimeIndicatorVIXTerm, cluster: "vol", row: regimerows.VIXTerm(now, r.VIXTermStructure), asOf: r.VIXTermStructure.AsOf, status: r.VIXTermStructure.Status, trip: stressIndicatorTrip(r.VIXTermStructure.Thresholds), thresholds: r.VIXTermStructure.Thresholds, eligibility: r.VIXTermStructure.Eligibility},
		{indicator: rpc.RegimeIndicatorVolOfVol, cluster: "vol", row: regimerows.VolOfVol(now, r.VolOfVol), asOf: r.VolOfVol.AsOf, date: r.VolOfVol.AsOfDate, status: r.VolOfVol.Status, trip: stressIndicatorTrip(r.VolOfVol.Thresholds), thresholds: r.VolOfVol.Thresholds, eligibility: r.VolOfVol.Eligibility},
		{indicator: rpc.RegimeIndicatorHYGSPY, cluster: "credit", row: regimerows.HYGSPY(now, r.HYGSPYDivergence), asOf: r.HYGSPYDivergence.AsOf, status: r.HYGSPYDivergence.Status, trip: stressIndicatorTrip(r.HYGSPYDivergence.Thresholds), thresholds: r.HYGSPYDivergence.Thresholds, eligibility: r.HYGSPYDivergence.Eligibility},
		{indicator: rpc.RegimeIndicatorCredit, cluster: "credit", row: regimerows.CreditSpreads(now, r.CreditSpreads), asOf: r.CreditSpreads.AsOf, date: r.CreditSpreads.AsOfDate, status: r.CreditSpreads.Status, trip: stressIndicatorTrip(r.CreditSpreads.Thresholds), thresholds: r.CreditSpreads.Thresholds, eligibility: r.CreditSpreads.Eligibility},
		{indicator: rpc.RegimeIndicatorFunding, cluster: "funding", row: regimerows.FundingStress(now, r.FundingStress), asOf: r.FundingStress.AsOf, date: r.FundingStress.AsOfDate, status: r.FundingStress.Status, trip: stressIndicatorTrip(r.FundingStress.Thresholds), thresholds: r.FundingStress.Thresholds, eligibility: r.FundingStress.Eligibility},
		{indicator: rpc.RegimeIndicatorUSDJPY, cluster: "fx", row: regimerows.USDJPY(now, r.USDJPY), asOf: r.USDJPY.AsOf, status: r.USDJPY.Status, trip: stressIndicatorTrip(r.USDJPY.Thresholds), thresholds: r.USDJPY.Thresholds, eligibility: r.USDJPY.Eligibility},
		{indicator: rpc.RegimeIndicatorGammaZero, cluster: "gamma", row: regimerows.Gamma(now, r.GammaZero), asOf: r.GammaZero.AsOf, status: r.GammaZero.Status, trip: stressGammaTrip(r.GammaZero), thresholds: r.GammaZero.Thresholds, eligibility: r.GammaZero.Eligibility},
		{indicator: rpc.RegimeIndicatorBreadth, cluster: "breadth", row: regimerows.Breadth(now, r.Breadth), asOf: r.Breadth.AsOf, status: r.Breadth.Status, trip: stressIndicatorTrip(r.Breadth.Thresholds), thresholds: r.Breadth.Thresholds, eligibility: r.Breadth.Eligibility},
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
			Status:  stressIndicatorStatus(item.row.Band, item.status, contextOnly, item.eligibility),
			AsOf:    stressIndicatorAsOf(item.asOf, item.date, item.row.AsOf),
			Reading: reading,
			Comment: stressIndicatorComment(item.row, reading, contextOnly, stressEligibilityComment(item.indicator, item.eligibility)),
			Trip:    stressBandTrip(item.row.Band, item.thresholds, item.trip),
		})
	}
	return out
}

// stressBandTrip anchors an amber row with the served amber band prose, so an
// amber face names the line it crossed instead of only the red trip it hasn't.
// Green and red rows keep the red trip alone. All prose is served threshold
// text; this layer authors no cutoff of its own.
func stressBandTrip(b regimerows.Band, t *rpc.RegimeThresholds, trip string) string {
	if b != regimerows.BandYellow || t == nil || t.Yellow == "" {
		return trip
	}
	if trip == "" {
		return t.Yellow
	}
	return t.Yellow + " · " + trip
}

// stressIndicatorTrip passes the row's served compact trip through untouched.
func stressIndicatorTrip(t *rpc.RegimeThresholds) string {
	if t == nil {
		return ""
	}
	return t.Trip
}

// stressGammaTrip prefers the measured γ-zero/spot pair, because dealer
func stressGammaTrip(g rpc.RegimeGammaZero) string {
	if anchor := regimerows.GammaTripAnchor(g); anchor != "" {
		return anchor
	}
	return stressIndicatorTrip(g.Thresholds)
}

func stressIndicatorStatus(b regimerows.Band, status string, contextOnly bool, eligibility *rpc.RegimeEligibility) string {
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
		if eligibility == nil || !eligibility.Eligible {
			return "amber"
		}
		return "red"
	default:
		if contextOnly {
			return "context"
		}
		return "n/a"
	}
}

func stressIndicatorAsOf(meta *rpc.RegimeAsOfSummary, date, fallback string) string {
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

func stressIndicatorComment(row regimerows.Row, reading string, contextOnly bool, eligibilityComment string) string {
	parts := []string{}
	add := func(part string) {
		part = strings.TrimSpace(part)
		if part == "" || strings.EqualFold(part, strings.TrimSpace(reading)) || slices.Contains(parts, part) {
			return
		}
		parts = append(parts, part)
	}
	add(row.Reason)
	add(eligibilityComment)
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

func stressEligibilityComment(indicator string, eligibility *rpc.RegimeEligibility) string {
	if eligibility == nil || eligibility.Eligible {
		return ""
	}
	if indicator == rpc.RegimeIndicatorHYGSPY {
		gate, _ := rpc.RegimeGateFor(indicator)
		if slices.Contains(eligibility.Reasons, "depth_below_min") {
			return fmt.Sprintf("Provisional: confirmation starts at %.2f%% below the 50-day average", gate.MinDepth)
		}
		for _, reason := range eligibility.Reasons {
			if strings.HasPrefix(reason, "streak_") {
				return fmt.Sprintf("Provisional: confirmation needs %d sessions", gate.MinSessions)
			}
		}
	}
	return "Provisional: waiting for confirmation"
}

func stressGammaDegraded(g rpc.RegimeGammaZero) bool {
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
		return !stressGammaContextOnly(g)
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

func stressRow(title string, direction risk.SignalDirection, severity risk.SignalSeverity, guidance, evidence string) StressRow {
	return StressRow{
		Title:     title,
		Direction: direction,
		Severity:  severity,
		Guidance:  guidance,
		Evidence:  evidence,
	}
}

func stressMarginRow(p StressPortfolioSummary) StressRow {
	cushion := stressWorstCushionPct(p)
	if cushion != nil {
		switch {
		case *cushion < stressPolicy.MarginUrgentPct:
			return stressRow("Immediate margin safety", risk.DirectionDefensive, risk.SeverityUrgent, fmt.Sprintf("Move to cash-heavy / near-flat now; margin cushion is below %.0f%%.", stressPolicy.MarginUrgentPct), stressCushionEvidence(p))
		case *cushion < stressPolicy.MarginActPct:
			return stressRow("Immediate margin safety", risk.DirectionDefensive, risk.SeverityAct, fmt.Sprintf("Cut gross and net exposure until cushion is back above %.0f%%.", stressPolicy.MarginTargetPct), stressCushionEvidence(p))
		case *cushion < stressPolicy.MarginWatchPct:
			return stressRow("Immediate margin safety", risk.DirectionDefensive, risk.SeverityWatch, fmt.Sprintf("Do not add risk; prepare a reduction plan if cushion falls below %.0f%%.", stressPolicy.MarginTargetPct), stressCushionEvidence(p))
		}
		return stressRow("Immediate margin safety", "", risk.SeverityObserve, "No forced margin action.", stressCushionEvidence(p))
	}
	return stressRow("Immediate margin safety", risk.DirectionDataQuality, risk.SeverityWatch, "No forced margin action, but confirm account cushion before sizing new risk.", "cushion unavailable")
}

func stressPnLShockRow(p StressPortfolioSummary) StressRow {
	if p.DailyPnLPct == nil {
		return stressRow("Portfolio P&L shock", risk.DirectionDataQuality, risk.SeverityWatch, "Daily P&L is unavailable; this indicator cannot confirm or reject a P&L shock.", "daily P&L unavailable")
	}
	pct := *p.DailyPnLPct
	absPct := math.Abs(pct)
	evidence := fmt.Sprintf("daily P&L %+.1f%% NLV (watch at ±%.0f%%)", pct, stressPolicy.DailyPnLWatchPct)
	if absPct >= stressPolicy.DailyPnLActPct {
		if pct < 0 {
			return stressRow("Portfolio P&L shock", risk.DirectionDefensive, risk.SeverityAct, "Large daily loss; review defensive actions and protect liquidity.", evidence)
		}
		return stressRow("Portfolio P&L shock", risk.DirectionDefensive, risk.SeverityWatch, "Large daily gain; protect gains and avoid accidental chase-risk.", evidence)
	}
	if absPct >= stressPolicy.DailyPnLWatchPct {
		if pct < 0 {
			return stressRow("Portfolio P&L shock", risk.DirectionDefensive, risk.SeverityWatch, "Daily loss is large enough to review risk before adding exposure.", evidence)
		}
		return stressRow("Portfolio P&L shock", risk.DirectionDefensive, risk.SeverityWatch, "Daily gain is large enough to review sizing and opportunity deliberately.", evidence)
	}
	return stressRow("Portfolio P&L shock", "", risk.SeverityObserve, "No daily P&L shock signal.", evidence)
}

func stressMarketRow(m StressMarketSummary) StressRow {
	evidence := stressMarketEvidence(m)
	actConfirmable := stressGovernedSeverityAllows(m, risk.SeverityAct)
	switch {
	case m.EligibleRedClusters >= 3 && m.RankedClusters >= 4 && actConfirmable:
		return stressRow("Confirmed market stress", risk.DirectionDefensive, risk.SeverityAct, "Reduce equity beta materially; reserve urgent action for margin or exposure rows.", evidence)
	case m.EligibleRedClusters >= 2 && m.RankedClusters >= 3 && actConfirmable:
		return stressRow("Confirmed market stress", risk.DirectionDefensive, risk.SeverityAct, "Cut marginal longs and short-convexity exposure; keep only intentional hedged risk.", evidence)
	case m.EligibleRedClusters >= 2 && m.RankedClusters >= 3:
		return stressRow("Confirmed stress held at watch", risk.DirectionDefensive, risk.SeverityWatch, "Regime policy holds this confirmed stage at watch; freeze new risk and stage reductions — no act-grade de-risking from this signal alone.", evidence)
	case stressFastCarryUnwind(m):
		return stressRow("Fast carry unwind", risk.DirectionDefensive, risk.SeverityAct, "Reduce fragile beta and short-vol exposure; FX stress is confirmed by tape or breadth.", evidence)
	case m.RedClusters >= 2:
		return stressRow("Stress pending confirmation", risk.DirectionDefensive, risk.SeverityWatch, "Stress clusters are visible but not confirmed yet (need more depth, persistence, or fresh data); hold de-risking at watch.", evidence)
	case m.RedClusters == 1 && m.YellowClusters >= 1:
		return stressRow("Early stress filtered", risk.DirectionDefensive, risk.SeverityWatch, "Wait for a second independent red cluster before major de-risking.", evidence)
	case m.YellowClusters >= 3:
		return stressRow("Deteriorating tape", risk.DirectionDefensive, risk.SeverityWatch, "Freeze new risk and review hedges; no urgent action without red confirmation.", evidence)
	default:
		if len(m.UnconfirmedRedClusterNames) > 0 {
			// The overall summary calls this warning out; the check still
			return stressRow("Market stress", "", risk.SeverityObserve, "An early warning is flashing ("+stressClusterList(m.UnconfirmedRedClusterNames)+" red, not confirmed); below the de-risking trigger.", evidence)
		}
		return stressRow("Market stress", "", risk.SeverityObserve, "No market-regime de-risking trigger.", evidence)
	}
}

func stressTapeShockRow(p StressPortfolioSummary, m StressMarketSummary) StressRow {
	evidence := stressTapeEvidence(m)
	if m.SPYChangePct == nil && m.VIXChangePct == nil {
		return stressRow("Index tape shock", risk.DirectionDataQuality, risk.SeverityWatch, "Direct SPY/VIX tape is unavailable; do not treat quiet regime clusters as complete overnight coverage.", evidence)
	}
	spyDrop := pctAtMost(m.SPYChangePct, stressPolicy.SPYDropPct)
	spyHardDrop := pctAtMost(m.SPYChangePct, stressPolicy.SPYHardDropPct)
	spyCrash := pctAtMost(m.SPYChangePct, stressPolicy.SPYCrashPct)
	vixSpike := pctAtLeast(m.VIXChangePct, stressPolicy.VIXSpikePct)
	vixHardSpike := pctAtLeast(m.VIXChangePct, stressPolicy.VIXHardSpikePct)
	if !stressTapeConfirmable(m) {
		if spyDrop || vixSpike {
			return stressRow("Index tape shock", "", risk.SeverityObserve, stressTapeDemotedGuidance(m), evidence)
		}
		return stressRow("Index tape shock", "", risk.SeverityObserve, "No direct SPY/VIX overnight tape shock.", evidence)
	}
	confirmed := (spyDrop && vixSpike) || m.EligibleRedClusters >= 1
	switch {
	case spyCrash && confirmed:
		return stressRow("Index tape shock", risk.DirectionDefensive, risk.SeverityAct, "Cut broad equity beta now; SPY is in a severe direct tape drawdown with confirmation.", evidence)
	case spyHardDrop && confirmed:
		return stressRow("Index tape shock", risk.DirectionDefensive, risk.SeverityAct, "Cut marginal longs and pre-hedge remaining beta; direct SPY stress is confirmed.", evidence)
	case vixHardSpike && (spyDrop || m.EligibleRedClusters >= 1):
		return stressRow("Index tape shock", risk.DirectionDefensive, risk.SeverityAct, "Reduce short-vol and high-beta exposure; direct VIX stress is confirmed.", evidence)
	case spyHardDrop || vixHardSpike || (spyDrop && vixSpike):
		return stressRow("Index tape shock", risk.DirectionDefensive, risk.SeverityWatch, "Freeze new risk and run a second pass; direct overnight tape stress needs confirmation before urgent action.", evidence)
	case spyDrop || vixSpike:
		return stressRow("Index tape shock", risk.DirectionDefensive, risk.SeverityWatch, "Freeze new risk; direct SPY/VIX tape is flashing early stress but not enough for defensive action alone.", evidence)
	default:
		return stressRow("Index tape shock", "", risk.SeverityObserve, "No direct SPY/VIX overnight tape shock.", evidence)
	}
}

func stressExposureRow(p StressPortfolioSummary, m StressMarketSummary) StressRow {
	gross := derefPct(p.GrossExposurePctNLV)
	delta := derefPct(p.NetDeltaPctNLV)
	grossDelta := derefPct(p.GrossDeltaPctNLV)
	evidence := fmt.Sprintf("gross %.0f%% NLV (watch %.0f%%); net delta %.0f%% NLV (watch %.0f%%); gross delta %.0f%% NLV (watch %.0f%%)",
		gross, stressPolicy.GrossExposureWatchPct, delta, stressPolicy.NetDeltaWatchPct, grossDelta, stressPolicy.GrossDeltaWatchPct)
	// Disclosure is unconditional; only the pass verdict below is conditional.
	gap := stressUnmeasuredNames(p.ExposureUnmeasured)
	if gap != "" {
		evidence += "; " + gap
	}
	stressed := stressClusterStressed(m)
	switch {
	case (gross >= stressPolicy.GrossExposureStressUrgentPct || delta >= stressPolicy.NetDeltaStressUrgentPct || grossDelta >= stressPolicy.GrossDeltaStressUrgentPct) && stressed:
		return stressRow("US equity/options exposure", risk.DirectionDefensive, risk.SeverityUrgent, "Go near-flat on broad equity beta; close or hedge option delta first.", evidence)
	case (gross >= stressPolicy.GrossExposureStressActPct || delta >= stressPolicy.NetDeltaStressActPct || grossDelta >= stressPolicy.GrossDeltaStressActPct) && stressed:
		return stressRow("US equity/options exposure", risk.DirectionDefensive, risk.SeverityAct, "Cut 30-50% of net equity delta and avoid adding long gamma-dollar exposure.", evidence)
	case gross >= stressPolicy.GrossExposureWatchPct || delta >= stressPolicy.NetDeltaWatchPct || grossDelta >= stressPolicy.GrossDeltaWatchPct:
		return stressRow("US equity/options exposure", risk.DirectionRebalance, risk.SeverityWatch, "Exposure is high; rebalance toward risk limits without treating this as confirmed market stress.", evidence)
	default:
		if gap != "" {
			return stressRow("US equity/options exposure", risk.DirectionDataQuality, risk.SeverityWatch, "These readings are a subtotal over the names that could be measured; measure the rest before treating the book as within exposure limits.", evidence)
		}
		return stressRow("US equity/options exposure", "", risk.SeverityObserve, "No exposure-based de-risking trigger.", evidence)
	}
}

func stressConcentrationRow(p StressPortfolioSummary, m StressMarketSummary) StressRow {
	gap := stressUnmeasuredNames(p.ExposureUnmeasured)
	if (p.LargestExposurePct == nil || p.LargestExposure == "") && (p.LargestDeltaPctNLV == nil || p.LargestDeltaExposure == "") {
		if gap != "" {
			return stressRow("Largest concentration", risk.DirectionDataQuality, risk.SeverityWatch, "No concentration verdict is possible: no held name could be measured in the base currency.", gap)
		}
		return stressRow("Largest concentration", "", risk.SeverityObserve, "No concentration action from available base-currency exposure map.", "no dominant exposure")
	}
	pct := math.Abs(derefPct(p.LargestExposurePct))
	deltaPct := derefPct(p.LargestDeltaPctNLV)
	evidence := stressConcentrationEvidence(p)
	if gap != "" {
		evidence += "; " + gap
	}
	if (pct >= stressPolicy.SingleNameExposureWatchPct || deltaPct >= stressPolicy.SingleNameDeltaWatchPct) && stressClusterStressed(m) {
		return stressRow("Largest concentration", risk.DirectionDefensive, risk.SeverityAct, fmt.Sprintf("Trim this concentration before smaller positions; cap it below %.0f%% NLV in stress.", stressPolicy.SingleNameTargetPct), evidence)
	}
	if pct >= stressPolicy.SingleNameExposureWatchPct || deltaPct >= stressPolicy.SingleNameDeltaWatchPct {
		return stressRow("Largest concentration", risk.DirectionRebalance, risk.SeverityWatch, "Concentration is above risk limits; rebalance this position without treating it as confirmed market stress.", evidence)
	}
	if gap != "" {
		return stressRow("Largest concentration", risk.DirectionDataQuality, risk.SeverityWatch, "The largest position may be one of the names that could not be measured; this is not a clean concentration pass.", evidence)
	}
	return stressRow("Largest concentration", "", risk.SeverityObserve, "No concentration trim required by the stress policy.", evidence)
}

func stressProtectionCoverageRow(p StressPortfolioSummary) StressRow {
	coverage := p.ProtectionCoverage
	if coverage == nil {
		return stressRow("Protection coverage", risk.DirectionDataQuality, risk.SeverityWatch, "Protection coverage is unavailable; use positions risk and open orders before relying on stop coverage.", "coverage unavailable")
	}
	evidence := formatProtectionCoverageEvidence(coverage)
	if coverage.Counts.OrphanedOrder > 0 || coverage.Counts.ReconcileRequired > 0 {
		return stressRow("Protection coverage", risk.DirectionRebalance, risk.SeverityWatch, "Reconcile stale protective orders before counting them as coverage.", evidence)
	}
	if coverage.Counts.Unprotected > 0 || coverage.Counts.Partial > 0 {
		guidance := "Review largest unprotected stock/ETF exposures before adding risk."
		// Naming the position turns "go look somewhere" into a decision the
		// row itself supports; the phrase shape mirrors the Monitor protection
		// panel ("largest unprotected SYM amount").
		if largest := largestUnprotectedPhrase(coverage); largest != "" {
			guidance = "Review largest unprotected stock/ETF exposures before adding risk; largest unprotected " + largest + "."
		}
		return stressRow("Protection coverage", risk.DirectionRebalance, risk.SeverityWatch, guidance, evidence)
	}
	if coverage.Counts.Unknown > 0 || coverage.Status == rpc.ProtectionCoverageStateUnknown {
		return stressRow("Protection coverage", risk.DirectionDataQuality, risk.SeverityWatch, "Open-order coverage is unknown; confirm open orders before relying on stop coverage.", evidence)
	}
	return stressRow("Protection coverage", "", risk.SeverityObserve, "No stock/ETF protection coverage issue in the current open-order ledger.", evidence)
}

func heldStressRow(p StressPortfolioSummary, m StressMarketSummary) StressRow {
	if len(p.HeldStress) == 0 {
		return stressRow("Held-name stress", "", risk.SeverityObserve, "No material held-name stress from existing positions data.", "no material held-name stress")
	}
	signals := heldStressSignals(p.HeldStress, m)
	direction, severity := heldStressRowState(signals)
	evidence := heldStressEvidence(p.HeldStress)
	switch direction {
	case risk.DirectionDefensive:
		return stressRow("Held-name stress", direction, severity, "Held-name stress aligns with confirmed market pressure; review material underlyings before smaller positions.", evidence)
	case risk.DirectionRebalance:
		return stressRow("Held-name stress", direction, severity, "Review material held names before adding risk; rebalance stressed names without treating this as market-confirmed defense.", evidence)
	case risk.DirectionDataQuality:
		return stressRow("Held-name stress", direction, severity, "Confirm held-name quotes and option bid/ask context before acting on those names.", evidence)
	default:
		return stressRow("Held-name stress", "", risk.SeverityObserve, "No material held-name stress from existing positions data.", evidence)
	}
}

func heldStressRowState(signals []risk.Signal) (risk.SignalDirection, risk.SignalSeverity) {
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

func stressOptionsRow(p StressPortfolioSummary, pos rpc.PositionsResult, m StressMarketSummary) StressRow {
	if pos.Portfolio == nil || pos.Portfolio.GreeksTotal == 0 {
		if len(pos.Options) > 0 {
			return stressRow("Options convexity", risk.DirectionDataQuality, risk.SeverityWatch, "Option positions are present but greeks coverage is unavailable; do not escalate options-specific actions from this snapshot.", "option greeks unavailable")
		}
		return stressRow("Options convexity", "", risk.SeverityObserve, "No option-greeks action from the current portfolio snapshot.", "no option greeks required")
	}
	coverage := float64(pos.Portfolio.GreeksCoverage) / float64(pos.Portfolio.GreeksTotal) * 100
	evidence := fmt.Sprintf("greeks %.0f%% covered (%s)", coverage, p.OptionGreeks)
	if coverage < stressPolicy.OptionGreeksMinCoveragePct {
		return stressRow("Options convexity", risk.DirectionDataQuality, risk.SeverityWatch, fmt.Sprintf("Do not escalate options-specific actions until greeks coverage is at least %.0f%%.", stressPolicy.OptionGreeksMinCoveragePct), evidence)
	}
	if pos.Portfolio.Gamma != nil && *pos.Portfolio.Gamma < 0 && m.EligibleRedClusters >= 2 && stressGovernedSeverityAllows(m, risk.SeverityAct) {
		return stressRow("Options convexity", risk.DirectionDefensive, risk.SeverityAct, "Reduce negative-gamma structures first; prefer defined-risk or hedged residuals.", evidence)
	}
	return stressRow("Options convexity", "", risk.SeverityObserve, "No option-convexity de-risking trigger.", evidence)
}

func stressDataQualityRow(m StressMarketSummary, r rpc.RegimeSnapshotResult) StressRow {
	if stressHasMarketDataIssue(m) && (m.RedClusters > 0 || m.YellowClusters > 0) {
		return stressRow("Ambiguity filter", risk.DirectionDataQuality, risk.SeverityWatch, "Some market inputs cannot be confirmed right now; treat the stress readings as tentative until those inputs report.", stressAmbiguityEvidence(m))
	}
	if stressHasMarketDataIssue(m) {
		return stressRow("Ambiguity filter", risk.DirectionDataQuality, risk.SeverityWatch, "Some market inputs are incomplete; treat this snapshot as partial until coverage and freshness recover.", stressAmbiguityEvidence(m))
	}
	if m.RankedClusters < 4 {
		return stressRow("Data quality gate", risk.DirectionDataQuality, risk.SeverityWatch, "Verify market coverage before action; fewer than four of six market clusters are reporting.", stressMarketEvidence(m))
	}
	if r.GammaZero.Status == rpc.RegimeStatusComputing || r.Breadth.Status == rpc.RegimeStatusComputing {
		return stressRow("Data quality gate", risk.DirectionDataQuality, risk.SeverityWatch, "Do not escalate on gamma/breadth while their data is still computing.", stressMarketEvidence(m))
	}
	return stressRow("Data quality gate", "", risk.SeverityObserve, "Market data coverage is sufficient for the stress policy.", stressMarketEvidence(m))
}

func stressOverallRow(direction risk.SignalDirection, severity risk.SignalSeverity, summary string, m StressMarketSummary, p StressPortfolioSummary) StressRow {
	return stressRow("Portfolio stress", direction, severity, summary, fmt.Sprintf("%s; %s", stressMarketEvidence(m), stressPortfolioEvidence(p)))
}

const (
	stressMarketNone      = "none"
	stressMarketPartial   = "partial"
	stressMarketConfirmed = "confirmed"
	stressMarketBlocked   = "blocked"

	stressPortfolioFitUnknown = "unknown"
	stressPortfolioFitLow     = "low"
	stressPortfolioFitMedium  = "medium"
	stressPortfolioFitHigh    = "high"

	stressInputOK       = "ok"
	stressInputWarming  = "warming"
	stressInputDegraded = "degraded"
	stressInputFailed   = "failed"

	stressActionStandDown     = "stand_down"
	stressActionWatch         = "watch"
	stressActionDefend        = "defend"
	stressActionRebalance     = "rebalance"
	stressActionDeploy        = "deploy"
	stressActionConfirmInputs = "confirm_inputs"
)

func stressMarketConfirmation(m StressMarketSummary) string {
	if m.RankedClusters < 4 {
		return stressMarketBlocked
	}
	if confirmedMarketStress(m) || confirmedConstructiveTape(m) {
		return stressMarketConfirmed
	}
	if partialMarketPressure(m) || partialConstructiveTape(m) {
		return stressMarketPartial
	}
	return stressMarketNone
}

// stressGovernedSeverityAllows reports whether cluster-count evidence may
// The lifecycle is the authority on heuristic cluster severity: its governor
// combination. Tape-driven arms never route through this gate: the tape is
// without a ranked stress lifecycle stage fail open to the raw counts.
func stressGovernedSeverityAllows(m StressMarketSummary, want risk.SignalSeverity) bool {
	switch m.RegimePosture.Stage {
	case rpc.LifecycleConfirmedStress, rpc.LifecyclePanic:
		severity := risk.SignalSeverity(strings.ToLower(strings.TrimSpace(m.RegimePosture.Severity)))
		return severityRankAtLeast(severity, want)
	}
	return true
}

// stressClusterStressed is the act-grade market-stress context used by
// portfolio-side escalation: governed-confirmable eligible reds, or tape
// confirmation.
func stressClusterStressed(m StressMarketSummary) bool {
	return (m.EligibleRedClusters >= 2 && stressGovernedSeverityAllows(m, risk.SeverityAct)) || confirmedTapeStress(m)
}

func confirmedMarketStress(m StressMarketSummary) bool {
	return (m.EligibleRedClusters >= 2 && len(unhealthyConfirmingClusters(m)) == 0 && stressGovernedSeverityAllows(m, risk.SeverityAct)) || confirmedTapeStress(m)
}

func partialMarketPressure(m StressMarketSummary) bool {
	return m.RedClusters >= 1 ||
		m.YellowClusters >= 3 ||
		len(m.UnconfirmedRedClusterNames) > 0 ||
		(stressTapeConfirmable(m) &&
			(pctAtMost(m.SPYChangePct, stressPolicy.SPYDropPct) ||
				pctAtLeast(m.VIXChangePct, stressPolicy.VIXSpikePct)))
}

func confirmedConstructiveTape(m StressMarketSummary) bool {
	return stressTapeConfirmable(m) &&
		(pctAtLeast(m.SPYChangePct, stressPolicy.SPYHardRallyPct) ||
			pctAtMost(m.VIXChangePct, stressPolicy.VIXHardCrushPct))
}

func partialConstructiveTape(m StressMarketSummary) bool {
	return stressTapeConfirmable(m) &&
		(pctAtLeast(m.SPYChangePct, stressPolicy.SPYRallyPct) ||
			pctAtMost(m.VIXChangePct, stressPolicy.VIXCrushPct))
}

func stressPortfolioFit(p StressPortfolioSummary, signals []risk.Signal) string {
	if p.NetLiquidation <= 0 {
		return stressPortfolioFitUnknown
	}
	hasMedium := false
	blindExposure := false
	for _, sig := range signals {
		if len(sig.BlockedBy) > 0 || sig.Direction == risk.DirectionDataQuality {
			// A skipped exposure-family signal means the classifier is blind
			// on that axis. "Low" must remain a measurement; when the
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
			return stressPortfolioFitHigh
		case risk.SignalMarginCushionLow,
			risk.SignalLookAheadCushionLow,
			risk.SignalPortfolioPnLShock,
			risk.SignalOptionGreeksDegraded:
			hasMedium = true
		}
	}
	if hasMedium {
		return stressPortfolioFitMedium
	}
	if blindExposure {
		return stressPortfolioFitUnknown
	}
	return stressPortfolioFitLow
}

// stressPortfolioAlertRelevant is the single policy copy for "does this
// snapshot concern the live portfolio enough to alert on": only a low-fit,
// flat book (no held stress, every exposure print under 0.5% NLV) is market
// weather rather than a portfolio alert. Unknown fit stays relevant — an
// unmeasurable portfolio must never be silenced. The app alert gate and the
// SPA preview gate read the stamped PortfolioAlertRelevant field instead of
// re-deriving these edge cases.
func stressPortfolioAlertRelevant(r *StressResult) bool {
	if r.PortfolioFit != stressPortfolioFitLow {
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

func stressInputHealth(in StressInput, m StressMarketSummary, sourceIssues []stressSourceIssue, now time.Time) string {
	switch {
	case in.Account.NetLiquidation <= 0:
		return stressInputFailed
	case stressAccountDailyPnLFailed(in.Account, now):
		if in.Account.DailyPnLObservation != nil {
			return stressInputDegraded
		}
		return stressInputWarming
	case len(sourceIssues) > 0 || stressHasMarketDataIssue(m):
		return stressInputDegraded
	default:
		return stressInputOK
	}
}

func stressDecisionState(marketConfirmation, portfolioFit, inputHealth string, m StressMarketSummary, signals []risk.Signal) (risk.SignalDirection, risk.SignalSeverity) {
	if inputHealth == stressInputFailed || marketConfirmation == stressMarketBlocked {
		return risk.DirectionDataQuality, risk.SeverityWatch
	}
	if stressHasConfirmedConstructiveSignal(signals) && inputHealth == stressInputOK && portfolioFit == stressPortfolioFitLow {
		return risk.DirectionConstructive, risk.SeverityWatch
	}
	if marketConfirmation == stressMarketConfirmed && portfolioFit == stressPortfolioFitHigh {
		if inputHealth == stressInputOK {
			if stressHasUrgentPortfolioShape(signals) {
				return risk.DirectionDefensive, risk.SeverityUrgent
			}
			if stressPanicMarket(m) {
				return risk.DirectionDefensive, risk.SeverityUrgent
			}
			return risk.DirectionDefensive, risk.SeverityAct
		}
		return risk.DirectionDefensive, risk.SeverityWatch
	}
	if marketConfirmation == stressMarketConfirmed && portfolioFit == stressPortfolioFitMedium {
		return risk.DirectionDefensive, risk.SeverityWatch
	}
	if marketConfirmation == stressMarketConfirmed && portfolioFit == stressPortfolioFitLow {
		return risk.DirectionDefensive, risk.SeverityWatch
	}
	if marketConfirmation == stressMarketPartial && (portfolioFit == stressPortfolioFitHigh || portfolioFit == stressPortfolioFitMedium) {
		return risk.DirectionDefensive, risk.SeverityWatch
	}
	if marketConfirmation == stressMarketPartial && portfolioFit == stressPortfolioFitLow {
		return risk.DirectionDefensive, risk.SeverityWatch
	}
	// Unmeasured exposure against live market pressure keeps the defensive
	// watch frame: the market signal is real and must not be demoted to a
	// data-quality footnote just because the portfolio side is blind.
	if (marketConfirmation == stressMarketConfirmed || marketConfirmation == stressMarketPartial) && portfolioFit == stressPortfolioFitUnknown {
		return risk.DirectionDefensive, risk.SeverityWatch
	}
	if marketConfirmation == stressMarketNone && portfolioFit == stressPortfolioFitHigh {
		return risk.DirectionRebalance, risk.SeverityWatch
	}
	if inputHealth == stressInputWarming || inputHealth == stressInputDegraded {
		return risk.DirectionDataQuality, risk.SeverityWatch
	}
	return "", risk.SeverityObserve
}

func stressHasUrgentPortfolioShape(signals []risk.Signal) bool {
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

func stressHasConfirmedConstructiveSignal(signals []risk.Signal) bool {
	for _, sig := range signals {
		if sig.Direction == risk.DirectionConstructive && severityRankAtLeast(sig.Severity, risk.SeverityWatch) && len(sig.BlockedBy) == 0 {
			return true
		}
	}
	return false
}

func stressPanicMarket(m StressMarketSummary) bool {
	return (m.EligibleRedClusters >= 3 && stressGovernedSeverityAllows(m, risk.SeverityUrgent)) ||
		(stressTapeConfirmable(m) &&
			(pctAtMost(m.SPYChangePct, stressPolicy.SPYCrashPct) ||
				(pctAtLeast(m.VIXChangePct, stressPolicy.VIXHardSpikePct) && m.EligibleRedClusters >= 1)))
}

func stressAction(direction risk.SignalDirection, severity risk.SignalSeverity, marketConfirmation, portfolioFit, inputHealth string) string {
	if inputHealth == stressInputFailed || marketConfirmation == stressMarketBlocked {
		return stressActionConfirmInputs
	}
	if direction == risk.DirectionDataQuality {
		if portfolioFit == stressPortfolioFitHigh && marketConfirmation == stressMarketPartial {
			return stressActionWatch
		}
		return stressActionConfirmInputs
	}
	if direction == risk.DirectionDefensive {
		if severityRankAtLeast(severity, risk.SeverityAct) && marketConfirmation == stressMarketConfirmed && portfolioFit == stressPortfolioFitHigh && inputHealth == stressInputOK {
			return stressActionDefend
		}
		return stressActionWatch
	}
	if direction == risk.DirectionRebalance {
		return stressActionRebalance
	}
	if direction == risk.DirectionConstructive {
		if marketConfirmation == stressMarketConfirmed && inputHealth == stressInputOK {
			return stressActionDeploy
		}
		return stressActionWatch
	}
	if severity == risk.SeverityWatch {
		return stressActionWatch
	}
	return stressActionStandDown
}

func stressPlannerModeFromAction(action string) risk.PlannerMode {
	switch action {
	case stressActionConfirmInputs:
		return risk.PlannerModeConfirmData
	case stressActionDefend:
		return risk.PlannerModeDefend
	case stressActionRebalance:
		return risk.PlannerModeRebalance
	case stressActionDeploy:
		return risk.PlannerModeDeploy
	case stressActionWatch:
		return risk.PlannerModeStage
	default:
		return risk.PlannerModeNone
	}
}

func stressPlannerReadinessFromAction(action string, severity risk.SignalSeverity, inputHealth string) risk.PlannerReadiness {
	switch action {
	case stressActionConfirmInputs:
		return risk.PlannerReadinessBlocked
	case stressActionDefend, stressActionDeploy:
		if inputHealth == stressInputOK {
			return risk.PlannerReadinessReady
		}
		return risk.PlannerReadinessPrestage
	case stressActionRebalance:
		return risk.PlannerReadinessReady
	case stressActionWatch:
		return risk.PlannerReadinessPrestage
	default:
		if severity == risk.SeverityWatch {
			return risk.PlannerReadinessWatch
		}
		return risk.PlannerReadinessNone
	}
}

func stressDecisionSummary(r StressResult) string {
	switch r.Action {
	case stressActionDefend:
		return "Market stress is confirmed against a vulnerable portfolio; review defensive actions."
	case stressActionWatch:
		if r.PortfolioFit == stressPortfolioFitLow {
			if r.MarketConfirmation == stressMarketConfirmed {
				return "Market stress is confirmed, but your exposure is low; keep watching — no reductions needed."
			}
			return stressPartialMarketSummary(r.Market) + ", but your exposure is low; keep watching — no reductions needed."
		}
		if r.PortfolioFit == stressPortfolioFitUnknown {
			head := "Market stress is confirmed"
			if r.MarketConfirmation != stressMarketConfirmed {
				head = stressPartialMarketSummary(r.Market)
			}
			return head + ", and your portfolio exposure could not be measured from this snapshot; verify exposure before relying on this reading."
		}
		if r.MarketConfirmation == stressMarketPartial {
			return stressPartialMarketSummary(r.Market) + " and the portfolio is exposed; freeze new risk and stage reductions."
		}
		return "Watch this portfolio against market weather; do not run a major action from this snapshot alone."
	case stressActionRebalance:
		return "Portfolio shape is outside risk limits, but market stress is not confirmed; rebalance through the portfolio-risk workflow."
	case stressActionDeploy:
		return "Constructive pressure is present and input health is clean; deploy only inside risk budget."
	case stressActionConfirmInputs:
		return "Confirm input health before treating this stress read as a market-context signal."
	default:
		return "No market-context stress action."
	}
}

func stressPartialMarketSummary(m StressMarketSummary) string {
	if m.EligibleRedClusters == 0 && len(m.UnconfirmedRedClusterNames) > 0 {
		return "An early market warning is flashing (" + stressClusterList(m.UnconfirmedRedClusterNames) + " red, not confirmed yet)"
	}
	if m.EligibleRedClusters >= 2 && !stressGovernedSeverityAllows(m, risk.SeverityAct) {
		return "Confirmed market stress is held at watch by regime policy"
	}
	return "Market pressure is building"
}

func stressSignals(p StressPortfolioSummary, pos rpc.PositionsResult, m StressMarketSummary, r rpc.RegimeSnapshotResult) []risk.Signal {
	signals := []risk.Signal{}
	signals = append(signals, stressMarginSignals(p)...)
	signals = append(signals, stressPnLSignals(p)...)
	signals = append(signals, stressTapeSignals(p, m)...)
	signals = append(signals, stressRegimeSignals(m)...)
	signals = append(signals, stressExposureSignals(p, m)...)
	signals = append(signals, stressConcentrationSignals(p, m)...)
	signals = append(signals, heldStressSignals(p.HeldStress, m)...)
	signals = append(signals, stressOptionSignals(pos, m)...)
	signals = append(signals, stressDataQualitySignals(m, r)...)
	for i := range signals {
		if signals[i].Posture == "" {
			signals[i].Posture = stressSignalPosture(signals[i].Direction)
		}
	}
	return signals
}

func stressMarginSignals(p StressPortfolioSummary) []risk.Signal {
	out := []risk.Signal{}
	addCushion := func(id risk.SignalID, metric string, observed *float64) {
		if observed == nil {
			return
		}
		severity, threshold, ok := stressCushionSeverity(*observed)
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
			out[len(out)-1].Target = new(stressPolicy.MarginTargetPct)
		}
	}
	addCushion(risk.SignalMarginCushionLow, "cushion", p.CushionPct)
	addCushion(risk.SignalLookAheadCushionLow, "lookahead_cushion", p.LookAheadCushionPct)
	return out
}

func stressCushionSeverity(v float64) (risk.SignalSeverity, float64, bool) {
	switch {
	case v < stressPolicy.MarginUrgentPct:
		return risk.SeverityUrgent, stressPolicy.MarginUrgentPct, true
	case v < stressPolicy.MarginActPct:
		return risk.SeverityAct, stressPolicy.MarginActPct, true
	case v < stressPolicy.MarginWatchPct:
		return risk.SeverityWatch, stressPolicy.MarginWatchPct, true
	default:
		return "", 0, false
	}
}

func stressPnLSignals(p StressPortfolioSummary) []risk.Signal {
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
	if absPct < stressPolicy.DailyPnLWatchPct {
		return nil
	}
	direction := risk.DirectionDefensive
	severity := risk.SeverityWatch
	threshold := stressPolicy.DailyPnLWatchPct
	confidenceImpact := ""
	if pct < 0 && absPct >= stressPolicy.DailyPnLActPct {
		severity = risk.SeverityAct
		threshold = stressPolicy.DailyPnLActPct
	} else if pct > 0 && absPct >= stressPolicy.DailyPnLActPct {
		threshold = stressPolicy.DailyPnLActPct
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

func stressTapeSignals(p StressPortfolioSummary, m StressMarketSummary) []risk.Signal {
	out := []risk.Signal{}
	if !stressTapeConfirmable(m) {
		// Closed market date: the frozen day-change prints stay visible as
		// evidence on the tape row, but emit no defensive or constructive
		// tape signals until live prints return at the next open.
		return out
	}
	spyDrop := pctAtMost(m.SPYChangePct, stressPolicy.SPYDropPct)
	vixSpike := pctAtLeast(m.VIXChangePct, stressPolicy.VIXSpikePct)
	confirmedDrop := (spyDrop && vixSpike) || m.EligibleRedClusters >= 1
	confirmedVIXSpike := spyDrop || m.EligibleRedClusters >= 1
	if m.SPYChangePct != nil {
		switch {
		case *m.SPYChangePct <= stressPolicy.SPYCrashPct:
			severity, blockedBy := confirmedSignalSeverity(confirmedDrop)
			out = append(out, tapeSignal(risk.SignalMarketSelloffViolent, risk.DirectionDefensive, severity, "spy_change_pct", *m.SPYChangePct, stressPolicy.SPYCrashPct, blockedBy...))
		case *m.SPYChangePct <= stressPolicy.SPYHardDropPct:
			severity, blockedBy := confirmedSignalSeverity(confirmedDrop)
			out = append(out, tapeSignal(risk.SignalMarketSelloffViolent, risk.DirectionDefensive, severity, "spy_change_pct", *m.SPYChangePct, stressPolicy.SPYHardDropPct, blockedBy...))
		case *m.SPYChangePct <= stressPolicy.SPYDropPct:
			out = append(out, tapeSignal(risk.SignalMarketSelloffViolent, risk.DirectionDefensive, risk.SeverityWatch, "spy_change_pct", *m.SPYChangePct, stressPolicy.SPYDropPct))
		case *m.SPYChangePct >= stressPolicy.SPYHardRallyPct:
			out = append(out, tapeSignal(risk.SignalMarketRallyViolent, risk.DirectionConstructive, risk.SeverityAct, "spy_change_pct", *m.SPYChangePct, stressPolicy.SPYHardRallyPct))
		case *m.SPYChangePct >= stressPolicy.SPYRallyPct:
			out = append(out, tapeSignal(risk.SignalMarketRallyViolent, risk.DirectionConstructive, risk.SeverityWatch, "spy_change_pct", *m.SPYChangePct, stressPolicy.SPYRallyPct))
		}
	}
	if m.VIXChangePct != nil {
		switch {
		case *m.VIXChangePct >= stressPolicy.VIXHardSpikePct:
			severity, blockedBy := confirmedSignalSeverity(confirmedVIXSpike)
			out = append(out, tapeSignal(risk.SignalVolSpikeConfirmed, risk.DirectionDefensive, severity, "vix_change_pct", *m.VIXChangePct, stressPolicy.VIXHardSpikePct, blockedBy...))
		case *m.VIXChangePct >= stressPolicy.VIXSpikePct:
			out = append(out, tapeSignal(risk.SignalVolSpikeConfirmed, risk.DirectionDefensive, risk.SeverityWatch, "vix_change_pct", *m.VIXChangePct, stressPolicy.VIXSpikePct))
		case *m.VIXChangePct <= stressPolicy.VIXHardCrushPct:
			out = append(out, tapeSignal(risk.SignalVolCrushConfirmed, risk.DirectionConstructive, risk.SeverityAct, "vix_change_pct", *m.VIXChangePct, stressPolicy.VIXHardCrushPct))
		case *m.VIXChangePct <= stressPolicy.VIXCrushPct:
			out = append(out, tapeSignal(risk.SignalVolCrushConfirmed, risk.DirectionConstructive, risk.SeverityWatch, "vix_change_pct", *m.VIXChangePct, stressPolicy.VIXCrushPct))
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

func stressRegimeSignals(m StressMarketSummary) []risk.Signal {
	out := []risk.Signal{}
	switch {
	case m.EligibleRedClusters >= 2 && m.RankedClusters >= 3:
		// Confirmation-grade: only ELIGIBLE reds (depth + persistence +
		// freshness) may put the act-severity stress signal on the wire.
		observed := float64(m.EligibleRedClusters)
		threshold := 2.0
		sig := risk.Signal{ID: risk.SignalRegimeStressConfirmed, Direction: risk.DirectionDefensive, Severity: risk.SeverityAct, Metric: "eligible_red_clusters", Observed: &observed, Threshold: &threshold, Evidence: stressMarketEvidence(m), Confidence: "medium"}
		if !stressGovernedSeverityAllows(m, risk.SeverityAct) {
			sig.Severity = risk.SeverityWatch
			sig.ConfidenceImpact = "regime governor holds confirmed-stage severity at watch; not act-grade until validated or tape co-signs"
		}
		if unhealthy := unhealthyConfirmingClusters(m); len(unhealthy) > 0 {
			sig.BlockedBy = unhealthy
			sig.ConfidenceImpact = "confirmed stress includes unhealthy cluster input; verify before severe market-only action"
		}
		out = append(out, sig)
	case stressFastCarryUnwind(m):
		observed := 1.0
		threshold := 1.0
		out = append(out, risk.Signal{ID: risk.SignalFXCarryUnwind, Direction: risk.DirectionDefensive, Severity: risk.SeverityAct, Subject: "fx", Metric: "red_fx_cluster_with_tape_confirmation", Observed: &observed, Threshold: &threshold, Evidence: stressMarketEvidence(m), Confidence: "medium"})
	case m.RedClusters >= 2 || (m.RedClusters == 1 && m.YellowClusters >= 1):
		// Visible reds without confirmation eligibility warn, never act.
		observed := float64(m.RedClusters)
		threshold := 1.0
		out = append(out, risk.Signal{ID: risk.SignalRegimeStressEarly, Direction: risk.DirectionDefensive, Severity: risk.SeverityWatch, Metric: "red_clusters", Observed: &observed, Threshold: &threshold, Evidence: stressMarketEvidence(m), Confidence: "medium"})
	}
	if slices.Contains(m.RedClusterNames, "gamma") {
		observed := 1.0
		out = append(out, risk.Signal{ID: risk.SignalGammaRed, Direction: risk.DirectionDefensive, Severity: risk.SeverityWatch, Subject: "gamma", Metric: "red_cluster", Observed: &observed, Evidence: "gamma cluster red", Confidence: "medium", ConfidenceImpact: "lower when gamma is degraded"})
	}
	return out
}

func unhealthyConfirmingClusters(m StressMarketSummary) []string {
	out := []string{}
	for _, cluster := range stressUniqueClusters(m.DegradedClusters, m.StaleClusters, m.PartialClusters, m.ComputingClusters) {
		if slices.Contains(m.RedClusterNames, cluster) {
			out = append(out, cluster)
		}
	}
	slices.Sort(out)
	return out
}

func stressExposureSignals(p StressPortfolioSummary, m StressMarketSummary) []risk.Signal {
	stressed := stressClusterStressed(m)
	out := []risk.Signal{}
	out = appendExposureSignal(out, risk.SignalGrossExposureHigh, "gross_exposure_pct_nlv", p.GrossExposurePctNLV, stressPolicy.GrossExposureWatchPct, stressPolicy.GrossExposureStressActPct, stressPolicy.GrossExposureStressUrgentPct, stressed)
	out = appendExposureSignal(out, risk.SignalNetDeltaHigh, "net_delta_pct_nlv", p.NetDeltaPctNLV, stressPolicy.NetDeltaWatchPct, stressPolicy.NetDeltaStressActPct, stressPolicy.NetDeltaStressUrgentPct, stressed)
	out = appendExposureSignal(out, risk.SignalGrossDeltaHigh, "gross_delta_pct_nlv", p.GrossDeltaPctNLV, stressPolicy.GrossDeltaWatchPct, stressPolicy.GrossDeltaStressActPct, stressPolicy.GrossDeltaStressUrgentPct, stressed)
	// Every reading above is a subtotal when a held name went unmeasured, so the
	// overall verdict must not read healthy off them alone.
	if len(p.ExposureUnmeasured) > 0 {
		observed := float64(len(p.ExposureUnmeasured))
		out = append(out, risk.Signal{
			ID: risk.SignalRiskDataDegraded, Direction: risk.DirectionDataQuality, Severity: risk.SeverityWatch,
			Subject: "exposure_base", Metric: "exposure_unmeasured_names", Observed: &observed,
			Evidence: stressUnmeasuredNames(p.ExposureUnmeasured), Confidence: "medium-low",
			ConfidenceImpact: "exposure and concentration readings are subtotals", BlockedBy: []string{"exposure_base"},
		})
	}
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

func stressConcentrationSignals(p StressPortfolioSummary, m StressMarketSummary) []risk.Signal {
	stressed := stressClusterStressed(m)
	severity := risk.SeverityWatch
	direction := risk.DirectionRebalance
	if stressed {
		severity = risk.SeverityAct
		direction = risk.DirectionDefensive
	}
	out := []risk.Signal{}
	if p.LargestExposurePct != nil && math.Abs(*p.LargestExposurePct) >= stressPolicy.SingleNameExposureWatchPct {
		observed := math.Abs(*p.LargestExposurePct)
		out = append(out, risk.Signal{ID: risk.SignalSingleNameExposureHigh, Direction: direction, Severity: severity, Subject: p.LargestExposure, Metric: "market_value_pct_nlv", Observed: &observed, Threshold: new(stressPolicy.SingleNameExposureWatchPct), Target: new(stressPolicy.SingleNameTargetPct), Unit: "pct_nlv", Evidence: fmt.Sprintf("%s market %.0f%% NLV", p.LargestExposure, observed), Confidence: "high"})
	}
	if p.LargestDeltaPctNLV != nil && *p.LargestDeltaPctNLV >= stressPolicy.SingleNameDeltaWatchPct {
		out = append(out, risk.Signal{ID: risk.SignalSingleNameDeltaHigh, Direction: direction, Severity: severity, Subject: p.LargestDeltaExposure, Metric: "delta_pct_nlv", Observed: p.LargestDeltaPctNLV, Threshold: new(stressPolicy.SingleNameDeltaWatchPct), Target: new(stressPolicy.SingleNameTargetPct), Unit: "pct_nlv", Evidence: fmt.Sprintf("%s delta %.0f%% NLV", p.LargestDeltaExposure, *p.LargestDeltaPctNLV), Confidence: "high"})
	}
	return out
}

func heldStressSignals(stresses []rpc.HeldStress, m StressMarketSummary) []risk.Signal {
	out := []risk.Signal{}
	stressed := confirmedMarketStress(m)
	for _, stress := range stresses {
		subject := strings.ToUpper(strings.TrimSpace(stress.Underlying))
		if subject == "" {
			subject = "held_underlying"
		}
		direction := risk.DirectionRebalance
		if stressed {
			direction = risk.DirectionDefensive
		}
		if stress.DailyPnLPctNLV != nil && *stress.DailyPnLPctNLV <= -stressPolicy.HeldUnderlyingPnLWatchPct {
			observed := *stress.DailyPnLPctNLV
			severity := risk.SeverityWatch
			threshold := -stressPolicy.HeldUnderlyingPnLWatchPct
			if observed <= -stressPolicy.HeldUnderlyingPnLActPct {
				severity = risk.SeverityAct
				threshold = -stressPolicy.HeldUnderlyingPnLActPct
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
		if stress.NearExpiryDeltaPctNLV != nil && *stress.NearExpiryDeltaPctNLV >= stressPolicy.HeldOptionDeltaWatchPct {
			observed := *stress.NearExpiryDeltaPctNLV
			severity := risk.SeverityWatch
			threshold := stressPolicy.HeldOptionDeltaWatchPct
			confidenceImpact := ""
			if observed >= stressPolicy.HeldOptionDeltaActPct {
				severity = risk.SeverityAct
				threshold = stressPolicy.HeldOptionDeltaActPct
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

func stressOptionSignals(pos rpc.PositionsResult, m StressMarketSummary) []risk.Signal {
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
	if coverage < stressPolicy.OptionGreeksMinCoveragePct {
		out = append(out, risk.Signal{ID: risk.SignalOptionGreeksDegraded, Direction: risk.DirectionDataQuality, Severity: risk.SeverityWatch, Metric: "option_greeks_coverage_pct", Observed: new(coverage), Threshold: new(stressPolicy.OptionGreeksMinCoveragePct), Unit: "pct", Evidence: fmt.Sprintf("greeks %.0f%% covered", coverage), Confidence: "medium-low", ConfidenceImpact: "blocks option-specific planning"})
	}
	if pos.Portfolio.Gamma != nil && *pos.Portfolio.Gamma < 0 && m.EligibleRedClusters >= 2 && stressGovernedSeverityAllows(m, risk.SeverityAct) {
		out = append(out, risk.Signal{ID: risk.SignalShortConvexityHigh, Direction: risk.DirectionDefensive, Severity: risk.SeverityAct, Metric: "portfolio_gamma", Observed: pos.Portfolio.Gamma, Evidence: "negative portfolio gamma in confirmed market stress", Confidence: "medium"})
	}
	return out
}

func stressDataQualitySignals(m StressMarketSummary, r rpc.RegimeSnapshotResult) []risk.Signal {
	out := []risk.Signal{}
	blockedBy := stressUniqueClusters(m.AmbiguousClusters, m.PartialClusters, m.DegradedClusters, m.ComputingClusters)
	if len(blockedBy) > 0 {
		observed := float64(len(blockedBy))
		out = append(out, risk.Signal{ID: risk.SignalRiskDataDegraded, Direction: risk.DirectionDataQuality, Severity: risk.SeverityWatch, Metric: "degraded_inputs", Observed: &observed, Evidence: stressAmbiguityEvidence(m), Confidence: "medium-low", ConfidenceImpact: "requires verification before severe action", BlockedBy: blockedBy})
	}
	if len(m.StaleClusters) > 0 {
		observed := float64(len(m.StaleClusters))
		out = append(out, risk.Signal{ID: risk.SignalMarketDataStale, Direction: risk.DirectionDataQuality, Severity: risk.SeverityWatch, Metric: "stale_clusters", Observed: &observed, Evidence: "stale " + strings.Join(m.StaleClusters, ","), Confidence: "medium-low", ConfidenceImpact: "requires fresh data", BlockedBy: m.StaleClusters})
	}
	for _, w := range r.WarningDetails {
		if strings.TrimSpace(w.Scope) != "" && strings.Contains(strings.ToLower(w.Severity), "data") {
			out = append(out, risk.Signal{ID: risk.SignalRiskDataDegraded, Direction: risk.DirectionDataQuality, Severity: risk.SeverityWatch, Subject: w.Scope, Evidence: stressWarningLine(w), Confidence: "medium-low", ConfidenceImpact: "source warning"})
		}
	}
	return out
}

func stressUniqueClusters(groups ...[]string) []string {
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

func stressSignalPosture(direction risk.SignalDirection) risk.PortfolioPosture {
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

type stressSourceIssue struct {
	Source string
	Status string
	Reason string
}

func stressSourceIssues(in StressInput, now time.Time) []stressSourceIssue {
	issues := []stressSourceIssue{}
	switch {
	case in.Account.AsOf.IsZero() && in.Account.NetLiquidation > 0:
		issues = append(issues, stressSourceIssue{Source: "account", Status: rpc.RegimeStatusUnavailable, Reason: "account snapshot timestamp missing"})
	case stressSourceStale(in.Account.AsOf, now):
		issues = append(issues, stressSourceIssue{Source: "account", Status: rpc.RegimeStatusStale, Reason: "account snapshot stale"})
	}
	switch {
	case in.Positions.AsOf.IsZero() && in.Account.NetLiquidation > 0:
		// A never-fetched positions snapshot against a real account is a
		// from it is blind, so dependent signals must block and portfolio
		// fit must derive unknown instead of defaulting to low.
		issues = append(issues, stressSourceIssue{Source: "positions", Status: rpc.RegimeStatusUnavailable, Reason: "positions snapshot never fetched"})
	case stressSourceStale(in.Positions.AsOf, now):
		issues = append(issues, stressSourceIssue{Source: "positions", Status: rpc.RegimeStatusStale, Reason: "positions snapshot stale"})
	}
	if issue, ok := stressRegimeAuthorityIssue(in.Regime); ok {
		issues = append(issues, issue)
	}
	issues = append(issues, stressMarketEventSourceIssues(in.Positions, in.MarketEvents, now)...)
	return issues
}

// stressEstablishedSourceIssues is the exact ad5b77b source-decision
// boundary. In particular, a missing account timestamp, Regime authority
func stressEstablishedSourceIssues(in StressInput, now time.Time) []stressSourceIssue {
	issues := []stressSourceIssue{}
	if stressSourceStale(in.Account.AsOf, now) {
		issues = append(issues, stressSourceIssue{Source: "account", Status: rpc.RegimeStatusStale, Reason: "account snapshot stale"})
	}
	switch {
	case in.Positions.AsOf.IsZero() && in.Account.NetLiquidation > 0:
		issues = append(issues, stressSourceIssue{Source: "positions", Status: rpc.RegimeStatusUnavailable, Reason: "positions snapshot never fetched"})
	case stressSourceStale(in.Positions.AsOf, now):
		issues = append(issues, stressSourceIssue{Source: "positions", Status: rpc.RegimeStatusStale, Reason: "positions snapshot stale"})
	}
	return issues
}

func stressRegimeAuthorityIssue(regime rpc.RegimeSnapshotResult) (stressSourceIssue, bool) {
	if regime.AuthorityHealth == nil {
		return stressSourceIssue{}, false
	}
	health := regime.AuthorityHealth
	reason := "regime last-good authority " + string(health.Status)
	if health.FailureCode != rpc.RegimeAuthorityFailureNone {
		reason += " (" + string(health.FailureCode) + ")"
	}
	switch health.Status {
	case rpc.RegimeAuthorityFresh:
		return stressSourceIssue{}, false
	case rpc.RegimeAuthorityStale:
		return stressSourceIssue{Source: "regime", Status: rpc.RegimeStatusStale, Reason: reason}, true
	case rpc.RegimeAuthorityUnavailable:
		return stressSourceIssue{Source: "regime", Status: rpc.RegimeStatusUnavailable, Reason: reason}, true
	default:
		return stressSourceIssue{Source: "regime", Status: rpc.RegimeStatusUnavailable, Reason: "regime authority status invalid"}, true
	}
}

func stressMarketEventSourceIssues(pos rpc.PositionsResult, events rpc.MarketEventsResult, now time.Time) []stressSourceIssue {
	// The daemon requests market-event context only for held underlyings. A
	// must never turn a missing snapshot into an implicit "no flags" answer.
	if len(stressMarketEventSymbols(pos)) == 0 {
		return nil
	}
	if !stressHasMarketEventsInput(events) {
		return []stressSourceIssue{{
			Source: "market_events",
			Status: rpc.SourceStatusUnknown,
			Reason: "market-event snapshot missing for held underlyings",
		}}
	}

	shortStock := stressHasShortStockExposure(pos)
	issues := []stressSourceIssue{}
	issueBySource := map[string]int{}
	seen := map[string]bool{}
	umbrellaSeen := false
	addIssue := func(source, status, reason string) {
		source = stressMarketEventSourceName(source)
		if source == "" {
			source = "market_events"
		}
		if stressMarketEventBorrowSource(source) && !shortStock {
			return
		}
		status = stressMarketEventHealthStatus(status)
		if existing, ok := issueBySource[source]; ok {
			if stressMarketEventHealthRank(status) > stressMarketEventHealthRank(issues[existing].Status) {
				issues[existing].Status = status
				issues[existing].Reason = reason
			}
			return
		}
		issueBySource[source] = len(issues)
		issues = append(issues, stressSourceIssue{Source: source, Status: status, Reason: reason})
	}
	// The result timestamp is part of the decision contract. Child rows that
	// say OK cannot make a never-dated or stale aggregate current.
	switch {
	case events.AsOf.IsZero():
		addIssue("market_events", rpc.SourceStatusUnknown, "market-event snapshot timestamp missing")
	case stressSourceStale(events.AsOf, now):
		addIssue("market_events", rpc.SourceStatusStale, "market-event snapshot stale")
	}

	for _, health := range events.SourceHealth {
		source := stressMarketEventSourceName(health.Source)
		if source == "" {
			source = strings.ToLower(strings.TrimSpace(health.Source))
		}
		if source == "" {
			source = "market_events"
		}
		if stressMarketEventBorrowSource(source) && !shortStock {
			continue
		}
		seen[source] = true
		if source == "market_events" {
			umbrellaSeen = true
		}
		status := stressMarketEventHealthStatus(health.Status)
		if status == rpc.SourceStatusOK {
			switch {
			case health.AsOf.IsZero():
				status = rpc.SourceStatusUnknown
			case stressMarketEventHealthStale(health, now):
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
		source := stressMarketEventSourceName(warning.Scope + " " + warning.Code)
		if source == "" {
			source = "market_events"
		}
		if stressMarketEventBorrowSource(source) && !shortStock {
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

	slices.SortStableFunc(issues, func(a, b stressSourceIssue) int {
		return strings.Compare(a.Source, b.Source)
	})
	return issues
}

func stressHasShortStockExposure(pos rpc.PositionsResult) bool {
	for _, stock := range pos.Stocks {
		if stressPositionIsStock(stock) && stock.Quantity < 0 {
			return true
		}
	}
	for _, group := range pos.ByUnderlying {
		if group.Stock != nil && stressPositionIsStock(*group.Stock) && group.Stock.Quantity < 0 {
			return true
		}
	}
	return false
}

func stressPositionIsStock(position rpc.PositionView) bool {
	secType := strings.ToUpper(strings.TrimSpace(position.SecType))
	// Empty is retained as a compatibility-safe legacy stock projection. Live
	// rows carry STOCK; explicit FUT, IND, or OPTION rows never make stock-borrow
	return secType == "" || secType == rpc.SecTypeStock || secType == "STK" || secType == "ETF"
}

func stressMarketEventBorrowSource(source string) bool {
	source = strings.ToLower(strings.TrimSpace(source))
	return strings.Contains(source, "borrow_inventory") || strings.Contains(source, "borrow_fee")
}

func stressMarketEventSourceName(source string) string {
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

func stressMarketEventHealthStatus(status string) string {
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

func stressMarketEventHealthRank(status string) int {
	switch stressMarketEventHealthStatus(status) {
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

func stressSourceStale(asOf, now time.Time) bool {
	return !asOf.IsZero() && stressSourceAgeSeconds(now, asOf) > stressSourceMaxAgeSeconds(now)
}

// stressMarketEventHealthStale honors the producer-authored per-source age
// contract. Daily official files must not be re-staled by Stress's generic
// fails closed. AgeSeconds is authoritative because some fallbacks age the
// takes precedence over wall-clock age, but never over an explicit non-OK
func stressMarketEventHealthStale(health rpc.SourceHealth, now time.Time) bool {
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
		return stressSourceStale(health.AsOf, now)
	}
	return health.AgeSeconds >= health.MaxAgeSeconds
}

func stressApplySourceBlocks(signals []risk.Signal, issues []stressSourceIssue) []risk.Signal {
	if len(issues) == 0 {
		return signals
	}
	accountBlocked := stressSourceIssuePresent(issues, "account")
	positionsBlocked := stressSourceIssuePresent(issues, "positions")
	for i := range signals {
		if accountBlocked && stressSignalDependsOnAccount(signals[i].ID) {
			stressBlockSignal(&signals[i], "account", "requires fresh account snapshot")
		}
		if positionsBlocked && stressSignalDependsOnPositions(signals[i].ID) {
			stressBlockSignal(&signals[i], "positions", "requires fresh positions snapshot")
		}
	}
	return signals
}

func stressSourceIssuePresent(issues []stressSourceIssue, source string) bool {
	for _, issue := range issues {
		if issue.Source == source {
			return true
		}
	}
	return false
}

func stressSignalDependsOnAccount(id risk.SignalID) bool {
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

func stressSignalDependsOnPositions(id risk.SignalID) bool {
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

func stressBlockSignal(sig *risk.Signal, source, impact string) {
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

func stressSourceDataQualitySignals(issues []stressSourceIssue) []risk.Signal {
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

// stressEstablishedSourceDataQualitySignals preserves the exact classified
// signal fields hashed by the established Stress compatibility projection.
func stressEstablishedSourceDataQualitySignals(issues []stressSourceIssue) []risk.Signal {
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

func stressPrimaryDrivers(signals []risk.Signal) []risk.SignalID {
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

func stressWarnings(m StressMarketSummary, r rpc.RegimeSnapshotResult, now time.Time) []string {
	var warnings []string
	detailWarnings, detailedClusters := stressRegimeWarningDetails(r.WarningDetails, stressMarketContextClusters(r, now))
	ambiguousClusters := stressClustersWithoutDetailedWarning(m.AmbiguousClusters, detailedClusters)
	partialClusters := stressClustersWithoutDetailedWarning(m.PartialClusters, detailedClusters)
	degradedClusters := stressClustersWithoutDetailedWarning(m.DegradedClusters, detailedClusters)
	staleClusters := stressClustersWithoutDetailedWarning(m.StaleClusters, detailedClusters)

	if len(ambiguousClusters) > 0 {
		warnings = append(warnings, "ambiguous clusters: "+stressClusterList(ambiguousClusters))
	}
	if len(partialClusters) > 0 {
		warnings = append(warnings, "partial clusters: "+stressClusterList(partialClusters))
	}
	if len(degradedClusters) > 0 {
		warnings = append(warnings, "degraded clusters: "+stressClusterList(degradedClusters))
	}
	if len(staleClusters) > 0 {
		warnings = append(warnings, "stale clusters: "+stressClusterList(staleClusters))
	}
	warnings = append(warnings, detailWarnings...)
	return warnings
}

func stressMarketEventWarnings(issues []stressSourceIssue) []string {
	warnings := []string{}
	for _, issue := range issues {
		if issue.Source == "account" || issue.Source == "positions" {
			continue
		}
		warnings = append(warnings, fmt.Sprintf("market-event source %s: %s", issue.Source, issue.Status))
	}
	return warnings
}

func stressRegimeWarningDetails(details []rpc.RegimeWarning, contextClusters map[string]bool) ([]string, map[string]bool) {
	lines := []string{}
	clusters := map[string]bool{}
	for _, w := range details {
		if stressRegimeWarningIsContext(w, contextClusters) {
			continue
		}
		line := stressWarningLine(w)
		if line == "" {
			continue
		}
		lines = append(lines, line)
		if cluster := stressRegimeWarningCluster(w); cluster != "" {
			clusters[cluster] = true
		}
	}
	return lines, clusters
}

func stressRegimeWarningIsContext(w rpc.RegimeWarning, contextClusters map[string]bool) bool {
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

func stressClustersWithoutDetailedWarning(clusters []string, detailed map[string]bool) []string {
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

func stressRegimeWarningCluster(w rpc.RegimeWarning) string {
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

// stressEstablishedSourceHealth preserves the exact ad5b77b source-health
// projection that participates in the established Stress fingerprint. New authority and
// required-source interpretations stay on the main Stress result only.
func stressEstablishedSourceHealth(in StressInput, now time.Time, accountFP, positionsFP, regimeFP, marketEventsFP rpc.Fingerprint, inputHealth string, m StressMarketSummary) []rpc.SourceHealth {
	out := []rpc.SourceHealth{
		stressAccountSourceHealth(in.Account, now, accountFP),
		stressTimedSourceHealth("positions", in.Positions.AsOf, now, positionsFP, stressPositionsSourceStatus(in.Positions, now), stressPositionsSourceConfidence(in.Positions)),
		stressEstablishedRegimeSourceHealth(in.Regime.AsOf, now, regimeFP, stressInputHealthConfidence(inputHealth), m),
	}
	if stressHasMarketEventsInput(in.MarketEvents) {
		out = append(out, stressEstablishedMarketEventsSourceHealth(in.Positions, in.MarketEvents, now, marketEventsFP))
	}
	return out
}

func stressEstablishedRegimeSourceHealth(asOf, now time.Time, fp rpc.Fingerprint, dataConfidence string, m StressMarketSummary) rpc.SourceHealth {
	status := rpc.RegimeStatusOK
	notes := []string{}
	if len(m.StaleClusters) > 0 {
		notes = append(notes, "stale clusters: "+strings.Join(m.StaleClusters, ","))
	}
	if len(m.DegradedClusters) > 0 {
		notes = append(notes, "degraded clusters: "+strings.Join(m.DegradedClusters, ","))
	}
	if len(m.PartialClusters) > 0 || len(m.AmbiguousClusters) > 0 {
		notes = append(notes, stressAmbiguityEvidence(m))
	}
	switch {
	case len(m.PartialClusters) > 0 || len(m.AmbiguousClusters) > 0:
		status = "partial"
	case len(m.DegradedClusters) > 0:
		status = "degraded"
	case len(m.StaleClusters) > 0:
		status = rpc.RegimeStatusStale
	}
	health := stressTimedSourceHealth("regime", asOf, now, fp, status, dataConfidence)
	health.Notes = notes
	return health
}

func stressEstablishedMarketEventsSourceHealth(pos rpc.PositionsResult, events rpc.MarketEventsResult, now time.Time, fp rpc.Fingerprint) rpc.SourceHealth {
	events = stressRelevantMarketEvents(pos, events)
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
	health := stressTimedSourceHealth("market_events", events.AsOf, now, fp, status, confidence)
	health.Notes = notes
	return health
}

func stressSourceHealth(in StressInput, now time.Time, accountFP, positionsFP, regimeFP, marketEventsFP rpc.Fingerprint, inputHealth string, m StressMarketSummary) []rpc.SourceHealth {
	out := []rpc.SourceHealth{
		stressAccountSourceHealth(in.Account, now, accountFP),
		stressTimedSourceHealth("positions", in.Positions.AsOf, now, positionsFP, stressPositionsSourceStatus(in.Positions, now), stressPositionsSourceConfidence(in.Positions)),
		stressRegimeSourceHealth(in.Regime, now, regimeFP, stressInputHealthConfidence(inputHealth), m),
	}
	if stressHasMarketEventsInput(in.MarketEvents) || len(stressMarketEventSymbols(in.Positions)) > 0 {
		out = append(out, stressMarketEventsSourceHealth(in.Positions, in.MarketEvents, now, marketEventsFP))
	}
	return out
}

func stressHasMarketEventsInput(events rpc.MarketEventsResult) bool {
	return events.Kind != "" ||
		!events.AsOf.IsZero() ||
		len(events.Symbols) > 0 ||
		len(events.Flags) > 0 ||
		len(events.BorrowFeeCoverage) > 0 ||
		len(events.SourceHealth) > 0 ||
		len(events.WarningDetails) > 0
}

func stressMarketEventsSourceHealth(pos rpc.PositionsResult, events rpc.MarketEventsResult, now time.Time, fp rpc.Fingerprint) rpc.SourceHealth {
	status := rpc.SourceStatusOK
	confidence := "medium"
	notes := []string{}
	if len(events.Flags) > 0 {
		notes = append(notes, fmt.Sprintf("%d active/recent market-event flags", len(events.Flags)))
	}
	issues := stressMarketEventSourceIssues(pos, events, now)
	for _, issue := range issues {
		if stressMarketEventHealthRank(issue.Status) > stressMarketEventHealthRank(status) {
			status = issue.Status
		}
		notes = append(notes, issue.Source+" "+issue.Status)
	}
	if len(issues) > 0 {
		confidence = "medium-low"
	}
	health := stressTimedSourceHealth("market_events", events.AsOf, now, fp, status, confidence)
	health.Notes = notes
	return health
}

func stressInputHealthConfidence(inputHealth string) string {
	switch inputHealth {
	case stressInputOK:
		return "high"
	case stressInputFailed:
		return "low"
	default:
		return "medium-low"
	}
}

func stressTimedSourceHealth(source string, asOf, now time.Time, fp rpc.Fingerprint, status, confidence string) rpc.SourceHealth {
	maxAge := stressSourceMaxAgeSeconds(now)
	age := stressSourceAgeSeconds(now, asOf)
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

func stressRegimeSourceHealth(regime rpc.RegimeSnapshotResult, now time.Time, fp rpc.Fingerprint, dataConfidence string, m StressMarketSummary) rpc.SourceHealth {
	status := rpc.RegimeStatusOK
	notes := []string{}
	if len(m.StaleClusters) > 0 {
		notes = append(notes, "stale clusters: "+strings.Join(m.StaleClusters, ","))
	}
	if len(m.DegradedClusters) > 0 {
		notes = append(notes, "degraded clusters: "+strings.Join(m.DegradedClusters, ","))
	}
	if len(m.PartialClusters) > 0 || len(m.AmbiguousClusters) > 0 {
		notes = append(notes, stressAmbiguityEvidence(m))
	}
	switch {
	case len(m.PartialClusters) > 0 || len(m.AmbiguousClusters) > 0:
		status = "partial"
	case len(m.DegradedClusters) > 0:
		status = "degraded"
	case len(m.StaleClusters) > 0:
		status = rpc.RegimeStatusStale
	}
	if issue, ok := stressRegimeAuthorityIssue(regime); ok {
		if stressMarketEventHealthRank(issue.Status) > stressMarketEventHealthRank(status) {
			status = issue.Status
		}
		notes = append(notes, issue.Reason)
	}
	health := stressTimedSourceHealth("regime", regime.AsOf, now, fp, status, dataConfidence)
	health.Notes = notes
	return health
}

func stressAccountSourceHealth(acct rpc.AccountResult, now time.Time, fp rpc.Fingerprint) rpc.SourceHealth {
	health := stressTimedSourceHealth("account", acct.AsOf, now, fp, stressAccountSourceStatus(acct, now), stressAccountSourceConfidence(acct, now))
	failed, notDue := stressAccountDailyPnLState(acct, now)
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

func stressAccountSourceStatus(acct rpc.AccountResult, now time.Time) string {
	if acct.NetLiquidation <= 0 {
		return "partial"
	}
	if acct.AsOf.IsZero() {
		return "partial"
	}
	if failed, _ := stressAccountDailyPnLState(acct, now); failed {
		if acct.DailyPnLObservation == nil {
			return "partial"
		}
		return rpc.SourceStatusDegraded
	}
	if stressSourceAgeSeconds(now, acct.AsOf) > stressSourceMaxAgeSeconds(now) {
		return rpc.RegimeStatusStale
	}
	return rpc.RegimeStatusOK
}

func stressAccountSourceConfidence(acct rpc.AccountResult, now time.Time) string {
	failed, _ := stressAccountDailyPnLState(acct, now)
	if acct.NetLiquidation <= 0 || acct.AsOf.IsZero() || failed {
		return "medium-low"
	}
	return "high"
}

func stressAccountDailyPnLFailed(acct rpc.AccountResult, now time.Time) bool {
	failed, _ := stressAccountDailyPnLState(acct, now)
	return failed
}

func stressAccountDailyPnLState(acct rpc.AccountResult, now time.Time) (failed, notDue bool) {
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
		if stressDailyPnLDue(now) {
			return true, false
		}
		return false, true
	}
	return false, false
}

// stressDailyPnLDue follows the same official US-equity session authority as
// the daemon's subscription repair. Calendar failures and dates outside the
// embedded coverage fail closed: a missing P&L remains required.
func stressDailyPnLDue(now time.Time) bool {
	session, err := marketcal.New().SessionAt(marketcal.MarketUSEquity, now)
	if err != nil || session.State == marketcal.StateUnknown {
		return true
	}
	return session.IsOpen
}

func stressPositionsSourceStatus(pos rpc.PositionsResult, now time.Time) string {
	if pos.AsOf.IsZero() {
		return "partial"
	}
	if stressSourceAgeSeconds(now, pos.AsOf) > stressSourceMaxAgeSeconds(now) {
		return rpc.RegimeStatusStale
	}
	return rpc.RegimeStatusOK
}

func stressPositionsSourceConfidence(pos rpc.PositionsResult) string {
	if pos.AsOf.IsZero() {
		return "medium-low"
	}
	return "high"
}

func stressSourceAgeSeconds(now, asOf time.Time) int64 {
	if now.IsZero() || asOf.IsZero() {
		return 0
	}
	age := now.Sub(asOf)
	if age < 0 {
		return 0
	}
	return int64(age.Seconds())
}

func stressSourceMaxAgeSeconds(now time.Time) int64 {
	switch rpc.ClassifySession(now) {
	case rpc.SessionPre, rpc.SessionRTH:
		return int64((10 * time.Minute).Seconds())
	default:
		return int64((90 * time.Minute).Seconds())
	}
}

func stressWarningLine(w rpc.RegimeWarning) string {
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

// stressMarketEvidence renders only the cluster facts that carry information:
// zero-count buckets never render, and the reporting fraction appears only
// while coverage is incomplete.
func stressMarketEvidence(m StressMarketSummary) string {
	parts := []string{}
	if m.RedClusters > 0 {
		parts = append(parts, fmt.Sprintf("%d red (%s)", m.RedClusters, stressClusterList(m.RedClusterNames)))
	}
	if m.YellowClusters > 0 {
		parts = append(parts, fmt.Sprintf("%d yellow (%s)", m.YellowClusters, stressClusterList(m.YellowClusterNames)))
	}
	if len(parts) == 0 {
		parts = append(parts, "no stressed clusters")
	}
	if len(m.UnconfirmedRedClusterNames) > 0 {
		parts = append(parts, "red but unconfirmed: "+stressClusterList(m.UnconfirmedRedClusterNames))
	}
	total := m.RankedClusters + m.UnrankedClusters
	if m.RankedClusters < total {
		parts = append(parts, fmt.Sprintf("%d of %d clusters reporting", m.RankedClusters, total))
	}
	return strings.Join(parts, "; ")
}

// Trigger levels render next to the observed numbers so a reading can be
// judged as near-miss or comfortable without opening the policy.
func stressTapeEvidence(m StressMarketSummary) string {
	parts := []string{}
	if m.SPYChangePct != nil {
		parts = append(parts, fmt.Sprintf("SPY %+.2f%% (drop trigger %.1f%%)", *m.SPYChangePct, stressPolicy.SPYDropPct))
	} else {
		parts = append(parts, "SPY change unavailable")
	}
	if m.VIXChangePct != nil {
		parts = append(parts, fmt.Sprintf("VIX %+.2f%% (spike trigger %+.0f%%)", *m.VIXChangePct, stressPolicy.VIXSpikePct))
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

// stressTapeSession delegates to the shared rpc.TapeSessionFor policy copy —
func stressTapeSession(now time.Time) (state, reason string, nextOpen *time.Time) {
	return rpc.TapeSessionFor(now)
}

// stressTapeConfirmable reports whether direct SPY/VIX day-change prints may
// carry severity or confirm stress right now.
func stressTapeConfirmable(m StressMarketSummary) bool {
	return m.TapeSessionState != rpc.TapeSessionClosedDate
}

func stressTapeDemotedGuidance(m StressMarketSummary) string {
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

func confirmedTapeStress(m StressMarketSummary) bool {
	if !stressTapeConfirmable(m) {
		// Frozen prints cannot confirm; only the cluster-side carry-unwind
		return stressFastCarryUnwind(m)
	}
	spyDrop := pctAtMost(m.SPYChangePct, stressPolicy.SPYDropPct)
	spyHardDrop := pctAtMost(m.SPYChangePct, stressPolicy.SPYHardDropPct)
	vixSpike := pctAtLeast(m.VIXChangePct, stressPolicy.VIXSpikePct)
	vixHardSpike := pctAtLeast(m.VIXChangePct, stressPolicy.VIXHardSpikePct)
	return (spyHardDrop && (vixSpike || m.EligibleRedClusters >= 1)) ||
		(vixHardSpike && (spyDrop || m.EligibleRedClusters >= 1)) ||
		(spyDrop && vixSpike && m.EligibleRedClusters >= 1) ||
		stressFastCarryUnwind(m)
}

func stressFastCarryUnwind(m StressMarketSummary) bool {
	fxRed := slices.Contains(m.RedClusterNames, "fx") || slices.Contains(m.UnconfirmedRedClusterNames, "fx")
	if !fxRed {
		return false
	}
	tapeConfirms := stressTapeConfirmable(m) &&
		(pctAtMost(m.SPYChangePct, stressPolicy.SPYDropPct) ||
			pctAtLeast(m.VIXChangePct, stressPolicy.VIXSpikePct))
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

func stressAmbiguityEvidence(m StressMarketSummary) string {
	parts := []string{stressMarketEvidence(m)}
	if len(m.StaleClusters) > 0 {
		parts = append(parts, "stale "+stressClusterList(m.StaleClusters))
	}
	if len(m.AmbiguousClusters) > 0 {
		parts = append(parts, "ambiguous "+stressClusterList(m.AmbiguousClusters))
	}
	if len(m.PartialClusters) > 0 {
		parts = append(parts, "partial "+stressClusterList(m.PartialClusters))
	}
	if len(m.DegradedClusters) > 0 {
		parts = append(parts, "degraded "+stressClusterList(m.DegradedClusters))
	}
	if len(m.ComputingClusters) > 0 {
		parts = append(parts, "computing "+stressClusterList(m.ComputingClusters))
	}
	return strings.Join(parts, "; ")
}

func stressPortfolioEvidence(p StressPortfolioSummary) string {
	out := fmt.Sprintf("%s, gross %.0f%% NLV, net delta %.0f%% NLV, gross delta %.0f%% NLV",
		stressCushionEvidence(p), derefPct(p.GrossExposurePctNLV), derefPct(p.NetDeltaPctNLV), derefPct(p.GrossDeltaPctNLV))
	if p.ProtectionCoverage != nil {
		out += ", protection " + formatProtectionCoverageEvidence(p.ProtectionCoverage)
	}
	if len(p.HeldStress) > 0 {
		out += ", held stress " + heldStressNames(p.HeldStress, 2)
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

// FormatProtectionCoverageEvidence formats the current protection coverage
func FormatProtectionCoverageEvidence(c *rpc.ProtectionCoverageSummary) string {
	return formatProtectionCoverageEvidence(c)
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

// stressClusterStaleNotDue reports whether every stale row in a cluster
// this only keeps a closed publication window from paging the desk as a data
func stressClusterStaleNotDue(statuses []string, meta []rpc.RegimeIndicatorMeta) bool {
	if len(statuses) != len(meta) {
		return false
	}
	sawStale := false
	for i, status := range statuses {
		if status != rpc.RegimeStatusStale {
			continue
		}
		sawStale = true
		if meta[i].Freshness == nil || meta[i].Freshness.Class != rpc.RegimeFreshnessNotDue {
			return false
		}
	}
	return sawStale
}

func stressHasMarketDataIssue(m StressMarketSummary) bool {
	return len(m.AmbiguousClusters) > 0 ||
		len(m.PartialClusters) > 0 ||
		len(m.DegradedClusters) > 0 ||
		len(m.ComputingClusters) > 0 ||
		len(m.StaleClusters) > 0
}

func stressWorstCushionPct(p StressPortfolioSummary) *float64 {
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
func stressCushionEvidence(p StressPortfolioSummary) string {
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
	return strings.Join(parts, "; ") + fmt.Sprintf(" (watch below %.0f%%)", stressPolicy.MarginWatchPct)
}

func stressConcentrationEvidence(p StressPortfolioSummary) string {
	parts := []string{}
	if p.LargestExposurePct != nil && p.LargestExposure != "" {
		parts = append(parts, fmt.Sprintf("%s market %.0f%% NLV (watch %.0f%%)", p.LargestExposure, math.Abs(*p.LargestExposurePct), stressPolicy.SingleNameExposureWatchPct))
	}
	if p.LargestDeltaPctNLV != nil && p.LargestDeltaExposure != "" {
		parts = append(parts, fmt.Sprintf("%s delta %.0f%% NLV (watch %.0f%%)", p.LargestDeltaExposure, *p.LargestDeltaPctNLV, stressPolicy.SingleNameDeltaWatchPct))
	}
	return strings.Join(parts, "; ")
}

func heldStressEvidence(stresses []rpc.HeldStress) string {
	if len(stresses) == 0 {
		return "no material held-name stress"
	}
	parts := []string{}
	for _, stress := range stresses {
		items := []string{}
		if stress.DailyPnLPctNLV != nil && *stress.DailyPnLPctNLV <= -stressPolicy.HeldUnderlyingPnLWatchPct {
			items = append(items, fmt.Sprintf("daily P&L %+.1f%% NLV", *stress.DailyPnLPctNLV))
		}
		if stress.NearExpiryDeltaPctNLV != nil && *stress.NearExpiryDeltaPctNLV >= stressPolicy.HeldOptionDeltaWatchPct {
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

func heldStressNames(stresses []rpc.HeldStress, limit int) string {
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
		clean = append(clean, stressClusterDisplayName(part))
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

func stressClusterList(clusters []string) string {
	return humanList(strings.Join(clusters, ","))
}

func stressClusterDisplayName(cluster string) string {
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
