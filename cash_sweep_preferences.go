package canary

import (
	"context"
	"github.com/osauer/canary/v2/internal/rpc"
)

// CashSweepPreferences is the daemon's runtime ordering preference and receipt.
type CashSweepPreferences = rpc.CashSweepPreferences

// SetCashSweepPriorityRequest names one immutable ordering-preference edit.
type SetCashSweepPriorityRequest = rpc.SetCashSweepPriorityRequest

// CodeSettingsConflict reports a stale expected settings revision.
const CodeSettingsConflict = rpc.CodeSettingsConflict

// CashSweepPreferences reads runtime ordering preferences independently of
// proposal/risk readiness. This method is outside the MCP catalogue.
func (c *Client) CashSweepPreferences(ctx context.Context) (*CashSweepPreferences, error) {
	conn, err := c.connect(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	var out CashSweepPreferences
	err = conn.Call(ctx, rpc.MethodCashSweepPreferencesGet, nil, &out)
	return &out, daemonError(err)
}

// SetCashSweepPriority is restricted to an ordering preference. It claims no
// human-terminal origin, and cannot change a protection policy or broker gate.
func (c *Client) SetCashSweepPriority(ctx context.Context, in SetCashSweepPriorityRequest) (*CashSweepPreferences, error) {
	conn, err := c.connect(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	var out CashSweepPreferences
	err = conn.Call(ctx, rpc.MethodCashSweepPrioritySet, in, &out)
	return &out, daemonError(err)
}
