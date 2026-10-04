package rpc

import (
	"cmp"
	"errors"
	"math"
	"slices"
	"strings"
	"time"
)

// MethodLendingScreen reads the daemon's existing US bulk borrow-fee source.
const MethodLendingScreen = "lending.screen"

// LendingScreenParams bounds discovery outside an optional explicit symbol set.
// MinRate is an annualized borrower percentage, not a lending yield or risk rule.
type LendingScreenParams struct {
	MinRate               float64  `json:"min_rate"`
	Limit                 int      `json:"limit"`
	Exclude               []string `json:"exclude,omitempty"`
	MinPrice              float64  `json:"min_price,omitempty"`
	MinAvgDollarVolume20D float64  `json:"min_avg_dollar_volume_20d,omitempty"`
	MinHighDates          int      `json:"min_high_dates,omitempty"`
	SortBy                string   `json:"sort_by,omitempty"`
	SortDir               string   `json:"sort_dir,omitempty"`
}

// LendingFeeSample is the latest observed fee on one provider source date.
type LendingFeeSample struct {
	AsOf    time.Time `json:"as_of"`
	FeeRate float64   `json:"fee_rate"`
}

// LendingScreenRow carries provider identity, not resolved contract eligibility.
type LendingScreenRow struct {
	MarketEventBorrowFeeCoverage
	Name      string             `json:"name,omitempty"`
	Currency  string             `json:"currency"`
	Market    *LendingMarketRow  `json:"market,omitempty"`
	HighDates int                `json:"high_dates"`
	History   []LendingFeeSample `json:"history"`
}

// LendingScreenCoverage describes evidence across fee/exclusion candidates before
// market filters and result limits. Complete means all candidates have the
// fields needed for this query; it does not promise trading liquidity.
type LendingScreenCoverage struct {
	Candidates  int  `json:"candidates"`
	Covered     int  `json:"covered"`
	Pending     int  `json:"pending"`
	Unavailable int  `json:"unavailable"`
	Complete    bool `json:"complete"`
}

// LendingScreenResult distinguishes a usable empty screen from missing evidence.
// Counts describe parsed USD rows; malformed provider rows are reported separately.
type LendingScreenResult struct {
	Kind         string                `json:"kind"`
	Universe     string                `json:"universe"`
	Status       string                `json:"status"`
	AsOf         time.Time             `json:"as_of"`
	ObservedAt   time.Time             `json:"observed_at"`
	Params       LendingScreenParams   `json:"params"`
	Total        int                   `json:"total"`
	Usable       int                   `json:"usable"`
	Matching     int                   `json:"matching"`
	SkippedRows  int                   `json:"skipped_rows"`
	Truncated    bool                  `json:"truncated"`
	Rows         []LendingScreenRow    `json:"rows"`
	SourceHealth SourceHealth          `json:"source_health"`
	Coverage     LendingScreenCoverage `json:"coverage"`
}

// NormalizeLendingScreenParams applies shared bounds before any daemon read.
func NormalizeLendingScreenParams(p LendingScreenParams) (LendingScreenParams, error) {
	if math.IsNaN(p.MinRate) || math.IsInf(p.MinRate, 0) || p.MinRate < 0 {
		return p, errors.New("min-rate must be a finite nonnegative annualized percentage")
	}
	for _, v := range []float64{p.MinPrice, p.MinAvgDollarVolume20D} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return p, errors.New("price and turnover minimums must be finite and nonnegative")
		}
	}
	if p.MinHighDates < 0 || p.MinHighDates > 7 {
		return p, errors.New("min-high-dates must be 0-7 distinct provider dates within seven days")
	}
	if p.SortBy != "" && !slices.Contains([]string{"symbol", "price", "day_change_pct", "volume", "avg_dollar_volume_20d", "ytd_change_pct", "fee_rate", "high_dates"}, p.SortBy) {
		return p, errors.New("unsupported lending sort-by")
	}
	if p.SortDir != "" && p.SortDir != "asc" && p.SortDir != "desc" {
		return p, errors.New("sort-dir must be asc or desc")
	}
	if p.Limit == 0 {
		p.Limit = 50
	}
	if p.Limit < 1 || p.Limit > 100 {
		return p, errors.New("limit must be 1-100")
	}
	if len(p.Exclude) > 0 {
		var err error
		p.Exclude, err = NormalizeLendingRateSymbols(p.Exclude)
		if err != nil {
			return p, err
		}
	}
	return p, nil
}

// ValidateLendingScreenResult fences source, scope, bounds and rate ordering in adapters.
func ValidateLendingScreenResult(r LendingScreenResult, p LendingScreenParams) error {
	bad := func() error { return errors.New("invalid lending screen evidence or scope") }
	if r.Kind != "lending_screen" || r.Universe != "us_short_stock" || r.Params.MinRate != p.MinRate || r.Params.Limit != p.Limit || !slices.Equal(r.Params.Exclude, p.Exclude) || r.Params.MinPrice != p.MinPrice || r.Params.MinAvgDollarVolume20D != p.MinAvgDollarVolume20D || r.Params.MinHighDates != p.MinHighDates || r.Params.SortBy != p.SortBy || r.Params.SortDir != p.SortDir || r.Total < 0 || r.Usable < 0 || r.Usable > r.Total || r.Matching < 0 || r.Matching > r.Usable || r.SkippedRows < 0 || len(r.Rows) > p.Limit || len(r.Rows) > r.Matching || r.Truncated != (r.Matching > len(r.Rows)) {
		return bad()
	}
	if r.Status != "observed" {
		if r.Status != "unavailable" || len(r.Rows) != 0 || r.Matching != 0 || r.Usable != 0 {
			return bad()
		}
		return nil
	}
	if r.AsOf.IsZero() || r.ObservedAt.IsZero() || r.AsOf.After(r.ObservedAt) || r.SourceHealth.Status != SourceStatusOK || (r.SourceHealth.RefreshState != SourceRefreshCurrent && r.SourceHealth.RefreshState != SourceRefreshNotDue) || len(r.Rows) != min(r.Matching, p.Limit) {
		return bad()
	}
	c := r.Coverage
	if c.Candidates < 0 || c.Candidates > r.Usable || c.Covered < 0 || c.Pending < 0 || c.Unavailable < 0 || c.Covered+c.Pending+c.Unavailable != c.Candidates || c.Complete != (c.Covered == c.Candidates) {
		return bad()
	}
	seen := map[string]bool{}
	for i, row := range r.Rows {
		if _, err := NormalizeLendingRateSymbols([]string{row.Symbol}); err != nil {
			return bad()
		}
		if seen[row.Symbol] || slices.Contains(p.Exclude, row.Symbol) || row.Currency != "USD" || row.Status != BorrowFeeCoverageObserved || row.Source != BorrowFeeSourceBulkShortStock || row.DataType != BorrowFeeDataTypeBulkFeeRate || row.CoverageScope != BorrowFeeCoverageGlobal || row.ScaleStatus != BorrowFeeScalePercentAnnualized || !row.PolicyEligible || !row.AsOf.Equal(r.AsOf) || !row.ObservedAt.Equal(r.ObservedAt) || row.FeeRate == nil || math.IsNaN(*row.FeeRate) || math.IsInf(*row.FeeRate, 0) || *row.FeeRate < p.MinRate {
			return bad()
		}
		if row.HighDates < 0 || row.HighDates > 7 || row.HighDates < p.MinHighDates {
			return bad()
		}
		if len(row.History) > 7 {
			return bad()
		}
		for j, sample := range row.History {
			if sample.AsOf.IsZero() || sample.AsOf.After(r.AsOf) || math.IsNaN(sample.FeeRate) || math.IsInf(sample.FeeRate, 0) || sample.FeeRate < 0 || (j > 0 && row.History[j-1].AsOf.UTC().Format(time.DateOnly) >= sample.AsOf.UTC().Format(time.DateOnly)) {
				return bad()
			}
		}
		if row.Market != nil {
			if row.Market.Symbol != row.Symbol || ValidateLendingMarketResult(LendingMarketResult{Kind: "lending_market", Symbols: []string{row.Symbol}, Rows: []LendingMarketRow{*row.Market}}, []string{row.Symbol}) != nil {
				return bad()
			}
		}
		if !LendingScreenMatchesMarket(row.Market, p) {
			return bad()
		}
		seen[row.Symbol] = true
		if i > 0 {
			prev := r.Rows[i-1]
			if CompareLendingScreenRows(prev, row, p) >= 0 {
				return bad()
			}
		}
	}
	return nil
}

// LendingScreenMatchesMarket applies only enabled filters; absent is never zero.
func LendingScreenMatchesMarket(m *LendingMarketRow, p LendingScreenParams) bool {
	if p.MinPrice > 0 && (m == nil || m.Price == nil || *m.Price < p.MinPrice) {
		return false
	}
	if p.MinAvgDollarVolume20D > 0 && (m == nil || m.AvgDollarVolume20D == nil || *m.AvgDollarVolume20D < p.MinAvgDollarVolume20D) {
		return false
	}
	return true
}

// LendingScreenMarketCovered measures the requested fields. With no market
// filter/sort it reports useful default price+liquidity coverage of the universe.
func LendingScreenMarketCovered(m *LendingMarketRow, p LendingScreenParams) bool {
	if m == nil || (m.Status != "available" && m.Status != "partial") {
		return false
	}
	if p.MinPrice > 0 && m.Price == nil || p.MinAvgDollarVolume20D > 0 && m.AvgDollarVolume20D == nil {
		return false
	}
	switch p.SortBy {
	case "price":
		return m.Price != nil
	case "day_change_pct":
		return m.DayChangePct != nil
	case "volume":
		return m.Volume != nil
	case "avg_dollar_volume_20d":
		return m.AvgDollarVolume20D != nil
	case "ytd_change_pct":
		return m.YTDChangePct != nil
	}
	if p.MinPrice > 0 || p.MinAvgDollarVolume20D > 0 {
		return true
	}
	return m.Price != nil && m.AvgDollarVolume20D != nil
}

// CompareLendingScreenRows is shared across the daemon and adapter validators.
// Missing fields stay last in either direction; symbols always break ties.
func CompareLendingScreenRows(a, b LendingScreenRow, p LendingScreenParams) int {
	dir := -1
	if p.SortDir == "asc" {
		dir = 1
	}
	key := p.SortBy
	if key == "" {
		key = "fee_rate"
	}
	if key == "symbol" {
		return dir * strings.Compare(a.Symbol, b.Symbol)
	}
	value := func(r LendingScreenRow) *float64 {
		switch key {
		case "fee_rate":
			return r.FeeRate
		case "high_dates":
			v := float64(r.HighDates)
			return &v
		}
		if r.Market == nil {
			return nil
		}
		switch key {
		case "price":
			return r.Market.Price
		case "day_change_pct":
			return r.Market.DayChangePct
		case "avg_dollar_volume_20d":
			return r.Market.AvgDollarVolume20D
		case "ytd_change_pct":
			return r.Market.YTDChangePct
		case "volume":
			if r.Market.Volume != nil {
				v := float64(*r.Market.Volume)
				return &v
			}
		}
		return nil
	}
	av, bv := value(a), value(b)
	if av == nil && bv != nil {
		return 1
	}
	if av != nil && bv == nil {
		return -1
	}
	if av != nil && bv != nil {
		if c := cmp.Compare(*av, *bv); c != 0 {
			return dir * c
		}
	}
	return strings.Compare(a.Symbol, b.Symbol)
}
