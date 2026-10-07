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
)

// Synthetic book throughout; no number is an account's. One underlying,
// SYNB, at 75 EUR: 1,000 shares long, 10 long puts and 8 short puts (delta
// −0.4), 8 long calls and 8 short calls (delta 0.5), all with multiplier
// 100. Dollar deltas in base currency as positionDollarDelta measures them:
// stock +75,000; long puts −30,000; short puts +24,000; long calls +30,000;
// short calls −30,000; net +69,000.
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
	return deltaReductionEvidence{Current: true, Underlying: "SYNB", BaseCurrency: "EUR", NetBefore: 69000,
		Legs: map[int]deltaReductionLeg{
			deltaTestStock:      {Quantity: 1000, UnitBase: 75},
			deltaTestLongPuts:   {Quantity: 10, UnitBase: -3000},
			deltaTestShortPuts:  {Quantity: -8, UnitBase: -3000},
			deltaTestLongCalls:  {Quantity: 8, UnitBase: 3750},
			deltaTestShortCalls: {Quantity: -8, UnitBase: 3750},
		}}
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
	return rpc.StrategyOrderLeg{Contract: rpc.ContractParams{ConID: conID, Symbol: "SYNB", SecType: "OPT", Currency: "EUR", Multiplier: 100},
		Ratio: ratio, Action: action, Quantity: qty, Before: before, After: after}
}

// A close or reduction that lowers the absolute net delta of its underlying
// passes the notional cap and the option-contract cap (owner decision
// 2026-10-07 08:13 CEST). A close that raises it, an unknown delta, an
// opening or adding order and a flip keep the caps; the sell-as-short
// re-read stays. The cap in force here is 12,000 EUR (5% of NLV 240,000) and
// 5 option contracts.
func TestDeltaReducingExitPassesTheCaps(t *testing.T) {
	t.Parallel()
	ev := deltaTestEvidence()
	stale := deltaReductionEvidence{Underlying: "SYNB", Reason: "the SYNB stock line has a stale quote"}
	mismatch := deltaTestEvidence()
	mismatch.Legs[deltaTestStock] = deltaReductionLeg{Quantity: 900, UnitBase: 75}
	cases := []struct {
		name     string
		edit     func(*risk.ConstitutionOrderLimits)
		draft    rpc.OrderDraft
		position rpc.OrderPositionImpact
		notional float64
		evidence deltaReductionEvidence
		wantErr  []string
		wantNot  []string
	}{
		{name: "a stock sale that shrinks a long above the notional cap passes", edit: func(o *risk.ConstitutionOrderLimits) { o.AllowStockShort = new(true) },
			draft: deltaTestStockDraft(rpc.OrderActionSell, 400), position: protectiveExitTestPosition(1000, rpc.OrderActionSell, 400), notional: 30000, evidence: ev},
		{name: "the same sale keeps the sell-as-short re-read",
			draft: deltaTestStockDraft(rpc.OrderActionSell, 400), position: protectiveExitTestPosition(1000, rpc.OrderActionSell, 400), notional: 30000, evidence: ev,
			wantErr: []string{"allow_stock_short"}, wantNot: []string{"order cap in force"}},
		{name: "an option buy-to-close above the contract cap that lowers the delta passes",
			draft: deltaTestOptionDraft(deltaTestShortPuts, "P", rpc.OrderActionBuy, 8), position: protectiveExitTestPosition(-8, rpc.OrderActionBuy, 8), notional: 2000, evidence: ev},
		{name: "an option sell-to-close above both caps that lowers the delta passes", edit: func(o *risk.ConstitutionOrderLimits) { o.AllowOptionSellToOpen = new(true) },
			draft: deltaTestOptionDraft(deltaTestLongCalls, "C", rpc.OrderActionSell, 8), position: protectiveExitTestPosition(8, rpc.OrderActionSell, 8), notional: 16000, evidence: ev},
		{name: "closing a long put that hedges the long stock raises the delta and stays capped", edit: func(o *risk.ConstitutionOrderLimits) { o.AllowOptionSellToOpen = new(true) },
			draft: deltaTestOptionDraft(deltaTestLongPuts, "P", rpc.OrderActionSell, 10), position: protectiveExitTestPosition(10, rpc.OrderActionSell, 10), notional: 15000, evidence: ev,
			wantErr: []string{"cap in force", "does not lower the absolute delta of SYNB (69,000 EUR before, 99,000 EUR after)"}},
		{name: "buying back a short call that hedges the long stock stays capped",
			draft: deltaTestOptionDraft(deltaTestShortCalls, "C", rpc.OrderActionBuy, 8), position: protectiveExitTestPosition(-8, rpc.OrderActionBuy, 8), notional: 2000, evidence: ev,
			wantErr: []string{"option cap in force of 5 contracts", "does not lower the absolute delta of SYNB"}},
		{name: "an unknown delta stays capped and says so", edit: func(o *risk.ConstitutionOrderLimits) { o.AllowStockShort = new(true) },
			draft: deltaTestStockDraft(rpc.OrderActionSell, 400), position: protectiveExitTestPosition(1000, rpc.OrderActionSell, 400), notional: 30000,
			wantErr: []string{"order cap in force 12,000 EUR", "cannot be measured (no current measurement)"}},
		{name: "a stale measurement stays capped and names the stale line", edit: func(o *risk.ConstitutionOrderLimits) { o.AllowStockShort = new(true) },
			draft: deltaTestStockDraft(rpc.OrderActionSell, 400), position: protectiveExitTestPosition(1000, rpc.OrderActionSell, 400), notional: 30000, evidence: stale,
			wantErr: []string{"order cap in force 12,000 EUR", "the SYNB stock line has a stale quote"}},
		{name: "a measurement of another position does not exempt", edit: func(o *risk.ConstitutionOrderLimits) { o.AllowStockShort = new(true) },
			draft: deltaTestStockDraft(rpc.OrderActionSell, 400), position: protectiveExitTestPosition(1000, rpc.OrderActionSell, 400), notional: 30000, evidence: mismatch,
			wantErr: []string{"order cap in force 12,000 EUR", "differs from the order's"}},
		{name: "an opening order above the cap stays refused",
			draft: deltaTestStockDraft(rpc.OrderActionBuy, 400), position: protectiveExitTestPosition(0, rpc.OrderActionBuy, 400), notional: 30000, evidence: ev,
			wantErr: []string{"order cap in force 12,000 EUR"}, wantNot: []string{"delta"}},
		{name: "an order that adds to a position above the cap stays refused",
			draft: deltaTestStockDraft(rpc.OrderActionBuy, 400), position: protectiveExitTestPosition(1000, rpc.OrderActionBuy, 400), notional: 30000, evidence: ev,
			wantErr: []string{"order cap in force 12,000 EUR"}, wantNot: []string{"delta"}},
		{name: "an order that would flip the position stays refused", edit: func(o *risk.ConstitutionOrderLimits) { o.AllowStockShort = new(true) },
			draft: deltaTestStockDraft(rpc.OrderActionSell, 1200), position: protectiveExitTestPosition(1000, rpc.OrderActionSell, 1200), notional: 90000, evidence: ev,
			wantErr: []string{"order cap in force 12,000 EUR"}, wantNot: []string{"delta"}},
		{name: "an option opening above the contract cap stays refused",
			draft: deltaTestOptionDraft(deltaTestLongCalls, "C", rpc.OrderActionBuy, 8), position: protectiveExitTestPosition(0, rpc.OrderActionBuy, 8), notional: 2000, evidence: ev,
			wantErr: []string{"option cap in force of 5 contracts"}, wantNot: []string{"delta"}},
		{name: "a bond sale above the cap keeps it: a bond carries no equity delta",
			draft: rpc.OrderDraft{Action: rpc.OrderActionSell, Quantity: 30, OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay,
				Contract: rpc.ContractParams{ConID: 7901, Symbol: "SYNB", SecType: "BOND", Exchange: "SMART", Currency: "EUR"}},
			position: protectiveExitTestPosition(100, rpc.OrderActionSell, 30), notional: 30000, evidence: ev,
			wantErr: []string{"order cap in force 12,000 EUR", "a bill or bond carries no equity delta, so the exemption for delta-reducing exits cannot apply"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			table := testOrderLimitsTable(10000)
			if tc.edit != nil {
				tc.edit(table)
			}
			limits := risk.EvaluateOrderLimits(table, "EUR", risk.OrderLimitsNLV{Base: 240000, AsOf: time.Now()}, nil, "")
			err := validateOrderRiskAuthority(limits, tc.draft, tc.position, protectiveExitTestNotional(tc.notional), "EUR", protectiveExitInventory{}, tc.evidence)
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
// that raises it keeps both.
func TestDeltaReducingStrategyClosePassesTheCaps(t *testing.T) {
	t.Parallel()
	table := testOrderLimitsTable(10000)
	limits := risk.EvaluateOrderLimits(table, "EUR", risk.OrderLimitsNLV{Base: 240000, AsOf: time.Now()}, nil, "")
	ev := deltaTestEvidence()

	// Long calls sold and short puts bought back: −30,000 − 24,000 → 15,000.
	draft, position := deltaTestStrategyDraft(8, deltaTestLeg(deltaTestLongCalls, 1, rpc.OrderActionSell, 8, 8), deltaTestLeg(deltaTestShortPuts, -1, rpc.OrderActionBuy, 8, -8))
	if err := validateOrderRiskAuthority(limits, draft, position, protectiveExitTestNotional(20000), "EUR", protectiveExitInventory{}, ev); err != nil {
		t.Fatalf("delta-lowering strategy close refused: %v", err)
	}
	if err := validateOrderRiskAuthority(limits, draft, position, protectiveExitTestNotional(20000), "EUR", protectiveExitInventory{}, deltaReductionEvidence{}); err == nil ||
		!strings.Contains(err.Error(), "order cap in force 12,000 EUR") || !strings.Contains(err.Error(), "cannot be measured") {
		t.Fatalf("without a measurement the strategy close must keep the cap: %v", err)
	}
	if err := validateOrderRiskAuthority(limits, draft, position, protectiveExitTestNotional(2000), "EUR", protectiveExitInventory{}, deltaReductionEvidence{}); err == nil ||
		!strings.Contains(err.Error(), "proportional option quantity limit") {
		t.Fatalf("without a measurement each leg must keep the contract cap: %v", err)
	}

	// Long puts sold and short calls bought back: +30,000 + 30,000 → 129,000.
	draft, position = deltaTestStrategyDraft(2, deltaTestLeg(deltaTestLongPuts, 5, rpc.OrderActionSell, 10, 10), deltaTestLeg(deltaTestShortCalls, -4, rpc.OrderActionBuy, 8, -8))
	if err := validateOrderRiskAuthority(limits, draft, position, protectiveExitTestNotional(15000), "EUR", protectiveExitInventory{}, ev); err == nil ||
		!strings.Contains(err.Error(), "does not lower the absolute delta of SYNB (69,000 EUR before, 129,000 EUR after)") {
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
			Mark: 3, MarketValue: 300 * qty, Expiry: "20261218", Strike: 75, Right: right, Delta: f(delta), Underlying: f(75)}
	}
	return &rpc.PositionsResult{
		AsOf: time.Date(2026, 10, 7, 8, 0, 0, 0, time.UTC),
		Authority: &rpc.AccountDataAuthority{Availability: rpc.AccountDataAvailable, Freshness: rpc.AccountDataFreshnessCurrent,
			Scope: rpc.AccountDataScope{AccountID: deltaTestScope.Account, AccountMode: deltaTestScope.Mode}},
		Portfolio: &rpc.PositionsPortfolio{BaseCurrency: "EUR"},
		Stocks: []rpc.PositionView{
			{Symbol: "SYNB", SecType: rpc.SecTypeStock, ConID: deltaTestStock, Currency: "EUR", Quantity: 1000, Multiplier: 1, Mark: 75, MarketValue: 75000},
			{Symbol: "SYNB", SecType: "BOND", ConID: 7901, Currency: "EUR", Quantity: 10000, Multiplier: 1, Mark: 99, MarketValue: 9900},
			{Symbol: "OTHR", SecType: rpc.SecTypeStock, ConID: 7002, Currency: "EUR", Quantity: 50, Multiplier: 1, Mark: 10, MarketValue: 500},
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
// anything it cannot measure.
func TestMeasureDeltaReductionReadsTheDaemonsDeltas(t *testing.T) {
	t.Parallel()
	sell := deltaTestStockDraft(rpc.OrderActionSell, 400)
	ev := measureDeltaReduction(deltaTestPositions(), deltaTestScope, sell)
	if !ev.Current || ev.NetBefore != 69000 || ev.BaseCurrency != "EUR" || ev.Legs[deltaTestStock] != (deltaReductionLeg{Quantity: 1000, UnitBase: 75}) {
		t.Fatalf("measurement = %+v, want net 69,000 EUR and the stock leg", ev)
	}
	if ok, why := deltaReducingExit(sell, protectiveExitTestPosition(1000, rpc.OrderActionSell, 400), ev); !ok || why != "" {
		t.Fatalf("stock sale: exempt %v %q, want exempt", ok, why)
	}
	puts := deltaTestOptionDraft(deltaTestLongPuts, "P", rpc.OrderActionSell, 10)
	ev = measureDeltaReduction(deltaTestPositions(), deltaTestScope, puts)
	if !ev.Current || ev.Legs[deltaTestLongPuts] != (deltaReductionLeg{Quantity: 10, UnitBase: -3000}) {
		t.Fatalf("put measurement = %+v", ev)
	}
	if ok, why := deltaReducingExit(puts, protectiveExitTestPosition(10, rpc.OrderActionSell, 10), ev); ok || !strings.Contains(why, "does not lower the absolute delta of SYNB (69,000 EUR before, 99,000 EUR after)") {
		t.Fatalf("hedge put sale: exempt %v %q, want capped", ok, why)
	}
	if _, why := deltaReducingExit(deltaTestStockDraft(rpc.OrderActionBuy, 400), protectiveExitTestPosition(0, rpc.OrderActionBuy, 400), ev); why != "" {
		t.Fatalf("an opening order must carry no delta reason, got %q", why)
	}

	cases := []struct {
		name   string
		edit   func(*rpc.PositionsResult)
		draft  rpc.OrderDraft
		reason string
	}{
		{name: "missing option delta", edit: func(p *rpc.PositionsResult) { p.Options[1].Delta = nil }, draft: sell, reason: "the SYNB 20261218 P 75 option line has no delta or underlying spot"},
		{name: "missing option spot", edit: func(p *rpc.PositionsResult) { p.Options[0].Underlying = nil }, draft: sell, reason: "has no delta or underlying spot"},
		{name: "stale stock quote", edit: func(p *rpc.PositionsResult) { p.Stocks[0].Stale = true }, draft: sell, reason: "the SYNB stock line has a stale quote"},
		{name: "stock without a mark", edit: func(p *rpc.PositionsResult) { p.Stocks[0].Mark = 0 }, draft: sell, reason: "the SYNB stock line has no mark"},
		{name: "leg without an FX rate", edit: func(p *rpc.PositionsResult) { p.Options[2].Currency = "USD" }, draft: sell, reason: "has no FX rate to EUR"},
		{name: "positions not current", edit: func(p *rpc.PositionsResult) { p.Authority.Freshness = rpc.AccountDataFreshnessStale }, draft: sell, reason: "current positions are unavailable"},
		{name: "another account", edit: func(p *rpc.PositionsResult) { p.Authority.Scope.AccountID = "DU7654321" }, draft: sell, reason: "belongs to another account session"},
		{name: "unknown base currency", edit: func(p *rpc.PositionsResult) { p.Portfolio = nil }, draft: sell, reason: "the account base currency is unknown"},
		{name: "duplicate rows", edit: func(p *rpc.PositionsResult) { p.Stocks = append(p.Stocks, p.Stocks[0]) }, draft: sell, reason: "appears in duplicate rows"},
		{name: "contract not held", edit: func(*rpc.PositionsResult) {}, draft: deltaTestOptionDraft(7199, "C", rpc.OrderActionSell, 1), reason: "contract 7199 is not a held line of SYNB"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pos := deltaTestPositions()
			tc.edit(pos)
			ev := measureDeltaReduction(pos, deltaTestScope, tc.draft)
			if ev.Current || !strings.Contains(ev.Reason, tc.reason) {
				t.Fatalf("measurement = %+v, want not current with %q", ev, tc.reason)
			}
		})
	}
	if ev := measureDeltaReduction(nil, deltaTestScope, sell); ev.Current || ev.Reason != "current positions are unavailable" {
		t.Fatalf("nil positions = %+v", ev)
	}
}

// Through the preview: an option buy-to-close above the contract cap passes
// when the positions read shows it lowers the underlying's absolute delta,
// and is refused with order_risk_limit, saying why, when no positions can be
// read.
func TestOrderPreviewAdmitsADeltaReducingExitAboveTheCaps(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	srv.orderPreviewQuote = fixedPreviewQuote(2.4, 2.6)
	srv.orderPreviewPositionImpact = fixedPreviewPosition(-8, 0, rpc.OrderPositionEffectClose)
	srv.orderDeltaPositionsForTest = func(context.Context) (*rpc.PositionsResult, error) { return deltaTestPositions(), nil }
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
		!strings.Contains(blockers[0].Message, "option cap in force of 5 contracts") || !strings.Contains(blockers[0].Message, "cannot be measured (the positions read failed: gateway unavailable)") {
		t.Fatalf("preview err %v blockers %+v, want order_risk_limit naming the cap and the failed read", err, blockers)
	}
}
