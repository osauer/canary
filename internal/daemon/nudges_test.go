package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

func validRiskPolicyV4TOML() string {
	v4 := strings.Replace(validRiskPolicyV3TOML(), "policy_version = 3", "policy_version = 4", 1)
	return v4 + `

[cadence.nudges]
timezone = "Europe/Berlin"
reconcile_warning_days = 2

[cadence.monthly]
class = "advisory"
day_of_month = 1
nudge_at_local = "09:00"

[inventory.rulebook]
id = "rulebook-v2"
version = "2"

[inventory.protection]
id = "protection-mvp"
version = "1"

[inventory.stress]
id = "active-v1"
version = "risk-policy-v1"
`
}

func validRiskPolicyV3WithPinsTOML() string {
	return validRiskPolicyV3TOML() + `

[inventory.rulebook]
id = "rulebook-v2"
version = "2"

[inventory.protection]
id = "protection-mvp"
version = "1"

[inventory.stress]
id = "active-v1"
version = "risk-policy-v1"
`
}

func incompleteCadenceRiskPolicyV4TOML() string {
	v4 := validRiskPolicyV4TOML()
	v4 = strings.Replace(v4, "reconcile_warning_days = 2\n", "", 1)
	return strings.Replace(v4, "day_of_month = 1\n", "", 1)
}

func newV4NudgeTestServer(t *testing.T, now time.Time) *Server {
	t.Helper()
	return newNudgeAuthorityTestServer(t, now, validRiskPolicyV4TOML())
}

func newNudgeAuthorityTestServer(t *testing.T, now time.Time, policyTOML string) *Server {
	t.Helper()
	s := newRiskPolicyTestServer(t, policyTOML)
	s.now = func() time.Time { return now }
	s.riskCapital.now = s.now
	if s.riskPolicies != nil {
		s.riskPolicies.mu.Lock()
		s.riskPolicies.now = s.now
		s.riskPolicies.loadedAt = now
		s.riskPolicies.lastCheckedAt = now
		s.riskPolicies.mu.Unlock()
	}
	s.installNudgeStateStore()
	pm := newProtectionPolicyManager("", false, time.Second, s.now)
	pm.reload()
	s.protectionPolicies = pm
	return s
}

func primeNudgeBlockEpisode(s *Server, now time.Time, consumedKnown bool) {
	s.riskCapital.mu.Lock()
	s.riskCapital.loadLocked()
	s.riskCapital.state.Seeded = true
	s.riskCapital.state.AdjustedPeakBase = 260000
	if consumedKnown {
		s.riskCapital.state.LastEquityBase = 240000
		s.riskCapital.state.LastEquityAsOf = now
	} else {
		s.riskCapital.state.LastEquityBase = 0
		s.riskCapital.state.LastEquityAsOf = time.Time{}
	}
	s.riskCapital.state.BlockLatched = true
	s.riskCapital.state.LatchedAt = now.Add(-2 * time.Hour)
	s.riskCapital.state.LatchEpisodeSeq = 1
	s.riskCapital.mu.Unlock()
}

func primeIndependentNudgeFacts(t *testing.T, s *Server, now time.Time, withReconciliation bool) {
	t.Helper()
	primeNudgeBlockEpisode(s, now, true)
	stockBuy := rpc.OrderDraft{Action: "BUY", Contract: rpc.ContractParams{Symbol: "MSFT", SecType: "STK"}}
	if warnings := s.riskPolicyPreviewWarnings(stockBuy, rpc.OrderPositionImpact{Effect: "open"}); len(warnings) != 1 {
		t.Fatalf("prime shadow preview warnings=%+v", warnings)
	}
	s.protectionPolicies.mu.Lock()
	s.protectionPolicies.status.PolicyVersion++
	s.protectionPolicies.mu.Unlock()
	if withReconciliation {
		day := now.Format("20060102")
		writeFlexFixture(t, "independent-nudge-exception.xml", now.Format("20060102")+";090000", day, day,
			cashLine("independent-nudge-exception", "Unknown Broker Instruction", 12, day))
	}
}

func assertNudgeKinds(t *testing.T, candidates []rpc.NudgeCandidate, present, absent []string) {
	t.Helper()
	for _, kind := range present {
		if !candidateKindPresent(candidates, kind) {
			t.Fatalf("candidates=%+v, missing %s", candidates, kind)
		}
	}
	for _, kind := range absent {
		if candidateKindPresent(candidates, kind) {
			t.Fatalf("candidates=%+v, unexpected %s", candidates, kind)
		}
	}
}

func TestNudgeIndependentFactsSurviveV3AndIncompleteV4(t *testing.T) {
	now := time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name                string
		policyTOML          string
		wantCadenceStatus   string
		wantConfirmedStatus string
		wantAggregate       string
		observeCutover      bool
	}{
		{
			name: "valid current v3", policyTOML: validRiskPolicyV3WithPinsTOML(),
			wantCadenceStatus: rpc.NudgeInputStatusInactive, wantConfirmedStatus: rpc.NudgeInputStatusInactive,
			wantAggregate: rpc.NudgeAggregateReady,
		},
		{
			// Cadence keys default in code, so an incompletely authored
			// cadence stays healthy; only the unreviewed cutover degrades.
			name: "incomplete v4 cadence", policyTOML: incompleteCadenceRiskPolicyV4TOML(),
			wantCadenceStatus: rpc.NudgeInputStatusOK, wantConfirmedStatus: rpc.NudgeInputStatusUnapproved,
			wantAggregate:  rpc.NudgeAggregateDegraded,
			observeCutover: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newNudgeAuthorityTestServer(t, now, tt.policyTOML)
			primeIndependentNudgeFacts(t, s, now, true)
			if tt.observeCutover {
				report := s.buildReconReport()
				s.observeConfirmedFlows(nudgeConfirmedFlowSnapshot{
					PolicyVersion: 4, PolicyIdentity: nudgePolicyIdentity(s.riskPolicies.snapshot().policy),
					ReportStatus: report.Status, ReportIdentity: opaqueIdentity("recon-report", report.ReportID),
					StatementAsOf: report.StatementAsOf, StatementsHealthy: true,
				})
				if coverage, _, ok := s.nudges.confirmedSnapshot(nil); !ok || coverage == nil || !coverage.PreCutoverFlowsUnreviewed {
					t.Fatalf("incomplete cadence suppressed event-driven cutover coverage: coverage=%+v ok=%v", coverage, ok)
				}
			}

			beforeTree := stateTree(t, os.Getenv("XDG_STATE_HOME"))
			beforeContents := stateContents(t, os.Getenv("XDG_STATE_HOME"))
			s.nudges.mu.Lock()
			beforeShadowCount := s.nudges.state.Shadow.Count
			s.nudges.mu.Unlock()
			var first *rpc.NudgesSnapshotResult
			for range 2 {
				result, err := s.handleNudgesSnapshot(context.Background(), &rpc.Request{})
				if err != nil {
					t.Fatal(err)
				}
				assertNudgeKinds(t, result.Candidates,
					[]string{rpc.NudgeKindPolicyDrift, rpc.NudgeKindDrawdownLatched, rpc.NudgeKindShadowWouldBlock, rpc.NudgeKindReconcileException},
					[]string{rpc.NudgeKindReconcileDue, rpc.NudgeKindMonthlyPulse, rpc.NudgeKindConfirmedFlow})
				if result.SourceHealth.Policy.Status != rpc.NudgeInputStatusOK || result.SourceHealth.Cadence.Status != tt.wantCadenceStatus ||
					result.SourceHealth.ConfirmedFlow.Status != tt.wantConfirmedStatus || result.SourceHealth.Aggregate != tt.wantAggregate {
					t.Fatalf("dependency-isolated source health=%+v", result.SourceHealth)
				}
				if first == nil {
					first = result
				} else if !reflect.DeepEqual(first, result) {
					t.Fatalf("repeated snapshot changed: first=%+v second=%+v", first, result)
				}
			}
			if afterTree := stateTree(t, os.Getenv("XDG_STATE_HOME")); !slices.Equal(beforeTree, afterTree) {
				t.Fatalf("repeated snapshot changed state tree: before=%v after=%v", beforeTree, afterTree)
			}
			if afterContents := stateContents(t, os.Getenv("XDG_STATE_HOME")); !maps.Equal(beforeContents, afterContents) {
				t.Fatalf("repeated snapshot changed state contents: before=%v after=%v", beforeContents, afterContents)
			}
			s.nudges.mu.Lock()
			afterShadowCount := s.nudges.state.Shadow.Count
			s.nudges.mu.Unlock()
			if beforeShadowCount != 1 || afterShadowCount != beforeShadowCount {
				t.Fatalf("snapshot advanced shadow bookkeeping: before=%d after=%d", beforeShadowCount, afterShadowCount)
			}
		})
	}
}

func TestNudgeSourceOutagesOnlySuppressDependentFacts(t *testing.T) {
	now := time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC)
	t.Run("reconciliation unavailable", func(t *testing.T) {
		s := newNudgeAuthorityTestServer(t, now, validRiskPolicyV4TOML())
		primeIndependentNudgeFacts(t, s, now, false)
		result, err := s.handleNudgesSnapshot(context.Background(), &rpc.Request{})
		if err != nil {
			t.Fatal(err)
		}
		assertNudgeKinds(t, result.Candidates,
			[]string{rpc.NudgeKindPolicyDrift, rpc.NudgeKindDrawdownLatched, rpc.NudgeKindShadowWouldBlock},
			[]string{rpc.NudgeKindReconcileException})
		if result.SourceHealth.Reconciliation.Status != rpc.NudgeInputStatusUnavailable || result.SourceHealth.Aggregate != rpc.NudgeAggregateDegraded {
			t.Fatalf("reconciliation outage health=%+v", result.SourceHealth)
		}
	})

	t.Run("nudge store unavailable", func(t *testing.T) {
		s := newNudgeAuthorityTestServer(t, now, validRiskPolicyV4TOML())
		primeIndependentNudgeFacts(t, s, now, true)
		s.nudges.mu.Lock()
		s.nudges.fault = true
		s.nudges.mu.Unlock()
		result, err := s.handleNudgesSnapshot(context.Background(), &rpc.Request{})
		if err != nil {
			t.Fatal(err)
		}
		assertNudgeKinds(t, result.Candidates,
			[]string{rpc.NudgeKindPolicyDrift, rpc.NudgeKindDrawdownLatched, rpc.NudgeKindReconcileException},
			[]string{rpc.NudgeKindShadowWouldBlock, rpc.NudgeKindConfirmedFlow})
		if result.SourceHealth.ConfirmedFlow.Status != rpc.NudgeInputStatusError || result.SourceHealth.Aggregate != rpc.NudgeAggregateDegraded {
			t.Fatalf("nudge-store outage health=%+v", result.SourceHealth)
		}
	})

	t.Run("current capital magnitude unavailable", func(t *testing.T) {
		s := newNudgeAuthorityTestServer(t, now, validRiskPolicyV4TOML())
		primeIndependentNudgeFacts(t, s, now, true)
		s.riskCapital.mu.Lock()
		s.riskCapital.state.LastEquityBase = 0
		s.riskCapital.state.LastEquityAsOf = time.Time{}
		s.riskCapital.mu.Unlock()
		result, err := s.handleNudgesSnapshot(context.Background(), &rpc.Request{})
		if err != nil {
			t.Fatal(err)
		}
		assertNudgeKinds(t, result.Candidates,
			[]string{rpc.NudgeKindPolicyDrift, rpc.NudgeKindDrawdownLatched, rpc.NudgeKindShadowWouldBlock, rpc.NudgeKindReconcileException}, nil)
		if result.SourceHealth.Capital.Status != rpc.NudgeInputStatusUnavailable || result.Context == nil || result.Context.Drawdown == nil || result.Context.Drawdown.ConsumedPct != nil {
			t.Fatalf("unavailable capital magnitude hid durable facts: health=%+v context=%+v", result.SourceHealth, result.Context)
		}
	})
}

func TestConfirmedFlowCutoverIsIndependentOfIncompleteTimedCadence(t *testing.T) {
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	s, _ := newCutoverReviewTestServerWithPolicy(t, now, incompleteCadenceRiskPolicyV4TOML())
	authority := s.currentNudgeAuthority(now)
	if !authority.eligible || !authority.confirmedFlowEligible || !authority.cadenceEligible {
		t.Fatalf("cadence defaults left authority ineligible=%+v", authority)
	}
	result, err := s.handleNudgesCutoverReview(context.Background(), cutoverReviewRequest(t))
	if err != nil || result == nil || !result.OK {
		t.Fatalf("event-driven cutover was gated by timed cadence: result=%+v err=%v", result, err)
	}
	snapshot, err := s.handleNudgesSnapshot(context.Background(), &rpc.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.SourceHealth.Cadence.Status != rpc.NudgeInputStatusOK || snapshot.SourceHealth.ConfirmedFlow.Status != rpc.NudgeInputStatusOK {
		t.Fatalf("cutover/cadence source isolation=%+v", snapshot.SourceHealth)
	}
}

func TestGovernanceAuthorityGatesCandidatesShadowAndMonthly(t *testing.T) {
	// Monday, August's first working day, past the 09:00 Berlin due instant,
	// so wantMonthly asserts presence gating rather than not-due absence.
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name            string
		policyTOML      string
		mutate          func(*Server)
		wantStatus      string
		wantReason      string
		wantIndependent bool
		wantMonthly     bool
	}{
		{name: "active approved v4", policyTOML: validRiskPolicyV4TOML(), wantStatus: rpc.NudgeInputStatusOK, wantIndependent: true, wantMonthly: true},
		{name: "v3", policyTOML: validRiskPolicyV3TOML(), wantStatus: rpc.NudgeInputStatusOK, wantIndependent: true},
		{name: "inactive", policyTOML: validRiskPolicyV4TOML(), mutate: func(s *Server) { s.riskPolicies.status = rpc.RiskPolicyStatusAbsent }, wantStatus: rpc.NudgeInputStatusUnavailable, wantReason: rpc.NudgeHealthReasonSourceUnavailable},
		{name: "cadence defaults absorb missing keys", policyTOML: validRiskPolicyV4TOML(), mutate: func(s *Server) {
			s.riskPolicies.active.Cadence.Monthly.DayOfMonth = nil
			s.riskPolicies.lastFingerprint = s.riskPolicies.active.FingerprintKey()
		}, wantStatus: rpc.NudgeInputStatusOK, wantIndependent: true, wantMonthly: true},
		{name: "error", policyTOML: validRiskPolicyV4TOML(), mutate: func(s *Server) { s.riskPolicies.status = rpc.RiskPolicyStatusError }, wantStatus: rpc.NudgeInputStatusError, wantReason: rpc.NudgeHealthReasonEvaluationError},
		{name: "unavailable", policyTOML: validRiskPolicyV4TOML(), mutate: func(s *Server) { s.riskPolicies.source = "" }, wantStatus: rpc.NudgeInputStatusUnavailable, wantReason: rpc.NudgeHealthReasonSourceUnavailable},
		{name: "stale", policyTOML: validRiskPolicyV4TOML(), mutate: func(s *Server) { s.riskPolicies.lastCheckedAt = now.Add(-2 * time.Minute) }, wantStatus: rpc.NudgeInputStatusStale, wantReason: rpc.NudgeHealthReasonEvidenceStale},
		{name: "drifted", policyTOML: validRiskPolicyV4TOML(), mutate: func(s *Server) { s.riskPolicies.status = rpc.RiskPolicyStatusDrift }, wantStatus: rpc.NudgeInputStatusUnapproved, wantReason: rpc.NudgeHealthReasonPolicyUnapproved},
		{name: "internally inconsistent", policyTOML: validRiskPolicyV4TOML(), mutate: func(s *Server) { s.riskPolicies.active.Kind = "wrong.kind" }, wantStatus: rpc.NudgeInputStatusError, wantReason: rpc.NudgeHealthReasonEvaluationError},
	}
	stockBuy := rpc.OrderDraft{Action: "BUY", Contract: rpc.ContractParams{Symbol: "MSFT", SecType: "STK"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newNudgeAuthorityTestServer(t, now, tt.policyTOML)
			primeNudgeBlockEpisode(s, now, true)
			if tt.mutate != nil {
				s.riskPolicies.mu.Lock()
				tt.mutate(s)
				s.riskPolicies.mu.Unlock()
			}
			warnings := s.riskPolicyPreviewWarnings(stockBuy, rpc.OrderPositionImpact{Effect: "open"})
			if tt.wantIndependent && len(warnings) != 1 {
				t.Fatalf("independent policy warnings=%+v", warnings)
			}
			result, err := s.handleNudgesSnapshot(context.Background(), &rpc.Request{})
			if err != nil {
				t.Fatal(err)
			}
			if result.SourceHealth.Policy.Status != tt.wantStatus || result.SourceHealth.Policy.Reason != tt.wantReason {
				t.Fatalf("policy health=%+v, want %s/%s", result.SourceHealth.Policy, tt.wantStatus, tt.wantReason)
			}
			if tt.wantIndependent {
				for _, kind := range []string{rpc.NudgeKindShadowWouldBlock, rpc.NudgeKindDrawdownLatched} {
					if !candidateKindPresent(result.Candidates, kind) {
						t.Fatalf("independent candidates=%+v, missing %s", result.Candidates, kind)
					}
				}
				if candidateKindPresent(result.Candidates, rpc.NudgeKindMonthlyPulse) != tt.wantMonthly {
					t.Fatalf("monthly candidate present=%v, want %v; candidates=%+v", candidateKindPresent(result.Candidates, rpc.NudgeKindMonthlyPulse), tt.wantMonthly, result.Candidates)
				}
				if result.Context == nil || result.Context.Shadow == nil || result.Context.Shadow.Count != 1 || result.Context.Drawdown == nil {
					t.Fatalf("active context=%+v", result.Context)
				}
				if tt.name == "v3" {
					if result.SourceHealth.Cadence.Status != rpc.NudgeInputStatusInactive || result.SourceHealth.ConfirmedFlow.Status != rpc.NudgeInputStatusInactive {
						t.Fatalf("v3 reminder sources=%+v, want explicitly inactive", result.SourceHealth)
					}
				}
				if tt.name == "cadence defaults absorb missing keys" && result.SourceHealth.Cadence.Status != rpc.NudgeInputStatusOK {
					t.Fatalf("cadence health=%+v, want ok under code defaults", result.SourceHealth.Cadence)
				}
				return
			}
			if len(result.Candidates) != 0 || result.Context != nil || result.IsCleanEmpty() {
				t.Fatalf("blocked result candidates=%+v context=%+v clean=%v", result.Candidates, result.Context, result.IsCleanEmpty())
			}
			s.nudges.mu.Lock()
			count := s.nudges.state.Shadow.Count
			monthly := len(s.nudges.state.MonthlyCompletions)
			s.nudges.mu.Unlock()
			if count != 0 || monthly != 0 {
				t.Fatalf("blocked authority advanced state: shadow=%d monthly=%d", count, monthly)
			}
		})
	}
}

func TestShadowOccurrenceUsesFirstPreviewTimeNotLatchTime(t *testing.T) {
	latchTime := time.Date(2026, 8, 2, 8, 0, 0, 0, time.UTC)
	previewTime := latchTime.Add(90 * time.Minute)
	s := newV4NudgeTestServer(t, previewTime)
	primeNudgeBlockEpisode(s, latchTime.Add(2*time.Hour), true)
	s.riskCapital.mu.Lock()
	s.riskCapital.state.LatchedAt = latchTime
	s.riskCapital.state.LastEquityAsOf = previewTime
	s.riskCapital.mu.Unlock()
	stockBuy := rpc.OrderDraft{Action: "BUY", Contract: rpc.ContractParams{Symbol: "MSFT", SecType: "STK"}}
	if got := s.riskPolicyPreviewWarnings(stockBuy, rpc.OrderPositionImpact{Effect: "open"}); len(got) != 1 {
		t.Fatalf("preview warnings=%+v", got)
	}
	result, err := s.handleNudgesSnapshot(context.Background(), &rpc.Request{})
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range result.Candidates {
		if candidate.Kind == rpc.NudgeKindShadowWouldBlock {
			if !candidate.OccurredAt.Equal(previewTime) {
				t.Fatalf("shadow occurred_at=%s, want first preview %s (latch %s)", candidate.OccurredAt, previewTime, latchTime)
			}
			return
		}
	}
	t.Fatal("missing shadow candidate")
}

func TestNudgeSnapshotContextKnownUnknownAndValidatorCorrelation(t *testing.T) {
	now := time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC)
	known := newV4NudgeTestServer(t, now)
	primeNudgeBlockEpisode(known, now, true)
	knownResult := known.composeNudgesSnapshot()
	if knownResult.Context == nil || knownResult.Context.Drawdown == nil || knownResult.Context.Drawdown.ConsumedPct == nil {
		t.Fatalf("known drawdown context=%+v", knownResult.Context)
	}
	if _, err := json.Marshal(knownResult); err != nil {
		t.Fatalf("known context rejected: %v", err)
	}

	unknown := newV4NudgeTestServer(t, now)
	primeNudgeBlockEpisode(unknown, now, false)
	unknownResult := unknown.composeNudgesSnapshot()
	if unknownResult.Context == nil || unknownResult.Context.Drawdown == nil || unknownResult.Context.Drawdown.ConsumedPct != nil {
		t.Fatalf("unknown drawdown context=%+v, want explicit nil magnitude", unknownResult.Context)
	}
	wire, err := json.Marshal(unknownResult)
	if err != nil {
		t.Fatalf("unknown context rejected: %v", err)
	}
	if !strings.Contains(string(wire), `"consumed_pct":null`) {
		t.Fatalf("unknown magnitude was not JSON null: %s", wire)
	}

	inconsistent := knownResult
	inconsistent.Context = nil
	if _, err := json.Marshal(inconsistent); err == nil || !strings.Contains(err.Error(), "drawdown summary") {
		t.Fatalf("inconsistent candidate/context error=%v", err)
	}
}

func TestCapitalReportAndLatchAreCapturedFromOneGeneration(t *testing.T) {
	now := time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC)
	s := newV4NudgeTestServer(t, now)
	primeNudgeBlockEpisode(s, now, true)
	policy := s.riskPolicies.snapshot().policy
	entered := make(chan struct{})
	release := make(chan struct{})
	s.riskCapital.nudgeCaptureHook = func() {
		s.riskCapital.nudgeCaptureHook = nil
		close(entered)
		<-release
	}
	authorityCh := make(chan nudgeAuthorityState, 1)
	go func() { authorityCh <- s.currentNudgeAuthority(now) }()
	<-entered
	resetDone := make(chan error, 1)
	go func() { resetDone <- s.riskCapital.ResetDrawdown("atomic capture test", policy) }()
	select {
	case err := <-resetDone:
		t.Fatalf("reset crossed locked capture: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	authority := <-authorityCh
	if err := <-resetDone; err != nil {
		t.Fatal(err)
	}
	if !authority.report.Capital.BlockLatched || !authority.capitalNudge.LatchOpen || authority.capitalNudge.Episode == "" || authority.report.Capital.ConsumedPct == nil {
		t.Fatalf("captured mixed capital/latch generation: capital=%+v latch=%+v", authority.report.Capital, authority.capitalNudge)
	}
	after := s.currentNudgeAuthority(now)
	if after.report.Capital.BlockLatched || after.capitalNudge.LatchOpen || after.capitalNudge.Episode != "" {
		t.Fatalf("post-reset capture retained old latch: capital=%+v latch=%+v", after.report.Capital, after.capitalNudge)
	}
}

func TestNudgeSnapshotAuthoritativeCleanEmptyHasNoContext(t *testing.T) {
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	s, _ := newCutoverReviewTestServer(t, now)
	s.riskCapital.mu.Lock()
	s.riskCapital.loadLocked()
	s.riskCapital.state.Seeded = true
	s.riskCapital.state.AdjustedPeakBase = 250000
	s.riskCapital.state.LastEquityBase = 250000
	s.riskCapital.state.LastEquityAsOf = now
	s.riskCapital.lastReconciledAt = now
	s.riskCapital.mu.Unlock()
	if _, err := s.handleNudgesCutoverReview(context.Background(), cutoverReviewRequest(t)); err != nil {
		t.Fatal(err)
	}
	rendered, _ := s.composeBrief(context.Background())
	if _, err := s.handleBriefAck(context.Background(), rawParams(t, rpc.BriefAckParams{
		Kind: rpc.BriefKindMonthly, Month: "2026-08", Evidence: rpc.BriefAckEvidenceRender,
		BriefFingerprint: rendered.BriefFingerprint, Origin: rpc.OrderOriginPairedDevice,
	})); err != nil {
		t.Fatal(err)
	}
	result, err := s.handleNudgesSnapshot(context.Background(), &rpc.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != 0 || result.Context != nil || !result.IsCleanEmpty() || result.SourceHealth.Aggregate != rpc.NudgeAggregateReady {
		t.Fatalf("authoritative clean empty candidates=%+v context=%+v health=%+v clean=%v", result.Candidates, result.Context, result.SourceHealth, result.IsCleanEmpty())
	}
}

func TestNudgesSnapshotAssemblesTypedTriggersAndRedactsHostileSources(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	s := newV4NudgeTestServer(t, now)
	day := now.Format("20060102")
	sentinel := "HOSTILE_U1234567_ORDER_99_TOKEN_https://evil.example"
	body := strings.Replace(cashLine("flow-private", "Deposits/Withdrawals", 250, day), "FIXTURE", sentinel, 1) + "\n" +
		strings.Replace(cashLine("exception-private", "Unknown Broker Instruction", 12, day), "FIXTURE", sentinel, 1)
	writeFlexFixture(t, "flex-nudges.xml", now.Add(-time.Minute).Format("20060102;150405"), day, day, body)
	report := s.buildReconReport()
	if len(report.Confirmed) != 1 || report.Unresolved != 1 {
		t.Fatalf("fixture report confirmed=%d unresolved=%d", len(report.Confirmed), report.Unresolved)
	}

	s.riskCapital.mu.Lock()
	s.riskCapital.loadLocked()
	s.riskCapital.state.Seeded = true
	s.riskCapital.state.LastEquityBase = 240000
	s.riskCapital.state.LastEquityAsOf = now
	s.riskCapital.state.BlockLatched = true
	s.riskCapital.state.LatchedAt = now.Add(-3 * time.Hour)
	s.riskCapital.lastReconciledAt = now.Add(-6 * 24 * time.Hour)
	s.riskCapital.mu.Unlock()

	policyIdentity := nudgePolicyIdentity(s.riskPolicies.snapshot().policy)
	_, episode, _ := s.riskCapital.NudgeLatch()
	if err := s.nudges.recordShadow(policyIdentity, episode, true, false, true); err != nil {
		t.Fatal(err)
	}
	currentRows := []string{confirmedFlowContentIdentity(report.Confirmed[0])}
	if err := s.nudges.observeConfirmedFlows(nudgeConfirmedFlowSnapshot{
		PolicyVersion: 4, ReportIdentity: opaqueIdentity("report", "cutover"),
	}); err != nil {
		t.Fatal(err)
	}
	governanceIdentity := nudgeAuthorityToken(s.currentNudgeAuthority(now))
	if _, _, err := s.nudges.reviewConfirmedCutover(nudgeCutoverReviewEvidence{
		ReviewedAt: now, PolicyIdentity: policyIdentity, PolicyVersion: 4,
		ReportIdentity: opaqueIdentity("report", "cutover"), ConfirmedRows: 0, ReviewedRows: []string{},
		GovernanceIdentity: governanceIdentity,
		AuthorityIdentity:  cutoverAuthorityIdentity(governanceIdentity, opaqueIdentity("report", "cutover"), time.Time{}, nil),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.nudges.observeConfirmedFlows(nudgeConfirmedFlowSnapshot{
		PolicyVersion: 4, ReportIdentity: opaqueIdentity("report", "current"), ConfirmedRows: currentRows,
	}); err != nil {
		t.Fatal(err)
	}

	before := stateTree(t, os.Getenv("XDG_STATE_HOME"))
	beforeContents := stateContents(t, os.Getenv("XDG_STATE_HOME"))
	result, err := s.handleNudgesSnapshot(context.Background(), &rpc.Request{})
	if err != nil {
		t.Fatal(err)
	}
	after := stateTree(t, os.Getenv("XDG_STATE_HOME"))
	if !slices.Equal(before, after) {
		t.Fatalf("snapshot changed state tree: before=%v after=%v", before, after)
	}
	if afterContents := stateContents(t, os.Getenv("XDG_STATE_HOME")); !maps.Equal(beforeContents, afterContents) {
		t.Fatalf("snapshot changed state or journal contents: before=%v after=%v", beforeContents, afterContents)
	}
	kinds := make(map[string]bool)
	for _, candidate := range result.Candidates {
		kinds[candidate.Kind] = true
	}
	for _, want := range []string{
		rpc.NudgeKindReconcileDue, rpc.NudgeKindReconcileException,
		rpc.NudgeKindShadowWouldBlock, rpc.NudgeKindDrawdownLatched,
		rpc.NudgeKindConfirmedFlow,
	} {
		if !kinds[want] {
			t.Fatalf("candidate kinds=%v, missing %s; health=%+v coverage=%+v", kinds, want, result.SourceHealth, result.ConfirmedFlowCoverage)
		}
	}
	wire, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	stateRaw, err := os.ReadFile(s.nudges.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{sentinel, "flow-private", "exception-private", "U1234567", "evil.example"} {
		if strings.Contains(string(wire), forbidden) {
			t.Fatalf("nudge wire leaked %q: %s", forbidden, wire)
		}
		if strings.Contains(string(stateRaw), forbidden) {
			t.Fatalf("nudge state leaked %q: %s", forbidden, stateRaw)
		}
	}
}

func TestNudgesSnapshotFailClosedHealthAndNoReadWrites(t *testing.T) {
	s := newRiskPolicyTestServer(t, validRiskPolicyV3TOML())
	s.installNudgeStateStore()
	root := os.Getenv("XDG_STATE_HOME")
	before := stateTree(t, root)
	beforeContents := stateContents(t, root)
	for range 3 {
		result, err := s.handleNudgesSnapshot(context.Background(), &rpc.Request{})
		if err != nil {
			t.Fatal(err)
		}
		if result.SourceHealth.Aggregate != rpc.NudgeAggregateSuppressed || result.IsCleanEmpty() {
			t.Fatalf("health=%+v clean=%v, want suppressed non-clean empty", result.SourceHealth, result.IsCleanEmpty())
		}
		if result.SourceHealth.ConfirmedFlow.Status == rpc.NudgeInputStatusOK {
			t.Fatal("v3 without cutover coverage reported confirmed-flow ready")
		}
	}
	after := stateTree(t, root)
	if !slices.Equal(before, after) {
		t.Fatalf("repeated snapshot created state: before=%v after=%v", before, after)
	}
	if afterContents := stateContents(t, root); !maps.Equal(beforeContents, afterContents) {
		t.Fatalf("repeated snapshot changed state contents: before=%v after=%v", beforeContents, afterContents)
	}
}

func stateContents(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("read state %s: %v", path, readErr)
		}
		out[rel] = string(raw)
		return nil
	})
	return out
}

func TestNudgesMonthlyRequiresMatchingPinsWhileReadableDriftStillTriggers(t *testing.T) {
	// 2026-08-01 is a Saturday; working day 1 of August is Monday the 3rd.
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	s := newV4NudgeTestServer(t, now)
	matching := s.composeNudgesSnapshot()
	if !candidateKindPresent(matching.Candidates, rpc.NudgeKindMonthlyPulse) {
		t.Fatalf("matching pins did not produce due monthly pulse: %+v", matching.Candidates)
	}

	s.protectionPolicies.mu.Lock()
	s.protectionPolicies.status.PolicyVersion++
	s.protectionPolicies.mu.Unlock()
	drifted := s.composeNudgesSnapshot()
	if !candidateKindPresent(drifted.Candidates, rpc.NudgeKindPolicyDrift) {
		t.Fatalf("readable drift did not produce policy candidate: %+v", drifted.Candidates)
	}
	if candidateKindPresent(drifted.Candidates, rpc.NudgeKindMonthlyPulse) {
		t.Fatalf("monthly pulse remained eligible under drift: %+v", drifted.Candidates)
	}
	if drifted.SourceHealth.Pins.Status != rpc.NudgeInputStatusOK {
		t.Fatalf("readable drift pin health=%+v, want ok evidence", drifted.SourceHealth.Pins)
	}
}

func TestNudgePinOutagePreservesIndependentCandidates(t *testing.T) {
	// Monday the 3rd, past the monthly due instant, so the absent monthly
	// pulse below is proven suppressed by the pin outage, not by not-due.
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	s := newV4NudgeTestServer(t, now)
	primeNudgeBlockEpisode(s, now, true)
	policyIdentity := nudgePolicyIdentity(s.riskPolicies.snapshot().policy)
	_, episode, _ := s.riskCapital.NudgeLatch()
	if err := s.nudges.recordShadow(policyIdentity, episode, true, false, true); err != nil {
		t.Fatal(err)
	}
	s.riskCapital.mu.Lock()
	s.riskCapital.lastReconciledAt = now.Add(-7 * 24 * time.Hour)
	s.riskCapital.mu.Unlock()
	s.protectionPolicies = nil
	day := now.Format("20060102")
	writeFlexFixture(t, "pin-outage-exception.xml", now.Format("20060102")+";090000", day, day,
		cashLine("pin-independent-exception", "Unknown Broker Instruction", 12, day))

	result, err := s.handleNudgesSnapshot(context.Background(), &rpc.Request{})
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{rpc.NudgeKindDrawdownLatched, rpc.NudgeKindShadowWouldBlock, rpc.NudgeKindReconcileDue, rpc.NudgeKindReconcileException} {
		if !candidateKindPresent(result.Candidates, kind) {
			t.Fatalf("pin outage hid independent %s candidate: candidates=%+v health=%+v", kind, result.Candidates, result.SourceHealth)
		}
	}
	if candidateKindPresent(result.Candidates, rpc.NudgeKindPolicyDrift) || candidateKindPresent(result.Candidates, rpc.NudgeKindMonthlyPulse) {
		t.Fatalf("pin-dependent candidate survived unreadable pin: %+v", result.Candidates)
	}
	if result.SourceHealth.Pins.Status != rpc.NudgeInputStatusUnavailable || result.SourceHealth.Aggregate != rpc.NudgeAggregateDegraded || result.IsCleanEmpty() {
		t.Fatalf("pin outage health=%+v clean=%v", result.SourceHealth, result.IsCleanEmpty())
	}
}

func TestConfirmedFlowObservationRequiresCurrentHealthyAuthorityAndReport(t *testing.T) {
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	row := opaqueIdentity("flow", "current")
	tests := []struct {
		name        string
		mutate      func(*Server, *nudgeConfirmedFlowSnapshot)
		wantObserve bool
	}{
		{name: "absent retained v4", mutate: func(s *Server, _ *nudgeConfirmedFlowSnapshot) { s.riskPolicies.status = rpc.RiskPolicyStatusAbsent }},
		{name: "error retained v4", mutate: func(s *Server, _ *nudgeConfirmedFlowSnapshot) { s.riskPolicies.status = rpc.RiskPolicyStatusError }},
		{name: "drift retained v4", mutate: func(s *Server, _ *nudgeConfirmedFlowSnapshot) { s.riskPolicies.status = rpc.RiskPolicyStatusDrift }},
		{name: "unapproved v4", mutate: func(s *Server, _ *nudgeConfirmedFlowSnapshot) { s.riskPolicies.active.Cadence.Monthly.DayOfMonth = nil }},
		{name: "stale statement", mutate: func(_ *Server, snap *nudgeConfirmedFlowSnapshot) { snap.StatementAsOf = now.Add(-10 * 24 * time.Hour) }},
		{name: "degraded report", mutate: func(_ *Server, snap *nudgeConfirmedFlowSnapshot) { snap.ReportStatus = rpc.ReconStatusDegraded }},
		{name: "unhealthy statements", mutate: func(_ *Server, snap *nudgeConfirmedFlowSnapshot) { snap.StatementsHealthy = false }},
		{name: "current healthy v4", wantObserve: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newV4NudgeTestServer(t, now)
			snap := nudgeConfirmedFlowSnapshot{
				PolicyVersion: 4, PolicyIdentity: nudgePolicyIdentity(s.riskPolicies.active),
				ReportStatus: rpc.ReconStatusActive, ReportIdentity: opaqueIdentity("report", tt.name),
				StatementAsOf: now, StatementsHealthy: true, ConfirmedRows: []string{row},
			}
			s.riskPolicies.mu.Lock()
			if tt.mutate != nil {
				tt.mutate(s, &snap)
			}
			s.riskPolicies.mu.Unlock()
			s.riskCapital.IncorporateStatementSnapshot(statementCapitalSnapshot{NudgeConfirmedFlows: snap})
			coverage, _, ok := s.nudges.confirmedSnapshot([]string{row})
			if ok != tt.wantObserve || (coverage != nil) != tt.wantObserve {
				t.Fatalf("coverage=%+v ok=%v want_observe=%v", coverage, ok, tt.wantObserve)
			}
		})
	}
}

func TestNudgesHandlersHonorCanceledContextBeforePersistence(t *testing.T) {
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	s, _ := newCutoverReviewTestServer(t, now)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.handleNudgesSnapshot(ctx, &rpc.Request{}); err == nil {
		t.Fatal("canceled snapshot unexpectedly succeeded")
	}
	if _, err := s.handleNudgesCutoverReview(ctx, cutoverReviewRequest(t)); err == nil {
		t.Fatal("canceled cutover review unexpectedly succeeded")
	}
	coverage, _, ok := s.nudges.confirmedSnapshot(nil)
	if !ok || coverage == nil || !coverage.PreCutoverFlowsUnreviewed {
		t.Fatalf("canceled cutover persisted review: %+v ok=%v", coverage, ok)
	}
}

func TestNudgesSnapshotCancelsDuringRetainedStatementScan(t *testing.T) {
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	s, _ := newCutoverReviewTestServer(t, now)
	ctx, cancel := context.WithCancel(context.Background())
	s.nudgeScanCheckpoint = func(stage string) {
		if stage == "retained_statement_file" {
			cancel()
		}
	}
	if _, err := s.handleNudgesSnapshot(ctx, &rpc.Request{}); err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("scan cancellation error=%v", err)
	}
}

func TestGovernanceWritesRevalidateAuthorityAtomically(t *testing.T) {
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name   string
		kind   string
		mutate func(*Server, string)
	}{
		{name: "monthly policy drift", kind: "monthly", mutate: func(s *Server, _ string) {
			s.riskPolicies.mu.Lock()
			s.riskPolicies.status = rpc.RiskPolicyStatusDrift
			s.riskPolicies.mu.Unlock()
		}},
		{name: "monthly pin change", kind: "monthly", mutate: func(s *Server, _ string) {
			s.protectionPolicies.mu.Lock()
			s.protectionPolicies.status.PolicyVersion++
			s.protectionPolicies.mu.Unlock()
		}},
		{name: "monthly report change", kind: "monthly", mutate: func(_ *Server, fixture string) {
			day := now.Format("20060102")
			writeFlexFixture(t, fixture, now.Format("20060102")+";090000", day, day,
				cashLine("cutover-flow", "Deposits/Withdrawals", 250, day)+"\n"+cashLine("monthly-raced-flow", "Deposits/Withdrawals", 50, day))
		}},
		{name: "cutover policy drift", kind: "cutover", mutate: func(s *Server, _ string) {
			s.riskPolicies.mu.Lock()
			s.riskPolicies.status = rpc.RiskPolicyStatusDrift
			s.riskPolicies.mu.Unlock()
		}},
		{name: "cutover pin change", kind: "cutover", mutate: func(s *Server, _ string) {
			s.protectionPolicies.mu.Lock()
			s.protectionPolicies.status.PolicyVersion++
			s.protectionPolicies.mu.Unlock()
		}},
		{name: "cutover report change", kind: "cutover", mutate: func(_ *Server, fixture string) {
			day := now.Format("20060102")
			writeFlexFixture(t, fixture, now.Format("20060102")+";090000", day, day,
				cashLine("cutover-flow", "Deposits/Withdrawals", 250, day)+"\n"+cashLine("raced-flow", "Deposits/Withdrawals", 50, day))
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, fixture := newCutoverReviewTestServer(t, now)
			s.nudgeBeforeCommit = func(kind string) {
				if kind == tt.kind {
					tt.mutate(s, fixture)
				}
			}
			if tt.kind == "monthly" {
				rendered, _ := s.composeBrief(context.Background())
				_, err := s.handleBriefAck(context.Background(), rawParams(t, rpc.BriefAckParams{
					Kind: rpc.BriefKindMonthly, Month: "2026-08", Evidence: rpc.BriefAckEvidenceRender,
					BriefFingerprint: rendered.BriefFingerprint, Origin: rpc.OrderOriginPairedDevice,
				}))
				if err == nil {
					t.Fatal("monthly write survived authority race")
				}
				if _, ok := s.nudges.monthlyCompletionRecord("2026-08", nudgePolicyIdentity(s.riskPolicies.active)); ok {
					t.Fatal("monthly evidence persisted after authority race")
				}
				return
			}
			if _, err := s.handleNudgesCutoverReview(context.Background(), cutoverReviewRequest(t)); err == nil {
				t.Fatal("cutover write survived authority race")
			}
			coverage, _, ok := s.nudges.confirmedSnapshot(nil)
			if !ok || coverage == nil || !coverage.PreCutoverFlowsUnreviewed {
				t.Fatalf("cutover evidence persisted after authority race: %+v", coverage)
			}
		})
	}
}

func TestCutoverCancellationDuringFinalRevalidationWritesNothing(t *testing.T) {
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	s, _ := newCutoverReviewTestServer(t, now)
	ctx, cancel := context.WithCancel(context.Background())
	s.nudgeBeforeCommit = func(kind string) {
		if kind == "cutover" {
			s.nudgeScanCheckpoint = func(stage string) {
				if stage == "recon_start" {
					cancel()
				}
			}
		}
	}
	if _, err := s.handleNudgesCutoverReview(ctx, cutoverReviewRequest(t)); err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("cutover final-scan cancellation error=%v", err)
	}
	coverage, _, ok := s.nudges.confirmedSnapshot(nil)
	if !ok || coverage == nil || !coverage.PreCutoverFlowsUnreviewed {
		t.Fatalf("canceled final revalidation persisted evidence: %+v", coverage)
	}
}

func candidateKindPresent(candidates []rpc.NudgeCandidate, kind string) bool {
	for _, candidate := range candidates {
		if candidate.Kind == kind {
			return true
		}
	}
	return false
}

func TestConfirmedFlowCutoverRevalidationRestatementAndRestart(t *testing.T) {
	now := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), governanceNudgeStateFile)
	st := &nudgeStateStore{path: path, now: func() time.Time { return now }}
	oldID := opaqueIdentity("flow", "old")
	newID := opaqueIdentity("flow", "new")
	restatedID := opaqueIdentity("flow", "restated")

	if err := st.observeConfirmedFlows(nudgeConfirmedFlowSnapshot{PolicyVersion: 4, ReportIdentity: opaqueIdentity("report", "cutover"), ConfirmedRows: []string{oldID}}); err != nil {
		t.Fatal(err)
	}
	coverage, events, ok := st.confirmedSnapshot([]string{oldID})
	if !ok || coverage == nil || !coverage.PreCutoverFlowsUnreviewed || len(events) != 0 {
		t.Fatalf("cutover coverage=%+v events=%+v ok=%v", coverage, events, ok)
	}
	// Every row incorporated before review belongs to the exact reviewed
	// baseline, including rows that arrived after coverage was first created.
	if err := st.observeConfirmedFlows(nudgeConfirmedFlowSnapshot{PolicyVersion: 4, ReportIdentity: opaqueIdentity("report", "incremental"), ConfirmedRows: []string{oldID, newID}}); err != nil {
		t.Fatal(err)
	}
	if _, events, _ := st.confirmedSnapshot([]string{oldID, newID}); len(events) != 0 {
		t.Fatalf("pre-review incremental rows became candidates: %+v", events)
	}
	evidence := nudgeCutoverReviewEvidence{
		ReviewedAt: now.Add(time.Minute), PolicyIdentity: opaqueIdentity("policy", "v4"), PolicyVersion: 4,
		ReportIdentity: opaqueIdentity("report", "incremental"), ConfirmedRows: 2, ReviewedRows: []string{oldID, newID},
	}
	if _, reviewed, err := st.reviewConfirmedCutover(evidence); err == nil || reviewed {
		t.Fatalf("future cutover review reviewed=%v err=%v", reviewed, err)
	}
	now = now.Add(time.Minute)
	evidence.ReviewedAt = now
	if _, already, err := st.reviewConfirmedCutover(evidence); err != nil || already {
		t.Fatalf("already=%v err=%v", already, err)
	}
	now = now.Add(2 * time.Minute)
	postReviewID := opaqueIdentity("flow", "post-review")
	if err := st.observeConfirmedFlows(nudgeConfirmedFlowSnapshot{PolicyVersion: 4, ReportIdentity: opaqueIdentity("report", "next"), ConfirmedRows: []string{oldID, newID, postReviewID}}); err != nil {
		t.Fatal(err)
	}
	coverage, events, _ = st.confirmedSnapshot([]string{oldID, newID, postReviewID})
	if coverage.PreCutoverFlowsUnreviewed || len(events) != 1 || events[0].ContentIdentity != postReviewID {
		t.Fatalf("post-cutover coverage=%+v events=%+v", coverage, events)
	}
	st.mu.Lock()
	anchoredReport := st.state.ConfirmedCoverage.ReportIdentity
	st.mu.Unlock()
	if anchoredReport != opaqueIdentity("report", "cutover") {
		t.Fatalf("cutover report anchor changed to %s", anchoredReport)
	}
	// A snapshot revalidation suppresses a row missing from current broker
	// truth without writing or resolving the durable event.
	if _, hidden, _ := st.confirmedSnapshot([]string{oldID}); len(hidden) != 0 {
		t.Fatalf("removed current row remained eligible: %+v", hidden)
	}
	if err := st.observeConfirmedFlows(nudgeConfirmedFlowSnapshot{PolicyVersion: 4, ReportIdentity: opaqueIdentity("report", "removed"), ConfirmedRows: []string{oldID}}); err != nil {
		t.Fatal(err)
	}

	reloaded := &nudgeStateStore{path: path, now: func() time.Time { return now }}
	if _, got, ok := reloaded.confirmedSnapshot([]string{oldID, newID}); !ok || len(got) != 0 {
		t.Fatalf("restart replay resurrected superseded event: ok=%v events=%+v", ok, got)
	}
	now = now.Add(time.Minute)
	if err := reloaded.observeConfirmedFlows(nudgeConfirmedFlowSnapshot{PolicyVersion: 4, ReportIdentity: opaqueIdentity("report", "restated"), ConfirmedRows: []string{oldID, restatedID}}); err != nil {
		t.Fatal(err)
	}
	if _, got, _ := reloaded.confirmedSnapshot([]string{oldID, restatedID}); len(got) != 1 || got[0].ContentIdentity != restatedID {
		t.Fatalf("material restatement did not rearm: %+v", got)
	}
}

func TestConfirmedFlowEmptyCurrentRowsRemainObservedAcrossReload(t *testing.T) {
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), governanceNudgeStateFile)
	st := &nudgeStateStore{path: path, now: func() time.Time { return now }}
	row := opaqueIdentity("flow", "covered")
	firstReport := opaqueIdentity("report", "nonzero")
	if err := st.observeConfirmedFlows(nudgeConfirmedFlowSnapshot{PolicyVersion: 4, ReportIdentity: firstReport, ConfirmedRows: []string{row}}); err != nil {
		t.Fatal(err)
	}
	governance := opaqueIdentity("governance", "stable")
	policyID := opaqueIdentity("policy", "stable")
	firstAuthority := cutoverAuthorityIdentity(governance, firstReport, now, []string{row})
	if _, _, err := st.reviewConfirmedCutover(nudgeCutoverReviewEvidence{ReviewedAt: now, PolicyIdentity: policyID,
		PolicyVersion: 4, ReportIdentity: firstReport, ConfirmedRows: 1, ReviewedRows: []string{row}, StatementAsOf: now,
		AuthorityIdentity: firstAuthority, GovernanceIdentity: governance}); err != nil {
		t.Fatal(err)
	}

	now = now.Add(time.Minute)
	emptyReport := opaqueIdentity("report", "empty")
	if err := st.observeConfirmedFlows(nudgeConfirmedFlowSnapshot{PolicyVersion: 4, ReportIdentity: emptyReport, ConfirmedRows: []string{}}); err != nil {
		t.Fatal(err)
	}
	emptyAuthority := cutoverAuthorityIdentity(governance, emptyReport, now, nil)
	emptyEvidence := nudgeCutoverReviewEvidence{ReviewedAt: now, PolicyIdentity: policyID,
		PolicyVersion: 4, ReportIdentity: emptyReport, ConfirmedRows: 0, ReviewedRows: []string{}, StatementAsOf: now,
		AuthorityIdentity: emptyAuthority, GovernanceIdentity: governance}
	if _, already, err := st.reviewConfirmedCutover(emptyEvidence); err != nil || already {
		t.Fatalf("empty replacement already=%v err=%v", already, err)
	}

	reloaded := &nudgeStateStore{path: path, now: func() time.Time { return now }}
	reloaded.mu.Lock()
	reloaded.loadLocked()
	coverage := cloneNudgeStateForTest(t, reloaded.state).ConfirmedCoverage
	reloaded.mu.Unlock()
	if coverage == nil || coverage.CurrentRowCount != 0 || len(coverage.CurrentRows) != 0 {
		t.Fatalf("reloaded empty current truth=%+v", coverage)
	}
	currentAuthority := confirmedFlowCurrentAuthority{GovernanceIdentity: governance, ReportIdentity: emptyReport, StatementAsOf: now}
	projected, _, ok, err := reloaded.confirmedSnapshotContext(context.Background(), nil, currentAuthority)
	if err != nil || !ok || projected == nil || projected.PreCutoverFlowsUnreviewed {
		t.Fatalf("reloaded empty projection=%+v ok=%v err=%v", projected, ok, err)
	}
	if _, already, err := reloaded.reviewConfirmedCutover(emptyEvidence); err != nil || !already {
		t.Fatalf("reloaded empty retry already=%v err=%v", already, err)
	}
}

func TestConfirmedFlowLegacyCoverageMigratesMissingCurrentRowsPresence(t *testing.T) {
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), governanceNudgeStateFile)
	row := opaqueIdentity("flow", "legacy")
	report := opaqueIdentity("report", "legacy")
	legacy := fmt.Sprintf(`{"version":1,"confirmed_coverage":{"coverage_from":%q,"report_identity":%q,"covered_row_count":1,"pre_cutover_unreviewed":true,"known_rows":[%q]}}`,
		now.Format(time.RFC3339Nano), report, row)
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	st := &nudgeStateStore{path: path, now: func() time.Time { return now }}
	st.mu.Lock()
	st.loadLocked()
	coverage := cloneNudgeStateForTest(t, st.state).ConfirmedCoverage
	st.mu.Unlock()
	if coverage == nil || coverage.CurrentRowCount != 1 || !slices.Equal(coverage.CurrentRows, []string{row}) {
		t.Fatalf("legacy migration=%+v", coverage)
	}
	if err := st.observeConfirmedFlows(nudgeConfirmedFlowSnapshot{PolicyVersion: 4, ReportIdentity: report, ConfirmedRows: []string{row}}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"current_rows_observed":true`)) {
		t.Fatalf("migrated presence marker was not persisted: %s", raw)
	}
}

func newCutoverReviewTestServer(t *testing.T, now time.Time) (*Server, string) {
	t.Helper()
	return newCutoverReviewTestServerWithPolicy(t, now, validRiskPolicyV4TOML())
}

func newCutoverReviewTestServerWithPolicy(t *testing.T, now time.Time, policyTOML string) (*Server, string) {
	t.Helper()
	s := newNudgeAuthorityTestServer(t, now, policyTOML)
	day := now.Format("20060102")
	name := "flex-cutover-review.xml"
	writeFlexFixture(t, name, now.Format("20060102")+";090000", day, day,
		cashLine("cutover-flow", "Deposits/Withdrawals", 250, day))
	report, snapshot := s.buildReconReportWithSnapshot()
	if report == nil || report.Status != rpc.ReconStatusActive || snapshot == nil || report.ReportID == "" {
		t.Fatalf("cutover fixture report=%+v snapshot=%+v", report, snapshot)
	}
	s.riskCapital.IncorporateStatementSnapshot(*snapshot)
	return s, name
}

func cutoverReviewRequest(t *testing.T) *rpc.Request {
	t.Helper()
	return rawParams(t, rpc.NudgesCutoverReviewParams{
		Origin:   rpc.NudgeCutoverReviewOriginPairedDevice,
		Evidence: rpc.NudgeCutoverReviewEvidencePairedDeviceForegroundRender,
	})
}

func TestNudgesCutoverReviewProductionAuthorizationRevalidationAndPersistence(t *testing.T) {
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	s, fixtureName := newCutoverReviewTestServer(t, now)

	for _, raw := range []string{
		`{"origin":"agent","evidence":"paired_device_foreground_render_review"}`,
		`{"origin":"human-cli","evidence":"paired_device_foreground_render_review"}`,
		`{"origin":"unpaired","evidence":"paired_device_foreground_render_review"}`,
		`{"origin":"paired_device","evidence":"monthly_completion"}`,
	} {
		if _, err := s.handleNudgesCutoverReview(context.Background(), &rpc.Request{Params: json.RawMessage(raw)}); err == nil {
			t.Fatalf("unauthorized cutover request accepted: %s", raw)
		}
	}
	coverage, _, ok := s.nudges.confirmedSnapshot(nil)
	if !ok || coverage == nil || !coverage.PreCutoverFlowsUnreviewed {
		t.Fatalf("refused requests changed coverage=%+v ok=%v", coverage, ok)
	}

	first, err := s.handleNudgesCutoverReview(context.Background(), cutoverReviewRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if !first.OK || first.AlreadyReviewed || !first.ReviewedAt.Equal(now) || first.CoverageFrom.IsZero() || first.Evidence != rpc.NudgeCutoverReviewEvidencePairedDeviceForegroundRender {
		t.Fatalf("first cutover result=%+v", first)
	}
	repeat, err := s.handleNudgesCutoverReview(context.Background(), cutoverReviewRequest(t))
	if err != nil || !repeat.AlreadyReviewed || !repeat.ReviewedAt.Equal(first.ReviewedAt) || !repeat.CoverageFrom.Equal(first.CoverageFrom) {
		t.Fatalf("idempotent retry result=%+v err=%v", repeat, err)
	}

	s.nudges.mu.Lock()
	pinned := *s.nudges.state.ConfirmedCoverage
	statePath := s.nudges.path
	s.nudges.mu.Unlock()
	if pinned.ReviewPolicyIdentity == "" || pinned.ReviewPolicyVersion != 4 || pinned.ReviewReportIdentity == "" || pinned.ReviewedRowCount != 1 {
		t.Fatalf("cutover evidence not pinned to current authority/report: %+v", pinned)
	}

	// Reopening the durable store preserves the exact idempotent receipt.
	s.nudges = &nudgeStateStore{path: statePath, now: s.now}
	s.riskCapital.nudges = s.nudges
	restarted, err := s.handleNudgesCutoverReview(context.Background(), cutoverReviewRequest(t))
	if err != nil || !restarted.AlreadyReviewed || !restarted.ReviewedAt.Equal(first.ReviewedAt) {
		t.Fatalf("restart retry result=%+v err=%v", restarted, err)
	}

	// A materially changed current broker report conflicts with the pinned
	// review; an identical request may not bless the new truth implicitly.
	day := now.Format("20060102")
	writeFlexFixture(t, fixtureName, now.Format("20060102")+";090000", day, day,
		cashLine("cutover-flow", "Deposits/Withdrawals", 250, day)+"\n"+
			cashLine("later-flow", "Deposits/Withdrawals", 100, day))
	if _, err := s.handleNudgesCutoverReview(context.Background(), cutoverReviewRequest(t)); err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("changed-report retry error=%v, want conflict", err)
	}
}

func TestStaleCutoverReviewCanBeReplacedAfterRoutineReportDrift(t *testing.T) {
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	s, fixtureName := newCutoverReviewTestServer(t, now)
	first, err := s.handleNudgesCutoverReview(context.Background(), cutoverReviewRequest(t))
	if err != nil || first == nil || !first.OK || first.AlreadyReviewed {
		t.Fatalf("initial review=%+v err=%v", first, err)
	}

	// Advance the broker-backed statement/report authority without adding a
	// confirmed-flow row. The old exact review becomes inert and a fresh paired
	// action must be able to establish the new baseline.
	day := now.Format("20060102")
	writeFlexFixture(t, fixtureName, now.Format("20060102;150405"), day, day,
		cashLine("cutover-flow", "Deposits/Withdrawals", 250, day)+"\n"+equityRow(day, 250000))
	report, capitalSnapshot := s.buildReconReportWithSnapshot()
	if report == nil || capitalSnapshot == nil || report.ReportID == "" {
		t.Fatalf("drifted report=%+v capital=%+v", report, capitalSnapshot)
	}
	s.riskCapital.IncorporateStatementSnapshot(*capitalSnapshot)

	before, err := s.handleNudgesSnapshot(context.Background(), &rpc.Request{})
	if err != nil || before.ConfirmedFlowCoverage == nil || !before.ConfirmedFlowCoverage.PreCutoverFlowsUnreviewed {
		t.Fatalf("stale review did not reopen: snapshot=%+v err=%v", before, err)
	}
	replacement, err := s.handleNudgesCutoverReview(context.Background(), cutoverReviewRequest(t))
	if err != nil || replacement == nil || !replacement.OK || replacement.AlreadyReviewed {
		t.Fatalf("replacement review=%+v err=%v", replacement, err)
	}
	after, err := s.handleNudgesSnapshot(context.Background(), &rpc.Request{})
	if err != nil || after.ConfirmedFlowCoverage == nil || after.ConfirmedFlowCoverage.PreCutoverFlowsUnreviewed {
		t.Fatalf("replacement review not current: snapshot=%+v err=%v", after, err)
	}

	path := s.nudges.path
	s.nudges = &nudgeStateStore{path: path, now: s.now}
	s.riskCapital.nudges = s.nudges
	reloaded, err := s.handleNudgesSnapshot(context.Background(), &rpc.Request{})
	if err != nil || reloaded.ConfirmedFlowCoverage == nil || reloaded.ConfirmedFlowCoverage.PreCutoverFlowsUnreviewed {
		t.Fatalf("replacement did not survive reload: snapshot=%+v err=%v", reloaded, err)
	}

	// A genuinely current review remains an immutable idempotency boundary.
	s.nudges.mu.Lock()
	current := cloneNudgeStateForTest(t, s.nudges.state)
	s.nudges.mu.Unlock()
	coverage := current.ConfirmedCoverage
	_, _, err = s.nudges.reviewConfirmedCutover(nudgeCutoverReviewEvidence{
		ReviewedAt: replacement.ReviewedAt, PolicyIdentity: coverage.ReviewPolicyIdentity + "-conflict",
		PolicyVersion: coverage.ReviewPolicyVersion, ReportIdentity: coverage.ReviewReportIdentity,
		ConfirmedRows: coverage.ReviewedRowCount, ReviewedRows: coverage.ReviewedRows,
		StatementAsOf: coverage.ReviewStatementAsOf, AuthorityIdentity: coverage.ReviewAuthority,
		GovernanceIdentity: coverage.ReviewGovernance,
	})
	if err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("current conflicting retry error=%v", err)
	}
}

func TestStaleCutoverReplacementRollsBackOnPersistenceFailure(t *testing.T) {
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	s, fixtureName := newCutoverReviewTestServer(t, now)
	if _, err := s.handleNudgesCutoverReview(context.Background(), cutoverReviewRequest(t)); err != nil {
		t.Fatal(err)
	}
	day := now.Format("20060102")
	writeFlexFixture(t, fixtureName, now.Format("20060102;150405"), day, day,
		cashLine("cutover-flow", "Deposits/Withdrawals", 250, day)+"\n"+equityRow(day, 250000))
	_, capitalSnapshot := s.buildReconReportWithSnapshot()
	if capitalSnapshot == nil {
		t.Fatal("routine report drift produced no capital snapshot")
	}
	s.riskCapital.IncorporateStatementSnapshot(*capitalSnapshot)

	s.nudges.mu.Lock()
	before := cloneNudgeStateForTest(t, s.nudges.state)
	s.nudges.writeState = func(string, []byte) error { return errors.New("injected replacement rename failure") }
	s.nudges.mu.Unlock()
	if result, err := s.handleNudgesCutoverReview(context.Background(), cutoverReviewRequest(t)); err == nil || result != nil {
		t.Fatalf("failed replacement result=%+v err=%v", result, err)
	}
	s.nudges.mu.Lock()
	after := cloneNudgeStateForTest(t, s.nudges.state)
	s.nudges.writeState = nil
	s.nudges.fault = false
	s.nudges.mu.Unlock()
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("failed replacement leaked nested state:\n got=%+v\nwant=%+v", after, before)
	}

	result, err := s.handleNudgesCutoverReview(context.Background(), cutoverReviewRequest(t))
	if err != nil || result == nil || !result.OK || result.AlreadyReviewed {
		t.Fatalf("fresh replacement after rollback=%+v err=%v", result, err)
	}
}

func TestMonthlyCompletionSurvivesRoutineAuthorityRefresh(t *testing.T) {
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	s, fixtureName := newCutoverReviewTestServer(t, now)
	rendered, _ := s.composeBrief(context.Background())
	params := rpc.BriefAckParams{Kind: rpc.BriefKindMonthly, Month: "2026-08", Evidence: rpc.BriefAckEvidenceRender,
		BriefFingerprint: rendered.BriefFingerprint, Origin: rpc.OrderOriginPairedDevice}
	ack, err := s.handleBriefAck(context.Background(), rawParams(t, params))
	if err != nil || ack == nil || !ack.OK {
		t.Fatalf("monthly completion=%+v err=%v", ack, err)
	}

	// Daily report and protection-pin generations are volatile diagnostic
	// authority after completion, not a second monthly key.
	day := now.Format("20060102")
	writeFlexFixture(t, fixtureName, now.Format("20060102;150405"), day, day,
		cashLine("cutover-flow", "Deposits/Withdrawals", 250, day)+"\n"+equityRow(day, 250000))
	s.protectionPolicies.mu.Lock()
	s.protectionPolicies.status.PolicyVersion++
	s.protectionPolicies.mu.Unlock()
	authority := s.currentNudgeAuthority(now)
	evaluation, _ := s.governanceMonthlyPulseForAuthority(authority, authority.policy, s.buildReconReport(), now)
	if evaluation.Status != risk.MonthlyPulseStatusCompleted {
		t.Fatalf("routine authority refresh reopened monthly pulse: %+v", evaluation)
	}
	retry, err := s.handleBriefAck(context.Background(), rawParams(t, params))
	if err != nil || retry == nil || !retry.AlreadyStamped || retry.BriefFingerprint != rendered.BriefFingerprint {
		t.Fatalf("routine authority refresh broke idempotent retry: ack=%+v err=%v", retry, err)
	}

	path := s.nudges.path
	s.nudges = &nudgeStateStore{path: path, now: s.now}
	s.riskCapital.nudges = s.nudges
	authority = s.currentNudgeAuthority(now)
	evaluation, _ = s.governanceMonthlyPulseForAuthority(authority, authority.policy, s.buildReconReport(), now)
	if evaluation.Status != risk.MonthlyPulseStatusCompleted {
		t.Fatalf("reload reopened monthly pulse after routine refresh: %+v", evaluation)
	}
	retry, err = s.handleBriefAck(context.Background(), rawParams(t, params))
	if err != nil || retry == nil || !retry.AlreadyStamped || retry.BriefFingerprint != rendered.BriefFingerprint {
		t.Fatalf("reload broke idempotent retry after routine refresh: ack=%+v err=%v", retry, err)
	}

	s.riskPolicies.mu.Lock()
	revisedWarning := 3
	s.riskPolicies.active.Cadence.Nudges.ReconcileWarningDays = &revisedWarning
	s.riskPolicies.lastFingerprint = s.riskPolicies.active.FingerprintKey()
	s.riskPolicies.mu.Unlock()
	authority = s.currentNudgeAuthority(now)
	evaluation, _ = s.governanceMonthlyPulseForAuthority(authority, authority.policy, s.buildReconReport(), now)
	if evaluation.Status == risk.MonthlyPulseStatusCompleted {
		t.Fatalf("policy identity revision did not reopen monthly pulse: %+v", evaluation)
	}
}

func TestMonthlyCompletionRecoversFromTransientWriteFailure(t *testing.T) {
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	s := newV4NudgeTestServer(t, now)
	rendered, _ := s.composeBrief(context.Background())
	params := rpc.BriefAckParams{Kind: rpc.BriefKindMonthly, Month: "2026-08", Evidence: rpc.BriefAckEvidenceRender,
		BriefFingerprint: rendered.BriefFingerprint, Origin: rpc.OrderOriginPairedDevice}
	attempts := 0
	s.nudges.writeState = func(path string, raw []byte) error {
		attempts++
		if attempts == 1 {
			return errors.New("injected transient atomic rename failure")
		}
		return writePrivateStateAtomic(path, raw)
	}
	if ack, err := s.handleBriefAck(context.Background(), rawParams(t, params)); err == nil || ack != nil {
		t.Fatalf("first write unexpectedly succeeded: ack=%+v err=%v", ack, err)
	}
	if s.nudges.healthOK() {
		t.Fatal("failed write did not leave reads faulted")
	}
	if result := s.composeNudgesSnapshot(); result.SourceHealth.ConfirmedFlow.Status != rpc.NudgeInputStatusError || result.IsCleanEmpty() {
		t.Fatalf("faulted snapshot health=%+v clean=%v", result.SourceHealth, result.IsCleanEmpty())
	}

	ack, err := s.handleBriefAck(context.Background(), rawParams(t, params))
	if err != nil || ack == nil || !ack.OK || ack.AlreadyStamped {
		t.Fatalf("authorized retry did not recover: ack=%+v err=%v", ack, err)
	}
	if attempts != 2 || !s.nudges.healthOK() {
		t.Fatalf("retry attempts=%d healthy=%v", attempts, s.nudges.healthOK())
	}
	path := s.nudges.path
	s.nudges = &nudgeStateStore{path: path, now: s.now}
	authority := s.currentNudgeAuthority(now)
	evaluation, completion := s.governanceMonthlyPulseForAuthority(authority, authority.policy, s.buildReconReport(), now)
	if evaluation.Status != risk.MonthlyPulseStatusCompleted || completion == nil || !completion.CompletedAt.Equal(ack.At) {
		t.Fatalf("reloaded completion evaluation=%+v completion=%+v ack=%+v", evaluation, completion, ack)
	}
}

func TestNudgeStoreTransientWriteRecoveryIsSharedAndLoadCorruptionIsNot(t *testing.T) {
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	failOnce := func() func(string, []byte) error {
		attempts := 0
		return func(path string, raw []byte) error {
			attempts++
			if attempts == 1 {
				return errors.New("injected transient atomic rename failure")
			}
			return writePrivateStateAtomic(path, raw)
		}
	}

	t.Run("shadow", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), governanceNudgeStateFile)
		st := &nudgeStateStore{path: path, now: func() time.Time { return now }, writeState: failOnce()}
		policyID, episode := opaqueIdentity("policy", "recovery"), opaqueIdentity("latch", "recovery")
		if err := st.recordShadow(policyID, episode, true, false, true); err == nil || st.healthOK() {
			t.Fatalf("first shadow write err=%v healthy=%v", err, st.healthOK())
		}
		if err := st.recordShadow(policyID, episode, true, false, true); err != nil || !st.healthOK() {
			t.Fatalf("shadow retry err=%v healthy=%v", err, st.healthOK())
		}
		reloaded := &nudgeStateStore{path: path, now: func() time.Time { return now }}
		if candidate := reloaded.shadowCandidate(policyID, episode, true); candidate == nil {
			t.Fatal("reloaded shadow recovery lost the occurrence")
		}
	})

	t.Run("confirmed flow", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), governanceNudgeStateFile)
		st := &nudgeStateStore{path: path, now: func() time.Time { return now }, writeState: failOnce()}
		snapshot := nudgeConfirmedFlowSnapshot{PolicyVersion: 4, ReportIdentity: opaqueIdentity("report", "recovery"),
			ConfirmedRows: []string{opaqueIdentity("flow", "recovery")}}
		if err := st.observeConfirmedFlows(snapshot); err == nil || st.healthOK() {
			t.Fatalf("first confirmed write err=%v healthy=%v", err, st.healthOK())
		}
		if err := st.observeConfirmedFlows(snapshot); err != nil || !st.healthOK() {
			t.Fatalf("confirmed retry err=%v healthy=%v", err, st.healthOK())
		}
		reloaded := &nudgeStateStore{path: path, now: func() time.Time { return now }}
		if coverage, _, ok := reloaded.confirmedSnapshot(snapshot.ConfirmedRows); !ok || coverage == nil {
			t.Fatalf("reloaded confirmed recovery coverage=%+v ok=%v", coverage, ok)
		}
	})

	t.Run("cutover", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), governanceNudgeStateFile)
		st := &nudgeStateStore{path: path, now: func() time.Time { return now }}
		row := opaqueIdentity("flow", "cutover-recovery")
		reportID := opaqueIdentity("report", "cutover-recovery")
		if err := st.observeConfirmedFlows(nudgeConfirmedFlowSnapshot{PolicyVersion: 4, ReportIdentity: reportID, ConfirmedRows: []string{row}}); err != nil {
			t.Fatal(err)
		}
		st.writeState = failOnce()
		evidence := nudgeCutoverReviewEvidence{ReviewedAt: now, PolicyIdentity: opaqueIdentity("policy", "cutover-recovery"),
			PolicyVersion: 4, ReportIdentity: reportID, ConfirmedRows: 1, ReviewedRows: []string{row}, StatementAsOf: now}
		if _, _, err := st.reviewConfirmedCutover(evidence); err == nil || st.healthOK() {
			t.Fatalf("first cutover write err=%v healthy=%v", err, st.healthOK())
		}
		if _, already, err := st.reviewConfirmedCutover(evidence); err != nil || already || !st.healthOK() {
			t.Fatalf("cutover retry already=%v err=%v healthy=%v", already, err, st.healthOK())
		}
		reloaded := &nudgeStateStore{path: path, now: func() time.Time { return now }}
		if coverage, _, ok := reloaded.confirmedSnapshot([]string{row}); !ok || coverage == nil || coverage.PreCutoverFlowsUnreviewed {
			t.Fatalf("reloaded cutover recovery coverage=%+v ok=%v", coverage, ok)
		}
	})

	t.Run("repeated failure stays faulted", func(t *testing.T) {
		st := &nudgeStateStore{path: filepath.Join(t.TempDir(), governanceNudgeStateFile), now: func() time.Time { return now },
			writeState: func(string, []byte) error { return errors.New("persistent rename failure") }}
		for range 2 {
			if err := st.recordShadow(opaqueIdentity("policy", "persistent"), opaqueIdentity("latch", "persistent"), true, false, true); err == nil {
				t.Fatal("persistent failure unexpectedly succeeded")
			}
		}
		if st.healthOK() {
			t.Fatal("repeated persistence failure cleared fault")
		}
	})

	t.Run("load corruption is not recoverable", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), governanceNudgeStateFile)
		if err := os.WriteFile(path, []byte("not-json"), 0o600); err != nil {
			t.Fatal(err)
		}
		st := &nudgeStateStore{path: path, now: func() time.Time { return now }}
		if st.writeReady() {
			t.Fatal("corrupt loaded state was treated as transient-write recovery")
		}
		if err := st.recordShadow(opaqueIdentity("policy", "corrupt"), opaqueIdentity("latch", "corrupt"), true, false, true); err == nil {
			t.Fatal("corrupt loaded state accepted a governance write")
		}
	})
}

func TestMonthlyCompletionLookupIsStableAcrossAuthorityABA(t *testing.T) {
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	s, fixtureName := newCutoverReviewTestServer(t, now)
	rendered, _ := s.composeBrief(context.Background())
	params := rpc.BriefAckParams{Kind: rpc.BriefKindMonthly, Month: "2026-08", Evidence: rpc.BriefAckEvidenceRender,
		BriefFingerprint: rendered.BriefFingerprint, Origin: rpc.OrderOriginPairedDevice}
	first, err := s.handleBriefAck(context.Background(), rawParams(t, params))
	if err != nil || first == nil || !first.OK {
		t.Fatalf("A completion=%+v err=%v", first, err)
	}
	day := now.Format("20060102")
	initialProtectionVersion := s.protectionPolicies.status.PolicyVersion
	writeFlexFixture(t, fixtureName, now.Format("20060102;150405"), day, day,
		cashLine("cutover-flow", "Deposits/Withdrawals", 250, day)+"\n"+equityRow(day, 250000))
	s.protectionPolicies.mu.Lock()
	s.protectionPolicies.status.PolicyVersion++
	s.protectionPolicies.mu.Unlock()
	for _, phase := range []string{"B", "A"} {
		if phase == "A" {
			writeFlexFixture(t, fixtureName, now.Format("20060102")+";090000", day, day,
				cashLine("cutover-flow", "Deposits/Withdrawals", 250, day))
			s.protectionPolicies.mu.Lock()
			s.protectionPolicies.status.PolicyVersion = initialProtectionVersion
			s.protectionPolicies.mu.Unlock()
		}
		retry, err := s.handleBriefAck(context.Background(), rawParams(t, params))
		if err != nil || retry == nil || !retry.AlreadyStamped || retry.BriefFingerprint != first.BriefFingerprint || !retry.At.Equal(first.At) {
			t.Fatalf("%s retry=%+v err=%v, want original durable record %+v", phase, retry, err, first)
		}
	}
	s.nudges.mu.Lock()
	completionRows := len(s.nudges.state.MonthlyCompletions)
	path := s.nudges.path
	s.nudges.mu.Unlock()
	if completionRows != 1 {
		t.Fatalf("A-B-A created %d monthly records, want one", completionRows)
	}
	s.nudges = &nudgeStateStore{path: path, now: s.now}
	retry, err := s.handleBriefAck(context.Background(), rawParams(t, params))
	if err != nil || retry == nil || !retry.AlreadyStamped || !retry.At.Equal(first.At) {
		t.Fatalf("reloaded A retry=%+v err=%v", retry, err)
	}

	s.riskPolicies.mu.Lock()
	revisedWarning := 3
	s.riskPolicies.active.Cadence.Nudges.ReconcileWarningDays = &revisedWarning
	s.riskPolicies.lastFingerprint = s.riskPolicies.active.FingerprintKey()
	s.riskPolicies.mu.Unlock()
	authority := s.currentNudgeAuthority(now)
	evaluation, _ := s.governanceMonthlyPulseForAuthority(authority, authority.policy, s.buildReconReport(), now)
	if evaluation.Status == risk.MonthlyPulseStatusCompleted {
		t.Fatalf("policy C did not reopen monthly duty: %+v", evaluation)
	}
}

func TestNudgesCutoverReviewRejectsStaleCurrentReportAndDispatches(t *testing.T) {
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	s, _ := newCutoverReviewTestServer(t, now)
	now = now.Add(5 * 24 * time.Hour)
	s.now = func() time.Time { return now }
	s.riskCapital.now = s.now
	s.nudges.now = s.now
	s.riskPolicies.mu.Lock()
	s.riskPolicies.now = s.now
	s.riskPolicies.loadedAt = now
	s.riskPolicies.lastCheckedAt = now
	s.riskPolicies.mu.Unlock()
	if _, err := s.handleNudgesCutoverReview(context.Background(), cutoverReviewRequest(t)); err == nil || !strings.Contains(err.Error(), "current active broker-backed reconciliation report") {
		t.Fatalf("stale report error=%v", err)
	}

	// Restore current time and prove the production dispatcher routes the
	// fixed-shape write through the typed result boundary.
	now = time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	s.riskPolicies.mu.Lock()
	s.riskPolicies.loadedAt = now
	s.riskPolicies.lastCheckedAt = now
	s.riskPolicies.mu.Unlock()
	s.logger = NewLogger(&bytes.Buffer{}, "error")
	request := cutoverReviewRequest(t)
	request.ID = "cutover-1"
	request.Method = rpc.MethodNudgesCutoverReview
	var output bytes.Buffer
	if terminal := s.dispatch(context.Background(), request, json.NewEncoder(&output), bufio.NewReader(strings.NewReader(""))); terminal {
		t.Fatal("nudges.cutover_review unexpectedly marked streaming")
	}
	var response rpc.Response
	if err := json.Unmarshal(output.Bytes(), &response); err != nil || response.Error != nil {
		t.Fatalf("dispatch response error=%v response=%+v wire=%s", err, response, output.String())
	}
	var result rpc.NudgesCutoverReviewResult
	if err := json.Unmarshal(response.Result, &result); err != nil || !result.OK {
		t.Fatalf("typed cutover result=%+v err=%v", result, err)
	}
}

func TestShadowEpisodeCountRestartResolutionAndRearm(t *testing.T) {
	now := time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), governanceNudgeStateFile)
	st := &nudgeStateStore{path: path, now: func() time.Time { return now }}
	policyA, policyB := opaqueIdentity("policy", "a"), opaqueIdentity("policy", "b")
	episode := opaqueIdentity("latch", "one")
	for range 2 {
		if err := st.recordShadow(policyA, episode, true, false, true); err != nil {
			t.Fatal(err)
		}
	}
	st.mu.Lock()
	count := st.state.Shadow.Count
	st.mu.Unlock()
	if count != 2 {
		t.Fatalf("episode count=%d, want 2", count)
	}
	reloaded := &nudgeStateStore{path: path, now: func() time.Time { return now }}
	first := reloaded.shadowCandidate(policyA, episode, true)
	if first == nil || reloaded.shadowCandidate(policyA, episode, false) != nil {
		t.Fatalf("restart/open resolution candidate=%+v", first)
	}
	if reloaded.shadowCandidate(policyB, episode, true) != nil {
		t.Fatal("old policy episode survived a policy revision")
	}
	now = now.Add(time.Minute)
	if err := reloaded.recordShadow(policyB, episode, true, false, true); err != nil {
		t.Fatal(err)
	}
	second := reloaded.shadowCandidate(policyB, episode, true)
	if second == nil || second.Fingerprint == first.Fingerprint {
		t.Fatalf("policy revision did not rearm: first=%+v second=%+v", first, second)
	}
}

func TestNudgeMutationRollsBackOnPersistenceFailure(t *testing.T) {
	now := time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC)
	root := t.TempDir()
	goodPath := filepath.Join(root, governanceNudgeStateFile)
	failWrite := func(string, []byte) error { return errors.New("injected atomic rename failure") }
	policyID, episode := opaqueIdentity("policy", "rollback"), opaqueIdentity("latch", "rollback")

	t.Run("shadow creation and update", func(t *testing.T) {
		st := &nudgeStateStore{path: goodPath, now: func() time.Time { return now }, writeState: failWrite}
		if err := st.recordShadow(policyID, episode, true, false, true); err == nil {
			t.Fatal("shadow creation unexpectedly persisted")
		}
		if got := st.state.Shadow; got != (nudgeShadowEpisodeState{}) {
			t.Fatalf("failed creation leaked in memory: %+v", got)
		}

		st.writeState, st.fault = nil, false
		if err := st.recordShadow(policyID, episode, true, false, true); err != nil {
			t.Fatal(err)
		}
		before := st.state.Shadow
		st.writeState = failWrite
		if err := st.recordShadow(policyID, episode, true, false, true); err == nil {
			t.Fatal("shadow update unexpectedly persisted")
		}
		if st.state.Shadow != before {
			t.Fatalf("failed update leaked in memory: got=%+v want=%+v", st.state.Shadow, before)
		}
		st.writeState, st.fault = nil, false
		if err := st.observeConfirmedFlows(nudgeConfirmedFlowSnapshot{
			PolicyVersion: 4, ReportIdentity: opaqueIdentity("report", "unrelated"),
		}); err != nil {
			t.Fatal(err)
		}
		reopened := &nudgeStateStore{path: goodPath, now: func() time.Time { return now }}
		reopened.loadLocked()
		if reopened.state.Shadow != before {
			t.Fatalf("later save flushed failed shadow update: got=%+v want=%+v", reopened.state.Shadow, before)
		}
	})

	t.Run("confirmed creation and nested update", func(t *testing.T) {
		path := filepath.Join(root, "confirmed.json")
		st := &nudgeStateStore{path: path, now: func() time.Time { return now }, writeState: failWrite}
		oldID, newID := opaqueIdentity("flow", "old"), opaqueIdentity("flow", "new")
		first := nudgeConfirmedFlowSnapshot{PolicyVersion: 4, ReportIdentity: opaqueIdentity("report", "first"), ConfirmedRows: []string{oldID}}
		if err := st.observeConfirmedFlows(first); err == nil {
			t.Fatal("coverage creation unexpectedly persisted")
		}
		if st.state.ConfirmedCoverage != nil || len(st.state.ConfirmedEvents) != 0 {
			t.Fatalf("failed coverage creation leaked state: %+v %+v", st.state.ConfirmedCoverage, st.state.ConfirmedEvents)
		}

		st.writeState, st.fault = nil, false
		if err := st.observeConfirmedFlows(first); err != nil {
			t.Fatal(err)
		}
		before := cloneNudgeStateForTest(t, st.state)
		st.writeState = failWrite
		if err := st.observeConfirmedFlows(nudgeConfirmedFlowSnapshot{
			PolicyVersion: 4, ReportIdentity: opaqueIdentity("report", "update"), ConfirmedRows: []string{oldID, newID},
		}); err == nil {
			t.Fatal("coverage update unexpectedly persisted")
		}
		if !reflect.DeepEqual(st.state, before) {
			t.Fatalf("failed nested update leaked state:\n got=%+v\nwant=%+v", st.state, before)
		}
		st.writeState, st.fault = nil, false
		if _, _, err := st.reviewConfirmedCutover(nudgeCutoverReviewEvidence{
			ReviewedAt: now, PolicyIdentity: policyID, PolicyVersion: 4,
			ReportIdentity: first.ReportIdentity, ConfirmedRows: 1, ReviewedRows: []string{oldID},
		}); err != nil {
			t.Fatal(err)
		}
		if err := st.observeConfirmedFlows(nudgeConfirmedFlowSnapshot{
			PolicyVersion: 4, ReportIdentity: opaqueIdentity("report", "event"), ConfirmedRows: []string{oldID, newID},
		}); err != nil {
			t.Fatal(err)
		}
		beforeEventMutation := cloneNudgeStateForTest(t, st.state)
		st.writeState = failWrite
		if err := st.observeConfirmedFlows(nudgeConfirmedFlowSnapshot{
			PolicyVersion: 4, ReportIdentity: opaqueIdentity("report", "supersede"), ConfirmedRows: []string{oldID},
		}); err == nil {
			t.Fatal("event supersession unexpectedly persisted")
		}
		if !reflect.DeepEqual(st.state, beforeEventMutation) {
			t.Fatalf("failed event/supersession mutation leaked state:\n got=%+v\nwant=%+v", st.state, beforeEventMutation)
		}

		// A later unrelated successful write must not flush the failed row.
		st.writeState, st.fault = nil, false
		if err := st.recordShadow(policyID, episode, true, false, true); err != nil {
			t.Fatal(err)
		}
		reopened := &nudgeStateStore{path: path, now: func() time.Time { return now }}
		reopened.loadLocked()
		if !reflect.DeepEqual(reopened.state.ConfirmedCoverage, beforeEventMutation.ConfirmedCoverage) || !reflect.DeepEqual(reopened.state.ConfirmedEvents, beforeEventMutation.ConfirmedEvents) {
			t.Fatalf("later save flushed failed confirmed state: %+v", reopened.state)
		}
	})
}

func TestPersistedGovernanceEvidenceIsInertAfterAuthorityRaces(t *testing.T) {
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	type mutation struct {
		name   string
		mutate func(*Server, string)
	}
	mutations := []mutation{
		{name: "policy revision", mutate: func(s *Server, _ string) {
			s.riskPolicies.mu.Lock()
			revised := 3
			s.riskPolicies.active.Cadence.Nudges.ReconcileWarningDays = &revised
			s.riskPolicies.lastFingerprint = s.riskPolicies.active.FingerprintKey()
			s.riskPolicies.mu.Unlock()
		}},
		{name: "pin change", mutate: func(s *Server, _ string) {
			s.protectionPolicies.mu.Lock()
			s.protectionPolicies.status.PolicyVersion++
			s.protectionPolicies.mu.Unlock()
		}},
		{name: "report row change", mutate: func(_ *Server, fixture string) {
			day := now.Format("20060102")
			writeFlexFixture(t, fixture, now.Format("20060102")+";090000", day, day,
				cashLine("cutover-flow", "Deposits/Withdrawals", 250, day)+"\n"+cashLine("post-validation-flow", "Deposits/Withdrawals", 25, day))
		}},
	}
	for _, phase := range []string{"after_validation", "after_persist"} {
		for _, mutation := range mutations {
			t.Run("monthly/"+phase+"/"+mutation.name, func(t *testing.T) {
				s, fixture := newCutoverReviewTestServer(t, now)
				rendered, _ := s.composeBrief(context.Background())
				hook := func(kind string) {
					if kind == "monthly" {
						mutation.mutate(s, fixture)
					}
				}
				if phase == "after_validation" {
					s.nudgeAfterValidation = hook
				} else {
					s.nudgeAfterPersist = hook
				}
				ack, err := s.handleBriefAck(context.Background(), rawParams(t, rpc.BriefAckParams{
					Kind: rpc.BriefKindMonthly, Month: "2026-08", Evidence: rpc.BriefAckEvidenceRender,
					BriefFingerprint: rendered.BriefFingerprint, Origin: rpc.OrderOriginPairedDevice,
				}))
				if phase == "after_validation" {
					if err == nil || ack != nil {
						t.Fatalf("pre-write authority race persisted monthly evidence: ack=%+v err=%v", ack, err)
					}
					s.nudges.mu.Lock()
					completionCount := len(s.nudges.state.MonthlyCompletions)
					s.nudges.mu.Unlock()
					if completionCount != 0 {
						t.Fatalf("pre-write authority race left %d monthly completion rows", completionCount)
					}
					return
				}
				if err != nil || ack == nil || !ack.OK {
					t.Fatalf("raced monthly write ack=%+v err=%v", ack, err)
				}
				authority := s.currentNudgeAuthority(now)
				report := s.buildReconReport()
				evaluation, _ := s.governanceMonthlyPulseForAuthority(authority, authority.policy, report, now)
				wantCompleted := mutation.name != "policy revision"
				if gotCompleted := evaluation.Status == risk.MonthlyPulseStatusCompleted; gotCompleted != wantCompleted {
					t.Fatalf("monthly completion current=%v want=%v after %s: %+v", gotCompleted, wantCompleted, mutation.name, evaluation)
				}
				statePath := s.nudges.path
				s.nudges = &nudgeStateStore{path: statePath, now: s.now}
				evaluation, _ = s.governanceMonthlyPulseForAuthority(s.currentNudgeAuthority(now), s.riskPolicies.snapshot().policy, s.buildReconReport(), now)
				if gotCompleted := evaluation.Status == risk.MonthlyPulseStatusCompleted; gotCompleted != wantCompleted {
					t.Fatalf("restart monthly completion current=%v want=%v after %s: %+v", gotCompleted, wantCompleted, mutation.name, evaluation)
				}
			})

			t.Run("cutover/"+phase+"/"+mutation.name, func(t *testing.T) {
				s, fixture := newCutoverReviewTestServer(t, now)
				hook := func(kind string) {
					if kind == "cutover" {
						mutation.mutate(s, fixture)
					}
				}
				if phase == "after_validation" {
					s.nudgeAfterValidation = hook
				} else {
					s.nudgeAfterPersist = hook
				}
				if result, err := s.handleNudgesCutoverReview(context.Background(), cutoverReviewRequest(t)); err != nil || result == nil || !result.OK {
					t.Fatalf("raced cutover result=%+v err=%v", result, err)
				}
				snapshot, err := s.handleNudgesSnapshot(context.Background(), &rpc.Request{})
				if err != nil {
					t.Fatal(err)
				}
				if snapshot.ConfirmedFlowCoverage == nil || !snapshot.ConfirmedFlowCoverage.PreCutoverFlowsUnreviewed {
					t.Fatalf("stale cutover evidence reviewed current authority: coverage=%+v health=%+v", snapshot.ConfirmedFlowCoverage, snapshot.SourceHealth)
				}
				statePath := s.nudges.path
				s.nudges = &nudgeStateStore{path: statePath, now: s.now}
				restarted, err := s.handleNudgesSnapshot(context.Background(), &rpc.Request{})
				if err != nil || restarted.ConfirmedFlowCoverage == nil || !restarted.ConfirmedFlowCoverage.PreCutoverFlowsUnreviewed {
					t.Fatalf("restart revived stale cutover evidence: snapshot=%+v err=%v", restarted, err)
				}
			})
		}
	}
}

func cloneNudgeStateForTest(t *testing.T, state nudgeStateFileV1) nudgeStateFileV1 {
	t.Helper()
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var cloned nudgeStateFileV1
	if err := json.Unmarshal(raw, &cloned); err != nil {
		t.Fatal(err)
	}
	return cloned
}

func TestLatchResetRearmsOpaqueEpisodeEvenAtSameTimestamp(t *testing.T) {
	now := time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC)
	s := newRiskPolicyTestServer(t, validRiskPolicyTOML)
	s.now = func() time.Time { return now }
	s.riskCapital.now = s.now
	policy := s.riskPolicies.snapshot().policy
	if _, err := s.riskCapital.ApplyCapitalEvent(rpc.CapitalEventParams{Type: "reconcile"}, rpc.OrderOriginHumanTTY); err != nil {
		t.Fatal(err)
	}
	s.riskCapital.Observe(260000, now, policy, testLiveObserveScope)
	s.riskCapital.Observe(240000, now, policy, testLiveObserveScope)
	open, first, _ := s.riskCapital.NudgeLatch()
	if !open || first == "" {
		t.Fatalf("first latch open=%v episode=%q", open, first)
	}
	if err := s.riskCapital.ResetDrawdown("test reset", policy); err != nil {
		t.Fatal(err)
	}
	s.riskCapital.Observe(220000, now, policy, testLiveObserveScope)
	open, second, _ := s.riskCapital.NudgeLatch()
	if !open || second == "" || second == first {
		t.Fatalf("reset did not rearm at same time: first=%q second=%q open=%v", first, second, open)
	}
}

func TestNudgesSnapshotDispatchUsesTypedMarshalBoundary(t *testing.T) {
	s := newRiskPolicyTestServer(t, validRiskPolicyV3TOML())
	s.installNudgeStateStore()
	s.logger = NewLogger(&bytes.Buffer{}, "error")
	request := &rpc.Request{ID: "nudge-1", Method: rpc.MethodNudgesSnapshot}
	var output bytes.Buffer
	terminal := s.dispatch(context.Background(), request, json.NewEncoder(&output), bufio.NewReader(strings.NewReader("")))
	if terminal {
		t.Fatal("nudges.snapshot unexpectedly marked streaming")
	}
	var response rpc.Response
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatalf("dispatch output: %v: %s", err, output.String())
	}
	if response.Error != nil || len(response.Result) == 0 {
		t.Fatalf("dispatch response=%+v", response)
	}
	var result rpc.NudgesSnapshotResult
	if err := json.Unmarshal(response.Result, &result); err != nil {
		t.Fatal(err)
	}
	if result.SourceHealth.Aggregate != rpc.NudgeAggregateSuppressed {
		t.Fatalf("aggregate=%q, mandatory marshal normalization did not run", result.SourceHealth.Aggregate)
	}
}
