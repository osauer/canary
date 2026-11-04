package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/broker"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
)

// The intraday account value (internal-docs/design/account-intraday.md):
// one net-liquidation sample per five-minute bucket of each US-equity
// session, the current and the previous session retained per broker scope.
// Display evidence only; nothing here feeds a decision.

const (
	accountIntradayStateKind = "account_intraday"
	// IBKR refreshes account values about every three minutes; a finer
	// bucket would repeat values, not add evidence.
	accountIntradayBucket   = 5 * time.Minute
	accountIntradayEvery    = 30 * time.Second
	accountIntradaySessions = 2
	accountIntradayScopes   = 4
)

type accountIntradayDocument struct {
	Version int `json:"version"`
	// Scopes is keyed by the opaque broker-scope fingerprint the Daily P&L
	// authorities use; no account identity is persisted.
	Scopes map[string]accountIntradayScope `json:"scopes,omitempty"`
}

type accountIntradayScope struct {
	UpdatedAt time.Time `json:"updated_at"`
	// Sessions are newest first.
	Sessions []accountIntradaySession `json:"sessions"`
}

type accountIntradaySession struct {
	Date         string                     `json:"date"`
	Open         time.Time                  `json:"open"`
	Close        time.Time                  `json:"close"`
	BaseCurrency string                     `json:"base_currency"`
	Points       []rpc.AccountIntradayPoint `json:"points"`
}

type accountIntradayAuthority struct {
	mu       sync.Mutex
	core     *corestore.Store
	revision int64
	scopes   map[string]accountIntradayScope
}

func (a *accountIntradayAuthority) bindCore(ctx context.Context, core *corestore.Store) error {
	if a == nil || core == nil {
		return fmt.Errorf("account intraday SQLite authority is unavailable")
	}
	doc, ok, err := core.GetStateDocument(ctx, daemonStateScope, accountIntradayStateKind)
	if err != nil {
		return fmt.Errorf("load account intraday authority: %w", err)
	}
	state := accountIntradayDocument{Version: 1}
	if ok {
		if err := json.Unmarshal(doc.JSON, &state); err != nil {
			return fmt.Errorf("decode account intraday authority: %w", err)
		}
		if err := validateAccountIntradayDocument(state); err != nil {
			return err
		}
	} else {
		raw, _ := json.Marshal(state)
		doc, err = core.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{
			ScopeKey: daemonStateScope, Kind: accountIntradayStateKind, JSON: raw,
		})
		if err != nil {
			return fmt.Errorf("initialize account intraday authority: %w", err)
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.core = core
	a.revision = doc.Revision
	a.scopes = make(map[string]accountIntradayScope, len(state.Scopes))
	maps.Copy(a.scopes, state.Scopes)
	return nil
}

func (a *accountIntradayAuthority) bound() bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.core != nil
}

// hasBucket reports whether sourceKey already holds a point in sess's bucket.
func (a *accountIntradayAuthority) hasBucket(sourceKey string, sess marketcal.Session, bucket int) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, retained := range a.scopes[sourceKey].Sessions {
		if retained.Date == sess.Date && len(retained.Points) > 0 {
			return accountIntradayBucketIndex(retained.Open, retained.Points[len(retained.Points)-1].At) >= bucket
		}
	}
	return false
}

// record appends point to sess for sourceKey and persists the document. A
// point in a bucket already held, out of order, or in another currency than
// the session's is refused, leaving a gap. Memory changes only after the
// write, so a restart never loses a point a reader has seen.
func (a *accountIntradayAuthority) record(ctx context.Context, sourceKey string, sess marketcal.Session, currency string, point rpc.AccountIntradayPoint, now time.Time) (bool, error) {
	if a == nil {
		return false, fmt.Errorf("account intraday authority is unavailable")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.core == nil {
		return false, fmt.Errorf("account intraday authority is not bound")
	}
	scope := a.scopes[sourceKey]
	sessions := slices.Clone(scope.Sessions)
	// Only the newest session grows; an older one is history.
	if len(sessions) == 0 || sessions[0].Date < sess.Date {
		sessions = slices.Insert(sessions, 0, accountIntradaySession{Date: sess.Date, Open: sess.Open.UTC(), Close: sess.Close.UTC(), BaseCurrency: currency})
	} else if sessions[0].Date != sess.Date {
		return false, nil
	}
	target := sessions[0]
	if target.BaseCurrency != currency {
		return false, nil
	}
	if n := len(target.Points); n > 0 {
		last := target.Points[n-1].At
		if !point.At.After(last) || accountIntradayBucketIndex(target.Open, point.At) <= accountIntradayBucketIndex(target.Open, last) {
			return false, nil
		}
	}
	target.Points = append(slices.Clone(target.Points), rpc.AccountIntradayPoint{At: point.At.UTC(), Value: point.Value})
	sessions[0] = target
	if len(sessions) > accountIntradaySessions {
		sessions = sessions[:accountIntradaySessions]
	}
	next := make(map[string]accountIntradayScope, len(a.scopes)+1)
	maps.Copy(next, a.scopes)
	next[sourceKey] = accountIntradayScope{UpdatedAt: now.UTC(), Sessions: sessions}
	for len(next) > accountIntradayScopes {
		oldest := ""
		for key, s := range next {
			if key != sourceKey && (oldest == "" || s.UpdatedAt.Before(next[oldest].UpdatedAt)) {
				oldest = key
			}
		}
		delete(next, oldest)
	}
	raw, err := json.Marshal(accountIntradayDocument{Version: 1, Scopes: next})
	if err != nil {
		return false, fmt.Errorf("encode account intraday authority: %w", err)
	}
	saved, err := a.core.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{
		ScopeKey: daemonStateScope, Kind: accountIntradayStateKind, ExpectedRevision: a.revision, JSON: raw,
	})
	if err != nil {
		return false, fmt.Errorf("persist account intraday authority: %w", err)
	}
	a.revision = saved.Revision
	a.scopes = next
	return true, nil
}

func (a *accountIntradayAuthority) sessionsFor(sourceKey string) []accountIntradaySession {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	out := slices.Clone(a.scopes[sourceKey].Sessions)
	for i := range out {
		out[i].Points = slices.Clone(out[i].Points)
	}
	return out
}

func validateAccountIntradayDocument(doc accountIntradayDocument) error {
	if doc.Version != 1 {
		return fmt.Errorf("account intraday authority has unsupported version %d", doc.Version)
	}
	for key, scope := range doc.Scopes {
		if raw, err := hex.DecodeString(key); err != nil || len(raw) != sha256.Size {
			return fmt.Errorf("account intraday authority has invalid scope key")
		}
		for _, sess := range scope.Sessions {
			if _, err := time.Parse(time.DateOnly, sess.Date); err != nil {
				return fmt.Errorf("account intraday authority has invalid session date")
			}
			if sess.Open.IsZero() || !sess.Close.After(sess.Open) || strings.TrimSpace(sess.BaseCurrency) == "" {
				return fmt.Errorf("account intraday authority session is incomplete")
			}
			for _, p := range sess.Points {
				if p.At.IsZero() || !accountIntradayValue(p.Value) {
					return fmt.Errorf("account intraday authority has an invalid point")
				}
			}
		}
	}
	return nil
}

func accountIntradayValue(v float64) bool {
	return v > 0 && !math.IsInf(v, 0) && !math.IsNaN(v)
}

func accountIntradayBucketIndex(open, at time.Time) int {
	return int(at.Sub(open) / accountIntradayBucket)
}

// accountIntradayWindow reports whether at lies inside one of sess's trading
// windows. Only regular and early-close sessions are recorded.
func accountIntradayWindow(sess marketcal.Session, at time.Time) bool {
	if sess.State != marketcal.StateRegular && sess.State != marketcal.StateEarlyClose {
		return false
	}
	windows := sess.Windows
	if len(windows) == 0 {
		windows = []marketcal.Window{{Open: sess.Open, Close: sess.Close}}
	}
	for _, w := range windows {
		if !w.Open.IsZero() && !at.Before(w.Open) && at.Before(w.Close) {
			return true
		}
	}
	return false
}

// accountIntradaySample admits one account read as a sample for sess, or
// refuses it. Every guard fails toward a gap.
func accountIntradaySample(sess marketcal.Session, scope brokerStateScope, snap accountSnapshot) (rpc.AccountIntradayPoint, string, bool) {
	raw := snap.raw
	if raw == nil || snap.provenance != broker.ProvenanceRequest || snap.observedAt.IsZero() ||
		!sameBrokerScope(snap.source.scope, scope) || raw.NetLiquidation == nil || !accountIntradayValue(*raw.NetLiquidation) ||
		!raw.BaseCurrencyProvenance.Proven() || strings.TrimSpace(raw.BaseCurrency) == "" ||
		!accountIntradayWindow(sess, snap.observedAt) {
		return rpc.AccountIntradayPoint{}, "", false
	}
	return rpc.AccountIntradayPoint{At: snap.observedAt.UTC(), Value: *raw.NetLiquidation}, strings.ToUpper(strings.TrimSpace(raw.BaseCurrency)), true
}

func (s *Server) runAccountIntradayLoop(ctx context.Context) {
	ticker := time.NewTicker(accountIntradayEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sampleAccountIntraday(ctx)
		}
	}
}

// sampleAccountIntraday reads the account once per empty bucket of an open
// session. The read is the shared single-flight account snapshot, so another
// reader's fresh read serves it without a second broker request.
func (s *Server) sampleAccountIntraday(ctx context.Context) {
	now := s.nowUTC()
	sess, err := marketcal.New().SessionAt(marketcal.MarketUSEquity, now)
	if err != nil || !accountIntradayWindow(sess, now) || !s.accountIntraday.bound() {
		return
	}
	scope := s.currentBrokerStateScope()
	if !brokerScopeConcrete(scope) {
		return
	}
	key := dailyPnLObservationSourceKey(dailyPnLScopeSource(scope))
	if s.accountIntraday.hasBucket(key, sess, accountIntradayBucketIndex(sess.Open, now)) {
		return
	}
	connector := s.gatewayConnector()
	if connector == nil {
		return
	}
	readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	snap, err := s.readAccountSnapshot(readCtx, s.adapterFor(connector))
	cancel()
	if err != nil {
		return
	}
	point, currency, ok := accountIntradaySample(sess, scope, snap)
	if !ok {
		return
	}
	if _, err := s.accountIntraday.record(ctx, key, sess, currency, point, s.nowUTC()); err != nil {
		s.logger.Warnf("Account intraday sample for session %s failed: %v", sess.Date, err)
	}
}

func (s *Server) handleAccountIntraday(_ context.Context, req *rpc.Request) (*rpc.AccountIntradayResult, error) {
	if err := rpc.ValidateAccountIntradayParams(req.Params); err != nil {
		return nil, errBadRequest(err.Error())
	}
	now := s.nowUTC()
	out := &rpc.AccountIntradayResult{Kind: rpc.AccountIntradayKind, AsOf: now, Status: rpc.AccountIntradayAvailable, Sessions: []rpc.AccountIntradaySession{}}
	if sess, err := marketcal.New().SessionAt(marketcal.MarketUSEquity, now); err == nil && sess.State != marketcal.StateUnknown {
		out.MarketSession = &rpc.AccountIntradayMarketSession{Date: sess.Date, State: string(sess.State), IsOpen: accountIntradayWindow(sess, now), Open: sess.Open.UTC(), Close: sess.Close.UTC()}
	}
	scope := s.currentBrokerStateScope()
	switch {
	case !brokerScopeConcrete(scope):
		out.Status, out.Reason = rpc.AccountIntradayUnavailable, "No broker account is resolved, so no account history can be attributed."
		return out, nil
	case !s.accountIntraday.bound():
		out.Status, out.Reason = rpc.AccountIntradayUnavailable, "The daemon's state store is not available."
		return out, nil
	}
	out.Sessions = accountIntradayProjection(s.accountIntraday.sessionsFor(dailyPnLObservationSourceKey(dailyPnLScopeSource(scope))))
	return out, nil
}

func accountIntradayProjection(sessions []accountIntradaySession) []rpc.AccountIntradaySession {
	out := make([]rpc.AccountIntradaySession, 0, len(sessions))
	for _, sess := range sessions {
		row := rpc.AccountIntradaySession{Date: sess.Date, Open: sess.Open, Close: sess.Close, BaseCurrency: sess.BaseCurrency,
			IntervalSeconds: int(accountIntradayBucket / time.Second), Points: slices.Clone(sess.Points)}
		if row.Points == nil {
			row.Points = []rpc.AccountIntradayPoint{}
		}
		if len(sess.Points) > 0 && accountIntradayBucketIndex(sess.Open, sess.Points[0].At) == 0 {
			row.OpeningValue = new(sess.Points[0].Value)
		}
		out = append(out, row)
	}
	return out
}
