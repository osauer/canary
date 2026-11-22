package strategy

import (
	"testing"

	"github.com/osauer/canary/v3/internal/rpc"
)

func shapeLeg(conID int, expiry, right string, strike, quantity float64) rpc.PositionView {
	return rpc.PositionView{Symbol: "SYNC", SecType: "OPT", ConID: conID, Currency: "USD", Expiry: expiry, Right: right, Strike: strike, Multiplier: 100, Quantity: quantity}
}

func shapeStock(quantity float64) []rpc.PositionView {
	return []rpc.PositionView{{Symbol: "SYNC", SecType: "STK", ConID: 77, Exchange: "NASDAQ", Currency: "USD", Quantity: quantity}}
}

// The daemon names the held position, direction included, so no client
// derives "bear put spread" from the legs. The shape names clients already
// read stay as they were.
func TestTwoLegStrategiesCarryTheTradersName(t *testing.T) {
	const near, far = "20261218", "20270115"
	for _, tc := range []struct {
		a, b       rpc.PositionView
		kind, name string
	}{
		{shapeLeg(1, near, "P", 145, 2), shapeLeg(2, near, "P", 135, -2), "vertical", "bear put spread"},
		{shapeLeg(1, near, "P", 135, 2), shapeLeg(2, near, "P", 145, -2), "vertical", "bull put spread"},
		{shapeLeg(1, near, "C", 100, 1), shapeLeg(2, near, "C", 110, -1), "vertical", "bull call spread"},
		{shapeLeg(1, near, "C", 110, 1), shapeLeg(2, near, "C", 100, -1), "vertical", "bear call spread"},
		// Uneven legs have no direction a name could promise.
		{shapeLeg(1, near, "P", 180, 3), shapeLeg(2, near, "P", 170, -6), "vertical", "put ratio spread"},
		{shapeLeg(1, near, "C", 110, 4), shapeLeg(2, near, "C", 100, -2), "vertical", "call backspread"},
		{shapeLeg(1, far, "C", 100, 1), shapeLeg(2, near, "C", 100, -1), "calendar", "call calendar spread"},
		{shapeLeg(1, near, "P", 100, 1), shapeLeg(2, far, "P", 100, -1), "calendar", "short put calendar spread"},
		{shapeLeg(1, far, "C", 95, 1), shapeLeg(2, near, "C", 105, -1), "diagonal", "call diagonal spread"},
		{shapeLeg(1, far, "C", 95, 2), shapeLeg(2, near, "C", 105, -3), "diagonal", ""},
		{shapeLeg(1, near, "C", 100, 1), shapeLeg(2, near, "P", 100, 1), "straddle", "long straddle"},
		{shapeLeg(1, near, "C", 100, -2), shapeLeg(2, near, "P", 100, -2), "straddle", "short straddle"},
		{shapeLeg(1, near, "C", 110, 1), shapeLeg(2, near, "P", 90, 1), "strangle", "long strangle"},
		{shapeLeg(1, near, "C", 110, -1), shapeLeg(2, near, "P", 90, -1), "strangle", "short strangle"},
		{shapeLeg(1, near, "C", 110, 1), shapeLeg(2, near, "P", 90, -1), "risk_reversal", "bullish risk reversal"},
		{shapeLeg(1, near, "P", 90, 1), shapeLeg(2, near, "C", 110, -1), "risk_reversal", "bearish risk reversal"},
		{shapeLeg(1, near, "C", 100, 1), shapeLeg(2, near, "P", 100, -1), "risk_reversal", "synthetic long"},
		{shapeLeg(1, near, "P", 100, 1), shapeLeg(2, near, "C", 100, -1), "risk_reversal", "synthetic short"},
		{shapeLeg(1, near, "C", 100, 1), shapeLeg(2, far, "P", 100, -1), "two_leg", ""},
	} {
		strategies, issues := InferPositionStrategies([]rpc.PositionView{tc.a, tc.b})
		if len(issues) != 0 || len(strategies) != 1 {
			t.Fatalf("%s: strategies=%+v issues=%+v", tc.name, strategies, issues)
		}
		if got := strategies[0]; got.Kind != tc.kind || got.Name != tc.name || got.Stock != nil || !got.Actionable {
			t.Errorf("legs %g%s/%g%s: kind %q name %q, want %q %q", tc.a.Strike, tc.a.Right, tc.b.Strike, tc.b.Right, got.Kind, got.Name, tc.kind, tc.name)
		}
	}
}

// Three or four legs form a strategy only on an exact match of one shape.
func TestButterfliesAndIronCondorsGroupOnAnExactMatch(t *testing.T) {
	const expiry = "20261218"
	for _, tc := range []struct {
		rows       []rpc.PositionView
		kind, name string
		units      int
	}{
		{[]rpc.PositionView{shapeLeg(1, expiry, "C", 90, 2), shapeLeg(2, expiry, "C", 100, -4), shapeLeg(3, expiry, "C", 110, 2)}, "butterfly", "long call butterfly", 2},
		{[]rpc.PositionView{shapeLeg(1, expiry, "P", 90, -1), shapeLeg(2, expiry, "P", 100, 2), shapeLeg(3, expiry, "P", 110, -1)}, "butterfly", "short put butterfly", 1},
		{[]rpc.PositionView{shapeLeg(1, expiry, "P", 80, 3), shapeLeg(2, expiry, "P", 90, -3), shapeLeg(3, expiry, "C", 110, -3), shapeLeg(4, expiry, "C", 120, 3)}, "iron_condor", "iron condor", 3},
		{[]rpc.PositionView{shapeLeg(1, expiry, "P", 80, -1), shapeLeg(2, expiry, "P", 90, 1), shapeLeg(3, expiry, "C", 110, 1), shapeLeg(4, expiry, "C", 120, -1)}, "iron_condor", "reverse iron condor", 1},
		{[]rpc.PositionView{shapeLeg(1, expiry, "P", 90, 1), shapeLeg(2, expiry, "P", 100, -1), shapeLeg(3, expiry, "C", 100, -1), shapeLeg(4, expiry, "C", 110, 1)}, "iron_butterfly", "iron butterfly", 1},
	} {
		strategies, issues := InferPositionStrategies(tc.rows)
		if len(issues) != 0 || len(strategies) != 1 {
			t.Fatalf("%s: strategies=%+v issues=%+v", tc.name, strategies, issues)
		}
		got := strategies[0]
		if got.Kind != tc.kind || got.Name != tc.name || got.Units != tc.units || len(got.Legs) != len(tc.rows) || !got.Actionable || got.ID == "" || got.PositionFingerprint == "" {
			t.Errorf("%s: %+v", tc.name, got)
		}
		for _, leg := range got.Legs {
			if float64(leg.Ratio*got.Units) != leg.Quantity {
				t.Errorf("%s: leg %d ratio %d × %d units is not its %g contracts", tc.name, leg.Contract.ConID, leg.Ratio, got.Units, leg.Quantity)
			}
		}
	}

	// Anything short of the exact shape keeps the conservative answer: the
	// legs stay standalone and the underlying is named as ungrouped.
	for name, rows := range map[string][]rpc.PositionView{
		"uneven wings":           {shapeLeg(1, expiry, "C", 90, 1), shapeLeg(2, expiry, "C", 100, -2), shapeLeg(3, expiry, "C", 115, 1)},
		"a wing one short":       {shapeLeg(1, expiry, "C", 90, 2), shapeLeg(2, expiry, "C", 100, -4), shapeLeg(3, expiry, "C", 110, 1)},
		"two expiries":           {shapeLeg(1, expiry, "C", 90, 1), shapeLeg(2, expiry, "C", 100, -2), shapeLeg(3, "20270115", "C", 110, 1)},
		"mixed rights of three":  {shapeLeg(1, expiry, "C", 90, 1), shapeLeg(2, expiry, "P", 100, -2), shapeLeg(3, expiry, "C", 110, 1)},
		"spreads that overlap":   {shapeLeg(1, expiry, "P", 80, 1), shapeLeg(2, expiry, "P", 115, -1), shapeLeg(3, expiry, "C", 110, -1), shapeLeg(4, expiry, "C", 120, 1)},
		"condor sides disagree":  {shapeLeg(1, expiry, "P", 80, 1), shapeLeg(2, expiry, "P", 90, -1), shapeLeg(3, expiry, "C", 110, 1), shapeLeg(4, expiry, "C", 120, -1)},
		"condor of uneven sides": {shapeLeg(1, expiry, "P", 80, 1), shapeLeg(2, expiry, "P", 90, -1), shapeLeg(3, expiry, "C", 110, -2), shapeLeg(4, expiry, "C", 120, 2)},
		"five legs":              {shapeLeg(1, expiry, "P", 80, 1), shapeLeg(2, expiry, "P", 90, -1), shapeLeg(3, expiry, "C", 110, -1), shapeLeg(4, expiry, "C", 120, 1), shapeLeg(5, expiry, "C", 130, 1)},
	} {
		strategies, issues := InferPositionStrategies(rows)
		if len(strategies) != 0 || len(issues) != 1 || issues[0].Reason != "multiple strategy decompositions are possible" || issues[0].LegCount != len(rows) {
			t.Errorf("%s: strategies=%+v issues=%+v", name, strategies, issues)
		}
	}
}

// A call written against the shares, or a long put with a short call beside
// them, is named for what it is. The shares name the position and join no
// combo: the option legs stay the strategy's legs.
func TestCoveredCallsAndCollarsAreNamedWithTheirShares(t *testing.T) {
	const expiry = "20261218"
	call := shapeLeg(1, expiry, "C", 110, -2)
	put := shapeLeg(2, expiry, "P", 90, 2)

	covered, issues := InferStrategies(shapeStock(250), []rpc.PositionView{call})
	if len(issues) != 0 || len(covered) != 1 {
		t.Fatalf("covered call: strategies=%+v issues=%+v", covered, issues)
	}
	got := covered[0]
	if got.Kind != "covered_call" || got.Name != "covered call" || got.Units != 2 || len(got.Legs) != 1 || got.Legs[0].Ratio != -1 ||
		got.Stock == nil || got.Stock.Contract.ConID != 77 || got.Stock.Contract.SecType != "STK" || got.Stock.Quantity != 200 || got.Stock.Ratio != 100 {
		t.Fatalf("covered call: %+v stock %+v", got, got.Stock)
	}
	// Its call is bought back on its own order: no combo carries the group.
	if got.Actionable || got.Reason == "" {
		t.Fatalf("a covered call reads as a combo Canary routes: %+v", got)
	}

	collared, issues := InferStrategies(shapeStock(200), []rpc.PositionView{call, put})
	if len(issues) != 0 || len(collared) != 1 {
		t.Fatalf("collar: strategies=%+v issues=%+v", collared, issues)
	}
	got = collared[0]
	if got.Kind != "collar" || got.Name != "collar" || got.Units != 2 || len(got.Legs) != 2 || got.Stock == nil || got.Stock.Quantity != 200 || !got.Actionable {
		t.Fatalf("collar: %+v stock %+v", got, got.Stock)
	}
	// The shares are no part of the group's identity: the option pair keeps
	// the id and fingerprint it has without them.
	bare, _ := InferPositionStrategies([]rpc.PositionView{call, put})
	if bare[0].Kind != "risk_reversal" || bare[0].ID != got.ID || bare[0].PositionFingerprint != got.PositionFingerprint {
		t.Fatalf("the shares changed the option pair's identity: %+v against %+v", got, bare[0])
	}

	// Shares that do not cover every contract, shares in another currency
	// or on two lines, a long call and a short stock leave things as they
	// are without the shares.
	short := shapeStock(-200)
	foreign := shapeStock(200)
	foreign[0].Currency = "EUR"
	twoLines := append(shapeStock(200), rpc.PositionView{Symbol: "SYNC", SecType: "STK", ConID: 78, Currency: "USD", Quantity: 100})
	for name, stocks := range map[string][]rpc.PositionView{"too few shares": shapeStock(150), "short shares": short, "another currency": foreign, "two lines": twoLines} {
		if got, issues := InferStrategies(stocks, []rpc.PositionView{call}); len(got) != 0 || len(issues) != 0 {
			t.Errorf("%s: a lone call was grouped: %+v %+v", name, got, issues)
		}
		if got, _ := InferStrategies(stocks, []rpc.PositionView{call, put}); len(got) != 1 || got[0].Kind != "risk_reversal" || got[0].Stock != nil {
			t.Errorf("%s: the pair was read as a collar: %+v", name, got)
		}
	}
	if got, issues := InferStrategies(shapeStock(200), []rpc.PositionView{shapeLeg(1, expiry, "C", 110, 2)}); len(got) != 0 || len(issues) != 0 {
		t.Fatalf("a long call beside shares was grouped: %+v", got)
	}
	// A spread beside shares stays the spread.
	spread, _ := InferStrategies(shapeStock(200), []rpc.PositionView{shapeLeg(1, expiry, "P", 100, 2), shapeLeg(2, expiry, "P", 90, -2)})
	if len(spread) != 1 || spread[0].Kind != "vertical" || spread[0].Name != "bear put spread" || spread[0].Stock != nil {
		t.Fatalf("a put spread beside shares: %+v", spread)
	}
}
