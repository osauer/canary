package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/setups"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// setupBarSource is the broker surface a setup evaluation reads. The live
// connector implements it; daemon tests replace it.
type setupBarSource interface {
	CaptureHistoricalSession() (ibkrlib.HistoricalSessionBinding, bool)
	HistoricalSessionCurrent(ibkrlib.HistoricalSessionBinding) bool
	ResolveOrderContractForSession(context.Context, ibkrlib.ConnectorSessionBinding, ibkrlib.Contract, time.Duration) (ibkrlib.ResolvedOrderContract, error)
	FetchSetupBars(context.Context, ibkrlib.Contract, time.Time, time.Time, time.Duration) ([]ibkrlib.HistoricalBar, error)
}

// setupSource returns the test override, else the live connector, else nil.
func (s *Server) setupSource() setupBarSource {
	if s.setupSourceForTest != nil {
		return s.setupSourceForTest
	}
	if c := s.gatewayConnector(); c != nil {
		return c
	}
	return nil
}

// setupClock is the daemon clock for setup decisions and acquisition stamps.
func (s *Server) setupClock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *Server) handleSetupsEvaluate(ctx context.Context, req *rpc.Request) (*rpc.SetupResult, error) {
	var p rpc.SetupEvaluateParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	now := s.setupClock()
	var err error
	p, err = rpc.NormalizeSetupEvaluateParams(p, now)
	if err != nil {
		return nil, errBadRequest(err.Error())
	}
	ev := setupCoverageEvent{at: now, contract: p.Contract}
	r, err := s.evaluateSetup(ctx, p, now, &ev)
	if err == nil && ev.live {
		ev.state = setupCoverageState(r)
		s.recordSetupCoverage(ev)
	}
	return r, err
}

// countingSetupSource counts the historical requests one evaluation issues.
type countingSetupSource struct {
	setupBarSource
	reads *int
}

func (c countingSetupSource) FetchSetupBars(ctx context.Context, contract ibkrlib.Contract, start, end time.Time, timeout time.Duration) ([]ibkrlib.HistoricalBar, error) {
	*c.reads++
	return c.setupBarSource.FetchSetupBars(ctx, contract, start, end, timeout)
}

// evaluateSetup serves one evaluation and describes it in ev for coverage.
func (s *Server) evaluateSetup(ctx context.Context, p rpc.SetupEvaluateParams, now time.Time, ev *setupCoverageEvent) (*rpc.SetupResult, error) {
	at := p.At
	historical := !at.IsZero()
	if at.IsZero() {
		at = now
	}
	current, prior, err := setupSessionWindows(at, p.Spec.BaselineSessions)
	// Coverage counts live evaluations inside a regular session only.
	ev.session = current
	ev.live = !historical && !current.Open.IsZero() && !at.Before(current.Open) && at.Before(current.Close)
	base := setups.Input{Contract: p.Contract, At: at, ObservedAt: now, Historical: historical, Current: current}
	unavailable := func(reason string) *rpc.SetupResult {
		r := setups.Evaluate(p.Spec, base)
		r.Reasons = []string{reason}
		return &r
	}
	if err != nil {
		return unavailable(err.Error()), nil
	}
	var c setupBarSource = s.setupSource()
	if c == nil {
		return unavailable("gateway_unavailable"), nil
	}
	c = countingSetupSource{setupBarSource: c, reads: &ev.requests}
	binding, ok := c.CaptureHistoricalSession()
	if !ok {
		return unavailable("gateway_unavailable"), nil
	}
	contract, _, _, err := normaliseStockQuoteContract(p.Contract)
	if err != nil {
		return nil, err
	}
	resolved, err := c.ResolveOrderContractForSession(ctx, binding, contract, 10*time.Second)
	if err != nil {
		return unavailable("contract_resolution_unavailable"), nil
	}
	contract = resolved.Contract
	if contract.ConID <= 0 || contract.SecType != "STK" || contract.Currency != "USD" || !usChartCalendar(rpc.ContractParams{SecType: "STK", Currency: "USD", PrimaryExch: contract.PrimaryExch, Exchange: contract.Exchange}) {
		return unavailable("unsupported_underlying_calendar"), nil
	}
	base.Contract = rpc.ContractParams{ConID: contract.ConID, Symbol: contract.Symbol, SecType: contract.SecType, Exchange: contract.Exchange, PrimaryExch: contract.PrimaryExch, Currency: contract.Currency, LocalSymbol: contract.LocalSymbol, TradingClass: contract.TradingClass}
	ev.contract = base.Contract
	rawKey, _ := json.Marshal(base.Contract)
	contractKey := string(rawKey)
	key := s.setupProfiles.rowKey(contractKey, current.Date, historical)
	// A cached profile is read without the gate. Cold collection is serialized
	// so one watchlist cannot fan out into hundreds of HMDS reads; the
	// underlying client retains its own pacing.
	profile, observed, missing := s.setupProfiles.assemble(key, current.Date, prior)
	if len(missing) > 0 {
		// A remembered miss answers without a read until it expires.
		if reason, ok := s.setupProfiles.miss(contractKey, current.Date, now); ok {
			return unavailable("baseline_history_unavailable: " + reason), nil
		}
		if err := s.setupProfiles.lock(ctx); err != nil {
			return unavailable("profile_acquisition_cancelled"), nil
		}
		// Another request may have filled the profile, or failed to, while
		// this one waited.
		profile, observed, missing = s.setupProfiles.assemble(key, current.Date, prior)
		var reason string
		if len(missing) > 0 {
			reason, _ = s.setupProfiles.miss(contractKey, current.Date, s.setupClock())
		}
		if len(missing) > 0 && reason == "" {
			var e error
			profile, observed, e = s.acquireSetupProfile(ctx, c, contract, key, current.Date, prior, missing)
			switch {
			case e == nil:
				s.setupProfiles.clearMiss(contractKey, current.Date)
			case ctx.Err() != nil || !c.HistoricalSessionCurrent(binding):
				// The caller's cancellation or a broker reconnect says nothing
				// about the history; the next evaluation reads again.
				reason = e.Error()
			default:
				reason = s.setupProfiles.rememberMiss(contractKey, current.Date, e.Error(), s.setupClock())
			}
		}
		s.setupProfiles.unlock()
		if reason != "" {
			return unavailable("baseline_history_unavailable: " + reason), nil
		}
	}
	if !c.HistoricalSessionCurrent(binding) {
		return unavailable("broker_session_changed"), nil
	}
	if historical {
		bars, err := c.FetchSetupBars(ctx, contract, current.Open, at, 20*time.Second)
		if err != nil {
			return unavailable("current_history_unavailable: " + err.Error()), nil
		}
		base.Current = attachSetupBars([]setups.Session{current}, bars)[0]
		base.ObservedAt = s.setupClock()
	} else {
		// A live evaluation reads the current session at most once per
		// completed bar. Reused bars reproduce the evaluation at their own
		// decision and acquisition clocks.
		bars, _, err := s.liveCurrentBars(ctx, c, binding, contract, contractKey, current, at, observed)
		if err != nil {
			return unavailable("current_history_unavailable: " + err.Error()), nil
		}
		base.At, base.ObservedAt, base.Current = bars.at, bars.acquired, bars.session
	}
	if !c.HistoricalSessionCurrent(binding) {
		return unavailable("broker_session_changed"), nil
	}
	base.Prior = profile
	base.BaselineObservedAt = observed
	// An explicit historical clock is reconstructed now, never evidence that
	// these corrected bars were available then. Current reads expire on age.
	r := setups.Evaluate(p.Spec, base)
	return &r, nil
}

// acquireSetupProfile reads only the window sessions the cache lacks, keeps
// every complete session it received, and reports an incomplete window.
func (s *Server) acquireSetupProfile(ctx context.Context, c setupBarSource, contract ibkrlib.Contract, key, session string, window, missing []setups.Session) ([]setups.Session, time.Time, error) {
	var bars []ibkrlib.HistoricalBar
	var readErr error
	for _, r := range setupFetchRanges(missing) {
		got, err := collectSetupHistory(ctx, r[0], r[1], func(ctx context.Context, start, end time.Time) ([]ibkrlib.HistoricalBar, error) {
			return c.FetchSetupBars(ctx, contract, start, end, 20*time.Second)
		})
		if err == nil && len(bars)+len(got) > 2000 {
			err = fmt.Errorf("setup_history_exceeds_2000_bars")
		}
		if err != nil {
			readErr = err
			break
		}
		bars = append(bars, got...)
	}
	acquired := s.setupClock()
	var complete []setupProfileSession
	for _, session := range attachSetupBars(missing, bars) {
		if setups.ValidateSession(session, true) == nil {
			complete = append(complete, setupProfileSession{session: session, acquired: acquired})
		}
	}
	prior, observed, still := s.setupProfiles.fill(key, session, window, complete)
	if readErr != nil {
		return nil, time.Time{}, readErr
	}
	if len(still) > 0 {
		return nil, time.Time{}, fmt.Errorf("baseline_bars_incomplete")
	}
	return prior, observed, nil
}

func setupSessionWindows(at time.Time, count int) (setups.Session, []setups.Session, error) {
	cal := marketcal.New()
	session, err := cal.SessionAt(marketcal.MarketUSEquity, at)
	if err != nil || session.State == marketcal.StateUnknown {
		return setups.Session{}, nil, fmt.Errorf("calendar_unavailable")
	}
	current := setups.Session{Date: session.Date, Open: session.Open, Close: session.Close}
	if !session.IsOpen {
		return current, nil, fmt.Errorf("outside_regular_session")
	}
	var prior []setups.Session
	for offset := 1; offset <= 120 && len(prior) < count; offset++ {
		day := at.AddDate(0, 0, -offset)
		candidate, e := cal.SessionAt(marketcal.MarketUSEquity, day)
		if e != nil || candidate.State == marketcal.StateUnknown {
			return current, nil, fmt.Errorf("baseline_calendar_unavailable")
		}
		if !candidate.Open.IsZero() && candidate.Close.Sub(candidate.Open) == session.Close.Sub(session.Open) {
			prior = append(prior, setups.Session{Date: candidate.Date, Open: candidate.Open, Close: candidate.Close})
		}
	}
	if len(prior) != count {
		return current, nil, fmt.Errorf("comparable_baseline_sessions_unavailable")
	}
	sort.Slice(prior, func(i, j int) bool { return prior[i].Open.Before(prior[j].Open) })
	return current, prior, nil
}

func collectSetupHistory(ctx context.Context, start, end time.Time, fetch func(context.Context, time.Time, time.Time) ([]ibkrlib.HistoricalBar, error)) ([]ibkrlib.HistoricalBar, error) {
	if !end.After(start) || end.Sub(start) > 120*24*time.Hour {
		return nil, fmt.Errorf("invalid_history_window")
	}
	var out []ibkrlib.HistoricalBar
	for cursor := start; cursor.Before(end); {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		next := cursor.Add(7 * 24 * time.Hour)
		if next.After(end) {
			next = end
		}
		bars, err := fetch(ctx, cursor, next)
		if err != nil {
			return nil, err
		}
		for _, bar := range bars {
			if !bar.Time.Before(cursor) && bar.Time.Before(next) {
				out = append(out, bar)
			}
		}
		if len(out) > 2000 {
			return nil, fmt.Errorf("setup_history_exceeds_2000_bars")
		}
		cursor = next
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out, nil
}

func attachSetupBars(sessions []setups.Session, bars []ibkrlib.HistoricalBar) []setups.Session {
	out := append([]setups.Session(nil), sessions...)
	for i := range out {
		out[i].Bars = nil
		for _, b := range bars {
			end := b.Time.Add(5 * time.Minute)
			if !b.Time.Before(out[i].Open) && !end.After(out[i].Close) {
				out[i].Bars = append(out[i].Bars, rpc.SetupBar{Start: b.Time, End: end, Open: b.Open, High: b.High, Low: b.Low, Close: b.Close, Volume: b.Volume})
			}
		}
	}
	return out
}
