package chainrec

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
)

type fakeClock struct {
	mu    sync.Mutex
	now   time.Time
	slept []time.Duration
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.slept = append(c.slept, d)
	c.mu.Unlock()
	return ctx.Err()
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func (c *fakeClock) sleeps() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.slept)
}

// fakeBroker scripts the gateway. Quote answers come from quoteFn, which
// sees the call number; Load answers from loadFn.
type fakeBroker struct {
	mu        sync.Mutex
	clock     *fakeClock
	held      []string
	heldErr   error
	listed    map[string][]ListedExpiry
	spot      map[string]float64
	notListed map[string]bool
	quoteCost time.Duration
	quoteFn   func(c Contract, call int) (Quote, error)
	loadFn    func(call int) (Load, bool)
	quotes    []Contract
	loads     int
	prepared  []string
}

func (b *fakeBroker) HeldUnderlyings(context.Context) ([]string, error) { return b.held, b.heldErr }

func (b *fakeBroker) ListedExpiries(_ context.Context, symbol string) ([]ListedExpiry, error) {
	l, ok := b.listed[symbol]
	if !ok {
		return nil, errors.New("security definition farm unavailable")
	}
	return l, nil
}

func (b *fakeBroker) Spot(_ context.Context, symbol string) (Spot, error) {
	px, ok := b.spot[symbol]
	if !ok {
		return Spot{}, errors.New("no price")
	}
	return Spot{Price: px, Source: "last", DataType: "live", AsOf: b.clock.Now()}, nil
}

func (b *fakeBroker) Prepare(_ context.Context, symbol, class string, expiries []string) ListedFilter {
	b.mu.Lock()
	b.prepared = append(b.prepared, fmt.Sprintf("%s/%s/%v", symbol, class, expiries))
	b.mu.Unlock()
	return func(expiry string, strike float64, right string) (bool, bool) {
		return !b.notListed[fmt.Sprintf("%s|%g|%s", expiry, strike, right)], true
	}
}

func (b *fakeBroker) Load() (Load, bool) {
	b.mu.Lock()
	b.loads++
	n := b.loads
	b.mu.Unlock()
	if b.loadFn == nil {
		return Load{LinesHeld: 10, LinesCapacity: 100}, true
	}
	return b.loadFn(n)
}

func (b *fakeBroker) Quote(ctx context.Context, c Contract, _ time.Duration) (Quote, error) {
	b.mu.Lock()
	b.quotes = append(b.quotes, c)
	n := len(b.quotes)
	b.mu.Unlock()
	b.clock.advance(b.quoteCost)
	if b.quoteFn != nil {
		return b.quoteFn(c, n)
	}
	return liveQuote(b.clock.Now()), nil
}

func (b *fakeBroker) quoteCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.quotes)
}

func liveQuote(at time.Time) Quote {
	return Quote{Bid: new(1.10), Ask: new(1.20), BidSize: new(12.0), AskSize: new(9.0), Volume: new(int64(140)),
		OpenInterest: new(int64(2300)), IV: new(0.18), Delta: new(0.42), Gamma: new(0.01), Vega: new(0.3), Theta: new(-0.05),
		UnderlyingPrice: new(100.0), ModelSource: "live", Feed: "live", ConID: 7001, RequestedAt: at, ObservedAt: at, QuoteReceivedAt: at}
}

// memSink keeps a run in memory.
type memSink struct {
	mu      sync.Mutex
	start   RunStart
	symbols []SymbolPlan
	rows    []Row
	outcome *Outcome
}

func (m *memSink) BeginRun(_ context.Context, start RunStart) (int64, error) {
	m.start = start
	return 1, nil
}

func (m *memSink) RecordSymbol(_ context.Context, _ int64, plan SymbolPlan) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.symbols = append(m.symbols, plan)
	return nil
}

func (m *memSink) RecordRows(_ context.Context, _ int64, rows []Row) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows = append(m.rows, rows...)
	return nil
}

func (m *memSink) FinishRun(_ context.Context, _ int64, o Outcome) error {
	m.outcome = &o
	return nil
}

func (m *memSink) statuses() map[string]int {
	out := map[string]int{}
	for _, r := range m.rows {
		out[r.Status+"/"+r.Detail]++
	}
	return out
}

// testSlot is the 2026-10-14 session's slot, started on time.
func testSlot(t *testing.T) Slot {
	return Slot{SessionDate: "2026-10-14", Close: ny(t, "2026-10-14 16:00"), ScheduledAt: ny(t, "2026-10-14 15:30"), Deadline: ny(t, "2026-10-14 16:00")}
}

// newFake lists one synthetic underlying per symbol with daily expiries and
// one-point strikes around 100.
func newFake(t *testing.T, symbols ...string) (*fakeBroker, *fakeClock) {
	clock := &fakeClock{now: ny(t, "2026-10-14 15:30")}
	b := &fakeBroker{clock: clock, listed: map[string][]ListedExpiry{}, spot: map[string]float64{}, notListed: map[string]bool{}}
	for _, s := range symbols {
		b.listed[s] = listing(s, strikesRange(60, 140, 1), "2026-10-16", "2026-10-23", "2026-11-20")
		b.spot[s] = 100
	}
	return b, clock
}

func runWith(t *testing.T, b *fakeBroker, clock *fakeClock, sink Sink, lines int, symbols ...string) Outcome {
	t.Helper()
	r := &Runner{Broker: b, Sink: sink, Clock: clock, Days: days(), Lines: lines}
	out, err := r.Run(context.Background(), testSlot(t), Settings{Enabled: true, Symbols: symbols})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return out
}

func TestRunRecordsEveryPlannedContract(t *testing.T) {
	b, clock := newFake(t, "SYNA", "SYNB")
	b.held = []string{"SYNB"}
	b.notListed["2026-10-23|100|P"] = true
	sink := &memSink{}
	out := runWith(t, b, clock, sink, 1, "SYNA")

	// 3 expiries × 40 strikes × 2 rights per symbol, less each symbol's
	// unlisted put.
	if out.Status != RunComplete || out.Planned != 2*(3*40*2-1) || out.Attempted != out.Planned || out.Quoted != out.Planned {
		t.Fatalf("outcome = %+v", out)
	}
	if len(sink.rows) != out.Planned || sink.outcome == nil || sink.outcome.Status != RunComplete {
		t.Fatalf("stored %d rows, outcome %+v", len(sink.rows), sink.outcome)
	}
	// Held option underlyings are recorded before the research list.
	if len(sink.symbols) != 2 || sink.symbols[0].Source != SourceHeld || sink.symbols[1].Source != SourceExplicit ||
		sink.symbols[0].Planned != 239 || len(sink.symbols[0].Expiries) != 3 || sink.symbols[0].DiscoveredAt.IsZero() {
		t.Fatalf("symbols = %+v", sink.symbols)
	}
	for _, r := range sink.rows {
		if r.Expiry == "2026-10-23" && r.Strike == 100 && r.Right == "P" {
			t.Fatal("an unlisted contract was requested")
		}
	}
	// Work is ordered at-the-money first across both symbols: five
	// at-the-money contracts each before any other strike.
	first := b.quotes[:10]
	for _, c := range first {
		if c.Strike != 100 {
			t.Fatalf("first requests %v are not at the money", first)
		}
	}
	if !slices.ContainsFunc(first, func(c Contract) bool { return c.Symbol == "SYNB" }) {
		t.Fatalf("first requests %v do not interleave symbols", first)
	}
}

// Pacing signals back off 5 s, 10 s, ... and a success resets the count.
func TestRunBacksOffOnPacingAndRecovers(t *testing.T) {
	b, clock := newFake(t, "SYNA")
	b.quoteFn = func(_ Contract, call int) (Quote, error) {
		if call <= 2 {
			return Quote{}, Fail(StatusPacing, errors.New("max rate of messages exceeded"))
		}
		return liveQuote(clock.Now()), nil
	}
	sink := &memSink{}
	out := runWith(t, b, clock, sink, 1, "SYNA")
	if out.Status != RunComplete || out.Attempted != out.Planned || sink.statuses()[StatusPacing+"/pacing: max rate of messages exceeded"] != 2 {
		t.Fatalf("outcome %+v statuses %v", out, sink.statuses())
	}
	if got := clock.sleeps(); !slices.Equal(got, []time.Duration{5 * time.Second, 10 * time.Second}) {
		t.Fatalf("backoff sleeps = %v, want 5s then 10s", got)
	}
}

// Consecutive pacing signals stop the run; nothing is retried and the rest
// is recorded as not attempted.
func TestRunStopsAfterRepeatedPacing(t *testing.T) {
	b, clock := newFake(t, "SYNA")
	b.quoteFn = func(Contract, int) (Quote, error) { return Quote{}, Fail(StatusPacing, errors.New("pacing")) }
	sink := &memSink{}
	out := runWith(t, b, clock, sink, 1, "SYNA")
	if out.Status != RunPartial || out.StopReason != StopPacing || out.Attempted != MaxPacingSignals || b.quoteCount() != MaxPacingSignals {
		t.Fatalf("outcome = %+v after %d requests", out, b.quoteCount())
	}
	if n := sink.statuses()[StatusNotAttempted+"/"+StopPacing]; n != out.Planned-MaxPacingSignals {
		t.Fatalf("not attempted = %d, want %d", n, out.Planned-MaxPacingSignals)
	}
	if got := clock.sleeps(); !slices.Equal(got, []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second}) {
		t.Fatalf("backoff sleeps = %v", got)
	}
}

// A pacing error caused by anyone on the connection pauses the recorder
// before its next request.
func TestRunYieldsToConnectionPacingAndLineReserve(t *testing.T) {
	b, clock := newFake(t, "SYNA")
	errAt := clock.Now().Add(-time.Hour)
	b.loadFn = func(call int) (Load, bool) {
		switch {
		case call == 1:
			return Load{LinesHeld: 10, LinesCapacity: 100, LastPacingError: errAt}, true // before the run: ignored
		case call == 2:
			return Load{LinesHeld: 10, LinesCapacity: 100, LastPacingError: errAt.Add(time.Hour)}, true // new error
		case call <= 5:
			return Load{LinesHeld: 85, LinesCapacity: 100, LastPacingError: errAt.Add(time.Hour)}, true // 15 free ≤ reserve
		default:
			return Load{LinesHeld: 20, LinesCapacity: 100, LastPacingError: errAt.Add(time.Hour)}, true
		}
	}
	out := runWith(t, b, clock, &memSink{}, 1, "SYNA")
	if out.Status != RunComplete {
		t.Fatalf("outcome = %+v", out)
	}
	if got := clock.sleeps(); !slices.Equal(got, []time.Duration{5 * time.Second, time.Second, time.Second}) {
		t.Fatalf("sleeps = %v, want a 5s pacing pause (covering the first line wait) then two 1s line waits", got)
	}
}

// The close bounds a run: what is not reached is recorded, never chased.
func TestRunStopsAtDeadline(t *testing.T) {
	b, clock := newFake(t, "SYNA")
	b.quoteCost = 20 * time.Second
	sink := &memSink{}
	out := runWith(t, b, clock, sink, 1, "SYNA")
	if out.Status != RunPartial || out.StopReason != StopDeadline || out.Attempted != 90 {
		t.Fatalf("outcome = %+v, want 90 contracts in 30 minutes", out)
	}
	if n := sink.statuses()[StatusNotAttempted+"/"+StopDeadline]; n != out.Planned-90 {
		t.Fatalf("not attempted = %d", n)
	}
}

func TestRunStopsWhenGatewayDrops(t *testing.T) {
	b, clock := newFake(t, "SYNA")
	b.quoteFn = func(_ Contract, call int) (Quote, error) {
		if call == 3 {
			return Quote{}, Fail(StatusUnavailable, errors.New("not connected"))
		}
		return liveQuote(clock.Now()), nil
	}
	out := runWith(t, b, clock, &memSink{}, 1, "SYNA")
	if out.Status != RunPartial || out.StopReason != StopGateway || out.Attempted != 3 || out.Quoted != 2 {
		t.Fatalf("outcome = %+v", out)
	}
}

// A symbol without data entitlement stops asking after a few refusals; the
// other symbols are unaffected.
func TestRunGivesUpOnUnentitledSymbol(t *testing.T) {
	b, clock := newFake(t, "SYNA", "SYNB")
	b.quoteFn = func(c Contract, _ int) (Quote, error) {
		if c.Symbol == "SYNB" {
			return Quote{}, Fail(StatusEntitlement, errors.New("354 not subscribed"))
		}
		return liveQuote(clock.Now()), nil
	}
	sink := &memSink{}
	out := runWith(t, b, clock, sink, 1, "SYNA", "SYNB")
	st := sink.statuses()
	if out.Status != RunPartial || st[StatusEntitlement+"/no_entitlement: 354 not subscribed"] != EntitlementGiveUp ||
		st[StatusNotAttempted+"/"+StopSymbolNoEntitle] != 240-EntitlementGiveUp || out.Quoted != 240 {
		t.Fatalf("outcome %+v statuses %v", out, st)
	}
}

func TestRunRecordsDiscoveryFailuresAndSkippedSymbols(t *testing.T) {
	b, clock := newFake(t, "SYNA", "SYNC")
	delete(b.spot, "SYNC")
	b.heldErr = errors.New("portfolio stream down")
	sink := &memSink{}
	symbols := []string{"SYNA", "SYNB", "SYNC", "SYND", "SYNE", "SYNF", "SYNG", "SYNH", "SYNI", "SYNJ", "SYNK", "SYNL", "SYNM"}
	out := runWith(t, b, clock, sink, 0, symbols...)
	if !out.HeldUnavailable || out.Status != RunComplete || out.Planned != 240 {
		t.Fatalf("outcome = %+v", out)
	}
	byStatus := map[string][]string{}
	for _, s := range sink.symbols {
		byStatus[s.Status] = append(byStatus[s.Status], s.Symbol)
	}
	if !slices.Equal(byStatus[SymbolSkippedCap], []string{"SYNM"}) || !slices.Equal(byStatus[SymbolSpotUnavailable], []string{"SYNC"}) ||
		len(byStatus[SymbolChainUnavailable]) != 10 || !slices.Equal(byStatus[SymbolPlanned], []string{"SYNA"}) {
		t.Fatalf("symbol statuses = %v", byStatus)
	}
}

// A run persisted to the store reads back with its coverage, pages its rows
// and keeps every table append-only.
func TestStorePersistsAndReadsBack(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state", StoreFile)
	if s, err := OpenExisting(ctx, path); s != nil || err != nil {
		t.Fatalf("OpenExisting on a missing file = %v, %v; want nothing created", s, err)
	}
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	b, clock := newFake(t, "SYNA", "SYNB")
	b.quoteFn = func(c Contract, call int) (Quote, error) {
		if c.Symbol == "SYNB" && c.Right == "P" {
			return Quote{IV: new(0.2), Delta: new(-0.3), RequestedAt: clock.Now(), ObservedAt: clock.Now()}, nil
		}
		return liveQuote(clock.Now()), nil
	}
	out := runWith(t, b, clock, store, 0, "SYNA", "SYNB")
	if out.Status != RunComplete {
		t.Fatalf("outcome = %+v", out)
	}
	if has, err := store.HasRun(ctx, "2026-10-14"); err != nil || !has {
		t.Fatalf("HasRun = %v, %v", has, err)
	}
	runs, err := store.Runs(ctx, rpc.ChainsRecordedParams{}, 0)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %v, %v", runs, err)
	}
	run := runs[0]
	c := run.Coverage
	if run.Status != RunComplete || c.Planned != 480 || c.Attempted != 480 || c.Quoted != 360 || c.TwoSided != 360 ||
		c.WithIV != 480 || c.WithGreeks != 480 || c.WithOI != 360 || c.ByStatus[StatusModelOnly] != 120 || len(run.Symbols) != 2 {
		t.Fatalf("run = %+v coverage %+v", run, c)
	}
	if s := run.Symbols[1]; s.Symbol != "SYNB" || s.Spot != 100 || len(s.Expiries) != 3 || s.Expiries[0].Selected != 40 || s.Coverage.Quoted != 120 {
		t.Fatalf("symbol = %+v", s)
	}
	only, err := store.Runs(ctx, rpc.ChainsRecordedParams{Symbol: "SYNB", Date: "2026-10-14"}, 0)
	if err != nil || len(only) != 1 || len(only[0].Symbols) != 1 || only[0].Coverage.Planned != 240 {
		t.Fatalf("filtered runs = %+v, %v", only, err)
	}
	if none, _ := store.Runs(ctx, rpc.ChainsRecordedParams{Date: "2026-10-15"}, 0); len(none) != 0 {
		t.Fatalf("other date returned %v", none)
	}

	var all []rpc.ChainRecordedRow
	cursor := ""
	for pages := 0; ; pages++ {
		page, next, err := store.Rows(ctx, rpc.ChainsRecordedRowsParams{RunID: run.RunID, After: cursor, Limit: 200})
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, page...)
		if next == "" {
			if pages != 2 {
				t.Fatalf("pages = %d, want 3 pages of at most 200", pages+1)
			}
			break
		}
		cursor = next
	}
	if len(all) != 480 {
		t.Fatalf("paged %d rows", len(all))
	}
	r := all[0]
	if r.SessionDate != "2026-10-14" || r.Bid == nil || *r.Bid != 1.10 || r.OpenInterest == nil || *r.OpenInterest != 2300 ||
		r.ConID != 7001 || r.ObservedAt.IsZero() || !r.LastTradeAt.IsZero() {
		t.Fatalf("first row = %+v", r)
	}
	quotes, err := store.TwoSidedQuotes(ctx, "2026-10-01", "2026-10-31", []string{"SYNB"})
	if err != nil || len(quotes) != 120 || quotes[0].SessionDate != "2026-10-14" || quotes[0].Symbol != "SYNB" || quotes[0].Right != "C" || quotes[0].Bid <= 0 || quotes[0].Ask < quotes[0].Bid {
		t.Fatalf("two-sided SYNB quotes = %d (%+v), %v; want the 120 quoted calls", len(quotes), quotes[:min(1, len(quotes))], err)
	}
	if outside, err := store.TwoSidedQuotes(ctx, "2026-10-15", "2026-10-31", []string{"SYNA", "SYNB"}); err != nil || len(outside) != 0 {
		t.Fatalf("quotes after the session = %d, %v", len(outside), err)
	}
	for _, stmt := range []string{`UPDATE quotes SET bid = 0`, `DELETE FROM runs`, `DELETE FROM run_outcomes`, `UPDATE run_symbols SET spot = 0`} {
		if _, err := store.db.ExecContext(ctx, stmt); err == nil {
			t.Fatalf("%s succeeded on an append-only table", stmt)
		}
	}
}

// A run without an outcome reads as running while this process executes it
// and as interrupted afterwards, with its unrecorded contracts missing.
func TestStoreInterruptedRun(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), StoreFile))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	slot := testSlot(t)
	id, err := store.BeginRun(ctx, RunStart{Slot: slot, StartedAt: slot.ScheduledAt, Settings: Settings{Symbols: DefaultSymbols}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSymbol(ctx, id, SymbolPlan{Symbol: "SYNA", Source: SourceHeld, Status: SymbolPlanned, Planned: 10, DiscoveredAt: slot.ScheduledAt}); err != nil {
		t.Fatal(err)
	}
	q := liveQuote(slot.ScheduledAt)
	if err := store.RecordRows(ctx, id, []Row{{Symbol: "SYNA", TradingClass: "SYNA", Expiry: "2026-11-20", Slot: SlotMonthly1, Strike: 100, Right: "C", Status: StatusQuoted, Quote: q}}); err != nil {
		t.Fatal(err)
	}
	runs, err := store.Runs(ctx, rpc.ChainsRecordedParams{}, id)
	if err != nil || runs[0].Status != RunRunning {
		t.Fatalf("active run = %+v, %v", runs, err)
	}
	runs, _ = store.Runs(ctx, rpc.ChainsRecordedParams{}, 0)
	if c := runs[0].Coverage; runs[0].Status != RunInterrupted || c.Planned != 10 || c.Quoted != 1 || c.NotAttempted != 9 {
		t.Fatalf("interrupted run = %+v", runs[0])
	}
	stats, err := store.Stats(ctx)
	if err != nil || stats.Runs != 1 || stats.Rows != 1 || stats.Bytes == 0 {
		t.Fatalf("stats = %+v, %v", stats, err)
	}
}
