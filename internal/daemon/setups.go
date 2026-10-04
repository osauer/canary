package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
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

// Only complete, comparable baseline observations are memoized. This bounded
// cache is disposable market evidence, not a candidate or execution ledger.
// mu guards rows; gate serializes cold acquisition only, so a cached profile
// never waits behind another contract's broker reads.
type setupProfileCache struct {
	mu   sync.Mutex
	gate chan struct{}
	rows map[string]setupProfile
	// waitForTest observes a request blocking on a busy gate.
	waitForTest func()
}

func (p *setupProfileCache) lock(ctx context.Context) error {
	p.mu.Lock()
	if p.gate == nil {
		p.gate = make(chan struct{}, 1)
	}
	gate, wait := p.gate, p.waitForTest
	p.mu.Unlock()
	select {
	case gate <- struct{}{}:
		return nil
	default:
	}
	if wait != nil {
		wait()
	}
	select {
	case gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (p *setupProfileCache) unlock() { <-p.gate }

func (p *setupProfileCache) lookup(key string) (setupProfile, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	profile, ok := p.rows[key]
	return profile, ok
}

func (p *setupProfileCache) store(key string, profile setupProfile) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rows == nil {
		p.rows = map[string]setupProfile{}
	}
	if _, ok := p.rows[key]; !ok && len(p.rows) >= 20 {
		for k := range p.rows {
			delete(p.rows, k)
			break
		}
	}
	p.rows[key] = profile
}

type setupProfile struct {
	prior    []setups.Session
	observed time.Time
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
	at := p.At
	historical := !at.IsZero()
	if at.IsZero() {
		at = now
	}
	current, prior, err := setupSessionWindows(at, p.Spec.BaselineSessions)
	base := setups.Input{Contract: p.Contract, At: at, ObservedAt: now, Historical: historical, Current: current}
	unavailable := func(reason string) *rpc.SetupResult {
		r := setups.Evaluate(p.Spec, base)
		r.Reasons = []string{reason}
		return &r
	}
	if err != nil {
		return unavailable(err.Error()), nil
	}
	c := s.setupSource()
	if c == nil {
		return unavailable("gateway_unavailable"), nil
	}
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
	keyBytes, _ := json.Marshal(struct {
		Contract rpc.ContractParams
		Date     string
	}{base.Contract, current.Date})
	key := string(keyBytes)
	// A cached profile is read without the gate. Cold collection is serialized
	// so one watchlist cannot fan out into hundreds of HMDS reads; the
	// underlying client retains its own pacing.
	profile, found := s.setupProfiles.lookup(key)
	if !found {
		if err := s.setupProfiles.lock(ctx); err != nil {
			return unavailable("profile_acquisition_cancelled"), nil
		}
		// Another request may have filled the profile while this one waited.
		profile, found = s.setupProfiles.lookup(key)
		var e error
		if !found {
			var bars []ibkrlib.HistoricalBar
			bars, e = collectSetupHistory(ctx, prior[0].Open, prior[len(prior)-1].Close, func(ctx context.Context, start, end time.Time) ([]ibkrlib.HistoricalBar, error) {
				return c.FetchSetupBars(ctx, contract, start, end, 20*time.Second)
			})
			if e == nil {
				profile = setupProfile{prior: attachSetupBars(prior, bars), observed: s.setupClock()}
				if setupBaselineComplete(profile.prior) {
					s.setupProfiles.store(key, profile)
				} else {
					e = fmt.Errorf("baseline_bars_incomplete")
				}
			}
		}
		s.setupProfiles.unlock()
		if e != nil {
			return unavailable("baseline_history_unavailable: " + e.Error()), nil
		}
	}
	if !c.HistoricalSessionCurrent(binding) {
		return unavailable("broker_session_changed"), nil
	}
	bars, err := c.FetchSetupBars(ctx, contract, current.Open, at, 20*time.Second)
	if err != nil {
		return unavailable("current_history_unavailable: " + err.Error()), nil
	}
	if !c.HistoricalSessionCurrent(binding) {
		return unavailable("broker_session_changed"), nil
	}
	base.Current = attachSetupBars([]setups.Session{current}, bars)[0]
	base.Prior = profile.prior
	base.BaselineObservedAt = profile.observed
	base.ObservedAt = s.setupClock()
	// An explicit historical clock is reconstructed now, never evidence that
	// these corrected bars were available then. Current reads expire on age.
	r := setups.Evaluate(p.Spec, base)
	return &r, nil
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
func setupBaselineComplete(prior []setups.Session) bool {
	for _, p := range prior {
		if setups.ValidateSession(p, true) != nil {
			return false
		}
	}
	return true
}
