package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/marketcal"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

const dailyPnLCloseCaptureStateKind = "daily_pnl_close_capture"

// dailyPnLCloseCaptureWindow bounds how long after the official close a frame
// may still be captured as that session's result. The broker's daily P&L keeps
// recomputing at extended-hours marks, so a late capture is a drifted one; a
// daemon that was not running and connected inside this window leaves the
// session uncaptured, and the brief must say so rather than substitute a
// drifted value. The window also ends well before TWS's own trading-day
// rollover and scheduled restarts, so a relogin recomputation cannot masquerade
// as a close print.
const dailyPnLCloseCaptureWindow = 10 * time.Minute

// dailyPnLScopeSource is the broker-scope identity behind the daily P&L
// observation authority and the close-capture authority. It is persisted only
// as an opaque fingerprint (dailyPnLObservationSourceKey).
func dailyPnLScopeSource(scope brokerStateScope) string {
	return scope.Mode + "|" + strings.TrimSpace(scope.Account)
}

type dailyPnLCloseCaptureDocument struct {
	Version int `json:"version"`
	// Captures holds the newest close capture per opaque broker-scope
	// fingerprint; no account identity is persisted.
	Captures map[string]persistedDailyPnLCloseCapture `json:"captures,omitempty"`
}

// persistedDailyPnLCloseCapture pins one broker scope's account Daily P&L to
// one session's official close.
type persistedDailyPnLCloseCapture struct {
	SessionKey   string    `json:"session_key"`
	DailyPnL     float64   `json:"daily_pnl"`
	BaseCurrency string    `json:"base_currency"`
	SessionClose time.Time `json:"session_close"`
	CapturedAt   time.Time `json:"captured_at"`
}

// dailyPnLCloseCaptureAuthority retains, per broker scope, the account-level
// reqPnL Daily P&L observed at (or on the first frame after) each official
// US-equity close. The value is the desk's only honest "last completed
// session" figure: everything the broker serves later moves on off-session
// marks. A session with no capture stays absent — serving surfaces report it
// as not captured instead of substituting a running value.
type dailyPnLCloseCaptureAuthority struct {
	mu       sync.Mutex
	core     *corestore.Store
	revision int64
	captures map[string]persistedDailyPnLCloseCapture
}

// bindCore loads retained captures before the daemon publishes its socket.
// Invalid persisted semantics block startup instead of becoming an empty,
// falsely clean state.
func (a *dailyPnLCloseCaptureAuthority) bindCore(ctx context.Context, core *corestore.Store) error {
	if a == nil || core == nil {
		return fmt.Errorf("daily P&L close-capture SQLite authority is unavailable")
	}
	doc, ok, err := core.GetStateDocument(ctx, daemonStateScope, dailyPnLCloseCaptureStateKind)
	if err != nil {
		return fmt.Errorf("load Daily P&L close-capture authority: %w", err)
	}
	state := dailyPnLCloseCaptureDocument{Version: 1}
	if ok {
		if err := json.Unmarshal(doc.JSON, &state); err != nil {
			return fmt.Errorf("decode Daily P&L close-capture authority: %w", err)
		}
		if err := validateDailyPnLCloseCaptureDocument(state); err != nil {
			return err
		}
	} else {
		raw, _ := json.Marshal(state)
		doc, err = core.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{
			ScopeKey: daemonStateScope, Kind: dailyPnLCloseCaptureStateKind, JSON: raw,
		})
		if err != nil {
			return fmt.Errorf("initialize Daily P&L close-capture authority: %w", err)
		}
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	a.core = core
	a.revision = doc.Revision
	a.captures = make(map[string]persistedDailyPnLCloseCapture, len(state.Captures))
	maps.Copy(a.captures, state.Captures)
	return nil
}

// capture persists record as source's newest close capture. Re-capturing the
// same session for the same scope is an idempotent no-op, so the monitor loop
// cannot overwrite the first eligible frame with a later, more drifted one.
func (a *dailyPnLCloseCaptureAuthority) capture(ctx context.Context, source string, record persistedDailyPnLCloseCapture) error {
	if a == nil {
		return fmt.Errorf("daily P&L close-capture authority is unavailable")
	}
	sourceKey := dailyPnLObservationSourceKey(source)

	a.mu.Lock()
	defer a.mu.Unlock()
	if existing, ok := a.captures[sourceKey]; ok && existing.SessionKey == record.SessionKey {
		return nil
	}
	next := make(map[string]persistedDailyPnLCloseCapture, len(a.captures)+1)
	maps.Copy(next, a.captures)
	next[sourceKey] = record
	if a.core != nil {
		raw, err := json.Marshal(dailyPnLCloseCaptureDocument{Version: 1, Captures: next})
		if err != nil {
			return fmt.Errorf("encode Daily P&L close-capture authority: %w", err)
		}
		saved, err := a.core.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{
			ScopeKey: daemonStateScope, Kind: dailyPnLCloseCaptureStateKind, ExpectedRevision: a.revision, JSON: raw,
		})
		if err != nil {
			return fmt.Errorf("persist Daily P&L close-capture authority: %w", err)
		}
		a.revision = saved.Revision
	}
	a.captures = next
	return nil
}

// captureFor returns source's retained capture. Callers own the session-date
// match: a capture for any other session must read as not captured.
func (a *dailyPnLCloseCaptureAuthority) captureFor(source string) (persistedDailyPnLCloseCapture, bool) {
	if a == nil {
		return persistedDailyPnLCloseCapture{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	capture, ok := a.captures[dailyPnLObservationSourceKey(source)]
	return capture, ok
}

func (a *dailyPnLCloseCaptureAuthority) capturedSession(source string) string {
	capture, ok := a.captureFor(source)
	if !ok {
		return ""
	}
	return capture.SessionKey
}

func validateDailyPnLCloseCaptureDocument(doc dailyPnLCloseCaptureDocument) error {
	if doc.Version != 1 {
		return fmt.Errorf("daily P&L close-capture authority has unsupported version %d", doc.Version)
	}
	for key, capture := range doc.Captures {
		if len(key) != sha256.Size*2 {
			return fmt.Errorf("daily P&L close-capture authority has invalid scope key")
		}
		if _, err := hex.DecodeString(key); err != nil {
			return fmt.Errorf("daily P&L close-capture authority has invalid scope key")
		}
		if _, err := time.Parse(time.DateOnly, capture.SessionKey); err != nil {
			return fmt.Errorf("daily P&L close-capture authority has invalid session key")
		}
		if math.IsNaN(capture.DailyPnL) || math.IsInf(capture.DailyPnL, 0) {
			return fmt.Errorf("daily P&L close-capture authority has non-finite value")
		}
		if strings.TrimSpace(capture.BaseCurrency) == "" {
			return fmt.Errorf("daily P&L close-capture authority has no base currency")
		}
		if capture.SessionClose.IsZero() || capture.CapturedAt.IsZero() {
			return fmt.Errorf("daily P&L close-capture authority capture is incomplete")
		}
	}
	return nil
}

// dailyPnLCloseCaptureEligible reports whether now sits inside sess's
// close-capture window: a regular or early-close trading date whose official
// close is at most dailyPnLCloseCaptureWindow behind now. Holidays, weekends,
// unknown-coverage dates, and pre-close instants are never eligible.
func dailyPnLCloseCaptureEligible(sess marketcal.Session, now time.Time) bool {
	if sess.State != marketcal.StateRegular && sess.State != marketcal.StateEarlyClose {
		return false
	}
	if sess.Close.IsZero() || now.Before(sess.Close) {
		return false
	}
	return now.Sub(sess.Close) < dailyPnLCloseCaptureWindow
}

// dailyPnLCloseFrameUsable reports whether snap can stand as the close print.
// A frame older than the connector's own staleness window predates the close
// and may be a mid-session value from a wedged stream; the post-close
// self-heal is deliberately off (the market is closed), so such a frame stays
// unusable and the session reads not captured.
func dailyPnLCloseFrameUsable(snap ibkrlib.AccountDailyPnL, hasFrame bool, sessionClose, now time.Time) bool {
	if !hasFrame || snap.DailyPnL == nil || math.IsNaN(*snap.DailyPnL) || math.IsInf(*snap.DailyPnL, 0) {
		return false
	}
	if status := snap.DailyPnLStatus; status != "" && status != ibkrlib.DailyPnLFrameAvailable {
		return false
	}
	if snap.AsOf.IsZero() || snap.AsOf.After(now) {
		return false
	}
	return !snap.AsOf.Before(sessionClose.Add(-dailyPnLStaleGrace))
}

// dailyPnLCloseCaptureSource is the connector slice the capture needs: the
// newest account reqPnL frame and the streaming account cache that proves the
// base currency the frame is denominated in.
type dailyPnLCloseCaptureSource interface {
	AccountDailyPnL() (ibkrlib.AccountDailyPnL, bool)
	CachedAccountSummary() *ibkrlib.RawAccountSummary
}

// maybeCaptureDailyPnLClose runs on the account P&L monitor cadence and pins
// the account Daily P&L to sess's official close, once per scope and session.
// Every guard fails toward absence: no concrete scope, no eligible window, no
// usable frame, or no proven base currency leaves the session uncaptured
// rather than recording a value that cannot be proven to be the close print.
func (s *Server) maybeCaptureDailyPnLClose(ctx context.Context, source dailyPnLCloseCaptureSource, sess marketcal.Session, now time.Time) {
	if s == nil || source == nil || !dailyPnLCloseCaptureEligible(sess, now) {
		return
	}
	scope := s.currentBrokerStateScope()
	if !brokerScopeConcrete(scope) {
		return
	}
	scopeSource := dailyPnLScopeSource(scope)
	if s.dailyPnLCloseCaptures.capturedSession(scopeSource) == sess.Date {
		return
	}
	snap, hasFrame := source.AccountDailyPnL()
	if !dailyPnLCloseFrameUsable(snap, hasFrame, sess.Close, now) {
		return
	}
	// The capture is durable, so its currency label must be proven rather than
	// inferred: RawAccountSummary.Currency is the legacy numeric-row fallback,
	// and on the flat streaming map this reads it can name a currency no broker
	// field established.
	summary := source.CachedAccountSummary()
	if summary == nil || !summary.BaseCurrencyProvenance.Proven() {
		return
	}
	record := persistedDailyPnLCloseCapture{
		SessionKey:   sess.Date,
		DailyPnL:     *snap.DailyPnL,
		BaseCurrency: summary.BaseCurrency,
		SessionClose: sess.Close.UTC(),
		CapturedAt:   snap.AsOf.UTC(),
	}
	if err := s.dailyPnLCloseCaptures.capture(ctx, scopeSource, record); err != nil {
		s.logger.Warnf("Daily P&L close capture for session %s failed: %v", sess.Date, err)
		return
	}
	s.logger.Infof("Daily P&L close captured for session %s (frame %s, close %s)",
		sess.Date, record.CapturedAt.Format(time.RFC3339), record.SessionClose.Format(time.RFC3339))
}

// lastCompletedUSEquitySessionDate resolves which official session date the
// brief may call "last completed" at now: today once today's close has passed,
// otherwise the newest completed prior trading date. False when embedded
// calendar coverage cannot prove one.
func lastCompletedUSEquitySessionDate(now time.Time) (string, bool) {
	sess, err := marketcal.New().SessionAt(marketcal.MarketUSEquity, now)
	if err != nil || sess.State == marketcal.StateUnknown {
		return "", false
	}
	if (sess.State == marketcal.StateRegular || sess.State == marketcal.StateEarlyClose) &&
		!sess.Close.IsZero() && !now.Before(sess.Close) {
		return sess.Date, true
	}
	dates := regimePrevSessionDates(now, 1)
	if len(dates) == 0 {
		return "", false
	}
	return dates[0], true
}
