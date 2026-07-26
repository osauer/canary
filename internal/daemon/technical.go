package daemon

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"

	"github.com/osauer/canary/v2/internal/rpc"
)

const (
	technicalDefaultLookbackDays = 420
	technicalMaxLookbackDays     = 800
	technicalMaxSymbols          = 50
	technicalWorkers             = 4
	technicalPerSymbolTimeout    = 15 * time.Second
)

func (s *Server) handleTechnical(ctx context.Context, req *rpc.Request) (*rpc.TechnicalResult, error) {
	var p rpc.TechnicalParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	symbols := normalizeTechnicalSymbols(p.Symbols)
	if len(symbols) == 0 {
		return nil, errBadRequest("symbols required")
	}
	if len(symbols) > technicalMaxSymbols {
		return nil, errBadRequest(fmt.Sprintf("symbols capped at %d", technicalMaxSymbols))
	}
	benchmark := normSym(p.Benchmark)
	if benchmark == "" {
		benchmark = "SPY"
	}
	days := p.LookbackDays
	if days <= 0 {
		days = technicalDefaultLookbackDays
	}
	if days > technicalMaxLookbackDays {
		days = technicalMaxLookbackDays
	}
	c := s.gatewayConnector()
	if c == nil {
		return nil, s.gatewayUnavailableError()
	}
	route, routed, err := technicalRoute(p)
	if err != nil {
		return nil, err
	}

	res := &rpc.TechnicalResult{
		Benchmark:    benchmark,
		LookbackDays: days,
		Market:       route.Market,
		Exchange:     route.Exchange,
		PrimaryExch:  route.PrimaryExch,
		Currency:     route.Currency,
		Rows:         make([]rpc.TechnicalRow, len(symbols)),
		AsOf:         time.Now(),
	}
	benchCtx, cancelBench := context.WithTimeout(ctx, technicalPerSymbolTimeout)
	benchBars, benchErr := c.FetchHistoricalDailyBars(benchCtx, benchmark, days, 0)
	cancelBench()
	var benchRow rpc.TechnicalRow
	var bench63, bench126 *float64
	if benchErr != nil {
		res.WarningDetails = append(res.WarningDetails, rpc.DataWarning{
			Code:     "technical_benchmark_unavailable",
			Scope:    benchmark,
			Severity: "data_quality",
			Message:  fmt.Sprintf("Benchmark history for %s was unavailable within the technical-screen budget.", benchmark),
			Impact:   "Rows can still report trend and liquidity fields, but relative-strength fields are omitted.",
			Action:   "Retry when the gateway is responsive, use a warm contract cache, or run smaller batches.",
		})
	} else {
		benchRow = buildTechnicalRow(benchmark, benchBars, nil, nil, res.AsOf)
		bench63 = benchRow.Return63D
		bench126 = benchRow.Return126D
	}
	type job struct {
		idx int
		sym string
	}
	jobs := make([]job, 0, len(symbols))
	for i, sym := range symbols {
		if sym == benchmark {
			if benchErr != nil {
				res.Rows[i] = rpc.TechnicalRow{
					Symbol:         sym,
					DataQuality:    "error",
					MissingReasons: []string{"history_unavailable"},
					Error:          benchErr.Error(),
					AsOf:           time.Now(),
				}
			} else {
				res.Rows[i] = buildTechnicalRow(sym, benchBars, bench63, bench126, res.AsOf)
			}
			continue
		}
		jobs = append(jobs, job{idx: i, sym: sym})
	}
	runBounded(jobs, technicalWorkers, func(j job) {
		if ctx.Err() != nil {
			return
		}
		fetchCtx, cancel := context.WithTimeout(ctx, technicalPerSymbolTimeout)
		defer cancel()
		bars, err := fetchTechnicalBars(fetchCtx, c, route, routed, j.sym, days)
		if err != nil {
			res.Rows[j.idx] = rpc.TechnicalRow{
				Symbol:         j.sym,
				DataQuality:    "error",
				MissingReasons: []string{"history_unavailable"},
				Error:          err.Error(),
				AsOf:           time.Now(),
			}
			return
		}
		res.Rows[j.idx] = buildTechnicalRow(j.sym, bars, bench63, bench126, res.AsOf)
	})
	if err := ctx.Err(); err != nil {
		res.WarningDetails = append(res.WarningDetails, rpc.DataWarning{
			Code:     "technical_partial_timeout",
			Severity: "data_quality",
			Message:  "Technical screen hit the handler deadline and returned partial rows.",
			Impact:   "Rows marked error or timeout should be excluded from ranking.",
			Action:   "Retry in smaller batches or pass explicit market/exchange routing for symbols that need it.",
		})
		for i, row := range res.Rows {
			if row.Symbol != "" {
				continue
			}
			res.Rows[i] = rpc.TechnicalRow{
				Symbol:         symbols[i],
				DataQuality:    "error",
				MissingReasons: []string{"history_unavailable"},
				Error:          err.Error(),
				AsOf:           time.Now(),
			}
		}
	}
	return res, nil
}

func technicalRoute(p rpc.TechnicalParams) (rpc.ContractParams, bool, error) {
	route := rpc.ContractParams{
		Market:       strings.TrimSpace(p.Market),
		Exchange:     strings.ToUpper(strings.TrimSpace(p.Exchange)),
		PrimaryExch:  strings.ToUpper(strings.TrimSpace(p.PrimaryExch)),
		Currency:     strings.ToUpper(strings.TrimSpace(p.Currency)),
		LocalSymbol:  strings.TrimSpace(p.LocalSymbol),
		TradingClass: strings.TrimSpace(p.TradingClass),
	}
	routed := route.Market != "" ||
		route.Exchange != "" ||
		route.PrimaryExch != "" ||
		route.Currency != "" ||
		route.LocalSymbol != "" ||
		route.TradingClass != ""
	if !routed {
		return rpc.ContractParams{}, false, nil
	}
	probe := route
	probe.Symbol = "AAPL"
	if _, echo, _, err := normaliseStockQuoteContract(probe); err != nil {
		return rpc.ContractParams{}, false, err
	} else {
		echo.Symbol = ""
		route = echo
	}
	return route, true, nil
}

func fetchTechnicalBars(ctx context.Context, c *ibkrlib.Connector, route rpc.ContractParams, routed bool, symbol string, days int) ([]ibkrlib.HistoricalBar, error) {
	if !routed {
		return c.FetchHistoricalDailyBars(ctx, symbol, days, 0)
	}
	params := route
	params.Symbol = symbol
	contract, _, _, err := normaliseStockQuoteContract(params)
	if err != nil {
		return nil, err
	}
	return c.FetchHistoricalDailyBarsWithContract(ctx, contract, days, 0)
}

func normalizeTechnicalSymbols(raw []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, token := range raw {
		for part := range strings.SplitSeq(token, ",") {
			sym := normSym(part)
			if sym == "" || seen[sym] {
				continue
			}
			seen[sym] = true
			out = append(out, sym)
		}
	}
	return out
}

func buildTechnicalRow(symbol string, bars []ibkrlib.HistoricalBar, bench63, bench126 *float64, asOf time.Time) rpc.TechnicalRow {
	row := rpc.TechnicalRow{
		Symbol: symbol,
		Bars:   len(bars),
		AsOf:   asOf,
	}
	latest, ok := latestTechnicalBar(bars)
	if !ok {
		row.TrendState = "insufficient_data"
		row.DataQuality = "insufficient_data"
		row.MissingReasons = []string{"price_unavailable", "insufficient_history"}
		return row
	}
	row.Price = ptrIfPos(latest.Close)
	row.PriceAsOf = barDate(latest)
	row.SMA50 = technicalSMA(bars, 50)
	row.SMA200 = technicalSMA(bars, 200)
	row.Return21D = technicalReturn(bars, 21)
	row.Return63D = technicalReturn(bars, 63)
	row.Return126D = technicalReturn(bars, 126)
	row.BenchmarkReturn63D = bench63
	row.BenchmarkReturn126D = bench126
	if row.Price != nil && row.SMA50 != nil && *row.SMA50 > 0 {
		v := (*row.Price - *row.SMA50) / *row.SMA50
		row.PctAbove50DMA = &v
	}
	if row.Price != nil && row.SMA200 != nil && *row.SMA200 > 0 {
		v := (*row.Price - *row.SMA200) / *row.SMA200
		row.PctAbove200DMA = &v
	}
	if row.Return63D != nil && bench63 != nil {
		v := *row.Return63D - *bench63
		row.RS63D = &v
	}
	if row.Return126D != nil && bench126 != nil {
		v := *row.Return126D - *bench126
		row.RS126D = &v
	}
	if atr := technicalATR(bars, 14); atr > 0 {
		row.ATR14 = &atr
		if row.Price != nil && *row.Price > 0 {
			v := atr / *row.Price
			row.ATRPct = &v
		}
	}
	liq := computeHistoricalLiquidity20D(bars)
	row.AvgVolume20D = liq.avgVolume
	row.AvgDollarVolume20D = liq.avgDollarVolume
	row.LiquiditySampleDays = liq.sampleDays
	row.TrendState = technicalTrendState(row)
	row.MissingReasons = technicalMissingReasons(row)
	row.DataQuality = technicalDataQuality(row)
	return row
}

func latestTechnicalBar(bars []ibkrlib.HistoricalBar) (ibkrlib.HistoricalBar, bool) {
	for _, bar := range slices.Backward(bars) {
		if bar.Close > 0 {
			return bar, true
		}
	}
	return ibkrlib.HistoricalBar{}, false
}

func technicalSMA(bars []ibkrlib.HistoricalBar, n int) *float64 {
	if n <= 0 || len(bars) < n {
		return nil
	}
	var sum float64
	var count int
	for _, b := range bars[len(bars)-n:] {
		if b.Close <= 0 {
			continue
		}
		sum += b.Close
		count++
	}
	if count != n {
		return nil
	}
	v := sum / float64(count)
	return &v
}

func technicalReturn(bars []ibkrlib.HistoricalBar, n int) *float64 {
	if n <= 0 || len(bars) <= n {
		return nil
	}
	last := bars[len(bars)-1].Close
	prev := bars[len(bars)-1-n].Close
	if last <= 0 || prev <= 0 {
		return nil
	}
	v := last/prev - 1
	return &v
}

func technicalATR(bars []ibkrlib.HistoricalBar, n int) float64 {
	if n <= 0 || len(bars) <= n {
		return 0
	}
	start := len(bars) - n
	var sum float64
	var count int
	for i := start; i < len(bars); i++ {
		high, low, prevClose := bars[i].High, bars[i].Low, bars[i-1].Close
		if high <= 0 || low <= 0 || prevClose <= 0 {
			continue
		}
		tr := max(high-low, math.Abs(high-prevClose), math.Abs(low-prevClose))
		sum += tr
		count++
	}
	if count != n {
		return 0
	}
	return sum / float64(count)
}

type historicalLiquidity20D struct {
	avgVolume       *int64
	avgDollarVolume *float64
	sampleDays      int
	asOf            time.Time
}

func computeHistoricalLiquidity20D(bars []ibkrlib.HistoricalBar) historicalLiquidity20D {
	const n = 20
	start := max(len(bars)-n, 0)
	var volumeSum int64
	var dollarSum float64
	var count int
	var asOf time.Time
	for _, b := range bars[start:] {
		if b.Close <= 0 || b.Volume <= 0 {
			continue
		}
		volumeSum += b.Volume
		dollarSum += b.Close * float64(b.Volume)
		count++
		if !b.Time.IsZero() {
			asOf = b.Time
		}
	}
	if count == 0 {
		return historicalLiquidity20D{}
	}
	avgVol := volumeSum / int64(count)
	avgDollar := dollarSum / float64(count)
	return historicalLiquidity20D{
		avgVolume:       &avgVol,
		avgDollarVolume: &avgDollar,
		sampleDays:      count,
		asOf:            asOf,
	}
}

func technicalTrendState(row rpc.TechnicalRow) string {
	if row.Price == nil || row.SMA50 == nil || row.SMA200 == nil {
		return "insufficient_data"
	}
	if *row.Price < *row.SMA200 {
		return "broken"
	}
	if row.PctAbove200DMA != nil && *row.PctAbove200DMA > 0.35 {
		return "extended"
	}
	if *row.Price > *row.SMA50 && *row.SMA50 > *row.SMA200 {
		return "uptrend"
	}
	return "recovering"
}

func technicalMissingReasons(row rpc.TechnicalRow) []string {
	var missing []string
	if row.Price == nil {
		missing = append(missing, "price_unavailable")
	}
	if row.SMA50 == nil {
		missing = append(missing, "sma_50_unavailable")
	}
	if row.SMA200 == nil {
		missing = append(missing, "sma_200_unavailable")
	}
	if row.Return63D == nil {
		missing = append(missing, "return_63d_unavailable")
	}
	if row.Return126D == nil {
		missing = append(missing, "return_126d_unavailable")
	}
	if row.BenchmarkReturn63D == nil {
		missing = append(missing, "benchmark_return_63d_unavailable")
	}
	if row.BenchmarkReturn126D == nil {
		missing = append(missing, "benchmark_return_126d_unavailable")
	}
	if row.ATR14 == nil {
		missing = append(missing, "atr_14_unavailable")
	}
	if row.AvgDollarVolume20D == nil {
		missing = append(missing, "avg_dollar_volume_20d_unavailable")
	}
	return missing
}

func technicalDataQuality(row rpc.TechnicalRow) string {
	if row.Error != "" {
		return "error"
	}
	if row.Price == nil || row.Bars < 50 {
		return "insufficient_data"
	}
	if len(row.MissingReasons) > 0 {
		return "partial"
	}
	return "ok"
}
