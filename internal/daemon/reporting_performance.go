package daemon

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/flexstmt"
	"github.com/osauer/canary/v2/internal/rpc"
)

const performanceDayFormat = "2006-01-02"

// handleReportingPerformance serves reporting.performance from the retained
// Flex statements of the selected broker account. It reads local files only,
// changes no reconciliation state and returns facts, never a return figure.
func (s *Server) handleReportingPerformance(ctx context.Context) (*rpc.ReportingPerformanceResult, error) {
	now := time.Now()
	if s != nil && s.now != nil {
		now = s.now()
	}
	result := emptyReportingPerformance(now)
	if s == nil {
		result.Evidence.State = rpc.ReportingEvidenceDegraded
		result.Evidence.Reason = rpc.ReconReportReasonAuthorityUnavailable
		return &result, nil
	}
	scope := s.currentBrokerStateScope()
	if !brokerScopeConcrete(scope) {
		result.Evidence.State = rpc.ReportingEvidenceDegraded
		result.Evidence.Reason = rpc.ReconReportReasonAuthorityUnavailable
		return &result, nil
	}
	statements, _, err := s.loadActiveRetainedFlexStatementsContext(ctx, nil)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		s.warnf("reporting performance: retained Flex statement read failed: %v", err)
		result.Evidence.State = rpc.ReportingEvidenceDegraded
		result.Evidence.Reason = rpc.ReconReportReasonStorageFailed
		return &result, nil
	}
	statements, _ = retainedStatementsForScope(statements, scope)
	result = buildReportingPerformance(statements, now)
	if err := rpc.ValidateReportingPerformanceResult(result); err != nil {
		return nil, err
	}
	return &result, nil
}

func emptyReportingPerformance(now time.Time) rpc.ReportingPerformanceResult {
	return rpc.ReportingPerformanceResult{
		SchemaVersion: rpc.ReportingPerformanceSchemaVersion,
		AsOf:          now.UTC(),
		Evidence:      rpc.ReportingPerformanceEvidence{State: rpc.ReportingEvidenceNotReceived},
		Days:          []rpc.PerformanceDay{},
		Flows:         []rpc.PerformanceFlow{},
	}
}

// buildReportingPerformance folds one account's retained statements into the
// equity series, its external flows and the year-to-date income sums. It
// reuses the reconciliation merge, so restatements resolve the same way:
// the newest broker generation wins per day and per line.
func buildReportingPerformance(statements []flexstmt.Statement, now time.Time) rpc.ReportingPerformanceResult {
	result := emptyReportingPerformance(now)
	if len(statements) == 0 {
		return result
	}
	merged := mergeRetainedStatements(statements)
	result.BaseCurrency = inferEdgeBaseCurrency(statements)
	for _, row := range merged.equityByDay {
		result.Days = append(result.Days, rpc.PerformanceDay{Day: row.ReportDate.UTC().Format(performanceDayFormat), EquityBase: row.TotalBase})
	}
	sort.Slice(result.Days, func(i, j int) bool { return result.Days[i].Day < result.Days[j].Day })
	for _, flow := range merged.flows {
		if flow.amountBase == 0 {
			continue
		}
		result.Flows = append(result.Flows, rpc.PerformanceFlow{Day: flow.valueDate.UTC().Format(performanceDayFormat), Type: performanceFlowType(flow), AmountBase: flow.amountBase})
	}
	sort.SliceStable(result.Flows, func(i, j int) bool { return result.Flows[i].Day < result.Flows[j].Day })
	result.Evidence = rpc.ReportingPerformanceEvidence{
		State:                rpc.ReportingEvidenceObserved,
		StatementGeneratedAt: merged.statementAsOf.UTC(),
		EquityDays:           len(result.Days),
		UnclassifiedLines:    len(merged.exceptions),
	}
	if !merged.coverageFrom.IsZero() {
		result.Evidence.CoverageFrom = merged.coverageFrom.UTC().Format(performanceDayFormat)
	}
	if !merged.coverageTo.IsZero() {
		result.Evidence.CoverageTo = merged.coverageTo.UTC().Format(performanceDayFormat)
	}
	result.YearToDate = performanceYearToDate(statements, result.BaseCurrency, now, merged.coverageTo)
	return result
}

func performanceFlowType(flow reconFlow) string {
	switch {
	case strings.HasPrefix(flow.typ, "Transfer ") && flow.amountBase < 0:
		return rpc.PerformanceFlowTransferOut
	case strings.HasPrefix(flow.typ, "Transfer "):
		return rpc.PerformanceFlowTransferIn
	case flow.amountBase < 0:
		return rpc.PerformanceFlowWithdrawal
	default:
		return rpc.PerformanceFlowDeposit
	}
}

// performanceYearToDate sums execution-level trade rows and classified income
// lines dated from 1 January of the as-of year. Overlapping statements are
// read newest generation first and every row counts once; closed-lot and
// order-level trade rows are skipped because they restate executions.
func performanceYearToDate(statements []flexstmt.Statement, base string, now, coverageTo time.Time) *rpc.PerformanceYearToDate {
	from := time.Date(now.UTC().Year(), time.January, 1, 0, 0, 0, 0, time.UTC)
	through := coverageTo.UTC()
	if through.IsZero() || through.Before(from) {
		return nil
	}
	ordered := append([]flexstmt.Statement(nil), statements...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].WhenGenerated.After(ordered[j].WhenGenerated) })
	out := &rpc.PerformanceYearToDate{From: from.Format(performanceDayFormat), Through: through.Format(performanceDayFormat)}
	seen := map[string]bool{}
	for _, statement := range ordered {
		for _, trade := range statement.Trades {
			if trade.LevelOfDetail != "" && !strings.EqualFold(trade.LevelOfDetail, "EXECUTION") {
				continue
			}
			key := performanceTradeKey(trade)
			if key == "" || seen["trade\x00"+key] {
				continue
			}
			seen["trade\x00"+key] = true
			day := trade.ReportDate
			if day.IsZero() {
				day = trade.ExecutedAt
			}
			if day.IsZero() || day.UTC().Before(from) {
				continue
			}
			rate, ok := performanceRate(trade.Currency, trade.FXRateToBase, base)
			if trade.RealizedPNL != nil {
				if !ok {
					out.UnconvertedTrades++
				} else {
					out.RealisedBase += *trade.RealizedPNL * rate
					if *trade.RealizedPNL != 0 {
						out.Closes++
					}
				}
			}
			if trade.Commission != nil && ok {
				out.CommissionsBase += *trade.Commission * rate
			}
		}
		for _, line := range statement.Cash {
			if line.Category != flexstmt.CategoryClassified || line.AmountBase == nil || seen["cash\x00"+line.ID] {
				continue
			}
			seen["cash\x00"+line.ID] = true
			if line.ValueDate.IsZero() || line.ValueDate.UTC().Before(from) {
				continue
			}
			amount := *line.AmountBase
			switch line.Type {
			case "Dividends", "Payment In Lieu Of Dividends":
				out.DividendsBase += amount
			case "Withholding Tax":
				out.WithholdingTaxBase += amount
			case "Broker Interest Paid", "Broker Interest Received", "Bond Interest Paid", "Bond Interest Received":
				out.InterestBase += amount
			case "Other Fees", "Commission Adjustments", "Price Adjustments":
				out.FeesBase += amount
			default:
				out.OtherBase += amount
			}
		}
	}
	return out
}

func performanceTradeKey(trade flexstmt.Trade) string {
	switch {
	case trade.RecordID != "":
		return trade.RecordID
	case trade.TransactionID != "":
		return "transaction:" + trade.TransactionID
	case trade.ExecutionID != "":
		return "execution:" + trade.ExecutionID
	case trade.TradeID != "":
		return "trade:" + trade.TradeID + "\x00" + trade.ExecutedAt.UTC().Format(time.RFC3339)
	default:
		return ""
	}
}

// performanceRate converts a trade amount to the base currency at the
// statement's own rate. A base-currency row needs no rate; any other row
// without a rate stays unconverted rather than guessed.
func performanceRate(currency string, rate *float64, base string) (float64, bool) {
	if rate != nil && *rate > 0 {
		return *rate, true
	}
	if currency == "" || base == "" || strings.EqualFold(currency, base) {
		return 1, true
	}
	return 0, false
}
