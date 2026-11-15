package rpc

import "github.com/osauer/canary/v3/internal/risk"

// CashSweepOperationalObservation exposes risk-owned partial observation.
// Gross components never certify net funding or broker-write authority.
type CashSweepOperationalObservation = risk.CashSweepOperationalObservation

// CashSweepCalibrationStudy is an advisory study, never a reserve admission.
type CashSweepCalibrationStudy = risk.CashSweepCalibrationStudy
