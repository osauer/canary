// Command _preview renders the user-facing CLI screens with realistic
// synthetic fixture data — used for social-preview screenshots, README
// updates, and visual review without exposing a live account.
//
// The leading underscore in the directory name makes `go build ./...`
// skip this tool, so it never lands in release tarballs. Invoke by file
// path:
//
//	go run cmd/_preview/main.go              # every screen, top-to-bottom
//	go run cmd/_preview/main.go account      # one screen
//	go run cmd/_preview/main.go positions    # positions --by underlying
//	go run cmd/_preview/main.go positions-flat
//	go run cmd/_preview/main.go chain        # chain SYM (expiry list)
//	go run cmd/_preview/main.go chain-strikes
//	go run cmd/_preview/main.go quote
//	go run cmd/_preview/main.go history
//	go run cmd/_preview/main.go scan
//	go run cmd/_preview/main.go size
//	go run cmd/_preview/main.go status
//	go run cmd/_preview/main.go canary
//
// Color is forced on so a tee'd capture (`… | tee /tmp/preview.txt`) keeps
// the ANSI escapes that screenshot tools like freezer.dev / ray.so /
// carbon.now.sh need to render the right bold / dim / sign-color layout.
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/osauer/ibkr/v2/internal/cli"
	"github.com/osauer/ibkr/v2/internal/risk"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

func main() {
	which := "all"
	if len(os.Args) > 1 {
		which = os.Args[1]
	}
	env := &cli.Env{Stdout: os.Stdout, Stderr: os.Stderr, Color: true}
	screens := map[string]func(){
		"account":        func() { cli.PreviewRenderAccount(env, fixtureAccount()) },
		"positions":      func() { cli.PreviewRenderPositionsByUnderlying(env, fixturePositions()) },
		"positions-flat": func() { cli.PreviewRenderPositions(env, fixturePositions()) },
		"chain":          func() { cli.PreviewRenderChainExpiries(env, fixtureChain(), true) },
		"chain-strikes":  func() { cli.PreviewRenderChainStrikes(env, fixtureChainStrikes()) },
		"quote":          func() { cli.PreviewRenderQuoteSnapshot(env, fixtureQuotes()) },
		"history":        func() { cli.PreviewRenderHistory(env, fixtureHistory()) },
		"scan":           func() { cli.PreviewRenderScan(env, fixtureScan()) },
		"size":           func() { cli.PreviewRenderSize(env, fixtureSize()) },
		"status":         func() { cli.PreviewRenderStatus(env, fixtureStatus()) },
		"regime":         func() { cli.PreviewRenderRegime(env, fixtureRegime()) },
		"canary":         func() { cli.PreviewRenderStress(env, fixtureCanary()) },
	}
	order := []string{"status", "account", "positions", "positions-flat", "chain", "chain-strikes", "quote", "history", "scan", "size", "regime", "canary"}

	if which == "all" {
		for i, key := range order {
			if i > 0 {
				fmt.Fprintln(os.Stdout)
			}
			screens[key]()
		}
		return
	}
	fn, ok := screens[which]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown preview: %q\n", which)
		fmt.Fprintln(os.Stderr, "screens: account | positions | positions-flat | chain | chain-strikes | quote | history | scan | size | status | regime | canary | all")
		os.Exit(2)
	}
	fn()
}

func fixtureAccount() *rpc.AccountResult {
	return &rpc.AccountResult{
		AccountID:            "DU0000000",
		AccountType:          "IB-MARGIN",
		BaseCurrency:         "EUR",
		NetLiquidation:       248310.42,
		BuyingPower:          992841.68,
		AvailableFunds:       124055.21,
		ExcessLiquidity:      124055.21,
		TotalCash:            119084.21,
		MaintenanceMargin:    24318.10,
		InitialMargin:        29182.32,
		GrossPositionValue:   188420.10,
		UnrealizedPnL:        2418.07,
		RealizedPnL:          -312.50,
		Cushion:              0.50,
		LookAheadInitMargin:  29182.32,
		LookAheadMaintMargin: 24318.10,
		LookAheadAvailable:   124055.21,
		LookAheadExcess:      124055.21,
		DailyPnL:             f64(1247.30),
		PnLUnrealizedTotal:   f64(-44485.00),
		PnLRealizedTotal:     f64(1830.00),
		CurrencyExposure: []rpc.CurrencyExposure{
			{Currency: "USD", NetLiquidationCcy: 92418.07, ExchangeRate: 1.0823, NetLiquidationBase: 85398.92},
			{Currency: "GBP", NetLiquidationCcy: 12061.40, ExchangeRate: 1.1718, NetLiquidationBase: 14034.83},
		},
		// AccountResult.DataType is omitempty-emit-empty per the v0.15.0
		// wire-honesty pass; the field exists on the struct for renderer-
		// fallback compatibility but the daemon never sets it. Mirror that
		// here so the screenshots match the real binary's output.
		AsOf: time.Date(2026, 5, 13, 14, 32, 18, 0, time.UTC),
	}
}

func fixturePositions() *rpc.PositionsResult {
	ny := time.FixedZone("EDT", -4*60*60)
	// AAPL: long stock plus a covered call and a protective put.
	aaplStock := rpc.PositionView{
		Symbol: "AAPL", SecType: rpc.SecTypeStock, Currency: "USD", Multiplier: 1,
		Quantity: 100, AvgCost: 192.10, Mark: 207.42,
		DataType: rpc.MarketDataLive, PriceSource: "last",
		PrevClose: f64(206.10), DayChange: f64(1.32), DayChangePct: f64(0.64), DayChangeMoney: f64(132.00),
		DayLow: f64(205.85), DayHigh: f64(209.80), Week52Low: f64(164.08), Week52High: f64(237.49),
		Volume: ptrInt64(41_762_007), AvgVolume: ptrInt64(58_900_000),
		PriceAt: time.Date(2026, 5, 13, 10, 32, 18, 0, ny),
		SessionContext: &rpc.MarketSession{
			Timezone: "America/New_York",
			State:    "regular",
			IsOpen:   true,
		},
		MarketValue: 20742.00, UnrealizedPnL: 1532.00, DailyPnL: f64(132.00),
	}
	// AvgCost is per-contract on OPT (multiplier-inclusive) — mirrors what
	// IBKR sends over the wire. The CLI normalises to per-share on display.
	aaplCall := rpc.PositionView{
		Symbol: "AAPL", SecType: rpc.SecTypeOption, Multiplier: 100,
		Right: "C", Strike: 210, Expiry: "20251219",
		Quantity: 2, AvgCost: 510.00, Mark: 7.85,
		MarketValue: 1570.00, UnrealizedPnL: 550.00,
		Delta: f64(0.42), Gamma: f64(0.018), Theta: f64(-0.08), Vega: f64(0.42),
	}
	aaplPut := rpc.PositionView{
		Symbol: "AAPL", SecType: rpc.SecTypeOption, Multiplier: 100,
		Right: "P", Strike: 195, Expiry: "20251219",
		Quantity: -1, AvgCost: 385.00, Mark: 3.21,
		MarketValue: -321.00, UnrealizedPnL: 64.00,
		Delta: f64(-0.18), Gamma: f64(0.024), Theta: f64(-0.06), Vega: f64(0.31),
	}

	// NVDA: long stock plus a long upside call.
	nvdaStock := rpc.PositionView{
		Symbol: "NVDA", SecType: rpc.SecTypeStock, Currency: "USD", Multiplier: 1,
		Quantity: 250, AvgCost: 119.05, Mark: 128.54,
		DataType: rpc.MarketDataLive, PriceSource: "last",
		PrevClose: f64(129.52), DayChange: f64(-0.98), DayChangePct: f64(-0.77), DayChangeMoney: f64(-245.00),
		DayLow: f64(126.70), DayHigh: f64(130.12), Week52Low: f64(86.62), Week52High: f64(153.13),
		Volume: ptrInt64(248_000_000), AvgVolume: ptrInt64(212_400_000),
		PriceAt: time.Date(2026, 5, 13, 10, 32, 18, 0, ny),
		SessionContext: &rpc.MarketSession{
			Timezone: "America/New_York",
			State:    "regular",
			IsOpen:   true,
		},
		MarketValue: 32135.00, UnrealizedPnL: 2372.50, DailyPnL: f64(-245.00),
	}
	nvdaCall := rpc.PositionView{
		Symbol: "NVDA", SecType: rpc.SecTypeOption, Multiplier: 100,
		Right: "C", Strike: 135, Expiry: "20250621",
		Quantity: 5, AvgCost: 620.00, Mark: 4.80,
		MarketValue: 2400.00, UnrealizedPnL: -700.00,
		Delta: f64(0.31), Gamma: f64(0.029), Theta: f64(-0.12), Vega: f64(0.55),
	}

	// SPY: pure-options hedge — a long downside put pair.
	spyPut1 := rpc.PositionView{
		Symbol: "SPY", SecType: rpc.SecTypeOption, Multiplier: 100,
		Right: "P", Strike: 560, Expiry: "20260619",
		Quantity: 3, AvgCost: 840.00, Mark: 6.75,
		MarketValue: 2025.00, UnrealizedPnL: -495.00,
		Delta: f64(-0.22), Gamma: f64(0.008), Theta: f64(-0.11), Vega: f64(0.68),
	}
	spyPut2 := rpc.PositionView{
		Symbol: "SPY", SecType: rpc.SecTypeOption, Multiplier: 100,
		Right: "P", Strike: 540, Expiry: "20260619",
		Quantity: 2, AvgCost: 410.00, Mark: 3.05,
		MarketValue: 610.00, UnrealizedPnL: -210.00,
		Delta: f64(-0.14), Gamma: f64(0.005), Theta: f64(-0.09), Vega: f64(0.61),
	}

	return &rpc.PositionsResult{
		Stocks:  []rpc.PositionView{aaplStock, nvdaStock},
		Options: []rpc.PositionView{aaplCall, aaplPut, nvdaCall, spyPut1, spyPut2},
		ByUnderlying: []rpc.PositionGroup{
			{
				Underlying:         "AAPL",
				Stock:              &aaplStock,
				Options:            []rpc.PositionView{aaplCall, aaplPut},
				GroupMarketValue:   21991.00,
				GroupUnrealizedPnL: 2146.00,
			},
			{
				Underlying:         "NVDA",
				Stock:              &nvdaStock,
				Options:            []rpc.PositionView{nvdaCall},
				GroupMarketValue:   34535.00,
				GroupUnrealizedPnL: 1672.50,
			},
			{
				Underlying:         "SPY",
				Options:            []rpc.PositionView{spyPut1, spyPut2},
				GroupMarketValue:   2635.00,
				GroupUnrealizedPnL: -705.00,
			},
		},
		Portfolio: &rpc.PositionsPortfolio{
			EffectiveDelta:      f64(1847.0),
			DollarDelta:         f64(326584.50),
			DollarDeltaCurrency: "USD",
			DailyTheta:          f64(-42.18),
			DailyThetaCurrency:  "USD",
			Gamma:               f64(12.4),
			Vega:                f64(1205.00),
			GreeksCoverage:      5,
			GreeksTotal:         5,
			FXSensitivityPerPct: f64(-854.32),
			FXBaseCurrency:      "EUR",
		},
		AsOf: time.Date(2026, 5, 13, 14, 32, 18, 0, time.UTC),
		// PositionsResult.DataType is omitempty-emit-empty per v0.15.0.
	}
}

// fixtureChainStrikes is a 5-strike SPY June chain centered on the spot at
// 583.18. The ATM row carries only the strike (no quotes) to exercise the
// em-dash placeholder path — illiquid wings on real symbols look the same.
func fixtureChainStrikes() *rpc.ChainResult {
	mk := func(b, a, l, iv float64) (*float64, *float64, *float64, *float64) {
		return f64(b), f64(a), f64(l), f64(iv)
	}
	cb1, ca1, cl1, civ1 := mk(20.10, 20.35, 20.20, 0.182)
	pb1, pa1, pl1, piv1 := mk(0.42, 0.51, 0.47, 0.241)
	cb2, ca2, cl2, civ2 := mk(11.20, 11.45, 11.30, 0.171)
	pb2, pa2, pl2, piv2 := mk(1.85, 1.95, 1.90, 0.205)
	cb4, ca4, cl4, civ4 := mk(0.95, 1.05, 1.00, 0.215)
	pb4, pa4, pl4, piv4 := mk(11.05, 11.30, 11.15, 0.182)
	cb5, ca5, cl5, civ5 := mk(0.18, 0.24, 0.20, 0.238)
	pb5, pa5, pl5, piv5 := mk(20.10, 20.40, 20.25, 0.198)
	return &rpc.ChainResult{
		Symbol:   "SPY",
		Spot:     583.18,
		Expiry:   "2026-06-20",
		DTE:      38,
		DataType: "live",
		Strikes: []rpc.ChainStrike{
			{Strike: 565, CallBid: cb1, CallAsk: ca1, CallLast: cl1, CallIV: civ1, PutBid: pb1, PutAsk: pa1, PutLast: pl1, PutIV: piv1},
			{Strike: 575, CallBid: cb2, CallAsk: ca2, CallLast: cl2, CallIV: civ2, PutBid: pb2, PutAsk: pa2, PutLast: pl2, PutIV: piv2},
			{Strike: 585, IsATM: true},
			{Strike: 595, CallBid: cb4, CallAsk: ca4, CallLast: cl4, CallIV: civ4, PutBid: pb4, PutAsk: pa4, PutLast: pl4, PutIV: piv4},
			{Strike: 605, CallBid: cb5, CallAsk: ca5, CallLast: cl5, CallIV: civ5, PutBid: pb5, PutAsk: pa5, PutLast: pl5, PutIV: piv5},
		},
		AsOf: time.Date(2026, 5, 13, 14, 32, 18, 0, time.UTC),
	}
}

// fixtureQuotes is a 3-symbol snapshot mixing a live quote (full data),
// a partial quote (Last only, no previous close — pre-market case), and
// a delayed quote (entitlement-limited).
func fixtureQuotes() []rpc.Quote {
	// change_pct is in percent (0.70 = 0.70%), matching the position view's
	// day-change-pct convention and how the daemon serializes the tick.
	bid1, ask1, last1 := 207.82, 207.88, 207.85
	prev1, chg1, pct1 := 206.40, 1.45, 0.70
	bidSz1, askSz1 := 240, 510
	vol1 := int64(38_400_000)
	iv1 := 0.247

	last2 := 128.54

	bid3, ask3, last3 := 461.04, 461.20, 461.12
	prev3, chg3, pct3 := 463.50, -2.38, -0.51
	vol3 := int64(8_120_000)

	return []rpc.Quote{
		{
			Symbol: "AAPL", Bid: &bid1, Ask: &ask1, Last: &last1,
			PrevClose: &prev1, Change: &chg1, ChangePct: &pct1,
			BidSize: &bidSz1, AskSize: &askSz1, Volume: &vol1,
			IV: &iv1, DataType: "live",
		},
		{Symbol: "NVDA", Last: &last2, DataType: "live"},
		{
			Symbol: "SPY", Bid: &bid3, Ask: &ask3, Last: &last3,
			PrevClose: &prev3, Change: &chg3, ChangePct: &pct3,
			Volume:   &vol3,
			DataType: "delayed",
		},
	}
}

func fixtureHistory() *rpc.HistoryDailyResult {
	// HistoryDailyResult.DataType is omitempty-emit-empty per v0.15.0.
	return &rpc.HistoryDailyResult{
		Symbol: "AAPL",
		Days:   5,
		Bars: []rpc.HistoryBar{
			{Date: "2026-05-06", Open: 204.30, High: 207.10, Low: 203.85, Close: 206.40, Volume: 41_200_000},
			{Date: "2026-05-07", Open: 206.50, High: 208.95, Low: 205.70, Close: 207.85, Volume: 36_500_000},
			{Date: "2026-05-08", Open: 207.90, High: 210.40, Low: 207.10, Close: 209.55, Volume: 44_800_000},
			{Date: "2026-05-09", Open: 209.20, High: 210.05, Low: 206.30, Close: 207.42, Volume: 39_100_000},
			{Date: "2026-05-12", Open: 207.55, High: 209.80, Low: 206.95, Close: 208.91, Volume: 31_700_000},
		},
		AsOf: time.Date(2026, 5, 13, 14, 32, 18, 0, time.UTC),
	}
}

// fixtureScan is a 6-row mixed-exchange screen — USD-quoted US rows,
// one EUR row, one HKD row, plus a partial row (no IV, no 52w range,
// no Currency) so the per-row currency, em-dash, and fallback paths render.
func fixtureScan() *rpc.ScanResult {
	mkRow := func(rank int, sym, ccy string, last, pct, iv, lo, hi float64, vol int64) rpc.ScanRow {
		return rpc.ScanRow{
			Rank: rank, Symbol: sym, Currency: ccy,
			Last: f64(last), ChangePct: f64(pct), IV: f64(iv),
			Week52Low: f64(lo), Week52High: f64(hi),
			Volume: &vol,
		}
	}
	// CHG% is in percent units (7.21 = 7.21%) — formatChangePct prints
	// the value with a trailing % without multiplying.
	partial := rpc.ScanRow{Rank: 6, Symbol: "ZZZ", Last: f64(12.34), ChangePct: f64(4.10)}
	return &rpc.ScanResult{
		Preset: "top_pct_gain",
		Type:   "TOP_PERC_GAIN",
		Rows: []rpc.ScanRow{
			mkRow(1, "AAPL", "USD", 207.85, 7.21, 0.247, 142.10, 219.50, 38_400_000),
			mkRow(2, "NVDA", "USD", 128.54, 5.12, 0.382, 75.20, 145.80, 248_000_000),
			mkRow(3, "AMD", "USD", 145.22, 4.28, 0.318, 89.40, 178.90, 62_500_000),
			mkRow(4, "SAP", "EUR", 174.50, 2.85, 0.221, 130.20, 188.40, 4_100_000),
			mkRow(5, "0700", "HKD", 412.20, 3.42, 0.298, 285.00, 458.80, 18_400_000),
			partial,
		},
		AsOf: time.Date(2026, 5, 13, 14, 32, 18, 0, time.UTC),
	}
}

// fixtureSize mirrors a typical EUR account sizing a long AAPL trade
// with a 2R target — covers the full screen including the reward block.
func fixtureSize() *risk.SizeResult {
	tgt, r, reward, be := 215.50, 2.0, 4000.0, 1.0/3.0
	return &risk.SizeResult{
		Symbol: "AAPL", Side: "long",
		Entry: 207.50, Stop: 202.50, Target: &tgt,
		RiskPct: 1.0, Lot: 1, FX: 1.0,
		NLV: 248310.42, BaseCurrency: "EUR",
		RiskBase: 2483.10, RiskQuote: 2483.10,
		PerShareRisk: 5.00,
		Shares:       496, Notional: 102920.00, MaxLoss: 2480.00,
		R: &r, RewardQuote: &reward, BreakevenWinRate: &be,
		Status: "ok",
	}
}

// fixtureStatus is a healthy connected daemon — the happy-path screen
// the user sees after `ibkr status` on a working setup.
func fixtureStatus() *rpc.HealthResult {
	return &rpc.HealthResult{
		DaemonVersion: "v1.0.0",
		UptimeSeconds: 1842,
		Account:       "DU0000000",
		GatewayHost:   "127.0.0.1",
		GatewayPort:   4001,
		GatewayTLS:    false,
		NegotiatedTLS: false,
		PortOrigin:    "discovered",
		ClientID:      15,
		Connected:     true,
		ServerVersion: 178,
		Members: rpc.MembersHealth{
			Source:       "cache",
			AsOf:         time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC),
			Count:        503,
			RefreshState: "healthy",
		},
		// HealthResult.DataType is omitempty-emit-empty post-v0.16.0
		// (the daemon stopped hardcoding "live" — status has no per-
		// reqID feed type to honestly report). Renderer falls back to
		// MarketDataLive when empty.
	}
}

func fixtureCanary() *rpc.StressResult {
	acct := *fixtureAccount()
	regime := *fixtureRegime()

	acct.AsOf = regime.AsOf
	acct.BuyingPower = acct.NetLiquidation * 4
	acct.AvailableFunds = acct.NetLiquidation
	acct.ExcessLiquidity = acct.NetLiquidation
	acct.TotalCash = acct.NetLiquidation
	acct.MaintenanceMargin = 0
	acct.InitialMargin = 0
	acct.GrossPositionValue = 0
	acct.UnrealizedPnL = 0
	acct.RealizedPnL = 0
	acct.Cushion = 1
	acct.LookAheadInitMargin = 0
	acct.LookAheadMaintMargin = 0
	acct.LookAheadAvailable = acct.NetLiquidation
	acct.LookAheadExcess = acct.NetLiquidation
	acct.DailyPnL = f64(0)
	acct.PnLUnrealizedTotal = f64(0)
	acct.PnLRealizedTotal = f64(0)

	pos := &rpc.PositionsResult{
		AsOf:      regime.AsOf,
		AccountID: acct.AccountID,
		Portfolio: &rpc.PositionsPortfolio{BaseCurrency: acct.BaseCurrency, NetLiquidationBase: f64(acct.NetLiquidation)},
		DataType:  rpc.MarketDataLive,
		Stocks:    []rpc.PositionView{},
		Options:   []rpc.PositionView{},
	}

	res := cli.ComputeStress(cli.StressInput{
		Account:   acct,
		Positions: *pos,
		Regime:    regime,
		Now:       regime.AsOf,
	})
	return &res
}

func fixtureChain() *rpc.ChainExpiriesResult {
	// All ImpliedMove values satisfy spot × IV × √(DTE/365), so the
	// formula caption isn't decorative — the screen is self-consistent.
	expiry := func(date string, dte int, iv, move, pct float64) rpc.ChainExpiry {
		return rpc.ChainExpiry{
			Date: date, DTE: dte,
			IV: f64(iv), IVStatus: "ok",
			ImpliedMove: f64(move), ImpliedMovePct: f64(pct),
		}
	}
	return &rpc.ChainExpiriesResult{
		Symbol: "SPY",
		Spot:   583.18,
		Expiries: []rpc.ChainExpiry{
			expiry("2026-05-16", 3, 0.142, 7.51, 0.0129),
			expiry("2026-05-23", 10, 0.168, 16.21, 0.0278),
			expiry("2026-05-30", 17, 0.181, 22.78, 0.0391),
			expiry("2026-06-20", 38, 0.194, 36.51, 0.0626),
			expiry("2026-07-18", 66, 0.207, 51.36, 0.0881),
			expiry("2026-08-15", 94, 0.214, 63.31, 0.1086),
			expiry("2026-09-19", 129, 0.218, 75.60, 0.1297),
			expiry("2026-12-19", 220, 0.221, 100.04, 0.1716),
		},
		AsOf: time.Date(2026, 5, 13, 14, 32, 18, 0, time.UTC),
	}
}

func f64(v float64) *float64 { return &v }

func ptrInt64(v int64) *int64 { return &v }

func previewQuality(at time.Time, freshness, confidence, source string) *rpc.Quality {
	return &rpc.Quality{AsOf: at, FreshnessClass: freshness, Confidence: confidence, Source: source}
}

func previewAsOf(label string, at time.Time, freshness, source string) *rpc.RegimeAsOfSummary {
	return &rpc.RegimeAsOfSummary{Label: label, Time: at, Freshness: freshness, Source: source}
}

func previewDateAsOf(label, date, freshness, source string) *rpc.RegimeAsOfSummary {
	return &rpc.RegimeAsOfSummary{Label: label, Date: date, Freshness: freshness, Source: source}
}

// fixtureRegime mirrors the audited pre-market read where market weather is
// mixed: dealer gamma is the only red cluster, breadth is on watch, VIX/VVIX,
// cash credit, and funding are calm, and HYG/FX are set aside because required
// history fields are missing.
func fixtureRegime() *rpc.RegimeSnapshotResult {
	cest := time.FixedZone("CEST", 2*60*60)
	now := time.Date(2026, 6, 4, 11, 24, 0, 0, cest)
	officialDate := "2026-06-03"
	r := &rpc.RegimeSnapshotResult{
		AsOf:    now,
		SpecDoc: "https://osauer.dev/ibkr/docs/internals/regime-dashboard.html",
		Summary: rpc.RegimeSummary{
			Label:             "Stress signal present",
			Evidence:          "3 green clusters / 1 yellow cluster / 1 red cluster / 1 waiting",
			IndicatorEvidence: "4 green / 1 yellow / 1 red / 2 unranked",
			PunchLine:         "Stress is visible, but not broad enough to dominate the read.",
			Confidence:        "medium",
			DominantRisks:     []string{"dealer gamma", "breadth"},
			NotAdvice:         "Regime read only; no orders are placed by ibkr.",
		},
		VIXTermStructure: rpc.RegimeVIXTerm{
			RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{
				Band:       "green",
				BandReason: "vol curve in contango",
				AsOf:       previewAsOf("live", now, rpc.FreshnessLive, "IBKR index ticks"),
			},
			Status:       rpc.RegimeStatusOK,
			VIX:          f64(16.48),
			VIX3M:        f64(19.76),
			Ratio:        f64(16.48 / 19.76),
			VIXPrevClose: f64(16.06),
			VIXChangePct: f64((16.48 - 16.06) / 16.06 * 100),
			DataType:     rpc.MarketDataLive,
			VIXQuality:   previewQuality(now, rpc.FreshnessLive, rpc.ConfidenceFirm, "VIX tick"),
			VIX3MQuality: previewQuality(now, rpc.FreshnessLive, rpc.ConfidenceFirm, "VIX3M tick"),
			Notes:        "VIX (30-day implied vol) divided by VIX3M (3-month implied vol). Spec thresholds: <0.92 green (healthy contango), 0.92-1.00 yellow (flattening), >1.00 red (backwardation — acute stress pricing).",
		},
		VolOfVol: rpc.RegimeVolOfVol{
			RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{
				Band:       "green",
				BandReason: "vol-of-vol calm",
				AsOf:       previewDateAsOf("close D-1", officialDate, rpc.FreshnessDerived, "Cboe VVIX daily file"),
			},
			Status:       rpc.RegimeStatusOK,
			Symbol:       "VVIX",
			Last:         f64(89.8),
			Change20D:    f64(-5.7),
			AsOfDate:     officialDate,
			Source:       "Cboe official daily VVIX",
			ValueQuality: previewQuality(now.Add(-16*time.Hour), rpc.FreshnessDerived, rpc.ConfidenceFirm, "Cboe VVIX daily file"),
			Notes:        "VVIX vol-of-vol. Spec thresholds: <90 green, 90-110 yellow, >110 red.",
		},
		HYGSPYDivergence: rpc.RegimeHYGSPYDivergence{
			RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{
				Band:       "unranked",
				BandReason: "need HYG 50-day average",
				AsOf:       previewAsOf("live", now, rpc.FreshnessLive, "IBKR ETF ticks"),
			},
			Status:        rpc.RegimeStatusOK,
			HYGPrice:      f64(79.70),
			SPYPrice:      f64(751.20),
			SPY52WHigh:    f64(760.39),
			SPYPrevClose:  f64(754.24),
			SPYChange:     f64(751.20 - 754.24),
			SPYChangePct:  f64((751.20 - 754.24) / 754.24 * 100),
			HYGDataType:   rpc.MarketDataLive,
			FieldsMissing: []string{"hyg_50dma"},
			HYGQuality:    previewQuality(now, rpc.FreshnessLive, rpc.ConfidenceFirm, "HYG tick"),
			SPYQuality:    previewQuality(now, rpc.FreshnessLive, rpc.ConfidenceFirm, "SPY tick"),
			Notes:         "HYG (high-yield corporate bond ETF) vs SPY context. Spec thresholds: green when both trending up and HYG above 50-day SMA; yellow when HYG breaks 50-day SMA while SPY within 3% of 52-week high.",
		},
		CreditSpreads: rpc.RegimeCreditSpreads{
			RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{
				Band:       "green",
				BandReason: "cash spreads calm",
				AsOf:       previewDateAsOf("close D-1", officialDate, rpc.FreshnessDerived, "FRED ICE BofA OAS"),
			},
			Status:        rpc.RegimeStatusOK,
			HYOAS:         f64(2.71),
			IGOAS:         f64(0.74),
			HYIGSpread:    f64(1.97),
			HY20DChange:   f64(-0.04),
			AsOfDate:      officialDate,
			Source:        "FRED ICE BofA OAS",
			HYOASQuality:  previewQuality(now.Add(-16*time.Hour), rpc.FreshnessDerived, rpc.ConfidenceFirm, "FRED BAMLH0A0HYM2"),
			IGOASQuality:  previewQuality(now.Add(-16*time.Hour), rpc.FreshnessDerived, rpc.ConfidenceFirm, "FRED BAMLC0A0CM"),
			SpreadQuality: previewQuality(now.Add(-16*time.Hour), rpc.FreshnessDerived, rpc.ConfidenceFirm, "HY minus IG OAS"),
			Notes:         "Cash credit spreads from official ICE BofA OAS series. Spec thresholds: HY OAS <4 green, 4-5.5 yellow, >5.5 red, with widening overlays.",
		},
		FundingStress: rpc.RegimeFundingStress{
			RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{
				Band:       "green",
				BandReason: "funding calm",
				AsOf:       previewDateAsOf("close D-1", officialDate, rpc.FreshnessDerived, "FRED CP and T-bill rates"),
			},
			Status:         rpc.RegimeStatusOK,
			CP3M:           f64(3.70),
			TBill3M:        f64(3.63),
			SpreadBps:      f64(7),
			AsOfDate:       officialDate,
			Source:         "FRED CP3M and DTB3",
			CP3MQuality:    previewQuality(now.Add(-16*time.Hour), rpc.FreshnessDerived, rpc.ConfidenceFirm, "FRED CP3M"),
			TBill3MQuality: previewQuality(now.Add(-16*time.Hour), rpc.FreshnessDerived, rpc.ConfidenceFirm, "FRED DTB3"),
			SpreadQuality:  previewQuality(now.Add(-16*time.Hour), rpc.FreshnessDerived, rpc.ConfidenceFirm, "CP minus T-bill spread"),
			Notes:          "3-month AA financial commercial paper minus 3-month Treasury bill. Spec thresholds: <25bp green, 25-75bp yellow, >75bp red.",
		},
		USDJPY: rpc.RegimeUSDJPY{
			RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{
				Band:       "unranked",
				BandReason: "need weekly move",
				AsOf:       previewAsOf("live", now, rpc.FreshnessLive, "IBKR FX tick"),
			},
			Status:        rpc.RegimeStatusOK,
			Symbol:        "USD.JPY",
			Last:          f64(159.8700),
			DataType:      rpc.MarketDataLive,
			FieldsMissing: []string{"close_7d_ago", "weekly_change_pct"},
			LastQuality:   previewQuality(now, rpc.FreshnessLive, rpc.ConfidenceFirm, "USD.JPY midpoint"),
			Notes:         "USD/JPY exchange rate. Spec thresholds: stable or <1% weekly move (green); 1-2% weekly yen strength (yellow); >2% in 3 days or >3% in a week (red).",
		},
		GammaZero: rpc.RegimeGammaZero{
			RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{
				Band:       "red",
				BandReason: "dealer short-γ · amplifying",
				AsOf:       previewAsOf("cached 11:23", now.Add(-time.Minute), rpc.FreshnessModelled, "gamma.zero_spx"),
			},
			Status: rpc.RegimeStatusOK,
			Envelope: rpc.GammaZeroSPXResult{
				Status: rpc.GammaZeroStatusReady,
				Result: &rpc.GammaZeroComputed{
					SpotUnderlying:          7553.68,
					SpotAt:                  now.Add(-time.Minute),
					GammaSign:               "negative",
					GammaTotalAbs:           29_683_981_889,
					GammaTotalAbsConvention: "sign-agnostic",
					Method:                  "perfiliev-bs-sweep-v1",
					AsOf:                    now.Add(-time.Minute),
					DurationMS:              18_430,
					Scope:                   rpc.GammaZeroScopeSPX,
					Quality: &rpc.GammaSignalQuality{
						Rankability: rpc.GammaRankabilityRankable,
						Freshness:   "fresh",
						Session:     "premarket",
						AsOf:        now.Add(-time.Minute),
						Coverage: rpc.GammaQualityCoverage{
							PricedLegs:      1840,
							OIObservedLegs:  1729,
							OIPositiveLegs:  1698,
							GEXLegs:         1698,
							OIObservedPct:   94.0,
							OIPositivePct:   92.3,
							ExpirationCount: 8,
							Has0DTE:         true,
							Has1To7DTE:      true,
							HasTerm:         true,
						},
					},
					Summary: &rpc.GammaZeroSummary{
						PrimaryStatement: "SPX dealer gamma is short across the swept window.",
						ZeroGammaStatus:  "none_in_window",
						Regime:           "short_gamma",
						Confidence:       "medium",
					},
				},
			},
			ZeroGammaQuality:     previewQuality(now.Add(-time.Minute), rpc.FreshnessModelled, rpc.ConfidenceProxy, "perfiliev-bs-sweep-v1"),
			GammaTotalAbsQuality: previewQuality(now.Add(-time.Minute), rpc.FreshnessDerived, rpc.ConfidenceEstimate, "observed OI and IV aggregation"),
			Notes:                "SPX dealer zero-gamma flip level. Spec thresholds: spot >2% above zero_gamma (green); within 2% (yellow); below (red), or whole-sweep short-gamma as red. Methodology: Perfiliev BS-sweep; see spec for limitations and the calibration ritual.",
		},
		Breadth: rpc.RegimeBreadth{
			RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{
				Band:       "yellow",
				BandReason: "participation narrowing",
				AsOf:       previewDateAsOf("close D-1", officialDate, rpc.FreshnessDerived, "local S&P 500 breadth cache"),
			},
			Status: rpc.RegimeStatusOK,
			Envelope: rpc.BreadthSPXResult{
				State:          rpc.BreadthStateReady,
				PctAbove50DMA:  49.5,
				PctAbove200DMA: 53.2,
				NewHighsToday:  26,
				NewLowsToday:   19,
				NetNewHighsPct: 1.4,
				Source:         "Computed from S&P-500 constituent daily bars (IBKR HMDS)",
				Method:         "constituent-fanout-50/200dma-hl",
				AsOf:           time.Date(2026, 6, 3, 22, 0, 0, 0, cest),
			},
			PctAbove50DMA:  49.5,
			PctAbove200DMA: 53.2,
			NewHighsToday:  26,
			NewLowsToday:   19,
			NetNewHighsPct: 1.4,
			ValueQuality:   previewQuality(now.Add(-13*time.Hour), rpc.FreshnessDerived, rpc.ConfidenceEstimate, "S&P 500 constituent daily bars"),
			Notes:          "% S&P 500 stocks above their 50-day SMA. Spec thresholds: >55 green (healthy participation); 40-55 yellow; <40 with SPX within 3% of 52-week high is the textbook late-cycle divergence (red). IBKR doesn't redistribute S&P DJI's S5FI index on retail subscriptions, so the daemon computes the same number locally from the 500 constituent daily closes (once-daily refresh post-close).",
		},
	}
	r.Composite = rpc.RegimeComposite{
		Verdict:              "Stress signal present",
		GreenCount:           4,
		YellowCount:          1,
		RedCount:             1,
		RankedCount:          6,
		UnrankedCount:        2,
		ClusterGreenCount:    3,
		ClusterYellowCount:   1,
		ClusterRedCount:      1,
		ClusterRankedCount:   5,
		ClusterUnrankedCount: 1,
	}
	r.WarningDetails = []rpc.RegimeWarning{
		{
			Code:     "missing_required_fields",
			Scope:    "HYG vs SPY",
			Severity: "warning",
			Message:  "HYG tick arrived, but the HYG 50-day average is missing.",
			Impact:   "The ETF credit proxy is shown for context but does not vote in the composite.",
			Action:   "Refresh historical bars or wait for the next cache build.",
		},
		{
			Code:     "missing_required_fields",
			Scope:    "USD/JPY",
			Severity: "warning",
			Message:  "USD/JPY spot arrived, but the 7-day close and weekly change are missing.",
			Impact:   "The FX carry proxy is shown for context but does not vote in the composite.",
			Action:   "Refresh FX historical bars.",
		},
	}
	r.DataQuality = []rpc.DataQualityHealth{{
		Surface:         "regime",
		Status:          "partial",
		Summary:         "partial: credit, fx",
		PartialClusters: []string{"credit", "fx"},
		AsOf:            r.AsOf,
	}}
	r.SourceHealth = rpc.BuildRegimeSourceHealth(r, r.AsOf)
	r.Lifecycle = rpc.BuildRegimeLifecycle(r)
	r.Fingerprint = rpc.BuildRegimeFingerprint(r)
	return r
}
