// Package stress composes the portfolio Stress assessment from typed account,
// position, regime, and market-event inputs.
//
// ComputeStress owns deterministic evaluation after its inputs and clock are
// supplied; it performs no broker writes and owns no runtime state. The Fetch
// helpers obtain the required snapshots through the daemon's typed call
// surface, preserve unavailable and stale evidence, and then invoke the same
// computation.
package stress
