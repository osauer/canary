package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

const dailyPnLObservationStateKind = "daily_pnl_health"

type dailyPnLObservationDocument struct {
	Version int                                  `json:"version"`
	Failure *persistedDailyPnLObservationFailure `json:"failure,omitempty"`
}

type persistedDailyPnLObservationFailure struct {
	SourceKey  string                        `json:"source_key"`
	SessionKey string                        `json:"session_key"`
	Status     rpc.DailyPnLObservationStatus `json:"status"`
	AsOf       time.Time                     `json:"as_of"`
}

// dailyPnLObservationAuthority retains a detected regular-session failure
// until a newer valid frame proves recovery. Only an opaque scope fingerprint
// and value-free health state are persisted.
type dailyPnLObservationAuthority struct {
	mu            sync.Mutex
	core          *corestore.Store
	revision      int64
	failureSource string
	failure       *rpc.DailyPnLObservation
}

// bindCore loads the retained failure before the daemon publishes its socket.
// Invalid persisted semantics block startup instead of becoming an empty,
// falsely healthy state.
func (a *dailyPnLObservationAuthority) bindCore(ctx context.Context, core *corestore.Store) error {
	if a == nil || core == nil {
		return fmt.Errorf("daily P&L observation SQLite authority is unavailable")
	}
	doc, ok, err := core.GetStateDocument(ctx, daemonStateScope, dailyPnLObservationStateKind)
	if err != nil {
		return fmt.Errorf("load Daily P&L observation authority: %w", err)
	}
	state := dailyPnLObservationDocument{Version: 1}
	if ok {
		if err := json.Unmarshal(doc.JSON, &state); err != nil {
			return fmt.Errorf("decode Daily P&L observation authority: %w", err)
		}
		if err := validateDailyPnLObservationDocument(state); err != nil {
			return err
		}
	} else {
		raw, _ := json.Marshal(state)
		doc, err = core.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{
			ScopeKey: daemonStateScope, Kind: dailyPnLObservationStateKind, JSON: raw,
		})
		if err != nil {
			return fmt.Errorf("initialize Daily P&L observation authority: %w", err)
		}
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	a.core = core
	a.revision = doc.Revision
	a.failureSource = ""
	a.failure = nil
	if state.Failure != nil {
		a.failureSource = state.Failure.SourceKey
		a.failure = &rpc.DailyPnLObservation{
			Status: state.Failure.Status, SessionKey: state.Failure.SessionKey, AsOf: state.Failure.AsOf.UTC(),
		}
	}
	return nil
}

func (a *dailyPnLObservationAuthority) observe(ctx context.Context, source, sessionKey string, now time.Time, due bool, snap ibkrlib.AccountDailyPnL, hasFrame bool) (rpc.DailyPnLObservation, error) {
	now = now.UTC()
	sourceKey := dailyPnLObservationSourceKey(source)
	sessionKey = strings.TrimSpace(sessionKey)

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failure != nil && (a.failureSource != sourceKey || a.failure.SessionKey != sessionKey) {
		if err := a.clearFailureLocked(ctx); err != nil {
			return rpc.DailyPnLObservation{Status: rpc.DailyPnLObservationInvalid, SessionKey: sessionKey, AsOf: now}, err
		}
	}

	frameStatus := snap.DailyPnLStatus
	if frameStatus == "" {
		switch {
		case snap.DailyPnL == nil:
			frameStatus = ibkrlib.DailyPnLFrameUnavailable
		case math.IsNaN(*snap.DailyPnL) || math.IsInf(*snap.DailyPnL, 0):
			frameStatus = ibkrlib.DailyPnLFrameMalformed
		default:
			frameStatus = ibkrlib.DailyPnLFrameAvailable
		}
	}
	valid := hasFrame && frameStatus == ibkrlib.DailyPnLFrameAvailable && snap.DailyPnL != nil &&
		!math.IsNaN(*snap.DailyPnL) && !math.IsInf(*snap.DailyPnL, 0) && !snap.AsOf.IsZero() && !snap.AsOf.After(now)

	if a.failure != nil && valid && snap.AsOf.After(a.failure.AsOf) {
		recovered := rpc.DailyPnLObservation{Status: rpc.DailyPnLObservationOK, SessionKey: sessionKey, AsOf: snap.AsOf.UTC()}
		if err := a.clearFailureLocked(ctx); err != nil {
			return *a.failure, err
		}
		return recovered, nil
	}
	if a.failure != nil {
		if frameStatus == ibkrlib.DailyPnLFrameMalformed || (snap.DailyPnL != nil && !valid) {
			return a.recordFailureLocked(ctx, sourceKey, rpc.DailyPnLObservationInvalid, sessionKey, now)
		}
		return *a.failure, nil
	}

	if frameStatus == ibkrlib.DailyPnLFrameMalformed || (snap.DailyPnL != nil && !valid) {
		return a.recordFailureLocked(ctx, sourceKey, rpc.DailyPnLObservationInvalid, sessionKey, now)
	}
	if due {
		status := rpc.DailyPnLObservationMissing
		if valid && now.Sub(snap.AsOf) >= dailyPnLStaleGrace {
			status = rpc.DailyPnLObservationStale
		} else if valid {
			return rpc.DailyPnLObservation{Status: rpc.DailyPnLObservationOK, SessionKey: sessionKey, AsOf: snap.AsOf.UTC()}, nil
		}
		return a.recordFailureLocked(ctx, sourceKey, status, sessionKey, now)
	}
	if valid {
		return rpc.DailyPnLObservation{Status: rpc.DailyPnLObservationOK, SessionKey: sessionKey, AsOf: snap.AsOf.UTC()}, nil
	}
	return rpc.DailyPnLObservation{Status: rpc.DailyPnLObservationNotDue, SessionKey: sessionKey, AsOf: now}, nil
}

func dailyPnLObservationSourceKey(source string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(source)))
	return hex.EncodeToString(digest[:])
}

func (a *dailyPnLObservationAuthority) recordFailureLocked(ctx context.Context, sourceKey string, status rpc.DailyPnLObservationStatus, sessionKey string, at time.Time) (rpc.DailyPnLObservation, error) {
	if a.failure != nil && a.failureSource == sourceKey && a.failure.Status == status && a.failure.SessionKey == sessionKey {
		return *a.failure, nil
	}
	failure := rpc.DailyPnLObservation{Status: status, SessionKey: sessionKey, AsOf: at.UTC()}
	if err := a.persistLocked(ctx, dailyPnLObservationDocument{Version: 1, Failure: &persistedDailyPnLObservationFailure{
		SourceKey: sourceKey, SessionKey: sessionKey, Status: status, AsOf: failure.AsOf,
	}}); err != nil {
		return failure, err
	}
	a.failureSource = sourceKey
	a.failure = &failure
	return failure, nil
}

func (a *dailyPnLObservationAuthority) clearFailureLocked(ctx context.Context) error {
	if err := a.persistLocked(ctx, dailyPnLObservationDocument{Version: 1}); err != nil {
		return err
	}
	a.failureSource = ""
	a.failure = nil
	return nil
}

func (a *dailyPnLObservationAuthority) persistLocked(ctx context.Context, state dailyPnLObservationDocument) error {
	if a.core == nil {
		return nil
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode Daily P&L observation authority: %w", err)
	}
	saved, err := a.core.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{
		ScopeKey: daemonStateScope, Kind: dailyPnLObservationStateKind, ExpectedRevision: a.revision, JSON: raw,
	})
	if err != nil {
		return fmt.Errorf("persist Daily P&L observation authority: %w", err)
	}
	a.revision = saved.Revision
	return nil
}

func validateDailyPnLObservationDocument(doc dailyPnLObservationDocument) error {
	if doc.Version != 1 {
		return fmt.Errorf("daily P&L observation authority has unsupported version %d", doc.Version)
	}
	if doc.Failure == nil {
		return nil
	}
	failure := doc.Failure
	if len(failure.SourceKey) != sha256.Size*2 || strings.TrimSpace(failure.SessionKey) == "" || failure.AsOf.IsZero() {
		return fmt.Errorf("daily P&L observation authority failure is incomplete")
	}
	if _, err := hex.DecodeString(failure.SourceKey); err != nil {
		return fmt.Errorf("daily P&L observation authority has invalid source key")
	}
	if _, err := time.Parse(time.DateOnly, failure.SessionKey); err != nil {
		return fmt.Errorf("daily P&L observation authority has invalid session key")
	}
	switch failure.Status {
	case rpc.DailyPnLObservationMissing, rpc.DailyPnLObservationInvalid, rpc.DailyPnLObservationStale:
		return nil
	default:
		return fmt.Errorf("daily P&L observation authority has invalid failure status")
	}
}
