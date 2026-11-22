//go:build !trading

package daemon

import "context"

// protectiveStopGuardWrite is unreachable without the trading write
// capability: runProtectiveStopGuard returns before planning.
func (s *Server) protectiveStopGuardWrite(context.Context, protectiveStopGuardStep) error {
	return ErrTradingDisabled
}

// withdrawUnprotectedEntries has nothing to withdraw without the trading
// write capability: no protected add was ever sent.
func (s *Server) withdrawUnprotectedEntries(context.Context) {}
