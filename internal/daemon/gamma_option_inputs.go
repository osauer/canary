package daemon

import (
	"context"
	"time"

	ibkr "github.com/osauer/canary/v3/pkg/ibkr"
)

type gammaOptionInputReader struct {
	model  func() (float64, int, bool)
	greeks func() (ibkr.Greeks, bool)
	oi     func() (int64, bool)
}

// Spend the existing 1.5 s model wait plus 750 ms OI grace as one bounded
// collection window. Early OI must not cut off a model tick still in flight.
const gammaOptionInputBudget = 1500*time.Millisecond + optionOpenInterestGrace

func collectGammaOptionInputs(ctx context.Context, dataType, key string, reject <-chan ibkr.SubscriptionRejection, read gammaOptionInputReader) (legResult, error) {
	var result legResult
	deadline := time.Now().Add(gammaOptionInputBudget)
	modelWait := time.NewTimer(gammaOptionInputBudget)
	defer modelWait.Stop()
	poll := time.NewTicker(pollCadence)
	defer poll.Stop()
	var oiDeadline time.Time
	observe := func() bool {
		if !result.OK {
			if iv, kind, ok := read.model(); ok && iv > 0 && gammaOptionModelDataTypeCompatible(dataType, kind) {
				// Prompt model receipts still wait at most the original OI
				// grace; late ones cannot extend the total collection budget.
				oiDeadline = time.Now().Add(optionOpenInterestGrace)
				if oiDeadline.After(deadline) {
					oiDeadline = deadline
				}
				result.IV = iv
				if g, ok := read.greeks(); ok {
					result.Gamma = g.Gamma
				}
				result.OK, result.IVSource = true, gammaIVSourceModelTick
			}
		}
		if result.OK {
			result.OI, result.OIObserved = read.oi()
			return result.OIObserved || !time.Now().Before(oiDeadline)
		}
		return false
	}
	for {
		if err := ctx.Err(); err != nil {
			return legResult{}, err
		}
		if observe() {
			return result, nil
		}
		select {
		case <-ctx.Done():
			return legResult{}, ctx.Err()
		case rej := <-reject:
			return legResult{}, &SubscriptionRejectedError{Key: key, Rejection: rej}
		case <-modelWait.C:
			if err := ctx.Err(); err != nil {
				return legResult{}, err
			}
			// Include a receipt at the deadline, then take one OI snapshot
			// for fallback pricing without starting another grace period.
			observe()
			if !result.OK {
				result.OI, result.OIObserved = read.oi()
			}
			return result, nil
		case <-poll.C:
		}
	}
}
