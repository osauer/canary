package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestOpportunityPolicyDefaultDriftAndHigherVersion(t *testing.T) {
	now := time.Date(2026, 6, 12, 15, 0, 0, 0, time.UTC)
	pm := newOpportunityPolicyManager("", true, time.Second, func() time.Time { return now })
	pm.reload()
	_, st := pm.Active()
	if st.Status != rpc.OpportunityPolicyStatusDefault {
		t.Fatalf("default status=%q, want default", st.Status)
	}

	path := filepath.Join(t.TempDir(), "opportunity-policy.toml")
	pm.path = path
	writeOpportunityPolicy(t, path, 1, 35)
	pm.reload()
	_, st = pm.Active()
	if st.Status != rpc.OpportunityPolicyStatusDrift {
		t.Fatalf("same-version changed policy status=%q, want drift", st.Status)
	}
	if len(st.Blockers) == 0 {
		t.Fatal("drifted policy should expose blockers")
	}

	writeOpportunityPolicy(t, path, 2, 35)
	pm.reload()
	p, st := pm.Active()
	if st.Status != rpc.OpportunityPolicyStatusActive || p.PolicyVersion != 2 {
		t.Fatalf("higher version status=%q version=%d, want active v2", st.Status, p.PolicyVersion)
	}
}

func TestOpportunityPolicyDefaultsValidationAndFingerprint(t *testing.T) {
	t.Parallel()
	policy := defaultOpportunityPolicy()
	if policy.Kind != opportunityPolicyKind || policy.SchemaVersion != 1 {
		t.Fatalf("default identity = %q/%d", policy.Kind, policy.SchemaVersion)
	}
	if policy.Authority.ExerciseReduceOnly || policy.Authority.AutoSubmit {
		t.Fatalf("default authority = %+v, want no exposure-effect gate and no auto-submit", policy.Authority)
	}
	if !policy.Buckets.OptionExercise.Enabled {
		t.Fatal("default option_exercise bucket disabled")
	}
	if policy.Buckets.OptionExercise.AllowNoOptionBid {
		t.Fatal("default option_exercise allow_no_option_bid = true, want false")
	}
	if got, err := policy.Buckets.OptionExercise.maxQuoteAgeDuration(); err != nil || got != 30*time.Second {
		t.Fatalf("default max quote age = %v err=%v, want 30s", got, err)
	}
	if err := validateOpportunityPolicy(policy); err != nil {
		t.Fatalf("default policy invalid: %v", err)
	}

	changed := policy
	changed.Buckets.OptionExercise.MinTotalGain++
	if fingerprintOpportunityPolicy(changed).Key == fingerprintOpportunityPolicy(policy).Key {
		t.Fatal("changing option_exercise thresholds must change policy fingerprint")
	}

	invalid := policy
	invalid.Authority.ExerciseReduceOnly = true
	if err := validateOpportunityPolicy(invalid); err != nil {
		t.Fatalf("legacy exercise_reduce_only flag should be accepted for compatibility: %v", err)
	}
	invalid = policy
	invalid.Authority.AutoSubmit = true
	if err := validateOpportunityPolicy(invalid); err == nil || !strings.Contains(err.Error(), "auto_submit") {
		t.Fatalf("invalid auto-submit err=%v, want auto_submit", err)
	}
	invalid = policy
	invalid.Buckets.OptionExercise.MaxQuoteAge = "0s"
	if err := validateOpportunityPolicy(invalid); err == nil || !strings.Contains(err.Error(), "max_quote_age") {
		t.Fatalf("invalid max quote age err=%v, want max_quote_age", err)
	}
}

func TestOpportunityPolicyFileDefaultsBucket(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "opportunity-policy.toml")
	body := `kind = "ibkr.opportunity_policy"
schema_version = 1
policy_id = "custom-opportunity"
policy_version = 2

[authority]
exercise_reduce_only = true
auto_submit = false
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	pm := newOpportunityPolicyManager(path, false, time.Second, time.Now)
	pm.reload()
	p, st := pm.Active()
	if st.Status != rpc.OpportunityPolicyStatusActive {
		t.Fatalf("policy status=%q message=%q, want active", st.Status, st.Message)
	}
	if p.Profile != "custom-opportunity" {
		t.Fatalf("profile=%q, want policy_id default", p.Profile)
	}
	if !p.Buckets.OptionExercise.Enabled || p.Buckets.OptionExercise.MinTotalGain != defaultOpportunityPolicy().Buckets.OptionExercise.MinTotalGain {
		t.Fatalf("option_exercise defaults not applied: %+v", p.Buckets.OptionExercise)
	}
}

func writeOpportunityPolicy(t *testing.T, path string, version int, minGain float64) {
	t.Helper()
	body := fmt.Sprintf(`kind = "ibkr.opportunity_policy"
schema_version = 1
policy_id = "opportunity-option-exercise-mvp"
policy_version = %d
profile = "conservative-exercise-mvp"

[authority]
exercise_reduce_only = true
auto_submit = false

[buckets.option_exercise]
enabled = true
min_total_gain = %.2f
min_gain_pct_intrinsic = 0.5
require_rth = true
max_quote_age = "30s"
allow_no_option_bid = true
require_american_style = true
`, version, minGain)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
