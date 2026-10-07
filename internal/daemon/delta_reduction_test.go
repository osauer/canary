package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// Synthetic book throughout; no number is an account's. One underlying,
// SYNB, at 120 EUR: 1,000 shares long, 10 long puts and 8 short puts (delta
// −0.4), 8 long calls and 8 short calls (delta 0.5), all with multiplier
// 100. Dollar deltas in base currency as positionDollarDelta measures them:
// stock +120,000; long puts −48,000; short puts +38,400; long calls +48,000;
// short calls −48,000; net +110,400. A second stock, OTHR, adds +25,000, so
// the whole book is +135,400.
const (
	deltaTestStock      = 7001
	deltaTestLongPuts   = 7101
	deltaTestShortPuts  = 7102
	deltaTestLongCalls  = 7103
	deltaTestShortCalls = 7104
)

var deltaTestScope = brokerStateScope{Account: "DU1234567", Mode: rpc.AccountModePaper}

// deltaTestEvidence is the measurement the gate would read for the book
// above, built directly.
func deltaTestEvidence() deltaReductionEvidence {
	return deltaReductionEvidence{Current: true, Underlying: "SYNB", BaseCurrency: "EUR", NetBefore: 110400, BookBefore: 135400,
		Legs: map[int]deltaReductionLeg{
			deltaTestStock:      {Quantity: 1000, UnitBase: 120},
			deltaTestLongPuts:   {Quantity: 10, UnitBase: -4800},
			deltaTestShortPuts:  {Quantity: -8, UnitBase: -4800},
			deltaTestLongCalls:  {Quantity: 8, UnitBase: 6000},
			deltaTestShortCalls: {Quantity: -8, UnitBase: 6000},
		},
		Cover: deltaCoverage{Shares: map[int]float64{deltaTestStock: 1000}, Options: []deltaCoverLeg{
			{ConID: deltaTestLongPuts, Right: "P", Quantity: 10, Multiplier: 100},
			{ConID: deltaTestShortPuts, Right: "P", Quantity: -8, Multiplier: 100},
			{ConID: deltaTestLongCalls, Right: "C", Quantity: 8, Multiplier: 100},
			{ConID: deltaTestShortCalls, Right: "C", Quantity: -8, Multiplier: 100},
		}}}
}

func deltaTestStockDraft(action string, qty int) rpc.OrderDraft {
	return rpc.OrderDraft{Action: action, Quantity: qty, OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay,
		Contract: rpc.ContractParams{ConID: deltaTestStock, Symbol: "SYNB", SecType: "STK", Exchange: "SMART", Currency: "EUR"}}
}

func deltaTestOptionDraft(conID int, right string, action string, qty int) rpc.OrderDraft {
	return rpc.OrderDraft{Action: action, Quantity: qty, OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay,
		Contract: rpc.ContractParams{ConID: conID, Symbol: "SYNB", SecType: "OPT", Exchange: "SMART", Currency: "EUR",
			Expiry: "20261218", Strike: 75, Right: right, Multiplier: 100}}
}

// deltaTestStrategyDraft is a guaranteed two-leg combo on SYNB: legA sells
// its long contracts, legB buys back its short ones.
func deltaTestStrategyDraft(units int, legA, legB rpc.StrategyOrderLeg) (rpc.OrderDraft, rpc.OrderPositionImpact) {
	group := &rpc.StrategyOrderDraft{StrategyID: "syn-strategy", StrategyRevision: 1, Operation: rpc.StrategyOperationClose,
		Units: units, UnitsBefore: units, UnitsAfter: 0, GuaranteedCombo: true, Legs: []rpc.StrategyOrderLeg{legA, legB}}
	draft := rpc.OrderDraft{Action: rpc.OrderActionSell, Quantity: units, OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay, OpenClose: "C",
		Contract: rpc.ContractParams{Symbol: "SYNB", SecType: "BAG", Exchange: "SMART", Currency: "EUR"}, StrategyGroup: group}
	return draft, rpc.OrderPositionImpact{Before: float64(units), After: 0, Effect: rpc.OrderPositionEffectClose}
}

func deltaTestLeg(conID int, ratio int, action string, qty int, before float64) rpc.StrategyOrderLeg {
	after := before + float64(qty)
	if action == rpc.OrderActionSell {
		after = before - float64(qty)
	}
	return rpc.StrategyOrderLeg{Contract: rpc.ContractParams{ConID: conID, Symbol: "SYNB", SecType: "OPT", Currency: "EUR", Multiplier: 100, Expiry: "20261218", Strike: 75, Right: "C"},
		Ratio: ratio, Action: action, Quantity: qty, Before: before, After: after}
}

// deltaTestInventory is a current open-order inventory with nothing else
// working on the line.
var deltaTestInventory = protectiveExitInventory{Current: true}

// A close or reduction that lowers the absolute net delta of its underlying
// passes the notional cap and the option-contract cap (owner decision
// 2026-10-07 08:13 CEST). A close that raises it, an unknown delta, an
// opening or adding order and a flip keep the caps; the sell-as-short
// re-read stays; a second exit of a line that another working order already
// exits, or an unreadable open-order list, keeps the caps too. The cap in
// force here is 12,000 EUR (5% of NLV 240,000) and 5 option contracts.
func TestDeltaReducingExitPassesTheCaps(t *testing.T) {
	t.Parallel()
	ev := deltaTestEvidence()
	stale := deltaReductionEvidence{Underlying: "SYNB", Reason: "the SYNB stock line has a stale quote"}
	mismatch := deltaTestEvidence()
	mismatch.Legs[deltaTestStock] = deltaReductionLeg{Quantity: 900, UnitBase: 120}
	allowShort := func(o *risk.ConstitutionOrderLimits) { o.AllowStockShort = new(true) }
	allowSTO := func(o *risk.ConstitutionOrderLimits) { o.AllowOptionSellToOpen = new(true) }
	cases := []struct {
		name     string
		edit     func(*risk.ConstitutionOrderLimits)
		draft    rpc.OrderDraft
		position rpc.OrderPositionImpact
		notional float64
		evidence deltaReductionEvidence
		inv      protectiveExitInventory
		wantErr  []string
		wantNot  []string
	}{
		{name: "a stock sale that shrinks a long above the notional cap passes", edit: allowShort,
			draft: deltaTestStockDraft(rpc.OrderActionSell, 400), position: protectiveExitTestPosition(1000, rpc.OrderActionSell, 400), notional: 30000, evidence: ev, inv: deltaTestInventory},
		{name: "the same sale keeps the sell-as-short re-read",
			draft: deltaTestStockDraft(rpc.OrderActionSell, 400), position: protectiveExitTestPosition(1000, rpc.OrderActionSell, 400), notional: 30000, evidence: ev, inv: deltaTestInventory,
			wantErr: []string{"allow_stock_short"}, wantNot: []string{"order cap in force"}},
		{name: "an option buy-to-close above the contract cap that lowers the delta passes",
			draft: deltaTestOptionDraft(deltaTestShortPuts, "P", rpc.OrderActionBuy, 8), position: protectiveExitTestPosition(-8, rpc.OrderActionBuy, 8), notional: 2000, evidence: ev, inv: deltaTestInventory},
		{name: "an option sell-to-close above both caps that lowers the delta passes", edit: allowSTO,
			draft: deltaTestOptionDraft(deltaTestLongCalls, "C", rpc.OrderActionSell, 8), position: protectiveExitTestPosition(8, rpc.OrderActionSell, 8), notional: 16000, evidence: ev, inv: deltaTestInventory},
		{name: "closing a long put that hedges the long stock raises the delta and stays capped", edit: allowSTO,
			draft: deltaTestOptionDraft(deltaTestLongPuts, "P", rpc.OrderActionSell, 10), position: protectiveExitTestPosition(10, rpc.OrderActionSell, 10), notional: 15000, evidence: ev, inv: deltaTestInventory,
			wantErr: []string{"cap in force", "does not lower the absolute delta of SYNB (110,400 EUR before, 158,400 EUR after)"}},
		{name: "buying back a short call that hedges the long stock stays capped",
			draft: deltaTestOptionDraft(deltaTestShortCalls, "C", rpc.OrderActionBuy, 8), position: protectiveExitTestPosition(-8, rpc.OrderActionBuy, 8), notional: 2000, evidence: ev, inv: deltaTestInventory,
			wantErr: []string{"option cap in force of 5 contracts", "does not lower the absolute delta of SYNB"}},
		{name: "an unknown delta stays capped and says so", edit: allowShort,
			draft: deltaTestStockDraft(rpc.OrderActionSell, 400), position: protectiveExitTestPosition(1000, rpc.OrderActionSell, 400), notional: 30000, inv: deltaTestInventory,
			wantErr: []string{"order cap in force 12,000 EUR", "cannot be measured (no current measurement)"}},
		{name: "a stale measurement stays capped and names the stale line", edit: allowShort,
			draft: deltaTestStockDraft(rpc.OrderActionSell, 400), position: protectiveExitTestPosition(1000, rpc.OrderActionSell, 400), notional: 30000, evidence: stale, inv: deltaTestInventory,
			wantErr: []string{"order cap in force 12,000 EUR", "the SYNB stock line has a stale quote"}},
		{name: "a measurement of another position does not exempt", edit: allowShort,
			draft: deltaTestStockDraft(rpc.OrderActionSell, 400), position: protectiveExitTestPosition(1000, rpc.OrderActionSell, 400), notional: 30000, evidence: mismatch, inv: deltaTestInventory,
			wantErr: []string{"order cap in force 12,000 EUR", "the SYNB stock position has changed since the order was previewed; preview again"}},
		{name: "a cap that did not apply at the preview asks for a new preview", edit: allowShort,
			draft: deltaTestStockDraft(rpc.OrderActionSell, 400), position: protectiveExitTestPosition(1000, rpc.OrderActionSell, 400), notional: 30000,
			evidence: deltaReductionEvidence{Unread: true, Underlying: "SYNB"}, inv: deltaTestInventory,
			wantErr: []string{"order cap in force 12,000 EUR", "the order cap did not apply when you previewed this order; preview it again"}},
		{name: "a second buy-back of a short-8 option line is refused",
			draft: deltaTestOptionDraft(deltaTestShortPuts, "P", rpc.OrderActionBuy, 8), position: protectiveExitTestPosition(-8, rpc.OrderActionBuy, 8), notional: 2000, evidence: ev,
			inv:     protectiveExitInventory{Current: true, OtherWorkingSameSide: 8},
			wantErr: []string{"option cap in force of 5 contracts", "another working order already buys back 8 of the 8 held of SYNB 20261218 P 75 option, so with this one more would be bought back than is held; cancel it first"}},
		{name: "a second sale of the long stock line is refused", edit: allowShort,
			draft: deltaTestStockDraft(rpc.OrderActionSell, 400), position: protectiveExitTestPosition(1000, rpc.OrderActionSell, 400), notional: 30000, evidence: ev,
			inv:     protectiveExitInventory{Current: true, OtherWorkingSameSide: 700},
			wantErr: []string{"order cap in force 12,000 EUR", "another working order already sells 700 of the 1000 held of SYNB stock, so with this one more would be sold than is held; cancel it first"}},
		{name: "working orders that fit beside this one do not count against it", edit: allowShort,
			draft: deltaTestStockDraft(rpc.OrderActionSell, 400), position: protectiveExitTestPosition(1000, rpc.OrderActionSell, 400), notional: 30000, evidence: ev,
			inv: protectiveExitInventory{Current: true, OtherWorkingSameSide: 600}},
		{name: "an unavailable open-order list keeps the cap", edit: allowShort,
			draft: deltaTestStockDraft(rpc.OrderActionSell, 400), position: protectiveExitTestPosition(1000, rpc.OrderActionSell, 400), notional: 30000, evidence: ev,
			wantErr: []string{"order cap in force 12,000 EUR", "Canary cannot read the broker's open orders right now, so it cannot rule out another exit of this line; preview again"}},
		{name: "an opening order above the cap stays refused",
			draft: deltaTestStockDraft(rpc.OrderActionBuy, 400), position: protectiveExitTestPosition(0, rpc.OrderActionBuy, 400), notional: 30000, evidence: ev, inv: deltaTestInventory,
			wantErr: []string{"order cap in force 12,000 EUR"}, wantNot: []string{"delta"}},
		{name: "an order that adds to a position above the cap stays refused",
			draft: deltaTestStockDraft(rpc.OrderActionBuy, 400), position: protectiveExitTestPosition(1000, rpc.OrderActionBuy, 400), notional: 30000, evidence: ev, inv: deltaTestInventory,
			wantErr: []string{"order cap in force 12,000 EUR"}, wantNot: []string{"delta"}},
		{name: "an order that would flip the position stays refused", edit: allowShort,
			draft: deltaTestStockDraft(rpc.OrderActionSell, 1200), position: protectiveExitTestPosition(1000, rpc.OrderActionSell, 1200), notional: 90000, evidence: ev, inv: deltaTestInventory,
			wantErr: []string{"order cap in force 12,000 EUR"}, wantNot: []string{"delta"}},
		{name: "an option opening above the contract cap stays refused",
			draft: deltaTestOptionDraft(deltaTestLongCalls, "C", rpc.OrderActionBuy, 8), position: protectiveExitTestPosition(0, rpc.OrderActionBuy, 8), notional: 2000, evidence: ev, inv: deltaTestInventory,
			wantErr: []string{"option cap in force of 5 contracts"}, wantNot: []string{"delta"}},
		{name: "a bond sale above the cap keeps it: a bond carries no equity delta",
			draft: rpc.OrderDraft{Action: rpc.OrderActionSell, Quantity: 30, OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay,
				Contract: rpc.ContractParams{ConID: 7901, Symbol: "SYNB", SecType: "BOND", Exchange: "SMART", Currency: "EUR"}},
			position: protectiveExitTestPosition(100, rpc.OrderActionSell, 30), notional: 30000, evidence: ev, inv: deltaTestInventory,
			wantErr: []string{"order cap in force 12,000 EUR", "a bill or bond carries no equity delta, so the exemption for delta-reducing exits cannot apply"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			table := testOrderLimitsTable(10000)
			if tc.edit != nil {
				tc.edit(table)
			}
			limits := risk.EvaluateOrderLimits(table, "EUR", risk.OrderLimitsNLV{Base: 240000, AsOf: time.Now()}, nil, "")
			err := validateOrderRiskAuthority(limits, tc.draft, tc.position, protectiveExitTestNotional(tc.notional), "EUR", tc.inv, tc.evidence)
			if len(tc.wantErr) == 0 {
				if err != nil {
					t.Fatalf("err = %v, want the delta-reducing exit to pass", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("err = nil, want a refusal naming %q", tc.wantErr)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("err = %v, want it to name %q", err, want)
				}
			}
			for _, not := range tc.wantNot {
				if strings.Contains(err.Error(), not) {
					t.Fatalf("err = %v, must not mention %q", err, not)
				}
			}
		})
	}
}

// A strategy close whose legs together lower the underlying's absolute
// delta passes the per-leg option-contract cap and the notional cap; one
// that raises it keeps both, and so does one whose leg another working order
// already exits.
func TestDeltaReducingStrategyClosePassesTheCaps(t *testing.T) {
	t.Parallel()
	table := testOrderLimitsTable(10000)
	limits := risk.EvaluateOrderLimits(table, "EUR", risk.OrderLimitsNLV{Base: 240000, AsOf: time.Now()}, nil, "")
	ev := deltaTestEvidence()

	// Long calls sold and short puts bought back: −48,000 − 38,400 → 24,000.
	draft, position := deltaTestStrategyDraft(8, deltaTestLeg(deltaTestLongCalls, 1, rpc.OrderActionSell, 8, 8), deltaTestLeg(deltaTestShortPuts, -1, rpc.OrderActionBuy, 8, -8))
	if err := validateOrderRiskAuthority(limits, draft, position, protectiveExitTestNotional(20000), "EUR", deltaTestInventory, ev); err != nil {
		t.Fatalf("delta-lowering strategy close refused: %v", err)
	}
	if err := validateOrderRiskAuthority(limits, draft, position, protectiveExitTestNotional(20000), "EUR", deltaTestInventory, deltaReductionEvidence{}); err == nil ||
		!strings.Contains(err.Error(), "order cap in force 12,000 EUR") || !strings.Contains(err.Error(), "cannot be measured") {
		t.Fatalf("without a measurement the strategy close must keep the cap: %v", err)
	}
	if err := validateOrderRiskAuthority(limits, draft, position, protectiveExitTestNotional(2000), "EUR", deltaTestInventory, deltaReductionEvidence{}); err == nil ||
		!strings.Contains(err.Error(), "proportional option quantity limit") {
		t.Fatalf("without a measurement each leg must keep the contract cap: %v", err)
	}
	busy := protectiveExitInventory{Current: true, OtherWorkingSameSideByLeg: map[int]float64{deltaTestLongCalls: 0, deltaTestShortPuts: 1}}
	if err := validateOrderRiskAuthority(limits, draft, position, protectiveExitTestNotional(20000), "EUR", busy, ev); err == nil ||
		!strings.Contains(err.Error(), "another working order already buys back 1 of the 8 held of SYNB 20261218 C 75 option") {
		t.Fatalf("a leg another order already exits must keep the cap: %v", err)
	}

	// Long puts sold and short calls bought back: +48,000 + 48,000 → 206,400.
	draft, position = deltaTestStrategyDraft(2, deltaTestLeg(deltaTestLongPuts, 5, rpc.OrderActionSell, 10, 10), deltaTestLeg(deltaTestShortCalls, -4, rpc.OrderActionBuy, 8, -8))
	if err := validateOrderRiskAuthority(limits, draft, position, protectiveExitTestNotional(15000), "EUR", deltaTestInventory, ev); err == nil ||
		!strings.Contains(err.Error(), "does not lower the absolute delta of SYNB (110,400 EUR before, 206,400 EUR after)") {
		t.Fatalf("a strategy close that raises the delta must keep the cap: %v", err)
	}
}

// deltaTestPositions is the book above as positions.list reports it, with
// the fields positionDollarDelta reads: mark for the stock, delta, model
// spot and multiplier for the options. A bond row with the same symbol
// carries no equity delta and is not a leg.
func deltaTestPositions() *rpc.PositionsResult {
	f := func(v float64) *float64 { return &v }
	option := func(conID int, right string, qty, delta float64) rpc.PositionView {
		return rpc.PositionView{Symbol: "SYNB", SecType: rpc.SecTypeOption, ConID: conID, Currency: "EUR", Quantity: qty, Multiplier: 100,
			Mark: 3, MarketValue: 300 * qty, Expiry: "20261218", Strike: 75, Right: right, Delta: f(delta), Underlying: f(120)}
	}
	return &rpc.PositionsResult{
		AsOf: time.Date(2026, 10, 7, 8, 0, 0, 0, time.UTC),
		Authority: &rpc.AccountDataAuthority{Availability: rpc.AccountDataAvailable, Freshness: rpc.AccountDataFreshnessCurrent,
			Scope: rpc.AccountDataScope{AccountID: deltaTestScope.Account, AccountMode: deltaTestScope.Mode}},
		Portfolio: &rpc.PositionsPortfolio{BaseCurrency: "EUR"},
		Stocks: []rpc.PositionView{
			{Symbol: "SYNB", SecType: rpc.SecTypeStock, ConID: deltaTestStock, Currency: "EUR", Quantity: 1000, Multiplier: 1, Mark: 120, MarketValue: 120000},
			{Symbol: "SYNB", SecType: "BOND", ConID: 7901, Currency: "EUR", Quantity: 10000, Multiplier: 1, Mark: 99, MarketValue: 9900},
			{Symbol: "OTHR", SecType: rpc.SecTypeStock, ConID: 7002, Currency: "EUR", Quantity: 2500, Multiplier: 1, Mark: 10, MarketValue: 25000},
		},
		Options: []rpc.PositionView{
			option(deltaTestLongPuts, "P", 10, -0.4),
			option(deltaTestShortPuts, "P", -8, -0.4),
			option(deltaTestLongCalls, "C", 8, 0.5),
			option(deltaTestShortCalls, "C", -8, 0.5),
		},
	}
}

// The measurement reads the deltas the daemon's risk verdicts use, sums the
// underlying's equity and option legs in base currency, and fails closed on
// anything it cannot measure, in plain words.
func TestMeasureDeltaReductionReadsTheDaemonsDeltas(t *testing.T) {
	t.Parallel()
	sell := deltaTestStockDraft(rpc.OrderActionSell, 400)
	ev := measureDeltaReduction(deltaTestPositions(), deltaTestScope, sell)
	if !ev.Current || ev.NetBefore != 110400 || ev.BaseCurrency != "EUR" || ev.Legs[deltaTestStock] != (deltaReductionLeg{Quantity: 1000, UnitBase: 120}) {
		t.Fatalf("measurement = %+v, want net 110,400 EUR and the stock leg", ev)
	}
	if ok, why := deltaReducingExit(sell, protectiveExitTestPosition(1000, rpc.OrderActionSell, 400), ev, deltaTestInventory); !ok || why != "" {
		t.Fatalf("stock sale: exempt %v %q, want exempt", ok, why)
	}
	puts := deltaTestOptionDraft(deltaTestLongPuts, "P", rpc.OrderActionSell, 10)
	ev = measureDeltaReduction(deltaTestPositions(), deltaTestScope, puts)
	if !ev.Current || ev.Legs[deltaTestLongPuts] != (deltaReductionLeg{Quantity: 10, UnitBase: -4800}) {
		t.Fatalf("put measurement = %+v", ev)
	}
	if ok, why := deltaReducingExit(puts, protectiveExitTestPosition(10, rpc.OrderActionSell, 10), ev, deltaTestInventory); ok || !strings.Contains(why, "does not lower the absolute delta of SYNB (110,400 EUR before, 158,400 EUR after)") {
		t.Fatalf("hedge put sale: exempt %v %q, want capped", ok, why)
	}
	if _, why := deltaReducingExit(deltaTestStockDraft(rpc.OrderActionBuy, 400), protectiveExitTestPosition(0, rpc.OrderActionBuy, 400), ev, deltaTestInventory); why != "" {
		t.Fatalf("an opening order must carry no delta reason, got %q", why)
	}

	cases := []struct {
		name   string
		edit   func(*rpc.PositionsResult)
		draft  rpc.OrderDraft
		reason string
	}{
		{name: "missing option delta", edit: func(p *rpc.PositionsResult) { p.Options[1].Delta = nil }, draft: sell, reason: "the SYNB 20261218 P 75 option line has no delta or no underlying price"},
		{name: "missing option spot", edit: func(p *rpc.PositionsResult) { p.Options[0].Underlying = nil }, draft: sell, reason: "has no delta or no underlying price"},
		{name: "stale stock quote", edit: func(p *rpc.PositionsResult) { p.Stocks[0].Stale = true }, draft: sell, reason: "the SYNB stock line has a stale quote"},
		{name: "stock without a mark", edit: func(p *rpc.PositionsResult) { p.Stocks[0].Mark = 0 }, draft: sell, reason: "the SYNB stock line has no price"},
		{name: "leg without an FX rate", edit: func(p *rpc.PositionsResult) { p.Options[2].Currency = "USD" }, draft: sell, reason: "has no exchange rate to EUR"},
		{name: "positions not current", edit: func(p *rpc.PositionsResult) { p.Authority.Freshness = rpc.AccountDataFreshnessStale }, draft: sell, reason: "Canary has no current positions; preview again"},
		{name: "another account", edit: func(p *rpc.PositionsResult) { p.Authority.Scope.AccountID = "DU7654321" }, draft: sell, reason: "the positions Canary read are not this account's; preview again"},
		{name: "unknown base currency", edit: func(p *rpc.PositionsResult) { p.Portfolio = nil }, draft: sell, reason: "the account base currency is unknown"},
		{name: "duplicate rows", edit: func(p *rpc.PositionsResult) { p.Stocks = append(p.Stocks, p.Stocks[0]) }, draft: sell, reason: "the SYNB stock line appears twice in the positions"},
		{name: "contract not held", edit: func(*rpc.PositionsResult) {}, draft: deltaTestOptionDraft(7199, "C", rpc.OrderActionSell, 1), reason: "the SYNB 20261218 C 75 option is not a held position; preview again"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pos := deltaTestPositions()
			tc.edit(pos)
			ev := measureDeltaReduction(pos, deltaTestScope, tc.draft)
			if ev.Current || !strings.Contains(ev.Reason, tc.reason) {
				t.Fatalf("measurement = %+v, want not current with %q", ev, tc.reason)
			}
			if strings.Contains(ev.Reason, "contract 7") {
				t.Fatalf("reason names a contract id: %q", ev.Reason)
			}
		})
	}
	if ev := measureDeltaReduction(nil, deltaTestScope, sell); ev.Current || ev.Reason != "Canary has no current positions; preview again" {
		t.Fatalf("nil positions = %+v", ev)
	}
}

// An option whose symbol differs from the stock it relates to (an adjusted
// option after a corporate action keeps a numbered symbol) is measured under
// its own symbol, as the daemon's by-underlying grouping (groupByUnderlying)
// measures it: the stock's delta does not enter, and the option's close is
// judged on the option lines alone.
func TestMeasureDeltaReductionGroupsBySymbolAsTheDaemonDoes(t *testing.T) {
	t.Parallel()
	pos := deltaTestPositions()
	pos.Stocks[0].Symbol = "SYNC"
	for i := range pos.Options {
		pos.Options[i].Symbol = "SYNC1"
	}
	puts := deltaTestOptionDraft(deltaTestLongPuts, "P", rpc.OrderActionSell, 10)
	puts.Contract.Symbol = "SYNC1"
	ev := measureDeltaReduction(pos, deltaTestScope, puts)
	// Options alone: −48,000 + 38,400 + 48,000 − 48,000 = −9,600.
	if !ev.Current || ev.Underlying != "SYNC1" || ev.NetBefore != -9600 {
		t.Fatalf("adjusted-option measurement = %+v, want the option lines alone at −9,600 EUR", ev)
	}
	if ok, why := deltaReducingExit(puts, protectiveExitTestPosition(10, rpc.OrderActionSell, 10), ev, deltaTestInventory); ok || !strings.Contains(why, "does not lower the absolute delta of SYNC1 (9,600 EUR before, 38,400 EUR after)") {
		t.Fatalf("closing the long puts of the option-only group: exempt %v %q", ok, why)
	}
	stock := deltaTestStockDraft(rpc.OrderActionSell, 400)
	stock.Contract.Symbol = "SYNC"
	if ev := measureDeltaReduction(pos, deltaTestScope, stock); !ev.Current || ev.NetBefore != 120000 {
		t.Fatalf("stock measurement = %+v, want the stock alone at 120,000 EUR", ev)
	}
}

// Through the preview: an option buy-to-close above the contract cap passes
// when the positions read shows it lowers the underlying's absolute delta
// and the open-order list shows no other buy-back, and is refused with
// order_risk_limit, saying why, when the positions or the open orders cannot
// be read.
func TestOrderPreviewAdmitsADeltaReducingExitAboveTheCaps(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	srv.orderPreviewQuote = fixedPreviewQuote(2.4, 2.6)
	srv.orderPreviewPositionImpact = fixedPreviewPosition(-8, 0, rpc.OrderPositionEffectClose)
	srv.orderDeltaPositionsForTest = func(context.Context) (*rpc.PositionsResult, error) { return deltaTestPositions(), nil }
	srv.openOrderInventoryForTest = func(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
		return ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: srv.orderNow()}, deltaTestScope, nil
	}
	limit := 2.5
	params := rpc.OrderPreviewParams{Action: "buy", Quantity: 8, LimitPrice: &limit,
		Contract: rpc.ContractParams{ConID: deltaTestShortPuts, Symbol: "SYNB", SecType: "OPT", Currency: "EUR", Expiry: "20261218", Right: "P", Strike: 75, Multiplier: 100}}
	res, err := srv.previewOrder(t.Context(), params)
	if err != nil {
		t.Fatalf("preview of a delta-reducing close above the contract cap: %v", err)
	}
	if res.Draft.OpenClose != "C" || res.Draft.Quantity != 8 || res.Notional != 2000 {
		t.Fatalf("preview = open_close %q quantity %d notional %.2f, want C 8 2000.00", res.Draft.OpenClose, res.Draft.Quantity, res.Notional)
	}

	srv.orderDeltaPositionsForTest = func(context.Context) (*rpc.PositionsResult, error) { return nil, errors.New("gateway unavailable") }
	_, err = srv.previewOrder(t.Context(), params)
	blockers := previewFailureBlockers(err)
	if err == nil || len(blockers) != 1 || blockers[0].Code != previewRiskLimitCode ||
		!strings.Contains(blockers[0].Message, "option cap in force of 5 contracts") || !strings.Contains(blockers[0].Message, "cannot be measured (Canary cannot read the positions right now; preview again)") ||
		strings.Contains(blockers[0].Message, "gateway unavailable") {
		t.Fatalf("preview err %v blockers %+v, want order_risk_limit naming the cap and the failed read in plain words", err, blockers)
	}

	srv.orderDeltaPositionsForTest = func(context.Context) (*rpc.PositionsResult, error) { return deltaTestPositions(), nil }
	srv.openOrderInventoryForTest = func(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
		return ibkrlib.OpenOrderSnapshot{}, brokerStateScope{}, errors.New("no snapshot")
	}
	_, err = srv.previewOrder(t.Context(), params)
	blockers = previewFailureBlockers(err)
	if err == nil || len(blockers) != 1 || blockers[0].Code != previewRiskLimitCode ||
		!strings.Contains(blockers[0].Message, "Canary cannot read the broker's open orders right now, so it cannot rule out another exit of this line; preview again") {
		t.Fatalf("preview err %v blockers %+v, want order_risk_limit naming the unread open orders", err, blockers)
	}
}

// deltaTestLimits is the cap in force of the tests: 12,000 EUR (5% of NLV
// 240,000) and 5 option contracts, with every re-read permission on so the
// delta rule alone decides.
func deltaTestLimits() risk.OrderLimitsInForce {
	table := testOrderLimitsTable(10000)
	table.AllowStockShort, table.AllowOptionSellToOpen = new(true), new(true)
	return risk.EvaluateOrderLimits(table, "EUR", risk.OrderLimitsNLV{Base: 240000, AsOf: time.Now()}, nil, "")
}

// deltaTestGate measures draft against pos and judges it as the gate does,
// with a clean open-order inventory.
func deltaTestGate(pos *rpc.PositionsResult, draft rpc.OrderDraft, position rpc.OrderPositionImpact, notional float64) error {
	ev := measureDeltaReduction(pos, deltaTestScope, draft)
	return validateOrderRiskAuthority(deltaTestLimits(), draft, position, protectiveExitTestNotional(notional), "EUR", deltaTestInventory, ev)
}

// Owner decision 2026-10-07 09:51 CEST, "Both": the exit must lower the
// absolute net delta of its underlying and of the whole book, measured with
// the same per-row measure as the portfolio reduce sweep. Closing index puts
// that hedge a net-long book of single stocks lowers the index's own delta
// but raises the book's, so it keeps the cap; so does covering a short stock
// in a net-long book. A stock sale in a net-long book passes. One unmeasured
// line anywhere keeps the cap and is named; a zero-value defunct row and a
// bond row count as zero and do not.
func TestDeltaRuleJudgesTheWholeBookToo(t *testing.T) {
	t.Parallel()
	f := func(v float64) *float64 { return &v }
	const indexPuts, shortStock = 7301, 7003
	withHedges := func() *rpc.PositionsResult {
		pos := deltaTestPositions()
		// Two long index puts (delta −0.4, spot 500): −40,000; book 95,400.
		pos.Options = append(pos.Options, rpc.PositionView{Symbol: "SYNX", SecType: rpc.SecTypeOption, ConID: indexPuts, Currency: "EUR", Quantity: 2, Multiplier: 100,
			Mark: 12, MarketValue: 2400, Expiry: "20261218", Strike: 500, Right: "P", Delta: f(-0.4), Underlying: f(500)})
		return pos
	}
	sellIndexPuts := rpc.OrderDraft{Action: rpc.OrderActionSell, Quantity: 2, OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay,
		Contract: rpc.ContractParams{ConID: indexPuts, Symbol: "SYNX", SecType: "OPT", Exchange: "SMART", Currency: "EUR", Expiry: "20261218", Strike: 500, Right: "P", Multiplier: 100}}
	err := deltaTestGate(withHedges(), sellIndexPuts, protectiveExitTestPosition(2, rpc.OrderActionSell, 2), 24000)
	if err == nil || !strings.Contains(err.Error(), "does not lower the absolute delta of the whole book (95,400 EUR before, 135,400 EUR after), so the cap applies") {
		t.Fatalf("closing the index hedge of a net-long book: %v, want the book refusal naming both figures", err)
	}

	withShort := deltaTestPositions()
	// A short stock of 100 shares at 50: −5,000; book 130,400.
	withShort.Stocks = append(withShort.Stocks, rpc.PositionView{Symbol: "SHRT", SecType: rpc.SecTypeStock, ConID: shortStock, Currency: "EUR", Quantity: -100, Multiplier: 1, Mark: 50, MarketValue: -5000})
	coverShort := rpc.OrderDraft{Action: rpc.OrderActionBuy, Quantity: 100, OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay,
		Contract: rpc.ContractParams{ConID: shortStock, Symbol: "SHRT", SecType: "STK", Exchange: "SMART", Currency: "EUR"}}
	err = deltaTestGate(withShort, coverShort, protectiveExitTestPosition(-100, rpc.OrderActionBuy, 100), 15000)
	if err == nil || !strings.Contains(err.Error(), "does not lower the absolute delta of the whole book (130,400 EUR before, 135,400 EUR after)") {
		t.Fatalf("covering a short stock in a net-long book: %v, want the book refusal", err)
	}

	sell := deltaTestStockDraft(rpc.OrderActionSell, 400)
	if err := deltaTestGate(deltaTestPositions(), sell, protectiveExitTestPosition(1000, rpc.OrderActionSell, 400), 30000); err != nil {
		t.Fatalf("a stock sale in a net-long book must pass: %v", err)
	}

	unmeasured := deltaTestPositions()
	unmeasured.Stocks[2].Mark = 0 // OTHR, another underlying, has no price
	err = deltaTestGate(unmeasured, sell, protectiveExitTestPosition(1000, rpc.OrderActionSell, 400), 30000)
	if err == nil || !strings.Contains(err.Error(), "cannot be measured (the OTHR stock line has no price)") {
		t.Fatalf("an unmeasured line elsewhere in the book: %v, want the cap with the line named", err)
	}

	defunct := deltaTestPositions()
	defunct.Stocks = append(defunct.Stocks, rpc.PositionView{Symbol: "DEFN", SecType: rpc.SecTypeStock, ConID: 7004, Currency: "EUR", Quantity: 10, Multiplier: 1,
		Stale: true, StaleReason: "zero-value portfolio row; likely inactive or defunct", WarningDetails: []rpc.DataWarning{{Code: zeroValueStockPositionCode, Scope: "DEFN"}}})
	ev := measureDeltaReduction(defunct, deltaTestScope, sell)
	if !ev.Current || ev.BookBefore != 135400 || ev.NetBefore != 110400 {
		t.Fatalf("a zero-value defunct row and a bond row must count as zero: %+v", ev)
	}
	if err := deltaTestGate(defunct, sell, protectiveExitTestPosition(1000, rpc.OrderActionSell, 400), 30000); err != nil {
		t.Fatalf("the sale must still pass beside a defunct row and a bond: %v", err)
	}
}

// Owner decision 2026-10-07 09:51 CEST, "Keep the cap": an exit that would
// leave a short option uncovered keeps the order cap, on the principle of
// Canary's own option combos (never leave a short leg uncovered). A
// covered-call stock sale above the cap stays capped though delta falls; a
// sale that leaves enough shares to cover the calls passes; selling the long
// leg of a call spread on its own stays capped; closing the spread as one
// combo passes.
func TestDeltaRuleKeepsTheCapForAnUncoveredShortLeg(t *testing.T) {
	t.Parallel()
	f := func(v float64) *float64 { return &v }
	call := func(conID int, qty, delta float64) rpc.PositionView {
		return rpc.PositionView{Symbol: "SYNB", SecType: rpc.SecTypeOption, ConID: conID, Currency: "EUR", Quantity: qty, Multiplier: 100,
			Mark: 3, MarketValue: 300 * qty, Expiry: "20261218", Strike: 75, Right: "C", Delta: f(delta), Underlying: f(75)}
	}
	book := func(options ...rpc.PositionView) *rpc.PositionsResult {
		pos := deltaTestPositions()
		pos.Options = options
		return pos
	}
	// 1,000 shares (+120,000) under 8 short calls (−48,000): net 72,000.
	covered := book(call(deltaTestShortCalls, -8, 0.5))
	err := deltaTestGate(covered, deltaTestStockDraft(rpc.OrderActionSell, 400), protectiveExitTestPosition(1000, rpc.OrderActionSell, 400), 30000)
	if err == nil || !strings.Contains(err.Error(), "order cap in force 12,000 EUR") || !strings.Contains(err.Error(), "this sale would leave 2 short calls on SYNB uncovered, so the order cap applies") {
		t.Fatalf("selling the stock under covered calls: %v, want the cap with the uncovered calls named", err)
	}
	if err := deltaTestGate(covered, deltaTestStockDraft(rpc.OrderActionSell, 200), protectiveExitTestPosition(1000, rpc.OrderActionSell, 200), 15000); err != nil {
		t.Fatalf("a sale that leaves 800 shares under 8 short calls must pass: %v", err)
	}

	// A call spread without stock: 12 long calls (delta 0.6, +86,400) over 8
	// short calls (delta 0.5, −48,000): net 38,400.
	spread := book(call(deltaTestLongCalls, 12, 0.6), call(deltaTestShortCalls, -8, 0.5))
	spread.Stocks = spread.Stocks[1:]
	err = deltaTestGate(spread, deltaTestOptionDraft(deltaTestLongCalls, "C", rpc.OrderActionSell, 6), protectiveExitTestPosition(12, rpc.OrderActionSell, 6), 1800)
	if err == nil || !strings.Contains(err.Error(), "option cap in force of 5 contracts") || !strings.Contains(err.Error(), "this sale would leave 2 short calls on SYNB uncovered, so the order cap applies") {
		t.Fatalf("selling the long leg of a spread on its own: %v, want the cap with the uncovered calls named", err)
	}
	draft, _ := deltaTestStrategyDraft(8, deltaTestLeg(deltaTestLongCalls, 1, rpc.OrderActionSell, 8, 12), deltaTestLeg(deltaTestShortCalls, -1, rpc.OrderActionBuy, 8, -8))
	draft.StrategyGroup.Operation, draft.StrategyGroup.UnitsBefore, draft.StrategyGroup.UnitsAfter = rpc.StrategyOperationReduce, 12, 4
	draft.Quantity = 8
	position := rpc.OrderPositionImpact{Before: 12, After: 4, Effect: rpc.OrderPositionEffectReduce}
	if err := deltaTestGate(spread, draft, position, 20000); err != nil {
		t.Fatalf("closing the spread as one combo must pass: %v", err)
	}
}
