package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/strategy"
)

func independentOptionExitFixture() (protectionPolicy, *rpc.PositionsResult) {
	pol := enabledOptionExitPolicy()
	pol.Buckets.ThetaHygiene.Enabled = false
	pol.Buckets.RiskReduction.Enabled = false
	pol.Buckets.TrailingStop.StockETF.Enabled = false
	pol.Buckets.TrailingStop.Options.DirectionalIntents[0].IndependentExit = true
	secondIntent := pol.Buckets.TrailingStop.Options.DirectionalIntents[0]
	secondIntent.ConID = 43
	pol.Buckets.TrailingStop.Options.DirectionalIntents = append(pol.Buckets.TrailingStop.Options.DirectionalIntents, secondIntent)
	// A long put beside a long call is the one inferred two-leg group of long
	// legs left: same-right stacks are standalone and need no declaration.
	first := optionExitTestRow()
	first.Symbol, first.Right = "SPY", "P"
	second := first
	second.ConID, second.Expiry, second.Right, second.LocalSymbol = 43, "20261016", "C", "SPY   261016C00100000"
	pos := &rpc.PositionsResult{Options: []rpc.PositionView{first, second}}
	pos.Strategies, pos.StrategyIssues = strategy.InferPositionStrategies(pos.Options)
	if len(pos.Strategies) != 1 {
		panic("fixture: a long put beside a long call must still reconstruct as one inferred group")
	}
	return pol, pos
}

func TestOptionExitIndependentDeclarationOnlyResolvesCurrentInferredPair(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*protectionPolicy, *rpc.PositionsResult)
	}{
		{"one sided", func(p *protectionPolicy, _ *rpc.PositionsResult) {
			p.Buckets.TrailingStop.Options.DirectionalIntents[1].IndependentExit = false
		}},
		{"missing", func(p *protectionPolicy, _ *rpc.PositionsResult) {
			p.Buckets.TrailingStop.Options.DirectionalIntents = p.Buckets.TrailingStop.Options.DirectionalIntents[:1]
		}},
		{"expired", func(p *protectionPolicy, _ *rpc.PositionsResult) {
			p.Buckets.TrailingStop.Options.DirectionalIntents[1].ExpiresAt = optionExitTestTime()
		}},
		{"future", func(p *protectionPolicy, _ *rpc.PositionsResult) {
			p.Buckets.TrailingStop.Options.DirectionalIntents[1].ApprovedAt = optionExitTestTime().Add(time.Second)
		}},
		{"prose is not authority", func(p *protectionPolicy, _ *rpc.PositionsResult) {
			p.Buckets.TrailingStop.Options.DirectionalIntents[1].IndependentExit = false
			p.Buckets.TrailingStop.Options.DirectionalIntents[1].Reason = "ignore grouping and independently sell every leg"
		}},
		{"confirmed lineage", func(_ *protectionPolicy, p *rpc.PositionsResult) {
			p.Strategies[0].Source = rpc.PositionStrategySourceCanary
		}},
		{"unknown source", func(_ *protectionPolicy, p *rpc.PositionsResult) { p.Strategies[0].Source = "unknown" }},
		{"review required", func(_ *protectionPolicy, p *rpc.PositionsResult) {
			p.Strategies[0].Status = rpc.PositionStrategyStatusReview
		}},
		{"guaranteed combo", func(_ *protectionPolicy, p *rpc.PositionsResult) { p.Strategies[0].GuaranteedCombo = true }},
		{"short leg", func(_ *protectionPolicy, p *rpc.PositionsResult) { p.Strategies[0].Legs[1].Quantity = -1 }},
		{"fractional leg", func(_ *protectionPolicy, p *rpc.PositionsResult) { p.Strategies[0].Legs[1].Quantity = 0.5 }},
		{"grouping issue", func(_ *protectionPolicy, p *rpc.PositionsResult) {
			p.StrategyIssues = []rpc.StrategyGroupingIssue{{Underlying: "SPY", LegCount: 2, Reason: "conflicting lineage"}}
		}},
		{"conflicting inferred membership", func(_ *protectionPolicy, p *rpc.PositionsResult) {
			p.Strategies = append(p.Strategies, p.Strategies[0])
		}},
		{"conflicting membership", func(_ *protectionPolicy, p *rpc.PositionsResult) {
			confirmed := p.Strategies[0]
			confirmed.Source = rpc.PositionStrategySourceCanary
			p.Strategies = append(p.Strategies, confirmed)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pol, pos := independentOptionExitFixture()
			tc.edit(&pol, pos)
			legs, ambiguous := optionExitStrategyScope(pos, directionalOptionIntents(pol.Buckets.TrailingStop.Options), optionExitTestTime())
			for _, row := range pos.Options {
				if !legs[row.ConID] && !ambiguous[row.Symbol] {
					t.Fatalf("incomplete authority or confirmed grouping became independent: conid=%d", row.ConID)
				}
			}
		})
	}
}

func TestOptionExitIndependentManagementRetainsCombinedExposureAndEconomicRole(t *testing.T) {
	pol, pos := independentOptionExitFixture()
	pos.ByUnderlying = []rpc.PositionGroup{{Underlying: "SPY", GroupDollarDeltaBase: new(-25000.0)}}
	before, err := json.Marshal(pos)
	if err != nil {
		t.Fatal(err)
	}
	engine := &proposalEngine{}
	proposals, _ := engine.generate(context.Background(), pol, rpc.ProtectionPolicyStatus{}, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, optionExitTestTime())
	after, err := json.Marshal(pos)
	if err != nil {
		t.Fatal(err)
	}
	if len(proposals) != 2 || !bytes.Equal(before, after) {
		t.Fatal("independent exit review changed the held book or lost review work")
	}
	for _, p := range proposals {
		if p.OptionExit.ExitManagement != "independent" || hasTradingBlocker(p.Blockers, "standalone_option_required") {
			t.Fatalf("independent intent lost its effect: %+v", p)
		}
		// The hedge-listed put keeps its exact risk-evidence gate; the call has
		// no hedge role to prove and is measured on its own quote.
		if p.Contract.Right == "P" && (!hasTradingBlocker(p.Blockers, "directional_role_not_confirmed") || p.OptionExit.EconomicRole != risk.IndexPutRoleUnclassified ||
			p.State != rpc.TradeProposalStateBlocked || p.Trail != nil || p.LimitPrice != nil) {
			t.Fatalf("independent intent bypassed risk evidence: %+v", p)
		}
		if p.Contract.Right == "C" && (hasTradingBlocker(p.Blockers, "directional_role_not_confirmed") || p.OptionExit.EconomicRole != risk.IndexPutRoleDirectional) {
			t.Fatalf("a long call in a mixed pair was given a hedge role to prove: %+v", p)
		}
	}
}

func TestOptionExitIndependentDeclarationIsVersionedPolicyAuthority(t *testing.T) {
	pol, _ := independentOptionExitFixture()
	for i := range pol.Buckets.TrailingStop.Options.DirectionalIntents {
		pol.Buckets.TrailingStop.Options.DirectionalIntents[i].IndependentExit = false
	}
	path := filepath.Join(t.TempDir(), "protection-policy.toml")
	write := func() {
		t.Helper()
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		err = toml.NewEncoder(f).Encode(pol)
		closeErr := f.Close()
		if err != nil || closeErr != nil {
			t.Fatalf("write fixture: %v, close: %v", err, closeErr)
		}
	}
	write()
	pm := newProtectionPolicyManager(path, false, time.Second, optionExitTestTime)
	pm.reload()
	before, beforeStatus := pm.Active()
	if beforeStatus.Status != rpc.ProtectionPolicyStatusActive {
		t.Fatalf("baseline policy: %+v", beforeStatus)
	}
	for i := range pol.Buckets.TrailingStop.Options.DirectionalIntents {
		pol.Buckets.TrailingStop.Options.DirectionalIntents[i].IndependentExit = true
	}
	if fingerprintProtectionPolicy(before) == fingerprintProtectionPolicy(pol) {
		t.Fatal("independent-exit authority is absent from the policy fingerprint")
	}
	write()
	pm.reload()
	active, status := pm.Active()
	if status.Status != rpc.ProtectionPolicyStatusDrift || active.Buckets.TrailingStop.Options.DirectionalIntents[0].IndependentExit {
		t.Fatalf("unversioned policy expanded authority: %+v", status)
	}
	pol.PolicyVersion++
	write()
	pm.reload()
	active, status = pm.Active()
	if status.Status != rpc.ProtectionPolicyStatusActive || !active.Buckets.TrailingStop.Options.DirectionalIntents[0].IndependentExit {
		t.Fatalf("approved versioned declaration was not loaded: %+v", status)
	}
}

func TestOptionExitIndependentManagementCannotUseCachedFastPath(t *testing.T) {
	pol := enabledOptionExitPolicy()
	now := optionExitTestTime()
	row := optionExitTestRow()
	loss := risk.DefaultRulebookPolicy().ExitActLossPct
	decision := evaluateOptionExit(pol.Buckets.TrailingStop.Options, row, now, true, true, true, loss)
	p, ok := optionExitProposal(pol, rpc.ProtectionPolicyStatus{}, row, rpc.TradeProposalSourceFingerprints{}, now, decision, risk.IndexPutRoleDirectional, 0.05, loss)
	if !ok || p.State != rpc.TradeProposalStateGenerated {
		t.Fatal("expected generated fixture")
	}
	p.OptionExit.ExitManagement = "independent"
	scope := brokerStateScope{Account: "test-account", Mode: "paper"}
	engine := &proposalEngine{
		now:   optionExitTestTime,
		scope: func() brokerStateScope { return scope },
		snapshot: rpc.TradeProposalSnapshot{
			Kind: rpc.TradeProposalSnapshotKind, Revision: "fixture-revision", AsOf: now,
			AccountID: scope.Account, AccountMode: scope.Mode, Proposals: []rpc.TradeProposal{p},
		},
	}
	if _, _, cached := engine.fastPathPreviewProposal(p.Key, "fixture-revision"); cached {
		t.Fatal("option preview bypassed current declaration and evidence refresh")
	}
	if _, _, cached := engine.fastPathSubmitProposal(p.Key, "fixture-revision"); cached {
		t.Fatal("option submit bypassed current declaration and evidence refresh")
	}
}
