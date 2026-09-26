package spx

import (
	"errors"
	"slices"
	"time"
)

// RecoveryGateError stops parallel fan-out but permits a bounded serial read.
// It is appropriate for a historical-farm notice on a ready connection, never
// for a disconnected API or a broken TWS/server link. The original warning
// remains authoritative; successful reads prove only their own data.
type RecoveryGateError struct {
	Cause error
}

// Error reports the original warning without claiming that recovery occurred.
func (e *RecoveryGateError) Error() string { return e.Cause.Error() }

// Unwrap preserves the provider's original transport diagnostic.
func (e *RecoveryGateError) Unwrap() error { return e.Cause }

func recoverableGate(err error) bool {
	_, ok := errors.AsType[*RecoveryGateError](err)
	return ok
}

// Rotate after a failed symbol so a symbol-specific timeout cannot become
// another universe-wide veto. Completed windows leave the next plan normally.
func (e *Engine) rotateRecoveryPlan(plan []fetchPlan) []fetchPlan {
	for i, item := range plan {
		if item.Symbol == e.recoveryAfter && i+1 < len(plan) {
			return slices.Concat(plan[i+1:], plan[:i+1])
		}
	}
	return plan
}

// Include pacing, resolution and the response in one recovery-read budget.
const recoveryReadBudget = 2 * time.Minute

func (e *Engine) recordRecoveryResult(err error) {
	if err == nil {
		e.recoveryFailures = 0
		e.recoveryRetryAt = time.Time{}
		return
	}
	e.recoveryFailures = min(e.recoveryFailures+1, 5)
	delay := min(15*time.Minute, time.Minute*time.Duration(1<<(e.recoveryFailures-1)))
	e.recoveryRetryAt = e.clock().Add(delay)
}
