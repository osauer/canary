package daemon

import (
	"encoding/json"
	"errors"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkr "github.com/osauer/canary/v2/pkg/ibkr"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAddCallSelectionUsesActualExpiryInventory(t *testing.T) {
	// SecDef lists 772/773/774 somewhere in the chain. For this expiry,
	// only five-dollar strikes exist. The former three nearest probes all
	// failed; 770 is valid and in range after 775's delta proves too low.
	now := time.Now()
	// A 14-day expiry keeps the 0.50–0.70 range these deltas are drawn for.
	expiry := now.AddDate(0, 0, 14).Format("20060102")
	call := func(k float64) ibkr.ResolvedOrderContract {
		return ibkr.ResolvedOrderContract{UnderConID: 17, MinTick: .01, ValidExchanges: "SMART", MarketRuleIDs: "27", Contract: ibkr.Contract{ConID: 10000 + int(k), Symbol: "SYNTH", SecType: "OPT", Expiry: expiry, Strike: k, Right: "C", Multiplier: 100, Currency: "USD", Exchange: "SMART", TradingClass: "SYNTH", LocalSymbol: "SYNTH call"}}
	}
	var quoted []float64
	var mu sync.Mutex
	quote := func(exact ibkr.ResolvedOrderContract) rpc.AddCallCandidate {
		k := exact.Contract.Strike
		mu.Lock()
		quoted = append(quoted, k)
		mu.Unlock()
		if k != 765 && k != 770 && k != 775 && k != 780 {
			return rpc.AddCallCandidate{Unavailable: "No exact contract"}
		}
		if exact.UnderConID != 17 || exact.MarketRuleIDs != "27" || exact.MinTick != .01 {
			t.Fatal("inventory routing evidence lost before quote", exact)
		}
		delta := map[float64]float64{765: .75, 770: .62, 775: .47, 780: .35}[k]
		contract := setupOptionContract(exact.Contract)
		return rpc.AddCallCandidate{Contract: &contract, PriceIncrements: []rpc.SetupOptionPriceIncrement{{LowEdge: 0, Increment: .01}}, Quote: &rpc.SetupOptionQuote{Status: rpc.SetupQuoteQuoted, DataType: rpc.MarketDataLive, ModelDataType: rpc.MarketDataLive, AsOf: now, ModelAsOf: now, IV: new(.2), Delta: &delta}}
	}
	union := []ibkr.ResolvedOrderContract{call(765), call(770), call(772), call(773), call(774), call(775), call(780)}
	before := selectAddCall(testCallSide, union, 772.94, quote)
	if before.Contract != nil || !reflect.DeepEqual(sortedStrikes(quoted), []float64{772, 773, 774}) {
		t.Fatalf("fixture did not reproduce the old union probe failure: %+v %v", before, quoted)
	}
	quoted = nil
	actual := []ibkr.ResolvedOrderContract{call(765), call(770), call(775), call(780)}
	after := selectAddCall(testCallSide, actual, 772.94, quote)
	if after.Contract == nil || after.Contract.Strike != 770 || !reflect.DeepEqual(sortedStrikes(quoted), []float64{770, 775, 780}) {
		t.Fatalf("valid expiry call not selected: %+v %v", after, quoted)
	}
}

func TestAddCallSelectionKeepsQuoteBoundAndEligibility(t *testing.T) {
	for _, mode := range []string{"low-delta", "high-delta", "stale-model", "delayed-model", "missing-iv", "missing-grid"} {
		t.Run(mode, func(t *testing.T) {
			calls := make([]ibkr.ResolvedOrderContract, 6)
			for i := range calls {
				calls[i].Contract.Strike = 100 + float64(i)
			}
			var count atomic.Int32
			got := selectAddCall(testCallSide, calls, 100, func(ibkr.ResolvedOrderContract) rpc.AddCallCandidate {
				count.Add(1)
				now := time.Now()
				q := &rpc.SetupOptionQuote{Status: rpc.SetupQuoteQuoted, DataType: rpc.MarketDataLive, ModelDataType: rpc.MarketDataLive, AsOf: now, ModelAsOf: now, IV: new(.2), Delta: new(.6)}
				row := rpc.AddCallCandidate{Contract: &rpc.ContractParams{ConID: 10001}, Quote: q, PriceIncrements: []rpc.SetupOptionPriceIncrement{{LowEdge: 0, Increment: .01}}}
				switch mode {
				case "low-delta":
					q.Delta = new(.49)
				case "high-delta":
					q.Delta = new(.71)
				case "stale-model":
					q.ModelAsOf = now.Add(-16 * time.Second)
				case "delayed-model":
					q.ModelDataType = rpc.MarketDataDelayed
				case "missing-iv":
					q.IV = nil
				case "missing-grid":
					row.PriceIncrements = nil
				}
				return row
			})
			if count.Load() != 3 || got.Contract != nil || got.Unavailable == "" {
				t.Fatalf("quote bound or eligibility lost: %d %+v", count.Load(), got)
			}
		})
	}
}

func TestAddHorizonSkipsWeekAndKeepsDistinctRanges(t *testing.T) {
	now := time.Date(2026, 10, 21, 15, 0, 0, 0, time.UTC)
	rows := []rpc.SetupOptionExpiry{{Date: "20261028"}, {Date: "20261030"}, {Date: "20261120"}, {Date: "20261218"}, {Date: "20270219"}}
	for _, test := range []struct {
		target, min, max int
		want             string
	}{{14, 8, 21, "20261030"}, {30, 22, 45, "20261120"}, {60, 46, 90, "20261218"}, {120, 91, 150, "20270219"}} {
		date, _ := addHorizonExpiry(rows, test.target, test.min, test.max, now)
		if date != test.want {
			t.Fatalf("%d: %s", test.target, date)
		}
	}
}
func TestAddCallNeedsIndependentLiveModelEvidence(t *testing.T) {
	now := time.Now()
	q := &rpc.SetupOptionQuote{Status: "quoted", DataType: rpc.MarketDataLive, AsOf: now, IV: new(.3), Delta: new(.6), ModelAsOf: now, ModelDataType: rpc.MarketDataLive}
	row := rpc.AddCallCandidate{Contract: &rpc.ContractParams{ConID: 10001}, Quote: q, PriceIncrements: []rpc.SetupOptionPriceIncrement{{LowEdge: 0, Increment: .01}}}
	if !eligibleAddCall(testCallSide, row, now) {
		t.Fatal("fresh exact model rejected")
	}
	q.ModelAsOf = now.Add(-16 * time.Second)
	if eligibleAddCall(testCallSide, row, now) {
		t.Fatal("price freshened model")
	}
	q.ModelAsOf = now
	q.ModelDataType = rpc.MarketDataDelayed
	if eligibleAddCall(testCallSide, row, now) {
		t.Fatal("delayed model upgraded")
	}
}
func TestRoundCallQuantityPreservesSmallLotsAndRoundsBothDirections(t *testing.T) {
	for n, want := range map[int]int{0: 0, 1: 1, 2: 2, 3: 5, 6: 5, 7: 5, 8: 10, 9: 10, 10: 10, 12: 10, 13: 15} {
		if got := roundCallQuantity(n); got != want {
			t.Fatalf("%d -> %d, want %d", n, got, want)
		}
	}
}

func TestAddMarketStockCurrencyDoesNotEnableForeignCalls(t *testing.T) {
	for _, tc := range []struct {
		currency string
		discover bool
		allowed  bool
	}{{"EUR", false, true}, {"JPY", false, true}, {"USD", true, true}, {"EUR", true, false}, {"usd", false, false}, {"", false, false}} {
		p := rpc.AddMarketParams{Underlying: rpc.ContractParams{ConID: 10001, Symbol: "SYNX", SecType: "STK", Exchange: "SMART", Currency: tc.currency}, Discover: tc.discover}
		raw, _ := json.Marshal(p)
		_, err := (&Server{}).handleAddMarket(t.Context(), &rpc.Request{Params: raw})
		// A zero server has no broker. Allowed identities reach that explicit boundary;
		// unsupported call currencies must fail validation before any broker access.
		if got := errors.Is(err, ibkr.ErrIBKRUnavailable); got != tc.allowed {
			t.Fatalf("currency=%s discover=%v allowed=%v: %v", tc.currency, tc.discover, tc.allowed, err)
		}
	}
}
