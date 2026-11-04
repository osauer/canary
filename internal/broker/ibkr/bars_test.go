package ibkr

import (
	"context"
	"errors"
	"testing"
	"time"

	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

type delayedChartFake struct {
	delayedBars []ibkrlib.HistoricalBar
	delayedErr  error
	leaseErr    error
	calls       int
	leased      int
	released    int
	inLease     bool
}

func (f *delayedChartFake) FetchChartBars(context.Context, ibkrlib.Contract, int, string, time.Duration) (ibkrlib.ChartSeries, error) {
	f.calls++
	if !f.inLease {
		return ibkrlib.ChartSeries{}, errors.New("read outside the delayed lease")
	}
	return ibkrlib.ChartSeries{Bars: f.delayedBars}, f.delayedErr
}

func (f *delayedChartFake) BeginDelayedHistoricalData(context.Context) (func(), error) {
	if f.leaseErr != nil {
		return nil, f.leaseErr
	}
	f.leased++
	f.inLease = true
	return func() { f.released++; f.inLease = false }, nil
}

// A history read IBKR refuses for want of a market data permission is read
// again under a delayed lease, which is released whatever the answer; a
// refusal of the delayed read too, or no lease, returns the original refusal
// (owner, 2026-10-31: no Xetra subscription). Moved from the daemon with the
// retry in WP2; the assertions are the daemon's.
func TestHistoryRefusedForPermissionReadsDelayed(t *testing.T) {
	refused := &ibkrlib.HistoricalRequestError{Code: 162, Message: "Historical Market Data Service error message:No market data permissions for IBIS2 STK"}
	if !ibkrlib.HistoricalNotEntitled(refused) {
		t.Fatal("the fixture refusal is not an entitlement refusal")
	}
	contract := ibkrlib.Contract{Symbol: "MFEH", SecType: "STK", Exchange: "IBIS2", Currency: "EUR"}
	var logged []string
	logf := func(format string, args ...any) { logged = append(logged, format) }

	f := &delayedChartFake{delayedBars: []ibkrlib.HistoricalBar{{Close: 101.5}, {Close: 102}}}
	series, err := fetchChartBarsDelayed(t.Context(), f, contract, 30, "1 day", time.Second, refused, logf)
	if err != nil || len(series.Bars) != 2 || f.leased != 1 || f.released != 1 || f.calls != 1 || len(logged) != 1 {
		t.Fatalf("delayed read: %+v %v, fake %+v, logged %d", series, err, f, len(logged))
	}

	f = &delayedChartFake{delayedErr: &ibkrlib.HistoricalRequestError{Code: 162, Message: "No market data permissions for IBIS2 STK"}}
	if _, err := fetchChartBarsDelayed(t.Context(), f, contract, 30, "1 day", time.Second, refused, logf); err != refused || f.released != 1 {
		t.Fatalf("delayed read refused too: %v, fake %+v, want the original refusal and the lease released", err, f)
	}

	f = &delayedChartFake{leaseErr: errors.New("IBKR connection not available")}
	if _, err := fetchChartBarsDelayed(t.Context(), f, contract, 30, "1 day", time.Second, refused, logf); err != refused || f.calls != 0 {
		t.Fatalf("no lease: %v, calls %d, want the original refusal and no read", err, f.calls)
	}

	other := errors.New("broker session changed")
	f = &delayedChartFake{delayedErr: other}
	if _, err := fetchChartBarsDelayed(t.Context(), f, contract, 30, "1 day", time.Second, refused, nil); !errors.Is(err, other) || f.released != 1 {
		t.Fatalf("another failure of the delayed read: %v, want it named", err)
	}
}
