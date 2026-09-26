package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
)

func effectiveOpportunityPolicy(p opportunityPolicy) rpc.Fingerprint {
	p.Kind, p.SchemaVersion, p.PolicyVersion, p.Profile = opportunityPolicyKind, 0, 0, ""
	p.Authority.ExerciseReduceOnly = false            // retired; code owns close/reduce-only
	p.Buckets.OptionExercise.AllowNoOptionBid = false // retired; a bid is required
	return effectivePolicyFingerprint(p)
}

func effectiveProtectionPolicy(p protectionPolicy) rpc.Fingerprint {
	// Profile is display text; no evaluator or grant reader consumes it.
	// Every authority field, including pre-authorised buckets, stays in the key.
	p.Kind, p.SchemaVersion, p.PolicyVersion, p.Profile = protectionPolicyKind, 0, 0, ""
	return effectivePolicyFingerprint(p)
}

func effectivePolicyFingerprint(policy any) rpc.Fingerprint {
	raw, err := json.Marshal(policy)
	if err != nil {
		return rpc.Fingerprint{} // invalid data is never an equivalence proof
	}
	sum := sha256.Sum256(raw)
	return rpc.Fingerprint{Version: rpc.EffectivePolicyFingerprintVersion, Key: "sha256:" + hex.EncodeToString(sum[:])}
}

func sameEffectivePolicy(a, b rpc.Fingerprint) bool {
	return a.Version == rpc.EffectivePolicyFingerprintVersion && a.Key != "" && a == b
}

func policyDiagnosticsMessage(diagnostics []rpc.PolicyDiagnostic) string {
	var messages []string
	for _, d := range diagnostics {
		messages = append(messages, d.Key+": "+d.Message)
	}
	return strings.Join(messages, "; ")
}

// Existing public revisions carry vetoes, receipts and reviewed preparations.
// Reuse one only when current settings/evidence prove equivalence. A legacy
// mapping additionally requires the exact old provenance and old hash. Persist
// the effective revision in the same snapshot so this proof survives restart.
func retainedPolicyRevision(effective, legacy, previous, previousEffective string, sameCurrent, exactLegacy bool) string {
	if previous != "" && ((sameCurrent && previousEffective == effective) || (exactLegacy && previousEffective == "" && previous == legacy)) {
		return previous
	}
	return effective
}

func proposalPolicyRevision(previous rpc.TradeProposalSnapshot, status rpc.ProtectionPolicyStatus, sources rpc.TradeProposalSourceFingerprints, scope brokerStateScope, rows []rpc.TradeProposal) (string, string) {
	effective := proposalRevision(status.EffectiveFingerprint, sources, scope, rows)
	legacySources := sources
	legacySources.EffectiveRulebook = rpc.Fingerprint{}
	legacy := proposalRevision(status.Fingerprint, legacySources, scope, rows)
	scoped := previous.Kind == rpc.TradeProposalSnapshotKind && sameBrokerScope(brokerStateScope{Account: previous.AccountID, Mode: previous.AccountMode}, scope) && previous.PolicyID == status.PolicyID
	current := scoped && sameEffectivePolicy(previous.EffectivePolicyFingerprint, status.EffectiveFingerprint)
	exactLegacy := scoped && previous.EffectivePolicyFingerprint.Key == "" && previous.PolicyVersion == status.PolicyVersion && previous.PolicyFingerprint.Key != "" && previous.PolicyFingerprint == status.Fingerprint
	return retainedPolicyRevision(effective, legacy, previous.Revision, previous.EffectiveRevision, current, exactLegacy), effective
}

func opportunityPolicyRevision(previous rpc.OpportunitySnapshot, status rpc.OpportunityPolicyStatus, sources rpc.OpportunitySourceFingerprints, scope brokerStateScope, rows []rpc.Opportunity) (string, string) {
	effective := opportunityRevision(status.EffectiveFingerprint, sources, scope, rows)
	legacy := opportunityRevision(status.Fingerprint, sources, scope, rows)
	scoped := previous.Kind == rpc.OpportunitySnapshotKind && sameBrokerScope(brokerStateScope{Account: previous.AccountID, Mode: previous.AccountMode}, scope) && previous.PolicyID == status.PolicyID
	current := scoped && sameEffectivePolicy(previous.EffectivePolicyFingerprint, status.EffectiveFingerprint)
	exactLegacy := scoped && previous.EffectivePolicyFingerprint.Key == "" && previous.PolicyVersion == status.PolicyVersion && previous.PolicyFingerprint.Key != "" && previous.PolicyFingerprint == status.Fingerprint
	return retainedPolicyRevision(effective, legacy, previous.Revision, previous.EffectiveRevision, current, exactLegacy), effective
}
