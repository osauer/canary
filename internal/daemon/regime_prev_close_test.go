package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/broker"
	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

// prevCloseOnlyDeps wires the regime fetchers to the production price ladder
// over a look in which the named symbols deliver nothing but the close tick
// (the previous regular session's close) on a live feed, and every other
// symbol delivers a current print.
func prevCloseOnlyDeps(now time.Time, closeOnly ...string) *regimeDeps {
	stale := map[string]bool{}
	for _, sym := range closeOnly {
		stale[sym] = true
	}
	look := func(_ context.Context, symbol string, _ []string, _ time.Duration, done func(broker.BriefQuote) bool) (broker.BriefQuote, error) {
		q := broker.BriefQuote{Close: 100, FeedType: 1, ObservedAt: now}
		if !stale[symbol] {
			q.Last, q.Week52High = 101, 110
		}
		done(q)
		return q, nil
	}
	bars := make([]ibkrlib.HistoricalBar, 0, 60)
	for i := range 60 {
		d := now.AddDate(0, 0, i-59)
		bars = append(bars, ibkrlib.HistoricalBar{Date: d.Format("2006-01-02"), Time: d, Close: 100, High: 110, Low: 90})
	}
	return &regimeDeps{
		snapshot: func(ctx context.Context, sym string, timeout time.Duration) snapshotQuote {
			return priceWithClose(briefTicks(ctx, look, sym, timeout, nil))
		},
		snapshotWith52WHigh: func(ctx context.Context, sym string, timeout time.Duration) snapshotQuote {
			return briefWith52WHigh(ctx, look, sym, timeout, nil)
		},
		history:  func(context.Context, string, int) ([]ibkrlib.HistoricalBar, error) { return bars, nil },
		logWarnf: func(string, ...any) {},
		now:      func() time.Time { return now },
	}
}

func regimeRTHInstant(t *testing.T) time.Time {
	t.Helper()
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load NY location: %v", err)
	}
	return time.Date(2026, 11, 18, 11, 0, 0, 0, ny)
}

// A price that is only the previous session's close says so, whatever the
// feed it arrived on.
func TestSnapshotLadderLabelsThePreviousClose(t *testing.T) {
	q := priceWithClose(snapshotTicks{closePx: 100, dataType: rpc.MarketDataLive})
	if q.price != 100 || q.dataType != rpc.MarketDataPrevClose {
		t.Fatalf("priceWithClose close-only = price %v, data type %q; want 100, %q", q.price, q.dataType, rpc.MarketDataPrevClose)
	}
	now := regimeRTHInstant(t)
	deps := prevCloseOnlyDeps(now, "SPY")
	q = deps.snapshotWith52WHigh(t.Context(), "SPY", time.Second)
	if q.price != 100 || q.dataType != rpc.MarketDataPrevClose {
		t.Fatalf("briefWith52WHigh close-only = price %v, data type %q; want 100, %q", q.price, q.dataType, rpc.MarketDataPrevClose)
	}
	if q := priceWithClose(snapshotTicks{last: 101, closePx: 100, dataType: rpc.MarketDataLive}); q.dataType != rpc.MarketDataLive {
		t.Fatalf("a current print keeps its feed label, got %q", q.dataType)
	}
}

// No regime indicator counts the previous session's close as a live print.
func TestRegimeNeverReadsThePreviousCloseAsLive(t *testing.T) {
	now := regimeRTHInstant(t)
	ctx := t.Context()

	term := fetchRegimeVIXTerm(ctx, prevCloseOnlyDeps(now, "VIX3M"))
	if term.Status == rpc.RegimeStatusOK || rpc.IsLiveDataType(term.DataType) || term.VIX3MAnchorVIX != nil {
		t.Errorf("VIX3M at its previous close: status %q, data type %q, anchor %v; want a non-live, non-OK row without an anchor", term.Status, term.DataType, term.VIX3MAnchorVIX)
	}
	term = fetchRegimeVIXTerm(ctx, prevCloseOnlyDeps(now, "VIX"))
	if term.Status == rpc.RegimeStatusOK || term.VIXChangePct != nil {
		t.Errorf("VIX at its previous close: status %q, day change %v; want a non-OK row and no day change", term.Status, term.VIXChangePct)
	}

	credit := fetchRegimeHYGSPY(ctx, prevCloseOnlyDeps(now, "HYG"))
	if credit.Status == rpc.RegimeStatusOK {
		t.Errorf("HYG at its previous close: status %q, want not OK", credit.Status)
	}
	credit = fetchRegimeHYGSPY(ctx, prevCloseOnlyDeps(now, "SPY"))
	if credit.Status == rpc.RegimeStatusOK || credit.SPYChangePct != nil {
		t.Errorf("SPY at its previous close: status %q, day change %v; want a non-OK row and no day change", credit.Status, credit.SPYChangePct)
	}

	fx := fetchRegimeUSDJPY(ctx, prevCloseOnlyDeps(now, "USD.JPY"))
	if fx.Status == rpc.RegimeStatusOK {
		t.Errorf("USD/JPY at its previous close: status %q, want not OK", fx.Status)
	}

	// The control: with current prints everywhere the rows are live.
	live := prevCloseOnlyDeps(now)
	if got := fetchRegimeVIXTerm(ctx, live); got.Status != rpc.RegimeStatusOK {
		t.Errorf("VIX term on current prints: status %q, want ok", got.Status)
	}
	if got := fetchRegimeUSDJPY(ctx, live); got.Status != rpc.RegimeStatusOK {
		t.Errorf("USD/JPY on current prints: status %q, want ok", got.Status)
	}
}

// The as-of line names a previous close in plain words and ranks it with
// other old values.
func TestRegimeAsOfNamesThePreviousClose(t *testing.T) {
	now := regimeRTHInstant(t)
	got := gatewayAsOf(now, rpc.RegimeStatusStale, rpc.MarketDataPrevClose, "source")
	if got == nil || got.Label != "previous close" || got.Freshness != rpc.FreshnessFrozen {
		t.Fatalf("as-of for a previous close = %+v; want label %q, freshness %q", got, "previous close", rpc.FreshnessFrozen)
	}
}
