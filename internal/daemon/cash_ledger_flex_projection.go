package daemon

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

var flexCashProjectionCoverageGaps = []string{
	"unsettled debit obligations already outstanding at the statement baseline",
	"complete account-wide intraday executions, including manual and offline trades",
	"intraday withdrawals, transfers, FX cash legs, corporate actions and other cash movements",
	"exact fees and uninterrupted activity coverage since the baseline",
}

// attachFlexCashProjections makes optional historical settlement evidence
// useful without converting missing activity into cash authority. This adapter
// deliberately has no complete-coverage witness: no estimate enters Ledger.
func (e *proposalEngine) attachFlexCashProjections(ctx context.Context, bucket *protectionCashSweepPolicy, acct *rpc.AccountResult, scope brokerStateScope, in *cashSweepInput) {
	if e == nil || e.server == nil || in == nil || in.LedgerReason != "" || ctx.Err() != nil {
		return
	}
	needed := false
	for _, row := range in.Ledger {
		needed = needed || row.Settled == nil
	}
	if !needed {
		return
	}
	s := e.server
	connector := s.gatewayConnector()
	if connector == nil {
		return
	}
	session, ok := connector.CaptureSession()
	if !ok || !sameBrokerScope(scope, s.currentBrokerStateScope()) {
		return
	}
	baseline, baselineErr := s.flexSettledCashBaseline(scope, s.nowUTC())
	known := cashSweepSettlement{Reason: "local purchase and fee evidence is unavailable"}
	var fillCutoff time.Time
	if baselineErr == nil {
		views, events, err := s.loadOrderViews()
		if err == nil {
			if cutoff, err := cashLedgerFillCutoffFrom(views, events, scope, s.nowUTC()); err == nil {
				fillCutoff = cutoff
				known = cashSweepSettlementFrom(views, events, scope, baseline.ActivityFrom)
			}
		}
	}
	// Recheck the actual captured broker session after all disk/journal work.
	// A same-account reconnection still invalidates the original current cash.
	if ctx.Err() != nil || s.gatewayConnector() != connector || !connector.SessionCurrent(session) || !sameBrokerScope(scope, s.currentBrokerStateScope()) {
		in.LedgerReason = "broker session changed while reading Flex cash evidence"
		in.FlexProjections = nil
		return
	}
	now := s.nowUTC()
	if acct == nil || acct.Authority == nil || acct.Authority.Scope != accountDataScope(scope) || acct.Authority.AsOf.IsZero() || acct.Authority.AsOf.After(now) || now.Sub(acct.Authority.AsOf) >= accountSnapshotFreshFor {
		in.LedgerReason = "current-account cash expired while reading Flex evidence; wait for a fresh broker observation"
		return
	}
	if baselineErr == nil && known.Known {
		cutoff, err := s.cashLedgerFillCutoff(scope)
		if err != nil || !cutoff.Equal(fillCutoff) {
			known = cashSweepSettlement{Reason: "local fills changed while reading Flex evidence"}
		}
	}
	_, currentLedger, ledgerReason := cashSweepLedgerAt(acct, now)
	if ledgerReason != "" {
		in.LedgerReason = ledgerReason
		return
	}
	in.FlexProjections = map[string]*rpc.CashSweepSettlementProjection{}
	for ccy, row := range currentLedger {
		if row.Settled != nil {
			continue
		}
		in.FlexProjections[ccy] = cashSweepFlexProjection(baseline, baselineErr, scope, ccy, row, known, in.Commitments, cashSweepKeepOrZero(bucket, ccy))
	}
}

// cashSweepFlexProjection computes diagnostic estimates only. In particular,
// neither a balanced statement nor an empty local journal proves the absence
// of hidden offsetting credits/debits or obligations predating the statement.
func cashSweepFlexProjection(baseline flexCashBaseline, baselineErr error, scope brokerStateScope, ccy string, current cashSweepLedgerRow, known cashSweepSettlement, commitments cashSweepCommitments, reserve float64) *rpc.CashSweepSettlementProjection {
	out := &rpc.CashSweepSettlementProjection{State: "unavailable", Source: "flex_projection", CoverageGaps: slices.Clone(flexCashProjectionCoverageGaps)}
	if baselineErr != nil {
		out.Reason = baselineErr.Error()
		return out
	}
	row, exists := baseline.Currencies[ccy]
	if !sameBrokerScope(baseline.Scope, scope) || !exists || row.EndingCash == nil || row.EndingSettledCash == nil ||
		!finiteProtectionOptionPolicyValue(*row.EndingCash) || !finiteProtectionOptionPolicyValue(*row.EndingSettledCash) ||
		baseline.ReportFingerprint == "" || baseline.QueryFingerprint == "" || baseline.ToDate.IsZero() || baseline.ActivityFrom.IsZero() {
		out.Reason = "Flex cash baseline has no exact current-account native-currency row"
		return out
	}
	out.State = "held"
	out.Reason = "Flex baseline found; sweep held until baseline obligations and complete intraday cash activity are verified"
	out.QueryFingerprint, out.ReportFingerprint = baseline.QueryFingerprint, baseline.ReportFingerprint
	out.StatementDate, out.AcceptedAt, out.ActivityFrom = baseline.ToDate.Format(time.DateOnly), baseline.AcceptedAt, baseline.ActivityFrom
	out.GeneratedLabel = baseline.GeneratedAt.Format("20060102;150405")
	out.BaselineCash, out.BaselineSettledCash = new(*row.EndingCash), new(*row.EndingSettledCash)
	if !known.Known || cashSweepUnknownReason(known.Known, known.Reason, known.Unknown, ccy) != "" {
		out.Reason += "; known purchase deductions are unavailable"
		return out
	}
	purchases, sales := known.PurchaseCosts[ccy], known.SaleProceeds[ccy]
	if purchases < 0 || sales < 0 || !finiteProtectionOptionPolicyValue(purchases) || !finiteProtectionOptionPolicyValue(sales) {
		out.Reason += "; known purchase or sale totals are invalid"
		return out
	}
	out.KnownPurchases, out.KnownExcludedSales = new(purchases), new(sales)
	if !current.Observed || !finiteProtectionOptionPolicyValue(current.TradeDate) {
		return out
	}
	// Excluding observed sales from the live-cash ceiling also catches known
	// sale credits offsetting a cash withdrawal. Same-report-day credits may
	// be excluded twice; that caution still does not prove unseen activity.
	cash := min(current.TradeDate-sales, *row.EndingSettledCash-purchases)
	if !finiteProtectionOptionPolicyValue(cash) {
		return out
	}
	out.EstimatedCash = new(max(0, cash))
	// Sale proceeds are never added to the baseline. A later confirmed Flex
	// baseline is the only credit; neither journal principal nor fees are
	// certified account-wide evidence.
	if cashSweepUnknownReason(commitments.Known, commitments.Reason, commitments.Unknown, ccy) != "" {
		return out
	}
	committed := commitments.ByCurrency[ccy]
	if committed < 0 || reserve < 0 || !finiteProtectionOptionPolicyValue(committed) || !finiteProtectionOptionPolicyValue(reserve) {
		return out
	}
	free := cash - committed - reserve
	if !finiteProtectionOptionPolicyValue(free) {
		return out
	}
	out.EstimatedFree = new(max(0, free))
	return out
}

func flexCashProjectionEvidence(p *rpc.CashSweepSettlementProjection) []string {
	if p == nil {
		return nil
	}
	out := []string{p.Reason}
	if p.StatementDate != "" {
		out = append(out, fmt.Sprintf("Flex statement %s; estimates are unverified and cannot authorise a sweep", p.StatementDate))
	}
	return out
}

// cashSweepKeepOrZero is ccy's written keep_cash, 0 while none is written
// (the currency then holds at needs_your_number).
func cashSweepKeepOrZero(bucket *protectionCashSweepPolicy, ccy string) float64 {
	keep, _ := bucket.keepCash(ccy)
	return keep
}
