//go:build !trading

package daemon

import (
	"context"
	"time"
)

// stepMandateCut is unreachable without the trading write capability:
// runMandateCutSteps returns before any record is read.
func (e *proposalEngine) stepMandateCut(context.Context, automaticSubmissionRecord, time.Duration, int) (string, error) {
	return "", ErrTradingDisabled
}
