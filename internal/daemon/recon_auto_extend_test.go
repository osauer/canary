package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestAutoExtendEligibilityRefusals(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	pol := testV3Constitution()
	divergence := 0.5
	clean := func() *rpc.ReconResult {
		return &rpc.ReconResult{
			Status:        rpc.ReconStatusActive,
			ReportID:      "recon-clean",
			StatementAsOf: now.Add(-time.Hour),
			Equity: &rpc.ReconEquityCheck{
				StatementDate: now.Add(-time.Hour), SameDay: true, DivergencePct: &divergence,
			},
			InputHealth: []rpc.SourceHealth{{Source: "statements", Status: "ok"}},
		}
	}
	for _, tc := range []struct {
		name   string
		status string
		policy func() *risk.Constitution
		report func() *rpc.ReconResult
	}{
		{"unresolved exception", rpc.RiskPolicyStatusActive, func() *risk.Constitution { return pol }, func() *rpc.ReconResult { r := clean(); r.Unresolved = 1; return r }},
		{"stale statements", rpc.RiskPolicyStatusActive, func() *risk.Constitution { return pol }, func() *rpc.ReconResult { r := clean(); r.StatementAsOf = now.Add(-5 * 24 * time.Hour); return r }},
		{"no report buildable", rpc.RiskPolicyStatusActive, func() *risk.Constitution { return pol }, func() *rpc.ReconResult { return nil }},
		{"statement health not ok", rpc.RiskPolicyStatusActive, func() *risk.Constitution { return pol }, func() *rpc.ReconResult { r := clean(); r.InputHealth[0].Status = "degraded"; return r }},
		{"report degraded", rpc.RiskPolicyStatusActive, func() *risk.Constitution { return pol }, func() *rpc.ReconResult { r := clean(); r.Status = rpc.ReconStatusDegraded; return r }},
		{"divergence unavailable", rpc.RiskPolicyStatusActive, func() *risk.Constitution { return pol }, func() *rpc.ReconResult { r := clean(); r.Equity.DivergencePct = nil; return r }},
		{"no same-day pair", rpc.RiskPolicyStatusActive, func() *risk.Constitution { return pol }, func() *rpc.ReconResult { r := clean(); r.Equity.SameDay = false; return r }},
		{"same-day pair too old", rpc.RiskPolicyStatusActive, func() *risk.Constitution { return pol }, func() *rpc.ReconResult { r := clean(); r.Equity.StatementDate = now.Add(-5 * 24 * time.Hour); return r }},
		{"divergence above bound", rpc.RiskPolicyStatusActive, func() *risk.Constitution { return pol }, func() *rpc.ReconResult { r := clean(); v := 1.5; r.Equity.DivergencePct = &v; return r }},
		{"negative divergence above bound", rpc.RiskPolicyStatusActive, func() *risk.Constitution { return pol }, func() *rpc.ReconResult { r := clean(); v := -1.5; r.Equity.DivergencePct = &v; return r }},
		{"divergence key absent", rpc.RiskPolicyStatusActive, func() *risk.Constitution { c := *pol; c.Recon.MaxEquityDivergencePct = nil; return &c }, clean},
		{"policy version two", rpc.RiskPolicyStatusActive, testConstitution, clean},
		{"policy drift", rpc.RiskPolicyStatusDrift, func() *risk.Constitution { return pol }, clean},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if autoExtendEligible(tc.status, tc.policy(), tc.report(), now) {
				t.Fatal("ineligible report was accepted")
			}
		})
	}
	if !autoExtendEligible(rpc.RiskPolicyStatusActive, pol, clean(), now) {
		t.Fatal("clean report was refused")
	}
}

func prepareAutoExtendServer(t *testing.T) (*Server, time.Time) {
	t.Helper()
	s := newReconV3TestServer(t)
	now := time.Now().UTC().Truncate(time.Second)
	s.now = func() time.Time { return now }
	s.riskCapital.now = s.now
	day := now.Format("20060102")
	writeFlexFixture(t, "flex-auto.xml", now.Format("20060102")+";060000", day, day, equityRow(day, 250000))
	s.riskCapital.mu.Lock()
	s.riskCapital.loadLocked()
	s.riskCapital.state.GenesisAt = now.Add(-24 * time.Hour)
	s.riskCapital.state.Seeded = true
	s.riskCapital.state.AdjustedPeakBase = 250000
	s.riskCapital.state.PeakAsOf = now
	s.riskCapital.state.LastEquityBase = 250000
	s.riskCapital.state.LastEquityAsOf = now
	s.riskCapital.state.DailyEquity = map[string]float64{now.Format("2006-01-02"): 250000}
	s.riskCapital.persistLocked(true)
	s.riskCapital.mu.Unlock()
	return s, now
}

func TestRiskPolicyV3AutoExtendSuccessAndExactlyOnce(t *testing.T) {
	s, now := prepareAutoExtendServer(t)
	if !s.evaluateRiskPolicyV3Reconciliation() {
		t.Fatal("clean report did not auto-extend")
	}
	if s.evaluateRiskPolicyV3Reconciliation() {
		t.Fatal("same report auto-extended twice")
	}
	rep := s.buildReconReport()
	if rep.LastAutoExtendReportID != rep.ReportID || !rep.LastAutoExtendedAt.Equal(now) {
		t.Fatalf("auto disclosure report=%s/%s at=%s", rep.LastAutoExtendReportID, rep.ReportID, rep.LastAutoExtendedAt)
	}
	capital := s.riskCapital.Report(s.riskPolicies.snapshot().policy, nil, testLiveObserveScope)
	if !capital.LastReconciledAt.Equal(now) || capital.LastReconcileReportID != rep.ReportID || capital.LastReconcileSource != rpc.ReconcileSourceAutomatic || capital.ReconcileStale {
		t.Fatalf("capital auto evidence = %+v", capital)
	}
	data, err := os.ReadFile(filepath.Join(os.Getenv("XDG_STATE_HOME"), "ibkr", capitalEventsJournalFile))
	if err != nil {
		t.Fatal(err)
	}
	var count int
	for _, line := range splitJournalLines(data) {
		var ev capitalEventV1
		if unmarshalJournalLine(line, &ev) && ev.Type == "reconcile" && ev.ReportID == rep.ReportID {
			count++
			if ev.Origin != riskCapitalAutoOrigin || ev.CoverageTo.IsZero() {
				t.Fatalf("automatic event = %+v", ev)
			}
		}
	}
	if count != 1 {
		t.Fatalf("automatic events for report = %d, want 1", count)
	}
}

func TestRiskPolicyV3StartupCatchUpReplaysAndDoesNotDuplicate(t *testing.T) {
	s, _ := prepareAutoExtendServer(t)
	s.riskCapital = &riskCapitalStore{now: s.now}
	s.riskCapital.EnsureLoaded()
	if !s.evaluateRiskPolicyV3Reconciliation() {
		t.Fatal("startup catch-up did not append missing evidence")
	}
	s.riskCapital = &riskCapitalStore{now: s.now}
	s.riskCapital.EnsureLoaded()
	if s.evaluateRiskPolicyV3Reconciliation() {
		t.Fatal("startup catch-up duplicated an already covered report")
	}
}

func TestRiskPolicyV3ExistingHumanReconcileReportBlocksAutoDuplicate(t *testing.T) {
	s, _ := prepareAutoExtendServer(t)
	rep := s.buildReconReport()
	if _, err := s.riskCapital.ApplyCapitalEventForPolicy(rpc.CapitalEventParams{Type: "reconcile"}, rpc.OrderOriginHumanTTY,
		s.riskPolicies.snapshot().policy, &capitalReconRef{ReportID: rep.ReportID, CoverageTo: rep.CoverageTo}); err != nil {
		t.Fatal(err)
	}
	if s.evaluateRiskPolicyV3Reconciliation() {
		t.Fatal("report already referenced by a human reconcile auto-extended")
	}
}

func TestLateSameDayEquityTriggersAutomaticReconcileOnlyOnce(t *testing.T) {
	s := newReconV3TestServer(t)
	now := time.Date(2026, 7, 21, 7, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	s.riskCapital.now = s.now
	day := now.Format("20060102")
	writeFlexFixture(t, "flex-before-equity.xml", day+";063000", day, day, equityRow(day, 250000))

	s.riskCapital.mu.Lock()
	s.riskCapital.loadLocked()
	s.riskCapital.state.GenesisAt = now.Add(-24 * time.Hour)
	s.riskCapital.state.Seeded = true
	s.riskCapital.state.AdjustedPeakBase = 250000
	s.riskCapital.state.PeakAsOf = now.Add(-time.Hour)
	s.riskCapital.state.DailyEquity = nil
	if err := s.riskCapital.persistLocked(true); err != nil {
		s.riskCapital.mu.Unlock()
		t.Fatal(err)
	}
	s.riskCapital.mu.Unlock()

	if s.evaluateRiskPolicyV3Reconciliation() {
		t.Fatal("report auto-reconciled before a same-day runtime account value existed")
	}
	policy := s.riskPolicies.snapshot().policy
	if first := s.riskCapital.Observe(250000, now, policy, testLiveObserveScope); !first {
		t.Fatal("first same-day runtime account value was not identified")
	}
	if !s.evaluateRiskPolicyV3Reconciliation() {
		t.Fatal("late same-day runtime account value did not complete automatic reconciliation")
	}

	if first := s.riskCapital.Observe(250000, now.Add(time.Minute), policy, testLiveObserveScope); first {
		t.Fatal("second account read on the same day was treated as the first")
	}
	if s.evaluateRiskPolicyV3Reconciliation() {
		t.Fatal("same report was automatically reconciled more than once")
	}

	report := s.buildReconReport()
	if report.ReportID == "" || report.LastAutoExtendReportID != report.ReportID {
		t.Fatalf("automatic evidence does not pin the current report: report=%q auto=%q", report.ReportID, report.LastAutoExtendReportID)
	}
}

func TestAccountSummaryWiresFirstDailyObservationToV3Reconciliation(t *testing.T) {
	data, err := os.ReadFile("handlers.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	wrapperStart := strings.Index(source, "func (s *Server) buildAccountSummary(ctx")
	if wrapperStart < 0 {
		t.Fatal("buildAccountSummary production hook is missing")
	}
	wrapperEnd := strings.Index(source[wrapperStart+1:], "\nfunc ")
	if wrapperEnd < 0 || !strings.Contains(source[wrapperStart:wrapperStart+1+wrapperEnd], "s.buildAccountSummaryWithAuthority(ctx, observe)") {
		t.Fatal("buildAccountSummary must delegate to the authority-preserving production path")
	}

	start := strings.Index(source, "func (s *Server) buildAccountSummaryWithAuthority")
	if start < 0 {
		t.Fatal("buildAccountSummaryWithAuthority production hook is missing")
	}
	end := strings.Index(source[start+1:], "\nfunc ")
	if end < 0 {
		t.Fatal("could not isolate buildAccountSummary production hook")
	}
	block := source[start : start+1+end]
	observeAt := strings.Index(block, "firstDailyObservation := s.riskCapital.Observe")
	evaluateAt := strings.Index(block, "s.evaluateRiskPolicyV3Reconciliation()")
	if observeAt < 0 || evaluateAt < 0 || evaluateAt < observeAt {
		t.Fatal("first daily account observation must invoke v3 reconciliation in the production account-summary path")
	}
}

func splitJournalLines(data []byte) [][]byte {
	var lines [][]byte
	for _, line := range bytesSplitLines(data) {
		if len(line) > 0 {
			lines = append(lines, line)
		}
	}
	return lines
}

func bytesSplitLines(data []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, b := range data {
		if b == '\n' {
			lines = append(lines, data[start:i])
			start = i + 1
		}
	}
	if start < len(data) {
		lines = append(lines, data[start:])
	}
	return lines
}

func unmarshalJournalLine(line []byte, out any) bool {
	return json.Unmarshal(line, out) == nil
}
