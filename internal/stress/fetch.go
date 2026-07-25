package stress

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/osauer/ibkr/v2/internal/rpc"
)

// FetchStress reads the three existing snapshots needed by ComputeStress.
// dial.Conn serializes calls internally, so this stays sequential and avoids
// hidden socket contention in scheduled MCP runs.
func FetchStress(ctx context.Context, conn interface {
	Call(context.Context, string, any, any) error
}) (StressResult, error) {
	res, _, err := FetchStressSnapshot(ctx, conn)
	return res, err
}

// FetchStressSnapshot returns the computed assessment and the positions input
// used to produce it.
func FetchStressSnapshot(ctx context.Context, conn interface {
	Call(context.Context, string, any, any) error
}) (StressResult, rpc.PositionsResult, error) {
	res, positions, _, err := FetchStressSnapshotWithRegime(ctx, conn)
	return res, positions, err
}

// FetchStressSnapshotWithRegime reads account, positions, regime, and relevant
// held-name market-event context sequentially, then returns the assessment,
// positions input, and compacted regime input. Required-source errors abort the call;
// market-event failure is retained as unknown source health.
func FetchStressSnapshotWithRegime(ctx context.Context, conn interface {
	Call(context.Context, string, any, any) error
}) (StressResult, rpc.PositionsResult, rpc.RegimeSnapshotResult, error) {
	var acct rpc.AccountResult
	if err := conn.Call(ctx, rpc.MethodAccountSummary, nil, &acct); err != nil {
		return StressResult{}, rpc.PositionsResult{}, rpc.RegimeSnapshotResult{}, fmt.Errorf("account: %w", err)
	}
	var pos rpc.PositionsResult
	if err := conn.Call(ctx, rpc.MethodPositionsList, rpc.PositionsListParams{}, &pos); err != nil {
		return StressResult{}, rpc.PositionsResult{}, rpc.RegimeSnapshotResult{}, fmt.Errorf("positions: %w", err)
	}
	var regime rpc.RegimeSnapshotResult
	if err := conn.Call(ctx, rpc.MethodRegimeSnapshot, rpc.RegimeSnapshotParams{}, &regime); err != nil {
		return StressResult{}, rpc.PositionsResult{}, rpc.RegimeSnapshotResult{}, fmt.Errorf("regime: %w", err)
	}
	marketEvents := fetchStressMarketEvents(ctx, conn, pos)
	if acct.DailyPnL == nil {
		var refreshed rpc.AccountResult
		if err := conn.Call(ctx, rpc.MethodAccountSummary, nil, &refreshed); err == nil && refreshed.DailyPnL != nil {
			acct = refreshed
		}
	}
	res := ComputeStress(StressInput{Account: acct, Positions: pos, Regime: regime, MarketEvents: marketEvents})
	rpc.CompactRegimeSnapshot(&regime)
	return res, pos, regime, nil
}

func fetchStressMarketEvents(ctx context.Context, conn interface {
	Call(context.Context, string, any, any) error
}, pos rpc.PositionsResult) rpc.MarketEventsResult {
	symbols := stressMarketEventSymbols(pos)
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
				Status:               rpc.MarketEventStatusUnknown,
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
			NotExecution: "Market-event flags are observed context and daemon safety gates; no orders are placed by ibkr.",
		}
		out.Fingerprint = rpc.BuildMarketEventsFingerprint(&out)
	}
	return out
}

func stressMarketEventSymbols(pos rpc.PositionsResult) []string {
	seen := map[string]bool{}
	out := []string{}
	add := func(value string) {
		sym := strings.ToUpper(strings.TrimSpace(value))
		if sym == "" || seen[sym] {
			return
		}
		seen[sym] = true
		out = append(out, sym)
	}
	for _, stock := range pos.Stocks {
		add(stock.Symbol)
	}
	for _, group := range pos.ByUnderlying {
		add(group.Underlying)
		if group.Stock != nil {
			add(group.Stock.Symbol)
		}
		for _, opt := range group.Options {
			add(opt.Symbol)
		}
	}
	slices.Sort(out)
	return out
}
