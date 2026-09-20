package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/strategy"
)

func TestOptionExitGenerationDoesNotLoseUnconfirmedHeldOptions(t *testing.T) {
	for _, state := range []string{"missing", "future", "expired"} {
		t.Run(state, func(t *testing.T) {
			pol := enabledOptionExitPolicy()
			pol.Buckets.ThetaHygiene.Enabled = false
			pol.Buckets.RiskReduction.Enabled = false
			pol.Buckets.TrailingStop.StockETF.Enabled = false
			now := optionExitTestTime()
			switch state {
			case "missing":
				pol.Buckets.TrailingStop.Options.DirectionalIntents = nil
			case "future":
				pol.Buckets.TrailingStop.Options.DirectionalIntents[0].ApprovedAt = now.Add(time.Hour)
			case "expired":
				pol.Buckets.TrailingStop.Options.DirectionalIntents[0].ExpiresAt = now
			}
			long := optionExitTestRow()
			short := long
			short.ConID, short.Quantity = 43, -1
			flat := long
			flat.ConID, flat.Quantity = 44, 0
			pos := &rpc.PositionsResult{Options: []rpc.PositionView{long, short, flat}}
			engine := &proposalEngine{}
			proposals, _ := engine.generate(context.Background(), pol, rpc.ProtectionPolicyStatus{}, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now)
			if len(proposals) != 2 {
				t.Fatalf("held options disappeared: got %d reviews, want 2", len(proposals))
			}
			for i, p := range proposals {
				if p.State != rpc.TradeProposalStateBlocked || p.Bucket != rpc.TradeProposalBucketOptionExitReview ||
					p.OptionExit == nil || p.OptionExit.Kind != "review" || p.OptionExit.Intent != "unconfirmed" ||
					!hasTradingBlocker(p.Blockers, "directional_intent_required") {
					t.Fatalf("unconfirmed option became an action or lost its owner task: %+v", p)
				}
				if p.LimitPrice != nil || p.Trail != nil || p.OptionExit.ReferencePrice != nil || p.OptionExit.ReturnPct != nil {
					t.Fatalf("shared quote authorized an unconfirmed exit: %+v", p)
				}
				// No quote was requested for an unconfirmed leg; reporting its
				// absence as a quote failure was an artefact the owner could not clear.
				for _, code := range []string{"live_option_quote_required", "fresh_option_quote_required", "two_sided_option_quote_required", "option_spread_too_wide"} {
					if hasTradingBlocker(p.Blockers, code) {
						t.Fatalf("quote blocker %q invented for a leg whose quote was never requested: %+v", code, p.Blockers)
					}
				}
				for _, b := range p.Blockers {
					if b.Action == "" {
						t.Fatalf("blocker %q has no next step", b.Code)
					}
				}
				if i == 1 && (p.Action != rpc.OrderActionBuy || !hasTradingBlocker(p.Blockers, "long_option_required")) {
					t.Fatalf("short option was represented as a supported sell: %+v", p)
				}
			}
			if counts := proposalCounts(proposals, "USD"); counts.Total != 2 || counts.Actionable != 0 || counts.OptionExitReview != 2 {
				t.Fatalf("blocked reviews count as ready protection: %+v", counts)
			}
			pol.Buckets.TrailingStop.Options.Enabled = false
			proposals, _ = engine.generate(context.Background(), pol, rpc.ProtectionPolicyStatus{}, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now)
			if len(proposals) != 0 {
				t.Fatal("disabled policy generated option work")
			}
		})
	}
}

func TestOptionExitReviewDoesNotInventFlatReturnOrDirectionalIntent(t *testing.T) {
	pol := enabledOptionExitPolicy()
	pol.Buckets.TrailingStop.Options.DirectionalIntents = nil
	row, now := optionExitTestRow(), optionExitTestTime()
	loss := risk.DefaultRulebookPolicy().ExitActLossPct
	decision := evaluateOptionExit(pol.Buckets.TrailingStop.Options, row, now, false, true, true, loss)
	p, ok := optionExitProposal(pol, rpc.ProtectionPolicyStatus{}, row, rpc.TradeProposalSourceFingerprints{}, now, decision, risk.IndexPutRoleDirectional, 0, loss)
	if !ok || p.OptionExit == nil || p.OptionExit.ReferencePrice == nil {
		t.Fatalf("expected blocked review retaining its measured price: %+v", p)
	}
	if p.OptionExit.Intent != "unconfirmed" || p.OptionExit.ReturnPct != nil || p.OptionExit.Kind != "review" || p.State != rpc.TradeProposalStateBlocked {
		t.Fatalf("ineligible measurement became a 0%% return or approved intent: %+v", p)
	}
}

// Two long puts of one underlying are a same-direction stack, not a spread:
// each leg is standalone, and the exact-contract risk evidence still gates it.
func TestOptionExitCoverageRetainsEconomicRoleGateForStandaloneStack(t *testing.T) {
	pol := enabledOptionExitPolicy()
	pol.Buckets.ThetaHygiene.Enabled = false
	pol.Buckets.RiskReduction.Enabled = false
	pol.Buckets.TrailingStop.StockETF.Enabled = false
	now := optionExitTestTime()
	first := optionExitTestRow()
	first.Symbol, first.Right = "SPY", "P"
	second := first
	second.ConID, second.Strike = 43, 105
	intent := pol.Buckets.TrailingStop.Options.DirectionalIntents[0]
	intent.ConID = second.ConID
	pol.Buckets.TrailingStop.Options.DirectionalIntents = append(pol.Buckets.TrailingStop.Options.DirectionalIntents, intent)
	pos := &rpc.PositionsResult{Options: []rpc.PositionView{first, second}}
	pos.Strategies, pos.StrategyIssues = strategy.InferPositionStrategies(pos.Options)
	engine := &proposalEngine{}
	proposals, _ := engine.generate(context.Background(), pol, rpc.ProtectionPolicyStatus{}, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now)
	if len(pos.Strategies) != 0 || len(pos.StrategyIssues) != 0 {
		t.Fatalf("a same-direction put stack was grouped: %+v %+v", pos.Strategies, pos.StrategyIssues)
	}
	if len(proposals) != 2 {
		t.Fatalf("standalone options disappeared: got %d reviews", len(proposals))
	}
	for _, p := range proposals {
		if p.OptionExit.Intent != "directional" || p.State != rpc.TradeProposalStateBlocked ||
			hasTradingBlocker(p.Blockers, "standalone_option_required") ||
			!hasTradingBlocker(p.Blockers, "directional_role_not_confirmed") ||
			p.OptionExit.EconomicRole != risk.IndexPutRoleUnclassified || p.OptionExit.ExitManagement != "standalone" || p.Trail != nil {
			t.Fatalf("a standalone leg was grouped, or the exact risk evidence gate was lost: %+v", p)
		}
	}
}
