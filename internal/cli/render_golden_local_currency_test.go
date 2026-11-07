package cli

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

// TestRenderGoldenLocalCurrency pins what a Hong Kong dollar or Singapore dollar
// account reads, at 80 columns with colour off and on. Every value is synthetic.
func TestRenderGoldenLocalCurrency(t *testing.T) {
	t.Setenv("COLUMNS", "80")
	for _, tc := range []renderGoldenCase{
		{name: "positions_singapore_base", argv: []string{"positions"}, render: func(env *Env) { renderPositionsByUnderlyingTo(env, env.Stdout, goldenSingaporeBook()) }},
		{name: "account_hong_kong_base", argv: []string{"account"}, conn: goldenConn{rpc.MethodAccountSummary: goldenLocalCurrencyAccount("HKD", rpc.CurrencyExposure{Currency: "SGD", NetLiquidationCcy: 43750, ExchangeRate: 5.8, NetLiquidationBase: 253750}, rpc.CurrencyExposure{Currency: "USD", NetLiquidationCcy: 10000, ExchangeRate: 7.8, NetLiquidationBase: 78000})}},
		{name: "account_singapore_base", argv: []string{"account"}, conn: goldenConn{rpc.MethodAccountSummary: goldenLocalCurrencyAccount("SGD", rpc.CurrencyExposure{Currency: "HKD", NetLiquidationCcy: 194100, ExchangeRate: 0.17, NetLiquidationBase: 32997}, rpc.CurrencyExposure{Currency: "USD", NetLiquidationCcy: 10000, ExchangeRate: 1.3, NetLiquidationBase: 13000})}},
		{name: "brief_singapore_base", argv: []string{"brief"}, conn: goldenConn{rpc.MethodBriefSnapshot: goldenBriefSingapore()}},
		{name: "technical_hong_kong", argv: []string{"technical", "700,5", "--market", "hk"}, conn: goldenConn{rpc.MethodTechnical: goldenTechnicalHongKong()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var doc strings.Builder
			for _, color := range []bool{false, true} {
				mode := "colour off"
				if color {
					mode = `colour on, ESC written as \x1b`
				}
				fmt.Fprintf(&doc, "# canary %s · COLUMNS=80 · %s\n", strings.Join(tc.argv, " "), mode)
				doc.WriteString(goldenText(renderGolden(t, tc, color)))
			}
			compareGolden(t, filepath.Join("testdata", "golden_local_currency", tc.name+".txt"), doc.String())
		})
	}
}

// goldenSingaporeBook is an SGD-base book with Hong Kong, Singapore and US
// shares: each row's money stays in its own currency, none in dollars.
func goldenSingaporeBook() *rpc.PositionsResult {
	hk := rpc.PositionView{Symbol: "700", SecType: rpc.SecTypeStock, PrimaryExch: "SEHK", Currency: "HKD", Quantity: 500, Multiplier: 1,
		AvgCost: 340.40, Mark: 388.20, MarketValue: 194100, UnrealizedPnL: 23900, DayChangeMoney: new(1100.0), DayChangePct: new(0.57), MarketValueBase: new(32997.0)}
	sg := rpc.PositionView{Symbol: "D05", SecType: rpc.SecTypeStock, PrimaryExch: "SGX", Currency: "SGD", Quantity: 1000, Multiplier: 1,
		AvgCost: 41.10, Mark: 43.75, MarketValue: 43750, UnrealizedPnL: 2650, DayChangeMoney: new(-250.0), DayChangePct: new(-0.57), MarketValueBase: new(43750.0)}
	us := rpc.PositionView{Symbol: "SYNU", SecType: rpc.SecTypeStock, PrimaryExch: "NASDAQ", Currency: "USD", Quantity: 100, Multiplier: 1,
		AvgCost: 90, Mark: 100, MarketValue: 10000, UnrealizedPnL: 1000, DayChangeMoney: new(50.0), DayChangePct: new(0.5), MarketValueBase: new(13000.0)}
	return &rpc.PositionsResult{AsOf: goldenAt, DataType: "live",
		Stocks: []rpc.PositionView{hk, sg, us},
		ByUnderlying: []rpc.PositionGroup{
			{Underlying: "D05", Stock: &sg, GroupMarketValue: 43750, GroupUnrealizedPnL: 2650},
			{Underlying: "700", Stock: &hk, GroupMarketValue: 194100, GroupUnrealizedPnL: 23900},
			{Underlying: "SYNU", Stock: &us, GroupMarketValue: 10000, GroupUnrealizedPnL: 1000},
		}}
}

// goldenLocalCurrencyAccount is the account view of a book whose base is a
// local Asian currency, with its holdings in other currencies converted at the
// broker's ledger rates. The figures are invented.
func goldenLocalCurrencyAccount(base string, exposure ...rpc.CurrencyExposure) rpc.AccountResult {
	a := goldenAccount()
	a.BaseCurrency = base
	a.CurrencyExposure = exposure
	a.BaseCurrencyLedger = &rpc.CurrencyExposure{Currency: base}
	return a
}

// goldenBriefSingapore is the brief of an SGD-base book at a Singapore morning.
func goldenBriefSingapore() rpc.BriefResult {
	res := goldenBrief(false)
	review, ready := &res.Review, &res.Ready
	review.SessionPnL.EquityBase, review.SessionPnL.DailyPnLBase, review.SessionPnL.BaseCurrency = new(182500.0), new(-1120.5), "SGD"
	review.LastSession.DailyPnLBase, review.LastSession.BaseCurrency = new(410.25), "SGD"
	review.Attribution.Rows = []rpc.BriefMover{{Symbol: "D05", DailyPnLBase: -250}, {Symbol: "700", DailyPnLBase: 187}}
	ready.Session.Market, ready.Session.State = "sg_sgx", "regular"
	ready.PremiumAtRisk.AmountBase, ready.PremiumAtRisk.BaseCurrency = new(1500.0), "SGD"
	ready.HedgeCost.AmountBase, ready.HedgeCost.BaseCurrency = new(150.0), "SGD"
	return res
}

// goldenTechnicalHongKong is a Hong Kong screen: every amount in the listing's
// own currency, the benchmark's return read against it.
func goldenTechnicalHongKong() rpc.TechnicalResult {
	return rpc.TechnicalResult{Benchmark: "SPY", LookbackDays: 420, Market: "hk", Exchange: "SEHK", Currency: "HKD", AsOf: goldenAt, Rows: []rpc.TechnicalRow{
		{Symbol: "700", Currency: "HKD", Price: new(388.2), PriceAsOf: "2026-11-06", Bars: 280, SMA50: new(370.5), SMA200: new(340.25), PctAbove200DMA: new(0.14), RS63D: new(0.05), RS126D: new(0.1), ATRPct: new(0.02), AvgVolume20D: new(int64(12_000_000)), AvgDollarVolume20D: new(4_500_000_000.0), TrendState: "uptrend", DataQuality: "ok"},
		{Symbol: "5", Currency: "HKD", Error: "No security definition has been found for the request", DataQuality: "error", MissingReasons: []string{"history_unavailable"}},
	}}
}
