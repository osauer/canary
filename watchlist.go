package canary

import (
	"context"
	"github.com/osauer/canary/v2/internal/rpc"
)

// Watchlist is Canary's accepted owner list and current receipt clock.
type Watchlist = rpc.Watchlist

// WatchlistContract is a stock underlying; zero ConID is explicitly unresolved.
type WatchlistContract = rpc.WatchlistContract

// CodeWatchlistConflict identifies an explicit stale-revision or changed-terms rejection.
const CodeWatchlistConflict = rpc.CodeWatchlistConflict

// Watchlist reads durable owner preferences independently of broker connectivity.
// Mutations are intentionally absent from the public client and model tools.
func (c *Client) Watchlist(ctx context.Context) (*Watchlist, error) {
	conn, err := c.connect(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	var out Watchlist
	err = conn.Call(ctx, rpc.MethodWatchlistList, nil, &out)
	return &out, daemonError(err)
}
