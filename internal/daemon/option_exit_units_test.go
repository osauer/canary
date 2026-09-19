package daemon

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/strategy"
)

func unitTestLeg(conID int, symbol, expiry, right string, strike, quantity, avgCost float64) rpc.PositionView {
	return rpc.PositionView{Symbol: symbol, SecType: "OPTION", ConID: conID, Exchange: "SMART", Currency: "USD",
		LocalSymbol: fmt.Sprintf("%-6s%s%s%08d", symbol, expiry[2:], right, int(strike*1000)), TradingClass: symbol,
		Quantity: quantity, Multiplier: 100, AvgCost: avgCost, Mark: avgCost / 100, MarketValue: quantity * avgCost,
		Expiry: expiry, Strike: strike, Right: right}
}

func unitQuote(now time.Time, bid, ask float64) rpc.OrderQuoteSnapshot {
	return rpc.OrderQuoteSnapshot{Bid: new(bid), Ask: new(ask), DataType: rpc.MarketDataLive, PriceAt: now, SessionContext: &rpc.MarketSession{IsOpen: true}}
}

// unitFixture holds a long/short call vertical on an ordinary underlying: two
// long 100 calls paid 3.00 per share against two short 110 calls received
// 1.00, so the unit paid 2.00 net per share. Leg quotes are exact per contract.
func unitFixture(t *testing.T, longBid, longAsk, shortBid, shortAsk float64) (*proposalEngine, *optionEvidenceFixture, protectionPolicy, *rpc.PositionsResult, time.Time) {
	t.Helper()
	f, _, now := newOptionEvidenceFixture()
	long := unitTestLeg(42, "TEST", "20261016", "C", 100, 2, 300)
	short := unitTestLeg(43, "TEST", "20261016", "C", 110, -2, 100)
	pos := &rpc.PositionsResult{Options: []rpc.PositionView{long, short}}
	pos.Strategies, pos.StrategyIssues = strategy.InferPositionStrategies(pos.Options)
	if len(pos.Strategies) != 1 || pos.Strategies[0].Kind != "vertical" || pos.Strategies[0].Units != 2 {
		t.Fatalf("fixture did not reconstruct a vertical: %+v %+v", pos.Strategies, pos.StrategyIssues)
	}
	f.prices = map[int]rpc.OrderQuoteSnapshot{42: unitQuote(now, longBid, longAsk), 43: unitQuote(now, shortBid, shortAsk)}
	engine := &proposalEngine{server: &Server{}, optionExitSource: f, now: func() time.Time { return now }}
	return engine, f, standingOptionExitPolicy(), pos, now
}

// A spread used to produce one blocked review per leg, each asking for a
// declaration. The unit is one position: net premium paid against net close
// value at fresh leg quotes, closed as one combo.
func TestUnitExitClosesAVerticalOnNetPremiumLoss(t *testing.T) {
	engine, _, pol, pos, now := unitFixture(t, 0.50, 0.55, 0.20, 0.22)
	proposals, _ := engine.generate(context.Background(), pol, rpc.ProtectionPolicyStatus{}, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now)
	if len(proposals) != 1 {
		t.Fatalf("expected one unit row and no per-leg rows: %+v", proposals)
	}
	p := proposals[0]
	if p.Bucket != rpc.TradeProposalBucketStrategyExit || p.SecType != "BAG" || p.Symbol != "TEST" || p.State != rpc.TradeProposalStateGenerated || p.Unit == nil || p.OptionExit == nil ||
		p.OptionExit.Kind != risk.OptionExitActionLoss || p.OptionExit.ExitManagement != "unit" || p.OptionExit.Readiness != "ready" || p.Quantity != 2 || len(p.Blockers) != 0 {
		t.Fatalf("unit loss exit not measured: %+v", p)
	}
	// close = long bid 0.50 − short ask 0.22 = 0.28 per share against 2.00 paid: −86%.
	if len(p.Unit.Legs) != 2 || !p.Unit.ComboRoute || p.Unit.StrategyID != pos.Strategies[0].ID || math.Abs(p.Unit.CostPerShare-2) > 1e-9 ||
		p.Unit.CloseValuePerShare == nil || math.Abs(*p.Unit.CloseValuePerShare-0.28) > 1e-9 || p.OptionExit.ReturnPct == nil || math.Abs(*p.OptionExit.ReturnPct+86) > 1e-6 {
		t.Fatalf("unit valuation wrong: %+v", p.Unit)
	}
	if !strings.Contains(p.Unit.Route, "canary strategies close "+pos.Strategies[0].ID) {
		t.Fatalf("unit lost its combo route: %q", p.Unit.Route)
	}
	for _, code := range []string{"directional_intent_required", "standalone_option_required", "long_option_required"} {
		if hasTradingBlocker(p.Blockers, code) {
			t.Fatalf("unit raised a per-leg question: %s", code)
		}
	}
	// The single-leg order path refuses a unit; its route is the answer.
	if blockers := unitProposalOrderBlockers(p); len(blockers) != 1 || blockers[0].Code != "strategy_workflow_required" || blockers[0].Action != p.Unit.Route {
		t.Fatalf("unit reached the single-leg order path: %+v", blockers)
	}
	if counts := proposalCounts(proposals, "USD"); counts.StrategyExit != 1 || counts.Actionable != 1 {
		t.Fatalf("unit not counted: %+v", counts)
	}
}

func TestUnitExitProfitTrailArmsThenTakesFromHighWater(t *testing.T) {
	engine, f, pol, pos, now := unitFixture(t, 3.50, 3.55, 0.20, 0.22)
	cfg := pol.Buckets.TrailingStop.Options
	proposals, _ := engine.generate(context.Background(), pol, rpc.ProtectionPolicyStatus{}, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now)
	if len(proposals) != 1 {
		t.Fatalf("expected one armed unit row: %+v", proposals)
	}
	armed := proposals[0]
	// close = 3.50 − 0.22 = 3.28 on 2.00 paid: +64%, armed but not retraced.
	if armed.State != rpc.TradeProposalStateBlocked || armed.OptionExit.Kind != "review" || !hasTradingBlocker(armed.Blockers, "unit_profit_trail_armed") ||
		armed.Unit.HighWaterPerShare == nil || math.Abs(*armed.Unit.HighWaterPerShare-3.28) > 1e-9 || armed.Unit.ProfitTrailStop == nil {
		t.Fatalf("armed trail not recorded: %+v", armed)
	}
	// The last published row carries a higher high water forward.
	carried := armed
	unit := *armed.Unit
	hwm := 4.0
	unit.HighWaterPerShare = &hwm
	carried.Unit = &unit
	engine.snapshot = rpc.TradeProposalSnapshot{Proposals: []rpc.TradeProposal{carried}}
	spread := 0.07
	trail := math.Max(hwm*cfg.DefaultPct/100, math.Max(cfg.MinTrailAbs, cfg.SpreadMultiple*spread))
	stop := hwm - trail
	if (stop/2-1)*100 < cfg.LockedGainPct {
		t.Fatalf("fixture cannot lock the approved gain: stop %.2f", stop)
	}
	shortBid, shortAsk := 0.10, 0.12
	longBid := stop - 0.05 + shortAsk // close lands 0.05 below the stop
	f.prices = map[int]rpc.OrderQuoteSnapshot{42: unitQuote(now, longBid, longBid+0.05), 43: unitQuote(now, shortBid, shortAsk)}
	proposals, _ = engine.generate(context.Background(), pol, rpc.ProtectionPolicyStatus{}, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now)
	if len(proposals) != 1 {
		t.Fatalf("expected one unit row: %+v", proposals)
	}
	take := proposals[0]
	if take.State != rpc.TradeProposalStateGenerated || take.OptionExit.Kind != optionExitUnitProfitTake || len(take.Blockers) != 0 ||
		take.Unit.HighWaterPerShare == nil || math.Abs(*take.Unit.HighWaterPerShare-hwm) > 1e-9 || take.Unit.ProfitTrailStop == nil || math.Abs(*take.Unit.ProfitTrailStop-stop) > 1e-6 {
		t.Fatalf("retrace from the carried high water did not close the unit: %+v", take)
	}
}

func TestUnitExitLeavesNetCreditUnitsToReview(t *testing.T) {
	engine, _, pol, pos, now := unitFixture(t, 0.50, 0.55, 0.20, 0.22)
	pos.Options[0].AvgCost, pos.Options[1].AvgCost = 100, 300 // received more than paid
	proposals, _ := engine.generate(context.Background(), pol, rpc.ProtectionPolicyStatus{}, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now)
	if len(proposals) != 1 || proposals[0].State != rpc.TradeProposalStateBlocked || !hasTradingBlocker(proposals[0].Blockers, "unit_long_premium_required") || proposals[0].OptionExit.Kind != "review" {
		t.Fatalf("a net-credit unit was measured with long-premium rules: %+v", proposals)
	}
}

// Three legs of one underlying have no single decomposition, so they were left
// standalone with a grouping issue and no rule at all. They are one unit.
func TestUnitExitCoversAnAmbiguousUnderlyingAsOneUnit(t *testing.T) {
	f, _, now := newOptionEvidenceFixture()
	pos := &rpc.PositionsResult{Options: []rpc.PositionView{
		unitTestLeg(42, "TEST", "20261016", "C", 100, 1, 300),
		unitTestLeg(43, "TEST", "20261016", "C", 110, -1, 100),
		unitTestLeg(44, "TEST", "20261120", "C", 120, 1, 50),
	}}
	pos.Strategies, pos.StrategyIssues = strategy.InferPositionStrategies(pos.Options)
	if len(pos.Strategies) != 0 || len(pos.StrategyIssues) != 1 {
		t.Fatalf("fixture is not ambiguous: %+v %+v", pos.Strategies, pos.StrategyIssues)
	}
	f.prices = map[int]rpc.OrderQuoteSnapshot{42: unitQuote(now, 0.50, 0.55), 43: unitQuote(now, 0.20, 0.22), 44: unitQuote(now, 0.05, 0.06)}
	engine := &proposalEngine{server: &Server{}, optionExitSource: f, now: func() time.Time { return now }}
	proposals, _ := engine.generate(context.Background(), standingOptionExitPolicy(), rpc.ProtectionPolicyStatus{}, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now)
	if len(proposals) != 1 {
		t.Fatalf("expected one unit row for the ambiguous underlying: %+v", proposals)
	}
	p := proposals[0]
	// net paid 3.00 − 1.00 + 0.50 = 2.50; close 0.50 − 0.22 + 0.05 = 0.33.
	if p.Unit == nil || len(p.Unit.Legs) != 3 || p.Unit.ComboRoute || p.Unit.StrategyID != "" || p.Unit.Units != 1 || math.Abs(p.Unit.CostPerShare-2.5) > 1e-9 ||
		p.Unit.CloseValuePerShare == nil || math.Abs(*p.Unit.CloseValuePerShare-0.33) > 1e-9 || p.OptionExit.Kind != risk.OptionExitActionLoss || !strings.Contains(p.Unit.Route, "no combo route") {
		t.Fatalf("ambiguous underlying was not evaluated as one unit: %+v", p)
	}
}

// A hedge-listed put spread whose role cannot be measured is protection, like
// a standalone hedge: no exit row, and no per-leg question either.
func TestUnitExitKeepsAnUnmeasuredIndexPutSpreadAsProtection(t *testing.T) {
	f, _, now := newOptionEvidenceFixture()
	pos := &rpc.PositionsResult{Options: []rpc.PositionView{
		unitTestLeg(42, "SPY", "20261016", "P", 100, 1, 300),
		unitTestLeg(43, "SPY", "20261016", "P", 90, -1, 100),
	}}
	pos.Strategies, pos.StrategyIssues = strategy.InferPositionStrategies(pos.Options)
	f.readErr = context.DeadlineExceeded
	engine := &proposalEngine{server: &Server{}, optionExitSource: f, now: func() time.Time { return now }}
	proposals, _ := engine.generate(context.Background(), standingOptionExitPolicy(), rpc.ProtectionPolicyStatus{}, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now)
	if len(proposals) != 0 {
		t.Fatalf("an unmeasured index put spread produced exit work: %+v", proposals)
	}
}

func TestUnitsFromBookSkipsPairsResolvedAsIndependent(t *testing.T) {
	pos := &rpc.PositionsResult{Options: []rpc.PositionView{
		unitTestLeg(42, "SPY", "20260918", "P", 100, 2, 300),
		unitTestLeg(43, "SPY", "20261016", "C", 100, 2, 100),
	}}
	pos.Strategies, pos.StrategyIssues = strategy.InferPositionStrategies(pos.Options)
	if len(pos.Strategies) != 1 {
		t.Fatalf("fixture: %+v", pos.StrategyIssues)
	}
	if units := optionExitUnitsFromBook(pos, map[int]bool{}, map[string]bool{}); len(units) != 0 {
		t.Fatalf("an independently exit-managed pair became a unit: %+v", units)
	}
	if units := optionExitUnitsFromBook(pos, map[int]bool{42: true, 43: true}, map[string]bool{}); len(units) != 1 || len(units[0].legs) != 2 || units[0].units != 2 {
		t.Fatalf("a grouped pair was not a unit: %+v", units)
	}
}
