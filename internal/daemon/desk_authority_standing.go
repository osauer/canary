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

// automaticDisarmedError preserves the standing-authority cause through the
// place path without changing its public error text or ErrTradingDisabled class.
type automaticDisarmedError struct{ error }

func (e *automaticDisarmedError) Unwrap() error { return e.error }

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

// automaticGrantBlocker is standingAuthorityBlocker for the write in flight:
// the scheduler's grant must also name the arming that stands now, so a rearm
// landing after the cycle's read cannot send on the old veto clock. It reads
// as a disarm: the record ends with no clock to carry.
func (s *Server) automaticGrantBlocker(ctx context.Context, account, mode string) *rpc.TradingBlocker {
	if blocker := s.standingAuthorityBlocker(ctx, account, mode); blocker != nil {
		return blocker
	}
	if grant := s.automaticGrant.Load(); grant == nil || !grant.ArmedAt.Equal(s.standingAuthorityConfirmedAt(ctx)) {
		return &rpc.TradingBlocker{Code: automaticDisarmedCode, Message: "automatic trading was armed again since this row's veto clock started", Action: "A new veto window starts for the row."}
	}
	return nil
}

// lockStandingAuthorityForWire keeps a disarm serialized with the first bytes
// of a daemon-preauthorised write, as lockDeskAuthorityForWire does for the
// controller's. Lock order: brokerWriteMu (dispatch), deskAuthorityMu.
func (s *Server) lockStandingAuthorityForWire(ctx context.Context, status rpc.TradingStatus) (func(), error) {
	s.deskAuthorityMu.RLock()
	if blocker := s.automaticGrantBlocker(ctx, status.Account, status.Mode); blocker != nil {
		s.deskAuthorityMu.RUnlock()
		return nil, tradingBlockersError([]rpc.TradingBlocker{*blocker})
	}
	return s.deskAuthorityMu.RUnlock, nil
}
