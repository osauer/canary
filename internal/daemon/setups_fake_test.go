package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkr "github.com/osauer/canary/v2/pkg/ibkr"
)

// setupTestClock is a settable daemon clock shared with request goroutines.
type setupTestClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *setupTestClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *setupTestClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func setupNY(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

type setupFetchCall struct {
	symbol     string
	start, end time.Time
}

// fakeSetupSource serves synthetic five-minute regular-session bars for every
// weekday slot between 09:30 and 16:00 New York time. Holidays and early
// closes are left to the session windows, which never attach bars outside a
// listed session.
type fakeSetupSource struct {
	mu      sync.Mutex
	calls   []setupFetchCall
	stale   bool
	hold    map[string]chan struct{}
	entered chan string
	// gaps removes the 10:00 bar from these session dates.
	gaps map[string]bool
	// fail returns this error for every read of the named symbol.
	fail map[string]error
	// volume overrides the synthetic volume of the bar starting at a time.
	volume map[time.Time]int64
}

func newFakeSetupSource() *fakeSetupSource {
	return &fakeSetupSource{hold: map[string]chan struct{}{}, entered: make(chan string, 256), gaps: map[string]bool{}, fail: map[string]error{}, volume: map[time.Time]int64{}}
}

// holdSymbol blocks every read for symbol until the returned func runs.
func (f *fakeSetupSource) holdSymbol(symbol string) func() {
	ch := make(chan struct{})
	f.mu.Lock()
	f.hold[symbol] = ch
	f.mu.Unlock()
	return sync.OnceFunc(func() { close(ch) })
}

func (f *fakeSetupSource) setGap(date string, gap bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if gap {
		f.gaps[date] = true
	} else {
		delete(f.gaps, date)
	}
}

func (f *fakeSetupSource) setVolume(start time.Time, volume int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.volume[start.UTC()] = volume
}

func (f *fakeSetupSource) setFail(symbol string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		delete(f.fail, symbol)
	} else {
		f.fail[symbol] = err
	}
}

func (f *fakeSetupSource) CaptureHistoricalSession() (ibkr.HistoricalSessionBinding, bool) {
	return ibkr.HistoricalSessionBinding{}, true
}

func (f *fakeSetupSource) HistoricalSessionCurrent(ibkr.HistoricalSessionBinding) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.stale
}

func (f *fakeSetupSource) ResolveOrderContractForSession(_ context.Context, _ ibkr.ConnectorSessionBinding, c ibkr.Contract, _ time.Duration) (ibkr.ResolvedOrderContract, error) {
	conID := c.ConID
	if conID == 0 {
		for _, r := range c.Symbol {
			conID = conID*31 + int(r)
		}
		conID = conID%900000 + 1000
	}
	return ibkr.ResolvedOrderContract{Contract: ibkr.Contract{ConID: conID, Symbol: c.Symbol, SecType: "STK", Exchange: "SMART", PrimaryExch: "NASDAQ", Currency: "USD"}}, nil
}

func (f *fakeSetupSource) FetchSetupBars(ctx context.Context, c ibkr.Contract, start, end time.Time, _ time.Duration) ([]ibkr.HistoricalBar, error) {
	f.mu.Lock()
	f.calls = append(f.calls, setupFetchCall{symbol: c.Symbol, start: start, end: end})
	hold, fail := f.hold[c.Symbol], f.fail[c.Symbol]
	gaps, volume := maps.Clone(f.gaps), maps.Clone(f.volume)
	f.mu.Unlock()
	if hold != nil {
		f.entered <- c.Symbol
		select {
		case <-hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if fail != nil {
		return nil, fail
	}
	if end.Sub(start) > 7*24*time.Hour || !end.After(start) {
		return nil, errors.New("unbounded synthetic read")
	}
	loc, _ := time.LoadLocation("America/New_York")
	var out []ibkr.HistoricalBar
	for at := start.Truncate(5 * time.Minute); at.Before(end); at = at.Add(5 * time.Minute) {
		local := at.In(loc)
		minute := local.Hour()*60 + local.Minute()
		if at.Before(start) || local.Weekday() == time.Saturday || local.Weekday() == time.Sunday || minute < 9*60+30 || minute >= 16*60 {
			continue
		}
		if gaps[local.Format("2006-01-02")] && minute == 10*60 {
			continue
		}
		v, ok := volume[at.UTC()]
		if !ok {
			v = 100
		}
		out = append(out, ibkr.HistoricalBar{Time: at, Open: 100, High: 101, Low: 99, Close: 100, Volume: v})
	}
	return out, nil
}

// isBaselineRead reports a prior-session read: those end at a 16:00 New York
// close, while current-session reads end at the decision clock.
func isBaselineRead(call setupFetchCall) bool {
	loc, _ := time.LoadLocation("America/New_York")
	end := call.end.In(loc)
	return end.Hour() == 16 && end.Minute() == 0 && end.Second() == 0
}

// reads counts the recorded baseline and current-session reads for symbol.
func (f *fakeSetupSource) reads(symbol string) (baseline, current int) {
	for _, call := range f.symbolReads(symbol) {
		if isBaselineRead(call) {
			baseline++
		} else {
			current++
		}
	}
	return baseline, current
}

// baselineReads returns the recorded baseline reads for symbol in order.
func (f *fakeSetupSource) baselineReads(symbol string) []setupFetchCall {
	var out []setupFetchCall
	for _, call := range f.symbolReads(symbol) {
		if isBaselineRead(call) {
			out = append(out, call)
		}
	}
	return out
}

func (f *fakeSetupSource) symbolReads(symbol string) []setupFetchCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []setupFetchCall
	for _, call := range f.calls {
		if call.symbol == symbol {
			out = append(out, call)
		}
	}
	return out
}

func setupTestServer(src *fakeSetupSource, clock *setupTestClock) *Server {
	return &Server{setupSourceForTest: src, now: clock.now}
}

func evaluateSetupForTest(ctx context.Context, s *Server, symbol string, conID int, at time.Time) (*rpc.SetupResult, error) {
	raw, err := json.Marshal(rpc.SetupEvaluateParams{Spec: rpc.SetupSpec{Revision: "test-v1"}, Contract: rpc.ContractParams{Symbol: symbol, ConID: conID}, At: at})
	if err != nil {
		return nil, err
	}
	return s.handleSetupsEvaluate(ctx, &rpc.Request{Method: rpc.MethodSetupsEvaluate, Params: raw})
}

func mustEvaluateSetup(t *testing.T, s *Server, symbol string, at time.Time) *rpc.SetupResult {
	t.Helper()
	r, err := evaluateSetupForTest(t.Context(), s, symbol, 0, at)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
