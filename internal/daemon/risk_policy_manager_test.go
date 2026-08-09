package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

const validRiskPolicyTOML = `
kind = "ibkr.risk_policy"
schema_version = 1
policy_id = "risk-constitution"
policy_version = 1

[capital]
base_currency = "EUR"
protected_floor = 200000.0
declared_risk_capital = 50000.0
max_equity_age_minutes = 240
max_unreconciled_days = 7

[drawdown]
warn_consumed_pct = 15.0
block_consumed_pct = 30.0
block_enforcement = "shadow"

[override]
max_duration_hours = 24

[recon]
amount_tolerance_pct = 0.5
amount_tolerance_min = 5.0
date_window_business_days = 3
max_report_age_days = 4

[cadence.morning]
class = "advisory"
`

func validRiskPolicyV3TOML() string {
	v3 := strings.Replace(validRiskPolicyTOML, "policy_version = 1", "policy_version = 3", 1)
	return strings.Replace(v3, "max_report_age_days = 4", "max_report_age_days = 4\nmax_equity_divergence_pct = 1.0", 1)
}

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
	manager := newProtectionPolicyManager("", false, time.Second, s.now)
	manager.reload()
	s.protectionPolicies = manager
	return s
}

func primeNudgeBlockEpisode(server *Server, now time.Time, consumedKnown bool) {
	server.riskCapital.mu.Lock()
	defer server.riskCapital.mu.Unlock()
	server.riskCapital.loadLocked()
	server.riskCapital.state.Seeded = true
	server.riskCapital.state.AdjustedPeakBase = 260000
	if consumedKnown {
		server.riskCapital.state.LastEquityBase = 240000
		server.riskCapital.state.LastEquityAsOf = now
	}
	server.riskCapital.state.BlockLatched = true
	server.riskCapital.state.LatchedAt = now.Add(-2 * time.Hour)
	server.riskCapital.state.LatchEpisodeSeq = 1
}

func newTestRiskPolicyManager(t *testing.T, contents string) (*riskPolicyManager, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "risk-policy.toml")
	if contents != "" {
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	m := newRiskPolicyManager(path, time.Second, time.Now)
	m.reload()
	return m, path
}

// The constitution has no embedded default: a missing file is a disclosed
// absent state with a nil policy, never silent code values.
func TestRiskPolicyManagerAbsentFile(t *testing.T) {
	m, _ := newTestRiskPolicyManager(t, "")
	snap := m.snapshot()
	if snap.status != rpc.RiskPolicyStatusAbsent {
		t.Fatalf("status = %s, want absent", snap.status)
	}
	if snap.policy != nil {
		t.Fatal("absent file must yield a nil policy, not a default")
	}
}

func TestRiskPolicyManagerLoadsValidFile(t *testing.T) {
	m, _ := newTestRiskPolicyManager(t, validRiskPolicyTOML)
	snap := m.snapshot()
	if snap.status != rpc.RiskPolicyStatusActive {
		t.Fatalf("status = %s (%s), want active", snap.status, snap.message)
	}
	if snap.policy == nil || snap.policy.PolicyID != "risk-constitution" {
		t.Fatalf("policy = %+v, want risk-constitution", snap.policy)
	}
	if got := snap.policy.UnapprovedKeys(); len(got) != 0 {
		t.Fatalf("fully specified file reports unapproved keys: %v", got)
	}
}

func TestRiskPolicyManagerRejectsRetiredCanaryInventoryPin(t *testing.T) {
	t.Parallel()
	text := validRiskPolicyTOML + `
[inventory.canary]
id = "active-v1"
version = "risk-policy-v1"
`
	manager, _ := newTestRiskPolicyManager(t, text)
	snapshot := manager.snapshot()
	if snapshot.status != rpc.RiskPolicyStatusError || !strings.Contains(snapshot.message, "unknown risk policy key(s): inventory.canary") {
		t.Fatalf("retired pin snapshot status=%s message=%q", snapshot.status, snapshot.message)
	}
}

func TestRiskPolicyManagerLoadsV3AndRejectsV3KeyUnderV2(t *testing.T) {
	m, _ := newTestRiskPolicyManager(t, validRiskPolicyV3TOML())
	snap := m.snapshot()
	if snap.status != rpc.RiskPolicyStatusActive || snap.policy == nil || snap.policy.PolicyVersion != 3 || snap.policy.Recon.MaxEquityDivergencePct == nil {
		t.Fatalf("v3 snapshot = %+v", snap)
	}
	v2WithKey := strings.Replace(validRiskPolicyTOML, "max_report_age_days = 4", "max_report_age_days = 4\nmax_equity_divergence_pct = 1.0", 1)
	m, _ = newTestRiskPolicyManager(t, v2WithKey)
	snap = m.snapshot()
	if snap.status != rpc.RiskPolicyStatusError || !strings.Contains(snap.message, "requires policy_version >= 3") {
		t.Fatalf("v2 key snapshot status=%s message=%q", snap.status, snap.message)
	}
}

func TestRiskPolicyManagerRejectsUnknownKeys(t *testing.T) {
	m, _ := newTestRiskPolicyManager(t, validRiskPolicyTOML+"\nsurprise_key = true\n")
	snap := m.snapshot()
	if snap.status != rpc.RiskPolicyStatusError {
		t.Fatalf("status = %s, want error", snap.status)
	}
	if !strings.Contains(snap.message, "unknown risk policy key") {
		t.Fatalf("message = %q, want unknown-key error", snap.message)
	}
}

func TestRiskPolicyManagerRejectsHardEnforcement(t *testing.T) {
	m, _ := newTestRiskPolicyManager(t, strings.Replace(validRiskPolicyTOML, `"shadow"`, `"hard"`, 1))
	snap := m.snapshot()
	if snap.status != rpc.RiskPolicyStatusError || !strings.Contains(snap.message, "not promotable") {
		t.Fatalf("status = %s message = %q, want error/not promotable", snap.status, snap.message)
	}
}

// Editing the file without bumping policy_version is drift: the last good
// policy stays active and the change is refused until a version bump.
func TestRiskPolicyManagerDriftWithoutVersionBump(t *testing.T) {
	m, path := newTestRiskPolicyManager(t, validRiskPolicyTOML)
	edited := strings.Replace(validRiskPolicyTOML, "declared_risk_capital = 50000.0", "declared_risk_capital = 90000.0", 1)
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	m.reload()
	snap := m.snapshot()
	if snap.status != rpc.RiskPolicyStatusDrift {
		t.Fatalf("status = %s, want drift", snap.status)
	}
	if got := *snap.policy.Capital.DeclaredRiskCapital; got != 50000 {
		t.Fatalf("active declared = %v, drifted file must not take effect", got)
	}

	bumped := strings.Replace(edited, "policy_version = 1", "policy_version = 2", 1)
	if err := os.WriteFile(path, []byte(bumped), 0o600); err != nil {
		t.Fatal(err)
	}
	m.reload()
	snap = m.snapshot()
	if snap.status != rpc.RiskPolicyStatusActive || *snap.policy.Capital.DeclaredRiskCapital != 90000 {
		t.Fatalf("after version bump: status = %s declared = %v, want active/90000", snap.status, *snap.policy.Capital.DeclaredRiskCapital)
	}
}

// A parse error keeps the last good policy active and discloses the error.
func TestRiskPolicyManagerKeepsLastGoodOnError(t *testing.T) {
	m, path := newTestRiskPolicyManager(t, validRiskPolicyTOML)
	if err := os.WriteFile(path, []byte("kind = ["), 0o600); err != nil {
		t.Fatal(err)
	}
	m.reload()
	snap := m.snapshot()
	if snap.status != rpc.RiskPolicyStatusError {
		t.Fatalf("status = %s, want error", snap.status)
	}
	if snap.policy == nil || snap.policy.PolicyID != "risk-constitution" {
		t.Fatal("last good policy must stay active through a parse error")
	}
	if !strings.Contains(snap.message, "last good policy stays active") {
		t.Fatalf("message %q must disclose last-good retention", snap.message)
	}
}
