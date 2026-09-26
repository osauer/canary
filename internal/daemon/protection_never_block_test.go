package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// A file broken since start and then repaired, even at version 1, is adopted;
// before, the embedded default held it back as drift until a restart. A file
// removed after a broken start returns to the defaults, not drift.
func TestProtectionPolicyRepairedAfterBrokenStartIsAdopted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "protection-policy.toml")
	if err := os.WriteFile(path, []byte("kind = \"ibkr.protection_policy\"\nschema_version = 1\npolicy_id = broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	pm := newProtectionPolicyManager(path, true, time.Second, time.Now)
	pm.reload()
	if st := pm.Status(); st.Status != rpc.ProtectionPolicyStatusError {
		t.Fatalf("broken start = %+v", st)
	}
	writePolicy(t, path, 1, 7)
	pm.reload()
	active, st := pm.Active()
	if st.Status != rpc.ProtectionPolicyStatusActive || active.Buckets.ThetaHygiene.MinAbsThetaPerDay != 7 {
		t.Fatalf("repaired file at version 1 = %+v (theta %v), want adopted", st, active.Buckets.ThetaHygiene.MinAbsThetaPerDay)
	}

	other := filepath.Join(dir, "other.toml")
	if err := os.WriteFile(other, []byte("not toml ["), 0o600); err != nil {
		t.Fatal(err)
	}
	pm = newProtectionPolicyManager(other, true, time.Second, time.Now)
	pm.reload()
	if err := os.Remove(other); err != nil {
		t.Fatal(err)
	}
	pm.reload()
	if st := pm.Status(); st.Status != rpc.ProtectionPolicyStatusDefault {
		t.Fatalf("file removed after a broken start = %+v, want default", st)
	}
}

// The option-exercise policy follows the same rule: a repaired file is
// adopted at any version after a broken start.
func TestOpportunityPolicyRepairedAfterBrokenStartIsAdopted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opportunity-policy.toml")
	if err := os.WriteFile(path, []byte("not toml ["), 0o600); err != nil {
		t.Fatal(err)
	}
	pm := newOpportunityPolicyManager(path, true, time.Second, time.Now)
	pm.reload()
	if st := pm.Status(); st.Status != rpc.OpportunityPolicyStatusError {
		t.Fatalf("broken start = %+v", st)
	}
	body := "kind = \"ibkr.opportunity_policy\"\nschema_version = 1\npolicy_id = \"opportunity-option-exercise-mvp\"\npolicy_version = 1\n\n[buckets.option_exercise]\nenabled = true\nmin_total_gain = 40.0\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	pm.reload()
	active, st := pm.Active()
	if st.Status != rpc.OpportunityPolicyStatusActive || active.Buckets.OptionExercise.MinTotalGain != 40 {
		t.Fatalf("repaired file = %+v (gain %v), want adopted", st, active.Buckets.OptionExercise.MinTotalGain)
	}
}

// A drifted or unreadable protection file never blocks risk reduction: the
// reduce-only proposal already staged stays previewable by hand and no policy
// blocker reaches the auto-trade status, while pre-authorised submission
// pauses.
func TestBrokenProtectionPolicyNeverBlocksReduceOnlyWork(t *testing.T) {
	rig := newAutomaticTestRig(t, `pre_authorised = ["trailing_stop"]`)
	prop := rig.stopProposal()
	path := rig.server.protectionPolicies.path
	if err := os.WriteFile(path, []byte("kind = \"ibkr.protection_policy\"\nschema_version = 1\npolicy_id = \"protection-mvp\"\npolicy_version = 2\n[authority]\nclose_reduce_only = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rig.server.protectionPolicies.reload()
	st := rig.server.protectionPolicies.Status()
	if st.Status != rpc.ProtectionPolicyStatusError || !st.AutomationPaused {
		t.Fatalf("broken file status = %+v", st)
	}
	revision := rig.install(prop)
	if _, blockers, handled := rig.engine.fastPathPreviewProposal(prop.Key, revision); handled && hasPolicyBlocker(blockers) {
		t.Fatalf("a broken policy file blocked a reduce-only preview: %+v", blockers)
	}
	if auto := rig.server.autoTradeStatus(); hasPolicyBlocker(auto.Blockers) {
		t.Fatalf("a broken policy file added an auto-trade blocker that gates manual work: %+v", auto.Blockers)
	}
	if _, ok := rig.engine.automaticPolicy(); ok {
		t.Fatal("pre-authorised submission must pause while the policy file is unreadable")
	}
	rig.cycle()
	rig.noRecord(prop.Key, revision)
}

func hasPolicyBlocker(blockers []rpc.TradingBlocker) bool {
	for _, b := range blockers {
		if strings.HasPrefix(b.Code, "policy_") {
			return true
		}
	}
	return false
}
