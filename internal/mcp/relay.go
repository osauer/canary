package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/osauer/canary/v3/internal/rpc"
)

// relay asks the daemon for m and returns its result as the daemon sent it,
// once this build's type has accepted it. The result is decoded into R and
// encoded again, so a result the typed path refused is refused here with the
// same error, and check, when given, sees the typed value. Only what is
// returned differs from the typed path: the daemon's bytes, which keep the
// fields this build does not know. A type that accepts only its own keys
// still refuses a result that carries others.
func relay[P, R any](ctx context.Context, conn rpc.Caller, m rpc.Method[P, R], p P, check func(R) error) (json.RawMessage, error) {
	raw, err := rpc.CallRaw(ctx, conn, m, p)
	if err != nil {
		return nil, err
	}
	var typed R
	if err := json.Unmarshal(raw, &typed); err != nil {
		return nil, fmt.Errorf("decode result: %w", err)
	}
	if check != nil {
		if err := check(typed); err != nil {
			return nil, err
		}
	}
	if _, err := json.Marshal(typed); err != nil {
		return nil, err
	}
	return raw, nil
}
