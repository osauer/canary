package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// A fence is captured from daemon-validated authority for one exact action.
// It is never decoded from ordinary order RPC input. The controller must
// obtain a new fence after reconciling outcomes and changing preferences.
type deskAuthorityFence struct {
	ID               string
	Generation       int64
	PreferencesHash  string
	AdaptivePriority string
	Action           string
	ValidUntil       time.Time
}

// lockDeskAuthorityForWire keeps revocation serialized with the first bytes.
// Lock order: brokerWriteMu (dispatch), deskAuthorityMu, trading-control lease.
// Control/confirmation acquire only deskAuthorityMu, never brokerWriteMu.
func (s *Server) lockDeskAuthorityForWire(ctx context.Context, fence *deskAuthorityFence, status rpc.TradingStatus) (func(), error) {
	if fence == nil {
		return func() {}, nil
	}
	s.deskAuthorityMu.RLock()
	fail := func() (func(), error) {
		s.deskAuthorityMu.RUnlock()
		return nil, fmt.Errorf("%w: automatic trading authority changed or expired before transmit", ErrTradingDisabled)
	}
	current, err := s.readDeskAuthority(ctx)
	if err != nil {
		s.deskAuthorityMu.RUnlock()
		return nil, err
	}
	if current.Terms == nil || current.Terms.ID != fence.ID || current.Generation != fence.Generation || !current.Running || current.Scope == "off" ||
		current.Terms.AccountID != status.Account || current.Terms.AccountMode != status.Mode || current.PreferencesHash != fence.PreferencesHash ||
		current.AdaptivePriority != fence.AdaptivePriority || fence.ValidUntil.IsZero() || !s.orderNow().Before(fence.ValidUntil) {
		return fail()
	}
	switch fence.Action {
	case "entry":
		if current.Scope != "full" {
			return fail()
		}
	case "protection", "reduction":
	default:
		return fail()
	}
	return s.deskAuthorityMu.RUnlock, nil
}
