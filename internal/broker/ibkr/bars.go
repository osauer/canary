package ibkr

import (
	"context"
	"time"

	"github.com/osauer/canary/v2/internal/broker"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// Bar maps one historical bar.
func Bar(b ibkrlib.HistoricalBar) broker.Bar {
	return broker.Bar{Time: b.Time, Date: b.Date, Open: b.Open, High: b.High, Low: b.Low, Close: b.Close, Volume: b.Volume, Average: b.Average, BarCount: b.BarCount}
}

// Bars maps a slice of historical bars; nil stays nil.
func Bars(in []ibkrlib.HistoricalBar) []broker.Bar {
	if in == nil {
		return nil
	}
	out := make([]broker.Bar, len(in))
	for i, b := range in {
		out[i] = Bar(b)
	}
	return out
}

// Series maps a chart series.
func Series(s ibkrlib.ChartSeries) broker.BarSeries {
	return broker.BarSeries{Instrument: Instrument(s.Contract), Bars: Bars(s.Bars), WhatToShow: s.WhatToShow}
}

// chartReader is the connector as the delayed history retry uses it; the
// retry is tested against a fake of it.
type chartReader interface {
	FetchChartBars(ctx context.Context, contract ibkrlib.Contract, days int, interval string, timeout time.Duration) (ibkrlib.ChartSeries, error)
	BeginDelayedHistoricalData(ctx context.Context) (func(), error)
}

// ChartBars reads a chart window through exact session-bound resolution.
// A read IBKR refuses for want of a market data permission is repeated in
// delayed mode, where IBKR serves the exchange's bars to an account without
// its subscription (owner, 2026-10-31: no Xetra subscription; the trend
// fund's line and sizing read its delayed closes). The retry runs only while
// the session that refused is still current; a changed session returns the
// unavailable error as the daemon did before the retry moved here.
func (a *Adapter) ChartBars(ctx context.Context, in broker.Instrument, days int, interval string, timeout time.Duration) (broker.BarSeries, error) {
	if a == nil || a.conn == nil {
		return broker.BarSeries{}, ibkrlib.ErrIBKRUnavailable
	}
	contract := Contract(in)
	cb, ok := a.conn.CaptureHistoricalSession()
	if !ok {
		return broker.BarSeries{}, ibkrlib.ErrIBKRUnavailable
	}
	series, err := a.conn.FetchChartBars(ctx, contract, days, interval, timeout)
	if ibkrlib.HistoricalNotEntitled(err) && a.conn.HistoricalSessionCurrent(cb) {
		series, err = fetchChartBarsDelayed(ctx, a.conn, contract, days, interval, timeout, err, a.logfSafe)
	}
	return Series(series), err
}

// fetchChartBarsDelayed repeats a refused history read in delayed mode. When
// the delayed read is refused too, the original refusal is returned, so the
// entitlement handling of every caller stands; another failure of the
// delayed read is its own.
func fetchChartBarsDelayed(ctx context.Context, c chartReader, contract ibkrlib.Contract, days int, interval string, timeout time.Duration, refused error, logf func(string, ...any)) (ibkrlib.ChartSeries, error) {
	release, err := c.BeginDelayedHistoricalData(ctx)
	if err != nil {
		return ibkrlib.ChartSeries{}, refused
	}
	defer release()
	series, err := c.FetchChartBars(ctx, contract, days, interval, timeout)
	switch {
	case ibkrlib.HistoricalNotEntitled(err):
		return ibkrlib.ChartSeries{}, refused
	case err != nil:
		return ibkrlib.ChartSeries{}, err
	}
	if logf != nil {
		logf("history %s %s: IBKR has no live market data permission for it; %d delayed bars read instead", contract.Symbol, contract.Exchange, len(series.Bars))
	}
	return series, nil
}

// SessionBars reads five-minute regular-session bars between two instants.
func (a *Adapter) SessionBars(ctx context.Context, in broker.Instrument, from, to time.Time, timeout time.Duration) ([]broker.Bar, error) {
	if a == nil || a.conn == nil {
		return nil, ibkrlib.ErrIBKRUnavailable
	}
	bars, err := a.conn.FetchSetupBars(ctx, Contract(in), from, to, timeout)
	return Bars(bars), err
}
