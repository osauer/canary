package daemon

import (
	"math"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// evaluateRiskPolicyV3Reconciliation is the only daemon mutation path from
// retained statement evidence. It first installs a fully healthy
// statement-authoritative capital snapshot, then appends at most one automatic
// reconcile event for the pinned report. Three paths reach it: daemon startup,
// a successful Flex ingest, and the account-summary read on the day's first
// observation, where the broker report can land before today's runtime equity.
// The report-id gate keeps that third path idempotent.
func (s *Server) evaluateRiskPolicyV3Reconciliation() (extended bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			extended = false
			s.warnf("risk-policy v3 reconcile evaluation recovered: %v", recovered)
		}
	}()
	if s == nil || s.riskPolicies == nil || s.riskCapital == nil {
		return false
	}

	s.reconMu.Lock()
	defer s.reconMu.Unlock()
	s.riskCapital.EnsureLoaded()
	mgr := s.riskPolicies.snapshot()
	pol := mgr.policy
	if pol == nil || pol.PolicyVersion < 3 {
		return false
	}

	rep, snapshot := s.buildReconReportWithSnapshot()
	if rep.Status == rpc.ReconStatusActive && statementsHealthOK(rep.InputHealth) && snapshot != nil {
		s.riskCapital.IncorporateStatementSnapshot(*snapshot)
	} else if rep.Status == rpc.ReconStatusUnavailable && strings.HasPrefix(rep.Message, "no retained Flex statements yet") {
		s.riskCapital.ActivateStatementAuthorityWithoutStatements()
	}

	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	if !autoExtendEligible(mgr.status, pol, rep, now) {
		return false
	}
	ev, appended, err := s.riskCapital.ApplyAutomaticReconcile(rep.ReportID, rep.CoverageTo)
	if err != nil {
		s.warnf("risk-policy v3 auto-extend failed for report %s: %v", rep.ReportID, err)
		return false
	}
	if appended {
		s.infof("risk-policy reconcile clock extended automatically from clean report %s at %s", rep.ReportID, ev.At.Format(time.RFC3339))
	}
	return appended
}

func autoExtendEligible(policyStatus string, pol *risk.Constitution, rep *rpc.ReconResult, now time.Time) bool {
	if policyStatus != rpc.RiskPolicyStatusActive || pol == nil || pol.PolicyVersion < 3 || pol.Recon.MaxEquityDivergencePct == nil || rep == nil {
		return false
	}
	if rep.Status != rpc.ReconStatusActive || !statementsHealthOK(rep.InputHealth) || rep.Unresolved != 0 || rep.ReportID == "" {
		return false
	}
	rc := reconPolicyOf(pol)
	if rc == nil || rep.StatementAsOf.IsZero() {
		return false
	}
	maxAge := time.Duration(*rc.MaxReportAgeDays) * 24 * time.Hour
	if now.Sub(rep.StatementAsOf) > maxAge || rep.Equity == nil || !rep.Equity.SameDay || rep.Equity.DivergencePct == nil {
		return false
	}
	if rep.Equity.StatementDate.IsZero() || now.Sub(rep.Equity.StatementDate) > maxAge {
		return false
	}
	return math.Abs(*rep.Equity.DivergencePct) <= *pol.Recon.MaxEquityDivergencePct
}

func statementsHealthOK(health []rpc.SourceHealth) bool {
	for _, row := range health {
		if row.Source == "statements" {
			return row.Status == "ok"
		}
	}
	return false
}

func (s *Server) reconAutomationStatus(report *rpc.ReconResult, now time.Time) rpc.ReconAutomationStatus {
	status := rpc.ReconAutomationStatus{
		Evaluation: rpc.ReconEvaluationStatus{State: rpc.ReconEvaluationStateWaiting, Reason: rpc.ReconEvaluationReasonReportPending},
	}
	if report == nil {
		status.Report = s.flexFetchStatusAt(now)
		if status.Report.State == rpc.ReconReportStateChecking {
			status.Evaluation = rpc.ReconEvaluationStatus{State: rpc.ReconEvaluationStateChecking, Reason: rpc.ReconEvaluationReasonReportPending}
		}
		return status
	}
	status.Report = report.Fetch
	if status.Report.State == rpc.ReconReportStateChecking {
		status.Evaluation = rpc.ReconEvaluationStatus{State: rpc.ReconEvaluationStateChecking, Reason: rpc.ReconEvaluationReasonReportPending}
		return status
	}
	if status.Report.State != rpc.ReconReportStateCurrent {
		return status
	}
	switch report.Status {
	case rpc.ReconStatusUnapproved:
		status.Evaluation = rpc.ReconEvaluationStatus{State: rpc.ReconEvaluationStateFailed, Reason: rpc.ReconEvaluationReasonPolicyUnapproved}
		return status
	case rpc.ReconStatusActive:
	case rpc.ReconStatusDegraded:
		status.Evaluation = rpc.ReconEvaluationStatus{State: rpc.ReconEvaluationStateFailed, Reason: rpc.ReconEvaluationReasonEvaluationFailed}
		return status
	default:
		status.Evaluation = rpc.ReconEvaluationStatus{State: rpc.ReconEvaluationStateFailed, Reason: rpc.ReconEvaluationReasonEvaluationFailed}
		return status
	}
	if report.Unresolved > 0 {
		status.Evaluation = rpc.ReconEvaluationStatus{State: rpc.ReconEvaluationStateAttentionRequired, Reason: rpc.ReconEvaluationReasonExceptionsNeedReview}
		return status
	}
	if report.ReportID != "" && report.LastAutoExtendReportID == report.ReportID {
		status.Evaluation = rpc.ReconEvaluationStatus{State: rpc.ReconEvaluationStateComplete}
		return status
	}
	if report.Equity == nil || !report.Equity.SameDay || report.Equity.DivergencePct == nil {
		status.Evaluation = rpc.ReconEvaluationStatus{State: rpc.ReconEvaluationStateWaiting, Reason: rpc.ReconEvaluationReasonAccountValuePending}
		return status
	}
	policy := s.riskPolicies.snapshot().policy
	if policy == nil || policy.Recon.MaxEquityDivergencePct == nil {
		status.Evaluation = rpc.ReconEvaluationStatus{State: rpc.ReconEvaluationStateFailed, Reason: rpc.ReconEvaluationReasonPolicyUnapproved}
		return status
	}
	if math.Abs(*report.Equity.DivergencePct) > *policy.Recon.MaxEquityDivergencePct {
		status.Evaluation = rpc.ReconEvaluationStatus{State: rpc.ReconEvaluationStateAttentionRequired, Reason: rpc.ReconEvaluationReasonAccountValueMismatch}
		return status
	}
	// A clean, current, same-day report inside tolerance should have been
	// extended by the evaluator before the pipeline reports completion.
	status.Evaluation = rpc.ReconEvaluationStatus{State: rpc.ReconEvaluationStateFailed, Reason: rpc.ReconEvaluationReasonEvaluationFailed}
	return status
}
