package daemon

import (
	"context"
	"math"
	"slices"
	"time"

	"github.com/osauer/canary/v2/internal/breadth/spx"
	"github.com/osauer/canary/v2/internal/core"
	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// Strategy one's screen (core.StrongNamesV1). The
// universe is the S&P 500 constituents the breadth engine already keeps:
// their last 253 closes, dated on the US calendar back from each window's
// last bar and checked against its dated bars, with volumes from those
// bars. SPY comes from the regime engine's cache as for the regime gate,
// highs and lows for the average true range only for the names a plan
// needs. The risk allocation is the register's, and no entry names
// strong-names-v1 yet: the plans are recorded and buy nothing (owner, A2,
// 2026-11-03 13:29 CET). Nothing here proposes, previews or sends.

// strategyOneATRDays is the calendar-day history read for a name's average
// true range: enough for 21 sessions after holidays.
const strategyOneATRDays = 45

func (s *Server) handleStrategyOne(ctx context.Context) (*rpc.StrategyOneResult, error) {
	now := s.nowUTC()
	out := &rpc.StrategyOneResult{EvaluatedAt: now, Status: "ready", ReadOnly: true}
	out.Rule, out.Spec, out.SpecHash, out.Numbers = core.StrongNamesRuleID, core.StrongNamesSpecID, core.StrongNamesSpecHash, core.StrongNamesV1
	hold := func(why string) (*rpc.StrategyOneResult, error) {
		out.Status, out.Reason = "hold", why
		return out, nil
	}
	if s.breadth == nil {
		return hold("the breadth engine is not running, so the constituents' history cannot be read")
	}
	latest := spx.CompletedSessionKey(now)
	settled := regimeGateSettledAt(latest, now)
	conn := s.gatewayConnector()
	spyBars, spyFetched := s.regimeGateBars(ctx, conn, "SPY", regimeGateSPYDays, now)
	spy := settledSeries(spyBars, spyFetched, latest, settled)
	if len(spy.Dates) == 0 {
		return hold("SPY's daily history could not be read")
	}
	sessions, _ := coreCalendar(spy.Dates, latest, now, marketcal.MarketUSEquity)
	in := core.StrongNamesInput{SPY: spy, Names: map[string]core.NameSeries{}, ATR: map[string]float64{}, Held: map[string]int{}}
	for sym, w := range s.breadth.Windows() {
		series, ok := strategyOneSeries(w, sessions, latest)
		if !ok {
			out.Undated++
			continue
		}
		in.Names[sym] = series
	}
	if g := s.regimeGateView(); g != nil {
		in.GateState, in.GateScale = g.State, g.ScalePct
	} else {
		in.GateState = core.GateOff
		out.Notes = append(out.Notes, "the regime gate has not been measured yet; it reads off")
	}
	out.GateState, out.GateScale = in.GateState, in.GateScale
	if res, _, _, _ := s.currentMandateVerdict(); res != nil && res.Verdict != nil && res.Verdict.Capital.Known {
		out.RiskBase, out.BaseCurrency = res.Verdict.Capital.RiskBase, res.Verdict.Capital.BaseCurrency
		if _, rate, _, _ := s.fillQuoteFX(ctx, "USD"); rate != nil && *rate > 0 {
			out.USDRate, out.RiskBaseUSD = *rate, out.RiskBase / *rate
		}
	}
	if !(out.RiskBaseUSD > 0) {
		out.Notes = append(out.Notes, "the risk base in USD is unknown, so no plan is sized")
	}
	in.RiskBase, in.AllocationPct = out.RiskBaseUSD, 0
	out.Notes = append(out.Notes, "no register entry names "+core.StrongNamesRuleID+": the strategy holds no risk allocation and no positions, so every plan is recorded and nothing is bought")
	first := core.StrongNamesV1.Screen(in)
	for _, e := range first.Entries {
		if a := s.strategyOneATR(ctx, conn, e.Symbol, latest, settled, now); a > 0 {
			in.ATR[e.Symbol] = a
		}
	}
	out.StrongNamesScreen = core.StrongNamesV1.Screen(in)
	if out.Reason != "" {
		out.Status = "hold"
	}
	return out, nil
}

// strategyOneSeries lays a window's closes on the US sessions ending at its
// last bar and checks them against its dated bars; false when they do not
// agree or the last bar is not a session.
func strategyOneSeries(w spx.ConstituentWindow, sessions []string, latest string) (core.NameSeries, bool) {
	end := slices.Index(sessions, w.LastBarAt)
	n := len(w.Closes)
	if end < 0 || n == 0 || end+1 < n {
		return core.NameSeries{}, false
	}
	dates := sessions[end+1-n : end+1]
	at := make(map[string]int, n)
	for i, d := range dates {
		at[d] = i
	}
	vols := make([]float64, n)
	for _, b := range w.Bars {
		i, ok := at[b.Date]
		if !ok {
			continue
		}
		if math.Abs(b.Close-w.Closes[i]) > 1e-6*math.Max(1, math.Abs(b.Close)) {
			return core.NameSeries{}, false
		}
		if b.Volume != nil {
			vols[i] = float64(*b.Volume)
		}
	}
	k := n
	for k > 0 && dates[k-1] > latest {
		k--
	}
	return core.NameSeries{Dates: slices.Clone(dates[:k]), Closes: slices.Clone(w.Closes[:k]), Volumes: vols[:k]}, k > 0
}

// strategyOneATR is a name's 20-session average true range on settled
// bars, or 0 when it cannot be read.
func (s *Server) strategyOneATR(ctx context.Context, conn *ibkrlib.Connector, sym, latest string, settled, now time.Time) float64 {
	bars, fetched := s.regimeGateBars(ctx, conn, sym, strategyOneATRDays, now)
	var kept []ibkrlib.HistoricalBar
	for _, b := range bars {
		d := historyBarSessionDate(b)
		if d == "" || d > latest || (d == latest && fetched.Before(settled)) {
			continue
		}
		kept = append(kept, b)
	}
	return technicalATR(kept, core.StrongNamesV1.ATRSessions)
}
