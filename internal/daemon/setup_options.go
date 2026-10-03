package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkr "github.com/osauer/canary/v2/pkg/ibkr"
)

func (s *Server) handleSetupOptions(ctx context.Context, req *rpc.Request) (*rpc.SetupOptionsResult, error) {
	var p rpc.SetupOptionsParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	p, err := rpc.NormalizeSetupOptionsParams(p, time.Now())
	if err != nil {
		return nil, errBadRequest(err.Error())
	}
	c := s.gatewayConnector()
	if c == nil {
		return nil, s.gatewayUnavailableError()
	}
	binding, ok := c.CaptureHistoricalSession()
	if !ok {
		return nil, s.gatewayUnavailableError()
	}
	requested, _, _, err := normaliseStockQuoteContract(p.Underlying)
	if err != nil {
		return nil, err
	}
	resolved, err := c.ResolveOrderContractForSession(ctx, binding, requested, 10*time.Second)
	if err != nil {
		return nil, err
	}
	u := resolved.Contract
	if u.ConID != p.Underlying.ConID || u.Symbol != p.Underlying.Symbol || u.SecType != "STK" || u.Currency != "USD" || u.Exchange != "SMART" || !usChartCalendar(rpc.ContractParams{SecType: u.SecType, Currency: u.Currency, Exchange: u.Exchange, PrimaryExch: u.PrimaryExch}) {
		return nil, errBadRequest("unsupported or mismatched underlying")
	}
	out := &rpc.SetupOptionsResult{Version: 1, Underlying: setupOptionContract(u), Expiry: p.Expiry, Expiries: []rpc.SetupOptionExpiry{}, Calls: []rpc.SetupOptionCall{}}
	listing, err := c.FetchSetupOptionStrikes(ctx, binding, u, 12*time.Second)
	if err != nil {
		return nil, err
	}
	if p.Expiry == "" {
		out.Expiries, out.Truncated = setupOptionExpiries(listing, time.Now())
	} else {
		strikes, err := setupListedStrikes(listing, u.Symbol, p.Expiry)
		if err != nil {
			return nil, err
		}
		if p.Strike != nil {
			if !slices.Contains(strikes, *p.Strike) {
				return nil, errBadRequest("strike is not listed for this standard expiry")
			}
			call := ibkr.Contract{Symbol: u.Symbol, SecType: "OPT", Exchange: "SMART", Currency: "USD", Expiry: p.Expiry, Strike: *p.Strike, Right: "C", Multiplier: 100, TradingClass: u.Symbol}
			exact, err := c.ResolveOrderContractForSession(ctx, binding, call, 10*time.Second)
			if err != nil {
				return nil, err
			}
			if err := ibkr.ValidateSetupCallResolution(exact, u, p.Expiry, *p.Strike); err != nil {
				return nil, err
			}
			contract := setupOptionContract(exact.Contract)
			out.Contract = &contract
		} else {
			// The center selects a bounded display window from real listed strikes;
			// it never invents contracts or recommends one for execution.
			raw, _ := json.Marshal(rpc.QuoteSnapshotParams{Contract: out.Underlying, TimeoutMs: 5000})
			quote, err := s.handleQuoteSnapshot(ctx, &rpc.Request{Params: raw})
			if err != nil {
				return nil, err
			}
			spot, _ := quoteCurrentPrice(quote)
			if spot == nil || *spot <= 0 {
				return nil, fmt.Errorf("underlying price unavailable for bounded option display")
			}
			rows := chainRowsFromListedStrikes(strikes, *spot, 10)
			if len(rows) > 21 {
				return nil, fmt.Errorf("option display exceeded bound")
			}
			out.Truncated = len(strikes) > len(rows)
			for _, row := range rows {
				out.Calls = append(out.Calls, rpc.SetupOptionCall{Strike: row.Strike, Status: "not_requested"})
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !c.HistoricalSessionCurrent(binding) {
		return nil, fmt.Errorf("broker session changed during option discovery")
	}
	out.AsOf = time.Now()
	return out, nil
}

func setupOptionContract(c ibkr.Contract) rpc.ContractParams {
	return rpc.ContractParams{ConID: c.ConID, Symbol: c.Symbol, SecType: c.SecType, Exchange: c.Exchange, PrimaryExch: c.PrimaryExch, Currency: c.Currency, Expiry: c.Expiry, Strike: c.Strike, Right: c.Right, Multiplier: c.Multiplier, TradingClass: c.TradingClass, LocalSymbol: c.LocalSymbol}
}

func setupOptionExpiries(listing map[string][]ibkr.ExpiryClassedStrikes, now time.Time) ([]rpc.SetupOptionExpiry, bool) {
	loc, _ := time.LoadLocation("America/New_York")
	today := now.In(loc).Format("20060102")
	dates := []string{}
	for date := range listing {
		date = strings.ReplaceAll(date, "-", "")
		d, err := time.Parse("20060102", date)
		if err == nil && date == d.Format("20060102") && date >= today {
			dates = append(dates, date)
		}
	}
	slices.Sort(dates)
	dates = slices.Compact(dates)
	out := make([]rpc.SetupOptionExpiry, 0, min(len(dates), 64))
	for _, d := range dates[:min(len(dates), 64)] {
		out = append(out, rpc.SetupOptionExpiry{Date: d})
	}
	return out, len(dates) > 64
}

func setupListedStrikes(listing map[string][]ibkr.ExpiryClassedStrikes, symbol, expiry string) ([]float64, error) {
	d, _ := time.Parse("20060102", expiry)
	entries := listing[d.Format("2006-01-02")]
	if len(entries) != 1 || entries[0].TradingClass != symbol {
		return nil, fmt.Errorf("standard expiry listing unavailable or ambiguous")
	}
	strikes := slices.Clone(entries[0].Strikes)
	if len(strikes) == 0 {
		return nil, fmt.Errorf("listed strikes unavailable")
	}
	for _, k := range strikes {
		if k <= 0 || math.IsNaN(k) || math.IsInf(k, 0) {
			return nil, fmt.Errorf("invalid listed strike")
		}
	}
	slices.Sort(strikes)
	return slices.Compact(strikes), nil
}
