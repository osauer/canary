package daemon

import (
	"math"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func portfolioRow(t *testing.T, r *rpc.PortfolioSnapshotResult, table string, name string) rpc.PortfolioAllocation {
	t.Helper()
	rows := r.Sectors
	if table == "assets" {
		rows = r.AssetClasses
	}
	for _, x := range rows {
		if x.Name == name {
			return x
		}
	}
	t.Fatalf("%s row %q absent in %+v", table, name, rows)
	return rpc.PortfolioAllocation{}
}

func TestPortfolioCannotMultiplyOptionCostTwiceOrHidePartialValues(t *testing.T) {
	n := func(v float64) *float64 { return &v }
	a := &rpc.AccountResult{BaseCurrency: "USD", NetLiquidation: 1000, TotalCash: 1200, Authority: &rpc.AccountDataAuthority{Fields: &rpc.AccountFieldAvailability{NetLiquidation: true, TotalCash: true}}}
	p := &rpc.PositionsResult{Options: []rpc.PositionView{{ConID: 11, Symbol: "SYNTH", SecType: "OPTION", Currency: "USD", Quantity: -2, AvgCost: 100, Multiplier: 100, MarketValueBase: n(-150), Delta: n(0.5), Underlying: n(50)}}}
	r := projectPortfolio(a, p, map[string]underlyingClassification{"SYNTH": {Sector: "Information Technology"}})
	if r.CostBasisBase == nil || *r.CostBasisBase != -200 {
		t.Fatal("option cost multiplied twice")
	}
	if r.CoverageStatus != "complete" {
		t.Fatalf("observed synthetic inputs incomplete: %+v", r)
	}
	p.Options = append(p.Options, rpc.PositionView{ConID: 12, Symbol: "GAP", SecType: "OPTION", Currency: "EUR", Quantity: 1})
	r = projectPortfolio(a, p, nil)
	if r.CoverageStatus != "partial" || r.CostBasisBase != nil {
		t.Fatal("missing FX/cost/classification became complete")
	}
	if x := portfolioRow(t, r, "assets", "Options"); x.Missing != 1 || x.PercentNLV != nil || x.ValueBase == nil || *x.ValueBase != -150 {
		t.Fatal("partial category became full total")
	}
	a.Authority.Fields.TotalCash = false
	a.Authority.Fields.NetLiquidation = false
	r = projectPortfolio(a, p, nil)
	if r.NetLiquidation != nil {
		t.Fatal("unobserved denominator became zero")
	}
	if x := portfolioRow(t, r, "assets", "Cash"); x.ValueBase != nil {
		t.Fatal("unobserved cash became zero")
	}
}

// A defunct holding has no exposure: it must not draw a zero bar, and it must
// not be reported as a classification gap.
func TestPortfolioCannotDrawADefunctHoldingAsExposure(t *testing.T) {
	n := func(v float64) *float64 { return &v }
	a := &rpc.AccountResult{BaseCurrency: "USD", NetLiquidation: 1000, TotalCash: 100, Authority: &rpc.AccountDataAuthority{Fields: &rpc.AccountFieldAvailability{NetLiquidation: true, TotalCash: true}}}
	p := &rpc.PositionsResult{Stocks: []rpc.PositionView{
		{ConID: 1, Symbol: "DEAD", SecType: "STK", Currency: "USD", Quantity: 1000, AvgCost: 3, Mark: 0, MarketValueBase: n(0), QuoteExpectation: rpc.QuoteExpectationNone},
		{ConID: 2, Symbol: "LIVE", SecType: "STK", Currency: "USD", Quantity: 10, AvgCost: 50, Mark: 60, MarketValueBase: n(600)},
	}}
	// A zero-mark row the broker has not ruled on is flagged, not defunct.
	p.Stocks = append(p.Stocks, rpc.PositionView{ConID: 3, Symbol: "LIMBO", SecType: "STK", Currency: "USD", Quantity: 500, AvgCost: 2, Mark: 0, MarketValueBase: n(0), Stale: true,
		WarningDetails: []rpc.DataWarning{{Code: "zero_value_stock_position", Scope: "LIMBO"}}})
	r := projectPortfolio(a, p, map[string]underlyingClassification{"LIVE": {Sector: "Industrials"}})
	if r.DefunctExcluded != 1 || r.UnquotedExcluded != 1 {
		t.Fatalf("defunct %d unquoted %d", r.DefunctExcluded, r.UnquotedExcluded)
	}
	for _, x := range r.Sectors {
		if x.Missing != 0 {
			t.Fatalf("unquoted holding drawn as an unvalued gap: %+v", x)
		}
	}
	for _, x := range r.Sectors {
		if x.Name == sectorUnclassified {
			t.Fatal("defunct holding drawn as unclassified exposure")
		}
	}
	if x := portfolioRow(t, r, "assets", "Stocks"); x.Observed != 1 {
		t.Fatalf("defunct holding counted in asset class: %+v", x)
	}
	if x := portfolioRow(t, r, "sectors", "Industrials"); x.PercentNLV == nil || math.Abs(*x.PercentNLV-60) > 1e-9 {
		t.Fatalf("live holding lost: %+v", x)
	}
	if r.CoverageStatus != "complete" {
		t.Fatalf("defunct exclusion reported as a gap: %s", r.CoverageStatus)
	}
}

// A long index put is an asset on the balance sheet and short the market in
// exposure. The two tables must disagree about it, in that order.
func TestPortfolioCannotCountALongPutAsLongTheMarket(t *testing.T) {
	n := func(v float64) *float64 { return &v }
	a := &rpc.AccountResult{BaseCurrency: "USD", NetLiquidation: 100000, TotalCash: 0, Authority: &rpc.AccountDataAuthority{Fields: &rpc.AccountFieldAvailability{NetLiquidation: true, TotalCash: true}}}
	put := rpc.PositionView{ConID: 5, Symbol: "SPY", SecType: "OPT", Currency: "USD", Quantity: 2, AvgCost: 500, Multiplier: 100, MarketValueBase: n(1200), Delta: n(-0.4), Underlying: n(500), Right: "P"}
	p := &rpc.PositionsResult{Options: []rpc.PositionView{put}}
	r := projectPortfolio(a, p, map[string]underlyingClassification{"SPY": classifyUnderlying("SPY", ibkrlib.MarketClassification{})})
	if x := portfolioRow(t, r, "assets", "Options"); x.ValueBase == nil || *x.ValueBase != 1200 {
		t.Fatalf("premium is the asset-class value: %+v", x)
	}
	// delta dollars: -0.4 × 2 × 100 × 500 = -40000, spread by SPY weights.
	tech := portfolioRow(t, r, "sectors", "Information Technology")
	if tech.ValueBase == nil || *tech.ValueBase >= 0 || math.Abs(*tech.ValueBase-(-40000*38.34/100)) > 1e-6 {
		t.Fatalf("long put not short technology: %+v", tech)
	}
	if len(r.LookThrough) != 1 || r.LookThrough[0].Symbol != "SPY" || r.LookThrough[0].AsOf == "" {
		t.Fatalf("look-through provenance missing: %+v", r.LookThrough)
	}
	if r.SectorMeasure != rpc.AllocationMeasureDeltaNotional || r.AssetClassMeasure != rpc.AllocationMeasureMarketValue {
		t.Fatal("measures unnamed")
	}
	for _, x := range r.Sectors {
		if x.Name == sectorUnclassified || x.Name == sectorFunds {
			t.Fatalf("index fund left unspread: %+v", x)
		}
	}
}

// An option without a delta has an unknown exposure, which is not zero.
func TestPortfolioCannotTreatAMissingDeltaAsZeroExposure(t *testing.T) {
	n := func(v float64) *float64 { return &v }
	a := &rpc.AccountResult{BaseCurrency: "USD", NetLiquidation: 1000, TotalCash: 0, Authority: &rpc.AccountDataAuthority{Fields: &rpc.AccountFieldAvailability{NetLiquidation: true, TotalCash: true}}}
	p := &rpc.PositionsResult{
		Stocks:  []rpc.PositionView{{ConID: 1, Symbol: "ACME", SecType: "STK", Currency: "USD", Quantity: 10, AvgCost: 10, Mark: 20, MarketValueBase: n(200)}},
		Options: []rpc.PositionView{{ConID: 2, Symbol: "ACME", SecType: "OPT", Currency: "USD", Quantity: -1, AvgCost: 100, Multiplier: 100, MarketValueBase: n(-50), Underlying: n(20)}},
	}
	r := projectPortfolio(a, p, map[string]underlyingClassification{"ACME": {Sector: "Energy"}})
	x := portfolioRow(t, r, "sectors", "Energy")
	if x.Missing != 1 || x.PercentNLV != nil || x.ValueBase == nil || *x.ValueBase != 200 {
		t.Fatalf("missing delta silently zero: %+v", x)
	}
	if y := portfolioRow(t, r, "assets", "Options"); y.Missing != 0 || y.ValueBase == nil || *y.ValueBase != -50 {
		t.Fatalf("premium view must not need a delta: %+v", y)
	}
}

func TestPortfolioCannotAssignClassificationAcrossListings(t *testing.T) {
	p := &rpc.PositionsResult{Stocks: []rpc.PositionView{{ConID: 1, Symbol: "SYNTH", SecType: "STOCK", Currency: "USD"}, {ConID: 2, Symbol: "SYNTH", SecType: "STOCK", Currency: "EUR"}}}
	if portfolioClassificationUnambiguous("SYNTH", p) {
		t.Fatal("ambiguous listing classification accepted")
	}
}

func TestClassificationPrecedenceAndBrokerMap(t *testing.T) {
	if c := classifyUnderlying("spy", ibkrlib.MarketClassification{StockType: "ETF"}); !c.Fund || c.LookThrough == nil {
		t.Fatalf("embedded fund table lost: %+v", c)
	}
	if c := classifyUnderlying("XYZ", ibkrlib.MarketClassification{StockType: "ETF", Industry: "Financial"}); !c.Fund || c.LookThrough != nil || c.Sector != "" {
		t.Fatalf("unlisted fund must pool, not map: %+v", c)
	}
	// Wikipedia's GICS sector beats the broker's industry for an S&P name.
	if c := classifyUnderlying("AMZN", ibkrlib.MarketClassification{Industry: "Communications", Category: "Internet"}); c.Sector != "Consumer Discretionary" {
		t.Fatalf("AMZN: %+v", c)
	}
	cases := map[[2]string]string{
		{"Consumer, Non-cyclical", "Pharmaceuticals"}:     "Health Care",
		{"Consumer, Non-cyclical", "Food"}:                "Consumer Staples",
		{"Consumer, Non-cyclical", "Commercial Services"}: "Industrials",
		{"Financial", "REITS"}:                            "Real Estate",
		{"Financial", "Banks"}:                            "Financials",
		{"Technology", "Semiconductors"}:                  "Information Technology",
		{"Communications", "Internet"}:                    "Consumer Discretionary",
		{"Communications", "Telecommunications"}:          "Communication Services",
		{"Basic Materials", "Chemicals"}:                  "Materials",
		{"Diversified", ""}:                               "",
	}
	for in, want := range cases {
		got, ok := gicsFromIBKR(in[0], in[1])
		if got != want || ok != (want != "") {
			t.Fatalf("%v → %q (%v), want %q", in, got, ok, want)
		}
	}
	if c := classifyUnderlying("ZZZZ", ibkrlib.MarketClassification{Industry: "Diversified"}); c.Sector != "" || c.Fund {
		t.Fatalf("unknown pairing must stay unclassified: %+v", c)
	}
}
