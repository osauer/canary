package daemon

import (
	"context"
	"encoding/json"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

func standingOptionExitPolicy() protectionPolicy {
	p := enabledOptionExitPolicy()
	p.Buckets.ThetaHygiene.Enabled = false
	p.Buckets.RiskReduction.Enabled = false
	p.Buckets.TrailingStop.StockETF.Enabled = false
	p.Buckets.TrailingStop.Options.DirectionalIntents = nil
	p.Buckets.TrailingStop.Options.DefaultLongCallsDirectional = true
	p.Buckets.TrailingStop.Options.DefaultIndexPutsProtection = true
	return p
}

func TestOptionExitStandingPurposePreservesExceptions(t *testing.T) {
	for name, change := range map[string]func(*protectionPolicy, *rpc.PositionsResult){
		"expired override": func(p *protectionPolicy, _ *rpc.PositionsResult) {
			p.Buckets.TrailingStop.Options.DirectionalIntents = []protectionOptionDirectionalIntent{{ConID: 42, ApprovedAt: optionExitTestTime().Add(-time.Hour), ExpiresAt: optionExitTestTime()}}
		},
		"future override": func(p *protectionPolicy, _ *rpc.PositionsResult) {
			p.Buckets.TrailingStop.Options.DirectionalIntents = []protectionOptionDirectionalIntent{{ConID: 42, ApprovedAt: optionExitTestTime().Add(time.Hour), ExpiresAt: optionExitTestTime().Add(2 * time.Hour)}}
		},
		"confirmed strategy": func(_ *protectionPolicy, pos *rpc.PositionsResult) {
			pos.Strategies = []rpc.PositionStrategy{{Source: rpc.PositionStrategySourceCanary, Legs: []rpc.PositionStrategyLeg{{Contract: rpc.ContractParams{ConID: 42}}}}}
		},
		"inferred strategy": func(_ *protectionPolicy, pos *rpc.PositionsResult) {
			pos.Strategies = []rpc.PositionStrategy{{Source: rpc.PositionStrategySourceInferred, Legs: []rpc.PositionStrategyLeg{{Contract: rpc.ContractParams{ConID: 42}}}}}
		},
		"ambiguous strategy": func(_ *protectionPolicy, pos *rpc.PositionsResult) {
			pos.StrategyIssues = []rpc.StrategyGroupingIssue{{Underlying: "TEST"}}
		},
		"invalid book": func(_ *protectionPolicy, pos *rpc.PositionsResult) {
			pos.Stocks = []rpc.PositionView{{Symbol: "OTHER", Quantity: math.NaN()}}
		},
		"adjusted contract": func(_ *protectionPolicy, pos *rpc.PositionsResult) { pos.Options[0].Multiplier = 10 },
		"missing identity":  func(_ *protectionPolicy, pos *rpc.PositionsResult) { pos.Options[0].ConID = 0 },
		"short call":        func(_ *protectionPolicy, pos *rpc.PositionsResult) { pos.Options[0].Quantity = -1 },
		"fractional call":   func(_ *protectionPolicy, pos *rpc.PositionsResult) { pos.Options[0].Quantity = 1.5 },
		"ordinary put":      func(_ *protectionPolicy, pos *rpc.PositionsResult) { pos.Options[0].Right = "P" },
		"disabled default": func(p *protectionPolicy, _ *rpc.PositionsResult) {
			p.Buckets.TrailingStop.Options.DefaultLongCallsDirectional = false
		},
	} {
		t.Run(name, func(t *testing.T) {
			policy, pos := standingOptionExitPolicy(), &rpc.PositionsResult{Options: []rpc.PositionView{optionExitTestRow()}}
			change(&policy, pos)
			legs, ambiguous := optionExitStrategyScope(pos, directionalOptionIntents(policy.Buckets.TrailingStop.Options), optionExitTestTime())
			if got := optionExitPurpose(policy.Buckets.TrailingStop.Options, pos.Options[0], pos, legs, ambiguous, optionExitTestTime()); got != "unconfirmed" {
				t.Fatalf("exception became %q", got)
			}
		})
	}
}

// A short position somewhere else in the book turned every long call into an
// open question. A call hedges only what it can cover: a short stock of its own
// underlying, or, for an index call, a short stock anywhere. Those are hedges;
// everything else is directional, with no declaration involved.
func TestOptionExitStandingCallHedgesOnlyAShortItCanCover(t *testing.T) {
	for name, tc := range map[string]struct {
		symbol string
		stocks []rpc.PositionView
		others []rpc.PositionView
		want   string
	}{
		"short stock elsewhere":      {"TEST", []rpc.PositionView{{Symbol: "OTHER", Quantity: -1}}, nil, "directional"},
		"short option elsewhere":     {"TEST", nil, []rpc.PositionView{{Symbol: "OTHER", SecType: "OPT", ConID: 77, Right: "P", Quantity: -1, Multiplier: 100}}, "directional"},
		"short stock same name":      {"TEST", []rpc.PositionView{{Symbol: "TEST", Quantity: -100}}, nil, "protection"},
		"index call over short book": {"SPY", []rpc.PositionView{{Symbol: "OTHER", Quantity: -1}}, nil, "protection"},
		"index call over long book":  {"SPY", []rpc.PositionView{{Symbol: "OTHER", Quantity: 1}}, nil, "directional"},
		"long book":                  {"TEST", []rpc.PositionView{{Symbol: "OTHER", Quantity: 1}}, nil, "directional"},
	} {
		t.Run(name, func(t *testing.T) {
			policy := standingOptionExitPolicy()
			row := optionExitTestRow()
			row.Symbol = tc.symbol
			pos := &rpc.PositionsResult{Stocks: tc.stocks, Options: append([]rpc.PositionView{row}, tc.others...)}
			legs, ambiguous := optionExitStrategyScope(pos, directionalOptionIntents(policy.Buckets.TrailingStop.Options), optionExitTestTime())
			if got := optionExitPurpose(policy.Buckets.TrailingStop.Options, pos.Options[0], pos, legs, ambiguous, optionExitTestTime()); got != tc.want {
				t.Fatalf("purpose %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOptionExitStandingCallFetchesExactQuoteWithoutManualDeclaration(t *testing.T) {
	f, pos, now := newOptionEvidenceFixture()
	row := optionExitTestRow()
	pos.Options[0] = row
	f.scope.Positions[1].Contract = *previewIBKRContract(proposalContractFromPosition(row, "OPT"))
	f.models[row.ConID].Contract = f.scope.Positions[1].Contract
	f.models[row.ConID].Delta = new(0.5)
	engine := &proposalEngine{server: &Server{}, optionExitSource: f, now: func() time.Time { return now }}
	proposals, _ := engine.generate(context.Background(), standingOptionExitPolicy(), rpc.ProtectionPolicyStatus{}, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now)
	if len(proposals) != 1 {
		t.Fatalf("got %d proposals", len(proposals))
	}
	p := proposals[0]
	if p.OptionExit == nil || p.OptionExit.Intent != "directional" || p.OptionExit.Kind != risk.OptionExitActionLoss || p.OptionExit.ReferencePrice == nil || *p.OptionExit.ReferencePrice != 0.35 || hasTradingBlocker(p.Blockers, "directional_intent_required") {
		t.Fatalf("standing call did not reach exact-quote loss assessment: %+v", p)
	}
	if p.Quantity != 2 || p.Action != rpc.OrderActionSell || p.PositionEffect != rpc.OrderPositionEffectClose {
		t.Fatal("default changed close-only sizing")
	}
	// The same authority cannot make missing/delayed/wide data executable.
	f.readErr = context.DeadlineExceeded
	proposals, _ = engine.generate(context.Background(), standingOptionExitPolicy(), rpc.ProtectionPolicyStatus{}, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now)
	if len(proposals) != 1 || proposals[0].State != rpc.TradeProposalStateBlocked || proposals[0].OptionExit.Kind != "review" {
		t.Fatal("default bypassed exact quote failure")
	}
}

// An index put the measured book does not need as a hedge is a directional
// position. The measurement settles its purpose; the standing default remains
// the fallback while nothing can be measured.
func TestOptionExitStandingIndexPutAdoptsMeasuredDirectionalRole(t *testing.T) {
	f, pos, now := newOptionEvidenceFixture()
	engine := &proposalEngine{server: &Server{}, optionExitSource: f, now: func() time.Time { return now }}
	proposals, _ := engine.generate(context.Background(), standingOptionExitPolicy(), rpc.ProtectionPolicyStatus{}, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now)
	if len(proposals) != 1 || proposals[0].OptionExit.Intent != "directional" || proposals[0].OptionExit.EconomicRole != risk.IndexPutRoleDirectional {
		t.Fatalf("measured directional role was not adopted: %+v", proposals)
	}
	for _, code := range []string{"option_purpose_conflict", "directional_intent_required", "directional_role_not_confirmed", "standalone_option_required"} {
		if hasTradingBlocker(proposals[0].Blockers, code) {
			t.Fatalf("measured role still raised %s: %+v", code, proposals[0].Blockers)
		}
	}
	if !slices.ContainsFunc(proposals[0].Details, func(d string) bool { return strings.Contains(d, "classifies it as directional, which applies") }) {
		t.Fatalf("the adopted measurement is not explained on the record: %+v", proposals[0].Details)
	}
	// A missing current classification is no permission to exit a standing hedge.
	f.readErr = context.DeadlineExceeded
	proposals, _ = engine.generate(context.Background(), standingOptionExitPolicy(), rpc.ProtectionPolicyStatus{}, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now)
	if len(proposals) != 0 {
		t.Fatal("standing protection became routine directional review without contradictory evidence")
	}
	f.readErr = nil
	pol := risk.DefaultRulebookPolicy()
	band := 2 * math.Max(pol.RegimeCalm.HedgeBandMaxPct, math.Max(pol.RegimeEarlyWarning.HedgeBandMaxPct, pol.RegimeConfirmed.HedgeBandMaxPct))
	pos.Stocks[0].Quantity = 15000 / (band / 100) / 100
	f.scope.Positions[0].Position = pos.Stocks[0].Quantity
	proposals, _ = engine.generate(context.Background(), standingOptionExitPolicy(), rpc.ProtectionPolicyStatus{}, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now)
	if len(proposals) != 0 {
		t.Fatal("confirmed portfolio protection became routine directional exit work")
	}
}

func TestOptionExitStandingPolicyFingerprintAndExactOverride(t *testing.T) {
	old := enabledOptionExitPolicy()
	data, err := json.Marshal(old.Buckets.TrailingStop.Options)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "default_long_calls") || strings.Contains(string(data), "default_index_puts") {
		t.Fatal("disabled defaults changed legacy fingerprint projection")
	}
	p := old
	p.Buckets.TrailingStop.Options.DefaultLongCallsDirectional = true
	if fingerprintProtectionPolicy(p) == fingerprintProtectionPolicy(old) {
		t.Fatal("standing call policy not fingerprinted")
	}
	p = old
	p.Buckets.TrailingStop.Options.DefaultIndexPutsProtection = true
	if fingerprintProtectionPolicy(p) == fingerprintProtectionPolicy(old) {
		t.Fatal("standing hedge policy not fingerprinted")
	}
	var cfg protectionTrailOptionPolicy
	if _, err := toml.Decode("default_long_calls_directional = true\ndefault_index_puts_protection = true", &cfg); err != nil || !cfg.DefaultLongCallsDirectional || !cfg.DefaultIndexPutsProtection {
		t.Fatal("policy does not decode defaults")
	}
	pos := &rpc.PositionsResult{Options: []rpc.PositionView{optionExitTestRow()}}
	pos.Options[0].Symbol, pos.Options[0].Right = "SPY", "P"
	if got := optionExitPurpose(p.Buckets.TrailingStop.Options, pos.Options[0], pos, nil, nil, optionExitTestTime()); got != "directional" {
		t.Fatal("standing hedge default replaced explicit current override")
	}
}
