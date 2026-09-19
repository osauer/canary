package rpc

import (
	"errors"
	"math"
	"regexp"
	"time"
)

// Reporting performance RPC: the retained statement equity series with dated
// external flows and year-to-date trading income. The daemon serves facts;
// the consumer computes cash-flow-adjusted returns, drawdowns and comparisons.
const (
	MethodReportingPerformance        = "reporting.performance"
	ReportingPerformanceSchemaVersion = "canary-reporting-performance-v1"

	PerformanceFlowDeposit     = "deposit"
	PerformanceFlowWithdrawal  = "withdrawal"
	PerformanceFlowTransferIn  = "transfer_in"
	PerformanceFlowTransferOut = "transfer_out"
)

// ReportingPerformanceResult is the reporting.performance envelope. Days are
// statement closes in the account base currency, ascending, one per report
// date; a missing report date is a gap, never an interpolated value. Flows are
// external capital movements by value date. Nothing here names the account.
type ReportingPerformanceResult struct {
	SchemaVersion string                       `json:"schema_version"`
	AsOf          time.Time                    `json:"as_of"`
	BaseCurrency  string                       `json:"base_currency,omitempty"`
	Evidence      ReportingPerformanceEvidence `json:"evidence"`
	Days          []PerformanceDay             `json:"days"`
	Flows         []PerformanceFlow            `json:"flows"`
	YearToDate    *PerformanceYearToDate       `json:"year_to_date,omitempty"`
}

// ReportingPerformanceEvidence says what the series is built from. State is
// observed, not_received or degraded; a degraded read carries its reason.
type ReportingPerformanceEvidence struct {
	State                string    `json:"state"`
	Reason               string    `json:"reason,omitempty"`
	CoverageFrom         string    `json:"coverage_from,omitempty"`
	CoverageTo           string    `json:"coverage_to,omitempty"`
	StatementGeneratedAt time.Time `json:"statement_generated_at,omitzero"`
	EquityDays           int       `json:"equity_days"`
	// UnclassifiedLines counts statement lines that are neither a known flow
	// nor known income; they may hide a capital movement the series treats as
	// performance.
	UnclassifiedLines int `json:"unclassified_lines"`
}

// PerformanceDay is one statement close: report date and total equity in the
// base currency.
type PerformanceDay struct {
	Day        string  `json:"day"`
	EquityBase float64 `json:"equity_base"`
}

// PerformanceFlow is one external capital movement on its value date, signed
// from the account's point of view: positive money in, negative money out.
type PerformanceFlow struct {
	Day        string  `json:"day"`
	Type       string  `json:"type"`
	AmountBase float64 `json:"amount_base"`
}

// PerformanceYearToDate sums trading and income lines from 1 January of the
// as-of year through the statement coverage date, in the base currency.
// RealisedBase is IBKR's FIFO realised P&L of execution rows; commissions,
// dividends, interest, withholding tax and fees are separate so a consumer
// can tie them to a statement P&L without double counting.
type PerformanceYearToDate struct {
	From               string  `json:"from"`
	Through            string  `json:"through"`
	RealisedBase       float64 `json:"realised_base"`
	Closes             int     `json:"closes"`
	CommissionsBase    float64 `json:"commissions_base"`
	DividendsBase      float64 `json:"dividends_base"`
	InterestBase       float64 `json:"interest_base"`
	WithholdingTaxBase float64 `json:"withholding_tax_base"`
	FeesBase           float64 `json:"fees_base"`
	OtherBase          float64 `json:"other_base"`
	// UnconvertedTrades counts execution rows whose realised P&L could not
	// be converted to the base currency and is therefore missing from
	// RealisedBase.
	UnconvertedTrades int `json:"unconverted_trades"`
}

var performanceDayPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// ValidateReportingPerformanceResult rejects an envelope that a consumer
// could misread: unknown states, unordered or duplicate days, non-finite
// money, or a flow of unknown kind.
func ValidateReportingPerformanceResult(result ReportingPerformanceResult) error {
	if result.SchemaVersion != ReportingPerformanceSchemaVersion {
		return errors.New("invalid reporting performance schema contract")
	}
	switch result.Evidence.State {
	case ReportingEvidenceObserved, ReportingEvidenceNotReceived, ReportingEvidenceDegraded:
	default:
		return errors.New("invalid reporting performance evidence state")
	}
	for _, day := range []string{result.Evidence.CoverageFrom, result.Evidence.CoverageTo} {
		if day != "" && !performanceDayPattern.MatchString(day) {
			return errors.New("invalid reporting performance coverage date")
		}
	}
	if result.Days == nil || result.Flows == nil {
		return errors.New("reporting performance series must be present, possibly empty")
	}
	previous := ""
	for _, day := range result.Days {
		if !performanceDayPattern.MatchString(day.Day) || day.Day <= previous {
			return errors.New("reporting performance days must be ascending and unique")
		}
		if math.IsNaN(day.EquityBase) || math.IsInf(day.EquityBase, 0) {
			return errors.New("reporting performance equity must be finite")
		}
		previous = day.Day
	}
	previous = ""
	for _, flow := range result.Flows {
		if !performanceDayPattern.MatchString(flow.Day) || flow.Day < previous {
			return errors.New("reporting performance flows must be ascending")
		}
		switch flow.Type {
		case PerformanceFlowDeposit, PerformanceFlowWithdrawal, PerformanceFlowTransferIn, PerformanceFlowTransferOut:
		default:
			return errors.New("invalid reporting performance flow type")
		}
		if math.IsNaN(flow.AmountBase) || math.IsInf(flow.AmountBase, 0) || flow.AmountBase == 0 {
			return errors.New("reporting performance flow amount must be finite and nonzero")
		}
		previous = flow.Day
	}
	if ytd := result.YearToDate; ytd != nil {
		if !performanceDayPattern.MatchString(ytd.From) || !performanceDayPattern.MatchString(ytd.Through) || ytd.Through < ytd.From {
			return errors.New("invalid reporting performance year-to-date window")
		}
		for _, value := range []float64{ytd.RealisedBase, ytd.CommissionsBase, ytd.DividendsBase, ytd.InterestBase, ytd.WithholdingTaxBase, ytd.FeesBase, ytd.OtherBase} {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return errors.New("reporting performance year-to-date sums must be finite")
			}
		}
	}
	return nil
}
