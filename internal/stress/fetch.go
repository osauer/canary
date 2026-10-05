// Package stress composes the portfolio Stress assessment from typed account,
// position, regime, and market-event inputs.
//
// ComputeStress owns deterministic evaluation after its inputs and clock are
// supplied; it performs no broker writes and owns no runtime state. Only the
// daemon evaluates: Compose gathers the snapshots through the daemon's own
// handlers and serves the result as MethodStressSnapshot. Readers (CLI, MCP,
// app, the exported Go client) call FetchStress and never compute a verdict,
// so a reader built at an older version cannot disagree with the daemon.
package stress

import (
	"context"
	"fmt"
	"github.com/osauer/canary/v2/internal/rpc"
	"time"
)

// FetchStress reads the daemon's portfolio Stress assessment. The daemon
// composes it (Compose), so a reader compiled at an older version still shows
// the installed daemon's verdict.
func FetchStress(ctx context.Context, conn interface {
	Call(context.Context, string, any, any) error
}) (rpc.StressResult, error) {
	var out rpc.StressSnapshotResult
	if err := conn.Call(ctx, rpc.MethodStressSnapshot, rpc.StressSnapshotParams{}, &out); err != nil {
		return rpc.StressResult{}, err
	}
	return out.Stress, nil
}

// FetchStressWithRegimeMonitor reads the daemon's Stress assessment together
// with the compact regime monitor read it was composed from.
func FetchStressWithRegimeMonitor(ctx context.Context, conn interface {
	Call(context.Context, string, any, any) error
}) (rpc.StressResult, *rpc.RegimeMonitorResult, error) {
	var out rpc.StressSnapshotResult
	if err := conn.Call(ctx, rpc.MethodStressSnapshot, rpc.StressSnapshotParams{RegimeMonitor: true}, &out); err != nil {
		return rpc.StressResult{}, nil, err
	}
	if out.RegimeMonitor == nil {
		return rpc.StressResult{}, nil, fmt.Errorf("stress: daemon omitted the regime monitor read")
	}
	return out.Stress, out.RegimeMonitor, nil
}

// Compose reads account, positions, regime, and relevant held-name
// market-event context sequentially through the daemon's own handlers, then
// returns the assessment and the compacted regime input. Only the daemon calls
// it; readers use FetchStress. Required-source errors abort the call;
// market-event failure is retained as unknown source health.
func Compose(ctx context.Context, conn interface {
	Call(context.Context, string, any, any) error
}) (rpc.StressResult, rpc.RegimeSnapshotResult, error) {
	var acct rpc.AccountResult
	if err := conn.Call(ctx, rpc.MethodAccountSummary, nil, &acct); err != nil {
		return rpc.StressResult{}, rpc.RegimeSnapshotResult{}, fmt.Errorf("account: %w", err)
	}
	var pos rpc.PositionsResult
	if err := conn.Call(ctx, rpc.MethodPositionsList, rpc.PositionsListParams{}, &pos); err != nil {
		return rpc.StressResult{}, rpc.RegimeSnapshotResult{}, fmt.Errorf("positions: %w", err)
	}
	var regime rpc.RegimeSnapshotResult
	if err := conn.Call(ctx, rpc.MethodRegimeSnapshot, rpc.RegimeSnapshotParams{}, &regime); err != nil {
		return rpc.StressResult{}, rpc.RegimeSnapshotResult{}, fmt.Errorf("regime: %w", err)
	}
	marketEvents := fetchStressMarketEvents(ctx, conn, pos)
	concentration, netExposure, margin := fetchStressRulebook(ctx, conn)
	if acct.DailyPnL == nil {
		var refreshed rpc.AccountResult
		if err := conn.Call(ctx, rpc.MethodAccountSummary, nil, &refreshed); err == nil && refreshed.DailyPnL != nil {
			acct = refreshed
		}
	}
	res := ComputeStress(StressInput{Account: acct, Positions: pos, Regime: regime, MarketEvents: marketEvents, Concentration: concentration, NetExposure: netExposure, MarginHeadroom: margin})
	rpc.CompactRegimeSnapshot(&regime)
	return res, regime, nil
}

// fetchStressRulebook reads the Rulebook's concentration verdicts (rules 1
// and 16), its net-exposure verdict (rule 15) and its margin-headroom verdict
// (rule 19) in one call; the stress read measures none of them itself. A
// failed read leaves every reading unavailable, which the concentration,
// exposure and margin rows report, never as a pass.
func fetchStressRulebook(ctx context.Context, conn interface {
	Call(context.Context, string, any, any) error
}) (*rpc.StressConcentration, *rpc.StressNetExposure, *rpc.StressMarginHeadroom) {
	var rules rpc.RulesResult
	if err := conn.Call(ctx, rpc.MethodRulesSnapshot, rpc.RulesSnapshotParams{}, &rules); err != nil {
		reason := "the Rulebook read failed: " + err.Error()
		return &rpc.StressConcentration{Reason: reason}, &rpc.StressNetExposure{Reason: reason}, &rpc.StressMarginHeadroom{Reason: reason}
	}
	return rpc.StressConcentrationFromRules(&rules), rpc.StressNetExposureFromRules(&rules), rpc.StressMarginHeadroomFromRules(&rules)
}

func fetchStressMarketEvents(ctx context.Context, conn interface {
	Call(context.Context, string, any, any) error
}, pos rpc.PositionsResult) rpc.MarketEventsResult {
	symbols, _ := rpc.MarketEventScope(&pos)
	if len(symbols) == 0 {
		return rpc.MarketEventsResult{}
	}
	var out rpc.MarketEventsResult
	if err := conn.Call(ctx, rpc.MethodMarketEventsSnapshot, rpc.MarketEventsParams{Symbols: symbols}, &out); err != nil {
		now := time.Now().UTC()
		out = rpc.MarketEventsResult{
			Kind:          rpc.MarketEventsKind,
			SchemaVersion: rpc.MarketEventsSchemaVersion,
			AsOf:          now,
			Symbols:       symbols,
			SourceHealth: []rpc.SourceHealth{{
				Source:               "market_events",
				Status:               rpc.SourceStatusUnknown,
				AsOf:                 now,
				Confidence:           "low",
				FingerprintStability: rpc.FingerprintStabilitySemanticBuckets,
				Notes:                []string{err.Error()},
			}},
			WarningDetails: []rpc.DataWarning{{
				Code:     "market_events_unavailable",
				Scope:    "market_events",
				Severity: "data_quality",
				Message:  "Market-event snapshot unavailable: " + err.Error(),
				Impact:   "Held-name market-event flags remain unknown, not inactive.",
				Action:   "Retry market-events before relying on absence of halt, LULD, Reg SHO, or borrow pressure tags.",
			}},
			NotExecution: "Market-event flags are observed context and daemon safety gates; no orders are placed by Canary.",
		}
		out.Fingerprint = rpc.BuildMarketEventsFingerprint(&out)
	}
	return out
}

// ComposeMethods lists Compose's sequential reads, including the optional P&L
// retry; MethodStressSnapshot's daemon deadline must cover all of them.
func ComposeMethods() []string {
	return []string{rpc.MethodAccountSummary, rpc.MethodPositionsList, rpc.MethodRegimeSnapshot, rpc.MethodMarketEventsSnapshot, rpc.MethodRulesSnapshot, rpc.MethodAccountSummary}
}
