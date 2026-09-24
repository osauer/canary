package cli

import (
	"context"
	"encoding/json"

	"github.com/osauer/canary/v2/internal/rpc"
)

func runMarket(ctx context.Context, env *Env, args []string) int {
	if idx := firstPositionalIndex(args); idx >= 0 && args[idx] == "tape" {
		return runMarketTape(ctx, env, append(append([]string{}, args[:idx]...), args[idx+1:]...))
	}
	fs := flagSet(env, "market")
	fs.Bool("json", false, "emit machine-readable JSON")
	watch := fs.Bool("watch", false, "stream complete display snapshots as NDJSON")
	symbol := fs.String("symbol", "", "underlying symbol for history")
	r := fs.String("range", "1D", "history range: 1D, 5D, 1M, 6M, YTD, 1Y, 5Y")
	exchange := fs.String("exchange", "SMART", "exact quote exchange")
	sec := fs.String("type", "STK", "security type: STK, IND, CASH")
	currency := fs.String("currency", "USD", "quote currency")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if *watch {
		if *symbol != "" {
			return fail(env, "market: --watch cannot select history")
		}
		enc := json.NewEncoder(env.Stdout)
		if err := env.Conn.Stream(ctx, rpc.MethodDisplaySubscribe, nil, func(raw json.RawMessage) error { return enc.Encode(raw) }); err != nil && ctx.Err() == nil {
			return fail(env, "market stream: %v", err)
		}
		return 0
	}
	if *symbol != "" {
		var result rpc.MarketHistoryResult
		if err := env.Conn.Call(ctx, rpc.MethodMarketHistory, rpc.MarketHistoryParams{Contract: rpc.ContractParams{Symbol: *symbol, SecType: *sec, Exchange: *exchange, Currency: *currency}, Range: *r}, &result); err != nil {
			return fail(env, "market history: %v", err)
		}
		return printJSON(env, result)
	}
	var result rpc.MarketSnapshotResult
	if err := env.Conn.Call(ctx, rpc.MethodMarketSnapshot, nil, &result); err != nil {
		return fail(env, "market: %v", err)
	}
	return printJSON(env, result)
}

func runPortfolio(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "portfolio")
	fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	var res rpc.PortfolioSnapshotResult
	if err := env.Conn.Call(ctx, rpc.MethodPortfolioSnapshot, nil, &res); err != nil {
		return fail(env, "portfolio: %v", err)
	}
	return printJSON(env, res)
}
