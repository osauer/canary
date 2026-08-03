package daemon

import (
	"context"
	"errors"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/marketcal"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// accountSnapshotFreshFor lets the daemon's closely spaced Rulebook, Stress,
// brief, app, and CLI reads share one completed broker snapshot. It is short
// enough that a decision never rides an old account value merely to save a
// request, while removing bursts of parallel reqAccountSummary subscriptions.
const accountSnapshotFreshFor = 15 * time.Second

// accountPnLMonitorEvery keeps reqPnL repair independent of account reads.
// The connector owns the 90-second stale threshold and retry throttle; this
// shorter poll only determines how soon it notices that threshold was crossed.
const accountPnLMonitorEvery = 15 * time.Second

type accountSnapshotSource struct {
	connector *ibkrlib.Connector
	session   ibkrlib.ConnectorSessionBinding
	scope     brokerStateScope
}

func (s accountSnapshotSource) same(other accountSnapshotSource) bool {
	return s.connector == other.connector && s.session == other.session && sameBrokerScope(s.scope, other.scope)
}

type accountSnapshot struct {
	raw        *ibkrlib.RawAccountSummary
	provenance ibkrlib.AccountSummaryProvenance
	observedAt time.Time
	source     accountSnapshotSource
}

type accountSnapshotFlight struct {
	source accountSnapshotSource
	done   chan struct{}
	result accountSnapshot
	err    error
}

// accountSnapshotAuthority owns the daemon's one-shot account-summary reads.
// One flight is allowed at a time, even across a broker-scope transition. A
// current request-authored result is reused briefly; an unstamped connector
// fallback is shared only by callers already waiting on that exact flight and
// is never installed as current authority.
type accountSnapshotAuthority struct {
	mu       sync.Mutex
	current  accountSnapshot
	hasValue bool
	flight   *accountSnapshotFlight
	now      func() time.Time
	freshFor time.Duration
}

type accountSnapshotFetch func(context.Context) (*ibkrlib.RawAccountSummary, ibkrlib.AccountSummaryProvenance, error)

func (a *accountSnapshotAuthority) read(ctx, operationCtx context.Context, source accountSnapshotSource, fetch accountSnapshotFetch) (accountSnapshot, error) {
	if ctx == nil {
		return accountSnapshot{}, errors.New("account snapshot: nil caller context")
	}
	if operationCtx == nil {
		operationCtx = context.Background()
	}
	if fetch == nil {
		return accountSnapshot{}, errors.New("account snapshot: nil fetcher")
	}

	for {
		a.mu.Lock()
		now := time.Now().UTC()
		if a.now != nil {
			now = a.now().UTC()
		}
		freshFor := a.freshFor
		if freshFor <= 0 {
			freshFor = accountSnapshotFreshFor
		}
		if a.hasValue && a.current.source.same(source) &&
			a.current.provenance == ibkrlib.AccountSummaryProvenanceRequest &&
			!a.current.observedAt.IsZero() {
			age := now.Sub(a.current.observedAt)
			if age >= 0 && age < freshFor {
				result := cloneAccountSnapshot(a.current)
				a.mu.Unlock()
				return result, nil
			}
		}

		if flight := a.flight; flight != nil {
			sameSource := flight.source.same(source)
			done := flight.done
			a.mu.Unlock()
			select {
			case <-ctx.Done():
				return accountSnapshot{}, ctx.Err()
			case <-done:
			}
			if sameSource {
				return cloneAccountSnapshot(flight.result), flight.err
			}
			// A scope or socket generation changed while the previous request
			// drained. Re-evaluate against the caller's current source without
			// ever opening a competing subscription.
			continue
		}

		flight := &accountSnapshotFlight{source: source, done: make(chan struct{})}
		a.flight = flight
		a.mu.Unlock()

		go a.runFlight(operationCtx, flight, fetch)
		select {
		case <-ctx.Done():
			return accountSnapshot{}, ctx.Err()
		case <-flight.done:
			return cloneAccountSnapshot(flight.result), flight.err
		}
	}
}

func (a *accountSnapshotAuthority) runFlight(ctx context.Context, flight *accountSnapshotFlight, fetch accountSnapshotFetch) {
	raw, provenance, err := fetch(ctx)
	result := accountSnapshot{
		raw: cloneRawAccountSummary(raw), provenance: provenance, source: flight.source,
	}
	if err == nil && raw != nil && provenance == ibkrlib.AccountSummaryProvenanceRequest {
		result.observedAt = raw.AsOf.UTC()
	}

	a.mu.Lock()
	flight.result = result
	flight.err = err
	if err == nil && result.raw != nil && provenance == ibkrlib.AccountSummaryProvenanceRequest && !result.observedAt.IsZero() {
		a.current = cloneAccountSnapshot(result)
		a.hasValue = true
	}
	if a.flight == flight {
		a.flight = nil
	}
	close(flight.done)
	a.mu.Unlock()
}

func cloneAccountSnapshot(in accountSnapshot) accountSnapshot {
	in.raw = cloneRawAccountSummary(in.raw)
	return in
}

func cloneRawAccountSummary(in *ibkrlib.RawAccountSummary) *ibkrlib.RawAccountSummary {
	if in == nil {
		return nil
	}
	out := *in
	out.NetLiquidation = cloneFloat64Ptr(in.NetLiquidation)
	out.BuyingPower = cloneFloat64Ptr(in.BuyingPower)
	out.AvailableFunds = cloneFloat64Ptr(in.AvailableFunds)
	out.ExcessLiquidity = cloneFloat64Ptr(in.ExcessLiquidity)
	out.TotalCashValue = cloneFloat64Ptr(in.TotalCashValue)
	out.MaintenanceMargin = cloneFloat64Ptr(in.MaintenanceMargin)
	out.InitMarginReq = cloneFloat64Ptr(in.InitMarginReq)
	out.GrossPositionValue = cloneFloat64Ptr(in.GrossPositionValue)
	out.UnrealizedPnL = cloneFloat64Ptr(in.UnrealizedPnL)
	out.RealizedPnL = cloneFloat64Ptr(in.RealizedPnL)
	out.Cushion = cloneFloat64Ptr(in.Cushion)
	out.LookAheadInitMargin = cloneFloat64Ptr(in.LookAheadInitMargin)
	out.LookAheadMaintMargin = cloneFloat64Ptr(in.LookAheadMaintMargin)
	out.LookAheadAvailable = cloneFloat64Ptr(in.LookAheadAvailable)
	out.LookAheadExcess = cloneFloat64Ptr(in.LookAheadExcess)
	if in.CurrencyLedger != nil {
		out.CurrencyLedger = make(map[string]ibkrlib.CurrencyLedger, len(in.CurrencyLedger))
		maps.Copy(out.CurrencyLedger, in.CurrencyLedger)
	}
	if in.Raw != nil {
		out.Raw = make(map[string]string, len(in.Raw))
		maps.Copy(out.Raw, in.Raw)
	}
	return &out
}

func (s *Server) readAccountSnapshot(ctx context.Context, connector *ibkrlib.Connector) (accountSnapshot, error) {
	if connector == nil {
		return accountSnapshot{}, ibkrlib.ErrIBKRUnavailable
	}
	session, ok := connector.CaptureSession()
	if !ok {
		return accountSnapshot{}, ibkrlib.ErrIBKRUnavailable
	}
	source := accountSnapshotSource{connector: connector, session: session, scope: s.currentBrokerStateScope()}
	operationCtx := context.Background()
	s.mu.Lock()
	if s.serverCtx != nil {
		operationCtx = s.serverCtx
	}
	s.mu.Unlock()

	result, err := s.accountSnapshots.read(ctx, operationCtx, source, func(parent context.Context) (*ibkrlib.RawAccountSummary, ibkrlib.AccountSummaryProvenance, error) {
		requestCtx, cancel := context.WithTimeout(parent, 8*time.Second)
		defer cancel()
		return connector.RequestAccountSummaryWithProvenance(requestCtx, 8*time.Second)
	})
	if err != nil {
		return accountSnapshot{}, err
	}
	if !connector.SessionCurrent(session) || !sameBrokerScope(source.scope, s.currentBrokerStateScope()) {
		return accountSnapshot{}, ibkrlib.ErrIBKRUnavailable
	}
	if result.raw != nil && brokerScopeAccountConcrete(source.scope.Account) &&
		!strings.EqualFold(strings.TrimSpace(result.raw.AccountID), strings.TrimSpace(source.scope.Account)) {
		return accountSnapshot{}, ibkrlib.ErrAccountSummaryScopeConflict
	}
	return result, nil
}

type dailyPnLAuthorityConnector interface {
	AccountDailyPnL() (ibkrlib.AccountDailyPnL, bool)
	SubscribeAccountPnL(string) error
	MaybeResubscribeStaleDailyPnL(bool) bool
}

func maintainDailyPnLAuthority(connector dailyPnLAuthorityConnector, account string, marketOpen bool) {
	if connector == nil || !brokerScopeAccountConcrete(account) {
		return
	}
	// Ensure the stream exists even if post-connect setup raced account
	// discovery. MaybeResubscribe also handles an active stream whose first
	// frame or later frames stopped arriving.
	if _, ok := connector.AccountDailyPnL(); !ok {
		_ = connector.SubscribeAccountPnL(account)
	}
	connector.MaybeResubscribeStaleDailyPnL(marketOpen)
}

func (s *Server) runAccountPnLAuthorityLoop(ctx context.Context) {
	s.runAccountPnLAuthorityLoopWith(ctx, accountPnLMonitorEvery)
}

func (s *Server) runAccountPnLAuthorityLoopWith(ctx context.Context, cadence time.Duration) {
	if ctx == nil || cadence <= 0 {
		return
	}
	ticker := time.NewTicker(cadence)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			connector := s.gatewayConnector()
			if connector == nil {
				continue
			}
			now := time.Now()
			if s.now != nil {
				now = s.now()
			}
			var session marketcal.Session
			if current, err := marketcal.New().SessionAt(marketcal.MarketUSEquity, now); err == nil {
				session = current
			}
			maintainDailyPnLAuthority(connector, s.currentBrokerStateScope().Account, session.IsOpen)
			s.maybeCaptureDailyPnLClose(ctx, connector, session, now)
		}
	}
}
