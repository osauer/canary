//go:build !trading

package daemon

import "context"

// protectiveStopGuardWrite is unreachable without the trading write
// capability: runProtectiveStopGuard returns before planning.
func (s *Server) protectiveStopGuardWrite(context.Context, protectiveStopGuardStep) error {
	return ErrTradingDisabled
}
