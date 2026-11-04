package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// Standing authority for the daemon's own sends (owner, 2026-11-04 20:17 CET:
// Canary must not sell by itself unless armed). The protection policy lists
// which buckets may go; the device-confirmed standing authority says whether
// Canary may act at all. Protection or Fully armed both allow it, a breach
// holding Fully armed at Protection included. It stands across restarts and
// needs no running controller, so protection does not stop when Desk does.
// An unreadable authority reads as disarmed.
const automaticDisarmedCode = "automatic_trading_disarmed"

// standingAuthorityBlocker is nil while the standing authority covers this
// account and mode. An empty account (no broker scope yet) checks the scope
// only: nothing can be sent without a concrete scope, and the wire checks the
// exact account.
func (s *Server) standingAuthorityBlocker(ctx context.Context, account, mode string) *rpc.TradingBlocker {
	disarmed := func(why string) *rpc.TradingBlocker {
		return &rpc.TradingBlocker{Code: automaticDisarmedCode, Message: "automatic trading is disarmed: " + why, Action: "Arm Protection or Fully armed on your device, or submit by hand."}
	}
	if s == nil {
		return disarmed("the daemon is unavailable")
	}
	current, err := s.readStoredDeskAuthority(ctx)
	switch {
	case err != nil:
		return disarmed(fmt.Sprintf("the standing authority cannot be read (%v)", err))
	case current.Terms == nil || current.Scope == "off":
		return disarmed("no device-confirmed Protection or Fully armed authority stands")
	case account != "" && (current.Terms.AccountID != account || current.Terms.AccountMode != mode):
		return disarmed("the standing authority names another account")
	}
	return nil
}

// standingAuthorityConfirmedAt is when the owner last armed. A veto clock
// from before it never carries over: arming starts a full window.
func (s *Server) standingAuthorityConfirmedAt(ctx context.Context) time.Time {
	current, err := s.readStoredDeskAuthority(ctx)
	if err != nil {
		return time.Time{}
	}
	return current.ConfirmedAt
}

// lockStandingAuthorityForWire keeps a disarm serialized with the first bytes
// of a daemon-preauthorised write, as lockDeskAuthorityForWire does for the
// controller's. Lock order: brokerWriteMu (dispatch), deskAuthorityMu.
func (s *Server) lockStandingAuthorityForWire(ctx context.Context, status rpc.TradingStatus) (func(), error) {
	s.deskAuthorityMu.RLock()
	if blocker := s.standingAuthorityBlocker(ctx, status.Account, status.Mode); blocker != nil {
		s.deskAuthorityMu.RUnlock()
		return nil, fmt.Errorf("%w: %s", ErrTradingDisabled, blocker.Message)
	}
	return s.deskAuthorityMu.RUnlock, nil
}
