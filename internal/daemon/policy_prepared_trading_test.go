//go:build trading

package daemon

import (
	"github.com/osauer/canary/v2/internal/rpc"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPreparedProposalSurvivesCosmeticPolicyEditWithoutNewPreview(t *testing.T) {
	rig, prepared, broker, previews := preparedTestRig(t)
	path := rig.server.protectionPolicies.path
	source := string(readPolicyTestFile(t, path))
	source = strings.Replace(source, "ibkr.protection_policy", "canary.protection_policy", 1)
	source = strings.Replace(source, "policy_version = 1", "policy_version = 50\nprofile = \"owner label\"", 1)
	writePolicyTestFile(t, path, source)
	rig.server.protectionPolicies.reload()
	revision := rig.install(rig.stopProposal())
	if revision != prepared.Proposal.Revision {
		t.Fatal("cosmetic policy edit changed reviewed action revision")
	}
	reopenPreparedRig(t, rig)
	out := preparedSubmit(t, rig, prepared)
	if !out.Accepted || broker.count() != 1 || previews.Load() != 1 || !reflect.DeepEqual(out.Place.Draft, prepared.Preview.Draft) {
		t.Fatalf("cosmetic edit/restart changed exact confirmation: accepted=%v blockers=%v calls=%d previews=%d", out.Accepted, out.Blockers, broker.count(), previews.Load())
	}
	retry := preparedSubmit(t, rig, prepared)
	if retry.Accepted || broker.count() != 1 || previews.Load() != 1 {
		t.Fatal("cosmetic edit reminted a consumed authorisation")
	}
}

func TestPreparedProposalMaterialGrantChangeStillRequiresFreshReview(t *testing.T) {
	rig, prepared, broker, previews := preparedTestRig(t)
	path := rig.server.protectionPolicies.path
	source := string(readPolicyTestFile(t, path))
	source = strings.Replace(source, "policy_version = 1", "policy_version = 2", 1)
	source = strings.Replace(source, "[authority]", "[authority]\npre_authorised = [\"trailing_stop\"]", 1)
	writePolicyTestFile(t, path, source)
	rig.server.protectionPolicies.reload()
	if rig.install(rig.stopProposal()) == prepared.Proposal.Revision {
		t.Fatal("a changed real grant was cosmetic")
	}
	out := preparedSubmit(t, rig, prepared)
	if out.Accepted || broker.count() != 0 || previews.Load() != 1 {
		t.Fatal("changed grant reused reviewed action or silently repreviewed")
	}
}

// Reminder readiness never supplies or revokes the separately scoped grant.
func TestAutomaticSubmissionUsesRealGrantDespiteLaterRevisionOrReminderFailure(t *testing.T) {
	for _, broken := range []bool{false, true} {
		name := "current_revision_50"
		if broken {
			name = "retained_policy_reminder_failure"
		}
		t.Run(name, func(t *testing.T) {
			rig := newAutomaticTradingRig(t, automaticTrailingStopAuthority)
			broker := &brokerCallLog{}
			broker.install(rig.server)
			source := strings.Replace(validRiskPolicyV3TOML(), "policy_version = 3", "policy_version = 50", 1)
			path := filepath.Join(t.TempDir(), "risk-policy.toml")
			writePolicyTestFile(t, path, source)
			manager := newRiskPolicyManager(path, time.Second, rig.server.now)
			manager.reload()
			rig.server.riskPolicies = manager
			if broken {
				manager.mu.Lock()
				manager.status = rpc.RiskPolicyStatusError
				manager.mu.Unlock()
			}
			if ready := rig.server.currentNudgeAuthority(rig.now).eligible; ready == broken {
				t.Fatalf("fixture reminder readiness=%v", ready)
			}
			prop := rig.stopProposal()
			revision := rig.install(prop)
			rig.cycle()
			rig.notice(rig.record(prop.Key, revision))
			rig.advance(31 * time.Minute)
			if !broken {
				manager.reload()
			}
			rig.cycle()
			rig.cycle()
			if broker.count() != 1 {
				t.Fatalf("bookkeeping changed automatic submission: calls=%d record=%+v", broker.count(), rig.record(prop.Key, revision))
			}
		})
	}
}

func TestLegacyProposalUpgradePreservesWindowVetoAndConsumedSubmission(t *testing.T) {
	for _, state := range []string{"pending", "vetoed", "submitted"} {
		t.Run(state, func(t *testing.T) {
			rig := newAutomaticTradingRig(t, automaticTrailingStopAuthority)
			broker := &brokerCallLog{}
			broker.install(rig.server)
			policy, status := rig.server.protectionPolicies.Active()
			prop := rig.stopProposal()
			legacy := proposalRevision(status.Fingerprint, rpc.TradeProposalSourceFingerprints{}, rig.scope, []rpc.TradeProposal{prop})
			prop.Revision = legacy
			snap := rpc.TradeProposalSnapshot{Kind: rpc.TradeProposalSnapshotKind, SchemaVersion: rpc.TradeProposalSnapshotSchemaVersion, AsOf: rig.now, Revision: legacy, AccountID: rig.scope.Account, AccountMode: rig.scope.Mode, PolicyID: policy.PolicyID, PolicyVersion: policy.PolicyVersion, PolicyFingerprint: status.Fingerprint, PolicyStatus: status, Proposals: []rpc.TradeProposal{prop}}
			if err := rig.engine.installSnapshot(snap, false); err != nil {
				t.Fatal(err)
			}
			rig.cycle()
			rig.notice(rig.record(prop.Key, legacy))
			switch state {
			case "vetoed":
				if _, err := rig.engine.Veto(t.Context(), rpc.TradeProposalVetoParams{Key: prop.Key, Reason: "owner veto before upgrade", Origin: rpc.OrderOriginHumanTTY}); err != nil {
					t.Fatal(err)
				}
			case "submitted":
				rig.advance(31 * time.Minute)
				rig.cycle()
				if broker.count() != 1 {
					t.Fatal("fixture did not submit")
				}
			}
			before := rig.record(prop.Key, legacy)
			rig.restart()
			if got := rig.install(rig.stopProposal()); got != legacy {
				t.Fatal("provably equivalent upgrade changed public revision")
			}
			path := rig.server.protectionPolicies.path
			source := strings.Replace(string(readPolicyTestFile(t, path)), "policy_version = 1", "policy_version = 50\nprofile = \"cosmetic\"", 1)
			writePolicyTestFile(t, path, source)
			rig.server.protectionPolicies.reload()
			rig.restart()
			if got := rig.install(rig.stopProposal()); got != legacy {
				t.Fatal("restart or cosmetic edit lost proven legacy mapping")
			}
			rig.cycle()
			after := rig.record(prop.Key, legacy)
			if after.State != before.State || !after.SubmitAt.Equal(before.SubmitAt) || !after.NoticedAt.Equal(before.NoticedAt) {
				t.Fatalf("upgrade rewrote authority history: before=%+v after=%+v", before, after)
			}
			switch state {
			case "pending":
				rig.advance(31 * time.Minute)
				rig.cycle()
				if broker.count() != 1 {
					t.Fatal("original pending window failed to submit once")
				}
			case "vetoed":
				rig.advance(time.Hour)
				rig.cycle()
				if broker.count() != 0 {
					t.Fatal("upgrade bypassed owner veto")
				}
			case "submitted":
				if broker.count() != 1 {
					t.Fatal("upgrade repeated consumed submission")
				}
			}
		})
	}
}
