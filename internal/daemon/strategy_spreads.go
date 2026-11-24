package daemon

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v3/internal/broker"
	brokerIBKR "github.com/osauer/canary/v3/internal/broker/ibkr"
	"github.com/osauer/canary/v3/internal/rpc"
	"github.com/osauer/canary/v3/internal/rpc/api"
	ibkr "github.com/osauer/canary/v3/pkg/ibkr"
)

// The sold strikes `strategy.spreads` reads beyond the bought option: the
// nearest listed ones, resolved in one parallel batch and quoted four at a
// time, a market-data budget rather than a policy figure.
const (
	spreadCandidateLimit = 6
	spreadResolveWidth   = 10
)

// handleStrategySpreads serves the pricing evidence for opening a debit
// vertical (rpc.MethodStrategySpreads): the bought option's quote, the sold
// strikes of its expiry beyond it with each one's quote, the combination's
// net figures, and the default sold strike by the rule in force
// (addSpreadRule). Every price is the broker's; nothing is modelled here
// but the arithmetic of the two quotes and the one-sigma move.
func (s *Server) handleStrategySpreads(ctx context.Context, req *rpc.Request) (*rpc.StrategySpreadsResult, error) {
	var p rpc.StrategySpreadsParams
	if err := decodeFor(api.StrategySpreads, req.Params, &p); err != nil {
		return nil, err
	}
	u := p.Underlying
	if u.Symbol == "" || len(u.Symbol) > 32 || u.SecType != "STK" || u.Currency != "USD" || u.Exchange != "SMART" || u.ConID < 0 || u.Expiry != "" || u.Strike != 0 || u.Right != "" {
		return nil, errBadRequest("a spread needs an exact US dollar stock underlying on SMART")
	}
	if p.Long.Right != "C" && p.Long.Right != "P" {
		return nil, errBadRequest("the bought option must be a call (C) or a put (P)")
	}
	side := s.addSide(p.Long.Right)
	if !validAddCallSelection(side, p.Long, u.Symbol) || strings.TrimSpace(p.Long.Expiry) == "" {
		return nil, errBadRequest("the bought option must be an exact standard USD SMART option on the underlying, as add market returns it")
	}
	// The read seam (brokerReads): option chains and models are IBKR's, so
	// another broker answers that it does not serve them.
	reads, c, err := s.spreadReads()
	if err != nil {
		return nil, err
	}
	binding, ok := c.CaptureHistoricalSession()
	if !ok {
		return nil, s.gatewayUnavailableError()
	}
	exact, err := c.ResolveOrderContractForSession(ctx, binding, ibkr.Contract{ConID: u.ConID, Symbol: u.Symbol, SecType: "STK", Currency: u.Currency, Exchange: u.Exchange}, 8*time.Second)
	if err != nil {
		return nil, err
	}
	if exact.Contract.Symbol != u.Symbol || exact.Contract.SecType != "STK" || exact.Contract.Currency != u.Currency || u.ConID > 0 && exact.Contract.ConID != u.ConID {
		return nil, errBadRequest("underlying identity changed")
	}
	under := exact.Contract
	now := s.orderNow()
	class := &addClassSession{calendar: addOptionsCalendar(now), now: now}
	rule := s.addSpreadRule()
	out := &rpc.StrategySpreadsResult{Version: 1, Underlying: setupOptionContract(under), Rule: rule, Default: -1, Candidates: []rpc.StrategySpreadCandidate{}}
	if class.calendar.mode == "unavailable" {
		out.OptionMode, out.DefaultReason, out.AsOf = "unavailable", "the options calendar is unavailable", time.Now()
		return out, nil
	}
	// Hold the underlying's shared line while the broker computes option
	// models, as add market does.
	_, release, err := s.subs.HoldContract(ctx, under)
	if err != nil {
		return nil, err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		release(cleanup)
	}()
	longExact, err := c.ResolveOrderContractForSession(ctx, binding, ibkr.Contract{ConID: p.Long.ConID, Symbol: u.Symbol, SecType: "OPT", Exchange: "SMART", Currency: "USD", Expiry: p.Long.Expiry, Strike: p.Long.Strike, Right: p.Long.Right, Multiplier: 100, TradingClass: p.Long.TradingClass}, 5*time.Second)
	if err != nil || ibkr.ValidateSetupOptionResolution(longExact, under, p.Long.Expiry, p.Long.Strike, p.Long.Right) != nil || longExact.Contract.ConID != p.Long.ConID {
		return nil, errBadRequest("the bought option could not be resolved exactly at the broker")
	}
	listing, err := c.FetchSetupOptionStrikes(ctx, binding, under, 10*time.Second)
	if err != nil {
		return nil, err
	}
	listed, err := setupListedStrikes(listing, under.Symbol, p.Long.Expiry)
	if err != nil {
		return nil, errBadRequest("the bought option's expiry has no standard listing to sell against")
	}
	out.Spot = s.spreadSpot(ctx, c, binding, out.Underlying)
	out.Long = s.spreadLeg(ctx, c, binding, longExact, class)
	shorts := s.spreadShortContracts(ctx, c, binding, under, p.Long, listed)
	rows := make([]rpc.AddCallCandidate, len(shorts))
	// Four legs at a time, as add market reads its horizons. A loop of its
	// own: a shared runner of function values would join this route, in the
	// call graph broker-reach-check reads, to every caller's reads.
	var wg sync.WaitGroup
	slots := make(chan struct{}, addHorizonSlots)
	for i := range shorts {
		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			rows[i] = s.spreadLeg(ctx, c, binding, shorts[i], class)
		})
	}
	wg.Wait()
	if !c.HistoricalSessionCurrent(binding) || s.brokerReads() != reads {
		return nil, fmt.Errorf("broker session changed during spread discovery")
	}
	_, out.DTE = addHorizonExpiry([]rpc.SetupOptionExpiry{{Date: p.Long.Expiry}}, 0, 0, 10000, time.Now())
	priceSpreads(out, rows)
	out.OptionMode, out.AsOf = class.result().mode, time.Now()
	return out, nil
}

// spreadShortContracts resolves the listed strikes of the bought option's
// expiry beyond it (above a call, below a put), nearest first, and returns
// at most spreadCandidateLimit that exist, in strike order outward.
func (s *Server) spreadShortContracts(ctx context.Context, c *ibkr.Connector, binding ibkr.ConnectorSessionBinding, under ibkr.Contract, long rpc.ContractParams, listed []float64) []ibkr.ResolvedOrderContract {
	beyond := make([]float64, 0, len(listed))
	for _, k := range listed {
		if long.Right == "C" && k > long.Strike || long.Right == "P" && k < long.Strike {
			beyond = append(beyond, k)
		}
	}
	slices.SortFunc(beyond, func(a, b float64) int { return compareDistance(a, b, long.Strike) })
	beyond = beyond[:min(len(beyond), spreadResolveWidth)]
	found := make([]*ibkr.ResolvedOrderContract, len(beyond))
	var wg sync.WaitGroup
	for i, k := range beyond {
		wg.Go(func() {
			requested := ibkr.Contract{Symbol: under.Symbol, SecType: "OPT", Exchange: "SMART", Currency: "USD", Expiry: long.Expiry, Strike: k, Right: long.Right, Multiplier: 100, TradingClass: long.TradingClass}
			exact, err := c.ResolveOrderContractForSession(ctx, binding, requested, 5*time.Second)
			if err == nil && ibkr.ValidateSetupOptionResolution(exact, under, long.Expiry, k, long.Right) == nil {
				found[i] = &exact
			}
		})
	}
	wg.Wait()
	out := make([]ibkr.ResolvedOrderContract, 0, spreadCandidateLimit)
	for _, r := range found {
		if r != nil && len(out) < spreadCandidateLimit {
			out = append(out, *r)
		}
	}
	return out
}

// spreadLeg is one leg's contract and quote: the live quote and model
// while its options session is open, the broker's frozen closing quote
// while it is closed (planning, context only).
func (s *Server) spreadLeg(ctx context.Context, c *ibkr.Connector, binding ibkr.ConnectorSessionBinding, exact ibkr.ResolvedOrderContract, class *addClassSession) rpc.AddCallCandidate {
	contract := setupOptionContract(exact.Contract)
	contract.MinTick = exact.MinTick
	row := rpc.AddCallCandidate{Contract: &contract, PriceIncrements: addPriceRules(ctx, c, binding, exact)}
	switch class.of(exact).mode {
	case "live":
		row.Quote = s.spreadOptionQuote(ctx, c, binding, contract)
	case "planning":
		row.Quote = s.spreadOptionQuote(ctx, c, binding, contract)
		row.Planning = &rpc.AddCallPlanning{DeltaStatus: "unavailable"}
		if mid, ok := spreadLegMid(row); ok {
			row.Planning.Price, row.Planning.PriceSource, row.Planning.ObservedAt = &mid, "broker_midpoint", row.Quote.AsOf
		}
		if row.Quote != nil && row.Quote.Delta != nil {
			row.Planning.DeltaStatus = "in_range"
		}
	default:
		row.Unavailable = "Options calendar unavailable"
	}
	if _, ok := spreadLegMid(row); !ok && row.Unavailable == "" {
		row.Unavailable = "No two-sided quote"
	}
	return row
}

// spreadReads is the IBKR connector behind the read seam: brokerReads, then
// its IBKR adapter's connector, which answers the option-chain reads no
// other broker supplies.
func (s *Server) spreadReads() (broker.Reads, *ibkr.Connector, error) {
	reads := s.brokerReads()
	if reads == nil {
		return nil, nil, s.sessionUnavailableError()
	}
	a, ok := reads.(*brokerIBKR.Adapter)
	if !ok || a.Connector() == nil {
		return nil, nil, errBadRequest("Spread pricing reads IBKR option chains and models; it is not served on this broker")
	}
	return reads, a.Connector(), nil
}

// spreadOptionQuote is one exact option's two-sided quote with its model
// delta and volatility, read on the seam's session and kept only while that
// session is current: live while the class trades, the broker's frozen
// closing quote while it is closed. A failed read is a missing quote.
func (s *Server) spreadOptionQuote(ctx context.Context, c *ibkr.Connector, binding ibkr.ConnectorSessionBinding, contract rpc.ContractParams) *rpc.SetupOptionQuote {
	read, err := s.readExactContractQuote(ctx, c, binding, contract, setupQuoteBudget, true, true, true)
	if err != nil || !c.SessionCurrent(binding) {
		return &rpc.SetupOptionQuote{Status: rpc.SetupQuoteMissing}
	}
	quote, _ := setupOptionQuote(s.finishExactContractQuote(read, contract, true), s.orderNow())
	if quote.Status == rpc.SetupQuoteQuoted {
		quote.BidSize, quote.AskSize = read.sizes.Bid, read.sizes.Ask
		if m := read.sizes.Model; m != nil && m.Contract.ConID == contract.ConID && !m.ReceivedAt.Before(m.RequestedAt) && !m.ReceivedAt.After(time.Now()) && time.Since(m.ReceivedAt) <= setupQuoteMaxAge {
			quote.IV, quote.Delta, quote.ModelAsOf, quote.ModelDataType = m.IV, m.Delta, m.ReceivedAt, marketDataTypeName(m.DataType)
		}
	}
	return &quote
}

// spreadSpot is the underlying's price on the seam's session: its midpoint,
// else its last trade, else nothing.
func (s *Server) spreadSpot(ctx context.Context, c *ibkr.Connector, binding ibkr.ConnectorSessionBinding, under rpc.ContractParams) *float64 {
	read, err := s.readExactContractQuote(ctx, c, binding, under, 3*time.Second, false, false, false)
	if err != nil || !c.SessionCurrent(binding) {
		return nil
	}
	q := read.q
	switch {
	case q.Bid != nil && q.Ask != nil && positiveFinite(*q.Bid) && positiveFinite(*q.Ask) && *q.Bid <= *q.Ask:
		return new((*q.Bid + *q.Ask) / 2)
	case q.Last != nil && positiveFinite(*q.Last):
		return new(*q.Last)
	}
	return nil
}

// spreadLegMid is a leg's midpoint from a two-sided quote.
func spreadLegMid(row rpc.AddCallCandidate) (float64, bool) {
	q := row.Quote
	if q == nil || q.Status != rpc.SetupQuoteQuoted || q.Bid == nil || q.Ask == nil || !positiveFinite(*q.Bid) || !positiveFinite(*q.Ask) || *q.Ask < *q.Bid {
		return 0, false
	}
	return (*q.Bid + *q.Ask) / 2, true
}

// priceSpreads fills each candidate's net figures from the two quotes and
// picks the default sold strike by the rule (rpc.StrategySpreadRule): of the
// candidates priced as a debit below the width, saving at least MinSaving
// and at least CapExpectedMoves one-sigma moves beyond the price, the one
// whose delta is nearest ShortDeltaRatio times the bought option's; the
// nearer strike wins a tie. Nothing qualifies: no default, and the reason.
func priceSpreads(out *rpc.StrategySpreadsResult, shorts []rpc.AddCallCandidate) {
	long := out.Long
	rule := out.Rule
	longMid, longOK := spreadLegMid(long)
	var longDelta, longIV float64
	if q := long.Quote; q != nil && q.Delta != nil && q.IV != nil && positiveFinite(*q.IV) {
		longDelta, longIV = math.Abs(*q.Delta), *q.IV
	}
	if out.Spot != nil && positiveFinite(*out.Spot) && longIV > 0 && out.DTE > 0 {
		out.ExpectedMove = new(roundPrice(*out.Spot * longIV * math.Sqrt(float64(out.DTE)/365)))
	}
	call := long.Contract != nil && long.Contract.Right == "C"
	for _, short := range shorts {
		if short.Contract == nil || long.Contract == nil {
			continue
		}
		row := rpc.StrategySpreadCandidate{Short: short, Width: roundPrice(math.Abs(short.Contract.Strike - long.Contract.Strike))}
		mult := float64(contractMultiplier(*long.Contract))
		if shortMid, ok := spreadLegMid(short); ok && longOK {
			net := roundPrice(longMid - shortMid)
			row.NetMid = &net
			row.Saving = new(math.Round(shortMid/longMid*1000) / 1000)
			if net > 0 && net < row.Width {
				row.MaxLossPerUnit = new(math.Round(net*mult*100) / 100)
				row.MaxGainPerUnit = new(math.Round((row.Width-net)*mult*100) / 100)
				if call {
					row.Breakeven = new(roundPrice(long.Contract.Strike + net))
				} else {
					row.Breakeven = new(roundPrice(long.Contract.Strike - net))
				}
			}
			lq, sq := long.Quote, short.Quote
			row.NetBid, row.NetAsk = new(roundPrice(*lq.Bid-*sq.Ask)), new(roundPrice(*lq.Ask-*sq.Bid))
		}
		if out.ExpectedMove != nil && out.Spot != nil {
			distance := short.Contract.Strike - *out.Spot
			if !call {
				distance = -distance
			}
			row.BeyondExpectedMove = distance >= rule.CapExpectedMoves**out.ExpectedMove-1e-9
		}
		row.Eligible = row.MaxLossPerUnit != nil && row.Saving != nil && *row.Saving >= rule.MinSaving && row.BeyondExpectedMove && short.Quote != nil && short.Quote.Delta != nil && longDelta > 0
		out.Candidates = append(out.Candidates, row)
	}
	slices.SortStableFunc(out.Candidates, func(a, b rpc.StrategySpreadCandidate) int { return cmp.Compare(a.Width, b.Width) })
	switch {
	case !longOK:
		out.DefaultReason = "the bought option has no two-sided quote"
		return
	case longDelta == 0:
		out.DefaultReason = "the bought option has no delta or volatility from the broker yet"
		return
	case out.ExpectedMove == nil:
		out.DefaultReason = "the expected move cannot be measured without the underlying's price"
		return
	}
	target := rule.ShortDeltaRatio * longDelta
	for i, row := range out.Candidates {
		if !row.Eligible {
			continue
		}
		if out.Default < 0 || math.Abs(math.Abs(*row.Short.Quote.Delta)-target) < math.Abs(math.Abs(*out.Candidates[out.Default].Short.Quote.Delta)-target)-1e-12 {
			out.Default = i
		}
	}
	if out.Default < 0 {
		out.DefaultReason = fmt.Sprintf("no listed sold strike takes back at least %.0f%% of the premium at least %.2f expected moves beyond the price", rule.MinSaving*100, rule.CapExpectedMoves)
	}
}

// handleStrategySpreadPlan sizes a debit vertical (rpc.MethodStrategySpreadPlan)
// as add.call-plan sizes one option: each candidate size is an exact preview
// of the combination (previewStrategyOrder, operation open), whose accepted
// WhatIf gives the fee bound and whose gates judge it as it would be placed.
// The size is the smallest of what the budget buys, the option-contract cap
// per leg, the order cap on the net premium and, for a bear put spread on a
// Rulebook hedge symbol, the contracts of the spread's net delta that fit
// under rule 12's over-hedge line (addIndexPutRoomFrom, the one room Add's
// index put is sized with); HeldBy names the one that held it.
func (s *Server) handleStrategySpreadPlan(ctx context.Context, req *rpc.Request) (*rpc.StrategySpreadPlanResult, error) {
	var p rpc.StrategySpreadPlanParams
	if err := decodeFor(api.StrategySpreadPlan, req.Params, &p); err != nil {
		return nil, err
	}
	if !positiveFinite(p.Budget) || p.Budget > 1e9 || !positiveFinite(p.LimitPrice) {
		return nil, errBadRequest("a spread plan needs a positive net debit and a positive budget")
	}
	preview := func(units int) (*rpc.OrderPreviewResult, error) {
		return s.previewStrategyOrder(ctx, rpc.StrategyPreviewParams{Operation: rpc.StrategyOperationOpen, Units: units, LimitPrice: new(p.LimitPrice),
			Source: "strategy_plan", Open: &rpc.StrategyOpenLegs{Long: p.Long, Short: p.Short}})
	}
	windows, hoursOK := s.addCallHours(ctx, p.Long)
	if !addCallSessionLive(windows, hoursOK, s.orderNow()) {
		return nil, errBadRequest("Options session must be open to plan a spread")
	}
	// One unit first: its shape refusals, and the order cap and exchange
	// rate its net premium is judged with.
	one, err := preview(1)
	if err != nil {
		return nil, err
	}
	out := &rpc.StrategySpreadPlanResult{Long: p.Long, Short: p.Short, LimitPrice: p.LimitPrice, Budget: p.Budget, OptionCap: s.addOptionCap()}
	bounds := map[string]int{}
	if out.OptionCap > 0 {
		bounds[rpc.SpreadPlanHeldByOptionCap] = out.OptionCap
	}
	if perUnit := p.LimitPrice * 100 * one.FXRate; positiveFinite(perUnit) && positiveFinite(one.MaxNotional) {
		out.OrderCapUnits = int(math.Floor(one.MaxNotional/perUnit + 1e-9))
		bounds[rpc.SpreadPlanHeldByOrderCap] = out.OrderCapUnits
	}
	if p.Long.Right == "P" && s.rulebookPolicy().IsHedgeSymbol(p.Long.Symbol) {
		need := s.readAddIndexProtection(ctx, p.Long)
		var net, underlying *float64
		if need.measured() && need.view.RoomBase > 0 {
			ld, lu := s.addIndexPutModel(ctx, p.Long)
			sd, _ := s.addIndexPutModel(ctx, p.Short)
			if ld != nil && sd != nil {
				net, underlying = new(*ld-*sd), lu
				out.NetDelta = net
			}
		}
		room, view, err := addIndexPutRoomFrom(need, net, underlying, 100, out.OptionCap)
		if err != nil {
			return nil, err
		}
		out.IndexProtection = view
		if room > 0 {
			out.HedgeRoomUnits = room
			bounds[rpc.SpreadPlanHeldByHedgeRoom] = room
		}
	}
	limit, heldBy := math.MaxInt, rpc.SpreadPlanHeldByBudget
	for _, name := range []string{rpc.SpreadPlanHeldByHedgeRoom, rpc.SpreadPlanHeldByOptionCap, rpc.SpreadPlanHeldByOrderCap} {
		if b, ok := bounds[name]; ok && b < limit {
			limit, heldBy = b, name
		}
	}
	n := min(limit, int(math.Min(1e6, math.Floor(p.Budget/(p.LimitPrice*100)))))
	if n < limit {
		heldBy = rpc.SpreadPlanHeldByBudget
	}
	if n <= 0 {
		if heldBy == rpc.SpreadPlanHeldByBudget {
			return nil, errBadRequest("the budget buys no spread at this debit")
		}
		return nil, errBadRequest("no spread unit fits: held by " + strings.ReplaceAll(heldBy, "_", " "))
	}
	for attempt := 0; attempt < 4 && n > 0; attempt++ {
		pv := one
		if n != 1 {
			if pv, err = preview(n); err != nil {
				return nil, err
			}
		}
		fee, ok := cashSweepFeeUpper(pv, "USD")
		if !ok {
			return nil, fmt.Errorf("an accepted broker fee bound is required to size this spread")
		}
		debit := float64(n)*p.LimitPrice*100 + fee
		if debit <= p.Budget {
			out.Units, out.FeeBound, out.Debit, out.HeldBy, out.AsOf = n, fee, debit, heldBy, time.Now()
			if !addCallSessionLive(windows, hoursOK, s.orderNow()) {
				return nil, errBadRequest("Options session must be open to plan a spread")
			}
			return out, nil
		}
		heldBy = rpc.SpreadPlanHeldByBudget
		n = min(n-1, int(math.Floor((p.Budget-fee)/(p.LimitPrice*100))))
	}
	return nil, errBadRequest("no spread size fits this budget with the broker's fee")
}
