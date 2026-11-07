package daemon

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/pkg/futu/futusim"
)

// These run the whole read path for the users OpenD exists to serve: a moomoo
// Singapore and a moomoo Hong Kong account in local currency, holding local and US
// shares. Simulator only. What a real login returns is assumed from the definitions
// and the docs:
//   - the firm values: FutuSG for Singapore, FutuSecurities for Hong Kong;
//   - a universal account lists its markets in any order, and its base is its firm's
//     home currency;
//   - funds answer in the currency asked for;
//   - positions carry their own currency, at OpenD's value;
//   - a market without a quote right refuses the request as moomoo US does for
//     Singapore.
const asiaBook = `{"clock":"2026-11-04T02:30:00Z","subscription_quota":100,"history_quota":100,"seed":7,
	"fx":{"HKD":1,"USD":7.8,"SGD":5.8},
	"accounts":[%s],
	"instruments":[
		{"code":"SG.D05","name":"SYNTH BANK","type":"stock","lot_size":100,"currency":"SGD","listed":"2000-01-01","start":40,"tick":0.01},
		{"code":"SG.ES3","name":"SYNTH FUND","type":"etf","lot_size":100,"currency":"SGD","listed":"2000-01-01","start":3,"tick":0.01},
		{"code":"HK.00700","name":"SYNTH TECH","type":"stock","lot_size":100,"currency":"HKD","listed":"2000-01-01","start":400,"tick":0.2},
		{"code":"US.AAPL","name":"SYNTH APPLE","type":"stock","lot_size":1,"currency":"USD","exchange":"NASDAQ","listed":"2000-01-01","start":200,"tick":0.01},
		{"code":"US.SPY","name":"SYNTH SPY","type":"etf","lot_size":1,"currency":"USD","exchange":"AMEX","listed":"2000-01-01","start":500,"tick":0.01},
		{"code":"US.QQQ","name":"SYNTH QQQ","type":"etf","lot_size":1,"currency":"USD","exchange":"NASDAQ","listed":"2000-01-01","start":400,"tick":0.01},
		{"code":"US.HYG","name":"SYNTH HYG","type":"etf","lot_size":1,"currency":"USD","exchange":"AMEX","listed":"2000-01-01","start":80,"tick":0.01},
		{"code":"HK.TCH261230C400000","name":"SYNTH CALL","type":"option","lot_size":100,"currency":"HKD","listed":"2026-01-01","start":12,"tick":0.01}
	],
	"positions":[%s]}`

func moomooAsiaSim(t *testing.T, account, positions string) *futusim.Server {
	t.Helper()
	book, err := futusim.Parse(fmt.Appendf(nil, asiaBook, account, positions))
	if err != nil {
		t.Fatal(err)
	}
	return futusim.Start(t, futusim.Options{Book: book})
}

func asiaRead[T any](t *testing.T, srv *Server, method string) T {
	t.Helper()
	res := dispatchRaw(t, srv, method, `{}`)
	if !res.Ok {
		t.Fatalf("%s: %+v", method, res.Error)
	}
	var out T
	if err := json.Unmarshal(res.Result, &out); err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	return out
}

func TestAsiaUsersReadTheirBookInLocalCurrency(t *testing.T) {
	for _, tc := range []struct {
		name, account, positions string
		base                     string
		local, foreign           string
		wantMarkets              string
	}{
		{
			name:      "moomoo Singapore, listing the US first",
			account:   `{"acc_id":3000001,"env":"real","markets":["US","SG","HK"],"type":"margin","firm":"futusg","cash":{"SGD":20000,"USD":500}}`,
			positions: `{"acc_id":3000001,"code":"SG.D05","quantity":100,"cost":38},{"acc_id":3000001,"code":"US.AAPL","quantity":10,"cost":180},{"acc_id":3000001,"code":"HK.00700","quantity":100,"cost":390}`,
			base:      "SGD", local: "D05", foreign: "AAPL", wantMarkets: "US SG HK",
		},
		{
			name:      "moomoo Hong Kong",
			account:   `{"acc_id":3000002,"env":"real","markets":["HK","US"],"type":"margin","firm":"futusecurities","cash":{"HKD":90000}}`,
			positions: `{"acc_id":3000002,"code":"HK.00700","quantity":100,"cost":390},{"acc_id":3000002,"code":"US.AAPL","quantity":10,"cost":180}`,
			base:      "HKD", local: "00700", foreign: "AAPL", wantMarkets: "HK US",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sim := moomooAsiaSim(t, tc.account, tc.positions)
			srv, stop := startOpenDServer(t, sim, "")
			defer stop()
			waitOpenDReady(t, srv)

			health := srv.opendHealth()
			if got := strings.Join(health.Markets, " "); got != tc.wantMarkets || health.AccountEnding == "" {
				t.Errorf("health markets %q ending %q", got, health.AccountEnding)
			}
			account := asiaRead[rpc.AccountResult](t, srv, rpc.MethodAccountSummary)
			if account.BaseCurrency != tc.base || account.NetLiquidation <= 0 {
				t.Errorf("account base %q net %v", account.BaseCurrency, account.NetLiquidation)
			}
			positions := asiaRead[rpc.PositionsResult](t, srv, rpc.MethodPositionsList)
			rows := map[string]rpc.PositionView{}
			for _, row := range positions.Stocks {
				rows[row.Symbol] = row
				if row.Mark <= 0 || row.MarketValue <= 0 {
					t.Errorf("%s has no value: %+v", row.Symbol, row)
				}
			}
			local, foreign := rows[tc.local], rows[tc.foreign]
			if local.Currency != tc.base || local.MarketValueBase == nil {
				t.Errorf("local row %+v", local)
			}
			if foreign.Currency != "USD" {
				t.Errorf("foreign row %+v", foreign)
			}
			// A foreign holding is measured at the rate implied by OpenD's own totals.
			if foreign.MarketValueBase == nil || foreign.FXRate == nil {
				t.Errorf("foreign row not measured: %+v", foreign)
			} else if want := foreign.MarketValue * *foreign.FXRate; math.Abs(*foreign.MarketValueBase-want) > 1e-6 {
				t.Errorf("foreign base value %v, want %v", *foreign.MarketValueBase, want)
			}
			ledger := map[string]rpc.CurrencyExposure{}
			for _, row := range account.CurrencyExposure {
				ledger[row.Currency] = row
			}
			if !ledger["USD"].ExchangeRateImplied || ledger["USD"].ExchangeRate <= 0 {
				t.Errorf("USD ledger row %+v", ledger["USD"])
			}
			if base := account.BaseCurrencyLedger; base == nil || base.ExchangeRateImplied || base.ExchangeRate != 1 {
				t.Errorf("base ledger row %+v", base)
			}
			if res := dispatchRaw(t, srv, rpc.MethodRulesSnapshot, `{}`); !res.Ok {
				t.Errorf("rules: %+v", res.Error)
			}
			if res := dispatchRaw(t, srv, rpc.MethodBriefSnapshot, `{}`); !res.Ok {
				t.Errorf("brief: %+v", res.Error)
			}
		})
	}
}

// With the Singapore right, a Singapore line streams or snapshots like any other;
// without it, its holding keeps OpenD's position price and the rest keep their quotes.
func TestSingaporeUserWithAndWithoutTheRight(t *testing.T) {
	const account = `{"acc_id":3000001,"env":"real","markets":["SG","US"],"type":"margin","firm":"futusg","cash":{"SGD":20000}}`
	const positions = `{"acc_id":3000001,"code":"SG.D05","quantity":100,"cost":38},{"acc_id":3000001,"code":"US.AAPL","quantity":10,"cost":180}`
	for _, tc := range []struct {
		name         string
		right        int32
		rightWords   string
		sourceOfD05  func(string) bool
		quoteAnswers bool
	}{
		{"with the right", 2, "Level1", func(s string) bool { return s != "position" }, true},
		{"without the right", 5, "No", func(s string) bool { return s == "position" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sim := moomooAsiaSim(t, account, positions)
			sim.SetQuoteRight("SG", tc.right)
			srv, stop := startOpenDServer(t, sim, "")
			defer stop()
			waitOpenDReady(t, srv)
			deadline := time.Now().Add(5 * time.Second)
			for srv.opendHealth().QuoteRights["SG"] != tc.rightWords && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if got := srv.opendHealth().QuoteRights["SG"]; got != tc.rightWords {
				t.Fatalf("right %q", got)
			}
			positions := asiaRead[rpc.PositionsResult](t, srv, rpc.MethodPositionsList)
			for _, row := range positions.Stocks {
				switch row.Symbol {
				case "D05":
					if !tc.sourceOfD05(row.PriceSource) || row.Mark <= 0 {
						t.Errorf("D05 %+v", row)
					}
				case "AAPL":
					if row.PriceSource == "position" {
						t.Errorf("AAPL lost its quote")
					}
				}
			}
			res := dispatchRaw(t, srv, rpc.MethodQuoteSnapshot, `{"contract":{"instrument":{"broker":"opend","id":"SG.D05"}},"timeout_ms":2000}`)
			if res.Ok != tc.quoteAnswers {
				t.Errorf("quote: %+v", res)
			}
			if !tc.quoteAnswers && !strings.Contains(res.Error.Message, "no SG quote right") {
				t.Errorf("refusal words: %s", res.Error.Message)
			}
		})
	}
}

// What a Hong Kong or Singapore user holds beyond stocks stays apart, in its currency.
func TestAsiaUserOptionIsListedApart(t *testing.T) {
	const account = `{"acc_id":3000002,"env":"real","markets":["HK","US"],"type":"margin","firm":"futusecurities","cash":{"HKD":90000}}`
	sim := moomooAsiaSim(t, account, `{"acc_id":3000002,"code":"HK.TCH261230C400000","quantity":2,"cost":10},{"acc_id":3000002,"code":"HK.00700","quantity":100,"cost":390}`)
	srv, stop := startOpenDServer(t, sim, "")
	defer stop()
	waitOpenDReady(t, srv)
	positions := asiaRead[rpc.PositionsResult](t, srv, rpc.MethodPositionsList)
	if len(positions.NotServed) != 1 || positions.NotServed[0].Currency != "HKD" || positions.Authority.PortfolioComplete {
		t.Errorf("not served %+v complete %v", positions.NotServed, positions.Authority.PortfolioComplete)
	}
}

// The rate is implied, so nothing that sizes a sweep or a levelling order runs on it
// under OpenD, even with both enabled. Plans and proposals may read it to measure;
// no path can send: TestOpenDDaemonAuthorisesNoBrokerWrite and
// TestFutuConnectorHasNoWriteMethod. Phase D has to hold this line for orders.
func TestSweepAndLevellingDoNotRunUnderOpenD(t *testing.T) {
	const account = `{"acc_id":3000001,"env":"real","markets":["SG","US"],"type":"margin","firm":"futusg","cash":{"SGD":20000,"USD":9000}}`
	sim := moomooAsiaSim(t, account, `{"acc_id":3000001,"code":"US.AAPL","quantity":10,"cost":180}`)
	srv, stop := startOpenDServer(t, sim, "")
	defer stop()
	waitOpenDReady(t, srv)
	policy := defaultProtectionPolicy()
	policy.Cash.Sweep = &protectionCashSweepPolicy{Enabled: true}
	policy.Cash.Leveling = &protectionCurrencyLevelingPolicy{Enabled: true}
	engine := &proposalEngine{server: srv}
	ctx := t.Context()
	if rows, status := engine.cashSweepProposals(ctx, policy, rpc.ProtectionPolicyStatus{}, nil, nil, rpc.TradeProposalSourceFingerprints{}, srv.currentBrokerStateScope(), time.Now()); rows != nil || status != nil {
		t.Errorf("sweep ran: %v %v", rows, status)
	}
	if rows, status := engine.currencyLevelingProposals(ctx, policy, rpc.ProtectionPolicyStatus{}, nil, rpc.TradeProposalSourceFingerprints{}, srv.currentBrokerStateScope(), time.Now()); rows != nil || status != nil {
		t.Errorf("levelling ran: %v %v", rows, status)
	}
}
