package canary

import (
	"context"
	"github.com/osauer/canary/v2/internal/rpc"
	"time"
)

// SetupSpec is the fixed versioned intraday observation template.
type SetupSpec = rpc.SetupSpec

// SetupEvaluateParams selects one underlying and an optional reconstruction clock.
type SetupEvaluateParams = rpc.SetupEvaluateParams

// SetupContract preserves exact underlying identity for a setup evaluation.
type SetupContract = rpc.ContractParams

// SetupResult contains observed setup evidence without policy or order authority.
type SetupResult = rpc.SetupResult

// SetupBar contains explicit interval-start and interval-end timestamps.
type SetupBar = rpc.SetupBar

// SetupBaseline contains one comparable prior-session slot observation.
type SetupBaseline = rpc.SetupBaseline

// SetupFeatures contains measured, missing-aware setup values.
type SetupFeatures = rpc.SetupFeatures

// EvaluateSetup reads daemon-owned entry evidence. It does not save a playbook,
// create a delivery episode, change policy or transmit a broker order.
func (c *Client) EvaluateSetup(ctx context.Context, in SetupEvaluateParams) (*SetupResult, error) {
	budget, _ := rpc.LookupMethodTiming(rpc.MethodSetupsEvaluate)
	ctx, cancel := context.WithTimeout(ctx, budget.ClientTimeout(5*time.Second))
	defer cancel()
	conn, err := c.connect(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	var out SetupResult
	err = conn.Call(ctx, rpc.MethodSetupsEvaluate, in, &out)
	return &out, daemonError(err)
}

// SetupCoverageParams selects one retained session (default: the newest) and
// optionally one underlying symbol.
type SetupCoverageParams = rpc.SetupCoverageParams

// SetupCoverageResult is the per-session instrument for live setup
// evaluations; it is operational evidence, never a signal or authority.
type SetupCoverageResult = rpc.SetupCoverageResult

// SetupCoverageContract is one contract's live setup evaluations in a session.
type SetupCoverageContract = rpc.SetupCoverageContract

// SetupCoverage reads how live setup evaluations covered a session's completed
// five-minute bars and how many historical requests they issued. It needs no
// gateway and changes nothing.
func (c *Client) SetupCoverage(ctx context.Context, in SetupCoverageParams) (*SetupCoverageResult, error) {
	budget, _ := rpc.LookupMethodTiming(rpc.MethodSetupsCoverage)
	ctx, cancel := context.WithTimeout(ctx, budget.ClientTimeout(5*time.Second))
	defer cancel()
	conn, err := c.connect(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	var out SetupCoverageResult
	err = conn.Call(ctx, rpc.MethodSetupsCoverage, in, &out)
	return &out, daemonError(err)
}
