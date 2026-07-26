package cli

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

const (
	opportunityHistoricalBarDataType      = "historical_adjusted_bar"
	opportunityHistoricalBarQuoteQuality  = "bar_close"
	opportunityHistoricalPanelMethod      = "bars_panel_features_v1"
	opportunityHistoricalPanelSplitMethod = "bars_panel_retrospective_date_split_v1"
)

type opportunityHistoricalPanelOptions struct {
	Symbols          []string
	Benchmark        string
	StartDate        string
	EndDate          string
	HoldoutStartDate string
	HoldoutPlan      string
	SampleStepBars   int
	HorizonDays      int
	RoundTripCostBps float64
	CostModel        string
	MarketCluster    string
	Theme            string
}

func buildOpportunityPointInTimeRowsFromBars(ledger opportunityPriceBarLedger, opts opportunityHistoricalPanelOptions) ([]OpportunityPointInTimeRow, error) {
	if len(ledger.BySymbol) == 0 {
		return nil, fmt.Errorf("bars ledger is empty")
	}
	if ledger.SourceQuality != "ok" {
		return nil, fmt.Errorf("bars source quality %q is not alpha-grade; use manifest-backed adjusted bars", ledger.SourceQuality)
	}
	benchmark := strings.ToUpper(strings.TrimSpace(opts.Benchmark))
	if benchmark == "" {
		benchmark = "QQQ"
	}
	benchmarkRows := ledger.BySymbol[benchmark]
	if len(benchmarkRows) == 0 {
		return nil, fmt.Errorf("benchmark %s bars are required", benchmark)
	}
	symbols := opportunityPanelSymbols(ledger.BySymbol, opts.Symbols, benchmark)
	if len(symbols) == 0 {
		return nil, fmt.Errorf("no non-benchmark symbols in bars ledger")
	}
	step := opts.SampleStepBars
	if step <= 0 {
		step = 21
	}
	horizon := opts.HorizonDays
	if horizon <= 0 {
		horizon = 126
	}
	costBps := opts.RoundTripCostBps
	if costBps < 0 {
		costBps = 0
	}
	costModel := strings.TrimSpace(opts.CostModel)
	if costModel == "" {
		costModel = fmt.Sprintf("flat-%.0fbps-bars-panel", costBps)
	}
	start, err := optionalOpportunityPanelDate(opts.StartDate)
	if err != nil {
		return nil, fmt.Errorf("--start-date: %w", err)
	}
	end, err := optionalOpportunityPanelDate(opts.EndDate)
	if err != nil {
		return nil, fmt.Errorf("--end-date: %w", err)
	}
	if !start.IsZero() && !end.IsZero() && end.Before(start) {
		return nil, fmt.Errorf("--end-date must be on or after --start-date")
	}
	holdoutStart, err := optionalOpportunityPanelDate(opts.HoldoutStartDate)
	if err != nil {
		return nil, fmt.Errorf("--holdout-start-date: %w", err)
	}
	holdoutPlan := strings.TrimSpace(opts.HoldoutPlan)
	if !holdoutStart.IsZero() && holdoutPlan == "" {
		return nil, fmt.Errorf("--holdout-plan is required with --holdout-start-date")
	}
	if holdoutStart.IsZero() && holdoutPlan != "" {
		return nil, fmt.Errorf("--holdout-plan requires --holdout-start-date")
	}
	if !holdoutStart.IsZero() && !start.IsZero() && holdoutStart.Before(start) {
		return nil, fmt.Errorf("--holdout-start-date must be on or after --start-date")
	}
	if !holdoutStart.IsZero() && !end.IsZero() && holdoutStart.After(end) {
		return nil, fmt.Errorf("--holdout-start-date must be on or before --end-date")
	}
	cluster := strings.TrimSpace(opts.MarketCluster)
	if cluster == "" {
		cluster = "historical_bars_panel"
	}
	theme := strings.TrimSpace(opts.Theme)
	if theme == "" {
		theme = "historical_opportunity_replay"
	}

	var out []OpportunityPointInTimeRow
	for _, symbol := range symbols {
		rows := slices.Clone(ledger.BySymbol[symbol])
		sort.Slice(rows, func(i, j int) bool { return rows[i].Date < rows[j].Date })
		for i := 0; i < len(rows); i += step {
			rowDate, err := parseOpportunityDate(rows[i].Date)
			if err != nil {
				return nil, fmt.Errorf("%s %s: %w", symbol, rows[i].Date, err)
			}
			if !start.IsZero() && rowDate.Before(start) {
				continue
			}
			if !end.IsZero() && rowDate.After(end) {
				continue
			}
			if !opportunityPanelHasObservableOutcome(rows, benchmarkRows, rows[i].Date, horizon) {
				continue
			}
			features, ok := opportunityPanelFeatures(symbol, rows[:i+1], benchmarkRows, rows[i].Date)
			if !ok {
				continue
			}
			split, splitProvenance := opportunityHistoricalPanelSplit(rowDate, holdoutStart, holdoutPlan)
			costBpsCopy := costBps
			pit := OpportunityPointInTimeRow{
				Date:              rows[i].Date,
				Case:              fmt.Sprintf("historical bars panel %s %s", symbol, rows[i].Date),
				Split:             split,
				SplitProvenance:   splitProvenance,
				FeatureProvenance: opportunityFeatureProvenance(opportunityNonEmptyString(ledger.ManifestChecksum, ledger.Checksum), opportunityHistoricalPanelMethod, features),
				LabelStatus:       "unscored_forward_window_pending",
				MarketCluster:     cluster,
				Theme:             theme,
				Features:          features,
				Trade: OpportunityBacktestTrade{
					Instrument:       symbol,
					EntryRule:        "next_close",
					HorizonDays:      horizon,
					Benchmark:        benchmark,
					RoundTripCostBps: &costBpsCopy,
					CostModel:        costModel,
				},
				Notes: fmt.Sprintf("historical point-in-time row built from manifest-backed adjusted bars; sample_step_bars=%d", step),
			}
			out = append(out, pit)
		}
	}
	return out, nil
}

func opportunityHistoricalPanelSplit(rowDate, holdoutStart time.Time, holdoutPlan string) (string, OpportunitySplitProvenance) {
	if holdoutStart.IsZero() || rowDate.Before(holdoutStart) {
		return "tuning", OpportunitySplitProvenance{}
	}
	return "holdout", OpportunitySplitProvenance{
		Source:                  "build-opportunity-pit",
		Method:                  opportunityHistoricalPanelSplitMethod,
		PlanID:                  strings.TrimSpace(holdoutPlan),
		AssignedAt:              holdoutStart.UTC(),
		LabelStatusAtAssignment: "unscored_forward_window_pending",
		PreRegistered:           false,
	}
}

func opportunityPanelSymbols(bars map[string][]OpportunityPriceBarRow, requested []string, benchmark string) []string {
	benchmark = strings.ToUpper(strings.TrimSpace(benchmark))
	seen := map[string]struct{}{}
	var symbols []string
	add := func(symbol string) {
		symbol = strings.ToUpper(strings.TrimSpace(symbol))
		if symbol == "" || symbol == benchmark {
			return
		}
		if _, ok := bars[symbol]; !ok {
			return
		}
		if _, ok := seen[symbol]; ok {
			return
		}
		seen[symbol] = struct{}{}
		symbols = append(symbols, symbol)
	}
	if len(requested) > 0 {
		for _, symbol := range requested {
			add(symbol)
		}
	} else {
		for symbol := range bars {
			add(symbol)
		}
	}
	slices.Sort(symbols)
	return symbols
}

func optionalOpportunityPanelDate(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}
	return parseOpportunityDate(raw)
}

func opportunityPanelHasObservableOutcome(rows, benchmarkRows []OpportunityPriceBarRow, decisionDate string, horizonDays int) bool {
	entry, ok := firstOpportunityBarAfter(rows, decisionDate)
	if !ok {
		return false
	}
	exitDate, err := opportunityScoreExitDate(entry.Date, horizonDays)
	if err != nil {
		return false
	}
	exit, ok := firstOpportunityBarOnOrAfter(rows, exitDate)
	if !ok {
		return false
	}
	_, _, err = opportunityScoreBenchmarkWindow(entry.Date, exit.Date, benchmarkRows)
	return err == nil
}

func opportunityPanelFeatures(symbol string, rows, benchmarkRows []OpportunityPriceBarRow, date string) (OpportunityPointInTimeFeatures, bool) {
	const minBars = 200
	if len(rows) < minBars {
		return OpportunityPointInTimeFeatures{}, false
	}
	current := rows[len(rows)-1]
	price := opportunityBarClose(current)
	if price <= 0 {
		return OpportunityPointInTimeFeatures{}, false
	}
	benchmarkThroughDate := opportunityPanelBarsThrough(benchmarkRows, date)
	if len(benchmarkThroughDate) < 127 {
		return OpportunityPointInTimeFeatures{}, false
	}
	sma50 := opportunityPanelSMA(rows, 50)
	sma200 := opportunityPanelSMA(rows, 200)
	ret63 := opportunityPanelReturn(rows, 63)
	ret126 := opportunityPanelReturn(rows, 126)
	bench63 := opportunityPanelReturn(benchmarkThroughDate, 63)
	bench126 := opportunityPanelReturn(benchmarkThroughDate, 126)
	_, avgDollar := opportunityPanelLiquidity20D(rows)
	if sma50 == nil || sma200 == nil || ret63 == nil || ret126 == nil || bench63 == nil || bench126 == nil || avgDollar == nil {
		return OpportunityPointInTimeFeatures{}, false
	}
	pct50 := (price - *sma50) / *sma50
	pct200 := (price - *sma200) / *sma200
	rs63 := *ret63 - *bench63
	rs126 := *ret126 - *bench126
	changePct := opportunityPanelDailyChangePct(rows)
	features := OpportunityPointInTimeFeatures{
		Instrument:         strings.ToUpper(strings.TrimSpace(symbol)),
		SecType:            "STK",
		DataType:           opportunityHistoricalBarDataType,
		QuoteQuality:       opportunityHistoricalBarQuoteQuality,
		SessionContext:     opportunityPanelSession(date),
		PriceAsOf:          date,
		DataQuality:        "ok",
		TrendState:         opportunityPanelTrendState(price, pct200, *sma50, *sma200),
		Price:              &price,
		SMA50:              sma50,
		SMA200:             sma200,
		PctAbove50DMA:      &pct50,
		PctAbove200DMA:     &pct200,
		RS63D:              &rs63,
		RS126D:             &rs126,
		AvgDollarVolume20D: avgDollar,
		Volume:             &current.Volume,
		ChangePct:          changePct,
		EventGapPct:        changePct,
		ExtendedChaseRisk:  changePct != nil && *changePct > opportunityBuilderMaxGapPct,
	}
	return features, true
}

func opportunityPanelBarsThrough(rows []OpportunityPriceBarRow, date string) []OpportunityPriceBarRow {
	idx := sort.Search(len(rows), func(i int) bool { return rows[i].Date > date })
	return rows[:idx]
}

func opportunityPanelSMA(rows []OpportunityPriceBarRow, n int) *float64 {
	if n <= 0 || len(rows) < n {
		return nil
	}
	var sum float64
	for _, row := range rows[len(rows)-n:] {
		close := opportunityBarClose(row)
		if close <= 0 {
			return nil
		}
		sum += close
	}
	v := sum / float64(n)
	return &v
}

func opportunityPanelReturn(rows []OpportunityPriceBarRow, n int) *float64 {
	if n <= 0 || len(rows) <= n {
		return nil
	}
	last := opportunityBarClose(rows[len(rows)-1])
	prev := opportunityBarClose(rows[len(rows)-1-n])
	if last <= 0 || prev <= 0 {
		return nil
	}
	v := last/prev - 1
	return &v
}

func opportunityPanelLiquidity20D(rows []OpportunityPriceBarRow) (*int64, *float64) {
	const n = 20
	if len(rows) == 0 {
		return nil, nil
	}
	start := max(len(rows)-n, 0)
	var volumeSum int64
	var dollarSum float64
	var count int
	for _, row := range rows[start:] {
		close := opportunityBarClose(row)
		if close <= 0 || row.Volume <= 0 {
			continue
		}
		volumeSum += row.Volume
		dollarSum += close * float64(row.Volume)
		count++
	}
	if count == 0 {
		return nil, nil
	}
	avgVolume := volumeSum / int64(count)
	avgDollar := dollarSum / float64(count)
	return &avgVolume, &avgDollar
}

func opportunityPanelDailyChangePct(rows []OpportunityPriceBarRow) *float64 {
	if len(rows) < 2 {
		return nil
	}
	last := opportunityBarClose(rows[len(rows)-1])
	prev := opportunityBarClose(rows[len(rows)-2])
	if last <= 0 || prev <= 0 {
		return nil
	}
	v := (last/prev - 1) * 100
	return &v
}

func opportunityPanelTrendState(price, pct200, sma50, sma200 float64) string {
	if price < sma200 {
		return "broken"
	}
	if pct200 > 0.35 {
		return "extended"
	}
	if price > sma50 && sma50 > sma200 {
		return "uptrend"
	}
	return "recovering"
}

func opportunityPanelSession(date string) *rpc.MarketSession {
	return &rpc.MarketSession{
		Market:   "us_equity",
		Date:     date,
		Timezone: "America/New_York",
		State:    "regular",
		IsOpen:   true,
	}
}
