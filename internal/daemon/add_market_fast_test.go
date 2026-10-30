package daemon

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/pkg/ibkr"
)

// SecDef pools strikes across expiries: 772/773/774 are listed but this
// expiry has only five-dollar strikes. Resolving the nearest listed strikes
// one contract at a time drops the phantoms before any quote slot is used.
func TestNearestResolvedCallsDropPhantomStrikes(t *testing.T) {
	var mu sync.Mutex
	var asked []float64
	resolve := func(k float64) (ibkr.ResolvedOrderContract, error) {
		mu.Lock()
		asked = append(asked, k)
		mu.Unlock()
		if int(k)%5 != 0 {
			return ibkr.ResolvedOrderContract{}, errors.New("no definition")
		}
		return ibkr.ResolvedOrderContract{Contract: ibkr.Contract{ConID: 10000 + int(k), Strike: k}}, nil
	}
	listed := []float64{700, 765, 770, 772, 773, 774, 775, 780, 785, 900}
	got := nearestResolvedCalls(listed, 772.94, resolve)
	strikes := []float64{}
	for _, c := range got {
		strikes = append(strikes, c.Contract.Strike)
	}
	if !slices.Equal(strikes, []float64{775, 770, 780, 765, 785}) {
		t.Fatalf("resolved %v, want the real strikes nearest spot first", strikes)
	}
	if len(asked) != addResolveWidth || slices.Contains(asked, 700) || slices.Contains(asked, 900) {
		t.Fatalf("asked %v, want only the %d nearest listed strikes", asked, addResolveWidth)
	}
	now := time.Now()
	row := selectAddCall(testCallSide, got, 772.94, func(exact ibkr.ResolvedOrderContract) rpc.AddCallCandidate {
		delta := map[float64]float64{765: .75, 770: .62, 775: .47, 780: .35}[exact.Contract.Strike]
		contract := setupOptionContract(exact.Contract)
		return rpc.AddCallCandidate{Contract: &contract, PriceIncrements: []rpc.SetupOptionPriceIncrement{{LowEdge: 0, Increment: .01}}, Quote: &rpc.SetupOptionQuote{Status: rpc.SetupQuoteQuoted, DataType: rpc.MarketDataLive, ModelDataType: rpc.MarketDataLive, AsOf: now, ModelAsOf: now, IV: new(.2), Delta: &delta}}
	})
	if row.Contract == nil || row.Contract.Strike != 770 {
		t.Fatalf("selected %+v, want 770", row.Contract)
	}
}

// waitAll returns a func that blocks until n callers arrived, or fails.
func waitAll(t *testing.T, n int) func() {
	var mu sync.Mutex
	arrived, all := 0, make(chan struct{})
	return func() {
		mu.Lock()
		if arrived++; arrived == n {
			close(all)
		}
		mu.Unlock()
		select {
		case <-all:
		case <-time.After(2 * time.Second):
			t.Errorf("only %d of %d ran at once", arrived, n)
		}
	}
}

func TestAddCallSelectionQuotesItsThreeCandidatesAtOnce(t *testing.T) {
	calls := make([]ibkr.ResolvedOrderContract, 5)
	for i := range calls {
		calls[i].Contract.Strike = 100 + float64(i)
	}
	barrier := waitAll(t, 3)
	selectAddCall(testCallSide, calls, 100, func(ibkr.ResolvedOrderContract) rpc.AddCallCandidate {
		barrier()
		return rpc.AddCallCandidate{Unavailable: "test"}
	})
}

func TestAddDiscoveryRunsAllFourHorizonsAtOnce(t *testing.T) {
	barrier := waitAll(t, 4)
	addParallel(4, addHorizonSlots, func(int) { barrier() })
}

func sortedStrikes(s []float64) []float64 { return slices.Sorted(slices.Values(s)) }

// Owner, 2026-10-22 20:34 CEST: lower call delta for longer expiries.
func TestAddDeltaBandsFollowDaysToExpiry(t *testing.T) {
	for _, tc := range []struct {
		dte      int
		min, max float64
	}{{13, .5, .7}, {21, .5, .7}, {22, .35, .5}, {29, .35, .5}, {45, .35, .5}, {57, .25, .4}, {120, .25, .4}} {
		if b := testCallSide.bandForDTE(tc.dte); b.Min != tc.min || b.Max != tc.max {
			t.Errorf("%d days: %+v, want %v–%v", tc.dte, b, tc.min, tc.max)
		}
	}
}

// A long-dated row keeps the in-band call nearest its cap: the cheapest call
// that still carries the owner's maximum delta.
func TestAddLongHorizonPicksTheDeltaNearestItsCap(t *testing.T) {
	expiry := time.Now().AddDate(0, 0, 120).Format("20060102")
	deltas := map[float64]float64{325: .55, 330: .52, 350: .42, 355: .39, 360: .36}
	calls := []ibkr.ResolvedOrderContract{}
	for k := range deltas {
		calls = append(calls, ibkr.ResolvedOrderContract{Contract: ibkr.Contract{ConID: int(k), Strike: k, Expiry: expiry}})
	}
	now := time.Now()
	quote := func(exact ibkr.ResolvedOrderContract) rpc.AddCallCandidate {
		contract := setupOptionContract(exact.Contract)
		return rpc.AddCallCandidate{Contract: &contract, PriceIncrements: []rpc.SetupOptionPriceIncrement{{LowEdge: 0, Increment: .01}}, Quote: &rpc.SetupOptionQuote{Status: rpc.SetupQuoteQuoted, DataType: rpc.MarketDataLive, ModelDataType: rpc.MarketDataLive, AsOf: now, ModelAsOf: now, IV: new(.42), Delta: new(deltas[exact.Contract.Strike])}}
	}
	if row, iv := selectAddCallNear(testCallSide, calls, 325.5, quote); row.Contract != nil || iv != .42 {
		t.Fatalf("at-the-money calls are above the 0.40 cap, want none and the volatility to aim with: %+v %v", row.Contract, iv)
	}
	aim := addAimStrike(325.5, .42, 120, .4)
	if aim < 350 || aim > 360 {
		t.Fatalf("aim %v, want the strike where delta is about 0.40", aim)
	}
	if row, _ := selectAddCallNear(testCallSide, calls, aim, quote); row.Contract == nil || row.Contract.Strike != 355 {
		t.Fatalf("picked %+v, want 355 (0.39, nearest the cap)", row.Contract)
	}
}

// addPool serves strikes with fixed deltas for one expiry. missing makes
// every quote miss its window until it is cleared; refs records each
// round's starting strike.
type addPool struct {
	expiry  string
	deltas  map[float64]float64
	mu      sync.Mutex
	missing int
	refs    []float64
}

func newAddPool(dte int, deltas map[float64]float64) *addPool {
	return &addPool{expiry: time.Now().AddDate(0, 0, dte).Format("20060102"), deltas: deltas}
}

func (p *addPool) near(ref float64) ([]ibkr.ResolvedOrderContract, error) {
	p.mu.Lock()
	p.refs = append(p.refs, ref)
	p.mu.Unlock()
	out := []ibkr.ResolvedOrderContract{}
	for k := range p.deltas {
		out = append(out, ibkr.ResolvedOrderContract{Contract: ibkr.Contract{ConID: int(k * 10), Strike: k, Expiry: p.expiry}})
	}
	return out, nil
}

func (p *addPool) quote(exact ibkr.ResolvedOrderContract) rpc.AddCallCandidate {
	contract := setupOptionContract(exact.Contract)
	p.mu.Lock()
	miss := p.missing > 0
	if miss {
		p.missing--
	}
	p.mu.Unlock()
	if miss {
		return rpc.AddCallCandidate{Contract: &contract, Quote: &rpc.SetupOptionQuote{Status: rpc.SetupQuoteMissing}, Unavailable: "Exact call evidence unavailable"}
	}
	now := time.Now()
	return rpc.AddCallCandidate{Contract: &contract, PriceIncrements: []rpc.SetupOptionPriceIncrement{{LowEdge: 0, Increment: .01}}, Quote: &rpc.SetupOptionQuote{Status: rpc.SetupQuoteQuoted, DataType: rpc.MarketDataLive, ModelDataType: rpc.MarketDataLive, AsOf: now, ModelAsOf: now, IV: new(.42), Delta: new(p.deltas[exact.Contract.Strike])}}
}

var addLongDeltas = map[float64]float64{320: .6, 325: .55, 330: .52, 345: .44, 350: .42, 355: .39, 360: .36, 365: .33}

// The stock's volatility aims a capped horizon straight at its target: one
// round, starting near the 0.40 strike rather than at spot.
func TestAddCappedHorizonAimsWithTheStockVolatility(t *testing.T) {
	p := newAddPool(120, addLongDeltas)
	row, _, err := selectAddCallAimed(testCallSide, 325.5, .42, 120, p.quote, p.near)
	if err != nil || row.Contract == nil || row.Contract.Strike != 355 || len(p.refs) != 1 || p.refs[0] < 350 {
		t.Fatalf("picked %+v after rounds from %v, want 355 in one round aimed near 0.40", row.Contract, p.refs)
	}
}

// Without a stock volatility the first round starts at spot; when nothing
// there is in range, the options' own volatility aims the second (seen live
// 2026-10-22 21:03 CEST on a 30-day row).
func TestAddSecondRoundAimsWithTheOptionVolatility(t *testing.T) {
	p := newAddPool(29, map[float64]float64{320: .57, 322.5: .54, 325: .515, 327.5: .49, 330: .465})
	row, _, err := selectAddCallAimed(testCallSide, 322.6, 0, 29, p.quote, p.near)
	if err != nil || row.Contract == nil || row.Contract.Strike != 327.5 || len(p.refs) != 2 {
		t.Fatalf("picked %+v after rounds from %v, want 327.5 after aiming", row.Contract, p.refs)
	}
}

// Every quote of the first round missed its window (seen live under load
// on a 60-day row): the round is repeated rather than the row left empty.
func TestAddMissedQuotesAreRetriedOnce(t *testing.T) {
	p := newAddPool(57, addLongDeltas)
	p.missing = 3
	row, _, err := selectAddCallAimed(testCallSide, 325.5, .42, 57, p.quote, p.near)
	if err != nil || row.Contract == nil || testCallSide.bandForDTE(57).holds(*row.Quote.Delta) == false || len(p.refs) != 2 {
		t.Fatalf("picked %+v after rounds from %v, want an in-range call from the retry", row.Contract, p.refs)
	}
}

// A failed second round never discards what the first round found.
func TestAddFailedSecondRoundKeepsTheFirst(t *testing.T) {
	p := newAddPool(120, map[float64]float64{325: .55, 330: .52, 335: .49})
	calls := 0
	near := func(ref float64) ([]ibkr.ResolvedOrderContract, error) {
		if calls++; calls > 1 {
			return nil, errors.New("inventory timeout")
		}
		return p.near(ref)
	}
	row, _, err := selectAddCallAimed(testCallSide, 325.5, 0, 120, p.quote, near)
	if err != nil || row.Unavailable != "No eligible call" || calls != 2 {
		t.Fatalf("got %+v err=%v after %d rounds, want the first round's result kept", row, err, calls)
	}
}

// A SecDef list can mix 1, 2.5 and 5 USD spacing. A long expiry with only
// five-dollar strikes finds one or two in the first eight; the search widens
// instead of falling back to the slow complete inventory.
func TestNearestResolvedCallsWidenPastMixedSpacing(t *testing.T) {
	listed := []float64{}
	for k := 300.0; k <= 350; k++ {
		listed = append(listed, k)
		if k < 350 {
			listed = append(listed, k+.5)
		}
	}
	var mu sync.Mutex
	asked := 0
	got := nearestResolvedCalls(listed, 323.4, func(k float64) (ibkr.ResolvedOrderContract, error) {
		mu.Lock()
		asked++
		mu.Unlock()
		if int(k*10)%50 != 0 {
			return ibkr.ResolvedOrderContract{}, errors.New("no definition")
		}
		return ibkr.ResolvedOrderContract{Contract: ibkr.Contract{ConID: int(k), Strike: k}}, nil
	})
	strikes := []float64{}
	for _, c := range got {
		strikes = append(strikes, c.Contract.Strike)
	}
	if len(strikes) < 3 || strikes[0] != 325 || strikes[1] != 320 || asked > addResolveWidth*addResolveBatches {
		t.Fatalf("resolved %v after %d lookups, want 325, 320 and a third without the inventory", strikes, asked)
	}
}

// A put side aims below spot, judges the delta by its size and keeps the
// put nearest its target: 0.25 for 90 days by default.
func TestAddPutHorizonAimsBelowSpotAtItsTarget(t *testing.T) {
	puts := defaultAddSide("P")
	p := newAddPool(90, map[float64]float64{330: -.52, 320: -.45, 310: -.36, 300: -.27, 295: -.24, 290: -.21, 280: -.15})
	row, _, err := selectAddCallAimed(puts, 325.5, .42, 90, p.quote, p.near)
	if err != nil || row.Contract == nil || row.Contract.Strike != 295 || len(p.refs) != 1 || p.refs[0] > 300 {
		t.Fatalf("picked %+v after rounds from %v, want 295 in one round aimed below spot", row.Contract, p.refs)
	}
	calls := newAddPool(90, map[float64]float64{295: .24})
	if row := selectAddCall(puts, nil, 0, calls.quote); row.Contract != nil || row.Unavailable != "No eligible put" {
		t.Fatalf("an empty put expiry = %+v", row)
	}
	if row := pickAddCall(puts, calls.quote(ibkr.ResolvedOrderContract{Contract: ibkr.Contract{Strike: 295, Expiry: calls.expiry}})); row.Contract != nil {
		t.Fatal("a positive delta passed as a put")
	}
}
