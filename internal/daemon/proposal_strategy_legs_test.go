package daemon

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/config"
	"github.com/osauer/canary/v3/internal/risk"
	"github.com/osauer/canary/v3/internal/rpc"
	"github.com/osauer/canary/v3/internal/strategy"
)

const (
	spreadLongPut  = 9931
	spreadShortPut = 9932
	spreadUnits    = 15
)

// strategyLegBook is hedgeTrimBook with the index puts held as one bear put
// spread: 15 long 400 puts against 15 short 350 puts of one expiry. Counted
// as rule 12 counts them, by the pair, each unit is short 3,000 EUR of delta
// (5,000 from the long put less 2,000 the short put gives back): 45,000 EUR
// against a 30,000 EUR long book, 150%: past the 140% line, and 8 units over
// the 70% band top.
func strategyLegBook(t *testing.T) (*rpc.AccountResult, *rpc.PositionsResult, rpc.PositionStrategy) {
	t.Helper()
	acct, pos := hedgeTrimBook(30000)
	hedge := risk.DefaultRulebookPolicy().HedgeSymbols[0]
	long := pos.Options[1]
	long.ConID, long.Quantity = spreadLongPut, spreadUnits
	long.MarketValue = long.Mark * 100 * long.Quantity
	long.MarketValueBase = new(long.MarketValue * 0.5)
	short := long
	short.ConID, short.Strike, short.Quantity, short.Mark, short.AvgCost = spreadShortPut, 350, -spreadUnits, 2, 200
	short.Delta = new(-0.10)
	short.MarketValue = short.Mark * 100 * short.Quantity
	short.MarketValueBase = new(short.MarketValue * 0.5)
	pos.Options = []rpc.PositionView{pos.Options[0], long, short}
	pos.ByUnderlying[1] = rpc.PositionGroup{Underlying: hedge, Options: []rpc.PositionView{long, short},
		GroupDollarDeltaBase: new(-45000.0), GroupMarketValueBase: new(3000.0)}
	pos.Strategies, pos.StrategyIssues = strategy.InferPositionStrategies(pos.Options)
	if len(pos.Strategies) != 1 || len(pos.StrategyIssues) != 0 || pos.Strategies[0].Units != spreadUnits {
		t.Fatalf("fixture did not reconstruct one spread: %+v %+v", pos.Strategies, pos.StrategyIssues)
	}
	return acct, pos, pos.Strategies[0]
}

func strategyLegBlocked(p rpc.TradeProposal) bool {
	if p.State != rpc.TradeProposalStateBlocked {
		return false
	}
	for _, b := range p.Blockers {
		if b.Code == "strategy_workflow_required" {
			return true
		}
	}
	return false
}

// Seen on a live book: while 15 long puts were held against 15 short puts of
// the same expiry, the protection trim offered a sendable sale of 5 of the
// long puts alone. A held strategy trades as whole units: the trim proposes
// units of the spread, and no row of any bucket is a sendable single-leg
// order for one of its legs.
func TestNoProposalSellsOneLegOfAHeldStrategy(t *testing.T) {
	engine := &proposalEngine{server: withTestOrderLimits(&Server{})}
	policy := defaultProtectionPolicy()
	policy.Buckets.RiskReduction.MaxOrderNotional = 1e9
	_, status := (*protectionPolicyManager)(nil).Active()
	acct, pos, spread := strategyLegBook(t)
	// The long puts decay into a near expiry too, so theta hygiene has its
	// own reason to close the leg.
	near := mandateTestNow.AddDate(0, 0, 10).Format("20060102")
	for i := range pos.Options {
		if pos.Options[i].ConID == spreadLongPut || pos.Options[i].ConID == spreadShortPut {
			pos.Options[i].Expiry, pos.Options[i].Theta = near, new(-0.40)
			pos.Options[i].Strike = 500 - float64(i)*50
		}
	}
	pos.ByUnderlying[1].Options = []rpc.PositionView{pos.Options[1], pos.Options[2]}
	pos.Strategies, pos.StrategyIssues = strategy.InferPositionStrategies(pos.Options)
	if len(pos.Strategies) != 1 {
		t.Fatalf("fixture lost its spread: %+v", pos.StrategyIssues)
	}
	spread = pos.Strategies[0]

	rows, _ := engine.generate(context.Background(), policy, status, acct, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, mandateTestNow)
	legs := map[int]bool{spreadLongPut: true, spreadShortPut: true}
	var unit *rpc.TradeProposal
	buckets := map[string]bool{}
	for i, p := range rows {
		if p.Unit != nil && p.Unit.StrategyID == spread.ID && p.Bucket == rpc.TradeProposalBucketRiskReduction {
			unit = &rows[i]
			continue
		}
		if p.Unit != nil || !legs[p.Contract.ConID] {
			continue
		}
		buckets[p.Bucket] = true
		if !strategyLegBlocked(p) {
			t.Errorf("%s offers %s %d of one leg of a held spread as its own order: state %s", p.Bucket, p.Action, p.Quantity, p.State)
		}
	}
	if !buckets[rpc.TradeProposalBucketThetaHygiene] {
		t.Errorf("the fixture raised no theta row on a spread leg; buckets seen: %v", buckets)
	}
	// 8 long puts over the band top are 8 units of a 1:-1 spread; the
	// 5-contract order cap holds this order to 5, as it held the single-leg
	// sale, and the next cycle measures the rest.
	if unit == nil {
		t.Fatalf("the protection trim proposed no units of the spread; rows: %d", len(rows))
	}
	if unit.Quantity != 5 || unit.SecType != "BAG" || unit.Unit.StrategyRevision != spread.Revision || unit.Unit.Units != spreadUnits ||
		unit.PositionEffect != rpc.OrderPositionEffectReduce || !unit.Unit.ComboRoute || len(unit.Blockers) != 0 {
		t.Fatalf("unit row: %d of %d units, effect %s, blockers %+v", unit.Quantity, unit.Unit.Units, unit.PositionEffect, unit.Blockers)
	}
	if !strings.Contains(unit.Reason, "sells 8 of the 15") || !strings.Contains(strings.Join(unit.Details, " "), "hold one order to 5") {
		t.Fatalf("the unit row does not say what the plan asks and what the caps admit: %q %q", unit.Reason, unit.Details)
	}
	if want := "canary strategies reduce " + spread.ID; !strings.Contains(unit.Unit.Route, want) || !strings.Contains(unit.Unit.Route, "--units 5") {
		t.Fatalf("unit route %q does not name %q with its units", unit.Unit.Route, want)
	}
	// Protection is the owner's to sell: no pre-authorised bucket carries
	// the unit row, and the proposal order path refuses it.
	if bucket := engine.automaticBucket(*unit); bucket != "" {
		t.Fatalf("the unit trim maps to the pre-authorised bucket %q", bucket)
	}
	if blockers := unitProposalOrderBlockers(*unit); len(blockers) != 1 || blockers[0].Code != "strategy_workflow_required" {
		t.Fatalf("the proposal order path would carry the unit row: %+v", blockers)
	}
}

// Whatever rule raises it, a single-contract row for a leg of a held
// strategy cannot be sent: the pass every generated book ends with blocks
// it, and preview and submit refuse a row that is blocked.
func TestEveryBucketHoldsAStrategyLeg(t *testing.T) {
	_, pos, _ := strategyLegBook(t)
	hedge := risk.DefaultRulebookPolicy().HedgeSymbols[0]
	contract := func(conID int, strike float64) rpc.ContractParams {
		return rpc.ContractParams{ConID: conID, Symbol: hedge, SecType: "OPT", Right: "P", Expiry: "20270115", Strike: strike, Multiplier: 100}
	}
	row := func(bucket, action string, conID int, strike float64) rpc.TradeProposal {
		return rpc.TradeProposal{Key: bucket + ":synthetic", Revision: "r1", State: rpc.TradeProposalStateGenerated, Bucket: bucket, Symbol: hedge, SecType: "OPT",
			Action: action, Quantity: 5, OrderType: rpc.OrderTypeLMT, Contract: contract(conID, strike)}
	}
	var rows []rpc.TradeProposal
	for _, bucket := range []string{rpc.TradeProposalBucketThetaHygiene, rpc.TradeProposalBucketRiskReduction, rpc.TradeProposalBucketBudgetReduction,
		rpc.TradeProposalBucketMandateReduction, rpc.TradeProposalBucketTrailingStop, rpc.TradeProposalBucketOptionLossExit,
		rpc.TradeProposalBucketOptionExpiryClose, rpc.TradeProposalBucketOptionExitReview} {
		rows = append(rows, row(bucket, rpc.OrderActionSell, spreadLongPut, 400), row(bucket, rpc.OrderActionBuy, spreadShortPut, 350))
	}
	// A contract outside every strategy keeps its row as it was.
	free := row(rpc.TradeProposalBucketThetaHygiene, rpc.OrderActionSell, hedgeTrimCalls, 200)
	rows = append(rows, free)
	holdStrategyLegs(rows, pos, defaultProtectionPolicy(), time.Time{})
	for _, p := range rows[:len(rows)-1] {
		if !strategyLegBlocked(p) {
			t.Errorf("%s %s of a spread leg stayed sendable: state %s", p.Bucket, p.Action, p.State)
		}
	}
	if last := rows[len(rows)-1]; last.State != rpc.TradeProposalStateGenerated || len(last.Blockers) != 0 {
		t.Fatalf("a standalone contract was held: %+v", last)
	}

	// A covered call is a name for a call and its shares, not a unit: the
	// call stays a contract of its own.
	covered := &rpc.PositionsResult{Strategies: []rpc.PositionStrategy{{Kind: "covered_call", Status: rpc.PositionStrategyStatusCurrent,
		Legs:  []rpc.PositionStrategyLeg{{Contract: rpc.ContractParams{ConID: hedgeTrimCalls}, Quantity: -2, Ratio: -1}},
		Stock: &rpc.PositionStrategyLeg{Contract: rpc.ContractParams{ConID: 77, SecType: "STK"}, Quantity: 200, Ratio: 100}}}}
	call := []rpc.TradeProposal{row(rpc.TradeProposalBucketThetaHygiene, rpc.OrderActionBuy, hedgeTrimCalls, 200)}
	holdStrategyLegs(call, covered, defaultProtectionPolicy(), time.Time{})
	if call[0].State != rpc.TradeProposalStateGenerated || len(call[0].Blockers) != 0 {
		t.Fatalf("a covered call's call was held as a strategy leg: %+v", call[0].Blockers)
	}

	// A blocked row reaches neither the broker preview nor the wire.
	engine := &proposalEngine{server: withTestOrderLimits(&Server{cfg: &config.Resolved{}}), now: func() time.Time { return mandateTestNow },
		revalidateForTest: func(context.Context, string, string) (rpc.TradeProposal, []rpc.TradingBlocker, error) {
			return rows[0], rows[0].Blockers, nil
		}}
	preview, err := engine.Preview(context.Background(), rpc.TradeProposalPreviewParams{Key: rows[0].Key, Revision: rows[0].Revision})
	if err != nil || preview.Accepted || preview.Preview != nil || len(preview.Blockers) == 0 || preview.Blockers[0].Code != "strategy_workflow_required" {
		t.Fatalf("preview of a held leg: accepted=%v blockers=%+v err=%v", preview.Accepted, preview.Blockers, err)
	}
	submit, err := engine.Submit(context.Background(), rpc.TradeProposalSubmitParams{Key: rows[0].Key, Revision: rows[0].Revision})
	if err != nil || submit.Accepted || len(submit.Blockers) == 0 || submit.Blockers[0].Code != "strategy_workflow_required" {
		t.Fatalf("submit of a held leg: accepted=%v blockers=%+v err=%v", submit.Accepted, submit.Blockers, err)
	}
}

// A strategy with two bought hedge puts (a long put butterfly) is trimmed in
// one row, and its units sell no more of either put line than the plan names
// for it. Sized per line, the trim raised two rows under one key and, where
// the caps allowed, sold more protection than the band top asked for.
func TestUnitTrimOfTwoPutLinesIsOneRowInsideThePlan(t *testing.T) {
	engine := &proposalEngine{server: &Server{}}
	policy := defaultProtectionPolicy()
	policy.Buckets.RiskReduction.MaxOrderNotional = 1e9
	_, status := (*protectionPolicyManager)(nil).Active()
	acct, pos := hedgeTrimBook(30000)
	hedge := risk.DefaultRulebookPolicy().HedgeSymbols[0]
	base := pos.Options[1]
	leg := func(conID int, strike, qty, delta, mark float64) rpc.PositionView {
		r := base
		r.ConID, r.Strike, r.Quantity, r.Mark, r.AvgCost = conID, strike, qty, mark, mark*100
		r.Delta = new(delta)
		r.MarketValue = mark * 100 * qty
		r.MarketValueBase = new(r.MarketValue * 0.5)
		return r
	}
	hi, body, lo := leg(9941, 420, 10, -0.40, 9), leg(9942, 400, -20, -0.25, 6), leg(9943, 380, 10, -0.15, 3)
	pos.Options = []rpc.PositionView{pos.Options[0], hi, body, lo}
	pos.ByUnderlying[1] = rpc.PositionGroup{Underlying: hedge, Options: []rpc.PositionView{hi, body, lo}, GroupDollarDeltaBase: new(-10000.0), GroupMarketValueBase: new(0.0)}
	pos.Strategies, pos.StrategyIssues = strategy.InferStrategies(pos.Stocks, pos.Options)
	if len(pos.Strategies) != 1 || len(pos.Strategies[0].Legs) != 3 {
		t.Fatalf("fixture: strategies = %+v, want one three-leg butterfly", pos.Strategies)
	}
	plan, _, _, ok := engine.hedgeTrimPlan(context.Background(), acct, pos, mandateTestNow)
	if !ok || len(plan.Lines) != 2 {
		t.Fatalf("fixture: plan ok=%v lines=%+v, want a trim naming both put lines", ok, plan.Lines)
	}
	least := math.Inf(1)
	for _, line := range plan.Lines {
		least = math.Min(least, line.Sell)
	}
	rows, _ := engine.generate(context.Background(), policy, status, acct, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, mandateTestNow)
	units := 0
	for _, p := range rows {
		if p.Unit == nil {
			continue
		}
		units++
		if float64(p.Quantity) > least+1e-9 {
			t.Errorf("unit row sells %d units, more than the %v contracts the plan names for one of its put lines", p.Quantity, least)
		}
	}
	if units != 1 {
		t.Fatalf("unit rows = %d, want one for the one strategy", units)
	}
}
