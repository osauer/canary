package ibkr

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ChartMaxBars bounds both acquisition and the returned chart series.
const ChartMaxBars = 2000

// ChartSeries preserves the actual resolved identity and requested price basis.
type ChartSeries struct {
	Contract   Contract
	Bars       []HistoricalBar
	WhatToShow string
}

// FetchChartBars reads bounded bars through exact session-bound contract resolution.
// It shares request registration, pacing and cancellation with the historical client.
func (c *Connector) FetchChartBars(ctx context.Context, contract Contract, days int, interval string, timeout time.Duration) (ChartSeries, error) {
	result := ChartSeries{}
	if (interval != "5 mins" && interval != "30 mins" && interval != "1 day") || days < 1 || days > 1830 || (interval != "1 day" && days > 7) {
		return result, fmt.Errorf("invalid chart range or interval")
	}
	binding, ok := c.CaptureSession()
	if !ok {
		return result, fmt.Errorf("broker session unavailable")
	}
	resolved, err := c.ResolveOrderContractForSession(ctx, binding, contract, min(timeout, 10*time.Second))
	if err != nil {
		return result, err
	}
	if !c.SessionCurrent(binding) {
		return result, fmt.Errorf("broker session changed")
	}
	result.Contract = resolved.Contract
	result.WhatToShow = "TRADES"
	if contract.SecType == "CASH" {
		result.WhatToShow = "MIDPOINT"
	}
	result.Bars, err = c.fetchHistoricalWithContractOptions(ctx, resolved.Contract.Symbol, resolved.Contract, days, timeout, result.WhatToShow, historicalRequestOptions{formatDate: 2, chartBarSize: interval, chartOutsideRTH: interval != "1 day", strictDaily: true, waitForEnd: true, maxBars: ChartMaxBars})
	if !c.SessionCurrent(binding) {
		return ChartSeries{}, fmt.Errorf("broker session changed during history")
	}
	return result, err
}

// MarketClassification is the broker's own description of a contract's
// business: the coarse industry, the finer category, and the stock type
// (COMMON, ETF, ADR, ...). Any field may be empty; an ETF typically carries
// only its stock type.
type MarketClassification struct {
	Industry  string
	Category  string
	StockType string
}

// MarketClassification preserves the classification of an exact
// session-bound contract.
func (c *Connector) MarketClassification(ctx context.Context, contract Contract, timeout time.Duration) (MarketClassification, error) {
	binding, ok := c.CaptureSession()
	if !ok {
		return MarketClassification{}, fmt.Errorf("broker session unavailable")
	}
	resolved, err := c.ResolveOrderContractForSession(ctx, binding, contract, timeout)
	if err != nil {
		return MarketClassification{}, err
	}
	return MarketClassification{Industry: resolved.Industry, Category: resolved.Category, StockType: resolved.StockType}, nil
}

// FrontFuture identifies the nearest strictly future expiry from broker details.
// It is a dated contract, not a continuous or back-adjusted futures series.
func (c *Connector) FrontFuture(ctx context.Context, contract Contract, now time.Time) (Contract, error) {
	return c.frontFuture(ctx, contract, now, 3*time.Second, nil)
}

func selectFrontFuture(want Contract, details []ContractDetailsLite, now time.Time) (Contract, error) {
	candidates := []ContractDetailsLite{}
	for _, d := range details {
		if d.ConID <= 0 || d.SecType != "FUT" || d.Symbol != want.Symbol || d.Currency != want.Currency || !strings.EqualFold(d.Exchange, want.Exchange) || d.TradingClass != want.Symbol {
			continue
		}
		date, err := time.Parse("20060102", d.Expiry)
		if err != nil || date.Format("2006-01-02") <= now.UTC().Format("2006-01-02") {
			continue
		}
		candidates = append(candidates, d)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Expiry < candidates[j].Expiry })
	if len(candidates) == 0 {
		return Contract{}, fmt.Errorf("front future contract unavailable")
	}
	d := candidates[0]
	if len(candidates) > 1 && candidates[1].Expiry == d.Expiry && candidates[1].ConID != d.ConID {
		return Contract{}, fmt.Errorf("front future contract ambiguous")
	}
	return Contract{ConID: d.ConID, Symbol: d.Symbol, SecType: d.SecType, Exchange: d.Exchange, Currency: d.Currency, Expiry: d.Expiry, LocalSymbol: d.LocalSymbol, TradingClass: d.TradingClass, Multiplier: d.Multiplier}, nil
}
