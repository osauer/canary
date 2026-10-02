package canary

import "github.com/osauer/canary/v2/internal/rpc"

// FXResult is the cached completed-day FX attribution returned by
// canary_reporting_fx. It contains no account identity or position details.
type FXResult = rpc.FXResult

// FXDay is a reconciled daily contribution or a dated evidence gap.
type FXDay = rpc.FXDay

// FXPeriod is a complete period total or an explicit coverage failure.
type FXPeriod = rpc.FXPeriod

// FXBackfill is the background statement acquisition progress.
type FXBackfill = rpc.FXBackfill

// ValidateFXResult checks the versioned, finite, missing-aware FX contract.
func ValidateFXResult(result FXResult) error { return rpc.ValidateFXResult(result) }
