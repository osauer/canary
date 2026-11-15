package daemon

import (
	"strings"
	"testing"

	"github.com/osauer/canary/v3/internal/daemon/corestore"
	"github.com/osauer/canary/v3/internal/rpc"
)

func TestBudgetHoldingLocksRetainExposureAndSelectUnlockedLines(t *testing.T) {
	for _, basis := range []string{rpc.BudgetBasisDeclaredRiskCapital, rpc.BudgetBasisRulebook} {
		t.Run(basis, func(t *testing.T) {
			s, _, _ := watchlistServer(t)
			e := &proposalEngine{server: s}
			policy, input, book, locked := budgetTestPolicy(rpc.BudgetReductionModeActive, 20, 15), budgetLatchedInput(), budgetTestBook(), 602
			if basis == rpc.BudgetBasisRulebook {
				policy, input, book, locked = budgetPlanPolicy(rpc.BudgetReductionModeActive, 1e9), budgetPlanInput(), budgetPlanBook(), 701
			}
			run := func() ([]rpc.TradeProposal, *rpc.TradeProposalBudgetStatus) {
				return e.budgetReductionProposals(policy, rpc.ProtectionPolicyStatus{}, input, nil, book, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, optionExitTestTime())
			}
			before, statusBefore := run()
			if _, ok := budgetRowsByConID(before)[locked]; !ok {
				t.Fatal("precondition: unlocked line is selected")
			}
			if _, err := replaceHoldingLocks(t, s, []rpc.HoldingLock{{ConID: locked, Symbol: "LOCKED", Role: rpc.HoldingLockRoleCore}}, 0, "budget-lock"); err != nil {
				t.Fatal(err)
			}
			rows, status := run()
			if _, ok := budgetRowsByConID(rows)[locked]; ok {
				t.Error("budget sweep selected a locked holding")
			}
			if len(rows) == 0 {
				t.Error("other unlocked holdings still require reduction")
			}
			if status.MeasuredPremiumBase == nil || *status.MeasuredPremiumBase != *statusBefore.MeasuredPremiumBase {
				t.Error("locked premium disappeared from measured exposure")
			}
			for _, candidate := range status.Candidates {
				if candidate.Contract.ConID == locked {
					t.Error("locked holding remains a ranked candidate")
				}
			}
			for _, order := range status.Plan {
				if order.Contract.ConID == locked {
					t.Error("locked holding remains in the plan")
				}
			}
		})
	}
}

func TestBudgetHoldingLocksUnreadableFailsClosed(t *testing.T) {
	s, core, _ := watchlistServer(t)
	if _, err := core.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{ScopeKey: daemonStateScope, Kind: stateKindHoldingLocks, JSON: []byte(`{"version":1,"locks":null}`)}); err != nil {
		t.Fatal(err)
	}
	e := &proposalEngine{server: s}
	rows, status := e.budgetReductionProposals(budgetTestPolicy(rpc.BudgetReductionModeActive, 20, 15), rpc.ProtectionPolicyStatus{}, budgetLatchedInput(), nil, budgetTestBook(), rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, optionExitTestTime())
	if len(rows) != 0 || len(status.Plan) != 0 || len(status.Candidates) != 0 || !strings.Contains(status.Reason, "locked holdings") {
		t.Fatalf("unreadable keep list did not hold budget sweep: rows=%d status=%+v", len(rows), status)
	}
	if status.MeasuredPremiumBase == nil || *status.MeasuredPremiumBase != 28000 {
		t.Fatal("unreadable locks must not erase measured exposure")
	}
}

func TestBudgetHoldingLocksAutomaticAdmissionAndRevalidation(t *testing.T) {
	for _, unreadable := range []bool{false, true} {
		name := "locked"
		if unreadable {
			name = "unreadable"
		}
		t.Run(name, func(t *testing.T) {
			rig := newAutomaticTestRig(t, `pre_authorised = ["budget_reduction", "trailing_stop"]`)
			rig.server.coreStore = rig.core
			rows, _ := (&proposalEngine{}).budgetReductionProposals(budgetTestPolicy(rpc.BudgetReductionModeActive, 20, 15), rpc.ProtectionPolicyStatus{}, budgetLatchedInput(), nil, budgetTestBook(), rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, rig.now)
			prop := rows[0]
			revision := rig.install(prop)
			rig.cycle()
			rec := rig.record(prop.Key, revision)
			rig.notice(rec)
			prop.Revision = revision
			rig.server.automaticGrant.Store(&automaticWriteGrant{Key: prop.Key, Revision: revision, Bucket: preAuthorisedBucketBudgetReduction, Quantity: prop.Quantity})
			defer rig.server.automaticGrant.Store(nil)
			if blockers := rig.engine.automaticSubmitBlockers(prop); len(blockers) != 0 {
				t.Fatalf("precondition: unlocked budget refused: %+v", blockers)
			}
			wantCode := "holding_locked"
			if unreadable {
				wantCode = "holding_locks_unavailable"
				if _, err := rig.core.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{ScopeKey: daemonStateScope, Kind: stateKindHoldingLocks, JSON: []byte(`{"version":1,"locks":null}`)}); err != nil {
					t.Fatal(err)
				}
			} else if _, err := replaceHoldingLocks(t, rig.server, []rpc.HoldingLock{{ConID: prop.Contract.ConID, Symbol: "SYN", Role: rpc.HoldingLockRoleCore}}, 0, "budget-after-notice"); err != nil {
				t.Fatal(err)
			}
			if bucket := rig.engine.automaticBucket(prop); bucket != "" {
				t.Error("locked or unreadable budget remains automatically admissible")
			}
			if blockers := rig.engine.automaticSubmitBlockers(prop); len(blockers) == 0 || blockers[0].Code != wantCode {
				t.Errorf("post-notice submission blockers=%+v, want %s", blockers, wantCode)
			}
			protective := rig.stopProposal()
			protective.Contract.ConID = prop.Contract.ConID
			if rig.engine.automaticBucket(protective) != preAuthorisedBucketTrailingStop {
				t.Error("budget lock changed protective stop authority")
			}
			rig.engine.reconcileAutomatic(t.Context())
			if got := rig.record(prop.Key, revision); got.State != rpc.TradeProposalAutomaticSuperseded {
				t.Errorf("waiting budget notice remains executable: %s", got.State)
			}
		})
	}
}
