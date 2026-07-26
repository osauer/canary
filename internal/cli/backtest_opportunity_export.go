package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

const opportunityExportWhatToShow = "ADJUSTED_LAST"

type opportunityHistoryFetcher func(context.Context, string, int, string) (rpc.HistoryDailyResult, error)

type opportunityPriceBarExportOptions struct {
	Symbols         []string
	Benchmark       string
	LookbackDays    int
	BarsPath        string
	ManifestPath    string
	ExporterVersion string
}

type opportunityPriceBarExportResult struct {
	Symbols        []string  `json:"symbols"`
	Benchmark      string    `json:"benchmark"`
	LookbackDays   int       `json:"lookback_days"`
	BarsPath       string    `json:"bars_path"`
	ManifestPath   string    `json:"manifest_path"`
	RowCount       int       `json:"row_count"`
	SourceID       string    `json:"source_id"`
	WhatToShow     string    `json:"what_to_show"`
	PriceBasis     string    `json:"price_basis"`
	BarsSHA256     string    `json:"bars_sha256"`
	ManifestSHA256 string    `json:"manifest_sha256"`
	CreatedAt      time.Time `json:"created_at"`
}

func exportOpportunityPriceBars(ctx context.Context, opts opportunityPriceBarExportOptions, fetch opportunityHistoryFetcher, now time.Time) (opportunityPriceBarExportResult, error) {
	opts.Benchmark = strings.ToUpper(strings.TrimSpace(opts.Benchmark))
	if opts.Benchmark == "" {
		opts.Benchmark = "QQQ"
	}
	if opts.LookbackDays <= 0 {
		opts.LookbackDays = 420
	}
	opts.BarsPath = strings.TrimSpace(opts.BarsPath)
	opts.ManifestPath = strings.TrimSpace(opts.ManifestPath)
	if opts.BarsPath == "" {
		return opportunityPriceBarExportResult{}, fmt.Errorf("--bars is required")
	}
	if opts.ManifestPath == "" {
		return opportunityPriceBarExportResult{}, fmt.Errorf("--bars-manifest is required")
	}
	if opts.BarsPath == opts.ManifestPath {
		return opportunityPriceBarExportResult{}, fmt.Errorf("--bars and --bars-manifest must be different paths")
	}
	if fetch == nil {
		return opportunityPriceBarExportResult{}, fmt.Errorf("history fetcher is required")
	}
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	exporterVersion := strings.TrimSpace(opts.ExporterVersion)
	if exporterVersion == "" {
		exporterVersion = "dev"
	}

	userSymbols := opportunityCleanSymbols(opts.Symbols)
	if len(userSymbols) == 0 {
		return opportunityPriceBarExportResult{}, fmt.Errorf("--symbols is required")
	}
	symbols := opportunityExportSymbols(userSymbols, opts.Benchmark)

	rowsBySymbol := make(map[string][]OpportunityPriceBarRow, len(symbols))
	seenDates := make(map[string]map[string]struct{}, len(symbols))
	for _, symbol := range symbols {
		res, err := fetch(ctx, symbol, opts.LookbackDays, opportunityExportWhatToShow)
		if err != nil {
			return opportunityPriceBarExportResult{}, fmt.Errorf("%s history: %w", symbol, err)
		}
		if got := strings.ToUpper(strings.TrimSpace(res.Symbol)); got != "" && got != symbol {
			return opportunityPriceBarExportResult{}, fmt.Errorf("%s history returned symbol %q", symbol, res.Symbol)
		}
		if got := strings.ToUpper(strings.TrimSpace(res.WhatToShow)); got != opportunityExportWhatToShow {
			return opportunityPriceBarExportResult{}, fmt.Errorf("%s history returned what_to_show %q, expected %s", symbol, res.WhatToShow, opportunityExportWhatToShow)
		}
		if len(res.Bars) == 0 {
			return opportunityPriceBarExportResult{}, fmt.Errorf("%s history returned no bars", symbol)
		}
		for _, bar := range res.Bars {
			date := strings.TrimSpace(bar.Date)
			if _, err := parseOpportunityDate(date); err != nil {
				return opportunityPriceBarExportResult{}, fmt.Errorf("%s history invalid date %q: %w", symbol, bar.Date, err)
			}
			if bar.Close <= 0 {
				return opportunityPriceBarExportResult{}, fmt.Errorf("%s %s adjusted close must be positive", symbol, date)
			}
			if seenDates[symbol] == nil {
				seenDates[symbol] = map[string]struct{}{}
			}
			if _, ok := seenDates[symbol][date]; ok {
				return opportunityPriceBarExportResult{}, fmt.Errorf("%s history returned duplicate bar for %s", symbol, date)
			}
			seenDates[symbol][date] = struct{}{}
			rowsBySymbol[symbol] = append(rowsBySymbol[symbol], OpportunityPriceBarRow{
				Symbol:        symbol,
				Date:          date,
				Open:          bar.Open,
				High:          bar.High,
				Low:           bar.Low,
				Close:         bar.Close,
				AdjustedClose: bar.Close,
				Volume:        bar.Volume,
				Source:        opportunityTrustedBarSourceIBKRHMDSAdjustedOHLCV,
			})
		}
	}

	allRows := make([]OpportunityPriceBarRow, 0, opportunityPriceBarRowCount(rowsBySymbol))
	for symbol := range rowsBySymbol {
		sort.Slice(rowsBySymbol[symbol], func(i, j int) bool {
			return rowsBySymbol[symbol][i].Date < rowsBySymbol[symbol][j].Date
		})
		allRows = append(allRows, rowsBySymbol[symbol]...)
	}
	sort.Slice(allRows, func(i, j int) bool {
		if allRows[i].Symbol != allRows[j].Symbol {
			return allRows[i].Symbol < allRows[j].Symbol
		}
		return allRows[i].Date < allRows[j].Date
	})

	var barsBuf bytes.Buffer
	if err := writeOpportunityPriceBarsJSONL(&barsBuf, allRows); err != nil {
		return opportunityPriceBarExportResult{}, err
	}
	if err := writePrivateAtomic(opts.BarsPath, barsBuf.Bytes()); err != nil {
		return opportunityPriceBarExportResult{}, fmt.Errorf("write bars %s: %w", opts.BarsPath, err)
	}
	barsChecksum, err := sha256FileHex(opts.BarsPath)
	if err != nil {
		return opportunityPriceBarExportResult{}, fmt.Errorf("checksum bars %s: %w", opts.BarsPath, err)
	}

	manifest := opportunityPriceBarManifest{
		SchemaVersion:    opportunityPriceBarManifestSchemaV1,
		SourceID:         opportunityTrustedBarSourceIBKRHMDSAdjustedOHLCV,
		ExporterID:       opportunityTrustedBarSourceIBKRHMDSAdjustedOHLCV,
		ExporterVersion:  exporterVersion,
		Provider:         "IBKR HMDS",
		Method:           "history.daily via IBKR HMDS with explicit what_to_show=ADJUSTED_LAST",
		WhatToShow:       opportunityExportWhatToShow,
		PriceBasis:       "adjusted_close",
		BarSize:          "1 day",
		AdjustmentPolicy: "IBKR HMDS ADJUSTED_LAST daily bars; close is carried as adjusted_close",
		CreatedAt:        now,
		Command:          opportunityExportCommand(opts, symbols),
		BarFile:          filepath.Base(opts.BarsPath),
		BarsSHA256:       "sha256:" + barsChecksum,
		RowCount:         len(allRows),
		Symbols:          opportunityPriceBarManifestSymbolsFromBars(rowsBySymbol),
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return opportunityPriceBarExportResult{}, fmt.Errorf("encode bars manifest: %w", err)
	}
	manifestBytes = append(manifestBytes, '\n')
	if err := writePrivateAtomic(opts.ManifestPath, manifestBytes); err != nil {
		return opportunityPriceBarExportResult{}, fmt.Errorf("write bars manifest %s: %w", opts.ManifestPath, err)
	}
	ledger, err := readOpportunityPriceBarLedgerFromFileWithManifest(opts.BarsPath, opts.ManifestPath)
	if err != nil {
		return opportunityPriceBarExportResult{}, err
	}
	return opportunityPriceBarExportResult{
		Symbols:        symbols,
		Benchmark:      opts.Benchmark,
		LookbackDays:   opts.LookbackDays,
		BarsPath:       opts.BarsPath,
		ManifestPath:   opts.ManifestPath,
		RowCount:       len(allRows),
		SourceID:       opportunityTrustedBarSourceIBKRHMDSAdjustedOHLCV,
		WhatToShow:     opportunityExportWhatToShow,
		PriceBasis:     "adjusted_close",
		BarsSHA256:     ledger.Checksum,
		ManifestSHA256: ledger.ManifestChecksum,
		CreatedAt:      now,
	}, nil
}

func opportunityExportSymbols(symbols []string, benchmark string) []string {
	out := opportunityUniqueSymbols(symbols)
	seen := make(map[string]struct{}, len(out)+1)
	for _, symbol := range out {
		seen[symbol] = struct{}{}
	}
	benchmark = strings.ToUpper(strings.TrimSpace(benchmark))
	if benchmark == "" {
		benchmark = "QQQ"
	}
	if _, ok := seen[benchmark]; !ok {
		out = append(out, benchmark)
	}
	return out
}

func opportunityUniqueSymbols(symbols []string) []string {
	cleanSymbols := opportunityCleanSymbols(symbols)
	out := make([]string, 0, len(cleanSymbols))
	seen := map[string]struct{}{}
	for _, clean := range cleanSymbols {
		if _, ok := seen[clean]; ok {
			continue
		}
		seen[clean] = struct{}{}
		out = append(out, clean)
	}
	return out
}

func opportunityCleanSymbols(symbols []string) []string {
	out := make([]string, 0, len(symbols))
	for _, symbol := range symbols {
		clean := strings.ToUpper(strings.TrimSpace(symbol))
		if clean != "" {
			out = append(out, clean)
		}
	}
	return out
}

func opportunityExportCommand(opts opportunityPriceBarExportOptions, symbols []string) string {
	commandSymbols := opportunityUniqueSymbols(opts.Symbols)
	if len(commandSymbols) == 0 {
		commandSymbols = symbols
	}
	parts := []string{
		"canary backtest export-opportunity-bars",
		"--symbols", strings.Join(commandSymbols, ","),
		"--benchmark", strings.ToUpper(strings.TrimSpace(opts.Benchmark)),
		"--lookback-days", fmt.Sprintf("%d", opts.LookbackDays),
		"--bars", opts.BarsPath,
		"--bars-manifest", opts.ManifestPath,
	}
	return strings.Join(parts, " ")
}

func writeOpportunityPriceBarsJSONL(w io.Writer, rows []OpportunityPriceBarRow) error {
	enc := json.NewEncoder(w)
	for _, row := range rows {
		if err := enc.Encode(row); err != nil {
			return err
		}
	}
	return nil
}

func renderOpportunityPriceBarExportText(env *Env, out io.Writer, res opportunityPriceBarExportResult) int {
	fmt.Fprintf(out, "\nExported %d adjusted daily bar(s) for %d symbol(s)\n\n", res.RowCount, len(res.Symbols))
	fmt.Fprintf(out, "  Symbols   %s\n", strings.Join(res.Symbols, ", "))
	fmt.Fprintf(out, "  Bars      %s\n", res.BarsPath)
	fmt.Fprintf(out, "  Manifest  %s\n", res.ManifestPath)
	fmt.Fprintf(out, "  Source    %s | %s | %s | manifest %s\n", res.SourceID, res.WhatToShow, res.BarsSHA256, res.ManifestSHA256)
	fmt.Fprintln(out)
	return 0
}
