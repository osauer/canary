package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestConstitutionRevisionsDoNotSelectCurrentReminderOrCapitalSemantics(t *testing.T) {
	var effective string
	for _, tc := range []struct{ schema, revision int }{{1, 4}, {1, 5}, {1, 50}, {2, 1}, {2, 50}} {
		t.Run(fmt.Sprintf("schema%d_revision%d", tc.schema, tc.revision), func(t *testing.T) {
			source := strings.Replace(validRiskPolicyV3TOML(), "policy_version = 3", fmt.Sprintf("policy_version = %d", tc.revision), 1)
			source = strings.Replace(source, "schema_version = 1", fmt.Sprintf("schema_version = %d", tc.schema), 1)
			s := newRiskPolicyTestServer(t, source)
			a := s.currentNudgeAuthority(time.Now())
			if !a.eligible || !a.confirmedFlowEligible || !a.cadenceEligible || a.policyHealth.Status != rpc.NudgeInputStatusOK {
				t.Fatalf("current constitution lost reminder readiness: %+v", a)
			}
			if !a.policy.Semantics().StatementReconciliation {
				t.Fatal("lost statement accounting")
			}
			key := a.policy.EffectiveFingerprintKey()
			if effective == "" {
				effective = key
			} else if key != effective {
				t.Fatal("revision/format changed effective settings")
			}
			// File health affects reminders, not access to retained capital settings.
			s.riskPolicies.mu.Lock()
			s.riskPolicies.status = rpc.RiskPolicyStatusError
			s.riskPolicies.mu.Unlock()
			if s.currentNudgeAuthority(time.Now()).eligible {
				t.Fatal("bad file appeared healthy")
			}
			if s.acceptedRiskPolicy(time.Now()).policy != a.policy {
				t.Fatal("reminder health concealed accepted capital settings")
			}
		})
	}
	for _, revision := range []int{1, 2, 3} {
		c := testConstitution()
		c.PolicyVersion = revision
		if c.Semantics().ProcessReminders || c.Semantics().StatementReconciliation != (revision == 3) {
			t.Fatalf("legacy revision %d changed meaning", revision)
		}
	}
}

func TestRiskPolicyCosmeticAdoptionAndMaterialDrift(t *testing.T) {
	source := strings.Replace(validRiskPolicyV3TOML(), "policy_version = 3", "policy_version = 5", 1)
	m, path := newTestRiskPolicyManager(t, source)
	before := m.snapshot().policy.EffectiveFingerprintKey()
	source = strings.Replace(source, "ibkr.risk_policy", "canary.risk_policy", 1)
	writePolicyTestFile(t, path, source+"\n# reviewed label only\n")
	m.reload()
	if got := m.snapshot(); got.status != rpc.RiskPolicyStatusActive || got.policy.EffectiveFingerprintKey() != before {
		t.Fatal("cosmetic change became drift")
	}
	changed := strings.Replace(source, "protected_floor = 200000.0", "protected_floor = 210000.0", 1)
	writePolicyTestFile(t, path, changed)
	m.reload()
	if got := m.snapshot(); got.status != rpc.RiskPolicyStatusDrift || got.policy.EffectiveFingerprintKey() != before {
		t.Fatal("same-revision risk change was adopted")
	}
	writePolicyTestFile(t, path, strings.Replace(changed, "policy_version = 5", "policy_version = 6", 1))
	m.reload()
	if got := m.snapshot(); got.status != rpc.RiskPolicyStatusActive || got.policy.EffectiveFingerprintKey() == before {
		t.Fatal("material revision not adopted")
	}
}

func TestOpportunitySchemaPresenceAndLegacyConversion(t *testing.T) {
	legacy := `kind = "ibkr.opportunity_policy"
schema_version = 1
policy_id = "synthetic-opportunity"
policy_version = 7
profile = "legacy-label"
[authority]
exercise_reduce_only = false
auto_submit = false
[buckets.option_exercise]
enabled = true
allow_no_option_bid = true
`
	p, err := parseOpportunityPolicy([]byte(legacy))
	if err != nil {
		t.Fatal(err)
	}
	if p.Buckets.OptionExercise.RequireRTH || p.Buckets.OptionExercise.RequireAmericanStyle || p.Buckets.OptionExercise.MinTotalGain != 0 || p.Buckets.OptionExercise.MaxQuoteAge != "30s" || len(p.diagnostics) == 0 {
		t.Fatal("legacy omissions changed")
	}
	k := policyFileKind{name: PolicyFileOpportunity, migrate: migrateOpportunityPolicyFile}
	converted, _, _, err := previewPolicyFileMigration(k, []byte(legacy), "test")
	if err != nil {
		t.Fatal(err)
	}
	after, err := parseOpportunityPolicy(converted)
	if err != nil {
		t.Fatal(err)
	}
	if after.SchemaVersion != 2 || !sameEffectivePolicy(effectiveOpportunityPolicy(p), effectiveOpportunityPolicy(after)) {
		t.Fatal("conversion changed effective legacy choices")
	}
	template := string(OpportunityPolicyTemplate("test"))
	for _, key := range []string{"min_total_gain", "min_gain_pct_intrinsic", "require_rth", "max_quote_age", "require_american_style"} {
		t.Run(key, func(t *testing.T) {
			lines := strings.Split(template, "\n")
			var kept []string
			for _, line := range lines {
				if !strings.HasPrefix(line, key+" =") {
					kept = append(kept, line)
				}
			}
			if _, err := parseOpportunityPolicy([]byte(strings.Join(kept, "\n"))); err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("missing enabled field %s accepted: %v", key, err)
			}
		})
	}
	for _, bad := range []string{
		strings.Replace(template, "schema_version = 2", "schema_version = 99", 1),
		strings.Replace(template, "canary.opportunity_policy", "canary.risk_policy", 1),
		strings.Replace(template, "min_total_gain = 25", "min_total_gain = nan", 1),
		strings.Replace(template, `max_quote_age = "30s"`, `max_quote_age = "nonsense"`, 1),
		strings.Replace(legacy, "auto_submit = false", "auto_submit = true", 1),
		strings.Replace(template, "require_rth = true", "require_rthh = true", 1),
	} {
		if _, err := parseOpportunityPolicy([]byte(bad)); err == nil {
			t.Fatalf("invalid policy accepted:\n%s", bad)
		}
	}
	if _, err := parseOpportunityPolicy([]byte("schema_version = 2\npolicy_id = \"disabled-detector\"\npolicy_version = 1\n[buckets.option_exercise]\nenabled = false\n")); err != nil {
		t.Fatalf("explicitly disabled detector: %v", err)
	}
}

func TestOpportunityCosmeticEditsPreserveSnapshotAndRemovedFileRetainsSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opportunity.toml")
	text := string(OpportunityPolicyTemplate("test"))
	writePolicyTestFile(t, path, text)
	m := newOpportunityPolicyManager(path, true, time.Second, time.Now)
	m.reload()
	before, status := m.Active()
	snapshot := rpc.OpportunitySnapshot{PolicyID: before.PolicyID, PolicyVersion: before.PolicyVersion, PolicyFingerprint: status.Fingerprint, EffectivePolicyFingerprint: status.EffectiveFingerprint}
	edited := strings.Replace(text, "canary.opportunity_policy", "ibkr.opportunity_policy", 1)
	edited = strings.Replace(edited, "policy_version = 1", "policy_version = 50\nprofile = \"another label\"", 1)
	writePolicyTestFile(t, path, edited)
	m.reload()
	_, current := m.Active()
	if current.Status != rpc.OpportunityPolicyStatusActive || !sameOpportunityPolicy(snapshot, current) {
		t.Fatal("cosmetic edit invalidated current opportunity")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	m.reload()
	held, st := m.Active()
	if st.Status != rpc.OpportunityPolicyStatusDrift || !sameEffectivePolicy(effectiveOpportunityPolicy(held), status.EffectiveFingerprint) {
		t.Fatal("removed file replaced active settings")
	}
}

func TestNudgeIdentityMigrationKeepsCounterAcrossCosmeticEditAndRestart(t *testing.T) {
	c := testConstitutionV3()
	c.PolicyVersion = 5
	c.Kind = "ibkr.risk_policy"
	path := filepath.Join(t.TempDir(), "nudges.json")
	store := &nudgeStateStore{path: path, now: time.Now}
	old := opaqueIdentity("risk-policy", c.FingerprintKey())
	if err := store.recordShadow(old, "synthetic-latch", true, false, true); err != nil {
		t.Fatal(err)
	}
	if err := store.migratePolicyIdentity(c); err != nil {
		t.Fatal(err)
	}
	c.Kind = risk.ConstitutionKind
	c.PolicyVersion = 50
	c.SchemaVersion = 2
	reopened := &nudgeStateStore{path: path, now: time.Now}
	if _, count := reopened.shadowObservation(nudgePolicyIdentity(c), "synthetic-latch", true); count != 1 {
		t.Fatal("cosmetic edit/restart lost shadow observation")
	}
	c.Capital.ProtectedFloor = new(123456.0)
	if _, count := reopened.shadowObservation(nudgePolicyIdentity(c), "synthetic-latch", true); count != 0 {
		t.Fatal("materially different policy inherited counter")
	}
}

func TestDrawdownRecoveryDoesNotDependOnReminderRevision(t *testing.T) {
	for _, tc := range []struct{ schema, revision int }{{1, 5}, {1, 50}, {2, 1}} {
		t.Run(fmt.Sprintf("schema%d_revision%d", tc.schema, tc.revision), func(t *testing.T) {
			store, c, now := recoveryStore(t)
			c.SchemaVersion, c.PolicyVersion = tc.schema, tc.revision
			if err := store.IncorporateStatementSnapshotForScope(statementCapitalSnapshot{Scope: testLiveObserveScope, CoverageTo: *now}, c); err != nil {
				t.Fatal(err)
			}
			*now = now.Add(time.Minute)
			store.Observe(250000, *now, c, testLiveObserveScope, true)
			if store.state.BlockLatched || recoveryEvents(t, store) != 1 || store.state.AdjustedPeakBase != 260000 {
				t.Fatal("revision interfered with authorised automatic brake release")
			}
		})
	}
}

func TestLegacyPolicyRevisionReuseRequiresExactScopedEvidence(t *testing.T) {
	policy := defaultProtectionPolicy()
	status := protectionPolicyStatus(policy, rpc.ProtectionPolicyStatusActive, "file", "", time.Now())
	scope := brokerStateScope{Account: "SYNTHETIC", Mode: rpc.AccountModePaper}
	rows := []rpc.TradeProposal{{Key: "synthetic", Quantity: 1, PositionEffect: "protect"}}
	rulebook := risk.DefaultRulebookPolicy()
	legacyRulebook := rpc.Fingerprint{Version: rpc.RulebookPolicyFingerprintVersion, Key: rulebook.FingerprintKey()}
	sources := rpc.TradeProposalSourceFingerprints{Rulebook: &legacyRulebook, EffectiveRulebook: rpc.Fingerprint{Version: rpc.EffectivePolicyFingerprintVersion, Key: rulebook.EffectiveFingerprintKey()}}
	legacySources := sources
	legacySources.EffectiveRulebook = rpc.Fingerprint{}
	legacy := proposalRevision(status.Fingerprint, legacySources, scope, rows)
	old := rpc.TradeProposalSnapshot{Kind: rpc.TradeProposalSnapshotKind, Revision: legacy, AccountID: scope.Account, AccountMode: scope.Mode, PolicyID: policy.PolicyID, PolicyVersion: policy.PolicyVersion, PolicyFingerprint: status.Fingerprint}
	if got, _ := proposalPolicyRevision(old, status, sources, scope, rows); got != legacy {
		t.Fatal("exact legacy proof did not preserve identity")
	}
	for _, defect := range []string{"account", "mode", "id", "revision", "fingerprint", "effective_tag", "evidence", "grant"} {
		t.Run(defect, func(t *testing.T) {
			previous, current, currentSources := old, status, sources
			switch defect {
			case "account":
				previous.AccountID = "DIFFERENT"
			case "mode":
				previous.AccountMode = rpc.AccountModeLive
			case "id":
				previous.PolicyID = "different"
			case "revision":
				previous.PolicyVersion++
			case "fingerprint":
				previous.PolicyFingerprint.Key = "unproven"
			case "effective_tag":
				previous.EffectivePolicyFingerprint = rpc.Fingerprint{Version: "unknown", Key: "unproven"}
			case "evidence":
				currentSources.Positions = &rpc.Fingerprint{Version: "synthetic", Key: "new book"}
			case "grant":
				changed := policy
				changed.Authority.PreAuthorised = []string{preAuthorisedBucketTrailingStop}
				current.EffectiveFingerprint = effectiveProtectionPolicy(changed)
				current.Fingerprint = fingerprintProtectionPolicy(changed)
			}
			got, effective := proposalPolicyRevision(previous, current, currentSources, scope, rows)
			if got == legacy || got != effective {
				t.Fatal("unproven or materially changed legacy decision was reused")
			}
		})
	}
}

func TestLegacyOpportunityRevisionSurvivesCosmeticEditButNotChangedEvidence(t *testing.T) {
	policy := defaultOpportunityPolicy()
	policy.Kind, policy.SchemaVersion = "ibkr.opportunity_policy", 1
	status := opportunityPolicyStatus(policy, rpc.OpportunityPolicyStatusActive, "file", "", time.Now())
	scope := brokerStateScope{Account: "SYNTHETIC", Mode: rpc.AccountModePaper}
	rows := []rpc.Opportunity{{Key: "synthetic", Quantity: 1, PositionEffect: rpc.ExercisePositionEffectReduce, ExpectedGain: 30}}
	sources := rpc.OpportunitySourceFingerprints{Positions: &rpc.Fingerprint{Version: "synthetic", Key: "unchanged book"}}
	legacy := opportunityRevision(status.Fingerprint, sources, scope, rows)
	old := rpc.OpportunitySnapshot{Kind: rpc.OpportunitySnapshotKind, Revision: legacy, AccountID: scope.Account, AccountMode: scope.Mode, PolicyID: policy.PolicyID, PolicyVersion: policy.PolicyVersion, PolicyFingerprint: status.Fingerprint}
	revision, effective := opportunityPolicyRevision(old, status, sources, scope, rows)
	if revision != legacy {
		t.Fatal("exact legacy opportunity lost its reviewed identity")
	}
	old.EffectiveRevision, old.EffectivePolicyFingerprint = effective, status.EffectiveFingerprint
	raw, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	var restored rpc.OpportunitySnapshot
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	policy.Kind, policy.SchemaVersion, policy.PolicyVersion, policy.Profile = opportunityPolicyKind, 2, 50, "display label"
	status = opportunityPolicyStatus(policy, rpc.OpportunityPolicyStatusActive, "file", "", time.Now())
	if got, _ := opportunityPolicyRevision(restored, status, sources, scope, rows); got != legacy {
		t.Fatal("cosmetic edit or restart invalidated the opportunity")
	}
	rows[0].ExpectedGain++
	if got, _ := opportunityPolicyRevision(restored, status, sources, scope, rows); got == legacy {
		t.Fatal("changed economics reused the old reviewed identity")
	}
	rows[0].ExpectedGain--
	scope.Account = "DIFFERENT"
	if got, _ := opportunityPolicyRevision(restored, status, sources, scope, rows); got == legacy {
		t.Fatal("changed account reused the old reviewed identity")
	}
}
