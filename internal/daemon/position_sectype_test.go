package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// The non-option position slice carries every secType that is not OPT. Before
// 0fab26d a bond or bill frame discarded the whole staged download, so those
// rows never reached the quote path; relaxing the multiplier requirement made
// them publishable and exposed the assumption that the slice holds only stocks.
// A treasury symbolled "T" was decorated with, and day-changed against, AT&T.
func TestFillDailyChangeSkipsNonEquityRows(t *testing.T) {
	t.Parallel()
	srv := &Server{prevCloses: newPrevCloseCache()}
	// The equity's own close reaches the cache under its bare symbol, which is
	// the key a same-ticker bond would collide on.
	srv.prevCloses.put("T", prevCloseEntry{value: 28}, time.Now())

	rows := []rpc.PositionView{
		{Symbol: "T", SecType: "BOND", ConID: 500001, LocalSymbol: "T 4 1/8 11/15/32", Quantity: 10000, Mark: 98.5},
		{Symbol: "T", SecType: rpc.SecTypeStock, ConID: 265598, Quantity: 100, Mark: 29.5},
		{Symbol: "ESZ6", SecType: rpc.SecTypeFuture, ConID: 600001, Quantity: 1, Mark: 5000},
	}
	srv.fillDailyChange(rows)

	bond, stock, future := rows[0], rows[1], rows[2]
	if bond.PrevClose != nil || bond.RegularClose != nil || bond.DayChange != nil || bond.DayChangePct != nil {
		t.Fatalf("bond row borrowed the equity's close: prev=%v regular=%v change=%v pct=%v",
			bond.PrevClose, bond.RegularClose, bond.DayChange, bond.DayChangePct)
	}
	if future.PrevClose != nil || future.DayChange != nil {
		t.Fatalf("future row borrowed a close: prev=%v change=%v", future.PrevClose, future.DayChange)
	}
	if stock.PrevClose == nil || *stock.PrevClose != 28 {
		t.Fatalf("equity row lost its own close: %v", stock.PrevClose)
	}
	if stock.DayChange == nil || *stock.DayChange != 1.5 {
		t.Fatalf("equity day change = %v, want 1.5", stock.DayChange)
	}
}

// The 100-to-1 coercion exists because IB sends stocks with a wire multiplier
// of 100 while PositionView reports per-share semantics. Applying it to every
// non-option row understated any future whose contractual multiplier is exactly
// 100 by two orders of magnitude.
func TestPositionViewMultiplier(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		secType string
		raw     int
		want    int
	}{
		{name: "stock wire multiplier is per-share", secType: "STK", raw: 100, want: 1},
		{name: "stock already per-share", secType: "STK", raw: 1, want: 1},
		{name: "future keeps a contractual 100", secType: "FUT", raw: 100, want: 100},
		{name: "future keeps other multipliers", secType: "FUT", raw: 50, want: 50},
		{name: "option keeps its multiplier", secType: "OPT", raw: 100, want: 100},
		{name: "bond has none and normalises to one unit", secType: "BOND", raw: 0, want: 1},
		{name: "negative normalises to one unit", secType: "FUND", raw: -3, want: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := positionViewMultiplier(test.secType, test.raw); got != test.want {
				t.Fatalf("positionViewMultiplier(%q, %d) = %d, want %d", test.secType, test.raw, got, test.want)
			}
		})
	}
}

// A dual-listed symbol arrives as two equity rows. The raw totals counted both
// while the base-currency totals were derived from PositionGroup.Stock alone,
// which holds one — so GroupMarketValueBase undercounted. That figure is not
// display: rulebook.go feeds it to the single-name concentration rule as
// MarketValueBase, riskReductionProposal gates and sizes a trim from
// GroupMarketValuePctNLV, and proposal_stop_risk derives daily-P&L percent from
// it. Undercounting there lets a name pass a rule it should trip.
func TestGroupByUnderlyingCountsEverySameSymbolEquityRow(t *testing.T) {
	t.Parallel()
	nlv := 100000.0
	stocks := []rpc.PositionView{
		{Symbol: "RIO", SecType: rpc.SecTypeStock, ConID: 1, Currency: "USD", Quantity: 100, Mark: 60, MarketValue: 6000, UnrealizedPnL: 300},
		{Symbol: "RIO", SecType: rpc.SecTypeStock, ConID: 2, Currency: "USD", Quantity: 50, Mark: 60, MarketValue: 3000, UnrealizedPnL: 150},
	}
	groups := groupByUnderlying(stocks, nil, "USD", &nlv)
	if len(groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(groups))
	}
	g := groups[0]

	if g.GroupMarketValue != 9000 {
		t.Fatalf("GroupMarketValue = %v, want 9000", g.GroupMarketValue)
	}
	if g.GroupMarketValueBase == nil || *g.GroupMarketValueBase != 9000 {
		t.Fatalf("GroupMarketValueBase = %v, want 9000 — it must agree with the raw total", g.GroupMarketValueBase)
	}
	if g.GroupUnrealizedPnLBase == nil || *g.GroupUnrealizedPnLBase != 450 {
		t.Fatalf("GroupUnrealizedPnLBase = %v, want 450", g.GroupUnrealizedPnLBase)
	}
	// 9000 of 100000 is 9%; deriving from one row would read 6% and could pass
	// a concentration threshold the full position trips.
	if g.GroupMarketValuePctNLV == nil || *g.GroupMarketValuePctNLV != 9 {
		t.Fatalf("GroupMarketValuePctNLV = %v, want 9", g.GroupMarketValuePctNLV)
	}
	if g.GroupEffectiveDelta == nil || *g.GroupEffectiveDelta != 150 {
		t.Fatalf("GroupEffectiveDelta = %v, want 150 shares across both rows", g.GroupEffectiveDelta)
	}
	// The display gap is deliberate and remains: Stock still carries one row.
	if g.Stock == nil {
		t.Fatal("Stock is nil, want a display representative")
	}
}

func TestPositionQuotesAsStock(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		secType string
		want    bool
	}{
		{secType: rpc.SecTypeStock, want: true},
		{secType: "STK", want: true},
		{secType: "ETF", want: true},
		{secType: "stk", want: true},
		{secType: "BOND", want: false},
		{secType: "BILL", want: false},
		{secType: "FUND", want: false},
		{secType: "CASH", want: false},
		{secType: rpc.SecTypeFuture, want: false},
		{secType: "", want: false},
	} {
		t.Run(test.secType, func(t *testing.T) {
			t.Parallel()
			if got := positionQuotesAsStock(rpc.PositionView{SecType: test.secType}); got != test.want {
				t.Fatalf("positionQuotesAsStock(%q) = %v, want %v", test.secType, got, test.want)
			}
		})
	}
}
