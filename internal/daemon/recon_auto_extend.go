package daemon

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v3/internal/risk"
	"github.com/osauer/canary/v3/internal/rpc"
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
	if pol == nil || !pol.Semantics().StatementReconciliation {
		return false
	}

	evaluationScope := s.currentBrokerStateScope()
	if !brokerScopeConcrete(evaluationScope) {
		return false
	}
	rep, snapshot := s.buildReconReportWithSnapshot()
	mutationScope := evaluationScope
	if snapshot != nil {
		mutationScope = snapshot.Scope
	}
	if !sameBrokerScope(mutationScope, evaluationScope) || !sameBrokerScope(mutationScope, s.currentBrokerStateScope()) {
		s.warnf("risk-policy v3 reconciliation stopped because the selected broker account changed during evaluation")
		return false
	}
	if rep.Status == rpc.ReconStatusActive && statementsHealthOK(rep.InputHealth) && snapshot != nil {
		if err := s.riskCapital.IncorporateStatementSnapshotForScope(*snapshot, pol); err != nil {
			s.warnf("risk-policy statement capital snapshot unavailable: %v", err)
			return false
		}
	} else if rep.Status == rpc.ReconStatusUnavailable && strings.HasPrefix(rep.Message, "no retained Flex statements yet") {
		if err := s.riskCapital.ActivateStatementAuthorityWithoutStatementsForScope(mutationScope); err != nil {
			s.warnf("risk-policy statement authority unavailable: %v", err)
			return false
		}
	}
	if !sameBrokerScope(mutationScope, s.currentBrokerStateScope()) {
		s.warnf("risk-policy v3 reconciliation stopped because the selected broker account changed before automatic extension")
		return false
	}

	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	if err := s.riskCapital.retainReconValuation(rep, mutationScope); err != nil {
		s.warnf("risk-policy reconciliation valuation evidence could not be retained: %v", err)
		s.reconEvaluations.record(rep.ReportID, "the comparison evidence could not be retained")
		return false
	}
	if hold, held := autoExtendHoldFor(mgr.status, pol, rep, now); held {
		s.reconEvaluations.record(rep.ReportID, "")
		s.debugf("risk-policy v3 report %s not extended: %s", rep.ReportID, hold.Detail)
		return false
	}
	ev, appended, err := s.riskCapital.ApplyAutomaticReconcileForScope(rep.ReportID, rep.CoverageTo, mutationScope)
	if err != nil {
		s.warnf("risk-policy v3 auto-extend failed for report %s: %v", rep.ReportID, err)
		s.reconEvaluations.record(rep.ReportID, "the automatic extension could not be recorded in the capital journal; the daemon log carries the error")
		return false
	}
	s.reconEvaluations.record(rep.ReportID, "")
	if appended {
		s.infof("risk-policy reconcile clock extended automatically from clean report %s at %s", rep.ReportID, ev.At.Format(time.RFC3339))
	}
	return appended
}

// reconEvaluationRecord keeps the last report the evaluator judged and, when
// the extension it owed could not be recorded, why, so recon status can say
// so instead of a bare failure. The report id never leaves the daemon.
type reconEvaluationRecord struct {
	mu        sync.Mutex
	reportID  string
	notSealed string
}

func (r *reconEvaluationRecord) record(reportID, failure string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reportID, r.notSealed = reportID, failure
}

// failure is why the last evaluation of reportID could not record its
// extension, or "" when it was not that report or it recorded fine.
func (r *reconEvaluationRecord) failure(reportID string) string {
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if reportID == "" || r.reportID != reportID {
		return ""
	}
	return r.notSealed
}

// autoExtendHold is why a report does not extend the policy clock by itself,
// as recon status says it: the evaluation state and reason, and the cause in
// words without a report id, an amount or a threshold.
type autoExtendHold struct {
	State, Reason, Detail string
}

func (h autoExtendHold) status() rpc.ReconEvaluationStatus {
	return rpc.ReconEvaluationStatus{State: h.State, Reason: h.Reason, Detail: h.Detail}
}

// autoExtendHoldFor judges the conditions a clean report must meet to extend
// the clock (docs/docs/operate/reconciliation.md): the one judgement the
// evaluator acts on and recon status reports, so the status can never call
// a hold the evaluator knows a failure. held is false when the report
// extends the clock.
func autoExtendHoldFor(policyStatus string, pol *risk.Constitution, rep *rpc.ReconResult, now time.Time) (hold autoExtendHold, held bool) {
	failed := func(reason, detail string) (autoExtendHold, bool) {
		return autoExtendHold{State: rpc.ReconEvaluationStateFailed, Reason: reason, Detail: detail}, true
	}
	attention := func(reason, detail string) (autoExtendHold, bool) {
		return autoExtendHold{State: rpc.ReconEvaluationStateAttentionRequired, Reason: reason, Detail: detail}, true
	}
	waiting := func(reason, detail string) (autoExtendHold, bool) {
		return autoExtendHold{State: rpc.ReconEvaluationStateWaiting, Reason: reason, Detail: detail}, true
	}
	switch {
	case pol == nil:
		return failed(rpc.ReconEvaluationReasonPolicyUnapproved, "no risk policy is in force")
	case policyStatus != rpc.RiskPolicyStatusActive:
		return failed(rpc.ReconEvaluationReasonPolicyUnapproved, "the risk policy is "+policyStatus+", not active; the clock extends only under an active policy")
	case !pol.Semantics().StatementReconciliation:
		return waiting(rpc.ReconEvaluationReasonEvaluationPending, "the policy predates statement reconciliation (risk-policy v3): the clock is extended by canary policy capital-event reconcile")
	case reconPolicyOf(pol) == nil || pol.Recon.MaxEquityDivergencePct == nil:
		return failed(rpc.ReconEvaluationReasonPolicyUnapproved, "recon.* policy keys are unapproved")
	case rep == nil:
		return failed(rpc.ReconEvaluationReasonEvaluationFailed, "no reconciliation report could be built")
	case rep.Status == rpc.ReconStatusDegraded:
		return failed(rpc.ReconEvaluationReasonEvaluationFailed, "a retained statement could not be read, so the report is degraded and extends nothing; canary recon show names the input health")
	case rep.Status != rpc.ReconStatusActive:
		return failed(rpc.ReconEvaluationReasonEvaluationFailed, nonEmptyString(rep.Message, "the report is "+rep.Status))
	case !statementsHealthOK(rep.InputHealth):
		return failed(rpc.ReconEvaluationReasonEvaluationFailed, "the retained statements are not healthy; canary recon show names the input health")
	case rep.Unresolved != 0:
		noun := "exceptions"
		if rep.Unresolved == 1 {
			noun = "exception"
		}
		return attention(rpc.ReconEvaluationReasonExceptionsNeedReview, fmt.Sprintf("%d unresolved %s; declare or dismiss each, then the clean report extends the clock", rep.Unresolved, noun))
	case rep.ReportID == "":
		return failed(rpc.ReconEvaluationReasonEvaluationFailed, "the report carries no id")
	case rep.StatementAsOf.IsZero():
		return failed(rpc.ReconEvaluationReasonEvaluationFailed, "the newest statement carries no generation time")
	}
	maxAge := time.Duration(*pol.Recon.MaxReportAgeDays) * 24 * time.Hour
	switch {
	case now.Sub(rep.StatementAsOf) > maxAge:
		return attention(rpc.ReconEvaluationReasonReportStale, "the newest statement is older than recon.max_report_age_days; the clock extends when a fresher one lands (report.next_attempt)")
	case rep.Equity == nil || rep.Equity.Comparability != "comparable" || !rep.Equity.SameDay || rep.Equity.DivergencePct == nil || !reconFinite(*rep.Equity.DivergencePct):
		return waiting(rpc.ReconEvaluationReasonAccountValuePending, "the account reading cannot yet be compared on the statement’s closing prices and FX rates; complete valuation evidence is required")
	case rep.Equity.StatementDate.IsZero() || now.Sub(rep.Equity.StatementDate) > maxAge:
		return attention(rpc.ReconEvaluationReasonReportStale, "the statement day the account equity pairs with is older than recon.max_report_age_days; the clock extends when a fresher statement lands (report.next_attempt)")
	case math.Abs(*rep.Equity.DivergencePct) > *pol.Recon.MaxEquityDivergencePct:
		return attention(rpc.ReconEvaluationReasonAccountValueMismatch, "the statement equity and the account reading, valued at the same closing prices and FX rates, differ by more than recon.max_equity_divergence_pct")
	}
	return autoExtendHold{}, false
}

func autoExtendEligible(policyStatus string, pol *risk.Constitution, rep *rpc.ReconResult, now time.Time) bool {
	_, held := autoExtendHoldFor(policyStatus, pol, rep, now)
	return !held
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
	if report.Status == rpc.ReconStatusUnapproved {
		status.Evaluation = rpc.ReconEvaluationStatus{State: rpc.ReconEvaluationStateFailed, Reason: rpc.ReconEvaluationReasonPolicyUnapproved, Detail: report.Message}
		return status
	}
	if report.Status == rpc.ReconStatusActive && report.Unresolved == 0 && report.ReportID != "" && report.LastAutoExtendReportID == report.ReportID {
		status.Evaluation = rpc.ReconEvaluationStatus{State: rpc.ReconEvaluationStateComplete}
		return status
	}
	// The evaluator's own judgement of the report, so a stale statement, a
	// policy that is not active or a degraded report is named as what it is.
	mgr := s.riskPolicies.snapshot()
	if hold, held := autoExtendHoldFor(mgr.status, mgr.policy, report, now); held {
		status.Evaluation = hold.status()
		return status
	}
	// A clean, current, same-day report inside tolerance that the evaluator
	// has not extended: its extension failed to record, or the report changed
	// since the last evaluation (a policy revision, a dismissed exception)
	// and the next evaluation has not run yet.
	if failure := s.reconEvaluations.failure(report.ReportID); failure != "" {
		status.Evaluation = rpc.ReconEvaluationStatus{State: rpc.ReconEvaluationStateFailed, Reason: rpc.ReconEvaluationReasonEvaluationFailed, Detail: failure}
		return status
	}
	status.Evaluation = rpc.ReconEvaluationStatus{State: rpc.ReconEvaluationStateWaiting, Reason: rpc.ReconEvaluationReasonEvaluationPending,
		Detail: "the report is clean and not yet extended: it changed since the last automatic extension (a policy revision or a dismissed exception) and is evaluated at the next policy adoption, statement ingest or first account reading of the day"}
	return status
}
