package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/osauer/canary/v2/internal/breadth/spx"
	"github.com/osauer/canary/v2/internal/core"
	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// The regime gate (core.MeasureGate) is measured on
// the mandate's background cadence from the bars the regime engine already
// keeps (SPY 400 days, HYG 365 days, the same cache entries) and the
// breadth engine's daily history, and kept here for the mandate result and
// the strategies that declare it. A daily bar is read only once its session
// has settled: a copy fetched before the close carries the session's
// partial bar, which is never read as its close.

const (
	// regimeGateEvery is how often the gate is measured again.
	regimeGateEvery = 15 * time.Minute
	// regimeGateSettle is how long after a session's scheduled close its
	// daily bar is taken as final.
	regimeGateSettle = 20 * time.Minute
	// regimeGateBreadthDays is the breadth history requested. If it carries
	// no confirming run, the reading remains unavailable and counts off.
	regimeGateBreadthDays = 45
	regimeGateSPYDays     = 400
	regimeGateHYGDays     = HYGLookbackDays
)

type regimeGateCache struct {
	mu       sync.Mutex
	gate     *rpc.RegimeGateView
	fp       string
	inFlight atomic.Bool
}

// regimeGateView is the last measured gate, a copy.
func (s *Server) regimeGateView() *rpc.RegimeGateView {
	s.regimeGate.mu.Lock()
	defer s.regimeGate.mu.Unlock()
	if s.regimeGate.gate == nil {
		return nil
	}
	out := *s.regimeGate.gate
	out.Readings = append([]core.GateReading(nil), out.Readings...)
	return &out
}

// refreshRegimeGate starts a measurement when the gate is due: none yet,
// older than regimeGateEvery, or the constitution changed. It runs beside
// the caller, one at a time, so a slow history read never delays the
// mandate's own verdict.
func (s *Server) refreshRegimeGate(ctx context.Context, c *risk.Constitution) {
	now := s.nowUTC()
	fp := c.FingerprintKey()
	s.regimeGate.mu.Lock()
	last, lastFP := s.regimeGate.gate, s.regimeGate.fp
	s.regimeGate.mu.Unlock()
	if last != nil && lastFP == fp && now.Sub(last.EvaluatedAt) < regimeGateEvery {
		return
	}
	if !s.regimeGate.inFlight.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer s.regimeGate.inFlight.Store(false)
		s.measureRegimeGateNow(context.WithoutCancel(ctx), c, now, fp)
	}()
}

func (s *Server) measureRegimeGateNow(ctx context.Context, c *risk.Constitution, now time.Time, fp string) {
	gctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	g := s.measureRegimeGate(gctx, c, now)
	s.regimeGate.mu.Lock()
	s.regimeGate.gate, s.regimeGate.fp = g, fp
	s.regimeGate.mu.Unlock()
}

func (s *Server) measureRegimeGate(ctx context.Context, c *risk.Constitution, now time.Time) *rpc.RegimeGateView {
	var m *risk.ConstitutionMandate
	if c != nil {
		m = c.Mandate
	}
	b, h, stale, confirm, why := m.RegimeGateNumbers()
	if why != "" {
		return &rpc.RegimeGateView{State: core.GateOff, EvaluatedAt: now, Reason: why}
	}
	in := regimeGateInputs{Now: now, Latest: spx.CompletedSessionKey(now)}
	conn := s.gatewayConnector()
	in.SPY, in.SPYFetched = s.regimeGateBars(ctx, conn, "SPY", regimeGateSPYDays, now)
	in.HYG, in.HYGFetched = s.regimeGateBars(ctx, conn, "HYG", regimeGateHYGDays, now)
	if env, err := s.handleBreadthSPX(ctx, &rpc.Request{Method: rpc.MethodBreadthSPX, Params: json.RawMessage(`{"history_days":` + strconv.Itoa(regimeGateBreadthDays) + `}`)}); err == nil {
		in.Breadth = env
	}
	return regimeGateFrom(in, core.GateParams{BreadthMinPct: b, HalfScalePct: h, StaleAfterSessions: stale, ConfirmCloses: confirm})
}

// regimeGateBars reads the regime engine's cached bars for sym, fetching
// afresh when the cached copy predates the latest session's settle, and
// reports when the bars were fetched.
func (s *Server) regimeGateBars(ctx context.Context, conn *ibkrlib.Connector, sym string, days int, now time.Time) ([]ibkrlib.HistoricalBar, time.Time) {
	fetch := func(ctx context.Context, sym string, days int) ([]ibkrlib.HistoricalBar, error) {
		if conn == nil {
			return nil, errors.New("no gateway connection")
		}
		return conn.FetchHistoricalDailyBars(ctx, sym, days, 0)
	}
	if s.regimeHistory == nil {
		bars, _ := fetch(ctx, sym, days)
		return bars, now
	}
	settled := regimeGateSettledAt(spx.CompletedSessionKey(now), now)
	if e, ok := s.regimeHistory.get(sym, days); ok && e.FetchedAt.Before(settled) && conn != nil {
		if bars, err := fetch(ctx, sym, days); err == nil && len(bars) >= regimeHistoryMinBars(days) {
			s.regimeHistory.put(sym, days, bars, now)
			return bars, now
		}
	}
	bars, err := s.regimeHistory.fetch(ctx, sym, days, fetch)
	if err != nil {
		return nil, time.Time{}
	}
	if e, ok := s.regimeHistory.get(sym, days); ok {
		return bars, e.FetchedAt
	}
	return bars, now
}

// regimeGateSettledAt is when the session's daily bar is final: its
// scheduled close plus regimeGateSettle. Zero when the calendar does not
// know the session, so no copy is taken as too early.
func regimeGateSettledAt(session string, now time.Time) time.Time {
	cal := marketcal.NewWithClock(func() time.Time { return now })
	res, err := cal.Query(marketcal.Query{Market: marketcal.MarketUSEquity, Date: session, Days: 1})
	if err != nil || res.Session.Close.IsZero() {
		return time.Time{}
	}
	return res.Session.Close.Add(regimeGateSettle)
}

// settledSeries is the bars' closes on or before latest; a copy fetched
// before latest settled loses its bar for that session, which was partial.
func settledSeries(bars []ibkrlib.HistoricalBar, fetched time.Time, latest string, settled time.Time) core.Series {
	var s core.Series
	for _, b := range bars {
		d := historyBarSessionDate(b)
		if d == "" || d > latest || (d == latest && fetched.Before(settled)) {
			continue
		}
		s.Dates, s.Closes = append(s.Dates, d), append(s.Closes, b.Close)
	}
	return s
}

// regimeGateInputs is what one measurement read.
type regimeGateInputs struct {
	Now        time.Time
	Latest     string
	SPY, HYG   []ibkrlib.HistoricalBar
	SPYFetched time.Time
	HYGFetched time.Time
	Breadth    *rpc.BreadthSPXResult
}

// regimeGateFrom assembles the gate's input and measures it. A series
// fetched before the latest session settled loses its bar for that session.
func regimeGateFrom(in regimeGateInputs, p core.GateParams) *rpc.RegimeGateView {
	out := &rpc.RegimeGateView{EvaluatedAt: in.Now}
	settled := regimeGateSettledAt(in.Latest, in.Now)
	gi := core.GateInput{SPY: settledSeries(in.SPY, in.SPYFetched, in.Latest, settled), HYG: settledSeries(in.HYG, in.HYGFetched, in.Latest, settled), Breadth: map[string]float64{}}
	if b := in.Breadth; b != nil && (b.State == rpc.BreadthStateReady || b.State == rpc.BreadthStateDegraded) {
		for _, v := range b.History {
			if v.Date != "" && v.Date <= in.Latest && v.Coverage50 > 0 {
				gi.Breadth[v.Date] = v.PctAbove50DMA
			}
		}
		if b.SessionKey != "" && b.SessionKey <= in.Latest && b.Coverage50 > 0 {
			gi.Breadth[b.SessionKey] = b.PctAbove50DMA
		}
	}
	dates := gi.SPY.Dates
	if len(dates) == 0 {
		dates = gi.HYG.Dates
	}
	if len(dates) == 0 {
		out.Gate = core.Gate{State: core.GateOff, Params: p}
		out.Reason = "no SPY or HYG daily history could be read; the regime gate reads off"
		return out
	}
	gi.Calendar, _ = coreCalendar(dates, in.Latest, in.Now, marketcal.MarketUSEquity)
	out.Gate = core.MeasureGate(gi, p)
	return out
}
