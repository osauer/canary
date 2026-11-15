//go:build trading

package daemon

import (
	"testing"

	"github.com/osauer/canary/v3/internal/daemon/corestore"
	"github.com/osauer/canary/v3/internal/rpc"
)

// The real submit path must re-read the keep list even when the scheduler
// already captured a due record. Every broker call here goes to the fake wire.
func TestBudgetHoldingLocksPreventSendAfterNotice(t *testing.T) {
	for _, state := range []string{"unlocked", "locked", "unreadable"} {
		t.Run(state, func(t *testing.T) {
			rig := newAutomaticTradingRig(t, `pre_authorised = ["budget_reduction"]`)
			broker := &brokerCallLog{}
			broker.install(rig.server)
			rows, _ := (&proposalEngine{}).budgetReductionProposals(budgetTestPolicy(rpc.BudgetReductionModeActive, 20, 15), rpc.ProtectionPolicyStatus{}, budgetLatchedInput(), nil, budgetTestBook(), rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, rig.now)
			prop := budgetRowsByConID(rows)[602]
			rig.server.orderPreviewPositionImpact = fixedPreviewPosition(4, 0, rpc.OrderPositionEffectClose)
			revision := rig.install(prop)
			rig.engine.reconcileAutomatic(t.Context())
			rec := rig.record(prop.Key, revision)
			rig.notice(rec)
			if state == "locked" {
				if _, err := replaceHoldingLocks(t, rig.server, []rpc.HoldingLock{{ConID: prop.Contract.ConID, Symbol: "SYN", Role: rpc.HoldingLockRoleCore}}, 0, "budget-lock-after-notice"); err != nil {
					t.Fatal(err)
				}
			} else if state == "unreadable" {
				if _, err := rig.core.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{ScopeKey: daemonStateScope, Kind: stateKindHoldingLocks, JSON: []byte(`{"version":1,"locks":null}`)}); err != nil {
					t.Fatal(err)
				}
			}
			// Exercise submission directly: a stale scheduler decision must be safe
			// without relying on the next refresh or reconciliation pass.
			rig.engine.submitAutomatic(t.Context(), rec)
			want := 0
			if state == "unlocked" {
				want = 1
			}
			if got := broker.count(); got != want {
				t.Fatalf("broker sends=%d want=%d; record=%+v", got, want, rig.record(prop.Key, revision))
			}
			got := rig.record(prop.Key, revision)
			if state != "unlocked" && got.PreviewTokenID != "" {
				t.Fatal("locked budget progressed past admission to a preview")
			}
		})
	}
}
