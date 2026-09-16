package daemon

import (
	"context"
	"encoding/json"
	"errors"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
	"math"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func (s *Server) handleMarketSnapshot(ctx context.Context, req *rpc.Request) (*rpc.MarketSnapshotResult, error) {
	connector := s.gatewayConnector()
	if connector == nil {
		return nil, s.gatewayUnavailableError()
	}
	binding, ok := connector.CaptureSession()
	if !ok {
		return nil, s.gatewayUnavailableError()
	}
	positions, err := s.handlePositionsList(ctx, &rpc.Request{Params: json.RawMessage(`{}`)})
	if err != nil {
		return nil, err
	}
	if positions.Authority == nil || positions.Authority.Availability != rpc.AccountDataAvailable || positions.Authority.Freshness != rpc.AccountDataFreshnessCurrent {
		return nil, errors.New("current holdings scope unavailable")
	}
	result := &rpc.MarketSnapshotResult{AsOf: time.Now(), Authority: positions.Authority, CoverageStatus: "complete"}
	result.Instruments = marketReferences()
	for _, g := range positions.ByUnderlying {
		if !rpc.ExpectsMarketDataGroup(g) {
			continue
		}
		contract, ok := rpc.UnderlyingMarketContract(g)
		if !ok {
			continue
		}
		if len(result.Underlyings) >= 12 {
			result.Truncated = true
			result.CoverageStatus = "partial"
			break
		}
		result.Underlyings = append(result.Underlyings, rpc.MarketInstrument{Key: g.Underlying, Name: g.Underlying, Underlying: g.Underlying, Kind: "underlying", Quote: &rpc.Quote{Contract: contract}})
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	fetch := func(item *rpc.MarketInstrument) {
		defer wg.Done()
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			item.Quote = nil
			item.Error = "request expired"
			return
		}
		defer func() { <-sem }()

		if item.Kind == "future" {
			q := item.Quote.Contract
			c, err := connector.FrontFuture(ctx, ibkrlib.Contract{Symbol: q.Symbol, SecType: "FUT", Exchange: q.Exchange, Currency: q.Currency}, time.Now())
			if err != nil {
				item.Quote = nil
				item.Error = err.Error()
				return
			}
			item.Quote.Contract = rpc.ContractParams{ConID: c.ConID, Symbol: c.Symbol, SecType: c.SecType, Exchange: c.Exchange, Currency: c.Currency, Expiry: c.Expiry, LocalSymbol: c.LocalSymbol, TradingClass: c.TradingClass, Multiplier: c.Multiplier}
			item.Name += " · " + c.LocalSymbol
		}
		raw, _ := json.Marshal(rpc.QuoteSnapshotParams{Contract: item.Quote.Contract, TimeoutMs: 2000})
		q, err := s.handleQuoteSnapshot(ctx, &rpc.Request{Params: raw})
		item.Quote = q
		if err != nil {
			item.Error = err.Error()
		}
	}
	for i := range result.Instruments {
		wg.Add(1)
		go fetch(&result.Instruments[i])
	}
	for i := range result.Underlyings {
		wg.Add(1)
		go fetch(&result.Underlyings[i])
	}
	wg.Wait()
	for _, list := range [][]rpc.MarketInstrument{result.Instruments, result.Underlyings} {
		for _, r := range list {
			if r.Error != "" || r.Quote == nil || r.Quote.Stale || (r.Quote.QuotePrice == nil && r.Quote.RegularClose == nil) {
				result.CoverageStatus = "partial"
			}
		}
	}
	// Never attach holdings-derived scope across a broker-session transition.
	current, err := s.handlePositionsList(ctx, &rpc.Request{Params: json.RawMessage(`{}`)})
	if err != nil || !connector.SessionCurrent(binding) || current.Authority == nil || current.Authority.Availability != rpc.AccountDataAvailable || current.Authority.Freshness != rpc.AccountDataFreshnessCurrent || current.Authority.Scope != result.Authority.Scope {
		return nil, errors.New("portfolio scope changed during market observation")
	}
	for _, list := range [][]rpc.MarketInstrument{result.Instruments, result.Underlyings} {
		for _, item := range list {
			if item.Quote != nil && item.Quote.Contract.ConID > 0 {
				s.rememberMarketHistory(rpc.MarketHistoryParams{Contract: item.Quote.Contract, Range: "1D"})
				s.rememberMarketHistory(rpc.MarketHistoryParams{Contract: item.Quote.Contract, Range: "1Y"})
			}
		}
	}
	return result, nil
}

func marketHistoryWindow(r string, now time.Time) (int, string, error) {
	switch r {
	case "1D":
		return 1, "5 mins", nil
	case "5D":
		return 7, "30 mins", nil
	case "1M":
		return 31, "1 day", nil
	case "6M":
		return 186, "1 day", nil
	case "YTD":
		return now.YearDay(), "1 day", nil
	case "1Y":
		return 366, "1 day", nil
	case "5Y":
		return 1830, "1 day", nil
	default:
		return 0, "", errBadRequest("supported ranges: 1D, 5D, 1M, 6M, YTD, 1Y, 5Y")
	}
}

func (s *Server) fetchMarketHistoryDays(ctx context.Context, p rpc.MarketHistoryParams, tailDays int, now time.Time) (*rpc.MarketHistoryResult, error) {
	return fetchMarketHistory(ctx, p, tailDays, now, func(ctx context.Context, contract ibkrlib.Contract, days int, interval string, timeout time.Duration) (ibkrlib.ChartSeries, error) {
		c := s.gatewayConnector()
		if c == nil {
			return ibkrlib.ChartSeries{}, s.gatewayUnavailableError()
		}
		binding, ok := c.CaptureHistoricalSession()
		if !ok {
			return ibkrlib.ChartSeries{}, s.gatewayUnavailableError()
		}
		series, err := c.FetchChartBars(ctx, contract, days, interval, timeout)
		if !c.HistoricalSessionCurrent(binding) {
			return ibkrlib.ChartSeries{}, errors.New("broker session changed during history read")
		}
		s.observeHistorySource(len(series.Bars) > 0, err, c, binding)
		return series, err
	})
}

// fetchMarketHistory composes acquisition and selection around the broker reader;
// the reader owns exact contract resolution and session continuity.
func fetchMarketHistory(ctx context.Context, p rpc.MarketHistoryParams, tailDays int, now time.Time, fetch func(context.Context, ibkrlib.Contract, int, string, time.Duration) (ibkrlib.ChartSeries, error)) (*rpc.MarketHistoryResult, error) {
	days, interval, err := marketHistoryWindow(p.Range, now)
	if err != nil {
		return nil, err
	}
	if p.Range == "1D" {
		// SMART has no venue calendar until the broker resolves the contract.
		// Six days cover the supported holiday/weekend closures without a
		// second resolution request (at most 1,728 five-minute time slots).
		// Selection below uses the resolved venue, not this padded lookback.
		days = 6
	}
	if tailDays > 0 {
		days = min(days, tailDays)
	} else if tailDays < 0 {
		days = min(1830, -tailDays)
	}
	if isOptionQuoteContract(p.Contract) {
		return nil, errBadRequest("price history currently supports underlyings; option history is not supplied")
	}
	contract, echo, _, err := normaliseStockQuoteContract(p.Contract)
	if err != nil {
		return nil, err
	}
	switch contract.SecType {
	case "STK", "IND", "CASH", "FUT":
	default:
		return nil, errBadRequest("unsupported history security type")
	}
	series, err := fetch(ctx, contract, days, interval, 25*time.Second)
	bars := series.Bars
	echo.ConID = series.Contract.ConID
	echo.Exchange = series.Contract.Exchange
	echo.PrimaryExch = series.Contract.PrimaryExch
	echo.LocalSymbol = series.Contract.LocalSymbol
	echo.TradingClass = series.Contract.TradingClass

	if err != nil {
		return nil, err
	}
	if len(bars) == 0 {
		return nil, errors.New("history unavailable: no observed bars")
	}
	result := &rpc.MarketHistoryResult{Contract: echo, Range: p.Range, Interval: interval, Source: "IBKR historical bars · " + series.WhatToShow, AsOf: now, CoverageStatus: "available"}
	result.TimestampKind = "instant"
	if interval == "1 day" {
		result.TimestampKind = "session_date"
	}
	result.PriceBasis = series.WhatToShow
	result.RegularHoursOnly = interval == "1 day"
	p.Contract = echo
	result.RequestedStart = historyRequestStart(p, now)
	if tailDays < 0 {
		result.RequestedStart = now.AddDate(0, 0, -days)
	}
	if tailDays > 0 && now.AddDate(0, 0, -days).After(result.RequestedStart) {
		result.RequestedStart = now.AddDate(0, 0, -days)
	}
	if interval == "1 day" {
		start := result.RequestedStart
		result.RequestedStart = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
	}
	if len(bars) > ibkrlib.ChartMaxBars {
		return nil, errors.New("history exceeds bounded series size")
	}
	for _, b := range bars {
		if b.Time.IsZero() || b.Time.After(now.Add(time.Minute)) || math.IsNaN(b.Close) || math.IsInf(b.Close, 0) || b.Close <= 0 {
			return nil, errors.New("invalid historical observation")
		}
		cutoff := result.RequestedStart
		if interval == "1 day" {
			cutoff = time.Date(cutoff.Year(), cutoff.Month(), cutoff.Day(), 0, 0, 0, 0, time.UTC)
		}
		if b.Time.Before(cutoff) {
			continue
		}
		point := rpc.MarketHistoryPoint{At: b.Time, Value: b.Close}
		if b.Volume >= 0 && series.WhatToShow == "TRADES" {
			v := int64(b.Volume)
			point.Volume = &v
		}
		if len(result.Points) > 0 && !point.At.After(result.Points[len(result.Points)-1].At) {
			return nil, errors.New("unordered historical observations")
		}
		result.Points = append(result.Points, point)
	}
	if len(result.Points) == 0 {
		return nil, errors.New("no observations within requested range")
	}
	result.Start = result.Points[0].At
	result.End = result.Points[len(result.Points)-1].At
	return result, nil
}

func marketHistoryStart(r string, now time.Time) time.Time {
	switch r {
	case "1D":
		return now.AddDate(0, 0, -1)
	case "5D":
		return now.AddDate(0, 0, -7)
	case "1M":
		return marketHistoryMonthsBefore(now, 1)
	case "6M":
		return marketHistoryMonthsBefore(now, 6)
	case "YTD":
		return time.Date(now.Year(), 1, 1, 0, 0, 0, 0, time.UTC)
	case "1Y":
		return marketHistoryMonthsBefore(now, 12)
	case "5Y":
		return marketHistoryMonthsBefore(now, 60)
	}
	return now
}

// Clamp to the destination month end instead of normalizing an impossible date
// into the following month and silently shortening the requested history.
func marketHistoryMonthsBefore(now time.Time, months int) time.Time {
	first := time.Date(now.Year(), now.Month(), 1, now.Hour(), now.Minute(), now.Second(), now.Nanosecond(), now.Location()).AddDate(0, -months, 0)
	lastDay := first.AddDate(0, 1, -1).Day()
	return time.Date(first.Year(), first.Month(), min(now.Day(), lastDay), now.Hour(), now.Minute(), now.Second(), now.Nanosecond(), now.Location())
}
