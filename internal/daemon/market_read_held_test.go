package daemon

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/broker"
	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

// All symbols and prices here are invented; nothing is a real holding.

// held adds a held name whose prior session ranged 95 to 105 and closed at
// 100, quoted at last with the session open at open (zero: not quoted).
func (m *readMarket) held(symbol string, open, last float64) *marketReadHeldSource {
	priorDay, _ := time.Parse(time.DateOnly, m.prior.Date)
	h := marketReadHeldSource{Symbol: symbol,
		Quote: &broker.BriefQuote{Close: 100, Open: open, Last: last, TradeAt: m.session.Open.Add(time.Hour)},
		Daily: &rpc.MarketHistoryResult{Contract: rpc.ContractParams{Symbol: symbol}, Interval: "1 day", RegularHoursOnly: true,
			Points: []rpc.MarketHistoryPoint{{At: priorDay, Value: 100, Open: new(99.0), High: new(105.0), Low: new(95.0)}}}}
	m.src.Held = append(m.src.Held, h)
	return &m.src.Held[len(m.src.Held)-1]
}

// bars gives a held name today's five-minute bars as (high, low) pairs,
// fetched at fetched.
func (m *readMarket) heldBars(h *marketReadHeldSource, fetched time.Time, ranges ...[2]float64) {
	var points []rpc.MarketHistoryPoint
	for i, r := range ranges {
		mid := (r[0] + r[1]) / 2
		points = append(points, rpc.MarketHistoryPoint{At: m.session.Open.Add(time.Duration(i) * marketReadBar), Value: mid, Open: new(mid), High: new(r[0]), Low: new(r[1])})
	}
	h.Intraday = intraday(h.Symbol, fetched, points)
}

func heldRow(t *testing.T, r *rpc.MarketReadResult, symbol string) rpc.MarketReadHeldRow {
	t.Helper()
	if r.Held == nil {
		t.Fatal("held block absent")
	}
	for _, row := range r.Held.Rows {
		if row.Symbol == symbol {
			return row
		}
	}
	t.Fatalf("no held row for %s in %+v", symbol, r.Held)
	return rpc.MarketReadHeldRow{}
}

func eq(p *float64, want float64) bool { return p != nil && *p == want }

func TestMarketReadHeldDayLevelsFromBars(t *testing.T) {
	m := newReadMarket(t)
	h := m.held("HLDA", 102, 103)
	m.heldBars(h, m.at(3), [2]float64{104, 101}, [2]float64{106, 102}, [2]float64{105, 100})
	row := heldRow(t, m.read(m.at(3)), "HLDA")
	if row.DaySource != "bars" || !eq(row.DayHigh, 106) || !eq(row.DayLow, 100) || !eq(row.DayMid, 103) {
		t.Fatalf("day levels from bars: %+v", row)
	}
	if row.Prior == nil || row.Prior.High != 105 || row.Prior.Low != 95 || row.Prior.Close != 100 || row.Prior.Date != m.prior.Date {
		t.Fatalf("prior: %+v", row.Prior)
	}
	if row.Position != "inside" || row.LastSource != "trade" || !eq(row.Last, 103) || !eq(row.Open, 102) || row.OpenSource != "session_open" {
		t.Fatalf("position, last, open: %+v", row)
	}
	if row.State != rpc.LeaderHoldsGapUp {
		t.Fatalf("state %q (%s)", row.State, row.StateUnavailable)
	}
}

// The bars lag the price by up to a bar and the refresh; a price past the
// last completed bar widens today's range, so the midpoint stays inside it.
func TestMarketReadHeldDayRangeTakesInTheCurrentPrice(t *testing.T) {
	m := newReadMarket(t)
	h := m.held("HLDA", 102, 108)
	m.heldBars(h, m.at(2), [2]float64{104, 101}, [2]float64{106, 102})
	row := heldRow(t, m.read(m.at(2)), "HLDA")
	if !eq(row.DayHigh, 108) || !eq(row.DayLow, 101) || !eq(row.DayMid, 104.5) {
		t.Fatalf("range not widened to the price: %+v", row)
	}
	if row.Position != "above_prior_high" {
		t.Fatalf("position %q", row.Position)
	}
}

func TestMarketReadHeldDayLevelsFromTheQuote(t *testing.T) {
	m := newReadMarket(t)
	h := m.held("HLDA", 99, 94)
	h.DayHigh, h.DayLow = 100, 93
	row := heldRow(t, m.read(m.at(3)), "HLDA")
	if row.DaySource != "quote" || !eq(row.DayHigh, 100) || !eq(row.DayLow, 93) || !eq(row.DayMid, 96.5) {
		t.Fatalf("day levels from the quote: %+v", row)
	}
	if row.Position != "below_prior_low" || row.State != rpc.LeaderBelowPriorLow {
		t.Fatalf("position %q state %q", row.Position, row.State)
	}
	// Stale bars do not serve while the session is open: the quote does.
	m.heldBars(h, m.at(3).Add(-time.Hour), [2]float64{120, 80})
	if row := heldRow(t, m.read(m.at(20)), "HLDA"); row.DaySource != "quote" {
		t.Fatalf("stale bars served: %+v", row)
	}
}

func TestMarketReadHeldStates(t *testing.T) {
	for _, tc := range []struct {
		open, last float64
		want       string
	}{
		{102, 101.5, rpc.LeaderHoldsGapUp},
		{102, 99.5, rpc.LeaderGapUpFilled},
		{98, 98.5, rpc.LeaderHoldsGapDown},
		{98, 100.5, rpc.LeaderGapDownReclaimed},
		{100.1, 100.2, rpc.LeaderNoGap},
	} {
		m := newReadMarket(t)
		h := m.held("HLDA", tc.open, tc.last)
		h.Daily.Points[0].Low = new(97.0)
		if row := heldRow(t, m.read(m.at(3)), "HLDA"); row.State != tc.want {
			t.Errorf("open %v last %v: state %q, want %q (%s)", tc.open, tc.last, row.State, tc.want, row.StateUnavailable)
		}
	}
}

// Before the open the prior range and the last price are served; today's
// range, the position and the state are absent with reasons, whatever the
// quote's high and low say.
func TestMarketReadHeldBeforeTheOpen(t *testing.T) {
	m := newReadMarket(t)
	h := m.held("HLDA", 99, 101)
	h.Quote.TradeAt = m.session.Open.Add(-time.Hour)
	h.DayHigh, h.DayLow = 105, 95
	row := heldRow(t, m.read(m.session.Open.Add(-30*time.Minute)), "HLDA")
	if row.Prior == nil || !eq(row.Last, 101) || row.LastSource != "trade" {
		t.Fatalf("prior and last before the open: %+v", row)
	}
	if row.DayHigh != nil || row.DayMid != nil || row.DayUnavailable == "" || row.Position != "" || row.PositionUnavailable == "" || row.State != "" || row.StateUnavailable == "" || row.Open != nil {
		t.Fatalf("today's values drawn before the open: %+v", row)
	}
	// A pre-market trade of 101 inside the prior range of 95 to 105 is
	// indicated inside; one of 94 under its low.
	if row.IndicatedPosition != rpc.MarketReadInsidePrior {
		t.Fatalf("indicated position of a pre-market trade inside the range: %q", row.IndicatedPosition)
	}
	h.Quote.Last = 94
	if row := heldRow(t, m.read(m.session.Open.Add(-30*time.Minute)), "HLDA"); row.IndicatedPosition != rpc.MarketReadBelowPriorLow || row.Position != "" {
		t.Fatalf("indicated position under the prior low: %+v", row)
	}
	// A price that is the prior close is that close, and indicates nothing.
	h.Quote.Last = 100
	if row := heldRow(t, m.read(m.session.Open.Add(-30*time.Minute)), "HLDA"); row.LastSource != "prior_close" || !row.LastAt.Equal(m.prior.Close.UTC()) || row.IndicatedPosition != "" {
		t.Fatalf("prior close before the open: %+v", row)
	}
	// Once the session is open only position is drawn.
	h.Quote.TradeAt = m.session.Open.Add(time.Hour)
	if row := heldRow(t, m.read(m.session.Open.Add(90*time.Minute)), "HLDA"); row.IndicatedPosition != "" {
		t.Fatalf("indicated position in the session: %+v", row)
	}
}

// After the close the row reads the finished session from its daily bar and
// never a later print.
func TestMarketReadHeldAfterTheClose(t *testing.T) {
	m := newReadMarket(t)
	h := m.held("HLDA", 102, 120)
	sessionDay, _ := time.Parse(time.DateOnly, m.session.Date)
	h.Daily.Points = append(h.Daily.Points, rpc.MarketHistoryPoint{At: sessionDay, Value: 104, Open: new(102.0), High: new(107.0), Low: new(101.0)})
	row := heldRow(t, m.read(m.session.Close.Add(time.Hour)), "HLDA")
	if row.DaySource != "daily_bar" || !eq(row.DayHigh, 107) || !eq(row.DayLow, 101) || !eq(row.DayMid, 104) {
		t.Fatalf("closed day levels: %+v", row)
	}
	if !eq(row.Last, 104) || row.LastSource != "daily_bar" || row.Position != "inside" || row.State != rpc.LeaderHoldsGapUp || row.OpenSource != "daily_bar" {
		t.Fatalf("closed last, position, state: %+v", row)
	}
	h.Daily.Points = h.Daily.Points[:1]
	row = heldRow(t, m.read(m.session.Close.Add(time.Hour)), "HLDA")
	if row.Last != nil || row.LastUnavailable == "" || row.DayUnavailable == "" || row.StateUnavailable == "" || row.PositionUnavailable == "" {
		t.Fatalf("closed without the session's bar: %+v", row)
	}
}

func TestMarketReadHeldMissingValuesHaveReasons(t *testing.T) {
	m := newReadMarket(t)
	h := m.held("HLDA", 0, 0)
	h.Quote, h.Daily = nil, nil
	m.src.Held = append(m.src.Held, marketReadHeldSource{Symbol: "HLDB", Unquoted: "the holding takes no market data"})
	r := m.read(m.at(3))
	row := heldRow(t, r, "HLDA")
	if row.Prior != nil || row.PriorUnavailable == "" || row.Last != nil || row.LastUnavailable != "quote unavailable" || row.DayUnavailable == "" || row.PositionUnavailable == "" || row.StateUnavailable == "" {
		t.Fatalf("missing values: %+v", row)
	}
	row = heldRow(t, r, "HLDB")
	if row.LastUnavailable != "the holding takes no market data" || row.StateUnavailable != row.LastUnavailable || row.DayUnavailable == "" {
		t.Fatalf("unquoted holding: %+v", row)
	}
}

// A held leader is read once: its held row carries the leader row's values.
func TestMarketReadHeldLeaderMatchesItsLeaderRow(t *testing.T) {
	m := newReadMarket(t)
	m.leader("LDRA", new(102.0), new(101.0))
	m.src.Held = append(m.src.Held, marketReadHeldSource{Symbol: "LDRA", Leader: true, DayHigh: 103, DayLow: 99})
	r := m.read(m.at(3))
	leader, row := r.Leaders.Rows[0], heldRow(t, r, "LDRA")
	if !eq(row.Last, *leader.Last) || !eq(row.Open, *leader.Open) || row.OpenSource != leader.OpenSource || !row.LastAt.Equal(leader.LastAt) || row.State != leader.State || row.StateUnavailable != leader.StateUnavailable {
		t.Fatalf("held leader differs: held %+v leader %+v", row, leader)
	}
	if row.DaySource != "quote" || !eq(row.DayMid, 101) {
		t.Fatalf("held leader's day range: %+v", row)
	}
}

func TestMarketReadHeldNothingHeld(t *testing.T) {
	m := newReadMarket(t)
	r := m.read(m.at(3))
	if r.Held == nil || r.Held.Rows == nil || len(r.Held.Rows) != 0 || r.Held.RowsUnavailable != "nothing held" {
		t.Fatalf("nothing held: %+v", r.Held)
	}
	m.src.HeldUnavailable = "positions not readable"
	if r := m.read(m.at(3)); r.Held.RowsUnavailable != "positions not readable" {
		t.Fatalf("unreadable positions: %+v", r.Held)
	}
}

func syntheticPositions(symbols ...string) *rpc.PositionsResult {
	out := &rpc.PositionsResult{Authority: &rpc.AccountDataAuthority{Availability: rpc.AccountDataAvailable, Freshness: rpc.AccountDataFreshnessCurrent}}
	for i, sym := range symbols {
		out.ByUnderlying = append(out.ByUnderlying, rpc.PositionGroup{Underlying: sym,
			Stock: &rpc.PositionView{Symbol: sym, SecType: "STK", Currency: "USD", ConID: 9000 + i}})
	}
	return out
}

// The held names are picked as market.snapshot picks them, capped as there.
func TestMarketReadHoldingsFollowThePositions(t *testing.T) {
	var symbols []string
	for i := range marketReadHeldCap + 2 {
		symbols = append(symbols, fmt.Sprintf("HLD%c", 'A'+i))
	}
	positions := syntheticPositions(symbols...)
	positions.ByUnderlying = append(positions.ByUnderlying, positions.ByUnderlying[0])
	s := &Server{marketReadPositionsForTest: func(context.Context) (*rpc.PositionsResult, error) { return positions, nil }}
	got, truncated, reason := s.marketReadHoldings(t.Context())
	if len(got) != marketReadHeldCap || !truncated || reason != "" || got[0].Contract.Symbol != "HLDA" || got[0].Contract.ConID != 9000 {
		t.Fatalf("holdings %d truncated %v reason %q", len(got), truncated, reason)
	}
	positions.Authority.Freshness = rpc.AccountDataFreshnessStale
	if _, _, reason := s.marketReadHoldings(t.Context()); reason == "" {
		t.Fatal("stale positions served")
	}
	s.marketReadPositionsForTest = func(context.Context) (*rpc.PositionsResult, error) { return nil, context.DeadlineExceeded }
	if _, _, reason := s.marketReadHoldings(t.Context()); reason != "positions not readable" {
		t.Fatalf("unreadable positions: %q", reason)
	}
}

// The chart series market.snapshot keeps warm for a holding are the ones
// the read looks up: both spell the contract the same way.
func TestMarketReadHeldSharesTheSnapshotSeries(t *testing.T) {
	g := syntheticPositions("HLDA").ByUnderlying[0]
	g.Stock.Exchange = "NASDAQ"
	contract, ok := rpc.UnderlyingMarketContract(g)
	if !ok {
		t.Fatal("no contract")
	}
	_, echoed, _, err := normaliseStockQuoteContract(contract)
	if err != nil {
		t.Fatal(err)
	}
	for _, rng := range []string{"1D", "6M"} {
		follow := rng
		if rng == "6M" {
			follow = "1Y" // market.snapshot follows the year; it is the same daily series
		}
		a, _, errA := marketHistoryIdentity(rpc.MarketHistoryParams{Contract: contract, Range: rng})
		b, _, errB := marketHistoryIdentity(rpc.MarketHistoryParams{Contract: echoed, Range: follow})
		if errA != nil || errB != nil || a != b {
			t.Fatalf("%s: read key %s, snapshot key %s (%v %v)", rng, a, b, errA, errB)
		}
	}
}

// A line the daemon holds serves the held name it is for, and no other.
func TestMarketReadHeldLineMatchesItsContract(t *testing.T) {
	c := rpc.ContractParams{Symbol: "HLDA", SecType: "STK", Exchange: "SMART", Currency: "USD", ConID: 9000}
	stock := ibkrlib.MarketDataKeyForContract(ibkrlib.Contract{Symbol: "HLDA", SecType: "STK", Exchange: "SMART", Currency: "USD", ConID: 9000})
	other := ibkrlib.MarketDataKeyForContract(ibkrlib.Contract{Symbol: "HLDA", SecType: "STK", Exchange: "SMART", Currency: "USD", ConID: 9001})
	option := ibkrlib.MarketDataKeyForContract(ibkrlib.Contract{Symbol: "HLDA", SecType: "OPT", Exchange: "SMART", Currency: "USD", ConID: 9002})
	euro := ibkrlib.MarketDataKeyForContract(ibkrlib.Contract{Symbol: "HLDA", SecType: "STK", Exchange: "SMART", Currency: "EUR"})
	for key, want := range map[string]bool{stock: true, "HLDA": true, other: false, option: false, euro: false, "HLDB": false} {
		if got := marketReadLineIs(key, c); got != want {
			t.Errorf("%s: %v, want %v", key, got, want)
		}
	}
	at := time.Date(2026, 9, 23, 15, 0, 0, 0, time.UTC)
	lines := map[string]*broker.Quote{
		stock:  {Symbol: "HLDA", Last: 101, Close: 100, High: 102, Low: 99, Timestamp: at},
		option: {Symbol: "HLDA", Last: 3, Close: 2, Timestamp: at.Add(time.Minute)},
		"HLDA": {Symbol: "HLDA", Last: 0, Close: 100, Timestamp: at.Add(time.Minute)}, // no price yet
	}
	line := marketReadHeldLine([]string{option, "HLDA", stock}, lines, c)
	if line == nil || line.Last != 101 || line.High != 102 {
		t.Fatalf("line %+v", line)
	}
	if q := marketReadLineQuote(line); q.Last != 101 || q.Close != 100 || !q.ObservedAt.Equal(at) {
		t.Fatalf("line as a quote %+v", q)
	}
}

// Eight held names whose quotes never answer leave the read inside its
// budget, every row marked unavailable with a reason; the core looks still
// answer.
func TestMarketReadHeldQuotesThatNeverAnswerStayInsideTheBudget(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var symbols []string
	for i := range 8 {
		symbols = append(symbols, fmt.Sprintf("HLD%c", 'A'+i))
	}
	asked := make(chan string, 64)
	now := time.Date(2026, 9, 23, 15, 0, 0, 0, time.UTC)
	s := &Server{now: func() time.Time { return now },
		marketReadPositionsForTest: func(context.Context) (*rpc.PositionsResult, error) { return syntheticPositions(symbols...), nil },
		marketReadLookForTest: func(ctx context.Context, c rpc.ContractParams) (broker.BriefQuote, string, error) {
			if !strings.HasPrefix(c.Symbol, "HLD") {
				return broker.BriefQuote{Last: 50, Close: 49, Open: 49.5}, "", nil
			}
			asked <- c.Symbol
			<-release // a quote that never answers within the read
			return broker.BriefQuote{}, "", context.DeadlineExceeded
		}}
	start := time.Now()
	r, err := s.handleMarketRead(t.Context(), &rpc.Request{})
	took := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if took > marketReadBudget+time.Second {
		t.Fatalf("the read took %s, over its %s budget", took, marketReadBudget)
	}
	if r.Held == nil || len(r.Held.Rows) != len(symbols) {
		t.Fatalf("held rows %+v", r.Held)
	}
	for _, row := range r.Held.Rows {
		if row.Last != nil || row.LastUnavailable == "" || row.PositionUnavailable == "" || row.DayUnavailable == "" {
			t.Errorf("%s: unanswered quote not marked unavailable: %+v", row.Symbol, row)
		}
	}
	if len(asked) != marketReadHeldParallel {
		t.Errorf("%d held looks in flight, want the pool of %d", len(asked), marketReadHeldParallel)
	}
	if r.Leaders == nil || len(r.Leaders.Rows) == 0 || r.Leaders.Rows[0].Last == nil {
		t.Errorf("held looks crowded out the leaders: %+v", r.Leaders)
	}
}

// The positions read can probe the broker while it prewarms quotes; one
// that never answers leaves the rest of the read inside its budget and the
// held block says why it has no rows.
func TestMarketReadHeldPositionsThatNeverAnswerStayInsideTheBudget(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	now := time.Date(2026, 9, 23, 15, 0, 0, 0, time.UTC)
	s := &Server{now: func() time.Time { return now },
		marketReadPositionsForTest: func(context.Context) (*rpc.PositionsResult, error) {
			<-release
			return nil, context.DeadlineExceeded
		},
		marketReadLookForTest: func(context.Context, rpc.ContractParams) (broker.BriefQuote, string, error) {
			return broker.BriefQuote{Last: 50, Close: 49, Open: 49.5}, "", nil
		}}
	start := time.Now()
	r, err := s.handleMarketRead(t.Context(), &rpc.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > marketReadBudget+time.Second {
		t.Fatalf("the read took %s, over its %s budget", took, marketReadBudget)
	}
	if r.Held == nil || len(r.Held.Rows) != 0 || r.Held.RowsUnavailable != "positions not read within the read's budget" {
		t.Fatalf("held block %+v", r.Held)
	}
	if r.Leaders == nil || len(r.Leaders.Rows) == 0 || r.Leaders.Rows[0].Last == nil {
		t.Errorf("the positions read held up the leaders: %+v", r.Leaders)
	}
}

// Every leader and held row carries its day change against the prior close:
// in the session the last, before the open a same-day pre-market price (the
// indicated gap), after the close the session's close.
func TestMarketReadChangeAgainstThePriorClose(t *testing.T) {
	m := newReadMarket(t)
	m.leader("LDRA", new(102.0), new(103.0))
	h := m.held("HLDA", 101, 98)
	r := m.read(m.at(3))
	if l := r.Leaders.Rows[0]; !nearPtr(l.ChangePct, 3) || l.ChangeUnavailable != "" || !nearPtr(l.GapPct, 2) {
		t.Fatalf("leader change in the session: %+v", l)
	}
	if row := heldRow(t, r, "HLDA"); !nearPtr(row.ChangePct, -2) {
		t.Fatalf("held change in the session: %+v", row)
	}
	// Before the open: a pre-market trade of today is the indicated gap; the
	// prior session's price is no change.
	pre := m.session.Open.Add(-30 * time.Minute)
	m.src.Leaders[0].Quote.TradeAt = m.session.Open.Add(-time.Hour)
	h.Quote.Last, h.Quote.TradeAt = 100, time.Time{}
	r = m.read(pre)
	if l := r.Leaders.Rows[0]; !nearPtr(l.ChangePct, 3) || l.IndicatedGapPct == nil || *l.ChangePct != *l.IndicatedGapPct {
		t.Fatalf("leader change before the open: %+v", l)
	}
	if row := heldRow(t, r, "HLDA"); row.ChangePct != nil || row.ChangeUnavailable == "" {
		t.Fatalf("prior close read as a change before the open: %+v", row)
	}
	// After the close: the session's close against the prior close.
	sessionDay, _ := time.Parse(time.DateOnly, m.session.Date)
	h.Daily.Points = append(h.Daily.Points, rpc.MarketHistoryPoint{At: sessionDay, Value: 105, Open: new(101.0), High: new(106.0), Low: new(100.0)})
	r = m.read(m.session.Close.Add(time.Hour))
	if row := heldRow(t, r, "HLDA"); !nearPtr(row.ChangePct, 5) {
		t.Fatalf("held change after the close: %+v", row)
	}
	if l := r.Leaders.Rows[0]; l.ChangePct != nil || l.ChangeUnavailable == "" {
		t.Fatalf("leader without its closing bar: %+v", l)
	}
}

func nearPtr(p *float64, want float64) bool { return p != nil && math.Abs(*p-want) < 1e-9 }
