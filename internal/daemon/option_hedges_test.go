package daemon

import (
	"context"
	"testing"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// A protection leg used to leave the engine as nothing: no row, no record, so a
// consumer showed the holding with no cue at all. The engine now returns every
// leg it holds as protection beside the proposals, with what it covers and how
// the role was established, and still proposes nothing for it.
func TestOptionHedgesAreRecordedBesideProposals(t *testing.T) {
	policy, now := standingOptionExitPolicy(), optionExitTestTime()
	spyPut := optionExitTestRow()
	spyPut.Symbol, spyPut.Right, spyPut.ConID, spyPut.LocalSymbol, spyPut.Quantity = "SPY", "P", 501, "SPY   260918P00100000", 3
	testPut := optionExitTestRow()
	testPut.Right, testPut.ConID, testPut.LocalSymbol = "P", 502, "TEST  260918P00100000"
	call := optionExitTestRow()
	pos := &rpc.PositionsResult{
		Stocks:  []rpc.PositionView{{Symbol: "TEST", SecType: "STK", ConID: 7, Quantity: 100, Multiplier: 1}},
		Options: []rpc.PositionView{spyPut, testPut, call},
	}
	engine := &proposalEngine{}
	proposals, _, hedges := engine.generateBook(context.Background(), policy, rpc.ProtectionPolicyStatus{}, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now)
	if len(proposals) != 1 || proposals[0].Contract.ConID != call.ConID {
		t.Fatalf("a protection leg produced an exit row, or the directional call lost its review: %+v", proposals)
	}
	if len(hedges) != 2 {
		t.Fatalf("hedges %+v", hedges)
	}
	spy, single := hedges[0], hedges[1]
	if spy.Contract.ConID != 501 || spy.Symbol != "SPY" || spy.SecType != "OPT" || spy.Quantity != 3 || spy.Purpose != "protection" ||
		spy.Covers != "book" || spy.Role != risk.IndexPutRoleUnclassified || spy.RoleEvidence != rpc.OptionHedgeEvidenceUnmeasured ||
		spy.DTE <= 0 || spy.CostBasisPremium != 1 || spy.Mark != 1.52 || spy.MarketValuePctNLV != nil || spy.Detail == "" {
		t.Fatalf("index put hedge misdescribed: %+v", spy)
	}
	if single.Contract.ConID != 502 || single.Covers != "TEST" || single.Role != risk.IndexPutRoleProtection ||
		single.RoleEvidence != rpc.OptionHedgeEvidenceStructural || single.Detail != "This put covers the long TEST stock position." {
		t.Fatalf("covering put misdescribed: %+v", single)
	}
	// generate keeps its contract for every existing caller.
	if again, _ := engine.generate(context.Background(), policy, rpc.ProtectionPolicyStatus{}, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now); len(again) != 1 {
		t.Fatalf("generate changed: %+v", again)
	}
	// A measured protection role and a closed-market deferral name their evidence.
	measured := optionHedgeRecord(spyPut, pos, risk.DefaultRulebookPolicy(), risk.IndexPutRoleProtection, optionExitBookEvidence{}, now)
	if measured.RoleEvidence != rpc.OptionHedgeEvidenceMeasured || measured.Role != risk.IndexPutRoleProtection {
		t.Fatalf("measured role lost: %+v", measured)
	}
	deferred := optionHedgeRecord(spyPut, pos, risk.DefaultRulebookPolicy(), risk.IndexPutRoleUnclassified, optionExitBookEvidence{Closed: true}, now)
	if deferred.RoleEvidence != rpc.OptionHedgeEvidenceClosedMarket {
		t.Fatalf("closed-market deferral lost: %+v", deferred)
	}
	failed := optionHedgeRecord(spyPut, pos, risk.DefaultRulebookPolicy(), risk.IndexPutRoleUnclassified, optionExitBookEvidence{Failure: "exact-contract Greeks unavailable"}, now)
	if failed.RoleEvidence != rpc.OptionHedgeEvidenceUnmeasured || failed.Detail != "Standing policy holds this index put as portfolio protection. Canary's check of the whole book has not completed (exact-contract Greeks unavailable); the standing rule applies." {
		t.Fatalf("measurement failure not named: %+v", failed)
	}
	// The snapshot copy owns its hedge list and pointers.
	value := 1234.5
	snap := rpc.TradeProposalSnapshot{OptionHedges: []rpc.OptionHedge{{Symbol: "SPY", MarketValueBase: &value}}}
	copied := cloneProposalSnapshot(snap)
	*copied.OptionHedges[0].MarketValueBase = 0
	copied.OptionHedges[0].Symbol = "X"
	if *snap.OptionHedges[0].MarketValueBase != 1234.5 || snap.OptionHedges[0].Symbol != "SPY" {
		t.Fatal("snapshot copy shared its hedges with the original")
	}
}
