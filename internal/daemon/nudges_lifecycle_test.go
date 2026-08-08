package daemon

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func validRiskPolicyV4TOML() string {
	return strings.Replace(validRiskPolicyV3TOML(), "policy_version = 3", "policy_version = 4", 1) + `

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

func newV4NudgeTestServer(t *testing.T, now time.Time) *Server {
	t.Helper()
	s := newRiskPolicyTestServer(t, validRiskPolicyV4TOML())
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
	defer s.riskCapital.mu.Unlock()
	s.riskCapital.loadLocked()
	s.riskCapital.state.Seeded = true
	s.riskCapital.state.AdjustedPeakBase = 260000
	if consumedKnown {
		s.riskCapital.state.LastEquityBase = 240000
		s.riskCapital.state.LastEquityAsOf = now
	}
	s.riskCapital.state.BlockLatched = true
	s.riskCapital.state.LatchedAt = now.Add(-2 * time.Hour)
	s.riskCapital.state.LatchEpisodeSeq = 1
}

func candidateKindPresent(candidates []rpc.NudgeCandidate, kind string) bool {
	for _, candidate := range candidates {
		if candidate.Kind == kind {
			return true
		}
	}
	return false
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

func TestConfirmedFlowBaselinesExistingRowsAndEmitsOnlyNewFacts(t *testing.T) {
	now := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	st := &nudgeStateStore{path: filepath.Join(t.TempDir(), governanceNudgeStateFile), now: func() time.Time { return now }}
	oldRow, newRow := opaqueIdentity("flow", "old"), opaqueIdentity("flow", "new")
	if err := st.observeConfirmedFlows(nudgeConfirmedFlowSnapshot{PolicyVersion: 4, ReportIdentity: "report-1", ConfirmedRows: []string{oldRow}}); err != nil {
		t.Fatal(err)
	}
	coverage, events, ok := st.confirmedSnapshot([]string{oldRow})
	if !ok || coverage == nil || coverage.CoverageFrom != now || len(events) != 0 {
		t.Fatalf("initial baseline coverage=%+v events=%+v ok=%v", coverage, events, ok)
	}
	now = now.Add(time.Hour)
	if err := st.observeConfirmedFlows(nudgeConfirmedFlowSnapshot{PolicyVersion: 4, ReportIdentity: "report-2", ConfirmedRows: []string{oldRow, newRow}}); err != nil {
		t.Fatal(err)
	}
	_, events, ok = st.confirmedSnapshot([]string{oldRow, newRow})
	if !ok || len(events) != 1 || events[0].ContentIdentity != newRow {
		t.Fatalf("new facts events=%+v ok=%v", events, ok)
	}
	reloaded := &nudgeStateStore{path: st.path, now: st.now}
	_, events, ok = reloaded.confirmedSnapshot([]string{newRow})
	if !ok || len(events) != 1 || events[0].ContentIdentity != newRow {
		t.Fatalf("reloaded events=%+v ok=%v", events, ok)
	}
}
