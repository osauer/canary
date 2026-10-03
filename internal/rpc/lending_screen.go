package rpc

import (
	"errors"
	"math"
	"slices"
	"time"
)

// MethodLendingScreen reads the daemon's existing US bulk borrow-fee source.
const MethodLendingScreen = "lending.screen"

// LendingScreenParams bounds discovery outside an optional explicit symbol set.
// MinRate is an annualized borrower percentage, not a lending yield or risk rule.
type LendingScreenParams struct {
	MinRate float64  `json:"min_rate"`
	Limit   int      `json:"limit"`
	Exclude []string `json:"exclude,omitempty"`
}

// LendingScreenRow carries provider identity, not resolved contract eligibility.
type LendingScreenRow struct {
	MarketEventBorrowFeeCoverage
	Name     string `json:"name,omitempty"`
	Currency string `json:"currency"`
}

// LendingScreenResult distinguishes a usable empty screen from missing evidence.
// Counts describe parsed USD rows; malformed provider rows are reported separately.
type LendingScreenResult struct {
	Kind         string              `json:"kind"`
	Universe     string              `json:"universe"`
	Status       string              `json:"status"`
	AsOf         time.Time           `json:"as_of"`
	ObservedAt   time.Time           `json:"observed_at"`
	Params       LendingScreenParams `json:"params"`
	Total        int                 `json:"total"`
	Usable       int                 `json:"usable"`
	Matching     int                 `json:"matching"`
	SkippedRows  int                 `json:"skipped_rows"`
	Truncated    bool                `json:"truncated"`
	Rows         []LendingScreenRow  `json:"rows"`
	SourceHealth SourceHealth        `json:"source_health"`
}

// NormalizeLendingScreenParams applies shared bounds before any daemon read.
func NormalizeLendingScreenParams(p LendingScreenParams) (LendingScreenParams, error) {
	if math.IsNaN(p.MinRate) || math.IsInf(p.MinRate, 0) || p.MinRate < 0 {
		return p, errors.New("min-rate must be a finite nonnegative annualized percentage")
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
	if r.Kind != "lending_screen" || r.Universe != "us_short_stock" || r.Params.MinRate != p.MinRate || r.Params.Limit != p.Limit || !slices.Equal(r.Params.Exclude, p.Exclude) || r.Total < 0 || r.Usable < 0 || r.Usable > r.Total || r.Matching < 0 || r.Matching > r.Usable || r.SkippedRows < 0 || len(r.Rows) > p.Limit || len(r.Rows) > r.Matching || r.Truncated != (r.Matching > len(r.Rows)) {
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
	seen := map[string]bool{}
	for i, row := range r.Rows {
		if _, err := NormalizeLendingRateSymbols([]string{row.Symbol}); err != nil {
			return bad()
		}
		if seen[row.Symbol] || slices.Contains(p.Exclude, row.Symbol) || row.Currency != "USD" || row.Status != BorrowFeeCoverageObserved || row.Source != BorrowFeeSourceBulkShortStock || row.DataType != BorrowFeeDataTypeBulkFeeRate || row.CoverageScope != BorrowFeeCoverageGlobal || row.ScaleStatus != BorrowFeeScalePercentAnnualized || !row.PolicyEligible || !row.AsOf.Equal(r.AsOf) || !row.ObservedAt.Equal(r.ObservedAt) || row.FeeRate == nil || math.IsNaN(*row.FeeRate) || math.IsInf(*row.FeeRate, 0) || *row.FeeRate < p.MinRate {
			return bad()
		}
		seen[row.Symbol] = true
		if i > 0 {
			prev := r.Rows[i-1]
			if *prev.FeeRate < *row.FeeRate || (*prev.FeeRate == *row.FeeRate && prev.Symbol >= row.Symbol) {
				return bad()
			}
		}
	}
	return nil
}
