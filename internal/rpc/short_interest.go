package rpc

import (
	"errors"
	"math"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"
)

// MethodShortInterestScreen reads the cached FINRA equity short-interest screen.
const MethodShortInterestScreen = "short_interest.screen"

// ShortInterestScreenParams filters the FINRA publication before limiting rows.
type ShortInterestScreenParams struct {
	Limit                 int      `json:"limit"`
	SortBy                string   `json:"sort_by"`
	SortDir               string   `json:"sort_dir"`
	Exclude               []string `json:"exclude"`
	MinPrice              float64  `json:"min_price"`
	MinAvgDollarVolume20D float64  `json:"min_avg_dollar_volume_20d"`
	MinAverageVolume      int64    `json:"min_average_volume"`
	MinDaysToCover        float64  `json:"min_days_to_cover"`
	ListedOnly            bool     `json:"listed_only"`
}

// ShortInterestCoverage describes query-relevant market context across source candidates.
type ShortInterestCoverage struct {
	Candidates  int  `json:"candidates"`
	Covered     int  `json:"covered"`
	Pending     int  `json:"pending"`
	Unavailable int  `json:"unavailable"`
	Complete    bool `json:"complete"`
}

// ShortInterestRow retains one reported position and independently dated enrichments.
type ShortInterestRow struct {
	Symbol                      string   `json:"symbol"`
	Name                        string   `json:"name"`
	Market                      string   `json:"market"`
	ShortInterestShares         int64    `json:"short_interest_shares"`
	PreviousShortInterestShares int64    `json:"previous_short_interest_shares"`
	AverageDailyVolume          int64    `json:"average_daily_volume"`
	DaysToCover                 *float64 `json:"days_to_cover,omitempty"`
	ChangePct                   *float64 `json:"change_pct,omitempty"`
	// Absent until a dated, identity-matched free-float source is supported.
	ShortInterestPctFloat *float64          `json:"short_interest_pct_float,omitempty"`
	SettlementDate        string            `json:"settlement_date"`
	Split                 bool              `json:"split"`
	Revised               bool              `json:"revised"`
	MarketContext         *LendingMarketRow `json:"market_context,omitempty"`
	FeeRate               *float64          `json:"fee_rate,omitempty"`
	FeeAsOf               time.Time         `json:"fee_as_of,omitzero"`
}

// ShortInterestScreenResult binds ranked rows to their source, query and coverage.
type ShortInterestScreenResult struct {
	Kind           string                    `json:"kind"`
	Status         string                    `json:"status"`
	Source         string                    `json:"source"`
	SourceURL      string                    `json:"source_url"`
	SettlementDate string                    `json:"settlement_date,omitempty"`
	FetchedAt      time.Time                 `json:"fetched_at,omitzero"`
	CheckedAt      time.Time                 `json:"checked_at,omitzero"`
	Detail         string                    `json:"detail"`
	Params         ShortInterestScreenParams `json:"params"`
	Total          int                       `json:"total"`
	SkippedRows    int                       `json:"skipped_rows"`
	Matching       int                       `json:"matching"`
	Truncated      bool                      `json:"truncated"`
	Coverage       ShortInterestCoverage     `json:"coverage"`
	Rows           []ShortInterestRow        `json:"rows"`
}

// ShortInterestSortKeys lists supported source and market ranking fields.
var ShortInterestSortKeys = []string{"symbol", "short_interest_shares", "days_to_cover", "change_pct", "average_daily_volume", "price", "day_change_pct", "volume", "avg_dollar_volume_20d", "ytd_change_pct", "fee_rate"}

// NormalizeShortInterestScreenParams applies defaults and rejects unsupported query values.
func NormalizeShortInterestScreenParams(p ShortInterestScreenParams) (ShortInterestScreenParams, error) {
	if p.Limit == 0 {
		p.Limit = 50
	}
	if p.SortBy == "" {
		p.SortBy = "short_interest_shares"
	}
	if p.SortDir == "" {
		p.SortDir = "desc"
	}
	if p.Limit < 1 || p.Limit > 100 || !slices.Contains(ShortInterestSortKeys, p.SortBy) || (p.SortDir != "asc" && p.SortDir != "desc") {
		return p, errors.New("invalid short-interest limit, sort or direction")
	}
	for _, v := range []float64{p.MinPrice, p.MinAvgDollarVolume20D, p.MinDaysToCover} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return p, errors.New("short-interest filters must be finite and nonnegative")
		}
	}
	if p.MinAverageVolume < 0 {
		return p, errors.New("minimum average volume must be nonnegative")
	}
	if len(p.Exclude) > 0 {
		var err error
		p.Exclude, err = NormalizeLendingRateSymbols(p.Exclude)
		if err != nil {
			return p, err
		}
	}
	if p.Exclude == nil {
		p.Exclude = []string{}
	}
	return p, nil
}

// ValidateShortInterestScreenResult verifies source identity, scope, order and evidence clocks.
func ValidateShortInterestScreenResult(r ShortInterestScreenResult, p ShortInterestScreenParams) error {
	bad := func() error { return errors.New("invalid short-interest evidence or scope") }
	if r.Kind != "short_interest_screen" || !reflect.DeepEqual(r.Params, p) || r.Source != "FINRA equity short interest" || r.Total < 0 || r.SkippedRows < 0 || r.Matching < 0 || r.Matching > r.Total || len(r.Rows) != min(p.Limit, r.Matching) || r.Truncated != (r.Matching > len(r.Rows)) {
		return bad()
	}
	if r.SourceURL != "https://www.finra.org/finra-data/browse-catalog/equity-short-interest/files" && !regexp.MustCompile(`^https://cdn\.finra\.org/equity/otcmarket/biweekly/shrt[0-9]{8}\.csv$`).MatchString(r.SourceURL) {
		return bad()
	}
	if (r.Status == "pending" || r.Status == "unavailable") && (r.Total != 0 || r.Matching != 0 || len(r.Rows) != 0) {
		return bad()
	}
	if r.Status != "available" && r.Status != "stale" && r.Status != "pending" && r.Status != "unavailable" {
		return bad()
	}
	c := r.Coverage
	if c.Candidates < 0 || c.Candidates > r.Total || c.Covered < 0 || c.Pending < 0 || c.Unavailable < 0 || c.Covered+c.Pending+c.Unavailable != c.Candidates || c.Complete != (c.Candidates == c.Covered) {
		return bad()
	}
	if r.Status == "available" || r.Status == "stale" {
		if r.FetchedAt.IsZero() || r.CheckedAt.IsZero() || r.FetchedAt.After(r.CheckedAt) {
			return bad()
		}
		d, e := time.Parse(time.DateOnly, r.SettlementDate)
		if e != nil || d.After(r.FetchedAt) || r.SourceURL != "https://cdn.finra.org/equity/otcmarket/biweekly/shrt"+d.Format("20060102")+".csv" {
			return bad()
		}
		if r.Status == "available" && (r.CheckedAt.Sub(r.FetchedAt) > 48*time.Hour || r.CheckedAt.Sub(d) > 35*24*time.Hour) {
			return bad()
		}
	}
	sorted := slices.Clone(r.Rows)
	SortShortInterestRows(sorted, p)
	if !reflect.DeepEqual(sorted, r.Rows) {
		return bad()
	}
	seen := map[string]bool{}
	for _, row := range r.Rows {
		if _, err := NormalizeLendingRateSymbols([]string{row.Symbol}); err != nil {
			return bad()
		}
		if row.Symbol == "" || seen[row.Symbol] || slices.Contains(p.Exclude, row.Symbol) || row.SettlementDate != r.SettlementDate || row.ShortInterestShares < 0 || row.PreviousShortInterestShares < 0 || row.AverageDailyVolume < p.MinAverageVolume || row.ShortInterestPctFloat != nil || (row.Split && row.ChangePct != nil) || (p.ListedOnly && row.Market == "OTC") {
			return bad()
		}
		seen[row.Symbol] = true
		for _, v := range []*float64{row.DaysToCover, row.ChangePct, row.FeeRate} {
			if v != nil && (math.IsNaN(*v) || math.IsInf(*v, 0)) {
				return bad()
			}
		}
		if row.FeeRate != nil && (*row.FeeRate < 0 || row.FeeAsOf.IsZero() || row.FeeAsOf.After(r.CheckedAt) || r.CheckedAt.Sub(row.FeeAsOf) > 96*time.Hour) {
			return bad()
		}
		if row.DaysToCover != nil && (*row.DaysToCover < 1 || row.AverageDailyVolume == 0) {
			return bad()
		}
		if p.MinDaysToCover > 0 && (row.DaysToCover == nil || *row.DaysToCover < p.MinDaysToCover) {
			return bad()
		}
		if m := row.MarketContext; m != nil {
			if m.Symbol != row.Symbol {
				return bad()
			}
			if (m.Price != nil || m.AvgDollarVolume20D != nil || m.DayChangePct != nil || m.Volume != nil || m.YTDChangePct != nil) && (!m.ValidUntil.After(r.CheckedAt) || m.CheckedAt.After(r.CheckedAt)) {
				return bad()
			}
			if err := ValidateLendingMarketResult(LendingMarketResult{Kind: "lending_market", Symbols: []string{row.Symbol}, Rows: []LendingMarketRow{*m}}, []string{row.Symbol}); err != nil {
				return bad()
			}
		}
		if p.MinPrice > 0 && (row.MarketContext == nil || row.MarketContext.Price == nil || *row.MarketContext.Price < p.MinPrice) {
			return bad()
		}
		if p.MinAvgDollarVolume20D > 0 && (row.MarketContext == nil || row.MarketContext.AvgDollarVolume20D == nil || *row.MarketContext.AvgDollarVolume20D < p.MinAvgDollarVolume20D) {
			return bad()
		}
	}
	return nil
}

// SortShortInterestRows ranks in place, with missing values last and symbol tie breaks.
func SortShortInterestRows(rows []ShortInterestRow, p ShortInterestScreenParams) {
	slices.SortFunc(rows, func(a, b ShortInterestRow) int {
		if p.SortBy == "symbol" {
			c := strings.Compare(a.Symbol, b.Symbol)
			if p.SortDir == "desc" {
				return -c
			}
			return c
		}
		av, bv := ShortInterestValue(a, p.SortBy), ShortInterestValue(b, p.SortBy)
		if av == nil && bv != nil {
			return 1
		}
		if av != nil && bv == nil {
			return -1
		}
		if av != nil && bv != nil && *av != *bv {
			c := -1
			if *av > *bv {
				c = 1
			}
			if p.SortDir == "desc" {
				return -c
			}
			return c
		}
		return strings.Compare(a.Symbol, b.Symbol)
	})
}

// ShortInterestValue returns the numeric ranking field, or nil when unavailable.
func ShortInterestValue(r ShortInterestRow, key string) *float64 {
	switch key {
	case "short_interest_shares":
		return new(float64(r.ShortInterestShares))
	case "average_daily_volume":
		return new(float64(r.AverageDailyVolume))
	case "days_to_cover":
		return r.DaysToCover
	case "change_pct":
		return r.ChangePct
	case "fee_rate":
		return r.FeeRate
	}
	if r.MarketContext == nil {
		return nil
	}
	m := r.MarketContext
	switch key {
	case "price":
		return m.Price
	case "day_change_pct":
		return m.DayChangePct
	case "volume":
		if m.Volume != nil {
			return new(float64(*m.Volume))
		}
	case "avg_dollar_volume_20d":
		return m.AvgDollarVolume20D
	case "ytd_change_pct":
		return m.YTDChangePct
	}
	return nil
}
