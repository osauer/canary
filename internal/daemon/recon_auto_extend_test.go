package daemon

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/risk"
	"github.com/osauer/canary/v3/internal/rpc"
)

// cleanReconReport is a current, clean, same-day report inside tolerance,
// generated an hour ago: the report that extends the clock by itself.
func cleanReconReport(now time.Time) *rpc.ReconResult {
	div := 0.1
	return &rpc.ReconResult{
		AsOf: now, Status: rpc.ReconStatusActive, ReportID: "report-1",
		StatementAsOf: now.Add(-time.Hour), CoverageTo: utcDate(now.Year(), now.Month(), now.Day()),
		Equity:      &rpc.ReconEquityCheck{StatementDate: utcDate(now.Year(), now.Month(), now.Day()), SameDay: true, Comparability: "comparable", DivergencePct: &div},
		InputHealth: []rpc.SourceHealth{{Source: "statements", Status: "ok"}},
		Fetch:       rpc.ReconFetchStatus{Configured: true, State: rpc.ReconReportStateCurrent},
	}
}

// Recon status names what holds a clean report back from extending the
// clock, with the evaluator's own judgement, so a stale statement over a
// long weekend, a policy that is not active or a report whose extension
// failed to record never read as the bare evaluation_failed they did before
// 2026-10-18. Every status it produces passes the contract's validator.
func TestReconAutomationStatusNamesTheHold(t *testing.T) {
	s := newReconV3TestServer(t)
	now := time.Date(2026, 10, 18, 14, 0, 0, 0, time.UTC)
	pol := s.riskPolicies.snapshot().policy
	check := func(t *testing.T, rep *rpc.ReconResult, state, reason, detail string) {
		t.Helper()
		status := s.reconAutomationStatus(rep, now)
		if err := rpc.ValidateReconAutomationStatus(status); err != nil {
			t.Fatalf("invalid status %+v: %v", status, err)
		}
		if status.Evaluation.State != state || status.Evaluation.Reason != reason || !strings.Contains(status.Evaluation.Detail, detail) {
			t.Fatalf("evaluation = %+v, want %s/%s with %q", status.Evaluation, state, reason, detail)
		}
	}
	t.Run("clean and extended is complete", func(t *testing.T) {
		rep := cleanReconReport(now)
		rep.LastAutoExtendReportID = rep.ReportID
		check(t, rep, rpc.ReconEvaluationStateComplete, rpc.ReconEvaluationReasonNone, "")
	})
	t.Run("clean and not yet extended waits for the evaluator", func(t *testing.T) {
		check(t, cleanReconReport(now), rpc.ReconEvaluationStateWaiting, rpc.ReconEvaluationReasonEvaluationPending, "changed since the last automatic extension")
	})
	t.Run("a statement past max_report_age_days is stale, not failed", func(t *testing.T) {
		rep := cleanReconReport(now)
		rep.StatementAsOf = now.Add(-time.Duration(*pol.Recon.MaxReportAgeDays)*24*time.Hour - time.Hour)
		check(t, rep, rpc.ReconEvaluationStateAttentionRequired, rpc.ReconEvaluationReasonReportStale, "older than recon.max_report_age_days")
		if autoExtendEligible(rpc.RiskPolicyStatusActive, pol, rep, now) {
			t.Fatal("a stale report was eligible")
		}
	})
	t.Run("the paired statement day past max_report_age_days is stale too", func(t *testing.T) {
		rep := cleanReconReport(now)
		rep.Equity.StatementDate = rep.Equity.StatementDate.AddDate(0, 0, -*pol.Recon.MaxReportAgeDays-1)
		check(t, rep, rpc.ReconEvaluationStateAttentionRequired, rpc.ReconEvaluationReasonReportStale, "statement day the account equity pairs with")
	})
	t.Run("a degraded report says a statement could not be read", func(t *testing.T) {
		rep := cleanReconReport(now)
		rep.Status = rpc.ReconStatusDegraded
		rep.InputHealth = []rpc.SourceHealth{{Source: "statements", Status: "degraded"}}
		check(t, rep, rpc.ReconEvaluationStateFailed, rpc.ReconEvaluationReasonEvaluationFailed, "could not be read")
	})
	t.Run("exceptions, a missing pair and a divergence keep their reasons", func(t *testing.T) {
		rep := cleanReconReport(now)
		rep.Unresolved = 2
		check(t, rep, rpc.ReconEvaluationStateAttentionRequired, rpc.ReconEvaluationReasonExceptionsNeedReview, "2 unresolved exceptions")
		rep = cleanReconReport(now)
		rep.Equity.SameDay = false
		check(t, rep, rpc.ReconEvaluationStateWaiting, rpc.ReconEvaluationReasonAccountValuePending, "valuation evidence")
		rep = cleanReconReport(now)
		far := *pol.Recon.MaxEquityDivergencePct + 1
		rep.Equity.DivergencePct = &far
		check(t, rep, rpc.ReconEvaluationStateAttentionRequired, rpc.ReconEvaluationReasonAccountValueMismatch, "differ by more than")
	})
	t.Run("an extension that failed to record is a failure with its cause", func(t *testing.T) {
		rep := cleanReconReport(now)
		s.reconEvaluations.record(rep.ReportID, "the automatic extension could not be recorded in the capital journal; the daemon log carries the error")
		check(t, rep, rpc.ReconEvaluationStateFailed, rpc.ReconEvaluationReasonEvaluationFailed, "could not be recorded")
		s.reconEvaluations.record(rep.ReportID, "")
		check(t, rep, rpc.ReconEvaluationStateWaiting, rpc.ReconEvaluationReasonEvaluationPending, "")
	})
	t.Run("a policy that is not active is named", func(t *testing.T) {
		hold, held := autoExtendHoldFor(rpc.RiskPolicyStatusDrift, pol, cleanReconReport(now), now)
		if !held || hold.Reason != rpc.ReconEvaluationReasonPolicyUnapproved || !strings.Contains(hold.Detail, "drift") {
			t.Fatalf("drift policy hold = %+v", hold)
		}
		if _, held := autoExtendHoldFor(rpc.RiskPolicyStatusActive, pol, cleanReconReport(now), now); held {
			t.Fatal("a clean report under an active policy was held")
		}
	})
}

// A revised constitution gives the report a new id, so the manager tells the
// daemon to judge it again at once; the first load and an unchanged file do
// not.
func TestRiskPolicyRevisionTriggersReconEvaluation(t *testing.T) {
	m, path := newTestRiskPolicyManager(t, validRiskPolicyV3TOML())
	revisions := 0
	m.onRevision = func(c *risk.Constitution) {
		if c == nil || c.PolicyVersion != 4 {
			t.Fatalf("revision adopted %+v", c)
		}
		revisions++
	}
	m.reload()
	if revisions != 0 {
		t.Fatalf("an unchanged file counted as a revision")
	}
	v4 := strings.Replace(validRiskPolicyV3TOML(), "policy_version = 3", "policy_version = 4", 1)
	if err := os.WriteFile(path, []byte(v4), 0o600); err != nil {
		t.Fatal(err)
	}
	m.reload()
	m.reload()
	if revisions != 1 {
		t.Fatalf("revisions = %d, want one for one adopted change", revisions)
	}
}
