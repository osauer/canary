package daemon

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/core"
	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/register"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/rpc/api"
)

// The strategy replay (strategy-evaluator.md §9): the rule run over the
// month-ends of a register entry's instrument on the daemon's own data, in
// the lab's table shape, so `lab replay-compare` can hold the two side by
// side. Read-only: the daemon reads five years of daily closes through the
// charts' broker helper (the same read that fills the entry's series) and
// decides nothing with them; no month is frozen, no record written, no
// order path touched. The evaluator stays the only place the rule runs:
// the replay is core.Replay inside the daemon, behind an RPC.

// strategyReplayRange is the broker range the replay reads: the longest
// the history helper serves (about five years of daily bars).
const strategyReplayRange = "5Y"

// handleStrategyReplay serves strategy.replay: an entry's decision table
// from From to To (inclusive, ISO dates), To bounded at the last decided
// month-end.
func (s *Server) handleStrategyReplay(ctx context.Context, req *rpc.Request) (*rpc.StrategyReplayResult, error) {
	var p rpc.StrategyReplayParams
	if len(req.Params) > 0 && string(req.Params) != "null" {
		if err := decodeFor(api.StrategyReplay, req.Params, &p); err != nil {
			return nil, err
		}
	}
	id := strings.TrimSpace(p.ID)
	if id == "" {
		return nil, errBadRequest("strategy.replay needs the strategy's id; `canary mandate` lists the register")
	}
	from, to, err := replaySpan(p.From, p.To)
	if err != nil {
		return nil, err
	}
	c := s.riskPolicies.snapshot().policy
	r, ok := s.resolvedStrategy(c, id)
	if !ok {
		return nil, errBadRequest(unknownStrategyMessage(id, s.resolvedRegister(c)))
	}
	name := strategyName(r.Entry)
	inst := s.requestedInstrument(r)
	if inst.ISIN == "" {
		return nil, errBadRequest(fmt.Sprintf("%s names no instrument to replay; `canary strategy %s` says why it holds", name, id))
	}
	contract, pinHold, lookupErr, asked := s.instrumentPinState(inst)
	switch {
	case contract != nil:
	case pinHold != "":
		return nil, errBadRequest(pinHold)
	case lookupErr != "":
		return nil, errBadRequest(fmt.Sprintf("%s's contract is not resolved: IBKR gave no answer for %s in %s (%s)", name, inst.ISIN, inst.Currency, lookupErr))
	case !asked:
		return nil, errBadRequest(fmt.Sprintf("%s's contract is not resolved yet; the desk asks IBKR for %s in %s on the background lane", name, inst.ISIN, inst.Currency))
	}
	trendID := r.Trend
	if r.Entry.Form == risk.StrategyFormCore {
		trendID = core.TrendSMA10.ID
	}
	trend, ok := core.FindTrend(trendID)
	if !ok {
		return nil, errBadRequest(fmt.Sprintf("%s names signal %q, which this daemon's library lacks", name, nonEmptyString(trendID, "(unwritten)")))
	}
	now := s.nowUTC()
	fetch := s.fetchMarketHistoryDays
	if s.mandateHistoryFetchForTest != nil {
		fetch = s.mandateHistoryFetchForTest
	}
	need := bindMandateHistoryNeed(instrumentSeriesNeed(seriesKeyFor(r, contract.ConID), contract))
	hist, err := fetch(ctx, rpc.MarketHistoryParams{Contract: need.Contract, Range: strategyReplayRange}, 0, now)
	if err != nil {
		return nil, fmt.Errorf("no %s daily history for the replay: %w", contract.label(), err)
	}
	dates, closes, err := replaySeries(hist, need.Contract)
	if err != nil {
		return nil, fmt.Errorf("the %s daily history cannot be replayed: %w", contract.label(), err)
	}
	latest, _, why := coreDecisionDays(now, contract.Market)
	if why != "" {
		return nil, errBadRequest(why)
	}
	res := replayStrategySeries(r, contract, trend, dates, closes, from, min(to, latest), now)
	res.Source, res.FetchedAt = hist.Source, hist.AsOf
	return res, nil
}

// replaySpan reads the replay's bounds: two ISO dates in order.
func replaySpan(from, to string) (string, string, error) {
	from, to = strings.TrimSpace(from), strings.TrimSpace(to)
	for _, d := range []string{from, to} {
		if _, err := time.Parse(time.DateOnly, d); err != nil {
			return "", "", errBadRequest(fmt.Sprintf("strategy.replay needs FROM and TO as YYYY-MM-DD dates, not %q", d))
		}
	}
	if from > to {
		return "", "", errBadRequest(fmt.Sprintf("strategy.replay: FROM %s is after TO %s", from, to))
	}
	return from, to, nil
}

// replaySeries reads the broker's daily bars as the entry's series is read
// (fetchMandateSeries): ascending session dates with finite closes, for
// the contract asked.
func replaySeries(hist *rpc.MarketHistoryResult, want rpc.ContractParams) (dates []string, closes []float64, err error) {
	if hist == nil || len(hist.Points) == 0 {
		return nil, nil, errors.New("no daily bars")
	}
	if !mandateHistoryContractMatches(hist.Contract, want) {
		return nil, nil, errors.New("the daily history belongs to a different or unproven contract")
	}
	if hist.Interval != "1 day" {
		return nil, nil, fmt.Errorf("the broker answered with %s bars, not daily", hist.Interval)
	}
	for _, p := range hist.Points {
		if !isFinitePrice(p.Value) || math.IsNaN(p.Value) {
			return nil, nil, fmt.Errorf("invalid close on %s", p.At.UTC().Format(time.DateOnly))
		}
		day := p.At.UTC().Format(time.DateOnly)
		if n := len(dates); n > 0 && day <= dates[n-1] {
			return nil, nil, errors.New("unordered daily bars")
		}
		dates, closes = append(dates, day), append(closes, p.Value)
	}
	return dates, closes, nil
}

// replayStrategySeries runs core.Replay over a series on the entry's
// exchange calendar, as the monthly evaluation reads it (coreCalendar: the
// official sessions where the embedded calendar covers them, the series'
// own dates before), under the entry's terms and the fixed replay sizing.
// The handler calls it after the broker read; the series-file harness
// (strategy_replay_test.go) calls it on a `canary market --json` dump, so
// the two never differ.
func replayStrategySeries(r register.Resolved, contract *coreContract, trend core.Trend, dates []string, closes []float64, from, to string, now time.Time) *rpc.StrategyReplayResult {
	sessions, off := coreCalendar(dates, to, now, contract.Market)
	terms := core.ReplayTerms{BandPct: r.Terms.BandPct, MinOrderBase: r.Terms.MinOrderBase}
	ruleID, specHash := r.Entry.Rule, nonEmptyString(r.SpecHash, r.Entry.SpecHash)
	if r.Rule != nil {
		ruleID = r.Rule.ID
	}
	if r.Entry.Form == risk.StrategyFormCore {
		ruleID, specHash = core.RuleID, core.SpecHash
	}
	res := &rpc.StrategyReplayResult{AsOf: now, Strategy: r.Entry.ID, Rule: ruleID, SpecHash: specHash, Signal: trend.ID, From: from, To: to,
		Instrument: &rpc.CoreInstrument{Symbol: contract.Symbol, ISIN: contract.ISIN, ConID: contract.ConID, Exchange: contract.PrimaryExch, Currency: contract.Currency, Resolved: true,
			LocalSymbol: contract.LocalSymbol, TradingClass: contract.TradingClass, Calendar: string(contract.Market), CalendarLabel: marketcal.Label(contract.Market), ResolvedAt: contract.At},
		Calendar: string(contract.Market), CalendarLabel: marketcal.Label(contract.Market), Dates: dates, Closes: closes, OffCalendar: off,
		Sizing: core.ReplaySizing, Terms: terms,
		Note: "Paper table: fixed sizing, the paper holding carried under the entry's band and minimum from zero; nothing is frozen, recorded or sent. Compare with `lab replay-compare`.",
		Rows: core.Replay(trend, digestLabel(r), sessions, dates, closes, from, to, terms)}
	if len(dates) > 0 {
		res.SeriesFrom, res.SeriesTo = dates[0], dates[len(dates)-1]
	}
	return res
}
