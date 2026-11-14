package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

const (
	hedgeTrimCalls = 9911
	hedgeTrimNear  = 9921
	hedgeTrimFar   = 9922
)

// hedgeTrimBook is a EUR account holding USD lines at a rate of 0.5: SYNA
// calls worth long EUR of delta, beside 2 near and 20 far index puts, each
// contract short 5,000 EUR of delta, 110,000 EUR together.
func hedgeTrimBook(long float64) (*rpc.AccountResult, *rpc.PositionsResult) {
	hedge := risk.DefaultRulebookPolicy().HedgeSymbols[0]
	option := func(symbol string, conID int, right, expiry string, qty, delta, spot, mark float64) rpc.PositionView {
		mv := mark * 100 * qty
		return rpc.PositionView{Symbol: symbol, SecType: rpc.SecTypeOption, ConID: conID, Exchange: "SMART", Currency: "USD", FXRate: new(0.5),
			Quantity: qty, Multiplier: 100, Mark: mark, MarketValue: mv, MarketValueBase: new(mv * 0.5), AvgCost: mark * 100,
			Expiry: expiry, Strike: spot, Right: right, Delta: new(delta), Underlying: new(spot)}
	}
	calls := option("SYNA", hedgeTrimCalls, "C", "20270115", 10, long/100000, 200, 4)
	near := option(hedge, hedgeTrimNear, "P", "20261218", 2, -0.25, 400, 6)
	far := option(hedge, hedgeTrimFar, "P", "20270115", 20, -0.25, 400, 6)
	pos := &rpc.PositionsResult{
		AsOf: mandateTestNow,
		Authority: &rpc.AccountDataAuthority{Availability: rpc.AccountDataAvailable, Freshness: rpc.AccountDataFreshnessCurrent, PortfolioComplete: true,
			Scope: rpc.AccountDataScope{AccountID: deltaTestScope.Account, AccountMode: deltaTestScope.Mode}},
		Portfolio: &rpc.PositionsPortfolio{BaseCurrency: "EUR"},
		Options:   []rpc.PositionView{calls, far, near},
		ByUnderlying: []rpc.PositionGroup{
			{Underlying: "SYNA", Options: []rpc.PositionView{calls}, GroupDollarDeltaBase: new(long), GroupMarketValueBase: calls.MarketValueBase},
			{Underlying: hedge, Options: []rpc.PositionView{far, near}, GroupDollarDeltaBase: new(-110000.0), GroupMarketValueBase: new(6600.0)},
		},
	}
	return &rpc.AccountResult{AccountID: deltaTestScope.Account, NetLiquidation: 200000, BaseCurrency: "EUR"}, pos
}

// Once the calls are sold the index puts stay, and can be worth more than
// what is left to protect. Past the over-hedge line Canary offers their sale
// back to the band top, for the owner's device only (owner decision
// 2026-11-14 05:45 CET, "Trim card for my tap"; it amends "flatten to hedges"
// of 2026-10-14 and the rule of 2026-11-04 21:04 CET that a hedge-listed put
// is never sold, which stand for every unattended sale).
func TestHedgeTrimOffersTheSaleBackToTheBandTop(t *testing.T) {
	engine := &proposalEngine{server: &Server{}}
	policy := defaultProtectionPolicy()
	policy.Buckets.RiskReduction.MaxOrderNotional = 1e9
	_, status := (*protectionPolicyManager)(nil).Active()
	trims := func(long float64, edit func(*rpc.PositionsResult)) []rpc.TradeProposal {
		acct, pos := hedgeTrimBook(long)
		if edit != nil {
			edit(pos)
		}
		return engine.hedgeTrimProposals(context.Background(), policy, status, acct, pos, rpc.TradeProposalSourceFingerprints{}, mandateTestNow)
	}

	// 110,000 EUR short against 50,000 EUR long is 220%. No regime was
	// observed, so the highest band applies: a 140% line and a 70% top.
	// 35,000 EUR stays: 15 contracts go, the nearer expiry first. Measured in
	// dollars the puts would read 440% and 18 would go.
	props := trims(50000, nil)
	if len(props) != 2 {
		t.Fatalf("an over-hedged book produced %d rows, want 2: %+v", len(props), props)
	}
	near, far := props[0], props[1]
	if near.Contract.ConID != hedgeTrimNear || near.Quantity != 2 || near.PositionEffect != rpc.OrderPositionEffectClose ||
		far.Contract.ConID != hedgeTrimFar || far.Quantity != 13 || far.PositionEffect != rpc.OrderPositionEffectReduce {
		t.Fatalf("rows: near %d of con %d (%s), far %d of con %d (%s)", near.Quantity, near.Contract.ConID, near.PositionEffect, far.Quantity, far.Contract.ConID, far.PositionEffect)
	}
	for _, p := range props {
		if p.Bucket != rpc.TradeProposalBucketRiskReduction || p.Action != rpc.OrderActionSell {
			t.Fatalf("row %+v: want a risk-reduction sale", p)
		}
		// The row is the owner's to send: no pre-authorised bucket carries it.
		if bucket := automaticBucketFor(p); bucket != "" {
			t.Fatalf("the trim maps to the pre-authorised bucket %q", bucket)
		}
		for _, want := range []string{"220% of the long book", "140% over-hedge line", "70% band top"} {
			if !strings.Contains(p.Reason, want) {
				t.Fatalf("reason %q does not say %q", p.Reason, want)
			}
		}
	}

	// A positions read that is not current, or not the whole portfolio, can
	// have lost its long lines; with none left there is nothing to compare
	// with. Neither offers a put.
	if props := trims(50000, func(pos *rpc.PositionsResult) { pos.Authority.Freshness = rpc.AccountDataFreshnessStale }); len(props) != 0 {
		t.Fatalf("a stale positions read trimmed the hedge: %+v", props)
	}
	if props := trims(50000, func(pos *rpc.PositionsResult) { pos.Authority.PortfolioComplete = false }); len(props) != 0 {
		t.Fatalf("a partial positions read trimmed the hedge: %+v", props)
	}
	if props := trims(50000, func(pos *rpc.PositionsResult) {
		pos.Options, pos.ByUnderlying = pos.Options[1:], pos.ByUnderlying[1:]
	}); len(props) != 0 {
		t.Fatalf("a book whose long lines are gone offered its puts: %+v", props)
	}
	// Inside the line nothing is offered, and an unmeasured book sells no
	// protection.
	if props := trims(100000, nil); len(props) != 0 {
		t.Fatalf("a hedge at 110%%, inside the 140%% line, was trimmed: %+v", props)
	}
	if props := trims(50000, func(pos *rpc.PositionsResult) { pos.ByUnderlying[0].GroupDollarDeltaBase = nil }); len(props) != 0 {
		t.Fatalf("a book with an unmeasured long line trimmed its hedge: %+v", props)
	}
	if props := trims(50000, func(pos *rpc.PositionsResult) {
		pos.Options[1].Delta, pos.ByUnderlying[1].Options[0].Delta = nil, nil
	}); len(props) != 0 {
		t.Fatalf("a put without its delta was trimmed: %+v", props)
	}
}

// The trim's orders are sized as a forced cut's are: by the money caps, and
// by the option contract cap unless the order gate would pass the sale as a
// delta-reducing exit, judged against the whole book because it sells
// protection.
func TestHedgeTrimRowsFollowTheOrderGate(t *testing.T) {
	pol := risk.DefaultRulebookPolicy()
	policy := defaultProtectionPolicy()
	_, status := (*protectionPolicyManager)(nil).Active()
	_, pos := hedgeTrimBook(50000)
	hedge := pol.HedgeSymbols[0]
	plan := risk.HedgeTrimPlan{RatioPct: 220, ActPct: 140, TopPct: 70, GrossLongBase: 50000, ShortBase: 110000, TargetBase: 35000,
		Lines: []risk.HedgeTrimLine{
			{Symbol: hedge, Leg: legDesc(pos.Options[2]), Held: 2, Sell: 2, ShortPerContractBase: 5000},
			{Symbol: hedge, Leg: legDesc(pos.Options[1]), Held: 20, Sell: 13, ShortPerContractBase: 5000},
		}}
	rows := func(sizing mandateReductionInput) []rpc.TradeProposal {
		return hedgeTrimRows(policy, status, rpc.TradeProposalSourceFingerprints{}, mandateTestNow, pos, pol, plan, sizing, "EUR")
	}
	// A contract is 600 USD, 300 EUR: a 3,000 EUR cap admits 10. The book is
	// 60,000 EUR short with its puts, so their sale lowers its delta and the
	// gate lifts the 5-contract cap.
	sizing := mandateReductionInput{MaxOrderNotional: 1e9, OrderCapBase: 3000, MaxOptionContracts: 5, Exits: &deltaExitPlan{pos: pos, pol: pol}}
	props := rows(sizing)
	if len(props) != 2 || props[0].Quantity != 2 || props[1].Quantity != 10 || !strings.Contains(strings.Join(props[1].Details, " "), "hold one order to 10") {
		t.Fatalf("rows the gate admits: %+v", props)
	}
	// Without the gate's measurement the contract cap stays.
	sizing.Exits = nil
	if props := rows(sizing); len(props) != 2 || props[0].Quantity != 2 || props[1].Quantity != 5 {
		t.Fatalf("rows without current positions: %+v", props)
	}
	// A book long with its puts: their sale raises its delta, so the gate
	// keeps the cap and so does the row.
	_, long := hedgeTrimBook(200000)
	sizing.Exits = &deltaExitPlan{pos: long, pol: pol}
	props = hedgeTrimRows(policy, status, rpc.TradeProposalSourceFingerprints{}, mandateTestNow, long, pol, plan, sizing, "EUR")
	if len(props) != 2 || props[1].Quantity != 5 {
		t.Fatalf("rows in a book long with its puts: %+v", props)
	}
}

// No sale of a hedge-listed put goes unattended, whatever bucket names it and
// whatever the owner pre-authorised (the owner's rule of 2026-11-04 21:04
// CET; 2026-11-14 05:45 CET).
func TestHedgeListedPutIsNeverAutomatic(t *testing.T) {
	rig := newAutomaticTestRig(t, `pre_authorised = ["budget_reduction", "trailing_stop"]`)
	rig.server.coreStore = rig.core
	engine := rig.engine
	hedge := engine.rulebookPolicy().HedgeSymbols[0]
	// A loss exit, the bucket through which a put measured as directional
	// could otherwise leave unattended once the owner lists it.
	sale := func(symbol, right string) rpc.TradeProposal {
		return rpc.TradeProposal{Bucket: rpc.TradeProposalBucketOptionLossExit, Symbol: symbol, Action: rpc.OrderActionSell, Quantity: 1,
			OptionExit: &rpc.TradeProposalOptionExit{Kind: risk.OptionExitActionLoss},
			Contract:   rpc.ContractParams{ConID: 9931, Symbol: symbol, SecType: "OPT", Right: right, Expiry: "20270115", Strike: 400, Multiplier: 100}}
	}
	if bucket := engine.automaticBucket(sale("SYNA", "P")); bucket != preAuthorisedBucketOptionLossExit {
		t.Fatalf("a single-name put loss exit maps to %q, want the loss-exit bucket", bucket)
	}
	if bucket := engine.automaticBucket(sale(hedge, "C")); bucket != preAuthorisedBucketOptionLossExit {
		t.Fatalf("an index call loss exit maps to %q, want the loss-exit bucket", bucket)
	}
	if bucket := engine.automaticBucket(sale(hedge, "P")); bucket != "" {
		t.Fatalf("an index put loss exit maps to the pre-authorised bucket %q", bucket)
	}
	// The budget governor may name a put measured as directional; that row
	// waits for the owner too.
	budget := sale(hedge, "P")
	budget.Bucket, budget.OptionExit = rpc.TradeProposalBucketBudgetReduction, nil
	if automaticBucketFor(budget) != preAuthorisedBucketBudgetReduction || engine.automaticBucket(budget) != "" {
		t.Fatalf("an index put budget row maps to %q", engine.automaticBucket(budget))
	}
}

// Desk's controller sends reductions under its standing mandate. A put on a
// hedge-listed index is not among them: Canary refuses the item whatever
// class the grant names.
func TestDeskMandateNeverSellsAHedgeListedPut(t *testing.T) {
	server := &Server{}
	engine := &proposalEngine{server: server}
	hedge := engine.rulebookPolicy().HedgeSymbols[0]
	row := func(symbol string) rpc.TradeProposal {
		return rpc.TradeProposal{Key: "risk_reduction:synthetic", Revision: "rev-1", Bucket: rpc.TradeProposalBucketRiskReduction, Symbol: symbol,
			Action: rpc.OrderActionSell, Quantity: 2, OrderType: rpc.OrderTypeLMT,
			Contract: rpc.ContractParams{ConID: 9941, Symbol: symbol, SecType: "OPT", Right: "P", Expiry: "20270115", Strike: 400, Multiplier: 100}}
	}
	grant := &deskExecutionGrant{Key: "risk_reduction:synthetic", Revision: "rev-1", Class: rpc.DeskClassReduce}
	server.deskExecutionGrant.Store(grant)
	blockers := engine.deskSubmitBlockers(row(hedge), grant, "")
	if len(blockers) != 1 || blockers[0].Code != deskBlockerProtectionOwnerOnly {
		t.Fatalf("an index put under the standing mandate: %+v, want %s", blockers, deskBlockerProtectionOwnerOnly)
	}
	for _, b := range engine.deskSubmitBlockers(row("SYNA"), grant, "") {
		if b.Code == deskBlockerProtectionOwnerOnly {
			t.Fatalf("a single-name put was refused as protection: %+v", b)
		}
	}
}

// The trim stays its own card beside an exit of the same put. That exit is
// for the whole line and leads by size, and its preview needs the owner's
// declaration for the contract, so covering the trim would hide the card the
// owner can send behind one that may not preview.
func TestHedgeTrimIsNotCoveredByAnExitOfTheSamePut(t *testing.T) {
	hedge := risk.DefaultRulebookPolicy().HedgeSymbols[0]
	contract := rpc.ContractParams{ConID: hedgeTrimFar, Symbol: hedge, SecType: "OPT", Right: "P", Expiry: "20270115", Strike: 400, Multiplier: 100}
	exit := rpc.TradeProposal{Key: "option_loss_exit:synthetic", Bucket: rpc.TradeProposalBucketOptionLossExit, State: rpc.TradeProposalStateGenerated,
		Symbol: hedge, Action: rpc.OrderActionSell, Quantity: 20, OrderType: rpc.OrderTypeLMT, Contract: contract,
		OptionExit: &rpc.TradeProposalOptionExit{Kind: risk.OptionExitActionLoss}}
	trim := rpc.TradeProposal{Key: "risk_reduction:synthetic", Bucket: rpc.TradeProposalBucketRiskReduction, State: rpc.TradeProposalStateGenerated,
		Symbol: hedge, Action: rpc.OrderActionSell, Quantity: 13, OrderType: rpc.OrderTypeLMT, Contract: contract}
	issuer := trim
	issuer.Key, issuer.Issuer = "risk_reduction:issuer", "Synthetic Issuer"

	rows := []rpc.TradeProposal{exit, trim}
	mergeSameContractProposals(rows, nil)
	if rows[1].State != rpc.TradeProposalStateGenerated || rows[1].CoveredBy != "" || len(rows[1].Blockers) != 0 {
		t.Fatalf("the trim was covered by the exit: %+v", rows[1])
	}
	// An issuer trim of the same size on the same contract is still covered:
	// the exemption is the protection trim's alone.
	rows = []rpc.TradeProposal{exit, issuer}
	mergeSameContractProposals(rows, nil)
	if rows[1].CoveredBy != exit.Key {
		t.Fatalf("an issuer trim was not covered by the larger exit: %+v", rows[1])
	}
}
