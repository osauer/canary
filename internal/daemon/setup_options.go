package daemon

import (
	"context"
	"encoding/json"
	"errors"
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
			out.Quote = s.setupCallQuote(ctx, c, binding, contract)
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
				out.Calls = append(out.Calls, rpc.SetupOptionCall{Strike: row.Strike, Status: rpc.SetupQuoteNotRequested})
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

const (
	// setupQuoteBudget bounds the exact selected call's single quote read.
	setupQuoteBudget = 5 * time.Second
	// setupQuoteMaxAge is the oldest as_of a quoted premium may carry.
	setupQuoteMaxAge = 60 * time.Second
)

// setupCallQuote reads the one dated quote for the exact selected call. It
// reuses the daemon's exact-contract quote path, as option-exit evidence does
// with both sides required: a private positive-ConID line on the discovery's
// own broker session, released afterwards, so selection never acquires or
// cancels a shared option quote line. It has no error result: a failed read
// or a rule miss is a missing quote and never fails the selection.
func (s *Server) setupCallQuote(ctx context.Context, c *ibkr.Connector, binding ibkr.ConnectorSessionBinding, call rpc.ContractParams) *rpc.SetupOptionQuote {
	quote, reason, took := readSetupCallQuote(ctx, func(ctx context.Context, budget time.Duration) (rpc.OrderQuoteSnapshot, error) {
		authority := s.setupQuoteAuthority(c, binding)
		if authority == nil {
			return rpc.OrderQuoteSnapshot{}, errors.New("broker connector changed before the quote")
		}
		snap, err := s.previewExactSessionContractQuote(ctx, authority, call, budget, true)
		if err != nil {
			// The exact path labels every failure trading-disabled for order
			// previews; for discovery it is only an unavailable quote.
			return snap, errors.New(strings.TrimPrefix(err.Error(), ErrTradingDisabled.Error()+": "))
		}
		return snap, nil
	}, time.Now)
	s.debugf("setups.options quote %s %s %gC: %s (%s) in %s", call.Symbol, call.Expiry, call.Strike, quote.Status, reason, took.Round(time.Millisecond))
	return &quote
}

// setupQuoteAuthority pins the quote read to the session that resolved the
// call. The exact-contract quote path takes this session pin; it carries no
// order authority. A replaced connector yields none, so the quote is missing.
func (s *Server) setupQuoteAuthority(c *ibkr.Connector, binding ibkr.ConnectorSessionBinding) *orderPreviewBrokerAuthority {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c == nil || s.connector != c {
		return nil
	}
	return &orderPreviewBrokerAuthority{connector: c, connectorEpoch: s.connectorEpoch, session: binding}
}

// readSetupCallQuote runs one read within setupQuoteBudget and applies the
// quote rule. It returns the rule's reason and the read's duration for the
// debug log.
func readSetupCallQuote(ctx context.Context, read func(context.Context, time.Duration) (rpc.OrderQuoteSnapshot, error), now func() time.Time) (rpc.SetupOptionQuote, string, time.Duration) {
	started := now()
	snap, err := read(ctx, setupQuoteBudget)
	finished := now()
	if err != nil {
		return rpc.SetupOptionQuote{Status: rpc.SetupQuoteMissing}, "read failed: " + err.Error(), finished.Sub(started)
	}
	quote, reason := setupOptionQuote(snap, finished)
	return quote, reason, finished.Sub(started)
}

// setupOptionQuote applies the exact-call quote rule: quoted only for a finite
// positive bid and ask with bid <= ask, a broker-labelled live, delayed, frozen
// or delayed-frozen mode, and an as_of no older than setupQuoteMaxAge. Anything
// else is missing with no prices, so a zero or one-sided book never reads as a
// premium.
func setupOptionQuote(snap rpc.OrderQuoteSnapshot, now time.Time) (rpc.SetupOptionQuote, string) {
	missing := rpc.SetupOptionQuote{Status: rpc.SetupQuoteMissing}
	switch {
	case snap.Bid == nil || snap.Ask == nil || !positiveFinite(*snap.Bid) || !positiveFinite(*snap.Ask):
		return missing, "bid or ask unavailable"
	case *snap.Bid > *snap.Ask:
		return missing, "crossed bid and ask"
	}
	switch snap.DataType {
	case rpc.MarketDataLive, rpc.MarketDataDelayed, rpc.MarketDataFrozen, rpc.MarketDataDelayedFrozen:
	default:
		return missing, "data type unknown"
	}
	// A live quote is dated by its older side's receipt on the request's own
	// line. IBKR sends no source time for delayed or frozen sides, so those are
	// dated by the read that received them and data_type names their age.
	asOf := snap.PriceAt
	if asOf.IsZero() {
		asOf = snap.AsOf
	}
	switch {
	case asOf.IsZero() || asOf.After(now):
		return missing, "quote undated"
	case now.Sub(asOf) > setupQuoteMaxAge:
		return missing, "quote older than 60s"
	}
	return rpc.SetupOptionQuote{Bid: new(*snap.Bid), Ask: new(*snap.Ask), AsOf: asOf, DataType: snap.DataType, Status: rpc.SetupQuoteQuoted}, rpc.SetupQuoteQuoted
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
