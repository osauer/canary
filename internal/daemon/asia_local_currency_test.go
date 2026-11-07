package daemon

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/broker"
	"github.com/osauer/canary/v2/internal/rpc"
)

// A synthetic book in a Hong Kong dollar or Singapore dollar base, holding
// shares in HKD, SGD and USD, valued by the daemon's own functions from an
// invented broker ledger. The rates and holdings are made up; no broker runs.
func localCurrencyBook(t *testing.T, base string, rates map[string]float64) (*rpc.AccountResult, *rpc.PositionsResult) {
	t.Helper()
	stocks := []rpc.PositionView{
		{Symbol: "700", SecType: rpc.SecTypeStock, ConID: 900700, Exchange: "SEHK", PrimaryExch: "SEHK", Currency: "HKD", LocalSymbol: "700", Quantity: 500, Multiplier: 1, AvgCost: 340.40, Mark: 388.20, MarketValue: 194100, UnrealizedPnL: 23900, DailyPnL: new(1100.0), DataType: rpc.MarketDataLive},
		{Symbol: "D05", SecType: rpc.SecTypeStock, ConID: 900005, Exchange: "SGX", PrimaryExch: "SGX", Currency: "SGD", LocalSymbol: "D05", Quantity: 1000, Multiplier: 1, AvgCost: 41.10, Mark: 43.75, MarketValue: 43750, UnrealizedPnL: 2650, DailyPnL: new(-250.0), DataType: rpc.MarketDataLive},
		{Symbol: "SYNU", SecType: rpc.SecTypeStock, ConID: 900001, Exchange: "NASDAQ", PrimaryExch: "NASDAQ", Currency: "USD", LocalSymbol: "SYNU", Quantity: 100, Multiplier: 1, AvgCost: 90, Mark: 100, MarketValue: 10000, UnrealizedPnL: 1000, DailyPnL: new(50.0), DataType: rpc.MarketDataLive},
	}
	ledger := map[string]broker.CurrencyLedger{}
	for ccy, rate := range rates {
		ledger[ccy] = broker.CurrencyLedger{ExchangeRate: rate}
	}
	if missing := missingPositionFXCurrencies(stocks, nil, ledger, base); len(missing) != 0 {
		t.Fatalf("%s base has no rate for %v", base, missing)
	}
	fillFXRates(stocks, ledger, base)
	fillBaseValues(stocks, base)

	var groups []rpc.PositionGroup
	var total float64
	for i := range stocks {
		p := &stocks[i]
		if p.MarketValueBase == nil {
			t.Fatalf("%s in a %s base has no base value", p.Symbol, base)
		}
		total += *p.MarketValueBase
		groups = append(groups, rpc.PositionGroup{Underlying: p.Symbol, Stock: p, GroupMarketValue: p.MarketValue, GroupUnrealizedPnL: p.UnrealizedPnL, GroupMarketValueBase: new(*p.MarketValueBase), GroupDailyPnLBase: p.DailyPnLBase, GroupDollarDeltaBase: new(*p.MarketValueBase)})
	}
	now := asiaSimNow
	exposure := []rpc.CurrencyExposure{}
	for _, ccy := range []string{"HKD", "SGD", "USD"} {
		rate := 1.0
		if ccy != base {
			rate = rates[ccy]
		}
		exposure = append(exposure, rpc.CurrencyExposure{Currency: ccy, ExchangeRate: rate, CashObserved: true})
	}
	pos := &rpc.PositionsResult{AsOf: now, DataType: rpc.MarketDataLive, AccountID: asiaSimAccount, Stocks: stocks, ByUnderlying: groups, Authority: asiaSimAuthority(rpc.AccountDataSourcePortfolioStream, now)}
	acct := &rpc.AccountResult{AccountID: asiaSimAccount, AccountType: "INDIVIDUAL", BaseCurrency: base, NetLiquidation: total * 2, BuyingPower: total * 4, AvailableFunds: total, ExcessLiquidity: total, TotalCash: total, GrossPositionValue: total, UnrealizedPnL: 1000, Cushion: 0.5, DailyPnL: new(-120.0), DataType: rpc.MarketDataLive, AsOf: now, CurrencyExposure: exposure, Authority: asiaSimAuthority(rpc.AccountDataSourceAccountSummaryRequest, now)}
	return acct, pos
}

func TestLocalCurrencyBooksValueEveryLineInTheirOwnBase(t *testing.T) {
	for _, tc := range []struct {
		base     string
		rates    map[string]float64
		wantBase map[string]float64
	}{
		{
			base:     "HKD",
			rates:    map[string]float64{"SGD": 5.8, "USD": 7.8},
			wantBase: map[string]float64{"700": 194100, "D05": 43750 * 5.8, "SYNU": 78000},
		},
		{
			base:     "SGD",
			rates:    map[string]float64{"HKD": 0.17, "USD": 1.3},
			wantBase: map[string]float64{"700": 194100 * 0.17, "D05": 43750, "SYNU": 13000},
		},
	} {
		t.Run(tc.base, func(t *testing.T) {
			acct, pos := localCurrencyBook(t, tc.base, tc.rates)
			for _, p := range pos.Stocks {
				want := tc.wantBase[p.Symbol]
				if math.Abs(*p.MarketValueBase-want) > 1e-6 {
					t.Fatalf("%s market value in base = %v, want %v", p.Symbol, *p.MarketValueBase, want)
				}
				if p.Currency == tc.base && p.FXRate != nil {
					t.Fatalf("%s is in the base currency yet carries a rate %v", p.Symbol, *p.FXRate)
				}
				rate := tc.rates[p.Currency]
				if p.Currency == tc.base {
					rate = 1
				}
				if math.Abs(*p.UnrealizedPnLBase-p.UnrealizedPnL*rate) > 1e-6 || math.Abs(*p.DailyPnLBase-*p.DailyPnL*rate) > 1e-6 {
					t.Fatalf("%s P&L in base: unrealized %v daily %v at rate %v", p.Symbol, *p.UnrealizedPnLBase, *p.DailyPnLBase, rate)
				}
			}

			rules := asiaSimRules(acct, pos, asiaSimNow)
			if rules.BaseCurrency != tc.base {
				t.Fatalf("rules base = %s", rules.BaseCurrency)
			}
			sim := &asiaSim{srv: newTestServer(t), now: asiaSimNow, acct: acct, pos: pos, rules: rules}

			var brief rpc.BriefResult
			if err := sim.Call(context.Background(), rpc.MethodBriefSnapshot, nil, &brief); err != nil {
				t.Fatal(err)
			}
			if brief.Review.SessionPnL.BaseCurrency != tc.base || brief.Review.SessionPnL.EquityBase == nil {
				t.Fatalf("session P&L row: %+v", brief.Review.SessionPnL)
			}
			narrative, _ := json.Marshal(brief.Narrative)
			if !strings.Contains(string(narrative), tc.base+" ") {
				t.Fatalf("%s base narrative names no %s figure: %s", tc.base, tc.base, narrative)
			}
			if strings.Contains(string(narrative), "$") {
				t.Fatalf("%s base narrative reads in dollars: %s", tc.base, narrative)
			}
			// The largest holding by base value sets the session, whatever the
			// base: the Singapore line in both books, open at 10:00 Hong Kong time.
			if brief.Ready.Session.Market != "sg_sgx" || !brief.Ready.Session.IsOpen {
				t.Fatalf("session row: %+v", brief.Ready.Session)
			}

			var st rpc.StressSnapshotResult
			if err := sim.Call(context.Background(), rpc.MethodStressSnapshot, nil, &st); err != nil {
				t.Fatal(err)
			}
			if st.Stress.AsOf.IsZero() || st.Stress.Fingerprint.Key == "" {
				t.Fatalf("stress read composed nothing: %+v", st.Stress)
			}
		})
	}
}
